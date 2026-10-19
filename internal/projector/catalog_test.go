// SPDX-License-Identifier: Apache-2.0

package projector_test

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	eventlog "github.com/Pierre-Theophile/aisre/internal/log"
	"github.com/Pierre-Theophile/aisre/internal/projector"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
)

// The service catalog's events (feature 009, ADR-0015): entries, aliases, attributions and
// corrections, each checked for what it writes, what it refuses, and that a replay from the log
// writes the same thing.

const (
	catalogFeeder = "datadog:test"
	catalogHuman  = "human"
	catalogPerson = "oidc:pierre"
)

func catalogProjector(t *testing.T) (*projector.Projector, *postgres.Store) {
	t.Helper()
	store := openStoreT(t)
	return catalogProjectorOn(t, store), store
}

func catalogProjectorOn(t *testing.T, store *postgres.Store) *projector.Projector {
	t.Helper()
	p := projector.New(store)
	for _, src := range []string{catalogFeeder, catalogHuman, graph.AttributorSourceID} {
		if err := p.RegisterSource(context.Background(), eventlog.Source{
			SourceID: src, Kind: "test", Ordering: "none", SchemaVersion: "1.0.0",
		}); err != nil {
			t.Fatalf("register source %s: %v", src, err)
		}
	}
	return p
}

func catalogEnv(id, source string) *graphv1.EventEnvelope {
	return &graphv1.EventEnvelope{EventId: id, IdempotencyKey: id, SourceId: source, SchemaVersion: "1.0.0"}
}

func alertRef() *graphv1.Ref {
	return &graphv1.Ref{Namespace: "datadog.monitor", Value: "12345"}
}

func alertNode(id string) *graphv1.EventEnvelope {
	env := catalogEnv(id, catalogFeeder)
	env.Body = &graphv1.EventEnvelope_UpsertNode{UpsertNode: &graphv1.UpsertNode{
		Ref: alertRef(), Type: graphv1.NodeType_ALERT, DisplayName: "candidate-api error rate",
		ValidAt: timestamppb.New(mustTime("2026-10-19T08:00:00Z")),
	}}
	return env
}

func declareEntry(id, slug, name, status string) *graphv1.EventEnvelope {
	env := catalogEnv(id, catalogHuman)
	env.Body = &graphv1.EventEnvelope_DeclareCatalogEntry{DeclareCatalogEntry: &graphv1.DeclareCatalogEntry{
		Slug: slug, DisplayName: name, Kind: graph.CatalogKindService, Status: status,
	}}
	return env
}

func proposeEntry(id, slug, name string, aliases ...*graphv1.Ref) *graphv1.EventEnvelope {
	env := catalogEnv(id, graph.AttributorSourceID)
	env.Body = &graphv1.EventEnvelope_ProposeCatalogEntry{ProposeCatalogEntry: &graphv1.ProposeCatalogEntry{
		Slug: slug, DisplayName: name, Aliases: aliases,
	}}
	return env
}

func declareAlias(id string, name *graphv1.Ref, slug string) *graphv1.EventEnvelope {
	env := catalogEnv(id, catalogHuman)
	env.Body = &graphv1.EventEnvelope_DeclareAlias{DeclareAlias: &graphv1.DeclareAlias{Name: name, EntrySlug: slug}}
	return env
}

func retractAlias(id string, name *graphv1.Ref, slug string) *graphv1.EventEnvelope {
	env := catalogEnv(id, catalogHuman)
	env.Body = &graphv1.EventEnvelope_RetractAlias{RetractAlias: &graphv1.RetractAlias{Name: name, EntrySlug: slug}}
	return env
}

func attribute(id, source string, slugs ...string) *graphv1.EventEnvelope {
	env := catalogEnv(id, source)
	entries := make([]*graphv1.Attributed, 0, len(slugs))
	for _, slug := range slugs {
		entries = append(entries, &graphv1.Attributed{
			EntrySlug: slug, Layer: graph.AttributionLayerAlias, RuleId: "A1", Score: 1,
		})
	}
	env.Body = &graphv1.EventEnvelope_AttributeSignal{AttributeSignal: &graphv1.AttributeSignal{
		Signal: alertRef(), Entries: entries, Ruleset: graph.AttributionRuleset,
	}}
	return env
}

