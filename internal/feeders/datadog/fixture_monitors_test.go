// SPDX-License-Identifier: Apache-2.0

package datadog_test

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	ddfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/datadog"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/emit"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/record"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/source"
)

// The monitor fixtures (005 T059, T060, T062). Each is a sequence of polls of a structural twin of
// Datadog's monitor list, run through the real feeder with its payloads and events recorded. Only the
// polls at which something changed are recorded: the unchanged ones between them re-send ids already
// sent, which the graph answers DUPLICATE_NOOP, so they would add bytes and prove nothing. The
// transitions still state the live poll interval they were sampled at.
//
// Two assertions run on every test run, not only on regeneration: the recorded payloads, replayed
// through the feeder, reproduce the recorded events exactly (FR-044); and every transition's valid
// time is an instant Datadog stated in the payload that carried it (SC-003).

const genFixturesEnv = "SRE_AGENT_GEN_FIXTURES"

// twinMonitor is one monitor of the twin, with its groups' current states.
type twinMonitor struct {
	id       int64
	name     string
	kind     string
	query    string
	tags     []string
	priority int
	groups   map[string]group
}

func (m twinMonitor) json() monitor {
	p := m.priority
	out := monitor{ID: m.id, Name: m.name, Type: m.kind, Query: m.query, Tags: m.tags, Priority: &p,
		Message: "Paging the owning team; see the runbook.", Modified: "2026-09-01T09:00:00Z"}
	out.State.Groups = map[string]group{}
	for k, v := range m.groups {
		out.State.Groups[k] = v
	}
	return out
}

// step is one recorded poll or discovery tick: the changes applied to the twin at that instant.
type step struct {
	at        time.Time
	discovery []string // non-nil: a discovery tick naming these log sources
	changes   map[int64]map[string]group
}

// monitorFixture is one generated fixture.
type monitorFixture struct {
	dir, family, description string
	monitors                 []twinMonitor
	steps                    []step
	queries                  string
	end                      time.Time
}

// payloadsOf plays the steps against the twin: each poll lists every monitor as it stands.
func (fx monitorFixture) payloadsOf(t *testing.T) []feeder.Payload {
	t.Helper()
	state := map[int64]*twinMonitor{}
	order := make([]int64, 0, len(fx.monitors))
	for i := range fx.monitors {
		m := fx.monitors[i]
		m.groups = map[string]group{}
		for k, v := range fx.monitors[i].groups {
			m.groups[k] = v
		}
		state[m.id] = &m
		order = append(order, m.id)
	}
	var out []feeder.Payload
	for _, s := range fx.steps {
		if s.discovery != nil {
			raw, _ := json.Marshal(ddfeeder.DiscoveryTick{LogSources: s.discovery})
			out = append(out, feeder.Payload{Kind: ddfeeder.PayloadDiscovery, At: s.at, Bytes: raw})
			continue
		}
		for id, groups := range s.changes {
			for name, g := range groups {
				state[id].groups[name] = g
			}
		}
		page := make([]monitor, 0, len(order))
		for _, id := range order {
			page = append(page, state[id].json())
		}
		raw, err := json.Marshal(page)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, feeder.Payload{Kind: ddfeeder.PayloadMonitors, At: s.at, Bytes: raw}, poll(s.at.Add(time.Second), "complete"))
	}
	return out
}

func fixtureOptions() ddfeeder.Options {
	return ddfeeder.Options{OrgSlug: "twin", Site: "datadoghq.eu", MonitorTags: []string{"env:production"}}
}

func generateMonitorFixture(t *testing.T, fx monitorFixture) {
	t.Helper()
	generateFixture(t, fx.dir, fx.family, fx.description+" Only the polls at which something changed are "+
		"recorded; the unchanged polls between them re-send ids already sent.",
		fixtureOptions(), fx.payloadsOf(t), fx.steps[0].at.Add(-time.Minute), fx.end, fx.queries)
}

