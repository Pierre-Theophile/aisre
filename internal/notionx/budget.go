// SPDX-License-Identifier: Apache-2.0

package notionx

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The connector's call budget (007 FR-044, T007).
//
// Notion documents an average of three requests per second per integration and reports nothing about
// what is left: the only signal is a 429 with Retry-After. So the allowance is static, three calls in
// each wall-clock second, drawn through pkg/feeder's QuotaBudget so the usage report says
// `static_fallback` and cannot be read as a measurement. A call over the allowance waits for the next
// second rather than failing: a reconciliation is hundreds of calls and the point is to pace them.
//
// A 429 closes the budget until the instant Notion named. When that instant is further away than
// MaxWait, the call fails with a PartialPollError instead of blocking a poll for minutes, and the poll
// it belonged to is reported as cut short — a coverage gap, never an absence of pages.

// PerSecond is the static allowance.
const PerSecond = 3

// DefaultMaxWait is the longest a call waits for a closed budget before the poll is cut short.
const DefaultMaxWait = 30 * time.Second

// family is the one quota family: Notion meters the integration as a whole.
const family = "requests"

// PartialPollError is a poll cut short by Notion's rate limit. A caller must tell it apart from "no
// pages" without reading a message.
type PartialPollError struct {
	Op      feeder.ReadOperation
	RetryAt time.Time
}

func (e *PartialPollError) Error() string {
	return fmt.Sprintf("notionx: %s not issued: Notion's rate limit closed the budget until %s; the poll is cut "+
		"short, which is a coverage gap and not an absence of pages (FR-044)", e.Op, e.RetryAt.UTC().Format(time.RFC3339))
}

// IsPartial reports whether err is a poll cut short by the rate limit.
func IsPartial(err error) (*PartialPollError, bool) {
	var partial *PartialPollError
	if errors.As(err, &partial) {
		return partial, true
	}
	return nil, false
}

// BudgetOptions configures a budget.
type BudgetOptions struct {
	// Now is the clock; nil uses time.Now.
	Now func() time.Time
	// Sleep waits; nil uses a timer. Tests pass a fake that advances their clock.
	Sleep func(ctx context.Context, d time.Duration) error
	// MaxWait: see DefaultMaxWait.
	MaxWait time.Duration
}

// Budget paces one integration.
type Budget struct {
	quota   *feeder.QuotaBudget
	issuer  *feeder.Issuer
	now     func() time.Time
	sleep   func(ctx context.Context, d time.Duration) error
	maxWait time.Duration
	metered bool

	mu     sync.Mutex
	closed time.Time
	waited time.Duration
	limits int
}

// NewBudget builds a metered budget.
func NewBudget(opts BudgetOptions) *Budget {
	q, err := feeder.NewQuotaBudget(Platform+" integration", feeder.QuotaPolicy{Share: 1})
	if err != nil {
		panic(err)
	}
	b := newBudget(q, opts)
	b.metered = true
	return b
}

// unmetered is the budget of a client built without one: the surface gate runs, nothing is paced.
func unmetered() *Budget { return newBudget(nil, BudgetOptions{}) }

func newBudget(q *feeder.QuotaBudget, opts BudgetOptions) *Budget {
	issuer, err := feeder.NewIssuer(Surface, q, nil)
	if err != nil {
		panic(err)
	}
	b := &Budget{quota: q, issuer: issuer, now: opts.Now, sleep: opts.Sleep, maxWait: opts.MaxWait}
	if b.now == nil {
		b.now = time.Now
	}
	if b.sleep == nil {
		b.sleep = sleepCtx
	}
	if b.maxWait <= 0 {
		b.maxWait = DefaultMaxWait
	}
	return b
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// allow draws one call through the issuer, waiting for the next second when this one's allowance is
// spent and for Notion's Retry-After when it has answered 429.
func (b *Budget) allow(ctx context.Context, op feeder.ReadOperation) error {
	if !b.metered {
		return b.issuer.Issue(ctx, op, family, b.now())
	}
	for {
		now := b.now()
		b.mu.Lock()
		closed := b.closed
		b.mu.Unlock()
		if now.Before(closed) {
			wait := closed.Sub(now)
			if wait > b.maxWait {
				_ = b.issuer.Deferred(op)
				return &PartialPollError{Op: op, RetryAt: closed}
			}
			if err := b.wait(ctx, wait); err != nil {
				return err
			}
			continue
		}
		b.quota.Expire(family, now)
		if _, ok := b.quota.Headroom(family); !ok {
			b.quota.Observe(feeder.Reading{
				Family: family, Limit: PerSecond, Remaining: PerSecond,
				Reset: now.Truncate(time.Second).Add(time.Second), Source: feeder.QuotaStatic,
			})
		}
		err := b.issuer.Issue(ctx, op, family, now)
		yield, yielded := feeder.YieldedForQuota(err)
		if !yielded {
			return err
		}
		wait := time.Until(yield.RetryAt)
		if !yield.RetryAt.IsZero() {
			wait = yield.RetryAt.Sub(now)
		}
		if wait <= 0 {
			wait = time.Millisecond
		}
		if err := b.wait(ctx, wait); err != nil {
			return err
		}
	}
}

func (b *Budget) wait(ctx context.Context, d time.Duration) error {
	b.mu.Lock()
	b.waited += d
	b.mu.Unlock()
	return b.sleep(ctx, d)
}

// closeUntil closes the budget until Notion's Retry-After has passed.
func (b *Budget) closeUntil(until time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.limits++
	if until.After(b.closed) {
		b.closed = until
	}
	if b.quota != nil {
		b.quota.Observe(feeder.Reading{Family: family, Limit: PerSecond, Remaining: 0, Reset: until,
			Source: feeder.QuotaStatic})
	}
}

// Report is the usage report: calls issued per operation, the allowance and its provenance, the time
// spent waiting, and how many 429s Notion sent.
func (b *Budget) Report() string {
	issued := 0
	for _, row := range b.issuer.RequestLog() {
		issued += row.Issued
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return fmt.Sprintf("%d call(s) at a %d/s allowance (%s); waited %s; rate limited by Notion %d time(s)",
		issued, PerSecond, feeder.QuotaStatic, b.waited.Round(time.Millisecond), b.limits)
}
