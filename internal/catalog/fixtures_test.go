// SPDX-License-Identifier: Apache-2.0

package catalog_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"
	"gopkg.in/yaml.v3"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/catalog"
	"github.com/Pierre-Theophile/aisre/internal/fixture"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	eventlog "github.com/Pierre-Theophile/aisre/internal/log"
	"github.com/Pierre-Theophile/aisre/internal/projector"
	"github.com/Pierre-Theophile/aisre/internal/query"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres/pgtest"
)

// The labelled attribution cases (009 T009, T016, T017, T020, research R13).
//
// A case's events.jsonl holds the attributor's decisions as the log recorded them: a replay never
// runs the attributor. This test runs it anyway, from the case's events without them, and checks
// three things: that it takes exactly the recorded decisions (the recording is what the published
// ruleset decides), that the attributions it ends on are the labels, and that the case's
// catalog.yaml is the catalog its stream declares.
//
// With SRE_AGENT_GEN_FIXTURES=1 it rewrites the decisions in events.jsonl instead.

const casesDir = "../../fixtures/attribution"

type labelsFile struct {
	Labels []struct {
		Signal string   `yaml:"signal"`
		Expect []string `yaml:"expect"`
	} `yaml:"labels"`
}

func TestAttributionCases(t *testing.T) {
	dirs, err := filepath.Glob(filepath.Join(casesDir, "*", "manifest.yaml"))
	if err != nil || len(dirs) == 0 {
		t.Fatalf("no attribution cases under %s: %v", casesDir, err)
	}
	for _, manifest := range dirs {
		dir := filepath.Dir(manifest)
		t.Run(filepath.Base(dir), func(t *testing.T) {
			t.Parallel()
			runCase(t, dir)
		})
	}
}

