// SPDX-License-Identifier: Apache-2.0

package resolution

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// The vendor-notice rules (T077, T078, FR-070, FR-119).
//
//	C6  certain   the vendor allowlist maps a vendor to a host name, and an observed outbound
//	              dependency's server address is that host
//	P6  probable  two announcements about the same vendor and product whose windows are not the
//	              same and whose notice identifiers give nothing to merge on
//
// # Why C6 is certain and P6 can only ever suggest
//
// C6 rests on **a configured assertion**. Somebody maintaining the allowlist wrote `api.acme-gpu.test`
// next to `acme-gpu` on purpose, and the graph separately observed traffic to `api.acme-gpu.test`. The
// evidence is the allowlist entry, not a resemblance between two strings — which is the whole of FR-119
// and the reason `stripe` resembling `stripe-api` gets nowhere near a certain rule.
//
// This is deliberately more specific than C1, which would also fire: two sources do claim the same
// `server.address` identifier. C1's rationale would read "the same identifier in the same namespace
// denotes one entity", which is true and tells a reader nothing about *why* anyone believed the host
// belongs to that vendor. C6 is recorded instead because it names the allowlist, and a merge a reader
// cannot argue with is a merge nobody reviews (constitution VI). It is the same reason C2 outranks C1
// on a declared service name.
//
// P6 is the other half of FR-070. The feeder emits a composite identifier — vendor, product and
// announced window — precisely so that two sources carrying one announcement merge on a published rule
// rather than by the feeder silently suppressing one of them. When the windows agree, that identifier
// is the same string and C1 merges them. When they do **not** agree, nothing shared is left, and the
// pair has to surface as a suggestion: a vendor genuinely does announce two separate maintenances for
// one product, so this can never be automatic.

// The vendor namespaces and claim attributes. Re-declared here rather than imported from
// `internal/feeders/vendornotice` for the reason gcp.go gives: a rule must be testable without a
// connector present.
const (
	// NamespaceVendorNotice is `vendor.notice`, valued either `<vendor>/<notice-id>` or the
	// composite `<vendor>/<product>@<start>..<end>` that P6 reads.
	NamespaceVendorNotice = "vendor.notice"
	// NamespaceVendor is `vendor`, valued by the allowlist slug.
	NamespaceVendor = "vendor"
	// NamespaceServerAddress is `server.address`, the namespace an observed outbound dependency's
	// host is named in — and where the allowlist's host names are claimed, so that C6 has one
	// identifier with two reporters to stand on.
	NamespaceServerAddress = "server.address"
)

const (
	// AttrVendorAllowlistedHost marks a `server.address` claim as coming from **the allowlist**,
	// valued by the vendor slug it is mapped to. C6 requires it on exactly one side.
	//
	// Without the marker the rule would have to guess which side is the configured assertion, and
	// the only available guess — the source id — would make the rule stop firing the day somebody
	// renames a source. It is the same role AttrGCPDeclaredServiceName plays for C5.
	AttrVendorAllowlistedHost = "sre.vendor.allowlisted_host"
)

// ScoreP6 is two announcements about one vendor and product whose windows differ.
//
// Higher than a name resemblance, because the vendor and the product are both allowlisted values
// rather than strings that happen to look alike: the pair is already known to be about one product of
// one provider. Well short of certain, because the case it cannot tell apart from a duplicate is a real
// one — a vendor announcing two separate maintenance windows on the same product in the same month.
const ScoreP6 = 0.5

var certainRuleC6 = Rule{
	ID:      "C6",
	Certain: true,
	Score:   certainScore,
	// Above C1, which fires on the very same pair and is the only rule it collides with: a merge
	// explained by the allowlist entry is one a reader can check, and "two sources used the same
	// string" is not. Below C2 and C5, which each stand on three agreeing facts — a declaration, an
	// agreed environment or namespace, and an observation — where C6 stands on two.
	Specificity: 25,
	Namespaces:  []string{NamespaceServerAddress},
	Description: "The vendor allowlist maps a vendor to one or more host names, and an observed " +
		"outbound dependency's server address is one of them. The vendor the notices are about and " +
		"the third party the graph observed from traffic are one entity. Certain because it rests on " +
		"a configured assertion — somebody wrote that host name next to that vendor — and not on a " +
		"resemblance between two strings.",
	Eval: evalC6,
}

