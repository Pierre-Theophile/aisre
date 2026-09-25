// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Pierre-Theophile/aisre/internal/graph"
)

// The audit's input: a human-supplied incident list (T010, FR-069).
//
// Three properties of this format are contract rather than convenience.
//
// It is transcribable without leaking. Every value is an identifier the owner chooses, a date,
// or a member of a closed set from vocabulary.go. There is no title, no description, no
// "notes", and no free-text cause: the `stated_cause` the audit records is composed from the
// category and the class (`third_party_outage/change_induced`), so the detailed private audit
// can be transcribed into this file field by field and the file can be read by anyone.
//
// It never infers. `cause.category` and `cause.class` are required on every incident, and a
// list that omits them is refused by name rather than guessed at: the whole point of the audit
// is that a human supplied the cause and the graph was measured against it (FR-069). The same
// rule is why an `undecidable` verdict must carry a coded reason — "undecidable" with no reason
// is indistinguishable from "nobody looked".
//
// It is comparable to itself. Feeder sets are declared once, ordered, and cumulative: set n+1
// is set n plus the feeders it names. That is what makes `audit coverage compare` able to
// attribute a movement in the ceiling to the feeders that were added (FR-071a) rather than to
// an unexamined change in how the list was written.
//
// YAML and JSON are both accepted, by the same strict decoder: JSON is YAML, and a second
// parser would be a second set of bugs. Unknown keys are rejected everywhere.

// ListVersion is the only version of the input format this build accepts.
const ListVersion = 1

// List is a parsed incident list — the audit's whole input.
type List struct {
	// Version is the input format version. It must be ListVersion.
	Version int `yaml:"version" json:"version"`
	// AuditID names this audit run. It appears in the published result, in
	// `investigation.coverage_audits.audit_id`, and in every investigation whose π₀ came from
	// it, so it must be stable and meaningful — `2026-09` rather than a UUID.
	AuditID string `yaml:"audit_id" json:"audit_id"`
	// RunAt is when the audit was carried out.
	RunAt Instant `yaml:"run_at" json:"run_at"`
	// Author is the principal who carried it out. It is recorded with the row so the ceiling
	// is attributable (FR-069a).
	Author string `yaml:"author" json:"author"`
	// Corpus describes what was audited and what was left out.
	Corpus Corpus `yaml:"corpus" json:"corpus"`
	// FeederSets are the graph configurations the incidents were assessed against, in
	// increasing order of coverage. Each is cumulative over the one before it.
	FeederSets []FeederSet `yaml:"feeder_sets" json:"feeder_sets"`
	// Incidents is the audited list itself.
	Incidents []Incident `yaml:"incidents" json:"incidents"`
}

// Corpus records the shape of the list: which period it was drawn from, and which incidents of
// that period were deliberately left out. The ceiling is meaningless without its denominator,
// and the denominator is meaningless without the exclusions that produced it (FR-069a).
type Corpus struct {
	// Label names the corpus, e.g. `owner-org-2026-09`.
	Label string `yaml:"label" json:"label"`
	// From and To bound the period the incidents were drawn from.
	From Instant `yaml:"from" json:"from"`
	To   Instant `yaml:"to" json:"to"`
	// Excluded lists incidents of the period that are not in Incidents, each with a coded
	// reason.
	Excluded []Exclusion `yaml:"excluded" json:"excluded"`
}

// Exclusion is one incident of the period that is not audited, and why.
type Exclusion struct {
	// Ref is the opaque incident reference.
	Ref string `yaml:"ref" json:"ref"`
	// Reason is why it is not in the list.
	Reason ExclusionReason `yaml:"reason" json:"reason"`
}

// FeederSet is one graph configuration the list was assessed against.
type FeederSet struct {
	// Name identifies the set, e.g. `feature-001` or `+vendor-notice`.
	Name string `yaml:"name" json:"name"`
	// Order is its place in the cumulative sequence, starting at 1. The set with the highest
	// order is the best realistic configuration and is the one whose ceiling is in force
	// unless `--feeder-set` says otherwise.
	Order int `yaml:"order" json:"order"`
	// Feeders are the feeders this set ADDS to the set below it. The full configuration of a
	// set is the union of its own feeders and every lower set's.
	Feeders []string `yaml:"feeders" json:"feeders"`
}

