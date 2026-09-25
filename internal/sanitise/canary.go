// SPDX-License-Identifier: Apache-2.0

package sanitise

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Canaries (T041, FR-139, SC-018, contract §6 gate 2).
//
// A canary is a token seeded into the source data *before* a campaign, and asserted absent from
// every committed artifact afterwards. It is the only gate here that tests the sanitiser rather
// than the sanitiser's opinion of itself: the policy table can be wrong, the field-name vocabulary
// can have a hole in it, the recorder can grow a new path to disk — and a canary planted where the
// real thing lives catches all three, because it does not care *why* a value survived.
//
// The distinction that makes the gate real is that **a surviving canary fails the commit** rather
// than raising a warning (SC-018). A warning on a leak is a leak with a paper trail. There is no
// force flag and no baseline file for a canary, which is the difference between this gate and the
// secrets baseline beside it: an accepted secret placeholder is a judgement a human can make, and
// "this canary survived but it is fine" is not a judgement anybody can make, because the canary
// was planted in the place the real value lives.
//
// # Why a people canary is also checked in hashed form
//
// SC-019 asserts zero people identifiers "in any form, hashed included". A people canary that was
// pseudonymised rather than dropped is therefore a failure, and it is one the raw-token scan would
// miss — the raw token really is absent, because it was hashed. So Survivors also looks for the
// canary's pseudonym under **every published kind**, which is what makes "never hashed" testable
// rather than merely stated.

// CanaryKind is what a canary stands in for, which decides what "survived" means for it.
type CanaryKind string

// The published canary kinds.
const (
	// CanaryPerson stands in for a principal address. It must be absent in every form, hashed
	// included, because that is what FR-135 claims.
	CanaryPerson CanaryKind = "person"
	// CanaryInfrastructure stands in for a service or instance name. Its raw value must be absent;
	// its pseudonym is the correct outcome and is not a survival.
	CanaryInfrastructure CanaryKind = "infrastructure"
	// CanaryFreeText stands in for a description, a log line or a message body. Absent in every
	// form: free text has no disposition that keeps it.
	CanaryFreeText CanaryKind = "free_text"
	// CanarySecret stands in for a credential in a configuration value. Absent in every form.
	CanarySecret CanaryKind = "secret"
)

// canaryKindsHashedToo are the kinds for which a pseudonym is itself a survival.
var canaryKindsHashedToo = map[CanaryKind]bool{
	CanaryPerson:   true,
	CanaryFreeText: true,
	CanarySecret:   true,
}

// CanaryPrefix is the marker every canary token carries. It is a fixed, searchable string so that
// the commit-time scan — which is a grep, implemented separately from this package (FR-138) — can
// find a canary without importing anything.
const CanaryPrefix = "sreagentcanary"

// Canary is one seeded token.
type Canary struct {
	// Kind decides whether a pseudonym of this token counts as survival.
	Kind CanaryKind
	// Token is the value seeded into the source data.
	Token string
	// Carrier is the shape the token was planted inside, so a seeder plants something a rule would
	// plausibly see: an address for a person, a resource name for infrastructure.
	Carrier string
	// Where names the source the token was planted in, for the report.
	Where string
	// SeededAt is when, which is what makes "before the campaign" checkable after the fact.
	SeededAt time.Time
}

// String describes a canary without being confusable with one: the token is abbreviated wherever it
// appears, in the carrier as well as on its own. That second part is not fastidiousness — a report
// that printed the carrier in full would put the whole token in a log line and in a CI transcript,
// which is both a value the report has no business carrying and a string the commit-time canary
// scan would then find in its own output.
func (c Canary) String() string {
	short := c.Token
	if len(short) > len(CanaryPrefix)+6 {
		short = short[:len(CanaryPrefix)+6] + "…"
	}
	carrier := strings.ReplaceAll(c.Carrier, c.Token, short)
	// The free-text carrier embeds a truncated token as well, which the replacement above misses.
	if len(c.Token) > len(CanaryPrefix)+8 {
		carrier = strings.ReplaceAll(carrier, c.Token[:len(CanaryPrefix)+8], short)
	}
	return fmt.Sprintf("%s canary %s planted in %s as %s", c.Kind, short, c.Where, carrier)
}

// NewCanary mints one canary of the given kind for the given source.
func NewCanary(kind CanaryKind, where string) (Canary, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return Canary{}, fmt.Errorf("sanitise: minting a canary: %w", err)
	}
	token := CanaryPrefix + hex.EncodeToString(raw)
	return Canary{
		Kind:     kind,
		Token:    token,
		Carrier:  carrierFor(kind, token),
		Where:    where,
		SeededAt: time.Now().UTC(),
	}, nil
}

// carrierFor wraps a token in the shape its kind is normally written in. A person canary planted
// as a bare token tests nothing a field-name rule would see; planted as an address, it tests the
// rule that actually has to fire.
//
// The domain is deliberately *not* one of the reserved documentation names (`.example`,
// `.invalid`): those are allow-listed by the personal-data scan as placeholders, which is correct
// for a fixture author's `alice@shop.example` and would defeat the canary. A canary has to look
// like the thing the scan is meant to catch.
func carrierFor(kind CanaryKind, token string) string {
	switch kind {
	case CanaryPerson:
		return token + "@canary-domain.co"
	case CanaryInfrastructure:
		return "svc-" + token
	case CanaryFreeText:
		return "deployed by " + token + " for ticket " + token[:len(CanaryPrefix)+8]
	case CanarySecret:
		return "sk-" + token
	default:
		return token
	}
}

