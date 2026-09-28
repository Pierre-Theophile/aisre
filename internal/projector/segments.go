// SPDX-License-Identifier: Apache-2.0

package projector

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
)

// Valid-time segmentation (research §5, FR-021).
//
// The requirement this file exists for is one sentence long: *the valid-time state after
// processing must be independent of arrival order*. The way it is met is to make the valid
// timeline a pure function of the set of assertions, never of the sequence they arrived in.
//
// An assertion — one upsert event — says "from V onwards, source S says these are the facts".
// It does not say anything about what was true before V, and it does not say when it stops
// being true. So the state at a valid instant t is:
//
//	for every source S: the assertion from S with the greatest valid time ≤ t
//
// folded together. The segment boundaries are the instants where that fold changes, which is
// a property of the set. Applying the same assertions in any order therefore lands on the same
// partition: an assertion that arrives late splits the segment it lands in and propagates into
// the segments after it, and one that is older than what a segment already has from its source
// is ignored for that segment.
//
// Two holes in the timeline are not the same thing and are treated differently. Before an
// entity's first version there is nothing, so an assertion reaching back fills it. After a
// retraction there is a *fact*: the entity stopped being true. An assertion made before the
// retraction may not refill that, while one made at or after it resurrects the entity — which
// is what a node coming back under the same name is.
//
// Stated once, in the form the code has to honour: **the valid-time partition is a function of
// the set of (assertion, retraction) facts, including assertion instants** — not of the order
// the facts were delivered in, and not of which of them a stored row happens to have coalesced
// away. That last clause is the one this file learned the hard way. Coalescing neighbouring
// segments with equal content is a storage decision, and for a while it was a lossy one: a
// re-assertion folded into an earlier segment left no trace of *when* it was made, so a
// retraction arriving afterwards truncated a segment it should have split, and the interval the
// re-assertion had opened was lost. A coalesced segment therefore remembers, per source, the
// latest instant that source asserted it (`restatements`), and every planner expands a segment
// back into the sub-segments those instants imply before it does anything else. Expanding and
// re-coalescing round-trips exactly, so a plan that changes nothing writes nothing.
//
// Segments are planned here in terms of *which assertion each source contributes*, not in
// terms of materialized values, so the same planner serves nodes and edges.

// segment is one row of a version table as the planner thinks of it: a valid interval plus the
// assertion each source contributes to it.
type segment struct {
	// versionID is set for a segment that already exists in the database, empty for a planned
	// one.
	versionID string
	// start is the inclusive valid lower bound; end the exclusive upper bound, unset when the
	// fact is still true.
	start time.Time
	end   time.Time
	// fromUnknown marks a lower bound that is a first-observation placeholder rather than a
	// known instant (FR-011).
	fromUnknown bool
	toUnknown   bool
	// assertions maps source_id to the event_id of that source's contributing assertion —
	// the earliest one folded into this segment, which is the answer to "since when do we
	// believe this" and the one whose values the segment materializes.
	assertions map[string]string
	// restatements maps source_id to the event_id of the *latest* assertion that source
	// folded into this segment, when coalescing merged a later one in. It is empty on a
	// segment nobody restated, and empty on every segment a planner works with, because
	// expandRestatements turns those instants into real boundaries first.
	//
	// It exists because the instant of a restatement is a fact about the timeline, not
	// bookkeeping: a retraction has to know whether the fact was asserted again *after* the
	// instant it ends, and a coalesced segment that forgot the restatement could not say.
	restatements map[string]string
	// boundary holds events that shaped the interval without asserting content: the
	// retraction that closed it, the merge that re-cut it.
	boundary []string
	// closedAsConsequenceOf names the retracted node version that cascaded this edge closure
	// (edges only, edge case "retracting a node with live edges").
	closedAsConsequenceOf string
}

func (s segment) valid() postgres.TimeRange {
	if s.end.IsZero() {
		return postgres.OpenTimeRange(s.start)
	}
	return postgres.NewTimeRange(s.start, s.end)
}

