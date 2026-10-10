// SPDX-License-Identifier: Apache-2.0

package sanitise

// The vendor-notice connector's payloads, as its live poller writes them (003 T188, FR-071, FR-137).
//
// They carry no notice body: the extractor turns a message or a feed entry into typed fields before
// a payload exists, and has no field for the prose (vendornotice/sanitise.go). What reaches a
// recording is the allowlist's own configuration, closed vocabularies, instants, and three kinds of
// identifier — the resources a notice names, the vendor's notice id, and the pointer to the original.
//
//   - vendor and product are kept: they are the operator's allowlist, matched rather than parsed (§2.2).
//   - an affected resource is kept when it is shaped like a GCP location, so a notice about a region
//     still joins the GCP connector's region, and pseudonymised otherwise: a notice can name an
//     organisation's own resource as easily as a vendor's product.
//   - a notice id and a pointer are pseudonymised. A pointer is a message id or a feed entry's URL;
//     in a recording it only needs to stay the same thing twice, which is what makes a reading from
//     two sources one change (FR-076).
//   - a cycle gap's `why` is rewritten by the pre-pass (vendornotice.PreparePayload) before this
//     table sees it, because as written it is an error message, and an error from a mailbox read can
//     quote the mailbox.
func addVendorNoticeRows(p *Policy) {
	set := func(path string, rule Rule) { p.fields[CanonicalField(path)] = rule }
	keep := func(path, why string) { set(path, Rule{Disposition: Verbatim, Why: why}) }
	pseudo := func(path string, kind Kind, why string) {
		set(path, Rule{Disposition: Pseudonym, Kind: kind, Why: why})
	}
	for _, kind := range []string{"announcements", "corrections"} {
		root := "vendornotice." + kind
		a := root + ".announcements[]"
		keep(a+".vendor", "the allowlist's vendor slug: configuration (§2.2)")
		keep(a+".product", "the allowlist's product: configuration (§2.2)")
		keep(a+".kind", "a closed enumeration")
		keep(a+".state", "a closed enumeration")
		keep(a+".source", "which of the three sources read it")
		keep(a+".window_start", "the announced instant (§4)")
		keep(a+".window_start_unknown", "a stated omission")
		keep(a+".window_end", "the announced instant (§4)")
		set(a+".affected_resources[]", Rule{Disposition: Pseudonym, Kind: KindResource, Shape: ShapeTemplate,
			Template: "{region}" + TemplateAlternatives + "{resource}",
			Why:      "a location kept, so it joins the GCP region; anything else pseudonymised; a value with a path in it dropped"})
		keep(a+".alternate_products[]", "the vendor's own product titles, from its public feed; matched against the allowlist")
		set(a+".locations[]", Rule{Disposition: Pseudonym, Kind: KindResource, Shape: ShapeTemplate, Template: "{region}",
			Why: "a vendor location id; kept when shaped like one, dropped otherwise"})
		pseudo(a+".notice_id", KindNoticeID, "the vendor's own notice identifier")
		pseudo(a+".pointer", KindNoticeID, "a message id or feed entry; stable within the recording, which is all a replay needs")
		u := root + ".unextracted[]"
		keep(u+".reason", "a published reason")
		keep(u+".field", "the field that could not be read")
		keep(u+".source", "which source")
		pseudo(u+".pointer", KindNoticeID, "the reading that could not be extracted")
		keep(root+".considered", "a count")
		keep(root+".source", "which source the count belongs to")
	}
	keep("vendornotice.cycle.gaps[].source", "which source could not be read")
	keep("vendornotice.cycle.gaps[].why", "rewritten by the pre-pass to name no mailbox and no error text")
}