// CanarySet is the canaries seeded for one campaign.
type CanarySet struct {
	key      Key
	canaries []Canary
}

// NewCanarySet returns a set that will also recognise hashed forms under key. The key is the
// campaign's corpus key: the same key the sanitiser pseudonymises with, because the hashed form to
// look for is the one this campaign would have produced.
func NewCanarySet(key Key) *CanarySet { return &CanarySet{key: key} }

// Seed mints one canary per kind for a named source and adds them to the set. A campaign calls it
// once per source before recording starts; the returned canaries are what the operator plants.
func (s *CanarySet) Seed(where string, kinds ...CanaryKind) ([]Canary, error) {
	if len(kinds) == 0 {
		kinds = []CanaryKind{CanaryPerson, CanaryInfrastructure, CanaryFreeText, CanarySecret}
	}
	out := make([]Canary, 0, len(kinds))
	for _, kind := range kinds {
		canary, err := NewCanary(kind, where)
		if err != nil {
			return nil, err
		}
		s.canaries = append(s.canaries, canary)
		out = append(out, canary)
	}
	return out, nil
}

// Add adds an already-minted canary, for a campaign that seeded on one run and verifies on another.
func (s *CanarySet) Add(c Canary) { s.canaries = append(s.canaries, c) }

// Len returns how many canaries are seeded. SC-018 is a statement about this number and zero
// survivors, so a campaign that seeded none has not met it — Assert says so rather than passing.
func (s *CanarySet) Len() int { return len(s.canaries) }

// List returns the canaries, ordered by kind then token.
func (s *CanarySet) List() []Canary {
	out := append([]Canary(nil), s.canaries...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Token < out[j].Token
	})
	return out
}

// Survival is one canary found where it must not be.
type Survival struct {
	Canary Canary
	// Form is "raw" or the pseudonym kind the hashed form was found under. It is the difference
	// between "the sanitiser did not see this field" and "the sanitiser hashed a person", which
	// are different bugs with different fixes.
	Form string
}

// FindIn returns the first canary whose raw token appears in text, or nil. It is the cheap check
// the sanitiser runs per value.
func (s *CanarySet) FindIn(text string) *Canary {
	if text == "" || len(s.canaries) == 0 {
		return nil
	}
	for i := range s.canaries {
		if strings.Contains(text, s.canaries[i].Token) {
			return &s.canaries[i]
		}
	}
	return nil
}

// Survivors returns every canary found in artifact, in raw form and — for the kinds whose contract
// is "absent in any form" — under every published pseudonym kind.
//
// It takes bytes rather than a path so that it can be run over a buffer *before* the write as well
// as over a committed file afterwards. FR-137 is about the code path: the useful call is the one
// that happens before the bytes exist on disk.
func (s *CanarySet) Survivors(artifact []byte) ([]Survival, error) {
	if len(s.canaries) == 0 {
		return nil, nil
	}
	text := string(artifact)
	var out []Survival
	for _, canary := range s.canaries {
		if strings.Contains(text, canary.Token) {
			out = append(out, Survival{Canary: canary, Form: "raw"})
			continue
		}
		if !canaryKindsHashedToo[canary.Kind] || !s.key.Configured() {
			continue
		}
		// The hashed form. A people canary that was pseudonymised rather than dropped is a
		// failure of FR-135 that the raw scan cannot see, because the raw token really is gone.
		for _, kind := range Kinds() {
			token, err := s.key.Pseudonym(kind, canary.Token)
			if err != nil {
				return nil, err
			}
			if strings.Contains(text, token) {
				out = append(out, Survival{Canary: canary, Form: string(kind)})
				break
			}
		}
	}
	return out, nil
}

// CanarySurvivedError is the refusal. It is an error type rather than a boolean because the one
// thing this gate must not be is a warning (SC-018).
type CanarySurvivedError struct {
	Canary Canary
	Where  string
	Field  string
	Form   string
}

func (e *CanarySurvivedError) Error() string {
	form := e.Form
	if form == "" {
		form = "raw"
	}
	field := e.Field
	if field == "" {
		field = "an artifact"
	}
	return fmt.Sprintf("sanitise: a canary survived to %s (%s, %s form): %s. "+
		"A surviving canary fails the commit rather than raising a warning (FR-139, SC-018): every "+
		"rule above this point was wrong about this value, so the recording is not safe to keep.",
		field, e.Where, form, e.Canary)
}

// Assert refuses artifact if any canary survived into it, and refuses an empty set: SC-018 is a
// claim about canaries that were seeded, and a campaign that seeded none has not demonstrated it.
func (s *CanarySet) Assert(where string, artifact []byte) error {
	if len(s.canaries) == 0 {
		return fmt.Errorf("sanitise: no canaries were seeded for %s; SC-018 is a claim about "+
			"tokens planted before the campaign, and a set of none is vacuously satisfied by a "+
			"sanitiser that does nothing (FR-139)", where)
	}
	survivors, err := s.Survivors(artifact)
	if err != nil {
		return err
	}
	if len(survivors) == 0 {
		return nil
	}
	first := survivors[0]
	err = &CanarySurvivedError{Canary: first.Canary, Where: where, Form: first.Form}
	if len(survivors) == 1 {
		return err
	}
	return fmt.Errorf("%w (and %d more canaries survived into the same artifact)", err, len(survivors)-1)
}
