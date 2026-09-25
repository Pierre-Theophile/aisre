// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/investigation/ledger"
	sdk "github.com/Pierre-Theophile/aisre/pkg/backend"
)

// The deterministic first wave (T066, FR-046a, SC-013, plan Findings F2).
//
// For each top candidate, in parallel, **with no model in the loop**: error rate and latency
// before and after onset, new log patterns, error spans on the path edges, and errors split by
// version tag. Five questions, asked of the graph's own top candidates, producing evidence items
// and judgments directly.
//
// This is the answer to the plan's Finding F2. A single request on this model family can run for
// minutes on a hard problem, so `page`'s 90–120 second first-tested-hypothesis target cannot sit
// behind a model turn. It does not: the first wave is arithmetic over digests, and the first
// hypothesis to reach `supported` or `refuted` reaches it from here. The model's job starts after
// the wave, with a ledger that already has evidence in it.
//
// The judgments are deterministic, which is the property that makes this worth doing at all: the
// same digests produce the same judgments on any machine, so a first wave is reproducible without
// replaying a model. The rules are published below, one per query, each with the reasoning that
// sets its strength.

// FirstWaveCandidates is how many top candidates the wave tests. Three is what fits inside the
// 30-second target with five queries each against a recorded world, and is enough that the
// culprit is inside it whenever the prior is doing its job.
const FirstWaveCandidates = 3

// FirstWaveTarget is the published figure the wave completes within.
const FirstWaveTarget = 30 * time.Second

// FirstWaveQueries is the published query set, in the order a rendering lists them. It is exported
// because the fixture recorder asserts at record time that a fixture's recorded world holds every
// one of them for every top candidate — a world missing a first-wave query is a world that cannot
// answer the investigation it was recorded for.
var FirstWaveQueries = []string{
	"compare:error_rate", "compare:p95", "new_log_patterns", "error_spans", "errors_by_version",
}

// FirstWave is what the wave produced.
type FirstWave struct {
	// Elapsed is how long it took, measured against FirstWaveTarget.
	Elapsed time.Duration
	// Answers is every call it made, in a deterministic order.
	Answers []*Answer
	// Judgments is every judgment it applied. They are the ledger's, sourced `first_wave`: no
	// model was involved (data-model §investigation.judgments).
	Judgments []ledger.Judgment
	// Tested names the hypotheses the wave moved.
	Tested []string
	// Missing names the queries the recorded world did not hold, so a fixture can be reported
	// insufficient rather than silently under-tested.
	Missing []string
	// Untested names the candidates the wave did not test — beyond its cap, or too old to be a
	// cause — each recorded with its reason and the exact next query (FR-031).
	Untested []string
}