func correct(id, scope string, name *graphv1.Ref, slugs ...string) *graphv1.EventEnvelope {
	env := catalogEnv(id, catalogHuman)
	env.Body = &graphv1.EventEnvelope_CorrectAttribution{CorrectAttribution: &graphv1.CorrectAttribution{
		Signal: alertRef(), EntrySlugs: slugs, Scope: scope, Name: name, Rationale: "it is the candidate view",
	}}
	return env
}

func revert(id, correction string) *graphv1.EventEnvelope {
	env := catalogEnv(id, catalogHuman)
	env.Body = &graphv1.EventEnvelope_RevertCorrection{RevertCorrection: &graphv1.RevertCorrection{
		CorrectionEventId: correction,
	}}
	return env
}

func ddService(value string) *graphv1.Ref {
	return &graphv1.Ref{Namespace: "datadog.service", Value: value}
}

func mustApply(t *testing.T, p *projector.Projector, env *graphv1.EventEnvelope, principal, at string) {
	t.Helper()
	if r := applyAt(t, p, env, principal, at); r.GetStatus() != graphv1.IngestResult_APPLIED {
		t.Fatalf("%s: %s %s %s", env.GetEventId(), r.GetStatus(), r.GetReasonCode(), r.GetReasonDetail())
	}
}

func mustReject(t *testing.T, p *projector.Projector, env *graphv1.EventEnvelope, principal, at, reason string) {
	t.Helper()
	r := applyAt(t, p, env, principal, at)
	if r.GetStatus() != graphv1.IngestResult_REJECTED || r.GetReasonCode() != reason {
		t.Fatalf("%s: want rejected %s, got %s %s %s", env.GetEventId(), reason,
			r.GetStatus(), r.GetReasonCode(), r.GetReasonDetail())
	}
}

// entryStatus reads an entry's status and display name as of now. Earlier decisions stay as the
// valid-time history before it.
func entryStatus(t *testing.T, store *postgres.Store, slug string) (status, name string, versions int) {
	t.Helper()
	rows, err := store.Pool().Query(context.Background(), `
		SELECT props->$2->>'value', display_name FROM graph.entity_versions
		WHERE entity_id = $1 AND upper_inf(observed) AND upper_inf(valid)`,
		graph.CatalogRef(slug).EntityID(), graph.CatalogStatusProp)
	if err != nil {
		t.Fatalf("read entry %s: %v", slug, err)
	}
	defer rows.Close()
	for rows.Next() {
		versions++
		if err := rows.Scan(&status, &name); err != nil {
			t.Fatalf("scan entry: %v", err)
		}
	}
	return status, name, versions
}

// currentAliases lists the open alias rows as "ns=value>slug(decider)".
func currentAliases(t *testing.T, store *postgres.Store) []string {
	t.Helper()
	rows, err := store.Pool().Query(context.Background(), `
		SELECT namespace || '=' || value || '>' || entry_slug || '(' || coalesce(principal, rule_id) || ')'
		FROM graph.catalog_alias_versions WHERE upper_inf(observed) ORDER BY 1`)
	if err != nil {
		t.Fatalf("read aliases: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan alias: %v", err)
		}
		out = append(out, s)
	}
	return out
}

// currentAttributions lists the alert's open ATTRIBUTED_TO edges as "slug/layer/principal".
func currentAttributions(t *testing.T, store *postgres.Store) []string {
	t.Helper()
	rows, err := store.Pool().Query(context.Background(), `
		SELECT d.entity_id, e.props->$2->>'value', coalesce(e.props->$3->>'value', '')
		FROM graph.edge_versions e JOIN graph.entities d ON d.entity_id = e.dst_id
		WHERE e.type = $1 AND upper_inf(e.observed) ORDER BY 1`,
		string(graph.EdgeTypeAttributedTo), graph.AttributionLayerProp, graph.AttributionPrincipalProp)
	if err != nil {
		t.Fatalf("read attributions: %v", err)
	}
	defer rows.Close()
	slugOf := map[string]string{}
	for _, slug := range []string{"candidate-view", "infra"} {
		slugOf[graph.CatalogRef(slug).EntityID()] = slug
	}
	var out []string
	for rows.Next() {
		var id, layer, principal string
		if err := rows.Scan(&id, &layer, &principal); err != nil {
			t.Fatalf("scan attribution: %v", err)
		}
		out = append(out, slugOf[id]+"/"+layer+"/"+principal)
	}
	return out
}

