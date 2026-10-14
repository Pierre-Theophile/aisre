// SPDX-License-Identifier: Apache-2.0

package slackx

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The reader app's call budget (006 FR-014, T008; research R7).
//
// Slack limits each Web API method per app and workspace, in published tiers of calls per minute,
// and reports neither the limit nor what is left: the only signal is a 429 with `Retry-After` once
// the limit is crossed. So the budget is a static allowance per method and per minute, drawn through
// pkg/feeder's QuotaBudget so that its usage report says `static_fallback` and cannot be read as a
// measurement. A 429 closes the method's window until the instant Slack named.
//
// Three priorities share it, in the order the spec sets: intake, then reading, then anything that
// can wait. A lower priority leaves part of each window for the ones above it — reading stops at a
// quarter of the window left, so a chat digest never spends the call that would have seen a new
// severity channel. Delivery is on the reporter app, which Slack meters separately, so it never
// competes with either.

// Tier is one of Slack's published rate-limit tiers.
type Tier struct {
	Name      string
	PerMinute int
}

// The tiers the reader's methods are in, at their published floor ("20+", "50+", "100+" per minute).
var (
	Tier2 = Tier{Name: "tier2", PerMinute: 20}
	Tier3 = Tier{Name: "tier3", PerMinute: 50}
	Tier4 = Tier{Name: "tier4", PerMinute: 100}
)

// MethodTiers is the published tier of each read operation.
var MethodTiers = map[feeder.ReadOperation]Tier{
	OpAuthTest:             Tier4,
	OpConversationsList:    Tier2,
	OpConversationsInfo:    Tier3,
	OpConversationsHistory: Tier3,
}

// Priority orders the callers that share a method's window.
type Priority int

// The priorities, highest first.
const (
	PriorityIntake Priority = iota
	PriorityReading
	PriorityBackground
)

func (p Priority) String() string {
	switch p {
	case PriorityIntake:
		return "intake"
	case PriorityReading:
		return "reading"
	default:
		return "background"
	}
}

// minHeadroom is the share of a window a priority must leave for the ones above it.
var minHeadroom = map[Priority]float64{PriorityIntake: 0, PriorityReading: 0.25, PriorityBackground: 0.5}

type priorityKey struct{}

// WithPriority marks every call made with ctx as drawn at p. Unmarked, a call is drawn at the
// priority of its operation's area: intake for the channel list, reading for history.
func WithPriority(ctx context.Context, p Priority) context.Context {
	return context.WithValue(ctx, priorityKey{}, p)
}

func priorityOf(ctx context.Context, op feeder.ReadOperation) Priority {
	if p, ok := ctx.Value(priorityKey{}).(Priority); ok {
		return p
	}
	if op == OpConversationsHistory {
		return PriorityReading
	}
	return PriorityIntake
}

// DeferredError is a call a lower priority left for a higher one. Like a quota yield, it is a stop
// and not an absence of findings.
type DeferredError struct {
	Op       feeder.ReadOperation
	Priority Priority
	Headroom float64
	RetryAt  time.Time
}

func (e *DeferredError) Error() string {
	return fmt.Sprintf("slackx: %s deferred at %s priority: %.0f%% of this minute's allowance is left, and "+
		"%.0f%% is kept for the callers above it; retry after %s. This is a budget stop and not an absence "+
		"of findings (FR-014)", e.Op, e.Priority, 100*e.Headroom, 100*minHeadroom[e.Priority],
		e.RetryAt.UTC().Format(time.RFC3339))
}

// Budget paces one reader app.
type Budget struct {
	quota *feeder.QuotaBudget
	now   func() time.Time

	mu       sync.Mutex
	deferred map[Priority]int
	limited  int
}

// NewBudget builds the reader's budget. now is the clock; nil uses time.Now.
func NewBudget(now func() time.Time) *Budget {
	if now == nil {
		now = time.Now
	}
	// Share 1 and no reserve: the allowance is already Slack's floor for the method, and the only
	// other spender of it is this app.
	q, err := feeder.NewQuotaBudget(Platform+" reader app", feeder.QuotaPolicy{Share: 1})
	if err != nil {
		panic(err)
	}
	return &Budget{quota: q, now: now, deferred: map[Priority]int{}}
}

func family(op feeder.ReadOperation) string {
	_, path, _ := strings.Cut(string(op), " ")
	return strings.TrimPrefix(path, "/api/")
}

// window opens the method's current minute if it has none, or if the one it has has passed.
func (b *Budget) window(op feeder.ReadOperation, now time.Time) string {
	name := family(op)
	b.quota.Expire(name, now)
	if _, ok := b.quota.Headroom(name); !ok {
		tier, known := MethodTiers[op]
		if !known {
			tier = Tier2
		}
		b.quota.Observe(feeder.Reading{
			Family: name, Limit: tier.PerMinute, Remaining: tier.PerMinute,
			Reset: now.Truncate(time.Minute).Add(time.Minute), Source: feeder.QuotaStatic,
		})
	}
	return name
}

// allow draws one call, or refuses with a typed stop.
func (b *Budget) allow(ctx context.Context, op feeder.ReadOperation) error {
	now := b.now()
	name := b.window(op, now)
	p := priorityOf(ctx, op)
	if left, ok := b.quota.Headroom(name); ok && minHeadroom[p] > 0 && left <= minHeadroom[p] {
		b.mu.Lock()
		b.deferred[p]++
		b.mu.Unlock()
		retryAt, _ := b.quota.ResetOf(name)
		return &DeferredError{Op: op, Priority: p, Headroom: left, RetryAt: retryAt}
	}
	if err := b.quota.Allow(name); err != nil {
		b.mu.Lock()
		b.deferred[p]++
		b.mu.Unlock()
		return err
	}
	return nil
}

// limitedUntil closes the method's window until Slack's Retry-After has passed.
func (b *Budget) limitedUntil(op feeder.ReadOperation, until time.Time) {
	tier, known := MethodTiers[op]
	if !known {
		tier = Tier2
	}
	b.quota.Observe(feeder.Reading{
		Family: family(op), Limit: tier.PerMinute, Remaining: 0, Reset: until, Source: feeder.QuotaStatic,
	})
	b.mu.Lock()
	b.limited++
	b.mu.Unlock()
}

// Report is the usage report: calls per method in the current window, against a static allowance,
// what each priority deferred, and how many 429s Slack sent.
func (b *Budget) Report() string {
	var lines []string
	for _, f := range b.quota.Report().Families {
		lines = append(lines, fmt.Sprintf("%s: %d call(s) this window of a %d/min allowance (%s)",
			f.Family, f.Spent, f.Limit, f.Source))
	}
	b.mu.Lock()
	for p, n := range b.deferred {
		lines = append(lines, fmt.Sprintf("%s: %d call(s) deferred", p, n))
	}
	if b.limited > 0 {
		lines = append(lines, fmt.Sprintf("rate limited by Slack %d time(s)", b.limited))
	}
	b.mu.Unlock()
	sort.Strings(lines)
	if len(lines) == 0 {
		return "no calls"
	}
	return strings.Join(lines, "; ")
}
