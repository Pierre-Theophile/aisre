// SPDX-License-Identifier: Apache-2.0

package datadog_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	ddfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/datadog"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// datadog-tags-01: tags become owners and identity claims, and nothing else (005 T076–T078).
//
// `checkout`'s lines carry, on every line, `team:payments` and `owner:payments-team` — two keys naming
// one owner differently, both kept (FR-068) — and `kube_namespace:shop` with `kube_deployment:checkout`,
// claimed as the Kubernetes deployment `shop/checkout`. They also carry `cost_center:cc-1234`, off the
// allowlist, which becomes nothing (FR-066); a `team:42` a misconfigured agent writes, which measures
// nothing an owner could be and becomes nothing (FR-067); and `team:search` on a third of the lines, a
// shared host's tag, which is below the share and is not checkout's owner.

const tagsFixture = "fixtures/datadog-tags-01"

func tagsPayloads(t *testing.T) []feeder.Payload {
	t.Helper()
	m := ddfeeder.SourceMeasurement{Source: "production/checkout", Lines: 9000, ErrorLines: 90, HostLines: 9000,
		Candidates: []ddfeeder.CandidateCount{{Label: "version (tag)", Lines: 9000, ErrorLines: 90}},
		Tags: []ddfeeder.TagCount{
			{Key: "team", Value: "payments", Lines: 9000},
			{Key: "owner", Value: "payments-team", Lines: 9000},
			{Key: ddfeeder.KubeDeploymentPair, Value: "shop/checkout", Lines: 9000},
			{Key: "cost_center", Value: "cc-1234", Lines: 9000},
			{Key: "team", Value: "42", Lines: 9000},
			{Key: "team", Value: "search", Lines: 3000},
		}}
	raw, err := json.Marshal(ddfeeder.DiscoveryTick{LogSources: []string{"production/checkout"},
		Window: &ddfeeder.DiscoveryWindow{From: hm(13, 0), To: hm(14, 0)}, Measurements: []ddfeeder.SourceMeasurement{m}})
	if err != nil {
		t.Fatal(err)
	}
	return []feeder.Payload{{Kind: ddfeeder.PayloadDiscovery, At: hm(14, 0), Bytes: raw}}
}

func TestGenerateDatadogTagsFixture(t *testing.T) {
	if os.Getenv(genFixturesEnv) == "" {
		t.Skipf("set %s=1 to regenerate %s", genFixturesEnv, tagsFixture)
	}
	generateFixture(t, tagsFixture, "datadog-tags",
		"Tags on a watched log source's lines become owners and identity claims, and nothing else. `team:payments` "+
			"and `owner:payments-team` are on every line — two keys naming one owner differently — and both "+
			"become OWNER nodes with owned-by edges, left for the resolution layer to decide about in the open; "+
			"`kube_namespace:shop` with `kube_deployment:checkout` is claimed as the Kubernetes deployment "+
			"shop/checkout. `cost_center` is off the allowlist and `team:42` measures something, so both become "+
			"nothing; `team:search` is on a third of the lines and is not checkout's owner.",
		ddfeeder.Options{OrgSlug: "twin"}, tagsPayloads(t), hm(13, 59), hm(15, 0), `
queries:
  # The log source, its two owners, and no property from a tag off the allowlist or a number.
  - name: checkout-owners
    kind: subgraph
    focus: datadog.service=production/checkout
    valid_at: 2026-09-21T15:00:00Z
    observed_at: 2026-09-21T15:00:00Z
    hops: 1
    direction: both
  # The two owners are two entities: nothing says payments and payments-team are one team.
  - name: why-the-two-owner-spellings-are-apart
    kind: audit
    ref_a: owner.team=payments
    ref_b: owner.team=payments-team
    observed_at: 2026-09-21T15:00:00Z
`)
}

// FR-065–FR-068 on the payload alone.
func TestTagsBecomeOwnersAndClaimsOnly(t *testing.T) {
	t.Parallel()
	em := run(t, ddfeeder.Options{}, tagsPayloads(t)...)
	var owners, edges, claims []string
	for _, ev := range em.Events() {
		switch {
		case ev.GetUpsertNode().GetType() == graphv1.NodeType_OWNER:
			owners = append(owners, ev.GetUpsertNode().GetRef().GetValue())
		case ev.GetUpsertEdge().GetType() == graphv1.EdgeType_OWNED_BY:
			edges = append(edges, ev.GetUpsertEdge().GetDst().GetValue())
		case ev.GetIdentityClaim() != nil:
			claims = append(claims, ev.GetIdentityClaim().GetClaim().GetNamespace()+"="+ev.GetIdentityClaim().GetClaim().GetValue())
		}
		if ev.GetSourceCheckpoint() != nil {
			continue // the checkpoint names the refused key, which is the point; never its value
		}
		raw, _ := json.Marshal(ev)
		if strings.Contains(string(raw), "cc-1234") || strings.Contains(string(raw), "cost_center") {
			t.Errorf("an off-allowlist tag reached the graph: %s", raw)
		}
	}
	if strings.Join(owners, ",") != "payments-team,payments" || strings.Join(edges, ",") != "payments-team,payments" {
		t.Errorf("owners %v, edges %v", owners, edges)
	}
	if strings.Join(claims, ",") != feeder.NSK8sDeployment+"=shop/checkout" {
		t.Errorf("claims %v", claims)
	}
	notes := checkpointNotes(em)
	for _, want := range []string{`"cost_center" is not on the allowlist`, "team:42 measures something", "team:search is on 33.3% of lines"} {
		if !strings.Contains(notes, want) {
			t.Errorf("the checkpoint does not state %q:\n%s", want, notes)
		}
	}
}
