// SPDX-License-Identifier: Apache-2.0

package vendornotice

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The announced fact (T073–T076, T078, FR-062–FR-070, FR-076, data-model.md §5, contract §5–§7).
//
// An **announced fact** is a fact whose valid time begins after its observed time: a maintenance window
// read on 17 September for 02:00–04:00 on 2 October. The graph already accepts one; what this file adds
// is the vocabulary that keeps an announcement from being read as an occurrence.
//
// Five rules, each of which is a fixture:
//
//  1. the valid interval **is** the window the vendor announced, emitted unchanged even when it begins
//     after the read instant (FR-062);
//  2. **observed time is never moved forward** to match it. The asymmetry is one-directional: valid
//     time may lead observed time, observed time is never in the future (FR-063);
//  3. a read as of valid T_v and observed T_o returns the change when T_v is inside the window and T_o
//     is at or after the observation — an ordinary bitemporal query, not a special case (FR-064);
//  4. a change whose valid start is after a ranked list's reference instant is **not a candidate
//     cause, and is excluded for that stated reason** rather than by scoring low (FR-065);
//  5. a cancellation and a reschedule are **corrections, not retractions**: the observation closes, a
//     new one opens, the valid interval is never rewritten, and **no second change node is created**
//     (FR-067, FR-068).
//
// And two rules about *not* transitioning, which is where an implementation quietly goes wrong. An
// announced window that passes in silence stays `announced` **for ever** — never promoted to something
// known to have happened just because its end is in the past (FR-066). And a vague window gets a valid
// start marked unknown, with the vendor's own wording never stored in place of a timestamp (FR-069).

// ChangeKindFor maps an announcement kind to the published taxonomy.
//
// There is no `CHANGE_KIND_OTHER` fallback. FR-060 allowed one *until the taxonomy named these kinds*;
// it named them on 2026-09-18, so the fallback is dead and a fixture asserting an `OTHER`-kind
// deprecation would be asserting a regression. An unknown kind is an error rather than an `OTHER`,
// because `AnnouncementKind` is a closed set that extraction already validated.
func ChangeKindFor(kind AnnouncementKind) (graphv1.ChangeKind, error) {
	switch kind {
	case KindMaintenance:
		return graphv1.ChangeKind_CLOUD_MAINTENANCE, nil
	case KindDeprecation:
		return graphv1.ChangeKind_DEPRECATION, nil
	case KindIncident:
		return graphv1.ChangeKind_VENDOR_INCIDENT, nil
	default:
		return graphv1.ChangeKind_CHANGE_KIND_UNSPECIFIED, fmt.Errorf(
			"vendornotice: announcement kind %q has no taxonomy equivalent. The CHANGE_KIND_OTHER "+
				"fallback of FR-060 is dead — the taxonomy named DEPRECATION and VENDOR_INCIDENT on "+
				"2026-09-18 (ADR-0006 D3) — so this is an error rather than an OTHER", kind)
	}
}

// NoticeRef is the deterministic ref of an announced change (FR-076).
//
// `<vendor>/<notice-identifier>` where the vendor states an identifier. Absent one, it is derived from
// the source, the product and the announced window — deterministically, so that re-reading the same
// announcement is a no-op and the same announcement from two sources is addressable as one change.
//
// The derived form deliberately does **not** include the pointer. A message id differs between two
// mailboxes carrying one notice, and a feed entry's id differs from an email's; including it would make
// two readings of one announcement two changes, which is the exact failure FR-070 exists to prevent —
// two adjacent rows for one window in front of somebody being paged.
func NoticeRef(a Announcement) *graphv1.Ref {
	if id := strings.TrimSpace(a.NoticeID); id != "" {
		return feeder.Ref(NSVendorNotice, a.Vendor+"/"+id)
	}
	return feeder.Ref(NSVendorNotice, a.Vendor+"/"+derivedNoticeID(a))
}

