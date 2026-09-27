// SPDX-License-Identifier: Apache-2.0

package datadog

import (
	"context"
	"fmt"
	"sort"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	eventlog "github.com/Pierre-Theophile/aisre/internal/log"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// Transitions from group states (T053, T054; contract §3).
//
// Datadog keeps, per group, its current status and the LAST instant it triggered, resolved and went to
// no data. A poll therefore sees a state and three stated instants, and a transition is dated from the
// instant that corresponds to it — `last_triggered_ts` into alert or warn, `last_resolved_ts` into ok,
// `last_nodata_ts` into no data — never from the poll (FR-020).
//
// The rule applied at each poll: every stated instant newer than the newest one already emitted for
// the group is a transition, in time order, each from the state the previous one entered. That one
// rule covers the ordinary case (a status change since the last poll), the case polling alone would
// miss (a group that alerted and resolved between two polls still moves both instants), and a first
// sight (every stated instant within the history horizon). When the status changed and no newer
// instant corresponds, the transition cannot be dated: it is stated in the checkpoint and not emitted,
// because a transition is keyed on its instant and the schema refuses one without it.
//
// Because each id is the published 4-tuple (source, entity, group, instant), a restarted feeder that
// re-derives the same instants re-sends the same ids, and a poll triggered by the doorbell and the
// scheduled poll after it deliver one event (FR-025a).

// groupState is what the feeder remembers of one alerting group between polls.
type groupState struct {
	status string    // the status the last poll read, in the published vocabulary
	from   string    // the state the next transition is from: the last one not into no data
	newest time.Time // the newest stated instant already emitted or skipped
}

// stateOf maps a Datadog group status onto the published vocabulary. A status with no mapping —
// Ignored, Skipped, Unknown — is `no_data`, which the published filter drops (internal/log/alert.go).
func stateOf(status string) string {
	switch status {
	case "OK":
		return eventlog.AlertStateOK
	case "Warn":
		return eventlog.AlertStateWarn
	case "Alert":
		return eventlog.AlertStateAlert
	default:
		return eventlog.AlertStateNoData
	}
}

// stated is one instant a group state names, with the state it entered then.
type stated struct {
	at time.Time
	to string
}

// statedInstants are a group's stated instants, oldest first. A trigger enters the current status when
// that is alert or warn — Datadog's `last_triggered_ts` covers both — and alert otherwise.
func statedInstants(g groupJSON) []stated {
	triggered := stateOf(g.Status)
	if triggered != eventlog.AlertStateWarn {
		triggered = eventlog.AlertStateAlert
	}
	var out []stated
	for _, s := range []struct {
		ts int64
		to string
	}{
		{g.LastTriggeredTS, triggered},
		{g.LastResolvedTS, eventlog.AlertStateOK},
		{g.LastNoDataTS, eventlog.AlertStateNoData},
	} {
		if s.ts > 0 {
			out = append(out, stated{at: time.Unix(s.ts, 0).UTC(), to: s.to})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].at.Before(out[j].at) })
	return out
}

// emitTransitions derives and emits the transitions of every group of one monitor.
func (f *Feeder) emitTransitions(ctx context.Context, em feeder.Emitter, m monitorObservation, at time.Time) error {
	names := make([]string, 0, len(m.Groups))
	for name := range m.Groups {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := f.emitGroup(ctx, em, m, name, m.Groups[name], at); err != nil {
			return err
		}
	}
	return nil
}

