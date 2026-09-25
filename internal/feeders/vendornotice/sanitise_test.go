// SPDX-License-Identifier: Apache-2.0

package vendornotice_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/feeders/vendornotice"
	"github.com/Pierre-Theophile/aisre/internal/sanitise"
)

// The notice boundary's own tests (T042, FR-071, FR-072).

func noticeSanitiser(t *testing.T) *sanitise.Sanitiser {
	t.Helper()
	material := make([]byte, sanitise.KeyBytes)
	for i := range material {
		material[i] = byte(i * 11)
	}
	key, err := sanitise.NewKey(material)
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}
	s, err := sanitise.New(sanitise.ContractPolicy(), key)
	if err != nil {
		t.Fatalf("sanitise.New: %v", err)
	}
	return s
}

// The strongest statement this file makes is structural, and a test can assert it: Notice has no
// field a body could be stored in. A reviewer looking for where the body went finds that there is no
// field, not a field left empty — and if somebody adds one, this fails.
func TestNoticeHasNoFieldABodyCouldBeStoredIn(t *testing.T) {
	forbidden := map[string]bool{
		"body": true, "bodyhtml": true, "subject": true, "snippet": true,
		"sender": true, "from": true, "to": true, "cc": true, "bcc": true,
		"recipients": true, "text": true, "html": true, "quotedthread": true,
		"description": true, "raw": true, "content": true,
	}
	typ := reflect.TypeOf(vendornotice.Notice{})
	for i := 0; i < typ.NumField(); i++ {
		name := strings.ToLower(typ.Field(i).Name)
		if forbidden[name] {
			t.Errorf("Notice has a %s field; a notice body is never stored, and a field for it is "+
				"where the first %%+v puts it in a log (FR-071)", typ.Field(i).Name)
		}
		if sanitise.IsPeopleField(typ.Field(i).Name) {
			t.Errorf("Notice.%s names a person (FR-072)", typ.Field(i).Name)
		}
	}
}

func TestAnExtractorCannotBeBuiltWithoutASanitiser(t *testing.T) {
	if _, err := vendornotice.NewExtractor(nil); err == nil {
		t.Fatal("an extractor with no sanitiser was constructed (FR-071, FR-137)")
	}
	if _, err := vendornotice.NewExtractor(noticeSanitiser(t)); err != nil {
		t.Fatalf("a valid extractor was refused: %v", err)
	}
}

// The ordinary path: typed fields survive, the sender does not, and the subjects are pseudonymised so
// the notice joins to the graph.
func TestANoticeKeepsItsTypedFieldsAndLosesItsSenderAndBody(t *testing.T) {
	extractor, err := vendornotice.NewExtractor(noticeSanitiser(t))
	if err != nil {
		t.Fatalf("NewExtractor: %v", err)
	}
	parse := func(context.Context, string) (vendornotice.ExtractedFields, error) {
		return vendornotice.ExtractedFields{
			Fields: map[string]string{
				"notice.vendor_notice_id": "CLOUD-2026-09-0042",
				"vendor.slug":             "gcp",
				"vendor.product":          "cloudsql",
				"change.kind":             "maintenance",
				"message.sender":          "cloud-noreply@google.com",
				"message.body":            "We will perform maintenance on your instance...",
				"message.subject":         "Scheduled maintenance for your Cloud SQL instance",
			},
			Subjects: []string{"orders-primary", "storefront"},
		}, nil
	}
	notice, err := extractor.Extract(context.Background(), "the configured mailbox", "the raw body", parse)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if notice.Vendor != "gcp" || notice.Product != "cloudsql" || notice.Kind != "maintenance" {
		t.Fatalf("the typed fields did not survive: %+v", notice)
	}
	if !strings.HasPrefix(notice.NoticeID, "px_nid_") {
		t.Fatalf("the notice id is %q, not a pseudonym", notice.NoticeID)
	}
	if notice.NoticeID == "CLOUD-2026-09-0042" {
		t.Fatal("the vendor's notice id was recorded verbatim")
	}
	if len(notice.Subjects) != 2 || notice.SubjectsFound != 2 {
		t.Fatalf("the notice named 2 subjects and kept %d (found %d)", len(notice.Subjects), notice.SubjectsFound)
	}
	for _, subject := range notice.Subjects {
		if !strings.HasPrefix(subject, "px_svc_") {
			t.Fatalf("a subject is %q, not a service pseudonym", subject)
		}
	}
	// Nothing from the body or the sender survives anywhere in the struct.
	rendered := fmt.Sprintf("%+v", notice)
	for _, leaked := range []string{"cloud-noreply", "google.com", "maintenance on your instance", "Scheduled maintenance"} {
		if strings.Contains(rendered, leaked) {
			t.Fatalf("the notice carries %q: %s", leaked, rendered)
		}
	}
}