// RunFirstWave runs the wave over the top candidates in the ledger.
//
// Evidence gathering is parallel and reasoning is not, and the ordering discipline that follows
// from that is worth stating in three parts. The telemetry calls of every candidate are issued
// concurrently. **Admission is not**: every planned call passes the budget sequentially, in the
// wave's plan order, before any of them is issued, so which call is refused when a cap binds is a
// function of the plan and never of goroutine scheduling. And the ledger writes that turn answers
// into evidence happen afterwards, in candidate order. Two runs over the same world therefore
// admit the same set, produce the same evidence ids, the same judgments and the same trajectory —
// which is what makes a recorded first wave replayable at all (FR-013a).
func (e *Engine) RunFirstWave(ctx context.Context) (*FirstWave, error) {
	started := e.now()
	onset := e.Onset()
	reference := e.subject.FiredAt
	if onset != nil {
		reference = onset.ReferenceUsed
	}

	plans, skipped := e.planFirstWave(reference)
	wave := &FirstWave{}
	// The candidates the wave will not reach are recorded before it runs, not after: a run that
	// fails mid-wave still leaves them stated, with the reason and the query that would test them
	// (FR-031).
	for _, each := range skipped {
		if err := e.recordUntestedCandidate(each.hypothesisID, each.reason); err != nil {
			return nil, err
		}
		wave.Untested = append(wave.Untested, each.hypothesisID)
	}

	// The pointer calls come first and sequentially. They are cheap graph reads, every telemetry
	// call depends on the ids they mint, and doing them in order is what makes those ids stable.
	requests := make([][]*investigationv1.AlgebraRequest, len(plans))
	for i, plan := range plans {
		pointers, err := e.pointersFor(ctx, plan.targetRef)
		if err != nil {
			return nil, err
		}
		wave.Answers = append(wave.Answers, pointers)
		built, err := e.planQueries(plan)
		if err != nil {
			return nil, err
		}
		requests[i] = built
	}

	// Admission, sequentially, in the wave's own plan order: candidate rank, then query order
	// within a candidate.
	//
	// This is the ordering discipline's second half and it is not a tidiness. Admission books
	// against a shared budget, so when a cap binds — the wave wants seven `expensive` calls and
	// the reserve leaves six — *which* call is refused is decided by the order the admissions were
	// made in. Made inside the goroutines below, that order was the Go scheduler's, and the same
	// world produced a 27-call run or a 26-call run depending on it. Made here, it is the plan's:
	// the lowest-ranked candidate's last query is the one that loses, every run, on every machine.
	admissions := make([][]*admitted, len(plans))
	refusals := make([][]*Answer, len(plans))
	for i := range plans {
		for _, req := range requests[i] {
			adm, refused, err := e.preAdmit(req)
			if err != nil {
				return nil, err
			}
			if refused != nil {
				refusals[i] = append(refusals[i], refused)
				continue
			}
			admissions[i] = append(admissions[i], adm)
		}
	}

	// The telemetry calls, in parallel across candidates. Every one of them has already been
	// admitted, so a goroutine here can only issue a call the budget has already booked.
	answers := make([][]*Answer, len(plans))
	errs := make([]error, len(plans))
	// Each candidate records into a trajectory of its own, and the branches are spliced back in
	// plan order below.
	//
	// This is the same discipline as the admission loop above, applied to the *record* rather
	// than to the call. Writing straight into the engine's trajectory from these goroutines put
	// it in completion order, so the trajectory the engine held — and therefore its digest, and
	// therefore every assertion made on it without going through `replay.Write` — was a function
	// of the Go scheduler. It was invisible on an idle machine, where a goroutine per candidate
	// usually runs to completion before the next one starts, and it surfaced as a failed
	// determinism test the moment two full test runs competed for cores.
	sinks := make([]*Trajectory, len(plans))
	var wg sync.WaitGroup
	for i := range plans {
		sinks[i] = e.trajectory.Concurrent()
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out := make([]*Answer, 0, len(admissions[i]))
			for _, adm := range admissions[i] {
				answer, err := e.issueAdmittedInto(ctx, sinks[i], adm)
				if err != nil {
					errs[i] = err
					return
				}
				out = append(out, answer)
			}
			answers[i] = out
		}(i)
	}
	wg.Wait()
	for i := range plans {
		e.trajectory.Splice(sinks[i])
	}
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}

	// The ledger writes, in candidate order and then in query order.
	for i, plan := range plans {
		for _, answer := range answers[i] {
			if err := e.record(answer); err != nil {
				return nil, err
			}
			wave.Answers = append(wave.Answers, answer)
			if answer.Response.GetOutcome() == investigationv1.TermOutcome_NOT_RECORDED {
				wave.Missing = append(wave.Missing, plan.hypothesisID+":"+answer.Capability)
			}
		}
		judgments, err := e.judgeFirstWave(plan, answers[i])
		if err != nil {
			return nil, err
		}
		if len(judgments) > 0 {
			wave.Judgments = append(wave.Judgments, judgments...)
			wave.Tested = append(wave.Tested, plan.hypothesisID)
			continue
		}
		// A candidate whose queries a budget refused is untested *for that reason*, with the
		// typed dimension and the exact query attached, rather than falling through to the
		// run's generic end-of-investigation sweep (FR-031, FR-047a). A candidate the wave
		// still judged keeps its judgment: a refused fifth query does not undo four answered
		// ones.
		if len(refusals[i]) > 0 {
			refused := refusals[i][0]
			if err := e.recordUntested(plan.hypothesisID, refused.Request, refused.Refused); err != nil {
				return nil, err
			}
			wave.Untested = append(wave.Untested, plan.hypothesisID)
		}
	}
	wave.Elapsed = e.now().Sub(started)
	return wave, nil
}

