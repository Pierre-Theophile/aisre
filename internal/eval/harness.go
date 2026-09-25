// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/fixture"
	"github.com/Pierre-Theophile/aisre/internal/investigation/audit"
	backend "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/internal/investigation/budget"
	"github.com/Pierre-Theophile/aisre/internal/investigation/engine"
	"github.com/Pierre-Theophile/aisre/internal/investigation/ledger"
	"github.com/Pierre-Theophile/aisre/internal/investigation/model"
	"github.com/Pierre-Theophile/aisre/internal/investigation/model/modeltest"
	"github.com/Pierre-Theophile/aisre/internal/investigation/replay"
	graphworker "github.com/Pierre-Theophile/aisre/internal/investigation/workers/graph"
	"github.com/Pierre-Theophile/aisre/internal/investigation/workers/logs"
	"github.com/Pierre-Theophile/aisre/internal/investigation/workers/metrics"
	"github.com/Pierre-Theophile/aisre/internal/investigation/workers/traces"
	"github.com/Pierre-Theophile/aisre/internal/projector"
	"github.com/Pierre-Theophile/aisre/internal/query"
	sdk "github.com/Pierre-Theophile/aisre/pkg/backend"
	"github.com/Pierre-Theophile/aisre/pkg/worker"
)

// The harness (T107, T111; FR-042a, FR-061, FR-063, contracts/incident-format.md).
//
// A fixture is investigated against exactly two sources — the graph, replayed from its own
// `events.jsonl` into a database of its own, and the telemetry, answered from its recorded
// `world/`. Nothing else is consulted and nothing reaches a network.
//
// **The run is deterministic by construction**, so that re-recording an unchanged fixture writes
// the same bytes to the same path and a re-record is a `git status` a reviewer can read:
//
//   - the clock is **constant**, not merely injected. The first wave issues its calls in
//     parallel, so a clock that advanced on every read would advance in whatever order the
//     scheduler chose. A constant clock makes every recorded instant a function of the fixture.
//     Elapsed wall time is then zero everywhere, which is right for a recording: a replayed run
//     took no time in the world it is a recording of;
//   - the run id is derived from the trajectory's own digest (`replay.RunID`), so the file name
//     is a function of the content;
//   - `duration_ms` is cleared from every recorded answer by `replay.Normalize`, which is the
//     rule `pkg/backend.ResponseDigest` already publishes.
//
// Two wirings exist and there is exactly **one** builder for both, which is the point of
// `EngineWiring`. `fixture record-trajectory --model-free` runs the deterministic engine against
// the **recorded world** and writes what it did; `fixture record-world` runs the *same* engine
// against the **synthetic backend** and records every term it asks for. If those two wirings
// could differ — a different lookback, a different budget profile, a different set of registered
// workers — then the world would be a recording of an investigation the corpus never replays,
// which is exactly the failure Phase 7 found: a miss rate of 1.0000 with no single line to blame
// it on.

// The two recordings the corpus ships, named once.
const (
	// KindModelFree is the deterministic engine end to end: no model record at all.
	KindModelFree = "model-free"
	// KindFakeModel adds two canned turns through the real transport seam, so the
	// model-request digest matching path the gate is built on is exercised by the corpus rather
	// than only by a unit test.
	KindFakeModel = "fake-model"
	// KindLive calls the configured providers for real. It is the only kind that costs money and
	// the only one that produces the *production* recording: a trajectory whose model exchanges
	// are real turns, which is what FR-061 means by "the configuration used in production".
	//
	// It is never the default, and a run that cannot resolve a credential falls back to
	// model-free and says which variable was missing rather than failing mid-corpus.
	KindLive = "live"
)

// DefaultAuditPath is the published coverage audit π₀ is read from (ADR-0005 D9).
const DefaultAuditPath = "docs/evaluation/coverage-audit-2026-09.json"

// Options is how a harness is asked for.
type Options struct {
	// Dir is the incident fixture directory.
	Dir string
	// DSN is a PostgreSQL DSN whose role has CREATEDB. It is used to build NewStore when
	// NewStore is nil.
	DSN string
	// NewStore builds the disposable database the fixture's events are replayed into. Nil
	// means "derive one from DSN", which is what every command does; a test supplies its own.
	NewStore fixture.StoreFactory
	// AuditPath is the published coverage audit π₀ is read from. Empty means no audit, which
	// is total ignorance and is legal: a fixture investigated before an audit exists is still
	// an investigation.
	AuditPath string
	// Prior overrides AuditPath entirely, for a caller that has already loaded one.
	Prior *audit.PriorRecord
	// Kind is KindModelFree or KindFakeModel. Empty means KindModelFree.
	Kind string
	// ModelYAML and PricesYAML are the model configuration and price table. They are read for
	// the configuration digest FR-061 requires on every outcome, and for the fake-model
	// client. Empty means the published defaults.
	ModelYAML  string
	PricesYAML string
	// Client is an explicit model boundary: a live client, or a fake one a test built. It wins
	// over Kind, so a caller that has already built a transport does not have to describe it
	// again.
	Client *model.Client
	// Mode is what every call records itself as. Zero means recorded.
	Mode worker.Mode
	// InvestigationID overrides the derived id. It is derived from the fixture and the kind by
	// default, and **not** from the run index, so that repeated runs of a deterministic engine
	// produce the same trajectory digest and a divergence means what it says.
	InvestigationID string
	// Graph replaces the replayed graph, for a caller that already has a query service. When
	// it is set no database is opened.
	Graph graphworker.QueryService
	// Telemetry replaces the recorded world, for `fixture record-world`, which runs the same
	// engine against the world's own generator.
	Telemetry sdk.TelemetryBackend
}

