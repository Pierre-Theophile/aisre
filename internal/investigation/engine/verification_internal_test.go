// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/investigation/verify"
)

// TestTheVerifierCannotKeepWhatTheCheckerDemoted: a figure no cited digest holds stays unmatched
// whatever the model says about the sentence around it, while a removal by the verifier stands.
func TestTheVerifierCannotKeepWhatTheCheckerDemoted(t *testing.T) {
	t.Parallel()

	checker := []verify.Finding{
		{ClaimRef: "hypothesis.h-4", Verdict: verify.VerdictNumberMismatch, ActionTaken: verify.ActionDemoted, OffendingValue: "0.99"},
		{ClaimRef: "hypothesis.h-3", Verdict: verify.VerdictSupported, ActionTaken: verify.ActionKept},
	}
	verifier := []verify.Finding{
		{ClaimRef: "hypothesis.h-4", Verdict: verify.VerdictSupported, ActionTaken: verify.ActionKept},
		{ClaimRef: "hypothesis.h-3", Verdict: verify.VerdictUnsupported, ActionTaken: verify.ActionRemoved},
		{ClaimRef: "hypothesis.h-9", Verdict: verify.VerdictUnsupported, ActionTaken: verify.ActionRemoved},
	}
	got := merge(checker, verifier)
	if len(got) != 2 {
		t.Fatalf("merged = %+v, want one finding per claim sent", got)
	}
	if got[0].ClaimRef != "hypothesis.h-3" || got[0].ActionTaken != verify.ActionRemoved {
		t.Errorf("the verifier's removal did not stand: %+v", got[0])
	}
	if got[1].ClaimRef != "hypothesis.h-4" || got[1].ActionTaken != verify.ActionDemoted {
		t.Errorf("the verifier overrode the checker's demotion: %+v", got[1])
	}
	if s := summarise(got, "test"); s != "2 claim(s): 0 kept, 1 demoted, 1 removed; test" {
		t.Errorf("summary = %q", s)
	}
}
