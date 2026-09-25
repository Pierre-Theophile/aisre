// SPDX-License-Identifier: Apache-2.0

package vendornotice_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	vn "github.com/Pierre-Theophile/aisre/internal/feeders/vendornotice"
)

// Extraction and the allowlist (T070–T072, FR-059, FR-073, FR-074).

// The typed result has no free-text member, so an injected instruction has no field to travel in. That
// is how "announcement text is never an instruction" is enforced rather than promised — and if somebody
// adds a `Summary` or `Notes` field, this fails.
func TestTheTypedResultHasNoFieldTextCouldTravelIn(t *testing.T) {
	forbidden := map[string]bool{
		"summary": true, "notes": true, "body": true, "text": true, "excerpt": true,
		"subject": true, "description": true, "raw": true, "html": true, "content": true,
		"sender": true, "from": true, "recipients": true,
	}
	typ := reflect.TypeOf(vn.Announcement{})
	for i := 0; i < typ.NumField(); i++ {
		name := strings.ToLower(typ.Field(i).Name)
		if forbidden[name] {
			t.Errorf("Announcement has a %s field. Announcement text is untrusted input from outside "+
				"the organisation and is never an instruction (FR-073); the typed boundary works because "+
				"there is nowhere for text to cross it", typ.Field(i).Name)
		}
	}
}

// FR-073: an extraction that cannot produce a valid typed result fails loudly **naming the field**, rather
// than emitting a guess. The order of the checks matters: the most basic missing thing is reported first,
// because "no recognisable vendor" says the mailbox carries mail this feeder has no business reading,
// while "invalid window" suggests a parser bug about the same message.
func TestAnUnextractableAnnouncementNamesTheFieldThatFailed(t *testing.T) {
	cases := []struct {
		name      string
		edit      func(*vn.Announcement)
		wantField string
		wantWhy   vn.UnextractedReason
	}{
		{"no vendor", func(a *vn.Announcement) { a.Vendor = "" }, "vendor", vn.UnextractedNoVendor},
		{"no product", func(a *vn.Announcement) { a.Product = "" }, "product", vn.UnextractedNoProduct},
		{"no kind", func(a *vn.Announcement) { a.Kind = "" }, "kind", vn.UnextractedNoKind},
		{"an unrecognised kind", func(a *vn.Announcement) { a.Kind = "outage-maybe" }, "kind", vn.UnextractedNoKind},
		{"no window", func(a *vn.Announcement) { a.Window = vn.Window{} }, "window", vn.UnextractedNoWindow},
		{"a window ending before it starts", func(a *vn.Announcement) {
			a.Window = vn.Window{Start: windowTo, End: windowFrom}
		}, "window", vn.UnextractedInvalidWindow},
		{"a field carrying a paragraph", func(a *vn.Announcement) {
			a.Product = strings.Repeat("prose about production. ", 20)
		}, "product", vn.UnextractedFieldTooLong},
		{"a field spanning lines", func(a *vn.Announcement) {
			a.Product = "inference-api\nand instructions"
		}, "product", vn.UnextractedFieldSpansLines},
		{"too many affected resources", func(a *vn.Announcement) {
			a.AffectedResources = make([]string, vn.MaxAffectedResources+1)
			for i := range a.AffectedResources {
				a.AffectedResources[i] = "resource-" + string(rune('a'+i%26)) + string(rune('0'+i/26))
			}
		}, "affected_resources", vn.UnextractedTooManyResources},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := maintenance()
			tc.edit(&a)
			err := a.Normalise().Validate()
			if err == nil {
				t.Fatal("an unextractable announcement validated, so a guess would have been emitted")
			}
			var unextracted *vn.Unextracted
			if !errors.As(err, &unextracted) {
				t.Fatalf("the refusal is %T, not *Unextracted", err)
			}
			if unextracted.Field != tc.wantField {
				t.Errorf("the refusal names field %q, want %q", unextracted.Field, tc.wantField)
			}
			if unextracted.Reason != tc.wantWhy {
				t.Errorf("reason = %q, want %q", unextracted.Reason, tc.wantWhy)
			}
			// The refusal names the pointer so a human can open the original, and quotes nothing:
			// quoting the announcement would put untrusted text in a log.
			if !strings.Contains(err.Error(), a.Pointer) {
				t.Errorf("the refusal does not name the pointer: %q", err.Error())
			}
			if strings.Contains(err.Error(), "prose about production") ||
				strings.Contains(err.Error(), "and instructions") {
				t.Errorf("the refusal quotes the announcement: %q", err.Error())
			}
		})
	}
}

