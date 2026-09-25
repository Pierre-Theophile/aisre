// SPDX-License-Identifier: Apache-2.0

package mcpfeeder

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/emit"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/source"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/testkit"

	"github.com/Pierre-Theophile/aisre/examples/mcp-feeder/mockserver"
)

// fixtureDir is the recording every check below is measured against. It was produced by the
// command in ../cmd/mcp-feeder/main.go and its events.jsonl was read line by line by hand.
const fixtureDir = "../testdata/directory-01"

// mockServerEnv makes this test binary run the mock MCP server instead of the tests, so that
// the live check can spawn a real child process over a real stdio transport without a build
// step. It is the standard helper-process pattern.
const mockServerEnv = "MCP_FEEDER_RUN_MOCKSERVER"

// seed is the directory the fixture was recorded from.
const seed = 1

func TestMain(m *testing.M) {
	if os.Getenv(mockServerEnv) == "1" {
		if err := mockserver.New(mockserver.Options{Seed: seed}).Run(context.Background(), &mcp.StdioTransport{}); err != nil {
			fmt.Fprintln(os.Stderr, "mockserver:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// newFeeder builds the connector exactly as the recording was made with it. Every field is
// part of the mapping, so a test that varied one would be testing a different connector.
func newFeeder() *Feeder {
	return &Feeder{
		SourceID:    "mcp:" + mockserver.ServerName,
		ServerName:  mockserver.ServerName,
		Environment: "prod",
	}
}

// TestConformance is the whole of what the SDK asks of a connector: replay the recording and
// compare against events.jsonl, replay it permuted inside the declared reordering window, and
// replay it twice into one graph and require every second delivery to be a no-op.
func TestConformance(t *testing.T) {
	testkit.Conformance(t, newFeeder(), fixtureDir)
}

// TestLiveMatchesRecording spawns the mock server over stdio and asserts the connector
// produces, from the protocol, exactly the payloads and exactly the events that were
// recorded. It is what makes the fixture a recording of something rather than a golden file
// nobody can regenerate.
func TestLiveMatchesRecording(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, os.Args[0])
	cmd.Env = append(os.Environ(), mockServerEnv+"=1")
	cmd.Stderr = os.Stderr

	client, err := DialCommand(ctx, cmd)
	if err != nil {
		t.Fatalf("connect to the mock server: %v", err)
	}
	defer client.Close()

	if got := client.ServerName(); got != mockserver.ServerName {
		t.Errorf("server name = %q, want %q", got, mockserver.ServerName)
	}

	f := newFeeder()
	// The same clock the recording was made with, so that the events compare as whole
	// envelopes rather than with observed time carved out.
	observedAt := time.Date(2026, 9, 16, 9, 5, 0, 0, time.UTC)
	em := emit.NewMemoryEmitter(f.Describe(),
		emit.WithMemoryClock(func() time.Time { return observedAt }))

	live := &teeSource{inner: NewPollSource(client, time.Millisecond, 2)}
	if err := f.Run(ctx, live, em); err != nil {
		t.Fatalf("live run: %v", err)
	}
	if rejected := em.Rejected(); len(rejected) > 0 {
		t.Fatalf("live run had %d refused events, first: %s", len(rejected), rejected[0].GetReasonCode())
	}

	// The payloads first: a recorded fixture of an MCP connector is the protocol's own
	// answers, so if these differ the events being equal would only mean the mapping is
	// insensitive to the difference.
	recordedPayloads, err := source.ReadPayloads(fixtureDir)
	if err != nil {
		t.Fatalf("read recorded payloads: %v", err)
	}
	if len(live.seen) != len(recordedPayloads) {
		t.Fatalf("live produced %d payloads, the recording has %d", len(live.seen), len(recordedPayloads))
	}
	for i, got := range live.seen {
		want := recordedPayloads[i]
		if got.Kind != want.Kind || !got.At.Equal(want.At) || got.Seq != want.Seq {
			t.Errorf("payload %d: live {%s %s seq=%d}, recorded {%s %s seq=%d}",
				i, got.Kind, got.At, got.Seq, want.Kind, want.At, want.Seq)
			continue
		}
		if !jsonEqual(got.Bytes, want.Bytes) {
			t.Errorf("payload %d (%s): live bytes differ from the recording\nlive:     %s\nrecorded: %s",
				i, got.Kind, got.Bytes, want.Bytes)
		}
	}

	// Then the events, whole, in order.
	recordedEvents := readEvents(t, filepath.Join(fixtureDir, "events.jsonl"))
	liveEvents := em.Events()
	if len(liveEvents) != len(recordedEvents) {
		t.Fatalf("live emitted %d events, the recording has %d", len(liveEvents), len(recordedEvents))
	}
	for i := range liveEvents {
		if !proto.Equal(liveEvents[i], recordedEvents[i]) {
			t.Errorf("event %d differs\nlive:     %s\nrecorded: %s",
				i+1, mustJSON(t, liveEvents[i]), mustJSON(t, recordedEvents[i]))
		}
	}
}

// TestAllowlistRefusesUnknownCalls is the read-only claim, tested rather than asserted: the
// connector cannot be made to call something outside its allowlist even by a caller inside
// this package.
func TestAllowlistRefusesUnknownCalls(t *testing.T) {
	c := &Client{}
	if _, err := c.callTool(t.Context(), "send_message"); err == nil {
		t.Fatal("callTool accepted a tool outside the allowlist")
	}
	if _, err := c.readResource(t.Context(), "slack://messages"); err == nil {
		t.Fatal("readResource accepted a resource outside the allowlist")
	}
}

// TestDescribeIsUsable checks the promises the harness holds the connector to are well formed
// and that every namespace it emits is declared.
func TestDescribeIsUsable(t *testing.T) {
	d := newFeeder().Describe()
	if err := d.Validate(); err != nil {
		t.Fatalf("Describe() is not usable: %v", err)
	}
	for _, ns := range []string{feeder.NSOTelService, feeder.NSOwnerTeam, NSMCPChange} {
		if !d.DeclaresNamespace(ns) {
			t.Errorf("namespace %q is emitted but not declared", ns)
		}
	}
}

// teeSource remembers every payload it passes through, which is how the live run is compared
// against the recording without writing a second recording to disk.
type teeSource struct {
	inner feeder.Source
	seen  []feeder.Payload
}

func (s *teeSource) Next(ctx context.Context) (feeder.Payload, error) {
	p, err := s.inner.Next(ctx)
	if err == nil {
		s.seen = append(s.seen, p)
	}
	return p, err
}

// VerifyReadOnly forwards the FR-046 check, so that the live run exercises it.
func (s *teeSource) VerifyReadOnly(ctx context.Context) error {
	v, ok := s.inner.(interface {
		VerifyReadOnly(context.Context) error
	})
	if !ok {
		return nil
	}
	return v.VerifyReadOnly(ctx)
}

// readEvents loads a recorded events.jsonl, dropping the two fields the log adds and the
// schema does not have.
func readEvents(t *testing.T, path string) []*graphv1.EventEnvelope {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out []*graphv1.EventEnvelope
	for i, line := range splitLines(raw) {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(line, &fields); err != nil {
			t.Fatalf("%s line %d: %v", path, i+1, err)
		}
		delete(fields, "observedAt")
		delete(fields, "appendedSeq")
		body, err := json.Marshal(fields)
		if err != nil {
			t.Fatalf("%s line %d: %v", path, i+1, err)
		}
		ev := &graphv1.EventEnvelope{}
		if err := protojson.Unmarshal(body, ev); err != nil {
			t.Fatalf("%s line %d: %v", path, i+1, err)
		}
		out = append(out, ev)
	}
	return out
}

func splitLines(raw []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, b := range raw {
		if b != '\n' {
			continue
		}
		if i > start {
			out = append(out, raw[start:i])
		}
		start = i + 1
	}
	if start < len(raw) {
		out = append(out, raw[start:])
	}
	return out
}

// jsonEqual compares two JSON documents by value, so that a difference in key order — which
// no reader of the graph can observe — is not reported as a difference in the recording.
func jsonEqual(a, b []byte) bool {
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}

func mustJSON(t *testing.T, ev *graphv1.EventEnvelope) string {
	t.Helper()
	raw, err := protojson.Marshal(ev)
	if err != nil {
		t.Fatalf("encode event: %v", err)
	}
	return string(raw)
}
