// SPDX-License-Identifier: Apache-2.0

package datadog_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	ddfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/datadog"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/emit"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/source"
)

var t0 = time.Date(2026, 9, 21, 14, 0, 0, 0, time.UTC)

// group is one group state as `group_states=all` reports it.
type group struct {
	Status    string `json:"status"`
	Triggered int64  `json:"last_triggered_ts,omitempty"`
	Resolved  int64  `json:"last_resolved_ts,omitempty"`
	NoData    int64  `json:"last_nodata_ts,omitempty"`
}

type monitor struct {
	ID       int64    `json:"id"`
	Name     string   `json:"name"`
	Type     string   `json:"type"`
	Query    string   `json:"query"`
	Message  string   `json:"message"`
	Tags     []string `json:"tags"`
	Priority *int     `json:"priority,omitempty"`
	Modified string   `json:"modified"`
	State    struct {
		Groups map[string]group `json:"groups"`
	} `json:"state"`
}

func errorsMonitor(groups map[string]group) monitor {
	p := 2
	m := monitor{ID: 7, Name: "checkout errors", Type: "log alert",
		Query:   `logs("service:checkout env:production status:error").index("*").rollup("count").last("5m") > 10`,
		Message: "Errors are up. @oncall-checkout https://runbooks.example/checkout",
		Tags:    []string{"team:payments"}, Priority: &p, Modified: "2026-09-01T09:00:00Z"}
	m.State.Groups = groups
	return m
}

func at(min int) time.Time { return t0.Add(time.Duration(min) * time.Minute) }

func unix(tm time.Time) int64 { return tm.Unix() }

func page(t *testing.T, when time.Time, monitors ...monitor) feeder.Payload {
	t.Helper()
	raw, err := json.Marshal(monitors)
	if err != nil {
		t.Fatal(err)
	}
	return feeder.Payload{Kind: ddfeeder.PayloadMonitors, At: when, Bytes: raw}
}

func poll(when time.Time, outcome string) feeder.Payload {
	raw, _ := json.Marshal(ddfeeder.PollMarker{Outcome: outcome, Pages: 1})
	return feeder.Payload{Kind: ddfeeder.PayloadPoll, At: when, Bytes: raw}
}

func run(t *testing.T, opts ddfeeder.Options, payloads ...feeder.Payload) *emit.MemoryEmitter {
	t.Helper()
	if opts.OrgSlug == "" {
		opts.OrgSlug = "twin"
	}
	f, err := ddfeeder.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	em := emit.NewMemoryEmitter(f.Describe(), emit.WithMemoryClock(func() time.Time { return at(120) }))
	if err := f.Run(context.Background(), source.NewSliceSource(payloads), em); err != nil {
		t.Fatal(err)
	}
	return em
}

func transitions(em *emit.MemoryEmitter) []*graphv1.AlertTransition {
	var out []*graphv1.AlertTransition
	for _, ev := range em.Events() {
		if a := ev.GetAlertTransition(); a != nil {
			out = append(out, a)
		}
	}
	return out
}

