// SPDX-License-Identifier: Apache-2.0

package runner_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/investigation/audit"
	investigationbackend "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/internal/investigation/engine"
	"github.com/Pierre-Theophile/aisre/internal/investigation/intake"
	"github.com/Pierre-Theophile/aisre/internal/investigation/ledger"
	"github.com/Pierre-Theophile/aisre/internal/investigation/model"
	"github.com/Pierre-Theophile/aisre/internal/investigation/model/modeltest"
	"github.com/Pierre-Theophile/aisre/internal/investigation/replay"
	"github.com/Pierre-Theophile/aisre/internal/investigation/runner"
	investigationstore "github.com/Pierre-Theophile/aisre/internal/investigation/store"
	"github.com/Pierre-Theophile/aisre/internal/investigation/workers/logs"
	"github.com/Pierre-Theophile/aisre/internal/investigation/workers/metrics"
	"github.com/Pierre-Theophile/aisre/internal/investigation/workers/traces"
	"github.com/Pierre-Theophile/aisre/internal/query"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres/pgtest"
	sdk "github.com/Pierre-Theophile/aisre/pkg/backend"
	"github.com/Pierre-Theophile/aisre/pkg/backend/synthetic"
	"github.com/Pierre-Theophile/aisre/pkg/worker"
)

// The runner against a real database (Phase 7 Track D; FR-008b, FR-035, FR-046a, FR-057b, FR-067).
//
// Every test here runs with **no model client and no API key**, which is the point rather than a
// convenience: FR-067 says the engine is fully operable with no vendor connector configured, and a
// test suite that needed one would be a suite that could not prove it. The one test that does
// exercise the model boundary hands it a canned transport.
//
// The database is real, because everything this package adds over the engine is persistence: the
// idempotency key that stops a re-delivered alert opening a second investigation, the deferred sum
// trigger the hypothesis set is written under, the single decision record, the guarded conclusion.
// A mocked store would assert that the SQL was spelled the way the test expected it to be spelled.
//
// The *graph* is a stub and the *telemetry* is synthetic, and that pairing is deliberate. The
// engine's own end-to-end behaviour — culprit first, decoy exonerated — is proved over the same
// scenario in `internal/investigation/engine`; what is under test here is the assembly around it,
// so the world it reasons over is the one already known to produce a clean answer.

func TestMain(m *testing.M) { pgtest.TestMain(m) }

const (
	checkoutRef   = "otel.service.name=checkout"
	paymentsRef   = "otel.service.name=payments"
	storefrontRef = "otel.service.name=storefront"

	rolloutChange = "k8s.change=shop/payments@rev7"
	scalingChange = "k8s.change=shop/storefront@scale-1415"

	requester = "https://login.example.invalid/#sam@example.invalid"
)

var (
	windowStart = time.Date(2026, 9, 1, 13, 30, 0, 0, time.UTC)
	onsetAt     = time.Date(2026, 9, 1, 14, 21, 0, 0, time.UTC)
	firedAt     = time.Date(2026, 9, 1, 14, 32, 0, 0, time.UTC)

	rolloutAt = onsetAt.Add(-2 * time.Minute)
	scalingAt = onsetAt.Add(5 * time.Minute)
)

// --- the world -------------------------------------------------------------------------------

// stubGraph is feature 001's read surface, answered in memory.
//
// It ranks with `internal/query.Rank` rather than with a ranking of its own, so the ordering the
// runner persists is feature 001's published formula, and it resolves every reference to the same
// canonical entity id the graph would mint — which is what makes the resolution audit the runner
// records an audit rather than a formality.
type stubGraph struct{}

// Subgraph describes the neighbourhood: three services and the CALLS edges between them.
//
// The engine reads it before anything else and depends on what it says — a change's targets come
// back from `Diff` as canonical entity ids while every read takes a reference, and a span query
// names the edge's own published type — so a stub that answered nothing would be a graph unable
// to describe its own topology.
func (stubGraph) Subgraph(context.Context, *graphv1.SubgraphRequest) (*graphv1.SubgraphResponse, error) {
	return &graphv1.SubgraphResponse{
		Focus: serviceNode(checkoutRef),
		Nodes: []*graphv1.NodeVersion{serviceNode(paymentsRef), serviceNode(storefrontRef)},
		Edges: []*graphv1.EdgeVersion{
			callsEdge(checkoutRef, paymentsRef, 1),
			callsEdge(checkoutRef, storefrontRef, 3),
		},
	}, nil
}

// serviceNode is one service as this stub publishes it. The entity id is the reference itself,
// which is this stub's convention throughout and is what the synthetic scenario beside it keys
// its entities by; a real graph mints an opaque id and publishes the reference as an alias, and
// that translation is exercised against the corpus in `internal/cli` rather than here.
func serviceNode(ref string) *graphv1.NodeVersion {
	namespace, value, _ := strings.Cut(ref, "=")
	return &graphv1.NodeVersion{
		EntityId:    ref,
		VersionId:   ref + "@1",
		Type:        graphv1.NodeType_SERVICE,
		DisplayName: value,
		Aliases:     []*graphv1.Ref{{Namespace: namespace, Value: value}},
	}
}

func callsEdge(src, dst string, weight uint32) *graphv1.EdgeVersion {
	return &graphv1.EdgeVersion{
		SrcId: src, DstId: dst, Type: graphv1.EdgeType_CALLS, WeightClass: &weight,
	}
}

func (stubGraph) Impact(context.Context, *graphv1.ImpactRequest) (*graphv1.ImpactResponse, error) {
	return &graphv1.ImpactResponse{}, nil
}

func (stubGraph) NodeHistory(context.Context, *graphv1.NodeHistoryRequest) (*graphv1.NodeHistoryResponse, error) {
	return &graphv1.NodeHistoryResponse{}, nil
}

func (stubGraph) ResolutionAudit(_ context.Context, req *graphv1.ResolutionAuditRequest) (*graphv1.ResolutionAuditResponse, error) {
	ref := graph.RefFromProto(req.GetA())
	if ref.IsZero() {
		return &graphv1.ResolutionAuditResponse{}, nil
	}
	return &graphv1.ResolutionAuditResponse{
		SameEntity:  true,
		CanonicalId: graph.EntityID(ref.Namespace, ref.Value),
	}, nil
}

func (stubGraph) Extent(context.Context, *graphv1.ExtentRequest) (*graphv1.Extent, error) {
	return &graphv1.Extent{
		EarliestObserved: timestamppb.New(windowStart.Add(-48 * time.Hour)),
		LatestObserved:   timestamppb.New(firedAt),
	}, nil
}

func (stubGraph) Pointers(_ context.Context, req *graphv1.PointersRequest) (*graphv1.PointersResponse, error) {
	entity := req.GetFocus().GetNamespace() + "=" + req.GetFocus().GetValue()
	name := req.GetFocus().GetValue()
	return &graphv1.PointersResponse{
		Node: &graphv1.NodeVersion{EntityId: entity, Type: graphv1.NodeType_SERVICE, DisplayName: name},
		ByKind: map[string]*graphv1.PointerList{
			"metric": {Pointers: []*graphv1.Pointer{{
				Kind:        graphv1.PointerKind_METRIC,
				BackendKind: "synthetic",
				Vocabulary:  "promql",
				Selector:    "http_errors{service=\"" + name + "\"}",
				JoinKeys:    map[string]string{"version": "service.version", "workload": "k8s.deployment.name"},
			}}},
			"log": {Pointers: []*graphv1.Pointer{{
				Kind:        graphv1.PointerKind_LOG,
				BackendKind: "synthetic",
				Vocabulary:  "logql",
				Selector:    "{service=\"" + name + "\"}",
				JoinKeys:    map[string]string{"version": "service.version"},
			}}},
		},
	}, nil
}

func (stubGraph) Diff(_ context.Context, req *graphv1.DiffRequest) (*graphv1.DiffResponse, error) {
	reference := req.GetT2().AsTime().UTC()
	if req.GetReferenceAt() != nil {
		reference = req.GetReferenceAt().AsTime().UTC()
	}
	params := query.RankParams{Reference: reference, Tau: query.DefaultTau, HopCap: 3}
	ranked, _ := query.Rank([]query.Candidate{
		{
			Change:    changeNode(rolloutChange, rolloutAt, graphv1.ChangeKind_ROLLOUT, graphv1.ActorKind_PERSON),
			TargetIDs: []string{paymentsRef}, Hop: 1, WeightClass: 1,
		},
		{
			Change:    changeNode(scalingChange, scalingAt, graphv1.ChangeKind_SCALING, graphv1.ActorKind_CONTROLLER),
			TargetIDs: []string{storefrontRef}, Hop: 1, WeightClass: 1,
		},
	}, params)
	return &graphv1.DiffResponse{Changes: ranked, RankingFormula: query.RankingFormula(params)}, nil
}

