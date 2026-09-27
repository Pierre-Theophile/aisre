// SPDX-License-Identifier: Apache-2.0

package datadog_test

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	ddfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/datadog"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/emit"
)

func tick(t *testing.T, at time.Time, ms ...ddfeeder.SourceMeasurement) feeder.Payload {
	t.Helper()
	var sources []string
	for _, m := range ms {
		sources = append(sources, m.Source)
	}
	raw, err := json.Marshal(ddfeeder.DiscoveryTick{LogSources: sources,
		Window: &ddfeeder.DiscoveryWindow{From: at.Add(-time.Hour), To: at}, Measurements: ms})
	if err != nil {
		t.Fatal(err)
	}
	return feeder.Payload{Kind: ddfeeder.PayloadDiscovery, At: at, Bytes: raw}
}

func measured(source string, counts map[string][2]int64) ddfeeder.SourceMeasurement {
	m := ddfeeder.SourceMeasurement{Source: source, Lines: 1000, ErrorLines: 10, HostLines: 1000}
	for label, n := range counts {
		m.Candidates = append(m.Candidates, ddfeeder.CandidateCount{Label: label, Lines: n[0], ErrorLines: n[1]})
	}
	return m
}

func serviceNodes(em *emit.MemoryEmitter) []*graphv1.EventEnvelope {
	var out []*graphv1.EventEnvelope
	for _, ev := range em.Events() {
		if n := ev.GetUpsertNode(); n != nil && n.GetRef().GetNamespace() == ddfeeder.NSService {
			out = append(out, ev)
		}
	}
	return out
}

// The accepted candidate becomes the pointer's version join key, spelled as the selector grammar spells
// it; an unchanged verdict is an unchanged id; a changed one is a new version dated from its tick.
func TestTheVerdictIsThePointersVersionJoinKey(t *testing.T) {
	t.Parallel()
	stamped := measured("production/checkout", map[string][2]int64{"service.version (attribute)": {1000, 10}})
	lost := measured("production/checkout", map[string][2]int64{"service.version (attribute)": {3, 0}})
	em := run(t, ddfeeder.Options{},
		tick(t, at(0), stamped), tick(t, at(60), stamped), tick(t, at(120), lost))
	nodes := serviceNodes(em)
	// The second tick's unchanged verdict re-sent the first id, a duplicate the graph does not keep.
	if len(nodes) != 2 {
		t.Fatalf("%d assertions kept, want 2 (the second tick's is a duplicate)", len(nodes))
	}
	first, changed := nodes[0], nodes[1]
	p := first.GetUpsertNode().GetPointers()[0]
	if p.GetVocabulary() != feeder.VocabDatadogLogs || p.GetSelector() != "service:checkout env:production" ||
		p.GetJoinKeys()["version"] != "@service.version" || p.GetJoinKeys()["host"] != "host" ||
		p.GetAttributes()[feeder.AttrServiceName] != "checkout" {
		t.Errorf("pointer %v", p)
	}
	if !first.GetUpsertNode().GetValidFromUnknown() {
		t.Errorf("the first assertion claims a start it cannot know")
	}
	c := changed.GetUpsertNode()
	if changed.GetEventId() == first.GetEventId() || !c.GetValidAt().AsTime().Equal(at(120)) {
		t.Errorf("the changed verdict: id %s, valid %v", changed.GetEventId(), c.GetValidAt())
	}
	if _, ok := c.GetPointers()[0].GetJoinKeys()["version"]; ok {
		t.Errorf("a lost stamp kept its join key")
	}
	if !strings.Contains(c.GetProps().GetFields()[ddfeeder.PropVersionVerdict].GetStringValue(), "below the line share") {
		t.Errorf("verdict %v", c.GetProps())
	}
	if !strings.Contains(checkpointNotes(em), "0.3%") {
		t.Errorf("the shares are not in the checkpoint:\n%s", checkpointNotes(em))
	}
}

// An operator override is the only candidate, recorded as such.
func TestAnOverrideIsRecordedAsTheOperators(t *testing.T) {
	t.Parallel()
	em := run(t, ddfeeder.Options{VersionOverrides: map[string]string{"production/checkout": "build_id"}},
		tick(t, at(0), measured("production/checkout", map[string][2]int64{"build_id (tag)": {1000, 10}})))
	n := serviceNodes(em)[0].GetUpsertNode()
	if n.GetPointers()[0].GetJoinKeys()["version"] != "build_id" ||
		n.GetProps().GetFields()[ddfeeder.PropVersionSource].GetStringValue() != "operator" {
		t.Errorf("node %v", n)
	}
}

// A measurement that failed leaves the pointer as last asserted and says so.
func TestAFailedMeasurementIsStated(t *testing.T) {
	t.Parallel()
	failed := ddfeeder.SourceMeasurement{Source: "production/checkout", Failed: "datadog answered 429"}
	em := run(t, ddfeeder.Options{}, tick(t, at(0), failed))
	for _, ev := range serviceNodes(em) {
		if len(ev.GetUpsertNode().GetPointers()) > 0 {
			t.Errorf("a pointer was minted from a failed measurement")
		}
	}
	if !strings.Contains(checkpointNotes(em), "not measured (datadog answered 429)") {
		t.Errorf("notes:\n%s", checkpointNotes(em))
	}
}

// FR-037: no Datadog pointer in any recorded fixture names the organisation, a site host or a
// credential — the selector is a query, and where to send it is configuration.
func TestNoSelectorCarriesAnOrganisationSiteOrCredential(t *testing.T) {
	t.Parallel()
	dirs, err := filepath.Glob(filepath.Join(repoRoot(t), "fixtures", "datadog-*", "events.jsonl"))
	if err != nil || len(dirs) == 0 {
		t.Fatalf("no datadog fixtures: %v", err)
	}
	checked := 0
	for _, path := range dirs {
		file, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 1<<20), 1<<24)
		for scanner.Scan() {
			var env graphv1.EventEnvelope
			if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(scanner.Bytes(), &env); err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			for _, p := range env.GetUpsertNode().GetPointers() {
				if p.GetBackendKind() != ddfeeder.Kind || p.GetKind() == graphv1.PointerKind_SOURCE_LINK {
					continue
				}
				checked++
				s := strings.ToLower(p.GetSelector())
				for _, bad := range []string{"datadoghq", "ddog-gov", "api_key", "application_key", "dd-api", "twin"} {
					if strings.Contains(s, bad) {
						t.Errorf("%s: selector %q contains %q", path, p.GetSelector(), bad)
					}
				}
			}
		}
		_ = file.Close()
	}
	if checked == 0 {
		t.Fatal("no Datadog pointer was checked")
	}
}
