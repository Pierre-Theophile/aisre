// SPDX-License-Identifier: Apache-2.0

package k8s_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	ddfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/datadog"
	k8sfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/k8s"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/record"
)

// datadog-events-merge-01: one rollout, whatever saw it (005 T092, SC-014).
//
// `payments` runs in a Kubernetes cluster, and the organisation's pipeline also posts a deployment event
// to Datadog. Both observe the same rollout of commit NEW to `prod`:
//
//  1. 14:00: the cluster lists `payments` at revision 7, and the Datadog connector's discovery names
//     `prod/payments` as a watched log source. C9 makes the log source and the Kubernetes workload one
//     entity, on the service name the workload declares and the environment both state.
//  2. 14:10: the cluster rolls `payments` out to revision 8, its pod template stating commit NEW under
//     the operator-trusted `commit-sha` key, so the rollout carries `deploy.commit_sha=NEW`.
//  3. 14:20: the events read returns the pipeline's deployment event, posted at 14:09:30, tagged with the
//     service, the environment and the commit; the connector emits one CHANGE and the same
//     `deploy.commit_sha` correlation key. C8 makes the two one change. The events read at 14:25 reaches
//     back over the same window (the overlap) and returns the event again, which re-sends ids already
//     sent and adds nothing.
//  4. An event of another kind on `billing`, a service nothing watches, is kept: "other", the vendor's
//     kind recorded, and unattached, attaching if `billing` ever appears.
//
// The connector merges nothing itself (FR-032): the join is C9 and then C8, on the published keys.

const (
	ddEventsFixtureDir = "fixtures/datadog-events-merge-01"
	ddEventDeploy      = "evt-4a7c19e0"
	ddEventFailover    = "evt-4a7c19e1"
	ddEventsOrg        = "twin"
)

func ddEventsPage(deployAt time.Time) []byte {
	data := []map[string]any{
		{
			"id": ddEventDeploy, "type": "event",
			"attributes": map[string]any{
				"timestamp": deployAt.UTC().Format(time.RFC3339Nano),
				"tags": []string{"env:prod", "service:payments", "git.commit.sha:" + k8sCommitNew, "version:v2.4.0",
					"user:release-runner", "triggered_by:ci"},
				"attributes": map[string]any{
					"title": "Deployed payments v2.4.0 to prod", "source_type_name": "jenkins",
					"evt": map[string]any{"type": "deployment"},
				},
			},
		},
		{
			"id": ddEventFailover, "type": "event",
			"attributes": map[string]any{
				"timestamp": k8sCommitStart.Add(12 * time.Minute).UTC().Format(time.RFC3339Nano),
				"tags":      []string{"env:prod", "service:billing"},
				"attributes": map[string]any{
					"title": "Failed over the billing database", "source_type_name": "custom_pipeline",
					"evt": map[string]any{"type": "db_failover"},
				},
			},
		},
	}
	raw, err := json.Marshal(map[string]any{"data": data})
	if err != nil {
		panic(err)
	}
	return raw
}

func ddEventsMarker(at time.Time, window time.Duration) []byte {
	raw, err := json.Marshal(ddfeeder.EventsMarker{Outcome: "complete", Pages: 1, Read: 9, OutOfScope: 7,
		Window: ddfeeder.DiscoveryWindow{From: at.Add(-window), To: at}})
	if err != nil {
		panic(err)
	}
	return raw
}