// Incident is one audited incident. It carries no prose at all.
type Incident struct {
	// Ref is an opaque reference the owner chooses. It is the only identifier that reaches
	// the database, so it must not be a title: `INC-0007`, not `checkout down after deploy`.
	Ref string `yaml:"ref" json:"ref"`
	// AlertAt is the instant the alert fired, or the date it fired on. A date is enough for
	// the audit's arithmetic and leaks less, so the format accepts both.
	AlertAt Instant `yaml:"alert_at" json:"alert_at"`
	// Cause is the human-supplied cause, as a category and a class. Required: the audit
	// measures, it never infers.
	Cause Cause `yaml:"cause" json:"cause"`
	// Observability is the verdict per feeder set, keyed by feeder set name. Every declared
	// feeder set must appear.
	Observability map[string]Observation `yaml:"observability" json:"observability"`
}

// Cause is a stated cause, reduced to the two closed-set fields the audit actually needs.
type Cause struct {
	// Category is the cause's category, from the published eighteen (vocabulary.go).
	Category Category `yaml:"category" json:"category"`
	// Class says whether a change caused it and, if not, what the correct engine answer is.
	Class CauseClass `yaml:"class" json:"class"`
	// Ref is an OPTIONAL graph identifier for the cause — an entity or change id — for an
	// auditor who wants the classification checked against the graph by hand. It is never
	// required and it is never derived.
	Ref string `yaml:"ref,omitempty" json:"ref,omitempty"`
}

// Observation is one verdict under one feeder set.
//
// It decodes from either a bare verdict (`feature-001: observed`) or a mapping
// (`feature-001: {verdict: undecidable, reason: no_record}`), because the bare form is what an
// auditor writes thirty-nine times and the mapping is what they write twice.
type Observation struct {
	// Verdict is the observability verdict.
	Verdict Verdict `yaml:"verdict" json:"verdict"`
	// Reason is required when Verdict is `undecidable` and forbidden otherwise.
	Reason Reason `yaml:"reason,omitempty" json:"reason,omitempty"`
}

// UnmarshalYAML accepts the scalar and mapping spellings of an observation.
func (o *Observation) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		o.Verdict = Verdict(node.Value)
		o.Reason = ""
		return nil
	}
	var full struct {
		Verdict Verdict `yaml:"verdict"`
		Reason  Reason  `yaml:"reason"`
	}
	if err := node.Decode(&full); err != nil {
		return err
	}
	o.Verdict, o.Reason = full.Verdict, full.Reason
	return nil
}

// Instant is a timestamp that accepts a date as well as an RFC 3339 instant.
//
// An audit's arithmetic needs neither minutes nor seconds, and an alert instant recorded to the
// second is a sharper join key against a private incident record than the audit needs. A
// date-only value is read as midnight UTC and is the recommended granularity.
type Instant struct {
	time.Time
	// DateOnly records how the value was written, so that a round-trip through the format
	// does not silently sharpen a date into an instant.
	DateOnly bool
}

const dateLayout = "2006-01-02"

// UnmarshalYAML reads `2026-01-05` or `2026-01-05T09:12:00Z`.
func (i *Instant) UnmarshalYAML(node *yaml.Node) error {
	return i.parse(node.Value)
}

// UnmarshalJSON reads the same two spellings from a JSON string.
func (i *Instant) UnmarshalJSON(raw []byte) error {
	var s string
	if err := yaml.Unmarshal(raw, &s); err != nil {
		return err
	}
	return i.parse(s)
}

func (i *Instant) parse(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("empty timestamp: want %s or an RFC 3339 instant", dateLayout)
	}
	if t, err := time.Parse(dateLayout, value); err == nil {
		i.Time, i.DateOnly = t.UTC(), true
		return nil
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return fmt.Errorf("timestamp %q: want %s or an RFC 3339 instant", value, dateLayout)
	}
	i.Time, i.DateOnly = t.UTC(), false
	return nil
}

// MarshalJSON writes the spelling the input used, in UTC.
func (i Instant) MarshalJSON() ([]byte, error) {
	return []byte(`"` + i.String() + `"`), nil
}

// MarshalYAML writes the spelling the input used, in UTC.
func (i Instant) MarshalYAML() (any, error) { return i.String(), nil }

// String renders the instant the way it was written.
func (i Instant) String() string {
	if i.DateOnly {
		return i.Time.UTC().Format(dateLayout)
	}
	return i.Time.UTC().Format(time.RFC3339)
}

// LoadList reads and validates an incident list from a YAML or JSON file.
func LoadList(path string) (*List, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // the operator names the file they audit.
	if err != nil {
		return nil, fmt.Errorf("audit: read incident list: %w", err)
	}
	list, err := ParseList(raw)
	if err != nil {
		return nil, fmt.Errorf("audit: %s: %w", path, err)
	}
	return list, nil
}

