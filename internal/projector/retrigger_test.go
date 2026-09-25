// SPDX-License-Identifier: Apache-2.0

package projector_test

import (
	"context"
	"fmt"
	"testing"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/Pierre-Theophile/aisre/internal/projector"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
)

// C8's re-trigger, and the graph question it rests on (004 T137, T138;
// specs/004-deploy-feeders/contracts/deploy-claims.md §2.1).
//
// C8 merges two change observations that state the same deploy identifier **for a target they share**,
// and "the same target" is a fact about the graph rather than about the keys. So its answer can change
// after its keys are stored — a change attaches to a target that did not exist, or two target identities
// merge — and rules run only when their evidence is stored. Without the re-trigger the merge is made or
// not made depending on the order the events happened to arrive in, which is what FR-021 forbids and
// what a fixture's shuffle exists to find.
//
// These tests drive the two orders through a real graph rather than a fake store, because the property
// under test is precisely the one a fake store cannot have.

const (
	retriggerCommit = "9f8e7d6c5b4a39281706f5e4d3c2b1a098765432"
	deployNamespace = "deploy.commit_sha"
)

func deployChangeEvent(id, source, changeValue, changeNamespace, target, targetNamespace, when string) *graphv1.EventEnvelope {
	return &graphv1.EventEnvelope{
		EventId: id, IdempotencyKey: id, SourceId: source, SchemaVersion: "1.0.0",
		Body: &graphv1.EventEnvelope_ObserveChange{ObserveChange: &graphv1.ObserveChange{
			Ref: &graphv1.Ref{Namespace: changeNamespace, Value: changeValue},
			Change: &graphv1.Change{
				Kind:    graphv1.ChangeKind_ROLLOUT,
				Summary: "rollout of " + retriggerCommit,
			},
			Targets: []*graphv1.Ref{{Namespace: targetNamespace, Value: target}},
			ValidAt: at(when),
		}},
	}
}

// envAttributes carries the environment C8 requires both sides to state. Two unstated environments are
// not an agreed one: a staging rollout of a commit and the production rollout of the same commit are
// two rollouts, and on a certain rule treating them as agreed would merge a promotion into its own
// staging deploy.
func envAttributes(environment string) *structpb.Struct {
	s, err := structpb.NewStruct(map[string]any{"deployment.environment.name": environment})
	if err != nil {
		panic(err)
	}
	return s
}

// deployKeyEvent is the commit as a CORRELATION key, which is what C8 reads (004 T148). It was an
// identity claim until T148, and the event log now refuses that form: one deployment ships one commit
// to every target it touches, so the value is shared and a claim could hold it for only one of them.
func deployKeyEvent(id, source, changeValue, changeNamespace string) *graphv1.EventEnvelope {
	return &graphv1.EventEnvelope{
		EventId: id, IdempotencyKey: id, SourceId: source, SchemaVersion: "1.0.0",
		Body: &graphv1.EventEnvelope_CorrelateEntity{CorrelateEntity: &graphv1.CorrelateEntity{
			Subject:    &graphv1.Ref{Namespace: changeNamespace, Value: changeValue},
			Key:        &graphv1.Ref{Namespace: deployNamespace, Value: retriggerCommit},
			Attributes: envAttributes("production"),
		}},
	}
}

// serviceClaimEvent is one source claiming the OTel service name of its own target, which is what lets
// C1 merge the two targets and so lets C8's intersection become non-empty.
func serviceClaimEvent(id, source, subject, subjectNamespace, otelName string) *graphv1.EventEnvelope {
	return &graphv1.EventEnvelope{
		EventId: id, IdempotencyKey: id, SourceId: source, SchemaVersion: "1.0.0",
		Body: &graphv1.EventEnvelope_IdentityClaim{IdentityClaim: &graphv1.IdentityClaim{
			Subject: &graphv1.Ref{Namespace: subjectNamespace, Value: subject},
			Claim:   &graphv1.Ref{Namespace: "otel.service.name", Value: otelName},
		}},
	}
}

func twoSourceManifest() manifest {
	var m manifest
	m.SchemaVersion = "1.0.0"
	m.Sources = append(m.Sources,
		struct {
			SourceID         string `yaml:"source_id"`
			Kind             string `yaml:"kind"`
			Ordering         string `yaml:"ordering"`
			ReorderingWindow string `yaml:"reordering_window"`
		}{SourceID: "github:acme", Kind: "feeder", Ordering: "none", ReorderingWindow: "1m"},
		struct {
			SourceID         string `yaml:"source_id"`
			Kind             string `yaml:"kind"`
			Ordering         string `yaml:"ordering"`
			ReorderingWindow string `yaml:"reordering_window"`
		}{SourceID: "gcp:acme", Kind: "feeder", Ordering: "none", ReorderingWindow: "1m"},
	)
	return m
}

