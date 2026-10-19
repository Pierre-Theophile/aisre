// SPDX-License-Identifier: Apache-2.0

package projector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	eventlog "github.com/Pierre-Theophile/aisre/internal/log"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
)

// Attributions (feature 009, ADR-0015 items 4 and 5, research R4, R8).
//
// The attributor decides; the projector only records. An attribute_signal event carries the
// decision the attributor took, outside the projector, so a replay projects the same edges without
// running a rule or a model (constitution III). What this file does with each event:
//
//	attribute_signal     closes the signal's open ATTRIBUTED_TO edges and opens one per entry
//	                     decided. An `unknown` decision opens none. Under a standing correction it
//	                     changes nothing and is recorded, as a `conflict` when it disagrees (FR-042).
//	correct_attribution  the same, with layer `correction`, rule H and the principal on the edge. A
//	                     name-level correction also makes the name an alias of the entry.
//	revert_correction    closes what the correction opened. The attributor re-attributes next.
//
// ATTRIBUTED_TO edges are written here and only here, one version per decision: valid from the
// signal's own valid start, observed from the decision. A superseding decision closes the observed
// interval, which is what makes "what did we believe this alert was about, and when" answerable.
// They are not asserted through applyUpsertEdge because an attribution is not a source's statement
// about the world that later statements fold with: it is the latest judgement, whole. Merges carry
// these rows to the canonical endpoints unchanged, as they do every row no assertion stands behind
// (refs.go, carryEdges).

// Decision kinds, as graph.attribution_decisions stores them.
const (
	decisionAttribute = "attribute"
	decisionCorrect   = "correct"
	decisionRevert    = "revert"
	decisionConflict  = "conflict"
)

// RuleCorrection is the rule id of layer 0 (ADR-0015 item 5).
const RuleCorrection = "H"

// decisionEntry is one entry of a decision, as stored in the entries and suggestions columns.
type decisionEntry struct {
	EntrySlug string  `json:"entry_slug"`
	Layer     string  `json:"layer"`
	RuleID    string  `json:"rule_id"`
	Score     float64 `json:"score"`
	Rationale string  `json:"rationale,omitempty"`
}

func decisionEntries(items []*graphv1.Attributed) []decisionEntry {
	out := make([]decisionEntry, 0, len(items))
	for _, item := range items {
		out = append(out, decisionEntry{
			EntrySlug: item.GetEntrySlug(),
			Layer:     item.GetLayer(),
			RuleID:    item.GetRuleId(),
			Score:     item.GetScore(),
			Rationale: item.GetRationale(),
		})
	}
	return out
}

func entrySlugs(entries []decisionEntry) []string {
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		out = append(out, entry.EntrySlug)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// attributionRecord is one graph.attribution_decisions row about to be written.
type attributionRecord struct {
	eventID      string
	kind         string
	signalID     string
	signalRef    graph.Ref
	entries      []decisionEntry
	suggestions  []decisionEntry
	ruleset      string
	evidence     []byte
	scope        string
	principal    string
	supersededBy string
	revertedBy   string
}

func (p *Projector) applyAttributeSignal(ctx context.Context, tx pgx.Tx, env *graphv1.EventEnvelope,
	body *graphv1.AttributeSignal, observedAt time.Time,
) error {
	signalRef := graph.RefFromProto(body.GetSignal())
	signalID, found, err := p.lookupRef(ctx, tx, signalRef)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("projector: attribute_signal %s: %s does not resolve", env.GetEventId(), signalRef)
	}
	evidence, err := marshalEvidence(body.GetEvidence())
	if err != nil {
		return fmt.Errorf("projector: attribute_signal %s: %w", env.GetEventId(), err)
	}
	record := attributionRecord{
		eventID:     env.GetEventId(),
		kind:        decisionAttribute,
		signalID:    signalID,
		signalRef:   signalRef,
		entries:     decisionEntries(body.GetEntries()),
		suggestions: decisionEntries(body.GetSuggestions()),
		ruleset:     body.GetRuleset(),
		evidence:    evidence,
	}

	correction, corrected, err := p.standingCorrection(ctx, tx, signalID)
	if err != nil {
		return err
	}
	if corrected {
		// A person's decision outranks any automated one, whatever its score (FR-042). Agreeing
		// with it adds nothing; disagreeing is shown, never applied.
		if slices.Equal(entrySlugs(record.entries), entrySlugs(correction.entries)) {
			record.supersededBy = correction.eventID
		} else {
			record.kind = decisionConflict
		}
		return p.insertAttribution(ctx, tx, record, observedAt)
	}

	if err := p.supersedeAttributions(ctx, tx, signalID, env.GetEventId()); err != nil {
		return err
	}
	if err := p.writeAttribution(ctx, tx, env, signalID, record.entries, "", observedAt); err != nil {
		return err
	}
	return p.insertAttribution(ctx, tx, record, observedAt)
}

