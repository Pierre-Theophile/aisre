// SPDX-License-Identifier: Apache-2.0

package vendornotice

import (
	"fmt"
	"strings"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The vendor node (T077, FR-077, FR-119, contract §8).
//
// A THIRD_PARTY node for every allowlisted vendor the feeder has an announcement for, carrying the
// vendor's name and its allowlisted products, and identity claims **including the host names the
// allowlist maps to the vendor**.
//
// Those host-name claims are the point. They are what satisfy FR-119's *certain* resolution rule: when
// an observed outbound dependency's server address matches a host name the allowlist maps to a vendor,
// the vendor the notices are about and the third party the graph already observed from traffic are one
// entity. The rule is certain because it rests on **a configured assertion** — somebody wrote that host
// name next to that vendor on purpose — and not on a resemblance between two strings.
//
// That is the difference between this and every probable rule: `stripe` resembling `stripe-api` is a
// guess, and `api.stripe.com` appearing under the `stripe` entry of an allowlist a human maintains is
// evidence.

// Property names for a vendor node. The SDK names the generic ones; the vendor-specific ones live under
// `sre.vendor.` so a reader can tell which half of the properties this feature invented (FR-007).
const (
	// PropVendorSlug is the vendor's identifier, which is also its ref value.
	PropVendorSlug = "sre.vendor.slug"
	// PropVendorProducts is the allowlisted products, sorted.
	PropVendorProducts = "sre.vendor.products"
	// PropVendorHosts is the host names the allowlist maps to the vendor, sorted. They are recorded
	// as a property as well as claimed, because a reader asking "why did these two merge" wants to
	// see the mapping without running an audit query.
	PropVendorHosts = "sre.vendor.hosts"
	// PropAllowlistedHost marks a `server.address` claim as the allowlist's own assertion, valued by
	// the vendor slug it maps the host to. It is what FR-119's certain rule orients on: without it
	// C6 cannot tell the configured side from the observed side, and the only other way to guess —
	// the source id — would stop working the day somebody renames a source.
	PropAllowlistedHost = "sre.vendor.allowlisted_host"
)

// PropVendorValidFromIsABound says the vendor node's valid start is a **bound** rather than the instant
// the organisation's relationship with the vendor began.
//
// That relationship predates this connector by definition, and nothing in a notice states when it
// started. The earliest instant this feeder can show is the first announcement it read, which is a
// bound: the relationship existed at least from then.
const PropVendorValidFromIsABound = "sre.vendor.valid_from_is_a_bound"

// VendorValidFromBoundReason is what the property holds.
const VendorValidFromBoundReason = "the earliest announcement this feeder read from the vendor; the " +
	"instant the organisation's relationship with them began is not a fact any notice carries"

// VendorNode renders the THIRD_PARTY node for one allowlisted vendor.
//
// # Its valid start is a bound, stated, rather than marked unknown
//
// The first version of this marked the start **unknown** and argued that a bound would be misleading: a
// vendor has no creation instant, the relationship predates the connector, and the first announcement we
// happened to read is not when it began.
//
// The conformance shuffle refused that, and was right to. An unknown start is stored as a
// first-observation placeholder, so its value is whichever of the vendor's events reached the graph
// first — the node or one of its claims. Permute them inside the reordering window and the interval
// starts somewhere else, which makes the fixture order-dependent and, worse, makes the graph's answer to
// "since when have we known about this vendor" depend on delivery order rather than on anything that
// happened.
//
// So it takes the bound and says so, exactly as FR-125's owner node does after the same argument: the
// earliest announcement instant, with PropVendorValidFromIsABound naming what the instant is and is not.
// A reader can tell a bound from a fact; nobody can tell a placeholder from either.
func VendorNode(vendor Vendor, observedAt time.Time) (feeder.NodeFact, error) {
	if observedAt.IsZero() {
		return feeder.NodeFact{}, fmt.Errorf("vendornotice: a vendor node with no instant to bound it " +
			"by; the bound is the earliest announcement this feeder read, and there is no version of " +
			"this that guesses one")
	}
	props := feeder.NewProps().
		Str(PropVendorSlug, vendor.Slug).
		Str(PropVendorValidFromIsABound, VendorValidFromBoundReason).
		Str(feeder.PropThirdParty, vendor.Slug)
	if products := sortedLower(vendor.Products); len(products) > 0 {
		props = props.Strs(PropVendorProducts, products...)
	}
	if hosts := sortedLower(vendor.Hosts); len(hosts) > 0 {
		props = props.Strs(PropVendorHosts, hosts...)
	}
	built, err := props.Build()
	if err != nil {
		return feeder.NodeFact{}, err
	}
	name := vendor.Name
	if name == "" {
		name = vendor.Slug
	}
	return feeder.NodeFact{
		Ref:         feeder.Ref(NSVendor, vendor.Slug),
		Type:        graphv1.NodeType_THIRD_PARTY,
		DisplayName: name,
		Props:       built,
		// The bound, not a placeholder. See the doc comment.
		ValidAt: observedAt,
	}, nil
}

// VendorClaims returns the identifiers this feeder knows a vendor by, including the one it addresses the
// vendor by (FR-115) and every host name the allowlist maps to it (FR-077).
//
// The host claims are minted in `server.address`, which is the namespace an observed outbound dependency
// is named in — so the claim and the observation land on the same identifier and FR-119's rule has
// something to fire on. Minting them in a vendor-specific namespace instead would be the commonest way
// to write this and would mean the rule never fires.
func VendorClaims(vendor Vendor) []Claim {
	claims := []Claim{
		{Namespace: NSVendor, Value: vendor.Slug, Why: "the ref this feeder addresses the vendor by"},
	}
	for _, host := range sortedLower(vendor.Hosts) {
		claims = append(claims, Claim{
			Namespace: NSServerAddress,
			Value:     host,
			Why: "a host name the allowlist maps to this vendor; FR-119's certain rule rests on this " +
				"configured assertion, not on a resemblance",
			Attrs: map[string]string{PropAllowlistedHost: vendor.Slug},
		})
	}
	return claims
}

// sortedLower lower-cases, trims, de-duplicates and sorts, so a node's properties do not gain a version
// because somebody reordered a YAML list.
func sortedLower(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, value := range in {
		trimmed := strings.ToLower(strings.TrimSpace(value))
		if trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		out = append(out, trimmed)
	}
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}
