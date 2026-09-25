// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/investigation/budget"
	"github.com/Pierre-Theophile/aisre/internal/investigation/engine"
	"github.com/Pierre-Theophile/aisre/internal/investigation/intake"
	"github.com/Pierre-Theophile/aisre/internal/investigation/ledger"
	"github.com/Pierre-Theophile/aisre/internal/investigation/render"
	investigationstore "github.com/Pierre-Theophile/aisre/internal/investigation/store"
)

// One investigation, end to end.
//
// The order below is the whole of the design and every step in it is load-bearing:
//
//	normalise      one symptom shape, whichever door it came through   (FR-002, FR-002a)
//	resolve        the subject, through the published resolution path  (FR-003)
//	group          against the incidents already open                  (FR-008a, FR-008c)
//	open           idempotent on the published key                     (FR-008b)
//	consult        the graph's extent over the window                  (FR-032)
//	publish        the prior-only ranking, labelled untested           (FR-046a)
//	run            the engine, to a typed stop                         (FR-045b)
//	persist        ledger, evidence, calls, spend, trajectory          (FR-021, FR-033, FR-042a)
//	record         exactly one decision record                         (FR-035)
//	conclude       the terminal state, guarded                         (FR-006, FR-007)
//
// Two orderings inside that are not obvious and are deliberate.
//
// **The decision record is emitted before the conclusion, not after.** A concluded row is
// immutable (FR-007) and 0008 enforces that with a trigger, so the link-back that names the
// decision event has to happen while the row is still `running`. Emitting first also means the
// conclusion is the *last* thing that happens: a run that crashed between the two leaves a
// `running` row with an event, which a reader can finish, rather than a `concluded` row with no
// provenance, which nobody can.
//
// **The ledger is saved before the worker calls.** `worker_calls.evidence_id` references
// `evidence_items`, and the ledger's save is what writes those.

// emitFunc is the anytime callback the service hands in. A nil one is legal: `investigate`
// without `--watch` wants the answer and not the working.
//
// It is an alias rather than a defined type so that the methods below satisfy `server.Runner`
// verbatim: a defined function type is not identical to its underlying one, and an interface is
// matched on identity.
type emitFunc = func(*investigationv1.Investigation) error

// Investigate runs one investigation to a terminal state (FR-064).
func (r *Runner) Investigate(
	ctx context.Context,
	req *investigationv1.InvestigateRequest,
	requester string,
	emit emitFunc,
) (*investigationv1.Investigation, error) {
	if req == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("investigate: %w", errNoRequest))
	}
	if requester == "" {
		return nil, investigationstore.ErrAnonymousPrincipal
	}
	in, err := r.intakeOf(req, requester)
	if err != nil {
		return nil, err
	}
	return r.start(ctx, startInput{
		intake:    in,
		requester: requester,
		profile:   req.GetProfile(),
		priority:  req.GetSymptom().GetSeverity(),
	}, emit)
}

// Declare opens an investigation from a human declaration (FR-001a).
//
// It is the same pipeline from the other door. Re-delivery under a seen key returns the existing
// investigation without running a second time (FR-008b), and a declaration from which no target
// can be read reaches `unknown` carrying "name the affected service(s)" — the engine does not
// invent one (FR-002b).
func (r *Runner) Declare(
	ctx context.Context,
	req *investigationv1.DeclareRequest,
	requester string,
	emit emitFunc,
) (*investigationv1.Investigation, error) {
	if req == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("declare: %w", errNoRequest))
	}
	if requester == "" {
		return nil, investigationstore.ErrAnonymousPrincipal
	}
	in, err := r.intakeOfDeclaration(req, requester)
	if err != nil {
		return nil, err
	}
	return r.start(ctx, startInput{
		intake:    in,
		requester: requester,
		profile:   req.GetProfile(),
		priority:  req.GetDeclaration().GetSeverity(),
	}, emit)
}