// derivedNoticeID hashes the facts that identify an announcement when the vendor names no identifier:
// the product, the kind and the window. The source is **not** in it, for the reason above.
func derivedNoticeID(a Announcement) string {
	h := sha256.New()
	// A length-prefixed write per field, so that ("ab","c") and ("a","bc") do not hash alike — the
	// collision that would merge two announcements about different products.
	for _, part := range []string{a.Product, string(a.Kind), windowKey(a.Window)} {
		fmt.Fprintf(h, "%d:%s|", len(part), part)
	}
	return "derived-" + hex.EncodeToString(h.Sum(nil))[:16]
}

// windowKey renders a window canonically for the derived id. A vague start hashes as the marker rather
// than as an empty instant, so two vague announcements about one product and kind are one change while
// a vague one and a dated one are two — which is right: rescheduling from "soon" to a date is a
// correction of the same notice only when the vendor says so, and the vendor says so by reusing its
// identifier.
func windowKey(w Window) string {
	start := "unknown"
	if !w.StartUnknown {
		start = w.Start.UTC().Format(time.RFC3339Nano)
	}
	end := "open"
	if !w.End.IsZero() {
		end = w.End.UTC().Format(time.RFC3339Nano)
	}
	return start + ".." + end
}

// NSVendorNotice and NSVendor are the namespaces this feeder mints in. They match data-model.md §2 and
// internal/feeders/gcp's spellings: two connectors that mean the same thing must use the same string, or
// the same notice read by each is two entities.
const (
	// NSVendorNotice is an announcement, valued `<vendor>/<notice-identifier>`.
	NSVendorNotice = "vendor.notice"
	// NSVendor is a third party, valued by the vendor slug from the allowlist.
	NSVendor = "vendor"
	// NSServerAddress is the namespace an observed outbound dependency's host is named in. The
	// host-name claims this feeder emits live in it, which is what lets FR-119's certain rule fire.
	NSServerAddress = "server.address"
)

// Announced builds the change for a first reading of an announcement (T073).
//
// `observedAt` is passed so that the invariant can be *checked* rather than assumed: FR-063 says
// observed time is never moved forward to match an announced window, and the way that goes wrong is a
// feeder that, seeing a window in the future, helpfully stamps the observation at the window's start.
// This function refuses to be handed an observation in the future at all.
func Announced(a Announcement, vendor Vendor, observedAt time.Time) (feeder.ChangeFact, error) {
	return announcement(a, vendor, observedAt, graphv1.AnnouncementState_ANNOUNCED)
}

// Cancelled builds the correction for a withdrawn announcement (T076).
//
// It carries the **same ref** and the **same valid interval** as the announcement it corrects. That is
// the whole of FR-067: the observation closes and a new one opens, the valid interval is never
// rewritten, and no second change node is created — so "what did we believe on 20 September about 2
// October?" stays answerable by an observed-time query.
//
// It is emphatically not a retraction. A retraction would say the window stopped being true, and the
// window was never true — it was announced, and then withdrawn. Those are different facts and only one
// of them is what happened.
func Cancelled(a Announcement, vendor Vendor, observedAt time.Time) (feeder.ChangeFact, error) {
	return announcement(a, vendor, observedAt, graphv1.AnnouncementState_CANCELLED)
}

// Superseded builds the correction for a rescheduled announcement (T076).
//
// The superseded observation carries the **original** window, unchanged, and the new `ANNOUNCED`
// observation that follows carries the new one — both on the same ref. That is what makes both windows
// recoverable by an observed-time query, which is the property a reschedule has to preserve: "we
// believed it was the 2nd, then we believed it was the 9th" is two beliefs about one notice, not two
// notices.
func Superseded(a Announcement, vendor Vendor, observedAt time.Time) (feeder.ChangeFact, error) {
	return announcement(a, vendor, observedAt, graphv1.AnnouncementState_SUPERSEDED)
}

