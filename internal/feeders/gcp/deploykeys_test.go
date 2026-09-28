// SPDX-License-Identifier: Apache-2.0

package gcp_test

import (
	"strings"
	"testing"

	gcpfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/gcp"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/versionstamp"
)

// The cross-source deploy correlation keys on a Cloud Run rollout (004 T135, T148; FR-041, FR-045,
// SC-004).
//
// These carry C8's platform side. Without them SC-004 is unmeetable: C8 compares two change
// observations, and until this landed no platform feeder minted a `deploy.*` key at all — so the
// rule would have been published, registered, evaluated on every claim and never fired, like C4, C5
// and C7 before their cross-source fixtures existed.
//
// They are correlation keys and not identity claims, which T148 corrected: a commit describes a rollout
// rather than naming one, and the event log now refuses these namespaces as claims. The type is the
// assertion — `[]feeder.CorrelationKey` cannot be handed to an identity emitter — so these tests check
// the values, and pkg/feeder/correlation_test.go checks the kind.

const (
	keyedCommit = "9f8e7d6c5b4a39281706f5e4d3c2b1a098765432"
	keyedImage  = "europe-docker.pkg.dev/twin-production/twin/storefront"
	keyedDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000042"
)

func revisionObservation(image, commit, commitLabel string) gcpfeeder.RevisionObservation {
	obs := gcpfeeder.RevisionObservation{Image: image}
	obs.Labels.Commit = commit
	obs.Labels.CommitSource = commitLabel
	return obs
}

func keyValue(t *testing.T, keys []feeder.CorrelationKey, namespace string) (string, bool) {
	t.Helper()
	for _, key := range keys {
		if key.Namespace == namespace {
			return key.Value, true
		}
	}
	return "", false
}

// A digest-pinned image and a declared commit both become keys, in the shared vocabulary and in
// pkg/feeder's normalised form.
func TestARolloutCorrelatesOnTheCommitAndTheDigestPinnedImage(t *testing.T) {
	t.Parallel()

	keys := gcpfeeder.RolloutDeployKeys(
		revisionObservation(keyedImage+"@"+keyedDigest, keyedCommit, "commit-sha"))

	image, ok := keyValue(t, keys, feeder.NSDeployImage)
	if !ok {
		t.Fatalf("no %s key on a rollout whose image pins a digest; C8 has no platform side without "+
			"it", feeder.NSDeployImage)
	}
	if image != keyedImage+"@"+keyedDigest {
		t.Errorf("deploy.image = %q, want the digest form %q", image, keyedImage+"@"+keyedDigest)
	}
	commit, ok := keyValue(t, keys, feeder.NSDeployCommitSHA)
	if !ok {
		t.Fatalf("no %s key although a label declared one; it is the only identifier a GitHub "+
			"rollout and a Cloud Run rollout can share", feeder.NSDeployCommitSHA)
	}
	if commit != keyedCommit {
		t.Errorf("deploy.commit_sha = %q, want %q", commit, keyedCommit)
	}
	// The reason names the label, because the commit is a convention of the operator's tooling rather
	// than an API field, and a reader of the key should be able to tell which label supplied it.
	for _, key := range keys {
		if key.Namespace == feeder.NSDeployCommitSHA && !strings.Contains(key.Why, "commit-sha") {
			t.Errorf("the commit key does not name the label it came from: %q", key.Why)
		}
	}
}

// A tag is not a digest: two rollouts running `:latest` are not one rollout, so no key is minted.
func TestARolloutRunningATagMintsNoImageKey(t *testing.T) {
	t.Parallel()

	for _, image := range []string{
		keyedImage + ":latest",
		keyedImage + ":v3.2.1",
		keyedImage,
		"",
	} {
		keys := gcpfeeder.RolloutDeployKeys(revisionObservation(image, "", ""))
		if value, ok := keyValue(t, keys, feeder.NSDeployImage); ok {
			t.Errorf("a rollout running %q minted deploy.image=%q; Cloud Run v2 states no resolved "+
				"digest anywhere, and a tag names whatever is there now rather than what was deployed",
				image, value)
		}
	}
}