// Harness is one fixture, wired and ready to investigate. It is reusable: Run may be called
// repeatedly, and each call builds a fresh engine and a fresh view of the recorded world so that
// one run's miss counters are not another's.
type Harness struct {
	opts     Options
	manifest *fixture.Manifest
	prior    audit.PriorRecord
	graph    graphworker.QueryService
	world    *sdk.World
	cleanup  func()

	modelDigest  string
	modelVersion string

	runs int
}

// NewHarness replays the fixture into a database of its own and loads its recorded world.
//
// The caller owns the result and must Close it; a harness that opened a database and was
// dropped would leave it behind for the next run to collide with.
func NewHarness(ctx context.Context, opts Options) (*Harness, error) {
	if strings.TrimSpace(opts.Dir) == "" {
		return nil, errors.New("eval: no fixture directory to investigate")
	}
	m, err := fixture.LoadManifest(opts.Dir)
	if err != nil {
		return nil, err
	}
	if !m.IsIncident() {
		return nil, fmt.Errorf(
			"eval: %s has no `incident:` block; there is no investigation to run", opts.Dir)
	}

	h := &Harness{opts: opts, manifest: m, cleanup: func() {}}
	if h.opts.Kind == "" {
		h.opts.Kind = KindModelFree
	}

	switch {
	case opts.Prior != nil:
		h.prior = *opts.Prior
	default:
		if h.prior, err = LoadPrior(opts.AuditPath); err != nil {
			return nil, err
		}
	}

	if h.modelDigest, h.modelVersion, err = modelConfiguration(opts.ModelYAML); err != nil {
		return nil, err
	}

	if opts.Graph != nil {
		h.graph = opts.Graph
	} else if err := h.openGraph(ctx); err != nil {
		return nil, err
	}

	if opts.Telemetry == nil {
		world, err := sdk.LoadWorld(m.WorldDir())
		if err != nil {
			h.Close()
			return nil, fmt.Errorf("eval: %s: load the recorded world: %w", m.ID, err)
		}
		h.world = world
	}
	return h, nil
}

// openGraph creates the disposable database and replays the fixture's own events into it.
func (h *Harness) openGraph(ctx context.Context) error {
	newStore := h.opts.NewStore
	if newStore == nil {
		if strings.TrimSpace(h.opts.DSN) == "" {
			return errors.New(
				"eval: no database to replay the fixture's graph into; supply a DSN whose role has CREATEDB, or a store factory")
		}
		newStore = fixture.NewStoreFactoryFromDSN(h.opts.DSN)
	}
	store, cleanup, err := newStore(ctx)
	if err != nil {
		return err
	}
	h.cleanup = cleanup
	if _, err := fixture.Load(ctx, projector.New(store), h.manifest.Dir, fixture.LoadOptions{}); err != nil {
		h.Close()
		return err
	}
	// No wrapper around the query service. There used to be one — it supplied the `as_of` a
	// pointers read arrived without and rewrote a diff's canonical target ids into references —
	// and both were shims over engine defects that Phase 7 Track F fixed at the source:
	// `engine.DecodeCall` now stamps both instants on every graph read, and the engine learns the
	// id ↔ reference translation from the neighbourhood it reads first (engine/catalogue.go).
	h.graph = graphworker.NewEngineService(query.NewEngine(store))
	return nil
}

// Close drops the disposable database. It is safe to call more than once.
func (h *Harness) Close() {
	if h == nil || h.cleanup == nil {
		return
	}
	cleanup := h.cleanup
	h.cleanup = nil
	cleanup()
}

// Manifest is the fixture the harness was built for.
func (h *Harness) Manifest() *fixture.Manifest { return h.manifest }

// Prior is π₀ as the harness loaded it.
func (h *Harness) Prior() audit.PriorRecord { return h.prior }

// Graph is the read surface the graph worker is bound to.
func (h *Harness) Graph() graphworker.QueryService { return h.graph }