func changeNode(entityID string, at time.Time, kind graphv1.ChangeKind, actor graphv1.ActorKind) *graphv1.NodeVersion {
	return &graphv1.NodeVersion{
		EntityId:  entityID,
		VersionId: entityID + "@1",
		Type:      graphv1.NodeType_CHANGE,
		Valid:     &graphv1.Interval{Start: timestamppb.New(at)},
		Change:    &graphv1.Change{Kind: kind, ActorKind: actor},
	}
}

// scenario is the synthetic world the telemetry workers answer from: the rollout degrades
// payments and checkout, the scaling degrades nothing, which is what makes it a decoy rather than
// a second culprit.
func scenario() synthetic.Scenario {
	return synthetic.Scenario{
		Seed:       "runner-rollout-regression",
		Start:      windowStart,
		End:        firedAt.Add(5 * time.Minute),
		Resolution: time.Minute,
		Entities: []synthetic.Entity{
			{EntityID: checkoutRef, Name: "checkout",
				Selectors:       []string{"http_errors{service=\"checkout\"}", "{service=\"checkout\"}"},
				BaselineVersion: "rev3"},
			{EntityID: paymentsRef, Name: "payments",
				Selectors:       []string{"http_errors{service=\"payments\"}", "{service=\"payments\"}"},
				BaselineVersion: "rev6"},
			{EntityID: storefrontRef, Name: "storefront",
				Selectors:       []string{"http_errors{service=\"storefront\"}", "{service=\"storefront\"}"},
				BaselineVersion: "rev2"},
		},
		Edges: []synthetic.Edge{
			{SrcEntityID: checkoutRef, DstEntityID: paymentsRef, WeightClass: 1},
			{SrcEntityID: checkoutRef, DstEntityID: storefrontRef, WeightClass: 3},
		},
		Changes: []synthetic.Change{
			{EntityID: rolloutChange, At: rolloutAt, TargetEntityIDs: []string{paymentsRef, checkoutRef},
				Version: "rev7", Degrades: true},
			{EntityID: scalingChange, At: scalingAt, TargetEntityIDs: []string{storefrontRef},
				Version: "scale-1415", Degrades: false},
		},
	}
}

// --- the harness -----------------------------------------------------------------------------

type harness struct {
	runner    *runner.Runner
	store     *postgres.Store
	dao       *investigationstore.InvestigationDAO
	ledgerDAO *investigationstore.LedgerDAO
	recording string
}

// newHarness builds the runner over a fresh database. `turns` nil means the model-free run, which
// is what every test but one uses.
func newHarness(t *testing.T, turns []modeltest.Turn) *harness {
	t.Helper()

	store := pgtest.Open(t)
	backend, err := synthetic.New(scenario())
	if err != nil {
		t.Fatalf("synthetic backend: %v", err)
	}
	registry := worker.NewRegistry()
	for _, w := range []worker.Worker{
		metrics.New(backend), logs.New(backend), traces.New(backend),
	} {
		if err := registry.Register(w); err != nil {
			t.Fatalf("register %T: %v", w, err)
		}
	}

	recording := t.TempDir()
	r, err := runner.New(runner.Config{
		Store:         store,
		Graph:         stubGraph{},
		Workers:       registry,
		Model:         modelClient(t, turns),
		RecordingRoot: recording,
		Prior:         publishedPrior(t),
		Mode:          worker.ModeRecorded,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}
	return &harness{
		runner:    r,
		store:     store,
		dao:       investigationstore.NewInvestigationDAO(store),
		ledgerDAO: investigationstore.NewLedgerDAO(store),
		recording: recording,
	}
}

// publishedPrior is π₀ from the checked-in September 2026 aggregate — the same number a deployed
// engine reads out of `investigation.coverage_audits` (ADR-0005 D9).
func publishedPrior(t *testing.T) audit.PriorRecord {
	t.Helper()
	result, err := audit.LoadResult(filepath.Join("..", "..", "..", "docs", "evaluation",
		"coverage-audit-2026-09.json"))
	if err != nil {
		t.Fatalf("load the published coverage audit: %v", err)
	}
	return audit.PriorRecordFromAudit(result)
}

// modelClient returns nil for the model-free runs and a canned transport otherwise. There is no
// path here that reaches a network or reads a credential.
func modelClient(t *testing.T, turns []modeltest.Turn) *model.Client {
	t.Helper()
	if turns == nil {
		return nil
	}
	config, prices, err := model.LoadPair(
		filepath.Join("..", "..", "..", "config", "model.yaml"),
		filepath.Join("..", "..", "..", "config", "prices.yaml"))
	if err != nil {
		t.Fatalf("model config: %v", err)
	}
	transport, err := modeltest.Transport(config.Roles[model.RoleInvestigator].Model, turns...)
	if err != nil {
		t.Fatalf("canned transport: %v", err)
	}
	client, err := model.NewClientWithTransport(config, prices, transport)
	if err != nil {
		t.Fatalf("model client: %v", err)
	}
	return client
}

// investigateRequest is the fixture's own intake: a Datadog monitor on checkout firing at 14:32,
// looking back 90 minutes, on the `page` profile (fixtures/incidents/
// rollout-regression-01-incident/manifest.yaml §incident.question).
func investigateRequest() *investigationv1.InvestigateRequest {
	ref, _ := graph.ParseRef(checkoutRef)
	return &investigationv1.InvestigateRequest{
		Symptom: &investigationv1.Symptom{
			OriginSystem:     "datadog",
			OriginRef:        "monitor=12345",
			Transport:        intake.TransportMonitor,
			Statement:        "checkout error rate above 5 % for 5 minutes",
			FiredAt:          timestamppb.New(firedAt),
			NamedIdentifiers: []*graphv1.Ref{ref.Proto()},
			Severity:         "P1",
		},
		ValidAt:         timestamppb.New(firedAt),
		LookbackSeconds: int64((90 * time.Minute) / time.Second),
		Profile:         "page",
	}
}

// collector records every state the anytime callback publishes.
type collector struct {
	states []*investigationv1.Investigation
}

func (c *collector) emit(inv *investigationv1.Investigation) error {
	c.states = append(c.states, inv)
	return nil
}

// --- tests -----------------------------------------------------------------------------------

// TestInvestigateRunsEndToEndWithNoModel is the MVP through the productised pipeline (FR-067).
//
// One call, and everything it is supposed to leave behind: a concluded row, a ranked ledger with
// the rollout first and the post-onset scaling exonerated, the anytime shape published before the
// evidence existed, exactly one decision record in the graph, a trajectory on disk, and a ledger
// that reloads from the database with the same numbers.
func TestInvestigateRunsEndToEndWithNoModel(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newHarness(t, nil)

	var states collector
	final, err := h.runner.Investigate(ctx, investigateRequest(), requester, states.emit)
	if err != nil {
		t.Fatalf("investigate: %v", err)
	}

	// FR-046a: the provisional prior-only answer is published first and says so.
	if len(states.states) < 2 {
		t.Fatalf("emit was called %d time(s); the anytime shape is the provisional answer and then "+
			"the concluded one (FR-046a)", len(states.states))
	}
	if !states.states[0].GetProvisional() {
		t.Error("the first published state is not marked provisional; nothing had been tested yet")
	}
	if states.states[0].GetLedger() == nil || len(states.states[0].GetLedger().GetHypotheses()) < 2 {
		t.Error("the provisional state carries no ranked candidates")
	}
	if final.GetProvisional() {
		t.Error("the final state is still marked provisional")
	}

	// The terminal state.
	if final.GetLifecycle() != investigationv1.Lifecycle_CONCLUDED {
		t.Fatalf("lifecycle = %v, want CONCLUDED (stop %v: %q)",
			final.GetLifecycle(), final.GetStopReason(), final.GetStopDetail())
	}
	if final.GetConclusionKind() != investigationv1.ConclusionKind_CONCLUSION_FINAL {
		t.Errorf("conclusion kind = %v, want final", final.GetConclusionKind())
	}
	if final.GetVerdictLine() == "" {
		t.Error("the run concluded with no verdict line (FR-057c)")
	}
	if final.GetRequester() != requester {
		t.Errorf("requester = %q, want the authenticated caller (FR-066)", final.GetRequester())
	}

	// The ranking, read back out of the database rather than out of the engine's memory.
	rows, err := h.ledgerDAO.Load(ctx, final.GetInvestigationId())
	if err != nil {
		t.Fatalf("load ledger: %v", err)
	}
	if len(rows.Hypotheses) < 3 {
		t.Fatalf("the stored ledger holds %d hypotheses, want the two candidates and the open one",
			len(rows.Hypotheses))
	}
	top := rows.Hypotheses[0]
	if top.CandidateChangeEntityID != rolloutChange {
		t.Errorf("rank 1 is %s (%s); want the rollout %s", top.ID, top.CandidateChangeEntityID, rolloutChange)
	}
	scaling := hypothesisFor(t, rows.Hypotheses, scalingChange)
	if scaling.Status != ledger.StatusExonerated {
		t.Errorf("the post-onset scaling is %s in the database, want exonerated (FR-029c)", scaling.Status)
	}
	if scaling.OnsetEvidenceID == "" {
		t.Error("the stored exoneration rests on no onset estimate (Invariant 9)")
	}

	// Invariant 1: the stored confidences are the ones the report published.
	byID := map[string]float64{}
	for _, h := range final.GetLedger().GetHypotheses() {
		byID[h.GetHypothesisId()] = h.GetConfidence()
	}
	for _, h := range rows.Hypotheses {
		if published, ok := byID[h.ID]; ok && published != h.Confidence {
			t.Errorf("%s: the database holds %.6f and the report published %.6f", h.ID, h.Confidence, published)
		}
	}

	// Every evidence item carries the source-of-truth declaration 0008 added (FR-022).
	items, err := h.dao.EvidenceChainItems(ctx, final.GetInvestigationId())
	if err != nil {
		t.Fatalf("evidence chain: %v", err)
	}
	if len(items) == 0 {
		t.Fatal("the run recorded no evidence")
	}
	var declared int
	for _, item := range items {
		if item.SourceOfTruth != "" {
			declared++
		}
	}
	if declared == 0 {
		t.Error("no evidence item records the source of truth it came from (FR-022, 0008)")
	}

	// FR-035, constitution III: exactly one event, and nothing else from the run reached the log.
	assertOneDecisionRecord(ctx, t, h.store)

	// FR-033: the calls are rows, in one interleaved sequence space.
	var calls int
	if err := h.store.Pool().QueryRow(ctx,
		`SELECT count(*) FROM investigation.worker_calls WHERE investigation_id = $1`,
		final.GetInvestigationId()).Scan(&calls); err != nil {
		t.Fatalf("count worker calls: %v", err)
	}
	if calls == 0 {
		t.Error("the run recorded no worker calls (FR-033)")
	}

	// FR-044: consumption against every budget.
	spend, err := h.dao.SpendOf(ctx, final.GetInvestigationId())
	if err != nil {
		t.Fatalf("spend: %v", err)
	}
	if spend == nil || spend.GetLimits().GetName() != "page" {
		t.Errorf("the run recorded no spend against the profile it ran under: %+v", spend)
	}

	// FR-042a: layer 1 is on disk, under the recording root, and the row points at it.
	if final.GetRecordingKey() == "" || final.GetRecordingDigest() == "" {
		t.Errorf("the row names no recording: key %q digest %q",
			final.GetRecordingKey(), final.GetRecordingDigest())
	}
	if _, err := os.Stat(filepath.Join(h.recording, final.GetRecordingKey())); err != nil {
		t.Errorf("the trajectory the row names is not on disk: %v", err)
	}
}

// TestARedeliveredAlertDoesNotOpenASecondInvestigation is FR-008b through the runner.
func TestARedeliveredAlertDoesNotOpenASecondInvestigation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newHarness(t, nil)

	first, err := h.runner.Investigate(ctx, investigateRequest(), requester, nil)
	if err != nil {
		t.Fatalf("first investigate: %v", err)
	}
	callsBefore := countCalls(ctx, t, h.store, first.GetInvestigationId())

	var states collector
	second, err := h.runner.Investigate(ctx, investigateRequest(), requester, states.emit)
	if err != nil {
		t.Fatalf("re-delivery: %v", err)
	}
	if second.GetInvestigationId() != first.GetInvestigationId() {
		t.Fatalf("re-delivery opened %s; the first investigation is %s (FR-008b)",
			second.GetInvestigationId(), first.GetInvestigationId())
	}
	if got := countCalls(ctx, t, h.store, first.GetInvestigationId()); got != callsBefore {
		t.Errorf("the re-delivery issued %d further worker calls; it is a no-op that returns the "+
			"existing investigation", got-callsBefore)
	}
	if !second.GetEndedAt().AsTime().Equal(first.GetEndedAt().AsTime()) {
		t.Error("the re-delivery moved the conclusion instant; the record is immutable (FR-007)")
	}
	if len(states.states) != 0 {
		t.Errorf("the re-delivery published %d anytime states; nothing ran", len(states.states))
	}
	assertOneDecisionRecord(ctx, t, h.store)
}

