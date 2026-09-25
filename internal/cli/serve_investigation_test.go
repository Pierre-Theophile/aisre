// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/fixture"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/investigation/runner"
	investigationstore "github.com/Pierre-Theophile/aisre/internal/investigation/store"
	"github.com/Pierre-Theophile/aisre/internal/investigation/workers/logs"
	"github.com/Pierre-Theophile/aisre/internal/investigation/workers/metrics"
	"github.com/Pierre-Theophile/aisre/internal/investigation/workers/traces"
	"github.com/Pierre-Theophile/aisre/internal/projector"
	"github.com/Pierre-Theophile/aisre/internal/query"
	"github.com/Pierre-Theophile/aisre/internal/server"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres/pgtest"
	"github.com/Pierre-Theophile/aisre/pkg/backend/synthetic"
	"github.com/Pierre-Theophile/aisre/pkg/worker"
)

// `investigate` → ConnectRPC → the real engine (002 T089, FR-064, FR-067).
//
// `internal/cli/investigate_test.go` drives the same commands against a stub runner, which is the
// right test for the CLI's own behaviour: exit codes, flag parsing, the two renderings. This one
// asks the other question — **is the wire actually connected?** — by mounting the engine
// `serve --enable-investigation` builds and running the command an on-call would type.
//
// There is no model client and no API key. That is FR-067 stated as a test: the whole path from a
// typed command to a concluded investigation with a ranked answer works with no vendor account and
// no network to one, which is what makes the quickstart reproducible on a laptop.

const investigateAtRFC = "2026-09-01T14:32:00Z"

var (
	cliFiredAt   = time.Date(2026, 9, 1, 14, 32, 0, 0, time.UTC)
	cliWindowAt  = time.Date(2026, 9, 1, 13, 30, 0, 0, time.UTC)
	cliOnsetAt   = time.Date(2026, 9, 1, 14, 21, 0, 0, time.UTC)
	cliRolloutAt = cliOnsetAt.Add(-2 * time.Minute)
	cliScalingAt = cliOnsetAt.Add(5 * time.Minute)
)

const (
	cliCheckoutRef   = "otel.service.name=checkout"
	cliPaymentsRef   = "otel.service.name=payments"
	cliStorefrontRef = "otel.service.name=storefront"
	cliRolloutChange = "k8s.change=shop/payments@rev7"
	cliScalingChange = "k8s.change=shop/storefront@scale-1415"
)

// cliStubGraph is feature 001's read surface answered in memory, ranking with the published
// formula so that the order the CLI prints is the graph's own.
type cliStubGraph struct{}

func (cliStubGraph) Subgraph(context.Context, *graphv1.SubgraphRequest) (*graphv1.SubgraphResponse, error) {
	return &graphv1.SubgraphResponse{}, nil
}

func (cliStubGraph) Impact(context.Context, *graphv1.ImpactRequest) (*graphv1.ImpactResponse, error) {
	return &graphv1.ImpactResponse{}, nil
}

func (cliStubGraph) NodeHistory(context.Context, *graphv1.NodeHistoryRequest) (*graphv1.NodeHistoryResponse, error) {
	return &graphv1.NodeHistoryResponse{}, nil
}

func (cliStubGraph) ResolutionAudit(_ context.Context, req *graphv1.ResolutionAuditRequest) (*graphv1.ResolutionAuditResponse, error) {
	ref := graph.RefFromProto(req.GetA())
	if ref.IsZero() {
		return &graphv1.ResolutionAuditResponse{}, nil
	}
	return &graphv1.ResolutionAuditResponse{
		SameEntity: true, CanonicalId: graph.EntityID(ref.Namespace, ref.Value),
	}, nil
}

func (cliStubGraph) Extent(context.Context, *graphv1.ExtentRequest) (*graphv1.Extent, error) {
	return &graphv1.Extent{
		EarliestObserved: timestamppb.New(cliWindowAt.Add(-48 * time.Hour)),
		LatestObserved:   timestamppb.New(cliFiredAt),
	}, nil
}

func (cliStubGraph) Pointers(_ context.Context, req *graphv1.PointersRequest) (*graphv1.PointersResponse, error) {
	name := req.GetFocus().GetValue()
	return &graphv1.PointersResponse{
		Node: &graphv1.NodeVersion{
			EntityId:    req.GetFocus().GetNamespace() + "=" + name,
			Type:        graphv1.NodeType_SERVICE,
			DisplayName: name,
		},
		ByKind: map[string]*graphv1.PointerList{
			"metric": {Pointers: []*graphv1.Pointer{{
				Kind: graphv1.PointerKind_METRIC, BackendKind: "synthetic", Vocabulary: "promql",
				Selector: "http_errors{service=\"" + name + "\"}",
				JoinKeys: map[string]string{"version": "service.version"},
			}}},
			"log": {Pointers: []*graphv1.Pointer{{
				Kind: graphv1.PointerKind_LOG, BackendKind: "synthetic", Vocabulary: "logql",
				Selector: "{service=\"" + name + "\"}",
				JoinKeys: map[string]string{"version": "service.version"},
			}}},
		},
	}, nil
}

