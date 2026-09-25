// SPDX-License-Identifier: Apache-2.0

package log_test

import (
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	eventlog "github.com/Pierre-Theophile/aisre/internal/log"
)

// The published alert-intake convention (ADR-0005 D2, ADR-0003 D10, ADR-0004 D4, 002 FR-008b).

func alertTransition() *graphv1.AlertTransition {
	return &graphv1.AlertTransition{
		Monitor:      &graphv1.Ref{Namespace: "datadog.monitor", Value: "42"},
		GroupKey:     "env:prod,service:checkout",
		TransitionAt: ts("2026-09-01T14:21:00Z"),
		FromState:    eventlog.AlertStateOK,
		ToState:      eventlog.AlertStateAlert,
		Watches:      []*graphv1.Ref{{Namespace: "otel.service.name", Value: "checkout"}},
		Transport:    "poll",
		OriginRef:    "https://app.datadoghq.com/monitors/42",
		Severity:     "sev2",
		Title:        "checkout 5xx rate above 2%",
	}
}

// TestAlertTransitionKeyIsTheFourTuple: the webhook and the poll behind it are one event, and
// so are the second, third and tenth poll that still report the same transition.
func TestAlertTransitionKeyIsTheFourTuple(t *testing.T) {
	t.Parallel()

	poll := alertTransition()
	webhook := alertTransition()
	webhook.Transport = "webhook"
	webhook.OriginRef = "https://example.com/webhooks/9f2c"
	webhook.Severity = "sev1" // a transport is entitled to disagree about the trimmings

	if eventlog.AlertTransitionKey("datadog:prod", poll) != eventlog.AlertTransitionKey("datadog:prod", webhook) {
		t.Error("the webhook and the poll behind it must share one idempotency key")
	}

	// Every one of the four parts moves the key.
	for _, tc := range []struct {
		name   string
		source string
		body   func(*graphv1.AlertTransition)
	}{
		{name: "source", source: "datadog:staging"},
		{name: "monitor", body: func(b *graphv1.AlertTransition) { b.Monitor.Value = "43" }},
		{name: "group", body: func(b *graphv1.AlertTransition) { b.GroupKey = "env:prod,service:payments" }},
		{name: "instant", body: func(b *graphv1.AlertTransition) { b.TransitionAt = ts("2026-09-01T14:22:00Z") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			other := alertTransition()
			if tc.body != nil {
				tc.body(other)
			}
			source := "datadog:prod"
			if tc.source != "" {
				source = tc.source
			}
			if eventlog.AlertTransitionKey(source, other) == eventlog.AlertTransitionKey("datadog:prod", poll) {
				t.Errorf("a different %s must produce a different key", tc.name)
			}
		})
	}
}

// TestDeclarationKeyHasAnEmptyGroup pins the half of the convention the human front door uses:
// the place the incident lives on its stable identifier, the instant of declaration, and an
// empty group. Both front doors, one key shape (FR-002a, FR-008b).
func TestDeclarationKeyHasAnEmptyGroup(t *testing.T) {
	t.Parallel()

	declared := &graphv1.AlertTransition{
		Monitor:           &graphv1.Ref{Namespace: "slack.channel", Value: "C0ABCDE"},
		TransitionAt:      ts("2026-09-01T14:21:00Z"),
		ToState:           eventlog.AlertStateDeclared,
		Transport:         "human_declared",
		ActorKind:         graphv1.ActorKind_PERSON,
		DeclaringIdentity: "sre@example.com",
	}
	want := eventlog.AlertTransitionKeyParts("slack:prod", "slack.channel=C0ABCDE", "",
		"2026-09-01T14:21:00Z")
	if got := eventlog.AlertTransitionKey("slack:prod", declared); got != want {
		t.Errorf("declaration key = %q, want the 4-tuple with an empty group %q", got, want)
	}

	// Observed again — a connector re-reading the channel, a second poll — is the same event.
	again := proto.Clone(declared).(*graphv1.AlertTransition)
	again.Title = "checkout is down"
	if eventlog.AlertTransitionKey("slack:prod", again) != want {
		t.Error("a declaration observed twice must be one event")
	}
}

// TestSuppressAlertTransition is the published feeder-side filter, and above all the rule it
// does NOT have: a recovery is kept, because a recovery is evidence.
func TestSuppressAlertTransition(t *testing.T) {
	t.Parallel()

	at := func(s string) time.Time { return mustTime(s) }
	tests := []struct {
		name string
		prev *eventlog.AlertSeries
		from string
		to   string
		when string
		want string
	}{
		{
			name: "first alert of a series is emitted",
			from: eventlog.AlertStateOK, to: eventlog.AlertStateAlert,
			when: "2026-09-01T14:21:00Z",
		},
		{
			name: "a recovery is KEPT",
			prev: &eventlog.AlertSeries{State: eventlog.AlertStateAlert, PreviousState: eventlog.AlertStateWarn, At: at("2026-09-01T14:21:00Z")},
			from: eventlog.AlertStateAlert, to: eventlog.AlertStateOK,
			when: "2026-09-01T14:31:00Z",
		},
		{
			name: "still alerting is not a new fact",
			prev: &eventlog.AlertSeries{State: eventlog.AlertStateAlert, At: at("2026-09-01T14:21:00Z")},
			from: eventlog.AlertStateAlert, to: eventlog.AlertStateAlert,
			when: "2026-09-01T14:26:00Z",
			want: eventlog.SuppressNoTransition,
		},
		{
			name: "no_data is the monitor talking about itself",
			prev: &eventlog.AlertSeries{State: eventlog.AlertStateOK, At: at("2026-09-01T14:00:00Z")},
			from: eventlog.AlertStateOK, to: eventlog.AlertStateNoData,
			when: "2026-09-01T14:21:00Z",
			want: eventlog.SuppressNoData,
		},
		{
			name: "a reversal inside the dwell window is flapping",
			prev: &eventlog.AlertSeries{State: eventlog.AlertStateAlert, PreviousState: eventlog.AlertStateOK, At: at("2026-09-01T14:21:00Z")},
			from: eventlog.AlertStateAlert, to: eventlog.AlertStateOK,
			when: "2026-09-01T14:21:30Z",
			want: eventlog.SuppressFlapping,
		},
		{
			name: "the same reversal after the dwell window is a recovery",
			prev: &eventlog.AlertSeries{State: eventlog.AlertStateAlert, PreviousState: eventlog.AlertStateOK, At: at("2026-09-01T14:21:00Z")},
			from: eventlog.AlertStateAlert, to: eventlog.AlertStateOK,
			when: "2026-09-01T14:23:00Z",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body := alertTransition()
			body.FromState, body.ToState, body.TransitionAt = tc.from, tc.to, ts(tc.when)
			if got := eventlog.SuppressAlertTransition(tc.prev, body, eventlog.DefaultAlertDwell); got != tc.want {
				t.Errorf("SuppressAlertTransition = %q, want %q", got, tc.want)
			}
		})
	}
}
