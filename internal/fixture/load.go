// SPDX-License-Identifier: Apache-2.0

package fixture

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	eventlog "github.com/Pierre-Theophile/aisre/internal/log"
	"github.com/Pierre-Theophile/aisre/internal/projector"
)

// Loading a fixture (FR-047, T027).
//
// A load is a replay of somebody else's day: the sources are registered as they declared
// themselves, then every accepted event is applied in file order with the observed time the log
// recorded for it — never the clock of the machine doing the loading, which is the whole point
// of FR-023 and the reason a replayed graph has the observed intervals the live one had.
//
// The must-be-rejected events go in last, after the graph is fully built, because a rejection
// is a statement about a populated graph: `otel:demo:bad-span-props-1` must be refused even
// though the entity it names exists and every other field is valid (SC-009). The graph is
// snapshotted either side of them, since "rejected" also means "changed nothing" (FR-024).

// DefaultLoadBatch is how many events share one transaction while a fixture is loaded, which
// is what `fixture verify`'s replay step is. It matches projector.DefaultReplayBatchSize for
// the same reason: replaying a log is not the live path, and committing once per event is
// what made the 1M-event replay miss its budget (docs/benchmarks/README.md).
const DefaultLoadBatch = 500

// LoadOptions carries what the events themselves do not say.
type LoadOptions struct {
	// Principal is the authenticated individual credited with the fixture's human decision
	// events (FR-041). Ordinary feeder events have none.
	Principal string
	// BatchSize is how many events share one transaction. Zero means DefaultLoadBatch; one
	// restores the transaction-per-event loop.
	//
	// It changes when the work is committed, never what is applied: events are still applied
	// one at a time, in file order, each with its own recorded observed time, so the graph a
	// load produces is the same graph at any batch size (FR-023). What it does change is the
	// failure mode — an event that fails takes its batch with it — which is the trade a
	// replay may make and a live ingest may not.
	BatchSize int
}

func (o LoadOptions) batchSize() int {
	if o.BatchSize <= 0 {
		return DefaultLoadBatch
	}
	return o.BatchSize
}

// LoadReport is what a load did.
type LoadReport struct {
	// Applied, DuplicateNoop and Rejected count every event submitted, from both
	// events.jsonl and rejected.jsonl.
	Applied       int
	DuplicateNoop int
	Rejected      int
	// Results are the ingest results in submission order: events.jsonl first, then
	// rejected.jsonl.
	Results []*graphv1.IngestResult
	// RejectedMismatches is empty when the fixture's rejection contract held exactly: every
	// accepted event was applied, every rejected.jsonl event was refused with the reason code
	// the manifest states, every expect_rejected entry was exercised, and the graph did not
	// move while they were submitted. Each entry is a human-readable failure.
	RejectedMismatches []string
}

// Load registers the fixture's sources, applies its accepted events with their recorded observed
// times, and then submits its must-be-rejected events and checks each is refused with the reason
// code the manifest states.
//
// An event that does not behave as the fixture says is recorded in RejectedMismatches, not
// returned as an error: a fixture disagreeing with the code is a verification result, and the
// caller decides what to do with it. Only an infrastructure failure returns an error.
func Load(ctx context.Context, p *projector.Projector, dir string, opts LoadOptions) (*LoadReport, error) {
	m, err := LoadManifest(dir)
	if err != nil {
		return nil, err
	}
	events, err := ReadEvents(m.EventsPath())
	if err != nil {
		return nil, err
	}
	events, err = WithHumanDecisions(m, events)
	if err != nil {
		return nil, err
	}
	return load(ctx, p, m, events, opts)
}

// WithHumanDecisions merges the manifest's `human_decisions` into an event stream, in observed
// order (contracts/fixture-format.md §Recording, FR-040).
//
// A human decision is not harness behaviour applied around a replay: it is an event, and the
// only honest way to verify FR-040 is to put it in the stream where it happened and then replay
// the whole thing. That is why it is merged here — before the loader, the double-delivery check
// and the shuffle check all see the same list — rather than applied afterwards by the verifier.
//
// Ordering: the merge is stable and keyed on observed time, so a decision lands after every
// feeder event the graph had already seen when it was taken and before every one it had not.
// A decision and a feeder event at the same instant put the feeder event first, because the
// person decided in response to what the graph already knew.
func WithHumanDecisions(m *Manifest, events []Event) ([]Event, error) {
	if len(m.HumanDecisions) == 0 {
		return events, nil
	}
	decisions, err := HumanDecisionEvents(m)
	if err != nil {
		return nil, err
	}
	merged := make([]Event, 0, len(events)+len(decisions))
	merged = append(merged, events...)
	merged = append(merged, decisions...)
	sort.SliceStable(merged, func(i, j int) bool {
		return merged[i].ObservedAt.Before(merged[j].ObservedAt)
	})
	for i := range merged {
		merged[i].AppendedSeq = int64(i + 1)
	}
	return merged, nil
}

