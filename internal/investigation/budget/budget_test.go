// SPDX-License-Identifier: Apache-2.0

package budget_test

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/investigation/budget"
	"github.com/Pierre-Theophile/aisre/internal/investigation/model"
)

// The budget manager (tasks.md T067–T073; FR-043, FR-044, FR-045, FR-047, FR-047a, FR-047b).
//
// Every test here runs on a fake clock. Wall time is a budget dimension, and a test that slept to
// exercise it would be a test that takes ten minutes to prove the ten-minute hard stop.

// clock is a controllable clock.
type clock struct{ now time.Time }

func newClock() *clock {
	return &clock{now: time.Date(2026, 9, 1, 14, 32, 0, 0, time.UTC)}
}

func (c *clock) tick(d time.Duration) { c.now = c.now.Add(d) }
func (c *clock) fn() func() time.Time { return func() time.Time { return c.now } }

func managerFor(t *testing.T, profile budget.Profile, c *clock) *budget.Manager {
	t.Helper()
	m, err := budget.New(profile, budget.OperatorCaps{}, c.fn())
	if err != nil {
		t.Fatalf("new budget: %v", err)
	}
	return m
}

func workerRequest(class string) budget.Request {
	return budget.Request{
		Kind:               budget.KindWorker,
		Worker:             "metrics",
		Backend:            "recorded",
		CostClass:          class,
		WindowWidth:        15 * time.Minute,
		ServesHypothesisID: "h-1",
		Question:           "did the error rate move across the onset?",
		DeepLink:           "https://example.invalid/q/1",
	}
}

// TestTheReserveIsExactlyFifteenPercent: a published constant, asserted, not an approximation
// (FR-045, SC-005).
func TestTheReserveIsExactlyFifteenPercent(t *testing.T) {
	t.Parallel()

	if budget.ReserveFraction != 0.15 {
		t.Fatalf("ReserveFraction = %v, want exactly 0.15", budget.ReserveFraction)
	}

	profile := budget.PageProfile()
	if profile.SynthesisReserveFraction != 0.15 {
		t.Errorf("page profile reserve = %v, want 0.15", profile.SynthesisReserveFraction)
	}
	profile.SynthesisReserveFraction = 0.10
	err := profile.Validate()
	if err == nil || !strings.Contains(err.Error(), "not configurable") {
		t.Fatalf("a profile that moved the reserve was accepted: %v", err)
	}
}

// TestTheUnreservedPortionIsSpentBeforeTheReserve: the page profile allows 40 calls to any one
// backend, so 34 (floor of 85%) may be spent on evidence and the 35th enters synthesis_only.
//
// It is the *per-backend* budget that binds first here rather than the per-cost-class one, and the
// test asserts that too: a refusal that did not name the dimension that actually bound would be a
// stop nobody can act on (FR-045b).
func TestTheUnreservedPortionIsSpentBeforeTheReserve(t *testing.T) {
	t.Parallel()

	c := newClock()
	m := managerFor(t, budget.PageProfile(), c)

	const unreserved = 34 // floor(40 * 0.85)
	for i := 0; i < unreserved; i++ {
		decision := m.Admit(workerRequest(budget.ClassCheap))
		if !decision.Admitted {
			t.Fatalf("call %d was refused before the reserve: %s", i+1, decision.Reason)
		}
		m.RecordWorkerCall(budget.WorkerCall{
			Worker: "metrics", Backend: "recorded", CostClass: budget.ClassCheap,
			QuotaUndetermined: true, Reservation: decision.Reservation,
		})
	}
	if m.Mode() != budget.ModeNormal {
		t.Fatalf("mode = %s after the unreserved portion, want normal", m.Mode())
	}

	decision := m.Admit(workerRequest(budget.ClassCheap))
	if decision.Admitted {
		t.Fatal("a call past the unreserved portion was admitted; the reserve is not a suggestion")
	}
	if !decision.EnteredReserve || m.Mode() != budget.ModeSynthesisOnly {
		t.Fatalf("mode = %s, want synthesis_only once the unreserved portion is exhausted", m.Mode())
	}
	if decision.Budget != "backend_calls:recorded" {
		t.Errorf("the refusal names %q; the per-backend budget is the one that bound", decision.Budget)
	}
}

