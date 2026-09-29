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

// datadog-log-source-no-env-01: where a service's environment is carried (005, the first live run).
//
// Unified service tagging puts the environment in the `env` tag; many organisations' logs carry it
// elsewhere or not at all. The connector discovers the field (envfield.go) and says, per source, how it
// read the environment or what to fix:
//
//   - `payments` (production) carries it as the `@env` attribute, discovered on 99.8 % of its lines: it
//     is measured and pointed at on `service:payments @env:production`, keeps its environment and its C9
//     claim, and the checkpoint states the field and suggests a Remapper for the other tools;
//   - `search` (production) is configured with `--env-field @env`: the same, without a discovery;
//   - `worker` is watched without an environment and its logs carry `@env`: the checkpoint names the
//     environments found and the source to watch instead;
//   - `ledger` is watched as `prod/ledger` while its `@env` says `staging`: no line matches, and the
//     checkpoint names the environments that exist;
//   - `checkout` is watched without an environment, and `billing` in `production`, and neither's lines
//     carry any published environment field: both say "missing env to match service".

const noEnvFixture = "fixtures/datadog-log-source-no-env-01"

func discovered(total int64, present map[string]int64, values ...ddfeeder.EnvValue) *ddfeeder.EnvDiscovery {
	d := &ddfeeder.EnvDiscovery{ServiceLines: total, Values: values}
	for _, c := range ddfeeder.EnvFields {
		d.Fields = append(d.Fields, ddfeeder.CandidateCount{Label: c.Label(), Lines: present[c.Field]})
		if ddfeeder.DecideEnvField(total, present) == c.Field {
			break
		}
	}
	return d
}

func stamped(source string, lines int64) ddfeeder.SourceMeasurement {
	return ddfeeder.SourceMeasurement{Source: source, Lines: lines, ErrorLines: lines / 100, HostLines: lines,
		Candidates: []ddfeeder.CandidateCount{{Label: "version (tag)", Lines: lines, ErrorLines: lines / 100}}}
}

func noEnvPayloads(t *testing.T) []feeder.Payload {
	t.Helper()
	payments := stamped("production/payments", 4990)
	payments.EnvField, payments.EnvDiscovery = "@env", discovered(5000, map[string]int64{"@env": 4990})
	search := stamped("production/search", 6000)
	search.EnvField = "@env"
	worker := stamped("worker", 5000)
	worker.EnvField, worker.EnvDiscovery = "@env", discovered(5000, map[string]int64{"@env": 5000},
		ddfeeder.EnvValue{Value: "production", Lines: 4990}, ddfeeder.EnvValue{Value: "staging", Lines: 10})
	ledger := ddfeeder.SourceMeasurement{Source: "prod/ledger", EnvField: "@env",
		EnvDiscovery: discovered(2000, map[string]int64{"@env": 2000}, ddfeeder.EnvValue{Value: "staging", Lines: 2000})}
	checkout := stamped("checkout", 8000)
	checkout.EnvDiscovery = discovered(8000, nil)
	billing := ddfeeder.SourceMeasurement{Source: "production/billing", EnvDiscovery: discovered(3000, nil)}
	all := []ddfeeder.SourceMeasurement{payments, search, worker, ledger, checkout, billing}

	var out []feeder.Payload
	for i, at := range []time.Time{hm(14, 0), hm(15, 0)} {
		measurements := all
		if i == 1 {
			// The second tick rediscovers nothing: the poller remembered each field. The checkpoint still
			// says what the first discovery found.
			measurements = nil
			for _, m := range all {
				m.EnvDiscovery = nil
				measurements = append(measurements, m)
			}
		}
		var sources []string
		for _, m := range measurements {
			sources = append(sources, m.Source)
		}
		raw, err := json.Marshal(ddfeeder.DiscoveryTick{LogSources: sources,
			Window: &ddfeeder.DiscoveryWindow{From: at.Add(-time.Hour), To: at}, Measurements: measurements})
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, feeder.Payload{Kind: ddfeeder.PayloadDiscovery, At: at, Bytes: raw})
	}
	return out
}

func noEnvOptions(log *slog.Logger) ddfeeder.Options {
	return ddfeeder.Options{OrgSlug: "twin", Log: log, LogSources: []ddfeeder.LogSource{
		{Env: "production", Service: "payments"}, {Env: "production", Service: "search", EnvField: "@env"},
		{Service: "worker"}, {Env: "prod", Service: "ledger"}, {Service: "checkout"}, {Env: "production", Service: "billing"},
	}}
}

