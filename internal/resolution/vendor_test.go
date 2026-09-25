// SPDX-License-Identifier: Apache-2.0

package resolution_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/resolution"
)

// The vendor-notice rules (T077, T078, FR-070, FR-119).
//
// C6 merges without asking a human, so the tests that matter most here are the ones that check what it
// refuses: an observed host that nobody mapped to a vendor, and two allowlists that disagree.

const (
	vendorHost = "api.acme-gpu.test"
	vendorSlug = "acme-gpu"
)

// allowlistedHostClaim is the claim the vendor-notice feeder emits for a host the allowlist maps.
func allowlistedHostClaim(entityID, host, slug string) resolution.Claim {
	return resolution.Claim{
		ClaimID: "claim-vendor-" + slug + "-" + host, EntityID: entityID,
		EntityType: graph.NodeTypeThirdParty, SourceID: "vendor-notice:nova",
		EventID: "vendor-notice:nova:claim:" + host, AppendedSeq: 10,
		Namespace: resolution.NamespaceServerAddress, Value: host,
		Attributes: map[string]string{resolution.AttrVendorAllowlistedHost: slug},
	}
}

// observedDependencyClaim is the claim the telemetry feeder emits for an outbound dependency: the same
// identifier, and no marker, because nothing configured it — it was seen.
func observedDependencyClaim(entityID, host string) resolution.Claim {
	return resolution.Claim{
		ClaimID: "claim-otel-dep-" + host, EntityID: entityID,
		EntityType: graph.NodeTypeThirdParty, SourceID: "otel:nova",
		EventID: "otel:nova:claim:" + host, AppendedSeq: 20,
		Namespace: resolution.NamespaceServerAddress, Value: host,
	}
}

// C6: a configured assertion, not a resemblance (FR-119).
func TestC6MergesAnAllowlistedVendorWithTheThirdPartyObservedFromTraffic(t *testing.T) {
	t.Parallel()

	vendor := allowlistedHostClaim("entity-vendor", vendorHost, vendorSlug)
	observed := observedDependencyClaim("entity-observed", vendorHost)
	store := fakeStore{claims: []resolution.Claim{vendor, observed}}

	// Both directions: either side may be the claim that just arrived, and the answer must not
	// depend on which feeder happened to run first.
	for _, trigger := range []resolution.Claim{vendor, observed} {
		matches, err := resolution.Evaluate(context.Background(), store, trigger)
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if !hasRule(matches, "C6") {
			t.Fatalf("triggered by %s, C6 did not fire; matches = %v", trigger.SourceID, ruleIDsOf(matches))
		}
		for _, match := range matches {
			if match.RuleID != "C6" {
				continue
			}
			if !match.Certain || match.Score != 1.0 {
				t.Errorf("C6 fired certain=%v score=%v, want certain with score 1.0", match.Certain, match.Score)
			}
			// The rationale is the whole reason this rule exists rather than letting C1 take the
			// pair: it has to name the allowlist and the vendor, or a reviewer cannot tell a
			// configured assertion from two sources having used the same string.
			for _, fragment := range []string{"allowlist", vendorSlug, vendorHost, "configured assertion"} {
				if !strings.Contains(match.Rationale, fragment) {
					t.Errorf("C6's rationale does not mention %q: %s", fragment, match.Rationale)
				}
			}
			if len(match.SupportingClaimIDs) != 2 {
				t.Errorf("C6 stands on %v, want both claims (FR-038)", match.SupportingClaimIDs)
			}
		}
	}

	// And it outranks C1, which fires on the same pair. The recorded reason must be the one a
	// reviewer can check.
	matches, err := resolution.Evaluate(context.Background(), store, observed)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 1 || matches[0].RuleID != "C6" {
		t.Errorf("the pair was recorded as %v, want exactly C6: a merge explained by \"two sources "+
			"used the same string\" tells a reviewer nothing about why the host belongs to the vendor",
			ruleIDsOf(matches))
	}
}

