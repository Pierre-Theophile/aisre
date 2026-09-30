// SPDX-License-Identifier: Apache-2.0

package datadog

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// Areas and quota stops (005 T084, T085; contract §5).
//
// Every live call is drawn for an area, and the areas are ranked by the published deferral order:
// transitions and monitor definitions (one read), then discovery, then the event stream, then the APM
// topology, then rollout detection. The client's budget (internal/datadogx/budget.go) makes a lower
// area leave part of the share for the areas ahead of it, so under pressure the connector stops looking
// for new versions first and stops reading alert transitions last.

// Area is what a call is for.
type Area string

// The areas, in the published deferral order (first deferred last).
const (
	// AreaStartup is the startup gate's reads: they happen once, before any area runs, and are never
	// deferred.
	AreaStartup   Area = "startup"
	AreaMonitors  Area = "monitors"
	AreaDiscovery Area = "discovery"
	// AreaChanges is the event stream read as a change source (section D). It yields before discovery
	// does and after rollout detection: an event an organisation posted is a statement, where a
	// rollout inferred from logs is a bound.
	AreaChanges Area = "changes"
	// AreaTopology is the APM service map, read for its structure (section B). It yields after the event
	// stream and before rollout detection: a dependency Datadog states is structure an investigation
	// walks, where a rollout inferred from logs is a bound.
	AreaTopology      Area = "topology"
	AreaRollouts      Area = "rollouts"
	AreaInvestigation Area = "investigation"
)

// MinHeadroom is the share of the window's allowance an area must leave unspent.
var MinHeadroom = map[Area]float64{AreaMonitors: 0, AreaInvestigation: 0, AreaDiscovery: 0.25, AreaChanges: 0.35, AreaTopology: 0.4, AreaRollouts: 0.5}

type areaKey struct{}

// WithArea marks every call made with ctx as drawn for area.
func WithArea(ctx context.Context, area Area) context.Context {
	return context.WithValue(ctx, areaKey{}, area)
}

// AreaOf is the area a call is drawn for; an unmarked call is an investigation's.
func AreaOf(ctx context.Context) Area {
	if a, ok := ctx.Value(areaKey{}).(Area); ok {
		return a
	}
	return AreaInvestigation
}

// DeferralError is a lower area yielding so the areas ahead of it keep their share. It is a quota stop,
// never an absence of findings.
type DeferralError struct {
	Area     Area
	Family   string
	Headroom float64
	// RetryAt is when the bucket's window resets, when Datadog said: the area may try again then.
	RetryAt time.Time
}

func (e *DeferralError) Error() string {
	return fmt.Sprintf("datadog: %s deferred on bucket %s: %.0f%% of the share is left and this area stops "+
		"at %.0f%%, leaving the rest to the areas ahead of it in the published order (FR-082)",
		e.Area, e.Family, e.Headroom*100, MinHeadroom[e.Area]*100)
}

// The typed stop reasons a poll marker states (FR-083): the connector stopped, and why, which is never
// the same fact as having looked and found nothing.
const (
	StopQuota       = "quota"
	StopRateLimited = "rate_limited"
)

// rateLimited is a 429 as the client reports it.
type rateLimited interface {
	HTTPStatus() int
	RetryAfterDuration() time.Duration
}

// stopOf classifies a failed call: a quota stop with the instant its window resets (zero when
// unstated), a 429 with its wait, or neither.
func stopOf(err error) (reason string, wait time.Duration, retryAt time.Time) {
	var d *DeferralError
	if errors.As(err, &d) {
		return StopQuota, 0, d.RetryAt
	}
	if y, ok := feeder.YieldedForQuota(err); ok {
		return StopQuota, 0, y.RetryAt
	}
	var r rateLimited
	if errors.As(err, &r) && r.HTTPStatus() == 429 {
		return StopRateLimited, r.RetryAfterDuration(), time.Time{}
	}
	return "", 0, time.Time{}
}
