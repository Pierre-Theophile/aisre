// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// A re-read node's states each get an id of their own, and the ids behave as idempotency keys should
// (003 T183): an unchanged re-poll repeats the id, a change gets a new one, and a change back to an
// earlier state is not mistaken for the earlier assertion.
func TestEachStateOfAReReadNodeHasItsOwnID(t *testing.T) {
	f := &Feeder{states: map[string]assertedState{}}
	created := time.Date(2026, 8, 22, 14, 0, 0, 0, time.UTC)
	poll := func(n int) time.Time {
		return time.Date(2026, 9, 21, 14, 0, 0, 0, time.UTC).Add(time.Duration(n) * 30 * time.Minute)
	}
	state := func(flags string) feeder.NodeFact {
		props, err := feeder.NewProps().Str("sre.gcp.sql_database_flags", flags).Build()
		if err != nil {
			t.Fatal(err)
		}
		return feeder.NodeFact{DisplayName: "orders-primary", Props: props, ValidAt: created}
	}
	assert := func(fact feeder.NodeFact, at time.Time) (string, feeder.NodeFact) {
		t.Helper()
		id, out, err := f.stateAssertion("gcp:twin", "sql_instance", "p/r/orders-primary", fact, at, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		return id, out
	}

	first, firstFact := assert(state("max_connections=200"), poll(0))
	if !firstFact.ValidAt.Equal(created) || firstFact.ValidFromUnknown {
		t.Errorf("the first state is dated %v (unknown=%v); it keeps the node's own start", firstFact.ValidAt, firstFact.ValidFromUnknown)
	}
	if again, _ := assert(state("max_connections=200"), poll(1)); again != first {
		t.Errorf("an unchanged re-poll sent %q, not the id already sent (%q); it would not be a no-op", again, first)
	}
	changed, changedFact := assert(state("max_connections=500"), poll(2))
	if changed == first {
		t.Fatal("a changed state reused the first state's id, so the graph would drop it as a duplicate")
	}
	if !changedFact.ValidAt.Equal(poll(2)) || !changedFact.ValidFromUnknown {
		t.Errorf("the changed state is dated %v (unknown=%v); with no stated instant it begins, unknown, at the observation",
			changedFact.ValidAt, changedFact.ValidFromUnknown)
	}
	back, _ := assert(state("max_connections=200"), poll(3))
	if back == first || back == changed {
		t.Errorf("a change back to the first state reused an earlier id (%q), so it would be dropped", back)
	}
}

// Where the platform states when the resource last changed — an alert policy's mutation record — a
// later state is dated from it rather than marked unknown.
func TestALaterStateIsDatedFromAStatedInstant(t *testing.T) {
	f := &Feeder{states: map[string]assertedState{}}
	created := time.Date(2026, 8, 22, 14, 0, 0, 0, time.UTC)
	mutated := time.Date(2026, 9, 21, 14, 12, 0, 0, time.UTC)
	fact := func(threshold string) feeder.NodeFact {
		props, err := feeder.NewProps().Str("sre.gcp.threshold", threshold).Build()
		if err != nil {
			t.Fatal(err)
		}
		return feeder.NodeFact{DisplayName: "latency", Props: props, ValidAt: created}
	}
	if _, _, err := f.stateAssertion("gcp:twin", "alert_policy", "p/123", fact("0.5"), created, time.Time{}); err != nil {
		t.Fatal(err)
	}
	_, later, err := f.stateAssertion("gcp:twin", "alert_policy", "p/123", fact("0.9"), mutated.Add(18*time.Minute), mutated)
	if err != nil {
		t.Fatal(err)
	}
	if !later.ValidAt.Equal(mutated) || later.ValidFromUnknown {
		t.Errorf("the later state is dated %v (unknown=%v); want the stated mutation instant %v",
			later.ValidAt, later.ValidFromUnknown, mutated)
	}
}
