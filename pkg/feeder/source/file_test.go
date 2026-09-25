// SPDX-License-Identifier: Apache-2.0

package source_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/source"
)

// writeFixture lays out a payloads directory, optionally with an index.
func writeFixture(t *testing.T, entries []source.IndexEntry, files map[string][]byte, withIndex bool) string {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, source.PayloadsDir)
	for name, body := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if withIndex {
		var buf []byte
		for _, entry := range entries {
			line, err := json.Marshal(entry)
			if err != nil {
				t.Fatalf("marshal index: %v", err)
			}
			buf = append(append(buf, line...), '\n')
		}
		if err := os.WriteFile(filepath.Join(root, source.IndexFile), buf, 0o600); err != nil {
			t.Fatalf("write index: %v", err)
		}
	}
	return dir
}

func TestFileSourceReplaysInIndexOrder(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 9, 1, 13, 0, 0, 0, time.UTC)
	// The index order deliberately interleaves the two kinds and is not the filename order,
	// so a source that fell back to walking the directories would fail this test.
	entries := []source.IndexEntry{
		{Kind: "deployments", At: base, Seq: 1001, File: "deployments/000001.json"},
		{Kind: "services", At: base.Add(time.Second), Seq: 1002, File: "services/000001.json"},
		{Kind: "deployments", At: base.Add(2 * time.Second), Seq: 1003, File: "deployments/000002.json"},
	}
	dir := writeFixture(t, entries, map[string][]byte{
		"deployments/000001.json": []byte(`{"name":"checkout"}`),
		"deployments/000002.json": []byte(`{"name":"payments"}`),
		"services/000001.json":    []byte(`{"name":"checkout-svc"}`),
	}, true)

	src, err := source.NewFileSource(dir)
	if err != nil {
		t.Fatalf("NewFileSource: %v", err)
	}
	if src.Len() != 3 {
		t.Fatalf("read %d payloads, want 3", src.Len())
	}

	got := drain(t, src)
	want := []struct {
		kind string
		seq  int64
		body string
	}{
		{"deployments", 1001, `{"name":"checkout"}`},
		{"services", 1002, `{"name":"checkout-svc"}`},
		{"deployments", 1003, `{"name":"payments"}`},
	}
	if len(got) != len(want) {
		t.Fatalf("drained %d payloads, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Kind != w.kind || got[i].Seq != w.seq || string(got[i].Bytes) != w.body {
			t.Errorf("payload %d = %+v, want kind %s seq %d body %s", i, got[i], w.kind, w.seq, w.body)
		}
		if !got[i].At.Equal(entries[i].At) {
			t.Errorf("payload %d arrival time = %s, want %s", i, got[i].At, entries[i].At)
		}
	}
}

func TestFileSourceFallsBackToFilenameOrder(t *testing.T) {
	t.Parallel()
	dir := writeFixture(t, nil, map[string][]byte{
		"deployments/000002.json": []byte(`{"n":2}`),
		"deployments/000001.json": []byte(`{"n":1}`),
		"deployments/000010.json": []byte(`{"n":10}`),
		"aaa/000001.json":         []byte(`{"n":"a"}`),
	}, false)

	src, err := source.NewFileSource(dir)
	if err != nil {
		t.Fatalf("NewFileSource: %v", err)
	}
	got := drain(t, src)
	// Kinds in directory-name order, files zero-padded so lexical order is numeric order.
	want := []string{`{"n":"a"}`, `{"n":1}`, `{"n":2}`, `{"n":10}`}
	if len(got) != len(want) {
		t.Fatalf("drained %d payloads, want %d", len(got), len(want))
	}
	for i, body := range want {
		if string(got[i].Bytes) != body {
			t.Errorf("payload %d = %s, want %s", i, got[i].Bytes, body)
		}
	}
}

