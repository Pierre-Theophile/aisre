// SPDX-License-Identifier: Apache-2.0

package fixture

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Pierre-Theophile/aisre/internal/graph"
)

// The manifest is the fixture's contract (contracts/fixture-format.md §manifest.yaml).
//
// It says who emitted the events and under which guarantees, what the clock window is, which
// queries golden/ must answer, and which events the graph is required to refuse. Everything the
// verifier does is driven from here, so this file is deliberately forgiving in one direction
// only: unknown *query* keys are kept in Query.Extra rather than rejected, because a later
// phase adds query kinds and the fixtures that use them must not have to wait for this loader.
// Unknown keys anywhere else are a broken fixture and are reported as such.

// DefaultEventsFile is the events file a manifest that does not name one is assumed to use.
const DefaultEventsFile = "events.jsonl"

// ManifestFile is the file name LoadManifest reads inside a fixture directory.
const ManifestFile = "manifest.yaml"

// Manifest is a parsed manifest.yaml.
type Manifest struct {
	// Dir is the directory the manifest was loaded from. It is set by LoadManifest and is not
	// a YAML key, so that a caller holding only a Manifest can still find events.jsonl.
	Dir string `yaml:"-"`

	// ID is the fixture id and must equal the directory name.
	ID string `yaml:"id"`
	// Family groups fixtures that exercise the same behaviour, e.g. "baseline-topology".
	Family string `yaml:"family"`
	// Description is the human summary shown in verification reports.
	Description string `yaml:"description"`
	// HandAuthored marks a fixture whose events.jsonl was written by a human against the
	// published schema rather than recorded from payloads/. `fixture record` must never
	// rewrite events.jsonl for such a fixture (fixtures/README.md).
	HandAuthored bool `yaml:"hand_authored"`

	// Events is the accepted-event file name, relative to Dir. Defaults to events.jsonl.
	Events string `yaml:"events"`
	// RejectedEvents is the must-be-rejected event file name, relative to Dir. Empty means
	// the fixture ships none.
	RejectedEvents string `yaml:"rejected_events"`

	// SchemaVersion is the event schema version the fixture's events declare.
	SchemaVersion string `yaml:"schema_version"`
	// SDKVersion is the feeder SDK version the fixture was produced with.
	SDKVersion string `yaml:"sdk_version"`

	// Sources are the feeders to register before the first event is applied (FR-018).
	Sources []Source `yaml:"sources"`

	// Clock bounds the fixture's window. End is what the pinned golden pass pins observed
	// time to (US7).
	Clock struct {
		Start time.Time `yaml:"start"`
		End   time.Time `yaml:"end"`
	} `yaml:"clock"`

	// Queries is what golden/ must contain: one file per entry.
	Queries []Query `yaml:"queries"`

	// GroundTruth is family-specific truth used by `fixture verify --report`, e.g. the
	// culprit change of a rollout-regression fixture. It is left untyped because each family
	// carries different keys.
	GroundTruth map[string]any `yaml:"ground_truth"`

	// ExpectRejected names the events that MUST be refused and the reason code they must be
	// refused with (SC-009).
	ExpectRejected []struct {
		EventID    string `yaml:"event_id"`
		ReasonCode string `yaml:"reason_code"`
	} `yaml:"expect_rejected"`

	// HumanDecisions are the resolution decisions a recording replays as events from a named
	// principal (FR-041). They survive replay, so they are part of the fixture, not of the
	// harness.
	HumanDecisions []HumanDecision `yaml:"human_decisions"`

	// Incident is feature 002's `incident:` block: the question, the ground truth, the shape
	// of the recorded world and the run count (002 contracts/incident-format.md, FR-061b). It
	// is nil for a 001 fixture, which is every fixture outside `fixtures/incidents/`.
	//
	// It lives on the 001 manifest rather than in a parallel loader because FR-063 says an
	// incident fixture is a 001 fixture with additions and not a second input format — and
	// because this decoder is strict, so an `incident:` key it did not know would make every
	// incident fixture unloadable.
	Incident *Incident `yaml:"incident"`
}

// IsIncident reports whether the fixture carries an `incident:` block.
func (m *Manifest) IsIncident() bool { return m != nil && m.Incident != nil }