// ParseList decodes an incident list strictly and validates it. Unknown keys are rejected: a
// misspelled field in an audit input is a silently wrong ceiling, and a wrong ceiling is a
// wrong π₀ in every investigation that cites it.
func ParseList(raw []byte) (*List, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)

	list := &List{}
	if err := decoder.Decode(list); err != nil {
		return nil, fmt.Errorf("decode incident list: %w", err)
	}
	if err := list.Validate(); err != nil {
		return nil, err
	}
	return list, nil
}

// Validate refuses a list the audit cannot honestly measure.
//
// Every refusal here is a refusal to guess. A missing cause is the important one — FR-069 makes
// the supplied cause the audit's whole premise — but a feeder set an incident says nothing
// about, or a verdict that improves and then regresses as feeders are added, are the same kind
// of error: a hole in the transcription that would otherwise be read as a measurement.
func (l *List) Validate() error {
	if l.Version != ListVersion {
		return fmt.Errorf("version %d: this build reads version %d", l.Version, ListVersion)
	}
	if strings.TrimSpace(l.AuditID) == "" {
		return fmt.Errorf("audit_id is required: every published ceiling is cited by id")
	}
	if l.RunAt.IsZero() {
		return fmt.Errorf("run_at is required")
	}
	if strings.TrimSpace(l.Author) == "" {
		return fmt.Errorf("author is required: a ceiling nobody signed is not a measurement")
	}
	if err := l.validateCorpus(); err != nil {
		return err
	}
	if err := l.validateFeederSets(); err != nil {
		return err
	}
	return l.validateIncidents()
}

func (l *List) validateCorpus() error {
	if strings.TrimSpace(l.Corpus.Label) == "" {
		return fmt.Errorf("corpus.label is required")
	}
	if l.Corpus.From.IsZero() || l.Corpus.To.IsZero() {
		return fmt.Errorf("corpus.from and corpus.to are required: a ceiling without its period is not comparable")
	}
	if l.Corpus.To.Before(l.Corpus.From.Time) {
		return fmt.Errorf("corpus.to %s is before corpus.from %s", l.Corpus.To, l.Corpus.From)
	}
	seen := map[string]bool{}
	for i, excluded := range l.Corpus.Excluded {
		if strings.TrimSpace(excluded.Ref) == "" {
			return fmt.Errorf("corpus.excluded[%d]: ref is required", i)
		}
		if seen[excluded.Ref] {
			return fmt.Errorf("corpus.excluded[%d]: %q is listed twice", i, excluded.Ref)
		}
		seen[excluded.Ref] = true
		if !excluded.Reason.Valid() {
			return fmt.Errorf("corpus.excluded[%d] (%s): reason %q: want one of %s",
				i, excluded.Ref, excluded.Reason, oneOf(exclusionOrder))
		}
	}
	return nil
}

func (l *List) validateFeederSets() error {
	if len(l.FeederSets) == 0 {
		return fmt.Errorf("feeder_sets is required: a ceiling is always measured against a named graph configuration")
	}
	names := map[string]bool{}
	orders := map[int]string{}
	for i, set := range l.FeederSets {
		if strings.TrimSpace(set.Name) == "" {
			return fmt.Errorf("feeder_sets[%d]: name is required", i)
		}
		if names[set.Name] {
			return fmt.Errorf("feeder_sets[%d]: %q is declared twice", i, set.Name)
		}
		names[set.Name] = true
		if set.Order < 1 {
			return fmt.Errorf("feeder_sets[%d] (%s): order %d: the cumulative sequence starts at 1",
				i, set.Name, set.Order)
		}
		if other, clash := orders[set.Order]; clash {
			return fmt.Errorf("feeder_sets[%d] (%s): order %d is already taken by %q",
				i, set.Name, set.Order, other)
		}
		orders[set.Order] = set.Name
		if len(set.Feeders) == 0 {
			return fmt.Errorf("feeder_sets[%d] (%s): feeders is required — name what this set adds",
				i, set.Name)
		}
	}
	return nil
}

func (l *List) validateIncidents() error {
	if len(l.Incidents) == 0 {
		return fmt.Errorf("incidents is required: an empty list has no ceiling to publish")
	}
	excluded := map[string]bool{}
	for _, e := range l.Corpus.Excluded {
		excluded[e.Ref] = true
	}
	seen := map[string]bool{}
	for i, incident := range l.Incidents {
		if strings.TrimSpace(incident.Ref) == "" {
			return fmt.Errorf("incidents[%d]: ref is required", i)
		}
		if seen[incident.Ref] {
			return fmt.Errorf("incidents[%d]: %q is listed twice", i, incident.Ref)
		}
		seen[incident.Ref] = true
		if excluded[incident.Ref] {
			return fmt.Errorf("incidents[%d] (%s): also listed in corpus.excluded", i, incident.Ref)
		}
		if incident.AlertAt.IsZero() {
			return fmt.Errorf("incidents[%d] (%s): alert_at is required", i, incident.Ref)
		}
		if err := l.validateCause(i, incident); err != nil {
			return err
		}
		if err := l.validateObservability(i, incident); err != nil {
			return err
		}
	}
	return nil
}