var probableRuleP6 = Rule{
	ID:          "P6",
	Certain:     false,
	Score:       ScoreP6,
	Specificity: 25,
	Namespaces:  []string{NamespaceVendorNotice},
	Description: "Two announcements name the same allowlisted vendor and product, state windows that " +
		"are not identical but are not disjoint either, and share no notice identifier — so no " +
		"certain rule can merge them. Suggestion only: a vendor does announce two separate " +
		"maintenance windows for one product, and a merge would hide one of them.",
	Eval: evalP6,
}

func init() { Register(certainRuleC6, probableRuleP6) }

// VendorRules returns the two rules the vendor-notice feeder relies on, in id order. It is what the
// contract documentation and `aisre rules` print for the announced-fact half of feature 003.
func VendorRules() []Rule { return []Rule{certainRuleC6, probableRuleP6} }

// evalC6: an allowlisted host name and an observed dependency on it (FR-119).
//
// Both sides claim `server.address=<host>`, so the candidates come from one lookup. What makes it
// certain is that one of them carries AttrVendorAllowlistedHost: that side is a human-maintained
// mapping, not an observation.
//
// It deliberately does **not** require two different sources the way C1 does. C1 needs corroboration
// because its whole evidence is that two reporters independently said the same string; C6's evidence is
// the allowlist entry, which is no less true if the same connector happens to report both halves.
func evalC6(ctx context.Context, store ClaimStore, claim Claim) ([]Match, error) {
	if claim.Namespace != NamespaceServerAddress {
		return nil, nil
	}
	others, err := store.ClaimsFor(ctx, claim.Namespace, claim.Value)
	if err != nil {
		return nil, err
	}
	var matches []Match
	for _, other := range others {
		if !distinctClaims(claim, other) {
			continue
		}
		allowlisted, observed, ok := orientC6(claim, other)
		if !ok {
			continue
		}
		matches = append(matches, Match{
			RuleID:  "C6",
			Certain: true,
			Score:   certainScore,
			Rationale: fmt.Sprintf(
				"the allowlist %s reads maps host %s to vendor %q, and %s observes an outbound "+
					"dependency on that host; the vendor the notices are about and the third party "+
					"observed from traffic are one entity. A configured assertion, not a resemblance "+
					"(FR-119)",
				allowlisted.SourceID, claim.Value, allowlisted.Attr(AttrVendorAllowlistedHost),
				observed.SourceID),
			EntityA:            claim.EntityID,
			EntityB:            other.EntityID,
			SupportingClaimIDs: supporting(claim, other),
		})
	}
	return matches, nil
}

// orientC6 decides which of two claims on one host name is the allowlist's assertion.
//
// Exactly one side must carry the marker. Two allowlisted sides means two allowlist entries map one
// host to two vendors, which is a **disagreement** for a person to settle rather than a merge to make
// automatically; neither side means two observations of one host, which is C1's business.
func orientC6(a, b Claim) (allowlisted, observed Claim, ok bool) {
	switch {
	case a.Attr(AttrVendorAllowlistedHost) != "" && b.Attr(AttrVendorAllowlistedHost) == "":
		return a, b, true
	case b.Attr(AttrVendorAllowlistedHost) != "" && a.Attr(AttrVendorAllowlistedHost) == "":
		return b, a, true
	default:
		return Claim{}, Claim{}, false
	}
}

// evalP6: two readings of what may be one announcement, with no shared identifier (FR-070).
//
// The candidate scan reads the vendor-notice namespace, for the reason probableMatches gives: the
// predicate is structural — same vendor, same product, windows that are not disjoint — and structure is
// not an indexable lookup. It is bounded by the number of announcements the allowlist let through,
// which is the smallest claim population in the graph.
func evalP6(ctx context.Context, store ClaimStore, claim Claim) ([]Match, error) {
	subject, ok := parseNoticeWindow(claim.Value)
	if !ok {
		return nil, nil
	}
	candidates, err := store.ClaimsInNamespaces(ctx, []string{NamespaceVendorNotice})
	if err != nil {
		return nil, err
	}
	var matches []Match
	for _, other := range candidates {
		if !distinctClaims(claim, other) {
			continue
		}
		candidate, ok := parseNoticeWindow(other.Value)
		if !ok {
			continue
		}
		why, ok := subject.comparableTo(candidate)
		if !ok {
			continue
		}
		if !typesMayMatch(claim.EntityType, other.EntityType) {
			continue
		}
		matches = append(matches, Match{
			RuleID:  "P6",
			Certain: false,
			Score:   ScoreP6,
			Rationale: fmt.Sprintf(
				"%s announces %s for vendor %q product %q over %s, and %s announces the same vendor "+
					"and product over %s; %s, and neither states a notice identifier the other shares, "+
					"so no certain rule can merge them. Probable only: nothing is merged until a "+
					"person decides (FR-070)",
				claim.SourceID, claim.Namespace, subject.vendor, subject.product, subject.window,
				other.SourceID, candidate.window, why),
			EntityA:            claim.EntityID,
			EntityB:            other.EntityID,
			SupportingClaimIDs: supporting(claim, other),
		})
	}
	return matches, nil
}