// Source is one feeder's declared contract, as the manifest states it.
type Source struct {
	// SourceID is the feeder's identity, e.g. "k8s:demo".
	SourceID string
	// Kind is the connector family: "k8s", "otel", and later vendors.
	Kind string
	// Ordering is "per_source_sequence" when the feeder numbers its events, "none" otherwise.
	Ordering string
	// ReorderingWindow is how far out of order the feeder may deliver. It is written as a Go
	// duration in YAML ("60s", "5m") and is the window the shuffle check permutes within
	// (FR-048).
	ReorderingWindow time.Duration
}

// UnmarshalYAML parses a source, turning reordering_window from "60s" into a duration.
func (s *Source) UnmarshalYAML(node *yaml.Node) error {
	var raw struct {
		SourceID         string `yaml:"source_id"`
		Kind             string `yaml:"kind"`
		Ordering         string `yaml:"ordering"`
		ReorderingWindow string `yaml:"reordering_window"`
	}
	if err := node.Decode(&raw); err != nil {
		return err
	}
	s.SourceID = raw.SourceID
	s.Kind = raw.Kind
	s.Ordering = raw.Ordering
	if raw.ReorderingWindow != "" {
		window, err := time.ParseDuration(raw.ReorderingWindow)
		if err != nil {
			return fmt.Errorf("source %s: reordering_window %q: %w", raw.SourceID, raw.ReorderingWindow, err)
		}
		s.ReorderingWindow = window
	}
	return nil
}

// HumanDecision is a resolution decision a human took, replayed as an event at a stated time.
//
// Exactly one of Confirm, Reject, Merge or Split is set; the others are empty. Writing them as
// four keys rather than as a `kind` plus arguments is what makes a manifest readable: the line
// says what happened, not which enum value to look up.
//
// The decision is replayed as an ordinary event carrying Principal, at the observed time At, so
// a fixture verifies exactly what FR-040 promises — that a human decision survives a full replay
// and re-resolution (SC-007).
type HumanDecision struct {
	// Confirm names the two refs a human declared to be the same entity (FR-040).
	Confirm []string `yaml:"confirm"`
	// Reject names the two refs a human declared to be different entities (FR-040).
	Reject []string `yaml:"reject"`
	// Merge names the two refs a human merged without a suggestion behind it (FR-040).
	Merge []string `yaml:"merge"`
	// Split names the entity a human took apart, with Detach naming the identifiers that left
	// it (FR-039).
	Split  string   `yaml:"split"`
	Detach []string `yaml:"detach"`
	// At is when the decision was taken; it becomes the event's observed time.
	At time.Time `yaml:"at"`
	// Reason is the human-readable rationale, which constitution VI requires to be stored.
	Reason string `yaml:"reason"`
	// Principal is the authenticated individual who decided, as auth.Principal.Key() spells it:
	// `<issuer>|<subject>`, e.g. `sre-agent-dev|alice` (FR-041, data-model.md §graph.principals).
	Principal string `yaml:"principal"`
}

// Kind returns the decision kind and the references it names, in the spelling the event carries.
// It reports an error for a decision that names none, or more than one, of the four forms.
func (d HumanDecision) Kind() (kind string, refs []string, err error) {
	set := 0
	if len(d.Confirm) > 0 {
		kind, refs, set = "confirm", d.Confirm, set+1
	}
	if len(d.Reject) > 0 {
		kind, refs, set = "reject", d.Reject, set+1
	}
	if len(d.Merge) > 0 {
		kind, refs, set = "merge", d.Merge, set+1
	}
	if d.Split != "" {
		kind, refs, set = "split", append([]string{d.Split}, d.Detach...), set+1
	}
	switch {
	case set == 0:
		return "", nil, fmt.Errorf("a human_decisions entry names no decision; want one of confirm, reject, merge, split")
	case set > 1:
		return "", nil, fmt.Errorf("a human_decisions entry names more than one decision; write them as separate entries")
	case kind != "split" && len(refs) != 2:
		return "", nil, fmt.Errorf("a %s decision names %d references; want exactly two", kind, len(refs))
	case kind == "split" && len(refs) < 2:
		return "", nil, fmt.Errorf("a split decision needs an entity and at least one identifier to detach")
	}
	return kind, refs, nil
}

