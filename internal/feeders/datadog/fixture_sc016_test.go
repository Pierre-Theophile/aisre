// SPDX-License-Identifier: Apache-2.0

package datadog_test

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	ddfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/datadog"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// datadog-monitor-to-owner-01: SC-016's corpus (005 T068).
//
// Starting from a Datadog monitor id alone, one command answers:
//
//   - the alert: monitor 8101 on `checkout`, triggered at 14:40:00 (Datadog's stated instant);
//   - what it watches: the `production/checkout` log source;
//   - what changed there in the preceding window: a new commit first seen in checkout's own logs
//     at 14:12:30 (the one it had run for weeks was first seen before the horizon, so it is not a
//     change);
//   - who owns it: `team:payments`, on every line;
//   - executable pointers: the monitor's query and the log source's datadog-logs/v1 selector with
//     its version join key.
//
// Every fact comes from the connector's own feeder over the payloads its live poller pushes, so a
// change to any of them changes this fixture.

const sc016Fixture = "fixtures/datadog-monitor-to-owner-01"

func sc016Payloads(t *testing.T) []feeder.Payload {
	t.Helper()
	m := ddfeeder.SourceMeasurement{Source: "production/checkout", Lines: 12000, ErrorLines: 300, HostLines: 12000,
		Candidates: []ddfeeder.CandidateCount{{Label: "version (tag)", Lines: 12000, ErrorLines: 300}},
		Values: []ddfeeder.ValueSighting{
			{Value: commitA, FirstSeen: hm(14, 30).Add(-8 * 24 * time.Hour), BeyondHorizon: true},
			{Value: commitB, FirstSeen: hms(14, 12, 30)},
		},
		Tags: []ddfeeder.TagCount{{Key: "team", Value: "payments", Lines: 12000}}}
	tick, err := json.Marshal(ddfeeder.DiscoveryTick{LogSources: []string{"production/checkout"},
		Window: &ddfeeder.DiscoveryWindow{From: hm(13, 30), To: hm(14, 30)}, Measurements: []ddfeeder.SourceMeasurement{m}})
	if err != nil {
		t.Fatal(err)
	}
	page, err := json.Marshal([]monitor{twinMonitor{
		id: 8101, name: "checkout error rate", kind: "log alert", priority: 1,
		query:  `logs("service:checkout env:production status:error").index("*").rollup("count").last("5m") > 50`,
		tags:   []string{"service:checkout", "env:production"},
		groups: map[string]group{"*": {Status: "Alert", Triggered: hm(14, 40).Unix()}},
	}.json()})
	if err != nil {
		t.Fatal(err)
	}
	return []feeder.Payload{
		{Kind: ddfeeder.PayloadDiscovery, At: hm(14, 30), Bytes: tick},
		{Kind: ddfeeder.PayloadMonitors, At: hms(14, 40, 20), Bytes: page},
		poll(hms(14, 40, 21), "complete"),
	}
}

func TestGenerateDatadogSC016Fixture(t *testing.T) {
	if os.Getenv(genFixturesEnv) == "" {
		t.Skipf("set %s=1 to regenerate %s", genFixturesEnv, sc016Fixture)
	}
	generateFixture(t, sc016Fixture, "datadog-cross-source",
		"SC-016's corpus: from a Datadog monitor id alone, the alert, what it watches, what changed there "+
			"before it fired, who owns it, and executable pointers. Monitor 8101 watches production/checkout "+
			"and triggers at 14:40:00; checkout's own logs first show a new commit at 14:12:30 (the one it "+
			"ran for weeks predates the horizon and is not a change); `team:payments` is on every line.",
		ddfeeder.Options{OrgSlug: "twin", MonitorTags: []string{"env:production"}}, sc016Payloads(t), hm(13, 29), hm(15, 0), `
queries:
  # SC-016 in one query: the alert's neighbourhood, and what changed in the two hours before it fired.
  - name: from-the-monitor-id
    kind: diff
    focus: datadog.monitor=8101
    t1: 2026-09-21T12:40:00Z
    t2: 2026-09-21T14:40:00Z
    reference_at: 2026-09-21T14:40:00Z
    observed_at: 2026-09-21T15:00:00Z
    hops: 2
    direction: both
  - name: monitor-pointers
    kind: pointers
    focus: datadog.monitor=8101
    valid_at: 2026-09-21T14:40:00Z
    observed_at: 2026-09-21T15:00:00Z
`)
}
