// SPDX-License-Identifier: Apache-2.0

package otel_test

import (
	"testing"
	"time"

	otelfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/otel"
)

// What this feeder promises about its own event order (FR-021, FR-048).
//
// The promise used to be the aggregation window, which was wrong in the one way that mattered:
// a retraction and a later re-assertion of the same call path are one aggregation window apart
// at the very least, and the graph did not survive having those two swapped.
// `fixtures/feeder-gap-01` had them 296 s apart inside a declared 300 s window and the shuffle
// step of `fixture verify` duly found it. The graph survives that ordering now
// (internal/projector: TestRetractionAndReassertionAreOrderIndependent), but the declaration
// stays a constant and stays small, because a promise should say what this feeder can show
// rather than what the graph happens to tolerate.
//
// Two properties keep that from coming back, and they are what is asserted here.

// TestDeclaredWindowIsIndependentOfTheAggregationWindow is the first: `--window` may be set to
// anything, and the promise does not move with it. A declaration derived from a tunable is a
// declaration nobody has reasoned about.
func TestDeclaredWindowIsIndependentOfTheAggregationWindow(t *testing.T) {
	t.Parallel()
	for _, window := range []time.Duration{0, 30 * time.Second, time.Minute, 5 * time.Minute, time.Hour} {
		f := &otelfeeder.Feeder{SourceID: "otel:window-test", Window: window}
		if got := f.Describe().ReorderingWindow; got != otelfeeder.DeclaredReorderingWindow {
			t.Errorf("--window %s: Describe().ReorderingWindow = %s, want the constant %s",
				window, got, otelfeeder.DeclaredReorderingWindow)
		}
	}
}

// TestDeclaredWindowCannotLicenseARetractionSwap is the second: the promise must stay below the
// shortest interval over which this feeder can emit a retraction and a re-assertion of one
// edge. A retraction carries the valid end of the last window the path was seen in and a
// re-assertion can only come from a later window, so that interval is one aggregation window.
// The default is the one anybody running the shipped CLI gets, and the recorded fixtures are
// all at it.
func TestDeclaredWindowCannotLicenseARetractionSwap(t *testing.T) {
	t.Parallel()
	if otelfeeder.DeclaredReorderingWindow >= otelfeeder.DefaultWindow {
		t.Errorf("DeclaredReorderingWindow = %s, which is not below the default aggregation "+
			"window (%s): a permutation inside it could swap a retraction with a later "+
			"re-assertion of the same edge, and the graph does not survive that",
			otelfeeder.DeclaredReorderingWindow, otelfeeder.DefaultWindow)
	}
	if otelfeeder.DeclaredReorderingWindow <= 0 {
		t.Errorf("DeclaredReorderingWindow = %s: a feeder that promises nothing should say "+
			"`none`, not zero", otelfeeder.DeclaredReorderingWindow)
	}
}
