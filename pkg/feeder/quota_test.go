// SPDX-License-Identifier: Apache-2.0

package feeder_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The budget as a share of what is LEFT (004 T035, T036; FR-071, FR-072, FR-073).
//
// The property that matters is not "we stay under a number". It is that the connector's share is of
// what the platform says remains, so that this poller and the operator's own tooling cannot each
// believe they are inside the same allowance.

func headers(limit, remaining, used int, resource string, reset time.Time) http.Header {
	h := http.Header{}
	h.Set("X-RateLimit-Limit", itoa(limit))
	h.Set("X-RateLimit-Remaining", itoa(remaining))
	h.Set("X-RateLimit-Used", itoa(used))
	h.Set("X-RateLimit-Reset", itoa(int(reset.Unix())))
	if resource != "" {
		h.Set("X-RateLimit-Resource", resource)
	}
	return h
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	negative := n < 0
	if negative {
		n = -n
	}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if negative {
		return "-" + string(digits)
	}
	return string(digits)
}

func budget(t *testing.T, share float64, reserve, static int) *feeder.QuotaBudget {
	t.Helper()
	b, err := feeder.NewQuotaBudget("github", feeder.QuotaPolicy{
		Share: share, Reserve: reserve, StaticAllowance: static,
	})
	if err != nil {
		t.Fatalf("NewQuotaBudget: %v", err)
	}
	return b
}

// The headers are read as the platform spells them, including which bucket the call came out of.
func TestTheRateLimitHeadersAreReadIncludingTheResourceFamily(t *testing.T) {
	t.Parallel()

	reset := time.Date(2026, 9, 22, 17, 0, 0, 0, time.UTC)
	reading, ok := feeder.ReadingFromHeaders(headers(5000, 4200, 800, "core", reset))
	if !ok {
		t.Fatal("ReadingFromHeaders refused a complete set of headers")
	}
	if reading.Limit != 5000 || reading.Remaining != 4200 || reading.Used != 800 {
		t.Errorf("reading = %+v, want the platform's own numbers", reading)
	}
	if reading.Family != "core" {
		t.Errorf("family = %q, want the platform's x-ratelimit-resource: the connector must not keep "+
			"its own map from endpoint to family when the response says which", reading.Family)
	}
	if !reading.Reset.Equal(reset) {
		t.Errorf("reset = %s, want %s", reading.Reset, reset)
	}
	if reading.Source != feeder.QuotaReported {
		t.Errorf("source = %q, want the reported form", reading.Source)
	}

	// No headers is a first-class answer, not a zero reading. "No headers" and "no requests left" are
	// opposite facts, and a caller that confused them would either stop for nothing or spend somebody
	// else's allowance.
	if _, ok := feeder.ReadingFromHeaders(http.Header{}); ok {
		t.Error("ReadingFromHeaders invented a reading from a response that said nothing")
	}
	// A partial set is also nothing: a limit with no remaining says how big the bucket is, not how
	// much is in it.
	partial := http.Header{}
	partial.Set("X-RateLimit-Limit", "5000")
	if _, ok := feeder.ReadingFromHeaders(partial); ok {
		t.Error("a limit with no remaining was read as a reading")
	}
	// Numbers without a named bucket are still true of whatever bucket they came from, and the family
	// is recorded as unnamed rather than guessed from the path.
	unnamed, ok := feeder.ReadingFromHeaders(headers(5000, 4200, 800, "", reset))
	if !ok || unnamed.Family != "unnamed" {
		t.Errorf("an unnamed family came out as %q (ok=%v), want \"unnamed\"", unnamed.Family, ok)
	}
}

// The share is a CEILING on the cycle, fixed against the window's opening reading.
//
// Fixed rather than recomputed per call, and that is the whole point: recomputing it against the
// latest reading would make it a floor and nothing else — every call would ask "is half of what is
// left still more than zero", the answer would stay yes almost to the end, and the share would be
// inert while looking enforced. That is exactly what the first version of this code did, and this test
// is what caught it.
func TestTheShareBoundsTheCycleAndIsNotRecomputedPerCall(t *testing.T) {
	t.Parallel()

	reset := time.Date(2026, 9, 22, 17, 0, 0, 0, time.UTC)
	b := budget(t, 0.5, 100, 0)
	reading, _ := feeder.ReadingFromHeaders(headers(5000, 1000, 4000, "core", reset))
	b.Observe(reading)

	// Half of what is LEFT — 500 — and not half of the 5000 limit.
	for i := 0; i < 500; i++ {
		if err := b.Allow("core"); err != nil {
			t.Fatalf("call %d of the share was refused: %v", i+1, err)
		}
	}

	// Still 400 above the reserve, so the floor is not what stops it: the share is.
	b.Observe(feeder.Reading{
		Family: "core", Limit: 5000, Remaining: 500, Used: 4500, Reset: reset,
		Source: feeder.QuotaReported,
	})
	err := b.Allow("core")
	yield, ok := feeder.YieldedForQuota(err)
	if !ok {
		t.Fatalf("Allow returned %v after the share was spent, want a typed quota yield", err)
	}
	if yield.Remaining <= yield.Reserve {
		t.Errorf("yield = %+v; this case is meant to stop on the SHARE with room above the reserve, so "+
			"a remaining at or below the reserve means the test is no longer testing the ceiling", yield)
	}
	if !yield.RetryAt.Equal(reset) {
		t.Errorf("retry-at = %s, want the window's reset instant", yield.RetryAt)
	}
	if yield.Source != feeder.QuotaReported {
		t.Errorf("source = %q; a yield has to say whether the allowance it yielded against was "+
			"measured or configured", yield.Source)
	}
}

