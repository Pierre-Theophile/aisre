// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/versionstamp"
)

// US5 scenario 9 (005 T075), on datadog-log-rollout-merge-01's recorded answer: the version group a
// log backend names for the new commit resolves to the one rollout C9 and C8 made of Cloud Run's and
// the log-observed one — whatever deployed it, whichever backend holds the logs.
func TestAVersionGroupResolvesToTheChangeThatShippedIt(t *testing.T) {
	t.Parallel()
	_, here, _, _ := runtime.Caller(0)
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(here), "../../../fixtures/datadog-log-rollout-merge-01/golden/diff.what-changed-on-checkout.json"))
	if err != nil {
		t.Fatal(err)
	}
	var diff graphv1.DiffResponse
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(raw, &diff); err != nil {
		t.Fatal(err)
	}
	if len(diff.GetChanges()) != 1 {
		t.Fatalf("%d ranked changes; the merge fixture's point is one", len(diff.GetChanges()))
	}
	cat := NewCatalogue()
	for _, change := range diff.GetChanges() {
		cat.NoteNode(change.GetChange())
		cat.NoteChangeKeys(change)
	}

	const newCommit = "7e6d5c4b3a2918070f1e2d3c4b5a69788796a5b4"
	group := func(version string) *investigationv1.VersionBreakdown {
		ref, reason := versionstamp.Normalise(version)
		return &investigationv1.VersionBreakdown{Version: version, DeployRef: ref, DeployRefAbsentReason: reason}
	}
	text := renderVersionChanges(&investigationv1.ErrorsByVersionDigest{Versions: []*investigationv1.VersionBreakdown{
		group(newCommit), group("0f1e2d3c4b5a69788796a5b4c3d2e1f0a9b8c7d6"), group("v2.3.0"),
	}}, cat)
	if !strings.Contains(text, `version "`+newCommit+`" is change`) || strings.Count(text, " is change ") != 1 {
		t.Fatalf("resolution:\n%s", text)
	}
	id, _ := cat.ChangeForDeployRef(group(newCommit).GetDeployRef())
	if id != diff.GetChanges()[0].GetChange().GetEntityId() {
		t.Errorf("resolved to %s, the merged change is %s", id, diff.GetChanges()[0].GetChange().GetEntityId())
	}
}
