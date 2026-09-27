// SPDX-License-Identifier: Apache-2.0

package datadog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/versionstamp"
)

// The live poller (T057, T058; contract §3, §3.1).
//
// It is the only part of the connector that talks to Datadog in a live run, and all it does is turn
// reads into payloads and push them: a page of monitors, then a poll marker saying whether every page
// was read; and, every discovery interval, a discovery tick naming the watched log sources. The feeder
// consumes those exactly as it consumes a recording, so a live run and its replay share one code path.
//
// A doorbell ring asks for a poll now and nothing else: PollNow is a non-blocking signal, several rings
// before the next poll coalesce into one, and nothing a ring carries reaches a payload.

// MonitorPager reads one page of the monitor list as its raw body.
type MonitorPager interface {
	ListMonitorsPage(ctx context.Context, tags string, page, pageSize int) ([]byte, error)
}

// Measurer measures one log source's presence counts over a window (discovery.go). It is the
// discovery reads, bounded: one aggregate for the totals, one for hosts, one per candidate.
type Measurer interface {
	Measure(ctx context.Context, src LogSource, override string, from, to time.Time) (SourceMeasurement, error)
	// Sightings lists the new values of an accepted stamp with their first indexed line (rollouts.go).
	Sightings(ctx context.Context, src LogSource, facet string, from, to time.Time, horizon time.Duration, known map[string]bool) ([]ValueSighting, error)
}

// DefaultPageSize is the monitor list's page size.
const DefaultPageSize = 100

// DefaultDiscoveryInterval is how often the watched log sources are asserted (contract §2).
const DefaultDiscoveryInterval = time.Hour

// Poller pushes the payloads of live polls.
type Poller struct {
	Pager             MonitorPager
	Tags              []string
	PageSize          int
	Interval          time.Duration
	DiscoveryInterval time.Duration
	LogSources        []LogSource
	// Measurer, when set, measures each source at every discovery tick; VersionOverrides are passed
	// to it; DiscoveryWindow is the window measured (default versionstamp.DefaultWindow).
	Measurer         Measurer
	VersionOverrides map[string]string
	DiscoveryWindow  time.Duration
	Capabilities     Capabilities
	Now              func() time.Time
	// Push hands a payload to the feeder's source.
	Push func(ctx context.Context, p feeder.Payload) error

	// FirstSeenHorizon is how far back a new value's first line is looked for. Zero uses the
	// measurer's default of seven days.
	FirstSeenHorizon time.Duration

	// Usage, when set, is the budget's usage report, stated on every poll marker (FR-084).
	Usage func() string

	pollNow chan struct{}
	// resumeAt is when Datadog said to read again after a 429; nothing is read before it.
	resumeAt time.Time
	// deferred are the areas that yielded to the budget since the last poll marker.
	deferred map[Area]bool
	// known are the stamp values already listed, per source. It only saves calls: a restarted poller
	// lists them again, and the feeder re-derives the ids already sent (contract §4).
	known map[string]map[string]bool
}

// PollNow asks for a poll as soon as possible. It never blocks, and rings between two polls are one.
func (p *Poller) PollNow() {
	p.init()
	select {
	case p.pollNow <- struct{}{}:
	default:
	}
}

func (p *Poller) init() {
	if p.pollNow == nil {
		p.pollNow = make(chan struct{}, 1)
	}
}

func (p *Poller) now() time.Time {
	if p.Now != nil {
		return p.Now().UTC()
	}
	return time.Now().UTC()
}

// Discover pushes one discovery tick.
func (p *Poller) Discover(ctx context.Context) error {
	if !p.Capabilities.Enabled(CapLogs) || len(p.LogSources) == 0 {
		return nil
	}
	tick := DiscoveryTick{}
	for _, src := range p.LogSources {
		tick.LogSources = append(tick.LogSources, src.Env+"/"+src.Service)
	}
	if p.Measurer != nil {
		window := p.DiscoveryWindow
		if window <= 0 {
			window = versionstamp.DefaultWindow
		}
		to := p.now().Truncate(time.Minute)
		tick.Window = &DiscoveryWindow{From: to.Add(-window), To: to}
		for _, src := range p.LogSources {
			key := src.Env + "/" + src.Service
			var m SourceMeasurement
			if p.waiting() {
				m = SourceMeasurement{Failed: StopRateLimited}
			} else {
				var err error
				m, err = p.Measurer.Measure(WithArea(ctx, AreaDiscovery), src, p.VersionOverrides[key], tick.Window.From, tick.Window.To)
				if err != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					m = SourceMeasurement{Failed: p.failure(AreaDiscovery, err)}
				}
			}
			m.Source = key
			if m.Failed == "" {
				p.sight(ctx, src, key, &m, *tick.Window)
			}
			tick.Measurements = append(tick.Measurements, m)
		}
	}
	raw, err := json.Marshal(tick)
	if err != nil {
		return err
	}
	return p.Push(ctx, feeder.Payload{Kind: PayloadDiscovery, At: p.now(), Bytes: raw})
}