// startInput is one arriving symptom and what the caller asked for.
type startInput struct {
	intake    *intake.Intake
	requester string
	profile   string
	priority  string
}

// run is one opened investigation, ready to reason.
type run struct {
	investigationID string
	incidentID      string
	intake          *intake.Intake
	resolution      intake.ResolutionSet
	grouping        *intake.GroupingDecision
	profile         budget.Profile
	requester       string
	// subjectRef is the reference the engine investigates, as `namespace=value`. Empty means the
	// intake named nothing resolvable, which is the FR-002b/FR-026 path and not an error.
	subjectRef string
	subjects   []*graphv1.Ref
	startedAt  time.Time
	// fact is the human fact that reopened a concluded investigation, where there was one.
	fact *investigationv1.HumanFact
	// spend is the run's consumption, rendered once at persistence so that the spend row and the
	// decision record quote one figure (FR-044).
	spend *investigationv1.BudgetSpend
	// book is the engine's one belief state, held here so the anytime stream can render it
	// without the engine having to hand it back on every snapshot.
	book *ledger.Ledger
}

// start resolves, groups, opens and then runs.
func (r *Runner) start(ctx context.Context, in startInput, emit emitFunc) (*investigationv1.Investigation, error) {
	// FR-008b, checked before anything is written: a symptom whose published key has been seen
	// belongs to an investigation that already exists, and re-delivery is a no-op that returns
	// it. Doing this first means a re-delivered alert costs one indexed lookup rather than a
	// resolution audit and a grouping decision.
	if existing, found, err := r.dao.InvestigationForKey(ctx, in.intake.IdempotencyKey()); err != nil {
		return nil, err
	} else if found {
		return r.dao.Get(ctx, existing)
	}

	set, err := intake.Resolve(ctx, r.cfg.Graph, in.intake)
	if err != nil {
		return nil, err
	}

	rule := r.cfg.GroupingRule
	open, err := r.dao.OpenIncidents(ctx, in.intake.ValidAt, groupingWindow(rule))
	if err != nil {
		return nil, err
	}
	decision, err := intake.Group(ctx, rule, in.intake, set.EntityIDs, open, r.cfg.Neighbourhood)
	if err != nil {
		return nil, err
	}
	in.intake.Symptom.GroupingEvidenceId = intake.AssociationEvidenceID(in.intake.Symptom.GetSymptomId())

	// FR-008a: one investigation covers one incident. A symptom that attaches to an incident
	// already under investigation joins it rather than starting a second run — a run costs money
	// and a second one about the same outage produces two answers to one question.
	if decision.Attached && decision.InvestigationID != "" {
		if _, err := r.dao.Attach(ctx, decision.IncidentID, in.intake.Symptom); err != nil {
			return nil, err
		}
		return r.dao.Get(ctx, decision.InvestigationID)
	}

	profile := r.profileFor(in.profile, in.priority)
	rn := &run{
		investigationID: investigationIDFor(in.intake.IdempotencyKey()),
		incidentID:      decision.IncidentID,
		intake:          in.intake,
		resolution:      set,
		grouping:        &decision,
		profile:         profile,
		requester:       in.requester,
		subjectRef:      subjectRefOf(set),
		subjects:        subjectsOf(in.intake, set),
		startedAt:       r.now(),
	}

	incident := investigationstore.Incident{
		IncidentID:             decision.IncidentID,
		CanonicalSubjectID:     firstEntityID(set),
		OpenedAt:               in.intake.ValidAt,
		LastSymptomAt:          in.intake.ValidAt,
		AssociationRuleVersion: decision.Rule.Version,
	}
	opened, err := r.dao.Open(ctx, incident, r.newInvestigation(rn, ""), in.intake.Symptom)
	if err != nil {
		return nil, err
	}
	if opened != rn.investigationID {
		// The key was seen between the lookup above and the insert. The published answer is the
		// investigation that already covers it, unrun (FR-008b).
		return r.dao.Get(ctx, opened)
	}
	return r.execute(ctx, rn, emit)
}