// candidatePlan is one candidate's share of the wave: which hypothesis, which entity's pointers,
// which windows.
type candidatePlan struct {
	hypothesisID string
	changeID     string
	targetRef    string
	reference    time.Time
	width        time.Duration
}

// skippedCandidate is a candidate the wave does not test, with the sentence that says why.
type skippedCandidate struct {
	hypothesisID string
	reason       string
}

// StaleBefore is how far before the reference instant a change stops being a candidate cause of a
// symptom that started there: 2τ, twice the ranker's own temporal decay constant (1 hour).
//
// It is the ranker's constant rather than a new one on purpose. The published ranking already
// decays a change's score with τ = 30 minutes, so a change 2τ before the onset has been scored as
// four e-foldings less relevant than one at the onset; testing it ahead of a candidate the clock
// likes would spend the wave's five queries on the least likely thing in the list. It is reported
// `untested` with that sentence and the exact query, never refuted: old is not innocent (FR-031).
const StaleBefore = 2 * 30 * time.Minute

// planFirstWave picks the top candidates and the entity whose telemetry would show their effect,
// and says which candidates it is leaving out and why.
//
// The entity is the change's *target*, not the change itself: a deploy has no error rate, the
// service it deployed does.
func (e *Engine) planFirstWave(reference time.Time) ([]candidatePlan, []skippedCandidate) {
	width := e.halfWindow()
	var out []candidatePlan
	var skipped []skippedCandidate
	for _, h := range e.ledger.Hypotheses() {
		if h.Kind != ledger.KindChange || h.Status == ledger.StatusExonerated {
			continue
		}
		if h.Status == ledger.StatusUntested {
			continue
		}
		if before, stale := e.staleBefore(h); stale {
			skipped = append(skipped, skippedCandidate{h.ID, fmt.Sprintf(
				"it changed %s before the reference instant %s, more than the published %s (2τ of the "+
					"ranker's own decay), so the first wave spent its queries on nearer candidates; it is "+
					"reported untested rather than refuted, because old is not innocent",
				before.Round(time.Second), reference.UTC().Format(time.RFC3339), StaleBefore)})
			continue
		}
		if len(out) >= FirstWaveCandidates {
			skipped = append(skipped, skippedCandidate{h.ID, fmt.Sprintf(
				"it ranked below the top %d candidates the first wave tests, and the wave reached its cap "+
					"before it; nothing was learned about it either way", FirstWaveCandidates)})
			continue
		}
		// The ledger keeps a change's targets as the graph names them — canonical entity ids —
		// and every read below takes a reference. The catalogue holds the translation, read out
		// of the node versions the neighbourhood read returned (catalogue.NoteNode). A target no
		// answer has described falls back to the subject, which is the honest reading of "the
		// graph cannot tell us what this change touched": the symptom is still the subject's.
		target := e.subject.EntityRef
		if len(h.TargetEntityIDs) > 0 {
			if ref := e.catalogue.RefFor(h.TargetEntityIDs[0]); ref != "" {
				target = ref
			}
		}
		out = append(out, candidatePlan{
			hypothesisID: h.ID,
			changeID:     h.CandidateChangeEntityID,
			targetRef:    target,
			reference:    reference,
			width:        width,
		})
	}
	return out, skipped
}

// staleBefore reports how long before the reference instant a candidate changed, and whether that
// is more than StaleBefore. A candidate the ranking did not measure is never called stale: an
// unknown distance is not a long one.
func (e *Engine) staleBefore(h ledger.Hypothesis) (time.Duration, bool) {
	e.mu.Lock()
	seconds, ok := e.changeOffsets[h.CandidateChangeEntityID]
	e.mu.Unlock()
	if !ok || seconds <= 0 {
		return 0, false
	}
	before := time.Duration(seconds) * time.Second
	return before, before > StaleBefore
}

