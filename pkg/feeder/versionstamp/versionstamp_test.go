// SPDX-License-Identifier: Apache-2.0

package versionstamp_test

import (
	"strings"
	"testing"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/versionstamp"
)

// The share test (005 T029–T030) and the normaliser (T031–T032).

func candidate(name string, form versionstamp.Form) versionstamp.Candidate {
	for _, c := range versionstamp.Conventions {
		if c.Name == name && c.Form == form {
			return c
		}
	}
	panic("no such convention: " + name)
}

// The audit's case: the SDK's own `version` attribute on 255 of 37.2 M lines, all start-up lines, none
// of the 3 606 error lines. Rejected, with its share stated — and a real stamp beside it is accepted.
func TestTheAuditsSDKVersionIsRejectedByShare(t *testing.T) {
	t.Parallel()
	totals := versionstamp.Totals{Lines: 37_199_813, ErrorLines: 3_606}
	sdk := versionstamp.Measurement{Candidate: candidate("version", versionstamp.FormAttribute), Lines: 255}
	v := versionstamp.Decide([]versionstamp.Measurement{sdk}, totals, versionstamp.DefaultThresholds(),
		versionstamp.DefaultWindow, versionstamp.SourceDiscovered)
	if v.Stamped() {
		t.Fatalf("the SDK version was accepted: %s", v)
	}
	if !strings.Contains(v.String(), "0.0007% of lines") {
		t.Errorf("the verdict does not state the share it measured: %s", v)
	}
}

// Stability is not a criterion: one value on every line — a service that did not deploy — is accepted.
// The first candidate in the list that passes wins, and the ones after it are marked not needed.
func TestAConstantStampOnEveryLineIsAccepted(t *testing.T) {
	t.Parallel()
	totals := versionstamp.Totals{Lines: 10_000, ErrorLines: 40}
	measured := []versionstamp.Measurement{
		{Candidate: candidate("version", versionstamp.FormTag), Lines: 10_000, ErrorLines: 40},
		{Candidate: candidate("service.version", versionstamp.FormAttribute), Lines: 10_000, ErrorLines: 40},
	}
	v := versionstamp.Decide(measured, totals, versionstamp.DefaultThresholds(), versionstamp.DefaultWindow,
		versionstamp.SourceDiscovered)
	if v.Attribute != "version (tag)" {
		t.Fatalf("accepted %q, want the first passing candidate `version (tag)`: %s", v.Attribute, v)
	}
	if v.Candidates[1].Accepted || !strings.Contains(v.Candidates[1].Reason, "not needed") {
		t.Errorf("the second candidate: %+v", v.Candidates[1])
	}
}

// A stamp missing from the error lines is refused: errors would fall into an unlabelled group.
func TestAStampMissingFromErrorLinesIsRejected(t *testing.T) {
	t.Parallel()
	totals := versionstamp.Totals{Lines: 10_000, ErrorLines: 100}
	m := versionstamp.Measurement{Candidate: candidate("version", versionstamp.FormTag), Lines: 9_950, ErrorLines: 10}
	if v := versionstamp.Decide([]versionstamp.Measurement{m}, totals, versionstamp.DefaultThresholds(),
		versionstamp.DefaultWindow, versionstamp.SourceDiscovered); v.Stamped() {
		t.Fatalf("a stamp on 10%% of error lines was accepted: %s", v)
	}
}

// A window with no lines measures nothing, and says so rather than accepting or rejecting on zero.
func TestAnEmptyWindowDecidesNothing(t *testing.T) {
	t.Parallel()
	m := versionstamp.Measurement{Candidate: candidate("version", versionstamp.FormTag)}
	v := versionstamp.Decide([]versionstamp.Measurement{m}, versionstamp.Totals{}, versionstamp.DefaultThresholds(),
		versionstamp.DefaultWindow, versionstamp.SourceDiscovered)
	if v.Stamped() || !strings.Contains(v.Candidates[0].Reason, "no lines") {
		t.Errorf("an empty window: %s", v)
	}
}

// Every value form, and every refusal.
func TestNormaliseEveryForm(t *testing.T) {
	t.Parallel()
	sha := "8F5B3C0D1E2A4B6C8D0E2F4A6B8C0D2E4F6A8B0C"
	cases := []struct {
		value  string
		ns     string
		reason investigationv1.DeployRefAbsentReason
	}{
		{sha, feeder.NSDeployCommitSHA, 0},
		{"europe-docker.pkg.dev/p/r/app@sha256:" + strings.Repeat("ab", 32), feeder.NSDeployImage, 0},
		{"v2.3.0-RC1", feeder.NSDeployRelease, 0},
		{"1.3.6", feeder.NSDeployRelease, 0},
		{"a1b2c3d", "", investigationv1.DeployRefAbsentReason_ABBREVIATED_SHA},
		{"repo/app:latest", "", investigationv1.DeployRefAbsentReason_MUTABLE_TAG},
		{"sha256:" + strings.Repeat("ab", 32), "", investigationv1.DeployRefAbsentReason_BARE_DIGEST},
		{"", "", investigationv1.DeployRefAbsentReason_NOT_A_STABLE_IDENTIFIER},
		{"two words", "", investigationv1.DeployRefAbsentReason_NOT_A_STABLE_IDENTIFIER},
	}
	for _, c := range cases {
		ref, reason := versionstamp.Normalise(c.value)
		if ref.GetNamespace() != c.ns || reason != c.reason {
			t.Errorf("%q → (%v, %v), want (%q, %v)", c.value, ref, reason, c.ns, c.reason)
		}
	}
	if ref, _ := versionstamp.Normalise(sha); ref.GetValue() != strings.ToLower(sha) {
		t.Errorf("a commit sha is not lower-cased: %v", ref)
	}
	if ref, reason := versionstamp.NormalisePair("europe-docker.pkg.dev/p/r/app", "sha256:"+strings.Repeat("cd", 32)); ref.GetNamespace() != feeder.NSDeployImage || reason != 0 {
		t.Errorf("an image name and digest pair: (%v, %v)", ref, reason)
	}
	if _, reason := versionstamp.NormalisePair("", "sha256:"+strings.Repeat("cd", 32)); reason != investigationv1.DeployRefAbsentReason_BARE_DIGEST {
		t.Errorf("a digest without a name: %v", reason)
	}
}