func TestGenerateDatadogEventsMergeFixture(t *testing.T) {
	if os.Getenv("SRE_AGENT_GEN_FIXTURES") == "" {
		t.Skip("set SRE_AGENT_GEN_FIXTURES=1 to regenerate the Datadog events merge fixture")
	}
	dir := filepath.Join(k8sRepoRoot(t), ddEventsFixtureDir)
	for _, generated := range []string{"payloads", "events.jsonl", "rejected.jsonl", "manifest.yaml", "golden"} {
		if err := os.RemoveAll(filepath.Join(dir, generated)); err != nil {
			t.Fatalf("clear %s: %v", generated, err)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create %s: %v", dir, err)
	}
	clock := &k8sArrivalClock{}

	dd, err := ddfeeder.New(ddfeeder.Options{
		OrgSlug: ddEventsOrg,
		Capabilities: ddfeeder.Capabilities{ddfeeder.CapLogs: true, ddfeeder.CapMonitors: true, ddfeeder.CapTags: true,
			ddfeeder.CapChanges: true},
		Changes: ddfeeder.ChangeScope{Sources: []string{"jenkins", "custom_pipeline"}},
	})
	if err != nil {
		t.Fatalf("new datadog feeder: %v", err)
	}
	ddDesc := dd.Describe()

	// 14:00: the watched log source.
	tick, err := json.Marshal(ddfeeder.DiscoveryTick{LogSources: []string{"prod/payments"}})
	if err != nil {
		t.Fatal(err)
	}
	recordHalf(t, "datadog 14:00", dir, clock, ddDesc, []feeder.Payload{
		{Kind: ddfeeder.PayloadDiscovery, At: k8sCommitStart, Bytes: tick},
	}, dd.Run)

	// 14:00–14:10: the cluster lists payments at revision 7 and rolls it out to revision 8.
	listed := commitDeployment("1003", "7", k8sCommitOld, "ghcr.io/twin/payments:v7", time.Time{}, "")
	rolled := commitDeployment("1050", "8", k8sCommitNew, "ghcr.io/twin/payments:v8", k8sCommitRolloutAt, "")
	ns := shopNamespace()
	opts := fixtureOptions()
	opts.CommitLabels = []string{k8sCommitTrustedKey}
	kf, err := k8sfeeder.New(opts, nil)
	if err != nil {
		t.Fatalf("new k8s feeder: %v", err)
	}
	k8sDesc := recordHalf(t, "k8s half", dir, clock, kf.Describe(), []feeder.Payload{
		payloadOf(t, k8sfeeder.KindNamespaces, k8sfeeder.EventAdded, ns, ns.ResourceVersion, k8sCommitStart),
		payloadOf(t, k8sfeeder.KindDeployments, k8sfeeder.EventAdded, listed, listed.ResourceVersion, k8sCommitStart),
		{Kind: k8sfeeder.KindSync, At: k8sCommitStart.Add(time.Minute), Seq: 1100},
		payloadOf(t, k8sfeeder.KindDeployments, k8sfeeder.EventModified, rolled, rolled.ResourceVersion, k8sCommitRolloutAt),
	}, kf.Run)

	// 14:20 and 14:25: the events reads, the second reaching back over the first.
	deployAt := k8sCommitStart.Add(9*time.Minute + 30*time.Second)
	page := ddEventsPage(deployAt)
	first, second := k8sCommitStart.Add(20*time.Minute), k8sCommitStart.Add(25*time.Minute)
	recordHalf(t, "datadog 14:20", dir, clock, ddDesc, []feeder.Payload{
		{Kind: ddfeeder.PayloadEvents, At: first, Bytes: page},
		{Kind: ddfeeder.PayloadEventsPoll, At: first, Bytes: ddEventsMarker(first, 24*time.Hour)},
		{Kind: ddfeeder.PayloadEvents, At: second, Bytes: page},
		{Kind: ddfeeder.PayloadEventsPoll, At: second, Bytes: ddEventsMarker(second, 10*time.Minute)},
	}, dd.Run)

	if err := record.WriteManifest(dir, record.Manifest{
		Family: "datadog-cross-source",
		Description: "One rollout, whatever saw it. The cluster rolls payments out to revision 8 at 14:10, its " +
			"pod template stating commit NEW under the trusted commit-sha key; the pipeline posted a " +
			"deployment event to Datadog at 14:09:30 stating the same commit. C9 makes the watched log " +
			"source and the workload one entity and C8 makes the two rollouts one change; the events read " +
			"repeated over its overlap adds nothing. A failover event on an unwatched service is kept as " +
			"another kind, unattached. Built from the published Events API shape, not yet verified against " +
			"a live organisation. Synthetic structural twin: no identifier is derived from any organisation " +
			"(contracts/sanitisation.md §7).",
		Sources: []record.ManifestSource{record.SourceOf(k8sDesc), record.SourceOf(ddDesc)},
		Start:   k8sCommitStart,
		End:     k8sCommitEnd,
	}); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	appendDatadogEventsQueries(t, dir)
}

func appendDatadogEventsQueries(t *testing.T, dir string) {
	t.Helper()
	pinned := k8sCommitPinnedAt.Format(time.RFC3339)
	queries := fmt.Sprintf(`queries:
  # SC-014: what changed on payments in the hour? ONE rollout, carrying the cluster's instant and revision
  # and Datadog's actor kind, because C9 made the log source and the workload one entity and C8 made the
  # two observations one change. Not two.
  - name: what-changed-on-payments
    kind: diff
    focus: k8s.deployment=shop/payments
    t1: %[1]s
    t2: %[2]s
    reference_at: %[2]s
    observed_at: %[3]s
    hops: 1
    direction: both
  # Why: the rule, and the commit both stood on.
  - name: why-the-datadog-event-is-the-cluster-rollout
    kind: audit
    ref_a: datadog.change=%[4]s
    ref_b: k8s.change=%[5]s
    observed_at: %[3]s

ground_truth:
  cross_source_pairs:
    - pair:
        - datadog.change=%[4]s
        - k8s.change=%[5]s
      same: true
      rule: C8
`, k8sCommitStart.Format(time.RFC3339), k8sCommitEnd.Format(time.RFC3339), pinned, ddEventDeploy, k8sCommitRolloutChange)
	path := filepath.Join(dir, "manifest.yaml")
	existing, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := os.WriteFile(path, append(existing, []byte("\n"+queries)...), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
