// SPDX-License-Identifier: Apache-2.0

package datadog

import "testing"

// Only the published subset is executed (005 T036, FR-040a, FR-053).
func TestTheSelectorSubset(t *testing.T) {
	t.Parallel()
	s, err := parseSelector("env:production service:voice-agent index:main version:abc")
	if err != nil {
		t.Fatal(err)
	}
	if got := s.query("status:error"); got != "service:voice-agent env:production version:abc status:error" {
		t.Errorf("query %q", got)
	}
	if got := s.indexes([]string{"default"}); len(got) != 1 || got[0] != "main" {
		t.Errorf("indexes %v", got)
	}
	// A service alone is the feeder's pointer for logs that carry no environment (005): it is executed
	// as minted, with no env term.
	noEnv, err := parseSelector("service:voice-agent")
	if err != nil {
		t.Fatalf("an environment-less pointer was refused: %v", err)
	}
	if got := noEnv.query("status:error"); got != "service:voice-agent status:error" || noEnv.entity() != "voice-agent" {
		t.Errorf("query %q, entity %q", got, noEnv.entity())
	}
	for _, bad := range []string{
		"env:production",                       // no service
		"service:* env:production",             // wildcard
		"service:a env:b timeout",              // free text
		"service:a env:b OR:x",                 // operator
		"service:a env:b -service:c",           // negation
		`service:a env:b message:"drop table"`, // quoted phrase
	} {
		if _, err := parseSelector(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}
