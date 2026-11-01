// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/event"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/pkg/attribution"
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
	store    Store
	events   EventLog
	names    *Names
	rules    Rules
	logger   *slog.Logger
	interval time.Duration
	batch    int
	now      func() time.Time
	// discover runs discovery before every sweep (the default).
	discover bool
	// classifier is layer 3, nil unless configured.
	classifier        attribution.Classifier
	classifierTimeout time.Duration

	meterProvider  metric.MeterProvider
	tracerProvider trace.TracerProvider
	telemetry      telemetry
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

// New builds an attributor that reads through store and appends to events. The Postgres adapter
// (internal/catalog/pgcatalog) builds both from the projector.
func New(store Store, events EventLog, opts ...Option) *Attributor {
	a := &Attributor{
		store:    store,
		events:   events,
		names:    NewNames(store),
		rules:    DefaultRules(),
		logger:   slog.Default(),
		interval: 2 * time.Second,
		batch:    500,
		now:      func() time.Time { return time.Time{} },
		discover: true,
	}
	for _, opt := range opts {
		opt(a)
	}
	a.telemetry = newTelemetry(a.meterProvider, a.tracerProvider)
	return a
}

// Register records the attributor's source, once; the log refuses events from an unknown one.
func (a *Attributor) Register(ctx context.Context) error {
	return a.events.RegisterSource(ctx, event.Source{
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
		err := a.events.Page(ctx, cursor, a.batch, func(seq int64, env *graphv1.EventEnvelope) error {
			read++
			cursor = seq
			if env.GetSourceId() == graph.AttributorSourceID && env.GetAttributeSignal() != nil {
				return nil
			}
			discover = discover || discovers(env)
			refs, all := affected(env)
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
func (a *Attributor) Sweep(ctx context.Context) (appended int, err error) {
	ctx, span := a.telemetry.tracer.Start(ctx, "catalog.attributor.sweep")
	defer func() {
		span.SetAttributes(attribute.Int("sre.attribution.appended", appended))
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}()
	// Discovery first, so a signal is attributed to an entry proposed in the same pass.
	if a.discover {
		n, err := a.Discover(ctx)
		if err != nil {
			return n, err
		}
		appended = n
	}
	ids, err := a.store.CurrentSignals(ctx)
	if err != nil {
		return appended, err
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
	result, err := a.events.Apply(ctx, env, a.now())
	if err != nil {
		return false, fmt.Errorf("catalog: append decision on %s: %w", entityID, err)
	}
	switch result.GetStatus() {
	case graphv1.IngestResult_APPLIED:
		layer, _ := env.GetAttributeSignal().GetEvidence().GetFields()["decided_by"].AsInterface().(string)
		a.telemetry.decided(ctx, layer)
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
	if a.classifier != nil && AsksClassifier(decision) {
		if decision, err = a.classify(ctx, signal, facts, decision); err != nil {
			return nil, err
		}
	}

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
	eventID, entries, suggestions, err := a.store.StandingDecision(ctx, signalID)
	if err != nil || eventID == "" {
		return currentDecision{}, err
	}
	return currentDecision{eventID: eventID, digest: decisionDigest(Decision{Entries: entries, Suggestions: suggestions})}, nil
}

// facts reads the part of the catalog a decision needs.
func (a *Attributor) facts(ctx context.Context, signal Signal) (Facts, error) {
	facts := Facts{Hidden: map[string]bool{}}

	correction, corrected, err := a.store.Correction(ctx, signal.EntityID)
	if err != nil {
		return facts, err
	}
	if corrected {
		facts.Corrected = true
		facts.Correction = append([]string{}, correction...)
	}
	if facts.Aliases, err = a.store.Aliases(ctx, RuleEntityClaim); err != nil {
		return facts, err
	}
	hidden, err := a.store.HiddenEntries(ctx)
	if err != nil {
		return facts, err
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
		facts.Entries[slug] = Entry{Display: e.display, Status: e.status, Description: e.description}
	}
	if len(signal.Owners) > 0 {
		if facts.EntryOwners, err = a.store.EntryOwners(ctx); err != nil {
			return facts, err
		}
	}
	return facts, nil
}

// lookup resolves a ref to its canonical entity, without creating anything.
func (a *Attributor) lookup(ctx context.Context, ref graph.Ref) (string, bool, error) {
	return a.store.Lookup(ctx, ref)
}

// logEnd is the log's last appended_seq: events before it are covered by the initial sweep.
func (a *Attributor) logEnd(ctx context.Context) (int64, error) { return a.store.LogEnd(ctx) }

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
