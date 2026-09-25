// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"sort"

	"google.golang.org/protobuf/types/known/timestamppb"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
)

// `monitor_state`, and the one risky dependency in this feature (contract §5).
//
// Cloud Monitoring *does* expose alerting incidents to a public read API: `projects.alerts.list`
// returns an `Alert` whose own documentation calls it *"a read-only resource that cannot be
// modified by the accompanied API"*, with `openTime` and `closeTime` — the transition instants
// Google reports, which are FR-046's valid time and SC-004's exactness.
//
// Two facts have to be stated together, and the mitigation is structural rather than a warning:
//
//  1. the API is **Public Preview**, under Pre-GA terms, with Google warning that *"the labels in
//     the response are subject to change while this feature is in preview"*;
//  2. it is **not in the first-party GAPIC** — the only Go binding is the
//     `google.golang.org/api/monitoring/v3` client, whose package header says it is in maintenance
//     mode.
//
// A Preview API through a maintenance-mode client is the riskiest dependency in this feature, so
// the reader sits behind a **declared capability flag** and a Transport may carry a nil one. With
// it off, this term still answers: NO_DATA with coverage naming the absent source, exactly as
// `error_spans` does — which is the contract's own answer for a missing source, not a special case
// invented for a flag. The alert **policy** half is GA and unaffected, so ALERT nodes exist either
// way.

// AlertSourceAbsent is the sentence a monitor_state answer carries when incident reads are off. It
// is a constant for the same reason TraceSourceAbsent is: a standing fact about the deployment
// should read identically on every window, so that nobody mistakes it for a finding.
const AlertSourceAbsent = "the alerting-incident source is not enabled for this backend: " +
	"projects.alerts is Public Preview and bound only in a maintenance-mode client, so incident " +
	"reads sit behind a declared capability flag and this one is off. Nothing was searched — this " +
	"is not evidence that no monitor fired"

// monitorState answers the term.
func (b *Backend) monitorState(ctx context.Context, term *investigationv1.MonitorStateTerm) (answer, error) {
	selector := term.GetPointer().GetSelector()
	facts := parseSelector(selector)
	window, narrowed := narrow(term.GetWindow(), WindowCapCheap)

	if b.transport == nil || b.transport.Alerts == nil {
		outcome, err := b.absent("cloud_monitoring_alerts:absent", AlertSourceAbsent, window)
		return answer{outcome: outcome, query: selector, vocabulary: VocabMonitoring}, err
	}

	alerts, err := b.transport.Alerts.ListAlerts(ctx, facts.scope(b.project),
		window.GetStart().AsTime().UTC(), window.GetEnd().AsTime().UTC())
	if err != nil {
		return b.queryFailed(err, selector, VocabMonitoring)
	}

	transitions, startState, endState, perGroup := transitionsOf(alerts, facts, window)

	criteria := []string(nil)
	if narrowed {
		criteria = append(criteria, criterion(CriterionWindowCap,
			"monitor_state is capped at "+WindowCapCheap.String()+" and the request was wider"))
	}
	coverage, err := b.coverage(coverageInput{
		Entities:   entitiesOf(facts),
		DataSource: "cloud_monitoring_alerts:projects/" + facts.scope(b.project),
		Window:     window,
		// The TRANSITION count, which is what FR-098 names — not the incident count, which is a
		// different number: one incident that opened and closed inside the window is two
		// transitions, and one still open is one.
		Volume:   int64(len(transitions)),
		Sampling: "projects.alerts.list;order_by=open_time desc;preview_api=true",
		Criteria: criteria,
		// An incident carries its own transition instants, so there is no ingestion pipeline
		// whose lag would qualify them. Reporting one would be reporting a figure about a
		// different surface.
		LagSource: LagUndetermined,
	})
	if err != nil {
		return answer{}, err
	}
	if len(alerts) == 0 {
		// The incident source WAS searched and held nothing, which is the one case where an
		// empty monitor state is evidence: no monitor fired over this window.
		return answer{
			outcome:    engine.NoData{Coverage: coverage},
			query:      selector,
			vocabulary: VocabMonitoring,
			deepLink:   b.deepLink(handleAlert, selector, window, facts),
		}, nil
	}
	return answer{
		outcome: engine.DigestOutcome{Body: &investigationv1.Digest{
			Body: &investigationv1.Digest_MonitorState{MonitorState: &investigationv1.MonitorStateDigest{
				Transitions:   transitions,
				StateAtStart:  startState,
				StateAtEnd:    endState,
				PerGroupState: perGroup,
			}},
			Coverage: coverage,
		}},
		query:      selector,
		vocabulary: VocabMonitoring,
		deepLink:   b.deepLink(handleAlert, selector, window, facts),
	}, nil
}

