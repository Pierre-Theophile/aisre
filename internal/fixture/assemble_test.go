// SPDX-License-Identifier: Apache-2.0

package fixture_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/fixture"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/record"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/source"
)

// `fixture assemble` (T082): three recording directories become one fixture.
//
// The scenario is the live run's feeder-gap shape reduced to its bones — a Kubernetes feeder
// recorded twice because it was restarted, and a telemetry feeder recorded once alongside — so
// the test exercises the two rules that make the merge non-trivial: payload files restart at
// 000001 in every run and must be renumbered without losing the index's reference to them, and
// `appendedSeq` is the log's counter and must come out dense and ascending over the observed-time
// order of the merged stream.

// recordingRun is one synthetic `--record` directory.
type recordingRun struct {
	dir      string
	sourceID string
	kind     string
	payloads []source.IndexEntry
	events   []string
}

func writeRun(t *testing.T, root string, run recordingRun) string {
	t.Helper()
	dir := filepath.Join(root, run.dir)
	payloads := filepath.Join(dir, source.PayloadsDir)

	var index strings.Builder
	for _, entry := range run.payloads {
		path := filepath.Join(payloads, filepath.FromSlash(entry.File))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		// The body names the run and the file, so a payload copied to the wrong place is
		// visible in the assertion rather than merely absent.
		body := fmt.Sprintf(`{"run":%q,"was":%q}`, run.dir, entry.File)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write payload: %v", err)
		}
		line, err := json.Marshal(entry)
		if err != nil {
			t.Fatalf("encode index entry: %v", err)
		}
		index.Write(line)
		index.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(payloads, source.IndexFile), []byte(index.String()), 0o600); err != nil {
		t.Fatalf("write index: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, record.EventsFile),
		[]byte(strings.Join(run.events, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write events: %v", err)
	}
	if err := record.WriteManifest(dir, record.Manifest{
		ID: run.dir,
		Sources: []record.ManifestSource{{
			SourceID:         run.sourceID,
			Kind:             run.kind,
			Ordering:         feeder.OrderingNone.String(),
			ReorderingWindow: 30 * time.Second,
		}},
	}); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return dir
}

// event renders one recorded event line in the recorder's canonical spelling: sorted keys, so
// `appendedSeq` comes first, which is what the renumbering rewrites in place.
func event(seq int, observedAt, sourceID, id string) string {
	return fmt.Sprintf(
		`{"appendedSeq":%d,"eventId":%q,"idempotencyKey":%q,"observedAt":%q,"schemaVersion":"1.0.0","sourceId":%q}`,
		seq, id, id, observedAt, sourceID)
}

func TestAssembleMergesRecordings(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	base := "2026-09-16T10:00:0"

	k8sFirst := writeRun(t, root, recordingRun{
		dir: "k8s", sourceID: "k8s:kind", kind: "k8s",
		payloads: []source.IndexEntry{
			{Kind: "services", At: at(t, base+"1Z"), Seq: 1, File: "services/000001.json"},
			{Kind: "deployments", At: at(t, base+"3Z"), Seq: 2, File: "deployments/000001.json"},
		},
		events: []string{
			event(1, base+"1Z", "k8s:kind", "k8s:kind:svc:shop/checkout"),
			event(2, base+"3Z", "k8s:kind", "k8s:kind:deploy:shop/checkout"),
		},
	})
	// The restart: the same feeder, a second directory, and payload numbering that starts over.
	k8sSecond := writeRun(t, root, recordingRun{
		dir: "k8s2", sourceID: "k8s:kind", kind: "k8s",
		payloads: []source.IndexEntry{
			{Kind: "services", At: at(t, base+"7Z"), Seq: 3, File: "services/000001.json"},
		},
		events: []string{event(1, base+"7Z", "k8s:kind", "k8s:kind:svc:shop/checkout@rv2")},
	})
	otel := writeRun(t, root, recordingRun{
		dir: "otel", sourceID: "otel:kind", kind: "otel",
		payloads: []source.IndexEntry{
			{Kind: "traces", At: at(t, base+"2Z"), File: "traces/000001.pb"},
		},
		events: []string{event(1, base+"2Z", "otel:kind", "otel:kind:svc:checkout@w1000")},
	})

	out := filepath.Join(root, "feeder-gap-99")
	report, err := fixture.Assemble([]string{k8sFirst, k8sSecond, otel}, out,
		fixture.AssembleOptions{Family: "feeder-gap", Description: "three runs, one fixture"})
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	if report.FixtureID != "feeder-gap-99" {
		t.Errorf("fixture id = %q, want the base name of --out", report.FixtureID)
	}
	if report.Events != 4 || report.Payloads != 4 {
		t.Errorf("events = %d, payloads = %d; want 4 and 4", report.Events, report.Payloads)
	}
	if report.EventsBySource["k8s:kind"] != 3 || report.EventsBySource["otel:kind"] != 1 {
		t.Errorf("events by source = %v, want k8s:kind 3 and otel:kind 1", report.EventsBySource)
	}
	if len(report.Sources) != 2 {
		t.Errorf("sources = %v, want the two feeders once each", report.Sources)
	}

	// The two runs of the Kubernetes feeder both had a services/000001.json; the second must
	// have become services/000002.json, and the index must point at it.
	assertFileBody(t, filepath.Join(out, "payloads", "services", "000001.json"), `"run":"k8s"`)
	assertFileBody(t, filepath.Join(out, "payloads", "services", "000002.json"), `"run":"k8s2"`)

	index := readLinesT(t, filepath.Join(out, "payloads", source.IndexFile))
	if len(index) != 4 {
		t.Fatalf("merged index has %d lines, want 4", len(index))
	}
	var last time.Time
	for i, line := range index {
		var entry source.IndexEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("index line %d: %v", i+1, err)
		}
		if entry.At.Before(last) {
			t.Errorf("index line %d is out of arrival order: %s before %s", i+1, entry.At, last)
		}
		last = entry.At
		if _, err := os.Stat(filepath.Join(out, "payloads", filepath.FromSlash(entry.File))); err != nil {
			t.Errorf("index line %d names %s, which is not on disk", i+1, entry.File)
		}
	}

	// Events: observed-time order, appendedSeq dense and ascending, ids untouched.
	wantIDs := []string{
		"k8s:kind:svc:shop/checkout",
		"otel:kind:svc:checkout@w1000",
		"k8s:kind:deploy:shop/checkout",
		"k8s:kind:svc:shop/checkout@rv2",
	}
	events := readLinesT(t, filepath.Join(out, "events.jsonl"))
	if len(events) != len(wantIDs) {
		t.Fatalf("merged stream has %d events, want %d", len(events), len(wantIDs))
	}
	for i, line := range events {
		var got struct {
			AppendedSeq int64  `json:"appendedSeq"`
			EventID     string `json:"eventId"`
		}
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatalf("event line %d: %v", i+1, err)
		}
		if got.AppendedSeq != int64(i+1) {
			t.Errorf("event line %d has appendedSeq %d, want %d", i+1, got.AppendedSeq, i+1)
		}
		if got.EventID != wantIDs[i] {
			t.Errorf("event line %d is %s, want %s", i+1, got.EventID, wantIDs[i])
		}
	}

	// The manifest the merge wrote is the one `fixture verify` will load.
	m, err := fixture.LoadManifest(out)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if len(m.Sources) != 2 {
		t.Errorf("manifest declares %d sources, want 2", len(m.Sources))
	}
	if m.Clock.Start.IsZero() || m.Clock.End.IsZero() || !m.Clock.Start.Before(m.Clock.End) {
		t.Errorf("manifest clock = [%s, %s], want the payload arrival range", m.Clock.Start, m.Clock.End)
	}
	if m.Family != "feeder-gap" {
		t.Errorf("family = %q, want feeder-gap", m.Family)
	}
}