// TestDeclareOpensAnInvestigationAndIsIdempotent is User Story 1a: a person declares an incident
// with no monitor, and re-delivering the identical declaration is a no-op (FR-001a, FR-008b).
func TestDeclareOpensAnInvestigationAndIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newHarness(t, nil)

	declaredAt := firedAt.Add(-time.Minute)
	ref, _ := graph.ParseRef(checkoutRef)
	req := &investigationv1.DeclareRequest{
		Declaration: &investigationv1.Symptom{
			OriginSystem:   "slack",
			OriginRef:      "slack:C0123STABLE",
			Transport:      intake.TransportHumanDeclared,
			Statement:      "checkout is throwing 500s",
			Title:          "checkout is throwing 500s",
			Severity:       "sev2",
			FiredAt:        timestamppb.New(declaredAt),
			Origin:         investigationv1.IntakeOrigin_INTAKE_ORIGIN_DECLARED,
			TargetRefs:     []*investigationv1.TargetRef{{Ref: ref.Proto(), Provenance: investigationv1.TargetRefProvenance_SUPPLIED_BY_HUMAN}},
			IdempotencyKey: intake.DeclarationKey("slack", "C0123STABLE", declaredAt),
		},
		Profile:         "page",
		LookbackSeconds: int64((90 * time.Minute) / time.Second),
	}

	first, err := h.runner.Declare(ctx, req, requester, nil)
	if err != nil {
		t.Fatalf("declare: %v", err)
	}
	if first.GetLifecycle() != investigationv1.Lifecycle_CONCLUDED {
		t.Fatalf("lifecycle = %v, want CONCLUDED (%q)", first.GetLifecycle(), first.GetStopDetail())
	}
	// FR-004a: the pin is the instant of declaration, never the instant the engine saw it.
	if got := first.GetValidAt().AsTime().UTC(); !got.Equal(declaredAt) {
		t.Errorf("valid_at = %s, want the declared instant %s", got, declaredAt)
	}
	if got := first.GetObservedAt().AsTime().UTC(); !got.Equal(declaredAt) {
		t.Errorf("observed_at = %s, want the declared instant %s (FR-004a)", got, declaredAt)
	}
	if len(first.GetSymptoms()) != 1 || first.GetSymptoms()[0].GetDeclaringIdentity() != requester {
		t.Errorf("the declaration does not record who declared it (FR-002a, FR-066): %+v", first.GetSymptoms())
	}

	second, err := h.runner.Declare(ctx, req, requester, nil)
	if err != nil {
		t.Fatalf("re-declare: %v", err)
	}
	if second.GetInvestigationId() != first.GetInvestigationId() {
		t.Errorf("re-delivery opened %s, want the existing %s (FR-008b)",
			second.GetInvestigationId(), first.GetInvestigationId())
	}
	assertOneDecisionRecord(ctx, t, h.store)
}

// TestADeclarationWithNoTargetReachesUnknownRatherThanGuessing is FR-002b.
func TestADeclarationWithNoTargetReachesUnknownRatherThanGuessing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newHarness(t, nil)

	declaredAt := firedAt.Add(-2 * time.Minute)
	inv, err := h.runner.Declare(ctx, &investigationv1.DeclareRequest{
		Declaration: &investigationv1.Symptom{
			OriginSystem: "slack",
			OriginRef:    "slack:C0999NOTARGET",
			Transport:    intake.TransportHumanDeclared,
			Statement:    "something is wrong with the shop",
			Title:        "something is wrong with the shop",
			Severity:     "sev3",
			FiredAt:      timestamppb.New(declaredAt),
			Origin:       investigationv1.IntakeOrigin_INTAKE_ORIGIN_DECLARED,
		},
		Profile: "page",
	}, requester, nil)
	if err != nil {
		t.Fatalf("declare: %v", err)
	}
	if inv.GetOutcome() != investigationv1.InvestigationOutcome_UNKNOWN {
		t.Errorf("outcome = %v, want UNKNOWN (FR-002b, FR-026)", inv.GetOutcome())
	}
	if inv.GetLifecycle() != investigationv1.Lifecycle_CONCLUDED {
		t.Errorf("lifecycle = %v, want CONCLUDED; the incident is not dropped", inv.GetLifecycle())
	}
	var asked bool
	for _, res := range inv.GetResolutions() {
		if res.GetKind() == intake.ResolutionKindConfirmIdentity {
			asked = true
		}
	}
	if !asked {
		t.Errorf("the investigation asks for nothing; FR-002b's resolving action is %q: %+v",
			intake.MissingTargetResolution, inv.GetResolutions())
	}
	// Nothing was invented.
	for _, h := range inv.GetLedger().GetHypotheses() {
		if h.GetCandidateChangeEntityId() != "" {
			t.Errorf("a candidate change was invented for a declaration that named no service: %s",
				h.GetCandidateChangeEntityId())
		}
	}
}