func (cliStubGraph) Diff(_ context.Context, req *graphv1.DiffRequest) (*graphv1.DiffResponse, error) {
	reference := req.GetT2().AsTime().UTC()
	if req.GetReferenceAt() != nil {
		reference = req.GetReferenceAt().AsTime().UTC()
	}
	params := query.RankParams{Reference: reference, Tau: query.DefaultTau, HopCap: 3}
	ranked, _ := query.Rank([]query.Candidate{
		{
			Change: cliChangeNode(cliRolloutChange, cliRolloutAt, graphv1.ChangeKind_ROLLOUT,
				graphv1.ActorKind_PERSON),
			TargetIDs: []string{cliPaymentsRef}, Hop: 1, WeightClass: 1,
		},
		{
			Change: cliChangeNode(cliScalingChange, cliScalingAt, graphv1.ChangeKind_SCALING,
				graphv1.ActorKind_CONTROLLER),
			TargetIDs: []string{cliStorefrontRef}, Hop: 1, WeightClass: 1,
		},
	}, params)
	return &graphv1.DiffResponse{Changes: ranked, RankingFormula: query.RankingFormula(params)}, nil
}

func cliChangeNode(entityID string, at time.Time, kind graphv1.ChangeKind, actor graphv1.ActorKind) *graphv1.NodeVersion {
	return &graphv1.NodeVersion{
		EntityId: entityID, VersionId: entityID + "@1", Type: graphv1.NodeType_CHANGE,
		Valid:  &graphv1.Interval{Start: timestamppb.New(at)},
		Change: &graphv1.Change{Kind: kind, ActorKind: actor},
	}
}

func cliScenario() synthetic.Scenario {
	return synthetic.Scenario{
		Seed:       "cli-rollout-regression",
		Start:      cliWindowAt,
		End:        cliFiredAt.Add(5 * time.Minute),
		Resolution: time.Minute,
		Entities: []synthetic.Entity{
			{EntityID: cliCheckoutRef, Name: "checkout", BaselineVersion: "rev3",
				Selectors: []string{"http_errors{service=\"checkout\"}", "{service=\"checkout\"}"}},
			{EntityID: cliPaymentsRef, Name: "payments", BaselineVersion: "rev6",
				Selectors: []string{"http_errors{service=\"payments\"}", "{service=\"payments\"}"}},
			{EntityID: cliStorefrontRef, Name: "storefront", BaselineVersion: "rev2",
				Selectors: []string{"http_errors{service=\"storefront\"}", "{service=\"storefront\"}"}},
		},
		Edges: []synthetic.Edge{
			{SrcEntityID: cliCheckoutRef, DstEntityID: cliPaymentsRef, WeightClass: 1},
			{SrcEntityID: cliCheckoutRef, DstEntityID: cliStorefrontRef, WeightClass: 3},
		},
		Changes: []synthetic.Change{
			{EntityID: cliRolloutChange, At: cliRolloutAt, Version: "rev7", Degrades: true,
				TargetEntityIDs: []string{cliPaymentsRef, cliCheckoutRef}},
			{EntityID: cliScalingChange, At: cliScalingAt, Version: "scale-1415", Degrades: false,
				TargetEntityIDs: []string{cliStorefrontRef}},
		},
	}
}

// liveInvestigationServer is `serve --enable-investigation` with the real engine behind it.
func liveInvestigationServer(t *testing.T) (baseURL, token string) {
	t.Helper()

	store := pgtest.Open(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	backend, err := synthetic.New(cliScenario())
	if err != nil {
		t.Fatalf("synthetic backend: %v", err)
	}
	registry := worker.NewRegistry()
	for _, w := range []worker.Worker{metrics.New(backend), logs.New(backend), traces.New(backend)} {
		if err := registry.Register(w); err != nil {
			t.Fatalf("register %T: %v", w, err)
		}
	}
	proj := projector.New(store)
	engine, err := runner.New(runner.Config{
		Store:         store,
		Projector:     proj,
		Graph:         cliStubGraph{},
		Workers:       registry,
		RecordingRoot: t.TempDir(),
		Mode:          worker.ModeRecorded,
		Logger:        logger,
	})
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}

	auth, err := server.NewDevAuthenticator(server.DevConfig{Enabled: true, Logger: logger})
	if err != nil {
		t.Fatalf("dev authenticator: %v", err)
	}
	dao := investigationstore.NewInvestigationDAO(store)
	srv, err := server.New(server.Config{
		Listen:    "127.0.0.1:0",
		Auth:      auth,
		Projector: proj,
		Logger:    logger,
		// The ledger reader, as `serve --enable-investigation` wires it: without it the read
		// path serves a verdict with nothing under it, and anything downstream of `investigate
		// get` — `to-incident`, the renderings — sees an empty ledger.
		Investigation: server.NewInvestigationService(engine, dao, logger,
			server.WithLedgerReader(investigationstore.NewLedgerDAO(store))),
		ShutdownTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("server did not shut down")
		}
	})

	minted, err := auth.Issuer().MintFor(server.DevUser{
		Name: "alice", Roles: []server.Role{server.RoleInvestigator, server.RoleReader},
	})
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}
	return "http://" + srv.Addr(), minted
}

