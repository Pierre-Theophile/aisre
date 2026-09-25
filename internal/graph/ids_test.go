// SPDX-License-Identifier: Apache-2.0

package graph_test

import (
	"regexp"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/graph"
)

// The strings pinned in this file are a CONTRACT, not test fixtures.
//
// Every entity, version and claim id in every fixture, every golden file and every deployed
// database is derived by these functions. Replay must be byte-identical (FR-023), so changing
// any of these values — by changing the hash, the 0x00 separator, the base32 alphabet, the
// case or the 26-character truncation — is a BREAKING CHANGE to the published schema
// (constitution IX). It requires a major version bump, a changelog entry and a migration
// expressed as an event-log transformation. If one of these tests fails, the fix is almost
// never to update the expected string.

const (
	checkoutEntityID = "xvi7oufivacscbjrosgfu4k6f5" // EntityID("otel.service.name", "checkout")
	paymentsEntityID = "qek4qgjtjjntsp7ly47jv2b3c5" // EntityID("otel.service.name", "payments")
)

func TestEntityIDGolden(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		namespace string
		value     string
		want      string
	}{
		{"otel service", "otel.service.name", "checkout", checkoutEntityID},
		{"otel service payments", "otel.service.name", "payments", paymentsEntityID},
		{"k8s deployment", "k8s.deployment", "demo/checkout-svc", "r6hm5ik7lb7wydczmn2mff7xwj"},
		{"empty", "", "", "ny2axhh7wn5jrhffittlw6akfr"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := graph.EntityID(tc.namespace, tc.value); got != tc.want {
				t.Errorf("EntityID(%q, %q) = %q, want %q (see the contract note in this file)",
					tc.namespace, tc.value, got, tc.want)
			}
		})
	}
}

func TestVersionIDGolden(t *testing.T) {
	t.Parallel()

	const want = "cmh3lpki4sx7jrlxrne3rjzkgz"
	if got := graph.VersionID(checkoutEntityID, "otel:demo:evt-1"); got != want {
		t.Errorf("VersionID = %q, want %q", got, want)
	}
}

func TestEdgeVersionIDGolden(t *testing.T) {
	t.Parallel()

	const want = "befb7galcg6dmlkoexu57fqqis"
	got := graph.EdgeVersionID(checkoutEntityID, paymentsEntityID, string(graph.EdgeTypeCalls), "otel:demo:evt-2")
	if got != want {
		t.Errorf("EdgeVersionID = %q, want %q", got, want)
	}
}

func TestClaimIDGolden(t *testing.T) {
	t.Parallel()

	const want = "lpjg7xcf5wypczdra5spyuwxpp"
	if got := graph.ClaimID("otel.service.name", "checkout", "otel:demo"); got != want {
		t.Errorf("ClaimID = %q, want %q", got, want)
	}
}

// idAlphabet is the second half of the contract: ids are lowercase base32 (RFC 4648, no
// padding) of exactly IDLength characters, so they are safe in URLs, file names, Postgres text
// columns and the `min|max` pair key.
var idAlphabet = regexp.MustCompile(`^[a-z2-7]{26}$`)

func TestIDShape(t *testing.T) {
	t.Parallel()

	ids := map[string]string{
		"EntityID":      graph.EntityID("otel.service.name", "checkout"),
		"VersionID":     graph.VersionID(checkoutEntityID, "otel:demo:evt-1"),
		"EdgeVersionID": graph.EdgeVersionID(checkoutEntityID, paymentsEntityID, "calls", "otel:demo:evt-2"),
		"ClaimID":       graph.ClaimID("otel.service.name", "checkout", "otel:demo"),
		// Values with unusual bytes must still produce a well-formed id.
		"unicode": graph.EntityID("k8s.namespace", "données/étoile-∞"),
		"long":    graph.EntityID("otel.service.name", string(make([]byte, 4096))),
	}
	for name, id := range ids {
		if len(id) != graph.IDLength {
			t.Errorf("%s: length %d, want %d", name, len(id), graph.IDLength)
		}
		if !idAlphabet.MatchString(id) {
			t.Errorf("%s: %q is not %s", name, id, idAlphabet)
		}
	}
}

func TestIDsAreDeterministicAndDistinct(t *testing.T) {
	t.Parallel()

	const ns, value = "otel.service.name", "checkout"
	first := graph.EntityID(ns, value)
	for range 100 {
		if got := graph.EntityID(ns, value); got != first {
			t.Fatalf("EntityID is not deterministic: %q then %q", first, got)
		}
	}

	// The 0x00 separator keeps concatenations apart: ("ab","c") and ("a","bc") must differ.
	if graph.EntityID("ab", "c") == graph.EntityID("a", "bc") {
		t.Error("EntityID: separator does not prevent a concatenation collision")
	}
	if graph.ClaimID("a", "b", "c") == graph.ClaimID("a", "b", "cx") {
		t.Error("ClaimID: source is not part of the digest")
	}
	// Note: the id kinds share one construction, so EntityID("a","b") and VersionID("a","b")
	// are the same string by design (research §4 specifies no kind tag). Ids are scoped per
	// column — an entity id is never compared against a version id — and the real inputs do
	// not overlap: an entity id is hashed from a namespace and a value, a version id from a
	// 26-character entity id and an event id.
	if graph.EdgeVersionID("a", "b", "c", "d") == graph.ClaimID("a", "b", "c") {
		t.Error("arity is not reflected in the digest")
	}
}

func TestRefEntityID(t *testing.T) {
	t.Parallel()

	ref := graph.Ref{Namespace: "otel.service.name", Value: "checkout"}
	if got := ref.EntityID(); got != checkoutEntityID {
		t.Errorf("Ref.EntityID() = %q, want %q", got, checkoutEntityID)
	}
}

func TestPairKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		a, b string
		want string
	}{
		{"ordered", "aaa", "bbb", "aaa|bbb"},
		{"reversed", "bbb", "aaa", "aaa|bbb"},
		{"equal", "aaa", "aaa", "aaa|aaa"},
		{"real ids", checkoutEntityID, paymentsEntityID, paymentsEntityID + "|" + checkoutEntityID},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := graph.PairKey(tc.a, tc.b)
			if got != tc.want {
				t.Errorf("PairKey(%q, %q) = %q, want %q", tc.a, tc.b, got, tc.want)
			}
			if rev := graph.PairKey(tc.b, tc.a); rev != got {
				t.Errorf("PairKey is not symmetric: %q vs %q", got, rev)
			}
		})
	}
}
