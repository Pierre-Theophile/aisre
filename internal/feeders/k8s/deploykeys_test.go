// SPDX-License-Identifier: Apache-2.0

package k8s_test

import (
	"testing"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	k8sfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/k8s"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The correlation key a Kubernetes rollout carries, and the one it refuses (004 T136, FR-041, SC-017).
//
// C8 compares two change observations. Cloud Run got its side in T135; this is the cluster's, and
// without it a GitHub deployment targeting a Kubernetes workload has nothing to agree with.
//
// The refusal is the half worth testing hardest. `:latest` is shared by every workload that has ever run
// it, so a key minted from a tag would be carried by every rollout in every cluster — and C8, which is
// CERTAIN, would merge two unrelated workloads with no human in the loop.

func TestADigestPinnedImageMintsADeployImageKey(t *testing.T) {
	t.Parallel()
	const digest = "sha256:0000000000000000000000000000000000000000000000000000000000000042"
	keys := k8sfeeder.RolloutDeployCorrelations("europe-docker.pkg.dev/twin/shop/api@" + digest)
	if len(keys) != 1 {
		t.Fatalf("got %d keys, want exactly one deploy.image", len(keys))
	}
	if keys[0].Namespace != feeder.NSDeployImage {
		t.Errorf("namespace is %q, want %q", keys[0].Namespace, feeder.NSDeployImage)
	}
	if want := "europe-docker.pkg.dev/twin/shop/api@" + digest; keys[0].Value != want {
		t.Errorf("value is %q, want %q — C8 compares these byte for byte across sources (SC-017)",
			keys[0].Value, want)
	}
	if keys[0].Why == "" {
		t.Error("the key states no reason; the audit prints it as the merge's evidence")
	}
}

// A tag preceding the digest is dropped, so `repo:v3@sha256:…` and `repo@sha256:…` are ONE image.
// Two sources that spell the same image differently would otherwise fail to join.
func TestATagBeforeTheDigestDoesNotChangeTheValue(t *testing.T) {
	t.Parallel()
	const digest = "sha256:0000000000000000000000000000000000000000000000000000000000000042"
	withTag := k8sfeeder.RolloutDeployCorrelations("ghcr.io/twin/api:v3@" + digest)
	without := k8sfeeder.RolloutDeployCorrelations("ghcr.io/twin/api@" + digest)
	if len(withTag) != 1 || len(without) != 1 {
		t.Fatalf("both forms must mint a key: %d and %d", len(withTag), len(without))
	}
	if withTag[0].Value != without[0].Value {
		t.Errorf("the tag changed the value: %q vs %q; the same image from two sources would not join",
			withTag[0].Value, without[0].Value)
	}
}

// And the refusals. Each of these states nothing immutable, so each must mint NOTHING rather than a
// tag-shaped guess (FR-041).
func TestAWorkloadWithNoImmutableImageMintsNoKey(t *testing.T) {
	t.Parallel()
	for _, image := range []string{
		"",                        // no container image at all
		"shop/api",                // a bare name: implicitly :latest
		"shop/api:latest",         // the shared-by-everything case
		"shop/api:v1.4.2",         // a version tag is still mutable; a retag moves it
		"shop/api@sha256:",        // a digest algorithm with no hex
		"shop/api@sha256:nothex!", // not a digest
		"shop/api@:0000",          // no algorithm
		"@sha256:0000",            // no name
	} {
		if keys := k8sfeeder.RolloutDeployCorrelations(image); len(keys) != 0 {
			t.Errorf("%q minted %d key(s) (%q); a value several workloads share would let C8 merge two "+
				"unrelated workloads in two unrelated clusters, certainly and with no human in the loop",
				image, len(keys), keys[0].Value)
		}
	}
}

// The cluster's key and Cloud Run's are the same function of the same image, which is what makes the two
// sides of C8 comparable at all. Asserting it here rather than trusting it keeps the two from drifting:
// either feeder could have grown its own normalisation.
func TestTheClusterAndCloudRunAgreeOnAnImageValue(t *testing.T) {
	t.Parallel()
	const image = "europe-docker.pkg.dev/twin-production/twin/storefront@sha256:" +
		"0000000000000000000000000000000000000000000000000000000000000042"
	shared, ok := feeder.Image(image)
	if !ok {
		t.Fatal("the shared normaliser refused a digest-pinned image; this test is checking the wrong input")
	}
	keys := k8sfeeder.RolloutDeployCorrelations(image)
	if len(keys) != 1 || keys[0].Value != shared {
		t.Errorf("the cluster minted %v, the shared normaliser says %q; C8's two sides would not join",
			keys, shared)
	}
}

// And the key actually reaches the stream (004 T136).
//
// The two tests above check the function; this one checks that the feeder CALLS it. That distinction is
// the one this project keeps paying for: C4, C5, C7 and `Skew` were each correct functions nothing
// invoked, and a unit test on the function passes either way. A rollout whose correlation is never
// emitted gives C8 nothing to compare, exactly as if the function did not exist.
func TestARolloutEmitsItsDeployCorrelation(t *testing.T) {
	const digest = "sha256:0000000000000000000000000000000000000000000000000000000000000042"

	for _, tc := range []struct {
		name    string
		image   string
		wantKey bool
	}{
		{"a digest-pinned image is correlated", "ghcr.io/twin/payments@" + digest, true},
		{"a mutable tag is not", "nginx:1.31.6-alpine-slim", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := testFeeder(t)
			before := paymentsDeployment()
			mapOne(t, f, k8sfeeder.KindDeployments, k8sfeeder.EventAdded, before, fixtureStart)

			after := paymentsDeployment()
			after.ResourceVersion = "1050"
			after.Annotations[k8sfeeder.AnnotationRevision] = "8"
			after.Spec.Template.Spec.Containers[0].Image = tc.image
			at := fixtureStart.Add(5 * time.Minute)
			events := mapOne(t, f, k8sfeeder.KindDeployments, k8sfeeder.EventModified, after, at)

			change, ok := changeOf(events, graphv1.ChangeKind_ROLLOUT)
			if !ok {
				t.Fatal("no ROLLOUT change, so this test is not exercising a rollout at all")
			}

			var found []*graphv1.CorrelateEntity
			for _, e := range events {
				if body := e.GetCorrelateEntity(); body != nil {
					found = append(found, body)
				}
			}
			if !tc.wantKey {
				if len(found) != 0 {
					t.Fatalf("a mutable tag emitted %d correlation(s); the first is %s=%s", len(found),
						found[0].GetKey().GetNamespace(), found[0].GetKey().GetValue())
				}
				return
			}
			if len(found) != 1 {
				t.Fatalf("got %d correlation events, want exactly one; C8 has nothing to compare without it",
					len(found))
			}
			got := found[0]
			if ns := got.GetKey().GetNamespace(); ns != feeder.NSDeployImage {
				t.Errorf("key namespace = %q, want %q", ns, feeder.NSDeployImage)
			}
			if want := "ghcr.io/twin/payments@" + digest; got.GetKey().GetValue() != want {
				t.Errorf("key value = %q, want %q", got.GetKey().GetValue(), want)
			}
			// The subject is the CHANGE, not the workload. C8 compares two change observations, so a key
			// attached to the workload would be compared against nothing.
			if s := feeder.RefString(got.GetSubject()); s != feeder.RefString(change.GetRef()) {
				t.Errorf("subject = %q, want the rollout change %q; C8 compares CHANGES",
					s, feeder.RefString(change.GetRef()))
			}
			// And the environment, which C8 requires to agree and reads from here.
			if env := got.GetAttributes().GetFields()[feeder.AttrDeploymentEnvironment].GetStringValue(); env == "" {
				t.Errorf("the key states no %s; C8 refuses a pair whose environments do not agree, so a "+
					"key without one can never merge", feeder.AttrDeploymentEnvironment)
			}
		})
	}
}
