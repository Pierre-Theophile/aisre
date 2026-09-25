// SPDX-License-Identifier: Apache-2.0

package backend_test

import (
	"strings"
	"testing"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
)

// Redaction (tasks.md T035, FR-038, ADR-0003 D9).

func redactor(t *testing.T) *engine.Redactor {
	t.Helper()
	r, err := engine.NewRedactor(engine.DefaultRedactionPolicy(), []byte("test-key"))
	if err != nil {
		t.Fatalf("new redactor: %v", err)
	}
	return r
}

// TestPeopleIdentifiersAreDroppedNotHashed: a stable hash of a person is still a person. It
// joins across every digest in the corpus and is re-identifiable from any one known example.
func TestPeopleIdentifiersAreDroppedNotHashed(t *testing.T) {
	t.Parallel()

	resp := &engine.Response{Digest: &investigationv1.Digest{
		Body: &investigationv1.Digest_Metric{Metric: &investigationv1.MetricDigest{
			Series: []*investigationv1.SeriesSummary{{
				Tags: map[string]string{
					"env":          "prod",
					"user.email":   "alice@example.com",
					"userEmail":    "bob@example.com",
					"triggered_by": "carol",
					"pod":          "checkout-7f9",
				},
				JoinKeys: &investigationv1.JoinKeys{
					Version:   "rev7",
					Workload:  "checkout",
					PodOrHost: "checkout-7f9",
				},
			}},
		}},
		Coverage: coverage(t),
	}}

	r := redactor(t)
	if err := r.Apply(resp); err != nil {
		t.Fatalf("apply: %v", err)
	}
	tags := resp.GetDigest().GetMetric().GetSeries()[0].GetTags()

	for key := range tags {
		if engine.IsPeopleAttribute(key) {
			t.Errorf("tag %q names a person and survived redaction", key)
		}
	}
	for key, value := range tags {
		if strings.Contains(value, "alice") || strings.Contains(value, "bob") || strings.Contains(value, "carol") {
			t.Errorf("tag %s=%q still carries a person's identifier", key, value)
		}
	}
	if tags["env"] != "prod" {
		t.Errorf("env tag = %q; a tag that is not about a person is what a comparison is about and must survive", tags["env"])
	}
}

// TestInfrastructureIdentifiersJoin: pseudonymisation is consistent, so the same pod is the same
// pseudonym in every digest of one recording. A digest whose keys do not join is evidence about
// nothing.
func TestInfrastructureIdentifiersJoin(t *testing.T) {
	t.Parallel()
	r := redactor(t)

	first := r.Pseudonymise("checkout-7f9")
	second := r.Pseudonymise("checkout-7f9")
	other := r.Pseudonymise("checkout-8a1")

	if first != second {
		t.Errorf("the same pod produced two pseudonyms (%s, %s); joins would not survive", first, second)
	}
	if first == other {
		t.Errorf("two different pods produced one pseudonym (%s)", first)
	}
	if !strings.HasPrefix(first, engine.PseudonymPrefix) {
		t.Errorf("pseudonym %q is not marked as one; a reader must never paste it into a vendor console", first)
	}
	if strings.Contains(first, "checkout") {
		t.Errorf("pseudonym %q still carries the original", first)
	}
	if again := r.Pseudonymise(first); again != first {
		t.Errorf("pseudonymising a pseudonym changed it (%s → %s); applying the policy twice must be a no-op", first, again)
	}

	// A different recording is a different key and therefore a different pseudonym: a corpus
	// cannot be joined across fixtures by infrastructure identity.
	elsewhere, err := engine.NewRedactor(engine.DefaultRedactionPolicy(), []byte("another-recording"))
	if err != nil {
		t.Fatalf("new redactor: %v", err)
	}
	if elsewhere.Pseudonymise("checkout-7f9") == first {
		t.Error("two recordings produced the same pseudonym for one pod; the HMAC key is what keeps them apart")
	}
}

// TestVersionSurvivesRedaction: the deploy revision is what joins a metric to a change in the
// graph, and ADR-0005 D6 exists to make that join possible. Pseudonymising it would break the
// single most decisive piece of evidence in a rollout regression.
func TestVersionSurvivesRedaction(t *testing.T) {
	t.Parallel()

	resp := &engine.Response{Digest: &investigationv1.Digest{
		Body: &investigationv1.Digest_ErrorsByVersion{ErrorsByVersion: &investigationv1.ErrorsByVersionDigest{
			Versions: []*investigationv1.VersionBreakdown{{
				Version:  "rev7",
				JoinKeys: &investigationv1.JoinKeys{Version: "rev7", PodOrHost: "checkout-7f9"},
			}},
		}},
		Coverage: coverage(t),
	}}
	if err := redactor(t).Apply(resp); err != nil {
		t.Fatalf("apply: %v", err)
	}

	keys := resp.GetDigest().GetErrorsByVersion().GetVersions()[0].GetJoinKeys()
	if keys.GetVersion() != "rev7" {
		t.Errorf("version join key = %q, want rev7; it is what joins a digest to a change", keys.GetVersion())
	}
	if !strings.HasPrefix(keys.GetPodOrHost(), engine.PseudonymPrefix) {
		t.Errorf("pod join key = %q, want a pseudonym", keys.GetPodOrHost())
	}
}

