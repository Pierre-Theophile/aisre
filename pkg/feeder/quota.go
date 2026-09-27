// SPDX-License-Identifier: Apache-2.0

package feeder

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A budget that is a share of the REMAINING quota, not of a number somebody wrote down
// (004 FR-071, FR-072, FR-073; T035, T036).
//
// ---------------------------------------------------------------------------------------------
// Why "remaining" changes the shape
//
// Feature 003's budget (internal/gcpx/budget.go) paces against a configured per-class limit, because
// GCP states quotas per project and per API and does not tell a caller how much of one is left. A
// REST platform that reports its own remaining allowance is a different problem: the honest budget is
// a share of what is actually left, because the operator's own tooling is spending the same bucket and
// a static share of the *limit* would let two consumers each believe they were inside it.
//
// What this reads is established from the platform's reference rather than assumed:
//
//	x-ratelimit-limit      the maximum requests per hour
//	x-ratelimit-remaining  requests left in the current window
//	x-ratelimit-used       requests made in the current window
//	x-ratelimit-reset      when the window resets, UTC epoch seconds
//	x-ratelimit-resource   WHICH resource family the request counted against
//
// GitHub documents these as "the authoritative source for your current rate limit status", which is
// why they and not the cycle's opening endpoint reading drive the decrement. The endpoint alone would
// let a cycle overspend between readings; the headers alone would lose the families the connector has
// not called yet. Both, and the endpoint once per cycle.
//
// `x-ratelimit-resource` is the reason this does not maintain its own map from endpoint to family. The
// platform says which bucket a call came out of, so a call metered against the wrong one is not a
// mistake this code can make.
//
// # Where no remaining quota is reported
//
// FR-071's other half: the budget falls back to a static allowance and **the usage report says which
// it used**. That is not a detail. A static budget presented as a share of something measured is a
// number an operator would trust more than it deserves, so Reading carries its own provenance and
// UsageReport prints it.

// QuotaSource is where a budget's numbers came from.
type QuotaSource string

const (
	// QuotaUnknown is a budget nobody has given a reading. It spends nothing: a budget with no
	// numbers is not an unlimited budget.
	QuotaUnknown QuotaSource = ""
	// QuotaReported means the platform stated its remaining allowance.
	QuotaReported QuotaSource = "platform_reported"
	// QuotaStatic means the platform reports none and the allowance is the configured fallback. The
	// word "static" is in the value so a usage report cannot be read as a measurement.
	QuotaStatic QuotaSource = "static_fallback"
)

// Reading is one statement of what is left, for one resource family.
type Reading struct {
	// Family is the platform's own name for the bucket — `x-ratelimit-resource` where it says so, or
	// the configured family name under a static fallback.
	Family string
	// Limit, Remaining and Used are as the platform reported them. Under a static fallback Limit is
	// the configured allowance and Remaining is what is left of it.
	Limit     int
	Remaining int
	Used      int
	// Reset is when the window refills. Under a static fallback it is the configured cycle boundary.
	Reset time.Time
	// Source is the provenance, and it travels with the numbers so a report cannot present a
	// fallback as a measurement.
	Source QuotaSource
	// Period is the length of the platform's rate-limit window where it states one (Datadog's
	// `X-RateLimit-Period`). Zero means not stated.
	Period time.Duration
}

// ReadingFromHeaders reads one response's rate-limit headers.
//
// It returns ok=false when the platform said nothing, which is a first-class answer rather than a
// zero reading: "no headers" and "no requests left" are opposite facts and a caller that confused them
// would either stop for nothing or spend somebody else's allowance.
func ReadingFromHeaders(h http.Header) (Reading, bool) {
	limit, hasLimit := headerInt(h, "X-RateLimit-Limit")
	remaining, hasRemaining := headerInt(h, "X-RateLimit-Remaining")
	if !hasLimit || !hasRemaining {
		return Reading{}, false
	}
	used, _ := headerInt(h, "X-RateLimit-Used")
	reading := Reading{
		Family:    strings.TrimSpace(h.Get("X-RateLimit-Resource")),
		Limit:     limit,
		Remaining: remaining,
		Used:      used,
		Source:    QuotaReported,
	}
	if seconds, ok := headerInt(h, "X-RateLimit-Reset"); ok {
		reading.Reset = time.Unix(int64(seconds), 0).UTC()
	}
	if reading.Family == "" {
		// The platform reported numbers without naming the bucket. Recorded as `unnamed` rather than
		// guessed from the path: a family this code invented would be a family the usage report
		// attributes wrongly, and the numbers are still true of whatever bucket they came from.
		reading.Family = "unnamed"
	}
	return reading, true
}

