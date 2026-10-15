// SPDX-License-Identifier: Apache-2.0

package knowledge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// The published limits (contracts §4, data-model §Section). A source splits a longer section into
// parts before sending it; a store refuses what exceeds them rather than truncating, because a
// silently truncated section is one whose tail can never be matched.
const (
	// MaxSectionChars is the longest section text, in characters.
	MaxSectionChars = 1200
	// MaxLabelChars is the longest section label, in characters.
	MaxLabelChars = 200
	// MaxSections is the most sections one document may carry.
	MaxSections = 500
)

// Origin says where a section's text came from.
type Origin string

// The published origins.
const (
	// OriginProperty is a named property of the source document.
	OriginProperty Origin = "property"
	// OriginBody is the document's body, under one heading.
	OriginBody Origin = "body"
	// OriginSummary is synthesised from the title and summary registered with the document.
	OriginSummary Origin = "summary"
	// OriginVerdict is a past investigation's verdict line.
	OriginVerdict Origin = "verdict"
)

// Section is the unit of retrieval: one labelled piece of a document's text.
type Section struct {
	// Label is the property name or the heading; "Body" for text before the first heading.
	Label string
	// Part is 0 unless the section was split to fit MaxSectionChars.
	Part int
	// Text is sanitised plain text, 1 to MaxSectionChars characters.
	Text string
	// Origin says where the text came from.
	Origin Origin
}

// Digest is the identity of a section version: SHA-256 over its label, part and text. Two
// documents carrying the same text under the same label share a digest, and so a vector.
func (s Section) Digest() string {
	h := sha256.New()
	h.Write([]byte(s.Label))
	h.Write([]byte{0})
	h.Write([]byte(strconv.Itoa(s.Part)))
	h.Write([]byte{0})
	h.Write([]byte(s.Text))
	return hex.EncodeToString(h.Sum(nil))
}

// ErrInvalidSections is wrapped by every refusal of ValidateSections.
var ErrInvalidSections = errors.New("knowledge: invalid sections")

// ValidateSections refuses a section set a store must not accept: more than MaxSections, an empty
// or over-long label, an empty or over-long text, or two sections with the same label and part.
func ValidateSections(sections []Section) error {
	if len(sections) > MaxSections {
		return fmt.Errorf("%w: %d sections, the limit is %d", ErrInvalidSections, len(sections), MaxSections)
	}
	seen := make(map[sectionKey]struct{}, len(sections))
	for i, s := range sections {
		switch {
		case strings.TrimSpace(s.Label) == "":
			return fmt.Errorf("%w: section %d has an empty label", ErrInvalidSections, i)
		case utf8.RuneCountInString(s.Label) > MaxLabelChars:
			return fmt.Errorf("%w: section %d's label is over %d characters", ErrInvalidSections, i, MaxLabelChars)
		case strings.TrimSpace(s.Text) == "":
			return fmt.Errorf("%w: section %q part %d is empty", ErrInvalidSections, s.Label, s.Part)
		case utf8.RuneCountInString(s.Text) > MaxSectionChars:
			return fmt.Errorf("%w: section %q part %d is over %d characters; split it into parts",
				ErrInvalidSections, s.Label, s.Part, MaxSectionChars)
		case s.Part < 0:
			return fmt.Errorf("%w: section %q has a negative part", ErrInvalidSections, s.Label)
		}
		key := sectionKey{label: s.Label, part: s.Part}
		if _, dup := seen[key]; dup {
			return fmt.Errorf("%w: section %q part %d appears twice", ErrInvalidSections, s.Label, s.Part)
		}
		seen[key] = struct{}{}
	}
	return nil
}

type sectionKey struct {
	label string
	part  int
}

// PutResult counts what one Put changed.
type PutResult struct {
	// Opened is the number of new section versions.
	Opened int
	// Closed is the number of versions closed because they changed or disappeared.
	Closed int
	// Unchanged is the number of sections whose text was already current.
	Unchanged int
}

// Hit is one section a search returned.
type Hit struct {
	DocumentRef string
	Label       string
	Part        int
	Text        string
	Digest      string
	Origin      Origin
	// Rank is 1-based: the best hit is 1.
	Rank int
}

// PendingSection is a section version that has no vector yet for some model.
type PendingSection struct {
	Digest string
	Label  string
	Text   string
}

// DigestVector is one section's vector.
type DigestVector struct {
	Digest string
	Vector []float32
}

// ErrDimensionMismatch is returned when a vector's length is not its model's dimensions.
var ErrDimensionMismatch = errors.New("knowledge: vector length does not match the model's dimensions")

// Store holds every version of every document's sections, each with the instant it became known,
// and answers searches as of an instant. Implementations must pass storetest.Run.
type Store interface {
	// Put replaces the current section set of one document, at instant at. Sections absent from
	// sections are closed at at; a section whose digest equals its open version is a no-op.
	Put(ctx context.Context, documentRef string, sections []Section, at time.Time) (PutResult, error)

	// Lexical returns up to k sections of the given documents, known at observed, ranked by the
	// store's lexical relevance to query. Only sections sharing a term with query are returned.
	// Rank order is deterministic for a given store state; ties go to document ref, label, part.
	Lexical(ctx context.Context, query string, documents []string, observed time.Time, k int) ([]Hit, error)

	// Semantic returns up to k sections of the given documents, known at observed, that have a
	// vector for model, ranked by cosine similarity to vector. A vector whose length is not
	// model.Dimensions is refused with ErrDimensionMismatch.
	Semantic(ctx context.Context, vector []float32, model ModelID, documents []string, observed time.Time, k int) ([]Hit, error)

	// Pending lists section digests known now that have no vector for model, oldest first.
	Pending(ctx context.Context, model ModelID, limit int) ([]PendingSection, error)

	// PutVectors stores vectors for model; idempotent by (digest, model). A vector whose length
	// is not model.Dimensions is refused with ErrDimensionMismatch.
	PutVectors(ctx context.Context, model ModelID, vectors []DigestVector) error
}
