// SPDX-License-Identifier: Apache-2.0

package backend_test

import (
	"math"
	"strings"
	"testing"

	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
)

// Miss-rate accounting (tasks.md T040, FR-042c, SC-018).

func report(served int, missed ...string) engine.MissReport {
	r := engine.MissReport{Served: served, WorldDir: "world", WorldSize: served}
	for i, term := range missed {
		r.Missed = append(r.Missed, engine.Miss{TermKey: string(rune('a' + i)), Term: term})
	}
	return r
}

func TestMissRate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		report     engine.MissReport
		rate       float64
		sufficient bool
	}{
		{"a world that answered everything", report(100), 0, true},
		{"a run that asked nothing", report(0), 0, true},
		{"one miss in a hundred", report(99, "compare"), 0.01, true},
		{"exactly at the threshold", report(95, "a", "b", "c", "d", "e"), 0.05, true},
		{"one over the threshold", report(94, "a", "b", "c", "d", "e", "f"), 0.06, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.report.MissRate(); got != tc.rate {
				t.Errorf("miss rate = %v, want %v", got, tc.rate)
			}
			if got := tc.report.Sufficient(0); got != tc.sufficient {
				t.Errorf("sufficient = %v, want %v", got, tc.sufficient)
			}
		})
	}
}

// TestRefusalsAreNotMisses: a request outside the algebra is a defect in the caller, not a gap
// in the recording. Counting it as a miss would let a buggy engine condemn a good fixture.
func TestRefusalsAreNotMisses(t *testing.T) {
	t.Parallel()

	r := report(100)
	r.Refused = 50
	if got := r.MissRate(); got != 0 {
		t.Errorf("miss rate = %v with 50 out-of-algebra refusals, want 0", got)
	}
	if got := r.InAlgebra(); got != 100 {
		t.Errorf("in-algebra requests = %d, want 100; refusals are counted separately", got)
	}
}

// TestTheVerdictGatesTheFixtureAndNotTheEngine: above the threshold a fixture is reported
// insufficient and is **not scored**, and the message says which terms to widen the recording
// for. That is a louder signal than a low score and one that names its own remedy.
func TestTheVerdictGatesTheFixtureAndNotTheEngine(t *testing.T) {
	t.Parallel()

	insufficient := report(90, "compare", "onset", "compare", "error_spans", "onset", "exemplars",
		"compare", "onset", "compare", "monitor_state")
	verdict := insufficient.Verdict("rollout-regression-01-incident", 0)

	for _, want := range []string{
		"INSUFFICIENT", "not scored", "fixture record-world",
		"gates the fixture, never the engine",
		"compare", "onset",
	} {
		if !strings.Contains(verdict, want) {
			t.Errorf("the verdict does not mention %q:\n%s", want, verdict)
		}
	}
	if strings.Contains(verdict, "admitted") {
		t.Errorf("an insufficient fixture was reported as admitted:\n%s", verdict)
	}

	// The remedy names each distinct term once, not once per miss.
	if got := insufficient.MissedTerms(); len(got) != 5 {
		t.Errorf("missed terms = %v, want the five distinct ones", got)
	}

	admitted := report(100, "compare").Verdict("good-fixture", 0)
	if !strings.Contains(admitted, "admitted") {
		t.Errorf("a fixture under the threshold was not admitted:\n%s", admitted)
	}
}

// TestCorpusMissRateIsWeightedByTraffic: a corpus of one busy fixture and nine quiet ones must
// not hide the busy one's gaps behind nine zeros.
func TestCorpusMissRateIsWeightedByTraffic(t *testing.T) {
	t.Parallel()

	corpus := engine.NewCorpusMissReport(0)
	corpus.Add("busy", report(80, "a", "b", "c", "d", "e", "f", "g", "h", "i", "j",
		"k", "l", "m", "n", "o", "p", "q", "r", "s", "t"))
	for _, id := range []string{"quiet-1", "quiet-2", "quiet-3", "quiet-4", "quiet-5"} {
		corpus.Add(id, report(2))
	}

	// 20 misses over 110 in-algebra requests: the mean of the per-fixture rates would be 0.033.
	if got := corpus.MissRate(); got != 0.181818 {
		t.Errorf("corpus miss rate = %v, want 0.181818 (misses over requests, not the mean of rates)", got)
	}
	if got := corpus.Insufficient(); len(got) != 1 || got[0] != "busy" {
		t.Errorf("insufficient fixtures = %v, want [busy]", got)
	}

	markdown := corpus.Markdown()
	for _, want := range []string{"busy", "insufficient, not scored", "quiet-1", "**corpus**"} {
		if !strings.Contains(markdown, want) {
			t.Errorf("the published table does not mention %q:\n%s", want, markdown)
		}
	}
}

// The per-term breakdown (003 T112, FR-108). "compare missed forty times and drill_down once" and
// "both missed" call for different remedies, and only the first tells a reviewer which.
func TestTheCorpusReportSaysWhichTermIsThin(t *testing.T) {
	corpus := engine.NewCorpusMissReport(0.05)
	corpus.Add("busy-01", engine.MissReport{
		Served: 10,
		Missed: []engine.Miss{
			{TermKey: "a", Term: "compare"},
			{TermKey: "b", Term: "compare"},
			{TermKey: "c", Term: "drill_down"},
		},
	})
	corpus.Add("quiet-01", engine.MissReport{
		Served: 20,
		Missed: []engine.Miss{{TermKey: "d", Term: "compare"}},
	})

	byTerm := corpus.ByTerm()
	if len(byTerm) != 2 {
		t.Fatalf("got %d terms, want compare and drill_down: %+v", len(byTerm), byTerm)
	}
	// Worst first, so the reader's eye lands on the part of the cross product to widen.
	if byTerm[0].Term != "compare" || byTerm[0].Missed != 3 {
		t.Errorf("first row = %+v, want compare missed 3 times", byTerm[0])
	}
	if byTerm[1].Term != "drill_down" || byTerm[1].Missed != 1 {
		t.Errorf("second row = %+v, want drill_down missed once", byTerm[1])
	}

	// The corpus rate is misses over in-algebra requests across every fixture, not the mean of
	// the per-fixture rates: a busy fixture's gaps must not hide behind a quiet one's zeros.
	if got, want := corpus.MissRate(), round6(4.0/34.0); got != want {
		t.Errorf("corpus miss rate = %v, want %v", got, want)
	}
	if corpus.Sufficient() {
		t.Error("a corpus missing 4 of 34 is above the 0.05 threshold and is not sufficient")
	}
	if !strings.Contains(corpus.Markdown(), "| compare | 3 |") {
		t.Errorf("the markdown does not carry the breakdown:\n%s", corpus.Markdown())
	}
}

func round6(v float64) float64 { return math.Round(v*1e6) / 1e6 }
