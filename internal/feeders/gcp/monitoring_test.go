// SPDX-License-Identifier: Apache-2.0

package gcp_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	gcpfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/gcp"
	"github.com/Pierre-Theophile/aisre/internal/investigation/intake"
	eventlog "github.com/Pierre-Theophile/aisre/internal/log"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// Cloud Monitoring alerts on the shared intake (T113–T121, T124, T128).

const alertProject = "nova-production"

// The policy as `projects.alertPolicies.list` returns it: one threshold condition on Cloud Run
// request latency, grouped by nothing.
const latencyPolicy = `{
  "name": "projects/nova-production/alertPolicies/1122334455",
  "displayName": "checkout p99 latency above 800ms",
  "combiner": "OR",
  "enabled": true,
  "severity": "WARNING",
  "userLabels": {"team": "platform", "environment": "production"},
  "creationRecord": {"mutateTime": "2026-08-01T09:00:00Z"},
  "conditions": [{
    "name": "projects/nova-production/alertPolicies/1122334455/conditions/9988",
    "displayName": "p99 above 800ms",
    "conditionThreshold": {
      "filter": "metric.type=\"run.googleapis.com/request_latencies\" AND resource.type=\"cloud_run_revision\" AND resource.labels.project_id=\"nova-production\" AND resource.labels.location=\"europe-west1\" AND resource.labels.service_name=\"checkout\"",
      "comparison": "COMPARISON_GT",
      "thresholdValue": 800
    }
  }]
}`

// The same policy, grouped by revision: one incident per revision, which is what SC-005 is about.
const groupedPolicy = `{
  "name": "projects/nova-production/alertPolicies/5566778899",
  "displayName": "checkout errors by revision",
  "combiner": "OR",
  "enabled": true,
  "conditions": [{
    "displayName": "5xx above 1%",
    "conditionThreshold": {
      "filter": "metric.type=\"run.googleapis.com/request_count\" AND resource.type=\"cloud_run_revision\" AND resource.labels.project_id=\"nova-production\" AND resource.labels.location=\"europe-west1\" AND resource.labels.service_name=\"checkout\"",
      "aggregations": [{"alignmentPeriod": "60s", "groupByFields": ["resource.labels.revision_name"]}],
      "comparison": "COMPARISON_GT",
      "thresholdValue": 0.01
    }
  }]
}`

// A policy whose condition is PromQL: this feeder mints no pointer in that vocabulary, and the
// absence has to be stated rather than look like a policy with no conditions.
const promQLPolicy = `{
  "name": "projects/nova-production/alertPolicies/7777",
  "displayName": "checkout saturation",
  "enabled": true,
  "conditions": [{
    "displayName": "promql saturation",
    "conditionPrometheusQueryLanguage": {"query": "rate(http_requests_total[5m]) > 100"}
  }]
}`

func policyPayload(t *testing.T, policies ...string) []byte {
	t.Helper()
	raw := make([]json.RawMessage, 0, len(policies))
	for _, p := range policies {
		raw = append(raw, json.RawMessage(p))
	}
	body, err := json.Marshal(map[string]any{"alertPolicies": raw})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return body
}