// TestInvestigateAgainstTheRealEngineEndToEnd is the path an on-call takes, with nothing stubbed
// between the command and the database (FR-064, FR-067).
func TestInvestigateAgainstTheRealEngineEndToEnd(t *testing.T) {
	baseURL, token := liveInvestigationServer(t)
	ctx := context.Background()

	args := []string{
		"--server", baseURL, "--token", token,
		"investigate", cliCheckoutRef,
		"--at", investigateAtRFC,
		"--lookback", "90m",
		"--profile", "page",
	}

	stdout, stderr, code := run(t, ctx, args...)
	if code != ExitOK {
		t.Fatalf("investigate: exit %d (stderr %q)", code, stderr)
	}
	// FR-057c's published order, produced by the engine rather than by a stub.
	for _, section := range []string{"## verdict", "## ranked", "## timeline", "## narrative"} {
		if !strings.Contains(stdout, section) {
			t.Errorf("the human rendering has no %q section:\n%s", section, stdout)
		}
	}
	if !strings.Contains(stdout, cliRolloutChange) {
		t.Errorf("the rendering never names the culprit %s:\n%s", cliRolloutChange, stdout)
	}

	// FR-064: the machine form of the same run, with the same claims.
	jsonOut, stderr, code := run(t, ctx, append([]string{"--output", "json"}, args...)...)
	if code != ExitOK {
		t.Fatalf("investigate --output json: exit %d (stderr %q)", code, stderr)
	}
	var inv map[string]any
	if err := json.Unmarshal([]byte(jsonOut), &inv); err != nil {
		t.Fatalf("the machine rendering is not canonical JSON: %v\n%s", err, jsonOut)
	}
	id, _ := inv["investigationId"].(string)
	if id == "" {
		t.Fatalf("the machine rendering carries no investigation id:\n%s", jsonOut)
	}
	if lifecycle, _ := inv["lifecycle"].(string); lifecycle != "CONCLUDED" {
		t.Errorf("lifecycle = %q, want CONCLUDED", lifecycle)
	}
	if verdict, _ := inv["verdictLine"].(string); verdict == "" {
		t.Error("the machine rendering carries no verdict line")
	}

	// FR-008b end to end: the second invocation is the same question and gets the same answer.
	if !strings.Contains(jsonOut, id) {
		t.Fatalf("the json rendering does not name its own id")
	}
	again, stderr, code := run(t, ctx, append([]string{"--output", "json"}, args...)...)
	if code != ExitOK {
		t.Fatalf("re-run: exit %d (stderr %q)", code, stderr)
	}
	var second map[string]any
	if err := json.Unmarshal([]byte(again), &second); err != nil {
		t.Fatalf("re-run is not canonical JSON: %v", err)
	}
	if got, _ := second["investigationId"].(string); got != id {
		t.Errorf("the identical question opened %s; the first investigation is %s (FR-008b)", got, id)
	}

	// And the read path serves the same row over the same connection.
	readOut, stderr, code := run(t, ctx,
		"--server", baseURL, "--token", token, "--output", "json", "investigate", "get", id)
	if code != ExitOK {
		t.Fatalf("investigate get: exit %d (stderr %q)", code, stderr)
	}
	if !strings.Contains(readOut, id) {
		t.Errorf("`investigate get %s` did not return it:\n%s", id, readOut)
	}
}

// TestInvestigateWatchPublishesTheProvisionalAnswerFirst is FR-046a over the wire: `--watch`
// streams the prior-only ranking before any evidence exists, and it says it is untested.
func TestInvestigateWatchPublishesTheProvisionalAnswerFirst(t *testing.T) {
	baseURL, token := liveInvestigationServer(t)

	stdout, stderr, code := run(t, context.Background(),
		"--server", baseURL, "--token", token,
		"investigate", cliCheckoutRef, "--at", investigateAtRFC, "--watch")
	if code != ExitOK {
		t.Fatalf("investigate --watch: exit %d (stderr %q)", code, stderr)
	}
	// Two states, so two verdict sections: the provisional one and the concluded one.
	if got := strings.Count(stdout, "## verdict"); got < 2 {
		t.Errorf("--watch printed %d states; the anytime shape is the provisional answer and then "+
			"the concluded one (FR-046a):\n%s", got, stdout)
	}
}

