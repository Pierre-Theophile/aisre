// SPDX-License-Identifier: Apache-2.0

package audit_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/investigation/audit"
)

// fixturePath resolves a synthetic audit fixture from this package's directory.
func fixturePath(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join("..", "..", "..", "fixtures", "audits", name, "incidents.yaml")
}

// loadFixture loads a synthetic audit fixture, failing the test if it does not parse. The
// fixtures are checked in and are part of the contract, so a parse failure here is a broken
// fixture rather than a skipped test.
func loadFixture(t *testing.T, name string) *audit.List {
	t.Helper()
	list, err := audit.LoadList(fixturePath(t, name))
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	return list
}

// validList is the smallest list that passes validation. Tests mutate one field of it at a time
// so that a rejection is attributable to the field, not to the shape.
const validList = `
version: 1
audit_id: t-01
run_at: 2026-09-17
author: test|auditor
corpus:
  label: test-org
  from: 2026-01-01
  to: 2026-09-01
feeder_sets:
  - name: base
    order: 1
    feeders: [otel.spans]
  - name: +deploy
    order: 2
    feeders: [cicd.deploys]
incidents:
  - ref: T-01
    alert_at: 2026-02-01
    cause: {category: iac_apply, class: change_induced}
    observability: {base: not_observable, +deploy: observed}
  - ref: T-02
    alert_at: 2026-03-01
    cause: {category: latent_bug, class: unobserved}
    observability: {base: not_observable, +deploy: not_observable}
`

func TestParseListAcceptsTheSyntheticFixtures(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"synthetic-01", "synthetic-02"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			list := loadFixture(t, name)
			if got, want := len(list.Incidents), 13; got != want {
				t.Errorf("incidents = %d, want %d", got, want)
			}
			if got, want := len(list.Corpus.Excluded), 3; got != want {
				t.Errorf("exclusions = %d, want %d", got, want)
			}
			if list.AuditID != name {
				t.Errorf("audit_id = %q, want %q", list.AuditID, name)
			}
		})
	}
}

// TestParseListAcceptsJSON proves the "YAML and JSON" half of FR-069's machine-readable input:
// one strict decoder reads both, so the two spellings cannot drift apart.
func TestParseListAcceptsJSON(t *testing.T) {
	t.Parallel()

	const asJSON = `{
	  "version": 1,
	  "audit_id": "t-json",
	  "run_at": "2026-09-17T00:00:00Z",
	  "author": "test|auditor",
	  "corpus": {"label": "test-org", "from": "2026-01-01", "to": "2026-09-01"},
	  "feeder_sets": [{"name": "base", "order": 1, "feeders": ["otel.spans"]}],
	  "incidents": [
	    {"ref": "T-01", "alert_at": "2026-02-01",
	     "cause": {"category": "iac_apply", "class": "change_induced"},
	     "observability": {"base": "observed"}}
	  ]
	}`

	list, err := audit.ParseList([]byte(asJSON))
	if err != nil {
		t.Fatalf("ParseList(JSON): %v", err)
	}
	if list.AuditID != "t-json" {
		t.Errorf("audit_id = %q, want t-json", list.AuditID)
	}
	if got := list.Incidents[0].Observability["base"].Verdict; got != audit.VerdictObserved {
		t.Errorf("verdict = %q, want %q", got, audit.VerdictObserved)
	}
	// An RFC 3339 run_at stays an instant; a date stays a date.
	if list.RunAt.DateOnly {
		t.Error("run_at parsed as date-only, want an instant")
	}
	if !list.Incidents[0].AlertAt.DateOnly {
		t.Error("alert_at parsed as an instant, want date-only")
	}
}

// TestObservationSpellings covers both spellings of a verdict: the bare scalar an auditor writes
// most of the time, and the mapping they need when the verdict is `undecidable`.
func TestObservationSpellings(t *testing.T) {
	t.Parallel()

	list, err := audit.ParseList([]byte(`
version: 1
audit_id: t-spell
run_at: 2026-09-17
author: test|auditor
corpus: {label: test-org, from: 2026-01-01, to: 2026-09-01}
feeder_sets: [{name: base, order: 1, feeders: [otel.spans]}]
incidents:
  - ref: T-01
    alert_at: 2026-02-01
    cause: {category: other, class: change_induced}
    observability:
      base: {verdict: undecidable, reason: no_record}
`))
	if err != nil {
		t.Fatalf("ParseList: %v", err)
	}
	observation := list.Incidents[0].Observability["base"]
	if observation.Verdict != audit.VerdictUndecidable {
		t.Errorf("verdict = %q, want undecidable", observation.Verdict)
	}
	if observation.Reason != audit.ReasonNoRecord {
		t.Errorf("reason = %q, want no_record", observation.Reason)
	}
}