// Run investigates the fixture's question once and gathers everything a scorer needs.
//
// It never returns a partial outcome with a nil error: a run that failed is a `failed` stop with
// the reason in it, which is an outcome, and a run that could not be *built* is an error.
func (h *Harness) Run(ctx context.Context) (*RunOutcome, error) {
	h.runs++
	telemetry := h.opts.Telemetry
	var recorded *backend.Recorded
	if telemetry == nil {
		// A fresh view per run, so that one run's `not_recorded` count is that run's.
		recorded = backend.NewRecordedFromWorld(h.world)
		telemetry = recorded
	}

	client, err := h.client()
	if err != nil {
		return nil, err
	}
	e, err := NewEngine(EngineWiring{
		Manifest:        h.manifest,
		Prior:           h.prior,
		Graph:           h.graph,
		Telemetry:       telemetry,
		Kind:            h.opts.Kind,
		Client:          client,
		ModelYAML:       h.opts.ModelYAML,
		PricesYAML:      h.opts.PricesYAML,
		Mode:            h.opts.Mode,
		InvestigationID: h.opts.InvestigationID,
	})
	if err != nil {
		return nil, err
	}

	stop, err := e.Run(ctx)
	if err != nil {
		return nil, fmt.Errorf("eval: %s (%s): %w", h.manifest.ID, h.opts.Kind, err)
	}

	out := h.outcome(e, stop, recorded, client)
	out.RunIndex = h.runs
	return out, nil
}

// Engine builds the engine one run would use, for a caller that needs the engine itself —
// `fixture record-trajectory`, which writes the trajectory the engine produced.
func (h *Harness) Engine(telemetry sdk.TelemetryBackend) (*engine.Engine, error) {
	client, err := h.client()
	if err != nil {
		return nil, err
	}
	return NewEngine(EngineWiring{
		Manifest:        h.manifest,
		Prior:           h.prior,
		Graph:           h.graph,
		Telemetry:       telemetry,
		Kind:            h.opts.Kind,
		Client:          client,
		ModelYAML:       h.opts.ModelYAML,
		PricesYAML:      h.opts.PricesYAML,
		Mode:            h.opts.Mode,
		InvestigationID: h.opts.InvestigationID,
	})
}

// client is the model boundary for one run: the one the caller supplied, or a freshly built
// canned one for a fake-model run.
//
// It is built **per run** rather than once per harness because the canned transport is a
// recording being replayed, and two runs sharing one would be two runs sharing a position in it.
func (h *Harness) client() (*model.Client, error) {
	if h.opts.Client != nil {
		return h.opts.Client, nil
	}
	switch h.opts.Kind {
	case KindFakeModel:
		return CannedClient(h.opts.ModelYAML, h.opts.PricesYAML)
	case KindLive:
		return LiveClient(h.opts.ModelYAML, h.opts.PricesYAML)
	default:
		return nil, nil
	}
}

// World is the recorded world the harness loaded, for a caller that needs to serve from it
// directly.
func (h *Harness) World() *sdk.World { return h.world }

// outcome gathers the run into the record a scorer reads.
func (h *Harness) outcome(
	e *engine.Engine, stop engine.Stop, recorded *backend.Recorded, client *model.Client,
) *RunOutcome {
	out := &RunOutcome{
		FixtureID:          h.manifest.ID,
		Dir:                h.manifest.Dir,
		Kind:               h.opts.Kind,
		ModelConfigDigest:  h.modelDigest,
		ModelConfigVersion: h.modelVersion,
		StopReason:         string(stop.Reason),
		StopDetail:         stop.Detail,
	}

	out.Ledger = hypothesesOf(e)
	ranked := false
	for _, hyp := range out.Ledger {
		if hyp.Status == statusSupported {
			ranked = true
			break
		}
	}
	out.Outcome = outcomeName(stop.Outcome(ranked))
	out.Verdict, out.VerdictRefs, out.VerdictConfidence, out.VerdictBucket =
		verdictOf(out.Ledger, out.Outcome, h.manifest.Incident.GroundTruth.CauseCategory())
	out.CausalPath = causalPathOf(e, out)
	out.Onset = onsetOf(e.Onset())

	traj := e.Trajectory()
	out.TrajectoryRecords = traj.Len()
	records := traj.Records()
	out.Citations = citationsOf(e, digestBodies(records))
	if len(records) > 0 {
		replay.Normalize(records)
		if digest, err := replay.Digest(records); err == nil {
			out.TrajectoryDigest = digest
		}
		if runID, err := replay.RunID(records); err == nil {
			out.RunID = runID
		}
		out.WorkerCalls, out.WorkerCallsTotal = workerCallsOf(records)
		out.TimeToProvisional, out.TimeToFirstTestedHypothis, out.TimeToConclusion =
			latenciesOf(records, e.Trajectory().Elapsed())
	}

	if manager := e.Budget(); manager != nil {
		// A *typed* nil client is not a nil interface, and the budget manager prices whatever is
		// not nil. Passing one through would be a nil dereference inside the price table on every
		// model-free run, which is every run the corpus ships today.
		var pricing budget.Client
		if client != nil {
			pricing = client
		}
		if spend, err := manager.Spend(pricing); err == nil {
			out.Spend = spend
		}
		if tokens := manager.Tokens(); tokens != nil {
			out.ModelIDs = tokens.Models()
		}
	}

	if recorded != nil {
		misses := recorded.Misses()
		out.NotRecorded = len(misses.Missed)
		for _, miss := range misses.Missed {
			out.MissedTerms = append(out.MissedTerms, miss.Term)
		}
		if total := misses.Served + len(misses.Missed); total > 0 {
			out.MissRate = float64(len(misses.Missed)) / float64(total)
		}
	}
	return out
}

