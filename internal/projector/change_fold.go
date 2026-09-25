// SPDX-License-Identifier: Apache-2.0

package projector

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
)

// Folding several observations of one change into one record (004 T152).
//
// ---------------------------------------------------------------------------------------------
// Why a change record is a fold and not the last observation
//
// A change used to be stored as whatever its latest observe_change said: writeRecordVersion closed
// the current row and wrote the new one. With one source per change that is a correction and it
// is right. C8 made it wrong. When C8 decides that a GitHub deployment and a Cloud Run rollout are
// the same production rollout, the two change entities become one, and "the latest observation"
// is then whichever source happened to be applied last — the GitHub half's actor and commit or
// the Cloud Run half's revision, never both, and which one depends on arrival order.
//
// It was worse than that on the merge itself. absorb re-folds a merged entity's facts from the
// NODE assertions that produced them, and a change has none: its rows come from observe_change.
// So the survivor of a C8 merge was rewritten with no summary, no kind and no change body at all,
// and `diff` — which skips a version with no change — stopped reporting the production rollout
// on the service it rolled out to. `deploy-cross-source-merge-01` asked "what changed on
// storefront?" and the answer held the canary and not the rollout the fixture is about.
//
// # The fold
//
// Every observation attributed to the entity is read back from the log, and:
//
//   - a source re-observing the same change (same source, same ref) is a correction: only its
//     latest statement counts, by when the source learned it and then by event id;
//   - the PRIMARY observation is the one with a known start, then the earliest start, then source
//     and event id. It decides the valid interval — the instant the change happened — because a
//     stated instant is evidence and an unknown one is a placeholder, and of two stated instants
//     for one rollout the earlier is when production first moved;
//   - the change body is the primary's, with every field it leaves unset filled from the others in
//     the same order. The GitHub half names the actor and the Cloud Run half the revision, and an
//     investigation wants both;
//   - properties keep one record per source, as every other node's do, so two sources disagreeing
//     about a key is surfaced as a conflict rather than resolved by arrival order;
//   - pointers are unioned: two sources naming two places to look are both right;
//   - the unattached-target list is recomputed from what resolves now, across every observation's
//     targets, so a merge never resurrects a target that has since been attached.
//
// Every rule above is a function of the SET of observations, never of the order they arrived in,
// which is what a fixture's shuffle step checks (FR-021). A change observed once is stored exactly
// as it was before this file existed.

// changeObservation is one observe_change event, read back from the log or being applied.
type changeObservation struct {
	eventID  string
	sourceID string
	ref      graph.Ref
	body     *graphv1.ObserveChange
	// learnedAt orders two statements of the same change by the same source: when the source
	// learned it where the feeder said, the ingest instant otherwise.
	learnedAt   time.Time
	validAt     time.Time
	validEnd    time.Time
	fromUnknown bool
}

func newChangeObservation(eventID, sourceID string, body *graphv1.ObserveChange, sourceObservedAt *time.Time, observedAt time.Time) changeObservation {
	obs := changeObservation{
		eventID:   eventID,
		sourceID:  sourceID,
		ref:       graph.RefFromProto(body.GetRef()),
		body:      body,
		learnedAt: observedAt,
	}
	if sourceObservedAt != nil {
		obs.learnedAt = sourceObservedAt.UTC()
	}
	obs.validAt, obs.validEnd, obs.fromUnknown = changeInterval(body, sourceObservedAt, observedAt)
	return obs
}

// changeInterval is when a change happened, as applyObserveChange documents: the stated instant,
// or the instant the source learned of it marked unknown, over the shortest storable interval unless
// an end was stated.
func changeInterval(body *graphv1.ObserveChange, sourceObservedAt *time.Time, observedAt time.Time) (validAt, validEnd time.Time, fromUnknown bool) {
	fromUnknown = body.GetValidFromUnknown()
	validAt = body.GetValidAt().AsTime().UTC()
	if ts := body.GetValidAt(); ts == nil || fromUnknown {
		fromUnknown = true
		validAt = observedAt
		if sourceObservedAt != nil {
			validAt = sourceObservedAt.UTC()
		}
	}
	validEnd = validAt.Add(changeInstant)
	if ts := body.GetValidEnd(); ts != nil && ts.AsTime().After(validAt) {
		validEnd = ts.AsTime().UTC()
	}
	return validAt, validEnd, fromUnknown
}