func TestGenerateDatadogNoEnvFixture(t *testing.T) {
	if os.Getenv(genFixturesEnv) == "" {
		t.Skipf("set %s=1 to regenerate %s", genFixturesEnv, noEnvFixture)
	}
	generateFixture(t, noEnvFixture, "datadog-log-sources",
		"Where a service's environment is carried, discovered rather than required. `payments` carries it as "+
			"the `@env` attribute on 99.8% of its lines: it is measured and pointed at on `service:payments "+
			"@env:production` and keeps its C9 claim. `search` is configured with --env-field @env. `worker` is "+
			"watched without an environment and its logs carry `@env`: the checkpoint names the environments "+
			"found. `ledger` is watched as `prod` while its `@env` says `staging`: the checkpoint names what "+
			"exists. `checkout` and `billing` carry no environment field at all: missing env to match service.",
		noEnvOptions(nil), noEnvPayloads(t), hm(12, 59), hm(15, 30), `
queries:
  # The discovered @env field: the pointer searches it, and the environment is kept.
  - name: payments-pointer-on-the-discovered-env-field
    kind: pointers
    focus: datadog.service=production/payments
    valid_at: 2026-09-21T15:10:00Z
    observed_at: 2026-09-21T15:30:00Z
  # An environment-less source: service alone.
  - name: checkout-pointer-without-an-environment
    kind: pointers
    focus: datadog.service=checkout
    valid_at: 2026-09-21T15:10:00Z
    observed_at: 2026-09-21T15:30:00Z
`)
}

func TestTheEnvironmentIsDiscoveredAndStated(t *testing.T) {
	t.Parallel()
	var logged bytes.Buffer
	em := run(t, noEnvOptions(slog.New(slog.NewTextHandler(&logged, nil))), noEnvPayloads(t)...)

	selectors := map[string]string{}
	correlated := map[string]bool{}
	var notes []string
	for _, ev := range em.Events() {
		if n := ev.GetUpsertNode(); n != nil {
			for _, p := range n.GetPointers() {
				selectors[n.GetRef().GetValue()] = p.GetSelector()
			}
		}
		if c := ev.GetCorrelateEntity(); c != nil && c.GetKey().GetNamespace() == ddfeeder.NSLogService {
			correlated[c.GetSubject().GetValue()] = true
		}
		if c := ev.GetSourceCheckpoint(); c != nil && strings.HasPrefix(c.GetNote(), "datadog log-source discovery") {
			notes = append(notes, c.GetNote())
		}
	}
	for source, want := range map[string]string{
		"production/payments": "service:payments @env:production",
		"production/search":   "service:search @env:production",
		"worker":              "service:worker",
		"checkout":            "service:checkout",
	} {
		if selectors[source] != want {
			t.Errorf("%s's pointer %q, want %q", source, selectors[source], want)
		}
	}
	if !correlated["production/payments"] || !correlated["production/search"] || correlated["worker"] || correlated["checkout"] {
		t.Errorf("C9 claims %v: a source with an environment keeps one, one without has none", correlated)
	}
	if len(notes) != 2 {
		t.Fatalf("%d discovery checkpoints, want 2", len(notes))
	}
	for i, note := range notes {
		for _, want := range []string{
			"production/payments: the environment is read from @env, present on 99.8% of the service's lines",
			"worker: its logs carry the environment in @env: production (4990 lines), staging (10 lines). Watch <env>/worker (for example --watch production/worker)",
			"prod/ledger: no line carries @env:prod, and the service's lines carry @env: staging (2000 lines). Check the environment's name: --watch staging/ledger",
			`checkout: missing env to match service "checkout"`,
			"production/billing: none of env, @env, @environment, @deployment.environment.name is on the service's lines",
		} {
			if i == 1 && strings.HasPrefix(want, "production/payments") {
				want = "production/payments: the environment is read from @env"
			}
			if !strings.Contains(note, want) {
				t.Errorf("checkpoint %d does not say %q:\n%s", i, want, note)
			}
		}
	}
	for _, want := range []string{`msg="worker: its logs carry`, `level=INFO msg="production/payments: the environment is read from @env`} {
		if n := strings.Count(logged.String(), want); n != 1 {
			t.Errorf("%q logged %d times, want once:\n%s", want, n, logged.String())
		}
	}
}
