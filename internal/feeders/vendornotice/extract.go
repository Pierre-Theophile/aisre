// SPDX-License-Identifier: Apache-2.0

package vendornotice

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Extraction (T071, T072, FR-073, FR-074, contract §3).
//
// **Announcement text is untrusted input from outside the organisation.** Anyone who can send mail to
// the mailbox can put text in front of this feeder, and a status page is a third party's mutable
// document. Three rules follow, and the first is the one the rest depend on:
//
//  1. extraction produces values **validated against the published typed schema** — six fields, each
//     with a type, and nothing else crosses;
//  2. **no text from any announcement is ever treated as an instruction by any component.** The typed
//     extraction boundary is the same boundary that governs what may be sent to a model provider
//     (FR-141): nothing crosses it that would not be permitted into a recording;
//  3. an extraction that cannot produce a valid typed result **fails loudly, naming the field**,
//     rather than emitting a guess.
//
// Rule 2 is structural here rather than a promise. The typed result has no free-text field — no
// summary, no excerpt, no "notes" — so there is no member an injected instruction could travel in.
// The summary a change node carries is built from the typed fields afterwards (§4), which is the whole
// difference between a summary and an excerpt.
//
// # Unextracted is a first-class outcome, not an error to swallow
//
// Most of what arrives in the mailbox is not a notice. An announcement the feeder cannot place — no
// recognisable vendor, no window, no product — is recorded as **unextracted**, with its reason and its
// message pointer so a human can see what the feeder could not read, and it is **not emitted as a
// change** (FR-074). Emitting a guess would put a window in the graph that somebody will plan around.

// AnnouncementKind is the typed reading of what an announcement is about. It is a closed set: an
// announcement whose kind cannot be determined is unextracted rather than defaulted, because the kind
// decides which ChangeKind the graph records and there is no harmless default.
type AnnouncementKind string

// The published announcement kinds, one per row of contract §5.
const (
	// KindMaintenance is a maintenance window → CLOUD_MAINTENANCE.
	KindMaintenance AnnouncementKind = "maintenance"
	// KindDeprecation is an announced removal or breaking change → DEPRECATION.
	KindDeprecation AnnouncementKind = "deprecation"
	// KindIncident is a provider-declared incident → VENDOR_INCIDENT.
	KindIncident AnnouncementKind = "incident"
)

// Valid reports whether k is one of the three published kinds.
func (k AnnouncementKind) Valid() bool {
	switch k {
	case KindMaintenance, KindDeprecation, KindIncident:
		return true
	default:
		return false
	}
}

// Window is the announced valid interval.
//
// `StartUnknown` exists because a vendor who states a window only vaguely — "in the coming weeks", "in
// a future release" — gets a valid start marked **unknown**, and the vendor's own wording is **not**
// stored in place of a timestamp (FR-069). That is the point at which "no free text" and "do not
// guess" agree rather than conflict: the honest record is a marked-unknown start, not a sentence.
type Window struct {
	// Start is when the announced change begins. Zero with StartUnknown set means the vendor was
	// vague.
	Start time.Time
	// StartUnknown marks a vague start. It is not "we failed to parse": it is "the vendor did not
	// say", which is a fact about the announcement.
	StartUnknown bool
	// End bounds a window that has one. Zero means open-ended, which a deprecation usually is.
	End time.Time
}

// Validate refuses a window that cannot be recorded honestly.
func (w Window) Validate() error {
	if w.StartUnknown && !w.Start.IsZero() {
		return errors.New("vendornotice: a window marked start-unknown that also carries a start; " +
			"one of the two is a guess")
	}
	if !w.StartUnknown && w.Start.IsZero() {
		return errors.New("vendornotice: a window with no start and no unknown marker")
	}
	if !w.End.IsZero() && !w.Start.IsZero() && w.End.Before(w.Start) {
		return fmt.Errorf("vendornotice: a window ending %s before it starts %s",
			w.End.Format(time.RFC3339), w.Start.Format(time.RFC3339))
	}
	return nil
}

