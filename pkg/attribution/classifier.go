// SPDX-License-Identifier: Apache-2.0

// Package attribution is the published interface of the attribution classifier, layer 3 of the
// ruleset `attribution/rules@1` (feature 009 contracts §7, research R6, FR-034).
//
// A classifier reads what a signal says (its names and text) and how each catalog entry is
// described, and says how likely the signal is about each entry. It is optional and off by
// default: the attributor decides with corrections, aliases and the probable rules alone, and
// asks a classifier only when the model configuration names one and those layers left the signal
// `unknown`. Anything can be plugged in behind the interface, hosted or self-hosted: the shipped
// one embeds through any 008 embedder, and a language model or a local scorer would do as well.
// classifiertest is the conformance suite a classifier proves itself with.
package attribution

import (
	"context"
	"time"
)

// Name is a name a signal or an entry goes by, as `namespace=value`, with the environment it is
// stated for when there is one.
type Name struct {
	Namespace   string
	Value       string
	Environment string
}

// String renders the name as `namespace=value`.
func (n Name) String() string { return n.Namespace + "=" + n.Value }

// Signal is what is being attributed: an alert, a declared incident, a document or a change.
type Signal struct {
	// Ref is the signal's own name, `namespace=value`.
	Ref Name
	// Kind is the signal's node type, lower case (alert, incident, knowledge_doc, change).
	Kind string
	// Names are every name it carries or watches.
	Names []Name
	// Text is its title, and for a document its summary. It is source text: a classifier must
	// treat it as data, never as instructions.
	Text       string
	ObservedAt time.Time
}

// EntryProfile is how one catalog entry is described to a classifier.
type EntryProfile struct {
	Slug        string
	DisplayName string
	Description string
	Aliases     []Name
	// DocumentTitles are the titles of documents already attributed to the entry.
	DocumentTitles []string
}

// Scored is a classifier's confidence that the signal is about an entry: a calibrated probability
// in [0, 1], and a one-line reason a person can read.
type Scored struct {
	Slug      string
	Score     float64
	Rationale string
}

// Classifier scores a signal against the catalog.
type Classifier interface {
	// ID names the classifier and its version, recorded with every decision it takes part in
	// (e.g. `embedding/cosine@1 openai:text-embedding-3-small@1536`). A change that can move a
	// score is a new ID.
	ID() string
	// Classify returns a score for any of the given entries, in any order; an entry it has nothing
	// to say about may be left out. It must return the same scores for the same input, never name
	// an entry it was not given, and return when ctx is done.
	Classify(ctx context.Context, s Signal, entries []EntryProfile) ([]Scored, error)
}