// hypothesesOf reads the belief state the run ended at, in the ledger's own rank order.
func hypothesesOf(e *engine.Engine) []Hypothesis {
	byID := map[string]*investigationv1.FinalHypothesis{}
	for _, h := range e.FinalLedger() {
		byID[h.GetHypothesisId()] = h
	}
	judgments := map[string]*investigationv1.Hypothesis{}
	for _, h := range e.Ledger().Proto().GetHypotheses() {
		judgments[h.GetHypothesisId()] = h
	}
	rows := e.Ledger().Hypotheses()
	out := make([]Hypothesis, 0, len(rows))
	for _, h := range rows {
		row := Hypothesis{
			ID:                      h.ID,
			Rank:                    h.Rank,
			Kind:                    string(h.Kind),
			Statement:               h.Statement,
			Posterior:               h.Confidence,
			Bucket:                  h.Bucket.Name,
			BucketLow:               h.Bucket.Low,
			BucketHi:                h.Bucket.High,
			Status:                  string(h.Status),
			CandidateChangeEntityID: h.CandidateChangeEntityID,
			Rationale:               h.Rationale,
			CausalRole:              string(h.CausalRole),
			UntestedReason:          h.UntestedReason,
		}
		if fh, ok := byID[h.ID]; ok {
			row.CandidateChangeRefs = append([]string(nil), fh.GetCandidateChangeEntityRefs()...)
		}
		if ph, ok := judgments[h.ID]; ok {
			row.SupportingEvidenceIDs = append([]string(nil), ph.GetSupportingEvidenceIds()...)
			row.RefutingEvidenceIDs = append([]string(nil), ph.GetRefutingEvidenceIds()...)
			row.JudgmentIDs = append([]string(nil), ph.GetJudgmentIds()...)
		}
		out = append(out, row)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Rank < out[j].Rank })
	return out
}

// verdictOf renders what the run answered, in the ground truth's own vocabulary.
//
// The rule, stated once: a run that reached `unknown` answers `unknown`, whatever sits at the
// top of its ledger; a run whose top supported hypothesis is the open `no_observed_change` one
// answers `unobserved`, or `not_change_induced: <category>` where the fixture labels a category
// — the engine cannot tell those two apart and is not asked to, because the difference is a
// label on the incident and not a finding about it; and any other run answers with the change it
// named.
func verdictOf(rows []Hypothesis, outcome, category string) (verdict string, refs []string, confidence float64, bucket string) {
	var top Hypothesis
	for _, h := range rows {
		if top.ID == "" || (h.Rank > 0 && h.Rank < top.Rank) {
			top = h
		}
	}
	if top.ID == "" || outcome != outcomeName(investigationv1.InvestigationOutcome_RANKED) {
		return VerdictUnknown, nil, top.Posterior, top.Bucket
	}
	if top.Status != statusSupported {
		return VerdictUnknown, nil, top.Posterior, top.Bucket
	}
	if top.Kind == string(ledger.KindNoObservedChange) {
		if category != "" {
			return VerdictNotChangeInducedPrefix + ": " + category, nil, top.Posterior, top.Bucket
		}
		return VerdictUnobserved, nil, top.Posterior, top.Bucket
	}
	refs = append([]string(nil), top.CandidateChangeRefs...)
	named := top.CandidateChangeEntityID
	if len(refs) > 0 {
		// A ground truth is written in the origin system's vocabulary, so the published
		// reference is the spelling a reader — and a grader — recognises. The canonical entity
		// id stays in the list beside it.
		named = refs[0]
		refs = append(refs, top.CandidateChangeEntityID)
		sort.Strings(refs)
		refs = dedupe(refs)
	}
	if named == "" {
		return VerdictUnknown, nil, top.Posterior, top.Bucket
	}
	return named, refs, top.Posterior, top.Bucket
}