// TestReopenLinksANewRunAndLeavesTheParentAsProduced is FR-057b and SC-024.
func TestReopenLinksANewRunAndLeavesTheParentAsProduced(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newHarness(t, nil)

	parent, err := h.runner.Investigate(ctx, investigateRequest(), requester, nil)
	if err != nil {
		t.Fatalf("investigate: %v", err)
	}
	parentVerdict := parent.GetVerdictLine()

	child, err := h.runner.Reopen(ctx, parent.GetInvestigationId(), &investigationv1.HumanFact{
		Kind:      investigationstore.FactKindManualAction,
		Statement: "I restarted the payments pods by hand at 14:25",
		EntityIds: []string{paymentsRef},
		Author:    requester,
	}, requester)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}

	if child.GetInvestigationId() == parent.GetInvestigationId() {
		t.Fatal("the reopen edited the parent; a reopen is a new linked row (FR-007, FR-057b)")
	}
	if child.GetReopensInvestigationId() != parent.GetInvestigationId() {
		t.Errorf("the child links to %q, want %q",
			child.GetReopensInvestigationId(), parent.GetInvestigationId())
	}
	if child.GetIncidentId() != parent.GetIncidentId() {
		t.Errorf("the child covers incident %q, want the parent's %q",
			child.GetIncidentId(), parent.GetIncidentId())
	}
	if child.GetLifecycle() != investigationv1.Lifecycle_CONCLUDED {
		t.Errorf("the child is %v, want CONCLUDED (%q)", child.GetLifecycle(), child.GetStopDetail())
	}
	// The child inherits the parent's question, not its answer: the same two instants.
	if !child.GetValidAt().AsTime().Equal(parent.GetValidAt().AsTime()) ||
		!child.GetObservedAt().AsTime().Equal(parent.GetObservedAt().AsTime()) {
		t.Error("the reopen re-pinned the instants; it would then see everything learned since")
	}
	// The fact landed on the child, weighted strong and never decisive (FR-057a).
	if len(child.GetFacts()) != 1 {
		t.Fatalf("the child holds %d facts, want the one that reopened it", len(child.GetFacts()))
	}
	if got := child.GetFacts()[0].GetWeightClass(); got != investigationstore.WeightClassStrong {
		t.Errorf("weight class = %q, want strong and never decisive", got)
	}
	var inLedger bool
	for _, e := range child.GetLedger().GetEvidence() {
		if e.GetEvidenceId() == child.GetFacts()[0].GetEvidenceId() {
			inLedger = true
		}
	}
	if !inLedger {
		t.Error("the fact is not in the child's ledger as an evidence item (FR-057a)")
	}

	// The parent is readable exactly as produced, with only its status moved.
	reread, err := h.dao.Get(ctx, parent.GetInvestigationId())
	if err != nil {
		t.Fatalf("re-read the parent: %v", err)
	}
	if reread.GetLifecycle() != investigationv1.Lifecycle_REOPENED {
		t.Errorf("parent lifecycle = %v, want REOPENED", reread.GetLifecycle())
	}
	if reread.GetVerdictLine() != parentVerdict {
		t.Errorf("the parent's verdict changed to %q", reread.GetVerdictLine())
	}
	if len(reread.GetFacts()) != 0 {
		t.Error("the fact landed on the parent; it belongs to the run that will use it")
	}

	// Two runs, two decision records — one each, which is what "exactly one per investigation"
	// means when there are two investigations.
	if got := countDecisionRecords(ctx, t, h.store); got != 2 {
		t.Errorf("the log holds %d record_investigation events for a parent and a child, want 2", got)
	}
}

// TestTheModelBoundaryIsRecorded hands the runner a canned transport and asserts the model calls
// reach the database (FR-033, FR-061).
func TestTheModelBoundaryIsRecorded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	h := newHarness(t, []modeltest.Turn{
		{
			Text: "The graph puts a second candidate near the subject; recording it.",
			ToolUses: []modeltest.ToolUse{{
				ID: "toolu_1", Name: engine.ToolProposeHypothesis, Input: map[string]any{
					"kind":               "condition",
					"statement":          "a downstream dependency is saturated",
					"target_entity_refs": []string{paymentsRef},
				},
			}},
			Usage: model.Usage{Input: 12000, CacheWrite: 9000, Output: 300},
		},
		{
			Text: "The version split is decisive; recording the judgment.",
			ToolUses: []modeltest.ToolUse{{
				ID: "toolu_2", Name: engine.ToolProposeJudgments, Input: map[string]any{
					"judgments": []map[string]any{{
						"hypothesis_id": "h-1", "evidence_id": "e-2",
						"direction": "supports", "strength": "moderate",
						"rationale": "the graph puts the rollout one hop from the subject, before onset",
					}},
				},
			}},
			Usage: model.Usage{Input: 400, CacheRead: 9000, Output: 200},
		},
		{Text: "Verdict: the rollout.", Usage: model.Usage{Input: 400, CacheRead: 9000, Output: 120}},
	})

	final, err := h.runner.Investigate(ctx, investigateRequest(), requester, nil)
	if err != nil {
		t.Fatalf("investigate: %v", err)
	}
	if final.GetLifecycle() != investigationv1.Lifecycle_CONCLUDED {
		t.Fatalf("lifecycle = %v, want CONCLUDED (%q)", final.GetLifecycle(), final.GetStopDetail())
	}

	rows, err := h.store.Pool().Query(ctx, `
		SELECT seq, role, model_id, request_digest, response_digest,
		       input_tokens, cache_write_tokens, cache_read_tokens, output_tokens
		FROM investigation.model_calls WHERE investigation_id = $1 ORDER BY seq`,
		final.GetInvestigationId())
	if err != nil {
		t.Fatalf("read model calls: %v", err)
	}
	defer rows.Close()

	var calls, tokens int
	for rows.Next() {
		var (
			seq                                      int
			role, modelID, reqDigest, respDigest     string
			input, cacheWrite, cacheRead, outputToks int
		)
		if err := rows.Scan(&seq, &role, &modelID, &reqDigest, &respDigest,
			&input, &cacheWrite, &cacheRead, &outputToks); err != nil {
			t.Fatalf("scan model call: %v", err)
		}
		calls++
		tokens += input + cacheWrite + cacheRead + outputToks
		if role != string(model.RoleInvestigator) {
			t.Errorf("model call %d has role %q, want investigator", seq, role)
		}
		if reqDigest == "" || respDigest == "" {
			t.Errorf("model call %d is not digested; the replay gate matches on those", seq)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read model calls: %v", err)
	}
	if calls == 0 {
		t.Fatal("the model boundary produced no model_calls rows (FR-033)")
	}
	if tokens == 0 {
		t.Error("the model calls record no tokens; the spend report is derived from them (FR-048)")
	}

	// The model's own judgment is in the ledger, sourced `model` and carrying the published ln LR
	// rather than a number the model chose (FR-023).
	stored, err := h.ledgerDAO.Load(ctx, final.GetInvestigationId())
	if err != nil {
		t.Fatalf("load ledger: %v", err)
	}
	var applied bool
	for _, j := range stored.Judgments {
		if j.Source == ledger.SourceModel {
			applied = true
			if j.LnLR != ledger.LnLRModerate {
				t.Errorf("ln LR = %v, want the published %v", j.LnLR, ledger.LnLRModerate)
			}
		}
	}
	if !applied {
		t.Error("the model proposed a judgment and the ledger holds none from it")
	}
}

