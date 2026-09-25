// SPDX-License-Identifier: Apache-2.0

package feeder_test

import (
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// Observed clock skew (003 FR-153, 004 FR-058, T045).
//
// This file exists because the code it tests did not have one. It arrived as `internal/gcpx/skew.go`
// and **nothing ever called it**: no `Observe`, no `Report`, no test, and an `Instruments.Skew` method
// never invoked. So a requirement reading "the skew MUST be reported" was satisfied by machinery that
// never ran — the third time that shape has turned up here, after C4, C5 and C7.

var (
	local    = time.Date(2026, 9, 22, 14, 30, 0, 0, time.UTC)
	platform = func(offset time.Duration) time.Time { return local.Add(offset) }
)

// Both directions are kept, because a vendor running ahead and one running behind cause different
// symptoms: the first makes facts appear to arrive from the future, the second makes a poll look like
// it missed something. An absolute value would hide which is happening.
func TestSkewKeepsBothDirectionsRatherThanAnAbsoluteValue(t *testing.T) {
	t.Parallel()

	var skew feeder.Skew
	skew.Observe(platform(2*time.Second), local)  // vendor ahead
	skew.Observe(platform(-6*time.Second), local) // vendor behind

	report := skew.Report()
	if report.Samples != 2 {
		t.Fatalf("samples = %d, want 2", report.Samples)
	}
	if report.Max != 2*time.Second {
		t.Errorf("max = %s, want the vendor-ahead extreme", report.Max)
	}
	if report.Min != -6*time.Second {
		t.Errorf("min = %s, want the vendor-behind extreme; an absolute value would hide which "+
			"direction the clocks disagree in, and the two cause different symptoms", report.Min)
	}
	if report.Mean != -2*time.Second {
		t.Errorf("mean = %s, want the signed mean", report.Mean)
	}
	if report.Last != -6*time.Second {
		t.Errorf("last = %s, want the most recent sample", report.Last)
	}
}

// "The clocks agree" and "we never looked" are different claims, and Samples is what keeps them apart.
func TestAnUnobservedSkewIsNotAMeasuredZero(t *testing.T) {
	t.Parallel()

	var skew feeder.Skew
	report := skew.Report()
	if report.Samples != 0 {
		t.Fatalf("samples = %d on an unobserved skew, want 0", report.Samples)
	}
	if report.Mean != 0 || report.Beyond() {
		t.Errorf("an unobserved skew reports mean %s and beyond=%v; it must not look like a "+
			"measurement", report.Mean, report.Beyond())
	}

	// A measured zero is a real observation and says so.
	skew.Observe(platform(0), local)
	if measured := skew.Report(); measured.Samples != 1 {
		t.Errorf("a sample of exactly zero skew was not recorded; \"the clocks agree\" is a finding")
	}

	// A zero instant on either side is not a sample: it is a missing timestamp, and averaging it in
	// would invent a skew from an absence.
	var ignored feeder.Skew
	ignored.Observe(time.Time{}, local)
	ignored.Observe(platform(time.Hour), time.Time{})
	if got := ignored.Report().Samples; got != 0 {
		t.Errorf("samples = %d, want 0: a missing timestamp is not a skew of anything", got)
	}
}

// FR-058's threshold: beyond it the skew is worth an operator's attention, and the report says how
// often rather than only how far.
func TestTheThresholdCountsHowOftenAndNotOnlyHowFar(t *testing.T) {
	t.Parallel()

	skew := feeder.Skew{Threshold: 5 * time.Second}
	skew.Observe(platform(time.Second), local)     // inside
	skew.Observe(platform(-time.Second), local)    // inside, other direction
	skew.Observe(platform(30*time.Second), local)  // beyond
	skew.Observe(platform(-30*time.Second), local) // beyond, other direction

	report := skew.Report()
	if report.Exceeded != 2 {
		t.Errorf("exceeded = %d, want 2: the threshold is on the magnitude, so a vendor 30s behind is "+
			"as far out as one 30s ahead", report.Exceeded)
	}
	if !report.Beyond() {
		t.Error("Beyond() is false with samples past the threshold")
	}
	if report.Threshold != 5*time.Second {
		t.Errorf("threshold = %s, want the configured one carried into the report so it reads without "+
			"the configuration that produced it", report.Threshold)
	}
	if report.Samples != 4 {
		t.Errorf("samples = %d, want 4: every sample counts, exceeded or not — one outlier in a "+
			"thousand and a thousand in a thousand are different problems", report.Samples)
	}

	// Inside the threshold nothing is flagged, and that is not the same as nothing being observed.
	inside := feeder.Skew{Threshold: time.Minute}
	inside.Observe(platform(time.Second), local)
	if quiet := inside.Report(); quiet.Beyond() || quiet.Samples != 1 {
		t.Errorf("a sample inside the threshold reported beyond=%v samples=%d",
			quiet.Beyond(), quiet.Samples)
	}
}

// An unset or nonsensical threshold falls back to the published default rather than turning the report
// into noise.
func TestAnUnsetThresholdIsTheDefaultAndNotZero(t *testing.T) {
	t.Parallel()

	for name, configured := range map[string]time.Duration{
		"unset":    0,
		"negative": -time.Minute,
	} {
		skew := feeder.Skew{Threshold: configured}
		skew.Observe(platform(time.Second), local)
		report := skew.Report()
		if report.Threshold != feeder.DefaultSkewThreshold {
			t.Errorf("%s threshold came out as %s, want the published default %s",
				name, report.Threshold, feeder.DefaultSkewThreshold)
		}
		if report.Exceeded != 0 {
			t.Errorf("%s: a one-second skew was flagged against the default %s; a threshold of zero "+
				"would make every sample exceed it and the report would say nothing",
				name, feeder.DefaultSkewThreshold)
		}
	}
}

// A nil Skew is safe to observe and to report, so a connector that has not configured one does not
// crash on the path that was meant to be telemetry.
func TestANilSkewIsSafe(t *testing.T) {
	t.Parallel()

	var skew *feeder.Skew
	skew.Observe(platform(time.Hour), local)
	if report := skew.Report(); report.Samples != 0 || report.Beyond() {
		t.Errorf("a nil skew reported %+v", report)
	}
}
