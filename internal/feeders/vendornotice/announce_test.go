// SPDX-License-Identifier: Apache-2.0

package vendornotice_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	vn "github.com/Pierre-Theophile/aisre/internal/feeders/vendornotice"
)

// The announced fact (T073–T076, T078, FR-062–FR-070).

var (
	// read is when the notice arrived; announced is the window it announced. Seventeen September for
	// the second of October: valid time leads observed time by a fortnight.
	readAt     = time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	windowFrom = time.Date(2026, 10, 2, 2, 0, 0, 0, time.UTC)
	windowTo   = time.Date(2026, 10, 2, 4, 0, 0, 0, time.UTC)
	clockNow   = time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
)

func testVendor() vn.Vendor {
	return vn.Vendor{
		Slug: "acme-gpu", Name: "Acme GPU",
		Products: []string{"inference-api", "training"},
		Hosts:    []string{"api.acme-gpu.test", "acme-gpu.test"},
	}
}

func maintenance() vn.Announcement {
	return vn.Announcement{
		Vendor:   "acme-gpu",
		Product:  "inference-api",
		Kind:     vn.KindMaintenance,
		Window:   vn.Window{Start: windowFrom, End: windowTo},
		NoticeID: "ACME-2026-1002",
		Pointer:  "mailbox:msg-0001",
		Source:   vn.SourceMailbox,
	}
}

// FR-062: the valid interval **is** the announced window, emitted unchanged even though it begins a
// fortnight after the notice was read. Nothing clamps it to now and nothing rejects it for being ahead.
func TestTheValidIntervalIsTheAnnouncedWindowEvenThoughItStartsInTheFuture(t *testing.T) {
	change, err := vn.Announced(maintenance(), testVendor(), readAt)
	if err != nil {
		t.Fatalf("Announced: %v", err)
	}
	if !change.ValidAt.Equal(windowFrom) {
		t.Fatalf("valid start = %s, want the announced %s", change.ValidAt, windowFrom)
	}
	if !change.ValidEnd.Equal(windowTo) {
		t.Fatalf("valid end = %s, want the announced %s", change.ValidEnd, windowTo)
	}
	if !change.ValidAt.After(readAt) {
		t.Fatal("the fixture no longer announces a future window, so it asserts nothing")
	}
	if change.Kind != graphv1.ChangeKind_CLOUD_MAINTENANCE {
		t.Errorf("kind = %s, want CLOUD_MAINTENANCE", change.Kind)
	}
	if change.ActorKind != graphv1.ActorKind_VENDOR {
		t.Errorf("actor kind = %s, want VENDOR", change.ActorKind)
	}
	if change.AnnouncementState != graphv1.AnnouncementState_ANNOUNCED {
		t.Errorf("state = %s, want ANNOUNCED, which is the default and is never skipped", change.AnnouncementState)
	}
	// The vendor's THIRD_PARTY node is the target; no depends-on edge is invented (FR-061).
	if len(change.Targets) != 1 || change.Targets[0].GetNamespace() != vn.NSVendor {
		t.Errorf("targets = %v, want just the vendor's third-party node", change.Targets)
	}
}

// FR-063: observed time is never moved forward to match an announced window. The asymmetry is
// one-directional, and the way it goes wrong is a feeder that saw a window ahead of it and helpfully
// stamped its own observation at the window's start.
func TestObservedTimeIsNeverMovedForwardToMatchTheWindow(t *testing.T) {
	if err := vn.AssertObservedNotInFuture(readAt, clockNow); err != nil {
		t.Fatalf("an observation in the past was refused: %v", err)
	}
	// The failure mode: the observation stamped at the window's start.
	err := vn.AssertObservedNotInFuture(windowFrom, clockNow)
	if err == nil {
		t.Fatal("an observation stamped at the announced window's start was accepted; valid time may " +
			"lead observed time, observed time is never in the future (FR-063)")
	}
	if !errors.Is(err, vn.ErrObservedInFuture) {
		t.Fatalf("the refusal is %v, want ErrObservedInFuture", err)
	}
	// The boundary: an observation at the clock is not in the future.
	if err := vn.AssertObservedNotInFuture(clockNow, clockNow); err != nil {
		t.Errorf("an observation at the current instant was refused: %v", err)
	}
}