// TestExportThenReplayRoundTripsWithNoNetwork is the plumbing gate through the runner
// (FR-039–FR-042, SC-003).
//
// Export what the run left behind, replay the artifact's trajectory layer, and assert it
// reproduces itself byte for byte. The replay makes no network call and touches no database: the
// whole point of layer 1 is that a recorded run is re-derivable from the file alone.
func TestExportThenReplayRoundTripsWithNoNetwork(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newHarness(t, nil)

	inv, err := h.runner.Investigate(ctx, investigateRequest(), requester, nil)
	if err != nil {
		t.Fatalf("investigate: %v", err)
	}

	out := filepath.Join(t.TempDir(), "export")
	exported, err := h.runner.Export(ctx, &investigationv1.ExportRequest{
		InvestigationId: inv.GetInvestigationId(), OutDir: out,
	})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if exported.GetExportDigest() == "" {
		t.Error("the export carries no digest; a reviewer compares artifacts by it (FR-042)")
	}

	replayed, err := h.runner.Replay(ctx, &investigationv1.ReplayRequest{
		InvestigationId: inv.GetInvestigationId(), ExportPath: out, Layer: "trajectory",
	})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	// **Identical**, not merely typed.
	//
	// This assertion used to be "identical, or a named divergence", and the comment explaining
	// why said that asserting `identical` was the replay gate's own job. It was not: what it was
	// really accommodating was a defect. `pkg/backend.ResponseDigest` hashed the response's
	// `mode` field, while the world recorder cleared that field before hashing and every reader
	// stamps its own mode afterwards — so a runner-recorded answer claimed a digest its own
	// content no longer hashed to, and every replay of a runner recording diverged at the first
	// telemetry answer. The digest rule now excludes `mode`, `duration_ms` and the digest field
	// itself (`pkg/backend.NormaliseForDigest`), and a run recorded here replays byte for byte.
	if !replayed.GetIdentical() {
		t.Errorf("a runner-recorded trajectory did not replay identically: %s",
			replayed.GetFirstDivergingRecord())
	}
	if replayed.GetFirstDivergingRecord() != "" {
		t.Errorf("the replay named a divergence: %s", replayed.GetFirstDivergingRecord())
	}

	// The four export-source reads, on their own, because they are what the fixture harness will
	// compose differently (T096).
	investigation, err := h.runner.Investigation(ctx, inv.GetInvestigationId())
	if err != nil {
		t.Fatalf("export source: investigation: %v", err)
	}
	if investigation.GetLedger() == nil || len(investigation.GetLedger().GetHypotheses()) == 0 {
		t.Error("the exported investigation carries no ledger")
	}
	trajectories, err := h.runner.Trajectories(ctx, inv.GetInvestigationId())
	if err != nil {
		t.Fatalf("export source: trajectories: %v", err)
	}
	if len(trajectories) != 1 {
		t.Errorf("the export source found %d trajectories, want the one this run recorded",
			len(trajectories))
	}
	events, err := h.runner.GraphEvents(ctx, inv.GetInvestigationId())
	if err != nil {
		t.Fatalf("export source: graph events: %v", err)
	}
	_ = events // an empty log is legal here: the graph in this harness is a stub, not a projection

	// An unreadable path is an error; a divergence is not. Both are typed, and neither is a nil
	// response with a nil error.
	if _, err := h.runner.Replay(ctx, &investigationv1.ReplayRequest{
		InvestigationId: inv.GetInvestigationId(),
		ExportPath:      filepath.Join(out, "no-such-directory"),
		Layer:           "trajectory",
	}); err == nil {
		t.Error("replaying a directory that does not exist reported success")
	}
}

// TestAnUnconfiguredTelemetryBackendConcludesRatherThanFailing is FR-027 applied to the
// configuration: a deployment with no telemetry recording still reaches a terminal state, and
// every hypothesis it could not test says why.
func TestAnUnconfiguredTelemetryBackendConcludesRatherThanFailing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	store := pgtest.Open(t)
	unconfigured := runner.UnconfiguredTelemetry("no telemetry backend is configured in this test")
	registry := worker.NewRegistry()
	for _, w := range []worker.Worker{
		metrics.New(unconfigured), logs.New(unconfigured), traces.New(unconfigured),
	} {
		if err := registry.Register(w); err != nil {
			t.Fatalf("register %T: %v", w, err)
		}
	}
	r, err := runner.New(runner.Config{
		Store:   store,
		Graph:   stubGraph{},
		Workers: registry,
		Prior:   publishedPrior(t),
		Mode:    worker.ModeLive,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}

	inv, err := r.Investigate(ctx, investigateRequest(), requester, nil)
	if err != nil {
		t.Fatalf("investigate: %v", err)
	}
	if inv.GetLifecycle() != investigationv1.Lifecycle_CONCLUDED {
		t.Fatalf("lifecycle = %v, want CONCLUDED: a missing backend is a finding, not an engine "+
			"failure (%q)", inv.GetLifecycle(), inv.GetStopDetail())
	}

	// Every telemetry call is a `failed` row with a reason, and not one of them is `empty`:
	// only `no_data` means "nothing happened in production", and this deployment did not look.
	rows, err := store.Pool().Query(ctx,
		`SELECT worker, outcome FROM investigation.worker_calls WHERE investigation_id = $1`,
		inv.GetInvestigationId())
	if err != nil {
		t.Fatalf("read worker calls: %v", err)
	}
	defer rows.Close()
	var telemetryCalls int
	for rows.Next() {
		var workerName, outcome string
		if err := rows.Scan(&workerName, &outcome); err != nil {
			t.Fatalf("scan worker call: %v", err)
		}
		if workerName == "graph" {
			continue
		}
		telemetryCalls++
		if outcome == investigationstore.CallOutcomeEmpty {
			t.Errorf("%s recorded `empty`; an unanswerable question is not evidence that nothing "+
				"happened (FR-027)", workerName)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read worker calls: %v", err)
	}
	if telemetryCalls == 0 {
		t.Error("no telemetry call was recorded at all; the refusal has to be in the record")
	}
	assertOneDecisionRecord(ctx, t, store)
}

// TestACancelledInvestigationEndsFailedRatherThanRunning (FR-006).
func TestACancelledInvestigationEndsFailedRatherThanRunning(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := h.runner.Investigate(ctx, investigateRequest(), requester, nil)
	if err == nil {
		t.Fatal("a cancelled investigation returned no error")
	}

	// Whatever row it opened is terminal. A `running` row nobody is running is the one state
	// this design does not have.
	read := context.Background()
	rows, qerr := h.store.Pool().Query(read,
		`SELECT investigation_id, lifecycle FROM investigation.investigations`)
	if qerr != nil {
		t.Fatalf("read investigations: %v", qerr)
	}
	defer rows.Close()
	for rows.Next() {
		var id, lifecycle string
		if err := rows.Scan(&id, &lifecycle); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if lifecycle == investigationstore.LifecycleRunning {
			t.Errorf("investigation %s is still `running` after the caller went away", id)
		}
	}
}

// --- helpers ---------------------------------------------------------------------------------

func hypothesisFor(t *testing.T, hypotheses []ledger.Hypothesis, changeEntityID string) ledger.Hypothesis {
	t.Helper()
	for _, h := range hypotheses {
		if h.CandidateChangeEntityID == changeEntityID {
			return h
		}
	}
	t.Fatalf("no hypothesis for %s", changeEntityID)
	return ledger.Hypothesis{}
}

func countCalls(ctx context.Context, t *testing.T, store *postgres.Store, investigationID string) int {
	t.Helper()
	var n int
	if err := store.Pool().QueryRow(ctx,
		`SELECT count(*) FROM investigation.worker_calls WHERE investigation_id = $1`,
		investigationID).Scan(&n); err != nil {
		t.Fatalf("count worker calls: %v", err)
	}
	return n
}

func countDecisionRecords(ctx context.Context, t *testing.T, store *postgres.Store) int {
	t.Helper()
	var n int
	if err := store.Pool().QueryRow(ctx,
		`SELECT count(*) FROM log.events WHERE type = 'record_investigation'`).Scan(&n); err != nil {
		t.Fatalf("count decision records: %v", err)
	}
	return n
}

// assertOneDecisionRecord is constitution III: one investigation, one event, and no working
// material in the log.
func assertOneDecisionRecord(ctx context.Context, t *testing.T, store *postgres.Store) {
	t.Helper()
	if got := countDecisionRecords(ctx, t, store); got != 1 {
		t.Errorf("the log holds %d record_investigation events, want exactly 1 (FR-035)", got)
	}
	var others int
	if err := store.Pool().QueryRow(ctx,
		`SELECT count(*) FROM log.events WHERE type <> 'record_investigation'`).Scan(&others); err != nil {
		t.Fatalf("count other events: %v", err)
	}
	if others != 0 {
		t.Errorf("the run emitted %d events other than its decision record; judgments, worker "+
			"calls and ledger updates are never events (constitution III)", others)
	}
}

// sdk is imported for the telemetry backend type the unconfigured backend satisfies.
var _ sdk.TelemetryBackend = runner.UnconfiguredTelemetry("")

// TestARunnerRecordingReplaysWithoutLeavingTheRecordingRoot is the tighter half of the same
// property: no export, no artifact, just the file the runner wrote and the replay of it.
//
// Export adds a layout, a manifest and a digest over the whole directory, all of which can hide a
// per-record problem behind a whole-artifact assertion. This goes straight at the recording the
// runner left behind, so what it proves is exactly "the bytes the engine recorded are the bytes
// the replayer reproduces" and nothing else.
func TestARunnerRecordingReplaysWithoutLeavingTheRecordingRoot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newHarness(t, nil)

	inv, err := h.runner.Investigate(ctx, investigateRequest(), requester, nil)
	if err != nil {
		t.Fatalf("investigate: %v", err)
	}

	dir := filepath.Join(h.recording, inv.GetInvestigationId(), replay.TrajectoriesDirName)
	paths, err := replay.Files(filepath.Join(h.recording, inv.GetInvestigationId()))
	if err != nil {
		t.Fatalf("list the recording: %v", err)
	}
	if len(paths) != 1 {
		t.Fatalf("the run left %d recordings in %s, want the one it made", len(paths), dir)
	}

	result, err := replay.ReplayFile(ctx, paths[0], nil)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !result.Identical {
		t.Fatalf("the recording did not reproduce itself: %s", result.Divergence.Error())
	}
	if result.WorkerCalls == 0 {
		t.Error("the replay re-keyed no worker call; a recording of a run that asked nothing " +
			"would replay identically and prove nothing")
	}
	if result.LedgerUpdates == 0 {
		t.Error("the replay re-derived no likelihood ratio; the first wave applies judgments and " +
			"they are what a ledger update records")
	}
}