// HumanDecisionEvents turns the manifest's decisions into the events that carry them.
//
// The event id is the published deterministic one (graph.HumanEventID), which is what makes a
// fixture's decisions idempotent: loading the fixture twice, or double-delivering it as the
// verifier does, re-submits the same ids and gets DUPLICATE_NOOP rather than a second merge.
func HumanDecisionEvents(m *Manifest) ([]Event, error) {
	out := make([]Event, 0, len(m.HumanDecisions))
	for i, decision := range m.HumanDecisions {
		kind, refs, err := decision.Kind()
		if err != nil {
			return nil, fmt.Errorf("fixture: %s: human_decisions[%d]: %w", m.ID, i, err)
		}
		env := &graphv1.EventEnvelope{
			EventId:       graph.HumanEventID(kind, refs, decision.Principal, decision.Reason),
			SourceId:      HumanSourceID,
			SchemaVersion: m.SchemaVersion,
		}
		env.IdempotencyKey = env.EventId
		if err := setDecisionBody(env, kind, refs, decision.Reason); err != nil {
			return nil, fmt.Errorf("fixture: %s: human_decisions[%d]: %w", m.ID, i, err)
		}
		out = append(out, Event{
			Envelope:   env,
			ObservedAt: decision.At.UTC(),
			Principal:  decision.Principal,
		})
	}
	return out, nil
}

// setDecisionBody fills in the event body for one decision kind.
func setDecisionBody(env *graphv1.EventEnvelope, kind string, refs []string, reason string) error {
	switch kind {
	case "confirm":
		env.Body = &graphv1.EventEnvelope_ConfirmMerge{
			ConfirmMerge: &graphv1.ConfirmMerge{EntityA: refs[0], EntityB: refs[1], Rationale: reason}}
	case "reject":
		env.Body = &graphv1.EventEnvelope_RejectMerge{
			RejectMerge: &graphv1.RejectMerge{EntityA: refs[0], EntityB: refs[1], Rationale: reason}}
	case "merge":
		env.Body = &graphv1.EventEnvelope_ManualMerge{
			ManualMerge: &graphv1.ManualMerge{EntityA: refs[0], EntityB: refs[1], Rationale: reason}}
	case "split":
		detach := make([]*graphv1.Ref, 0, len(refs)-1)
		for _, raw := range refs[1:] {
			ref, err := graph.ParseRef(raw)
			if err != nil {
				return fmt.Errorf("detach %q: %w", raw, err)
			}
			detach = append(detach, ref.Proto())
		}
		env.Body = &graphv1.EventEnvelope_SplitEntity{
			SplitEntity: &graphv1.SplitEntity{EntityId: refs[0], DetachClaims: detach, Rationale: reason}}
	default:
		return fmt.Errorf("unknown decision kind %q", kind)
	}
	return nil
}

// load is Load with the manifest and events already read, so the verifier does not parse them
// once per pass.
func load(ctx context.Context, p *projector.Projector, m *Manifest, events []Event, opts LoadOptions) (*LoadReport, error) {
	if err := registerSources(ctx, p, m); err != nil {
		return nil, err
	}

	report := &LoadReport{Results: make([]*graphv1.IngestResult, 0, len(events))}
	batch := opts.batchSize()
	for start := 0; start < len(events); start += batch {
		end := min(start+batch, len(events))
		results := make([]*graphv1.IngestResult, 0, end-start)
		if err := p.Store().WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			results = results[:0]
			for _, event := range events[start:end] {
				result, err := p.ApplyInTx(ctx, tx, event.Envelope, projector.ApplyOptions{
					ObservedAt: event.ObservedAt,
					Principal:  event.principalOr(opts.Principal),
				})
				if err != nil {
					return fmt.Errorf("fixture: apply %s: %w", event.Envelope.GetEventId(), err)
				}
				results = append(results, result)
			}
			return nil
		}); err != nil {
			return nil, err
		}
		// Between batches, never inside one: a rule whose answer depends on the graph — C8's shared
		// target — is re-evaluated in transactions of its own, because two merges in one transaction are
		// refused (projector/retrigger.go). A replay that skipped this would produce a different graph
		// from a live run, which is the one thing a fixture exists to rule out.
		if err := p.DrainPendingResolution(ctx); err != nil {
			return nil, fmt.Errorf("fixture: drain pending resolution: %w", err)
		}
		for i, result := range results {
			event := events[start+i]
			report.count(result)
			if result.GetStatus() == graphv1.IngestResult_REJECTED {
				report.RejectedMismatches = append(report.RejectedMismatches, fmt.Sprintf(
					"%s (%s line %d) was REJECTED (%s: %s); events.jsonl holds accepted events only",
					event.Envelope.GetEventId(), m.Events, event.AppendedSeq,
					result.GetReasonCode(), result.GetReasonDetail()))
			}
		}
	}

	if err := submitRejected(ctx, p, m, opts, report); err != nil {
		return nil, err
	}
	return report, nil
}

