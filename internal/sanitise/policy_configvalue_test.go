// SPDX-License-Identifier: Apache-2.0

package sanitise_test

import (
	"testing"
)

// An environment variable's value, in every place Vercel sends it (004 T095; FR-038, SC-006).
//
// The connector's own `ProjectEnv` struct has no field for any of these, so nothing reaches the graph.
// That is not the same guarantee as nothing reaching DISK: `--record` writes the response bytes, and the
// published response schema lists `value` among its REQUIRED fields and ships `legacyValue` and
// `internalContentHint.encryptedValue` beside it. `env[].value` — GCP's spelling — matched none of them,
// so the first Vercel recording would have committed a live secret to a corpus.
//
// Dropped rather than pseudonymised, and the difference matters here more than anywhere else in the
// table: a stable pseudonym for a value would let anybody holding the corpus ask "is this the same
// secret as that one", and would be a hash of a credential, which SC-006 counts among what must be zero.
func TestEveryCarrierOfAConfigurationValueIsDropped(t *testing.T) {
	s := newSanitiser(t)
	// The value, in each place the platform puts it, plus the free-text field beside it where a value
	// gets pasted by hand.
	for _, field := range []string{
		"envs[].value",
		"envs[].legacyValue",
		"envs[].internalContentHint.encryptedValue",
		"envs[].internalContentHint.type",
		"envs[].contentHint.storeId",
		"envs[].comment",
		"envs[].edgeConfigTokenId",
		"envs[].sunsetSecretId",
	} {
		got, keep, err := s.Field("a vercel environment variable", field,
			"postgres://payments:hunter2@db.internal:5432/payments")
		if err != nil {
			t.Fatalf("%s: %v", field, err)
		}
		if keep {
			t.Errorf("%s was kept as %q. FR-038 refuses the value plaintext, ciphertext, truncated or "+
				"hashed, and this field is one of the three the platform sends it in", field, got)
		}
		if got != "" {
			t.Errorf("%s produced %q rather than nothing. A pseudonym of a secret is a stable token for "+
				"that secret, and a hash of one is a hash SC-006 counts", field, got)
		}
	}
}

// Not vacuous: the metadata a configuration change is actually built from survives. A policy that
// dropped the whole payload would pass the test above and leave nothing to record.
func TestTheConfigurationMetadataSurvivesTheValueBeingDropped(t *testing.T) {
	s := newSanitiser(t)
	for field, value := range map[string]string{
		"envs[].key":    "PAYMENTS_API_URL",
		"envs[].type":   "encrypted",
		"envs[].target": "production",
	} {
		got, keep, err := s.Field("a vercel environment variable", field, value)
		if err != nil {
			t.Fatalf("%s: %v", field, err)
		}
		if !keep || got != value {
			t.Errorf("%s=%q recorded as %q (keep=%t); the KEY is what makes a configuration change "+
				"actionable, and a corpus without it cannot test FR-038 at all", field, value, got, keep)
		}
	}
}