// TestThePerCostClassBudgetBindsSeparately: an expensive call is not a cheap call, and a flat call
// count would price them the same (FR-043).
func TestThePerCostClassBudgetBindsSeparately(t *testing.T) {
	t.Parallel()

	c := newClock()
	profile := budget.PageProfile()
	profile.CallsPerBackend = map[string]int64{budget.AnyBackend: 1000}
	m := managerFor(t, profile, c)

	const unreserved = 6 // floor(8 * 0.85)
	for i := 0; i < unreserved; i++ {
		decision := m.Admit(workerRequest(budget.ClassExpensive))
		if !decision.Admitted {
			t.Fatalf("expensive call %d refused: %s", i+1, decision.Reason)
		}
		m.RecordWorkerCall(budget.WorkerCall{
			Worker: "logs", Backend: "recorded", CostClass: budget.ClassExpensive,
			QuotaUndetermined: true, Reservation: decision.Reservation,
		})
	}
	decision := m.Admit(workerRequest(budget.ClassExpensive))
	if decision.Admitted {
		t.Fatal("a seventh expensive call was admitted against a budget of 8 with a 15% reserve")
	}
	if decision.Budget != "worker_calls:"+budget.ClassExpensive {
		t.Errorf("budget = %q, want the expensive cost class", decision.Budget)
	}
}

// TestThereIsNoPathBackToNormal: continuing past a budget and discarding partial results are both
// impossible, so synthesis_only is terminal (FR-045).
func TestThereIsNoPathBackToNormal(t *testing.T) {
	t.Parallel()

	c := newClock()
	m := managerFor(t, budget.PageProfile(), c)
	m.EnterSynthesisOnly("model_tokens: the unreserved portion is gone")
	m.EnterSynthesisOnly("wall_time: this one came later")

	if m.Mode() != budget.ModeSynthesisOnly {
		t.Fatalf("mode = %s, want synthesis_only", m.Mode())
	}
	if !strings.Contains(m.ReserveReason(), "model_tokens") {
		t.Errorf("reserve reason = %q; the first dimension to run out is the one that bound", m.ReserveReason())
	}

	if decision := m.Admit(workerRequest(budget.ClassCheap)); decision.Admitted {
		t.Error("an evidence-gathering call was admitted in synthesis_only")
	}
	synthesis := budget.Request{Kind: budget.KindModel, ModelID: "claude-opus-5", Tokens: 1000, Synthesis: true}
	if decision := m.Admit(synthesis); !decision.Admitted {
		t.Errorf("the closing synthesis was refused from the reserve: %s", decision.Reason)
	}
}

// TestARefusedCallIsRecordedAsAnIntent: a call that would breach a budget is never issued, and its
// intent is recorded with the hypothesis it served and its deep link (FR-047a).
func TestARefusedCallIsRecordedAsAnIntent(t *testing.T) {
	t.Parallel()

	c := newClock()
	profile := budget.PageProfile()
	profile.CostUnits = 1000
	m := managerFor(t, profile, c)

	decision := m.Admit(budget.Request{
		Kind: budget.KindModel, ModelID: "claude-fable-5-1", Tokens: 999,
		ServesHypothesisID: "h-2", Question: "would the version split settle it?",
		DeepLink: "https://example.invalid/q/2",
	})
	if decision.Admitted {
		t.Fatal("a call past the unreserved token budget was admitted")
	}
	intents := m.Intents()
	if len(intents) != 1 {
		t.Fatalf("intents = %d, want 1", len(intents))
	}
	if intents[0].HypothesisID != "h-2" || intents[0].DeepLink == "" {
		t.Errorf("the refused call was not recorded with its hypothesis and deep link: %+v", intents[0])
	}
}