// causalPathOf reports the chain the run states from cause to symptom.
//
// The engine does not publish a path object; it publishes a ranked hypothesis whose candidate
// change has targets, and a subject. The path is therefore read off those three: the change, the
// entity it targeted, and the subject the question was asked about. Where the change's target
// *is* the subject the middle step is not repeated, so a one-hop incident reports two steps
// rather than a step twice.
func causalPathOf(e *engine.Engine, out *RunOutcome) []PathStep {
	if out.VerdictClass() != "change_induced" {
		// Even where no change explains the symptom, the path localises it — which is what
		// SC-023 grades, and what a culprit-deleted variant is checked on.
		return []PathStep{{Entity: e.Subject().EntityRef, Role: "symptom"}}
	}
	top := out.Top()
	var targets []string
	for _, h := range e.Ledger().Hypotheses() {
		if h.ID == top.ID {
			targets = h.TargetEntityIDs
			break
		}
	}
	steps := []PathStep{{Entity: out.Verdict, Via: "changed-by", Role: "cause"}}
	subject := e.Subject().EntityRef
	for _, target := range targets {
		ref := e.Catalogue().RefFor(target)
		if ref == "" {
			ref = target
		}
		if ref == subject {
			continue
		}
		steps = append(steps, PathStep{Entity: ref, Via: "calls", Role: "mechanism"})
	}
	return append(steps, PathStep{Entity: subject, Role: "symptom"})
}

// citationsOf reads every evidence item the run rested on, with the answer it addresses.
func citationsOf(e *engine.Engine, bodies map[string]json.RawMessage) []Citation {
	items := e.Ledger().EvidenceItems()
	out := make([]Citation, 0, len(items))
	for _, item := range items {
		out = append(out, Citation{
			EvidenceID:     item.ID,
			Worker:         item.Worker,
			Capability:     item.Capability,
			ResponseDigest: item.ResponseDigest,
			ResponseKey:    item.ResponseKey,
			Pointer:        pointerOf(item.Term),
			Term:           backend.TermName(item.Term),
			Outcome:        item.Outcome,
			Mode:           item.Mode,
			DeepLink:       item.DeepLink,
			Digest:         bodies[item.ResponseDigest],
		})
	}
	return out
}

