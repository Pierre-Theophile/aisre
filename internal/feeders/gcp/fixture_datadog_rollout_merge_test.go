// SPDX-License-Identifier: Apache-2.0

package gcp_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	ddfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/datadog"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/emit"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/record"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/source"
)

// datadog-log-rollout-merge-01: one rollout, whatever saw it (005 T073–T075).
//
// Cloud Run rolls `checkout` out to a new revision at 14:18, built from commit NEW and saying so in its
// `commit-sha` label — the stated instant. The Datadog connector watches `checkout`'s logs, which stamp
// the commit with the `version` tag, and first sees NEW at 14:19:12 — a bound. C9 makes the log source
// and the Cloud Run service one entity; C8 then makes the two rollouts of one commit on it one change.
// The same commit first seen in staging's logs at 14:40 is another rollout: C8 refuses it on the
// environment, and nothing in staging merges with the Cloud Run service.

const rolloutMergeFixture = "fixtures/datadog-log-rollout-merge-01"

const (
	mergeCommitOld = "0f1e2d3c4b5a69788796a5b4c3d2e1f0a9b8c7d6"
	mergeCommitNew = "7e6d5c4b3a2918070f1e2d3c4b5a69788796a5b4"
	mergeRevOld    = "checkout-00006-old"
	mergeRevNew    = "checkout-00007-new"
)

// mergeServiceJSON is `checkout` on Cloud Run, declaring its OpenTelemetry name, serving the new revision.
func mergeServiceJSON() string {
	return fmt.Sprintf(`{
  "name": "projects/%[1]s/locations/%[2]s/services/checkout",
  "uid": "uid-service-checkout",
  "generation": "7",
  "observedGeneration": "7",
  "createTime": %[3]q,
  "updateTime": %[4]q,
  "labels": {"team": %[5]q, "environment": "production", "service": "checkout"},
  "ingress": "INGRESS_TRAFFIC_ALL",
  "template": {"containers": [{"image": "europe-docker.pkg.dev/%[1]s/twin/checkout@sha256:0000000000000000000000000000000000000000000000000000000000000007",
    "env": [{"name": "OTEL_SERVICE_NAME", "value": "checkout"}]}]},
  "latestReadyRevision": "projects/%[1]s/locations/%[2]s/services/checkout/revisions/%[6]s",
  "latestCreatedRevision": "projects/%[1]s/locations/%[2]s/services/checkout/revisions/%[6]s",
  "trafficStatuses": [{"type": "TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION", "revision": %[6]q, "percent": 100}],
  "conditions": [{"type": "Ready", "state": "CONDITION_SUCCEEDED"}]
}`, twinProject, twinRegion, rfc3339(fixtureStart.Add(-720*time.Hour)), rfc3339(fixtureCreated), twinTeam, mergeRevNew)
}