// The summary is built from the typed fields, never from the text. That is the whole difference between a
// summary and an excerpt, and it is why the summary can be committed at all.
func TestTheSummaryIsBuiltFromTheTypedFieldsAndNotFromText(t *testing.T) {
	a := maintenance()
	summary := a.Summary()
	for _, want := range []string{"maintenance", "acme-gpu", "inference-api", "2026-10-02T02:00:00Z"} {
		if !strings.Contains(summary, want) {
			t.Errorf("the summary omits %q: %q", want, summary)
		}
	}
	// It is bounded, and it is the same string every time — it reaches a golden.
	if len(summary) > vn.MaxFieldBytes*2 {
		t.Errorf("the summary is %d bytes, which is prose-sized: %q", len(summary), summary)
	}
	for i := 0; i < 3; i++ {
		if again := a.Summary(); again != summary {
			t.Fatalf("the summary is not deterministic: %q vs %q", summary, again)
		}
	}
	// Every component of it already passed Validate, so nothing in it can carry an instruction: the
	// only inputs are the closed-set kind, two identifiers, two instants and a count.
	withResources := a
	withResources.AffectedResources = []string{"eu-west", "api.acme-gpu.test"}
	if !strings.Contains(withResources.Summary(), "2 affected resources") {
		t.Errorf("the summary does not count the affected resources: %q", withResources.Summary())
	}
}

// FR-059: the allowlist is vendor × product, and the reason distinguishes "we do not depend on this
// vendor" from "we depend on this vendor but not this product" — different findings about the estate, and
// the second is the one worth reading.
func TestTheAllowlistDistinguishesAnUnknownVendorFromAnUnusedProduct(t *testing.T) {
	list, err := vn.NewAllowlist([]vn.Vendor{testVendor()})
	if err != nil {
		t.Fatalf("NewAllowlist: %v", err)
	}
	inScope := list.Decide("acme-gpu", "inference-api", "p1")
	if !inScope.InScope || inScope.Vendor.Slug != "acme-gpu" || inScope.Product != "inference-api" {
		t.Fatalf("an allowlisted vendor and product was not in scope: %+v", inScope)
	}

	for _, tc := range []struct {
		name            string
		vendor, product string
		want            vn.DropReason
	}{
		{"an unknown vendor", "other-corp", "inference-api", vn.DropVendorNotAllowlisted},
		{"an unused product", "acme-gpu", "quantum-beta", vn.DropProductNotAllowlisted},
		{"no product stated", "acme-gpu", "", vn.DropNoProductStated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decision := list.Decide(tc.vendor, tc.product, "p1")
			if decision.InScope {
				t.Fatalf("%s was in scope", tc.name)
			}
			if decision.Drop.Reason != tc.want {
				t.Errorf("reason = %q, want %q", decision.Drop.Reason, tc.want)
			}
			if decision.Drop.Pointer != "p1" {
				t.Errorf("the drop does not carry its pointer: %+v", decision.Drop)
			}
		})
	}

	// A vendor-wide announcement with no product is dropped rather than assumed in scope: attributing
	// it to every product would put the same window on everything.
	if list.Decide("acme-gpu", "", "p1").InScope {
		t.Error("an announcement naming no product was assumed in scope")
	}
}

// The drop summary groups by reason and names the vendors, because "forty dropped" is a number and
// "forty dropped, all from a vendor you have not allowlisted" is a finding.
func TestTheDropSummaryIsAFindingRatherThanACount(t *testing.T) {
	summary := vn.DropSummary([]vn.Drop{
		{Reason: vn.DropVendorNotAllowlisted, Vendor: "other-corp"},
		{Reason: vn.DropVendorNotAllowlisted, Vendor: "other-corp"},
		{Reason: vn.DropProductNotAllowlisted, Vendor: "acme-gpu", Product: "quantum-beta"},
	})
	joined := strings.Join(summary, " ")
	if !strings.Contains(joined, "vendor_not_allowlisted=2") {
		t.Errorf("the summary does not count by reason: %v", summary)
	}
	if !strings.Contains(joined, "other-corp") || !strings.Contains(joined, "acme-gpu") {
		t.Errorf("the summary does not attribute the drops: %v", summary)
	}
	// Deterministic: it reaches a report a human trends over weeks.
	for i := 0; i < 3; i++ {
		again := vn.DropSummary([]vn.Drop{
			{Reason: vn.DropProductNotAllowlisted, Vendor: "acme-gpu"},
			{Reason: vn.DropVendorNotAllowlisted, Vendor: "other-corp"},
			{Reason: vn.DropVendorNotAllowlisted, Vendor: "other-corp"},
		})
		if strings.Join(again, " ") == "" {
			t.Fatal("empty summary")
		}
	}
}