// DatadogReadingFromHeaders reads Datadog's rate-limit headers (005 T021; research §2.3).
//
// It is a second reader rather than a flag on ReadingFromHeaders because the two platforms spell two
// things differently, and the GitHub reader applied to Datadog is wrong silently rather than loudly:
//
//   - `X-RateLimit-Reset` is **seconds until** the window resets, not an epoch. Read as an epoch it
//     is an instant in January 1970, so every bucket would look already refilled.
//   - The bucket is named by `X-RateLimit-Name`, and different endpoints can share one name, which is
//     exactly what makes it the right key for a budget: two endpoints drawing on one bucket must be
//     budgeted as one. GitHub's reader looks for `X-RateLimit-Resource` and would call every Datadog
//     bucket "unnamed", merging distinct buckets into one.
//
// receivedAt is the instant the response was received. The reset is relative to it, so the reading is
// a function of the response and that instant — in recorded mode the recorded instant — and never of
// the wall clock at replay (FR-005).
func DatadogReadingFromHeaders(h http.Header, receivedAt time.Time) (Reading, bool) {
	limit, hasLimit := headerInt(h, "X-RateLimit-Limit")
	remaining, hasRemaining := headerInt(h, "X-RateLimit-Remaining")
	if !hasLimit || !hasRemaining {
		return Reading{}, false
	}
	reading := Reading{
		Family:    strings.TrimSpace(h.Get("X-RateLimit-Name")),
		Limit:     limit,
		Remaining: remaining,
		Used:      max(limit-remaining, 0),
		Source:    QuotaReported,
	}
	if seconds, ok := headerInt(h, "X-RateLimit-Reset"); ok && seconds >= 0 && !receivedAt.IsZero() {
		reading.Reset = receivedAt.UTC().Add(time.Duration(seconds) * time.Second)
	}
	if seconds, ok := headerInt(h, "X-RateLimit-Period"); ok && seconds > 0 {
		reading.Period = time.Duration(seconds) * time.Second
	}
	if reading.Family == "" {
		reading.Family = "unnamed" // as ReadingFromHeaders: never a bucket name this code invented
	}
	return reading, true
}

func headerInt(h http.Header, key string) (int, bool) {
	raw := strings.TrimSpace(h.Get(key))
	if raw == "" {
		return 0, false
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, false
	}
	return value, true
}

// QuotaYieldError is FR-073's typed reason: the connector stopped because it ran out of its share,
// which is a different fact from having looked and found nothing.
//
// A caller must be able to tell them apart without reading a message, because a run that stopped for
// quota established nothing about the window it did not read — and reporting that as "no deployments"
// would be a confident answer to a question nobody asked.
type QuotaYieldError struct {
	Platform  string
	Family    string
	Remaining int
	Reserve   int
	RetryAt   time.Time
	Source    QuotaSource
}

