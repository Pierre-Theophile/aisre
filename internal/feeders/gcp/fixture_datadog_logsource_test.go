// SPDX-License-Identifier: Apache-2.0

package gcp_test

import (
	"encoding/json"
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

// datadog-log-source-01: a node and a pointer for every watched log source (005 T063–T067).
//
// Three services' logs are watched in Datadog, production:
//
//   - `checkout` stamps its version with the `version` tag on every line: the pointer's version join
//     key is `version`. Cloud Run also runs `checkout`, declaring the same OpenTelemetry name, and C9
//     merges the two — so the merged entity carries Cloud Run's pointers AND Datadog's (FR-039);
//   - `search` carries only the audit's shape: an SDK's own `@version` on its start-up lines, a few
//     lines in thousands. The share test rejects it, the pointer has no version join key, and the
//     engine answers errors_by_version itself (ADR-0010 item 2);
//   - `payments` has no stamp in the first window and one in the second, after its team set DD_VERSION:
//     the verdict changes, and the pointer is versioned in valid time (FR-038).
//
// The Datadog half is the connector's own feeder over recorded discovery payloads — presence counts
// per candidate, as the live poller measures them — so a change in how the verdict is decided changes
// this fixture.

const logSourceFixture = "fixtures/datadog-log-source-01"

func candidateCounts(counts map[string][2]int64) []ddfeeder.CandidateCount {
	var out []ddfeeder.CandidateCount
	for _, c := range []string{"version (tag)", "service.version (attribute)", "version (attribute)",
		"git.commit.sha (tag)", "git.commit.sha (attribute)",
		"container.image.name + container.image.digest (attribute_pair)", "faas.version (attribute)"} {
		n := counts[c]
		out = append(out, ddfeeder.CandidateCount{Label: c, Lines: n[0], ErrorLines: n[1]})
	}
	return out
}

func discoveryPayload(t *testing.T, at time.Time, measurements ...ddfeeder.SourceMeasurement) feeder.Payload {
	t.Helper()
	raw, err := json.Marshal(ddfeeder.DiscoveryTick{
		LogSources:   []string{"production/checkout", "production/search", "production/payments"},
		Window:       &ddfeeder.DiscoveryWindow{From: at.Add(-time.Hour), To: at},
		Measurements: measurements,
	})
	if err != nil {
		t.Fatal(err)
	}
	return feeder.Payload{Kind: ddfeeder.PayloadDiscovery, At: at, Bytes: raw}
}

func logSourcePayloads(t *testing.T) []feeder.Payload {
	checkout := ddfeeder.SourceMeasurement{Source: "production/checkout", Lines: 12000, ErrorLines: 300, HostLines: 12000,
		Candidates: candidateCounts(map[string][2]int64{"version (tag)": {12000, 300}})}
	search := ddfeeder.SourceMeasurement{Source: "production/search", Lines: 8000, ErrorLines: 40, HostLines: 8000,
		Candidates: candidateCounts(map[string][2]int64{"version (attribute)": {6, 0}})}
	paymentsBefore := ddfeeder.SourceMeasurement{Source: "production/payments", Lines: 5000, ErrorLines: 20, HostLines: 5000,
		Candidates: candidateCounts(nil)}
	paymentsAfter := paymentsBefore
	paymentsAfter.Candidates = candidateCounts(map[string][2]int64{"version (tag)": {4990, 20}})
	return []feeder.Payload{
		discoveryPayload(t, fixtureStart, checkout, search, paymentsBefore),
		discoveryPayload(t, fixtureStart.Add(time.Hour), checkout, search, paymentsAfter),
	}
}

func TestGenerateDatadogLogSourceFixture(t *testing.T) {
	if os.Getenv(genFixturesEnv) == "" {
		t.Skipf("set %s=1 to regenerate %s", genFixturesEnv, logSourceFixture)
	}
	dir := filepath.Join(repoRoot(t), logSourceFixture)
	for _, generated := range []string{"payloads", "events.jsonl", "rejected.jsonl", "manifest.yaml", "golden"} {
		if err := os.RemoveAll(filepath.Join(dir, generated)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	clock := &arrivalClock{base: fixtureStart}

	// The first discovery tick, at 14:00.
	payloads := logSourcePayloads(t)
	f, err := ddfeeder.New(ddfeeder.Options{OrgSlug: ddMergeOrg})
	if err != nil {
		t.Fatal(err)
	}
	ddDesc := f.Describe()
	memory := emit.NewMemoryEmitter(ddDesc, emit.WithMemoryClock(clock.Now))
	events := record.Emitter(memory, dir)
	first := record.Wrap(clock.wrap(source.NewSliceSource(payloads[:1])), dir)
	if err := f.Run(t.Context(), first, events); err != nil {
		t.Fatal(err)
	}
	assertRecorded(t, "datadog discovery 1", first, events, memory)

	// Cloud Run's poll at 14:30: `checkout` declaring the same OpenTelemetry name.
	gcpDesc := writeCheckoutCloudRun(t, dir, clock)

	// The second tick, at 15:00, on the same feeder: it remembers the verdicts it asserted.
	// A fresh event recorder: it numbers lines from what the file already holds, and the GCP half has
	// appended to it since the first one was made.
	events = record.Emitter(memory, dir)
	second := record.Wrap(clock.wrap(source.NewSliceSource(payloads[1:])), dir)
	if err := f.Run(t.Context(), second, events); err != nil {
		t.Fatal(err)
	}
	assertRecorded(t, "datadog discovery 2", second, events, memory)

	if err := record.WriteManifest(dir, record.Manifest{
		Family: "datadog-cross-source",
		Description: "A node and a pointer for every watched Datadog log source. `checkout` stamps its " +
			"version with the `version` tag, so its datadog-logs/v1 pointer's version join key is `version`; " +
			"Cloud Run also runs `checkout` under the same OpenTelemetry name and environment, C9 merges them, " +
			"and the merged entity carries both connectors' pointers. `search` carries only an SDK's own " +
			"`@version` on a handful of start-up lines, which the share test rejects: its pointer has no " +
			"version join key and its verdict says why. `payments` has no stamp from 13:00 to 14:00 and one " +
			"from 14:00 to 15:00, so its pointer gains the join key from the 15:00 tick, and an as-of read " +
			"either side shows each. Synthetic structural twin: no identifier is derived from the organisation.",
		Sources: []record.ManifestSource{record.SourceOf(ddDesc), record.SourceOf(gcpDesc)},
		Start:   fixtureStart.Add(-time.Minute),
		End:     fixtureStart.Add(2 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	appendQueries(t, dir, `
queries:
  # FR-039: on the merged entity, Datadog's log pointer is added to Cloud Run's, not substituted.
  - name: checkout-pointers-merged
    kind: pointers
    focus: datadog.service=production/checkout
    valid_at: 2026-09-21T15:30:00Z
    observed_at: 2026-09-21T16:00:00Z
  - name: why-the-log-service-is-the-cloud-run-service
    kind: audit
    ref_a: datadog.service=production/checkout
    ref_b: gcp.cloudrun.service=twin-production/europe-west1/checkout
    observed_at: 2026-09-21T16:00:00Z
  # The unstamped source: a pointer with no version join key, and the verdict naming why.
  - name: search-unstamped
    kind: subgraph
    focus: datadog.service=production/search
    valid_at: 2026-09-21T15:30:00Z
    observed_at: 2026-09-21T16:00:00Z
    hops: 0
    direction: both
  # FR-038: the pointer as of each side of the verdict change.
  - name: payments-pointers-before-the-stamp
    kind: pointers
    focus: datadog.service=production/payments
    valid_at: 2026-09-21T14:30:00Z
    observed_at: 2026-09-21T16:00:00Z
  - name: payments-pointers-after-the-stamp
    kind: pointers
    focus: datadog.service=production/payments
    valid_at: 2026-09-21T15:30:00Z
    observed_at: 2026-09-21T16:00:00Z
`)
}

// writeCheckoutCloudRun is the GCP half: one poll of one service declaring `checkout`.
func writeCheckoutCloudRun(t *testing.T, dir string, clock *arrivalClock) feeder.Description {
	t.Helper()
	labels := map[string]string{"team": twinTeam, "environment": "production"}
	return runGCPHalf(t, dir, clock, []feeder.Payload{
		servicesPayloadAt(t, cycleAt(1), twinDeclaringServiceJSON("checkout", "checkout", labels)),
		pollPayloadAt(cycleAt(1), "complete", ""),
	})
}