// digestBodies indexes the answers the run received by their response digest, which is the join
// key the ledger's evidence items address them by.
//
// It reads the trajectory rather than the ledger because the ledger holds the *address* of an
// answer and never the answer itself — that is constitution IV at the ledger boundary — while
// the recording, which exists precisely so a run can be replayed, holds both.
func digestBodies(records []*investigationv1.TrajectoryRecord) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	marshal := protojson.MarshalOptions{UseProtoNames: true}
	for _, record := range records {
		body, ok := record.GetRecord().(*investigationv1.TrajectoryRecord_WorkerResponse)
		if !ok {
			continue
		}
		resp := body.WorkerResponse.GetResponse()
		digest := resp.GetResponseDigest()
		if digest == "" || resp.GetDigest() == nil {
			continue
		}
		raw, err := marshal.Marshal(resp.GetDigest())
		if err != nil {
			continue
		}
		out[digest] = raw
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// pointerOf renders the telemetry pointer a term was taken over, where it has one.
//
// It is rendered as `<KIND> <selector>` rather than returned as a proto because a citation is
// read by a scorer and by a person, and neither wants a nested message; the selector is what
// identifies the series, and the kind is what says which of a service's several pointers it is.
func pointerOf(term *investigationv1.AlgebraTerm) string {
	var pointer *graphv1.Pointer
	switch t := term.GetTerm().(type) {
	case *investigationv1.AlgebraTerm_Compare:
		pointer = t.Compare.GetPointer()
	case *investigationv1.AlgebraTerm_Onset:
		pointer = t.Onset.GetPointer()
	case *investigationv1.AlgebraTerm_NewLogPatterns:
		pointer = t.NewLogPatterns.GetPointer()
	case *investigationv1.AlgebraTerm_ErrorsByVersion:
		pointer = t.ErrorsByVersion.GetPointer()
	case *investigationv1.AlgebraTerm_MonitorState:
		pointer = t.MonitorState.GetPointer()
	}
	if pointer == nil {
		return ""
	}
	return strings.TrimSpace(pointer.GetKind().String() + " " + pointer.GetSelector())
}

// onsetOf renders the onset estimate.
func onsetOf(o *engine.Onset) *OnsetEstimate {
	if o == nil {
		return nil
	}
	return &OnsetEstimate{
		Available:       o.Available,
		Reason:          o.Reason,
		At:              o.At,
		Uncertainty:     o.Uncertainty,
		Method:          o.Method,
		EvidenceID:      o.EvidenceID,
		ReferenceUsed:   o.ReferenceUsed,
		FellBackToAlert: o.FellBackToAlert,
	}
}

// workerCallsOf counts the calls per `<worker>/<capability>` from the recording.
func workerCallsOf(records []*investigationv1.TrajectoryRecord) (map[string]int, int) {
	counts := map[string]int{}
	total := 0
	for _, record := range records {
		body, ok := record.GetRecord().(*investigationv1.TrajectoryRecord_WorkerRequest)
		if !ok {
			continue
		}
		name := body.WorkerRequest.GetWorker()
		if capability := backend.TermName(body.WorkerRequest.GetRequest().GetTerm()); capability != "" {
			name += "/" + capability
		}
		counts[name]++
		total++
	}
	if len(counts) == 0 {
		return nil, 0
	}
	return counts, total
}

// latenciesOf reads the three published latencies (SC-013) off the run's wall-clock marks.
//
// It reads `elapsed` and not the records' own `at`, and the difference is the whole of why these
// rows used to publish `n/a`. A fixture run pins the investigation's clock to the alert instant so
// that a re-record is byte-identical, so every recorded `at` in a run is the same instant and
// every latency computed from them is zero — for a live run that took thirteen seconds exactly as
// much as for a replay. `Trajectory.Elapsed` is the second quantity: how long after the run's
// first record each record was made, on a real clock, kept beside the recording rather than in
// it. A run with no marks — a trajectory read back from a file — falls back to the recorded
// instants, which is the honest reading of a recording: it holds no wall time to report.
func latenciesOf(records []*investigationv1.TrajectoryRecord, elapsed []time.Duration) (provisional, firstTested, conclusion time.Duration) {
	if len(records) == 0 {
		return 0, 0, 0
	}
	at := func(i int) time.Duration {
		if i < len(elapsed) {
			return elapsed[i]
		}
		return records[i].GetAt().AsTime().Sub(records[0].GetAt().AsTime())
	}
	var seenWorker, seenJudgment bool
	for i, record := range records {
		at := at(i)
		switch record.GetRecord().(type) {
		case *investigationv1.TrajectoryRecord_WorkerRequest:
			if !seenWorker {
				provisional = at
				seenWorker = true
			}
		case *investigationv1.TrajectoryRecord_LedgerUpdate:
			if !seenJudgment {
				firstTested = at
				seenJudgment = true
			}
		case *investigationv1.TrajectoryRecord_Stop:
			conclusion = at
		}
	}
	return provisional, firstTested, conclusion
}

// outcomeName spells an investigation outcome the way the report rows and the ground truth both
// do: lower case, no enum prefix.
func outcomeName(outcome investigationv1.InvestigationOutcome) string {
	switch outcome {
	case investigationv1.InvestigationOutcome_RANKED:
		return "ranked"
	case investigationv1.InvestigationOutcome_UNKNOWN:
		return VerdictUnknown
	case investigationv1.InvestigationOutcome_BUDGET_EXHAUSTED:
		return "budget_exhausted"
	case investigationv1.InvestigationOutcome_INVESTIGATION_FAILED:
		return "failed"
	default:
		return "unspecified"
	}
}

func dedupe(in []string) []string {
	out := in[:0]
	var last string
	for i, each := range in {
		if i > 0 && each == last {
			continue
		}
		last = each
		out = append(out, each)
	}
	return out
}

// ---------- the engine wiring ----------

// EngineWiring is everything an engine built for a fixture needs that differs between the
// callers that build one.
type EngineWiring struct {
	// Manifest is the fixture whose `incident:` block states the question.
	Manifest *fixture.Manifest
	// Prior is π₀ from the published coverage audit.
	Prior audit.PriorRecord
	// Graph is feature 001's read surface over this fixture's own replayed events.
	Graph graphworker.QueryService
	// Telemetry is the one backend the metrics, logs and traces workers are bound to.
	Telemetry sdk.TelemetryBackend
	// Kind is KindModelFree, KindFakeModel or KindLive.
	Kind string
	// Client is an explicit model boundary; it wins over Kind.
	Client *model.Client
	// ModelYAML and PricesYAML build the client for KindFakeModel and KindLive.
	ModelYAML  string
	PricesYAML string
	// Mode is what every call records itself as. Zero means recorded.
	Mode worker.Mode
	// InvestigationID overrides the derived id.
	InvestigationID string
}

// NewEngine builds the engine for one fixture run.
func NewEngine(w EngineWiring) (*engine.Engine, error) {
	m := w.Manifest
	if m == nil || !m.IsIncident() {
		return nil, errors.New("eval: an engine needs a fixture with an `incident:` block")
	}
	registry := worker.NewRegistry()
	for _, each := range []worker.Worker{
		graphworker.New(w.Graph),
		metrics.New(w.Telemetry),
		logs.New(w.Telemetry),
		traces.New(w.Telemetry),
	} {
		if err := registry.Register(each); err != nil {
			return nil, fmt.Errorf("eval: %s: register %T: %w", m.ID, each, err)
		}
	}
	workers, err := engine.NewWorkers(registry, worker.RetryPolicy{})
	if err != nil {
		return nil, err
	}

	question := m.Incident.Question
	firedAt := question.FiredAt.UTC()
	lookback, err := QuestionLookback(question.Lookback)
	if err != nil {
		return nil, err
	}
	// A constant clock, not merely an injected one: the first wave issues its calls in parallel,
	// so a clock that advanced on every read would advance in whatever order the scheduler chose.
	clock := func() time.Time { return firedAt }

	profile := budget.ForPriority(question.Profile)
	if named, ok := budget.Published()[question.Profile]; ok {
		profile = named
	}
	manager, err := budget.New(profile, budget.OperatorCaps{}, clock)
	if err != nil {
		return nil, err
	}

	kind := w.Kind
	if kind == "" {
		kind = KindModelFree
	}
	client := w.Client
	if client == nil {
		switch kind {
		case KindFakeModel:
			if client, err = CannedClient(w.ModelYAML, w.PricesYAML); err != nil {
				return nil, err
			}
		case KindLive:
			if client, err = LiveClient(w.ModelYAML, w.PricesYAML); err != nil {
				return nil, err
			}
		}
	}

	mode := w.Mode
	if mode == "" {
		mode = worker.ModeRecorded
	}
	id := w.InvestigationID
	if id == "" {
		id = "inv-" + m.ID + "-" + kind
	}
	return engine.New(engine.Config{
		InvestigationID: id,
		Subject: engine.Subject{
			EntityRef: question.Subject,
			Statement: question.Statement,
			FiredAt:   firedAt,
			Origin:    strings.TrimSuffix(question.OriginSystem+":"+question.OriginRef, ":"),
			Priority:  question.Profile,
			Window: &investigationv1.Window{
				Start: timestamppb.New(firedAt.Add(-lookback)),
				End:   timestamppb.New(firedAt),
			},
		},
		Instants: engine.Instants{ValidAt: firedAt, ObservedAt: firedAt},
		Prior:    w.Prior,
		Workers:  workers,
		Client:   client,
		Budget:   manager,
		Mode:     mode,
		Clock:    clock,
	})
}

// DefaultModelYAML and DefaultPricesYAML are the published configuration paths.
const (
	DefaultModelYAML  = "config/model.yaml"
	DefaultPricesYAML = "config/prices.yaml"
)

// CannedClient is the fake-model transport: two turns, served through the same `model.Client` a
// live run uses, so the request build, the SDK decoder and the usage accounting are all real and
// only the wire is fake.
//
// The turns are deliberately minimal — one proposed hypothesis, one batch of judgments, then
// nothing further — because their job is to put `model_request` / `model_response` pairs into the
// recording, not to be a good investigation. The reasoning a real model does is what a *live*
// recording will hold. More turns are supplied than any path uses: a canned transport that runs
// out mid-loop fails the recording, and only the turns the loop actually consumed reach the
// trajectory, so the surplus costs nothing.
func CannedClient(modelYAML, pricesYAML string) (*model.Client, error) {
	turns := []modeltest.Turn{
		{
			ID:   "msg_fixture_001",
			Text: "The first wave already separates the candidates; I will name the condition it establishes.",
			ToolUses: []modeltest.ToolUse{{
				ID:   "toolu_fixture_001",
				Name: engine.ToolProposeHypothesis,
				Input: map[string]any{
					"kind":      "condition",
					"statement": "The symptom is carried by the rollout's own version rather than by a neighbour.",
				},
			}},
			Usage: model.Usage{Input: 1200, Output: 90},
		},
		{
			ID:   "msg_fixture_002",
			Text: "The first wave's own digests move the top candidate; recording the judgment.",
			ToolUses: []modeltest.ToolUse{{
				ID:   "toolu_fixture_002",
				Name: engine.ToolProposeJudgments,
				Input: map[string]any{"judgments": []map[string]any{{
					"hypothesis_id": "h-1",
					"evidence_id":   "e-1",
					"direction":     "neutral",
					"strength":      "none",
					"rationale": "the first wave has already been applied by the engine; this records " +
						"that the model read it and found nothing further to add.",
				}}},
			}},
			Usage: model.Usage{Input: 1400, Output: 120, CacheRead: 1200},
		},
		{ID: "msg_fixture_003", Text: "Nothing further separates the candidates.",
			Usage: model.Usage{Input: 1500, Output: 30, CacheRead: 1400}},
		{ID: "msg_fixture_004", Text: "The deterministic wave is the answer here.",
			Usage: model.Usage{Input: 1550, Output: 30, CacheRead: 1400}},
		{ID: "msg_fixture_005", Text: "Concluding.",
			Usage: model.Usage{Input: 1600, Output: 20, CacheRead: 1400}},
	}
	config, prices, err := model.LoadPair(orDefault(modelYAML, DefaultModelYAML), orDefault(pricesYAML, DefaultPricesYAML))
	if err != nil {
		return nil, err
	}
	// The canned turns are rendered in the body shape of the *configured* investigator's
	// provider, not in a fixed one. A fake-model run is meant to exercise the real request build
	// and the real decoder with only the wire faked; a canned Anthropic body decoded by a Mistral
	// client would exercise neither, and it would fail in a way that says nothing about the
	// engine.
	exchanges, err := modeltest.Exchanges(config.Roles[model.RoleInvestigator].Model, turns...)
	if err != nil {
		return nil, err
	}
	transport := model.NewReplayingTransportTolerant(exchanges)
	return model.NewClientWithTransport(config, prices, transport)
}

// LiveClient is the model boundary that actually calls the configured providers.
//
// It is the *recording* transport rather than the plain live one, because a live run's whole
// point here is the artefact it leaves: the exact request and response bodies, canonicalised and
// digested, which a later replay matches against with no network at all (FR-042a, FR-041).
//
// The credential check is the configuration's, not a single vendor's: `MissingCredentials`
// asks each provider the configuration actually names. A caller that gets an error here has a
// deployment that would have failed on its first turn.
func LiveClient(modelYAML, pricesYAML string) (*model.Client, error) {
	config, prices, err := model.LoadPair(
		orDefault(modelYAML, DefaultModelYAML), orDefault(pricesYAML, DefaultPricesYAML))
	if err != nil {
		return nil, err
	}
	if missing := config.MissingCredentials(); len(missing) > 0 {
		return nil, fmt.Errorf(
			"eval: a live run needs a credential for every provider %s configures; $%s is not set",
			orDefault(modelYAML, DefaultModelYAML), strings.Join(missing, ", $"))
	}
	return model.NewClientWithTransport(config, prices, model.NewRecordingTransport(nil))
}

// LiveAvailable reports whether a live run could be made with this configuration, and names the
// environment variables that are missing when it could not. It is what a command calls to decide
// between a live run and the model-free fallback without building a client it may not use.
func LiveAvailable(modelYAML string) (bool, []string, error) {
	config, err := model.LoadConfig(orDefault(modelYAML, DefaultModelYAML))
	if err != nil {
		return false, nil, err
	}
	missing := config.MissingCredentials()
	return len(missing) == 0, missing, nil
}

// LoadPrior reads π₀ from the published coverage audit. An absent audit is not fatal — the prior
// is then total ignorance and the run says so by carrying no audit id — because a fixture
// investigated before an audit exists is still an investigation.
func LoadPrior(path string) (audit.PriorRecord, error) {
	if strings.TrimSpace(path) == "" {
		return audit.PriorRecord{}, nil
	}
	result, err := audit.LoadResult(path)
	if err != nil {
		if os.IsNotExist(err) {
			return audit.PriorRecord{}, nil
		}
		return audit.PriorRecord{}, fmt.Errorf("eval: audit %s: %w", path, err)
	}
	return audit.PriorRecordFromAudit(result), nil
}

// QuestionLookback reads the `lookback:` a question declares.
func QuestionLookback(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Hour, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("eval: lookback %q: %w", value, err)
	}
	return d, nil
}