// The specification's sequence: OK, then ALERT at 14:32, then OK at 15:10. Two transitions, each dated
// by Datadog's instant rather than the poll that saw it, both sampled at the poll interval, the
// recovery kept (FR-020, FR-025c).
func TestTransitionsAreDatedByDatadogNotByThePoll(t *testing.T) {
	t.Parallel()
	alertAt, okAt := at(32), at(70)
	em := run(t, ddfeeder.Options{},
		page(t, at(20), errorsMonitor(map[string]group{"*": {Status: "OK"}})), poll(at(20), "complete"),
		page(t, at(33), errorsMonitor(map[string]group{"*": {Status: "Alert", Triggered: unix(alertAt)}})), poll(at(33), "complete"),
		page(t, at(33).Add(20*time.Second), errorsMonitor(map[string]group{"*": {Status: "Alert", Triggered: unix(alertAt)}})), poll(at(33).Add(20*time.Second), "complete"),
		page(t, at(71), errorsMonitor(map[string]group{"*": {Status: "OK", Triggered: unix(alertAt), Resolved: unix(okAt)}})), poll(at(71), "complete"),
	)
	got := transitions(em)
	if len(got) != 2 {
		t.Fatalf("%d transitions, want 2: %v", len(got), got)
	}
	want := []struct {
		at       time.Time
		from, to string
	}{{alertAt, "ok", "alert"}, {okAt, "alert", "ok"}}
	for i, w := range want {
		g := got[i]
		if !g.GetTransitionAt().AsTime().Equal(w.at) || g.GetFromState() != w.from || g.GetToState() != w.to {
			t.Errorf("transition %d: %s %s→%s, want %s %s→%s", i, g.GetTransitionAt().AsTime(), g.GetFromState(),
				g.GetToState(), w.at, w.from, w.to)
		}
		if !g.GetSampled() || g.GetSampledIntervalSeconds() != 20 || g.GetTransport() != feeder.TransportPoll {
			t.Errorf("transition %d is not marked sampled at the poll interval: %v", i, g)
		}
		if g.GetGroupKey() != "" || g.GetMonitor().GetValue() != "7" {
			t.Errorf("an ungrouped monitor's transition names a group: %v", g)
		}
		if len(g.GetWatches()) != 1 || g.GetWatches()[0].GetValue() != "production/checkout" {
			t.Errorf("watches %v", g.GetWatches())
		}
	}
}

// A transition that opened and closed between two polls is still delivered: both of Datadog's stated
// instants moved (contract §3).
func TestAnAlertBetweenTwoPollsIsNotLost(t *testing.T) {
	t.Parallel()
	em := run(t, ddfeeder.Options{},
		page(t, at(20), errorsMonitor(map[string]group{"*": {Status: "OK"}})), poll(at(20), "complete"),
		page(t, at(40), errorsMonitor(map[string]group{"*": {Status: "OK", Triggered: unix(at(30)), Resolved: unix(at(35))}})),
		poll(at(40), "complete"),
	)
	got := transitions(em)
	if len(got) != 2 || got[0].GetToState() != "alert" || got[1].GetToState() != "ok" {
		t.Fatalf("transitions %v", got)
	}
}

// A restarted feeder re-derives the same transitions, which are the same ids: nothing new reaches the
// graph (FR-025a; contract §4 "restarts need no memory").
func TestARestartReSendsTheSameIDs(t *testing.T) {
	t.Parallel()
	payloads := []feeder.Payload{
		page(t, at(40), errorsMonitor(map[string]group{"*": {Status: "OK", Triggered: unix(at(30)), Resolved: unix(at(35))}})),
		poll(at(40), "complete"),
	}
	first := run(t, ddfeeder.Options{}, payloads...)
	second := run(t, ddfeeder.Options{}, payloads...)
	ids := func(em *emit.MemoryEmitter) string {
		var out []string
		for _, ev := range em.Events() {
			if ev.GetAlertTransition() != nil || ev.GetUpsertNode() != nil {
				out = append(out, ev.GetEventId())
			}
		}
		return strings.Join(out, ",")
	}
	if ids(first) != ids(second) || ids(first) == "" {
		t.Fatalf("a restart minted different ids:\n%s\n%s", ids(first), ids(second))
	}
}