func decisionKinds(t *testing.T, store *postgres.Store) string {
	t.Helper()
	var kinds string
	if err := store.Pool().QueryRow(context.Background(), `
		SELECT coalesce(string_agg(event_id || ':' || kind || ':' || coalesce(superseded_by_event_id, '') || ':' ||
		       coalesce(reverted_by_event_id, ''), ' ' ORDER BY decided_at, event_id), '')
		FROM graph.attribution_decisions`).Scan(&kinds); err != nil {
		t.Fatalf("read decisions: %v", err)
	}
	return kinds
}

func TestCatalogEntryLifecycle(t *testing.T) {
	t.Parallel()
	p, store := catalogProjector(t)

	// A person's decision without a person is refused (001 FR-041).
	mustReject(t, p, declareEntry("d0", "candidate-view", "Candidate View", graph.CatalogStatusConfirmed),
		"", "2026-10-19T09:00:00Z", eventlog.ReasonMissingPrincipal)

	// The attributor proposes; the entry exists as proposed, with its entity's claims as aliases.
	mustApply(t, p, proposeEntry("p1", "candidate-view", "candidate-api", ddService("candidate-api")),
		"", "2026-10-19T09:00:01Z")
	if status, _, n := entryStatus(t, store, "candidate-view"); status != graph.CatalogStatusProposed || n != 1 {
		t.Fatalf("after proposal: status %q in %d versions", status, n)
	}
	if got := strings.Join(currentAliases(t, store), " "); got != "datadog.service=candidate-api>candidate-view(D1)" {
		t.Fatalf("aliases after proposal: %s", got)
	}

	// A person confirms and renames it: one current version, the latest decision.
	mustApply(t, p, declareEntry("d1", "candidate-view", "Candidate View", graph.CatalogStatusConfirmed),
		catalogPerson, "2026-10-19T09:00:02Z")
	if status, name, n := entryStatus(t, store, "candidate-view"); status != graph.CatalogStatusConfirmed ||
		name != "Candidate View" || n != 1 {
		t.Fatalf("after confirmation: %q %q in %d versions", status, name, n)
	}

	// A later proposal for an existing entry changes nothing.
	mustApply(t, p, proposeEntry("p2", "candidate-view", "candidate-api"), "", "2026-10-19T09:00:03Z")
	if status, name, _ := entryStatus(t, store, "candidate-view"); status != graph.CatalogStatusConfirmed ||
		name != "Candidate View" {
		t.Fatalf("a proposal overrode a person: %q %q", status, name)
	}

	// Hidden, then restored.
	mustApply(t, p, declareEntry("d2", "candidate-view", "Candidate View", graph.CatalogStatusHidden),
		catalogPerson, "2026-10-19T09:00:04Z")
	if status, _, _ := entryStatus(t, store, "candidate-view"); status != graph.CatalogStatusHidden {
		t.Fatalf("after hiding: %q", status)
	}

	// Double delivery is a no-op.
	if r := applyAt(t, p, declareEntry("d2", "candidate-view", "Candidate View", graph.CatalogStatusHidden),
		catalogPerson, "2026-10-19T09:00:05Z"); r.GetStatus() != graphv1.IngestResult_DUPLICATE_NOOP {
		t.Fatalf("redelivery: %s", r.GetStatus())
	}
}