// newInvestigation is the row one run opens.
func (r *Runner) newInvestigation(rn *run, reopens string) investigationstore.NewInvestigation {
	return investigationstore.NewInvestigation{
		InvestigationID:        rn.investigationID,
		IncidentID:             rn.incidentID,
		ReopensInvestigationID: reopens,
		ValidAt:                rn.intake.ValidAt,
		ObservedAt:             rn.intake.ObservedAt,
		ReviewMode:             rn.intake.ReviewMode,
		WindowStart:            rn.intake.Window.GetStart().AsTime(),
		WindowEnd:              rn.intake.Window.GetEnd().AsTime(),
		Profile:                rn.profile.Name,
		Requester:              rn.requester,
		ModelConfig:            r.modelConfig,
		AlgebraVersion:         engine.AlgebraVersion,
		LedgerRuleVersion:      ledgerRuleVersion,
		SchemaVersion:          SchemaVersion,
		StartedAt:              rn.startedAt,
	}
}

// execute reasons over an opened investigation and leaves it terminal.
//
// Every exit from here writes a terminal state. That is the property that makes "a `running` row
// is a run in flight" true: a panic would break it, a returned error does not.
func (r *Runner) execute(ctx context.Context, rn *run, emit emitFunc) (*investigationv1.Investigation, error) {
	// The intake named nothing the graph could resolve. The investigation still opened — dropping
	// it would lose the incident — and it concludes `unknown` carrying the concrete thing a
	// person can do about it (FR-002b, FR-003, FR-026).
	if rn.subjectRef == "" {
		return r.concludeUnresolved(ctx, rn)
	}

	extent, err := intake.ConsultExtent(ctx, r.cfg.Graph, rn.intake)
	if err != nil {
		return nil, r.failed(ctx, rn.investigationID, fmt.Sprintf("extent consultation: %v", err), err)
	}

	manager, err := budget.New(rn.profile, r.cfg.OperatorCaps, r.clock)
	if err != nil {
		return nil, r.failed(ctx, rn.investigationID, fmt.Sprintf("budget: %v", err), err)
	}
	eng, err := engine.New(engine.Config{
		InvestigationID: rn.investigationID,
		Subject: engine.Subject{
			EntityRef: rn.subjectRef,
			Statement: rn.intake.Symptom.GetStatement(),
			FiredAt:   rn.intake.ValidAt,
			Window:    rn.intake.Window,
			Origin:    originOf(rn.intake),
			Priority:  rn.intake.Symptom.GetSeverity(),
		},
		Instants: engine.Instants{ValidAt: rn.intake.ValidAt, ObservedAt: rn.intake.ObservedAt},
		Prior:    r.cfg.Prior,
		Workers:  r.workers,
		Client:   r.cfg.Model,
		Budget:   manager,
		Mode:     r.cfg.Mode,
		Links:    r.cfg.Links,
		Clock:    r.clock,
		MaxTurns: r.cfg.MaxTurns,
		// The anytime stream (FR-046a). The engine owns the phase order, so the engine is what
		// says when a phase ended; this turns each of its snapshots into the same
		// `Investigation` view the final answer is rendered from, so a subscriber reads one
		// shape throughout rather than a provisional shape and then a different final one.
		OnState: r.streamState(ctx, emit, rn),
	})
	if err != nil {
		return nil, r.failed(ctx, rn.investigationID, fmt.Sprintf("engine: %v", err), err)
	}
	book := eng.Ledger()
	rn.book = book

	// The intake's own findings are evidence like any other: the resolution audit that justifies
	// the subject (FR-003), the grouping decision with the distances it rested on (FR-008a), and
	// the extent consultation over the window (FR-032).
	for _, item := range rn.resolution.Evidence {
		if err := book.AddEvidence(item); err != nil {
			return nil, r.failed(ctx, rn.investigationID, fmt.Sprintf("resolution evidence: %v", err), err)
		}
	}
	if rn.grouping != nil {
		grouping := rn.grouping.Evidence(rn.intake.ValidAt, rn.intake.ObservedAt, rn.intake.ObservedAt)
		if err := book.AddEvidence(grouping); err != nil {
			return nil, r.failed(ctx, rn.investigationID, fmt.Sprintf("grouping evidence: %v", err), err)
		}
	}
	if extent.Evidence.ID != "" {
		// `evidence_id` is the primary key of the whole table, and intake names the extent
		// consultation after the *symptom*. A reopen inherits its parent's symptom, so two runs
		// would mint one id and the child's row would silently collide with the parent's. Scoping
		// it to the run is what keeps "one evidence item, one call" true across a reopen.
		item := extent.Evidence
		item.ID += ":" + rn.investigationID
		if err := book.AddEvidence(item); err != nil {
			return nil, r.failed(ctx, rn.investigationID, fmt.Sprintf("extent evidence: %v", err), err)
		}
	}
	// A fact that reopened a concluded run enters the child's ledger as evidence, weighted
	// `strong` and never decisive: where telemetry contradicts a person, both are kept (FR-057a).
	if rn.fact != nil {
		item := investigationstore.FactEvidence(rn.fact, rn.intake.ValidAt, rn.intake.ObservedAt)
		if err := book.AddEvidence(item); err != nil {
			return nil, r.failed(ctx, rn.investigationID, fmt.Sprintf("fact evidence: %v", err), err)
		}
	}

	// FR-046a: the graph's own ranking, published as an explicitly untested answer before any
	// telemetry has been asked for. `Run` republishes it — the ledger is unchanged by the second
	// pass, the two graph reads are cheap and are recorded as the separate calls they are
	// (Invariant 5) — which is the price of the engine owning its own phase order.
	if _, err := eng.Publish(ctx); err != nil {
		return nil, r.failed(ctx, rn.investigationID, fmt.Sprintf("provisional ranking: %v", err), err)
	}
	if _, err := intake.FlagGapAffected(book, extent); err != nil {
		return nil, r.failed(ctx, rn.investigationID, fmt.Sprintf("gap flagging: %v", err), err)
	}
	if err := r.attachFact(eng, rn); err != nil {
		return nil, r.failed(ctx, rn.investigationID, fmt.Sprintf("human fact: %v", err), err)
	}
	if err := r.emitState(ctx, emit, rn, book); err != nil {
		return nil, err
	}

	stop, runErr := eng.Run(ctx)
	if runErr != nil {
		// A run that failed is still a run: everything it established is persisted, and the row
		// ends `failed` with the typed detail rather than dangling `running`.
		r.persistQuietly(ctx, rn, eng)
		return nil, r.failed(ctx, rn.investigationID, stop.Detail, runErr)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		// The caller went away — but the engine returned a terminal state before it did, so the
		// investigation is *finished* and the only question left is whether the record of it
		// survives. It does: concluding is a handful of writes over work already paid for, and
		// they run on a context detached from the caller's (persist.go, detachedWrite).
		//
		// Failing here instead, which is what this used to do, was worse than losing the
		// answer: a terminal row is immutable (FR-007), so `failed` for a question that was in
		// fact answered is permanent, and the next identical question is a no-op onto that row.
		r.logger.Warn("investigation runner: the caller went away after the engine reached a "+
			"terminal state; the conclusion is recorded on a detached context",
			"investigation_id", rn.investigationID, "error", ctxErr.Error())
	}

	return r.conclude(ctx, rn, eng, stop, emit)
}

