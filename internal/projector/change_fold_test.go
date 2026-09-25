// SPDX-License-Identifier: Apache-2.0

package projector_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"google.golang.org/protobuf/types/known/structpb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
)

// A rollout two sources report, merged by C8, keeps what each of them said (004 T152).
//
// The GitHub half knows who deployed and which deployment it was; the Cloud Run half knows the revision
// and the instant production moved. An investigation asking "what changed on storefront?" needs both,
// and before change_fold.go it got neither: the merge rewrote the survivor from node assertions a change
// does not have, leaving a version with no change on it, which `diff` skips.

func foldGitHubChange() *graphv1.EventEnvelope {
	env := deployChangeEvent("gh-change", "github:acme", "gh/deploy/1", "github.change",
		"acme/storefront", "github.repo", "2026-09-21T14:00:05Z")
	body := env.GetObserveChange()
	body.Change = &graphv1.Change{
		Kind:      graphv1.ChangeKind_ROLLOUT,
		Summary:   "GitHub deployment 1 of acme/storefront to production",
		Actor:     "octocat",
		ActorKind: graphv1.ActorKind_PERSON,
		OriginRef: "https://github.com/acme/storefront/deployments/1",
	}
	body.Props = mustStructFold(map[string]any{"github.deployment.sha": retriggerCommit})
	return env
}

func foldCloudRunChange() *graphv1.EventEnvelope {
	env := deployChangeEvent("gcp-change", "gcp:acme", "rollout/storefront-00042", "gcp.change",
		"proj/europe-west1/storefront", "gcp.cloudrun.service", "2026-09-21T14:00:00Z")
	body := env.GetObserveChange()
	body.Change = &graphv1.Change{
		Kind:    graphv1.ChangeKind_ROLLOUT,
		Summary: "Cloud Run rollout of storefront-00042",
	}
	body.Props = mustStructFold(map[string]any{"gcp.cloudrun.revision": "storefront-00042"})
	return env
}

func foldEvents() []*graphv1.EventEnvelope {
	events := c8Events()
	events[0] = foldGitHubChange()
	events[1] = foldCloudRunChange()
	return events
}

func mustStructFold(fields map[string]any) *structpb.Struct {
	s, err := structpb.NewStruct(fields)
	if err != nil {
		panic(err)
	}
	return s
}

// currentChangeRecords is every current version of a surviving entity that carries a change.
func currentChangeRecords(t *testing.T, store *postgres.Store) []string {
	t.Helper()
	rows, err := store.Pool().Query(context.Background(), `
		SELECT v.display_name, v.change::text, v.props::text, lower(v.valid), upper(v.valid),
		       v.valid_from_unknown, v.produced_by_event_ids
		FROM graph.entity_versions v
		JOIN graph.entities e ON e.entity_id = v.entity_id
		WHERE e.merged_into IS NULL AND upper_inf(v.observed) AND v.change IS NOT NULL
		ORDER BY v.display_name`)
	if err != nil {
		t.Fatalf("read change records: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var (
			name, change, props string
			lower, upper        any
			unknown             bool
			producedBy          []string
		)
		if err := rows.Scan(&name, &change, &props, &lower, &upper, &unknown, &producedBy); err != nil {
			t.Fatalf("scan change record: %v", err)
		}
		out = append(out, fmt.Sprintf("%s|%s|%s|%v|%v|%v|%v", name, change, props, lower, upper, unknown, producedBy))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read change records: %v", err)
	}
	return out
}

func TestAMergedRolloutKeepsWhatEachSourceSaid(t *testing.T) {
	t.Parallel()
	p, store := newProjector(t, twoSourceManifest())
	applyInOrder(t, p, foldEvents(), []int{0, 1, 2, 3, 4, 5})
	if _, ok := mergedWithC8(t, store); !ok {
		t.Fatal("C8 did not merge the two halves; nothing below would be testing a merge")
	}

	var (
		name       string
		changeJSON []byte
		propsJSON  []byte
		lower      string
	)
	err := store.Pool().QueryRow(context.Background(), `
		SELECT v.display_name, v.change, v.props, to_char(lower(v.valid) AT TIME ZONE 'UTC', 'HH24:MI:SS')
		FROM graph.entity_versions v
		JOIN graph.entities e ON e.entity_id = v.entity_id
		WHERE e.merged_into IS NULL AND upper_inf(v.observed) AND v.change IS NOT NULL`).
		Scan(&name, &changeJSON, &propsJSON, &lower)
	if err != nil {
		t.Fatalf("the merged rollout has no current change record — the defect this file fixes: %v", err)
	}

	var change map[string]any
	if err := json.Unmarshal(changeJSON, &change); err != nil {
		t.Fatalf("decode change: %v", err)
	}
	if lower != "14:00:00" {
		t.Errorf("the merged rollout is dated %s; the earliest stated instant is 14:00:00 (Cloud Run's), and "+
			"of two stated instants for one rollout the earlier is when production first moved", lower)
	}
	if name != "Cloud Run rollout of storefront-00042" || change["summary"] != name {
		t.Errorf("summary %q / display name %q; the primary observation (earliest stated instant) names it",
			change["summary"], name)
	}
	if change["actor"] != "octocat" || change["actorKind"] != "PERSON" {
		t.Errorf("actor %v (%v); only the GitHub half names who deployed, and the merge must keep it",
			change["actor"], change["actorKind"])
	}
	if change["originRef"] != "https://github.com/acme/storefront/deployments/1" {
		t.Errorf("origin ref %v; the GitHub half's link is the only one stated", change["originRef"])
	}
	var props map[string]any
	if err := json.Unmarshal(propsJSON, &props); err != nil {
		t.Fatalf("decode props: %v", err)
	}
	for _, key := range []string{"github.deployment.sha", "gcp.cloudrun.revision"} {
		if _, ok := props[key]; !ok {
			t.Errorf("property %s is missing from the merged rollout; each source's properties are kept", key)
		}
	}
}

// The fold is a function of the set of observations, so every arrival order — the changes before the
// keys, after them, after the targets merge — stores the same record.
func TestAMergedRolloutIsTheSameWhateverTheArrivalOrder(t *testing.T) {
	t.Parallel()
	orders := [][]int{
		{0, 1, 2, 3, 4, 5},
		{1, 0, 2, 3, 4, 5},
		{0, 1, 4, 5, 2, 3},
		{2, 3, 4, 5, 0, 1},
		{2, 3, 4, 5, 1, 0},
		{4, 2, 1, 5, 3, 0},
	}
	var want []string
	for i, order := range orders {
		p, store := newProjector(t, twoSourceManifest())
		applyInOrder(t, p, foldEvents(), order)
		if _, ok := mergedWithC8(t, store); !ok {
			t.Fatalf("order %v: C8 did not merge", order)
		}
		got := currentChangeRecords(t, store)
		if len(got) != 1 {
			t.Fatalf("order %v: %d current change records, want the one merged rollout:\n%v", order, len(got), got)
		}
		if i == 0 {
			want = got
			continue
		}
		if got[0] != want[0] {
			t.Errorf("order %v stored a different rollout than order %v:\n got %s\nwant %s",
				order, orders[0], got[0], want[0])
		}
	}
}