// TestTheHardStopIsAbsolute: nothing runs past the profile's wall time, not even the synthesis.
func TestTheHardStopIsAbsolute(t *testing.T) {
	t.Parallel()

	c := newClock()
	m := managerFor(t, budget.PageProfile(), c)

	c.tick(11 * time.Minute)
	decision := m.Admit(budget.Request{Kind: budget.KindModel, Tokens: 10, Synthesis: true})
	if decision.Admitted {
		t.Fatal("a call was admitted past the 10-minute hard stop")
	}
	if decision.Budget != "wall_time" {
		t.Errorf("budget = %q, want wall_time", decision.Budget)
	}
}

// TestWallTimeEntersTheReserveAtEightyFivePercent.
func TestWallTimeEntersTheReserveAtEightyFivePercent(t *testing.T) {
	t.Parallel()

	c := newClock()
	m := managerFor(t, budget.PageProfile(), c)

	c.tick(8 * time.Minute) // 480s of 600s, below 510s
	if decision := m.Admit(workerRequest(budget.ClassCheap)); !decision.Admitted {
		t.Fatalf("a call at 8 minutes was refused: %s", decision.Reason)
	}
	c.tick(1 * time.Minute) // 540s, past the unreserved 510s
	if decision := m.Admit(workerRequest(budget.ClassCheap)); decision.Admitted {
		t.Fatal("a call was admitted past the unreserved share of wall time")
	}
	if m.Mode() != budget.ModeSynthesisOnly {
		t.Fatalf("mode = %s, want synthesis_only", m.Mode())
	}
}

// TestQuotaShareBoundsSpendAgainstWhatTheVendorSaysIsLeft (FR-047a).
func TestQuotaShareBoundsSpendAgainstWhatTheVendorSaysIsLeft(t *testing.T) {
	t.Parallel()

	c := newClock()
	m := managerFor(t, budget.PageProfile(), c)

	// The backend reports 8 calls of quota remaining; the share is 0.25, so 2 calls.
	for i := 0; i < 2; i++ {
		decision := m.Admit(workerRequest(budget.ClassCheap))
		if !decision.Admitted {
			t.Fatalf("call %d refused: %s", i+1, decision.Reason)
		}
		m.RecordWorkerCall(budget.WorkerCall{
			Worker: "metrics", Backend: "recorded", CostClass: budget.ClassCheap,
			RemainingQuota: 8, QuotaWindow: time.Hour, Reservation: decision.Reservation,
		})
	}
	decision := m.Admit(workerRequest(budget.ClassCheap))
	if decision.Admitted {
		t.Fatal("a third call was admitted against a quarter of 8 remaining calls")
	}
	if !strings.Contains(decision.Budget, "quota_share") {
		t.Errorf("budget = %q, want the quota share", decision.Budget)
	}
	if share := m.QuotaShareUsed()["recorded"]; share != 0.25 {
		t.Errorf("quota share used = %v, want 0.25 recorded per call", share)
	}
	if observed := m.RemainingQuotaObserved()["recorded"]; observed != 8 {
		t.Errorf("remaining quota observed = %d, want 8", observed)
	}
}

// TestAVendorThatReportsNoQuotaFallsBackAndSaysSo (FR-047a).
func TestAVendorThatReportsNoQuotaFallsBackAndSaysSo(t *testing.T) {
	t.Parallel()

	c := newClock()
	m := managerFor(t, budget.PageProfile(), c)
	for i := 0; i < 3; i++ {
		m.RecordWorkerCall(budget.WorkerCall{
			Worker: "metrics", Backend: "recorded", CostClass: budget.ClassCheap,
			QuotaUndetermined: true,
		})
	}
	if decision := m.Admit(workerRequest(budget.ClassCheap)); !decision.Admitted {
		t.Fatalf("a call was refused on a quota nobody reported: %s", decision.Reason)
	}
	fallbacks := m.QuotaFallbacks()
	if len(fallbacks) != 1 || fallbacks[0] != "recorded" {
		t.Fatalf("fallbacks = %v, want [recorded]; falling back is recorded, not assumed", fallbacks)
	}
	if !strings.Contains(m.Report(), "the absolute per-backend budget governed instead") {
		t.Error("the spend report does not say the absolute budget governed")
	}
}