// recordUntestedCandidate files one candidate the wave did not test, with its reason and the exact
// query that would test it (FR-031).
func (e *Engine) recordUntestedCandidate(hypothesisID, reason string) error {
	h, ok := e.ledger.Hypothesis(hypothesisID)
	if !ok {
		return fmt.Errorf("engine: first wave: untested %s is not in the ledger", hypothesisID)
	}
	term, err := e.NextQueryFor(h)
	if err != nil {
		return err
	}
	if term == nil {
		return nil
	}
	if err := e.ledger.SetStatus(hypothesisID, ledger.StatusUntested, ledger.StatusUpdate{
		Reason:    reason,
		NextQuery: term,
	}); err != nil {
		return fmt.Errorf("engine: first wave: record untested %s: %w", hypothesisID, err)
	}
	return nil
}

// Lookback is the investigation window's own width — the `lookback:` the question declared.
func (e *Engine) Lookback() time.Duration {
	return e.subject.Window.GetEnd().AsTime().Sub(e.subject.Window.GetStart().AsTime())
}

// halfWindow is the width each side of the reference instant the before/after comparison uses.
// The arithmetic is `CompareHalfWidth`'s, which `fixture record-world` calls too, so the windows
// a world holds are the windows this wave asks about (plan.go).
func (e *Engine) halfWindow() time.Duration { return CompareHalfWidth(e.Lookback()) }

// planQueries builds one candidate's first-wave queries, in a fixed order.
//
// They are built rather than issued here so that the issuing can be parallel and the recording
// cannot be: the request list is a pure function of the plan and the catalogue, and two runs over
// the same world build the same list.
func (e *Engine) planQueries(plan candidatePlan) ([]*investigationv1.AlgebraRequest, error) {
	metric := e.pointerOfKind(plan.targetRef, graphv1.PointerKind_METRIC)
	logPointer := e.pointerOfKind(plan.targetRef, graphv1.PointerKind_LOG)

	// The windows are the comparison pair's own two halves rather than a second derivation beside
	// it. One reference instant and one width produce every window in the wave, so "the fifteen
	// minutes after the onset" means the same interval in the version split, in the log diff and
	// in the span query as it does in the comparison — and a recording that holds the pair holds
	// all four.
	// The pair comes from the published plan rather than from the reference and the width
	// directly, because the plan is where the investigation's horizon is applied: nobody sees the
	// future, and a symptom half that reached past `observed_at` would be answered `no_data` by
	// every backend and held by no recorded world (plan.go, ComparePairs).
	pair := ComparePairs(plan.reference, e.at.ObservedAt, e.Lookback())[0]
	reference := pair.GetReferenceAt().AsTime().UTC()
	widthSeconds := pair.GetWidthSeconds()
	windowStart := pair.GetSymptom().GetStart().AsTime().Format(time.RFC3339)
	windowEnd := pair.GetSymptom().GetEnd().AsTime().Format(time.RFC3339)
	baselineStart := pair.GetBaseline().GetStart().AsTime().Format(time.RFC3339)
	baselineEnd := pair.GetBaseline().GetEnd().AsTime().Format(time.RFC3339)

	type query struct {
		tool   string
		fields map[string]any
	}
	var queries []query

	if metric != "" {
		for _, statistic := range []string{"error_rate", "p95"} {
			queries = append(queries, query{"compare", map[string]any{
				"pointer_id":              metric,
				"reference_at":            reference.Format(time.RFC3339),
				"width_seconds":           widthSeconds,
				"statistic":               statistic,
				"serves_hypothesis_id":    plan.hypothesisID,
				"discriminating_question": "did " + statistic + " on " + plan.targetRef + " move across the onset?",
			}})
		}
		queries = append(queries, query{"errors_by_version", map[string]any{
			"pointer_id":              metric,
			"window_start":            windowStart,
			"window_end":              windowEnd,
			"serves_hypothesis_id":    plan.hypothesisID,
			"discriminating_question": "is the error rate concentrated in one deployed version of " + plan.targetRef + "?",
		}})
	}
	if logPointer != "" {
		queries = append(queries, query{"new_log_patterns", map[string]any{
			"pointer_id":              logPointer,
			"window_start":            windowStart,
			"window_end":              windowEnd,
			"baseline_start":          baselineStart,
			"baseline_end":            baselineEnd,
			"serves_hypothesis_id":    plan.hypothesisID,
			"discriminating_question": "did any log template appear on " + plan.targetRef + " that was not there before?",
		}})
	}
	// The span query names the edge's own published type, read out of the neighbourhood, rather
	// than assuming `calls`: an edge the graph calls `depends_on` asked about as `calls` is a
	// question no backend holds an answer to and no world has recorded. Where the neighbourhood
	// holds no edge between the two at all, the question is not asked — there is no path for the
	// subject's errors to be on.
	if plan.targetRef != e.subject.EntityRef {
		if edge, ok := e.edgeToTarget(plan.targetRef); ok {
			queries = append(queries, query{"error_spans", map[string]any{
				"src_entity_ref":          e.subject.EntityRef,
				"dst_entity_ref":          plan.targetRef,
				"edge_type":               edgeTypeName(edge),
				"window_start":            windowStart,
				"window_end":              windowEnd,
				"serves_hypothesis_id":    plan.hypothesisID,
				"discriminating_question": "are the subject's errors on the edge to " + plan.targetRef + "?",
			}})
		}
	}

	out := make([]*investigationv1.AlgebraRequest, 0, len(queries))
	for _, q := range queries {
		req, err := DecodeCall(q.tool, jsonObject(q.fields), e.catalogue, e.at)
		if err != nil {
			return nil, fmt.Errorf("engine: first wave: %w", err)
		}
		out = append(out, req)
	}
	return out, nil
}