// attachFact ties the reopening fact to the hypotheses it concerns (FR-057a).
//
// The judgment it records is **neutral**, and that is the point rather than a placeholder. A
// person saying "I restarted that pod by hand at 14:20" is evidence about a candidate, but which
// way it points is an inference, and the engine does not make one on somebody's behalf: the fact
// is attached, weighted `strong` and never decisive, contributing a log-likelihood ratio of zero
// until an investigator — the model, or a reviewer — says which direction it runs in. What the
// attachment buys is that the fact is *visible* on the hypotheses it bears on, rather than sitting
// in the evidence chain where a reader has to notice it.
//
// A fact naming no entity the ledger knows attaches to nothing and stays in the evidence chain.
// Attaching it to the open hypothesis instead would be a claim that a person's note bears on "no
// observed change explains this", which it does not.
func (r *Runner) attachFact(eng *engine.Engine, rn *run) error {
	if rn.fact == nil {
		return nil
	}
	eng.Trajectory().HumanFact(rn.fact)
	book := eng.Ledger()
	rn.book = book
	concerns := make(map[string]bool, len(rn.fact.GetEntityIds()))
	for _, id := range rn.fact.GetEntityIds() {
		concerns[id] = true
	}
	if len(concerns) == 0 {
		return nil
	}
	for _, h := range book.Hypotheses() {
		if !factConcerns(h, concerns) {
			continue
		}
		judgment := investigationstore.FactJudgment(
			"j-fact-"+h.ID, h.ID, rn.fact, ledger.Neutral)
		if _, err := book.Judge(judgment); err != nil {
			return err
		}
	}
	return nil
}

