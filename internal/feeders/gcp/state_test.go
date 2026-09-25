// SPDX-License-Identifier: Apache-2.0

package gcp_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	gcpfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/gcp"
)

// Checkpoints, the silence rule, the horizon and the recreated name (T060–T062).

func scope() gcpfeeder.Scope {
	return gcpfeeder.Scope{Projects: []string{"nova-production"}, Regions: []string{"europe-west1"}}
}

func completePoll(at time.Time, observed ...string) gcpfeeder.Poll {
	return gcpfeeder.Poll{
		Outcome:  gcpfeeder.PollComplete,
		Scope:    scope(),
		At:       at,
		Observed: map[string][]string{gcpfeeder.NSService: observed},
	}
}

// §8.1, the single rule that keeps a network blip from looking like the organisation deleting half
// its estate: a partial poll retracts nothing **and does not count toward N**.
func TestAPartialPollRetractsNothingAndCountsTowardNothing(t *testing.T) {
	tracker, err := gcpfeeder.NewSilenceTracker(3)
	if err != nil {
		t.Fatalf("NewSilenceTracker: %v", err)
	}
	base := time.Date(2026, 9, 21, 14, 0, 0, 0, time.UTC)
	const service = "nova-production/europe-west1/checkout"

	if _, err := tracker.Observe(completePoll(base, service), base); err != nil {
		t.Fatalf("Observe: %v", err)
	}

	// Three partial polls in a row, none of which saw the service.
	for i := 1; i <= 3; i++ {
		partial := gcpfeeder.Poll{
			Outcome: gcpfeeder.PollPartial,
			Scope:   scope(),
			At:      base.Add(time.Duration(i) * time.Minute),
			Err:     errors.New("the region call timed out"),
		}
		retractions, err := tracker.Observe(partial, base)
		if err != nil {
			t.Fatalf("Observe partial: %v", err)
		}
		if len(retractions) != 0 {
			t.Fatalf("a partial poll retracted %d entities; only a complete poll is evidence of "+
				"absence (FR-012)", len(retractions))
		}
	}

	// A single complete poll that misses it is now the *first* miss, not the fourth. If the partial
	// polls had counted, this one would retract.
	retractions, err := tracker.Observe(completePoll(base.Add(4*time.Minute)), base)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if len(retractions) != 0 {
		t.Fatalf("the partial polls counted toward the threshold: %d retractions after one complete "+
			"miss with N=3", len(retractions))
	}
}

// N consecutive complete polls retract, with a valid end inside the last poll that saw it — the
// tightest interval the evidence supports.
func TestTheSilenceRuleRetractsAfterNCompletePollsAtTheLastInstantItWasSeen(t *testing.T) {
	tracker, err := gcpfeeder.NewSilenceTracker(3)
	if err != nil {
		t.Fatalf("NewSilenceTracker: %v", err)
	}
	base := time.Date(2026, 9, 21, 14, 0, 0, 0, time.UTC)
	const service = "nova-production/europe-west1/checkout"
	lastSeen := base.Add(time.Minute)

	if _, err := tracker.Observe(completePoll(base, service), base); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if _, err := tracker.Observe(completePoll(lastSeen, service), base); err != nil {
		t.Fatalf("Observe: %v", err)
	}

	for i := 1; i <= 2; i++ {
		retractions, err := tracker.Observe(completePoll(lastSeen.Add(time.Duration(i)*time.Minute)), base)
		if err != nil {
			t.Fatalf("Observe: %v", err)
		}
		if len(retractions) != 0 {
			t.Fatalf("retracted after %d misses with N=3", i)
		}
	}
	retractions, err := tracker.Observe(completePoll(lastSeen.Add(3*time.Minute)), base)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if len(retractions) != 1 {
		t.Fatalf("retractions = %d after 3 misses, want 1", len(retractions))
	}
	got := retractions[0]
	if got.Value != service {
		t.Errorf("retracted %q, want %q", got.Value, service)
	}
	if !got.ValidEnd.Equal(lastSeen) {
		t.Fatalf("valid end = %s, want the last poll that saw it (%s). The instant of the poll that "+
			"noticed the absence would be wrong by up to N poll intervals — the entity was gone "+
			"before that, the feeder only just noticed", got.ValidEnd, lastSeen)
	}
	if got.Why != gcpfeeder.RetractedBySilence {
		t.Errorf("reason = %q", got.Why)
	}
}