func (p *Projector) applyCorrectAttribution(ctx context.Context, tx pgx.Tx, env *graphv1.EventEnvelope,
	body *graphv1.CorrectAttribution, observedAt time.Time, principal string,
) error {
	signalRef := graph.RefFromProto(body.GetSignal())
	signalID, found, err := p.lookupRef(ctx, tx, signalRef)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("projector: correct_attribution %s: %s does not resolve", env.GetEventId(), signalRef)
	}
	entries := make([]decisionEntry, 0, len(body.GetEntrySlugs()))
	for _, slug := range body.GetEntrySlugs() {
		entries = append(entries, decisionEntry{
			EntrySlug: slug,
			Layer:     graph.AttributionLayerCorrection,
			RuleID:    RuleCorrection,
			Score:     1,
			Rationale: body.GetRationale(),
		})
	}

	if err := p.supersedeAttributions(ctx, tx, signalID, env.GetEventId()); err != nil {
		return err
	}
	if err := p.writeAttribution(ctx, tx, env, signalID, entries, principal, observedAt); err != nil {
		return err
	}
	if err := p.insertAttribution(ctx, tx, attributionRecord{
		eventID:   env.GetEventId(),
		kind:      decisionCorrect,
		signalID:  signalID,
		signalRef: signalRef,
		entries:   entries,
		evidence:  []byte("{}"),
		scope:     body.GetScope(),
		principal: principal,
	}, observedAt); err != nil {
		return err
	}

	if body.GetScope() != graph.CorrectionScopeName {
		return nil
	}
	// A name-level correction is also an alias decided by this person: the next signal carrying the
	// name is attributed by layer 1 without anyone correcting it again (FR-043).
	return p.openAlias(ctx, tx, aliasRow{
		name:        graph.RefFromProto(body.GetName()),
		environment: body.GetEnvironment(),
		entrySlug:   body.GetEntrySlugs()[0],
		principal:   principal,
		eventID:     env.GetEventId(),
	}, observedAt, true)
}

func (p *Projector) applyRevertCorrection(ctx context.Context, tx pgx.Tx, env *graphv1.EventEnvelope,
	body *graphv1.RevertCorrection, observedAt time.Time, principal string,
) error {
	correction, found, err := p.attributionDecision(ctx, tx, body.GetCorrectionEventId())
	if err != nil {
		return err
	}
	if !found || correction.kind != decisionCorrect {
		return fmt.Errorf("projector: revert_correction %s: %s is not a correction",
			env.GetEventId(), body.GetCorrectionEventId())
	}
	if _, err := tx.Exec(ctx, `
		UPDATE graph.attribution_decisions SET reverted_by_event_id = $2
		WHERE event_id = $1 AND reverted_by_event_id IS NULL`,
		correction.eventID, env.GetEventId()); err != nil {
		return fmt.Errorf("projector: revert correction %s: %w", correction.eventID, err)
	}

	// Close what the correction opened, and only that: a later decision's edges are its own.
	rows, err := tx.Query(ctx, `
		SELECT version_id FROM graph.edge_versions
		WHERE type = $1 AND upper_inf(observed) AND $2 = ANY(produced_by_event_ids)
		ORDER BY version_id`, string(graph.EdgeTypeAttributedTo), correction.eventID)
	if err != nil {
		return fmt.Errorf("projector: read edges of correction %s: %w", correction.eventID, err)
	}
	versions, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("projector: read edges of correction %s: %w", correction.eventID, err)
	}
	for _, versionID := range versions {
		if err := closeObserved(ctx, tx, "edge_versions", versionID, observedAt, env.GetEventId()); err != nil {
			return err
		}
	}

	var aliasVersion string
	err = tx.QueryRow(ctx, `
		SELECT version_id FROM graph.catalog_alias_versions
		WHERE decided_by_event_id = $1 AND upper_inf(observed)`, correction.eventID).Scan(&aliasVersion)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return fmt.Errorf("projector: read alias of correction %s: %w", correction.eventID, err)
	default:
		if err := closeObserved(ctx, tx, "catalog_alias_versions", aliasVersion, observedAt, env.GetEventId()); err != nil {
			return err
		}
	}

	// The evidence names the correction undone: a later correction of the same signal still stands.
	evidence, err := json.Marshal(map[string]string{graph.RevertedCorrectionEvidence: correction.eventID})
	if err != nil {
		return fmt.Errorf("projector: encode revert of %s: %w", correction.eventID, err)
	}
	return p.insertAttribution(ctx, tx, attributionRecord{
		eventID:   env.GetEventId(),
		kind:      decisionRevert,
		signalID:  correction.signalID,
		signalRef: correction.signalRef,
		evidence:  evidence,
		scope:     correction.scope,
		principal: principal,
	}, observedAt)
}

