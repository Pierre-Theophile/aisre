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
	Capabilities      Capabilities
	Now               func() time.Time
	// Push hands a payload to the feeder's source.
	Push func(ctx context.Context, p feeder.Payload) error

	pollNow chan struct{}
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
	for page := 0; ; page++ {
		raw, err := p.Pager.ListMonitorsPage(ctx, strings.Join(p.Tags, ","), page, size)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			marker.Outcome, marker.Reason = "partial", fmt.Sprintf("page %d: %v", page, err)
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
	raw, err := json.Marshal(marker)
	if err != nil {
		return err
	}
	return p.Push(ctx, feeder.Payload{Kind: PayloadPoll, At: p.now(), Bytes: raw})
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
