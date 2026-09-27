// SPDX-License-Identifier: Apache-2.0

package datadog

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/datadogx"
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// `monitor_state`: a monitor's groups and the transitions Datadog states for them (contract §3.3).
//
// Datadog keeps, per group, the current status and the LAST instant it triggered, resolved and went to
// no-data — not a history. So the transitions reported are those stated instants that fall inside the
// window, and the coverage says that earlier transitions of the same kind are not visible. A state at
// the window's end is stated only when it can be derived: when no stated instant is later than the
// end, nothing has changed since, and the current status was already the status then. Otherwise it is
// left unknown rather than guessed.

// stateUnknown marks a state this answer cannot derive.
const stateUnknown = "unknown"

func (b *Backend) monitorState(ctx context.Context, term *investigationv1.MonitorStateTerm) (answer, error) {
	raw := term.GetPointer().GetAttributes()[feeder.AttrDatadogMonitorID]
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return unsupportedMonitor(fmt.Errorf("datadog: a %s pointer carries its monitor id in the %s attribute; "+
			"%q is not one", feeder.VocabDatadogMonitor, feeder.AttrDatadogMonitorID, raw)), nil
	}
	query := fmt.Sprintf("GET /api/v1/monitor/%d?group_states=all", id)
	m, resp, err := b.client.GetMonitor(ctx, id)
	if err != nil {
		a, ferr := failed(err, query)
		a.vocabulary = feeder.VocabDatadogMonitor
		return a, ferr
	}

	window := term.GetWindow()
	start, end := window.GetStart().AsTime().UTC(), window.GetEnd().AsTime().UTC()
	groups := make([]string, 0, len(m.State.Groups))
	for g := range m.State.Groups {
		groups = append(groups, g)
	}
	sort.Strings(groups)

	var transitions []*investigationv1.MonitorTransition
	perGroup := make(map[string]string, len(groups))
	allKnownAtStart, allKnownAtEnd := true, true
	for _, g := range groups {
		st := m.State.Groups[g]
		latest := time.Time{}
		for _, stated := range statedInstants(st) {
			if stated.at.After(latest) {
				latest = stated.at
			}
			if !stated.at.Before(start) && stated.at.Before(end) {
				transitions = append(transitions, &investigationv1.MonitorTransition{
					At: timestamppb.New(stated.at), ToState: stated.to, GroupKey: g,
				})
			}
		}
		if latest.After(end) {
			perGroup[g] = stateUnknown
			allKnownAtEnd = false
		} else {
			perGroup[g] = st.Status
		}
		if latest.After(start) {
			allKnownAtStart = false
		}
	}
	sort.Slice(transitions, func(i, j int) bool {
		a, b := transitions[i], transitions[j]
		if !a.GetAt().AsTime().Equal(b.GetAt().AsTime()) {
			return a.GetAt().AsTime().Before(b.GetAt().AsTime())
		}
		return a.GetGroupKey() < b.GetGroupKey()
	})

	digest := &investigationv1.MonitorStateDigest{
		Transitions: transitions, PerGroupState: perGroup,
		StateAtStart: stateUnknown, StateAtEnd: stateUnknown,
	}
	if allKnownAtEnd {
		digest.StateAtEnd = m.OverallState
	}
	if allKnownAtStart {
		digest.StateAtStart = m.OverallState
	}

	in := engine.CoverageInput{
		SearchedEntities: []string{"monitor:" + raw},
		DataSource:       "datadog_monitors:group_states",
		WindowCovered:    window,
		VolumeConsidered: int64(len(groups)),
		Sampling: "stated instants: Datadog keeps each group's last triggered, resolved and no-data instant, " +
			"not a history; an earlier transition of the same kind inside the window is not visible, and a " +
			"transition's from-state is not stated",
		IngestionLagUndetermined: true,
		ExecutedAt:               b.now().UTC(),
		QuotaUndetermined:        true,
	}
	if resp != nil && resp.HasQuota {
		in.QuotaUndetermined = false
		in.RemainingQuota = int64(resp.Reading.Remaining)
		in.QuotaWindow = resp.Reading.Period
	}
	coverage, err := in.Coverage()
	if err != nil {
		return answer{}, err
	}
	link := ""
	if b.site != "" {
		link = fmt.Sprintf("https://app.%s/monitors/%d", b.site, id)
	}
	return answer{
		outcome: engine.DigestOutcome{Body: &investigationv1.Digest{
			Body: &investigationv1.Digest_MonitorState{MonitorState: digest}, Coverage: coverage,
		}},
		query: query, vocabulary: feeder.VocabDatadogMonitor, deepLink: link,
	}, nil
}

type statedInstant struct {
	at time.Time
	to string
}

// statedInstants are the transitions a group state names, in Datadog's status words.
func statedInstants(st datadogx.GroupState) []statedInstant {
	var out []statedInstant
	for _, s := range []struct {
		ts int64
		to string
	}{{st.LastTriggeredTS, "Alert"}, {st.LastResolvedTS, "OK"}, {st.LastNoDataTS, "No Data"}} {
		if s.ts > 0 {
			out = append(out, statedInstant{at: time.Unix(s.ts, 0).UTC(), to: s.to})
		}
	}
	return out
}

func unsupportedMonitor(err error) answer {
	return answer{outcome: engine.QueryFailed{Reason: investigationv1.FailureReason_UNSUPPORTED_POINTER,
		Detail: err.Error()}, query: "monitor_state(no monitor id)", vocabulary: feeder.VocabDatadogMonitor}
}