// FR-069: a vague window gets a valid start marked unknown, and the vendor's own wording is never stored
// in place of a timestamp. This is where "no free text" and "do not guess" agree rather than conflict.
func TestAVagueWindowMarksTheStartUnknownAndStoresNoWording(t *testing.T) {
	vague := maintenance()
	vague.Kind = vn.KindDeprecation
	vague.Window = vn.Window{StartUnknown: true}
	vague.NoticeID = "ACME-DEP-1"

	change, err := vn.Announced(vague, testVendor(), readAt)
	if err != nil {
		t.Fatalf("Announced: %v", err)
	}
	if !change.ValidAt.IsZero() {
		t.Fatalf("a vague window produced the valid start %s; there was no instant to produce", change.ValidAt)
	}
	if change.Kind != graphv1.ChangeKind_DEPRECATION {
		t.Errorf("kind = %s, want DEPRECATION", change.Kind)
	}
	// The summary says the start is unstated in this feeder's own words, and carries none of the
	// vendor's prose.
	if !strings.Contains(change.Summary, "unstated") {
		t.Errorf("the summary does not say the instant is unstated: %q", change.Summary)
	}
	for _, wording := range []string{"coming weeks", "in a future release", "soon"} {
		if strings.Contains(strings.ToLower(change.Summary), wording) {
			t.Errorf("the summary carries the vendor's wording %q in place of a timestamp (FR-069)", wording)
		}
	}
	// A window that claims both a start and an unknown marker is one of the two being a guess.
	both := vn.Window{Start: windowFrom, StartUnknown: true}
	if err := both.Validate(); err == nil {
		t.Error("a window with both a start and an unknown marker validated")
	}
}

// FR-066: an announced window that passes in silence stays announced for ever. The passage of time is not
// an observation, and a feeder that promoted on it would be claiming the maintenance happened — which
// nobody watched.
func TestAWindowThatPassesInSilenceIsNeverPromoted(t *testing.T) {
	passed := vn.Window{
		Start: time.Date(2026, 9, 1, 2, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 9, 1, 4, 0, 0, 0, time.UTC),
	}
	// Still announced after its window passed: no error, nothing to promote.
	if err := vn.AssertNotPromotedBySilence(graphv1.AnnouncementState_ANNOUNCED, passed, clockNow); err != nil {
		t.Fatalf("an announcement left announced after its window passed was refused: %v", err)
	}
	// Promoted to confirmed on nothing but the clock: refused.
	err := vn.AssertNotPromotedBySilence(graphv1.AnnouncementState_CONFIRMED, passed, clockNow)
	if err == nil {
		t.Fatal("an announcement was promoted to CONFIRMED on the strength of its window having " +
			"passed (FR-066)")
	}
	if !strings.Contains(err.Error(), "FR-066") {
		t.Errorf("the refusal does not name the rule: %q", err.Error())
	}
	// A confirmation of a window that has *not* passed is a real observation and is allowed: somebody
	// watched it happen.
	if err := vn.AssertNotPromotedBySilence(graphv1.AnnouncementState_CONFIRMED,
		vn.Window{Start: windowFrom, End: windowTo}, clockNow); err != nil {
		t.Errorf("a confirmation of a window still ahead was refused: %v", err)
	}
}

// FR-067, FR-068: a cancellation and a reschedule are corrections of the same change — the same ref, the
// valid interval never rewritten, and no second change node. So "what did we believe on 20 September
// about 2 October?" stays answerable.
func TestACancellationIsACorrectionOfTheSameChangeAndNotARetraction(t *testing.T) {
	a := maintenance()
	announced, err := vn.Announced(a, testVendor(), readAt)
	if err != nil {
		t.Fatalf("Announced: %v", err)
	}
	cancelledAt := time.Date(2026, 9, 25, 9, 0, 0, 0, time.UTC)
	cancelled, err := vn.Cancelled(a, testVendor(), cancelledAt)
	if err != nil {
		t.Fatalf("Cancelled: %v", err)
	}

	if cancelled.Ref.GetValue() != announced.Ref.GetValue() {
		t.Fatalf("the cancellation is a different change: %q vs %q. It is a correction of the same one, "+
			"or the prior belief becomes unrecoverable (FR-067)",
			cancelled.Ref.GetValue(), announced.Ref.GetValue())
	}
	// The valid interval is never rewritten: the window was announced, and then withdrawn. Those are
	// different facts and a retraction would claim the wrong one.
	if !cancelled.ValidAt.Equal(announced.ValidAt) || !cancelled.ValidEnd.Equal(announced.ValidEnd) {
		t.Fatalf("the cancellation rewrote the valid interval: [%s,%s) vs [%s,%s)",
			cancelled.ValidAt, cancelled.ValidEnd, announced.ValidAt, announced.ValidEnd)
	}
	if cancelled.AnnouncementState != graphv1.AnnouncementState_CANCELLED {
		t.Errorf("state = %s, want CANCELLED", cancelled.AnnouncementState)
	}
	if !cancelled.SourceObservedAt.Equal(cancelledAt) {
		t.Errorf("the correction is observed at %s, want %s", cancelled.SourceObservedAt, cancelledAt)
	}
}

