// SPDX-License-Identifier: Apache-2.0

package feeder_test

import (
	"context"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// Named query operations (005 T005–T006, ADR-0010 item 3).
//
// Datadog's log search and aggregation are POST with the query in the body. The rule admits exactly
// that, declared per operation with a published justification, and nothing more: these tests are the
// "nothing more".

const searchLogs feeder.ReadOperation = "POST /api/v2/logs/events/search"

func namedQuery() feeder.ReadOperationSpec {
	return feeder.ReadOperationSpec{
		Area:       "logs",
		Why:        "named query: returns the log events matching the query in the body; no field creates, updates or deletes anything",
		NamedQuery: true,
	}
}

// A POST declared as a named query, with its justification, is admissible and is not a state change —
// in the table and in a cycle's log alike.
func TestADeclaredNamedQueryIsARead(t *testing.T) {
	t.Parallel()
	ops := reads()
	ops[searchLogs] = namedQuery()
	s := surface(t, ops)
	if _, err := s.Issuable(searchLogs); err != nil {
		t.Fatalf("a declared named query was refused: %v", err)
	}
	if n := s.StateChanges(); n != 0 {
		t.Errorf("the table counts %d state changes; a named query is a read", n)
	}
	// A metered issuer, so the call is actually recorded as issued: an unmetered run records nothing
	// and would make the log's count vacuously zero.
	budget, err := feeder.NewQuotaBudget("testdog", feeder.QuotaPolicy{Share: 0.5, Reserve: 1})
	if err != nil {
		t.Fatal(err)
	}
	budget.Observe(feeder.Reading{Family: "logs", Limit: 100, Remaining: 100,
		Reset: issueWindow.Add(time.Hour), Source: feeder.QuotaReported})
	issuer, err := feeder.NewIssuer(s, budget, &feeder.AreaStats{})
	if err != nil {
		t.Fatal(err)
	}
	if err := issuer.Issue(context.Background(), searchLogs, "logs", issueWindow); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if row := recordOf(t, issuer, searchLogs); row.Issued != 1 {
		t.Fatalf("the named query was not recorded as issued: %+v", row)
	}
	if n := issuer.StateChanges(); n != 0 {
		t.Errorf("the cycle's log counts %d state changes for a named query", n)
	}
}

// Every way of getting a write past the rule is refused at construction.
func TestNamedQueriesAdmitOnlyAJustifiedPost(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		op   feeder.ReadOperation
		spec feeder.ReadOperationSpec
	}{
		"an undeclared POST":                 {searchLogs, feeder.ReadOperationSpec{Area: "logs", Why: "search logs"}},
		"a PUT declared as a named query":    {"PUT /api/v1/monitor/{monitor_id}", namedQuery()},
		"a PATCH declared as a named query":  {"PATCH /api/v1/monitor/{monitor_id}", namedQuery()},
		"a DELETE declared as a named query": {"DELETE /api/v1/monitor/{monitor_id}", namedQuery()},
		"a named query with no justification": {searchLogs, feeder.ReadOperationSpec{
			Area: "logs", Why: "search logs", NamedQuery: true}},
		"a GET declared as a named query": {"GET /api/v1/monitor", namedQuery()},
	}
	for name, c := range cases {
		ops := reads()
		ops[c.op] = c.spec
		if _, err := feeder.NewReadOnlySurface("testdog", ops); err == nil {
			t.Errorf("%s: a surface was built around %q; only a POST declared as a named query, with "+
				"its published justification, is admissible", name, c.op)
		}
	}
}

// The init-time panic still fires for an undeclared POST, which is what makes a binary containing one
// unbuildable rather than untested.
func TestMustReadOnlySurfacePanicsOnAnUndeclaredPost(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Error("MustReadOnlySurface accepted an undeclared POST")
		}
	}()
	ops := reads()
	ops[searchLogs] = feeder.ReadOperationSpec{Area: "logs", Why: "search logs"}
	feeder.MustReadOnlySurface("testdog", ops)
}
