// SPDX-License-Identifier: Apache-2.0

package k8s_test

import (
	"maps"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	k8sfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/k8s"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// A Kubernetes rollout's commit, from operator-trusted pod-template keys (004 T149).
//
// This is what lets C8 fire for a workload at all: GitHub states a commit and no image, so without a
// commit on the cluster side the GitHub deployment an operator maps onto `k8s.deployment` has nothing
// to agree with. Each case below is a way the commit could be minted wrongly, and C8 is certain, so a
// wrong key is a wrong merge with no human in the loop.

const (
	trustedCommitKey = "commit-sha"
	commitOld        = "af5bd3c0b0e4a1d2c3f4a5b6c7d8e9f0a1b2c3d4"
	commitNew        = "9f8e7d6c5b4a39281706f5e4d3c2b1a098765432"
)

func commitFeeder(t *testing.T, keys ...string) *k8sfeeder.Feeder {
	t.Helper()
	opts := fixtureOptions()
	opts.CommitLabels = keys
	f, err := k8sfeeder.New(opts, nil)
	if err != nil {
		t.Fatalf("new feeder: %v", err)
	}
	return f
}

// rolloutCommitKeys runs one rollout of payments from before to after and returns the commit keys the
// stream carries.
func rolloutCommitKeys(t *testing.T, f *k8sfeeder.Feeder, before, after *appsv1.Deployment) []string {
	t.Helper()
	mapOne(t, f, k8sfeeder.KindDeployments, k8sfeeder.EventAdded, before, fixtureStart)
	after.ResourceVersion = "1050"
	after.Annotations[k8sfeeder.AnnotationRevision] = "8"
	events := mapOne(t, f, k8sfeeder.KindDeployments, k8sfeeder.EventModified, after,
		fixtureStart.Add(5*time.Minute))
	if _, ok := changeOf(events, graphv1.ChangeKind_ROLLOUT); !ok {
		t.Fatal("no ROLLOUT change, so this test is not exercising a rollout at all")
	}
	var out []string
	for _, e := range events {
		if body := e.GetCorrelateEntity(); body != nil && body.GetKey().GetNamespace() == feeder.NSDeployCommitSHA {
			out = append(out, body.GetKey().GetValue())
		}
	}
	return out
}

// withTemplateCommit sets the commit on the pod template, as deploy tooling that stamps each rollout does.
// The image changes too, so there is a rollout to observe.
func withTemplateCommit(d *appsv1.Deployment, commit, image string) *appsv1.Deployment {
	if d.Spec.Template.Labels == nil {
		d.Spec.Template.Labels = map[string]string{}
	}
	d.Spec.Template.Labels[trustedCommitKey] = commit
	d.Spec.Template.Spec.Containers[0].Image = image
	return d
}

func TestATrustedTemplateCommitIsARolloutsCommitKey(t *testing.T) {
	t.Parallel()
	keys := rolloutCommitKeys(t, commitFeeder(t, trustedCommitKey),
		withTemplateCommit(paymentsDeployment(), commitOld, "ghcr.io/twin/payments:v7"),
		withTemplateCommit(paymentsDeployment(), commitNew, "ghcr.io/twin/payments:v8"))
	if len(keys) != 1 || keys[0] != commitNew {
		t.Fatalf("commit keys = %v, want exactly the new commit %s: the operator trusts this key, the "+
			"template states it, and this rollout changed it", keys, commitNew)
	}
}

// Opt-in: an operator who configured nothing gets no commit, whatever the cluster's labels say.
func TestNoCommitIsReadUnlessTheOperatorTrustsAKey(t *testing.T) {
	t.Parallel()
	keys := rolloutCommitKeys(t, commitFeeder(t),
		withTemplateCommit(paymentsDeployment(), commitOld, "ghcr.io/twin/payments:v7"),
		withTemplateCommit(paymentsDeployment(), commitNew, "ghcr.io/twin/payments:v8"))
	if len(keys) != 0 {
		t.Fatalf("commit keys = %v with no key configured; a label anyone with namespace edit can write "+
			"became a merge key for a certain rule", keys)
	}
}

// A rollout that did not change the commit did not ship it: a restart, or an image bumped by hand while
// the label was left behind. Minting it would give two rollouts one commit key.
func TestARolloutThatKeepsTheCommitDoesNotClaimIt(t *testing.T) {
	t.Parallel()
	keys := rolloutCommitKeys(t, commitFeeder(t, trustedCommitKey),
		withTemplateCommit(paymentsDeployment(), commitNew, "ghcr.io/twin/payments:v7"),
		withTemplateCommit(paymentsDeployment(), commitNew, "ghcr.io/twin/payments:v8"))
	if len(keys) != 0 {
		t.Fatalf("commit keys = %v for a rollout whose template commit did not change; the previous "+
			"rollout shipped that commit, and C8 would merge the pipeline's deployment with either", keys)
	}
}

// Only the pod template: a label on the Deployment's own metadata can outlive the rollout that set it.
func TestACommitOnTheWorkloadMetadataIsNotRead(t *testing.T) {
	t.Parallel()
	before, after := paymentsDeployment(), paymentsDeployment()
	// The fixture builder shares one map between the workload's labels and its template's; split them,
	// or the "workload" label below would be on the template too.
	for _, d := range []*appsv1.Deployment{before, after} {
		d.Labels = maps.Clone(d.Labels)
	}
	before.Labels[trustedCommitKey] = commitOld
	after.Labels[trustedCommitKey] = commitNew
	before.Spec.Template.Spec.Containers[0].Image = "ghcr.io/twin/payments:v7"
	after.Spec.Template.Spec.Containers[0].Image = "ghcr.io/twin/payments:v8"
	keys := rolloutCommitKeys(t, commitFeeder(t, trustedCommitKey), before, after)
	if len(keys) != 0 {
		t.Fatalf("commit keys = %v read from the workload's own labels; they are not part of the revision "+
			"being rolled out", keys)
	}
}

// An abbreviation is passed over and the next trusted key tried; it is never padded (FR-041).
func TestAnAbbreviatedCommitFallsThroughToTheNextTrustedKey(t *testing.T) {
	t.Parallel()
	before := withTemplateCommit(paymentsDeployment(), commitOld[:7], "ghcr.io/twin/payments:v7")
	after := withTemplateCommit(paymentsDeployment(), commitNew[:7], "ghcr.io/twin/payments:v8")
	after.Spec.Template.Annotations = map[string]string{"deploy.example/commit": commitNew}
	keys := rolloutCommitKeys(t, commitFeeder(t, trustedCommitKey, "deploy.example/commit"), before, after)
	if len(keys) != 1 || keys[0] != commitNew {
		t.Fatalf("commit keys = %v, want %s from the second trusted key: the first held an abbreviation, "+
			"which is shared by many commits", keys, commitNew)
	}
}