// endsAtOrBefore reports whether the segment is entirely before t.
func (s segment) endsAtOrBefore(t time.Time) bool {
	return !s.end.IsZero() && !s.end.After(t)
}

// producedBy is the segment's evidence: every event that contributed content plus every event
// that shaped its bounds, deduplicated and sorted so a replay writes the same array (FR-034).
func (s segment) producedBy() []string {
	ids := make([]string, 0, len(s.assertions)+len(s.restatements)+len(s.boundary))
	for _, id := range s.assertions {
		ids = append(ids, id)
	}
	// A restatement is evidence too, and storing it is what lets segmentsOf recover the
	// instant on the next event (FR-034).
	for _, id := range s.restatements {
		ids = append(ids, id)
	}
	ids = append(ids, s.boundary...)
	slices.Sort(ids)
	return slices.Compact(ids)
}

func (s segment) clone() segment {
	out := s
	out.assertions = maps(s.assertions)
	out.restatements = maps(s.restatements)
	out.boundary = slices.Clone(s.boundary)
	return out
}

// rememberRestatement records that sourceID said the same thing again, at a later instant, in a
// part of the timeline this segment has swallowed.
//
// **The first restatement is kept, not the latest**, and that is the whole reason this is
// affordable. A telemetry feeder re-asserts an unchanged edge once per aggregation window — 288
// times a day, for ever — and a segment that remembered the *latest* of those would change on
// every window, which means a new version row on every window: precisely the version-per-window
// explosion coalesce exists to prevent (FR-021, "no-op when nothing differs"). Remembering the
// first one instead is idempotent: once [10:40, ∞) knows it was restated at 10:45, no later
// restatement changes that, the row is byte-identical, and nothing is written.
//
// What it buys is exactly the property the caveat needed: a segment knows whether — and from
// when — the fact it carries was said again after the instant it starts. What it does not buy is
// a full index of every restatement, so a retraction landing *between* two restatements resumes
// at the first one it knows of rather than at the first one that exists. That is strictly more
// than the nothing it had before, and it is bounded: one extra version row per series, once.
func (s *segment) rememberRestatement(sourceID, eventID string) {
	if s.assertions[sourceID] == eventID {
		// The same assertion propagated into the next segment: a restatement of nothing.
		return
	}
	if _, known := s.restatements[sourceID]; known {
		return
	}
	if s.restatements == nil {
		s.restatements = map[string]string{}
	}
	s.restatements[sourceID] = eventID
}

