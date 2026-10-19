// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/structpb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	eventlog "github.com/Pierre-Theophile/aisre/internal/log"
	"github.com/Pierre-Theophile/aisre/internal/projector"
)

// The attributor (research R4): an in-process consumer in `serve` that reads what the projector
// wrote, runs the decision model, and appends its decision as an `attribute_signal` event under the
// source `sre.attributor`. The projector turns that event into ATTRIBUTED_TO edges.
//
// A replay never runs it: the log already holds every decision it took, so a replay from empty
// projects the same edges without calling a rule (constitution III, FR-036).
//
// It runs after projection, off the ingest path, by tailing the log: an event that touches a signal
// re-attributes that signal; an event that changes the catalog (an entry, an alias, a name-level
// correction, a revert) re-attributes every current signal. A decision equal to the signal's
// current one is not appended again, so a quiet catalog appends nothing.

// SchemaVersion is the event schema version the attributor stamps.
const SchemaVersion = "1.0.0"

// Attributor decides and appends.
type Attributor struct {
	projector *projector.Projector
	names     *Names
	rules     Rules
	logger    *slog.Logger
	interval  time.Duration
	batch     int
	now       func() time.Time
	// discover runs discovery before every sweep (the default).
	discover bool
}

// Option configures an Attributor.
type Option func(*Attributor)

// WithRules replaces the default ruleset.
func WithRules(r Rules) Option { return func(a *Attributor) { a.rules = r } }

// WithLogger sets the logger.
func WithLogger(l *slog.Logger) Option { return func(a *Attributor) { a.logger = l } }

// WithInterval sets how often Run reads the log for new events.
func WithInterval(d time.Duration) Option { return func(a *Attributor) { a.interval = d } }

// WithClock sets the clock a decision is observed at; the zero time lets the store stamp it.
func WithClock(now func() time.Time) Option { return func(a *Attributor) { a.now = now } }

// WithDiscovery turns discovery on or off. It is on by default; a test of the decision layers alone
// turns it off.
func WithDiscovery(on bool) Option { return func(a *Attributor) { a.discover = on } }