// TestAssembleRefusesAmbiguousPayloadKinds keeps the merge from guessing: two feeders producing
// one payload kind would give an index nobody can attribute.
func TestAssembleRefusesAmbiguousPayloadKinds(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	base := "2026-09-16T10:00:0"

	one := writeRun(t, root, recordingRun{
		dir: "a", sourceID: "k8s:one", kind: "k8s",
		payloads: []source.IndexEntry{{Kind: "services", At: at(t, base+"1Z"), File: "services/000001.json"}},
		events:   []string{event(1, base+"1Z", "k8s:one", "k8s:one:svc:a")},
	})
	two := writeRun(t, root, recordingRun{
		dir: "b", sourceID: "k8s:two", kind: "k8s",
		payloads: []source.IndexEntry{{Kind: "services", At: at(t, base+"2Z"), File: "services/000001.json"}},
		events:   []string{event(1, base+"2Z", "k8s:two", "k8s:two:svc:b")},
	})

	_, err := fixture.Assemble([]string{one, two}, filepath.Join(root, "out"), fixture.AssembleOptions{})
	if err == nil {
		t.Fatal("two feeders sharing a payload kind were merged silently")
	}
	if !strings.Contains(err.Error(), "services") {
		t.Errorf("the refusal does not name the kind: %v", err)
	}
}

func at(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		t.Fatalf("parse %q: %v", value, err)
	}
	return parsed
}

func assertFileBody(t *testing.T, path, want string) {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // a path this test just wrote
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !strings.Contains(string(raw), want) {
		t.Errorf("%s holds %s, want it to contain %s", path, raw, want)
	}
}

func readLinesT(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // a path this test just wrote
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}