// transitionsOf turns incidents into the transitions in the window, the state at each end, and the
// per-group states (FR-098).
//
// An incident that opened before the window and is still open at its start puts the group in
// `alert` at the start — the window did not begin calm merely because the transition predates it.
// That is the whole reason `openTime` and `closeTime` are read rather than a current state: a
// state read at query time would describe the instant of the query rather than the window asked
// about.
func transitionsOf(alerts []*Alert, facts selectorFacts, window *engine.Window) (
	[]*investigationv1.MonitorTransition, string, string, map[string]string,
) {
	start := window.GetStart().AsTime().UTC()
	end := window.GetEnd().AsTime().UTC()

	transitions := make([]*investigationv1.MonitorTransition, 0, len(alerts)*2)
	perGroup := make(map[string]string, len(alerts))
	openAtStart, openAtEnd := false, false

	for _, alert := range alerts {
		group := groupKeyOf(alert, facts)
		if _, seen := perGroup[group]; !seen {
			perGroup[group] = "ok"
		}
		if !alert.OpenTime.IsZero() {
			if alert.OpenTime.Before(start) {
				openAtStart = true
			} else if !alert.OpenTime.After(end) {
				transitions = append(transitions, &investigationv1.MonitorTransition{
					At:        timestamppb.New(alert.OpenTime.UTC()),
					FromState: "ok",
					ToState:   "alert",
					GroupKey:  group,
				})
			}
		}
		closed := !alert.CloseTime.IsZero()
		if closed && !alert.CloseTime.Before(start) && !alert.CloseTime.After(end) {
			transitions = append(transitions, &investigationv1.MonitorTransition{
				At:        timestamppb.New(alert.CloseTime.UTC()),
				FromState: "alert",
				ToState:   "ok",
				GroupKey:  group,
			})
		}
		stillOpen := !closed || alert.CloseTime.After(end)
		if stillOpen {
			openAtEnd = true
			perGroup[group] = "alert"
		}
	}

	// Chronological, then by group, so two recordings of the same incidents order identically.
	sort.SliceStable(transitions, func(i, j int) bool {
		a, b := transitions[i].GetAt().AsTime(), transitions[j].GetAt().AsTime()
		if !a.Equal(b) {
			return a.Before(b)
		}
		return transitions[i].GetGroupKey() < transitions[j].GetGroupKey()
	})

	return transitions, stateName(openAtStart), stateName(openAtEnd), perGroup
}

func stateName(open bool) string {
	if open {
		return "alert"
	}
	return "ok"
}

// groupKeyOf names the group a transition belongs to: the policy where the incident reports one,
// and the resource it watches otherwise. The labels come from the Preview API, which Google warns
// may change — so a missing label produces a less specific key rather than an empty one.
func groupKeyOf(alert *Alert, facts selectorFacts) string {
	if alert.PolicyName != "" {
		return "policy:" + alert.PolicyName
	}
	if service := alert.Resource[LabelServiceName]; service != "" {
		return "service:" + service
	}
	if facts.Service != "" {
		return "service:" + facts.Service
	}
	return "alert:" + alert.Name
}