// New builds an attributor over the projector's store.
func New(p *projector.Projector, opts ...Option) *Attributor {
	a := &Attributor{
		projector: p,
		names:     NewNames(p.Store()),
		rules:     DefaultRules(),
		logger:    slog.Default(),
		interval:  2 * time.Second,
		batch:     500,
		now:       func() time.Time { return time.Time{} },
		discover:  true,
	}
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// Register records the attributor's source, once; the log refuses events from an unknown one.
func (a *Attributor) Register(ctx context.Context) error {
	return a.projector.RegisterSource(ctx, eventlog.Source{
		SourceID: graph.AttributorSourceID, Kind: "attributor", Ordering: "none", SchemaVersion: SchemaVersion,
	})
}

// Run sweeps every current signal, then follows the log until ctx ends.
func (a *Attributor) Run(ctx context.Context) {
	if err := a.Register(ctx); err != nil {
		a.logger.Error("attributor: register source", "error", err.Error())
		return
	}
	cursor, err := a.logEnd(ctx)
	if err != nil {
		a.logger.Error("attributor: read the log", "error", err.Error())
		return
	}
	if n, err := a.Sweep(ctx); err != nil {
		a.logger.Warn("attributor: initial sweep", "error", err.Error())
	} else {
		a.logger.Info("attributor: initial sweep", "decisions", n, "ruleset", a.rules.Ruleset)
	}
	ticker := time.NewTicker(a.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		next, err := a.Follow(ctx, cursor)
		if err != nil {
			a.logger.Warn("attributor: follow the log", "error", err.Error())
			continue
		}
		cursor = next
	}
}

// Follow handles every event appended after cursor and returns the new cursor.
func (a *Attributor) Follow(ctx context.Context, cursor int64) (int64, error) {
	for {
		var (
			signals  = map[string]bool{}
			sweep    bool
			discover bool
			read     int
		)
		err := a.projector.Log().Page(ctx, cursor, a.batch, func(r *eventlog.Record) error {
			read++
			cursor = r.AppendedSeq
			if r.Envelope.GetSourceId() == graph.AttributorSourceID &&
				r.Envelope.GetAttributeSignal() != nil {
				return nil
			}
			discover = discover || discovers(r.Envelope)
			refs, all := affected(r.Envelope)
			sweep = sweep || all
			for _, ref := range refs {
				signals[ref.String()] = true
			}
			return nil
		})
		if err != nil {
			return cursor, err
		}
		if discover && !sweep && a.discover {
			n, err := a.Discover(ctx)
			if err != nil {
				return cursor, err
			}
			// A new entry may be what a signal in this batch is about.
			sweep = n > 0
		}
		if sweep {
			if _, err := a.Sweep(ctx); err != nil {
				return cursor, err
			}
		} else {
			for _, spelled := range sortedKeys(signals) {
				ref, _ := graph.ParseRef(spelled)
				id, found, err := a.lookup(ctx, ref)
				if err != nil {
					return cursor, err
				}
				if !found {
					continue
				}
				if _, err := a.Attribute(ctx, id); err != nil && !errors.Is(err, ErrNotSignal) {
					return cursor, err
				}
			}
		}
		if read < a.batch {
			return cursor, nil
		}
	}
}

// affected names the signals an event may change the attribution of, or says that it may change
// any of them.
func affected(env *graphv1.EventEnvelope) (refs []graph.Ref, all bool) {
	switch body := env.GetBody().(type) {
	case *graphv1.EventEnvelope_UpsertNode:
		return []graph.Ref{graph.RefFromProto(body.UpsertNode.GetRef())}, false
	case *graphv1.EventEnvelope_AlertTransition:
		return []graph.Ref{graph.RefFromProto(body.AlertTransition.GetMonitor())}, false
	case *graphv1.EventEnvelope_ObserveChange:
		return []graph.Ref{graph.RefFromProto(body.ObserveChange.GetRef())}, false
	case *graphv1.EventEnvelope_UpsertEdge:
		switch body.UpsertEdge.GetType() {
		case graphv1.EdgeType_WATCHES:
			return []graph.Ref{graph.RefFromProto(body.UpsertEdge.GetSrc())}, false
		case graphv1.EdgeType_CHANGED_BY:
			return []graph.Ref{graph.RefFromProto(body.UpsertEdge.GetDst())}, false
		}
	case *graphv1.EventEnvelope_CorrectAttribution:
		if body.CorrectAttribution.GetScope() == graph.CorrectionScopeSignal {
			return []graph.Ref{graph.RefFromProto(body.CorrectAttribution.GetSignal())}, false
		}
		return nil, true
	case *graphv1.EventEnvelope_ProposeCatalogEntry, *graphv1.EventEnvelope_DeclareCatalogEntry,
		*graphv1.EventEnvelope_DeclareAlias, *graphv1.EventEnvelope_RetractAlias,
		*graphv1.EventEnvelope_RevertCorrection, *graphv1.EventEnvelope_ManualMerge,
		*graphv1.EventEnvelope_ConfirmMerge, *graphv1.EventEnvelope_SplitEntity:
		return nil, true
	}
	return nil, false
}

// discovers says an event may bring a service or a Notion value discovery has not seen.
func discovers(env *graphv1.EventEnvelope) bool {
	switch body := env.GetBody().(type) {
	case *graphv1.EventEnvelope_UpsertNode:
		t := body.UpsertNode.GetType()
		return t == graphv1.NodeType_SERVICE || t == graphv1.NodeType_KNOWLEDGE_DOC
	case *graphv1.EventEnvelope_IdentityClaim, *graphv1.EventEnvelope_CorrelateEntity,
		*graphv1.EventEnvelope_RejectAlias:
		return true
	}
	return false
}

// Sweep runs discovery, re-attributes every current signal, and returns how many events it
// appended. A change is an instant, never valid now, so every change the graph still believes in
// is current.
func (a *Attributor) Sweep(ctx context.Context) (int, error) {
	// Discovery first, so a signal is attributed to an entry proposed in the same pass.
	appended := 0
	if a.discover {
		n, err := a.Discover(ctx)
		if err != nil {
			return n, err
		}
		appended = n
	}
	rows, err := a.projector.Store().Pool().Query(ctx, `
		SELECT e.entity_id FROM graph.entities e
		WHERE e.merged_into IS NULL AND e.type = ANY($1)
		  AND EXISTS (SELECT 1 FROM graph.entity_versions v
		              WHERE v.entity_id = e.entity_id AND upper_inf(v.observed)
		                AND (upper_inf(v.valid) OR e.type = $2))
		ORDER BY e.entity_id`,
		[]string{string(graph.NodeTypeAlert), string(graph.NodeTypeKnowledgeDoc), string(graph.NodeTypeChange)},
		string(graph.NodeTypeChange))
	if err != nil {
		return 0, fmt.Errorf("catalog: list current signals: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, fmt.Errorf("catalog: list current signals: %w", err)
	}
	for _, id := range ids {
		ok, err := a.Attribute(ctx, id)
		if err != nil && !errors.Is(err, ErrNotSignal) {
			return appended, err
		}
		if ok {
			appended++
		}
	}
	return appended, nil
}

// Attribute decides one signal and appends the decision when it differs from the current one. It
// reports whether it appended.
func (a *Attributor) Attribute(ctx context.Context, entityID string) (bool, error) {
	env, err := a.Decide(ctx, entityID)
	if err != nil || env == nil {
		return false, err
	}
	result, err := a.projector.Apply(ctx, env, a.now())
	if err != nil {
		return false, fmt.Errorf("catalog: append decision on %s: %w", entityID, err)
	}
	switch result.GetStatus() {
	case graphv1.IngestResult_APPLIED:
		return true, nil
	case graphv1.IngestResult_REJECTED:
		return false, fmt.Errorf("catalog: decision on %s refused: %s %s", entityID,
			result.GetReasonCode(), result.GetReasonDetail())
	default:
		return false, nil
	}
}

// Decide builds the event the attributor would append for one signal, or nil when the decision
// equals the current one.
func (a *Attributor) Decide(ctx context.Context, entityID string) (*graphv1.EventEnvelope, error) {
	signal, err := a.names.Signal(ctx, entityID)
	if err != nil {
		return nil, err
	}
	if signal.Ref.IsZero() {
		return nil, fmt.Errorf("catalog: %s has no ref to name it by: %w", entityID, ErrNotSignal)
	}
	facts, err := a.facts(ctx, signal)
	if err != nil {
		return nil, err
	}
	decision := a.rules.Decide(signal, facts)

	current, err := a.current(ctx, signal.EntityID)
	if err != nil {
		return nil, err
	}
	digest := decisionDigest(decision)
	if current.eventID != "" && current.digest == digest {
		return nil, nil
	}
	evidence, err := structpb.NewStruct(decision.Evidence)
	if err != nil {
		return nil, fmt.Errorf("catalog: evidence of %s: %w", entityID, err)
	}
	h := sha256.New()
	for _, part := range []string{signal.EntityID, a.rules.Ruleset, digest, current.eventID} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	id := graph.AttributorSourceID + ":attribute:" + hex.EncodeToString(h.Sum(nil))[:32]
	return &graphv1.EventEnvelope{
		EventId:        id,
		IdempotencyKey: id,
		SourceId:       graph.AttributorSourceID,
		SchemaVersion:  SchemaVersion,
		Body: &graphv1.EventEnvelope_AttributeSignal{AttributeSignal: &graphv1.AttributeSignal{
			Signal:            signal.Ref.Proto(),
			Entries:           attributedProto(decision.Entries),
			Suggestions:       attributedProto(decision.Suggestions),
			Ruleset:           a.rules.Ruleset,
			Evidence:          evidence,
			SupersedesEventId: current.eventID,
		}},
	}, nil
}

// decisionDigest is what makes two decisions the same: their entries and suggestions, with layer,
// rule and score. Evidence and rationale do not count: they explain a decision, they are not it.
func decisionDigest(d Decision) string {
	var b strings.Builder
	for _, list := range [][]Attributed{d.Entries, d.Suggestions} {
		for _, e := range list {
			fmt.Fprintf(&b, "%s|%s|%s|%.6f;", e.Slug, e.Layer, e.RuleID, e.Score)
		}
		b.WriteByte('#')
	}
	return b.String()
}

type currentDecision struct {
	eventID string
	digest  string
}

// current reads the signal's standing decision: the latest attribution or correction nothing has
// superseded or reverted.
func (a *Attributor) current(ctx context.Context, signalID string) (currentDecision, error) {
	var (
		out                  currentDecision
		entries, suggestions []byte
	)
	err := a.projector.Store().Pool().QueryRow(ctx, `
		SELECT event_id, entries, suggestions FROM graph.attribution_decisions
		WHERE signal_entity_id = $1 AND kind IN ('attribute', 'correct')
		  AND superseded_by_event_id IS NULL AND reverted_by_event_id IS NULL
		ORDER BY decided_at DESC, event_id DESC LIMIT 1`, signalID).Scan(&out.eventID, &entries, &suggestions)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return out, nil
	case err != nil:
		return out, fmt.Errorf("catalog: read the decision on %s: %w", signalID, err)
	}
	var d Decision
	for _, pair := range []struct {
		raw []byte
		to  *[]Attributed
	}{{entries, &d.Entries}, {suggestions, &d.Suggestions}} {
		var items []struct {
			EntrySlug string  `json:"entry_slug"`
			Layer     string  `json:"layer"`
			RuleID    string  `json:"rule_id"`
			Score     float64 `json:"score"`
		}
		if err := json.Unmarshal(pair.raw, &items); err != nil {
			return out, fmt.Errorf("catalog: decode the decision on %s: %w", signalID, err)
		}
		for _, item := range items {
			*pair.to = append(*pair.to, Attributed{Slug: item.EntrySlug, Layer: item.Layer, RuleID: item.RuleID, Score: item.Score})
		}
	}
	out.digest = decisionDigest(d)
	return out, nil
}

// facts reads the part of the catalog a decision needs.
func (a *Attributor) facts(ctx context.Context, signal Signal) (Facts, error) {
	pool := a.projector.Store().Pool()
	facts := Facts{Hidden: map[string]bool{}}
	signalID := signal.EntityID

	var entries []byte
	err := pool.QueryRow(ctx, `
		SELECT entries FROM graph.attribution_decisions
		WHERE signal_entity_id = $1 AND kind = 'correct'
		  AND superseded_by_event_id IS NULL AND reverted_by_event_id IS NULL
		ORDER BY decided_at DESC, event_id DESC LIMIT 1`, signalID).Scan(&entries)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return facts, fmt.Errorf("catalog: read corrections of %s: %w", signalID, err)
	default:
		var items []struct {
			EntrySlug string `json:"entry_slug"`
		}
		if err := json.Unmarshal(entries, &items); err != nil {
			return facts, fmt.Errorf("catalog: decode the correction of %s: %w", signalID, err)
		}
		facts.Corrected = true
		facts.Correction = []string{}
		for _, item := range items {
			facts.Correction = append(facts.Correction, item.EntrySlug)
		}
	}

	rows, err := pool.Query(ctx, `
		SELECT a.namespace, a.value, a.environment, a.entry_slug,
		       EXISTS (SELECT 1 FROM graph.attribution_decisions d
		               WHERE d.event_id = a.decided_by_event_id AND d.kind = 'correct'),
		       coalesce(a.rule_id, '') = $1
		FROM graph.catalog_alias_versions a WHERE upper_inf(a.observed)
		ORDER BY 1, 2, 3`, RuleEntityClaim)
	if err != nil {
		return facts, fmt.Errorf("catalog: read aliases: %w", err)
	}
	for rows.Next() {
		var alias Alias
		if err := rows.Scan(&alias.Name.Ref.Namespace, &alias.Name.Ref.Value, &alias.Name.Environment,
			&alias.Slug, &alias.ByCorrection, &alias.Discovered); err != nil {
			rows.Close()
			return facts, fmt.Errorf("catalog: scan alias: %w", err)
		}
		facts.Aliases = append(facts.Aliases, alias)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return facts, fmt.Errorf("catalog: read aliases: %w", err)
	}

	rows, err = pool.Query(ctx, `
		SELECT c.value FROM graph.identity_claims c
		JOIN graph.entity_versions v ON v.entity_id = c.entity_id
		WHERE c.namespace = $1 AND upper_inf(v.observed) AND upper_inf(v.valid)
		  AND v.props->$2->>'value' = $3`,
		graph.CatalogNamespace, graph.CatalogStatusProp, graph.CatalogStatusHidden)
	if err != nil {
		return facts, fmt.Errorf("catalog: read hidden entries: %w", err)
	}
	hidden, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return facts, fmt.Errorf("catalog: read hidden entries: %w", err)
	}
	for _, slug := range hidden {
		facts.Hidden[slug] = true
	}

	known, err := a.entries(ctx)
	if err != nil {
		return facts, err
	}
	facts.Entries = make(map[string]Entry, len(known))
	for slug, e := range known {
		facts.Entries[slug] = Entry{Display: e.display, Status: e.status}
	}
	if len(signal.Owners) > 0 {
		if facts.EntryOwners, err = a.entryOwners(ctx); err != nil {
			return facts, err
		}
	}
	return facts, nil
}