// writeAttribution makes entries the signal's current attributions: every open ATTRIBUTED_TO edge
// from it is closed, and one opened per entry.
func (p *Projector) writeAttribution(ctx context.Context, tx pgx.Tx, env *graphv1.EventEnvelope,
	signalID string, entries []decisionEntry, principal string, observedAt time.Time,
) error {
	rows, err := tx.Query(ctx, `
		SELECT version_id FROM graph.edge_versions
		WHERE src_id = $1 AND type = $2 AND upper_inf(observed)
		ORDER BY version_id`, signalID, string(graph.EdgeTypeAttributedTo))
	if err != nil {
		return fmt.Errorf("projector: read attributions of %s: %w", signalID, err)
	}
	open, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("projector: read attributions of %s: %w", signalID, err)
	}
	for _, versionID := range open {
		if err := closeObserved(ctx, tx, "edge_versions", versionID, observedAt, env.GetEventId()); err != nil {
			return err
		}
	}
	if len(entries) == 0 {
		return nil
	}

	validStart, err := p.signalValidStart(ctx, tx, signalID, observedAt)
	if err != nil {
		return err
	}
	ruleset := env.GetAttributeSignal().GetRuleset()
	written := map[string]bool{}
	for _, entry := range entries {
		dstID, found, err := p.lookupRef(ctx, tx, graph.CatalogRef(entry.EntrySlug))
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("projector: %s attributes %s to %q, which does not exist",
				env.GetEventId(), signalID, entry.EntrySlug)
		}
		if dstID == signalID || written[dstID] {
			// Two slugs merged into one entry are one attribution.
			continue
		}
		written[dstID] = true

		record := func(value any) []propRecord {
			return []propRecord{{Value: value, SourceID: env.GetSourceId(), EventID: env.GetEventId()}}
		}
		props := propSet{
			graph.AttributionLayerProp:    record(entry.Layer),
			graph.AttributionRuleProp:     record(entry.RuleID),
			graph.AttributionScoreProp:    record(entry.Score),
			graph.AttributionDecisionProp: record(env.GetEventId()),
		}
		if ruleset != "" {
			props[graph.AttributionRulesetProp] = record(ruleset)
		}
		if principal != "" {
			props[graph.AttributionPrincipalProp] = record(principal)
		}
		encoded, err := json.Marshal(props)
		if err != nil {
			return fmt.Errorf("projector: encode attribution props: %w", err)
		}
		typ := string(graph.EdgeTypeAttributedTo)
		if _, err := tx.Exec(ctx, `
			INSERT INTO graph.edge_versions (
				version_id, src_id, dst_id, type, valid, observed, props, produced_by_event_ids)
			VALUES ($1, $2, $3, $4, $5, $6, $7, ARRAY[$8]::text[])`,
			graph.EdgeSegmentVersionID(signalID, dstID, typ, env.GetEventId(), validStart),
			signalID, dstID, typ, postgres.OpenTimeRange(validStart), postgres.OpenTimeRange(observedAt.UTC()),
			encoded, env.GetEventId()); err != nil {
			return fmt.Errorf("projector: insert attribution %s->%s: %w", signalID, dstID, err)
		}
	}
	return nil
}

// signalValidStart is where an attribution's valid interval starts: the signal's own valid start,
// so an as-of read inside the alert's life sees the attribution once it was decided.
func (p *Projector) signalValidStart(ctx context.Context, tx pgx.Tx, signalID string, observedAt time.Time) (time.Time, error) {
	var start *time.Time
	if err := tx.QueryRow(ctx, `
		SELECT min(lower(valid)) FROM graph.entity_versions
		WHERE entity_id = $1 AND upper_inf(observed)`, signalID).Scan(&start); err != nil {
		return time.Time{}, fmt.Errorf("projector: read valid start of %s: %w", signalID, err)
	}
	if start == nil {
		return observedAt.UTC(), nil
	}
	return start.UTC(), nil
}

// supersedeAttributions marks the signal's current decisions as replaced by eventID.
func (p *Projector) supersedeAttributions(ctx context.Context, tx pgx.Tx, signalID, eventID string) error {
	if _, err := tx.Exec(ctx, `
		UPDATE graph.attribution_decisions SET superseded_by_event_id = $2
		WHERE signal_entity_id = $1 AND kind IN ('attribute', 'correct')
		  AND superseded_by_event_id IS NULL AND reverted_by_event_id IS NULL`,
		signalID, eventID); err != nil {
		return fmt.Errorf("projector: supersede decisions on %s: %w", signalID, err)
	}
	return nil
}