// A parse error must not carry the body it failed on. A provider parser that embedded its input in
// its error would defeat every rule above it, and the place that happens is a log line.
func TestAParseErrorDoesNotCarryTheBodyItFailedOn(t *testing.T) {
	extractor, err := vendornotice.NewExtractor(noticeSanitiser(t))
	if err != nil {
		t.Fatalf("NewExtractor: %v", err)
	}
	const body = "Dear jane.doe@acme-corp.io, your instance orders-primary will be migrated."
	parse := func(context.Context, string) (vendornotice.ExtractedFields, error) {
		return vendornotice.ExtractedFields{}, fmt.Errorf("unparseable notice: %q", body)
	}
	_, err = extractor.Extract(context.Background(), "the configured mailbox", body, parse)
	if err == nil {
		t.Fatal("an unparseable notice was extracted")
	}
	for _, leaked := range []string{"jane.doe", "acme-corp", "orders-primary", "will be migrated"} {
		if strings.Contains(err.Error(), leaked) {
			t.Fatalf("the parse error carries %q from the body: %q", leaked, err.Error())
		}
	}
}

// An unassigned field drops the notice rather than recording part of it.
func TestANoticeWithAnUnassignedFieldIsDropped(t *testing.T) {
	extractor, err := vendornotice.NewExtractor(noticeSanitiser(t))
	if err != nil {
		t.Fatalf("NewExtractor: %v", err)
	}
	parse := func(context.Context, string) (vendornotice.ExtractedFields, error) {
		return vendornotice.ExtractedFields{Fields: map[string]string{
			"vendor.slug":             "gcp",
			"notice.newFieldGCPAdded": "something nobody thought about",
		}}, nil
	}
	_, err = extractor.Extract(context.Background(), "the configured mailbox", "a body", parse)
	var unassigned *sanitise.UnassignedError
	if !errors.As(err, &unassigned) {
		t.Fatalf("the refusal is %T, not *UnassignedError: %v", err, err)
	}
}

// A body that reached the typed fields is caught by shape rather than by content, because content is
// exactly what cannot be bounded.
func TestABodyThatReachedTheTypedFieldsIsCaughtByShape(t *testing.T) {
	long := strings.Repeat("this is prose about production. ", 20)
	if err := vendornotice.AssertNoBody("a mailbox", map[string]string{"notice.summary": long}); err == nil {
		t.Fatal("a paragraph in a typed notice field was accepted (FR-071)")
	}
	if err := vendornotice.AssertNoBody("a mailbox", map[string]string{"notice.summary": "one\ntwo"}); err == nil {
		t.Fatal("a multi-line value in a typed notice field was accepted")
	}
	if err := vendornotice.AssertNoBody("a mailbox", map[string]string{
		"notice.vendor_notice_id": "px_nid_abcdefghijkl",
		"change.kind":             "maintenance",
	}); err != nil {
		t.Fatalf("ordinary typed fields were refused: %v", err)
	}
}
