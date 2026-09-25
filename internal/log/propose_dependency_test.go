// SPDX-License-Identifier: Apache-2.0

package log_test

import (
	"os"
	"regexp"
	"strings"
	"testing"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	eventlog "github.com/Pierre-Theophile/aisre/internal/log"
)

// The sixth event body (003 FR-029, plan item 2).
//
// A proposed dependency says an EDGE may exist between two different entities. Resolution could
// already suggest that two refs name the same ENTITY; nothing could suggest the former, so a
// feeder with suggestive but inconclusive evidence had to either invent an edge or drop the
// evidence, and both are worse than a stated gap.

func proposeDependencyEnvelope(body *graphv1.ProposeDependency) *graphv1.EventEnvelope {
	return &graphv1.EventEnvelope{
		EventId:       "gcp:demo:e1",
		SourceId:      "gcp:demo",
		SchemaVersion: "1.0.0",
		Body:          &graphv1.EventEnvelope_ProposeDependency{ProposeDependency: body},
	}
}

func TestProposeDependencyIsNamedAndCarried(t *testing.T) {
	t.Parallel()

	// A body the switch does not know returns "", which the NOT NULL column would refuse —
	// loudly, but at the wrong layer and after the feeder believed it had emitted something.
	env := proposeDependencyEnvelope(&graphv1.ProposeDependency{
		Src:  &graphv1.Ref{Namespace: "gcp.service", Value: "checkout"},
		Dst:  &graphv1.Ref{Namespace: "gcp.sql", Value: "orders-db"},
		Type: graphv1.EdgeType_DEPENDS_ON,
	})

	if got := eventlog.EventType(env); got != "propose_dependency" {
		t.Errorf("EventType = %q, want propose_dependency", got)
	}
	if eventlog.BodyMessage(env) == nil {
		t.Error("BodyMessage = nil; the body would be stored empty")
	}
}

// TestProposeDependencyIsAcceptedByTheStoredVocabulary keeps the Go switch and the SQL constraint
// from drifting.
//
// They are two lists of the same vocabulary in two languages, and 0001 says adding a type is a
// migration plus a version bump. Nothing enforced that the two agreed: a body added to the switch
// without a migration is refused by the database at insert time, and a type added to the migration
// without the switch is unreachable. Both are silent until a feeder emits one.
func TestProposeDependencyIsAcceptedByTheStoredVocabulary(t *testing.T) {
	t.Parallel()

	// 0010 is the latest migration to restate the constraint, so it is the authoritative list.
	// Reading 0009 here instead would pass while the database refused two of the three types.
	sql, err := os.ReadFile("../store/postgres/migrations/0010_proposed_dependencies.sql")
	if err != nil {
		t.Fatalf("read migration 0010: %v", err)
	}

	// The constraint's member list, as the migration actually writes it.
	constraint := regexp.MustCompile(`(?s)events_type_check CHECK \(type IN \((.*?)\)\)`).
		FindStringSubmatch(string(sql))
	if constraint == nil {
		t.Fatal("migration 0010 does not declare events_type_check")
	}
	stored := map[string]bool{}
	for _, line := range strings.Split(constraint[1], "\n") {
		if m := regexp.MustCompile(`'([a-z_]+)'`).FindStringSubmatch(line); m != nil {
			stored[m[1]] = true
		}
	}

	// Every body the switch names must be a member the database accepts.
	for _, env := range []*graphv1.EventEnvelope{
		proposeDependencyEnvelope(&graphv1.ProposeDependency{}),
		{Body: &graphv1.EventEnvelope_ConfirmDependency{ConfirmDependency: &graphv1.ConfirmDependency{}}},
		{Body: &graphv1.EventEnvelope_RejectDependency{RejectDependency: &graphv1.RejectDependency{}}},
		investigationEnvelope(&graphv1.SubmitHumanFact{}),
		investigationEnvelope(&graphv1.LabelInvestigation{}),
		{Body: &graphv1.EventEnvelope_UpsertNode{UpsertNode: &graphv1.UpsertNode{}}},
		{Body: &graphv1.EventEnvelope_ObserveChange{ObserveChange: &graphv1.ObserveChange{}}},
		{Body: &graphv1.EventEnvelope_SourceCheckpoint{SourceCheckpoint: &graphv1.SourceCheckpoint{}}},
	} {
		typ := eventlog.EventType(env)
		if typ == "" {
			t.Errorf("EventType returned \"\" for %T", env.GetBody())
			continue
		}
		if !stored[typ] {
			t.Errorf("EventType %q is not in migration 0009's events_type_check; the database "+
				"would refuse every event of this type at insert time", typ)
		}
	}

	// And the constraint replaces rather than relaxes: an unknown type is still refused.
	if stored["something_nobody_declared"] {
		t.Error("the constraint admits an undeclared type")
	}
}