// entryOwners reads, per entry, the owners of the entities its aliases name (rule P4).
func (a *Attributor) entryOwners(ctx context.Context) (map[string][]string, error) {
	rows, err := a.projector.Store().Pool().Query(ctx, `
		WITH named AS (
			SELECT a.entry_slug, c.entity_id FROM graph.catalog_alias_versions a
			JOIN graph.identity_claims c ON c.namespace = a.namespace AND c.value = a.value
			WHERE upper_inf(a.observed) AND a.entry_slug <> $2
			UNION
			SELECT a.entry_slug, k.entity_id FROM graph.catalog_alias_versions a
			JOIN graph.correlation_keys k ON k.namespace = a.namespace AND k.value = a.value
			WHERE upper_inf(a.observed) AND a.entry_slug <> $2
		)
		SELECT DISTINCT named.entry_slug, coalesce(t.merged_into, o.dst_id)
		FROM named
		JOIN graph.entities m ON m.entity_id = named.entity_id
		JOIN graph.edge_versions o ON o.src_id = coalesce(m.merged_into, m.entity_id)
		LEFT JOIN graph.entities t ON t.entity_id = o.dst_id
		WHERE o.type = $1 AND upper_inf(o.observed) AND upper_inf(o.valid)
		ORDER BY 1, 2`, string(graph.EdgeTypeOwnedBy), graph.HiddenEntrySlug)
	if err != nil {
		return nil, fmt.Errorf("catalog: read entry owners: %w", err)
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var slug, owner string
		if err := rows.Scan(&slug, &owner); err != nil {
			return nil, fmt.Errorf("catalog: scan entry owner: %w", err)
		}
		out[slug] = append(out[slug], owner)
	}
	return out, rows.Err()
}