// generateFixture runs the feeder over payloads, recording them and its events into dir, and writes the
// manifest with its queries.
func generateFixture(t *testing.T, fixture, family, description string, opts ddfeeder.Options, payloads []feeder.Payload, start, end time.Time, queries string) {
	t.Helper()
	dir := filepath.Join(repoRoot(t), fixture)
	for _, generated := range []string{"payloads", "events.jsonl", "rejected.jsonl", "manifest.yaml", "golden"} {
		if err := os.RemoveAll(filepath.Join(dir, generated)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := ddfeeder.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	clock := &arrivalClock{}
	src := record.Wrap(clock.wrap(source.NewSliceSource(payloads)), dir)
	memory := emit.NewMemoryEmitter(f.Describe(), emit.WithMemoryClock(clock.Now))
	events := record.Emitter(memory, dir)
	if err := f.Run(t.Context(), src, events); err != nil {
		t.Fatal(err)
	}
	if err := src.Err(); err != nil {
		t.Fatal(err)
	}
	if err := events.Err(); err != nil {
		t.Fatal(err)
	}
	if rejected := memory.Rejected(); len(rejected) > 0 {
		t.Fatalf("%d events refused; the first is %s (%s)", len(rejected), rejected[0].GetEventId(), rejected[0].GetReasonDetail())
	}
	if err := record.WriteManifest(dir, record.Manifest{
		Family:      family,
		Description: description + " Synthetic structural twin: no identifier is derived from the organisation.",
		Sources:     []record.ManifestSource{record.SourceOf(f.Describe())},
		Start:       start,
		End:         end,
	}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "manifest.yaml")
	existing, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(existing, []byte(queries)...), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ---- datadog-monitor-transitions-01 (T059) ----------------------------------------------------------

func hm(h, m int) time.Time { return time.Date(2026, 9, 21, h, m, 0, 0, time.UTC) }

func hms(h, m, s int) time.Time { return time.Date(2026, 9, 21, h, m, s, 0, time.UTC) }

func ok() group { return group{Status: "OK"} }

func transitionsFixture() monitorFixture {
	return monitorFixture{
		dir:    "fixtures/datadog-monitor-transitions-01",
		family: "datadog-monitors",
		description: "Four monitors polled from a structural twin of Datadog's monitor list. `checkout " +
			"errors` goes OK → ALERT at 14:32 → OK at 15:10, and the alert's validity is [14:32, 15:10), " +
			"Datadog's own instants rather than the polls that read them, with the history marked sampled " +
			"and the recovery kept. `checkout latency` flaps — alert 14:40:00, ok 14:40:30, alert " +
			"14:40:50 — and only the first alert is emitted, the rest stated as flapping in the " +
			"checkpoint, until its real recovery at 14:50. `search heartbeat` goes to no data at 14:45 " +
			"and back at 14:55, stated as not triggering. `billing errors` alerts at 14:51:30 on a " +
			"service nothing has asserted yet: its WATCHES edge is kept unattached, and attaches when the " +
			"billing log source is watched from 15:00.",
		monitors: []twinMonitor{
			{id: 7, name: "checkout errors", kind: "log alert", priority: 2,
				query: `logs("service:checkout env:production status:error").index("*").rollup("count").last("5m") > 10`,
				tags:  []string{"env:production", "team:payments"}, groups: map[string]group{"*": ok()}},
			{id: 9, name: "checkout latency", kind: "metric alert", priority: 3,
				query: `avg(last_1m):avg:trace.http.request.duration{env:production,service:checkout} > 0.5`,
				tags:  []string{"env:production", "team:payments"}, groups: map[string]group{"*": ok()}},
			{id: 11, name: "search heartbeat", kind: "metric alert", priority: 4,
				query: `avg(last_5m):sum:search.heartbeat{env:production,service:search} < 1`,
				tags:  []string{"env:production", "team:search"}, groups: map[string]group{"*": ok()}},
			{id: 12, name: "billing errors", kind: "log alert", priority: 2,
				query: `logs("service:billing env:production status:error").index("*").rollup("count").last("5m") > 5`,
				tags:  []string{"env:production", "team:payments"}, groups: map[string]group{"*": ok()}},
		},
		steps: []step{
			{at: hm(14, 20), discovery: []string{"production/checkout", "production/search"}},
			{at: hms(14, 20, 20)},
			{at: hms(14, 33, 0), changes: map[int64]map[string]group{7: {"*": {Status: "Alert", Triggered: hm(14, 32).Unix()}}}},
			{at: hms(14, 40, 20), changes: map[int64]map[string]group{9: {"*": {Status: "Alert", Triggered: hm(14, 40).Unix()}}}},
			{at: hms(14, 40, 40), changes: map[int64]map[string]group{9: {"*": {Status: "OK", Triggered: hm(14, 40).Unix(), Resolved: hms(14, 40, 30).Unix()}}}},
			{at: hms(14, 41, 0), changes: map[int64]map[string]group{9: {"*": {Status: "Alert", Triggered: hms(14, 40, 50).Unix(), Resolved: hms(14, 40, 30).Unix()}}}},
			{at: hms(14, 45, 20), changes: map[int64]map[string]group{11: {"*": {Status: "No Data", NoData: hm(14, 45).Unix()}}}},
			{at: hms(14, 50, 20), changes: map[int64]map[string]group{9: {"*": {Status: "OK", Triggered: hms(14, 40, 50).Unix(), Resolved: hm(14, 50).Unix()}}}},
			{at: hms(14, 52, 0), changes: map[int64]map[string]group{12: {"*": {Status: "Alert", Triggered: hms(14, 51, 30).Unix()}}}},
			{at: hms(14, 55, 20), changes: map[int64]map[string]group{11: {"*": {Status: "OK", NoData: hm(14, 45).Unix(), Resolved: hm(14, 55).Unix()}}}},
			{at: hm(15, 0), discovery: []string{"production/checkout", "production/search", "production/billing"}},
			{at: hms(15, 11, 0), changes: map[int64]map[string]group{7: {"*": {Status: "OK", Triggered: hm(14, 32).Unix(), Resolved: hm(15, 10).Unix()}}}},
		},
		end: hm(15, 30),
		queries: `
queries:
  # The alert while it was open: the query that fails if the alert is dated at the poll (14:33)
  # rather than at Datadog's 14:32, or its recovery at the poll (15:11) rather than 15:10.
  - name: checkout-errors-asof-1432
    kind: subgraph
    focus: datadog.monitor=7
    valid_at: 2026-09-21T14:32:00Z
    observed_at: 2026-09-21T15:30:00Z
    hops: 1
    direction: both
  - name: checkout-errors-asof-1509
    kind: subgraph
    focus: datadog.monitor=7
    valid_at: 2026-09-21T15:09:59Z
    observed_at: 2026-09-21T15:30:00Z
    hops: 1
    direction: both
  - name: checkout-errors-asof-1510
    kind: subgraph
    focus: datadog.monitor=7
    valid_at: 2026-09-21T15:10:00Z
    observed_at: 2026-09-21T15:30:00Z
    hops: 1
    direction: both
  # The flapping monitor at 14:45: still alerting from 14:40:00, because the 14:40:30 recovery and
  # the 14:40:50 re-alert were flapping and are stated, not applied.
  - name: checkout-latency-asof-1445
    kind: subgraph
    focus: datadog.monitor=9
    valid_at: 2026-09-21T14:45:00Z
    observed_at: 2026-09-21T15:30:00Z
    hops: 1
    direction: both
  # The no-data monitor never alerted.
  - name: search-heartbeat-asof-1450
    kind: subgraph
    focus: datadog.monitor=11
    valid_at: 2026-09-21T14:50:00Z
    observed_at: 2026-09-21T15:30:00Z
    hops: 1
    direction: both
  # The billing alert watches a service asserted only from 15:00: at 14:55 the edge's target is
  # unknown, and by 15:30 it is attached to the billing log source.
  - name: billing-errors-known-at-1455
    kind: subgraph
    focus: datadog.monitor=12
    valid_at: 2026-09-21T15:30:00Z
    observed_at: 2026-09-21T14:55:00Z
    hops: 1
    direction: both
  - name: billing-errors-asof-1530
    kind: subgraph
    focus: datadog.monitor=12
    valid_at: 2026-09-21T15:30:00Z
    observed_at: 2026-09-21T15:30:00Z
    hops: 1
    direction: both
`,
	}
}

// ---- datadog-grouped-monitor-01 (T060) --------------------------------------------------------------

func groupedFixture() monitorFixture {
	checkout, payments, search := "env:production,service:checkout", "env:production,service:payments", "env:production,service:search"
	return monitorFixture{
		dir:    "fixtures/datadog-grouped-monitor-01",
		family: "datadog-monitors",
		description: "One monitor grouped by service alerts on two groups at different instants — checkout at " +
			"14:32, payments at 14:41 — while search stays OK. That is two alerts, each its group's entity " +
			"(`datadog.monitor=21#<group>`) watching its own service, and no monitor-level alert: the " +
			"monitor is never reported alerting because one of its groups is (FR-022, SC-004).",
		monitors: []twinMonitor{
			{id: 21, name: "errors by service", kind: "log alert", priority: 2,
				query:  `logs("env:production status:error").index("*").rollup("count").by("service").last("5m") > 10`,
				tags:   []string{"env:production", "team:platform"},
				groups: map[string]group{checkout: ok(), payments: ok(), search: ok()}},
		},
		steps: []step{
			{at: hm(14, 20), discovery: []string{"production/checkout", "production/payments", "production/search"}},
			{at: hms(14, 20, 20)},
			{at: hms(14, 33, 0), changes: map[int64]map[string]group{21: {checkout: {Status: "Alert", Triggered: hm(14, 32).Unix()}}}},
			{at: hms(14, 41, 20), changes: map[int64]map[string]group{21: {payments: {Status: "Alert", Triggered: hm(14, 41).Unix()}}}},
		},
		end: hm(15, 0),
		queries: `
queries:
  # The monitor: one ALERT node with its query and link, and no state of its own.
  - name: errors-by-service-monitor
    kind: subgraph
    focus: datadog.monitor=21
    valid_at: 2026-09-21T15:00:00Z
    observed_at: 2026-09-21T15:00:00Z
    hops: 1
    direction: both
  # Each alerting group is its own alert, watching its own service.
  - name: errors-by-service-checkout
    kind: subgraph
    focus: datadog.monitor=21#` + checkout + `
    valid_at: 2026-09-21T15:00:00Z
    observed_at: 2026-09-21T15:00:00Z
    hops: 1
    direction: both
  - name: errors-by-service-payments-asof-1440
    kind: subgraph
    focus: datadog.monitor=21#` + payments + `
    valid_at: 2026-09-21T14:40:00Z
    observed_at: 2026-09-21T15:00:00Z
    hops: 1
    direction: both
    expect_empty: >-
      the payments group is an alert only from its first transition at 14:41; before it, the
      monitor has no alert of that group's, and a group-level alert conjured from the monitor would be the
      monitor reported alerting because one group is (FR-022)
  - name: errors-by-service-payments
    kind: subgraph
    focus: datadog.monitor=21#` + payments + `
    valid_at: 2026-09-21T15:00:00Z
    observed_at: 2026-09-21T15:00:00Z
    hops: 1
    direction: both
`,
	}
}

// ---- datadog-doorbell-forged-01 (T061) --------------------------------------------------------------

func doorbellFixture() monitorFixture {
	alert := group{Status: "Alert", Triggered: hm(14, 32).Unix()}
	return monitorFixture{
		dir:    "fixtures/datadog-doorbell-forged-01",
		family: "datadog-monitors",
		description: "What a doorbell ring costs: at most one extra poll, and nothing else. `checkout errors` " +
			"alerts at 14:32. The doorbell-triggered poll at 14:32:05 and the scheduled poll at 14:32:20 both " +
			"read it, and a forged or replayed ring that slipped under the rate limit would add the poll at " +
			"14:32:10: three reads of one transition, and the graph ends with exactly one transition, dated " +
			"14:32 (SC-003, SC-020). The ring's body reaches nothing (pkg/feeder/doorbell never reads it) and " +
			"a forged secret is refused before the bell rings (internal/feeders/datadog poller_test.go); what " +
			"this fixture proves is the other half, that an extra poll creates, alters and retracts nothing.",
		monitors: []twinMonitor{
			{id: 7, name: "checkout errors", kind: "log alert", priority: 2,
				query: `logs("service:checkout env:production status:error").index("*").rollup("count").last("5m") > 10`,
				tags:  []string{"env:production", "team:payments"}, groups: map[string]group{"*": ok()}},
		},
		steps: []step{
			{at: hm(14, 20), discovery: []string{"production/checkout"}},
			{at: hms(14, 20, 20)},
			{at: hms(14, 32, 5), changes: map[int64]map[string]group{7: {"*": alert}}},
			{at: hms(14, 32, 10)},
			{at: hms(14, 32, 20)},
		},
		end: hm(14, 45),
		queries: `
queries:
  # One transition and one state, however many polls read it.
  - name: checkout-errors-after-three-reads
    kind: subgraph
    focus: datadog.monitor=7
    valid_at: 2026-09-21T14:45:00Z
    observed_at: 2026-09-21T14:45:00Z
    hops: 1
    direction: both
`,
	}
}

func monitorFixtures() []monitorFixture {
	return []monitorFixture{transitionsFixture(), groupedFixture(), doorbellFixture()}
}

// Three polls of one transition are one event (SC-020).
func TestThreeReadsOfOneTransitionAreOneEvent(t *testing.T) {
	t.Parallel()
	em := run(t, fixtureOptions(), doorbellFixture().payloadsOf(t)...)
	if got := transitions(em); len(got) != 1 {
		t.Fatalf("%d transitions, want 1: %v", len(got), got)
	}
}

func TestGenerateDatadogMonitorFixtures(t *testing.T) {
	if os.Getenv(genFixturesEnv) == "" {
		t.Skipf("set %s=1 to regenerate the Datadog monitor fixtures", genFixturesEnv)
	}
	for _, fx := range monitorFixtures() {
		generateMonitorFixture(t, fx)
	}
}

// The recorded payloads, replayed through the feeder, reproduce the recorded events exactly: the
// fixture tests the code as it is, not as it was when recorded (FR-044).
func TestTheRecordedPayloadsReproduceTheEvents(t *testing.T) {
	t.Parallel()
	for _, fx := range monitorFixtures() {
		dir := filepath.Join(repoRoot(t), fx.dir)
		if _, err := os.Stat(filepath.Join(dir, "events.jsonl")); err != nil {
			t.Fatalf("%s: %v (regenerate with %s=1)", fx.dir, err, genFixturesEnv)
		}
		src, err := source.NewFileSource(dir)
		if err != nil {
			t.Fatalf("%s: %v (regenerate with %s=1)", fx.dir, err, genFixturesEnv)
		}
		f, err := ddfeeder.New(fixtureOptions())
		if err != nil {
			t.Fatal(err)
		}
		em := emit.NewMemoryEmitter(f.Describe())
		if err := f.Run(context.Background(), src, em); err != nil {
			t.Fatal(err)
		}
		var replayed []string
		for _, ev := range em.Events() {
			replayed = append(replayed, ev.GetEventId())
		}
		recorded := recordedEventIDs(t, filepath.Join(dir, "events.jsonl"))
		if strings.Join(replayed, "\n") != strings.Join(recorded, "\n") {
			t.Errorf("%s: the replay emits %d events, the recording holds %d, or in another order", fx.dir, len(replayed), len(recorded))
		}
	}
}

// SC-003: every transition's valid time is an instant Datadog stated in the payload that carried it,
// never a poll instant.
func TestEveryTransitionIsDatedByAStatedInstant(t *testing.T) {
	t.Parallel()
	for _, fx := range monitorFixtures() {
		stated := map[int64]bool{}
		polls := map[int64]bool{}
		for _, p := range fx.payloadsOf(t) {
			polls[p.At.Unix()] = true
			if p.Kind != ddfeeder.PayloadMonitors {
				continue
			}
			var page []monitor
			if err := json.Unmarshal(p.Bytes, &page); err != nil {
				t.Fatal(err)
			}
			for _, m := range page {
				for _, g := range m.State.Groups {
					stated[g.Triggered], stated[g.Resolved], stated[g.NoData] = true, true, true
				}
			}
		}
		em := run(t, fixtureOptions(), fx.payloadsOf(t)...)
		got := transitions(em)
		if len(got) == 0 {
			t.Fatalf("%s: no transitions", fx.dir)
		}
		for _, a := range got {
			at := a.GetTransitionAt().AsTime().Unix()
			if !stated[at] || polls[at] {
				t.Errorf("%s: transition at %s is not a stated instant, or is a poll instant", fx.dir, a.GetTransitionAt().AsTime())
			}
		}
	}
}

// SC-022: every listed in-scope monitor is an ALERT within one poll.
func TestEveryListedMonitorIsAnAlertWithinOnePoll(t *testing.T) {
	t.Parallel()
	for _, fx := range monitorFixtures() {
		payloads := fx.payloadsOf(t)
		var firstPoll []feeder.Payload
		for i, p := range payloads {
			if p.Kind == ddfeeder.PayloadMonitors {
				firstPoll = append(firstPoll, payloads[i], payloads[i+1])
				break
			}
		}
		em := run(t, fixtureOptions(), firstPoll...)
		alerts := map[string]bool{}
		for _, ev := range em.Events() {
			if n := ev.GetUpsertNode(); n != nil && n.GetRef().GetNamespace() == ddfeeder.NSMonitor {
				alerts[n.GetRef().GetValue()] = true
			}
		}
		var missing []string
		for _, m := range fx.monitors {
			if id := json.Number(itoa(m.id)).String(); !alerts[id] {
				missing = append(missing, id)
			}
		}
		sort.Strings(missing)
		if len(missing) > 0 {
			t.Errorf("%s: monitors listed and not an ALERT after one poll: %v", fx.dir, missing)
		}
	}
}

func itoa(n int64) string {
	raw, _ := json.Marshal(n)
	return string(raw)
}

func recordedEventIDs(t *testing.T, path string) []string {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var out []string
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1<<20), 1<<24)
	for scanner.Scan() {
		var ev struct {
			EventID string `json:"eventId"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			t.Fatal(err)
		}
		out = append(out, ev.EventID)
	}
	return out
}

// arrivalClock hands out observed time on each payload's arrival, a microsecond apart within one.
type arrivalClock struct {
	base time.Time
	step int64
	last time.Time
}

func (c *arrivalClock) Now() time.Time {
	at := c.base.Add(time.Duration(c.step) * time.Microsecond)
	c.step++
	if !c.last.IsZero() && !at.After(c.last) {
		at = c.last.Add(time.Microsecond)
	}
	c.last = at
	return at
}

func (c *arrivalClock) wrap(src feeder.Source) feeder.Source {
	return clockedSource{src: src, clock: c}
}

type clockedSource struct {
	src   feeder.Source
	clock *arrivalClock
}

func (s clockedSource) Next(ctx context.Context) (feeder.Payload, error) {
	p, err := s.src.Next(ctx)
	if err == nil {
		s.clock.base, s.clock.step = p.At, 0
	}
	return p, err
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test")
		}
		dir = parent
	}
}