// The reserve is a FLOOR, and it holds whatever the share works out to.
//
// A different fact from the ceiling above, and the reason it exists: an operator's own tooling and a
// person debugging share this bucket, and a poller that spent down to zero would lock them out
// (FR-072).
func TestTheReserveIsAFloorTheShareCannotCross(t *testing.T) {
	t.Parallel()

	reset := time.Date(2026, 9, 22, 17, 0, 0, 0, time.UTC)
	// A share of everything, so only the reserve can stop it.
	b := budget(t, 1.0, 100, 0)
	b.Observe(feeder.Reading{
		Family: "core", Limit: 5000, Remaining: 100, Reset: reset, Source: feeder.QuotaReported,
	})

	err := b.Allow("core")
	yield, ok := feeder.YieldedForQuota(err)
	if !ok {
		t.Fatalf("Allow returned %v at the reserve with a share of 1.0, want a typed quota yield", err)
	}
	if yield.Reserve != 100 || yield.Remaining != 100 {
		t.Errorf("yield = %+v, want the reserve and the remaining it yielded against", yield)
	}

	// And one call above the reserve is allowed, so the floor is a floor rather than a shutdown.
	above := budget(t, 1.0, 100, 0)
	above.Observe(feeder.Reading{
		Family: "core", Limit: 5000, Remaining: 101, Reset: reset, Source: feeder.QuotaReported,
	})
	if err := above.Allow("core"); err != nil {
		t.Errorf("one call above the reserve was refused: %v", err)
	}

	// The case only the floor can catch, and the realistic one: the cycle's share is nowhere near spent,
	// and the bucket drained anyway because the operator's own tooling is spending it too. The ceiling
	// was fixed when the window opened and would happily allow; only the floor sees what happened.
	drained := budget(t, 0.5, 100, 0)
	drained.Observe(feeder.Reading{
		Family: "core", Limit: 5000, Remaining: 1000, Reset: reset, Source: feeder.QuotaReported,
	})
	for i := 0; i < 10; i++ {
		if err := drained.Allow("core"); err != nil {
			t.Fatalf("call %d well inside the share was refused: %v", i+1, err)
		}
	}
	drained.Observe(feeder.Reading{
		Family: "core", Limit: 5000, Remaining: 50, Reset: reset, Source: feeder.QuotaReported,
	})
	err = drained.Allow("core")
	yield, ok = feeder.YieldedForQuota(err)
	if !ok {
		t.Fatalf("Allow returned %v when somebody else drained the bucket mid-cycle; the share was "+
			"fixed at the window's opening and cannot see that, so only the reserve floor can", err)
	}
	if yield.Remaining != 50 {
		t.Errorf("yield = %+v, want the remaining the platform last reported", yield)
	}
}

// A new window re-derives the allowance and forgets the old cycle's spend, because a share of what
// remains is a statement about a window rather than about all time.
func TestANewWindowReDerivesTheAllowance(t *testing.T) {
	t.Parallel()

	first := time.Date(2026, 9, 22, 17, 0, 0, 0, time.UTC)
	second := first.Add(time.Hour)
	b := budget(t, 0.5, 0, 0)

	b.Observe(feeder.Reading{Family: "core", Limit: 100, Remaining: 4, Reset: first, Source: feeder.QuotaReported})
	for i := 0; i < 2; i++ {
		if err := b.Allow("core"); err != nil {
			t.Fatalf("call %d in the first window was refused: %v", i+1, err)
		}
	}
	if err := b.Allow("core"); err == nil {
		t.Fatal("the first window's share was exceeded")
	}

	b.Observe(feeder.Reading{Family: "core", Limit: 100, Remaining: 100, Reset: second, Source: feeder.QuotaReported})
	if err := b.Allow("core"); err != nil {
		t.Errorf("the new window did not re-derive the allowance: %v", err)
	}
}

// Stopping for quota is a typed reason, distinct from having found nothing (FR-073).
func TestAQuotaStopIsNotAnAbsenceOfFindings(t *testing.T) {
	t.Parallel()

	b := budget(t, 0.5, 10, 0)
	b.Observe(feeder.Reading{Family: "core", Limit: 100, Remaining: 10, Source: feeder.QuotaReported})

	err := b.Allow("core")
	if err == nil {
		t.Fatal("Allow spent into the reserve")
	}
	if _, ok := feeder.YieldedForQuota(err); !ok {
		t.Errorf("a caller cannot tell this is a quota stop without reading the message: %v", err)
	}
	// And an ordinary error is not mistaken for one.
	if _, ok := feeder.YieldedForQuota(http.ErrNoCookie); ok {
		t.Error("an unrelated error was read as a quota yield")
	}
}