// TestTheAnytimeStreamImprovesTurnByTurn is FR-046a through the runner.
//
// The requirement is not "publish something early", it is that the answer **improves while the
// investigation runs**: the prior-only ranking first and marked untested, then the deterministic
// first wave, then the belief state after every turn the model takes. A consumer that subscribed
// and got one message at the end would be reading a batch job with extra steps.
//
// Two properties are asserted and the second is the one with teeth. The count says the stream
// exists. **Monotone non-decreasing evidence** says the stream is an improving answer rather than
// a changing one: an investigation never un-learns something, so a subscriber can render the
// latest state without having to reconcile it against the last.
func TestTheAnytimeStreamImprovesTurnByTurn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	turns := []modeltest.Turn{
		{
			Text: "Asking the version split on the candidate's target.",
			ToolUses: []modeltest.ToolUse{{
				ID: "toolu_stream_1", Name: engine.ToolProposeHypothesis, Input: map[string]any{
					"kind":      "condition",
					"statement": "the symptom is carried by one deployed version",
				},
			}},
			Usage: model.Usage{Input: 12000, CacheWrite: 9000, Output: 300},
		},
		{
			Text: "Recording what the first wave established.",
			ToolUses: []modeltest.ToolUse{{
				ID: "toolu_stream_2", Name: engine.ToolProposeJudgments, Input: map[string]any{
					"judgments": []map[string]any{{
						"hypothesis_id": "h-1", "evidence_id": "e-2",
						"direction": "supports", "strength": "moderate",
						"rationale": "the rollout sits one hop from the subject and before the onset",
					}},
				},
			}},
			Usage: model.Usage{Input: 400, CacheRead: 9000, Output: 200},
		},
		{Text: "Nothing further separates them.", Usage: model.Usage{Input: 400, CacheRead: 9000, Output: 120}},
		{Text: "Concluding.", Usage: model.Usage{Input: 400, CacheRead: 9000, Output: 60}},
	}

	h := newHarness(t, turns)
	states := &collector{}
	final, err := h.runner.Investigate(ctx, investigateRequest(), requester, states.emit)
	if err != nil {
		t.Fatalf("investigate: %v", err)
	}
	if final.GetLifecycle() != investigationv1.Lifecycle_CONCLUDED {
		t.Fatalf("lifecycle = %v, want CONCLUDED (%q)", final.GetLifecycle(), final.GetStopDetail())
	}

	// How many turns the model actually took, read off the rows rather than assumed from the
	// canned list: the loop stops when the investigator stops proposing, which may be before the
	// turns run out.
	var modelTurns int
	if err := h.store.Pool().QueryRow(ctx,
		`SELECT count(*) FROM investigation.model_calls WHERE investigation_id = $1`,
		final.GetInvestigationId()).Scan(&modelTurns); err != nil {
		t.Fatalf("count model calls: %v", err)
	}
	if modelTurns == 0 {
		t.Fatal("the run took no model turn; there is no per-turn stream to assert")
	}

	// Intake, the provisional ranking, the first wave, one per turn, and the conclusion.
	want := 3 + modelTurns
	t.Logf("the anytime stream published %d states over %d model turn(s); evidence per state: %v",
		len(states.states), modelTurns, evidenceCounts(states.states))
	if len(states.states) < want {
		t.Errorf("the stream published %d states over %d model turn(s), want at least %d "+
			"(intake, provisional, first wave, one per turn, conclusion) — FR-046a asks for an "+
			"answer that improves while the run happens, not one that arrives at the end",
			len(states.states), modelTurns, want)
	}

	first := states.states[0]
	if len(first.GetLedger().GetHypotheses()) == 0 {
		t.Error("the first published state carries no hypotheses; the provisional answer is the " +
			"graph's own ranking and is available before any telemetry has been asked for")
	}
	for _, h := range first.GetLedger().GetHypotheses() {
		if h.GetStatus() == investigationv1.HypothesisStatus_SUPPORTED {
			t.Errorf("hypothesis %s is already SUPPORTED in the first published state; the "+
				"provisional answer is explicitly untested and nothing in it is supported",
				h.GetHypothesisId())
		}
	}
	if last := states.states[len(states.states)-1]; last.GetLifecycle() != investigationv1.Lifecycle_CONCLUDED {
		t.Errorf("the last published state is %v, not the conclusion", last.GetLifecycle())
	}

	var previous int
	for i, state := range states.states {
		count := len(state.GetLedger().GetEvidence())
		if count < previous {
			t.Errorf("state %d carries %d evidence items after a state carrying %d; an "+
				"investigation does not un-learn, and a stream that goes backwards is one a "+
				"subscriber has to reconcile rather than render", i, count, previous)
		}
		previous = count
	}
	if previous == 0 {
		t.Error("no published state carried any evidence at all")
	}
}

// TestTheInvestigationRowsRebuildFromTheEventLogAndARegenerableWorld is SC-011 on the output side
// (plan §Complexity Tracking, analyze C3; Phase 7 Track F6).
//
// The principle the investigation store is admitted under is that it holds **no independent source
// of truth**: every row in schema `investigation` is a function of the event log and the
// regenerable world, and the schema could be dropped and rebuilt without losing a fact. That is a
// strong claim and it is the reason the feature was allowed a second schema at all, so it is
// checked rather than asserted.
//
// The existing test in `internal/investigation/replay` covers the *input* side: export, drop,
// re-migrate, replay the event log, and the graph answers the investigation's queries again. What
// it deliberately left open is the other half — whether the rows come **back**. This is that half:
//
//  1. run an investigation and export it, so there is a record of what the rows were;
//  2. `DROP SCHEMA investigation CASCADE`, and make the migration ledger forget the migration that
//     created it, or re-migrating is a no-op over a schema that is not there;
//  3. re-migrate, and assert the schema comes back **empty** — a rebuild that started from
//     surviving rows would prove nothing;
//  4. run the same question again against the same two sources: the graph, and the regenerable
//     world (here the deterministic generator the harness is wired to, which is what a fixture's
//     `world/` is a recording of);
//  5. compare the rebuilt ledger against the exported one, field by field, in canonical JSON.
//
// What the comparison deliberately normalises is the investigation id and the wall-clock columns:
// a rebuild is a *new run of the same question*, so it mints its own id and its own timestamps,
// and requiring those to match would be requiring the rebuild to be a restore. Everything else —
// every hypothesis with its posterior and its bucket, every evidence item, every judgment — must
// come back identical. Anything that does not is a substrate leak, and the failure names the
// field rather than reporting "the ledgers differ".
func TestTheInvestigationRowsRebuildFromTheEventLogAndARegenerableWorld(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, nil)

	inv, err := h.runner.Investigate(ctx, investigateRequest(), requester, nil)
	if err != nil {
		t.Fatalf("investigate: %v", err)
	}
	before, err := h.runner.Investigation(ctx, inv.GetInvestigationId())
	if err != nil {
		t.Fatalf("read the investigation back: %v", err)
	}
	out := filepath.Join(t.TempDir(), "export")
	if _, err := h.runner.Export(ctx, &investigationv1.ExportRequest{
		InvestigationId: inv.GetInvestigationId(), OutDir: out,
	}); err != nil {
		t.Fatalf("export: %v", err)
	}

	// 1a: the human channel, which has nothing to re-run. A fact, a reopen and a label are the
	// three rows nobody will produce a second time, so if they are not derivable from the event
	// log they are a substrate of their own (FR-057a, FR-057b, FR-057e).
	human := recordHumanChannel(t, ctx, h, inv.GetInvestigationId())

	// 2 and 3: the schema goes away and comes back empty.
	if _, err := h.store.Pool().Exec(ctx, `DROP SCHEMA investigation CASCADE`); err != nil {
		t.Fatalf("drop schema investigation: %v", err)
	}
	if _, err := h.store.Pool().Exec(ctx,
		`DELETE FROM log.schema_migrations WHERE version >= 6`); err != nil {
		t.Fatalf("forget the investigation migrations: %v", err)
	}
	if err := h.store.Migrate(ctx); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
	var remaining int
	if err := h.store.Pool().QueryRow(ctx,
		`SELECT count(*) FROM investigation.investigations`).Scan(&remaining); err != nil {
		t.Fatalf("count investigations: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("the rebuilt schema holds %d investigations; a rebuild starts from nothing", remaining)
	}

	// 4: the same question, the same graph, the same regenerable world. Nothing reads the export.
	rebuilt := newRunnerOver(t, h.store)
	again, err := rebuilt.Investigate(ctx, investigateRequest(), requester, nil)
	if err != nil {
		t.Fatalf("re-run the investigation over the rebuilt schema: %v", err)
	}
	after, err := rebuilt.Investigation(ctx, again.GetInvestigationId())
	if err != nil {
		t.Fatalf("read the rebuilt investigation back: %v", err)
	}

	// 5: the comparison, one part of the ledger at a time so a failure names what leaked.
	if after.GetLifecycle() != before.GetLifecycle() {
		t.Errorf("the rebuilt run ended %v and the original ended %v",
			after.GetLifecycle(), before.GetLifecycle())
	}
	// A vacuous comparison is the failure mode this test has to avoid above all others: two empty
	// ledgers are equal, and a rebuild that produced nothing would pass every assertion below.
	if len(before.GetLedger().GetHypotheses()) < 2 || len(before.GetLedger().GetEvidence()) == 0 ||
		len(before.GetLedger().GetJudgments()) == 0 {
		t.Fatalf("the original run left %d hypotheses, %d evidence items and %d judgments; there is "+
			"not enough here for the comparison below to mean anything",
			len(before.GetLedger().GetHypotheses()), len(before.GetLedger().GetEvidence()),
			len(before.GetLedger().GetJudgments()))
	}

	compareLedgerPart(t, "hypotheses",
		hypothesisRows(before.GetLedger()), hypothesisRows(after.GetLedger()))
	compareLedgerPart(t, "evidence",
		evidenceRows(before.GetLedger()), evidenceRows(after.GetLedger()))
	compareLedgerPart(t, "judgments",
		judgmentRows(before.GetLedger()), judgmentRows(after.GetLedger()))

	// 6: the human channel comes back from the log and from nothing else. The rebuild is handed
	// the database and reads the three events the channel emitted; it is handed no export, no
	// recording and no memory of what was pushed.
	assertHumanChannelRebuilds(t, ctx, h, again.GetInvestigationId(), human)
}