// first issues one first-wave call through the same decoder the model's calls go through, so a
// deterministic call and a proposed call cannot be formed differently.
func (e *Engine) first(ctx context.Context, tool string, fields map[string]any) (*Answer, error) {
	req, err := DecodeCall(tool, jsonObject(fields), e.catalogue, e.at)
	if err != nil {
		return nil, fmt.Errorf("engine: first wave: %w", err)
	}
	return e.Call(ctx, req)
}

// pointersFor obtains an entity's pointers, which every telemetry call in the wave depends on.
func (e *Engine) pointersFor(ctx context.Context, entityRef string) (*Answer, error) {
	return e.first(ctx, "pointers", map[string]any{
		"entity_ref":              entityRef,
		"discriminating_question": "which telemetry selectors describe " + entityRef + "?",
	})
}

// edgeToTarget returns the published type of the edge from the subject to a candidate's target,
// and whether the neighbourhood holds one.
func (e *Engine) edgeToTarget(targetRef string) (graphv1.EdgeType, bool) {
	src := e.catalogue.EntityIDFor(e.subject.EntityRef)
	dst := e.catalogue.EntityIDFor(targetRef)
	if src == "" || dst == "" {
		return graphv1.EdgeType_EDGE_TYPE_UNSPECIFIED, false
	}
	return e.catalogue.EdgeTypeBetween(src, dst)
}

// edgeTypeName spells an edge type the way the tool schema takes it.
func edgeTypeName(edge graphv1.EdgeType) string {
	return strings.ToLower(edge.String())
}

func (e *Engine) pointerOfKind(entityRef string, kind graphv1.PointerKind) string {
	for _, id := range e.catalogue.PointersFor(entityRef) {
		pointer, err := e.catalogue.Pointer(id)
		if err != nil {
			continue
		}
		if pointer.GetKind() == kind {
			return id
		}
	}
	return ""
}