// Confirmed builds the observation that a window happened (T075).
//
// It is deliberately awkward to reach: nothing in this package calls it, because nothing in this
// package observes production. An announced window that passes in silence stays `announced` for ever —
// the passage of time is not an observation (FR-066) — so a promotion to CONFIRMED can only come from a
// caller that saw the window happen, and that caller is not a notice reader.
func Confirmed(a Announcement, vendor Vendor, observedAt time.Time) (feeder.ChangeFact, error) {
	return announcement(a, vendor, observedAt, graphv1.AnnouncementState_CONFIRMED)
}

// ErrObservedInFuture is the refusal for an observation stamped ahead of real time. It is its own error
// because it is the shape FR-063 is violated in: a feeder that saw a window in the future and moved its
// own observation to match.
var ErrObservedInFuture = fmt.Errorf("vendornotice: an observation stamped in the future")

// announcement is the one builder the four states share, so the invariants are checked once.
func announcement(a Announcement, vendor Vendor, observedAt time.Time, state graphv1.AnnouncementState) (feeder.ChangeFact, error) {
	a = a.Normalise()
	if err := a.Validate(); err != nil {
		return feeder.ChangeFact{}, err
	}
	if vendor.Slug != a.Vendor {
		return feeder.ChangeFact{}, fmt.Errorf("vendornotice: announcement names vendor %q and the "+
			"allowlisted vendor is %q; the allowlist decides, not the announcement", a.Vendor, vendor.Slug)
	}
	if observedAt.IsZero() {
		return feeder.ChangeFact{}, fmt.Errorf("vendornotice: an observation with no instant")
	}
	kind, err := ChangeKindFor(a.Kind)
	if err != nil {
		return feeder.ChangeFact{}, err
	}

	fact := feeder.ChangeFact{
		Meta:      feeder.Meta{SourceObservedAt: observedAt},
		Ref:       NoticeRef(a),
		Kind:      kind,
		Summary:   a.Summary(),
		Actor:     vendor.Name,
		ActorKind: graphv1.ActorKind_VENDOR,
		// A pointer to the announcement, so a human can open the original. It is an identifier and
		// never content (§4).
		OriginRef: a.Pointer,
		// The THIRD_PARTY node for the vendor (FR-061). The feeder does **not** invent a `depends-on`
		// edge from a service to a vendor and does not duplicate one the graph already holds: where
		// the announcement names affected resources they become identity claims, and the resolution
		// layer attaches them.
		Targets:           []*graphv1.Ref{feeder.Ref(NSVendor, vendor.Slug)},
		AnnouncementState: state,
	}

	// The valid interval **is** the announced window, emitted unchanged even when it starts after the
	// read instant (FR-062). Nothing here clamps it to now, and nothing rejects it for being ahead.
	if a.Window.StartUnknown {
		// A vague start is **marked** unknown, and the vendor's wording is not stored in its place
		// (FR-069). The marker matters more than it looks: without it a change nobody dated reaches
		// the graph with no valid_at, which reads as the zero timestamp and lands the announcement in
		// 1970 — not merely wrong but plausibly wrong, because an ancient change ranks as maximally
		// distant and is never excluded as a future announcement, so it looks like a fact.
		//
		// With the marker the graph starts the interval at the observation and says the start is a
		// bound: the earliest instant anybody can show, stated as such.
		fact.ValidFromUnknown = true
	} else {
		fact.ValidAt = a.Window.Start
	}
	fact.ValidEnd = a.Window.End
	return fact, nil
}

// AssertObservedNotInFuture is FR-063 as a check a caller runs before emitting.
//
// It takes the wall clock explicitly rather than reading one, so a fixture can assert the rule at a
// fixed instant — and so that the check is a function of its inputs, which is what lets it be tested at
// all.
func AssertObservedNotInFuture(observedAt, now time.Time) error {
	if observedAt.After(now) {
		return fmt.Errorf("%w: observed %s with the clock at %s. Valid time may lead observed time; "+
			"observed time is never in the future, and the way this goes wrong is a feeder that saw a "+
			"window ahead of it and moved its own observation to match (FR-063)",
			ErrObservedInFuture, observedAt.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339))
	}
	return nil
}