// Announcement is the typed result of extraction: the six fields of contract §3, and nothing else.
//
// There is deliberately no free-text member. An injected instruction has no field to travel in, which
// is how rule 2 is enforced rather than promised.
type Announcement struct {
	// Vendor is the vendor slug, matched against the allowlist. Configuration is the authority on
	// vendor identity, so this is a slug and never a parsed sender domain.
	Vendor string
	// Product is the affected product.
	Product string
	// Kind is what the announcement is about.
	Kind AnnouncementKind
	// Window is the announced interval.
	Window Window
	// AffectedResources are the resources the announcement names — a region, an API endpoint, a
	// model or product name. They are emitted as **identity claims** so the resolution layer can
	// attach them; the feeder does not invent a `depends-on` edge from them (§5).
	AffectedResources []string
	// NoticeID is the vendor's own identifier for the announcement where it states one. It is what
	// makes the same announcement from two sources addressable as one change (FR-076).
	NoticeID string
	// Pointer is the message or feed-entry identifier, so a human can open the original. It is a
	// pointer and not content (§4).
	Pointer string
	// Source names which of the three sources read it, for the per-cycle report and for the
	// deterministic ref when the vendor states no identifier.
	Source SourceKind
}

// SourceKind is which of the three sources an announcement came from.
type SourceKind string

// The published source kinds.
const (
	SourceMailbox    SourceKind = "mailbox"
	SourceStatusPage SourceKind = "status_page"
	SourceChangelog  SourceKind = "changelog"
	// SourceDeprecationHeader is an RFC 9745 `Deprecation` or RFC 8594 `Sunset` header seen on a
	// vendor API this integration already calls — the only standardised deprecation source.
	SourceDeprecationHeader SourceKind = "deprecation_header"
)

// MaxFieldBytes bounds every extracted field. It is generous for an identifier and far too short for
// prose, which is the only property it needs: a field longer than this is body text that reached the
// typed result, and the whole point of the boundary is that body text cannot.
const MaxFieldBytes = 256

// MaxAffectedResources bounds the resource list. An announcement naming two hundred resources is a
// vendor-wide notice being read as a specific one, and truncating silently would hide that.
const MaxAffectedResources = 64

// UnextractedReason says what could not be read. They are published constants because each reaches the
// per-cycle report and the unextracted record a human reads.
type UnextractedReason string

// The published reasons.
const (
	// UnextractedNoVendor means no recognisable vendor.
	UnextractedNoVendor UnextractedReason = "no_recognisable_vendor"
	// UnextractedNoProduct means no recognisable product.
	UnextractedNoProduct UnextractedReason = "no_recognisable_product"
	// UnextractedNoKind means the announcement's kind could not be determined. There is no default:
	// the kind decides the ChangeKind the graph records.
	UnextractedNoKind UnextractedReason = "no_recognisable_kind"
	// UnextractedNoWindow means neither a window nor a vague-start marker could be produced.
	UnextractedNoWindow UnextractedReason = "no_recognisable_window"
	// UnextractedInvalidWindow means a window was produced and does not validate.
	UnextractedInvalidWindow UnextractedReason = "invalid_window"
	// UnextractedFieldTooLong means a field exceeded MaxFieldBytes, which means body text reached
	// the typed result.
	UnextractedFieldTooLong UnextractedReason = "field_too_long"
	// UnextractedFieldSpansLines means a field spans lines, same reasoning.
	UnextractedFieldSpansLines UnextractedReason = "field_spans_lines"
	// UnextractedTooManyResources means the resource list exceeded MaxAffectedResources.
	UnextractedTooManyResources UnextractedReason = "too_many_affected_resources"
)

// Unextracted is an announcement the feeder could not place. It is recorded so a human can see what
// the feeder could not read, and it carries a pointer rather than content.
type Unextracted struct {
	Reason UnextractedReason
	// Field names the field that failed, which is what FR-073's "failing loudly naming the field"
	// asks for. Empty where the failure is not about one field.
	Field string
	// Pointer is the message identifier.
	Pointer string
	// Source is which source read it.
	Source SourceKind
}

// Error renders the refusal. It names the field and never quotes the announcement: quoting it would
// put untrusted text in a log, which is the one place FR-071 does not want it either.
func (u *Unextracted) Error() string {
	field := u.Field
	if field == "" {
		field = "the announcement"
	}
	return fmt.Sprintf("vendornotice: %s could not be extracted from %s (%s): %s. Recorded as "+
		"unextracted with its pointer rather than emitted as a guessed change (FR-074)",
		field, u.Pointer, u.Source, u.Reason)
}