// A grouped monitor alerting on two groups gives two alerts, each its group's, and never a
// monitor-level one (FR-022, SC-004).
func TestAGroupedMonitorAlertsPerGroup(t *testing.T) {
	t.Parallel()
	m := errorsMonitor(map[string]group{
		"env:production,service:checkout": {Status: "Alert", Triggered: unix(at(32))},
		"env:production,service:payments": {Status: "Alert", Triggered: unix(at(41))},
		"env:production,service:search":   {Status: "OK"},
	})
	m.Query = `logs("env:production status:error").index("*").rollup("count").by("service").last("5m") > 10`
	em := run(t, ddfeeder.Options{}, page(t, at(42), m), poll(at(42), "complete"))
	got := transitions(em)
	if len(got) != 2 {
		t.Fatalf("%d transitions: %v", len(got), got)
	}
	for _, g := range got {
		if g.GetGroupKey() == "" || g.GetMonitor().GetValue() != "7#"+g.GetGroupKey() {
			t.Errorf("a grouped transition is not its group's: %v", g)
		}
		service := strings.TrimPrefix(strings.Split(g.GetGroupKey(), ",")[1], "service:")
		if len(g.GetWatches()) != 1 || g.GetWatches()[0].GetValue() != "production/"+service {
			t.Errorf("group %s watches %v", g.GetGroupKey(), g.GetWatches())
		}
	}
}

// No data is recorded and stated, not triggering; a recovery after it is still delivered.
func TestNoDataIsStatedAndTheRecoveryKept(t *testing.T) {
	t.Parallel()
	em := run(t, ddfeeder.Options{},
		page(t, at(20), errorsMonitor(map[string]group{"*": {Status: "OK"}})), poll(at(20), "complete"),
		page(t, at(40), errorsMonitor(map[string]group{"*": {Status: "Alert", Triggered: unix(at(30))}})), poll(at(40), "complete"),
		page(t, at(60), errorsMonitor(map[string]group{"*": {Status: "No Data", Triggered: unix(at(30)), NoData: unix(at(50))}})), poll(at(60), "complete"),
		page(t, at(80), errorsMonitor(map[string]group{"*": {Status: "OK", Triggered: unix(at(30)), NoData: unix(at(50)), Resolved: unix(at(75))}})), poll(at(80), "complete"),
	)
	got := transitions(em)
	if len(got) != 2 || got[1].GetFromState() != "alert" || got[1].GetToState() != "ok" {
		t.Fatalf("transitions %v", got)
	}
	if !strings.Contains(checkpointNotes(em), "(no_data)") {
		t.Errorf("the no-data spell is not stated:\n%s", checkpointNotes(em))
	}
}

// A status change Datadog states no instant for is not dated from the poll: it is stated.
func TestAnUndatedChangeIsStatedNotGuessed(t *testing.T) {
	t.Parallel()
	em := run(t, ddfeeder.Options{},
		page(t, at(20), errorsMonitor(map[string]group{"*": {Status: "OK"}})), poll(at(20), "complete"),
		page(t, at(40), errorsMonitor(map[string]group{"*": {Status: "Alert"}})), poll(at(40), "complete"),
	)
	if got := transitions(em); len(got) != 0 {
		t.Fatalf("an undated change was emitted: %v", got)
	}
	if !strings.Contains(checkpointNotes(em), "undated transitions") {
		t.Errorf("the undated change is not stated:\n%s", checkpointNotes(em))
	}
}

