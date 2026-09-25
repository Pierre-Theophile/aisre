// SPDX-License-Identifier: Apache-2.0

package feeder_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The metered door (004 FR-004, FR-071, FR-073, SC-007).
//
// The property under test is not "a call was allowed" but that the three checks cannot come apart:
// an operation off the published surface is refused before any quota is spent, a refusal is recorded
// as an attempt rather than dropped, and an unmetered run is still a gated one.

var issueWindow = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// metered returns an issuer whose budget has seen one reading for `core`, so the share is fixed and
// calls can actually be spent. remaining decides whether the reserve floor is already breached.
func metered(t *testing.T, remaining int) (*feeder.Issuer, *feeder.QuotaBudget, *feeder.AreaStats) {
	t.Helper()
	budget, err := feeder.NewQuotaBudget("testhub", feeder.QuotaPolicy{Share: 0.5, Reserve: 1})
	if err != nil {
		t.Fatalf("NewQuotaBudget: %v", err)
	}
	budget.Observe(feeder.Reading{
		Family: "core", Limit: 100, Remaining: remaining,
		Reset: issueWindow.Add(time.Hour), Source: feeder.QuotaReported,
	})
	stats := &feeder.AreaStats{}
	issuer, err := feeder.NewIssuer(surface(t, reads()), budget, stats)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	return issuer, budget, stats
}

func recordOf(t *testing.T, issuer *feeder.Issuer, op feeder.ReadOperation) feeder.RequestRecord {
	t.Helper()
	for _, row := range issuer.RequestLog() {
		if row.Operation == op {
			return row
		}
	}
	t.Fatalf("the request log has no row for %q; it holds %+v", op, issuer.RequestLog())
	return feeder.RequestRecord{}
}

// An issuer with no surface would admit everything, which is the opposite of what it is for.
func TestAnIssuerWithoutASurfaceIsRefused(t *testing.T) {
	t.Parallel()
	if _, err := feeder.NewIssuer(nil, nil, nil); err == nil {
		t.Error("an issuer was built with no published surface; every operation would be issuable")
	}
}

// The surface gate runs first, and it spends nothing. This is the ordering assertion: were the
// quota step first, an unpublished operation would consume an allowance deciding about a call that
// was never permitted in the first place.
func TestAnUnpublishedOperationSpendsNoQuota(t *testing.T) {
	t.Parallel()
	issuer, budget, stats := metered(t, 10)

	err := issuer.Issue(context.Background(), "DELETE /repos/{owner}/{repo}", "core", issueWindow)
	var unpublished *feeder.UnpublishedOperationError
	if !errors.As(err, &unpublished) {
		t.Fatalf("Issue on an unpublished operation returned %v, want UnpublishedOperationError", err)
	}
	for _, family := range budget.Report().Families {
		if family.Spent != 0 {
			t.Errorf("the budget spent %d call(s) on %s deciding about an operation the surface does "+
				"not carry; the gate must come before the meter", family.Spent, family.Family)
		}
	}
	// And the area report stays empty, because an unpublished operation belongs to no area — there is
	// no spec to read one off.
	if got := stats.Report(); len(got) != 0 {
		t.Errorf("the area report has %d line(s) after a blocked attempt: %+v; an unpublished "+
			"operation has no area to attribute it to", len(got), got)
	}
}

// The blocked attempt is in the log. This is the assertion that makes FR-004 falsifiable at all: the
// claim is about what the connector CAN issue, so an attempt the surface stopped is the evidence, and
// a log recording only permitted calls would report a clean run in exactly that case.
func TestABlockedAttemptIsRecordedWithItsReason(t *testing.T) {
	t.Parallel()
	issuer, _, _ := metered(t, 10)
	op := feeder.ReadOperation("GET /repos/{owner}/{repo}/contents/{path}")

	_ = issuer.Issue(context.Background(), op, "core", issueWindow)

	row := recordOf(t, issuer, op)
	switch {
	case row.Blocked != 1:
		t.Errorf("Blocked = %d after one refused attempt, want 1", row.Blocked)
	case row.Issued != 0:
		t.Errorf("Issued = %d for an operation the surface refused, want 0", row.Issued)
	case !strings.Contains(row.Refusal, "published read-only operation surface"):
		t.Errorf("the recorded refusal does not say the surface stopped it: %q", row.Refusal)
	case !row.First.Equal(issueWindow) || !row.Last.Equal(issueWindow):
		t.Errorf("First/Last = %v/%v for one attempt at %v", row.First, row.Last, issueWindow)
	}
}