// TestToIncidentWritesAManifestTheHarnessAccepts is FR-055 end to end: a reviewed investigation
// becomes a corpus incident "accepted by the evaluation harness with no hand-editing".
//
// Before the manifest was written, `investigate to-incident` produced a directory `fixture
// verify` did not recognise as a fixture at all — the replayable half with nothing saying what
// the question was or what the true answer is. This asserts the other half: the directory loads
// through the *harness's own* loader, carries an `incident:` block, and derives what can be
// derived from the run (the question, the reviewer's culprit, the prior rank, the knowability
// time, the sources) while naming what a reviewer still owes rather than inventing it.
func TestToIncidentWritesAManifestTheHarnessAccepts(t *testing.T) {
	baseURL, token := liveInvestigationServer(t)
	ctx := context.Background()

	jsonOut, stderr, code := run(t, ctx,
		"--server", baseURL, "--token", token, "--output", "json",
		"investigate", cliCheckoutRef, "--at", investigateAtRFC, "--lookback", "90m",
		"--profile", "page")
	if code != ExitOK {
		t.Fatalf("investigate: exit %d (stderr %q)", code, stderr)
	}
	var inv map[string]any
	if err := json.Unmarshal([]byte(jsonOut), &inv); err != nil {
		t.Fatalf("the machine rendering is not canonical JSON: %v", err)
	}
	id, _ := inv["investigationId"].(string)
	if id == "" {
		t.Fatal("the run returned no investigation id")
	}

	if _, stderr, code = run(t, ctx, "--server", baseURL, "--token", token,
		"investigate", "review", id, "--root-cause", cliRolloutChange,
		"--reason", "the rollout to rev7 carries the failing spans"); code != ExitOK {
		t.Fatalf("review: exit %d (stderr %q)", code, stderr)
	}
	if _, stderr, code = run(t, ctx, "--server", baseURL, "--token", token,
		"investigate", "label", id, "--right"); code != ExitOK {
		t.Fatalf("label: exit %d (stderr %q)", code, stderr)
	}

	// The directory's base name is the fixture id, because 001's loader requires the two to
	// agree; a corpus incident is written under the name it will be scored by.
	out := filepath.Join(t.TempDir(), "rollout-regression-01-recorded")
	stdout, stderr, code := run(t, ctx, "--server", baseURL, "--token", token,
		"investigate", "to-incident", id, "--out", out)
	if code != ExitOK {
		t.Fatalf("to-incident: exit %d (stderr %q)", code, stderr)
	}
	if !strings.Contains(stdout, "await a reviewer") {
		t.Errorf("to-incident did not say which fields are placeholders:\n%s", stdout)
	}

	manifest, err := fixture.LoadManifest(out)
	if err != nil {
		body, _ := os.ReadFile(filepath.Join(out, fixture.ManifestFile))
		t.Fatalf("the generated manifest does not load: %v\n--- manifest.yaml ---\n%s", err, body)
	}
	if !manifest.IsIncident() {
		t.Fatal("the generated manifest carries no `incident:` block, so the harness will not score it")
	}
	incident := manifest.Incident
	if got := incident.GroundTruth.Culprit; got != cliRolloutChange {
		t.Errorf("ground truth culprit = %q, want the reviewer's %q", got, cliRolloutChange)
	}
	if got := incident.Question.Subject; got != cliCheckoutRef {
		t.Errorf("question subject = %q, want %q", got, cliCheckoutRef)
	}
	if !incident.Question.FiredAt.Equal(cliFiredAt) {
		t.Errorf("question fired_at = %s, want %s", incident.Question.FiredAt, cliFiredAt)
	}
	if incident.GroundTruth.KnowabilityTime.IsZero() {
		t.Error("the manifest derives no knowability time; before it `unknown` is the correct answer")
	}
	if incident.GroundTruth.Provenance.Kind != "recorded" {
		t.Errorf("provenance kind = %q, want recorded", incident.GroundTruth.Provenance.Kind)
	}
	if incident.GroundTruth.PriorRankOfCulprit == nil {
		t.Error("the manifest states no prior_rank_of_culprit, so lift cannot be measured (FR-062b)")
	}
	if len(manifest.Sources) == 0 {
		t.Error("the manifest declares no sources; 001's loader requires at least one")
	}

	// The placeholders are reported rather than left to be discovered by a grader.
	placeholders, err := fixture.PlaceholderFields(out)
	if err != nil {
		t.Fatalf("read the placeholders: %v", err)
	}
	if len(placeholders) == 0 {
		t.Error("nothing is marked as awaiting a reviewer, but decoys and decisive evidence " +
			"cannot be derived from a run")
	}
}