func checkoutTick(t *testing.T, at time.Time, prod, staging []ddfeeder.ValueSighting) feeder.Payload {
	t.Helper()
	measure := func(source string, values []ddfeeder.ValueSighting) ddfeeder.SourceMeasurement {
		return ddfeeder.SourceMeasurement{Source: source, Lines: 12000, ErrorLines: 300, HostLines: 12000,
			Candidates: []ddfeeder.CandidateCount{{Label: "version (tag)", Lines: 12000, ErrorLines: 300}}, Values: values}
	}
	raw, err := json.Marshal(ddfeeder.DiscoveryTick{
		LogSources:   []string{"production/checkout", "staging/checkout"},
		Window:       &ddfeeder.DiscoveryWindow{From: at.Add(-time.Hour), To: at},
		Measurements: []ddfeeder.SourceMeasurement{measure("production/checkout", prod), measure("staging/checkout", staging)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return feeder.Payload{Kind: ddfeeder.PayloadDiscovery, At: at, Bytes: raw}
}

func TestGenerateDatadogLogRolloutMergeFixture(t *testing.T) {
	if os.Getenv(genFixturesEnv) == "" {
		t.Skipf("set %s=1 to regenerate %s", genFixturesEnv, rolloutMergeFixture)
	}
	dir := filepath.Join(repoRoot(t), rolloutMergeFixture)
	for _, generated := range []string{"payloads", "events.jsonl", "rejected.jsonl", "manifest.yaml", "golden"} {
		if err := os.RemoveAll(filepath.Join(dir, generated)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	clock := &arrivalClock{base: fixtureStart}
	old := ddfeeder.ValueSighting{Value: mergeCommitOld, FirstSeen: fixtureStart.Add(-8 * 24 * time.Hour), BeyondHorizon: true}

	f, err := ddfeeder.New(ddfeeder.Options{OrgSlug: ddMergeOrg})
	if err != nil {
		t.Fatal(err)
	}
	ddDesc := f.Describe()
	memory := emit.NewMemoryEmitter(ddDesc, emit.WithMemoryClock(clock.Now))
	runDatadog := func(p feeder.Payload, label string) {
		events := record.Emitter(memory, dir)
		src := record.Wrap(clock.wrap(source.NewSliceSource([]feeder.Payload{p})), dir)
		if err := f.Run(t.Context(), src, events); err != nil {
			t.Fatal(err)
		}
		assertRecorded(t, label, src, events, memory)
	}

	// 14:00: both environments run the old commit, deployed before the horizon.
	runDatadog(checkoutTick(t, fixtureStart, []ddfeeder.ValueSighting{old}, []ddfeeder.ValueSighting{old}), "datadog 14:00")

	// 14:30: Cloud Run's poll, the new revision created at 14:18 from commit NEW.
	gcpDesc := runGCPHalf(t, dir, clock, []feeder.Payload{
		servicesPayloadAt(t, cycleAt(1), mergeServiceJSON()),
		revisionsPayloadAt(t, cycleAt(1),
			deployRevisionJSON("checkout", mergeRevNew, fixtureCreated, "42", mergeCommitNew),
			deployRevisionJSON("checkout", mergeRevOld, fixtureStart.Add(-24*time.Hour), "41", mergeCommitOld)),
		pollPayloadAt(cycleAt(1), "complete", ""),
	})

	// 15:00: NEW first seen in production's logs at 14:19:12, and in staging's at 14:40.
	runDatadog(checkoutTick(t, fixtureStart.Add(time.Hour),
		[]ddfeeder.ValueSighting{{Value: mergeCommitNew, FirstSeen: fixtureCreated.Add(72 * time.Second)}},
		[]ddfeeder.ValueSighting{{Value: mergeCommitNew, FirstSeen: fixtureStart.Add(40 * time.Minute)}}), "datadog 15:00")

	if err := record.WriteManifest(dir, record.Manifest{
		Family: "datadog-cross-source",
		Description: "One rollout, whatever saw it. Cloud Run rolls checkout out to a new revision at 14:18 from " +
			"commit NEW, stated by its commit-sha label; the Datadog connector first sees NEW in checkout's " +
			"production logs at 14:19:12, a bound. C9 makes the log source and the Cloud Run service one entity " +
			"and C8 makes the two rollouts one change. The same commit first seen in staging's logs at 14:40 is " +
			"another rollout, refused by C8 on the environment. Synthetic structural twin: no identifier is " +
			"derived from the organisation.",
		Sources: []record.ManifestSource{record.SourceOf(ddDesc), record.SourceOf(gcpDesc)},
		Start:   fixtureStart.Add(-time.Minute),
		End:     fixtureStart.Add(2 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	prodChange := "datadog.change=production/checkout@" + mergeCommitNew + "@" + rfc3339(fixtureCreated.Add(72*time.Second))
	stagingChange := "datadog.change=staging/checkout@" + mergeCommitNew + "@" + rfc3339(fixtureStart.Add(40*time.Minute))
	gcpChange := "gcp.change=rollout-created/" + twinProject + "/" + twinRegion + "/checkout/" + mergeRevNew + "@" + rfc3339(fixtureCreated)
	appendQueries(t, dir, `
queries:
  # The question an investigation asks: what changed on checkout before the incident? ONE rollout,
  # carrying both sources' facts, because C9 and then C8 merged them.
  - name: what-changed-on-checkout
    kind: diff
    focus: gcp.cloudrun.service=`+twinProject+"/"+twinRegion+`/checkout
    t1: 2026-09-21T14:00:00Z
    t2: 2026-09-21T15:00:00Z
    reference_at: 2026-09-21T14:30:00Z
    observed_at: 2026-09-21T16:00:00Z
    hops: 1
    direction: both
  - name: why-the-log-rollout-is-the-cloud-run-rollout
    kind: audit
    ref_a: `+prodChange+`
    ref_b: `+gcpChange+`
    observed_at: 2026-09-21T16:00:00Z
  - name: why-the-staging-rollout-is-not-the-production-one
    kind: audit
    ref_a: `+stagingChange+`
    ref_b: `+gcpChange+`
    observed_at: 2026-09-21T16:00:00Z
`)
}