func maps(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// expandRestatements undoes the lossy half of coalescing: a stored segment that folded a later
// assertion of the same content is turned back into the sub-segments those instants imply, so
// every planner works on segments that carry exactly one assertion per source and no hidden
// boundaries.
//
// It is the whole of the order-independence fix. A segment [10:40, ∞) that coalesced an
// assertion at 10:40 with a restatement at 11:55 is two facts, not one; expanded into
// [10:40, 11:55) and [11:55, ∞) it becomes two segments, and the ordinary retraction and upsert
// rules — which already handle a segment asserted after the instant they act on — do the rest.
// Nothing downstream needs a special case.
//
// Two details make the round trip exact, so that expanding a segmentation nothing changed and
// coalescing it again writes no rows:
//
//   - `boundary` goes to the *last* piece, because the events in it shaped the segment's upper
//     bound — the retraction that closed it, the merge that re-cut it — and that bound belongs
//     to the last piece;
//   - `versionID` goes to the first, which is what coalesce takes back (it keeps the earliest
//     non-empty id).
//
// A restatement instant that does not lie strictly inside the segment cuts nothing: it is
// already the segment's own lower bound, or it belongs to a neighbour.
func expandRestatements(segments []segment, assertedAt func(string) time.Time) []segment {
	out := make([]segment, 0, len(segments))
	for _, x := range segments {
		cuts := restatementCuts(x, assertedAt)
		if len(cuts) == 0 {
			out = append(out, x)
			continue
		}
		bounds := append([]time.Time{x.start}, cuts...)
		for i, start := range bounds {
			piece := x.clone()
			piece.restatements = nil
			piece.start = start
			piece.versionID = ""
			piece.boundary = nil
			piece.fromUnknown = x.fromUnknown
			piece.toUnknown = false
			if i == 0 {
				piece.versionID = x.versionID
			} else {
				// Only the first piece inherits the segment's lower-bound flag: the cuts are
				// known instants, asserted by the restatement that made them.
				piece.fromUnknown = false
			}
			if i+1 < len(bounds) {
				piece.end = bounds[i+1]
			} else {
				piece.end = x.end
				piece.toUnknown = x.toUnknown
				piece.boundary = slices.Clone(x.boundary)
			}
			// Each piece takes, per source, the latest assertion at or before its own start.
			for sourceID, id := range x.assertions {
				piece.assertions[sourceID] = id
			}
			for sourceID, id := range x.restatements {
				if !assertedAt(id).After(piece.start) {
					piece.assertions[sourceID] = id
				}
			}
			out = append(out, piece)
		}
	}
	return out
}

// restatementCuts lists the instants inside a segment at which a source restated it, sorted and
// deduplicated. An instant at or before the segment's start, or at or after its end, cuts
// nothing.
func restatementCuts(s segment, assertedAt func(string) time.Time) []time.Time {
	var cuts []time.Time
	for _, id := range s.restatements {
		at := assertedAt(id)
		if !at.After(s.start) {
			continue
		}
		if !s.end.IsZero() && !at.Before(s.end) {
			continue
		}
		cuts = append(cuts, at)
	}
	slices.SortFunc(cuts, func(a, b time.Time) int { return a.Compare(b) })
	return slices.CompactFunc(cuts, func(a, b time.Time) bool { return a.Equal(b) })
}

// planUpsert returns the segmentation that results from adding one assertion to the current
// one. existing must be sorted by start and have pairwise disjoint intervals, which is what
// the exclusion constraint on the version tables guarantees.
//
// assertedAt maps an event id to the valid instant that event asserted, so the planner can
// tell a late-arriving old fact (which must not overwrite a newer one) from a new fact.
func planUpsert(existing []segment, sourceID, eventID string, at time.Time, fromUnknown bool, assertedAt func(string) time.Time, sameContent func(a, b segment) bool) []segment {
	existing = expandRestatements(existing, assertedAt)
	apply := func(s segment) segment {
		if prior, ok := s.assertions[sourceID]; ok && assertedAt(prior).After(at) {
			// This segment already carries a newer assertion from the same source; an older
			// fact does not overwrite a newer one, whichever arrived first.
			return s
		}
		next := s.clone()
		if len(s.assertions) == 0 && at.Before(s.start) {
			// A segment no node assertion supports is a placeholder: markPlaceholder minted it for
			// an edge endpoint, and its start — flagged unknown when the edge's was — is the first
			// instant anything referred to the entity. An assertion from before it says the entity
			// was already there, so that start is no longer a first observation, and keeping the
			// flag would stop coalesce merging across it: the entity would carry a boundary at the
			// edge's instant only when the edge happened to arrive first (003 T184).
			next.fromUnknown = false
		}
		next.assertions[sourceID] = eventID
		return next
	}
	fresh := func(start time.Time, end time.Time, unknown bool) segment {
		return segment{
			start:       start,
			end:         end,
			fromUnknown: unknown,
			assertions:  map[string]string{sourceID: eventID},
		}
	}

	planned := make([]segment, 0, len(existing)+2)
	var cursor *time.Time // upper bound of the coverage seen so far; nil = nothing yet
	unbounded := false    // an existing version already runs to infinity

	for _, x := range existing {
		// A hole immediately before x. Before the entity's first version there is no fact to
		// contradict, so any assertion fills it; after a retraction only an assertion made at
		// or after the retraction may (see the file comment).
		if (cursor == nil || cursor.Before(x.start)) && at.Before(x.start) {
			if cursor == nil || !at.Before(*cursor) {
				planned = append(planned, fresh(at, x.start, fromUnknown))
			}
		}

		switch {
		case x.endsAtOrBefore(at):
			// Entirely before the assertion: untouched.
			planned = append(planned, x)
		case x.start.Before(at):
			// The assertion lands inside x: the part before it keeps what it had, the part
			// from the assertion onwards takes it.
			left := x.clone()
			left.end = at
			left.versionID = x.versionID
			right := apply(x)
			right.versionID = ""
			right.start = at
			right.fromUnknown = fromUnknown
			planned = append(planned, left, right)
		default:
			planned = append(planned, apply(x))
		}
		if x.end.IsZero() {
			// Coverage runs to infinity, so no later version and no tail hole can exist.
			unbounded = true
			continue
		}
		end := x.end
		cursor = &end
	}

	// The tail: either the entity has no versions at all, or its last one is bounded because
	// it was retracted, in which case only an assertion at or after that end may resurrect it.
	if !unbounded && (cursor == nil || !at.Before(*cursor)) {
		planned = append(planned, fresh(at, time.Time{}, fromUnknown))
	}
	return coalesce(planned, sameContent)
}

// planRetract returns the segmentation that results from asserting that the fact stopped being
// true at validEnd (FR-013). Versions extending past the end are cut back to it; versions that
// lie entirely after it are dropped, which closes their observed interval with no successor.
//
// A version built from assertions made at or after the retraction instant survives it. The
// retraction says the fact stopped being true at validEnd; an assertion whose own valid time is
// at or later says it became true again from there, and the later statement about a valid
// instant wins whichever of the two the log happened to receive first. The two intervals are
// half-open and do not overlap — [start, validEnd) then [validEnd, …) — so "it ended at T" and
// "it began again at T" are not a contradiction the planner has to resolve.
//
// This is where a retraction *splits* rather than truncates. A segment that coalesced a
// re-assertion is expanded first (expandRestatements), so the re-assertion is a segment of its
// own by the time the rules below see it: the part before validEnd is cut, the part the
// re-assertion opened is kept. Delivering the retraction before or after the re-assertion
// therefore produces the same partition, which is what FR-021 asks for and what
// TestRetractionAndReassertionAreOrderIndependent pins.
func planRetract(existing []segment, eventID string, validEnd time.Time, consequenceOf string, assertedAt func(string) time.Time, sameContent func(a, b segment) bool) []segment {
	existing = expandRestatements(existing, assertedAt)
	planned := make([]segment, 0, len(existing))
	for _, x := range existing {
		switch {
		case x.endsAtOrBefore(validEnd):
			planned = append(planned, x)
		case !x.start.Before(validEnd):
			// Entirely at or after the end. What survives is what was asserted at or after it:
			// a source that spoke only before the retraction has been retracted, and its
			// contribution goes even though a *different* source keeps the interval alive.
			// Nothing surviving means the fact is no longer true at all and the segment has no
			// successor — dropping it from the plan closes its observed interval.
			kept := assertedAtOrAfter(x, validEnd, assertedAt)
			if len(kept.assertions) == 0 {
				continue
			}
			planned = append(planned, kept)
			continue
		default:
			cut := x.clone()
			cut.versionID = ""
			cut.end = validEnd
			cut.toUnknown = false
			cut.boundary = append(cut.boundary, eventID)
			cut.closedAsConsequenceOf = consequenceOf
			planned = append(planned, cut)
		}
	}
	// A retraction can leave two neighbouring segments saying the same thing — the part it cut
	// and the part a re-assertion at exactly validEnd kept — and those are one version, not two
	// (FR-021: the partition is cut where the facts change, not where the events landed).
	return coalesce(planned, sameContent)
}

// coalesce merges neighbouring segments whose content is the same.
//
// The comparison is on *values*, not on which event asserted them: a feeder re-emitting an
// unchanged node every five minutes asserts the same facts at a hundred different instants, and
// remembering each of those instants as a version boundary would cut the timeline into windows
// instead of into the instants the facts actually changed. Merging keeps the earlier segment's
// assertions, so a version's provenance points at the event that *first* said what it says —
// "since when do we believe this" — rather than at the latest restatement.
//
// The later statement is not thrown away, though: it is remembered in `restatements`, so that
// the *instant* it was made survives the merge. Forgetting it is what made a retraction
// delivered before a re-assertion truncate the whole coalesced segment instead of splitting it,
// and lose the interval the re-assertion had opened (see the file comment, and
// expandRestatements, which turns the instant back into a boundary whenever a planner needs
// one).
//
// This is also what makes the partition a function of the facts rather than of arrival order:
// two orderings that end up asserting the same values over the same intervals produce the same
// cuts (FR-021).
func coalesce(segments []segment, sameContent func(a, b segment) bool) []segment {
	out := make([]segment, 0, len(segments))
	for _, s := range segments {
		if len(out) == 0 {
			out = append(out, s)
			continue
		}
		prev := &out[len(out)-1]
		contiguous := !prev.end.IsZero() && prev.end.Equal(s.start)
		if contiguous && !s.fromUnknown && sameContent(*prev, s) {
			prev.end = s.end
			prev.toUnknown = s.toUnknown
			prev.boundary = append(prev.boundary, s.boundary...)
			if prev.versionID == "" {
				prev.versionID = s.versionID
			}
			// What the merged-away segment rested on is remembered rather than dropped — but
			// only the *first* restatement per source, never the latest. See rememberRestatement.
			for _, ids := range []map[string]string{s.assertions, s.restatements} {
				for sourceID, id := range ids {
					prev.rememberRestatement(sourceID, id)
				}
			}
			continue
		}
		out = append(out, s)
	}
	return out
}

// assertedAtOrAfter returns the segment with only the assertions made at or after the retraction
// instant, in valid time. An empty assertion set means nothing about it survived the retraction.
//
// "At" counts, and that is deliberate. The valid intervals are half-open, so a fact that ends
// at T and a fact that begins at T sit side by side without overlapping; a source re-asserting
// at exactly the instant another (or itself) retracted is saying "it came back then", and the
// planner has no business preferring whichever of the two events the log happened to receive
// second.
//
// Filtering per source rather than keeping or dropping the segment whole is what makes a
// two-source timeline order-independent: if one source asserted before the retraction and
// another after it, the interval after the retraction rests on the second source alone, whether
// the retraction was delivered before the second assertion or after it.
func assertedAtOrAfter(s segment, validEnd time.Time, assertedAt func(string) time.Time) segment {
	out := s.clone()
	out.assertions = map[string]string{}
	for sourceID, eventID := range s.assertions {
		if !assertedAt(eventID).Before(validEnd) {
			out.assertions[sourceID] = eventID
		}
	}
	for sourceID, eventID := range s.restatements {
		if _, kept := out.assertions[sourceID]; !kept && !assertedAt(eventID).Before(validEnd) {
			// The contributing assertion was retracted but a restatement of it was not; the
			// restatement is what the interval now rests on.
			out.assertions[sourceID] = eventID
		}
	}
	out.restatements = nil
	if len(out.assertions) != len(s.assertions) {
		// The segment is not the row that is stored any more.
		out.versionID = ""
	}
	return out
}

// ---------- properties ----------

// propRecord is one asserted property value with its provenance. This is the shape stored in
// graph.entity_versions.props and graph.edge_versions.props (data-model.md):
//
//	{"service.version": {"value": "1.4.2", "source_id": "otel:demo", "event_id": "otel:demo:…"}}
//
// and, when sources disagree about a key, the key holds the list of records instead and the
// key is named in `conflicts`. The projector never picks a winner (edge case "conflicting
// sources").
type propRecord struct {
	Value    any    `json:"value"`
	SourceID string `json:"source_id"`
	EventID  string `json:"event_id"`
}

// propSet is a whole props column: one entry per key, one record per source that asserted it,
// ordered by source id.
type propSet map[string][]propRecord

// conflicts returns the keys whose records disagree, sorted. Two sources asserting the same
// value for a key is corroboration, not a conflict; both records are kept either way.
func (p propSet) conflicts() []string {
	keys := []string{}
	for key, records := range p {
		if len(records) < 2 {
			continue
		}
		first, err := json.Marshal(records[0].Value)
		if err != nil {
			continue
		}
		for _, record := range records[1:] {
			other, err := json.Marshal(record.Value)
			if err != nil || string(other) != string(first) {
				keys = append(keys, key)
				break
			}
		}
	}
	slices.Sort(keys)
	return keys
}

// MarshalJSON writes the published shape: a bare record when only one source asserted the key,
// a list when several did.
func (p propSet) MarshalJSON() ([]byte, error) {
	out := make(map[string]json.RawMessage, len(p))
	for key, records := range p {
		var (
			raw []byte
			err error
		)
		if len(records) == 1 {
			raw, err = json.Marshal(records[0])
		} else {
			raw, err = json.Marshal(records)
		}
		if err != nil {
			return nil, fmt.Errorf("projector: encode prop %q: %w", key, err)
		}
		out[key] = raw
	}
	return json.Marshal(out)
}

// UnmarshalJSON accepts both shapes MarshalJSON writes.
func (p *propSet) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("projector: decode props: %w", err)
	}
	out := make(propSet, len(raw))
	for key, value := range raw {
		var records []propRecord
		if err := json.Unmarshal(value, &records); err == nil {
			out[key] = records
			continue
		}
		var single propRecord
		if err := json.Unmarshal(value, &single); err != nil {
			return fmt.Errorf("projector: decode prop %q: %w", key, err)
		}
		out[key] = []propRecord{single}
	}
	*p = out
	return nil
}