// A refusal by the budget is a different fact from a refusal by the surface, and the log tells them
// apart. If both landed in one counter, an operator could not distinguish a connector that ran out of
// allowance from one calling something it never published.
func TestAQuotaRefusalIsRecordedSeparatelyFromABlockedOne(t *testing.T) {
	t.Parallel()
	// Remaining 1 with a reserve of 1 breaches the floor, so the first call yields.
	issuer, _, stats := metered(t, 1)
	op := feeder.ReadOperation("GET /repos/{owner}/{repo}/deployments")

	err := issuer.Issue(context.Background(), op, "core", issueWindow)
	if _, yielded := feeder.YieldedForQuota(err); !yielded {
		t.Fatalf("Issue against a breached reserve returned %v, want a quota yield", err)
	}

	row := recordOf(t, issuer, op)
	switch {
	case row.Refused != 1:
		t.Errorf("Refused = %d after one quota yield, want 1", row.Refused)
	case row.Blocked != 0:
		t.Errorf("Blocked = %d for a call the SURFACE admitted, want 0; a quota yield and an "+
			"unpublished operation are different failures and an operator has to tell them apart",
			row.Blocked)
	case row.Issued != 0:
		t.Errorf("Issued = %d for a refused call, want 0", row.Issued)
	case row.Area != "deployments":
		t.Errorf("Area = %q, want the published spec's area even though the call did not go out", row.Area)
	}

	// The area report agrees, and counts it as a refusal rather than a call.
	report := stats.Report()
	if len(report) != 1 || report[0].Area != "deployments" {
		t.Fatalf("the area report is %+v, want one `deployments` line", report)
	}
	if report[0].Calls != 0 || report[0].Refusals != 1 {
		t.Errorf("the area line reports %d call(s) and %d refusal(s), want 0 and 1",
			report[0].Calls, report[0].Refusals)
	}
}

// A permitted call is recorded as issued, spends the budget, and counts in its area — so the refusal
// assertions above are not simply "everything is refused".
func TestAPermittedCallIsIssuedSpentAndCounted(t *testing.T) {
	t.Parallel()
	issuer, budget, stats := metered(t, 10)
	op := feeder.ReadOperation("GET /repos/{owner}/{repo}/deployments")

	if err := issuer.Issue(context.Background(), op, "core", issueWindow); err != nil {
		t.Fatalf("a published operation inside the budget was refused: %v", err)
	}

	row := recordOf(t, issuer, op)
	if row.Issued != 1 || row.Refused != 0 || row.Blocked != 0 {
		t.Errorf("the log row is %+v, want one issued call and nothing else", row)
	}
	var spent int
	for _, family := range budget.Report().Families {
		if family.Family == "core" {
			spent = family.Spent
		}
	}
	if spent != 1 {
		t.Errorf("the budget spent %d on `core` after one issued call, want 1", spent)
	}
	report := stats.Report()
	if len(report) != 1 || report[0].Calls != 1 || report[0].Refusals != 0 {
		t.Errorf("the area report is %+v, want one `deployments` line with one call", report)
	}
}

// An unmetered run is still a gated one: a nil budget skips the quota step and records nothing as
// issued, because nothing left the process — but the surface gate still refuses.
func TestANilBudgetIsGatedButNotMetered(t *testing.T) {
	t.Parallel()
	stats := &feeder.AreaStats{}
	issuer, err := feeder.NewIssuer(surface(t, reads()), nil, stats)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}

	if err := issuer.Issue(context.Background(), "GET /repos/{owner}/{repo}/deployments", "core", issueWindow); err != nil {
		t.Fatalf("an unmetered run refused a published operation: %v", err)
	}
	if got := issuer.RequestLog(); len(got) != 0 {
		t.Errorf("the request log holds %+v after an unmetered call; nothing went out, so nothing is "+
			"a request", got)
	}
	if got := stats.Report(); len(got) != 0 {
		t.Errorf("the area report holds %+v after an unmetered call", got)
	}

	// The half that is about capability still holds. "We are not counting calls" is never a reason to
	// stop checking what may be called.
	blocked := feeder.ReadOperation("GET /repos/{owner}/{repo}/contents/{path}")
	if err := issuer.Issue(context.Background(), blocked, "core", issueWindow); err == nil {
		t.Error("an unmetered run issued an operation off the published surface")
	}
	if row := recordOf(t, issuer, blocked); row.Blocked != 1 {
		t.Errorf("Blocked = %d on an unmetered run, want 1; the gate is what an unmetered run keeps",
			row.Blocked)
	}
}