// Query is one manifest query: the parameters of a read the fixture's golden/ must answer.
//
// It is a union over every query kind the contract names (subgraph, diff, impact, pointers,
// history, audit, suggestions, extent), because a manifest entry is a flat YAML mapping and a
// verifier has to be able to read one without knowing which kind it will turn out to be. The
// query layer picks the fields its kind uses; Extra carries anything this loader did not know
// about, so a fixture may use a key that arrives with a later phase.
type Query struct {
	// Name is the query's identity within the fixture and the middle part of its golden file
	// name.
	Name string `yaml:"name"`
	// Kind is one of subgraph, diff, impact, pointers, history, audit, suggestions, extent.
	Kind string `yaml:"kind"`

	// Focus is the entity the query starts from, written `<namespace>=<value>`, e.g.
	// `otel.service.name=checkout`. FocusRef parses it.
	Focus string `yaml:"focus"`

	// ValidAt is the valid-time instant to answer as of.
	ValidAt time.Time `yaml:"valid_at"`
	// ObservedAt pins observed time. Zero means "as known now" (constitution II).
	ObservedAt time.Time `yaml:"observed_at"`
	// T1 and T2 bound a diff window.
	T1 time.Time `yaml:"t1"`
	T2 time.Time `yaml:"t2"`
	// ReferenceAt is the reference instant a diff ranks changes against.
	ReferenceAt time.Time `yaml:"reference_at"`

	// Hops is the neighbourhood radius of a subgraph or impact query.
	Hops int `yaml:"hops"`
	// PerHopCap bounds the fan-out expanded at each hop; exceeding it must be reported as a
	// truncation (US1 scenario 3).
	PerHopCap int `yaml:"per_hop_cap"`
	// TotalCap bounds the whole result.
	TotalCap int `yaml:"total_cap"`
	// Direction is "upstream", "downstream" or "both".
	Direction string `yaml:"direction"`
	// EdgeTypes filters the traversal to these edge types.
	EdgeTypes []string `yaml:"edge_types"`
	// MinWeightClass filters edges below a weight class. It is a pointer because 0 is a
	// meaningful value and "unset" has to be distinguishable from it.
	MinWeightClass *int `yaml:"min_weight_class"`

	// RefA and RefB are the pair a resolution-audit query asks about.
	RefA string `yaml:"ref_a"`
	RefB string `yaml:"ref_b"`
	// Status filters a suggestions query, e.g. "pending".
	Status string `yaml:"status"`

	// ExpectEmpty declares that this query's answer is MEANT to hold nothing, and says why (T151).
	//
	// It exists because an empty golden is the most dangerous kind: it reads as coverage, it compares
	// equal to itself on every later run, and nothing about it looks wrong. Eight of this corpus's
	// deploy fixtures carried one for months — their principal subgraph query answered at an instant
	// no change was valid at, so the graph correctly returned nothing and `fixture verify` correctly
	// agreed with it, while the acceptance evidence for two user stories asserted nothing (T150).
	//
	// So the recorder refuses to write an empty answer unless a human has written down that it is
	// intended. The value is the REASON rather than a bare `true`, because the reason is what a
	// reviewer checks: `baseline-topology-01`'s "before recorded history" query is legitimately empty
	// and says so, and a query that is empty because it asks the wrong question cannot be made to
	// look like it by setting a boolean.
	ExpectEmpty string `yaml:"expect_empty"`

	// Pinned marks the second pass the verifier runs over every query with observed time
	// pinned to clock.end, whose goldens live in golden/pinned/ (US7). It is set by the
	// verifier, never by YAML.
	Pinned bool `yaml:"-"`

	// Extra holds manifest keys this loader does not know, so a fixture written against a
	// later query contract still loads. Keys are as written in YAML.
	Extra map[string]any `yaml:"-"`
}

// knownQueryKeys is every YAML key Query decodes into a named field. Anything else goes to
// Extra. Keeping the list here rather than deriving it by reflection keeps the mapping
// explicit: adding a field to Query without adding its key here is a visible omission.
var knownQueryKeys = map[string]bool{
	"name": true, "kind": true, "focus": true,
	"valid_at": true, "observed_at": true, "t1": true, "t2": true, "reference_at": true,
	"hops": true, "per_hop_cap": true, "total_cap": true, "direction": true,
	"edge_types": true, "min_weight_class": true,
	"ref_a": true, "ref_b": true, "status": true,
	"expect_empty": true,
}

