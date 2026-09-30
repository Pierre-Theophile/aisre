// SPDX-License-Identifier: Apache-2.0

package datadog

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The live events poll (T092; the `changes` capability).
//
// It reads the window since the last complete one, less an overlap, page by page, keeps the events the
// configured scope calls changes, and pushes them as `events` payloads followed by an `events-poll`
// marker. The scope is applied HERE and not in the feeder, so a recording holds only in-scope events and
// replays without the configuration (FR-029); the marker counts what was read and what the scope left
// out, which is the difference between "no change happened" and "we were not looking for that kind".
//
// A window that did not finish is partial and says so; nothing unread is evidence that nothing happened
// (FR-012), and the next window reaches back from the last COMPLETE one, so a partial window's gap is
// read again rather than lost.

func (p *Poller) eventsEnabled() bool { return p.Capabilities.Enabled(CapChanges) && p.Events != nil }

func (p *Poller) eventsInterval() time.Duration {
	if p.EventsInterval > 0 {
		return p.EventsInterval
	}
	return DefaultEventsInterval
}

// PollEvents reads one events window.
func (p *Poller) PollEvents(ctx context.Context) error {
	if !p.eventsEnabled() {
		return nil
	}
	overlap, history := p.EventsOverlap, p.EventsHistory
	if overlap <= 0 {
		overlap = DefaultEventsOverlap
	}
	if history <= 0 {
		history = DefaultHistory
	}
	to := p.now()
	from := to.Add(-history)
	if !p.eventsTo.IsZero() {
		from = p.eventsTo.Add(-overlap)
	}
	marker := EventsMarker{Outcome: "complete", Window: DiscoveryWindow{From: from, To: to}}
	area := WithArea(ctx, AreaChanges)
	cursor := ""
	for page := 0; ; page++ {
		switch {
		case p.waiting():
			marker.Outcome, marker.Reason = "partial", fmt.Sprintf("page %d: not read, Datadog asked to wait", page)
			marker.StopReason = StopRateLimited
		case page >= MaxEventsPages:
			marker.Outcome, marker.Reason = "partial", fmt.Sprintf("the window needs more than %d pages", MaxEventsPages)
		}
		if marker.Outcome == "partial" {
			break
		}
		raw, err := p.Events.ListEventsPage(area, EventsQuery{
			Query: p.Changes.Query(), From: from, To: to, Cursor: cursor, Limit: DefaultEventsPageSize,
		})
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			marker.Outcome, marker.Reason = "partial", fmt.Sprintf("page %d: %v", page, err)
			if reason := p.failure(AreaChanges, err); reason == StopQuota || reason == StopRateLimited {
				marker.StopReason = reason
			}
			break
		}
		var body struct {
			Data []json.RawMessage `json:"data"`
			Meta EventsMeta        `json:"meta"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			marker.Outcome, marker.Reason = "partial", fmt.Sprintf("page %d does not parse: %v", page, err)
			break
		}
		marker.Pages++
		kept := make([]json.RawMessage, 0, len(body.Data))
		for _, item := range body.Data {
			marker.Read++
			var ev EventJSON
			if json.Unmarshal(item, &ev) == nil {
				if in, _ := p.Changes.Matches(ev); in {
					kept = append(kept, item)
					continue
				}
			}
			marker.OutOfScope++
		}
		if len(kept) > 0 {
			filtered, err := json.Marshal(struct {
				Data []json.RawMessage `json:"data"`
			}{kept})
			if err != nil {
				return err
			}
			if err := p.Push(ctx, feeder.Payload{Kind: PayloadEvents, At: p.now(), Bytes: filtered}); err != nil {
				return err
			}
		}
		if body.Meta.Partial() {
			marker.Outcome, marker.Reason = "partial", fmt.Sprintf("page %d: Datadog reported the answer incomplete", page)
			break
		}
		if cursor = body.Meta.Page.After; cursor == "" {
			break
		}
	}
	if marker.Outcome == "complete" {
		p.eventsTo = to
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
	return p.Push(ctx, feeder.Payload{Kind: PayloadEventsPoll, At: p.now(), Bytes: raw})
}