func (l *List) validateCause(i int, incident Incident) error {
	// FR-069: the cause is supplied by the operator or the reviewer, never derived by the
	// engine. A list that supplies no cause is refused rather than audited.
	if incident.Cause.Category == "" || incident.Cause.Class == "" {
		return fmt.Errorf(
			"incidents[%d] (%s): cause.category and cause.class are required — the audit measures, it never infers (FR-069)",
			i, incident.Ref)
	}
	if !incident.Cause.Category.Valid() {
		return fmt.Errorf("incidents[%d] (%s): cause.category %q: want one of %s",
			i, incident.Ref, incident.Cause.Category, oneOf(categoryOrder))
	}
	if !incident.Cause.Class.Valid() {
		return fmt.Errorf("incidents[%d] (%s): cause.class %q: want one of %s",
			i, incident.Ref, incident.Cause.Class, oneOf(causeClassOrder))
	}
	return nil
}

func (l *List) validateObservability(i int, incident Incident) error {
	for name := range incident.Observability {
		if !slices.ContainsFunc(l.FeederSets, func(s FeederSet) bool { return s.Name == name }) {
			return fmt.Errorf("incidents[%d] (%s): observability names undeclared feeder set %q",
				i, incident.Ref, name)
		}
	}
	previousRank, havePrevious := 0, false
	previousName := ""
	for _, set := range l.orderedFeederSets() {
		observation, ok := incident.Observability[set.Name]
		if !ok {
			return fmt.Errorf("incidents[%d] (%s): no verdict for feeder set %q — every declared set needs one",
				i, incident.Ref, set.Name)
		}
		if !observation.Verdict.Valid() {
			return fmt.Errorf("incidents[%d] (%s): feeder set %q: verdict %q: want one of %s",
				i, incident.Ref, set.Name, observation.Verdict, oneOf(verdictOrder))
		}
		if observation.Verdict == VerdictUndecidable {
			if !observation.Reason.Valid() {
				return fmt.Errorf(
					"incidents[%d] (%s): feeder set %q: an undecidable verdict needs a reason, one of %s",
					i, incident.Ref, set.Name, oneOf(reasonOrder))
			}
			continue
		}
		if observation.Reason != "" {
			return fmt.Errorf("incidents[%d] (%s): feeder set %q: reason %q is only meaningful with verdict %s",
				i, incident.Ref, set.Name, observation.Reason, VerdictUndecidable)
		}
		rank, ok := observation.Verdict.observabilityRank()
		if !ok {
			continue
		}
		// Feeder sets are cumulative, so observability can only improve along the sequence.
		// A regression is a transcription error, and it would show up as a ceiling that
		// moved for a reason nobody can attribute (FR-071a).
		if havePrevious && rank < previousRank {
			return fmt.Errorf(
				"incidents[%d] (%s): feeder set %q is less observable (%s) than %q — feeder sets are cumulative",
				i, incident.Ref, set.Name, observation.Verdict, previousName)
		}
		previousRank, previousName, havePrevious = rank, set.Name, true
	}
	return nil
}

// orderedFeederSets returns the feeder sets sorted by their cumulative order.
func (l *List) orderedFeederSets() []FeederSet {
	sets := slices.Clone(l.FeederSets)
	slices.SortStableFunc(sets, func(a, b FeederSet) int { return a.Order - b.Order })
	return sets
}

// cumulativeFeeders returns the full feeder configuration of the named set: its own feeders
// plus every lower set's, deduplicated and in declaration order.
func (l *List) cumulativeFeeders(name string) []string {
	var out []string
	for _, set := range l.orderedFeederSets() {
		for _, feeder := range set.Feeders {
			if !slices.Contains(out, feeder) {
				out = append(out, feeder)
			}
		}
		if set.Name == name {
			break
		}
	}
	return out
}

// Digest is the SHA-256 of the canonical JSON of the parsed list. It is recorded with the audit
// row as `input_digest`, so that two runs can be shown to have been measured over the same list
// rather than asserted to have been (FR-071a).
func (l *List) Digest() (string, error) {
	canonical, err := graph.CanonicalJSON(l)
	if err != nil {
		return "", fmt.Errorf("audit: digest incident list: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