// Definitions are asserted on change only, and the notification message never reaches the graph.
func TestAMonitorIsAssertedOnChangeOnlyAndItsMessageDropped(t *testing.T) {
	t.Parallel()
	m := errorsMonitor(map[string]group{"*": {Status: "OK"}})
	renamed := m
	renamed.Name, renamed.Modified = "checkout error rate", "2026-09-21T14:25:00Z"
	em := run(t, ddfeeder.Options{Site: "datadoghq.eu"},
		page(t, at(20), m), poll(at(20), "complete"),
		page(t, at(21), m), poll(at(21), "complete"),
		page(t, at(30), renamed), poll(at(30), "complete"),
	)
	var nodes []*graphv1.EventEnvelope
	for _, ev := range em.Events() {
		if ev.GetUpsertNode() != nil {
			nodes = append(nodes, ev)
		}
	}
	distinct := map[string]bool{}
	for _, ev := range nodes {
		distinct[ev.GetEventId()] = true
		raw, _ := json.Marshal(ev)
		if strings.Contains(string(raw), "oncall") || strings.Contains(string(raw), "runbooks") {
			t.Errorf("the notification message reached the graph: %s", raw)
		}
	}
	if len(distinct) != 2 {
		t.Errorf("%d distinct monitor assertions over three polls, one edit: want 2", len(distinct))
	}
	n := nodes[len(nodes)-1].GetUpsertNode()
	if n.GetType() != graphv1.NodeType_ALERT || !n.GetValidAt().AsTime().Equal(at(25)) {
		t.Errorf("the edit is not dated from Datadog's modified instant: %v", n)
	}
	var kinds []graphv1.PointerKind
	for _, p := range n.GetPointers() {
		kinds = append(kinds, p.GetKind())
		if p.GetKind() == graphv1.PointerKind_LOG && (p.GetVocabulary() != feeder.VocabDatadogMonitor ||
			p.GetAttributes()[feeder.AttrDatadogMonitorID] != "7") {
			t.Errorf("query pointer %v", p)
		}
	}
	if len(kinds) != 2 {
		t.Errorf("pointers %v, want the query and the link", kinds)
	}
}

// A monitor absent from a complete poll is retracted with its groups; absent from a partial one it is
// not, and the gap is declared (FR-012, FR-085).
func TestRetractionOnlyOnACompletePoll(t *testing.T) {
	t.Parallel()
	grouped := errorsMonitor(map[string]group{
		"service:checkout,env:production": {Status: "Alert", Triggered: unix(at(10))},
		"service:search,env:production":   {Status: "OK"},
	})
	em := run(t, ddfeeder.Options{},
		page(t, at(20), grouped), poll(at(20), "complete"),
		poll(at(40), "partial"), // the page that would have listed it failed
		poll(at(60), "complete"),
	)
	var retracted []string
	var gaps []bool
	for _, ev := range em.Events() {
		if r := ev.GetRetractNode(); r != nil {
			retracted = append(retracted, r.GetRef().GetValue())
			if !r.GetValidEnd().AsTime().Equal(at(60)) {
				t.Errorf("retracted at %s, want the complete poll at 14:60", r.GetValidEnd().AsTime())
			}
		}
		if c := ev.GetSourceCheckpoint(); c != nil {
			gaps = append(gaps, c.GetGapBefore())
		}
	}
	if strings.Join(retracted, ",") != "7#service:checkout,env:production,7" {
		t.Errorf("retracted %v", retracted)
	}
	if len(gaps) != 3 || gaps[0] || !gaps[1] || gaps[2] {
		t.Errorf("gaps %v, want only the partial poll's", gaps)
	}
}

// The tag filter scopes monitors, and a monitor out of scope emits nothing (FR-025c).
func TestTheTagFilterScopesMonitors(t *testing.T) {
	t.Parallel()
	in := errorsMonitor(map[string]group{"*": {Status: "Alert", Triggered: unix(at(10))}})
	out := in
	out.ID, out.Tags = 8, []string{"team:search"}
	em := run(t, ddfeeder.Options{MonitorTags: []string{"team:payments"}}, page(t, at(20), in, out), poll(at(20), "complete"))
	for _, ev := range em.Events() {
		if n := ev.GetUpsertNode(); n != nil && n.GetRef().GetValue() == "8" {
			t.Errorf("an out-of-scope monitor was asserted")
		}
	}
	if !strings.Contains(checkpointNotes(em), "out of the tag scope and skipped: 1") {
		t.Errorf("the skip is not stated:\n%s", checkpointNotes(em))
	}
}

func checkpointNotes(em *emit.MemoryEmitter) string {
	var out []string
	for _, ev := range em.Events() {
		if c := ev.GetSourceCheckpoint(); c != nil {
			out = append(out, c.GetNote())
		}
	}
	return strings.Join(out, "\n---\n")
}