// The published first-wave judgment rules.
//
// Each is a rule about what a digest *says*, never about what it suggests. A rule that needed
// interpretation would be a model in disguise, and the whole point of the wave is that no model
// is involved.
const (
	// versionConcentration is how much higher one version's error rate must be than the rest
	// before `errors_by_version` is a strong support. Twice is a difference an on-call would act
	// on; anything less is within the noise a rollout produces anyway.
	versionConcentration = 2.0
	// newPatternsForSupport is how many genuinely new log templates count as a moderate support.
	newPatternsForSupport = 1
	// singleVersionErrorFloor is the error rate above which a breakdown holding only *one* version
	// is still evidence.
	//
	// It arises constantly: when the symptom window opens at the rollout, the rolled-out version is
	// the only one serving it, and there is nothing to compare it against inside the split. What
	// the answer then establishes is narrower but real — the version serving the symptom window
	// fails this often — so it enters at `moderate` rather than `strong`, and the separation it
	// cannot provide comes from the before/after comparison instead. One in twenty is the level at
	// which a failure rate is the sort of thing a monitor pages on.
	singleVersionErrorFloor = 0.05
)

// judgeFirstWave turns one candidate's answers into judgments, deterministically.
//
// The per-answer rules are FirstWaveVerdict's. One rule is *not* per-answer, and cannot be: a
// candidate whose own target did not move across the reference instant is refuted by that silence,
// and no single digest says it — it is the conjunction of the comparisons and the version split
// (flatTarget below). That judgment replaces the neutral one on the first comparison rather than
// being added beside it, because the ledger allows one judgment per (evidence, hypothesis) pair
// and because counting the same silence twice would be counting the same fact twice.
func (e *Engine) judgeFirstWave(plan candidatePlan, answers []*Answer) ([]ledger.Judgment, error) {
	flat, flatEvidenceID := flatTarget(answers)
	var out []ledger.Judgment
	for _, answer := range answers {
		if answer == nil || answer.EvidenceID == "" || answer.Refused != nil {
			continue
		}
		direction, strength, ok := FirstWaveVerdict(answer.Capability, answer.Response)
		if !ok {
			continue
		}
		if flat && answer.EvidenceID == flatEvidenceID {
			direction, strength = ledger.Refutes, ledger.Moderate
		}
		judgment, err := e.Judge(plan.hypothesisID, answer.EvidenceID, direction, strength,
			ledger.SourceFirstWave, answer.Response.GetTermKey())
		if err != nil {
			return nil, fmt.Errorf("engine: first wave judgment on %s: %w", plan.hypothesisID, err)
		}
		out = append(out, judgment)
	}
	if len(out) > 0 {
		e.budget.MarkFirstTested()
		if err := e.settleStatus(plan.hypothesisID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// flatTarget is the published wave-level rule (Phase 8 K3): the candidate's own target shows no
// movement across the reference instant.
//
// A change is a candidate because it happened near the symptom in time and topology. What makes it
// a *cause* is that something measurable changed where it landed. So when every comparison over
// the target is a digest that did not separate, or separated downward, and the version split does
// not concentrate the errors in one version either, the wave has an answer and it is a negative
// one: this change did not move its own target, and a change that did not move its own target did
// not cause a symptom downstream of it.
//
// Reported as `refutes` at `moderate`, never stronger. The target may be the subject itself where
// the graph could not say what the change touched, and a rollout can break a caller without moving
// the callee's own error rate; moderate is the strength at which an on-call would look elsewhere
// first without considering the matter closed.
//
// It returns the evidence id the judgment is recorded against — the first comparison, which is the
// answer the rule is mostly about — so the refutation cites a digest a reader can open rather than
// an absence a reader has to take on trust.
func flatTarget(answers []*Answer) (bool, string) {
	var first string
	var compared bool
	for _, answer := range answers {
		if answer == nil || answer.EvidenceID == "" || answer.Refused != nil {
			continue
		}
		direction, _, ok := FirstWaveVerdict(answer.Capability, answer.Response)
		switch answer.Capability {
		case "compare":
			if !ok {
				// The comparison said nothing at all about the hypothesis — no digest, or no
				// comparison inside it. Silence from a question that was not answered is not
				// evidence that nothing happened (FR-027).
				return false, ""
			}
			if direction == ledger.Supports {
				return false, ""
			}
			compared = true
			if first == "" {
				first = answer.EvidenceID
			}
		case "errors_by_version":
			if ok && direction == ledger.Supports {
				return false, ""
			}
		}
	}
	return compared && first != "", first
}

// FirstWaveVerdict is the published rule table: what one digest says about the hypothesis the call
// served. It returns ok=false where the answer says nothing about the hypothesis at all — which is
// different from a neutral judgment, and the difference is the one FR-027 protects.
//
// It is exported because it is the definition of the deterministic half of this engine: a fixture
// recorder, an evaluation harness and this loop must all agree on what a digest means, and one
// function is the only way to make that true.
func FirstWaveVerdict(capability string, resp *investigationv1.AlgebraResponse) (ledger.Direction, ledger.Strength, bool) {
	switch resp.GetOutcome() {
	case investigationv1.TermOutcome_DIGEST:
	case investigationv1.TermOutcome_NO_DATA:
		// The only outcome that is evidence that nothing happened. A window that was covered and
		// held nothing bears weakly against a change that should have shown there.
		if capability == "compare" || capability == "errors_by_version" {
			return ledger.Refutes, ledger.Weak, true
		}
		return ledger.Neutral, "", true
	default:
		// not_yet_ingested, query_failed, not_recorded, partial: none of them is evidence that
		// nothing happened, so none of them moves the hypothesis. The evidence item is still
		// recorded with its reason (FR-027).
		return "", "", false
	}

	digest := resp.GetDigest()
	switch capability {
	case "compare":
		comparisons := digest.GetMetric().GetComparisons()
		if len(comparisons) == 0 {
			// The answer carried no comparison at all, which says nothing about the hypothesis —
			// not even that it was tested.
			return "", "", false
		}
		// The first comparison is the one the term asked for; the digest's published ordering puts
		// it first, and a rule that scanned for the most favourable one would be a rule that
		// picked its own evidence.
		comparison := comparisons[0]
		switch {
		case !comparison.GetSeparable():
			// Tested and did not separate. Recorded, and moves nothing.
			return ledger.Neutral, "", true
		case comparison.GetDirection() == "up":
			return ledger.Supports, ledger.Moderate, true
		default:
			return ledger.Refutes, ledger.Weak, true
		}

	case "errors_by_version":
		versions := digest.GetErrorsByVersion().GetVersions()
		switch {
		case concentrated(digest.GetErrorsByVersion()):
			return ledger.Supports, ledger.Strong, true
		case len(versions) > 1:
			// Every version fails the same way, which is a real finding and bears against a
			// single rollout being the cause.
			return ledger.Refutes, ledger.Moderate, true
		case len(versions) == 1 && versions[0].GetErrorRate() >= singleVersionErrorFloor:
			return ledger.Supports, ledger.Moderate, true
		default:
			return "", "", false
		}

	case "new_log_patterns":
		if countNew(digest.GetLog()) >= newPatternsForSupport {
			return ledger.Supports, ledger.Moderate, true
		}
		return ledger.Neutral, "", true

	case "error_spans":
		if errorSpanCount(digest.GetTrace()) > 0 {
			return ledger.Supports, ledger.Moderate, true
		}
		return ledger.Neutral, "", true

	default:
		return "", "", false
	}
}

// concentrated reports whether one version's error rate is at least versionConcentration times the
// highest of the others — the shape of a rollout regression, and the reason `errors_by_version` is
// the most decisive telemetry question available in one.
func concentrated(digest *investigationv1.ErrorsByVersionDigest) bool {
	versions := digest.GetVersions()
	if len(versions) < 2 {
		return false
	}
	sorted := append([]*investigationv1.VersionBreakdown(nil), versions...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].GetErrorRate() > sorted[j].GetErrorRate() })
	top, next := sorted[0].GetErrorRate(), sorted[1].GetErrorRate()
	if top <= 0 {
		return false
	}
	if next <= 0 {
		return true
	}
	return top/next >= versionConcentration
}

func countNew(digest *investigationv1.LogDigest) int {
	var count int
	for _, pattern := range digest.GetPatterns() {
		if pattern.GetNewInWindow() {
			count++
		}
	}
	return count
}

func errorSpanCount(digest *investigationv1.TraceDigest) int64 {
	var count int64
	for _, group := range digest.GetGroups() {
		if group.GetErrorKind() != "" {
			count += group.GetCount()
		}
	}
	return count
}

// settleStatus moves a hypothesis to `supported` or `refuted` once the wave's judgments justify
// it, which is what the first-tested target is measured against.
//
// `supported` is refused by the ledger unless a supporting judgment rests on an evidence item from
// a source-of-truth worker, so this asks and accepts the refusal rather than asserting.
func (e *Engine) settleStatus(hypothesisID string) error {
	var supports, refutes int
	for _, j := range e.ledger.JudgmentsFor(hypothesisID) {
		switch j.Direction {
		case ledger.Supports:
			supports++
		case ledger.Refutes:
			refutes++
		case ledger.Neutral:
		}
	}
	switch {
	case supports > refutes:
		if err := e.ledger.SetStatus(hypothesisID, ledger.StatusSupported, ledger.StatusUpdate{}); err != nil {
			// The ledger refused it, which is the rule working: a hypothesis with no
			// source-of-truth support does not reach `supported`. `inconclusive` is the honest
			// status for "we tested it and the evidence leans this way but does not qualify".
			return e.ledger.SetStatus(hypothesisID, ledger.StatusInconclusive, ledger.StatusUpdate{})
		}
	case refutes > supports:
		return e.ledger.SetStatus(hypothesisID, ledger.StatusRefuted, ledger.StatusUpdate{})
	default:
		return e.ledger.SetStatus(hypothesisID, ledger.StatusInconclusive, ledger.StatusUpdate{})
	}
	return nil
}

// Render renders the wave for a log line and for the stream's second message.
func (w *FirstWave) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "First wave (no model in the loop) completed in %s against a %s target: "+
		"%d calls, %d judgments, %d hypotheses tested.\n",
		w.Elapsed.Round(time.Millisecond), FirstWaveTarget, len(w.Answers), len(w.Judgments), len(w.Tested))
	if len(w.Missing) > 0 {
		fmt.Fprintf(&b, "not recorded in this world: %s\n", strings.Join(w.Missing, ", "))
	}
	if len(w.Untested) > 0 {
		fmt.Fprintf(&b, "not tested by the wave, each with its reason and next query (FR-031): %s\n",
			strings.Join(w.Untested, ", "))
	}
	return b.String()
}