// A platform that reports nothing gets the static fallback, and the report says so.
func TestAStaticFallbackIsReportedAsStaticAndNotAsAShare(t *testing.T) {
	t.Parallel()

	b := budget(t, 1.0, 0, 3)
	for i := 0; i < 3; i++ {
		if err := b.Allow("deployments"); err != nil {
			t.Fatalf("call %d of the static allowance was refused: %v", i+1, err)
		}
	}
	if err := b.Allow("deployments"); err == nil {
		t.Error("the static allowance was exceeded")
	}

	report := b.Report()
	if len(report.Families) != 1 {
		t.Fatalf("report has %d families, want 1: %+v", len(report.Families), report.Families)
	}
	line := report.Families[0]
	if line.Source != feeder.QuotaStatic {
		t.Errorf("source = %q, want the static form; a static allowance presented as a measured share "+
			"is a number an operator would trust more than it deserves (FR-071)", line.Source)
	}
	if line.Spent != 3 {
		t.Errorf("spent = %d, want 3", line.Spent)
	}

	// With no static allowance configured, an unreported family spends nothing at all: a budget with
	// no numbers is not an unlimited budget.
	strict := budget(t, 0.5, 0, 0)
	err := strict.Allow("deployments")
	if err == nil {
		t.Fatal("a family the platform has said nothing about, with no configured fallback, was allowed")
	}
	// And the yield says nothing is KNOWN, not that a static allowance ran out. The distinction is the
	// whole reason this branch exists: reporting an allowance nobody configured as the thing that
	// stopped the run would put a number in the report that no operator chose.
	unknown, ok := feeder.YieldedForQuota(err)
	if !ok {
		t.Fatalf("the refusal is not a typed quota yield: %v", err)
	}
	if unknown.Source != feeder.QuotaUnknown {
		t.Errorf("source = %q, want the unknown form: there is no static allowance here, and claiming "+
			"one would attribute the stop to a number nobody configured", unknown.Source)
	}
}

// A share outside (0,1] is a configuration error rather than something to clamp.
func TestAnUnusableSharePolicyIsRefusedRatherThanClamped(t *testing.T) {
	t.Parallel()

	for name, policy := range map[string]feeder.QuotaPolicy{
		"zero share":       {Share: 0},
		"negative share":   {Share: -0.5},
		"more than all":    {Share: 1.5},
		"negative reserve": {Share: 0.5, Reserve: -1},
	} {
		if _, err := feeder.NewQuotaBudget("github", policy); err == nil {
			t.Errorf("%s was accepted; a clamped share is a share nobody chose", name)
		}
	}
	if _, err := feeder.NewQuotaBudget("", feeder.QuotaPolicy{Share: 0.5}); err == nil {
		t.Error("a budget with no platform name was accepted")
	}
}

// The reserve protects what the PLATFORM says is left, so it does not apply to a static allowance: an
// allowance smaller than the reserve would otherwise be zero, and the connector could never make the one
// read that tells it the real numbers (004 T157). Once the platform has reported, the reserve binds.
func TestTheReserveBindsMeasuredQuotaNotTheStaticBootstrap(t *testing.T) {
	t.Parallel()

	b := budget(t, 0.5, 500, 3)
	for i := range 3 {
		if err := b.Allow("core"); err != nil {
			t.Fatalf("bootstrap call %d refused under a reserve larger than the allowance: %v", i+1, err)
		}
	}
	if err := b.Allow("core"); err == nil {
		t.Error("the static allowance was exceeded")
	}

	b.Observe(feeder.Reading{Family: "core", Limit: 5000, Remaining: 400, Source: feeder.QuotaReported})
	err := b.Allow("core")
	yield, ok := feeder.YieldedForQuota(err)
	if !ok || yield.Reserve != 500 {
		t.Errorf("Allow = %v; with 400 measured left against a reserve of 500 the floor must bind", err)
	}
}

// A reading that said "nothing left" stands only until its window resets: after it, the budget draws
// one call from the static allowance, whose response re-derives the share (005 FR-083). Before it,
// nothing is spent.
func TestAnExhaustedWindowIsForgottenOnceItResets(t *testing.T) {
	t.Parallel()
	reset := time.Date(2026, 9, 27, 14, 6, 30, 0, time.UTC)
	b := budget(t, 0.5, 20, 30)
	b.Observe(feeder.Reading{Family: "monitors", Limit: 1000, Remaining: 0, Reset: reset, Source: feeder.QuotaReported})

	b.Expire("monitors", reset.Add(-time.Second))
	if _, ok := feeder.YieldedForQuota(b.Allow("monitors")); !ok {
		t.Fatal("a call was allowed inside a window Datadog said was exhausted")
	}
	b.Expire("monitors", reset)
	if err := b.Allow("monitors"); err != nil {
		t.Fatalf("the window reset and the budget still refused: %v", err)
	}
	if left, known := b.Headroom("monitors"); !known || left >= 1 {
		t.Errorf("headroom %v (%v): the probe should be drawn from the static allowance", left, known)
	}
}
