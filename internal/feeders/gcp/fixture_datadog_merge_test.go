// SPDX-License-Identifier: Apache-2.0

package gcp_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	ddfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/datadog"
	gcpfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/gcp"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/emit"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/record"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/source"
)

// datadog-log-service-merge-01: C9's acceptance fixture (005 T017).
//
// One service, `voice-agent`, whose logs live in Datadog and which Cloud Run also runs, declaring the
// same OpenTelemetry service name. The Datadog connector watches the service in two environments,
// production and staging. C9 must merge the production log service with the Cloud Run service, and
// must NOT merge the staging one: same name, different environment, which is the pair a certain rule
// may never gamble on.
//
// The GCP half is the real GCP feeder over a services payload, as gcp-cross-source-merge-01's is. The
// Datadog half is the connector's own LogSourceEvents — what `feed datadog` emits for a watched source
// before any API call, since a log source is configuration rather than a response — so a change in what
// the connector emits changes this fixture. It gains payloads when the feeder records its discovery
// responses (T064).
//
// C9 runs from both sides (a correlation key on the Datadog side, an identity claim on the Cloud Run
// side), and the two sources arrive further apart than any reordering window, so the shuffle step
// never swaps them. Each arm therefore gets a pair of its own: `voice-agent` is watched in Datadog
// BEFORE the Cloud Run poll, so C9 fires when the claim arrives; `billing` is watched only AFTER it, so
// C9 fires when the correlation arrives. Deleting either arm fails a golden (T019).

const (
	ddMergeService     = "voice-agent"
	ddMergeLateService = "billing"
	ddMergeOrg         = "twin"
)

