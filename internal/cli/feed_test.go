// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// `feed otel` end to end (T053, FR-044). The replay + dry run path needs no server and no
// database, which is the point: a connector author can see exactly what their recording would
// put in the graph before they have either.

// baselineShopDir is the OTLP feeder's acceptance corpus.
var baselineShopDir = filepath.Join("..", "feeders", "otel", "testdata", "baseline-shop")

func TestFeedOtelDryRunPrintsCanonicalEvents(t *testing.T) {
	stdout, _, code := run(t, t.Context(), "feed", "otel",
		"--replay", baselineShopDir,
		"--source-id", "otel:demo",
		"--id-format", "compat",
		"--dry-run",
		"--output", "json")
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d", code, ExitOK)
	}

	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 23 {
		t.Fatalf("printed %d events, want the 23 of fixtures/baseline-topology-01", len(lines))
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("the first line is not JSON: %v", err)
	}
	if got := first["eventId"]; got != "otel:demo:svc:storefront@w1300" {
		t.Errorf("first event id = %v, want otel:demo:svc:storefront@w1300", got)
	}
	if !strings.HasSuffix(lines[len(lines)-1], "}") ||
		!strings.Contains(lines[len(lines)-1], `"eventId":"otel:demo:ckpt:w1300"`) {
		t.Errorf("the last event is not the window's checkpoint: %s", lines[len(lines)-1])
	}
}

func TestFeedOtelDryRunTableNamesEveryEvent(t *testing.T) {
	stdout, _, code := run(t, t.Context(), "feed", "otel",
		"--replay", baselineShopDir,
		"--source-id", "otel:demo",
		"--dry-run")
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d", code, ExitOK)
	}
	for _, want := range []string{
		"otel:demo:svc:checkout@w20260901T1300Z",
		"upsert_edge",
		"otel.service.name=payments -> server.address=api.stripe.com",
		"identity_claim",
		"source_checkpoint",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the table does not mention %q", want)
		}
	}
}

func TestFeedOtelUsageErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{name: "no source id", args: []string{"feed", "otel", "--replay", baselineShopDir, "--dry-run"}},
		{
			name: "an id format nobody publishes",
			args: []string{"feed", "otel", "--replay", baselineShopDir, "--dry-run",
				"--source-id", "otel:demo", "--id-format", "short"},
		},
		{
			name: "a window of zero",
			args: []string{"feed", "otel", "--replay", baselineShopDir, "--dry-run",
				"--source-id", "otel:demo", "--window", "0"},
		},
		{
			name: "nothing to listen on and nothing to replay",
			args: []string{"feed", "otel", "--source-id", "otel:demo",
				"--listen-grpc", "", "--listen-http", ""},
		},
		{
			name: "a recording that is not there",
			args: []string{"feed", "otel", "--source-id", "otel:demo", "--dry-run",
				"--replay", filepath.Join("testdata", "no-such-fixture")},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, stderr, code := run(t, t.Context(), tc.args...)
			if code != ExitUsage {
				t.Fatalf("exit code = %d, want %d (stderr: %s)", code, ExitUsage, stderr)
			}
		})
	}
}

// TestFeedOtelIsRegisteredOnce guards the registry in feed.go: a subcommand registered twice,
// or a second connector shadowing this one, would be a silent loss of a command.
func TestFeedOtelIsRegisteredOnce(t *testing.T) {
	root := NewRootCommand()
	var feed *cobra.Command
	for _, cmd := range root.Commands() {
		if cmd.Name() == "feed" {
			feed = cmd
		}
	}
	if feed == nil {
		t.Fatal("the root command has no `feed`")
	}
	seen := 0
	for _, sub := range feed.Commands() {
		if sub.Name() == "otel" {
			seen++
		}
	}
	if seen != 1 {
		t.Errorf("`feed otel` is registered %d times, want once", seen)
	}
}

// TestFeedOtelRecordRoundTrip: what --record writes is a fixture --replay reads, and the second
// run emits exactly what the first one did. That round trip is FR-044's claim in one test.
func TestFeedOtelRecordRoundTrip(t *testing.T) {
	dir := t.TempDir()
	first, _, code := run(t, t.Context(), "feed", "otel",
		"--replay", baselineShopDir,
		"--record", dir,
		"--source-id", "otel:demo",
		"--id-format", "compat",
		"--dry-run",
		"--output", "json")
	if code != ExitOK {
		t.Fatalf("recording run exited %d, want %d", code, ExitOK)
	}

	for _, name := range []string{"manifest.yaml", "events.jsonl", filepath.Join("payloads", "index.jsonl")} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("the recording has no %s: %v", name, err)
		}
	}

	second, _, code := run(t, t.Context(), "feed", "otel",
		"--replay", dir,
		"--source-id", "otel:demo",
		"--id-format", "compat",
		"--dry-run",
		"--output", "json")
	if code != ExitOK {
		t.Fatalf("replay of the recording exited %d, want %d", code, ExitOK)
	}
	if first != second {
		t.Error("replaying the recording emitted something else than the run that recorded it")
	}
}