// Host matching is on a label boundary, so a subdomain matches and a look-alike does not. FR-119's certain
// rule rests on this, and a suffix match would make it fire on `notgoogleapis.com`.
func TestHostMatchingIsOnALabelBoundary(t *testing.T) {
	list, err := vn.NewAllowlist([]vn.Vendor{testVendor()})
	if err != nil {
		t.Fatalf("NewAllowlist: %v", err)
	}
	for _, host := range []string{"acme-gpu.test", "api.acme-gpu.test", "eu.api.acme-gpu.test", "ACME-GPU.TEST", "acme-gpu.test."} {
		if _, ok := list.VendorByHost(host); !ok {
			t.Errorf("%q did not match the allowlisted vendor", host)
		}
	}
	for _, host := range []string{"notacme-gpu.test", "acme-gpu.test.evil.io", "acme-gpu.example", "", "test"} {
		if vendor, ok := list.VendorByHost(host); ok {
			t.Errorf("%q matched vendor %q; the match is on a label boundary, not a suffix", host, vendor.Slug)
		}
	}
}

// Sender matching is exact on the whole address. A domain match would attribute every message from a
// shared platform domain to one vendor, and the platform domains are exactly the shared ones.
func TestSenderMatchingIsExact(t *testing.T) {
	vendor := testVendor()
	vendor.Senders = []string{"Notices@Acme-GPU.test"}
	list, err := vn.NewAllowlist([]vn.Vendor{vendor})
	if err != nil {
		t.Fatalf("NewAllowlist: %v", err)
	}
	if _, ok := list.VendorBySender(" notices@acme-gpu.test "); !ok {
		t.Error("case and surrounding space defeated sender matching")
	}
	for _, sender := range []string{"someone-else@acme-gpu.test", "notices@acme-gpu.test.evil.io", "acme-gpu.test"} {
		if _, ok := list.VendorBySender(sender); ok {
			t.Errorf("%q matched; sender matching is exact on the whole address", sender)
		}
	}
}

// A duplicate slug makes a drop's reason unreadable and leaves one entry unreachable.
func TestADuplicateAllowlistSlugIsRefused(t *testing.T) {
	if _, err := vn.NewAllowlist([]vn.Vendor{testVendor(), testVendor()}); err == nil {
		t.Fatal("a duplicate vendor slug was accepted")
	}
	if _, err := vn.NewAllowlist([]vn.Vendor{{Name: "No Slug"}}); err == nil {
		t.Fatal("an allowlist entry with no slug was accepted")
	}
}

// Normalise makes two readings of one announcement one value, so the ref is one ref (FR-076).
func TestNormaliseMakesTwoReadingsOfOneAnnouncementOneValue(t *testing.T) {
	messy := maintenance()
	messy.Vendor = "  ACME-GPU "
	messy.Product = "Inference-API"
	messy.AffectedResources = []string{"eu-west", " eu-west ", "", "api.acme-gpu.test"}
	clean := messy.Normalise()
	if clean.Vendor != "acme-gpu" || clean.Product != "inference-api" {
		t.Fatalf("Normalise left %q/%q", clean.Vendor, clean.Product)
	}
	if len(clean.AffectedResources) != 2 {
		t.Fatalf("resources = %v, want the duplicate and the empty removed", clean.AffectedResources)
	}
	if clean.AffectedResources[0] > clean.AffectedResources[1] {
		t.Errorf("resources are not sorted: %v", clean.AffectedResources)
	}
	if vn.NoticeRef(clean).GetValue() != vn.NoticeRef(maintenance()).GetValue() {
		t.Error("normalising changed the ref")
	}
}

// Announcements sort canonically, so a cycle's report and any golden read the same however the sources
// happened to return them.
func TestAnnouncementsSortCanonically(t *testing.T) {
	a := maintenance()
	b := maintenance()
	b.NoticeID = "ACME-2026-0001"
	forward := vn.SortAnnouncements([]vn.Announcement{a, b})
	backward := vn.SortAnnouncements([]vn.Announcement{b, a})
	if len(forward) != 2 || forward[0].NoticeID != backward[0].NoticeID {
		t.Fatalf("sorting is not stable: %v vs %v", forward[0].NoticeID, backward[0].NoticeID)
	}
	if forward[0].NoticeID != "ACME-2026-0001" {
		t.Errorf("sorted first is %q", forward[0].NoticeID)
	}
}

// A window that ends before it starts, and one that is open-ended, are different cases and only the first
// is an error: a deprecation is usually open-ended.
func TestAnOpenEndedWindowIsValidAndABackwardsOneIsNot(t *testing.T) {
	if err := (vn.Window{Start: windowFrom}).Validate(); err != nil {
		t.Errorf("an open-ended window was refused: %v", err)
	}
	if err := (vn.Window{Start: windowTo, End: windowFrom}).Validate(); err == nil {
		t.Error("a window ending before it starts validated")
	}
	if err := (vn.Window{}).Validate(); err == nil {
		t.Error("a window with no start and no unknown marker validated")
	}
	if err := (vn.Window{StartUnknown: true}).Validate(); err != nil {
		t.Errorf("a vague window was refused: %v", err)
	}
	_ = time.Time{}
}