// A stated deletion instant wins over an inferred bound (FR-023), and the entity is dropped from the
// tracker so the silence rule does not later retract it again at a worse instant.
func TestAnAuditDeletionInstantWinsOverTheInferredBound(t *testing.T) {
	tracker, err := gcpfeeder.NewSilenceTracker(2)
	if err != nil {
		t.Fatalf("NewSilenceTracker: %v", err)
	}
	base := time.Date(2026, 9, 21, 14, 0, 0, 0, time.UTC)
	const service = "nova-production/europe-west1/checkout"
	if _, err := tracker.Observe(completePoll(base, service), base); err != nil {
		t.Fatalf("Observe: %v", err)
	}

	deleted := base.Add(30 * time.Second)
	stated := tracker.RetractAtAuditDeletion(gcpfeeder.NSService, service, deleted)
	if !stated.ValidEnd.Equal(deleted) {
		t.Fatalf("valid end = %s, want the stated deletion instant %s", stated.ValidEnd, deleted)
	}
	if stated.Why != gcpfeeder.RetractedByAuditDeletion {
		t.Errorf("reason = %q", stated.Why)
	}

	// And the silence rule does not retract it a second time.
	for i := 1; i <= 3; i++ {
		retractions, err := tracker.Observe(completePoll(base.Add(time.Duration(i)*time.Minute)), base)
		if err != nil {
			t.Fatalf("Observe: %v", err)
		}
		for _, r := range retractions {
			if r.Value == service {
				t.Fatalf("the silence rule retracted an already-retracted entity at %s", r.ValidEnd)
			}
		}
	}
}

// An entity in a project that left the scope is "not in scope", not "absent" — retracting it would
// turn a scope change into a mass deletion (FR-009).
func TestAScopeChangeIsNotAMassDeletion(t *testing.T) {
	tracker, err := gcpfeeder.NewSilenceTracker(1)
	if err != nil {
		t.Fatalf("NewSilenceTracker: %v", err)
	}
	base := time.Date(2026, 9, 21, 14, 0, 0, 0, time.UTC)
	const service = "nova-staging/europe-west1/checkout"

	wide := completePoll(base, service)
	wide.Scope = gcpfeeder.Scope{Projects: []string{"nova-production", "nova-staging"}, Regions: []string{"europe-west1"}}
	if _, err := tracker.Observe(wide, base); err != nil {
		t.Fatalf("Observe: %v", err)
	}

	// The next poll covers only production. The staging service is out of scope, not gone.
	narrow := completePoll(base.Add(time.Minute))
	retractions, err := tracker.Observe(narrow, base)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	for _, r := range retractions {
		if r.Value == service {
			t.Fatal("narrowing the scope retracted an entity in the project that left it (FR-009)")
		}
	}
}

// A threshold below 1 retracts everything on the first missed poll — the flake-is-a-deletion bug with
// the safety rail removed.
func TestASilenceThresholdBelowOneIsRefused(t *testing.T) {
	for _, n := range []int{0, -1} {
		if _, err := gcpfeeder.NewSilenceTracker(n); err == nil {
			t.Errorf("NewSilenceTracker(%d) was accepted", n)
		}
	}
}

// A poll with no outcome has no default: a partial poll silently typed as complete is the retraction
// bug above.
func TestAPollWithNoOutcomeIsRefused(t *testing.T) {
	if err := (gcpfeeder.Poll{Scope: scope(), At: time.Now()}).Validate(); err == nil {
		t.Fatal("a poll with no outcome validated")
	}
	partial := gcpfeeder.Poll{Outcome: gcpfeeder.PollPartial, Scope: scope(), At: time.Now()}
	if err := partial.Validate(); err == nil {
		t.Fatal("a partial poll with no reason validated; the checkpoint has to declare the gap")
	}
	partial.Err = errors.New("timed out")
	if err := partial.Validate(); err != nil {
		t.Errorf("a partial poll with a reason was refused: %v", err)
	}
}

// A query reaching past the horizon is told so: "we did not look that far back" is a different answer
// from "there was no revision" (FR-024).
func TestAQueryPastTheHorizonIsToldSo(t *testing.T) {
	horizon := gcpfeeder.Horizon{
		Earliest: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		Reason:   gcpfeeder.HorizonConfigured,
	}
	if err := horizon.Check(time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("a query inside the horizon was refused: %v", err)
	}
	err := horizon.Check(time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC))
	if err == nil {
		t.Fatal("a query before the horizon was answered rather than told so (FR-024)")
	}
	var past *gcpfeeder.ErrPastHorizon
	if !errors.As(err, &past) {
		t.Fatalf("the refusal is %T, not *ErrPastHorizon", err)
	}
	if !strings.Contains(err.Error(), horizon.Reason) {
		t.Errorf("the refusal does not say why the horizon stops there: %q", err.Error())
	}
}