func factConcerns(h ledger.Hypothesis, concerns map[string]bool) bool {
	if h.CandidateChangeEntityID != "" && concerns[h.CandidateChangeEntityID] {
		return true
	}
	for _, id := range h.TargetEntityIDs {
		if concerns[id] {
			return true
		}
	}
	return false
}

// conclude persists everything the run produced and moves the row to its terminal state.
func (r *Runner) conclude(
	ctx context.Context,
	rn *run,
	eng *engine.Engine,
	stop engine.Stop,
	emit emitFunc,
) (*investigationv1.Investigation, error) {
	book := eng.Ledger()
	rn.book = book
	// Every write below is detached from the caller and bounded (persist.go, detachedWrite):
	// after this point the run has an answer, and the caller's patience is not what decides
	// whether it is recorded.
	writeCtx, cancel := detachedWrite(ctx)
	defer cancel()
	if err := r.persist(writeCtx, rn, eng); err != nil {
		return nil, r.failed(writeCtx, rn.investigationID, fmt.Sprintf("persist: %v", err), err)
	}

	verdict, rollback, ranked := verdictOf(book)
	conclusion := investigationstore.Conclusion{
		Kind:                    conclusionKindOf(stop),
		Outcome:                 investigationstore.OutcomeName(stop.Outcome(ranked)),
		StopReason:              stop.Reason.Proto(),
		StopDetail:              stop.Detail,
		VerdictLine:             verdict,
		RollbackCandidate:       rollback,
		EndedAt:                 r.now(),
		OnsetEstimateEvidenceID: onsetEvidenceOf(eng),
	}
	written := r.writeTrajectory(rn.investigationID, eng)
	conclusion.RecordingKey = written.key
	conclusion.RecordingDigest = written.digest

	// The decision record goes first: it updates the row, and after the conclusion the row is
	// immutable (FR-007, 0008's trigger).
	eventID, err := r.emitDecisionRecord(writeCtx, rn, eng, conclusion)
	if err != nil {
		return nil, r.failed(writeCtx, rn.investigationID, fmt.Sprintf("decision record: %v", err), err)
	}
	conclusion.DecisionEventID = eventID

	if err := r.lifecycle.Conclude(writeCtx, rn.investigationID, conclusion); err != nil {
		return nil, err
	}
	final, err := r.view(writeCtx, rn.investigationID, book, r.resolutionsFor(rn, book)...)
	if err != nil {
		return nil, err
	}
	if emit != nil {
		if err := emit(final); err != nil {
			// A caller that has hung up cannot be sent the answer, and saying so is not a
			// failure of the investigation: the row is already concluded and immutable.
			if ctx.Err() == nil {
				return nil, err
			}
			r.logger.Warn("investigation runner: could not publish the concluded state to a "+
				"caller that went away", "investigation_id", rn.investigationID, "error", err.Error())
		}
	}
	return final, nil
}