// ModelConfiguration digests the model configuration in force, for a caller outside this package
// that has to record which configuration a live run ran under — `fixture record-world
// --live-passes`, whose world declares it in `world/index.json`.
func ModelConfiguration(path string) (digest, version string, err error) {
	return modelConfiguration(path)
}

// modelConfiguration digests the model configuration in force.
//
// FR-061 requires every evaluation run to use production's configuration and to record it. The
// digest is over the file's bytes rather than over the parsed struct, because the thing that has
// to be comparable across runs is the document a reviewer edits — a comment that changes the
// meaning of a setting changes the run's interpretation even when the parse is identical.
//
// An absent file is not fatal: a model-free run over a corpus checked out without a
// configuration is still a run, and it records an empty digest, which says exactly that.
func modelConfiguration(path string) (digest, version string, err error) {
	path = orDefault(path, DefaultModelYAML)
	raw, err := os.ReadFile(path) //nolint:gosec // a configuration path the caller named
	if err != nil {
		if os.IsNotExist(err) {
			return "", "", nil
		}
		return "", "", fmt.Errorf("eval: read %s: %w", path, err)
	}
	sum := sha256.Sum256(raw)
	config, err := model.LoadConfig(path)
	if err != nil {
		// A configuration that does not parse still has a digest, and reporting the digest of a
		// broken file is more useful than refusing to report anything.
		return hex.EncodeToString(sum[:]), "", nil //nolint:nilerr // documented above
	}
	return hex.EncodeToString(sum[:]), config.Version, nil
}

func orDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