// valuesEqual compares two property sets by what they assert and who asserted it, ignoring
// which event carried it. Two events from one source re-stating the same fact are the same
// fact: without this a feeder re-emitting an unchanged node every window would cut the
// valid timeline into one segment per window (FR-021, "no-op when nothing differs").
func (p propSet) valuesEqual(other propSet) bool {
	if len(p) != len(other) {
		return false
	}
	for key, records := range p {
		otherRecords, ok := other[key]
		if !ok || len(records) != len(otherRecords) {
			return false
		}
		for i, record := range records {
			if record.SourceID != otherRecords[i].SourceID {
				return false
			}
			a, errA := json.Marshal(record.Value)
			b, errB := json.Marshal(otherRecords[i].Value)
			if errA != nil || errB != nil || string(a) != string(b) {
				return false
			}
		}
	}
	return true
}

// buildProps folds each source's contributing assertion into one property set. Records are
// ordered by source id so the stored JSON is canonical.
func buildProps(sources []string, propsOf func(sourceID string) (map[string]*structpb.Value, string, string)) propSet {
	out := propSet{}
	ordered := slices.Clone(sources)
	slices.Sort(ordered)
	for _, sourceID := range ordered {
		props, source, eventID := propsOf(sourceID)
		for _, key := range sortedKeys(props) {
			out[key] = append(out[key], propRecord{
				Value:    props[key].AsInterface(),
				SourceID: source,
				EventID:  eventID,
			})
		}
	}
	return out
}

