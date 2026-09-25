// SPDX-License-Identifier: Apache-2.0

package vendornotice

import (
	"fmt"
	"sort"
	"strings"
)

// The allowlist (T070, FR-059, contract §2).
//
// The mailbox this feeder reads is an ordinary mailbox that receives provider mail, not a
// purpose-built feed: **most of what arrives in it is not a notice.** So the allowlist and the
// extraction-failure path carry real traffic, and neither is an edge case.
//
// An allowlist is also the only shape that makes "this announcement is from a vendor we depend on" a
// decision somebody made rather than a guess about a sender's name. Adding a vendor is configuration,
// never a code change, and no address, domain or sender is hard-coded in this package (FR-132b).
//
// # The drop is visible, and that is the whole design
//
// A non-allowlisted announcement is **dropped, counted, and reported with its reason**. It is not
// emitted "just in case": an unused product's maintenance window is noise that dilutes the one signal
// this feeder exists to carry, and a ranked change list at 02:10 with three irrelevant vendor rows in
// it is worse than one with none.
//
// Counted matters as much as dropped. "The graph knows about no upcoming vendor change" and "the
// feeder dropped forty announcements because nothing matched" are indistinguishable from the graph
// alone, and only one of them is good news (FR-078).

// Vendor is one allowlisted vendor and the products, hosts and senders the operator mapped to it.
type Vendor struct {
	// Slug is the vendor's identifier in the graph: configuration is the authority on vendor
	// identity, never a parsed sender domain.
	Slug string
	// Name is the display name.
	Name string
	// Products are the vendor's products this organisation depends on. An announcement about a
	// product not listed here is dropped: the vendor is right and the product is not.
	Products []string
	// Hosts are the host names the allowlist maps to this vendor. They become identity claims and
	// are what satisfy FR-119's *certain* resolution rule — the rule is certain because somebody
	// wrote that host name next to that vendor on purpose, not because two strings resemble.
	Hosts []string
	// Senders are the addresses that carry this vendor's notices. They are matching *configuration*
	// and are never stored: a sender address is a people identifier (FR-072).
	Senders []string
	// HostProducts maps a host name to the product it serves, for the one case a header cannot
	// answer: a `Deprecation` or `Sunset` on `api.acme-gpu.test` says when, and says nothing about
	// which product. Where a vendor has several allowlisted products, this is how an operator states
	// the mapping rather than the feeder guessing one (T082).
	//
	// It is optional. A vendor with one product needs no map, and a vendor with several and no map
	// gets an unextracted record naming the product field — which is visible, and a guess is not.
	HostProducts map[string]string
}

// productForHost returns the product an operator mapped a host to, matching the host itself or any
// parent domain of it, the same way VendorByHost matches. Empty when nothing is mapped.
func (v Vendor) productForHost(host string) string {
	needle := strings.ToLower(strings.TrimSpace(strings.TrimSuffix(host, ".")))
	for needle != "" {
		for mapped, product := range v.HostProducts {
			if strings.ToLower(strings.TrimSpace(mapped)) == needle {
				return strings.ToLower(strings.TrimSpace(product))
			}
		}
		dot := strings.Index(needle, ".")
		if dot < 0 {
			return ""
		}
		needle = needle[dot+1:]
	}
	return ""
}

// Allowlist is the vendor × product filter in force.
type Allowlist struct {
	vendors map[string]Vendor
	// bySender indexes the sender addresses, lower-cased. It exists so that matching is a lookup
	// rather than a scan, and so that the addresses live in exactly one place.
	bySender map[string]string
	// byHost indexes the host names.
	byHost map[string]string
	// order preserves the configured order for reporting.
	order []string
}

// NewAllowlist builds the filter. A duplicate slug is refused: two entries under one name would make
// a drop's reason unreadable and would let one of them be silently unreachable.
func NewAllowlist(vendors []Vendor) (*Allowlist, error) {
	a := &Allowlist{
		vendors:  map[string]Vendor{},
		bySender: map[string]string{},
		byHost:   map[string]string{},
	}
	for _, vendor := range vendors {
		slug := strings.ToLower(strings.TrimSpace(vendor.Slug))
		if slug == "" {
			return nil, fmt.Errorf("vendornotice: an allowlist entry with no slug; configuration is " +
				"the authority on vendor identity, so the identity cannot be blank")
		}
		if _, clash := a.vendors[slug]; clash {
			return nil, fmt.Errorf("vendornotice: vendor %q appears twice in the allowlist; two "+
				"entries under one name make a drop's reason unreadable and leave one unreachable", slug)
		}
		vendor.Slug = slug
		a.vendors[slug] = vendor
		a.order = append(a.order, slug)
		for _, sender := range vendor.Senders {
			a.bySender[strings.ToLower(strings.TrimSpace(sender))] = slug
		}
		for _, host := range vendor.Hosts {
			a.byHost[strings.ToLower(strings.TrimSpace(host))] = slug
		}
	}
	return a, nil
}

// Vendors returns the allowlisted vendors in configured order.
func (a *Allowlist) Vendors() []Vendor {
	out := make([]Vendor, 0, len(a.order))
	for _, slug := range a.order {
		out = append(out, a.vendors[slug])
	}
	return out
}

// Len returns how many vendors are allowlisted. A campaign with an empty allowlist reads a mailbox and
// drops everything, which looks exactly like a quiet week — so callers check it.
func (a *Allowlist) Len() int { return len(a.order) }

// Vendor returns one vendor by slug.
func (a *Allowlist) Vendor(slug string) (Vendor, bool) {
	vendor, ok := a.vendors[strings.ToLower(strings.TrimSpace(slug))]
	return vendor, ok
}