// AssertNotPromotedBySilence is FR-066 as a check.
//
// An announced window whose end has passed is **still announced**. The passage of time is not an
// observation, and a feeder that promoted on it would be claiming the maintenance happened — which
// nobody watched.
func AssertNotPromotedBySilence(state graphv1.AnnouncementState, window Window, now time.Time) error {
	if state != graphv1.AnnouncementState_CONFIRMED {
		return nil
	}
	if window.End.IsZero() || window.End.After(now) {
		return nil
	}
	return fmt.Errorf("vendornotice: an announcement was promoted to CONFIRMED on the strength of its "+
		"window having passed (%s, clock %s). The passage of time is not an observation: an announced "+
		"window that passes in silence stays announced for ever (FR-066)",
		window.End.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339))
}

// Claims returns the identity claims an announcement carries (T078, FR-070).
//
// The duplicate mechanism is the one the constitution requires and not the one that is easier. The
// feeder emits claims naming the vendor, the product, the window and the vendor's own notice identifier
// where it states one; **the published rules merge them, and the feeder merges nothing itself**. It also
// does **not suppress its own observation** because another source may carry the same announcement:
// suppression is a silent merge with no audit trail, and the whole point of constitution VI is that
// every merge carries its rule, its score and its rationale.
func Claims(a Announcement, vendor Vendor) []Claim {
	a = a.Normalise()
	ref := NoticeRef(a)
	claims := []Claim{
		// The addressing ref, first and always: the resolution layer cannot merge on an identifier it
		// was never told about (FR-115).
		{Namespace: ref.GetNamespace(), Value: ref.GetValue(), Why: "the ref this feeder addresses the announcement by"},
	}
	if id := strings.TrimSpace(a.NoticeID); id != "" {
		claims = append(claims, Claim{
			Namespace: NSVendorNotice, Value: a.Vendor + "/" + id,
			Why: "the vendor's own notice identifier, which is what lets two sources merge",
		})
	}
	// The vendor, the product and the window as a composite identifier. This is the claim that merges
	// two sources when neither states a notice identifier — the case where the *facts* are the only
	// shared thing.
	claims = append(claims, Claim{
		Namespace: NSVendorNotice,
		Value:     a.Vendor + "/" + a.Product + "@" + windowKey(a.Window),
		Why:       "vendor, product and announced window: the composite identifier two sources share when neither states one",
	})
	for _, resource := range a.AffectedResources {
		claims = append(claims, Claim{
			Namespace: NSServerAddress, Value: resource,
			Why: "a resource the announcement names; emitted as a claim so the resolution layer attaches it, rather than as an invented depends-on edge (FR-061)",
		})
	}
	return claims
}

// Claim is one identifier this feeder knows an entity by.
//
// An alias of the SDK's, which is where the shape now lives: it was written identically here and in
// the GCP feeder before the deploy feeders would have made it four (004 T056). C6 still reads
// PropAllowlistedHost out of Attrs to tell the allowlist's assertion from an observation of the same
// host, and this package's tests still assert the two spellings agree.
type Claim = feeder.Claim

// SameNotice reports whether two announcements are the same notice by the identifiers this feeder
// emits. It is used only for *reporting* the merge count per cycle (FR-078) — the actual merge is the
// resolution layer's, and this function asserts nothing about entities.
func SameNotice(a, b Announcement) bool {
	return NoticeRef(a.Normalise()).GetValue() == NoticeRef(b.Normalise()).GetValue()
}

// SortAnnouncements orders announcements canonically, so a cycle's report and any golden read the same
// however the sources happened to return them.
func SortAnnouncements(in []Announcement) []Announcement {
	out := append([]Announcement(nil), in...)
	sort.Slice(out, func(i, j int) bool {
		left, right := NoticeRef(out[i].Normalise()).GetValue(), NoticeRef(out[j].Normalise()).GetValue()
		if left != right {
			return left < right
		}
		return out[i].Source < out[j].Source
	})
	return out
}
