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

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/catalog"
	"github.com/Pierre-Theophile/aisre/internal/eval/attribution"
	"github.com/Pierre-Theophile/aisre/internal/fixture"
	"github.com/Pierre-Theophile/aisre/internal/graph"
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
	c, err := attribution.Load(dir)
	if err != nil {
		t.Fatalf("case: %v", err)
	}
	m, decisions := c.Manifest, c.Recorded
	p, decided, err := attribution.Replay(ctx, pgtest.Open(t), c)
	if err != nil {
		t.Fatal(err)
	}

	if os.Getenv("SRE_AGENT_GEN_FIXTURES") == "1" {
		writeEvents(t, m.EventsPath(), c.Feed, decided)
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
	for _, label := range c.Labels {
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