// A reschedule keeps the original window on the superseded observation, so both windows stay recoverable
// by an observed-time query: "we believed the 2nd, then we believed the 9th" is two beliefs about one
// notice.
func TestARescheduleKeepsBothWindowsRecoverableOnOneChange(t *testing.T) {
	original := maintenance()
	superseded, err := vn.Superseded(original, testVendor(), time.Date(2026, 9, 25, 9, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("Superseded: %v", err)
	}
	if !superseded.ValidAt.Equal(windowFrom) {
		t.Fatalf("the superseded observation carries %s, want the original window start %s; the new "+
			"window belongs to the ANNOUNCED observation that follows", superseded.ValidAt, windowFrom)
	}

	// The new ANNOUNCED observation: the same notice identifier, a different window, one ref.
	moved := original
	moved.Window = vn.Window{
		Start: time.Date(2026, 10, 9, 2, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 10, 9, 4, 0, 0, 0, time.UTC),
	}
	rescheduled, err := vn.Announced(moved, testVendor(), time.Date(2026, 9, 25, 9, 0, 1, 0, time.UTC))
	if err != nil {
		t.Fatalf("Announced: %v", err)
	}
	if rescheduled.Ref.GetValue() != superseded.Ref.GetValue() {
		t.Fatalf("the reschedule created a second change node: %q vs %q (FR-068)",
			rescheduled.Ref.GetValue(), superseded.Ref.GetValue())
	}
	if rescheduled.ValidAt.Equal(superseded.ValidAt) {
		t.Fatal("the rescheduled observation carries the original window, so the move is invisible")
	}
}

// FR-076: the ref is deterministic, so re-reading is a no-op and one announcement from two sources is one
// change. The derived form excludes the pointer, because a message id differs between two mailboxes
// carrying one notice.
func TestTheRefIsDeterministicAndOneAnnouncementFromTwoSourcesIsOneChange(t *testing.T) {
	// With a vendor-stated identifier: the identifier is the ref.
	stated := maintenance()
	if got := vn.NoticeRef(stated).GetValue(); got != "acme-gpu/ACME-2026-1002" {
		t.Fatalf("ref = %q, want the vendor's own identifier", got)
	}
	// Read again from a different source with a different pointer: one ref.
	fromPage := stated
	fromPage.Source = vn.SourceStatusPage
	fromPage.Pointer = "statuspage:entry-77"
	if vn.NoticeRef(fromPage).GetValue() != vn.NoticeRef(stated).GetValue() {
		t.Fatal("the same notice from two sources produced two refs; the failure this guards against " +
			"is two adjacent rows for one window in front of somebody being paged (FR-070)")
	}
	if !vn.SameNotice(stated, fromPage) {
		t.Error("SameNotice disagrees with the refs")
	}

	// Without a stated identifier: derived from the product, the kind and the window, and still one
	// ref across two sources with two pointers.
	noID := stated
	noID.NoticeID = ""
	other := noID
	other.Source = vn.SourceChangelog
	other.Pointer = "changelog:item-9"
	if vn.NoticeRef(noID).GetValue() != vn.NoticeRef(other).GetValue() {
		t.Fatalf("the derived ref differs across sources: %q vs %q. It must exclude the pointer, "+
			"because a message id differs between two mailboxes carrying one notice",
			vn.NoticeRef(noID).GetValue(), vn.NoticeRef(other).GetValue())
	}
	if !strings.HasPrefix(vn.NoticeRef(noID).GetValue(), "acme-gpu/derived-") {
		t.Errorf("the derived ref %q is not marked as derived", vn.NoticeRef(noID).GetValue())
	}

	// A different product is a different notice, even with everything else identical. This is the
	// length-prefixed-hash property: two announcements must not collide into one change.
	otherProduct := noID
	otherProduct.Product = "training"
	if vn.NoticeRef(otherProduct).GetValue() == vn.NoticeRef(noID).GetValue() {
		t.Fatal("two products share a derived ref, so two announcements would merge into one change")
	}
	otherWindow := noID
	otherWindow.Window.Start = windowFrom.Add(time.Hour)
	if vn.NoticeRef(otherWindow).GetValue() == vn.NoticeRef(noID).GetValue() {
		t.Fatal("two windows share a derived ref")
	}
}

// The taxonomy names these kinds, so the CHANGE_KIND_OTHER fallback is dead. A fixture asserting an
// OTHER-kind deprecation would be asserting a regression.
func TestTheChangeKindOtherFallbackIsDead(t *testing.T) {
	for kind, want := range map[vn.AnnouncementKind]graphv1.ChangeKind{
		vn.KindMaintenance: graphv1.ChangeKind_CLOUD_MAINTENANCE,
		vn.KindDeprecation: graphv1.ChangeKind_DEPRECATION,
		vn.KindIncident:    graphv1.ChangeKind_VENDOR_INCIDENT,
	} {
		got, err := vn.ChangeKindFor(kind)
		if err != nil {
			t.Fatalf("ChangeKindFor(%s): %v", kind, err)
		}
		if got != want {
			t.Errorf("ChangeKindFor(%s) = %s, want %s", kind, got, want)
		}
		if got == graphv1.ChangeKind_CHANGE_KIND_OTHER {
			t.Errorf("%s fell back to CHANGE_KIND_OTHER; the taxonomy named it on 2026-09-18", kind)
		}
	}
	if _, err := vn.ChangeKindFor(vn.AnnouncementKind("something-new")); err == nil {
		t.Fatal("an unknown kind was mapped rather than refused")
	}
}

// FR-070: the feeder emits claims and merges nothing. The claims are what let two sources be merged by a
// published rule that carries its rationale, instead of by a silent suppression with no audit trail.
func TestTheFeederEmitsClaimsAndMergesNothing(t *testing.T) {
	claims := vn.Claims(maintenance(), testVendor())
	byNamespace := map[string][]string{}
	for _, claim := range claims {
		byNamespace[claim.Namespace] = append(byNamespace[claim.Namespace], claim.Value)
		if claim.Why == "" {
			t.Errorf("claim %s=%s carries no reason", claim.Namespace, claim.Value)
		}
	}
	// The addressing ref is claimed (FR-115).
	if !contains(byNamespace[vn.NSVendorNotice], "acme-gpu/ACME-2026-1002") {
		t.Error("the addressing ref was not claimed")
	}
	// The composite identifier is claimed, which is what merges two sources when neither states a
	// notice identifier.
	var composite bool
	for _, value := range byNamespace[vn.NSVendorNotice] {
		if strings.Contains(value, "inference-api@") {
			composite = true
		}
	}
	if !composite {
		t.Errorf("no vendor+product+window claim was emitted: %v", byNamespace[vn.NSVendorNotice])
	}

	// Affected resources become claims, not invented depends-on edges (FR-061).
	withResources := maintenance()
	withResources.AffectedResources = []string{"api.acme-gpu.test", "eu-west"}
	var addresses []string
	for _, claim := range vn.Claims(withResources, testVendor()) {
		if claim.Namespace == vn.NSServerAddress {
			addresses = append(addresses, claim.Value)
		}
	}
	if len(addresses) != 2 {
		t.Errorf("affected resources produced %d claims, want 2: %v", len(addresses), addresses)
	}
}

// The announcement must agree with the allowlisted vendor: the allowlist decides vendor identity, not the
// announcement, because configuration is the authority (FR-059).
func TestTheAllowlistDecidesVendorIdentityRatherThanTheAnnouncement(t *testing.T) {
	impostor := maintenance()
	impostor.Vendor = "someone-else"
	if _, err := vn.Announced(impostor, testVendor(), readAt); err == nil {
		t.Fatal("an announcement naming a different vendor than the allowlisted one was accepted")
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