// TestDiminishingReturnsFiresAtThePublishedThreshold: ‖p_t − p_(t−5)‖₁ < 0.02 (FR-045a).
func TestDiminishingReturnsFiresAtThePublishedThreshold(t *testing.T) {
	t.Parallel()

	if budget.DiminishingWindow != 5 || budget.DiminishingEpsilon != 0.02 {
		t.Fatalf("published values moved: n = %d, ε = %v", budget.DiminishingWindow, budget.DiminishingEpsilon)
	}

	c := newClock()
	m := managerFor(t, budget.PageProfile(), c)

	stable := map[string]float64{"h-1": 0.60, "h-2": 0.30, "h-no-observed-change": 0.10}
	for i := 0; i < budget.DiminishingWindow; i++ {
		if m.Observe(stable) {
			t.Fatalf("the stop fired at observation %d, before %d calls existed to compare over",
				i+1, budget.DiminishingWindow)
		}
	}
	if !m.Observe(stable) {
		t.Fatal("the stop did not fire on an unchanged posterior")
	}
	if detail := m.DiminishingDetail(); !strings.Contains(detail, "0.02") {
		t.Errorf("the detail does not name the threshold: %q", detail)
	}
}

// TestDiminishingReturnsDoesNotFireWhileThePosteriorMoves.
func TestDiminishingReturnsDoesNotFireWhileThePosteriorMoves(t *testing.T) {
	t.Parallel()

	c := newClock()
	m := managerFor(t, budget.PageProfile(), c)

	for i := 0; i < 12; i++ {
		shift := float64(i) * 0.05
		if m.Observe(map[string]float64{"h-1": 0.5 + shift, "h-2": 0.5 - shift}) {
			t.Fatalf("the stop fired at observation %d while the posterior was still moving", i+1)
		}
	}
}

// TestTheNonConfigurableSetIsRefusedWithAReason (FR-047b).
func TestTheNonConfigurableSetIsRefusedWithAReason(t *testing.T) {
	t.Parallel()

	for _, setting := range []string{
		budget.SettingWallTime, budget.SettingCostUnits, budget.SettingCallsPerBackend,
		budget.SettingCallsPerCostClass, budget.SettingMaxWindow,
		budget.SettingMaxConcurrentInvestigations, budget.SettingDailyGlobalSpend,
	} {
		if err := budget.Configure(setting); err != nil {
			t.Errorf("%s is configurable but was refused: %v", setting, err)
		}
	}

	for _, setting := range []string{
		budget.SettingEvidenceRequired, budget.SettingReadOnly, budget.SettingRecordCalls,
		budget.SettingVerificationPass, budget.SettingSynthesisReserve,
	} {
		err := budget.Configure(setting)
		if err == nil {
			t.Errorf("%s was accepted as configurable", setting)
			continue
		}
		if !errors.Is(err, budget.ErrNotConfigurable) {
			t.Errorf("%s: error does not match ErrNotConfigurable: %v", setting, err)
		}
		var refusal *budget.RefusalError
		if !errors.As(err, &refusal) || refusal.Reason == "" {
			t.Errorf("%s was refused without a reason: %v", setting, err)
		}
	}

	if err := budget.Configure("something_invented"); err == nil {
		t.Error("an unpublished setting was accepted")
	}
}