func TestParseListRejections(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		edit  func(string) string
		wants string
	}{
		{
			name: "no cause at all is refused, never inferred",
			edit: func(s string) string {
				return strings.Replace(s, "    cause: {category: iac_apply, class: change_induced}\n", "", 1)
			},
			wants: "the audit measures, it never infers",
		},
		{
			name:  "a category outside the published eighteen",
			edit:  func(s string) string { return strings.Replace(s, "category: iac_apply", "category: cosmic_rays", 1) },
			wants: "cause.category",
		},
		{
			name:  "a cause class outside the published three",
			edit:  func(s string) string { return strings.Replace(s, "class: change_induced", "class: vibes", 1) },
			wants: "cause.class",
		},
		{
			name: "a verdict outside the published four",
			edit: func(s string) string {
				return strings.Replace(s, "base: not_observable, +deploy: observed", "base: maybe, +deploy: observed", 1)
			},
			wants: "verdict",
		},
		{
			name: "an incident that says nothing about a declared feeder set",
			edit: func(s string) string {
				return strings.Replace(s, "{base: not_observable, +deploy: observed}", "{base: not_observable}", 1)
			},
			wants: "no verdict for feeder set",
		},
		{
			name: "a verdict for a feeder set nobody declared",
			edit: func(s string) string {
				return strings.Replace(s, "{base: not_observable, +deploy: observed}", "{base: not_observable, +deploy: observed, +ghost: observed}", 1)
			},
			wants: "undeclared feeder set",
		},
		{
			name: "an undecidable verdict with no reason",
			edit: func(s string) string {
				return strings.Replace(s, "base: not_observable, +deploy: observed", "base: undecidable, +deploy: observed", 1)
			},
			wants: "needs a reason",
		},
		{
			name: "observability that regresses as feeders are added",
			edit: func(s string) string {
				return strings.Replace(s, "{base: not_observable, +deploy: observed}", "{base: observed, +deploy: not_observable}", 1)
			},
			wants: "feeder sets are cumulative",
		},
		{
			name:  "an unknown key",
			edit:  func(s string) string { return s + "\nnotes: a leak waiting to happen\n" },
			wants: "field notes not found",
		},
		{
			name:  "a duplicated incident",
			edit:  func(s string) string { return strings.Replace(s, "ref: T-02", "ref: T-01", 1) },
			wants: "listed twice",
		},
		{
			name: "no feeder sets",
			edit: func(s string) string {
				return strings.Replace(s,
					"feeder_sets:\n  - name: base\n    order: 1\n    feeders: [otel.spans]\n  - name: +deploy\n    order: 2\n    feeders: [cicd.deploys]\n",
					"feeder_sets: []\n", 1)
			},
			wants: "always measured against a named graph configuration",
		},
		{
			name:  "a feeder set that names no feeders",
			edit:  func(s string) string { return strings.Replace(s, "feeders: [otel.spans]", "feeders: []", 1) },
			wants: "name what this set adds",
		},
		{
			name:  "a format version this build does not read",
			edit:  func(s string) string { return strings.Replace(s, "version: 1", "version: 99", 1) },
			wants: "this build reads version 1",
		},
		{
			name:  "no author",
			edit:  func(s string) string { return strings.Replace(s, "author: test|auditor", "author: \"\"", 1) },
			wants: "author is required",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := audit.ParseList([]byte(test.edit(validList)))
			if err == nil {
				t.Fatalf("ParseList accepted a list it should refuse")
			}
			if !strings.Contains(err.Error(), test.wants) {
				t.Errorf("error = %v, want it to mention %q", err, test.wants)
			}
		})
	}
}

func TestParseListAcceptsTheValidList(t *testing.T) {
	t.Parallel()

	if _, err := audit.ParseList([]byte(validList)); err != nil {
		t.Fatalf("ParseList refused the control list: %v", err)
	}
}

// TestDigestIsStableAndSensitive: the digest is what proves two runs were measured over the same
// list (FR-071a), so it has to be stable across parses and move when a verdict moves.
func TestDigestIsStableAndSensitive(t *testing.T) {
	t.Parallel()

	first, err := audit.ParseList([]byte(validList))
	if err != nil {
		t.Fatalf("ParseList: %v", err)
	}
	second, err := audit.ParseList([]byte(validList))
	if err != nil {
		t.Fatalf("ParseList: %v", err)
	}
	firstDigest, err := first.Digest()
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	secondDigest, err := second.Digest()
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	if firstDigest != secondDigest {
		t.Errorf("digest is not stable: %s then %s", firstDigest, secondDigest)
	}
	if !strings.HasPrefix(firstDigest, "sha256:") {
		t.Errorf("digest = %q, want a sha256: prefix", firstDigest)
	}

	moved, err := audit.ParseList([]byte(strings.Replace(validList,
		"{base: not_observable, +deploy: observed}", "{base: not_observable, +deploy: symptom_only}", 1)))
	if err != nil {
		t.Fatalf("ParseList: %v", err)
	}
	movedDigest, err := moved.Digest()
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	if movedDigest == firstDigest {
		t.Error("a changed verdict did not change the digest")
	}
}