// Without the marker there is no configured assertion, so there is no C6. Two observations of one host
// are C1's business, and the distinction is the whole of FR-119.
func TestC6DoesNotFireOnTwoObservationsOfOneHost(t *testing.T) {
	t.Parallel()

	left := observedDependencyClaim("entity-a", vendorHost)
	right := observedDependencyClaim("entity-b", vendorHost)
	right.ClaimID, right.SourceID = "claim-k8s-ingress", "k8s:nova"
	store := fakeStore{claims: []resolution.Claim{left, right}}

	matches, err := resolution.Evaluate(context.Background(), store, left)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if hasRule(matches, "C6") {
		t.Error("C6 fired on a host nobody mapped to a vendor; its evidence is the allowlist entry, " +
			"and without one there is no evidence")
	}
	// The pair is still matched — by C1 — so this test is about *which* rule explains it, not about
	// the merge going missing.
	if !hasRule(matches, "C1") {
		t.Errorf("no rule matched two sources claiming one host: %v", ruleIDsOf(matches))
	}
}

// Two allowlists mapping one host to two vendors is a disagreement for a person to settle. Merging it
// automatically would take one of the two answers without recording that there was a question.
func TestC6RefusesTwoAllowlistsThatDisagree(t *testing.T) {
	t.Parallel()

	ours := allowlistedHostClaim("entity-acme", vendorHost, vendorSlug)
	theirs := allowlistedHostClaim("entity-other", vendorHost, "acme-gpu-reseller")
	theirs.SourceID = "vendor-notice:other-org"
	store := fakeStore{claims: []resolution.Claim{ours, theirs}}

	matches, err := resolution.Evaluate(context.Background(), store, ours)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if hasRule(matches, "C6") {
		t.Error("C6 merged two allowlist entries that map one host to different vendors; that is a " +
			"disagreement, and FR-122 says disagreement surfaces rather than resolving silently")
	}
}

// noticeClaim is the composite identifier the vendor-notice feeder emits: vendor, product and the
// announced window. It is what merges two sources carrying one announcement, and P6's input when the
// windows do not agree.
func noticeClaim(entityID, source, product string, start, end time.Time) resolution.Claim {
	window := start.UTC().Format(time.RFC3339Nano) + ".." + end.UTC().Format(time.RFC3339Nano)
	return noticeClaimWithWindow(entityID, source, product, window)
}

func noticeClaimWithWindow(entityID, source, product, window string) resolution.Claim {
	value := vendorSlug + "/" + product + "@" + window
	return resolution.Claim{
		ClaimID: "claim-" + source + "-" + value, EntityID: entityID,
		EntityType: graph.NodeTypeChange, SourceID: source,
		EventID: source + ":claim:" + value, AppendedSeq: 50,
		Namespace: resolution.NamespaceVendorNotice, Value: value,
	}
}

// P6: two readings of what may be one announcement, and nothing shared to merge on (FR-070).
func TestP6SuggestsTwoAnnouncementsWhoseWindowsDisagree(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 10, 2, 2, 0, 0, 0, time.UTC)
	mailbox := noticeClaim("entity-mailbox", "vendor-notice:nova", "inference-api", start, start.Add(2*time.Hour))
	// The status page says the same maintenance runs an hour longer. Same vendor, same product,
	// overlapping window, different string — so no certain rule can touch it.
	page := noticeClaim("entity-page", "statuspage:acme", "inference-api", start, start.Add(3*time.Hour))
	store := fakeStore{claims: []resolution.Claim{mailbox, page}}

	matches, err := resolution.Evaluate(context.Background(), store, page)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !hasRule(matches, "P6") {
		t.Fatalf("P6 did not fire on two overlapping windows for one product: %v", ruleIDsOf(matches))
	}
	for _, match := range matches {
		if match.RuleID != "P6" {
			continue
		}
		if match.Certain {
			t.Error("P6 declared itself certain; a vendor does announce two maintenances for one " +
				"product, so this can never merge on its own (ADR-0001 D6)")
		}
		if match.Score != resolution.ScoreP6 {
			t.Errorf("P6 score = %v, want the published ScoreP6 %v", match.Score, resolution.ScoreP6)
		}
		for _, fragment := range []string{vendorSlug, "inference-api", "overlap"} {
			if !strings.Contains(match.Rationale, fragment) {
				t.Errorf("P6's rationale does not mention %q: %s", fragment, match.Rationale)
			}
		}
	}
}