// changeObservations reads back the observe_change events among eventIDs. Other event types —
// the attach event a dropped unattached target lists, for instance — shaped a version without
// asserting what the change was, and are skipped.
func (p *Projector) changeObservations(ctx context.Context, tx pgx.Tx, eventIDs []string) ([]changeObservation, error) {
	if len(eventIDs) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `
		SELECT event_id, source_id, source_observed_at, observed_at, payload
		FROM log.events
		WHERE event_id = ANY($1) AND type = 'observe_change'`, eventIDs)
	if err != nil {
		return nil, fmt.Errorf("projector: read change observations: %w", err)
	}
	defer rows.Close()

	var out []changeObservation
	for rows.Next() {
		var (
			eventID, sourceID string
			sourceObservedAt  *time.Time
			observedAt        time.Time
			payload           []byte
		)
		if err := rows.Scan(&eventID, &sourceID, &sourceObservedAt, &observedAt, &payload); err != nil {
			return nil, fmt.Errorf("projector: scan change observation: %w", err)
		}
		body := &graphv1.ObserveChange{}
		if err := protojson.Unmarshal(payload, body); err != nil {
			return nil, fmt.Errorf("projector: decode change observation %s: %w", eventID, err)
		}
		out = append(out, newChangeObservation(eventID, sourceID, body, sourceObservedAt, observedAt.UTC()))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("projector: read change observations: %w", err)
	}
	return out, nil
}

// latestPerStatement keeps one observation per (source, ref): a source saying something new about
// a change it already reported is correcting itself. The result is ordered primary first.
func latestPerStatement(observations []changeObservation) []changeObservation {
	latest := map[string]changeObservation{}
	for _, obs := range observations {
		key := statementKey(obs)
		held, ok := latest[key]
		if !ok || obs.learnedAt.After(held.learnedAt) ||
			(obs.learnedAt.Equal(held.learnedAt) && obs.eventID > held.eventID) {
			latest[key] = obs
		}
	}
	out := make([]changeObservation, 0, len(latest))
	for _, obs := range latest {
		out = append(out, obs)
	}
	slices.SortFunc(out, comparePrimary)
	return out
}

func statementKey(obs changeObservation) string {
	return obs.sourceID + "\x00" + refString(obs.ref)
}

// comparePrimary orders observations so the first is the one whose instant the record takes: a
// stated start before an unknown one, then the earliest, then source and event id.
func comparePrimary(a, b changeObservation) int {
	if a.fromUnknown != b.fromUnknown {
		if a.fromUnknown {
			return 1
		}
		return -1
	}
	if c := a.validAt.Compare(b.validAt); c != 0 {
		return c
	}
	return cmp.Or(cmp.Compare(a.sourceID, b.sourceID), cmp.Compare(a.eventID, b.eventID))
}

// foldedChange is what one change record holds once every statement about it is folded.
type foldedChange struct {
	validAt     time.Time
	validEnd    time.Time
	fromUnknown bool
	content     nodeContent
	// statements are the observations folded, primary first; their event ids are the version's
	// provenance and the next fold's input.
	statements []changeObservation
}

func (f foldedChange) eventIDs() []string {
	ids := make([]string, 0, len(f.statements))
	for _, obs := range f.statements {
		ids = append(ids, obs.eventID)
	}
	slices.Sort(ids)
	return ids
}

// foldChange folds the observations of one change entity. It reads the graph only to ask which of
// the change's targets resolve now.
func (p *Projector) foldChange(ctx context.Context, tx pgx.Tx, observations []changeObservation, facets []graph.NodeType) (foldedChange, error) {
	statements := latestPerStatement(observations)
	primary := statements[0]

	change := proto.Clone(primary.body.GetChange()).(*graphv1.Change)
	if change == nil {
		change = &graphv1.Change{}
	}
	for _, obs := range statements[1:] {
		fillUnset(change, obs.body.GetChange())
	}
	changeJSON, err := graph.CanonicalJSON(change)
	if err != nil {
		return foldedChange{}, fmt.Errorf("projector: encode change %s: %w", primary.eventID, err)
	}

	var unattached []string
	props := propSet{}
	pointerLists := make([][]*graphv1.Pointer, 0, len(statements))
	for _, obs := range statements {
		for _, target := range obs.body.GetTargets() {
			targetRef := graph.RefFromProto(target)
			_, found, err := p.lookupRef(ctx, tx, targetRef)
			if err != nil {
				return foldedChange{}, err
			}
			if !found {
				unattached = append(unattached, refString(targetRef))
			}
		}
		for key, records := range changeProps(nil, obs.body.GetProps(), obs.sourceID, obs.eventID) {
			props[key] = append(props[key], records...)
		}
		pointerLists = append(pointerLists, obs.body.GetPointers())
	}
	for key := range props {
		slices.SortFunc(props[key], func(a, b propRecord) int {
			return cmp.Or(cmp.Compare(a.SourceID, b.SourceID), cmp.Compare(a.EventID, b.EventID))
		})
	}
	slices.Sort(unattached)
	unattached = slices.Compact(unattached)
	for key, records := range changeProps(unattached, nil, primary.sourceID, primary.eventID) {
		props[key] = records
	}

	// One statement keeps its pointers exactly as the source listed them, so a change observed once
	// is stored byte for byte as it always was; several are unioned and put in canonical order.
	pointers := primary.body.GetPointers()
	if len(statements) > 1 {
		pointers = unionPointers(pointerLists)
	}

	return foldedChange{
		validAt:     primary.validAt,
		validEnd:    primary.validEnd,
		fromUnknown: primary.fromUnknown,
		content: nodeContent{
			displayName: change.GetSummary(),
			props:       props,
			conflicts:   props.conflicts(),
			pointers:    pointers,
			facets:      facets,
			change:      changeJSON,
		},
		statements: statements,
	}, nil
}