func TestGenerateDatadogLogServiceMergeFixture(t *testing.T) {
	if os.Getenv(genFixturesEnv) == "" {
		t.Skipf("set %s=1 to regenerate the Datadog log-service merge fixture", genFixturesEnv)
	}
	dir := filepath.Join(repoRoot(t), "fixtures/datadog-log-service-merge-01")
	for _, generated := range []string{"payloads", "events.jsonl", "rejected.jsonl", "manifest.yaml"} {
		if err := os.RemoveAll(filepath.Join(dir, generated)); err != nil {
			t.Fatalf("clear %s: %v", generated, err)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	clock := &arrivalClock{base: fixtureStart}
	ddDesc := writeDatadogLogSources(t, dir, clock, fixtureStart, []ddfeeder.LogSource{
		{Env: "production", Service: ddMergeService}, {Env: "staging", Service: ddMergeService},
	})
	gcpDesc := writeDatadogMergeGCPHalf(t, dir, clock)
	writeDatadogLogSources(t, dir, clock, cycleAt(1).Add(10*time.Minute), []ddfeeder.LogSource{
		{Env: "production", Service: ddMergeLateService},
	})

	if err := record.WriteManifest(dir, record.Manifest{
		Family: "datadog-cross-source",
		Description: "One service seen by two connectors: Cloud Run runs `voice-agent` declaring the " +
			"OpenTelemetry service name `voice-agent` in production, and the Datadog connector watches " +
			"`voice-agent`'s logs in production and in staging. C9 merges the production log service with " +
			"the Cloud Run service on the name and the agreeing environment; the staging log service " +
			"carries the same name and must stay a separate entity, because a certain rule may not " +
			"merge staging into production. A second service, `billing`, is watched in Datadog only " +
			"after the Cloud Run poll, so C9's other arm — the one run when the correlation arrives — " +
			"has a pair of its own. Synthetic structural twin: no identifier is derived from the " +
			"organisation.",
		Sources: []record.ManifestSource{record.SourceOf(ddDesc), record.SourceOf(gcpDesc)},
		Start:   fixtureStart,
		End:     cycleAt(1).Add(time.Hour),
	}); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	appendQueries(t, dir, datadogMergeQueries())
}

func writeDatadogLogSources(t *testing.T, dir string, clock *arrivalClock, from time.Time, sources []ddfeeder.LogSource) feeder.Description {
	t.Helper()
	desc, err := ddfeeder.Describe(ddMergeOrg)
	if err != nil {
		t.Fatal(err)
	}
	memory := emit.NewMemoryEmitter(desc, emit.WithMemoryClock(clock.Now))
	events := record.Emitter(memory, dir)
	for i, src := range sources {
		at := from.Add(time.Duration(i+1) * time.Minute)
		clock.base, clock.step = at, 0
		batch, err := ddfeeder.LogSourceEvents(desc, src, at)
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range batch {
			if _, err := events.Emit(t.Context(), ev); err != nil {
				t.Fatalf("emit %s: %v", ev.GetEventId(), err)
			}
		}
	}
	if err := events.Err(); err != nil {
		t.Fatalf("record datadog events: %v", err)
	}
	if rejected := memory.Rejected(); len(rejected) > 0 {
		t.Fatalf("%d datadog events were refused; the first is %s (%s)",
			len(rejected), rejected[0].GetEventId(), rejected[0].GetReasonDetail())
	}
	return desc
}

func writeDatadogMergeGCPHalf(t *testing.T, dir string, clock *arrivalClock) feeder.Description {
	t.Helper()
	labels := map[string]string{"team": twinTeam, "environment": "production"}
	payloads := []feeder.Payload{
		servicesPayloadAt(t, cycleAt(1),
			twinDeclaringServiceJSON(ddMergeService, ddMergeService, labels),
			twinDeclaringServiceJSON(ddMergeLateService, ddMergeLateService, labels)),
		pollPayloadAt(cycleAt(1), "complete", ""),
	}
	f, err := gcpfeeder.New(gcpfeeder.Options{
		OrgSlug:         "twin",
		Scope:           gcpfeeder.Scope{Projects: []string{twinProject}, Regions: []string{twinRegion}},
		Labels:          twinLabelPolicy(),
		Actors:          twinActorPolicy(),
		Horizon:         gcpfeeder.Horizon{Earliest: fixtureStart, Reason: gcpfeeder.HorizonConfigured},
		OmittedSurfaces: []string{"cloud_dns", "load_balancers"},
		FingerprintKey:  []byte(twinFingerprintKey),
	})
	if err != nil {
		t.Fatalf("new gcp feeder: %v", err)
	}
	desc := f.Describe()
	src := record.Wrap(clock.wrap(source.NewSliceSource(payloads)), dir)
	memory := emit.NewMemoryEmitter(desc, emit.WithMemoryClock(clock.Now))
	events := record.Emitter(memory, dir)
	if err := f.Run(t.Context(), src, events); err != nil {
		t.Fatalf("gcp half: run: %v", err)
	}
	assertRecorded(t, "gcp half", src, events, memory)
	return desc
}

func datadogMergeQueries() string {
	return `
queries:
  # C9's merge: the production log service IS the Cloud Run service, and the audit names C9.
  - name: why-the-production-log-service-is-the-cloud-run-service
    kind: audit
    ref_a: datadog.service=production/voice-agent
    ref_b: gcp.cloudrun.service=twin-production/europe-west1/voice-agent
    observed_at: 2026-09-21T20:00:00Z
  # The pair that must stay apart: same name, staging.
  - name: why-the-staging-log-service-is-not-the-cloud-run-service
    kind: audit
    ref_a: datadog.service=staging/voice-agent
    ref_b: gcp.cloudrun.service=twin-production/europe-west1/voice-agent
    observed_at: 2026-09-21T20:00:00Z
  # C9's other arm: billing's Datadog key arrives after the Cloud Run claim.
  - name: why-the-late-log-service-is-the-cloud-run-service
    kind: audit
    ref_a: datadog.service=production/billing
    ref_b: gcp.cloudrun.service=twin-production/europe-west1/billing
    observed_at: 2026-09-21T20:00:00Z
  # And the two Datadog log services are not one either.
  - name: why-staging-is-not-production
    kind: audit
    ref_a: datadog.service=staging/voice-agent
    ref_b: datadog.service=production/voice-agent
    observed_at: 2026-09-21T20:00:00Z

ground_truth:
  cross_source_pairs:
    - pair:
        - datadog.service=production/voice-agent
        - gcp.cloudrun.service=twin-production/europe-west1/voice-agent
      same: true
      rule: C9
    - pair:
        - datadog.service=production/billing
        - gcp.cloudrun.service=twin-production/europe-west1/billing
      same: true
      rule: C9
  distinct_pairs:
    - pair:
        - datadog.service=staging/voice-agent
        - gcp.cloudrun.service=twin-production/europe-west1/voice-agent
      same: false
    - pair:
        - datadog.service=staging/voice-agent
        - datadog.service=production/voice-agent
      same: false
`
}