// noticeWindow is a parsed composite vendor-notice identifier.
type noticeWindow struct {
	vendor  string
	product string
	// window is the key as the feeder wrote it, kept verbatim for the rationale.
	window string
	// start and end bound the announced interval. startUnknown marks the vendor's vagueness
	// (FR-069) and endOpen an open-ended announcement, which a deprecation usually is.
	start        time.Time
	startUnknown bool
	end          time.Time
	endOpen      bool
}

// parseNoticeWindow reads `<vendor>/<product>@<start>..<end>`, where each bound is an RFC 3339
// instant or one of the markers the feeder writes for a vague start and an open end.
//
// It is strict on purpose. The same namespace also holds `<vendor>/<notice-id>`, and a vendor is
// perfectly capable of issuing an identifier with an `@` in it; a lenient parser would read one as a
// window and have P6 compare instants it invented.
func parseNoticeWindow(value string) (noticeWindow, bool) {
	slash := strings.Index(value, "/")
	at := strings.LastIndex(value, "@")
	if slash <= 0 || at <= slash+1 || at == len(value)-1 {
		return noticeWindow{}, false
	}
	key := value[at+1:]
	startText, endText, found := strings.Cut(key, "..")
	if !found || startText == "" || endText == "" {
		return noticeWindow{}, false
	}
	parsed := noticeWindow{
		vendor:  value[:slash],
		product: value[slash+1 : at],
		window:  key,
	}
	switch startText {
	case "unknown":
		parsed.startUnknown = true
	default:
		start, err := time.Parse(time.RFC3339Nano, startText)
		if err != nil {
			return noticeWindow{}, false
		}
		parsed.start = start
	}
	switch endText {
	case "open":
		parsed.endOpen = true
	default:
		end, err := time.Parse(time.RFC3339Nano, endText)
		if err != nil {
			return noticeWindow{}, false
		}
		parsed.end = end
	}
	return parsed, true
}

// comparableTo reports whether two composite identifiers are worth putting in front of a person, and
// the clause that says why.
//
// Same vendor and same product, and then a judgement about the windows: two that **overlap** may be
// one announcement read twice with a different end, a window nobody dated may be the same one somebody
// else dated, and the same window under two notice identifiers is a duplicate whichever way it arose.
// Two dated windows that do not touch are two maintenances, and reporting them would train a reader to
// dismiss this rule.
//
// It does not skip a pair a certain rule would take. That is Evaluate's job — it never reports a pair
// a certain rule matched as probable — and duplicating the check here would only suppress the pair C1
// *cannot* take, which is one source having read the same window twice under two identifiers.
func (w noticeWindow) comparableTo(other noticeWindow) (string, bool) {
	if w.vendor == "" || w.product == "" || w.vendor != other.vendor || w.product != other.product {
		return "", false
	}
	switch {
	case w.window == other.window:
		// The same window under two notice identifiers. C1 merges this pair when two sources claim
		// it, and Evaluate never reports a pair a certain rule already matched — so what reaches
		// here is one source having read two announcements that say the same thing, which is
		// exactly the duplicate a person should look at.
		return "they state the same vendor, product and window under different notice identifiers", true
	case w.startUnknown && other.startUnknown:
		return "neither states when the window begins", true
	case w.startUnknown != other.startUnknown:
		return "one states a window and the other was vague about when it begins", true
	case w.overlaps(other):
		return "the announced windows overlap without being the same window", true
	default:
		return "", false
	}
}

// overlaps reports whether two dated windows intersect, an open end reaching forward without bound.
// Each side must begin before the other ends, which is the half-open reading the rest of the project
// uses for a valid interval.
func (w noticeWindow) overlaps(other noticeWindow) bool {
	return w.startsBeforeEndOf(other) && other.startsBeforeEndOf(w)
}

// startsBeforeEndOf reports whether w begins before other ends.
func (w noticeWindow) startsBeforeEndOf(other noticeWindow) bool {
	return other.endOpen || w.start.Before(other.end)
}
