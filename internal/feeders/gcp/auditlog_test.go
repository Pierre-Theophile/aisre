// SPDX-License-Identifier: Apache-2.0

package gcp_test

import (
	"context"
	"strings"
	"testing"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	gcpfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/gcp"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The audit stream as the general change stream (T141–T152; FR-037–FR-043).

// The taxonomy, and the one thing FR-039 is strict about: `IAM_CHANGE` and `QUOTA_CHANGE` exist, so
// falling back to "other" for them would be asserting a regression.
func TestTheAuditTaxonomyUsesAPublishedKindWhereOneFits(t *testing.T) {
	for _, tc := range []struct {
		method string
		want   graphv1.ChangeKind
	}{
		{"google.cloud.run.v2.Services.SetIamPolicy", graphv1.ChangeKind_IAM_CHANGE},
		{"google.iam.admin.v1.CreateServiceAccount", graphv1.ChangeKind_IAM_CHANGE},
		{"SetIamPolicy", graphv1.ChangeKind_IAM_CHANGE},
		{"google.api.serviceusage.v1beta1.ServiceUsage.UpdateQuotaOverride", graphv1.ChangeKind_QUOTA_CHANGE},
		{"compute.autoscalers.patch", graphv1.ChangeKind_SCALING},
		{"compute.instanceGroupManagers.resize", graphv1.ChangeKind_SCALING},
		{"serviceusage.services.enable", graphv1.ChangeKind_CONFIG_CHANGE},
		{"serviceusage.services.disable", graphv1.ChangeKind_CONFIG_CHANGE},
	} {
		got, other := gcpfeeder.ChangeKindForMethod(tc.method)
		if got != tc.want {
			t.Errorf("%s → %s, want %s", tc.method, got, tc.want)
		}
		if other != "" {
			t.Errorf("%s carries kind_other %q with a mapped kind", tc.method, other)
		}
	}
}

// And where nothing fits, GCP's own operation name is recorded and never dropped.
func TestAnUnmappableOperationKeepsGCPsOwnName(t *testing.T) {
	for _, method := range []string{
		"google.cloud.secretmanager.v1.SecretManagerService.AddSecretVersion",
		"storage.buckets.delete",
		"google.cloud.bigquery.v2.TableService.Delete",
	} {
		kind, other := gcpfeeder.ChangeKindForMethod(method)
		if kind != graphv1.ChangeKind_CHANGE_KIND_OTHER {
			t.Errorf("%s → %s, want CHANGE_KIND_OTHER", method, kind)
		}
		if other != method {
			t.Errorf("%s: kind_other = %q, want GCP's own operation name; a dropped name is a change "+
				"nobody can look up (FR-039)", method, other)
		}
	}
}

// A resource in a namespace this connector does not mint in gets NO target ref, and the change says
// so. A ref minted in another connector's namespace would be this connector naming entities it does
// not own (FR-116), and it fails testkit.
func TestAResourceInAnotherConnectorsNamespaceGetsNoTarget(t *testing.T) {
	change := gcpfeeder.AuditChange{
		Project: sqlProject, Key: "delete-1", At: sqlObserved,
		ServiceName: "storage.googleapis.com", MethodName: "storage.buckets.delete",
		ResourceName: "projects/_/buckets/nova-assets",
	}
	fact, err := gcpfeeder.AuditChangeFact(change, gcpfeeder.Actor{})
	if err != nil {
		t.Fatalf("AuditChangeFact: %v", err)
	}
	if len(fact.Targets) != 0 {
		t.Fatalf("targets = %v, want none for a bucket", fact.Targets)
	}
	props := propsMap(t, gcpfeeder.AuditChangeProps(change, gcpfeeder.Actor{}))
	if props[gcpfeeder.PropAuditTargetUnresolved] == "" {
		t.Error("the change does not state that its target is unresolved, so a reader cannot tell it " +
			"from a change about nothing (FR-036)")
	}
	if props[gcpfeeder.PropAuditResource] != change.ResourceName {
		t.Errorf("resource = %q, want the name recorded so a reader can check it in the console",
			props[gcpfeeder.PropAuditResource])
	}
	// And a resource this connector DOES own becomes a target.
	change.ResourceName = "projects/" + sqlProject + "/locations/" + sqlRegion + "/services/checkout"
	fact, err = gcpfeeder.AuditChangeFact(change, gcpfeeder.Actor{})
	if err != nil {
		t.Fatalf("AuditChangeFact: %v", err)
	}
	if len(fact.Targets) != 1 || fact.Targets[0].GetNamespace() != gcpfeeder.NSService {
		t.Errorf("targets = %v, want the Cloud Run service", fact.Targets)
	}
}

// A refused change is a different fact from one that happened, and the summary says so rather than
// leaving it to a property a reader may not open.
func TestARefusedOperationSaysSoInItsSummary(t *testing.T) {
	change := gcpfeeder.AuditChange{
		Project: sqlProject, Key: "k", At: sqlObserved,
		MethodName: "storage.buckets.delete", ResourceName: "projects/_/buckets/x", StatusCode: 7,
	}
	if summary := gcpfeeder.AuditSummary(change); !strings.Contains(summary, "refused") {
		t.Errorf("summary = %q, want it to say the change was refused", summary)
	}
	props := propsMap(t, gcpfeeder.AuditChangeProps(change, gcpfeeder.Actor{}))
	if props[gcpfeeder.PropAuditStatusCode] != "7" {
		t.Errorf("status code = %q, want it recorded", props[gcpfeeder.PropAuditStatusCode])
	}
}

// An entry with no timestamp is not a change that can be dated, and it is refused rather than dated
// at the poll: `timestamp` is the event instant and the only field used as valid time.
func TestAnEntryWithNoTimestampIsRefused(t *testing.T) {
	_, err := gcpfeeder.AuditChangeFact(gcpfeeder.AuditChange{
		Project: sqlProject, Key: "k", MethodName: "storage.buckets.delete",
	}, gcpfeeder.Actor{})
	if err == nil {
		t.Fatal("an entry with no timestamp was accepted")
	}
	// And one with no identifier at all: without either GCP key, the same entry read twice would be
	// two changes.
	_, err = gcpfeeder.AuditChangeFact(gcpfeeder.AuditChange{
		Project: sqlProject, At: sqlObserved, MethodName: "storage.buckets.delete",
	}, gcpfeeder.Actor{})
	if err == nil {
		t.Fatal("an entry with neither an operation id nor an insertId was accepted")
	}
}

// The scope is what lets a reader tell "no change happened" from "we were not looking for that kind
// of change" (FR-040). An empty scope is no restriction, which is the right default for a catch-all.
func TestTheAuditScopeDefaultsToEverythingAndNarrowsExplicitly(t *testing.T) {
	var everything gcpfeeder.AuditScope
	if !everything.InScope("storage.googleapis.com", "storage.buckets.delete") {
		t.Error("the zero scope excluded something; a default that listed services would make the " +
			"connector silently blind to every service nobody thought of")
	}
	if !strings.Contains(everything.String(), "every") {
		t.Errorf("the zero scope renders as %q; an empty list in a checkpoint note reads as "+
			"'nothing was in scope'", everything.String())
	}

	narrowed := gcpfeeder.AuditScope{Services: []string{"run.googleapis.com"}}
	if narrowed.InScope("storage.googleapis.com", "storage.buckets.delete") {
		t.Error("a narrowed scope admitted a service it does not name")
	}
	if !narrowed.InScope("run.googleapis.com", "anything") {
		t.Error("a narrowed scope excluded the service it names")
	}

	excluded := gcpfeeder.AuditScope{ExcludeMethods: []string{"storage.buckets.delete"}}
	if excluded.InScope("storage.googleapis.com", "storage.buckets.delete") {
		t.Error("an excluded method was in scope")
	}
	if !excluded.InScope("storage.googleapis.com", "storage.buckets.create") {
		t.Error("excluding one method narrowed the scope to an allowlist")
	}
}

// --- the consume-or-emit rule --------------------------------------------------------------------

// auditEntriesPage renders one general entry, in memory, with a principal — which a recording may not
// carry (FR-135) and a test may.
func auditEntriesPage(insertID, operationID, service, method, resource, at, principal string,
	delegated bool) string {
	auth := `"authenticationInfo":{"principalEmail":"` + principal + `"`
	if delegated {
		// The only documented signal that the caller is a Google platform agent (research §2).
		auth += `,"serviceAccountDelegationInfo":[{"firstPartyPrincipal":{"principalEmail":"` +
			principal + `"}}]`
	}
	auth += `}`
	operation := ""
	if operationID != "" {
		operation = `"operation":{"id":"` + operationID + `","producer":"` + service +
			`","first":true,"last":true},`
	}
	return `{"entries":[{"insertId":"` + insertID + `",
      "logName":"projects/nova-production/logs/cloudaudit.googleapis.com%2Factivity",
      "timestamp":"` + at + `","receiveTimestamp":"` + at + `",` + operation + `
      "protoPayload":{"serviceName":"` + service + `","methodName":"` + method + `",
        "resourceName":"` + resource + `",` + auth + `}}]}`
}

// T152's other half, asserted from memory because a recording may not carry the field it rests on.
//
// A CI principal deploying is a candidate cause; an autoscaler reacting four minutes after onset is a
// candidate effect. The kind is what lets feature 002's causal ordering exonerate it, and collapsing
// both into one "robot" kind removes the distinction (ADR-0005 D1).
func TestAPlatformPrincipalIsClassifiedAsAController(t *testing.T) {
	f := newFeeder(t)
	em := &recordingEmitter{}
	scaled := time.Date(2026, 9, 21, 14, 24, 0, 0, time.UTC)
	page := auditEntriesPage("scale-1", "", "compute.googleapis.com", "compute.autoscalers.patch",
		"projects/nova-production/locations/europe-west1/services/checkout",
		"2026-09-21T14:24:00Z", "service-1@gcp-sa-autoscaling.iam.gserviceaccount.com", true)

	src := &countingSource{payloads: []feeder.Payload{
		{Kind: gcpfeeder.PayloadAuditEntries, At: time.Date(2026, 9, 21, 14, 30, 0, 0, time.UTC),
			Bytes: []byte(page)},
		{Kind: gcpfeeder.PayloadPollMarker, At: time.Date(2026, 9, 21, 14, 30, 0, 0, time.UTC),
			Bytes: []byte(`{"outcome":"complete","projects":["nova-production"],"regions":["europe-west1"]}`)},
	}}
	if err := f.Run(context.Background(), src, em); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var change *graphv1.ObserveChange
	for _, ev := range em.events {
		if c := ev.GetObserveChange(); c != nil && strings.HasPrefix(c.GetRef().GetValue(), "audit/") {
			change = c
		}
	}
	if change == nil {
		t.Fatal("no general audit change was emitted")
	}
	if change.GetChange().GetKind() != graphv1.ChangeKind_SCALING {
		t.Errorf("kind = %s, want SCALING", change.GetChange().GetKind())
	}
	if got := change.GetChange().GetActorKind(); got != graphv1.ActorKind_CONTROLLER {
		t.Fatalf("actor kind = %s, want CONTROLLER from the firstPartyPrincipal delegation — the only "+
			"documented signal that the caller is a Google platform agent (research §2). Without the "+
			"kind, feature 002's causal ordering cannot exonerate an autoscaler that reacted four "+
			"minutes after onset (ADR-0005 D1)", got)
	}
	if got := change.GetValidAt().AsTime(); !got.Equal(scaled) {
		t.Errorf("valid_at = %s, want the entry's own timestamp %s", got, scaled)
	}
}

// An entry a specialised path uses does NOT also become a general change: two changes in the graph
// for one thing that happened is worse than either failure the catch-all was meant to avoid.
func TestAnEntryASpecialisedPathUsesDoesNotAlsoBecomeAGeneralChange(t *testing.T) {
	f := newFeeder(t)
	em := &recordingEmitter{}
	moved := strings.Replace(checkoutService,
		`"revision": "checkout-00041-xyz", "percent": 100`,
		`"revision": "checkout-00042-abc", "percent": 100`, 1)
	page := auditEntriesPage("shift-1", "op-77", "run.googleapis.com",
		"google.cloud.run.v2.Services.UpdateService",
		"projects/nova-production/locations/europe-west1/services/checkout",
		"2026-09-21T14:20:00Z", "deploy@nova-production.iam.gserviceaccount.com", false)

	marker := []byte(`{"outcome":"complete","projects":["nova-production"],"regions":["europe-west1"]}`)
	src := &countingSource{payloads: []feeder.Payload{
		{Kind: gcpfeeder.PayloadServices, At: time.Date(2026, 9, 21, 14, 19, 0, 0, time.UTC),
			Bytes: servicePayload(t, checkoutService)},
		{Kind: gcpfeeder.PayloadPollMarker, At: time.Date(2026, 9, 21, 14, 19, 0, 0, time.UTC), Bytes: marker},
		// The audit entry arrives a poll BEFORE the poll that notices the split, which is the
		// ordering a real poller produces — the log is queried on its own cadence.
		{Kind: gcpfeeder.PayloadAuditEntries, At: time.Date(2026, 9, 21, 14, 25, 0, 0, time.UTC),
			Bytes: []byte(page)},
		{Kind: gcpfeeder.PayloadPollMarker, At: time.Date(2026, 9, 21, 14, 25, 0, 0, time.UTC), Bytes: marker},
		{Kind: gcpfeeder.PayloadServices, At: time.Date(2026, 9, 21, 14, 31, 0, 0, time.UTC),
			Bytes: servicePayload(t, moved)},
		{Kind: gcpfeeder.PayloadPollMarker, At: time.Date(2026, 9, 21, 14, 31, 0, 0, time.UTC), Bytes: marker},
	}}
	if err := f.Run(context.Background(), src, em); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var rollouts, general int
	for _, ev := range em.events {
		c := ev.GetObserveChange()
		if c == nil {
			continue
		}
		switch {
		case strings.HasPrefix(c.GetRef().GetValue(), "rollout-traffic/"):
			rollouts++
		case strings.HasPrefix(c.GetRef().GetValue(), "audit/"):
			general++
		}
	}
	if rollouts != 1 {
		t.Errorf("traffic-shift changes = %d, want 1", rollouts)
	}
	if general != 0 {
		t.Fatalf("general audit changes = %d, want 0: the entry that dated the shift must not also "+
			"become a general change, and the poll that used it ran a cycle after the entry arrived "+
			"(auditlog.go, DefaultAuditStaleness)", general)
	}
}

// And an entry nothing uses DOES become a general change once the staleness bound has passed. That is
// the case an allowlist could not cover: an UpdateService that changed something this connector does
// not model.
func TestAnEntryNoSpecialisedPathUsesBecomesAGeneralChange(t *testing.T) {
	f := newFeeder(t)
	em := &recordingEmitter{}
	page := auditEntriesPage("shift-1", "op-77", "run.googleapis.com",
		"google.cloud.run.v2.Services.UpdateService",
		"projects/nova-production/locations/europe-west1/services/checkout",
		"2026-09-21T14:20:00Z", "deploy@nova-production.iam.gserviceaccount.com", false)

	marker := []byte(`{"outcome":"complete","projects":["nova-production"],"regions":["europe-west1"]}`)
	src := &countingSource{payloads: []feeder.Payload{
		{Kind: gcpfeeder.PayloadServices, At: time.Date(2026, 9, 21, 14, 19, 0, 0, time.UTC),
			Bytes: servicePayload(t, checkoutService)},
		{Kind: gcpfeeder.PayloadPollMarker, At: time.Date(2026, 9, 21, 14, 19, 0, 0, time.UTC), Bytes: marker},
		{Kind: gcpfeeder.PayloadAuditEntries, At: time.Date(2026, 9, 21, 14, 25, 0, 0, time.UTC),
			Bytes: []byte(page)},
		{Kind: gcpfeeder.PayloadPollMarker, At: time.Date(2026, 9, 21, 14, 25, 0, 0, time.UTC), Bytes: marker},
		// The split did not move, so nothing used the entry. Past the staleness bound it becomes a
		// general change rather than being lost.
		{Kind: gcpfeeder.PayloadServices, At: time.Date(2026, 9, 21, 15, 0, 0, 0, time.UTC),
			Bytes: servicePayload(t, checkoutService)},
		{Kind: gcpfeeder.PayloadPollMarker, At: time.Date(2026, 9, 21, 15, 0, 0, 0, time.UTC), Bytes: marker},
	}}
	if err := f.Run(context.Background(), src, em); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var general *graphv1.ObserveChange
	for _, ev := range em.events {
		if c := ev.GetObserveChange(); c != nil && strings.HasPrefix(c.GetRef().GetValue(), "audit/") {
			general = c
		}
	}
	if general == nil {
		t.Fatal("an entry nothing used was lost: the catch-all exists so that the graph's answer to " +
			"'what changed' is not 'only the things we wrote a special case for'")
	}
	want := time.Date(2026, 9, 21, 14, 20, 0, 0, time.UTC)
	if got := general.GetValidAt().AsTime(); !got.Equal(want) {
		t.Errorf("valid_at = %s, want the entry's own timestamp %s and never the poll", got, want)
	}
	if general.GetChange().GetKindOther() != "google.cloud.run.v2.Services.UpdateService" {
		t.Errorf("kind_other = %q, want GCP's own operation name", general.GetChange().GetKindOther())
	}
}

// A late entry is emitted at its own instant and the checkpoint says it arrived late. Clamping it
// forward would lie about when production changed; dropping it would make feature 002's reopening
// path unreachable for the change that most needs it.
func TestALateEntryIsEmittedAtItsOwnInstantAndStatedInTheCheckpoint(t *testing.T) {
	idx := gcpfeeder.NewScopedAuditIndex(gcpfeeder.AuditScope{}, 0)
	firstPoll := time.Date(2026, 9, 21, 15, 0, 0, 0, time.UTC)
	idx.Watermark(firstPoll)

	// An entry whose instant is well before the checkpointed extent, arriving now.
	page := auditEntriesPage("late-1", "", "storage.googleapis.com", "storage.buckets.delete",
		"projects/_/buckets/nova-assets", "2026-09-21T14:05:00Z", "someone@example.com", false)
	if _, err := idx.Ingest([]byte(page), firstPoll.Add(30*time.Minute)); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	changes := idx.Unconsumed(firstPoll.Add(30 * time.Minute))
	if len(changes) != 1 {
		t.Fatalf("unconsumed = %d, want the late entry", len(changes))
	}
	if !changes[0].Late {
		t.Fatal("an entry older than the checkpointed extent was not marked late, so a reader cannot " +
			"tell it from a timely one (§5.2)")
	}
	want := time.Date(2026, 9, 21, 14, 5, 0, 0, time.UTC)
	if !changes[0].At.Equal(want) {
		t.Errorf("At = %s, want the entry's own instant %s: a late entry is not clamped forward",
			changes[0].At, want)
	}
	props := propsMap(t, gcpfeeder.AuditChangeProps(changes[0], gcpfeeder.Actor{}))
	if props[gcpfeeder.PropAuditLate] == "" {
		t.Error("the change does not say it arrived late")
	}
}

// The lag distribution is measured, not assumed: Google publishes no ingestion-delay bound, so the
// configured reordering window has to be justifiable from data (FR-041, §5.2).
func TestTheLagDistributionIsMeasuredAndNegativeSamplesAreDropped(t *testing.T) {
	at := time.Date(2026, 9, 21, 14, 0, 0, 0, time.UTC)
	samples := gcpfeeder.AuditLagSamples([]gcpfeeder.AuditChange{
		{At: at, ReceivedAt: at.Add(20 * time.Second)},
		{At: at, ReceivedAt: at.Add(3 * time.Minute)},
		// Clocks that disagree: a fact about the clocks, not about ingestion.
		{At: at, ReceivedAt: at.Add(-time.Minute)},
		// Nothing to measure.
		{At: at},
	})
	if len(samples) != 2 {
		t.Fatalf("samples = %v, want the two measurable non-negative ones", samples)
	}
	if samples[0] != 20*time.Second || samples[1] != 3*time.Minute {
		t.Errorf("samples = %v, want 20s and 3m", samples)
	}
}

// An out-of-scope entry is not filed at all, and the scope in force reaches the checkpoint so the
// silence is explained rather than read as "nothing happened" (FR-040).
func TestAnOutOfScopeEntryIsNotFiledAndTheScopeIsStated(t *testing.T) {
	idx := gcpfeeder.NewScopedAuditIndex(gcpfeeder.AuditScope{
		Services: []string{"run.googleapis.com"},
	}, 0)
	page := auditEntriesPage("bucket-1", "", "storage.googleapis.com", "storage.buckets.delete",
		"projects/_/buckets/nova-assets", "2026-09-21T14:05:00Z", "someone@example.com", false)
	if _, err := idx.Ingest([]byte(page), time.Date(2026, 9, 21, 14, 30, 0, 0, time.UTC)); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if changes := idx.Unconsumed(time.Date(2026, 9, 21, 15, 0, 0, 0, time.UTC)); len(changes) != 0 {
		t.Fatalf("unconsumed = %+v, want none: the entry's service is out of scope", changes)
	}
	// And the checkpoint states the scope, which is the only thing that distinguishes this silence
	// from "no change happened".
	checkpoint := gcpfeeder.Checkpoint{
		From:       time.Date(2026, 9, 21, 14, 0, 0, 0, time.UTC),
		To:         time.Date(2026, 9, 21, 15, 0, 0, 0, time.UTC),
		Outcome:    gcpfeeder.PollComplete,
		Scope:      gcpfeeder.Scope{Projects: []string{sqlProject}, Regions: []string{sqlRegion}},
		AuditScope: gcpfeeder.AuditScope{Services: []string{"run.googleapis.com"}}.String(),
	}
	if !strings.Contains(checkpoint.Note(), "audit_scope=") {
		t.Errorf("the checkpoint note does not state the audit scope: %q", checkpoint.Note())
	}
	if !strings.Contains(checkpoint.Note(), "run.googleapis.com") {
		t.Errorf("the checkpoint note does not name the services in scope: %q", checkpoint.Note())
	}
}

// An entry read twice is one change: every poll re-queries a trailing overlap window, and the dedup
// is on `insertId`, GCP's own duplicate key.
func TestAGeneralEntryReadTwiceIsOneChange(t *testing.T) {
	idx := gcpfeeder.NewScopedAuditIndex(gcpfeeder.AuditScope{}, 0)
	page := auditEntriesPage("bucket-1", "", "storage.googleapis.com", "storage.buckets.delete",
		"projects/_/buckets/nova-assets", "2026-09-21T14:05:00Z", "someone@example.com", false)
	arrived := time.Date(2026, 9, 21, 14, 30, 0, 0, time.UTC)
	for i := range 2 {
		if _, err := idx.Ingest([]byte(page), arrived); err != nil {
			t.Fatalf("Ingest %d: %v", i, err)
		}
	}
	if changes := idx.Unconsumed(arrived); len(changes) != 1 {
		t.Fatalf("unconsumed = %d, want one change for one entry read twice", len(changes))
	}
	// And it is not re-emitted on the next poll.
	if changes := idx.Unconsumed(arrived.Add(time.Hour)); len(changes) != 0 {
		t.Fatalf("unconsumed again = %d, want none: an emitted change is emitted once", len(changes))
	}
}

// --- the pagination rule (T143) -------------------------------------------------------------------

// The rule T143 exists for: an empty page WITH a token is not the end of the search. Stopping there
// would under-read the window while the checkpoint claimed an extent it did not cover.
func TestAnEmptyPageWithATokenIsNotTheEndOfTheSearch(t *testing.T) {
	idx := gcpfeeder.NewScopedAuditIndex(gcpfeeder.AuditScope{}, 0)
	arrived := time.Date(2026, 9, 21, 14, 30, 0, 0, time.UTC)
	entry := auditEntriesPage("bucket-1", "", "storage.googleapis.com", "storage.buckets.delete",
		"projects/_/buckets/nova-assets", "2026-09-21T14:05:00Z", "someone@example.com", false)

	// Page 1: empty, with a token. Page 2: the entry, no token. A loop that stopped on page 1 would
	// read nothing and report the window covered.
	pages := []gcpfeeder.AuditPage{
		{Raw: []byte(`{"entries":[],"nextPageToken":"t1"}`), NextPageToken: "t1"},
		{Raw: []byte(entry)},
	}
	served := 0
	outcome, err := gcpfeeder.ReadAuditWindow(idx, arrived, gcpfeeder.AuditReadBudget{},
		func() time.Time { return arrived },
		func(string) (gcpfeeder.AuditPage, error) {
			page := pages[served]
			served++
			return page, nil
		})
	if err != nil {
		t.Fatalf("ReadAuditWindow: %v", err)
	}
	if served != 2 {
		t.Fatalf("pages served = %d, want 2: an empty page with a nextPageToken is not the end of the "+
			"search, and stopping there under-reads the window silently (contracts/gcp-feeder.md §5.1)",
			served)
	}
	if !outcome.Complete || outcome.Criterion != gcpfeeder.AuditStopFinished {
		t.Errorf("outcome = %+v, want complete because GCP said the search was over", outcome)
	}
	if outcome.Entries != 1 {
		t.Errorf("entries = %d, want the one entry on page 2", outcome.Entries)
	}
	if changes := idx.Unconsumed(arrived); len(changes) != 1 {
		t.Errorf("unconsumed = %d, want the entry from page 2", len(changes))
	}
}

// A read that stops on its own budget is PARTIAL and names the criterion. "Partial" without a
// criterion is a gap nobody can act on (FR-012).
func TestAWindowReadThatStopsOnItsBudgetNamesTheCriterion(t *testing.T) {
	arrived := time.Date(2026, 9, 21, 14, 30, 0, 0, time.UTC)
	entry := auditEntriesPage("bucket-1", "", "storage.googleapis.com", "storage.buckets.delete",
		"projects/_/buckets/nova-assets", "2026-09-21T14:05:00Z", "someone@example.com", false)

	for _, tc := range []struct {
		what   string
		budget gcpfeeder.AuditReadBudget
		clock  func(base time.Time) func() time.Time
		want   string
	}{
		{
			what:   "the page budget",
			budget: gcpfeeder.AuditReadBudget{MaxPages: 2},
			want:   gcpfeeder.AuditStopPages,
		},
		{
			what:   "the entry budget",
			budget: gcpfeeder.AuditReadBudget{MaxEntries: 1},
			want:   gcpfeeder.AuditStopEntries,
		},
		{
			what:   "the wall-clock budget",
			budget: gcpfeeder.AuditReadBudget{MaxWall: time.Second},
			clock: func(base time.Time) func() time.Time {
				calls := 0
				return func() time.Time {
					calls++
					// The first call is the start; every later one is well past the budget.
					return base.Add(time.Duration(calls) * time.Minute)
				}
			},
			want: gcpfeeder.AuditStopWall,
		},
	} {
		idx := gcpfeeder.NewScopedAuditIndex(gcpfeeder.AuditScope{}, 0)
		clock := func() time.Time { return arrived }
		if tc.clock != nil {
			clock = tc.clock(arrived)
		}
		// A reader that never runs out of pages: only the budget can stop this.
		served := 0
		outcome, err := gcpfeeder.ReadAuditWindow(idx, arrived, tc.budget, clock,
			func(string) (gcpfeeder.AuditPage, error) {
				served++
				page := auditEntriesPage("bucket-"+string(rune('a'+served)), "",
					"storage.googleapis.com", "storage.buckets.delete",
					"projects/_/buckets/nova-assets", "2026-09-21T14:05:00Z", "someone@example.com", false)
				if served == 1 {
					page = entry
				}
				return gcpfeeder.AuditPage{Raw: []byte(page), NextPageToken: "more"}, nil
			})
		if err != nil {
			t.Fatalf("%s: ReadAuditWindow: %v", tc.what, err)
		}
		if outcome.Complete {
			t.Errorf("%s: the read reported complete; it stopped on its own budget", tc.what)
		}
		if outcome.Criterion != tc.want {
			t.Errorf("%s: criterion = %q, want %q", tc.what, outcome.Criterion, tc.want)
		}
	}
}

// The data-access stream is never read, and an entry from it in a recording is refused rather than
// processed quietly (FR-042). No permission for it is requested either, which is the published
// operation list's business — and the refusal here is what stops a recording smuggling one in.
func TestAnEntryFromTheDataAccessStreamIsRefusedByTheWindowRead(t *testing.T) {
	idx := gcpfeeder.NewScopedAuditIndex(gcpfeeder.AuditScope{}, 0)
	arrived := time.Date(2026, 9, 21, 14, 30, 0, 0, time.UTC)
	page := `{"entries":[{"insertId":"d1",
      "logName":"projects/nova-production/logs/cloudaudit.googleapis.com%2Fdata_access",
      "timestamp":"2026-09-21T14:05:00Z","receiveTimestamp":"2026-09-21T14:05:10Z",
      "protoPayload":{"serviceName":"storage.googleapis.com","methodName":"storage.objects.get",
        "resourceName":"projects/_/buckets/nova-assets/objects/x"}}]}`
	_, err := gcpfeeder.ReadAuditWindow(idx, arrived, gcpfeeder.AuditReadBudget{},
		func() time.Time { return arrived },
		func(string) (gcpfeeder.AuditPage, error) {
			return gcpfeeder.AuditPage{Raw: []byte(page)}, nil
		})
	if err == nil {
		t.Fatal("an entry from the data-access stream was processed; it is not read and no permission " +
			"for it is requested (FR-042), so one in a recording is a mistake rather than something " +
			"to process quietly")
	}
	if !strings.Contains(err.Error(), "data-access") {
		t.Errorf("the refusal does not name the stream: %v", err)
	}
}