func TestNoConnectorWritesTheCatalog(t *testing.T) {
	t.Parallel()
	p, _ := catalogProjector(t)
	at := "2026-10-19T09:00:00Z"

	node := catalogEnv("u1", catalogFeeder)
	node.Body = &graphv1.EventEnvelope_UpsertNode{UpsertNode: &graphv1.UpsertNode{
		Ref: graph.CatalogRef("candidate-view").Proto(), Type: graphv1.NodeType_SERVICE,
		ValidAt: timestamppb.New(mustTime(at)),
	}}
	mustReject(t, p, node, "", at, eventlog.ReasonCatalogNamespaceReserved)

	feederProposal := proposeEntry("p1", "candidate-view", "x")
	feederProposal.SourceId = catalogFeeder
	mustReject(t, p, feederProposal, "", at, eventlog.ReasonCatalogNamespaceReserved)
	mustApply(t, p, alertNode("a1"), "", at)
	mustApply(t, p, declareEntry("d1", "candidate-view", "Candidate View", graph.CatalogStatusConfirmed),
		catalogPerson, "2026-10-19T09:00:01Z")
	mustReject(t, p, attribute("x1", catalogFeeder, "candidate-view"), "", "2026-10-19T09:00:02Z",
		eventlog.ReasonCatalogNamespaceReserved)
}