// concludeUnresolved is the FR-002b/FR-003 path: the investigation opened, nothing resolvable was
// named, and the honest terminal state is `unknown` with the concrete thing a person can do.
//
// It is a **final** conclusion, not a partial one: nothing was cut short. The investigation asked
// the only question it could — "what is this about?" — and the answer is that nobody said.
func (r *Runner) concludeUnresolved(ctx context.Context, rn *run) (*investigationv1.Investigation, error) {
	book, err := ledger.New(rn.investigationID, r.cfg.Prior)
	if err != nil {
		return nil, r.failed(ctx, rn.investigationID, fmt.Sprintf("ledger: %v", err), err)
	}
	for _, item := range rn.resolution.Evidence {
		if err := book.AddEvidence(item); err != nil {
			return nil, r.failed(ctx, rn.investigationID, fmt.Sprintf("resolution evidence: %v", err), err)
		}
	}
	if rn.grouping != nil {
		if err := book.AddEvidence(rn.grouping.Evidence(rn.intake.ValidAt, rn.intake.ObservedAt,
			rn.intake.ObservedAt)); err != nil {
			return nil, r.failed(ctx, rn.investigationID, fmt.Sprintf("grouping evidence: %v", err), err)
		}
	}
	if err := r.ledgerDAO.Save(ctx, book); err != nil {
		return nil, r.failed(ctx, rn.investigationID, fmt.Sprintf("save ledger: %v", err), err)
	}

	stop := engine.Stop{
		Reason: engine.StopCompleted,
		Detail: "the intake named no entity the graph could resolve; the engine will not guess one (FR-002b, FR-003)",
	}
	conclusion := investigationstore.Conclusion{
		Kind:        investigationstore.ConclusionFinal,
		Outcome:     investigationstore.OutcomeUnknown,
		StopReason:  stop.Reason.Proto(),
		StopDetail:  stop.Detail,
		VerdictLine: unresolvedVerdict(rn),
		EndedAt:     r.now(),
	}
	eventID, err := r.emitDecisionRecordFrom(ctx, rn, book, nil, conclusion)
	if err != nil {
		return nil, r.failed(ctx, rn.investigationID, fmt.Sprintf("decision record: %v", err), err)
	}
	conclusion.DecisionEventID = eventID
	if err := r.lifecycle.Conclude(ctx, rn.investigationID, conclusion); err != nil {
		return nil, err
	}
	return r.view(ctx, rn.investigationID, book, r.resolutionsFor(rn, book)...)
}

// unresolvedVerdict is the one line an on-call reads when nothing was named.
func unresolvedVerdict(rn *run) string {
	if rn.intake.UnresolvedTargets {
		return intake.MissingTargetResolution + ": the intake named none, and the engine does not " +
			"infer a service from a title (FR-002b); there is no rollback candidate"
	}
	names := make([]string, 0, len(rn.resolution.Unresolved))
	for _, ref := range rn.resolution.Unresolved {
		names = append(names, ref.String())
	}
	return fmt.Sprintf("the subject could not be resolved (%s); confirm the identity before "+
		"re-running (FR-003); there is no rollback candidate", strings.Join(names, ", "))
}

// resolutionsFor is "what would resolve this" (FR-026, FR-045): the ledger's untested hypotheses
// with the query that would test them, plus the requests intake produced that the ledger cannot
// know about.
func (r *Runner) resolutionsFor(rn *run, book *ledger.Ledger) []*investigationv1.Resolution {
	var extra []*investigationv1.Resolution
	if rn.intake.UnresolvedTargets {
		extra = append(extra, intake.MissingTargetRequest(rn.intake))
	}
	for _, ref := range rn.resolution.Unresolved {
		extra = append(extra, intake.UnresolvedSubjectRequest(ref))
	}
	for _, res := range rn.resolution.Resolutions {
		if len(res.LaterMerges) > 0 {
			extra = append(extra, &investigationv1.Resolution{
				Statement: res.Note,
				Kind:      intake.ResolutionKindConfirmIdentity,
			})
		}
	}
	return render.ResolutionsFor(book, extra...)
}