func (r *LoadReport) count(result *graphv1.IngestResult) {
	r.Results = append(r.Results, result)
	switch result.GetStatus() {
	case graphv1.IngestResult_APPLIED:
		r.Applied++
	case graphv1.IngestResult_DUPLICATE_NOOP:
		r.DuplicateNoop++
	case graphv1.IngestResult_REJECTED:
		r.Rejected++
	default:
	}
}

// HumanSourceID is the source id a human decision is logged under. It matches the id the server
// registers (internal/server/resolution.go); the two are spelled separately because the fixture
// package cannot import the server without a cycle.
const HumanSourceID = "human"

// principalOr returns the event's own principal, falling back to the load's default. Feeder
// events have none; a human decision carries the individual who made it (FR-041).
func (e Event) principalOr(fallback string) string {
	if e.Principal != "" {
		return e.Principal
	}
	return fallback
}

// registerSources declares every feeder before it may append (FR-018). Registering a source
// that is already registered is a no-op, so this is safe on a store that has already been
// loaded.
func registerSources(ctx context.Context, p *projector.Projector, m *Manifest) error {
	if len(m.HumanDecisions) > 0 {
		if err := p.RegisterSource(ctx, eventlog.Source{
			SourceID:      HumanSourceID,
			Kind:          "human",
			Ordering:      "none",
			SchemaVersion: m.SchemaVersion,
		}); err != nil {
			return fmt.Errorf("fixture: register the human decision source: %w", err)
		}
	}
	for _, src := range m.Sources {
		if err := p.RegisterSource(ctx, eventlog.Source{
			SourceID:         src.SourceID,
			Kind:             src.Kind,
			Ordering:         src.Ordering,
			ReorderingWindow: src.ReorderingWindow,
			SchemaVersion:    m.SchemaVersion,
		}); err != nil {
			return fmt.Errorf("fixture: register source %s: %w", src.SourceID, err)
		}
	}
	return nil
}

// submitRejected submits the fixture's must-be-rejected events and checks the contract around
// them: refused, refused for the stated reason, and with the graph left exactly as it was.
func submitRejected(ctx context.Context, p *projector.Projector, m *Manifest, opts LoadOptions, report *LoadReport) error {
	path := m.RejectedPath()
	if path == "" {
		if len(m.ExpectRejected) > 0 {
			report.RejectedMismatches = append(report.RejectedMismatches,
				"manifest names expect_rejected events but no rejected_events file; "+
					"a hand-authored fixture states them in rejected.jsonl (fixtures/README.md)")
		}
		return nil
	}

	rejected, err := ReadRejected(path)
	if err != nil {
		return err
	}
	expected := make(map[string]string, len(m.ExpectRejected))
	for _, e := range m.ExpectRejected {
		expected[e.EventID] = e.ReasonCode
	}

	before, err := snapshot(ctx, p.Store())
	if err != nil {
		return err
	}

	seen := map[string]bool{}
	for _, event := range rejected {
		id := event.Envelope.GetEventId()
		seen[id] = true

		result, err := p.ApplyWithOptions(ctx, event.Envelope, projector.ApplyOptions{
			ObservedAt: rejectedObservedAt(event),
			Principal:  opts.Principal,
		})
		if err != nil {
			return fmt.Errorf("fixture: submit rejected event %s: %w", id, err)
		}
		report.count(result)

		want, declared := expected[id]
		switch {
		case result.GetStatus() != graphv1.IngestResult_REJECTED:
			report.RejectedMismatches = append(report.RejectedMismatches, fmt.Sprintf(
				"%s: status %s, want REJECTED", id, result.GetStatus()))
		case !declared:
			report.RejectedMismatches = append(report.RejectedMismatches, fmt.Sprintf(
				"%s: refused with %q but the manifest has no expect_rejected entry for it", id, result.GetReasonCode()))
		case result.GetReasonCode() != want:
			report.RejectedMismatches = append(report.RejectedMismatches, fmt.Sprintf(
				"%s: reason_code %q, want %q (detail: %s)", id, result.GetReasonCode(), want, result.GetReasonDetail()))
		}
	}

	for _, e := range m.ExpectRejected {
		if !seen[e.EventID] {
			report.RejectedMismatches = append(report.RejectedMismatches, fmt.Sprintf(
				"%s: expected to be rejected with %q but %s does not contain it",
				e.EventID, e.ReasonCode, m.RejectedEvents))
		}
	}

	after, err := snapshot(ctx, p.Store())
	if err != nil {
		return err
	}
	if after != before {
		report.RejectedMismatches = append(report.RejectedMismatches,
			"a rejected event changed the graph; a refused event must not alter state (FR-024)")
	}
	return nil
}

// rejectedObservedAt picks the observed time to submit a must-be-rejected event with. The file
// normally carries none, since the log never assigned one, so the source's own timestamp is
// used; failing that the log stamps its clock, which is what a live feeder would get.
func rejectedObservedAt(event Event) time.Time {
	if !event.ObservedAt.IsZero() {
		return event.ObservedAt
	}
	if ts := event.Envelope.GetSourceObservedAt(); ts != nil {
		return ts.AsTime().UTC()
	}
	return time.Time{}
}