// No commit label, no commit key. Omission over invention: the API states no commit, so the absence
// of the label is the absence of the fact.
func TestARolloutWithNoCommitLabelMintsNoCommitKey(t *testing.T) {
	t.Parallel()

	keys := gcpfeeder.RolloutDeployKeys(
		revisionObservation(keyedImage+"@"+keyedDigest, "", ""))
	if value, ok := keyValue(t, keys, feeder.NSDeployCommitSHA); ok {
		t.Errorf("a rollout with no commit label minted deploy.commit_sha=%q", value)
	}
	// And the image key still stands, so the two are independent rather than all-or-nothing.
	if _, ok := keyValue(t, keys, feeder.NSDeployImage); !ok {
		t.Error("the image key was dropped with the commit; the two are separate identifiers")
	}
}

// The label policy reads a commit only from an allowlisted key, and only when the value is a full hex
// object id. An abbreviation is never padded.
func TestTheCommitLabelIsReadOnlyWhenItIsAllowlistedAndFull(t *testing.T) {
	t.Parallel()

	policy := gcpfeeder.DefaultLabelPolicy()
	if got := policy.Apply("p", map[string]string{"commit-sha": keyedCommit}); got.Commit != keyedCommit {
		t.Errorf("Apply did not read the commit from the published default key: %+v", got)
	} else if got.CommitSource != "commit-sha" {
		t.Errorf("CommitSource = %q, want the key the value came from", got.CommitSource)
	}

	for name, labels := range map[string]map[string]string{
		"an abbreviation":  {"commit-sha": keyedCommit[:12]},
		"not hex":          {"commit-sha": strings.Repeat("g", 40)},
		"empty":            {"commit-sha": ""},
		"a different key":  {"revision-sha": keyedCommit},
		"no labels at all": {},
	} {
		if got := policy.Apply("p", labels); got.Commit != "" {
			t.Errorf("Apply read a commit from %s: %q", name, got.Commit)
		}
	}

	// A key that is NOT on the allowlist is not read even when it is named in CommitFromLabels: the
	// allowlist is a disposal boundary as well as an observation one, and reading around it would make
	// a claim out of a label the operator chose not to observe (FR-124, FR-137).
	unlisted := policy
	unlisted.CommitFromLabels = []string{"deploy-commit"}
	if got := unlisted.Apply("p", map[string]string{"deploy-commit": keyedCommit}); got.Commit != "" {
		t.Errorf("Apply read a commit from a key the allowlist drops: %q", got.Commit)
	}
	// Allowlisting it makes it readable, so the guard above is the allowlist and not the key name.
	allowed := unlisted
	allowed.Allowlist = append(append([]string(nil), policy.Allowlist...), "deploy-commit")
	if got := allowed.Apply("p", map[string]string{"deploy-commit": keyedCommit}); got.Commit != keyedCommit {
		t.Errorf("an allowlisted commit key was still not read: %+v", got)
	}
}

// Both namespaces are declared by the feeder's description, or testkit refuses the ref.
func TestTheDeployNamespacesAreDeclared(t *testing.T) {
	t.Parallel()

	for _, ns := range []string{feeder.NSDeployCommitSHA, feeder.NSDeployImage} {
		if !gcpfeeder.DeclaresNamespace(ns) {
			t.Errorf("%q is minted on a rollout but is not in the feeder's declared namespaces; a ref "+
				"in an undeclared namespace fails testkit", ns)
		}
	}
}

// 005 T100: every rollout carries its revision name as deploy.release, the same value the GCP
// backend's errors_by_version group and a log stamped `DD_VERSION=$K_REVISION` normalise to, so both
// resolve to the rollout. No revision name, no key.
func TestARolloutCorrelatesOnItsRevisionNameAsARelease(t *testing.T) {
	t.Parallel()

	obs := revisionObservation("", "", "")
	obs.Revision.Revision = "checkout-00042-bbb"
	keys := gcpfeeder.RolloutDeployKeys(obs)
	release, ok := keyValue(t, keys, feeder.NSDeployRelease)
	if !ok || release != "checkout-00042-bbb" {
		t.Fatalf("deploy.release = %q (%v), want the revision name", release, ok)
	}
	ref, _ := versionstamp.Normalise("checkout-00042-bbb")
	if ref.GetNamespace() != feeder.NSDeployRelease || ref.GetValue() != release {
		t.Errorf("the backend's group normalises to %v, the rollout carries %s=%s; they would not join", ref, feeder.NSDeployRelease, release)
	}
	if _, ok := keyValue(t, gcpfeeder.RolloutDeployKeys(revisionObservation("", "", "")), feeder.NSDeployRelease); ok {
		t.Error("a rollout with no revision name minted a release key")
	}
}