func TestAliasReassignmentAndRetraction(t *testing.T) {
	t.Parallel()
	p, store := catalogProjector(t)
	mustApply(t, p, declareEntry("d1", "candidate-view", "Candidate View", graph.CatalogStatusConfirmed),
		catalogPerson, "2026-10-19T09:00:00Z")
	mustApply(t, p, declareEntry("d2", "infra", "Infra", graph.CatalogStatusConfirmed),
		catalogPerson, "2026-10-19T09:00:01Z")

	mustReject(t, p, declareAlias("x0", ddService("candidate-api"), "nowhere"), catalogPerson,
		"2026-10-19T09:00:02Z", eventlog.ReasonRefUnresolvable)
	mustApply(t, p, declareAlias("al1", ddService("candidate-api"), "infra"), catalogPerson, "2026-10-19T09:00:03Z")
	mustApply(t, p, declareAlias("al2", ddService("candidate-api"), "candidate-view"), catalogPerson,
		"2026-10-19T09:00:04Z")
	if got := strings.Join(currentAliases(t, store), " "); got != "datadog.service=candidate-api>candidate-view(oidc:pierre)" {
		t.Fatalf("after reassignment: %s", got)
	}
	var history int
	if err := store.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM graph.catalog_alias_versions`).Scan(&history); err != nil || history != 2 {
		t.Fatalf("history: %d rows (%v), want the closed row kept", history, err)
	}

	mustReject(t, p, retractAlias("r0", ddService("candidate-api"), "infra"), catalogPerson,
		"2026-10-19T09:00:05Z", eventlog.ReasonRefUnresolvable)
	mustApply(t, p, retractAlias("r1", ddService("candidate-api"), "candidate-view"), catalogPerson,
		"2026-10-19T09:00:06Z")
	if got := currentAliases(t, store); len(got) != 0 {
		t.Fatalf("after retraction: %v", got)
	}

	// A hidden name is an alias of "-", which has no node.
	mustApply(t, p, declareAlias("h1", ddService("misc"), graph.HiddenEntrySlug), catalogPerson, "2026-10-19T09:00:07Z")
	if got := strings.Join(currentAliases(t, store), " "); got != "datadog.service=misc>-(oidc:pierre)" {
		t.Fatalf("hidden name: %s", got)
	}
}

// attributionEvents is one signal's whole story: automated, re-attributed, corrected, an automated
// decision disagreeing with the correction, a revert, and a name-level correction.
func attributionEvents() []struct {
	env       *graphv1.EventEnvelope
	principal string
	at        string
} {
	return []struct {
		env       *graphv1.EventEnvelope
		principal string
		at        string
	}{
		{alertNode("a1"), "", "2026-10-19T09:00:00Z"},
		{declareEntry("d1", "candidate-view", "Candidate View", graph.CatalogStatusConfirmed), catalogPerson, "2026-10-19T09:00:01Z"},
		{declareEntry("d2", "infra", "Infra", graph.CatalogStatusConfirmed), catalogPerson, "2026-10-19T09:00:02Z"},
		{attribute("s1", graph.AttributorSourceID, "infra"), "", "2026-10-19T09:00:03Z"},
		{attribute("s2", graph.AttributorSourceID, "infra", "candidate-view"), "", "2026-10-19T09:00:04Z"},
		{correct("c1", graph.CorrectionScopeSignal, nil, "candidate-view"), catalogPerson, "2026-10-19T09:00:05Z"},
		{attribute("s3", graph.AttributorSourceID, "infra"), "", "2026-10-19T09:00:06Z"},
		{revert("v1", "c1"), catalogPerson, "2026-10-19T09:00:07Z"},
		{correct("c2", graph.CorrectionScopeName, ddService("candidate-api"), "candidate-view"), catalogPerson, "2026-10-19T09:00:08Z"},
	}
}

func TestAttributionCorrectionAndRevert(t *testing.T) {
	t.Parallel()
	p, store := catalogProjector(t)
	steps := attributionEvents()
	want := [][]string{
		nil, nil, nil,
		{"infra/alias/"},
		{"candidate-view/alias/", "infra/alias/"},
		{"candidate-view/correction/oidc:pierre"},
		{"candidate-view/correction/oidc:pierre"}, // an automated decision does not outvote a person
		nil, // reverted: unattributed until the attributor decides again
		{"candidate-view/correction/oidc:pierre"},
	}
	for i, step := range steps {
		mustApply(t, p, step.env, step.principal, step.at)
		if got := currentAttributions(t, store); strings.Join(got, " ") != strings.Join(want[i], " ") {
			t.Fatalf("after %s: %v, want %v", step.env.GetEventId(), got, want[i])
		}
	}
	if got, want := decisionKinds(t, store),
		"s1:attribute:s2: s2:attribute:c1: c1:correct::v1 s3:conflict:: v1:revert:: c2:correct::"; got != want {
		t.Errorf("decisions:\n got %s\nwant %s", got, want)
	}
	if got := strings.Join(currentAliases(t, store), " "); got != "datadog.service=candidate-api>candidate-view(oidc:pierre)" {
		t.Errorf("a name-level correction is an alias: %s", got)
	}

	// Refusals: no principal, an unknown correction, a revert twice.
	mustReject(t, p, correct("c9", graph.CorrectionScopeSignal, nil, "infra"), "", "2026-10-19T09:00:09Z",
		eventlog.ReasonMissingPrincipal)
	mustReject(t, p, revert("v9", "nope"), catalogPerson, "2026-10-19T09:00:10Z", eventlog.ReasonRefUnresolvable)
	mustReject(t, p, revert("v8", "c1"), catalogPerson, "2026-10-19T09:00:11Z", eventlog.ReasonRefUnresolvable)
}

func TestCatalogEventsSurviveReplay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	steps := attributionEvents()
	steps = append(steps[:3:3], append([]struct {
		env       *graphv1.EventEnvelope
		principal string
		at        string
	}{
		{proposeEntry("p1", "candidate-api", "candidate-api", ddService("candidate-api-2")), "", "2026-10-19T08:59:00Z"},
		{declareAlias("al1", ddService("checkout"), "infra"), catalogPerson, "2026-10-19T09:00:02.5Z"},
	}, steps[3:]...)...)

	live, liveStore := catalogProjector(t)
	for _, step := range steps {
		mustApply(t, live, step.env, step.principal, step.at)
	}
	assertInvariants(t, liveStore, "after the catalog events")

	rebuiltStore := openStoreT(t)
	rebuilt := catalogProjectorOn(t, rebuiltStore)
	for _, step := range steps {
		result, err := rebuilt.Log().Append(ctx, step.env, eventlog.AppendOptions{
			ObservedAt: mustTime(step.at), Principal: step.principal,
		})
		if err != nil || result.GetStatus() != graphv1.IngestResult_APPLIED {
			t.Fatalf("append %s: %v %s", step.env.GetEventId(), err, result.GetReasonDetail())
		}
	}
	if _, err := rebuilt.Replay(ctx); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if want, got := snapshot(t, liveStore), snapshot(t, rebuiltStore); want != got {
		t.Errorf("replay produced a different graph:\n%s", firstDifferingLine(want, got))
	}
	for _, read := range []func(*testing.T, *postgres.Store) string{
		func(t *testing.T, s *postgres.Store) string { return strings.Join(currentAliases(t, s), " ") },
		func(t *testing.T, s *postgres.Store) string { return strings.Join(currentAttributions(t, s), " ") },
		decisionKinds,
	} {
		if want, got := read(t, liveStore), read(t, rebuiltStore); want != got {
			t.Errorf("replay differs:\n live %s\nreplay %s", want, got)
		}
	}
}
