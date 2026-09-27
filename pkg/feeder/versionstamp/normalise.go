// SPDX-License-Identifier: Apache-2.0

package versionstamp

import (
	"strings"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// From a stamped value to a deploy reference (005 FR-040d, T031; contract §4).
//
// The value is normalised with pkg/feeder/deployref.go and nothing else, so a group names exactly the
// identifier a deploy feeder's change claims — whichever feeder that is. The order matters, because
// several forms overlap: a 40-hex string is also a valid release identifier, and the more specific
// reading must win.
//
// A value that yields no reference is still a group; the reason says why, and nothing is guessed. An
// abbreviated sha in particular is never padded or prefix-matched: a 7-hex prefix is shared by many
// commits across an organisation, and a reference on it would join rollouts of different code.

// Normalise turns one stamped value into its deploy reference, or the reason it has none.
func Normalise(value string) (*graphv1.Ref, investigationv1.DeployRefAbsentReason) {
	v := strings.TrimSpace(value)
	if ref, ok := feeder.CommitSHARef(v); ok {
		return ref, investigationv1.DeployRefAbsentReason_DEPLOY_REF_ABSENT_REASON_UNSPECIFIED
	}
	if ref, ok := feeder.ImageRef(v); ok {
		return ref, investigationv1.DeployRefAbsentReason_DEPLOY_REF_ABSENT_REASON_UNSPECIFIED
	}
	if reason, refused := refusal(v); refused {
		return nil, reason
	}
	if ref, ok := feeder.ReleaseRef(v); ok {
		return ref, investigationv1.DeployRefAbsentReason_DEPLOY_REF_ABSENT_REASON_UNSPECIFIED
	}
	return nil, investigationv1.DeployRefAbsentReason_NOT_A_STABLE_IDENTIFIER
}

// NormalisePair turns candidate 5's two attributes — an image name and its digest — into a reference.
// A digest alone is BARE_DIGEST: the image normaliser needs the name, and a digest without one could
// be any image.
func NormalisePair(name, digest string) (*graphv1.Ref, investigationv1.DeployRefAbsentReason) {
	n, d := strings.TrimSpace(name), strings.TrimSpace(digest)
	if n == "" {
		return nil, investigationv1.DeployRefAbsentReason_BARE_DIGEST
	}
	if ref, ok := feeder.ImageRef(n + "@" + d); ok {
		return ref, investigationv1.DeployRefAbsentReason_DEPLOY_REF_ABSENT_REASON_UNSPECIFIED
	}
	return nil, investigationv1.DeployRefAbsentReason_MUTABLE_TAG
}

// refusal names the forms that look like a deploy identifier and must not become one.
func refusal(v string) (investigationv1.DeployRefAbsentReason, bool) {
	lower := strings.ToLower(v)
	switch {
	case v == "" || strings.ContainsAny(v, " \t\n\r"):
		return investigationv1.DeployRefAbsentReason_NOT_A_STABLE_IDENTIFIER, true
	case len(lower) >= 7 && len(lower) < 40 && isHex(lower):
		// The UI's short sha. Not padded, not prefix-matched (deployref.go).
		return investigationv1.DeployRefAbsentReason_ABBREVIATED_SHA, true
	case strings.HasPrefix(lower, "sha256:") || strings.HasPrefix(lower, "sha512:"):
		return investigationv1.DeployRefAbsentReason_BARE_DIGEST, true
	case strings.Contains(v, "/") && !strings.Contains(v, "@"):
		// An image reference with a tag, or none: `repo/app:latest` is shared by every rollout of it.
		return investigationv1.DeployRefAbsentReason_MUTABLE_TAG, true
	}
	return 0, false
}

func isHex(s string) bool {
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