// A checkpoint that could mislead is refused, and its note is deterministic because it reaches a
// golden.
func TestACheckpointStatesWhatSectionEightRequires(t *testing.T) {
	base := time.Date(2026, 9, 21, 14, 0, 0, 0, time.UTC)
	checkpoint := gcpfeeder.Checkpoint{
		From:               base,
		To:                 base.Add(time.Minute),
		Outcome:            gcpfeeder.PollComplete,
		Scope:              scope(),
		Horizon:            gcpfeeder.Horizon{Earliest: base.Add(-720 * time.Hour), Reason: gcpfeeder.HorizonConfigured},
		AuditFilters:       []string{"logName=activity", "service=run.googleapis.com"},
		EnvironmentMapping: map[string]string{"nova-production": "production"},
		ReorderingWindow:   10 * time.Minute,
		OmittedSurfaces:    []string{"cloud_dns", "load_balancers"},
		MonitoringLimit:    60,
	}
	if err := checkpoint.Validate(); err != nil {
		t.Fatalf("a well-formed checkpoint was refused: %v", err)
	}
	note := checkpoint.Note()
	for _, want := range []string{"poll=complete", "nova-production", "horizon=", "audit_filters=",
		"environment_mapping=", "reordering_window=", "omitted_surfaces=", "monitoring_limit=60"} {
		if !strings.Contains(note, want) {
			t.Errorf("the checkpoint note omits %q: %s", want, note)
		}
	}
	for i := 0; i < 5; i++ {
		if again := checkpoint.Note(); again != note {
			t.Fatalf("the checkpoint note is not deterministic:\n%s\n%s", note, again)
		}
	}

	// The refusals.
	noScope := checkpoint
	noScope.Scope = gcpfeeder.Scope{}
	if err := noScope.Validate(); err == nil {
		t.Error("a checkpoint with an empty project scope validated; a reader could not tell " +
			"\"not present\" from \"not in scope\" (FR-009)")
	}
	noOutcome := checkpoint
	noOutcome.Outcome = gcpfeeder.PollUnknown
	if err := noOutcome.Validate(); err == nil {
		t.Error("a checkpoint with no poll outcome validated")
	}
	partial := checkpoint
	partial.Outcome = gcpfeeder.PollPartial
	if err := partial.Validate(); err == nil {
		t.Error("a partial checkpoint with no gap reason validated")
	}
}

// A service deleted and recreated under one name is not one continuous entity: the graph would
// otherwise hold a single service whose configuration changed at the recreation instant, losing the
// deletion and attributing the new service's history to the old one (FR-025).
func TestARecreatedNameIsNotOneContinuousEntity(t *testing.T) {
	service := gcpfeeder.Service{Project: "nova-production", Region: "europe-west1", Name: "checkout"}
	before := gcpfeeder.Identity{UID: "uid-old", CreateTime: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)}
	after := gcpfeeder.Identity{UID: "uid-new", CreateTime: time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)}

	rec, ok := gcpfeeder.DetectRecreation(service, before, after)
	if !ok {
		t.Fatal("a changed uid under one name was not detected as a recreation (FR-025)")
	}
	if rec.OldUID != "uid-old" || rec.NewUID != "uid-new" {
		t.Errorf("the recreation does not carry both identifiers: %+v", rec)
	}
	if len(rec.LinkingEvidence) == 0 {
		t.Error("the recreation carries no linking evidence; the link is emitted as claims for the " +
			"resolution layer rather than asserted or discarded")
	}
	if !strings.Contains(rec.String(), "not one continuous entity") {
		t.Errorf("the note does not state the rule: %q", rec.String())
	}

	// An unchanged uid is the same service, whatever else moved.
	if _, ok := gcpfeeder.DetectRecreation(service, before, gcpfeeder.Identity{UID: "uid-old", CreateTime: after.CreateTime}); ok {
		t.Error("an unchanged uid was reported as a recreation")
	}
	// A missing uid on either side is a gap in the evidence, not a recreation: guessing from
	// createTime would call a service recreated every time a poll read a stale cache.
	if _, ok := gcpfeeder.DetectRecreation(service, gcpfeeder.Identity{}, after); ok {
		t.Error("a missing previous uid was reported as a recreation")
	}
	if _, ok := gcpfeeder.DetectRecreation(service, before, gcpfeeder.Identity{}); ok {
		t.Error("a missing current uid was reported as a recreation")
	}
}