func (f *Feeder) emitGroup(ctx context.Context, em feeder.Emitter, m monitorObservation, name string, g groupJSON, at time.Time) error {
	entity, groupKey := m.entity(name)
	current := stateOf(g.Status)

	f.mu.Lock()
	prev, seen := f.groups[entity]
	f.mu.Unlock()

	floor := at.Add(-f.opts.History)
	from := eventlog.AlertStateOK
	if seen {
		floor = prev.newest
		from = prev.from
	}
	var fresh []stated
	for _, s := range statedInstants(g) {
		if s.at.After(floor) && !s.at.After(at) {
			fresh = append(fresh, s)
		}
	}
	// A stated instant later than the poll that read it is Datadog's clock ahead of ours; it is kept
	// for the next poll rather than emitted from the future.

	newest := floor
	if seen && current != prev.status && (len(fresh) == 0 || fresh[len(fresh)-1].to != current) {
		f.mu.Lock()
		f.undated = append(f.undated, fmt.Sprintf("monitor %s group %q: %s → %s, observed by the poll at %s",
			m.ID, orUnset(groupKey), prev.status, current, at.Format(time.RFC3339)))
		f.mu.Unlock()
	}
	for _, s := range fresh {
		if s.to == from {
			// Not a transition: a first sight of a group whose oldest stated instant is a resolution,
			// or a no-data spell's end back into the state it interrupted.
			newest = s.at
			continue
		}
		reason, err := f.emitTransition(ctx, em, m, name, entity, groupKey, from, s, at)
		if err != nil {
			return err
		}
		// A spell of no data is a fact about the monitor, not the system: the next transition is from
		// the state the group was in before it, so `alert → no data → ok` still delivers the recovery.
		if reason != eventlog.SuppressNoData {
			from = s.to
		}
		newest = s.at
	}

	f.mu.Lock()
	f.groups[entity] = groupState{status: current, from: from, newest: newest}
	f.mu.Unlock()
	return nil
}

// emitTransition emits one transition, or states why the published filter dropped it.
func (f *Feeder) emitTransition(ctx context.Context, em feeder.Emitter, m monitorObservation, group, entity, groupKey, from string, s stated, at time.Time) (string, error) {
	ref := feeder.Ref(NSMonitor, entity)
	fact := feeder.AlertFact{
		Meta:    feeder.Meta{SourceObservedAt: at},
		Monitor: ref, GroupKey: groupKey, TransitionAt: s.at, FromState: from, ToState: s.to,
		Transport: feeder.TransportPoll, Title: m.Name,
		// Every polled history is sampled (ADR-0009 item 3): a transition that opened and closed
		// between two polls is visible only through the instants Datadog states, and only the last of
		// each kind.
		Sampled: true, SampledInterval: f.opts.PollInterval,
	}
	if m.Priority > 0 {
		fact.Severity = fmt.Sprintf("P%d", m.Priority)
	}
	if f.opts.Site != "" {
		fact.OriginRef = "https://app." + f.opts.Site + "/monitors/" + m.ID
	}
	body := &graphv1.AlertTransition{
		Monitor: ref, GroupKey: groupKey, TransitionAt: timestamppb.New(s.at), FromState: from, ToState: s.to,
	}
	if reason := f.suppress(entity, body); reason != "" {
		f.mu.Lock()
		f.suppressed = append(f.suppressed, fmt.Sprintf("monitor %s group %q: %s → %s at %s (%s)",
			m.ID, orUnset(groupKey), from, s.to, s.at.Format(time.RFC3339), reason))
		f.mu.Unlock()
		return reason, nil
	}
	fact.Watches = f.watches(m, group)
	if err := emit(ctx, em, feeder.ObserveAlertTransition(f.desc, eventlog.AlertTransitionKey(f.desc.SourceID, body), fact)); err != nil {
		return "", err
	}
	f.mu.Lock()
	if f.emittedGroups[m.ID] == nil {
		f.emittedGroups[m.ID] = map[string]bool{}
	}
	if groupKey != "" {
		f.emittedGroups[m.ID][entity] = true
	}
	f.mu.Unlock()
	return "", nil
}

// suppress applies the published filter (flapping, no data) and advances the series. Recoveries are
// kept: `alert → ok` bounds the outage (internal/log/alert.go).
func (f *Feeder) suppress(entity string, body *graphv1.AlertTransition) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	prev := f.series[entity]
	if reason := eventlog.SuppressAlertTransition(prev, body, 0); reason != "" {
		return reason
	}
	next := &eventlog.AlertSeries{State: body.GetToState(), At: body.GetTransitionAt().AsTime(), PreviousState: body.GetFromState()}
	if prev != nil {
		next.PreviousState = prev.State
	}
	f.series[entity] = next
	return ""
}