// TestTheSpendReportCoversEveryBudget (FR-044, FR-048, SC-008).
func TestTheSpendReportCoversEveryBudget(t *testing.T) {
	t.Parallel()

	c := newClock()
	m := managerFor(t, budget.PageProfile(), c)

	m.RecordModelCall("claude-fable-5-1", model.Usage{Input: 1200, CacheWrite: 9800, CacheRead: 0, Output: 640})
	m.RecordModelCall("claude-fable-5-1", model.Usage{Input: 100, CacheRead: 9800, Output: 220})
	m.RecordModelCall("claude-opus-5", model.Usage{Input: 4000, Output: 300})
	m.RecordWorkerCall(budget.WorkerCall{
		Worker: "metrics", Backend: "recorded", CostClass: budget.ClassStandard,
		WindowWidth: 30 * time.Minute, RemainingQuota: 1000, QuotaWindow: time.Hour,
	})
	m.RecordWorkerCall(budget.WorkerCall{
		Worker: "logs", Backend: "recorded", CostClass: budget.ClassExpensive,
		WindowWidth: 90 * time.Minute, RemainingQuota: 999, QuotaWindow: time.Hour,
	})
	c.tick(42 * time.Second)

	spend, err := m.Spend(nil)
	if err != nil {
		t.Fatalf("spend: %v", err)
	}
	if spend.GetWallTimeSeconds() != 42 {
		t.Errorf("wall time = %ds, want 42", spend.GetWallTimeSeconds())
	}
	tokens := spend.GetTokensByModelAndClass()
	for key, want := range map[string]int64{
		"claude-fable-5-1:input":       1300,
		"claude-fable-5-1:cache_write": 9800,
		"claude-fable-5-1:cache_read":  9800,
		"claude-fable-5-1:output":      860,
		"claude-opus-5:input":          4000,
		"claude-opus-5:output":         300,
	} {
		if tokens[key] != want {
			t.Errorf("tokens[%s] = %d, want %d", key, tokens[key], want)
		}
	}
	if spend.GetCallsByWorker()["metrics"] != 1 || spend.GetCallsByWorker()["logs"] != 1 {
		t.Errorf("calls by worker = %v", spend.GetCallsByWorker())
	}
	if spend.GetCallsByCostClass()[budget.ClassExpensive] != 1 {
		t.Errorf("calls by cost class = %v", spend.GetCallsByCostClass())
	}
	if spend.GetWidestWindowSeconds() != int64((90 * time.Minute).Seconds()) {
		t.Errorf("widest window = %ds, want %d", spend.GetWidestWindowSeconds(), int64((90 * time.Minute).Seconds()))
	}
	if spend.GetLimits().GetName() != "page" {
		t.Errorf("the profile applied was not recorded: %v", spend.GetLimits())
	}
}

// TestPublishedProfilesStateTheirTargetsAndHardStops (FR-047).
func TestPublishedProfilesStateTheirTargetsAndHardStops(t *testing.T) {
	t.Parallel()

	page := budget.PageProfile()
	if page.FirstTestedTarget != 120*time.Second {
		t.Errorf("page first-tested target = %s, want 120s (FR-046a's published figure)", page.FirstTestedTarget)
	}
	if page.SubstantiveTarget != 5*time.Minute {
		t.Errorf("page substantive target = %s, want 5m", page.SubstantiveTarget)
	}
	if page.WallTime != 10*time.Minute {
		t.Errorf("page hard stop = %s, want 10m", page.WallTime)
	}
	if err := page.Validate(); err != nil {
		t.Errorf("the published page profile does not validate: %v", err)
	}

	review := budget.ReviewProfile()
	if review.WallTime != 0 {
		t.Errorf("review wall time = %s, want 0 — quality-bound, operator cap only", review.WallTime)
	}
	if err := review.Validate(); err != nil {
		t.Errorf("the published review profile does not validate: %v", err)
	}
	if !strings.Contains(review.Describe(), "operator cap governs") {
		t.Errorf("review does not say the operator cap governs: %q", review.Describe())
	}
}

// TestTheProfileIsSelectedByAlertPriority (FR-047).
func TestTheProfileIsSelectedByAlertPriority(t *testing.T) {
	t.Parallel()

	for priority, want := range map[string]string{
		"P1": "page", "critical": "page", "": "page",
		"P4": "review", "low": "review",
	} {
		if got := budget.ForPriority(priority).Name; got != want {
			t.Errorf("priority %q selected %s, want %s", priority, got, want)
		}
	}
}

