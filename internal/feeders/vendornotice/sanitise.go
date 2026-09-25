// SPDX-License-Identifier: Apache-2.0

package vendornotice

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/sanitise"
)

// Notice sanitisation (T042, FR-071, FR-072, FR-137, contract §2.1 and §5 call site 1).
//
// A notice is the one input in this feature that is untrusted prose written by somebody outside the
// organisation, and it is the input whose body must never be stored — not to the graph, not to disk,
// not to a log, not to any artifact, including on a failed or aborted run (FR-071).
//
// "Including on a failed run" is what makes this a type rather than a step. The obvious shape —
// fetch the notice, extract typed fields, drop the body — leaves a struct holding the body in memory
// across every error path, and the first `%+v` in an error message writes it to a log. So the body
// never enters a struct at all: Extract takes it as an argument, reads what it needs, and returns a
// Notice that has no field for it. There is nowhere for it to be stored, which is a stronger
// statement than nobody storing it.

// Notice is what survives sanitisation: typed fields, a pointer, and no prose.
//
// It deliberately has no Body, no Subject and no Sender field. A reviewer looking for where the body
// went will not find a field that is left empty — they will find that there is no field, which is
// the difference between a rule and a convention.
type Notice struct {
	// NoticeID is the vendor's own identifier, pseudonymised (§2.3).
	NoticeID string
	// Vendor and Product come from the configured allowlist, so they are the organisation's own
	// configuration and are recorded verbatim (§2.2).
	Vendor  string
	Product string
	// Kind is the change kind the notice announces: a published closed enumeration (§2.2).
	Kind string
	// ValidFrom and ValidTo are the announced window. An announced fact's valid time begins after
	// its observed time, which is the whole reason this feeder exists; the instants are recorded as
	// they are (§4).
	ValidFrom time.Time
	ValidTo   time.Time
	// ObservedAt is when the notice arrived.
	ObservedAt time.Time
	// Subjects are the pseudonymised infrastructure identifiers the notice names.
	Subjects []string
	// SubjectsFound is how many identifiers the extractor recognised, which is not the same as how
	// many it kept: a notice naming three services and one person's mailbox keeps three.
	SubjectsFound int
}

// Extractor turns a notice into a Notice. It holds the sanitiser, and it is the only thing in this
// package that does, so the set of places a body could be written is the set of methods here.
type Extractor struct {
	sanitiser *sanitise.Sanitiser
}

// NewExtractor returns an Extractor over s. A nil sanitiser is refused: see NewSanitisedRecorder for
// why this is a constructor check rather than a call-site one.
func NewExtractor(s *sanitise.Sanitiser) (*Extractor, error) {
	if s == nil {
		return nil, fmt.Errorf("vendornotice: an extractor with no sanitiser; a notice body must " +
			"never be written to disk, a log or any artifact, including on a failed or aborted run " +
			"(FR-071, FR-137)")
	}
	return &Extractor{sanitiser: s}, nil
}

// ExtractedFields is the typed result of reading a notice body: what the extractor recognised, by
// field path, ready for the disposition table. It carries no free text, so a caller that logs it
// logs nothing it should not.
type ExtractedFields struct {
	// Fields are flattened paths from the §2 table: `notice.vendor_notice_id`, `vendor.slug`,
	// `change.kind` and so on.
	Fields map[string]string
	// Subjects are the raw infrastructure identifiers named in the notice, before pseudonymisation.
	Subjects []string
}

// Extract reads body, keeps what the table allows, and returns a Notice. The body is a parameter and
// never a field: it goes out of scope when this function returns, and there is no struct anywhere in
// this package that could carry it into an error path.
//
// `parse` is the provider-family parser — a status page and a mailbox read different shapes — and it
// is a function argument rather than an interface method so that it, too, has no place to keep the
// body.
func (e *Extractor) Extract(ctx context.Context, where string, body string, parse func(context.Context, string) (ExtractedFields, error)) (Notice, error) {
	extracted, err := parse(ctx, body)
	if err != nil {
		// The parse error must not carry the body. A provider parser that embedded the input in its
		// error would defeat every rule above, so the error is replaced rather than wrapped, and the
		// original's text is not quoted.
		return Notice{}, fmt.Errorf("vendornotice: parsing a notice from %s failed (%T); the error "+
			"text is not carried, because a parse error that quotes its input is a body in a log "+
			"(FR-071)", where, err)
	}

	clean, err := e.sanitiser.Fields(where, extracted.Fields)
	if err != nil {
		return Notice{}, fmt.Errorf("vendornotice: a notice from %s was dropped rather than "+
			"recorded: %w", where, err)
	}

	notice := Notice{
		NoticeID:      clean["notice.vendor_notice_id"],
		Vendor:        clean["vendor.slug"],
		Product:       clean["vendor.product"],
		Kind:          clean["change.kind"],
		SubjectsFound: len(extracted.Subjects),
	}
	for _, subject := range extracted.Subjects {
		token, keep, err := e.sanitiser.Field(where, "service_name", subject)
		if err != nil {
			return Notice{}, err
		}
		if keep {
			notice.Subjects = append(notice.Subjects, token)
		}
	}
	return notice, nil
}

// AssertNoBody refuses an artifact that carries notice prose. It is the assertion for this call
// site, and it is a distinct check from AssertArtifact because what it looks for is different: a
// body has no people identifier in it and no canary, and it is still the thing that must not be
// there.
//
// The test is length and shape rather than content, because content is exactly what cannot be
// bounded. A notice field is an identifier, an enum or an instant; none of those is a paragraph.
func AssertNoBody(where string, fields map[string]string) error {
	for path, value := range fields {
		if len(value) > MaxTypedFieldBytes {
			return fmt.Errorf("vendornotice: %s in %s is %d bytes; a typed notice field is an "+
				"identifier, an enum or an instant, and none of those is a paragraph — this is a "+
				"body that reached the typed fields (FR-071)", path, where, len(value))
		}
		if strings.Count(value, "\n") > 0 {
			return fmt.Errorf("vendornotice: %s in %s spans lines; a typed notice field does not "+
				"(FR-071)", path, where)
		}
	}
	return nil
}

// MaxTypedFieldBytes is the longest a typed notice field may be. It is generous for an identifier
// and far too short for prose, which is the only property it needs.
const MaxTypedFieldBytes = 256
