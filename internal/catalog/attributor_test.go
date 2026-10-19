// SPDX-License-Identifier: Apache-2.0

package catalog_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/catalog"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	eventlog "github.com/Pierre-Theophile/aisre/internal/log"
	"github.com/Pierre-Theophile/aisre/internal/projector"
	"github.com/Pierre-Theophile/aisre/internal/query"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres/pgtest"
)

// The attributor end to end against a real graph: layers 0 and 1, environments, hidden names,
// corrections outranking every layer and surviving re-attribution, and idempotency.

const person = "oidc:pierre"

type world struct {
	t   *testing.T
	p   *projector.Projector
	a   *catalog.Attributor
	seq int
	at  time.Time
}

func newWorld(t *testing.T) *world {
	t.Helper()
	p := projector.New(pgtest.Open(t))
	for _, src := range []string{"datadog:test", "human"} {
		if err := p.RegisterSource(context.Background(), eventlog.Source{
			SourceID: src, Kind: "test", Ordering: "none", SchemaVersion: "1.0.0",
		}); err != nil {
			t.Fatalf("register %s: %v", src, err)
		}
	}
	w := &world{t: t, p: p, at: time.Date(2026, 10, 19, 9, 0, 0, 0, time.UTC)}
	w.a = catalog.New(p, catalog.WithClock(func() time.Time {
		w.at = w.at.Add(time.Millisecond)
		return w.at
	}))
	if err := w.a.Register(context.Background()); err != nil {
		t.Fatalf("register the attributor: %v", err)
	}
	return w
}

func (w *world) apply(source, principal string, body func(*graphv1.EventEnvelope)) string {
	w.t.Helper()
	w.seq++
	w.at = w.at.Add(time.Second)
	env := &graphv1.EventEnvelope{EventId: fmt.Sprintf("%s:%03d", source, w.seq), SourceId: source, SchemaVersion: "1.0.0"}
	env.IdempotencyKey = env.EventId
	body(env)
	r, err := w.p.ApplyWithOptions(context.Background(), env, projector.ApplyOptions{ObservedAt: w.at, Principal: principal})
	if err != nil || r.GetStatus() != graphv1.IngestResult_APPLIED {
		w.t.Fatalf("apply %s: %v %s %s", env.GetEventId(), err, r.GetReasonCode(), r.GetReasonDetail())
	}
	return env.GetEventId()
}

func ref(ns, value string) *graphv1.Ref { return &graphv1.Ref{Namespace: ns, Value: value} }

// service asserts a service entity in an environment, correlated by its bare name as the Datadog
// feeder does.
func (w *world) service(name, env string) {
	w.apply("datadog:test", "", func(e *graphv1.EventEnvelope) {
		e.Body = &graphv1.EventEnvelope_UpsertNode{UpsertNode: &graphv1.UpsertNode{
			Ref: ref("datadog.service", env+"/"+name), Type: graphv1.NodeType_SERVICE, DisplayName: name,
			ValidAt: timestamppb.New(w.at),
		}}
	})
	fields := map[string]any{}
	if env != "" {
		fields[catalog.AttrEnvironment] = env
	}
	attrs, err := structpb.NewStruct(fields)
	if err != nil {
		w.t.Fatalf("attributes: %v", err)
	}
	w.apply("datadog:test", "", func(e *graphv1.EventEnvelope) {
		e.Body = &graphv1.EventEnvelope_CorrelateEntity{CorrelateEntity: &graphv1.CorrelateEntity{
			Subject: ref("datadog.service", env+"/"+name), Key: ref("datadog.log_service", name),
			Attributes: attrs,
		}}
	})
}

// alert asserts a monitor watching one service.
func (w *world) alert(id, service, env string) {
	w.apply("datadog:test", "", func(e *graphv1.EventEnvelope) {
		e.Body = &graphv1.EventEnvelope_AlertTransition{AlertTransition: &graphv1.AlertTransition{
			Monitor: ref("datadog.monitor", id), TransitionAt: timestamppb.New(w.at), FromState: "ok", ToState: "alert",
			Watches: []*graphv1.Ref{ref("datadog.service", env+"/"+service)}, Transport: "poll",
			Title: service + " error rate",
		}}
	})
}

func (w *world) entry(slug, name, status string) {
	w.apply("human", person, func(e *graphv1.EventEnvelope) {
		e.Body = &graphv1.EventEnvelope_DeclareCatalogEntry{DeclareCatalogEntry: &graphv1.DeclareCatalogEntry{
			Slug: slug, DisplayName: name, Kind: "service", Status: status,
		}}
	})
}