// One source dates the window and the other was vague. FR-069 keeps the vagueness as a marked-unknown
// start rather than a guess, which means the two identifiers cannot match — and a human is exactly who
// should decide whether they are one notice.
func TestP6SuggestsADatedWindowAndAVagueOne(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 10, 2, 2, 0, 0, 0, time.UTC)
	dated := noticeClaim("entity-dated", "vendor-notice:nova", "inference-api", start, start.Add(2*time.Hour))
	vague := noticeClaimWithWindow("entity-vague", "changelog:acme", "inference-api", "unknown..open")
	store := fakeStore{claims: []resolution.Claim{dated, vague}}

	matches, err := resolution.Evaluate(context.Background(), store, vague)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !hasRule(matches, "P6") {
		t.Errorf("P6 did not fire on a dated window and a vague one: %v", ruleIDsOf(matches))
	}
}

// Two windows that do not touch are two maintenances. Reporting them would train a reader to dismiss
// the rule, which costs more than the suggestion is worth.
func TestP6IsSilentOnWindowsThatDoNotTouch(t *testing.T) {
	t.Parallel()

	october := time.Date(2026, 10, 2, 2, 0, 0, 0, time.UTC)
	december := time.Date(2026, 12, 4, 2, 0, 0, 0, time.UTC)
	first := noticeClaim("entity-october", "vendor-notice:nova", "inference-api", october, october.Add(2*time.Hour))
	second := noticeClaim("entity-december", "statuspage:acme", "inference-api", december, december.Add(2*time.Hour))
	store := fakeStore{claims: []resolution.Claim{first, second}}

	matches, err := resolution.Evaluate(context.Background(), store, second)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if hasRule(matches, "P6") {
		t.Error("P6 suggested merging a maintenance in October with one in December")
	}
}

// Different products are different announcements however similar the window, and a different vendor is
// a different company. Neither is a near miss worth a suggestion.
func TestP6DoesNotCrossProductsOrVendors(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 10, 2, 2, 0, 0, 0, time.UTC)
	inference := noticeClaim("entity-inference", "vendor-notice:nova", "inference-api", start, start.Add(2*time.Hour))
	training := noticeClaim("entity-training", "statuspage:acme", "training", start, start.Add(3*time.Hour))
	otherVendor := noticeClaim("entity-other-vendor", "statuspage:other", "inference-api", start, start.Add(3*time.Hour))
	otherVendor.Value = "other-gpu/inference-api@" +
		start.Format(time.RFC3339Nano) + ".." + start.Add(3*time.Hour).Format(time.RFC3339Nano)

	for name, other := range map[string]resolution.Claim{"another product": training, "another vendor": otherVendor} {
		store := fakeStore{claims: []resolution.Claim{inference, other}}
		matches, err := resolution.Evaluate(context.Background(), store, other)
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if hasRule(matches, "P6") {
			t.Errorf("P6 suggested merging an announcement with one about %s", name)
		}
	}
}

// The same window from two sources is the same string, so C1 merges it and the pair is not reported a
// second time as a suggestion: a suggestion to merge what is already merged is noise, and two reasons
// recorded on one pair make a decision unreadable. The suppression is Evaluate's, not P6's.
func TestAPairACertainRuleTookIsNotAlsoSuggested(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 10, 2, 2, 0, 0, 0, time.UTC)
	mailbox := noticeClaim("entity-mailbox", "vendor-notice:nova", "inference-api", start, start.Add(2*time.Hour))
	page := noticeClaim("entity-page", "statuspage:acme", "inference-api", start, start.Add(2*time.Hour))
	store := fakeStore{claims: []resolution.Claim{mailbox, page}}

	matches, err := resolution.Evaluate(context.Background(), store, page)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !hasRule(matches, "C1") {
		t.Fatalf("two sources claiming one notice identifier did not merge: %v", ruleIDsOf(matches))
	}
	if hasRule(matches, "P6") {
		t.Error("P6 suggested a pair a certain rule had already matched")
	}
}

