// SPDX-License-Identifier: Apache-2.0

// Package classifiertest is the conformance suite of attribution.Classifier (feature 009
// contracts §7, FR-034, FR-036). A classifier is conformant when Run passes against it
// unmodified: the shipped one runs it in CI, and a third-party classifier proves itself the same
// way.
package classifiertest

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/pkg/attribution"
)

// NewClassifier returns the classifier under test. Run calls it once per subtest.
type NewClassifier func(t *testing.T) attribution.Classifier

// Signal and Entries are the input every subtest uses: a classifier backed by recordings records
// its outputs for exactly these.
var (
	Signal = attribution.Signal{
		Ref:  attribution.Name{Namespace: "datadog.monitor", Value: "4242"},
		Kind: "alert",
		Names: []attribution.Name{
			{Namespace: "datadog.service", Value: "production/candidate-api"},
		},
		Text:       "candidate-api error rate above threshold",
		ObservedAt: time.Date(2026, 10, 19, 9, 5, 0, 0, time.UTC),
	}
	Entries = []attribution.EntryProfile{
		{Slug: "candidate-view", DisplayName: "Candidate View", Description: "the candidate-facing API and web app",
			DocumentTitles: []string{"Postmortem: candidate API latency"}},
		{Slug: "billing", DisplayName: "Billing", Description: "invoices and payments"},
		{Slug: "infra", DisplayName: "Infra", Description: "the shared platform: ingress, clusters, databases"},
	}
)

// Run runs the conformance suite against classifiers made by newClassifier.
func Run(t *testing.T, newClassifier NewClassifier) {
	t.Helper()
	for _, tc := range []struct {
		name string
		fn   func(t *testing.T, c attribution.Classifier)
	}{
		{"HasAnID", hasAnID},
		{"IsDeterministic", isDeterministic},
		{"ScoresAreProbabilities", scoresAreProbabilities},
		{"NamesOnlyTheEntriesGiven", namesOnlyTheEntriesGiven},
		{"HonoursTheDeadline", honoursTheDeadline},
	} {
		t.Run(tc.name, func(t *testing.T) { tc.fn(t, newClassifier(t)) })
	}
}

func classify(t *testing.T, c attribution.Classifier, entries []attribution.EntryProfile) []attribution.Scored {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := c.Classify(ctx, Signal, entries)
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	return out
}

func hasAnID(t *testing.T, c attribution.Classifier) {
	if strings.TrimSpace(c.ID()) == "" {
		t.Error("the classifier has no ID, so a decision could not say which one took part")
	}
}

func isDeterministic(t *testing.T, c attribution.Classifier) {
	first := classify(t, c, Entries)
	for range 3 {
		if again := classify(t, c, Entries); !reflect.DeepEqual(first, again) {
			t.Fatalf("the same input scored differently:\n %+v\n %+v", first, again)
		}
	}
}

func scoresAreProbabilities(t *testing.T, c attribution.Classifier) {
	for _, s := range classify(t, c, Entries) {
		if !(s.Score >= 0 && s.Score <= 1) {
			t.Errorf("%s scored %v, outside [0, 1]", s.Slug, s.Score)
		}
	}
}

func namesOnlyTheEntriesGiven(t *testing.T, c attribution.Classifier) {
	given := Entries[1:]
	seen := map[string]bool{}
	for _, s := range classify(t, c, given) {
		known := false
		for _, e := range given {
			known = known || e.Slug == s.Slug
		}
		if !known {
			t.Errorf("scored %q, which it was not given", s.Slug)
		}
		if seen[s.Slug] {
			t.Errorf("scored %q twice", s.Slug)
		}
		seen[s.Slug] = true
	}
}

func honoursTheDeadline(t *testing.T, c attribution.Classifier) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() {
		_, err := c.Classify(ctx, Signal, Entries)
		done <- err
	}()
	select {
	case err := <-done:
		// A classifier that answers from memory may finish before it looks at ctx; one that does
		// work must say why it stopped.
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("a cancelled call returned %v, which does not wrap context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("a cancelled call had not returned after 2 s")
	}
}