// A dead context is neither a request nor a refusal, so it is not this connector's behaviour to
// record — and the budget is untouched.
func TestADeadContextRecordsNothing(t *testing.T) {
	t.Parallel()
	issuer, budget, stats := metered(t, 10)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := issuer.Issue(ctx, "GET /repos/{owner}/{repo}/deployments", "core", issueWindow)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Issue with a cancelled context returned %v, want context.Canceled", err)
	}
	if got := issuer.RequestLog(); len(got) != 0 {
		t.Errorf("the request log holds %+v after a cancelled call", got)
	}
	if got := stats.Report(); len(got) != 0 {
		t.Errorf("the area report holds %+v after a cancelled call", got)
	}
	for _, family := range budget.Report().Families {
		if family.Spent != 0 {
			t.Errorf("the budget spent %d on %s for a call the context killed", family.Spent, family.Family)
		}
	}
}

// Repeated calls accumulate on one row, and First/Last bracket the cycle. Per operation rather than
// per call: the artifact a reader checks is "every operation this connector issued", and a row per
// call would be a payload log.
func TestRepeatedCallsAccumulateOnOneRowAndBracketTheCycle(t *testing.T) {
	t.Parallel()
	issuer, _, _ := metered(t, 10)
	op := feeder.ReadOperation("GET /repos/{owner}/{repo}/deployments")
	later := issueWindow.Add(90 * time.Second)

	// Out of order on purpose: First must be the earliest seen rather than the first recorded.
	if err := issuer.Issue(context.Background(), op, "core", later); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if err := issuer.Issue(context.Background(), op, "core", issueWindow); err != nil {
		t.Fatalf("second call: %v", err)
	}

	if got := len(issuer.RequestLog()); got != 1 {
		t.Fatalf("the request log has %d rows after two calls to one operation, want 1", got)
	}
	row := recordOf(t, issuer, op)
	switch {
	case row.Issued != 2:
		t.Errorf("Issued = %d after two calls, want 2", row.Issued)
	case !row.First.Equal(issueWindow):
		t.Errorf("First = %v, want the earliest instant seen (%v)", row.First, issueWindow)
	case !row.Last.Equal(later):
		t.Errorf("Last = %v, want the latest instant seen (%v)", row.Last, later)
	}
}

// The log reads in surface order, with operations the surface does not carry after it. A published
// operation cannot be sorted alongside one that does not exist on the surface, and a blocked attempt
// is the row a reader is looking for — so it is appended rather than dropped.
func TestTheLogReadsInSurfaceOrderWithUnpublishedAttemptsLast(t *testing.T) {
	t.Parallel()
	issuer, _, _ := metered(t, 10)
	ctx := context.Background()
	for _, op := range []feeder.ReadOperation{
		"HEAD /rate_limit",
		"POST /repos/{owner}/{repo}/deployments", // unpublished, and a write
		"GET /repos/{owner}/{repo}/deployments",
		"GET /zzz/unpublished",
	} {
		_ = issuer.Issue(ctx, op, "core", issueWindow)
	}

	var got []feeder.ReadOperation
	for _, row := range issuer.RequestLog() {
		got = append(got, row.Operation)
	}
	want := []feeder.ReadOperation{
		// Published, sorted as the surface sorts them.
		"GET /repos/{owner}/{repo}/deployments",
		"HEAD /rate_limit",
		// Then the attempts the surface refused, sorted among themselves.
		"GET /zzz/unpublished",
		"POST /repos/{owner}/{repo}/deployments",
	}
	if len(got) != len(want) {
		t.Fatalf("the request log is %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("the request log is %v, want %v", got, want)
		}
	}
}

