// SPDX-License-Identifier: Apache-2.0

package sanitise

import (
	"strings"
	"testing"
)

func TestRedactTextRemovesPeopleAndSecrets(t *testing.T) {
	person, err := NewCanary(CanaryPerson, "section")
	if err != nil {
		t.Fatal(err)
	}
	secret, err := NewCanary(CanarySecret, "section")
	if err != nil {
		t.Fatal(err)
	}
	text := "Paged " + person.Carrier + " and @oncall-lead; the key " + secret.Carrier +
		" leaked, also AKIAABCDEFGHIJKLMNOP and Bearer abcdefghijklmnop. p99 over 4s at checkout."
	got, n := RedactText(text)
	if strings.Contains(got, CanaryPrefix) || strings.Contains(got, "AKIA") || strings.Contains(got, "oncall-lead") ||
		strings.Contains(got, "abcdefghijklmnop") {
		t.Fatalf("redacted = %q", got)
	}
	if n != 5 {
		t.Errorf("%d redactions in %q, want 5", n, got)
	}
	if !strings.Contains(got, "p99 over 4s at checkout.") {
		t.Errorf("the symptom went too: %q", got)
	}
}

func TestRedactTextKeepsOrdinaryText(t *testing.T) {
	for _, text := range []string{
		"pool size 20 -> 50 at 14:02 UTC",
		"user:id lookups were slow; see ops/runbooks/pool.md",
		"retries@3 per call", // no space before the @: not a handle, and no domain: not an address
	} {
		if got, n := RedactText(text); got != text || n != 0 {
			t.Errorf("RedactText(%q) = %q (%d)", text, got, n)
		}
	}
}

func TestOnlyRedactions(t *testing.T) {
	cases := map[string]bool{
		"[redacted]":                true,
		" [redacted], [redacted] .": true,
		"key: [redacted]":           false,
		"":                          true,
		"été":                       false,
	}
	for text, want := range cases {
		if got := OnlyRedactions(text); got != want {
			t.Errorf("OnlyRedactions(%q) = %v, want %v", text, got, want)
		}
	}
}