// lookup resolves a ref to its canonical entity, without creating anything.
func (a *Attributor) lookup(ctx context.Context, ref graph.Ref) (string, bool, error) {
	var id string
	err := a.projector.Store().Pool().QueryRow(ctx, `
		SELECT entity_id FROM (
			SELECT entity_id, 0 AS rank, '' AS claim_id FROM graph.entities WHERE entity_id = $1
			UNION ALL
			SELECT entity_id, 1, claim_id FROM graph.identity_claims WHERE namespace = $2 AND value = $3
		) found ORDER BY rank, claim_id LIMIT 1`, ref.EntityID(), ref.Namespace, ref.Value).Scan(&id)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("catalog: look up %s: %w", ref, err)
	}
	return id, true, nil
}

// logEnd is the log's last appended_seq: events before it are covered by the initial sweep.
func (a *Attributor) logEnd(ctx context.Context) (int64, error) {
	var seq int64
	err := a.projector.Store().Pool().QueryRow(ctx,
		`SELECT coalesce(max(appended_seq), 0) FROM log.events`).Scan(&seq)
	return seq, err
}

func attributedProto(items []Attributed) []*graphv1.Attributed {
	out := make([]*graphv1.Attributed, 0, len(items))
	for _, item := range items {
		out = append(out, &graphv1.Attributed{
			EntrySlug: item.Slug, Layer: item.Layer, RuleId: item.RuleID, Score: item.Score,
			Rationale: item.Rationale,
		})
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