func TestMaskLine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		line string
		want string
	}{
		{"email", "login failed for alice@example.com", "login failed for <email>"},
		{"ip", "connection refused from 10.1.2.3", "connection refused from <ip>"},
		{"uuid", "request 3f2504e0-4f89-11d3-9a0c-0305e82c3301 failed", "request <uuid> failed"},
		{"duration", "upstream took 251ms", "upstream took <dur>"},
		{"number", "returned status 503 after 3 retries", "returned status <num> after <num> retries"},
		{"url", "GET https://payments.internal/orders failed", "GET <url> failed"},
		{"already masked", "upstream took <dur>", "upstream took <dur>"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := engine.MaskLine(tc.line)
			if got != tc.want {
				t.Errorf("MaskLine(%q) = %q, want %q", tc.line, got, tc.want)
			}
			if again := engine.MaskLine(got); again != got {
				t.Errorf("masking is not idempotent: %q → %q", got, again)
			}
		})
	}
}

// TestUndeclaredRedactionIsRejected: a backend that emits a field its declaration does not cover
// is refused at the boundary, not in a corpus review months later.
func TestUndeclaredRedactionIsRejected(t *testing.T) {
	t.Parallel()

	t.Run("a people identifier in a series tag", func(t *testing.T) {
		t.Parallel()
		r := redactor(t)
		resp := &engine.Response{Digest: &investigationv1.Digest{
			Body: &investigationv1.Digest_Metric{Metric: &investigationv1.MetricDigest{
				Series: []*investigationv1.SeriesSummary{{Tags: map[string]string{"user.email": "alice@example.com"}}},
			}},
			Coverage: coverage(t),
		}}
		// CheckDeclared is called directly: Apply would have dropped the tag first, which is
		// the point — the check is what catches a backend that bypassed Apply.
		err := r.CheckDeclared(resp)
		if engine.ReasonOf(err) != engine.ReasonUndeclaredRedaction {
			t.Fatalf("reason = %q, want %q (%v)", engine.ReasonOf(err), engine.ReasonUndeclaredRedaction, err)
		}
	})

	t.Run("a log digest under a policy that does not template log bodies", func(t *testing.T) {
		t.Parallel()
		policy := engine.DefaultRedactionPolicy()
		policy.LogBodiesAsTemplates = false
		r, err := engine.NewRedactor(policy, []byte("test-key"))
		if err != nil {
			t.Fatalf("new redactor: %v", err)
		}
		resp := &engine.Response{Digest: &investigationv1.Digest{
			Body: &investigationv1.Digest_Log{Log: &investigationv1.LogDigest{
				Patterns: []*investigationv1.LogPattern{{Template: "anything"}},
			}},
			Coverage: coverage(t),
		}}
		if reason := engine.ReasonOf(r.CheckDeclared(resp)); reason != engine.ReasonUndeclaredRedaction {
			t.Fatalf("reason = %q, want %q", reason, engine.ReasonUndeclaredRedaction)
		}
	})

	t.Run("an unmasked value in a template", func(t *testing.T) {
		t.Parallel()
		r := redactor(t)
		resp := &engine.Response{Digest: &investigationv1.Digest{
			Body: &investigationv1.Digest_Log{Log: &investigationv1.LogDigest{
				Patterns: []*investigationv1.LogPattern{{Template: "login failed for alice@example.com"}},
			}},
			Coverage: coverage(t),
		}}
		if reason := engine.ReasonOf(r.CheckDeclared(resp)); reason != engine.ReasonUndeclaredRedaction {
			t.Fatalf("reason = %q, want %q", reason, engine.ReasonUndeclaredRedaction)
		}
		// Applying the policy first fixes it: the template is masked before the check.
		if err := r.Apply(resp); err != nil {
			t.Fatalf("apply: %v", err)
		}
	})

	t.Run("a monitor body carried as free text", func(t *testing.T) {
		t.Parallel()
		r := redactor(t)
		resp := &engine.Response{Digest: &investigationv1.Digest{
			Body:     &investigationv1.Digest_MonitorState{MonitorState: &investigationv1.MonitorStateDigest{}},
			FreeText: "runbook: page the on-call and restart the pods",
			Coverage: coverage(t),
		}}
		if reason := engine.ReasonOf(r.CheckDeclared(resp)); reason != engine.ReasonUndeclaredRedaction {
			t.Fatalf("reason = %q, want %q", reason, engine.ReasonUndeclaredRedaction)
		}
		if err := r.Apply(resp); err != nil {
			t.Fatalf("apply: %v", err)
		}
		if resp.GetDigest().GetFreeText() != "" {
			t.Errorf("the monitor body survived redaction: %q", resp.GetDigest().GetFreeText())
		}
	})
}

// TestARedactorNeedsAVersionAndAKey: a recording must say what was applied to it, and an unkeyed
// pseudonym is a hash anybody can reverse by enumeration.
func TestARedactorNeedsAVersionAndAKey(t *testing.T) {
	t.Parallel()

	if _, err := engine.NewRedactor(&investigationv1.RedactionPolicy{}, []byte("k")); engine.ReasonOf(err) != engine.ReasonUndeclaredRedaction {
		t.Errorf("a policy with no version was accepted: %v", err)
	}
	if _, err := engine.NewRedactor(engine.DefaultRedactionPolicy(), nil); engine.ReasonOf(err) != engine.ReasonUndeclaredRedaction {
		t.Errorf("a redactor with no key was accepted: %v", err)
	}
}