func runCase(t *testing.T, dir string) {
	ctx := context.Background()
	m, err := fixture.LoadManifest(dir)
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	recorded, err := fixture.ReadEvents(m.EventsPath())
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	var feed, decisions []fixture.Event
	for _, e := range recorded {
		if e.Envelope.GetSourceId() == graph.AttributorSourceID {
			decisions = append(decisions, e)
		} else {
			feed = append(feed, e)
		}
	}
	stream, err := fixture.WithHumanDecisions(m, feed)
	if err != nil {
		t.Fatalf("human decisions: %v", err)
	}

	p := projector.New(pgtest.Open(t))
	sources := []eventlog.Source{{SourceID: fixture.HumanSourceID, Kind: "human", Ordering: "none", SchemaVersion: m.SchemaVersion}}
	for _, src := range m.Sources {
		if src.SourceID == graph.AttributorSourceID || src.SourceID == fixture.HumanSourceID {
			continue
		}
		sources = append(sources, eventlog.Source{SourceID: src.SourceID, Kind: src.Kind, Ordering: src.Ordering,
			ReorderingWindow: src.ReorderingWindow, SchemaVersion: m.SchemaVersion})
	}
	for _, src := range sources {
		if err := p.RegisterSource(ctx, src); err != nil {
			t.Fatalf("register %s: %v", src.SourceID, err)
		}
	}

	// The attributor follows the log as serve runs it, after every event. Its decisions are stamped
	// one reordering window after the event, the widest any source declares: a replay may deliver
	// any feeder event up to its window late (fixture verify's shuffles do), and a decision must
	// never arrive before the signal it is about.
	settle := time.Second
	for _, src := range m.Sources {
		settle = max(settle, src.ReorderingWindow+time.Second)
	}
	var clock time.Time
	a := catalog.New(p, catalog.WithClock(func() time.Time {
		clock = clock.Add(time.Millisecond)
		return clock
	}))
	if err := a.Register(ctx); err != nil {
		t.Fatalf("register the attributor: %v", err)
	}
	for _, e := range stream {
		r, err := p.ApplyWithOptions(ctx, e.Envelope, projector.ApplyOptions{ObservedAt: e.ObservedAt, Principal: e.Principal})
		if err != nil || r.GetStatus() == graphv1.IngestResult_REJECTED {
			t.Fatalf("apply %s: %v %s %s", e.Envelope.GetEventId(), err, r.GetReasonCode(), r.GetReasonDetail())
		}
		if at := e.ObservedAt.Add(settle); at.After(clock) {
			clock = at
		}
		if _, err := a.Sweep(ctx); err != nil {
			t.Fatalf("sweep after %s: %v", e.Envelope.GetEventId(), err)
		}
	}

	var decided []fixture.Event
	if err := p.Log().BySource(ctx, graph.AttributorSourceID, 0, func(r *eventlog.Record) error {
		decided = append(decided, fixture.Event{Envelope: r.Envelope, ObservedAt: r.ObservedAt.UTC()})
		return nil
	}); err != nil {
		t.Fatalf("read decisions: %v", err)
	}

	if os.Getenv("SRE_AGENT_GEN_FIXTURES") == "1" {
		writeEvents(t, m.EventsPath(), feed, decided)
	} else {
		if len(decided) != len(decisions) {
			t.Errorf("the attributor took %d decisions, the recording holds %d; regenerate with SRE_AGENT_GEN_FIXTURES=1",
				len(decided), len(decisions))
		}
		for i := range min(len(decided), len(decisions)) {
			if got, want := decided[i].Envelope.GetEventId(), decisions[i].Envelope.GetEventId(); got != want ||
				!decided[i].ObservedAt.Equal(decisions[i].ObservedAt) {
				t.Errorf("decision %d: the attributor decided %s at %s, the recording holds %s at %s",
					i, got, decided[i].ObservedAt, want, decisions[i].ObservedAt)
			}
		}
	}

	engine := query.NewEngine(p.Store())
	now := &graphv1.AsOf{ValidAt: timestamppb.New(m.Clock.End)}
	var labels labelsFile
	raw, err := os.ReadFile(filepath.Join(dir, "labels.yaml"))
	if err != nil {
		t.Fatalf("labels: %v", err)
	}
	if err := yaml.Unmarshal(raw, &labels); err != nil || len(labels.Labels) == 0 {
		t.Fatalf("labels: %v", err)
	}
	for _, label := range labels.Labels {
		ref, err := graph.ParseRef(label.Signal)
		if err != nil {
			t.Fatalf("label %q: %v", label.Signal, err)
		}
		why, err := engine.WhyAttributed(ctx, &graphv1.WhyAttributedRequest{Signal: ref.Proto(), AsOf: now})
		if err != nil {
			t.Fatalf("why %s: %v", label.Signal, err)
		}
		got := []string{}
		for _, e := range why.GetCurrent().GetEntries() {
			got = append(got, e.GetEntrySlug())
		}
		want := append([]string{}, label.Expect...)
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("%s: attributed to %v, labelled %v", label.Signal, got, want)
		}
	}

	file, err := os.ReadFile(filepath.Join(dir, "catalog.yaml"))
	if err != nil {
		t.Fatalf("catalog file: %v", err)
	}
	parsed, err := catalog.ParseFile(file)
	if err != nil {
		t.Fatalf("catalog file: %v", err)
	}
	current, err := engine.ListEntries(ctx, &graphv1.ListEntriesRequest{AsOf: now})
	if err != nil {
		t.Fatalf("list entries: %v", err)
	}
	if plan := catalog.PlanImport(parsed, current, "check", false); len(plan.Changes) > 0 {
		var changes []string
		for _, c := range plan.Changes {
			changes = append(changes, c.Describe)
		}
		t.Errorf("catalog.yaml is not the catalog the stream declares; importing it would %s", strings.Join(changes, "; "))
	}
}

// writeEvents rewrites events.jsonl: the feed and the decisions in observed order, canonical JSON.
func writeEvents(t *testing.T, path string, feed, decided []fixture.Event) {
	t.Helper()
	all := append(append([]fixture.Event{}, feed...), decided...)
	slices.SortStableFunc(all, func(a, b fixture.Event) int { return a.ObservedAt.Compare(b.ObservedAt) })
	var out bytes.Buffer
	for i, e := range all {
		body, err := protojson.Marshal(e.Envelope)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.UseNumber()
		var fields map[string]any
		if err := dec.Decode(&fields); err != nil {
			t.Fatalf("decode: %v", err)
		}
		fields["appendedSeq"] = i + 1
		fields["observedAt"] = e.ObservedAt.UTC().Format(time.RFC3339Nano)
		enc := json.NewEncoder(&out)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(fields); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}
	if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