// PollOnce reads every page of the monitor list and pushes them, then the marker. A page that fails
// ends the poll as partial, naming why: nothing unread is ever evidence of absence (FR-012).
func (p *Poller) PollOnce(ctx context.Context) error {
	if !p.Capabilities.Enabled(CapMonitors) {
		return nil
	}
	size := p.PageSize
	if size <= 0 {
		size = DefaultPageSize
	}
	marker := PollMarker{Outcome: "complete"}
	area := WithArea(ctx, AreaMonitors)
	for page := 0; ; page++ {
		if p.waiting() {
			marker.Outcome, marker.Reason = "partial", fmt.Sprintf("page %d: not read, Datadog asked to wait", page)
			marker.StopReason = StopRateLimited
			break
		}
		raw, err := p.Pager.ListMonitorsPage(area, strings.Join(p.Tags, ","), page, size)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			marker.Outcome, marker.Reason = "partial", fmt.Sprintf("page %d: %v", page, err)
			if reason := p.failure(AreaMonitors, err); reason == StopQuota || reason == StopRateLimited {
				marker.StopReason = reason
			}
			break
		}
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			marker.Outcome, marker.Reason = "partial", fmt.Sprintf("page %d does not parse: %v", page, err)
			break
		}
		marker.Pages++
		if len(items) > 0 {
			if err := p.Push(ctx, feeder.Payload{Kind: PayloadMonitors, At: p.now(), Bytes: raw}); err != nil {
				return err
			}
		}
		if len(items) < size {
			break
		}
	}
	if p.waiting() {
		resume := p.resumeAt
		marker.ResumeAt = &resume
	}
	marker.Deferred = p.takeDeferred()
	if p.Usage != nil {
		marker.Usage = p.Usage()
	}
	raw, err := json.Marshal(marker)
	if err != nil {
		return err
	}
	return p.Push(ctx, feeder.Payload{Kind: PayloadPoll, At: p.now(), Bytes: raw})
}

// waiting reports whether Datadog's Retry-After is still running.
func (p *Poller) waiting() bool {
	return !p.resumeAt.IsZero() && p.now().Before(p.resumeAt)
}

// failure classifies a failed read for area: a quota stop defers the area, a 429 starts the wait
// Datadog asked for. It returns what the payload states: the typed stop, or the error itself.
func (p *Poller) failure(area Area, err error) string {
	reason, wait := stopOf(err)
	switch reason {
	case StopQuota:
		if p.deferred == nil {
			p.deferred = map[Area]bool{}
		}
		p.deferred[area] = true
		return reason
	case StopRateLimited:
		if wait > 0 {
			p.resumeAt = p.now().Add(wait)
		}
		return reason
	default:
		return err.Error()
	}
}

// takeDeferred lists the deferred areas in the published order, and clears them.
func (p *Poller) takeDeferred() []string {
	var out []string
	for _, a := range []Area{AreaMonitors, AreaDiscovery, AreaRollouts, AreaInvestigation} {
		if p.deferred[a] {
			out = append(out, string(a))
		}
	}
	p.deferred = nil
	return out
}

// Run polls every Interval, discovers every DiscoveryInterval, and polls at once when PollNow is
// called, until ctx ends. The first discovery and poll happen immediately.
func (p *Poller) Run(ctx context.Context) error {
	p.init()
	interval := p.Interval
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	discovery := p.DiscoveryInterval
	if discovery <= 0 {
		discovery = DefaultDiscoveryInterval
	}
	if err := p.Discover(ctx); err != nil {
		return err
	}
	if err := p.PollOnce(ctx); err != nil {
		return err
	}
	poll := time.NewTicker(interval)
	defer poll.Stop()
	discover := time.NewTicker(discovery)
	defer discover.Stop()
	for {
		var err error
		select {
		case <-ctx.Done():
			return nil
		case <-poll.C:
			err = p.PollOnce(ctx)
		case <-p.pollNow:
			err = p.PollOnce(ctx)
		case <-discover.C:
			err = p.Discover(ctx)
		}
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
	}
}

// sight adds the new values of the source's accepted stamp to its measurement. A failure is stated on
// the measurement and costs only the rollouts of this interval: the next one looks again.
func (p *Poller) sight(ctx context.Context, src LogSource, key string, m *SourceMeasurement, w DiscoveryWindow) {
	v, err := DecideVerdict(*m, p.VersionOverrides[key], versionstamp.Thresholds{}, w.To.Sub(w.From))
	if err != nil || !v.Stamped() {
		return
	}
	if p.known == nil {
		p.known = map[string]map[string]bool{}
	}
	if p.known[key] == nil {
		p.known[key] = map[string]bool{}
	}
	if p.waiting() {
		m.ValuesFailed = StopRateLimited
		return
	}
	sightings, err := p.Measurer.Sightings(WithArea(ctx, AreaRollouts), src, FacetOf(v.Accepted), w.From, w.To, p.FirstSeenHorizon, p.known[key])
	if err != nil {
		m.ValuesFailed = p.failure(AreaRollouts, err)
		return
	}
	for _, s := range sightings {
		p.known[key][s.Value] = true
	}
	m.Values = sightings
}