// AuditWorldForFirstWave reports which of the published first-wave queries a recorded world does
// not hold for one candidate's target, given that target's metric and log selectors.
//
// It is the record-time assertion T066 asks for: a world that cannot answer the first wave is a
// world that cannot answer the investigation it was recorded for, and finding that out during an
// evaluation run rather than during recording wastes the recording.
func AuditWorldForFirstWave(world *sdk.World, metricSelector, logSelector, srcEntityID, dstEntityID string) []string {
	held := map[string]bool{}
	for _, key := range world.Keys() {
		term, ok := world.Term(key)
		if !ok {
			continue
		}
		switch body := term.GetTerm().(type) {
		case *investigationv1.AlgebraTerm_Compare:
			if body.Compare.GetPointer().GetSelector() != metricSelector {
				continue
			}
			switch body.Compare.GetStatistic() {
			case investigationv1.Statistic_ERROR_RATE:
				held["compare:error_rate"] = true
			case investigationv1.Statistic_P95:
				held["compare:p95"] = true
			}
		case *investigationv1.AlgebraTerm_NewLogPatterns:
			if body.NewLogPatterns.GetPointer().GetSelector() == logSelector {
				held["new_log_patterns"] = true
			}
		case *investigationv1.AlgebraTerm_ErrorsByVersion:
			if body.ErrorsByVersion.GetPointer().GetSelector() == metricSelector {
				held["errors_by_version"] = true
			}
		case *investigationv1.AlgebraTerm_ErrorSpans:
			if body.ErrorSpans.GetSrcEntityId() == srcEntityID &&
				body.ErrorSpans.GetDstEntityId() == dstEntityID {
				held["error_spans"] = true
			}
		}
	}

	var missing []string
	for _, query := range FirstWaveQueries {
		if !held[query] {
			missing = append(missing, query)
		}
	}
	return missing
}