// keepLatestPerSource leaves one record per source for each property. Two records from one source
// come from its two lanes (a definition and an alert transition, alert_transition.go) naming the
// same key, and they are not a disagreement between sources: the later assertion is what the
// source says now. Ties go to the greater event id, so the result does not depend on arrival order.
func (ps propSet) keepLatestPerSource(assertedAt func(eventID string) time.Time) {
	for key, records := range ps {
		kept := records[:0]
		for _, record := range records {
			replaced := false
			for i, other := range kept {
				if other.SourceID != record.SourceID {
					continue
				}
				replaced = true
				if c := assertedAt(record.EventID).Compare(assertedAt(other.EventID)); c > 0 || (c == 0 && record.EventID > other.EventID) {
					kept[i] = record
				}
			}
			if !replaced {
				kept = append(kept, record)
			}
		}
		ps[key] = kept
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// ---------- node types ----------

// typePrecedence is the published order used to resolve the type of an entity that several
// sources typed differently, highest first (research §10, ADR-0001 D7). A Kubernetes workload
// and the OpenTelemetry service running on it are one entity; it is a SERVICE, and WORKLOAD
// stays in its facets.
var typePrecedence = []graph.NodeType{
	graph.NodeTypeService,
	graph.NodeTypeWorkload,
	graph.NodeTypeThirdParty,
	graph.NodeTypeInfraResource,
	graph.NodeTypeDBSchema,
	graph.NodeTypeConfig,
	graph.NodeTypeFeatureFlag,
	graph.NodeTypeOwner,
	graph.NodeTypeAlert,
	graph.NodeTypeChange,
	// The feature 001 change package for 002 (ADR-0005 D3). Both go at the bottom, and the
	// order between them barely matters, because neither is a type any other source will ever
	// also assert: an investigation is named by the engine that ran it and a knowledge
	// document by the connector that indexed it, so the precedence is only ever consulted to
	// answer "what is this entity" for an entity with exactly one facet. What matters is that
	// they are in the list at all — an entity whose facets are all missing from it resolves to
	// no type at all, and `graph.entities.type` is NOT NULL.
	graph.NodeTypeInvestigation,
	graph.NodeTypeKnowledgeDoc,
}

// resolveType returns the type of an entity whose sources asserted the given facets.
func resolveType(facets []graph.NodeType) graph.NodeType {
	for _, candidate := range typePrecedence {
		if slices.Contains(facets, candidate) {
			return candidate
		}
	}
	return graph.NodeTypeUnspecified
}

// orderFacets deduplicates and sorts facets by the published precedence, so the stored array
// is canonical and `type` is simply its first element.
func orderFacets(facets []graph.NodeType) []graph.NodeType {
	out := make([]graph.NodeType, 0, len(facets))
	for _, candidate := range typePrecedence {
		if slices.Contains(facets, candidate) && !slices.Contains(out, candidate) {
			out = append(out, candidate)
		}
	}
	return out
}

func facetStrings(facets []graph.NodeType) []string {
	out := make([]string, 0, len(facets))
	for _, f := range facets {
		out = append(out, string(f))
	}
	return out
}

func facetsFromStrings(values []string) []graph.NodeType {
	out := make([]graph.NodeType, 0, len(values))
	for _, v := range values {
		out = append(out, graph.NodeType(v))
	}
	return orderFacets(out)
}

// ---------- pointers ----------

// pointersJSON renders a pointer list as the stored jsonb array, canonical so that two runs
// write the same bytes (FR-023).
func pointersJSON(pointers []*graphv1.Pointer) ([]byte, error) {
	if len(pointers) == 0 {
		return []byte("[]"), nil
	}
	raws := make([]json.RawMessage, 0, len(pointers))
	for _, pointer := range pointers {
		raw, err := graph.CanonicalJSON(pointer)
		if err != nil {
			return nil, fmt.Errorf("projector: encode pointer: %w", err)
		}
		raws = append(raws, raw)
	}
	return json.Marshal(raws)
}

func samePointers(a, b []*graphv1.Pointer) bool {
	if len(a) != len(b) {
		return false
	}
	rawA, errA := pointersJSON(a)
	rawB, errB := pointersJSON(b)
	return errA == nil && errB == nil && string(rawA) == string(rawB)
}

// pickPrimary returns the source whose assertion supplies the fields a version holds only one
// of — display name, pointers, weight class. The winner is the latest assertion by valid time,
// with source id and event id as deterministic tie-breaks, so the answer does not depend on
// arrival order.
func pickPrimary(sources []string, at func(sourceID string) (time.Time, string)) string {
	best := ""
	var bestAt time.Time
	var bestEvent string
	for _, sourceID := range sources {
		when, eventID := at(sourceID)
		if best == "" || cmpAssertion(when, sourceID, eventID, bestAt, best, bestEvent) > 0 {
			best, bestAt, bestEvent = sourceID, when, eventID
		}
	}
	return best
}

func cmpAssertion(aAt time.Time, aSource, aEvent string, bAt time.Time, bSource, bEvent string) int {
	if c := aAt.Compare(bAt); c != 0 {
		return c
	}
	if c := cmp.Compare(aSource, bSource); c != 0 {
		return c
	}
	return cmp.Compare(aEvent, bEvent)
}