// A cycle of reads changes no state. This is SC-007 over a real cycle rather than over the table
// alone — and it holds even though two of the four attempts above named a write, because the surface
// refused them.
func TestACycleOfReadsChangesNoState(t *testing.T) {
	t.Parallel()
	issuer, _, _ := metered(t, 10)
	ctx := context.Background()
	for _, op := range []feeder.ReadOperation{
		"GET /repos/{owner}/{repo}/deployments",
		"HEAD /rate_limit",
		"DELETE /repos/{owner}/{repo}/deployments/{id}",
	} {
		_ = issuer.Issue(ctx, op, "core", issueWindow)
	}
	if got := issuer.StateChanges(); got != 0 {
		t.Errorf("StateChanges() = %d over a cycle of reads and one refused write, want 0", got)
	}
}

// Observe on an unmetered issuer is a no-op rather than a panic, because a replay from disk hands
// recorded headers to an issuer that has no budget to put them in.
func TestObserveOnAnUnmeteredIssuerDoesNotPanic(t *testing.T) {
	t.Parallel()
	issuer, err := feeder.NewIssuer(surface(t, reads()), nil, nil)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	issuer.Observe(feeder.Reading{Family: "core", Limit: 100, Remaining: 99, Source: feeder.QuotaReported})
}

// A reading handed to Observe reaches the budget, which is what makes the meter track the platform's
// own number rather than the connector's count of its own calls.
func TestObserveReachesTheBudget(t *testing.T) {
	t.Parallel()
	issuer, budget, _ := metered(t, 10)
	issuer.Observe(feeder.Reading{
		Family: "core", Limit: 100, Remaining: 1,
		Reset: issueWindow.Add(2 * time.Hour), Source: feeder.QuotaReported,
	})
	// Remaining 1 against a reserve of 1: the next call must yield, which it would not have before.
	err := issuer.Issue(context.Background(), "GET /repos/{owner}/{repo}/deployments", "core", issueWindow)
	if _, yielded := feeder.YieldedForQuota(err); !yielded {
		t.Fatalf("after observing a reading at the reserve, Issue returned %v, want a quota yield", err)
	}
	for _, family := range budget.Report().Families {
		if family.Family == "core" && family.Remaining != 1 {
			t.Errorf("the budget reports %d remaining on `core`, want the 1 the reading said",
				family.Remaining)
		}
	}
}

// A nil AreaStats is a legitimate state: a unit test wants the gate and the meter without per-area
// accounting, and a connector that crashed for want of a counter would be reporting telemetry as a
// precondition for feeding.
func TestANilAreaStatsIsNotAPrecondition(t *testing.T) {
	t.Parallel()
	budget, err := feeder.NewQuotaBudget("testhub", feeder.QuotaPolicy{Share: 0.5, Reserve: 1})
	if err != nil {
		t.Fatalf("NewQuotaBudget: %v", err)
	}
	budget.Observe(feeder.Reading{
		Family: "core", Limit: 100, Remaining: 10,
		Reset: issueWindow.Add(time.Hour), Source: feeder.QuotaReported,
	})
	issuer, err := feeder.NewIssuer(surface(t, reads()), budget, nil)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	if err := issuer.Issue(context.Background(), "GET /repos/{owner}/{repo}/deployments", "core", issueWindow); err != nil {
		t.Fatalf("an issuer with no area stats refused a permitted call: %v", err)
	}
	if row := recordOf(t, issuer, "GET /repos/{owner}/{repo}/deployments"); row.Issued != 1 {
		t.Errorf("Issued = %d without area stats, want 1; the request log is not the area report", row.Issued)
	}
}