func (w *world) alias(name *graphv1.Ref, env, slug string) {
	w.apply("human", person, func(e *graphv1.EventEnvelope) {
		e.Body = &graphv1.EventEnvelope_DeclareAlias{DeclareAlias: &graphv1.DeclareAlias{Name: name, Environment: env, EntrySlug: slug}}
	})
}

// sweep runs the attributor and returns how many decisions it appended.
func (w *world) sweep() int {
	w.t.Helper()
	n, err := w.a.Sweep(context.Background())
	if err != nil {
		w.t.Fatalf("sweep: %v", err)
	}
	return n
}

// attributed is the monitor's current attributions as "slug/rule".
func (w *world) attributed(monitor string) string {
	w.t.Helper()
	rows, err := w.p.Store().Pool().Query(context.Background(), `
		SELECT c.value || '/' || (e.props->$3->>'value')
		FROM graph.edge_versions e
		JOIN graph.identity_claims c ON c.entity_id = e.dst_id AND c.namespace = 'sre.catalog'
		WHERE e.src_id = $1 AND e.type = $2 AND upper_inf(e.observed)`,
		graph.EntityID("datadog.monitor", monitor), string(graph.EdgeTypeAttributedTo), graph.AttributionRuleProp)
	if err != nil {
		w.t.Fatalf("read attributions: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			w.t.Fatalf("scan: %v", err)
		}
		out = append(out, s)
	}
	slices.Sort(out)
	return strings.Join(out, " ")
}

func TestTheAttributorFollowsAliasesAndEnvironments(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.service("candidate-api", "production")
	w.service("candidate-api", "staging")
	w.alert("1", "candidate-api", "production")
	w.alert("2", "candidate-api", "staging")
	w.entry("candidate-view", "Candidate View", "confirmed")

	// Before any alias, both signals are unknown, and the decision is recorded once.
	if n := w.sweep(); n != 2 {
		t.Fatalf("first sweep appended %d decisions, want 2 unknown ones", n)
	}
	if n := w.sweep(); n != 0 {
		t.Fatalf("an unchanged catalog appended %d decisions", n)
	}

	// An alias stated for production matches production only (research R2).
	w.alias(ref("datadog.log_service", "candidate-api"), "production", "candidate-view")
	w.sweep()
	if got := w.attributed("1"); got != "candidate-view/A1" {
		t.Errorf("production alert: %q", got)
	}
	if got := w.attributed("2"); got != "" {
		t.Errorf("staging alert attributed through a production alias: %q", got)
	}

	// Without an environment the alias matches both; the entities stay two.
	w.alias(ref("datadog.log_service", "candidate-api"), "", "candidate-view")
	w.sweep()
	if got := w.attributed("2"); got != "candidate-view/A1" {
		t.Errorf("staging alert with an environment-free alias: %q", got)
	}

	// Hiding the entry withdraws it.
	w.entry("candidate-view", "Candidate View", "hidden")
	w.sweep()
	if got := w.attributed("1") + w.attributed("2"); got != "" {
		t.Errorf("a hidden entry is still attributed: %q", got)
	}
}