// UnmarshalYAML parses a query and keeps any key it does not recognize in Extra.
func (q *Query) UnmarshalYAML(node *yaml.Node) error {
	type plain Query // a distinct type, so decoding it does not call this method again
	var decoded plain
	if err := node.Decode(&decoded); err != nil {
		return err
	}
	*q = Query(decoded)

	var all map[string]any
	if err := node.Decode(&all); err != nil {
		return err
	}
	for key, value := range all {
		if knownQueryKeys[key] {
			continue
		}
		if q.Extra == nil {
			q.Extra = map[string]any{}
		}
		q.Extra[key] = value
	}
	return nil
}

// FocusRef parses Focus into a ref. It is an error to call it on a query whose kind takes no
// focus (extent), which is why the parse is not done at load time.
func (q Query) FocusRef() (graph.Ref, error) {
	return graph.ParseRef(q.Focus)
}

// EventsPath is the absolute path of the fixture's accepted-event file.
func (m *Manifest) EventsPath() string { return filepath.Join(m.Dir, m.Events) }

// RejectedPath is the absolute path of the fixture's must-be-rejected event file, or "" when
// the fixture ships none.
func (m *Manifest) RejectedPath() string {
	if m.RejectedEvents == "" {
		return ""
	}
	return filepath.Join(m.Dir, m.RejectedEvents)
}

// ReorderingWindows maps each declared source to the window the shuffle check permutes within.
func (m *Manifest) ReorderingWindows() map[string]time.Duration {
	windows := make(map[string]time.Duration, len(m.Sources))
	for _, src := range m.Sources {
		windows[src.SourceID] = src.ReorderingWindow
	}
	return windows
}

// LoadManifest reads and validates dir/manifest.yaml.
//
// Decoding of the manifest's own keys is strict (KnownFields): a key this loader does not know
// is a broken fixture rather than a silently ignored instruction. Queries are the exception —
// an unknown key there is kept in Query.Extra so that a fixture can be written against a query
// contract this binary predates.
func LoadManifest(dir string) (*Manifest, error) {
	path := filepath.Join(dir, ManifestFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("fixture: read manifest: %w", err)
	}

	m := &Manifest{}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if err := decoder.Decode(m); err != nil {
		return nil, fmt.Errorf("fixture: parse %s: %w", path, err)
	}

	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("fixture: resolve %s: %w", dir, err)
	}
	m.Dir = abs
	if m.Events == "" {
		m.Events = DefaultEventsFile
	}
	if err := m.validate(path); err != nil {
		return nil, err
	}
	return m, nil
}

// validate refuses a manifest that would make verification meaningless rather than letting the
// verifier fail later with a confusing message.
func (m *Manifest) validate(path string) error {
	if m.ID == "" {
		return fmt.Errorf("fixture: %s: id is required", path)
	}
	if len(m.Sources) == 0 {
		return fmt.Errorf("fixture: %s: at least one source must be declared, so every event has a registered origin", path)
	}
	seen := map[string]bool{}
	for _, src := range m.Sources {
		if src.SourceID == "" {
			return fmt.Errorf("fixture: %s: a source has no source_id", path)
		}
		if seen[src.SourceID] {
			return fmt.Errorf("fixture: %s: source %s is declared twice", path, src.SourceID)
		}
		seen[src.SourceID] = true
	}
	names := map[string]bool{}
	for i, q := range m.Queries {
		if q.Name == "" || q.Kind == "" {
			return fmt.Errorf("fixture: %s: query %d needs both a name and a kind", path, i)
		}
		key := q.Kind + "." + q.Name
		if names[key] {
			return fmt.Errorf("fixture: %s: two queries are both named %s; golden file names would collide", path, key)
		}
		names[key] = true
	}
	for _, expect := range m.ExpectRejected {
		if expect.EventID == "" || expect.ReasonCode == "" {
			return fmt.Errorf("fixture: %s: every expect_rejected entry needs an event_id and a reason_code", path)
		}
	}
	for i, decision := range m.HumanDecisions {
		if _, _, err := decision.Kind(); err != nil {
			return fmt.Errorf("fixture: %s: human_decisions[%d]: %w", path, i, err)
		}
		if decision.Principal == "" {
			return fmt.Errorf("fixture: %s: human_decisions[%d] names no principal; the graph does not "+
				"accept an anonymous resolution decision (FR-041)", path, i)
		}
		if decision.At.IsZero() {
			return fmt.Errorf("fixture: %s: human_decisions[%d] has no `at`; a decision is replayed at "+
				"the observed time it was taken (FR-023)", path, i)
		}
	}
	return m.Incident.Validate(path)
}