func (e *QuotaYieldError) Error() string {
	when := "an unknown instant"
	if !e.RetryAt.IsZero() {
		when = e.RetryAt.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf("feeder: %s yielded on %s: %d call(s) left against a reserve of %d that is never "+
		"spent, from a %s allowance; retry after %s. This is a quota stop and not an absence of "+
		"findings (FR-073)", e.Platform, e.Family, e.Remaining, e.Reserve, e.Source, when)
}

// YieldedForQuota reports whether err is a quota yield, so a caller distinguishes "we stopped for
// quota" from "we looked and found nothing" without matching strings.
func YieldedForQuota(err error) (*QuotaYieldError, bool) {
	var yield *QuotaYieldError
	if errors.As(err, &yield) {
		return yield, true
	}
	return nil, false
}

// QuotaPolicy is the published share and reserve.
type QuotaPolicy struct {
	// Share is the fraction of what is LEFT that this connector may spend in a cycle.
	Share float64
	// Reserve is the number of calls left unspent whatever the share works out to, so that an
	// operator's own tooling and a person debugging are not locked out by a poller (FR-072).
	Reserve int
	// StaticAllowance is the per-cycle allowance used where the platform reports no remaining quota.
	// Zero means the connector may not run against such a platform at all, which is a stronger
	// default than picking a number nobody chose.
	StaticAllowance int
}

// QuotaBudget paces one connector against what a platform says is left.
//
// Safe for concurrent use: a connector polling several areas shares one, which is the only way the
// reserve means anything — two budgets would each honour it and together spend twice the share.
type QuotaBudget struct {
	platform string
	policy   QuotaPolicy

	mu       sync.Mutex
	readings map[string]Reading
	spent    map[string]int
	// allowance is how many calls the share works out to for the current window, fixed when the
	// family's first reading of that window arrives.
	//
	// Fixed rather than recomputed per call, and that is the whole point of the share. Recomputing it
	// against the latest reading would make it a floor and nothing else: every call would ask "is half
	// of what is left still more than zero", the answer would stay yes almost to the end, and the
	// share would be inert while looking enforced. Fixing it against the window's opening reading is
	// what makes "a share of what is left" a bound on this cycle rather than a description of it.
	allowance map[string]int
	// window is the reset instant the allowance was derived for, so a new window re-derives it.
	window map[string]time.Time
}

// NewQuotaBudget builds a budget. A share outside (0,1] or a negative reserve is a configuration
// error rather than something to clamp: a clamped share is a share nobody chose.
func NewQuotaBudget(platform string, policy QuotaPolicy) (*QuotaBudget, error) {
	switch {
	case strings.TrimSpace(platform) == "":
		return nil, fmt.Errorf("feeder: a quota budget with no platform name")
	case policy.Share <= 0 || policy.Share > 1:
		return nil, fmt.Errorf("feeder: %s's quota share is %v; it must be in (0,1], and a share "+
			"outside it is a configuration error rather than something to clamp — a clamped share is a "+
			"share nobody chose", platform, policy.Share)
	case policy.Reserve < 0:
		return nil, fmt.Errorf("feeder: %s's quota reserve is %d; a negative reserve is a share of "+
			"somebody else's allowance", platform, policy.Reserve)
	case policy.StaticAllowance < 0:
		return nil, fmt.Errorf("feeder: %s's static allowance is %d", platform, policy.StaticAllowance)
	}
	return &QuotaBudget{
		platform:  platform,
		policy:    policy,
		readings:  map[string]Reading{},
		spent:     map[string]int{},
		allowance: map[string]int{},
		window:    map[string]time.Time{},
	}, nil
}

// Observe records what a response said about one family. It is how the in-cycle decrement stays
// authoritative: the connector does not count its own calls against a number from the last reading,
// it believes the platform's latest one.
func (b *QuotaBudget) Observe(reading Reading) {
	if reading.Family == "" || reading.Source == QuotaUnknown {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.readings[reading.Family] = reading

	// A new window is a new allowance and a fresh spend count. Windows are told apart by their reset
	// instant, which the platform reports; where it reports none the allowance is derived once and
	// stands, because there is nothing to say it has rolled over.
	if previous, known := b.window[reading.Family]; !known || !previous.Equal(reading.Reset) {
		b.window[reading.Family] = reading.Reset
		b.spent[reading.Family] = 0
		b.allowance[reading.Family] = b.allowanceFor(reading)
	}
}

// allowanceFor is the share of what remains, floored by the reserve. Both bounds come off the same
// number: a share computed against the LIMIT would let this connector and the operator's own tooling
// each believe they were inside it.
func (b *QuotaBudget) allowanceFor(reading Reading) int {
	share := int(float64(reading.Remaining) * b.policy.Share)
	if headroom := reading.Remaining - b.policy.Reserve; share > headroom {
		share = headroom
	}
	if share < 0 {
		return 0
	}
	return share
}

// Allow decides whether one more call may be made against a family.
//
// It refuses with a typed yield rather than a boolean, so that a caller stopping here reports a quota
// stop and cannot accidentally report an absence of findings.
func (b *QuotaBudget) Allow(family string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	reading, known := b.readings[family]
	if !known {
		if b.policy.StaticAllowance == 0 {
			// Nothing measured and nothing configured. A budget with no numbers is not an unlimited
			// budget, so it spends nothing.
			return &QuotaYieldError{Platform: b.platform, Family: family, Source: QuotaUnknown}
		}
		reading = Reading{
			Family: family, Limit: b.policy.StaticAllowance,
			Remaining: b.policy.StaticAllowance - b.spent[family], Source: QuotaStatic,
		}
		if _, derived := b.allowance[family]; !derived {
			b.allowance[family] = b.policy.StaticAllowance
		}
	}

	// Two bounds, and they are different facts. The FLOOR is the reserve: the platform says this much
	// is left and some of it is never ours, whatever our share works out to — that is what keeps an
	// operator's own tooling and a person debugging from being locked out by a poller (FR-072). The
	// CEILING is the share of what was left when the window opened, which is what stops a cycle
	// consuming the whole remainder in one pass.
	//
	// The floor applies to a MEASURED reading only. A static allowance is not the platform saying what is
	// left — it is the operator's bootstrap for before the platform has said anything — so there is no
	// platform remainder for the reserve to protect, and applying it would turn any static allowance
	// smaller than the reserve into zero: a connector that can never make the one read that would tell it
	// the real numbers (004 T157 found exactly that).
	switch {
	case reading.Source != QuotaStatic && reading.Remaining <= b.policy.Reserve:
		return &QuotaYieldError{
			Platform: b.platform, Family: family, Remaining: reading.Remaining,
			Reserve: b.policy.Reserve, RetryAt: reading.Reset, Source: reading.Source,
		}
	case b.spent[family] >= b.allowance[family]:
		return &QuotaYieldError{
			Platform: b.platform, Family: family, Remaining: reading.Remaining,
			Reserve: b.policy.Reserve, RetryAt: reading.Reset, Source: reading.Source,
		}
	}
	b.spent[family]++
	return nil
}

// UsageReport is what the run reports about its own spending, per family.
type UsageReport struct {
	Platform string
	Families []FamilyUsage
}

// FamilyUsage is one family's line of the report.
type FamilyUsage struct {
	Family string
	// Spent is how many calls this connector made.
	Spent int
	// Limit and Remaining are the platform's last statement, or the static allowance.
	Limit     int
	Remaining int
	// Source is FR-071's "say which it used". A report that omitted it would present a static
	// allowance as a measured share, which is the one way this report could mislead.
	Source QuotaSource
}

// Report renders the usage, families in name order so two runs of the same shape read the same.
func (b *QuotaBudget) Report() UsageReport {
	b.mu.Lock()
	defer b.mu.Unlock()

	names := make([]string, 0, len(b.spent))
	seen := map[string]bool{}
	for family := range b.spent {
		names, seen[family] = append(names, family), true
	}
	for family := range b.readings {
		if !seen[family] {
			names = append(names, family)
		}
	}
	slices.Sort(names)

	report := UsageReport{Platform: b.platform, Families: make([]FamilyUsage, 0, len(names))}
	for _, family := range names {
		usage := FamilyUsage{Family: family, Spent: b.spent[family], Source: QuotaStatic}
		if reading, ok := b.readings[family]; ok {
			usage.Limit, usage.Remaining, usage.Source = reading.Limit, reading.Remaining, reading.Source
		} else if b.policy.StaticAllowance > 0 {
			usage.Limit = b.policy.StaticAllowance
			usage.Remaining = b.policy.StaticAllowance - b.spent[family]
		}
		report.Families = append(report.Families, usage)
	}
	return report
}

// Expire forgets a family's reading once the window it describes has reset. Without it a reading that
// said "nothing left" — a 429's — would stand forever: the budget would refuse every call, and no call
// would ever bring the reading that says the window refilled. The next call is then drawn from the
// static allowance, as before any reading, and its response re-derives the share.
func (b *QuotaBudget) Expire(family string, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	reading, ok := b.readings[family]
	if !ok || reading.Reset.IsZero() || now.Before(reading.Reset) {
		return
	}
	delete(b.readings, family)
	delete(b.allowance, family)
	delete(b.window, family)
	b.spent[family] = 0
}

// Headroom is the share of this window's allowance still unspent for a family, in [0,1], and whether
// the budget has an allowance for it at all. It is what lets a connector with several areas defer the
// less urgent ones first (005 FR-082): an area may be asked to leave a fraction of the share for the
// areas ahead of it in the published order, rather than every area racing for the last call.
func (b *QuotaBudget) Headroom(family string) (float64, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	allowance, ok := b.allowance[family]
	if !ok {
		return 1, false
	}
	if allowance <= 0 {
		return 0, true
	}
	left := float64(allowance-b.spent[family]) / float64(allowance)
	if left < 0 {
		left = 0
	}
	return left, true
}