// TestProfilesLoadFromTheCheckedInFile: `--budget-profiles config/budgets.yaml` is the published
// default, and the file and the code must agree.
func TestProfilesLoadFromTheCheckedInFile(t *testing.T) {
	t.Parallel()

	profiles, err := budget.LoadProfiles("../../../config/budgets.yaml")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	page, ok := profiles["page"]
	if !ok {
		t.Fatal("the file publishes no page profile")
	}
	want := budget.PageProfile()
	if page.WallTime != want.WallTime || page.FirstTestedTarget != want.FirstTestedTarget ||
		page.SubstantiveTarget != want.SubstantiveTarget || page.CostUnits != want.CostUnits ||
		page.MaxWindow != want.MaxWindow || page.QuotaShare != want.QuotaShare {
		t.Errorf("config/budgets.yaml has drifted from the published page profile:\n file: %+v\n code: %+v",
			page, want)
	}
	if page.SynthesisReserveFraction != budget.ReserveFraction {
		t.Errorf("file reserve = %v, want %v", page.SynthesisReserveFraction, budget.ReserveFraction)
	}
}

// TestAnOperatorCapOnlyTightens: a cap larger than the profile's does not raise it.
func TestAnOperatorCapOnlyTightens(t *testing.T) {
	t.Parallel()

	c := newClock()
	m, err := budget.New(budget.PageProfile(), budget.OperatorCaps{WallTime: 48 * time.Hour}, c.fn())
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	c.tick(11 * time.Minute)
	if decision := m.Admit(budget.Request{Kind: budget.KindModel, Tokens: 10, Synthesis: true}); decision.Admitted {
		t.Fatal("an operator cap of 48h raised the profile's 10-minute hard stop")
	}
}

// TestTheFirstTestedAndSubstantiveInstantsAreRecorded (SC-013, FR-047).
func TestTheFirstTestedAndSubstantiveInstantsAreRecorded(t *testing.T) {
	t.Parallel()

	c := newClock()
	m := managerFor(t, budget.PageProfile(), c)

	c.tick(28 * time.Second)
	m.MarkFirstTested()
	c.tick(2 * time.Minute)
	m.MarkSubstantive()
	c.tick(time.Minute)
	m.MarkFirstTested() // the first one is the one that counts

	if got := m.FirstTestedAfter(); got != 28*time.Second {
		t.Errorf("first tested after = %s, want 28s", got)
	}
	if got := m.SubstantiveAfter(); got != 2*time.Minute+28*time.Second {
		t.Errorf("substantive after = %s, want 2m28s", got)
	}
	if m.FirstTestedAfter() > budget.PageProfile().FirstTestedTarget {
		t.Errorf("first tested after %s misses the %s target", m.FirstTestedAfter(),
			budget.PageProfile().FirstTestedTarget)
	}
}