// humanChannelState is what a run's human channel held before the schema was dropped: the fact
// that was pushed, the child a reopen linked to it, and the label. It is captured from the
// returns of the three calls rather than read back, so the assertion after the rebuild compares
// against what the writer said rather than against what the reader happened to find.
type humanChannelState struct {
	parentID      string
	factStatement string
	factAuthor    string
	childID       string
	labelRight    bool
	labelAuthor   string
}

// recordHumanChannel pushes a fact at the concluded run, reopens it with a second fact, and
// labels it — the three human actions FR-057a/b/e define, through the paths production uses.
func recordHumanChannel(t *testing.T, ctx context.Context, h *harness, investigationID string) humanChannelState {
	t.Helper()
	dao := h.runner.DAO()
	state := humanChannelState{
		parentID:      investigationID,
		factStatement: "the canary was still on the old build at 14:20",
		factAuthor:    requester,
		labelRight:    true,
		labelAuthor:   requester,
	}

	if _, err := dao.SubmitFact(ctx, investigationID, &investigationv1.HumanFact{
		Kind:        investigationstore.FactKindManualAction,
		Statement:   state.factStatement,
		Author:      state.factAuthor,
		EntityIds:   []string{checkoutRef},
		SubmittedAt: timestamppb.New(firedAt.Add(20 * time.Minute)),
	}); err != nil {
		t.Fatalf("submit a human fact: %v", err)
	}

	child, err := h.runner.Reopen(ctx, investigationID, &investigationv1.HumanFact{
		Kind:        investigationstore.FactKindVendorNotice,
		Statement:   "the vendor confirms a regional incident overlapping the window",
		Author:      requester,
		SubmittedAt: timestamppb.New(firedAt.Add(30 * time.Minute)),
	}, requester)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	state.childID = child.GetInvestigationId()

	if _, err := dao.RecordLabel(ctx, investigationID, &investigationv1.Label{
		WasThisRight: state.labelRight,
		Author:       state.labelAuthor,
		LabelledAt:   timestamppb.New(firedAt.Add(40 * time.Minute)),
	}); err != nil {
		t.Fatalf("label: %v", err)
	}
	return state
}

// assertHumanChannelRebuilds runs the log-driven rebuild and checks that each of the three rows
// came back attached to the right investigation.
func assertHumanChannelRebuilds(
	t *testing.T, ctx context.Context, h *harness, investigationID string, want humanChannelState,
) {
	t.Helper()
	// The run's identifier is derived from the symptom's published idempotency key (FR-008b), so
	// the rebuilt run is the same investigation and the events that name it still land.
	if investigationID != want.parentID {
		t.Fatalf("the rebuilt run is %s and the human channel names %s; the rebuild can only "+
			"attach a fact to the investigation it was pushed at",
			investigationID, want.parentID)
	}
	report, err := investigationstore.RebuildHumanChannel(ctx, h.store)
	if err != nil {
		t.Fatalf("rebuild the human channel: %v", err)
	}
	if len(report.MissingInvestigations) > 0 {
		t.Errorf("the rebuild could not place events for %v", report.MissingInvestigations)
	}

	dao := h.runner.DAO()
	facts, err := dao.FactsOf(ctx, investigationID)
	if err != nil {
		t.Fatalf("read the rebuilt facts: %v", err)
	}
	var found *investigationv1.HumanFact
	for _, f := range facts {
		if f.GetStatement() == want.factStatement {
			found = f
		}
	}
	switch {
	case found == nil:
		t.Errorf("the fact did not come back from the log; %d fact(s) were rebuilt against %s",
			len(facts), investigationID)
	case found.GetAuthor() != want.factAuthor:
		t.Errorf("the rebuilt fact is attributed to %q, want %q", found.GetAuthor(), want.factAuthor)
	case found.GetWeightClass() != investigationstore.WeightClassStrong:
		t.Errorf("the rebuilt fact is weighted %q, want strong and never decisive",
			found.GetWeightClass())
	case found.GetEvidenceId() == "":
		t.Error("the rebuilt fact carries no evidence item; a fact enters the evidence log (FR-057a)")
	}

	child, err := dao.Get(ctx, want.childID)
	if err != nil {
		t.Fatalf("read the rebuilt child %s: %v", want.childID, err)
	}
	if child.GetReopensInvestigationId() != investigationID {
		t.Errorf("the rebuilt child %s reopens %q, want %s",
			want.childID, child.GetReopensInvestigationId(), investigationID)
	}
	parent, err := dao.Get(ctx, investigationID)
	if err != nil {
		t.Fatalf("re-read the parent: %v", err)
	}
	if parent.GetLifecycle() != investigationv1.Lifecycle_REOPENED {
		t.Errorf("the rebuilt parent is %v, want REOPENED: the reopen link is half a link if the "+
			"parent does not carry it", parent.GetLifecycle())
	}

	labels, err := dao.LabelsOf(ctx, investigationID)
	if err != nil {
		t.Fatalf("read the rebuilt labels: %v", err)
	}
	if len(labels) != 1 {
		t.Fatalf("the rebuild produced %d label(s), want the one that was recorded", len(labels))
	}
	if labels[0].GetWasThisRight() != want.labelRight || labels[0].GetAuthor() != want.labelAuthor {
		t.Errorf("the rebuilt label is (%v, %q), want (%v, %q)",
			labels[0].GetWasThisRight(), labels[0].GetAuthor(), want.labelRight, want.labelAuthor)
	}
}