// incidentJSON renders one incident as `projects.alerts.list` returns it.
func incidentJSON(id, policy, state, open, closed string, labels map[string]string) string {
	body := map[string]any{
		"name":     "projects/" + alertProject + "/alerts/" + id,
		"state":    state,
		"openTime": open,
		"policy": map[string]any{
			"name":        policy,
			"displayName": "checkout p99 latency above 800ms",
			"severity":    "WARNING",
		},
		"resource": map[string]any{"type": "cloud_run_revision", "labels": labels},
	}
	if closed != "" {
		body["closeTime"] = closed
	}
	raw, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func alertsPayloadOf(t *testing.T, incidents ...string) []byte {
	t.Helper()
	raw := make([]json.RawMessage, 0, len(incidents))
	for _, i := range incidents {
		raw = append(raw, json.RawMessage(i))
	}
	body, err := json.Marshal(map[string]any{"alerts": raw})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return body
}

func alertFeeder(t *testing.T, incidentsEnabled bool) *gcpfeeder.Feeder {
	t.Helper()
	f, err := gcpfeeder.New(gcpfeeder.Options{
		OrgSlug:             "nova",
		Scope:               scope(),
		Actors:              actorPolicy(),
		IncidentsAPIEnabled: incidentsEnabled,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return f
}

func runPayloads(t *testing.T, f *gcpfeeder.Feeder, payloads ...feeder.Payload) *recordingEmitter {
	t.Helper()
	em := &recordingEmitter{}
	if err := f.Run(context.Background(), &countingSource{payloads: payloads}, em); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return em
}

func transitionsOf(events []*graphv1.EventEnvelope) []*graphv1.AlertTransition {
	var out []*graphv1.AlertTransition
	for _, ev := range events {
		if body := ev.GetAlertTransition(); body != nil {
			out = append(out, body)
		}
	}
	return out
}

func transitionEventIDs(events []*graphv1.EventEnvelope) []string {
	var out []string
	for _, ev := range events {
		if ev.GetAlertTransition() != nil {
			out = append(out, ev.GetEventId())
		}
	}
	return out
}

// ---- T113: the policy read ---------------------------------------------------------------------

// The identity is the policy identifier GCP assigns. The display name is a claim and a property,
// never the identity — an operator renames a policy on a Tuesday, and a graph keyed on the name
// would give the renamed policy no history and let the old one stop firing silently.
func TestAnAlertPolicyIsKeyedOnItsIdentifierAndClaimsItsDisplayName(t *testing.T) {
	f := alertFeeder(t, false)
	at := time.Date(2026, 9, 21, 14, 0, 0, 0, time.UTC)
	em := runPayloads(t, f, feeder.Payload{
		Kind: gcpfeeder.PayloadAlertPolicies, At: at, Bytes: policyPayload(t, latencyPolicy),
	})

	node := firstNode(em.events, gcpfeeder.NSAlertPolicy)
	if node == nil {
		t.Fatal("no ALERT node was asserted")
	}
	if got, want := node.GetRef().GetValue(), "projects/nova-production/alertPolicies/1122334455"; got != want {
		t.Errorf("the alert is addressed as %q, want the policy resource name %q", got, want)
	}
	if node.GetType() != graphv1.NodeType_ALERT {
		t.Errorf("node type = %s, want ALERT", node.GetType())
	}
	props := node.GetProps().GetFields()
	if got := props[gcpfeeder.PropAlertPolicyDisplayName].GetStringValue(); got != "checkout p99 latency above 800ms" {
		t.Errorf("display name property = %q", got)
	}
	if got := props[gcpfeeder.PropAlertPolicySeverity].GetStringValue(); got != "WARNING" {
		t.Errorf("severity = %q, want the declared WARNING", got)
	}
	// A policy read observes no transition, so it asserts no state. A feeder that wrote one would
	// be claiming a state nobody looked at.
	for _, forbidden := range []string{"sre.alert.state", "sre.alert.transition_at"} {
		if _, present := props[forbidden]; present {
			t.Errorf("a policy read wrote %s; a policy says what it IS, a transition says what it is DOING", forbidden)
		}
	}

	// The display name is claimed, which is what lets the resolution layer merge a genuine
	// recreate-under-the-old-name with a rule and a rationale rather than by accident.
	var claimed []string
	for _, ev := range em.events {
		if claim := ev.GetIdentityClaim(); claim != nil {
			claimed = append(claimed, claim.GetClaim().GetValue())
		}
	}
	for _, want := range []string{
		"projects/nova-production/alertPolicies/1122334455",
		"1122334455",
		"checkout p99 latency above 800ms",
	} {
		if !containsString(claimed, want) {
			t.Errorf("claim %q was not emitted; the claims are %v", want, claimed)
		}
	}
}

// A rename changes the display name and nothing else, so the history survives it.
func TestARenamedPolicyKeepsItsIdentity(t *testing.T) {
	renamed := strings.Replace(latencyPolicy,
		`"displayName": "checkout p99 latency above 800ms"`,
		`"displayName": "checkout latency (renamed)"`, 1)

	f := alertFeeder(t, false)
	at := time.Date(2026, 9, 21, 14, 0, 0, 0, time.UTC)
	em := runPayloads(t, f,
		feeder.Payload{Kind: gcpfeeder.PayloadAlertPolicies, At: at, Bytes: policyPayload(t, latencyPolicy)},
		feeder.Payload{Kind: gcpfeeder.PayloadAlertPolicies, At: at.Add(time.Minute), Bytes: policyPayload(t, renamed)},
	)

	var refs []string
	for _, ev := range em.events {
		if node := ev.GetUpsertNode(); node != nil && node.GetRef().GetNamespace() == gcpfeeder.NSAlertPolicy {
			refs = append(refs, node.GetRef().GetValue())
		}
	}
	if len(refs) != 2 {
		t.Fatalf("got %d ALERT node assertions, want one per poll: %v", len(refs), refs)
	}
	if refs[0] != refs[1] {
		t.Errorf("the rename moved the identity: %q then %q", refs[0], refs[1])
	}
}

// ---- T114: watches -----------------------------------------------------------------------------

// WATCHES comes from the condition filters. A filter that pins a project, a region and a service
// names that service; one that pins only some of them names nothing, because a ref with an empty
// part resolves against something and names nothing anybody meant.
func TestWatchesAreDerivedFromTheConditionFilterAndNeverInvented(t *testing.T) {
	f := alertFeeder(t, true)
	at := time.Date(2026, 9, 21, 14, 0, 0, 0, time.UTC)
	em := runPayloads(t, f,
		feeder.Payload{Kind: gcpfeeder.PayloadAlertPolicies, At: at, Bytes: policyPayload(t, latencyPolicy)},
		feeder.Payload{Kind: gcpfeeder.PayloadAlerts, At: at.Add(time.Minute), Bytes: alertsPayloadOf(t,
			incidentJSON("inc-1", "projects/nova-production/alertPolicies/1122334455",
				"OPEN", "2026-09-21T02:07:00Z", "", map[string]string{
					"project_id":   alertProject,
					"location":     "europe-west1",
					"service_name": "checkout",
				}))},
	)

	transitions := transitionsOf(em.events)
	if len(transitions) != 1 {
		t.Fatalf("got %d transitions, want one", len(transitions))
	}
	var watched []string
	for _, ref := range transitions[0].GetWatches() {
		watched = append(watched, ref.GetNamespace()+"="+ref.GetValue())
	}
	want := "gcp.cloudrun.service=nova-production/europe-west1/checkout"
	if !containsString(watched, want) {
		t.Errorf("the transition watches %v, want %s", watched, want)
	}

	// A partial filter names nothing.
	partial := `metric.type="run.googleapis.com/request_count" AND resource.labels.project_id="nova-production" AND resource.labels.location="europe-west1"`
	if refs := gcpfeeder.WatchesFromFilterForTest(partial); len(refs) != 0 {
		t.Errorf("a filter naming no service produced %d watch(es); a ref with an empty part names nothing anybody meant", len(refs))
	}
}

// A condition whose query is PromQL yields no pointer, and the digest of that absence is a property
// rather than a silence: a policy with one threshold and one PromQL condition must not look like a
// policy with one condition.
func TestAnUnexecutableConditionIsStatedRatherThanDropped(t *testing.T) {
	f := alertFeeder(t, false)
	at := time.Date(2026, 9, 21, 14, 0, 0, 0, time.UTC)
	em := runPayloads(t, f, feeder.Payload{
		Kind: gcpfeeder.PayloadAlertPolicies, At: at, Bytes: policyPayload(t, promQLPolicy),
	})

	node := firstNode(em.events, gcpfeeder.NSAlertPolicy)
	if node == nil {
		t.Fatal("no ALERT node")
	}
	stated := node.GetProps().GetFields()[gcpfeeder.PropAlertPolicyUnexecutableConditions].GetListValue().GetValues()
	if len(stated) != 1 {
		t.Fatalf("the unexecutable condition is not stated: %v", stated)
	}
	if got := stated[0].GetStringValue(); !strings.Contains(got, "prometheus_query_language") {
		t.Errorf("the statement does not name the kind: %q", got)
	}
	// No metric pointer for it — only the console link.
	for _, p := range node.GetPointers() {
		if p.GetKind() == graphv1.PointerKind_METRIC {
			t.Errorf("a PromQL condition minted a metric pointer: %q", p.GetSelector())
		}
	}
}

// ---- T118, T120: the transition ----------------------------------------------------------------

// Valid time is the instant GOOGLE reports, never the instant the feeder learned of it, and the
// event id is the published 4-tuple.
func TestATransitionCarriesGooglesInstantAndThePublishedKey(t *testing.T) {
	f := alertFeeder(t, true)
	opened := time.Date(2026, 9, 21, 2, 7, 0, 0, time.UTC)
	polledAt := time.Date(2026, 9, 21, 2, 7, 20, 0, time.UTC)

	em := runPayloads(t, f,
		feeder.Payload{Kind: gcpfeeder.PayloadAlertPolicies, At: polledAt, Bytes: policyPayload(t, latencyPolicy)},
		feeder.Payload{Kind: gcpfeeder.PayloadAlerts, At: polledAt, Bytes: alertsPayloadOf(t,
			incidentJSON("inc-1", "projects/nova-production/alertPolicies/1122334455",
				"OPEN", "2026-09-21T02:07:00Z", "", map[string]string{
					"project_id": alertProject, "location": "europe-west1", "service_name": "checkout",
				}))},
	)

	transitions := transitionsOf(em.events)
	if len(transitions) != 1 {
		t.Fatalf("got %d transitions, want one", len(transitions))
	}
	body := transitions[0]
	if got := body.GetTransitionAt().AsTime().UTC(); !got.Equal(opened) {
		t.Errorf("transition_at = %s, want Google's openTime %s — not the poll instant %s",
			got, opened, polledAt)
	}
	if body.GetToState() != eventlog.AlertStateAlert || body.GetFromState() != eventlog.AlertStateOK {
		t.Errorf("states = %q → %q", body.GetFromState(), body.GetToState())
	}
	if body.GetTransport() != feeder.TransportPoll {
		t.Errorf("transport = %q, want poll", body.GetTransport())
	}

	// FR-051: a polled history is sampled, and says at what interval. Without the marker a gap in
	// it reads as "the alert did not fire" when it means "we were not looking".
	if !body.GetSampled() {
		t.Error("a polled transition is not marked sampled")
	}
	if got := body.GetSampledIntervalSeconds(); got != int64(gcpfeeder.DefaultAlertPollInterval/time.Second) {
		t.Errorf("sampled interval = %ds, want the default cadence", got)
	}

	// The key is derived, not invented.
	want := eventlog.AlertTransitionKey("gcp:nova", body)
	if got := transitionEventIDs(em.events)[0]; got != want {
		t.Errorf("event id = %q, want the published 4-tuple %q", got, want)
	}
}

// Two transports, one transition (FR-050, SC-004): the same incident read twice — as a poll would
// read it again while it is still open — produces one event id, so the second delivery is a
// DUPLICATE_NOOP rather than a second alert.
func TestTheSameTransitionReadTwiceIsOneEvent(t *testing.T) {
	incident := incidentJSON("inc-1", "projects/nova-production/alertPolicies/1122334455",
		"OPEN", "2026-09-21T02:07:00Z", "", map[string]string{
			"project_id": alertProject, "location": "europe-west1", "service_name": "checkout",
		})

	first := alertFeeder(t, true)
	at := time.Date(2026, 9, 21, 2, 7, 20, 0, time.UTC)
	emA := runPayloads(t, first,
		feeder.Payload{Kind: gcpfeeder.PayloadAlertPolicies, At: at, Bytes: policyPayload(t, latencyPolicy)},
		feeder.Payload{Kind: gcpfeeder.PayloadAlerts, At: at, Bytes: alertsPayloadOf(t, incident)},
	)
	// A second run — a different process, a later poll, the doorbell's poll — reading the same
	// incident.
	second := alertFeeder(t, true)
	emB := runPayloads(t, second,
		feeder.Payload{Kind: gcpfeeder.PayloadAlertPolicies, At: at.Add(time.Minute), Bytes: policyPayload(t, latencyPolicy)},
		feeder.Payload{Kind: gcpfeeder.PayloadAlerts, At: at.Add(time.Minute), Bytes: alertsPayloadOf(t, incident)},
	)

	idsA, idsB := transitionEventIDs(emA.events), transitionEventIDs(emB.events)
	if len(idsA) != 1 || len(idsB) != 1 {
		t.Fatalf("got %d and %d transitions, want one each", len(idsA), len(idsB))
	}
	if idsA[0] != idsB[0] {
		t.Errorf("two deliveries of one transition produced two keys:\n %s\n %s", idsA[0], idsB[0])
	}
}

// A closed incident emits both halves, and the recovery is not an optimisation to skip: it bounds
// the outage and is what tells an investigation whether the change it is looking at was the fix.
func TestAClosedIncidentEmitsItsRecovery(t *testing.T) {
	f := alertFeeder(t, true)
	at := time.Date(2026, 9, 21, 3, 0, 0, 0, time.UTC)
	em := runPayloads(t, f,
		feeder.Payload{Kind: gcpfeeder.PayloadAlertPolicies, At: at, Bytes: policyPayload(t, latencyPolicy)},
		feeder.Payload{Kind: gcpfeeder.PayloadAlerts, At: at, Bytes: alertsPayloadOf(t,
			incidentJSON("inc-1", "projects/nova-production/alertPolicies/1122334455",
				"CLOSED", "2026-09-21T02:07:00Z", "2026-09-21T02:51:00Z", map[string]string{
					"project_id": alertProject, "location": "europe-west1", "service_name": "checkout",
				}))},
	)

	transitions := transitionsOf(em.events)
	if len(transitions) != 2 {
		t.Fatalf("got %d transitions, want the open and the close", len(transitions))
	}
	if transitions[1].GetToState() != eventlog.AlertStateOK {
		t.Errorf("the second transition is %q → %q, want a recovery",
			transitions[1].GetFromState(), transitions[1].GetToState())
	}
	if got, want := transitions[1].GetTransitionAt().AsTime().UTC(),
		time.Date(2026, 9, 21, 2, 51, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("the recovery is at %s, want Google's closeTime %s", got, want)
	}
}

// ---- T119: per-group alerts --------------------------------------------------------------------

// N alerting groups produce exactly N alerts, and the policy-level entity is never reported as
// alerting because one group is (SC-005).
func TestAGroupedPolicyProducesOneAlertPerGroupAndNoPolicyLevelAlert(t *testing.T) {
	f := alertFeeder(t, true)
	at := time.Date(2026, 9, 21, 2, 10, 0, 0, time.UTC)
	policyName := "projects/nova-production/alertPolicies/5566778899"
	em := runPayloads(t, f,
		feeder.Payload{Kind: gcpfeeder.PayloadAlertPolicies, At: at, Bytes: policyPayload(t, groupedPolicy)},
		feeder.Payload{Kind: gcpfeeder.PayloadAlerts, At: at, Bytes: alertsPayloadOf(t,
			incidentJSON("inc-a", policyName, "OPEN", "2026-09-21T02:07:00Z", "", map[string]string{
				"project_id": alertProject, "location": "europe-west1",
				"service_name": "checkout", "revision_name": "checkout-00041-aaa",
			}),
			incidentJSON("inc-b", policyName, "OPEN", "2026-09-21T02:08:00Z", "", map[string]string{
				"project_id": alertProject, "location": "europe-west1",
				"service_name": "checkout", "revision_name": "checkout-00042-bbb",
			}))},
	)

	transitions := transitionsOf(em.events)
	if len(transitions) != 2 {
		t.Fatalf("got %d transitions, want one per alerting group", len(transitions))
	}
	seen := map[string]string{}
	for _, body := range transitions {
		monitor := body.GetMonitor().GetValue()
		if body.GetGroupKey() == "" {
			t.Errorf("a grouped policy's transition carries no group key: %s", monitor)
		}
		if monitor == policyName {
			t.Errorf("the POLICY-level entity was reported as alerting because a group is (SC-005): %s", monitor)
		}
		if !strings.HasPrefix(monitor, policyName+gcpfeeder.AlertPolicySeparator) {
			t.Errorf("monitor %q does not name its policy", monitor)
		}
		if prev, dup := seen[monitor]; dup {
			t.Errorf("two groups share the alerting entity %q (also %q); their histories would collide",
				monitor, prev)
		}
		seen[monitor] = body.GetGroupKey()
	}
	if len(seen) != 2 {
		t.Errorf("the two groups produced %d distinct alerts", len(seen))
	}

	// Each group's state history names its policy structurally — the ref is the policy's value
	// with the group appended — and the group key travels on the transition, so both are readable
	// off the alert itself.
	//
	// There is deliberately NO separate node assertion for the group. An earlier version emitted
	// one carrying the policy as a property, and the grouped fixture's shuffle step caught it: two
	// assertions of one node over one valid interval do not commute, so which property set the
	// version carried depended on the order two payloads of a single poll arrived in.
	for _, ev := range em.events {
		node := ev.GetUpsertNode()
		if node == nil || node.GetRef().GetNamespace() != gcpfeeder.NSAlertPolicy {
			continue
		}
		if strings.Contains(node.GetRef().GetValue(), gcpfeeder.AlertPolicySeparator) {
			t.Errorf("a separate node assertion for group %q; it does not commute with the "+
				"transition's own assertion of the same node", node.GetRef().GetValue())
		}
	}
	for _, body := range transitions {
		policy, group, found := strings.Cut(body.GetMonitor().GetValue(), gcpfeeder.AlertPolicySeparator)
		if !found {
			t.Errorf("monitor %q does not name a group", body.GetMonitor().GetValue())
			continue
		}
		if policy != policyName {
			t.Errorf("monitor %q does not name its policy", body.GetMonitor().GetValue())
		}
		if group != body.GetGroupKey() {
			t.Errorf("the ref's group %q and the transition's group key %q disagree", group, body.GetGroupKey())
		}
	}
}

// ---- T121: suppression -------------------------------------------------------------------------

// Flapping is suppressed, still recorded, and the suppression is STATED rather than applied
// silently: an operator asking "the monitor flapped six times, where are they" needs an answer.
func TestFlappingIsSuppressedAndTheSuppressionIsStated(t *testing.T) {
	f := alertFeeder(t, true)
	at := time.Date(2026, 9, 21, 2, 10, 0, 0, time.UTC)
	policyName := "projects/nova-production/alertPolicies/1122334455"
	labels := map[string]string{
		"project_id": alertProject, "location": "europe-west1", "service_name": "checkout",
	}
	em := runPayloads(t, f,
		feeder.Payload{Kind: gcpfeeder.PayloadAlertPolicies, At: at, Bytes: policyPayload(t, latencyPolicy)},
		// One incident that opened and closed within the dwell window, then re-opened: the
		// reversal inside the window is the flap.
		feeder.Payload{Kind: gcpfeeder.PayloadAlerts, At: at, Bytes: alertsPayloadOf(t,
			incidentJSON("inc-1", policyName, "CLOSED",
				"2026-09-21T02:07:00Z", "2026-09-21T02:07:30Z", labels))},
		feeder.Payload{Kind: gcpfeeder.PayloadPollMarker, At: at.Add(time.Minute), Bytes: []byte(`{"outcome":"complete"}`)},
	)

	transitions := transitionsOf(em.events)
	if len(transitions) != 1 {
		t.Fatalf("got %d transitions, want the open only — the close reverses inside the dwell window: %v",
			len(transitions), transitions)
	}
	suppressed := f.Suppressed()
	if len(suppressed) != 1 {
		t.Fatalf("got %d suppressions, want the flap: %v", len(suppressed), suppressed)
	}
	if suppressed[0].Reason != eventlog.SuppressFlapping {
		t.Errorf("suppression reason = %q, want %q", suppressed[0].Reason, eventlog.SuppressFlapping)
	}
	if !strings.Contains(suppressed[0].String(), eventlog.SuppressFlapping) {
		t.Errorf("the suppression does not render its reason: %q", suppressed[0].String())
	}
}

// ---- T115, T117: the capability flag -----------------------------------------------------------

// With the capability off, a policy read still produces ALERT nodes and an incidents payload is
// refused by name rather than silently dropped: a flag that could be bypassed by a payload is a
// flag that is off in the contract and on in production.
func TestWithTheIncidentCapabilityOffPoliciesLandAndIncidentsAreRefused(t *testing.T) {
	f := alertFeeder(t, false)
	at := time.Date(2026, 9, 21, 2, 10, 0, 0, time.UTC)
	em := runPayloads(t, f, feeder.Payload{
		Kind: gcpfeeder.PayloadAlertPolicies, At: at, Bytes: policyPayload(t, latencyPolicy),
	})
	if firstNode(em.events, gcpfeeder.NSAlertPolicy) == nil {
		t.Fatal("no ALERT node with the capability off; the policy read is GA and unaffected")
	}
	if got := len(transitionsOf(em.events)); got != 0 {
		t.Errorf("got %d transitions with the capability off", got)
	}

	// And the payload that could only exist with the capability on is refused.
	f2 := alertFeeder(t, false)
	err := f2.Run(context.Background(), &countingSource{payloads: []feeder.Payload{
		{Kind: gcpfeeder.PayloadAlerts, At: at, Bytes: alertsPayloadOf(t,
			incidentJSON("inc-1", "projects/nova-production/alertPolicies/1122334455",
				"OPEN", "2026-09-21T02:07:00Z", "", map[string]string{"project_id": alertProject}))},
	}}, &recordingEmitter{})
	if err == nil {
		t.Fatal("an incidents payload was accepted with the capability disabled")
	}
	if !strings.Contains(err.Error(), "Public Preview") {
		t.Errorf("the refusal does not say why the capability exists: %v", err)
	}
}

// A state this reader cannot map is refused rather than guessed at, which is what keeps a Preview
// API's label change from becoming a wrong alert history.
func TestAnUnmappableIncidentStateIsRefused(t *testing.T) {
	f := alertFeeder(t, true)
	at := time.Date(2026, 9, 21, 2, 10, 0, 0, time.UTC)
	err := f.Run(context.Background(), &countingSource{payloads: []feeder.Payload{
		{Kind: gcpfeeder.PayloadAlerts, At: at, Bytes: alertsPayloadOf(t,
			incidentJSON("inc-1", "projects/nova-production/alertPolicies/1122334455",
				"ACKNOWLEDGED", "2026-09-21T02:07:00Z", "", map[string]string{"project_id": alertProject}))},
	}}, &recordingEmitter{})
	if err == nil {
		t.Fatal("an unmappable state was accepted")
	}
	if !strings.Contains(err.Error(), "ACKNOWLEDGED") {
		t.Errorf("the refusal does not name the state it could not map: %v", err)
	}
}

// ---- T124: the handoff -------------------------------------------------------------------------

// The handoff carries the alert, what it watches, the reference instant and the pointers — and no
// telemetry. There is no field it could live in, which is the point: values that made an alert fire
// belong behind the algebra, bounded and sanitised, not in the intake.
func TestTheHandoffCarriesPointersAndNoTelemetry(t *testing.T) {
	f := alertFeeder(t, true)
	at := time.Date(2026, 9, 21, 2, 10, 0, 0, time.UTC)
	runPayloads(t, f, feeder.Payload{
		Kind: gcpfeeder.PayloadAlertPolicies, At: at, Bytes: policyPayload(t, latencyPolicy),
	})

	incident := gcpfeeder.AlertIncident{
		Policy:            gcpfeeder.AlertPolicy{Project: alertProject, ID: "1122334455"},
		PolicyDisplayName: "checkout p99 latency above 800ms",
		Severity:          "WARNING",
		State:             gcpfeeder.IncidentOpen,
		OpenTime:          time.Date(2026, 9, 21, 2, 7, 0, 0, time.UTC),
		Resource: map[string]string{
			"project_id": alertProject, "location": "europe-west1", "service_name": "checkout",
		},
	}
	handoff := f.Handoff(incident)
	if got, want := handoff.ReferenceAt, incident.OpenTime; !got.Equal(want) {
		t.Errorf("reference instant = %s, want the opening %s", got, want)
	}
	if len(handoff.Watches) == 0 {
		t.Error("the handoff names no entity to start from")
	}
	if len(handoff.Pointers) == 0 {
		t.Error("the handoff carries no pointers, so there is nothing to execute")
	}
	for _, p := range handoff.Pointers {
		if p.GetSelector() == "" {
			t.Error("a pointer with no selector")
		}
	}
	if !handoff.Sampled {
		t.Error("the handoff does not say the history around the instant is sampled")
	}
	// A handoff of a closed incident still references the OPENING: an investigation asks what
	// changed before this started, and dating it at the recovery would put the fix in the window
	// and the cause outside it.
	incident.State = gcpfeeder.IncidentClosed
	incident.CloseTime = incident.OpenTime.Add(44 * time.Minute)
	if got := f.Handoff(incident).ReferenceAt; !got.Equal(incident.OpenTime) {
		t.Errorf("a closed incident's reference instant = %s, want the opening", got)
	}
}

// ---- T128: convergence with the human-declared intake -----------------------------------------

// A GCP transition and a human declaration converge on one event shape and one convention, so the
// engine sees one kind of trigger rather than two shapes to reconcile (FR-047).
func TestAGCPTransitionAndAHumanDeclarationShareTheConvention(t *testing.T) {
	f := alertFeeder(t, true)
	at := time.Date(2026, 9, 21, 2, 10, 0, 0, time.UTC)
	em := runPayloads(t, f,
		feeder.Payload{Kind: gcpfeeder.PayloadAlertPolicies, At: at, Bytes: policyPayload(t, latencyPolicy)},
		feeder.Payload{Kind: gcpfeeder.PayloadAlerts, At: at, Bytes: alertsPayloadOf(t,
			incidentJSON("inc-1", "projects/nova-production/alertPolicies/1122334455",
				"OPEN", "2026-09-21T02:07:00Z", "", map[string]string{
					"project_id": alertProject, "location": "europe-west1", "service_name": "checkout",
				}))},
	)
	fromGCP := transitionsOf(em.events)
	if len(fromGCP) != 1 {
		t.Fatalf("got %d transitions", len(fromGCP))
	}

	declared := intake.DeclarationBody(intake.Declaration{
		PlaceID:           "C0123/1695000000.1",
		DeclaredAt:        time.Date(2026, 9, 21, 2, 9, 0, 0, time.UTC),
		Severity:          "sev2",
		Title:             "checkout is slow",
		DeclaringIdentity: "operator@example.com",
		Targets: []intake.DeclaredTarget{{
			Ref: &graphv1.Ref{Namespace: "gcp.cloudrun.service", Value: "nova-production/europe-west1/checkout"},
		}},
	})

	// Same body type, same key function, same published state vocabulary. The differences are the
	// ones the schema says are the differences: the transport, the actor kind, and an empty group.
	for name, body := range map[string]*graphv1.AlertTransition{"gcp": fromGCP[0], "declaration": declared} {
		if body.GetMonitor().GetValue() == "" {
			t.Errorf("%s: no monitor ref", name)
		}
		if body.GetTransitionAt() == nil {
			t.Errorf("%s: no transition instant", name)
		}
		if key := eventlog.AlertTransitionKey("gcp:nova", body); !strings.HasPrefix(key, "alert:") {
			t.Errorf("%s: the key is not the published shape: %q", name, key)
		}
	}
	if declared.GetGroupKey() != "" {
		t.Errorf("a declaration carries a group key %q; a declaration has no groups, and pinning "+
			"that is what makes the two doors one key", declared.GetGroupKey())
	}
	if declared.GetActorKind() != graphv1.ActorKind_PERSON {
		t.Errorf("a declaration's actor kind = %s, want PERSON", declared.GetActorKind())
	}
	if fromGCP[0].GetActorKind() == graphv1.ActorKind_PERSON {
		t.Error("a monitor transition claims a PERSON acted")
	}
	if fromGCP[0].GetTransport() == declared.GetTransport() {
		t.Errorf("both carry transport %q; the transport is what says whether the history around "+
			"them is complete", fromGCP[0].GetTransport())
	}
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// A policy that has never fired still says what it watches. Without it, a quiet policy and one
// watching nothing are the same node — and the WATCHES edges only exist once a transition has gone
// through the unattached convention, which a policy that never fired has not.
func TestAQuietPolicyStillSaysWhatItWatches(t *testing.T) {
	f := alertFeeder(t, false)
	at := time.Date(2026, 9, 21, 14, 0, 0, 0, time.UTC)
	em := runPayloads(t, f, feeder.Payload{
		Kind: gcpfeeder.PayloadAlertPolicies, At: at, Bytes: policyPayload(t, latencyPolicy),
	})

	node := firstNode(em.events, gcpfeeder.NSAlertPolicy)
	if node == nil {
		t.Fatal("no ALERT node")
	}
	watches := node.GetProps().GetFields()[gcpfeeder.PropAlertWatches].GetListValue().GetValues()
	if len(watches) == 0 {
		t.Fatal("the policy states nothing about what it watches; a quiet policy would be " +
			"indistinguishable from one watching nothing")
	}
	want := "gcp.cloudrun.service=nova-production/europe-west1/checkout"
	var got []string
	for _, v := range watches {
		got = append(got, v.GetStringValue())
	}
	if !containsString(got, want) {
		t.Errorf("the policy watches %v, want %s", got, want)
	}
	// And no WATCHES edge, because the feeder cannot tell whether the target exists and a
	// placeholder endpoint is indistinguishable from an alert watching something real.
	for _, ev := range em.events {
		if edge := ev.GetUpsertEdge(); edge != nil && edge.GetType() == graphv1.EdgeType_WATCHES {
			t.Errorf("a policy read asserted a WATCHES edge to %s=%s; the edges are the "+
				"transitions', where the unattached convention applies",
				edge.GetDst().GetNamespace(), edge.GetDst().GetValue())
		}
	}
}