// streamState turns the engine's per-phase snapshots into anytime emissions (FR-046a).
//
// It returns nil when there is nobody to emit to, so a run with no subscriber does no work per
// turn rather than rendering a view nothing reads.
//
// A failed emission is logged and does not fail the investigation. The subscriber is a
// convenience — the conclusion is persisted either way and is readable afterwards — and an
// incident abandoned because a websocket closed would be the stream costing more than it is
// worth.
func (r *Runner) streamState(ctx context.Context, emit emitFunc, rn *run) func(*engine.Snapshot) {
	if emit == nil {
		return nil
	}
	return func(snapshot *engine.Snapshot) {
		if ctx.Err() != nil {
			return
		}
		if err := r.emitState(ctx, emit, rn, rn.book); err != nil {
			r.logger.Warn("investigation runner: could not publish an intermediate state",
				"investigation_id", rn.investigationID, "phase", string(snapshot.Phase),
				"turn", snapshot.Turn, "error", err.Error())
		}
	}
}

// emitState publishes an intermediate state through the anytime callback (FR-046a).
func (r *Runner) emitState(ctx context.Context, emit emitFunc, rn *run, book *ledger.Ledger) error {
	if emit == nil {
		return nil
	}
	state, err := r.view(ctx, rn.investigationID, book, r.resolutionsFor(rn, book)...)
	if err != nil {
		return err
	}
	return emit(state)
}

// view reads the row back and renders it as the one structure both renderings project from
// (FR-064).
func (r *Runner) view(
	ctx context.Context,
	investigationID string,
	book *ledger.Ledger,
	resolutions ...*investigationv1.Resolution,
) (*investigationv1.Investigation, error) {
	inv, err := r.dao.Get(ctx, investigationID)
	if err != nil {
		return nil, err
	}
	report := render.NewReport(inv, book)
	report.Resolutions = resolutions
	return report.Machine(), nil
}

// failed moves the row to `failed` and returns the error the caller should see.
//
// The lifecycle write uses a context detached from the caller's, because the commonest reason to
// be here is that the caller's context went away — and a cancelled context cannot write the row
// that says the run was cancelled.
func (r *Runner) failed(ctx context.Context, investigationID, detail string, cause error) error {
	if detail == "" {
		detail = "the engine failed and recorded no detail"
	}
	writeCtx, cancel := detachedWrite(ctx)
	defer cancel()
	if err := r.lifecycle.Fail(writeCtx, investigationID, detail); err != nil &&
		!errors.Is(err, investigationstore.ErrNotRunning) {
		r.logger.Error("investigation runner: could not record the failure",
			"investigation_id", investigationID, "error", err.Error())
	}
	if cause == nil {
		return errors.New(detail)
	}
	return fmt.Errorf("investigation %s failed: %s: %w", investigationID, detail, cause)
}

// --- derivations -----------------------------------------------------------------------------

// verdictOf is the one line an on-call acts on, the change that could be rolled back, and
// whether the answer is a ranked one at all (FR-045b, FR-057c).
//
// Naming a rollback candidate is not proposing a remediation (FR-028): it names the change the
// evidence points at and stops there.
func verdictOf(book *ledger.Ledger) (verdict, rollback string, ranked bool) {
	hypotheses := book.Hypotheses()
	if len(hypotheses) == 0 {
		return "no hypotheses were formed; there is no rollback candidate", "", false
	}
	top := hypotheses[0]
	if top.Kind != ledger.KindNoObservedChange && top.Status != ledger.StatusExonerated {
		for _, j := range book.JudgmentsFor(top.ID) {
			if j.Direction == ledger.Supports {
				ranked = true
				break
			}
		}
	}
	if ranked {
		rollback = top.CandidateChangeEntityID
	}
	report := render.NewReport(&investigationv1.Investigation{}, book)
	return report.VerdictLine(), rollback, ranked
}

