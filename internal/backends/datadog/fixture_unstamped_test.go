// SPDX-License-Identifier: Apache-2.0

package datadog_test

import (
	"bufio"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/versionstamp"
)

// The audit's shape, end to end (005 T050): in datadog-log-source-01, `search` carries only an SDK's
// own `@version` on a handful of start-up lines. The feeder's share test rejects it, so the pointer it
// mints has no version join key — and errors_by_version on that pointer, exactly as recorded, is
// answered by the engine: NO_DATA naming every convention searched, with no Datadog call.
func TestTheUnstampedPointerIsAnsweredByTheEngine(t *testing.T) {
	t.Parallel()
	pointer := recordedPointer(t, "fixtures/datadog-log-source-01", "production/search")
	if _, ok := pointer.GetJoinKeys()["version"]; ok {
		t.Fatalf("the unstamped source's pointer has a version join key: %v", pointer)
	}
	b, calls := liveBackend(t, func(w http.ResponseWriter, _ *http.Request) {})
	resp, err := b.Execute(context.Background(), request(engine.ErrorsByVersion(pointer,
		engine.NewWindow(at.Add(-time.Hour), at), pointer.GetJoinKeys()["version"])))
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetOutcome() != investigationv1.TermOutcome_NO_DATA || *calls != 0 {
		t.Fatalf("got %v with %d calls", resp.GetOutcome(), *calls)
	}
	absent := resp.GetDigest().GetCoverage().GetTruncation()
	for _, label := range versionstamp.ConventionLabels() {
		if !strings.Contains(absent, label) {
			t.Errorf("the answer does not name %q: %s", label, absent)
		}
	}
}

// recordedPointer is the last datadog-logs pointer a fixture's events assert on a log source.
func recordedPointer(t *testing.T, fixture, source string) *graphv1.Pointer {
	t.Helper()
	file, err := os.Open(filepath.Join(repoRoot(t), fixture, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var found *graphv1.Pointer
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1<<20), 1<<24)
	for scanner.Scan() {
		var env graphv1.EventEnvelope
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(scanner.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		n := env.GetUpsertNode()
		if n.GetRef().GetValue() != source {
			continue
		}
		for _, p := range n.GetPointers() {
			if p.GetKind() == graphv1.PointerKind_LOG {
				found = p
			}
		}
	}
	if found == nil {
		t.Fatalf("%s holds no log pointer for %s", fixture, source)
	}
	return found
}