// One source reading the same window twice under two notice identifiers is the duplicate C1 cannot take
// — it requires two sources — and it is exactly the pair a person should see. The rule must not mistake
// "no certain rule applies" for "nothing to say".
func TestP6SurfacesOneSourceReadingTheSameWindowTwice(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 10, 2, 2, 0, 0, 0, time.UTC)
	first := noticeClaim("entity-notice-a", "vendor-notice:nova", "inference-api", start, start.Add(2*time.Hour))
	// Same source, same composite identifier, a different change entity: the vendor sent two notices
	// with two identifiers about one window, so the feeder addressed them as two changes.
	second := first
	second.ClaimID, second.EntityID = "claim-notice-b", "entity-notice-b"
	store := fakeStore{claims: []resolution.Claim{first, second}}

	matches, err := resolution.Evaluate(context.Background(), store, second)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if hasRule(matches, "C1") {
		t.Fatal("C1 fired on one source claiming an identifier twice; one source is one opinion, not " +
			"corroboration — if this changes, the assertion below is testing the wrong thing")
	}
	if !hasRule(matches, "P6") {
		t.Fatalf("P6 left two changes with the identical vendor, product and window unreported: %v",
			ruleIDsOf(matches))
	}
	// And it says so accurately. Falling through to the overlap clause would produce a suggestion
	// whose stated reason — "the windows overlap without being the same window" — is false of this
	// pair, and a rationale a reader can catch out is worse than no rationale.
	for _, match := range matches {
		if match.RuleID != "P6" {
			continue
		}
		if !strings.Contains(match.Rationale, "under different notice identifiers") {
			t.Errorf("P6 explained an identical window as something else: %s", match.Rationale)
		}
	}
}

// A notice identifier is not a window. The namespace holds both shapes, and a lenient parser would read
// `acme-gpu/INC-2026@10` as a window and compare instants it invented.
func TestP6DoesNotReadANoticeIdentifierAsAWindow(t *testing.T) {
	t.Parallel()

	// Each of these is paired against a real dated window for the same vendor and product, because
	// that is what makes the test bite: a parser that shrugged and used the zero instant would read
	// `@nonsense..open` as "begins at the dawn of time and never ends", which overlaps everything.
	start := time.Date(2026, 10, 2, 2, 0, 0, 0, time.UTC)
	dated := noticeClaim("entity-dated", "vendor-notice:nova", "inference-api", start, start.Add(2*time.Hour))

	for _, value := range []string{
		vendorSlug + "/ACME-2026-1002",
		vendorSlug + "/derived-0123456789abcdef",
		vendorSlug + "/INC-2026@10",
		vendorSlug + "/inference-api@nonsense..open",
		vendorSlug + "/inference-api@soon..later",
		vendorSlug + "/inference-api@2026-10-02T02:00:00Z",
		vendorSlug + "/inference-api@..2026-10-02T04:00:00Z",
	} {
		other := resolution.Claim{
			ClaimID: "claim-other-" + value, EntityID: "entity-other", EntityType: graph.NodeTypeChange,
			SourceID: "statuspage:acme", Namespace: resolution.NamespaceVendorNotice, Value: value,
		}
		store := fakeStore{claims: []resolution.Claim{dated, other}}

		matches, err := resolution.Evaluate(context.Background(), store, other)
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if hasRule(matches, "P6") {
			t.Errorf("P6 read %q as an announced window and compared it to a real one", value)
		}
	}
}

// Both rules are published with everything a recorded decision has to carry.
func TestTheVendorRulesArePublished(t *testing.T) {
	t.Parallel()

	seen := map[string]bool{}
	for _, rule := range resolution.VendorRules() {
		seen[rule.ID] = true
		if rule.Description == "" || len(rule.Namespaces) == 0 {
			t.Errorf("rule %s is missing a description or its namespaces", rule.ID)
		}
		if rule.Certain && rule.Score != 1.0 {
			t.Errorf("certain rule %s carries score %v, want 1.0", rule.ID, rule.Score)
		}
		if !rule.Certain && (rule.Score <= 0 || rule.Score >= 1) {
			t.Errorf("probable rule %s carries score %v, which is not a suggestion", rule.ID, rule.Score)
		}
	}
	for _, id := range []string{"C6", "P6"} {
		if !seen[id] {
			t.Errorf("rule %s is not published by VendorRules", id)
		}
	}
	registered := map[string]bool{}
	for _, rule := range resolution.Rules() {
		registered[rule.ID] = true
	}
	for id := range seen {
		if !registered[id] {
			t.Errorf("rule %s is not in the registry the CLI and the documentation print", id)
		}
	}
}

func ruleIDsOf(matches []resolution.Match) []string {
	out := make([]string, 0, len(matches))
	for _, match := range matches {
		out = append(out, match.RuleID)
	}
	return out
}