func (p *Projector) insertAttribution(ctx context.Context, tx pgx.Tx, record attributionRecord, observedAt time.Time) error {
	entries, err := json.Marshal(record.entries)
	if err != nil {
		return fmt.Errorf("projector: encode decision entries: %w", err)
	}
	if record.suggestions == nil {
		record.suggestions = []decisionEntry{}
	}
	suggestions, err := json.Marshal(record.suggestions)
	if err != nil {
		return fmt.Errorf("projector: encode decision suggestions: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO graph.attribution_decisions
			(event_id, kind, signal_entity_id, signal_ref, entries, suggestions, ruleset, evidence,
			 scope, principal, decided_at, superseded_by_event_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		record.eventID, record.kind, record.signalID, record.signalRef.String(), entries, suggestions,
		record.ruleset, record.evidence, record.scope, nullableString(record.principal), observedAt.UTC(),
		nullableString(record.supersededBy)); err != nil {
		return fmt.Errorf("projector: record attribution decision %s: %w", record.eventID, err)
	}
	return nil
}

// attributionDecision reads one attribution decision back.
func (p *Projector) attributionDecision(ctx context.Context, tx pgx.Tx, eventID string) (attributionRecord, bool, error) {
	var (
		record    attributionRecord
		signalRef string
		entries   []byte
		reverted  *string
	)
	err := tx.QueryRow(ctx, `
		SELECT event_id, kind, signal_entity_id, signal_ref, entries, scope, reverted_by_event_id
		FROM graph.attribution_decisions WHERE event_id = $1`, eventID).Scan(
		&record.eventID, &record.kind, &record.signalID, &signalRef, &entries, &record.scope, &reverted)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return record, false, nil
	case err != nil:
		return record, false, fmt.Errorf("projector: read attribution decision %s: %w", eventID, err)
	}
	// A stored ref was valid when written; a malformed one leaves the zero ref, which only the
	// revert's own decision row repeats.
	record.signalRef, _ = graph.ParseRef(signalRef)
	if reverted != nil {
		record.revertedBy = *reverted
	}
	if err := json.Unmarshal(entries, &record.entries); err != nil {
		return record, false, fmt.Errorf("projector: decode attribution decision %s: %w", eventID, err)
	}
	return record, true, nil
}

// standingCorrection is the signal's current correction, if a person has made one that nobody has
// reverted or replaced.
func (p *Projector) standingCorrection(ctx context.Context, tx pgx.Tx, signalID string) (attributionRecord, bool, error) {
	var eventID string
	err := tx.QueryRow(ctx, `
		SELECT event_id FROM graph.attribution_decisions
		WHERE signal_entity_id = $1 AND kind = 'correct'
		  AND superseded_by_event_id IS NULL AND reverted_by_event_id IS NULL
		ORDER BY decided_at DESC, event_id DESC
		LIMIT 1`, signalID).Scan(&eventID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return attributionRecord{}, false, nil
	case err != nil:
		return attributionRecord{}, false, fmt.Errorf("projector: read correction of %s: %w", signalID, err)
	}
	return p.attributionDecision(ctx, tx, eventID)
}

// checkAttribution refuses an attribution naming a signal or an entry the graph has never seen.
func (p *Projector) checkAttribution(ctx context.Context, tx pgx.Tx, field string, signal *graphv1.Ref, slugs []string) (*eventlog.Rejection, error) {
	ref := graph.RefFromProto(signal)
	if _, found, err := p.lookupRef(ctx, tx, ref); err != nil {
		return nil, err
	} else if !found {
		return &eventlog.Rejection{
			ReasonCode: eventlog.ReasonRefUnresolvable,
			ReasonDetail: fmt.Sprintf("%s.signal %s names an entity the graph has never observed",
				field, ref),
		}, nil
	}
	return p.checkEntries(ctx, tx, field, slugs...)
}

// checkRevert refuses a revert of something that is not a standing correction. The revert that
// already took it back is let through, so its re-delivery reaches the log as a duplicate.
func (p *Projector) checkRevert(ctx context.Context, tx pgx.Tx, eventID string, body *graphv1.RevertCorrection) (*eventlog.Rejection, error) {
	correction, found, err := p.attributionDecision(ctx, tx, body.GetCorrectionEventId())
	if err != nil {
		return nil, err
	}
	switch {
	case !found || correction.kind != decisionCorrect:
		return &eventlog.Rejection{
			ReasonCode:   eventlog.ReasonRefUnresolvable,
			ReasonDetail: fmt.Sprintf("revert_correction names %q, which is not a correction", body.GetCorrectionEventId()),
		}, nil
	case correction.revertedBy != "" && correction.revertedBy != eventID:
		return &eventlog.Rejection{
			ReasonCode:   eventlog.ReasonRefUnresolvable,
			ReasonDetail: fmt.Sprintf("the correction %q was already reverted", body.GetCorrectionEventId()),
		}, nil
	}
	return nil, nil
}

func attributedSlugs(items []*graphv1.Attributed) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.GetEntrySlug())
	}
	return out
}
