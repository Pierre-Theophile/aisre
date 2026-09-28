// SPDX-License-Identifier: Apache-2.0

package datadog_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	ddfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/datadog"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// datadog-log-source-no-env-01: logs that carry no environment (005, the first live run).
//
// Many organisations ship logs without unified service tagging's `env`; the first live organisation's
// logs carried none at all. An agent installed there must still measure them, and must say what to fix:
//
//   - `checkout` is watched without an environment (`--watch checkout`): it is measured on
//     `service:checkout` alone, its `version` tag is accepted, and its pointer is `service:checkout`.
//     It carries no C9 claim, because a name alone would merge a service across every environment it
//     runs in, and the checkpoint says "missing env to match service" with how to add one;
//   - `production/billing` is watched in an environment no line carries, while 3 000 of the service's
//     lines carry no `env` at all: the checkpoint says the logs have no environment, rather than
//     letting it read as a silent service.

const noEnvFixture = "fixtures/datadog-log-source-no-env-01"

func noEnvPayloads(t *testing.T) []feeder.Payload {
	t.Helper()
	checkout := ddfeeder.SourceMeasurement{Source: "checkout", Lines: 8000, ErrorLines: 80, HostLines: 8000,
		Candidates: []ddfeeder.CandidateCount{{Label: "version (tag)", Lines: 8000, ErrorLines: 80}}}
	billing := ddfeeder.SourceMeasurement{Source: "production/billing", LinesWithoutEnv: 3000}
	var out []feeder.Payload
	for _, at := range []time.Time{hm(14, 0), hm(15, 0)} {
		raw, err := json.Marshal(ddfeeder.DiscoveryTick{LogSources: []string{"checkout", "production/billing"},
			Window:       &ddfeeder.DiscoveryWindow{From: at.Add(-time.Hour), To: at},
			Measurements: []ddfeeder.SourceMeasurement{checkout, billing}})
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, feeder.Payload{Kind: ddfeeder.PayloadDiscovery, At: at, Bytes: raw})
	}
	return out
}

func noEnvOptions(log *slog.Logger) ddfeeder.Options {
	return ddfeeder.Options{OrgSlug: "twin", Log: log,
		LogSources: []ddfeeder.LogSource{{Service: "checkout"}, {Env: "production", Service: "billing"}}}
}

func TestGenerateDatadogNoEnvFixture(t *testing.T) {
	if os.Getenv(genFixturesEnv) == "" {
		t.Skipf("set %s=1 to regenerate %s", genFixturesEnv, noEnvFixture)
	}
	generateFixture(t, noEnvFixture, "datadog-log-sources",
		"Logs that carry no environment. `checkout` is watched without one: it is measured on "+
			"`service:checkout` alone, its `version` tag is accepted, and its pointer is `service:checkout`; it "+
			"carries no C9 claim, since a name alone would merge a service across every environment, and the "+
			"checkpoint says `missing env to match service` with how to add one. `production/billing` is "+
			"watched in an environment no line carries while 3000 of the service's lines carry none, and the "+
			"checkpoint says so rather than letting it read as a silent service.",
		noEnvOptions(nil), noEnvPayloads(t), hm(12, 59), hm(15, 30), `
queries:
  # The environment-less source's pointer: service alone, with its version join key.
  - name: checkout-pointer-without-an-environment
    kind: pointers
    focus: datadog.service=checkout
    valid_at: 2026-09-21T15:10:00Z
    observed_at: 2026-09-21T15:30:00Z
`)
}

func TestLogsWithoutAnEnvironmentAreMeasuredAndWarnedAbout(t *testing.T) {
	t.Parallel()
	var logged bytes.Buffer
	em := run(t, noEnvOptions(slog.New(slog.NewTextHandler(&logged, nil))), noEnvPayloads(t)...)

	var pointer *ddfeederPointer
	var notes []string
	correlated := map[string]bool{}
	for _, ev := range em.Events() {
		if n := ev.GetUpsertNode(); n != nil && n.GetRef().GetValue() == "checkout" {
			for _, p := range n.GetPointers() {
				pointer = &ddfeederPointer{selector: p.GetSelector(), version: p.GetJoinKeys()["version"]}
			}
			if _, ok := n.GetProps().GetFields()[feeder.AttrDeploymentEnvironment]; ok {
				t.Error("the environment-less node states an environment")
			}
		}
		if c := ev.GetCorrelateEntity(); c != nil && c.GetKey().GetNamespace() == ddfeeder.NSLogService {
			correlated[c.GetSubject().GetValue()] = true
		}
		if c := ev.GetSourceCheckpoint(); c != nil {
			notes = append(notes, c.GetNote())
		}
	}
	if pointer == nil || pointer.selector != "service:checkout" || pointer.version != "version" {
		t.Errorf("checkout's pointer %+v, want `service:checkout` with the version join key", pointer)
	}
	if correlated["checkout"] || !correlated["production/billing"] {
		t.Errorf("C9 claims on %v: an environment-less source must carry none, a sourced one keeps its own", correlated)
	}
	all := strings.Join(notes, "\n")
	for _, want := range []string{
		`checkout: missing env to match service "checkout"`,
		"production/billing: no line carries env:production, but 3000 line(s) of service billing carry no env at all",
		"--watch billing",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("no checkpoint says %q:\n%s", want, all)
		}
	}
	// Two ticks, each stated in its checkpoint, each warning logged once.
	if n := strings.Count(logged.String(), `missing env to match service \"checkout\"`); n != 1 {
		t.Errorf("the checkout warning was logged %d times, want once per run:\n%s", n, logged.String())
	}
}

type ddfeederPointer struct{ selector, version string }
