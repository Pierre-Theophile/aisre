// SPDX-License-Identifier: Apache-2.0

package backend_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/pkg/backend"
	"github.com/Pierre-Theophile/aisre/pkg/backend/testkit"
)

type stub struct{ d backend.Description }

func (s stub) Describe() backend.Description { return s.d }

func (s stub) Execute(context.Context, *backend.AlgebraRequest) (*backend.AlgebraResponse, error) {
	return nil, errors.New("stub: Execute lands with Phase 4")
}

func validDescription() backend.Description {
	return backend.Description{
		Name:   "recorded",
		Vendor: "recorded",
		Terms:  []string{"compare", "onset", "monitor_state"},
		CostClasses: map[string]backend.CostClass{
			"compare":       backend.CostClassStandard,
			"onset":         backend.CostClassExpensive,
			"monitor_state": backend.CostClassCheap,
		},
		Redaction:      &backend.RedactionPolicy{PolicyVersion: "1.0.0"},
		Version:        "0.1.0",
		AlgebraVersion: "1.0.0",
	}
}

func TestValidDeclarationPassesTheGate(t *testing.T) {
	t.Parallel()
	testkit.Declaration(t, stub{d: validDescription()})
}

func TestDescriptionValidateRejections(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		mutate  func(*backend.Description)
		mustSay string
	}{
		{
			name: "a graph term",
			mutate: func(d *backend.Description) {
				d.Terms[0] = "subgraph"
				d.CostClasses["subgraph"] = backend.CostClassCheap
			},
			mustSay: "telemetry family only",
		},
		{
			name: "a knowledge term",
			mutate: func(d *backend.Description) {
				d.Terms[0] = "knowledge_search"
				d.CostClasses["knowledge_search"] = backend.CostClassCheap
			},
			mustSay: "telemetry family only",
		},
		{
			name:    "a term the algebra does not publish",
			mutate:  func(d *backend.Description) { d.Terms[0] = "run_arbitrary_query" },
			mustSay: "does not publish",
		},
		{
			name:    "a term with no cost class",
			mutate:  func(d *backend.Description) { delete(d.CostClasses, "compare") },
			mustSay: "cheap, standard or expensive",
		},
		{
			name:    "a priced term the backend does not serve",
			mutate:  func(d *backend.Description) { d.CostClasses["exemplars"] = backend.CostClassExpensive },
			mustSay: "does not serve it",
		},
		{
			name:    "the same term declared twice",
			mutate:  func(d *backend.Description) { d.Terms = append(d.Terms, "compare") },
			mustSay: "twice",
		},
		{
			name:    "no terms at all",
			mutate:  func(d *backend.Description) { d.Terms = nil },
			mustSay: "never be called",
		},
		{
			name:    "no redaction policy version",
			mutate:  func(d *backend.Description) { d.Redaction = nil },
			mustSay: "redaction policy version",
		},
		{
			name:    "no algebra version",
			mutate:  func(d *backend.Description) { d.AlgebraVersion = "" },
			mustSay: "algebra version",
		},
		{
			name:    "no vendor",
			mutate:  func(d *backend.Description) { d.Vendor = "" },
			mustSay: "vendor",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := validDescription()
			tc.mutate(&d)
			err := d.Validate()
			if err == nil {
				t.Fatalf("Validate accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.mustSay) {
				t.Fatalf("error %q does not say %q", err, tc.mustSay)
			}
		})
	}
}

// The algebra's three families, as published. A term that moves between them, or appears
// without a family, is a change to the recorded worlds of every fixture, so it is spelled out
// here rather than derived.
func TestPublishedTermFamilies(t *testing.T) {
	t.Parallel()

	telemetry := backend.Terms(backend.FamilyTelemetry)
	want := []string{
		"compare", "drill_down", "error_spans", "errors_by_version",
		"exemplars", "monitor_state", "new_log_patterns", "onset",
	}
	if !slices.Equal(telemetry, want) {
		t.Errorf("telemetry terms = %v, want %v", telemetry, want)
	}
	if got := len(backend.Terms(backend.FamilyGraph)); got != 7 {
		t.Errorf("graph terms = %d, want the 7 published QueryService wrappers", got)
	}
	if got := backend.Terms(backend.FamilyKnowledge); !slices.Equal(got, []string{"knowledge_search"}) {
		t.Errorf("knowledge terms = %v, want [knowledge_search]", got)
	}
	if got := backend.FamilyOf("no_such_term"); got != backend.FamilyUnknown {
		t.Errorf("FamilyOf(unpublished) = %q, want FamilyUnknown", got)
	}
}

// The aliases must be the generated types, not copies of them: a backend author and the engine
// have to be unable to disagree about what a digest is.
func TestAliasesAreTheGeneratedTypes(t *testing.T) {
	t.Parallel()
	// Passing a generated message where the alias is expected only compiles when the two are
	// the same type, which is the whole claim.
	takesAliases := func(*backend.AlgebraRequest, *backend.AlgebraResponse, *backend.Coverage) {}
	takesAliases(&investigationv1.AlgebraRequest{}, &investigationv1.AlgebraResponse{}, &investigationv1.Coverage{})

	if backend.OutcomeNoData == backend.OutcomeNotRecorded {
		t.Fatal("NO_DATA and NOT_RECORDED are the same value; the typed outcomes must never collapse")
	}
}

// A world must say what redaction was applied to it, so a recorder built without a policy version
// is refused rather than producing a recording nobody can check against a policy (FR-038).
func TestRecorderRefusesAWorldThatCannotSayWhatWasAppliedToIt(t *testing.T) {
	t.Parallel()
	got, err := backend.NewRecorderWithOptions(t.TempDir(), backend.RecordOptions{DrillDownDepth: 1})
	if got != nil {
		t.Fatal("NewRecorderWithOptions returned a Recorder for a world with no redaction policy version")
	}
	if reason := backend.ReasonOf(err); reason != backend.ReasonUndeclaredRedaction {
		t.Fatalf("reason = %q, want %q (%v)", reason, backend.ReasonUndeclaredRedaction, err)
	}
}

// An empty world is a valid world: it records nothing, writes an index, and returns a digest
// that a later load checks. A recorder that failed here would make "this fixture exercises no
// telemetry" impossible to express.
func TestRecorderWritesAnIndexForAnEmptyWorld(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	recorder, err := backend.NewRecorderWithOptions(dir, backend.RecordOptions{
		DrillDownDepth:         1,
		RedactionPolicyVersion: "1.0.0",
	})
	if err != nil {
		t.Fatalf("new recorder: %v", err)
	}
	digest, err := recorder.Close(context.Background())
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if digest == "" {
		t.Fatal("Close returned no index digest; the digest is what a load checks the world against")
	}
	world, err := backend.LoadWorld(dir)
	if err != nil {
		t.Fatalf("load world: %v", err)
	}
	if world.Len() != 0 {
		t.Errorf("world holds %d answers, want 0", world.Len())
	}
	if world.Index.IndexDigest != digest {
		t.Errorf("index digest on disk = %q, want %q", world.Index.IndexDigest, digest)
	}
}