func TestFileSourceExpandsJSONLWithoutAnIndex(t *testing.T) {
	t.Parallel()
	dir := writeFixture(t, nil, map[string][]byte{
		"watch/events.jsonl": []byte("{\"a\":1}\n\n{\"a\":2}\n"),
	}, false)

	src, err := source.NewFileSource(dir)
	if err != nil {
		t.Fatalf("NewFileSource: %v", err)
	}
	got := drain(t, src)
	if len(got) != 2 {
		t.Fatalf("drained %d payloads from a two-line jsonl, want 2", len(got))
	}
	if string(got[0].Bytes) != `{"a":1}` || string(got[1].Bytes) != `{"a":2}` {
		t.Errorf("payloads = %s / %s", got[0].Bytes, got[1].Bytes)
	}
}

func TestFileSourceWithKinds(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 9, 1, 13, 0, 0, 0, time.UTC)
	dir := writeFixture(t, []source.IndexEntry{
		{Kind: "deployments", At: base, File: "deployments/000001.json"},
		{Kind: "services", At: base, File: "services/000001.json"},
	}, map[string][]byte{
		"deployments/000001.json": []byte(`{"k":"d"}`),
		"services/000001.json":    []byte(`{"k":"s"}`),
	}, true)

	src, err := source.NewFileSource(dir, source.WithKinds("services"))
	if err != nil {
		t.Fatalf("NewFileSource: %v", err)
	}
	got := drain(t, src)
	if len(got) != 1 || got[0].Kind != "services" {
		t.Fatalf("WithKinds did not restrict the replay: %+v", got)
	}
}

func TestFileSourceResetAndPayloads(t *testing.T) {
	t.Parallel()
	dir := writeFixture(t, nil, map[string][]byte{"k/000001.json": []byte(`{}`)}, false)
	src, err := source.NewFileSource(dir)
	if err != nil {
		t.Fatalf("NewFileSource: %v", err)
	}
	if len(src.Payloads()) != 1 {
		t.Fatalf("Payloads() = %d, want 1", len(src.Payloads()))
	}
	drain(t, src)
	src.Reset()
	if len(drain(t, src)) != 1 {
		t.Error("Reset did not rewind the source")
	}
}

func TestFileSourceMissingDirectory(t *testing.T) {
	t.Parallel()
	if _, err := source.NewFileSource(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Error("a missing payload directory was accepted")
	}
}

func TestSliceSource(t *testing.T) {
	t.Parallel()
	src := source.NewSliceSource([]feeder.Payload{{Kind: "a"}, {Kind: "b"}})
	got := drain(t, src)
	if len(got) != 2 || got[0].Kind != "a" || got[1].Kind != "b" {
		t.Fatalf("SliceSource replayed %+v", got)
	}
	src.Reset()
	if len(drain(t, src)) != 2 {
		t.Error("Reset did not rewind")
	}
}

func TestChanSource(t *testing.T) {
	t.Parallel()
	src := source.NewChanSource(2)
	ctx := t.Context()

	if err := src.Push(ctx, feeder.Payload{Kind: "watch", Bytes: []byte(`{"a":1}`)}); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if err := src.Push(ctx, feeder.Payload{Kind: "watch", Bytes: []byte(`{"a":2}`)}); err != nil {
		t.Fatalf("Push: %v", err)
	}
	src.Close()

	// Closing must not discard what was already pushed.
	got := drain(t, src)
	if len(got) != 2 {
		t.Fatalf("drained %d payloads after Close, want the 2 already pushed", len(got))
	}
	if err := src.Push(ctx, feeder.Payload{}); !errors.Is(err, source.ErrSourceClosed) {
		t.Errorf("Push after Close = %v, want ErrSourceClosed", err)
	}
	src.Close() // idempotent
}

func TestChanSourceRespectsContext(t *testing.T) {
	t.Parallel()
	src := source.NewChanSource(0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := src.Next(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Next on a cancelled context = %v, want context.Canceled", err)
	}
}

func drain(t *testing.T, src feeder.Source) []feeder.Payload {
	t.Helper()
	var out []feeder.Payload
	for {
		p, err := src.Next(t.Context())
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		out = append(out, p)
	}
}