// One issuer shared across goroutines is the normal case — a connector polls several areas at once —
// and the reserve only means anything if they share one. Run with -race.
func TestConcurrentCallsShareOneLogAndOneBudget(t *testing.T) {
	t.Parallel()
	issuer, budget, _ := metered(t, 100) // share 0.5 of 100 = an allowance of 50
	ctx := context.Background()
	done := make(chan struct{})
	for range 10 {
		go func() {
			defer func() { done <- struct{}{} }()
			for range 4 {
				_ = issuer.Issue(ctx, "GET /repos/{owner}/{repo}/deployments", "core", issueWindow)
				issuer.Observe(feeder.Reading{
					Family: "core", Limit: 100, Remaining: 100,
					Reset: issueWindow.Add(time.Hour), Source: feeder.QuotaReported,
				})
			}
		}()
	}
	for range 10 {
		<-done
	}
	row := recordOf(t, issuer, "GET /repos/{owner}/{repo}/deployments")
	if row.Issued != 40 {
		t.Errorf("Issued = %d after 40 concurrent calls inside an allowance of 50, want 40", row.Issued)
	}
	for _, family := range budget.Report().Families {
		if family.Family == "core" && family.Spent != 40 {
			t.Errorf("the budget spent %d, want 40; ten goroutines sharing one budget must not each "+
				"keep their own count", family.Spent)
		}
	}
}

// The issuer publishes the budget's report and the cycle's area report, because the issuer is the
// object a connector holds. A transport handed the budget separately in order to publish its spending
// would be a transport holding a way to spend calls outside the door.
func TestTheIssuerPublishesWhatItSpent(t *testing.T) {
	t.Parallel()
	issuer, budget, _ := metered(t, 10)
	if err := issuer.Issue(context.Background(), "GET /repos/{owner}/{repo}/deployments", "core", issueWindow); err != nil {
		t.Fatalf("Issue: %v", err)
	}

	report := issuer.Report()
	if report.Platform != "testhub" {
		t.Errorf("Report().Platform = %q, want the surface's platform", report.Platform)
	}
	if len(report.Families) != len(budget.Report().Families) {
		t.Errorf("the issuer reports %d families and the budget %d; they are the same numbers and a "+
			"connector reading one of them has to be reading the other",
			len(report.Families), len(budget.Report().Families))
	}
	var spent int
	for _, family := range report.Families {
		if family.Family == "core" {
			spent = family.Spent
		}
	}
	if spent != 1 {
		t.Errorf("the issuer reports %d spent on `core` after one issued call, want 1", spent)
	}

	areas := issuer.Areas()
	if len(areas) != 1 || areas[0].Area != "deployments" || areas[0].Calls != 1 {
		t.Errorf("Areas() = %+v, want one `deployments` line with one call", areas)
	}
}

// An unmetered run reports its platform and no families. That is a true statement rather than an empty
// one: nothing was metered, so there is nothing to attribute.
func TestAnUnmeteredIssuerReportsItsPlatformAndNoFamilies(t *testing.T) {
	t.Parallel()
	issuer, err := feeder.NewIssuer(surface(t, reads()), nil, nil)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	report := issuer.Report()
	if report.Platform != "testhub" {
		t.Errorf("Report().Platform = %q on an unmetered run, want the surface's platform", report.Platform)
	}
	if len(report.Families) != 0 {
		t.Errorf("Report().Families = %+v on an unmetered run", report.Families)
	}
	if got := issuer.Areas(); got != nil {
		t.Errorf("Areas() = %+v with no accounting, want nil", got)
	}
}

// A deferral is filed under the published area rather than a string at the call site, and an operation
// off the surface has no area to file it under at all — so that case is reported rather than guessed.
func TestADeferralIsFiledUnderThePublishedArea(t *testing.T) {
	t.Parallel()
	issuer, _, stats := metered(t, 10)

	if err := issuer.Deferred("GET /repos/{owner}/{repo}/deployments"); err != nil {
		t.Fatalf("Deferred on a published operation: %v", err)
	}
	report := stats.Report()
	if len(report) != 1 || report[0].Area != "deployments" || report[0].Deferred != 1 {
		t.Errorf("the area report is %+v, want one `deployments` line with one deferral", report)
	}

	err := issuer.Deferred("GET /repos/{owner}/{repo}/contents/{path}")
	var unpublished *feeder.UnpublishedOperationError
	if !errors.As(err, &unpublished) {
		t.Fatalf("Deferred on an unpublished operation returned %v, want UnpublishedOperationError", err)
	}
	if got := stats.Report(); len(got) != 1 {
		t.Errorf("the area report grew to %+v; an operation off the surface has no area to file a "+
			"deferral under, and inventing one would file it against a surface nobody polled", got)
	}
}