// conclusionKindOf maps a typed stop onto the conclusion kind (FR-006).
//
// `completed` and `diminishing_returns` are final: the first tested what there was to test, and
// the second is the control law saying there is nothing left worth asking. Everything else cut
// the run short and is partial, whatever it found.
func conclusionKindOf(stop engine.Stop) string {
	switch stop.Reason {
	case engine.StopCompleted, engine.StopDiminishingReturns:
		return investigationstore.ConclusionFinal
	case engine.StopBudgetExhausted, engine.StopWorkerUnavailable, engine.StopRefused, engine.StopFailed:
		return investigationstore.ConclusionPartial
	default:
		return investigationstore.ConclusionPartial
	}
}

func onsetEvidenceOf(eng *engine.Engine) string {
	if onset := eng.Onset(); onset != nil {
		return onset.EvidenceID
	}
	return ""
}

// subjectRefOf is the reference the engine investigates.
//
// It is the identifier the alert named, not the canonical entity id it resolved to: the graph's
// own reads take a `namespace=value` reference, and the canonical id is an opaque hash. What
// resolution buys is the *audit* — recorded as evidence, reported alongside the given identifier
// (FR-003) — and the entity ids the association rule groups on; it is not a substitute reference.
func subjectRefOf(set intake.ResolutionSet) string {
	for _, res := range set.Resolutions {
		if res.Resolved {
			return res.Given.String()
		}
	}
	return ""
}

func subjectsOf(in *intake.Intake, set intake.ResolutionSet) []*graphv1.Ref {
	out := make([]*graphv1.Ref, 0, len(set.Resolutions))
	for _, res := range set.Resolutions {
		out = append(out, res.Given.Proto())
	}
	if len(out) > 0 {
		return out
	}
	// A declaration with no target is still about the place the incident lives, and the decision
	// record has to be about something. Saying "this investigation was about that channel" is
	// true; inventing a service would not be (FR-002b).
	if ref, err := graph.ParseRef(in.Symptom.GetOriginRef()); err == nil {
		return []*graphv1.Ref{ref.Proto()}
	}
	return []*graphv1.Ref{intake.PlaceRef(placeValueOf(in)).Proto()}
}

func placeValueOf(in *intake.Intake) string {
	if ref := strings.TrimSpace(in.Symptom.GetOriginRef()); ref != "" {
		return ref
	}
	return in.Symptom.GetSymptomId()
}

func firstEntityID(set intake.ResolutionSet) string {
	if len(set.EntityIDs) == 0 {
		return ""
	}
	return set.EntityIDs[0]
}

func originOf(in *intake.Intake) string {
	system := in.Symptom.GetOriginSystem()
	ref := in.Symptom.GetOriginRef()
	switch {
	case system != "" && ref != "":
		return system + ":" + ref
	case system != "":
		return system
	default:
		return ref
	}
}

func groupingWindow(rule intake.GroupingRule) time.Duration {
	if rule.Window > 0 {
		return rule.Window
	}
	return intake.DefaultGroupingWindow
}

// investigationIDFor derives a run's identifier from the symptom's published idempotency key.
//
// Deriving it rather than minting a random one means re-delivery through a different path lands
// on the same id even when two processes race: the insert conflicts on the primary key rather
// than opening a second row for one incident (FR-008b).
func investigationIDFor(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "inv-" + hex.EncodeToString(sum[:])[:16]
}

func instantOf(primary, fallback *timestamppb.Timestamp) time.Time {
	if primary != nil {
		return primary.AsTime().UTC()
	}
	if fallback != nil {
		return fallback.AsTime().UTC()
	}
	return time.Time{}
}

func timestampOf(at time.Time) *timestamppb.Timestamp { return timestamppb.New(at.UTC()) }
