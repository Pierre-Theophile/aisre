// SPDX-License-Identifier: Apache-2.0

package datadogx

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	ddfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/datadog"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The budget (005 T084–T086; FR-081–FR-084a, contract datadog-feeder.md §5).
//
// Every call is drawn from a share of what Datadog says is left in the call's rate-limit bucket
// (X-RateLimit-Name), with the published reserve never spent — pkg/feeder's QuotaBudget, keyed by the
// bucket each operation was last seen to draw from (an operation's first call, before Datadog has named
// its bucket, is drawn from the operation's own static allowance).
//
// The areas are ranked by the published deferral order, and a lower area must leave part of the share
// for the areas above it: transitions and monitor definitions (one read) take the share down to
// nothing, discovery stops at a quarter left, rollout detection at half. Under pressure the connector
// therefore stops looking for new versions first and stops reading alert transitions last.

// Area is ddfeeder's; the ranking is its MinHeadroom.
type Area = ddfeeder.Area

// The areas, re-exported for the backend and the CLI.
const (
	AreaMonitors      = ddfeeder.AreaMonitors
	AreaDiscovery     = ddfeeder.AreaDiscovery
	AreaRollouts      = ddfeeder.AreaRollouts
	AreaInvestigation = ddfeeder.AreaInvestigation
)

// WithArea marks every call made with ctx as drawn for area.
func WithArea(ctx context.Context, area Area) context.Context { return ddfeeder.WithArea(ctx, area) }

// Budget paces a client.
type Budget struct {
	quota *feeder.QuotaBudget

	mu       sync.Mutex
	familyOf map[feeder.ReadOperation]string
	calls    map[Area]map[string]int
	deferred map[Area]int
}

// NewBudget builds a budget over the published policy.
func NewBudget(policy feeder.QuotaPolicy) (*Budget, error) {
	q, err := feeder.NewQuotaBudget("datadog", policy)
	if err != nil {
		return nil, err
	}
	return &Budget{quota: q, familyOf: map[feeder.ReadOperation]string{}, calls: map[Area]map[string]int{},
		deferred: map[Area]int{}}, nil
}

func (b *Budget) allow(area Area, op feeder.ReadOperation, now time.Time) error {
	b.mu.Lock()
	family, ok := b.familyOf[op]
	if !ok {
		family = string(op)
	}
	b.mu.Unlock()
	b.quota.Expire(family, now)
	if left, known := b.quota.Headroom(family); known && left <= ddfeeder.MinHeadroom[area] && ddfeeder.MinHeadroom[area] > 0 {
		b.mu.Lock()
		b.deferred[area]++
		b.mu.Unlock()
		return &ddfeeder.DeferralError{Area: area, Family: family, Headroom: left}
	}
	if err := b.quota.Allow(family); err != nil {
		b.mu.Lock()
		b.deferred[area]++
		b.mu.Unlock()
		return err
	}
	return nil
}

// sent counts one call that left the process, against the bucket Datadog charged it to when the
// response named one, and the operation's own name otherwise.
func (b *Budget) sent(area Area, op feeder.ReadOperation, reading *feeder.Reading) {
	b.mu.Lock()
	defer b.mu.Unlock()
	family, ok := b.familyOf[op]
	if !ok {
		family = string(op)
	}
	if reading != nil {
		family = reading.Family
	}
	if b.calls[area] == nil {
		b.calls[area] = map[string]int{}
	}
	b.calls[area][family]++
}

func (b *Budget) observe(op feeder.ReadOperation, reading feeder.Reading) {
	b.mu.Lock()
	b.familyOf[op] = reading.Family
	b.mu.Unlock()
	b.quota.Observe(reading)
}

// Calls is how many calls left the process per area, all buckets together: the number a recording's
// call count is checked against, to the exact call (SC-008).
func (b *Budget) Calls() map[Area]int {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[Area]int{}
	for area, families := range b.calls {
		for _, n := range families {
			out[area] += n
		}
	}
	return out
}

// Deferred is how many calls each area yielded to the budget.
func (b *Budget) Deferred() map[Area]int {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[Area]int, len(b.deferred))
	for area, n := range b.deferred {
		out[area] = n
	}
	return out
}

// IsQuotaStop reports whether err is the budget stopping a call: a quota yield or a deferral.
func IsQuotaStop(err error) bool {
	if _, ok := feeder.YieldedForQuota(err); ok {
		return true
	}
	var d *ddfeeder.DeferralError
	return errors.As(err, &d)
}

// Report is the usage report (FR-084): calls per area and bucket, what is left of each bucket and where
// the number came from, and what was deferred.
func (b *Budget) Report() string {
	b.mu.Lock()
	var lines []string
	for area, families := range b.calls {
		for family, n := range families {
			lines = append(lines, fmt.Sprintf("%s: %d call(s) on %s", area, n, family))
		}
	}
	for area, n := range b.deferred {
		lines = append(lines, fmt.Sprintf("%s: %d call(s) deferred for quota", area, n))
	}
	learned := map[string]bool{}
	for op := range b.familyOf {
		learned[string(op)] = true
	}
	b.mu.Unlock()
	for _, f := range b.quota.Report().Families {
		if learned[f.Family] {
			// An operation's first call, drawn from the static allowance before Datadog named its bucket;
			// the bucket's own line states what is left.
			continue
		}
		lines = append(lines, fmt.Sprintf("bucket %s: %d of %d left (%s), %d spent by this connector",
			f.Family, f.Remaining, f.Limit, f.Source, f.Spent))
	}
	sort.Strings(lines)
	return strings.Join(lines, "; ")
}