// VendorBySender returns the vendor a sender address is configured to carry notices for.
//
// It is an exact comparison on the whole address, not a domain match. A domain match would attribute
// every message from a shared platform domain to one vendor, and the platform domains are exactly the
// ones several vendors send from.
func (a *Allowlist) VendorBySender(sender string) (Vendor, bool) {
	slug, ok := a.bySender[strings.ToLower(strings.TrimSpace(sender))]
	if !ok {
		return Vendor{}, false
	}
	return a.vendors[slug], true
}

// VendorByHost returns the vendor a host name is mapped to, matching the host itself or any subdomain
// of it. `api.googleapis.com` matches an entry of `googleapis.com`; `notgoogleapis.com` does not,
// because the match is on a label boundary rather than a suffix.
func (a *Allowlist) VendorByHost(host string) (Vendor, bool) {
	needle := strings.ToLower(strings.TrimSpace(strings.TrimSuffix(host, ".")))
	if needle == "" {
		return Vendor{}, false
	}
	for {
		if slug, ok := a.byHost[needle]; ok {
			return a.vendors[slug], true
		}
		dot := strings.Index(needle, ".")
		if dot < 0 {
			return Vendor{}, false
		}
		needle = needle[dot+1:]
	}
}

// DropReason says why an announcement was not acted on. They are published constants because each one
// reaches the per-cycle report, and a report whose categories move between runs cannot be trended.
type DropReason string

// The published drop reasons.
const (
	// DropVendorNotAllowlisted means no allowlisted vendor claims the announcement.
	DropVendorNotAllowlisted DropReason = "vendor_not_allowlisted"
	// DropProductNotAllowlisted means the vendor is allowlisted and the product is not. This is the
	// common case and the reason the allowlist is vendor × product rather than vendor alone: a
	// provider we depend on announcing maintenance of a product we do not use is still noise.
	DropProductNotAllowlisted DropReason = "product_not_allowlisted"
	// DropNoProductStated means the announcement named an allowlisted vendor but no product, so the
	// feeder cannot tell whether it is in scope. It is dropped rather than assumed in scope: a
	// vendor-wide announcement attributed to every product would put the same window on everything.
	DropNoProductStated DropReason = "no_product_stated"
)

// Drop is one dropped announcement, as the per-cycle report carries it.
//
// It records the vendor and product **as the announcement stated them**, which is safe because both
// are identifiers rather than prose, and because a drop nobody can attribute is a drop nobody can act
// on: "forty dropped" is a number, "forty dropped, all from a vendor you have not allowlisted" is a
// finding. It carries no sender and no body.
type Drop struct {
	Reason DropReason
	// Vendor and Product are what the announcement claimed, empty where it claimed nothing.
	Vendor, Product string
	// Pointer is the message or feed-entry identifier, so a human can open the original.
	Pointer string
}

// Decision is what the allowlist made of one announcement.
type Decision struct {
	// Vendor is the matched vendor when InScope is true.
	Vendor Vendor
	// Product is the matched product, lower-cased.
	Product string
	// InScope says whether to act on the announcement.
	InScope bool
	// Drop explains why not, when InScope is false.
	Drop Drop
}

// Decide applies the allowlist to a claimed vendor slug and product.
//
// Both are matched, in that order, so the reason distinguishes "we do not depend on this vendor" from
// "we depend on this vendor but not this product" — which are different findings about the estate and
// the second one is the one worth reading.
func (a *Allowlist) Decide(vendorSlug, product, pointer string) Decision {
	slug := strings.ToLower(strings.TrimSpace(vendorSlug))
	vendor, known := a.vendors[slug]
	if !known {
		return Decision{Drop: Drop{
			Reason: DropVendorNotAllowlisted, Vendor: slug, Product: product, Pointer: pointer,
		}}
	}
	wanted := strings.ToLower(strings.TrimSpace(product))
	if wanted == "" {
		return Decision{Drop: Drop{
			Reason: DropNoProductStated, Vendor: slug, Pointer: pointer,
		}}
	}
	for _, allowed := range vendor.Products {
		if strings.ToLower(strings.TrimSpace(allowed)) == wanted {
			return Decision{Vendor: vendor, Product: wanted, InScope: true}
		}
	}
	return Decision{Drop: Drop{
		Reason: DropProductNotAllowlisted, Vendor: slug, Product: wanted, Pointer: pointer,
	}}
}

// DropSummary groups drops by reason for the per-cycle report, sorted so two runs of one cycle read
// the same.
func DropSummary(drops []Drop) []string {
	byReason := map[DropReason]int{}
	vendorsByReason := map[DropReason]map[string]bool{}
	for _, drop := range drops {
		byReason[drop.Reason]++
		if vendorsByReason[drop.Reason] == nil {
			vendorsByReason[drop.Reason] = map[string]bool{}
		}
		if drop.Vendor != "" {
			vendorsByReason[drop.Reason][drop.Vendor] = true
		}
	}
	reasons := make([]DropReason, 0, len(byReason))
	for reason := range byReason {
		reasons = append(reasons, reason)
	}
	sort.Slice(reasons, func(i, j int) bool { return reasons[i] < reasons[j] })

	out := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		vendors := make([]string, 0, len(vendorsByReason[reason]))
		for vendor := range vendorsByReason[reason] {
			vendors = append(vendors, vendor)
		}
		sort.Strings(vendors)
		line := fmt.Sprintf("%s=%d", reason, byReason[reason])
		if len(vendors) > 0 {
			line += " (" + strings.Join(vendors, ",") + ")"
		}
		out = append(out, line)
	}
	return out
}