// newRunnerOver builds a second runner over a store that already exists, with the same graph and
// the same generator the harness uses. It is the "nothing beyond the event log and the
// regenerable world" half of the rebuild: it is handed no export, no recording and no ledger.
func newRunnerOver(t *testing.T, store *postgres.Store) *runner.Runner {
	t.Helper()
	backend, err := synthetic.New(scenario())
	if err != nil {
		t.Fatalf("synthetic backend: %v", err)
	}
	registry := worker.NewRegistry()
	for _, w := range []worker.Worker{
		metrics.New(backend), logs.New(backend), traces.New(backend),
	} {
		if err := registry.Register(w); err != nil {
			t.Fatalf("register %T: %v", w, err)
		}
	}
	r, err := runner.New(runner.Config{
		Store:         store,
		Graph:         stubGraph{},
		Workers:       registry,
		RecordingRoot: t.TempDir(),
		Prior:         publishedPrior(t),
		Mode:          worker.ModeRecorded,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("new runner over the rebuilt schema: %v", err)
	}
	return r
}

// hypothesisRows renders each hypothesis as the facts a rebuild must reproduce: which hypothesis,
// what it claims, how confident the ledger ended, which published bucket that is, and what status
// it reached. The per-run identifiers are not among them.
func hypothesisRows(l *investigationv1.Ledger) []string {
	out := make([]string, 0, len(l.GetHypotheses()))
	for _, h := range l.GetHypotheses() {
		out = append(out, fmt.Sprintf(
			"%s kind=%s status=%s prior=%.6f confidence=%.6f bucket=%s change=%s statement=%q",
			h.GetHypothesisId(), h.GetKind(), h.GetStatus(), h.GetPrior(), h.GetConfidence(),
			h.GetBucket().GetName(), h.GetCandidateChangeEntityId(), h.GetStatement()))
	}
	sort.Strings(out)
	return out
}

func evidenceRows(l *investigationv1.Ledger) []string {
	out := make([]string, 0, len(l.GetEvidence()))
	for _, e := range l.GetEvidence() {
		out = append(out, fmt.Sprintf("%s kind=%s worker=%s capability=%s outcome=%s digest=%s",
			e.GetEvidenceId(), e.GetKind(), e.GetWorker(), e.GetCapability(), e.GetOutcome(),
			e.GetResponseDigest()))
	}
	sort.Strings(out)
	return out
}

func judgmentRows(l *investigationv1.Ledger) []string {
	out := make([]string, 0, len(l.GetJudgments()))
	for _, j := range l.GetJudgments() {
		out = append(out, fmt.Sprintf("%s->%s evidence=%s %s/%s source=%s lnLR=%+.6f",
			j.GetJudgmentId(), j.GetHypothesisId(), j.GetEvidenceId(), j.GetDirection(),
			j.GetStrength(), j.GetSource(), j.GetLnLr()))
	}
	sort.Strings(out)
	return out
}

// compareLedgerPart names the first row that differs rather than dumping two ledgers, because a
// substrate leak is one column and a diff of two whole ledgers hides which.
func compareLedgerPart(t *testing.T, part string, before, after []string) {
	t.Helper()
	if len(before) != len(after) {
		t.Errorf("%s: the original run left %d row(s) and the rebuild produced %d; a row that "+
			"cannot be rebuilt from the event log and the regenerable world is a substrate leak\n"+
			"  original: %v\n  rebuilt:  %v", part, len(before), len(after), before, after)
		return
	}
	for i := range before {
		if before[i] != after[i] {
			t.Errorf("%s row %d did not rebuild:\n  original: %s\n  rebuilt:  %s",
				part, i, before[i], after[i])
		}
	}
}

// evidenceCounts renders how much evidence each published state carried, for the log line that
// makes the shape of the stream visible when a run is investigated by hand.
func evidenceCounts(states []*investigationv1.Investigation) []int {
	out := make([]int, 0, len(states))
	for _, state := range states {
		out = append(out, len(state.GetLedger().GetEvidence()))
	}
	return out
}

// TestAnExportFromAFixtureServedRunCarriesLayerTwo is FR-042 against the way a deployment is
// actually wired.
//
// `serve --recording-root <dir>` answers telemetry from `<dir>/world` (or from `<dir>` itself when
// the root is the world directory). The exporter used to look for layer 2 only under
// `<root>/<investigation-id>/world`, which nothing writes, so every export from such a deployment
// carried layer 1 and silently no layer 2 — and `investigate replay --layer world` against it had
// nothing to replay. This is the assertion that the export finds the world the run actually read.
func TestAnExportFromAFixtureServedRunCarriesLayerTwo(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	copyWorldInto(t, filepath.Join("..", "..", "..", "fixtures", "incidents",
		"rollout-regression-01-incident", "world"), filepath.Join(root, runner.WorldRoot))

	store := pgtest.Open(t)
	world, err := investigationbackend.NewRecorded(filepath.Join(root, runner.WorldRoot))
	if err != nil {
		t.Fatalf("load the recorded world: %v", err)
	}
	registry := worker.NewRegistry()
	for _, w := range []worker.Worker{metrics.New(world), logs.New(world), traces.New(world)} {
		if err := registry.Register(w); err != nil {
			t.Fatalf("register %T: %v", w, err)
		}
	}
	r, err := runner.New(runner.Config{
		Store:         store,
		Graph:         stubGraph{},
		Workers:       registry,
		RecordingRoot: root,
		Prior:         publishedPrior(t),
		Mode:          worker.ModeRecorded,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}

	inv, err := r.Investigate(ctx, investigateRequest(), requester, nil)
	if err != nil {
		t.Fatalf("investigate: %v", err)
	}
	out := filepath.Join(t.TempDir(), "export")
	if _, err := r.Export(ctx, &investigationv1.ExportRequest{
		InvestigationId: inv.GetInvestigationId(), OutDir: out,
	}); err != nil {
		t.Fatalf("export: %v", err)
	}

	index := filepath.Join(out, replay.WorldDirName, sdk.IndexFile)
	if _, err := os.Stat(index); err != nil {
		entries, _ := os.ReadDir(out)
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("the export carries no layer 2 (%s is missing); it holds %v. An export with no "+
			"world cannot be replayed with --layer world (FR-042)", index, names)
	}
	manifest, err := replay.ReadManifest(out)
	if err != nil {
		t.Fatalf("read the export manifest: %v", err)
	}
	if manifest.WorldTermCount == 0 {
		t.Error("the export manifest counts no world terms, so nothing was copied")
	}
	var worldFiles int
	for path := range manifest.Files {
		if strings.HasPrefix(path, replay.WorldDirName+"/") {
			worldFiles++
		}
	}
	if worldFiles == 0 {
		t.Error("the export manifest digests no file under world/, so layer 2 is not addressable")
	}
}

// copyWorldInto copies a recorded world's files into dst. It copies rather than symlinks so the
// test exercises the same os.Stat path a deployment does.
func copyWorldInto(t *testing.T, src, dst string) {
	t.Helper()
	if err := os.MkdirAll(dst, 0o750); err != nil {
		t.Fatalf("create %s: %v", dst, err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		body, err := os.ReadFile(filepath.Join(src, entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		if err := os.WriteFile(filepath.Join(dst, entry.Name()), body, 0o600); err != nil {
			t.Fatalf("write %s: %v", entry.Name(), err)
		}
	}
}

// TestACallerThatGoesAwayStillGetsAConcludedRow is FR-007 against a hung-up client.
//
// A terminal row is immutable, so a terminal state written in error is written for good: there is
// no later pass that corrects a `failed` row for an investigation that in fact concluded, and the
// next identical question is a no-op onto it (FR-008b). The run itself costs a vendor's money.
// So once the engine has returned a terminal state, the record of it is written on a context
// detached from the caller's and bounded by PersistWindow, and a client that hangs up between the
// last tool call and the ledger insert loses the stream and nothing else.
func TestACallerThatGoesAwayStillGetsAConcludedRow(t *testing.T) {
	h := newHarness(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The caller goes away as soon as the provisional ranking reaches it — the anytime state
	// FR-046a publishes before any evidence exists, which is exactly when an impatient client
	// hangs up.
	var cancelled bool
	emit := func(*investigationv1.Investigation) error {
		if !cancelled {
			cancelled = true
			cancel()
		}
		return nil
	}

	inv, err := h.runner.Investigate(ctx, investigateRequest(), requester, emit)
	if err != nil {
		t.Fatalf("investigate with a caller that went away: %v", err)
	}
	if !cancelled {
		t.Fatal("the test never cancelled the caller's context, so it asserts nothing")
	}
	if inv.GetLifecycle() != investigationv1.Lifecycle_CONCLUDED {
		t.Errorf("the returned run is %v, want CONCLUDED", inv.GetLifecycle())
	}

	// And the row says so, read back on a live context.
	row, err := h.dao.Get(context.Background(), inv.GetInvestigationId())
	if err != nil {
		t.Fatalf("read the row back: %v", err)
	}
	if row.GetLifecycle() != investigationv1.Lifecycle_CONCLUDED {
		t.Errorf("the stored row is %v (%q), want CONCLUDED: a caller's patience does not decide "+
			"whether a finished run is recorded", row.GetLifecycle(), row.GetStopDetail())
	}
	if row.GetVerdictLine() == "" {
		t.Error("the concluded row carries no verdict line")
	}
}
