// SPDX-License-Identifier: Apache-2.0

package feeder_test

import (
	"strings"
	"testing"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The rollback marker on a change (004 T023, T024; FR-016, research §1.4).
//
// A rollback is a ROLLOUT in every other respect — it moves production to a version, it has an actor,
// it has an instant — so it is marked rather than given a kind of its own. A distinct ChangeKind would
// make every consumer that ranks rollouts remember to include it, and the first one to forget would
// silently stop ranking the change an incident is most often about.

func rollbackDescription() feeder.Description {
	return feeder.Description{
		SourceID: "vercel:acme", Kind: "feeder", SchemaVersion: "1.0.0",
		Namespaces: []string{feeder.NSVercelChange},
	}
}

// The marker reaches the event, and the change stays a ROLLOUT.
func TestARollbackIsAMarkedRolloutAndNotAKindOfItsOwn(t *testing.T) {
	t.Parallel()

	env := feeder.ObserveChange(rollbackDescription(), "event-1", feeder.ChangeFact{
		Ref:          feeder.Ref(feeder.NSVercelChange, "dpl_9aBc"),
		Kind:         graphv1.ChangeKind_ROLLOUT,
		Summary:      "production rolled back to dpl_7xYz",
		Rollback:     true,
		RolledBackTo: "dpl_7xYz",
	})
	change := env.GetObserveChange().GetChange()

	if !change.GetRollback() {
		t.Error("the rollback marker did not reach the event")
	}
	if got := change.GetRolledBackTo(); got != "dpl_7xYz" {
		t.Errorf("rolled_back_to = %q, want the restored deployment", got)
	}
	if got := change.GetKind(); got != graphv1.ChangeKind_ROLLOUT {
		t.Errorf("a rollback's kind = %s, want ROLLOUT; a kind of its own would drop it out of every "+
			"consumer that ranks rollouts", got)
	}
	// And there is no ROLLBACK kind to reach for, which is what makes the line above hold in future.
	for name := range graphv1.ChangeKind_value {
		if strings.Contains(strings.ToUpper(name), "ROLLBACK") {
			t.Errorf("ChangeKind has a %s member; a rollback is a marked rollout, and a kind would "+
				"split the ranking of rollouts in two", name)
		}
	}
}

// An unstated target is "a rollback whose target we have not been told", never "not a rollback".
func TestARollbackWithNoStatedTargetIsStillARollback(t *testing.T) {
	t.Parallel()

	env := feeder.ObserveChange(rollbackDescription(), "event-2", feeder.ChangeFact{
		Ref:      feeder.Ref(feeder.NSVercelChange, "dpl_9aBc"),
		Kind:     graphv1.ChangeKind_ROLLOUT,
		Summary:  "production rolled back",
		Rollback: true,
	})
	change := env.GetObserveChange().GetChange()
	if !change.GetRollback() || change.GetRolledBackTo() != "" {
		t.Errorf("rollback=%v rolled_back_to=%q, want the marker set and the target empty",
			change.GetRollback(), change.GetRolledBackTo())
	}

	// And an ordinary rollout carries neither, so the zero value says nothing rather than saying no.
	ordinary := feeder.ObserveChange(rollbackDescription(), "event-3", feeder.ChangeFact{
		Ref:     feeder.Ref(feeder.NSVercelChange, "dpl_0000"),
		Kind:    graphv1.ChangeKind_ROLLOUT,
		Summary: "production deployment",
	}).GetObserveChange().GetChange()
	if ordinary.GetRollback() || ordinary.GetRolledBackTo() != "" {
		t.Error("an ordinary rollout came out marked as a rollback")
	}
}

// The observation and the engine's suggestion are different fields on different messages, so a query
// cannot confuse a proposal with history (T024).
//
// `grep rollback_candidate` finds `investigation.proto`, and it would be an easy mistake to reuse: it
// is what an operator MIGHT do next. This marker is that somebody already did it. The two are facts
// about different moments, and a consumer that read one as the other would report a suggestion as a
// thing that happened.
func TestTheObservedRollbackIsNotTheEnginesSuggestedOne(t *testing.T) {
	t.Parallel()

	// The engine's field lives on an investigation and names a candidate; it has no counterpart on Change.
	investigation := &investigationv1.Investigation{RollbackCandidate: "dpl_7xYz"}
	if investigation.GetRollbackCandidate() == "" {
		t.Fatal("the engine's suggestion field is not where this test believes it is")
	}

	// The observation lives on Change, and Change has no `rollback_candidate`: the two vocabularies do
	// not overlap, which is what keeps them un-confusable in a query.
	change := &graphv1.Change{Rollback: true, RolledBackTo: "dpl_7xYz"}
	fields := change.ProtoReflect().Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		if name := string(fields.Get(i).Name()); strings.Contains(name, "candidate") {
			t.Errorf("Change carries a field named %q; the engine's suggestion and this observation "+
				"must not share a spelling", name)
		}
	}

	// And the reverse: a Conclusion has no observed-rollback marker to be mistaken for one.
	investigationFields := investigation.ProtoReflect().Descriptor().Fields()
	for i := 0; i < investigationFields.Len(); i++ {
		name := string(investigationFields.Get(i).Name())
		if name == "rollback" || name == "rolled_back_to" {
			t.Errorf("Investigation carries %q; that is the observation's spelling, and a reader would "+
				"take the engine's proposal for something that happened", name)
		}
	}
}
