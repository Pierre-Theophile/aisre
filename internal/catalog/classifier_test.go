// SPDX-License-Identifier: Apache-2.0

package catalog_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/catalog"
	"github.com/Pierre-Theophile/aisre/internal/query"
	"github.com/Pierre-Theophile/aisre/pkg/attribution"
)

// Layer 3 in the attributor (009 T034, FR-034, FR-035): asked only about what layers 0–2 left
// `unknown`, decided on with τ and δ, and never in the way.

type stubClassifier struct {
	scores map[string]float64
	err    error
	wait   bool
	calls  atomic.Int32
}

func (s *stubClassifier) ID() string { return "stub@1" }

func (s *stubClassifier) Classify(ctx context.Context, _ attribution.Signal, entries []attribution.EntryProfile) ([]attribution.Scored, error) {
	s.calls.Add(1)
	if s.wait {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if s.err != nil {
		return nil, s.err
	}
	var out []attribution.Scored
	for slug, score := range s.scores {
		out = append(out, attribution.Scored{Slug: slug, Score: score, Rationale: "stub"})
	}
	return out, nil
}

// evidence is the monitor's current decision's evidence, as JSON.
func (w *world) evidence(monitor string) string {
	w.t.Helper()
	why, err := query.NewEngine(w.p.Store()).WhyAttributed(context.Background(), &graphv1.WhyAttributedRequest{
		Signal: ref("datadog.monitor", monitor), AsOf: &graphv1.AsOf{ValidAt: timestamppb.New(w.at)},
	})
	if err != nil {
		w.t.Fatalf("why: %v", err)
	}
	b, err := why.GetCurrent().GetEvidence().MarshalJSON()
	if err != nil {
		w.t.Fatal(err)
	}
	return string(b)
}

func classifierWorld(t *testing.T, c attribution.Classifier, timeout time.Duration) *world {
	w := newWorld(t, catalog.WithClassifier(c, timeout))
	w.entry("checkout", "Checkout", "confirmed")
	w.entry("infra", "Infra", "confirmed")
	w.service("zeta-gateway", "production")
	w.alert("1", "zeta-gateway", "production")
	return w
}

func TestTheClassifierDecidesWhatTheRulesCannot(t *testing.T) {
	t.Parallel()
	c := &stubClassifier{scores: map[string]float64{"checkout": 0.91234, "infra": 0.2, "ghost": 0.99}}
	w := classifierWorld(t, c, 0)
	w.sweep()
	if got := w.attributed("1"); got != "checkout/M" {
		t.Errorf("attributed %q, want checkout/M", got)
	}
	if ev := strings.ReplaceAll(w.evidence("1"), " ", ""); !strings.Contains(ev, `"classifier":{"checkout":0.9123,"infra":0.2}`) ||
		!strings.Contains(ev, `"classifier_id":"stub@1"`) || !strings.Contains(ev, `"decided_by":"classifier"`) {
		t.Errorf("evidence %s", ev)
	}
}

func TestTwoEntriesOverTheThresholdAreUnknown(t *testing.T) {
	t.Parallel()
	// Both over τ, but one text read two ways is not two services.
	w := classifierWorld(t, &stubClassifier{scores: map[string]float64{"checkout": 0.95, "infra": 0.8}}, 0)
	w.sweep()
	if got := w.attributed("1"); got != "" {
		t.Errorf("attributed %q, want unknown", got)
	}
}

func TestAClassifierFailureLeavesTheRulesToDecide(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]*stubClassifier{
		"error":   {err: errors.New("no credential")},
		"timeout": {wait: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := classifierWorld(t, c, 50*time.Millisecond)
			w.sweep()
			if got := w.attributed("1"); got != "" {
				t.Errorf("attributed %q, want unknown", got)
			}
			want := map[string]string{"error": "no credential", "timeout": "did not answer within 50ms"}[name]
			if ev := w.evidence("1"); !strings.Contains(ev, `"classifier_fallback":"`) || !strings.Contains(ev, want) {
				t.Errorf("evidence %s, want a fallback saying %q", ev, want)
			}
		})
	}
}

func TestTheClassifierIsNotAskedWhenAnAliasDecides(t *testing.T) {
	t.Parallel()
	c := &stubClassifier{scores: map[string]float64{"infra": 0.99}}
	w := classifierWorld(t, c, 0)
	w.alias(ref("datadog.service", "production/zeta-gateway"), "", "checkout")
	w.sweep()
	if got := w.attributed("1"); got != "checkout/A1" {
		t.Errorf("attributed %q", got)
	}
	if n := c.calls.Load(); n != 0 {
		t.Errorf("the classifier was asked %d times", n)
	}
}

func TestAClassifierLeadUnderTheMarginIsUnknown(t *testing.T) {
	t.Parallel()
	w := classifierWorld(t, &stubClassifier{scores: map[string]float64{"checkout": 0.85, "infra": 0.79}}, 0)
	w.sweep()
	if got := w.attributed("1"); got != "" {
		t.Errorf("attributed %q, want unknown", got)
	}
}
