// SPDX-License-Identifier: Apache-2.0

package sanitise_test

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"

	gcpfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/gcp"
	"github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/internal/sanitise"
)

// The sanitisation tests (T044).
//
// Each one is written against the property the contract claims rather than against the
// implementation: a test that asserts `Pseudonym` returns twelve characters tells you nothing about
// whether the corpus joins, and the corpus joining is the whole claim.

// testKey is a fixed key, so that a failure names the same token every run.
func testKey(t *testing.T) sanitise.Key {
	t.Helper()
	material := make([]byte, sanitise.KeyBytes)
	for i := range material {
		material[i] = byte(i * 7)
	}
	key, err := sanitise.NewKey(material)
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}
	return key
}

func newSanitiser(t *testing.T) *sanitise.Sanitiser {
	t.Helper()
	s, err := sanitise.New(sanitise.ContractPolicy(), testKey(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// A corpus-consistent pseudonym joins across every place the same service is named: the topology
// payload, the audit entry that changed it, the digest join key that measures it, and the golden
// that asserts the result. If any one of those four produced a different token, the recording would
// hold four unrelated entities where production has one, and `errors_by_version` could no longer
// say "the new revision is failing and the old one is not" — the sentence the corpus exists for.
func TestACorpusConsistentPseudonymJoinsAcrossThePayloadTheAuditEntryTheJoinKeyAndTheGolden(t *testing.T) {
	s := newSanitiser(t)
	const service = "storefront"

	// Four field paths that all name the same service, spelled as each source spells it.
	paths := map[string]string{
		"payload":     "service_name",
		"audit entry": "resource.labels.service_name",
		"join key":    "otel.service.name",
		"golden":      "service",
	}
	tokens := map[string]string{}
	for source, path := range paths {
		value, keep, err := s.Field(source, path, service)
		if err != nil {
			t.Fatalf("Field(%s): %v", path, err)
		}
		if !keep {
			t.Fatalf("%s at %s was dropped; an infrastructure identifier is pseudonymised, not dropped", source, path)
		}
		tokens[source] = value
	}
	first := tokens["payload"]
	for source, token := range tokens {
		if token != first {
			t.Fatalf("the same service pseudonymised to %q in the %s and %q in the payload; "+
				"a digest whose keys do not join is evidence about nothing (contract §2.3 property 1)",
				token, source, first)
		}
	}
	if !strings.HasPrefix(first, "px_svc_") {
		t.Fatalf("a service pseudonymised to %q, which does not carry the service kind tag", first)
	}
}

// Typed by kind: a service and an instance that happen to share a name must not collide into one
// token, because that silently merges two entities in the recorded graph.
func TestTwoKindsSharingANameDoNotCollideIntoOnePseudonym(t *testing.T) {
	key := testKey(t)
	const shared = "payments"
	asService, err := key.Pseudonym(sanitise.KindService, shared)
	if err != nil {
		t.Fatalf("Pseudonym(service): %v", err)
	}
	asInstance, err := key.Pseudonym(sanitise.KindInstance, shared)
	if err != nil {
		t.Fatalf("Pseudonym(instance): %v", err)
	}
	if asService == asInstance {
		t.Fatalf("a service and an instance both named %q pseudonymised to the same token %q; "+
			"the recorded graph would hold one entity where production has two (contract §2.3 property 2)",
			shared, asService)
	}

	// The kind must be in the MAC input, not only in the prefix: stripping the prefix must still
	// leave two different digests, or a reader who compares tokens without their tags merges them
	// back together.
	if strings.TrimPrefix(asService, sanitise.KindService.Prefix()) ==
		strings.TrimPrefix(asInstance, sanitise.KindInstance.Prefix()) {
		t.Fatal("the kind is only in the prefix, not in the MAC input: two kinds share a digest")
	}
}

// No published kind may be a prefix of another, and every pair of kinds must separate the same
// value into different tokens.
//
// The first half is what keeps the MAC input unambiguous as the table grows. `kind || 0x00 || value`
// is injective whatever the kinds are; `kind || value` is injective only while no kind is a prefix
// of another, and the day somebody adds `ip` beside an existing `ipaddress` the two would MAC the
// same bytes for the values `address:x` and `:x`. The separator is there so that cannot happen, and
// this assertion is there so that a kind added without it is caught at the table rather than in a
// corpus — because the collision it would cause is silent, and looks like two entities that merged.
func TestNoPublishedKindIsAPrefixOfAnotherAndEveryPairSeparatesTheSameValue(t *testing.T) {
	key := testKey(t)
	kinds := sanitise.Kinds()
	if len(kinds) < 2 {
		t.Fatal("fewer than two published kinds; the anti-collision claim is vacuous")
	}
	for _, a := range kinds {
		for _, b := range kinds {
			if a == b {
				continue
			}
			if strings.HasPrefix(string(b), string(a)) {
				t.Errorf("kind %q is a prefix of kind %q; the MAC input is only unambiguous because "+
					"of the 0x00 separator, and a reviewer should not have to know that", a, b)
			}
			if a.Prefix() == b.Prefix() {
				t.Errorf("kinds %q and %q share the token prefix %q", a, b, a.Prefix())
			}
		}
	}

	// Every pair of kinds separates one value. This is the claim §2.3 property 2 actually makes,
	// over the whole table rather than over the one pair a spot check would pick.
	const value = "payments"
	seen := map[string]sanitise.Kind{}
	for _, kind := range kinds {
		token, err := key.Pseudonym(kind, value)
		if err != nil {
			t.Fatalf("Pseudonym(%s): %v", kind, err)
		}
		digest := strings.TrimPrefix(token, kind.Prefix())
		if other, clash := seen[digest]; clash {
			t.Fatalf("%q as a %s and as a %s share the digest %q: two entities named the same thing "+
				"would merge in the recorded graph", value, kind, other, digest)
		}
		seen[digest] = kind
	}
}

// Keyed, and rotated between organisations: two corpora recorded under different keys must not
// share a token, or a reader could join one organisation's graph to another's.
func TestTwoCorpusKeysDoNotShareAPseudonym(t *testing.T) {
	one := testKey(t)
	material := make([]byte, sanitise.KeyBytes)
	for i := range material {
		material[i] = byte(i*7 + 1)
	}
	two, err := sanitise.NewKey(material)
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}
	a, err := one.Pseudonym(sanitise.KindService, "storefront")
	if err != nil {
		t.Fatalf("Pseudonym: %v", err)
	}
	b, err := two.Pseudonym(sanitise.KindService, "storefront")
	if err != nil {
		t.Fatalf("Pseudonym: %v", err)
	}
	if a == b {
		t.Fatalf("two corpus keys produced the same token %q; the key is not in the MAC", a)
	}
	if one.Fingerprint() == two.Fingerprint() {
		t.Fatal("two keys share a fingerprint, so a manifest cannot say which key a recording used")
	}
	if strings.Contains(one.Fingerprint(), one.Hex()[:8]) {
		t.Fatal("the fingerprint leaks the key material it is supposed to stand in for")
	}
}

// An unkeyed pseudonym looks identical to a keyed one in a golden, so a missing key is refused
// rather than defaulted.
func TestAMissingCorpusKeyIsRefusedRatherThanProducingAnUnkeyedHash(t *testing.T) {
	var zero sanitise.Key
	if _, err := zero.Pseudonym(sanitise.KindService, "storefront"); !errors.Is(err, sanitise.ErrNoKey) {
		t.Fatalf("a zero key produced %v, want ErrNoKey; an unkeyed hash is a lookup table", err)
	}
	if _, err := sanitise.New(sanitise.ContractPolicy(), zero); !errors.Is(err, sanitise.ErrNoKey) {
		t.Fatalf("New with a zero key returned %v, want ErrNoKey", err)
	}
	if _, err := sanitise.NewKey(make([]byte, 8)); err == nil {
		t.Fatal("an 8-byte corpus key was accepted; a short key is refused rather than stretched")
	}
}

// A people identifier never survives, in any form, hashed included (FR-135, SC-019). The three
// mechanisms are tested separately, because each catches a different way the rule could be lost.
func TestAPeopleIdentifierNeverSurvivesInAnyFormIncludingHashed(t *testing.T) {
	s := newSanitiser(t)

	// 1. Every people field, however deeply nested and however the vendor spells it, is dropped.
	for _, path := range []string{
		"protoPayload.authenticationInfo.principalEmail",
		"protoPayload.authenticationInfo.serviceAccountDelegationInfo[3].firstPartyPrincipal.principalEmail",
		"user.email",
		"userEmail",
		"USER_EMAIL",
		"message.sender",
		"labels.owner",
		"resource.labels.triggered_by",
	} {
		value, keep, err := s.Field("an audit entry", path, "jane.doe@acme-corp.io")
		if err != nil {
			t.Fatalf("Field(%s): %v", path, err)
		}
		if keep {
			t.Fatalf("%s was kept as %q; people identifiers are dropped, never hashed", path, value)
		}
		if value != "" {
			t.Fatalf("%s returned %q while reporting it was dropped", path, value)
		}
	}

	// 2. The table itself cannot say otherwise. This is the check that matters most, because a
	// table edit is the one place no downstream scan would look.
	if err := sanitise.ContractPolicy().Validate(); err != nil {
		t.Fatalf("the published policy does not validate: %v", err)
	}

	// 3. A person's address inside a field that the allowlist keeps verbatim is dropped anyway.
	// The `version` label row was accepted on exactly this, so it is asserted rather than assumed.
	_, _, keep, err := s.Label("a Cloud Run service", "version", "v2-jane.doe@acme-corp.io")
	if err != nil {
		t.Fatalf("Label(version): %v", err)
	}
	if keep {
		t.Fatal("a verbatim-allowlisted label kept a value carrying a person")
	}

	// 4. The same guard on the field path. No row of today's verbatim allowlist can plausibly carry
	// an address — it is closed vocabularies and numbers — so the guard is exercised through a
	// policy built for the purpose. Untested defence in depth is not defence in depth: it is a
	// comment, and the next verbatim row somebody adds is what it exists for.
	widened, err := sanitise.New(sanitise.NewPolicy(sanitise.PolicyVersion, map[string]sanitise.Rule{
		"deployment.note": {Disposition: sanitise.Verbatim, Why: "a row a future reviewer waved through"},
	}, nil), testKey(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	value, keep, err := widened.Field("a Cloud Run service", "deployment.note", "rolled back by jane.doe@acme-corp.io")
	if err != nil {
		t.Fatalf("Field: %v", err)
	}
	if keep {
		t.Fatalf("a verbatim field kept %q, which carries a person", value)
	}
	// And the same field without a person is kept, so the assertion above is about the value.
	if _, keep, err := widened.Field("a Cloud Run service", "deployment.note", "rolled back"); err != nil || !keep {
		t.Fatalf("the same verbatim field without a person was dropped (keep=%v, err=%v)", keep, err)
	}
}

// The people vocabulary here must cover feature 002's, or a key added there goes missing in the
// corpus this contract governs.
func TestThePeopleVocabularyCoversFeature002s(t *testing.T) {
	for key := range backend.PeopleAttributeKeys {
		if !sanitise.IsPeopleField(key) {
			t.Errorf("feature 002 treats %q as a person and this package does not; "+
				"PeopleFieldKeys is meant to be a superset", key)
		}
	}
}

// A field with no disposition fails, and it fails as a distinct error so that the recording path
// can tell a question for a human from a transient failure to retry.
func TestAFieldWithNoDispositionFailsRatherThanDefaulting(t *testing.T) {
	s := newSanitiser(t)
	const path = "protoPayload.somethingNobodyThoughtAbout"

	_, _, err := s.Field("an audit entry", path, "whatever")
	if err == nil {
		t.Fatal("an unassigned field was accepted; a default is how the next field nobody thought " +
			"about gets recorded (FR-134)")
	}
	var unassigned *sanitise.UnassignedError
	if !errors.As(err, &unassigned) {
		t.Fatalf("an unassigned field produced %T (%v), want *UnassignedError", err, err)
	}
	if unassigned.Field != path {
		t.Fatalf("the refusal names %q, not the field that caused it (%q)", unassigned.Field, path)
	}
	if !strings.Contains(err.Error(), sanitise.PolicyVersion) {
		t.Fatalf("the refusal does not name the policy version: %q", err.Error())
	}

	// One unassigned field fails the whole payload: FR-140's "drop rather than promise" means a
	// payload that cannot be made safe is dropped, not partially recorded.
	out, err := s.Fields("an audit entry", map[string]string{
		"service_name": "storefront",
		path:           "whatever",
	})
	if err == nil {
		t.Fatal("a payload with one unassigned field was partially recorded")
	}
	if out != nil {
		t.Fatalf("a refused payload still returned %d fields", len(out))
	}
}

// A label key that is not on the allowlist becomes nothing — not a property, not a node, not a
// claim (FR-124) — and that is the published open-namespace rule rather than a default.
func TestAnUnlistedLabelKeyBecomesNothing(t *testing.T) {
	s := newSanitiser(t)
	key, value, keep, err := s.Label("a Cloud Run service", "jira-ticket", "PLAT-4412")
	if err != nil {
		t.Fatalf("Label: %v", err)
	}
	if keep {
		t.Fatalf("an unlisted label survived as %s=%s; config/gcp.yaml publishes on_unlisted: drop", key, value)
	}
	// An allowlisted key is kept, or the rule above would be vacuous.
	_, token, keep, err := s.Label("a Cloud Run service", "team", "payments")
	if err != nil {
		t.Fatalf("Label(team): %v", err)
	}
	if !keep {
		t.Fatal("an allowlisted label was dropped, so the unlisted case proves nothing")
	}
	if !strings.HasPrefix(token, "px_tm_") {
		t.Fatalf("the team label became %q, which is not a team pseudonym", token)
	}
}

// Log content reduces to masked templates before it leaves the process, and the masking is feature
// 002's so that a recorded template and a live one are the same string.
func TestLogContentReducesToMaskedTemplatesBeforeLeavingTheProcess(t *testing.T) {
	s := newSanitiser(t)
	const raw = "2026-09-01T14:32:00Z ERROR payment failed for 10.4.2.19 user " +
		"3f1a9c2e-4b5d-4f6a-9e8c-1d2b3a4c5d6e contact jane.doe@acme-corp.io after 1200ms"

	template, keep, err := s.Template("a log entry", raw)
	if err != nil {
		t.Fatalf("Template: %v", err)
	}
	if !keep {
		t.Fatalf("a maskable log line was dropped")
	}
	for _, leaked := range []string{"10.4.2.19", "3f1a9c2e", "jane.doe@acme-corp.io", "acme-corp"} {
		if strings.Contains(template, leaked) {
			t.Fatalf("the template %q still carries %q", template, leaked)
		}
	}
	if template != backend.MaskLine(raw) {
		t.Fatalf("the template is not feature 002's masking of the line: %q vs %q",
			template, backend.MaskLine(raw))
	}
	if unmasked := backend.UnmaskedLiterals(template); unmasked != "" {
		t.Fatalf("the template %q still carries an unmasked %s", template, unmasked)
	}

	// A handle survives masking, so the line is dropped rather than recorded, and the drop is on
	// the manifest.
	_, keep, err = s.Template("a log entry", "restarted by @jane-doe at request of ops")
	if err != nil {
		t.Fatalf("Template(handle): %v", err)
	}
	if keep {
		t.Fatal("a log line naming a person by handle was kept")
	}
	if !droppedField(s, "logentry.template") {
		t.Fatal("the dropped log line is not on the manifest; FR-140 requires what was dropped be stated")
	}
}

// A surviving canary fails rather than warns, and the raw-token check alone is not enough: a people
// canary that was pseudonymised rather than dropped is invisible to it, because the raw token
// really is gone. That is the case SC-019's "hashed forms included" is about.
func TestASurvivingCanaryFailsTheCommitIncludingInHashedForm(t *testing.T) {
	key := testKey(t)

	set := sanitise.NewCanarySet(key)
	seeded, err := set.Seed("the nova-production audit log", sanitise.CanaryPerson, sanitise.CanaryInfrastructure)
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}
	if set.Len() != 2 {
		t.Fatalf("seeded %d canaries, want 2", set.Len())
	}
	person, infra := seeded[0], seeded[1]

	// A raw survivor fails.
	if err := set.Assert("a world file", []byte(`{"principal": "`+person.Carrier+`"}`)); err == nil {
		t.Fatal("a raw canary survived into an artifact and the gate passed")
	} else {
		var survived *sanitise.CanarySurvivedError
		if !errors.As(err, &survived) {
			t.Fatalf("a surviving canary produced %T, want *CanarySurvivedError", err)
		}
		if strings.Contains(err.Error(), person.Token) {
			t.Fatalf("the refusal quoted the whole canary token, which is the value a report must not carry: %q", err.Error())
		}
	}

	// A *hashed* people canary fails too. This is the assertion that makes "never hashed" testable.
	hashed, err := key.Pseudonym(sanitise.KindResource, person.Token)
	if err != nil {
		t.Fatalf("Pseudonym: %v", err)
	}
	survivors, err := set.Survivors([]byte(`{"principal": "` + hashed + `"}`))
	if err != nil {
		t.Fatalf("Survivors: %v", err)
	}
	if len(survivors) != 1 {
		t.Fatalf("a hashed people canary produced %d survivors, want 1; a pseudonymised person is "+
			"still personal data (SC-019)", len(survivors))
	}
	if survivors[0].Form != string(sanitise.KindResource) {
		t.Fatalf("the survival reports form %q, not the kind it was hashed under; "+
			"\"not seen\" and \"hashed a person\" are different bugs", survivors[0].Form)
	}

	// An *infrastructure* canary in pseudonymised form is the correct outcome, not a survival — or
	// the check above would fire on every correctly sanitised identifier in the corpus.
	infraHashed, err := key.Pseudonym(sanitise.KindService, infra.Token)
	if err != nil {
		t.Fatalf("Pseudonym: %v", err)
	}
	survivors, err = set.Survivors([]byte(`{"service": "` + infraHashed + `"}`))
	if err != nil {
		t.Fatalf("Survivors: %v", err)
	}
	if len(survivors) != 0 {
		t.Fatalf("a correctly pseudonymised infrastructure canary was reported as %d survivors", len(survivors))
	}

	// A clean artifact passes, so the gate is not simply always red.
	if err := set.Assert("a world file", []byte(`{"service": "`+infraHashed+`"}`)); err != nil {
		t.Fatalf("a clean artifact was refused: %v", err)
	}
}

// A canary carrier must not be allow-listed by the personal-data scan as a placeholder, or the
// canary tests the allow-list rather than the sanitiser.
func TestAPersonCanaryIsNotWrittenAtAReservedDocumentationDomain(t *testing.T) {
	set := sanitise.NewCanarySet(testKey(t))
	seeded, err := set.Seed("a source", sanitise.CanaryPerson)
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}
	for _, reserved := range []string{".example", ".invalid", ".test", ".localhost", "example.com", "example.org", "example.net"} {
		if strings.Contains(seeded[0].Carrier, reserved) {
			t.Fatalf("the person canary is written at %q, which the personal-data scan allow-lists "+
				"as a fixture placeholder: the canary would test the allow-list, not the sanitiser", reserved)
		}
	}
	if !sanitise.LooksLikePerson(seeded[0].Carrier) {
		t.Fatalf("the person canary %q does not look like a person, so no people rule would fire on it", seeded[0].Carrier)
	}
}

// SC-018 is a claim about tokens that were seeded. A set of none is vacuously satisfied by a
// sanitiser that does nothing, so it is refused rather than passed.
func TestACampaignThatSeededNoCanariesHasNotSatisfiedTheGate(t *testing.T) {
	set := sanitise.NewCanarySet(testKey(t))
	if err := set.Assert("a world file", []byte(`{"service": "px_svc_abcdefghijkl"}`)); err == nil {
		t.Fatal("a canary set with no canaries passed its own gate")
	}
}

// The sanitiser refuses a canary at the boundary as well, before the bytes exist — the commit gate
// is the independent second opinion (FR-138), not the only one.
func TestTheSanitiserRefusesACanaryAtTheBoundaryNotOnlyAtCommit(t *testing.T) {
	key := testKey(t)
	set := sanitise.NewCanarySet(key)
	seeded, err := set.Seed("a source", sanitise.CanaryInfrastructure)
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}
	s, err := sanitise.New(sanitise.ContractPolicy(), key)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s = s.WithCanaries(set)

	if _, _, err := s.Field("a payload", "service_name", seeded[0].Carrier); err == nil {
		t.Fatal("the sanitiser pseudonymised a canary and reported success; the boundary check did not fire")
	}
	// Without the set, the same call succeeds — so the failure above is the canary and not the field.
	clean, err := sanitise.New(sanitise.ContractPolicy(), key)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, _, err := clean.Field("a payload", "service_name", seeded[0].Carrier); err != nil {
		t.Fatalf("the same field without a canary set failed: %v", err)
	}
}

// The handoff to feature 002. A value this package pseudonymised must pass through feature 002's
// redactor unchanged, or the graph and the digests hold two different tokens for one service and
// §2.3 property 1 is false across the corpus. It works in one direction only, which is why FR-137's
// ordering — sanitise in the connector, before anything else — is load-bearing twice over.
func TestAPseudonymSurvivesFeature002sRedactorUnchanged(t *testing.T) {
	key := testKey(t)
	token, err := key.Pseudonym(sanitise.KindService, "storefront")
	if err != nil {
		t.Fatalf("Pseudonym: %v", err)
	}
	redactor, err := backend.NewRedactor(backend.DefaultRedactionPolicy(), []byte("a different key entirely"))
	if err != nil {
		t.Fatalf("NewRedactor: %v", err)
	}
	if again := redactor.Pseudonymise(token); again != token {
		t.Fatalf("feature 002's redactor rewrote %q to %q; the graph and the digests would hold two "+
			"tokens for one service (contract §2.3 property 1)", token, again)
	}
	if !strings.HasPrefix(token, backend.PseudonymPrefix) {
		t.Fatalf("the token %q does not carry feature 002's prefix %q, which is what makes the "+
			"handoff idempotent", token, backend.PseudonymPrefix)
	}

	// The reverse order does not work, and saying so here is the point: applying this package to a
	// value feature 002 already pseudonymised leaves 002's token, untyped.
	theirs := redactor.Pseudonymise("storefront")
	ours, err := key.Pseudonym(sanitise.KindService, theirs)
	if err != nil {
		t.Fatalf("Pseudonym: %v", err)
	}
	if ours != theirs {
		t.Fatalf("sanitising a value 002 had already pseudonymised produced a third token %q", ours)
	}
}

// Applying the policy twice changes nothing, so a payload crossing two sanitisation boundaries is
// not double-hashed into a token that joins to nothing.
func TestApplyingThePolicyTwiceIsANoOp(t *testing.T) {
	s := newSanitiser(t)
	once, _, err := s.Field("a payload", "service_name", "storefront")
	if err != nil {
		t.Fatalf("Field: %v", err)
	}
	twice, _, err := s.Field("a payload", "service_name", once)
	if err != nil {
		t.Fatalf("Field: %v", err)
	}
	if once != twice {
		t.Fatalf("sanitising twice produced %q then %q", once, twice)
	}
}

// A policy that says a people field is anything but Dropped does not validate. This is FR-135 made
// structural: it cannot be walked back by a table edit.
func TestAPolicyThatPseudonymisesAPersonDoesNotValidate(t *testing.T) {
	// The published table validates.
	if err := sanitise.ContractPolicy().Validate(); err != nil {
		t.Fatalf("the published policy does not validate: %v", err)
	}
	// A pseudonym with no kind does not: an untyped pseudonym merges two entities.
	if err := sanitise.NewPolicy(sanitise.PolicyVersion, map[string]sanitise.Rule{
		"service_name": {Disposition: sanitise.Pseudonym},
	}, nil).Validate(); err == nil {
		t.Fatal("a pseudonym rule with no kind validated")
	}
	// A people field pseudonymised does not.
	err := sanitise.NewPolicy(sanitise.PolicyVersion, map[string]sanitise.Rule{
		"protoPayload.authenticationInfo.principalEmail": {Disposition: sanitise.Pseudonym, Kind: sanitise.KindResource},
	}, nil).Validate()
	if err == nil {
		t.Fatal("a policy pseudonymising a principal email address validated; FR-135 is not structural")
	}
	if !strings.Contains(err.Error(), "FR-135") {
		t.Fatalf("the refusal does not name the rule it enforces: %q", err.Error())
	}
	// And a people field kept verbatim does not either.
	if err := sanitise.NewPolicy(sanitise.PolicyVersion, map[string]sanitise.Rule{
		"user.email": {Disposition: sanitise.Verbatim},
	}, nil).Validate(); err == nil {
		t.Fatal("a policy recording an email address verbatim validated")
	}
}

// What was dropped reaches the manifest (FR-140): a reader learns that a principal was present and
// removed, which is a materially different statement from the field never existing.
func TestWhatWasDroppedIsRecordedForTheManifest(t *testing.T) {
	s := newSanitiser(t)
	for i := 0; i < 3; i++ {
		if _, _, err := s.Field("an audit entry", "protoPayload.authenticationInfo.principalEmail",
			"jane.doe@acme-corp.io"); err != nil {
			t.Fatalf("Field: %v", err)
		}
	}
	if _, _, err := s.Field("an audit entry", "message.body", "anything at all"); err != nil {
		t.Fatalf("Field: %v", err)
	}
	drops := s.Drops()
	if len(drops) != 2 {
		t.Fatalf("recorded %d drops, want 2 (one per field, however many payloads): %v", len(drops), drops)
	}
	for _, drop := range drops {
		if drop.Why == "" {
			t.Fatalf("the drop of %s carries no reason; a manifest line nobody can read is not a disclosure", drop.Field)
		}
		if strings.Contains(drop.Why, "jane.doe") || strings.Contains(drop.Field, "jane.doe") {
			t.Fatalf("the manifest carries the value it dropped: %+v", drop)
		}
	}
}

func droppedField(s *sanitise.Sanitiser, field string) bool {
	for _, drop := range s.Drops() {
		if drop.Field == field {
			return true
		}
	}
	return false
}

// FR-137's aborted run, made executable (T160).
//
// The clause is *"No unsanitised GCP payload ... may be written to disk, to a log or to any artifact
// at any point, **including during a failed or aborted run**"*, and that last phrase is the whole
// difficulty. A commit hook cannot help: by the time it runs, the bytes have been on a laptop's disk,
// in a temporary directory and possibly in a crash dump. So the guarantee has to be a property of the
// code path.
//
// Two triggers, because the path has two shapes and they are easy to conflate:
//
//   - a **people identifier** is dropped from a payload that is still written. The recording keeps
//     the payload and loses the person, in any form (FR-135, SC-019).
//   - an **unassigned field** aborts the recording of that payload entirely. Nothing of it reaches
//     the sink — not a redacted version, not a truncated prefix, nothing — and the run stops rather
//     than skipping ahead, because a recording with a hole in it is one whose gaps nobody can tell
//     from absences (FR-134, FR-140).
func TestAnAbortedRecordingLeavesNothingUnsanitisedBehind(t *testing.T) {
	t.Parallel()

	sink := &countingSink{}
	recorder, err := gcpfeeder.NewSanitisedRecorder(newSanitiser(t), sink)
	if err != nil {
		t.Fatalf("NewSanitisedRecorder: %v", err)
	}
	ctx := context.Background()

	const principal = "dana.okonkwo@example.com"

	// One safe payload, then one carrying a principal — which is written with the person removed.
	if err := recorder.Record(ctx, "services/000001", map[string]string{
		"name": "projects/twin-production/locations/europe-west1/services/storefront",
	}, encodeSorted); err != nil {
		t.Fatalf("first payload: %v", err)
	}
	if err := recorder.Record(ctx, "audit/000001", map[string]string{
		"protoPayload.authenticationInfo.principalEmail": principal,
	}, encodeSorted); err != nil {
		t.Fatalf("a payload carrying a principal email was refused outright: %v. FR-135 drops the "+
			"person; it does not throw away the change the person made", err)
	}

	// The abort: a field the policy has no disposition for. Fail-closed is the rule — a field nobody
	// classified is not quietly copied and not quietly dropped, it stops the recording (FR-134).
	abortErr := recorder.Record(ctx, "audit/000002", map[string]string{
		"protoPayload.authenticationInfo.principalEmail": principal,
		"protoPayload.someFieldNobodyClassified":         "surprise " + principal,
	}, encodeSorted)
	if abortErr == nil {
		t.Fatal("a payload carrying an unclassified field was recorded; a field with no disposition " +
			"must fail rather than default either way (FR-134)")
	}
	if !strings.Contains(abortErr.Error(), "dropped from the recording rather than written") {
		t.Errorf("the refusal reads %q; it must say the payload was dropped rather than written, "+
			"because a reader has to be able to tell a drop from a write that was cleaned later",
			abortErr.Error())
	}

	// The third trigger, and the one a check on the map alone would miss: an **encoder** that puts
	// back something the field walk never saw. `RecordBytes` asserts the encoded bytes rather than
	// the map for exactly this, and without a case for it that backstop is untested — deleting the
	// assertion from `RecordBytes` left this test green until this block existed.
	//
	// The value here is an **@handle** rather than an address, and not for variety. `principal` above
	// is `@example.com`, an RFC 2606 documentation name, which `PeopleInArtifact` exempts on purpose:
	// a fixture author's placeholder is not a person, and an assertion that fired on every
	// placeholder would be switched off within a day. So reintroducing `principal` through the
	// encoder would be caught by nothing and would prove nothing. A handle is person-shaped with no
	// exemption — and it keeps this repository free of a plausible real address, which is the same
	// rule the corpus follows.
	const handle = " @casey"
	leakyEncode := func(fields map[string]string) ([]byte, error) {
		clean, err := encodeSorted(fields)
		if err != nil {
			return nil, err
		}
		return append(clean, ("actor=" + handle + "\n")...), nil
	}
	leakErr := recorder.Record(ctx, "audit/000003", map[string]string{
		"name": "projects/twin-production/locations/europe-west1/services/orders",
	}, leakyEncode)
	if leakErr == nil {
		t.Fatal("an encoder that reintroduced a people identifier produced a recorded payload; the " +
			"assertion has to be on the BYTES the sink receives, because the map is not what is " +
			"written, and an encoder that helpfully adds a field the walk never saw is the case it " +
			"exists for (FR-137)")
	}

	// Nothing after the abort runs. That is what "aborted run" means here, and it is the state the
	// assertions below are made in.
	if recorder.Written() != 2 {
		t.Errorf("the recorder wrote %d payloads, want 2: the third aborted", recorder.Written())
	}
	if sink.writes != 2 {
		t.Errorf("the sink received %d writes, want 2: the aborted payload must not reach it at all",
			sink.writes)
	}

	// The assertion FR-137 is actually about: the offending value is nowhere in what reached the
	// sink, in any form. Checked over the whole concatenation rather than per write, because a value
	// split across two writes would pass a per-write check.
	all := sink.all()
	if strings.Contains(all, principal) {
		t.Errorf("the principal email reached the sink verbatim: %q", all)
	}
	// Not as a local part or a domain either, which is how a half-applied redaction leaks. And not
	// the "surprise" marker from the aborted payload, which would mean a partial write.
	for _, fragment := range []string{"dana.okonkwo", "dana", "okonkwo", "example.com", "surprise", "casey"} {
		if strings.Contains(all, fragment) {
			t.Errorf("%q reached the sink; a people identifier must not survive in any form and an "+
				"aborted payload must not survive at all (FR-135, FR-137, SC-019). Sink holds: %q",
				fragment, all)
		}
	}
	// A hashed form is a people identifier too (SC-019, "hashed forms included"), so no pseudonym of
	// it may be there either — under ANY declared kind, because the question is whether the value is
	// recoverable, not which label it was filed under. There is deliberately no person kind: a field
	// whose disposition is DROP is dropped, never pseudonymised, and the absence of that kind is how
	// the package says so.
	for _, kind := range sanitise.Kinds() {
		pseudonym, err := testKey(t).Pseudonym(kind, principal)
		if err != nil {
			continue
		}
		if strings.Contains(all, pseudonym) {
			t.Errorf("a %s-kind pseudonym of the principal email reached the sink (%q); a people "+
				"identifier must not survive in any form, hashed forms included (SC-019)",
				kind, pseudonym)
		}
	}

	// And the safe payload really was written, so the two assertions above are not passing because
	// the recorder wrote nothing at all. A sanitiser that refused everything and a source that
	// returned nothing look identical from the outside.
	if !strings.Contains(all, "name=") {
		t.Errorf("the safe payload did not reach the sink; the sink holds %q, so the absence "+
			"assertions above held over nothing", all)
	}

	// The drop is DOCUMENTED, which is the other half of FR-140: a drop nobody recorded is
	// indistinguishable from a payload the campaign never saw.
	drops := recorder.Dropped()
	if len(drops) == 0 {
		t.Fatal("the recorder documented no drop; FR-140 requires the drop be documented, because " +
			"a drop nobody recorded cannot be told from a payload that never arrived")
	}
	var named bool
	for _, drop := range drops {
		// Case-insensitively: the policy lowercases a field path when it records the drop, so a
		// reader matching the camelCase spelling GCP uses would find nothing.
		if strings.Contains(strings.ToLower(drop.Field), "principalemail") {
			named = true
		}
	}
	if !named {
		t.Errorf("no recorded drop names the principal field; the drops are %+v", drops)
	}
}

// countingSink records what it was handed, and how often.
type countingSink struct {
	payloads []string
	writes   int
}

func (s *countingSink) Write(_ context.Context, _ string, payload []byte) error {
	s.writes++
	s.payloads = append(s.payloads, string(payload))
	return nil
}

func (s *countingSink) all() string { return strings.Join(s.payloads, "\x00") }

// encodeSorted renders a flattened payload deterministically, which is what a recorder needs: the
// same observation must produce the same bytes twice.
func encodeSorted(fields map[string]string) ([]byte, error) {
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, key := range keys {
		b.WriteString(key)
		b.WriteString("=")
		b.WriteString(fields[key])
		b.WriteString("\n")
	}
	return []byte(b.String()), nil
}