func TestACorrectionOutranksEveryLayerAndSurvivesReattribution(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newWorld(t)
	w.service("candidate-api", "production")
	w.alert("1", "candidate-api", "production")
	w.alert("3", "candidate-api", "production")
	w.entry("candidate-view", "Candidate View", "confirmed")
	w.entry("infra", "Infra", "confirmed")
	w.alias(ref("datadog.log_service", "candidate-api"), "", "infra")
	w.sweep()
	if got := w.attributed("1"); got != "infra/A1" {
		t.Fatalf("before the correction: %q", got)
	}

	correction := w.apply("human", person, func(e *graphv1.EventEnvelope) {
		e.Body = &graphv1.EventEnvelope_CorrectAttribution{CorrectAttribution: &graphv1.CorrectAttribution{
			Signal: ref("datadog.monitor", "1"), EntrySlugs: []string{"candidate-view"}, Scope: "signal",
		}}
	})
	// The attributor re-evaluates and agrees with the person: nothing to append (FR-042).
	if n := w.sweep(); n != 0 {
		t.Errorf("re-attribution under a correction appended %d decisions", n)
	}
	if got := w.attributed("1"); got != "candidate-view/H" {
		t.Errorf("after the correction: %q", got)
	}
	// A later automated decision that disagrees (a ruleset that saw the signal before the
	// correction, say) is recorded as a conflict, never applied.
	w.apply(graph.AttributorSourceID, "", func(e *graphv1.EventEnvelope) {
		e.Body = &graphv1.EventEnvelope_AttributeSignal{AttributeSignal: &graphv1.AttributeSignal{
			Signal:  ref("datadog.monitor", "1"),
			Entries: []*graphv1.Attributed{{EntrySlug: "infra", Layer: graph.AttributionLayerAlias, RuleId: "A1", Score: 1}},
			Ruleset: "attribution/rules@1",
		}}
	})
	var conflicts int
	if err := w.p.Store().Pool().QueryRow(ctx, `SELECT count(*) FROM graph.attribution_decisions
		WHERE signal_entity_id = $1 AND kind = 'conflict'`, graph.EntityID("datadog.monitor", "1")).Scan(&conflicts); err != nil || conflicts != 1 {
		t.Errorf("a disagreeing automated decision: %d conflicts recorded, %v", conflicts, err)
	}
	if got := w.attributed("1"); got != "candidate-view/H" {
		t.Errorf("a conflict moved a corrected signal: %q", got)
	}

	// The alias changes; the correction still stands.
	w.alias(ref("datadog.log_service", "candidate-api"), "", "candidate-view")
	w.sweep()
	if got := w.attributed("1"); got != "candidate-view/H" {
		t.Errorf("a re-attribution moved a corrected signal: %q", got)
	}

	// A name-level correction attributes every other signal carrying the name at layer 0.
	w.apply("human", person, func(e *graphv1.EventEnvelope) {
		e.Body = &graphv1.EventEnvelope_CorrectAttribution{CorrectAttribution: &graphv1.CorrectAttribution{
			Signal: ref("datadog.monitor", "1"), EntrySlugs: []string{"infra"}, Scope: "name",
			Name: ref("datadog.log_service", "candidate-api"),
		}}
	})
	w.sweep()
	if got := w.attributed("3"); got != "infra/H" {
		t.Errorf("the next signal carrying a corrected name: %q", got)
	}

	// A revert of the first correction is shown in the audit; the later one stands.
	revert := w.apply("human", person, func(e *graphv1.EventEnvelope) {
		e.Body = &graphv1.EventEnvelope_RevertCorrection{RevertCorrection: &graphv1.RevertCorrection{CorrectionEventId: correction}}
	})
	var reverted string
	if err := w.p.Store().Pool().QueryRow(ctx, `SELECT coalesce(reverted_by_event_id, '') FROM graph.attribution_decisions
		WHERE event_id = $1`, correction).Scan(&reverted); err != nil || reverted != revert {
		t.Errorf("the revert is not in the audit: %q %v", reverted, err)
	}
	w.sweep()
	if got := w.attributed("1"); got != "infra/H" {
		t.Errorf("after reverting the earlier correction: %q", got)
	}
	why, err := query.NewEngine(w.p.Store()).WhyAttributed(ctx, &graphv1.WhyAttributedRequest{
		Signal: ref("datadog.monitor", "1"), AsOf: &graphv1.AsOf{ValidAt: timestamppb.New(w.at)}})
	if err != nil {
		t.Fatalf("why: %v", err)
	}
	if got := why.GetCurrent(); got.GetKind() != "correct" || got.GetEventId() == correction {
		t.Errorf("why answers %s %s, want the standing name-level correction", got.GetKind(), got.GetEventId())
	}

	// The revert re-delivered is a duplicate, not a revert of something already reverted.
	r, err := w.p.ApplyWithOptions(ctx, &graphv1.EventEnvelope{
		EventId: revert, IdempotencyKey: revert, SourceId: "human", SchemaVersion: "1.0.0",
		Body: &graphv1.EventEnvelope_RevertCorrection{RevertCorrection: &graphv1.RevertCorrection{CorrectionEventId: correction}},
	}, projector.ApplyOptions{ObservedAt: w.at, Principal: person})
	if err != nil || r.GetStatus() != graphv1.IngestResult_DUPLICATE_NOOP {
		t.Errorf("re-delivered revert: %v %s %s", err, r.GetStatus(), r.GetReasonDetail())
	}
}

func TestADecisionNamesItsRulesetAndEvidence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	w := newWorld(t)
	w.service("candidate-api", "production")
	w.alert("1", "candidate-api", "production")
	w.entry("candidate-view", "Candidate View", "confirmed")
	w.alias(ref("datadog.service", "production/candidate-api"), "", "candidate-view")

	env, err := w.a.Decide(ctx, graph.EntityID("datadog.monitor", "1"))
	if err != nil || env == nil {
		t.Fatalf("decide: %v %v", env, err)
	}
	body := env.GetAttributeSignal()
	if body.GetRuleset() != graph.AttributionRuleset || len(body.GetEntries()) != 1 ||
		body.GetEntries()[0].GetRuleId() != catalog.RuleAlias || body.GetEvidence().GetFields()["decided_by"].GetStringValue() != "alias" {
		t.Fatalf("decision: %v", body)
	}
	if env.GetSourceId() != graph.AttributorSourceID {
		t.Errorf("source %q", env.GetSourceId())
	}
}
