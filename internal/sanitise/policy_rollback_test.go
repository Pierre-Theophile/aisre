// SPDX-License-Identifier: Apache-2.0

package sanitise_test

import (
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/sanitise"
)

// The rollback marker and the restored deployment (004 T140; FR-016, FR-134).
//
// These two fields had no disposition, and a field with no disposition reads back as `Unassigned` —
// which FAILS the commit gate rather than defaulting either way. So the first Vercel recording
// carrying a rollback would have been refused, and the refusal would have arrived at the worst
// moment: during a recording campaign, from a payload already fetched, with the question "what
// should this field be?" unanswerable without a human. The gate is right to be a refusal; leaving
// the row unassigned is what was wrong.

// The boolean is recorded verbatim, because there is no value a boolean could carry that names
// anybody — and because a corpus that pseudonymised it could not test FR-016's own clause.
func TestTheRollbackMarkerIsRecordedAsItIs(t *testing.T) {
	s := newSanitiser(t)
	for _, value := range []string{"true", "false"} {
		got, keep, err := s.Field("a vercel deployment", "change.rollback", value)
		if err != nil {
			t.Fatalf("change.rollback=%s: %v", value, err)
		}
		if !keep || got != value {
			t.Errorf("change.rollback=%s recorded as %q (keep=%t); a pseudonym where `%s` belongs "+
				"makes a corpus that cannot test the one clause FR-016 states — that a rollback is "+
				"flagged", value, got, keep, value)
		}
	}
}

// The restored deployment is pseudonymised: `dpl_7Qk…` is a real Vercel deployment and
// `checkout-00042-abc` a real Cloud Run revision, and both name an estate.
func TestTheRestoredDeploymentIsPseudonymised(t *testing.T) {
	s := newSanitiser(t)
	const stated = "dpl_7QkWmXbN3vRtZ2"
	got, keep, err := s.Field("a vercel deployment", "change.rolled_back_to", stated)
	if err != nil {
		t.Fatalf("change.rolled_back_to: %v", err)
	}
	if !keep {
		t.Fatal("the restored deployment was dropped; FR-016 makes it the field that says WHAT " +
			"production was restored to, and a rollback that cannot name its target is the case the " +
			"empty value already covers")
	}
	if got == stated {
		t.Errorf("the restored deployment was recorded verbatim as %q", got)
	}
	if !strings.HasPrefix(got, sanitise.PseudonymPrefix) {
		t.Errorf("the token %q does not carry the %q prefix, so a reader of a golden could paste it "+
			"into a vendor console", got, sanitise.PseudonymPrefix)
	}
}

// And it takes the SAME kind as every other deployed-version identifier, which is the decision T140
// asked for rather than a detail.
//
// §2.3 property 1 is what forces it: a rollback and the rollout it undoes are joined through this
// value — the restored deployment's identifier appears here on the rollback AND as the identifier of
// the change that first shipped it. Property 2 says two kinds of one name give two tokens. So a
// mismatch would hand one deployment two pseudonyms, and 004's acceptance scenario — the rollback is
// distinguishable from the rollout it undoes, and both appear with their own valid times — would fail
// in exactly the sanitised corpus that exists to check it.
//
// The assertion is made by TOKEN EQUALITY rather than by reading the rule's Kind, because the token
// is what a golden carries and what the join is actually made of. A test that compared the kinds
// would pass on a policy that had the right kinds and a broken pseudonymiser.
func TestTheRestoredDeploymentJoinsToTheRolloutItUndoes(t *testing.T) {
	s := newSanitiser(t)
	// One identifier, named by the rollback and by the change that first shipped it. In a recording
	// the second arrives under `revision` or `resource.labels.revision_name`, depending on which API
	// stated it — so all three paths must agree.
	const deployed = "checkout-00042-abc"
	tokens := map[string]string{}
	for _, path := range []string{"change.rolled_back_to", "change.rolled_back_from", "revision",
		"revision_name", "resource.labels.revision_name"} {
		got, keep, err := s.Field("a rollback and the rollout it undoes", path, deployed)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if !keep {
			t.Fatalf("%s was dropped, so the join cannot be asserted", path)
		}
		tokens[path] = got
	}
	want := tokens["revision"]
	for path, got := range tokens {
		if got != want {
			t.Errorf("%s pseudonymises %q to %s but `revision` gives %s. One deployment with two "+
				"tokens breaks the join between a rollback and the rollout it undoes, and it breaks "+
				"it only in the sanitised corpus — which is where nobody would look for it",
				path, deployed, got, want)
		}
	}
}

// An empty value is not an unassigned field, and the distinction is FR-016's.
//
// An empty `rolled_back_to` alongside `rollback: true` means "a rollback whose target we have not
// been told", never "not a rollback": a Vercel instant rollback reports that it happened, and the
// deployment it restored is a second read. So the empty case must pass the gate rather than refuse
// the recording — a refusal here would make an honest platform limitation look like a policy hole.
func TestARollbackWithNoStatedTargetIsStillRecordable(t *testing.T) {
	s := newSanitiser(t)
	if _, _, err := s.Field("a vercel instant rollback", "change.rolled_back_to", ""); err != nil {
		t.Errorf("an empty restored deployment was refused: %v. FR-016 makes an empty value with "+
			"`rollback` true a rollback whose target we have not been told, and refusing it would "+
			"turn a platform limitation into a blocked recording", err)
	}
	got, keep, err := s.Field("a vercel instant rollback", "change.rollback", "true")
	if err != nil || !keep || got != "true" {
		t.Errorf("the marker did not survive beside an empty target: %q keep=%t err=%v", got, keep, err)
	}
}

// Neither field is left unassigned, which is the gate T140 exists to clear. Asserted directly,
// because the three tests above would all still pass if `Field` were made to default.
func TestNeitherRollbackFieldIsUnassigned(t *testing.T) {
	policy := sanitise.ContractPolicy()
	for _, path := range []string{"change.rollback", "change.rolled_back_to", "change.rolled_back_from"} {
		rule, err := policy.Field(path, "a deploy recording")
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if rule.Disposition == sanitise.Unassigned {
			t.Errorf("%s has no disposition, so the first recording carrying a rollback fails the "+
				"commit gate (FR-134)", path)
		}
		if rule.Why == "" {
			t.Errorf("%s has a disposition and no reason; the reason is what a refusal quotes, and a "+
				"refusal that cannot say why is one a developer routes around", path)
		}
	}
}