// Validate checks an extracted announcement against the published typed schema, returning an
// *Unextracted naming the first field that fails.
//
// The order is deliberate: vendor, product, kind, then the window. It reports the *most basic* missing
// thing first, because "no recognisable vendor" is a more useful finding than "invalid window" about
// the same message — the first says the mailbox carries mail this feeder has no business reading, and
// the second suggests a parser bug.
func (a Announcement) Validate() error {
	if strings.TrimSpace(a.Vendor) == "" {
		return &Unextracted{Reason: UnextractedNoVendor, Field: "vendor", Pointer: a.Pointer, Source: a.Source}
	}
	if strings.TrimSpace(a.Product) == "" {
		return &Unextracted{Reason: UnextractedNoProduct, Field: "product", Pointer: a.Pointer, Source: a.Source}
	}
	if !a.Kind.Valid() {
		return &Unextracted{Reason: UnextractedNoKind, Field: "kind", Pointer: a.Pointer, Source: a.Source}
	}
	if a.Window.Start.IsZero() && !a.Window.StartUnknown {
		return &Unextracted{Reason: UnextractedNoWindow, Field: "window", Pointer: a.Pointer, Source: a.Source}
	}
	if err := a.Window.Validate(); err != nil {
		return &Unextracted{Reason: UnextractedInvalidWindow, Field: "window", Pointer: a.Pointer, Source: a.Source}
	}
	if len(a.AffectedResources) > MaxAffectedResources {
		return &Unextracted{Reason: UnextractedTooManyResources, Field: "affected_resources", Pointer: a.Pointer, Source: a.Source}
	}
	// Every string field is checked for the two shapes body text takes. This is the check that makes
	// "no text crosses the boundary" enforced rather than intended: a parser that put a paragraph in
	// `Product` would otherwise put it in the graph.
	for field, value := range a.stringFields() {
		if len(value) > MaxFieldBytes {
			return &Unextracted{Reason: UnextractedFieldTooLong, Field: field, Pointer: a.Pointer, Source: a.Source}
		}
		if strings.ContainsAny(value, "\n\r") {
			return &Unextracted{Reason: UnextractedFieldSpansLines, Field: field, Pointer: a.Pointer, Source: a.Source}
		}
	}
	return nil
}

// stringFields returns every string-valued field, for the body-text checks.
func (a Announcement) stringFields() map[string]string {
	out := map[string]string{
		"vendor":    a.Vendor,
		"product":   a.Product,
		"kind":      string(a.Kind),
		"notice_id": a.NoticeID,
		"pointer":   a.Pointer,
	}
	for i, resource := range a.AffectedResources {
		out[fmt.Sprintf("affected_resources[%d]", i)] = resource
	}
	return out
}

// Normalise lower-cases and trims the identifier fields and sorts the resource list, so that the same
// announcement read twice — or read by two sources that spell a product differently in case — produces
// one value and therefore one deterministic ref (FR-076).
func (a Announcement) Normalise() Announcement {
	a.Vendor = strings.ToLower(strings.TrimSpace(a.Vendor))
	a.Product = strings.ToLower(strings.TrimSpace(a.Product))
	a.NoticeID = strings.TrimSpace(a.NoticeID)
	a.Pointer = strings.TrimSpace(a.Pointer)
	resources := make([]string, 0, len(a.AffectedResources))
	seen := map[string]bool{}
	for _, resource := range a.AffectedResources {
		trimmed := strings.TrimSpace(resource)
		if trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		resources = append(resources, trimmed)
	}
	sort.Strings(resources)
	a.AffectedResources = resources
	return a
}

// Summary builds the bounded derived summary a change node carries (§4).
//
// It is built **from the typed fields**, never from the text, and that is the whole difference between
// a summary and an excerpt — and why the summary can be committed at all. Every component of it is a
// value that already passed Validate, so nothing here can carry an instruction.
func (a Announcement) Summary() string {
	var b strings.Builder
	switch a.Kind {
	case KindMaintenance:
		b.WriteString("maintenance announced for ")
	case KindDeprecation:
		b.WriteString("deprecation announced for ")
	case KindIncident:
		b.WriteString("incident declared for ")
	default:
		b.WriteString("announcement for ")
	}
	b.WriteString(a.Vendor)
	b.WriteString(" ")
	b.WriteString(a.Product)

	switch {
	case a.Window.StartUnknown:
		// The vendor's wording is not stored in place of a timestamp (FR-069). "At an unstated time"
		// is this feeder's own phrase about a fact, not the vendor's prose.
		b.WriteString(", at an unstated time")
	case !a.Window.End.IsZero():
		b.WriteString(", " + a.Window.Start.UTC().Format(time.RFC3339) +
			" to " + a.Window.End.UTC().Format(time.RFC3339))
	default:
		b.WriteString(", from " + a.Window.Start.UTC().Format(time.RFC3339))
	}
	if n := len(a.AffectedResources); n > 0 {
		fmt.Fprintf(&b, ", %d affected resource", n)
		if n > 1 {
			b.WriteString("s")
		}
	}
	return b.String()
}