// fillUnset copies into dst every field src sets and dst does not. A field is taken whole, so a
// source that names an actor also supplies the actor's kind rather than one source's name being
// paired with another's reading of it — unless dst already had a kind, in which case the pairing
// is dst's own.
func fillUnset(dst, src *graphv1.Change) {
	if src == nil {
		return
	}
	to := dst.ProtoReflect()
	src.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if !to.Has(fd) {
			to.Set(fd, v)
		}
		return true
	})
}

// writeChangeRecord stores a change entity's record as the fold of every statement attributed to
// it: the ones behind its current rows (and, on a merge, the absorbed entity's), plus any being
// applied now.
//
// It is a no-op when the current row already holds the fold AND already cites a statement from
// each (source, ref) folded. The second half is what keeps a re-poll quiet — a feeder reporting the
// same deployment again changes nothing — while still recording a new source whose statement
// happens to add nothing, because the next fold reads its inputs from this row's provenance and a
// source it cannot see is a source it would drop.
func (p *Projector) writeChangeRecord(ctx context.Context, tx pgx.Tx, entityID string, current []*nodeRow, observations []changeObservation, facets []graph.NodeType, eventID string, observedAt time.Time) error {
	if len(observations) == 0 {
		return nil
	}
	fold, err := p.foldChange(ctx, tx, observations, facets)
	if err != nil {
		return err
	}
	for _, row := range current {
		if row.valid.Start.Equal(fold.validAt) && !row.valid.EndUnbounded && row.valid.End.Equal(fold.validEnd) &&
			row.fromUnknown == fold.fromUnknown && fold.content.equals(row) &&
			citesEveryStatement(row, observations, fold.statements) {
			return nil
		}
	}
	for _, row := range current {
		if err := closeObserved(ctx, tx, "entity_versions", row.versionID, observedAt, eventID); err != nil {
			return err
		}
	}
	seg := segment{
		start:       fold.validAt,
		end:         fold.validEnd,
		fromUnknown: fold.fromUnknown,
		boundary:    fold.eventIDs(),
	}
	return p.insertNodeVersion(ctx, tx, entityID, seg, fold.content, fold.versionKey(eventID), observedAt)
}

// versionKey is what the version id is derived from in place of the bare event id.
//
// A change observed once keeps the id it always had. A fold of several needs more: one event can
// write a change record twice at the same valid instant — attach.go drops a now-resolved target
// from it, and the C8 merge that attachment re-triggers then folds it — and the second write would
// otherwise reuse the first's id. The folded statements are what differ between the two, so they
// go into the id; it stays a pure function of logged values, so a replay reproduces it (FR-023).
func (f foldedChange) versionKey(eventID string) string {
	if len(f.statements) < 2 {
		return eventID
	}
	return eventID + "\x00" + strings.Join(f.eventIDs(), "\x00")
}

// citesEveryStatement reports whether row's provenance already names, for every (source, ref)
// folded, some observation of it.
func citesEveryStatement(row *nodeRow, observations []changeObservation, statements []changeObservation) bool {
	cited := map[string]bool{}
	for _, obs := range observations {
		if slices.Contains(row.producedBy, obs.eventID) {
			cited[statementKey(obs)] = true
		}
	}
	for _, obs := range statements {
		if !cited[statementKey(obs)] {
			return false
		}
	}
	return true
}

// hasChangeRecord reports whether any of rows is a change record rather than a segmented node
// version: the rows absorb has to re-fold from observations rather than from node assertions.
func hasChangeRecord(rows ...[]*nodeRow) bool {
	for _, list := range rows {
		for _, row := range list {
			if len(row.change) > 0 {
				return true
			}
		}
	}
	return false
}