// TestConcurrentAdmissionsNeverExceedTheCap: N+1 calls ask to be admitted at the same instant
// against a cap of N, and exactly N are admitted — every time, not usually (FR-030–FR-034).
//
// This is the test that would have caught Phase 8's determinism defect. Admission used to decide
// and book nothing, so two goroutines could both read "six of eight expensive calls spent", both
// see room for a seventh, and both be admitted; the counters only moved afterwards, in
// RecordWorkerCall. Against `rollout-regression-01-incident`, which wants seven expensive calls
// against the six the reserve leaves of eight, that made the whole investigation's call count a
// function of the Go scheduler. The fix is that Admit books under the same lock that decided, so
// the cap is enforced on the way in.
//
// It runs the whole thing many times over, because a check-then-act race that is lost one run in
// six is not caught by running it once.
func TestConcurrentAdmissionsNeverExceedTheCap(t *testing.T) {
	t.Parallel()

	profile := budget.PageProfile()
	profile.CallsPerBackend = map[string]int64{budget.AnyBackend: 1000}
	profile.QuotaShare = 1

	// floor(8 * 0.85) = 6 expensive calls before the reserve, which is the cap the wave hits.
	const capacity = 6

	for run := 0; run < 200; run++ {
		c := newClock()
		m := managerFor(t, profile, c)

		var (
			wg       sync.WaitGroup
			mu       sync.Mutex
			admitted int
		)
		start := make(chan struct{})
		for i := 0; i < capacity+1; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				decision := m.Admit(workerRequest(budget.ClassExpensive))
				if !decision.Admitted {
					return
				}
				mu.Lock()
				admitted++
				mu.Unlock()
				m.RecordWorkerCall(budget.WorkerCall{
					Worker: "logs", Backend: "recorded", CostClass: budget.ClassExpensive,
					QuotaUndetermined: true, Reservation: decision.Reservation,
				})
			}()
		}
		close(start)
		wg.Wait()

		if admitted != capacity {
			t.Fatalf("run %d admitted %d concurrent calls against a cap of %d; "+
				"a cap that admits N+1 under load is not a cap", run+1, admitted, capacity)
		}
		if got := m.CallsByCostClass()[budget.ClassExpensive]; got != capacity {
			t.Fatalf("run %d booked %d %s calls, want %d; the books and the admissions disagree",
				run+1, got, budget.ClassExpensive, capacity)
		}
		// One class out is not the investigation out: cheap and standard still have headroom, so
		// the refusal is local and the engine keeps gathering evidence (admission.go).
		if m.Mode() != budget.ModeNormal {
			t.Fatalf("run %d: mode = %s; one cost class reaching its cap is not the whole run exhausted",
				run+1, m.Mode())
		}
		if len(m.Intents()) != 1 {
			t.Fatalf("run %d filed %d refused intents, want exactly 1", run+1, len(m.Intents()))
		}
		if got := m.Intents()[0].Budget; got != "worker_calls:"+budget.ClassExpensive {
			t.Fatalf("run %d: the refusal is typed %q, want the cost class that bound", run+1, got)
		}
	}
}

// TestAReleasedReservationReturnsItsHeadroom: a call that is admitted and then never issued gives
// its booking back, so the budget is not spent by a call that did not happen.
func TestAReleasedReservationReturnsItsHeadroom(t *testing.T) {
	t.Parallel()

	profile := budget.PageProfile()
	profile.CallsPerBackend = map[string]int64{budget.AnyBackend: 1000}
	m := managerFor(t, profile, newClock())

	const capacity = 6 // floor(8 * 0.85)
	for i := 0; i < capacity; i++ {
		decision := m.Admit(workerRequest(budget.ClassExpensive))
		if !decision.Admitted {
			t.Fatalf("expensive call %d refused: %s", i+1, decision.Reason)
		}
		// Every one of them is admitted and then abandoned.
		decision.Reservation.Release()
	}
	if got := m.CallsByCostClass()[budget.ClassExpensive]; got != 0 {
		t.Fatalf("%d expensive calls are on the books after every reservation was released, want 0", got)
	}
	if m.Mode() != budget.ModeNormal {
		t.Fatalf("mode = %s; releasing the whole unreserved portion should not have entered the reserve", m.Mode())
	}
	// And the headroom is real: the cap can be spent again from scratch.
	for i := 0; i < capacity; i++ {
		decision := m.Admit(workerRequest(budget.ClassExpensive))
		if !decision.Admitted {
			t.Fatalf("expensive call %d refused after the releases: %s", i+1, decision.Reason)
		}
		m.RecordWorkerCall(budget.WorkerCall{
			Worker: "logs", Backend: "recorded", CostClass: budget.ClassExpensive,
			QuotaUndetermined: true, Reservation: decision.Reservation,
		})
	}
	if decision := m.Admit(workerRequest(budget.ClassExpensive)); decision.Admitted {
		t.Fatal("a seventh expensive call was admitted; the released headroom was double-counted")
	}
}