// pendingQueueLength is how much work is still queued. It must be zero once a drain has run: the queue
// is a work list and not a log, so a row that survives its drain is a row that will be re-processed on
// every future event, and the bound on drain rounds would turn a leak into a silent cost rather than a
// visible failure.
func pendingQueueLength(t *testing.T, store *postgres.Store) int {
	t.Helper()
	var n int
	if err := store.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM graph.pending_resolution`).Scan(&n); err != nil {
		t.Fatalf("count the queue: %v", err)
	}
	return n
}

// mergedWithC8 reports whether the two change entities ended up as one, and by which rule.
func mergedWithC8(t *testing.T, store *postgres.Store) (string, bool) {
	t.Helper()
	var ruleID string
	err := store.Pool().QueryRow(context.Background(), `
		SELECT rule_id FROM graph.resolution_decisions
		WHERE rule_id = 'C8' ORDER BY decided_at DESC LIMIT 1`).Scan(&ruleID)
	if err != nil {
		return "", false
	}
	return ruleID, true
}

// The order C8 already handled: the targets are merged before the deploy keys arrive, so the
// intersection is non-empty the first time the rule runs.
func TestC8MergesWhenTheTargetsAreAlreadyOne(t *testing.T) {
	t.Parallel()
	p, store := newProjector(t, twoSourceManifest())

	applyInOrder(t, p, c8Events(), []int{0, 1, 2, 3, 4, 5})
	if _, ok := mergedWithC8(t, store); !ok {
		t.Fatal("C8 did not fire even with the targets merged first; the rule itself is not working, " +
			"so the re-trigger test below would prove nothing")
	}
	if n := pendingQueueLength(t, store); n != 0 {
		t.Errorf("%d key(s) still queued after the drain; a row that survives its drain is re-processed "+
			"on every future event", n)
	}
}

// The order the re-trigger exists for: the deploy keys arrive FIRST, while the two targets are still
// two entities, so C8 finds an empty intersection. The service claim that merges them arrives last —
// and nothing would re-run C8 without retrigger.go.
func TestC8MergesWhenTheTargetsMergeAfterTheDeployKeys(t *testing.T) {
	t.Parallel()
	p, store := newProjector(t, twoSourceManifest())

	// Changes, then the deploy keys, then the claims that merge the two targets — so C8 runs while
	// the targets are still two entities and finds an empty intersection.
	applyInOrder(t, p, c8Events(), []int{0, 1, 4, 5, 2, 3})

	if _, ok := mergedWithC8(t, store); !ok {
		t.Error("C8 did not fire when the two targets merged AFTER the deploy keys were stored. " +
			"Rules run only when their evidence is stored, so a merge that moves a change's target set has to " +
			"re-ask the rules — otherwise the answer depends on arrival order (FR-021)")
	}
	if n := pendingQueueLength(t, store); n != 0 {
		t.Errorf("%d key(s) still queued after the drain; a row that survives its drain is re-processed "+
			"on every future event", n)
	}
}

// The OTHER order the re-trigger exists for, and the one the attachment path does not cover: both
// changes are attached from the start, because their targets already exist as nodes. So nothing
// attaches late — and C8 still has to fire when the two targets are merged afterwards.
//
// This case is why the merge path is queued as well as the attachment path. Without it, a change that
// was attached all along would never be re-examined when the thing it changed turned out to be the
// same thing another source's change changed.
func TestC8MergesWhenAlreadyAttachedTargetsMergeLater(t *testing.T) {
	t.Parallel()
	p, store := newProjector(t, twoSourceManifest())

	events := []*graphv1.EventEnvelope{
		// The targets exist as nodes first, so both changes attach immediately.
		targetNodeEvent("gh-target", "github:acme", "acme/storefront", "github.repo"),
		targetNodeEvent("gcp-target", "gcp:acme", "proj/europe-west1/storefront", "gcp.cloudrun.service"),
		deployChangeEvent("gh-change", "github:acme", "gh/deploy/1", "github.change",
			"acme/storefront", "github.repo", "2026-09-21T14:00:00Z"),
		deployChangeEvent("gcp-change", "gcp:acme", "rollout/storefront-00042", "gcp.change",
			"proj/europe-west1/storefront", "gcp.cloudrun.service", "2026-09-21T14:00:00Z"),
		deployKeyEvent("gh-claim", "github:acme", "gh/deploy/1", "github.change"),
		deployKeyEvent("gcp-claim", "gcp:acme", "rollout/storefront-00042", "gcp.change"),
		// And only now do the two targets become one entity.
		serviceClaimEvent("gh-service", "github:acme", "acme/storefront", "github.repo", "storefront"),
		serviceClaimEvent("gcp-service", "gcp:acme", "proj/europe-west1/storefront",
			"gcp.cloudrun.service", "storefront"),
	}
	applyInOrder(t, p, events, []int{0, 1, 2, 3, 4, 5, 6, 7})

	if _, ok := mergedWithC8(t, store); !ok {
		t.Error("C8 did not fire when two ALREADY ATTACHED targets merged. The attachment path cannot " +
			"cover this: nothing attached late, so only the merge itself says the rule should be asked " +
			"again")
	}
	if n := pendingQueueLength(t, store); n != 0 {
		t.Errorf("%d key(s) still queued after the drain; a row that survives its drain is re-processed "+
			"on every future event", n)
	}
}

// And the case only the attachment path covers: two sources naming the SAME target ref, so no merge
// ever happens — the target simply does not exist yet when either change arrives.
//
// It is not a contrived shape. The Vercel feeder claims `github.repo` where Vercel reports the connected
// repository, so two deploy feeders can name one repository as the target of their rollouts, and the
// repository is often something neither of them has described as a node yet.
func TestC8MergesWhenOneSharedTargetAppearsLate(t *testing.T) {
	t.Parallel()
	p, store := newProjector(t, twoSourceManifest())

	events := []*graphv1.EventEnvelope{
		deployChangeEvent("gh-change", "github:acme", "gh/deploy/1", "github.change",
			"acme/storefront", "github.repo", "2026-09-21T14:00:00Z"),
		deployChangeEvent("other-change", "gcp:acme", "vercel/dpl_1", "vercel.change",
			"acme/storefront", "github.repo", "2026-09-21T14:00:00Z"),
		deployKeyEvent("gh-claim", "github:acme", "gh/deploy/1", "github.change"),
		deployKeyEvent("other-claim", "gcp:acme", "vercel/dpl_1", "vercel.change"),
		// The shared target is described only now, which is what attaches both changes at once.
		targetNodeEvent("the-target", "github:acme", "acme/storefront", "github.repo"),
	}
	applyInOrder(t, p, events, []int{0, 1, 2, 3, 4})

	if _, ok := mergedWithC8(t, store); !ok {
		t.Error("C8 did not fire when the one target both changes name appeared after their deploy " +
			"keys. No merge happens in this shape, so only the attachment says the rule should be " +
			"asked again")
	}
	if n := pendingQueueLength(t, store); n != 0 {
		t.Errorf("%d key(s) still queued after the drain; a row that survives its drain is re-processed "+
			"on every future event", n)
	}
}

// targetNodeEvent upserts the thing a change was applied to, so that the change attaches on arrival
// rather than waiting for its target to appear.
func targetNodeEvent(id, source, value, namespace string) *graphv1.EventEnvelope {
	return &graphv1.EventEnvelope{
		EventId: id, IdempotencyKey: id, SourceId: source, SchemaVersion: "1.0.0",
		Body: &graphv1.EventEnvelope_UpsertNode{UpsertNode: &graphv1.UpsertNode{
			Ref:         &graphv1.Ref{Namespace: namespace, Value: value},
			Type:        graphv1.NodeType_SERVICE,
			DisplayName: value,
			ValidAt:     at("2026-09-21T13:00:00Z"),
		}},
	}
}

// applyInOrder applies the events in the given order, each at its own observed instant.
//
// Distinct instants matter: a merge closes the observed interval of the entity version it absorbs, and
// the store refuses to close an interval at the instant it opened — correctly, because a version that
// existed for no time is not a version. So every event gets its own second.
func applyInOrder(t *testing.T, p *projector.Projector, events []*graphv1.EventEnvelope, order []int) {
	t.Helper()
	for step, i := range order {
		apply(t, p, events[i], fmt.Sprintf("2026-09-21T14:%02d:00Z", 30+step))
	}
}

// c8Events is one rollout seen by two sources: a change each, a deploy key each, and the service
// claims that make the two targets one entity.
func c8Events() []*graphv1.EventEnvelope {
	return []*graphv1.EventEnvelope{
		deployChangeEvent("gh-change", "github:acme", "gh/deploy/1", "github.change",
			"acme/storefront", "github.repo", "2026-09-21T14:00:00Z"),
		deployChangeEvent("gcp-change", "gcp:acme", "rollout/storefront-00042", "gcp.change",
			"proj/europe-west1/storefront", "gcp.cloudrun.service", "2026-09-21T14:00:00Z"),
		serviceClaimEvent("gh-service", "github:acme", "acme/storefront", "github.repo", "storefront"),
		serviceClaimEvent("gcp-service", "gcp:acme", "proj/europe-west1/storefront",
			"gcp.cloudrun.service", "storefront"),
		deployKeyEvent("gh-claim", "github:acme", "gh/deploy/1", "github.change"),
		deployKeyEvent("gcp-claim", "gcp:acme", "rollout/storefront-00042", "gcp.change"),
	}
}
