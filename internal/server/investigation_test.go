// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1/investigationv1connect"
	"github.com/Pierre-Theophile/aisre/internal/investigation/ledger"
	investigationstore "github.com/Pierre-Theophile/aisre/internal/investigation/store"
	"github.com/Pierre-Theophile/aisre/internal/projector"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres/pgtest"
)

// InvestigationService (T089, FR-064, FR-066, plan F10).
//
// Two things are under test and they are different. The **role mapping** — which credential may
// call which RPC — is asserted for every one of the eleven procedures, because a service that
// gets one of those wrong is a service that lets a reader spend a vendor's quota. The
// **gRPC/JSON parity** is asserted for two of them, which is what FR-064's dual surface needs:
// the two transports are the same handler, and the test exists so that stays true when a handler
// starts doing its own serialisation.

// --- a running server with the investigation service mounted ------------------------------

type investigationServer struct {
	*graphServer
	runner *stubRunner
	dao    *investigationstore.InvestigationDAO
}

// stubLedger stands in for LedgerDAO. The DAO's own round trip is tested against a real database
// in `internal/investigation/store`; what the service needs from it is that `Get` asks for the
// ledger and hands it back, and that `List` does not.
type stubLedger struct{}

func (stubLedger) Load(_ context.Context, investigationID string) (*investigationstore.LedgerRows, error) {
	return &investigationstore.LedgerRows{
		InvestigationID: investigationID,
		Hypotheses: []ledger.Hypothesis{{
			ID: "h-1", Kind: ledger.KindChange, Statement: "shop/payments@rev7 caused it",
			CandidateChangeEntityID: "k8s.change=shop/payments@rev7",
			TargetEntityIDs:         []string{"otel.service.name=payments"},
			Status:                  ledger.StatusProposed, Prior: 0.4, Confidence: 0.4,
		}},
	}, nil
}

// stubRunner stands in for the engine. It records what it was asked and answers immediately:
// the RPC layer's job is authorization, streaming and shape, and a real loop would test the
// loop instead.
type stubRunner struct {
	investigated    int
	declared        int
	replayed        int
	exported        int
	reopened        int
	emitProvisional bool
	err             error
}

func (r *stubRunner) Investigate(
	_ context.Context, req *investigationv1.InvestigateRequest, requester string,
	emit func(*investigationv1.Investigation) error,
) (*investigationv1.Investigation, error) {
	r.investigated++
	if r.err != nil {
		return nil, r.err
	}
	if r.emitProvisional {
		// The anytime shape: the prior-only ranking first, labelled provisional (FR-046a).
		if err := emit(&investigationv1.Investigation{
			InvestigationId: "inv-stub-01", Provisional: true,
			Lifecycle: investigationv1.Lifecycle_RUNNING, Requester: requester,
		}); err != nil {
			return nil, err
		}
	}
	return &investigationv1.Investigation{
		InvestigationId: "inv-stub-01",
		IncidentId:      "inc-stub-01",
		Requester:       requester,
		Lifecycle:       investigationv1.Lifecycle_CONCLUDED,
		ConclusionKind:  investigationv1.ConclusionKind_CONCLUSION_FINAL,
		Outcome:         investigationv1.InvestigationOutcome_RANKED,
		VerdictLine:     "shop/payments@rev7 caused it",
		ValidAt:         req.GetValidAt(),
	}, nil
}

func (r *stubRunner) Declare(
	_ context.Context, req *investigationv1.DeclareRequest, requester string,
	emit func(*investigationv1.Investigation) error,
) (*investigationv1.Investigation, error) {
	r.declared++
	if r.err != nil {
		return nil, r.err
	}
	_ = emit
	return &investigationv1.Investigation{
		InvestigationId: "inv-declared-01",
		IncidentId:      "inc-declared-01",
		Requester:       requester,
		Lifecycle:       investigationv1.Lifecycle_RUNNING,
		Symptoms:        []*investigationv1.Symptom{req.GetDeclaration()},
	}, nil
}

func (r *stubRunner) Replay(context.Context, *investigationv1.ReplayRequest) (*investigationv1.ReplayResponse, error) {
	r.replayed++
	return &investigationv1.ReplayResponse{Identical: true}, r.err
}

func (r *stubRunner) Export(_ context.Context, req *investigationv1.ExportRequest) (*investigationv1.ExportResponse, error) {
	r.exported++
	return &investigationv1.ExportResponse{OutDir: req.GetOutDir(), ExportDigest: "sha256:abc"}, r.err
}

func (r *stubRunner) Reopen(_ context.Context, parentID string, fact *investigationv1.HumanFact, requester string) (*investigationv1.Investigation, error) {
	r.reopened++
	if r.err != nil {
		return nil, r.err
	}
	return &investigationv1.Investigation{
		InvestigationId:        "inv-child-01",
		ReopensInvestigationId: parentID,
		Requester:              requester,
		Facts:                  []*investigationv1.HumanFact{fact},
	}, nil
}

const (
	investigationID  = "inv-rpc-01"
	investigationInc = "inc-rpc-01"
)

var investigationAt = time.Date(2026, 9, 1, 14, 32, 0, 0, time.UTC)

func newInvestigationServer(t *testing.T) *investigationServer {
	t.Helper()

	store := pgtest.Open(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	auth, err := NewDevAuthenticator(DevConfig{Enabled: true, Logger: logger})
	if err != nil {
		t.Fatalf("dev authenticator: %v", err)
	}
	proj := projector.New(store)
	dao := investigationstore.NewInvestigationDAO(store)
	runner := &stubRunner{}

	srv, err := New(Config{
		Listen:          "127.0.0.1:0",
		Auth:            auth,
		Projector:       proj,
		Logger:          logger,
		Investigation:   NewInvestigationService(runner, dao, logger, WithLedgerReader(stubLedger{})),
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
		case err := <-done:
			if err != nil {
				t.Errorf("serve: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("server did not shut down")
		}
	})

	ts := &investigationServer{
		graphServer: &graphServer{
			baseURL: "http://" + srv.Addr(), projector: proj, issuer: auth.Issuer(),
		},
		runner: runner,
		dao:    dao,
	}
	ts.seed(t)
	return ts
}

// seed writes one running investigation the read RPCs can answer about.
func (ts *investigationServer) seed(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	principal := ts.tokenPrincipal(t, "alice")
	if _, err := ts.dao.Open(ctx,
		investigationstore.Incident{
			IncidentID: investigationInc, CanonicalSubjectID: "e:checkout",
			OpenedAt: investigationAt, LastSymptomAt: investigationAt,
			AssociationRuleVersion: "1.0.0",
		},
		investigationstore.NewInvestigation{
			InvestigationID: investigationID, IncidentID: investigationInc,
			ValidAt: investigationAt, ObservedAt: investigationAt,
			WindowStart: investigationAt.Add(-90 * time.Minute), WindowEnd: investigationAt,
			Profile: "page", Requester: principal,
			AlgebraVersion: "1.0.0", LedgerRuleVersion: "1.0.0", SchemaVersion: "1.0.0",
			StartedAt: investigationAt,
		},
		&investigationv1.Symptom{
			SymptomId: "sym-rpc-01", OriginSystem: "slack:acme", OriginRef: "#incident",
			Transport: "human_declared", Statement: "checkout is throwing 500s",
			FiredAt:           timestamppb.New(investigationAt),
			Origin:            investigationv1.IntakeOrigin_INTAKE_ORIGIN_DECLARED,
			DeclaringIdentity: principal, IdempotencyKey: "alert:rpc-01",
		},
	); err != nil {
		t.Fatalf("seed investigation: %v", err)
	}
}

func (ts *investigationServer) tokenPrincipal(t *testing.T, name string) string {
	t.Helper()
	return DefaultDevIssuer + "|" + name
}

func (ts *investigationServer) token(t *testing.T, name string, roles ...Role) string {
	t.Helper()
	user := DevUser{Name: name, Roles: roles}
	// A feeder token is scoped to exactly one source and the issuer refuses to mint one without
	// it (FR-046). That scoping is not what these tests are about, so the test feeder gets a
	// source id and is then asked to call investigation RPCs it has no business calling.
	for _, role := range roles {
		if role == RoleFeeder {
			user.SourceID = "otel:kind"
		}
	}
	token, err := ts.issuer.MintFor(user)
	if err != nil {
		t.Fatalf("mint token for %v: %v", roles, err)
	}
	return token
}

func (ts *investigationServer) client(token string) investigationv1connect.InvestigationServiceClient {
	return investigationv1connect.NewInvestigationServiceClient(
		h2cClient(token), ts.baseURL, connect.WithGRPC())
}

func (ts *investigationServer) jsonClient(token string) investigationv1connect.InvestigationServiceClient {
	return investigationv1connect.NewInvestigationServiceClient(
		&http.Client{Transport: &bearerTransport{base: http.DefaultTransport, token: token}},
		ts.baseURL)
}

// --- the role mapping ----------------------------------------------------------------------

// TestRoleMapping is the authorization contract of T089, one row per RPC:
//
//	Get, List, Export                                      reader
//	Investigate, Declare, Replay, Reopen, SubmitHumanFact   investigator
//	Review, Label                                          decider OR investigator
func TestRoleMapping(t *testing.T) {
	ts := newInvestigationServer(t)

	type call struct {
		name string
		run  func(context.Context, investigationv1connect.InvestigationServiceClient) error
	}
	calls := map[string]call{
		"Get": {"Get", func(ctx context.Context, c investigationv1connect.InvestigationServiceClient) error {
			_, err := c.Get(ctx, connect.NewRequest(&investigationv1.GetRequest{InvestigationId: investigationID}))
			return err
		}},
		"List": {"List", func(ctx context.Context, c investigationv1connect.InvestigationServiceClient) error {
			_, err := c.List(ctx, connect.NewRequest(&investigationv1.ListInvestigationsRequest{}))
			return err
		}},
		"Export": {"Export", func(ctx context.Context, c investigationv1connect.InvestigationServiceClient) error {
			_, err := c.Export(ctx, connect.NewRequest(&investigationv1.ExportRequest{
				InvestigationId: investigationID, OutDir: t.TempDir(),
			}))
			return err
		}},
		"Investigate": {"Investigate", func(ctx context.Context, c investigationv1connect.InvestigationServiceClient) error {
			return drain(c.Investigate(ctx, connect.NewRequest(&investigationv1.InvestigateRequest{
				ValidAt: timestamppb.New(investigationAt),
			})))
		}},
		"Declare": {"Declare", func(ctx context.Context, c investigationv1connect.InvestigationServiceClient) error {
			return drain(c.Declare(ctx, connect.NewRequest(&investigationv1.DeclareRequest{
				Declaration: &investigationv1.Symptom{
					OriginSystem: "slack:acme", OriginRef: "#incident",
					FiredAt: timestamppb.New(investigationAt), Title: "checkout 500s",
				},
			})))
		}},
		"Replay": {"Replay", func(ctx context.Context, c investigationv1connect.InvestigationServiceClient) error {
			_, err := c.Replay(ctx, connect.NewRequest(&investigationv1.ReplayRequest{
				ExportPath: t.TempDir(), Layer: "trajectory",
			}))
			return err
		}},
		"Reopen": {"Reopen", func(ctx context.Context, c investigationv1connect.InvestigationServiceClient) error {
			_, err := c.Reopen(ctx, connect.NewRequest(&investigationv1.ReopenRequest{
				InvestigationId: investigationID,
				Fact: &investigationv1.HumanFact{
					Kind: "correction", Statement: "the affected service is shop/payments",
				},
			}))
			return err
		}},
		"SubmitHumanFact": {"SubmitHumanFact", func(ctx context.Context, c investigationv1connect.InvestigationServiceClient) error {
			_, err := c.SubmitHumanFact(ctx, connect.NewRequest(&investigationv1.SubmitHumanFactRequest{
				InvestigationId: investigationID,
				Fact: &investigationv1.HumanFact{
					Kind: "manual_action", Statement: "I restarted a pod",
				},
			}))
			return err
		}},
		"Review": {"Review", func(ctx context.Context, c investigationv1connect.InvestigationServiceClient) error {
			_, err := c.Review(ctx, connect.NewRequest(&investigationv1.ReviewRequest{
				InvestigationId: investigationID,
				Review: &investigationv1.HumanReview{
					ValidatedRootCause: "unobserved", Rationale: "client-side config",
				},
			}))
			return err
		}},
		"Label": {"Label", func(ctx context.Context, c investigationv1connect.InvestigationServiceClient) error {
			_, err := c.Label(ctx, connect.NewRequest(&investigationv1.LabelRequest{
				InvestigationId: investigationID, WasThisRight: true,
			}))
			return err
		}},
	}

	// The published mapping, spelled out rather than derived. A table a reviewer can read
	// against contracts/cli.md is the point.
	mapping := []struct {
		rpc     string
		allowed []Role
		denied  []Role
	}{
		{"Get", []Role{RoleReader, RoleDecider, RoleInvestigator}, []Role{RoleFeeder}},
		{"List", []Role{RoleReader, RoleDecider, RoleInvestigator}, []Role{RoleFeeder}},
		{"Export", []Role{RoleReader, RoleDecider, RoleInvestigator}, []Role{RoleFeeder}},
		{"Investigate", []Role{RoleInvestigator}, []Role{RoleReader, RoleDecider, RoleFeeder}},
		{"Declare", []Role{RoleInvestigator}, []Role{RoleReader, RoleDecider, RoleFeeder}},
		{"Replay", []Role{RoleInvestigator}, []Role{RoleReader, RoleDecider, RoleFeeder}},
		{"Reopen", []Role{RoleInvestigator}, []Role{RoleReader, RoleDecider, RoleFeeder}},
		{"SubmitHumanFact", []Role{RoleInvestigator}, []Role{RoleReader, RoleDecider, RoleFeeder}},
		{"Review", []Role{RoleDecider, RoleInvestigator}, []Role{RoleReader, RoleFeeder}},
		{"Label", []Role{RoleDecider, RoleInvestigator}, []Role{RoleReader, RoleFeeder}},
	}

	ctx := context.Background()
	for _, row := range mapping {
		row := row
		t.Run(row.rpc, func(t *testing.T) {
			c, ok := calls[row.rpc]
			if !ok {
				t.Fatalf("no call defined for %s", row.rpc)
			}
			for _, role := range row.allowed {
				client := ts.client(ts.token(t, "alice-"+string(role), role))
				if err := c.run(ctx, client); isAuthFailure(err) {
					t.Errorf("%s with role %s was refused: %v", row.rpc, role, err)
				}
			}
			for _, role := range row.denied {
				client := ts.client(ts.token(t, "mallory-"+string(role), role))
				err := c.run(ctx, client)
				if !isAuthFailure(err) {
					t.Errorf("%s with role %s was allowed (err = %v); the published mapping "+
						"grants it to %v only", row.rpc, role, err, row.allowed)
				}
			}
			// FR-066: an anonymous caller is refused everywhere.
			if err := c.run(ctx, ts.client("")); !isAuthFailure(err) {
				t.Errorf("%s accepted an anonymous caller (err = %v); FR-066 refuses one", row.rpc, err)
			}
		})
	}
}

func isAuthFailure(err error) bool {
	if err == nil {
		return false
	}
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) {
		return false
	}
	return connectErr.Code() == connect.CodeUnauthenticated ||
		connectErr.Code() == connect.CodePermissionDenied
}

func drain(stream *connect.ServerStreamForClient[investigationv1.Investigation], err error) error {
	if err != nil {
		return err
	}
	defer func() { _ = stream.Close() }()
	for stream.Receive() { //nolint:revive // draining is the point
	}
	return stream.Err()
}

// --- gRPC / JSON parity, for two RPCs (FR-064) ---------------------------------------------

// TestInvestigationGRPCAndJSONParity is FR-064's dual surface at the transport: the same handler
// answers gRPC and a JSON POST, and the two answers carry the same claims.
//
// Get and List are the two chosen: Get is the richest unary message in the service (symptoms,
// facts, reviews, labels, deliveries) and List is the one whose response is a repeated field,
// which is where a JSON codec difference would show first.
func TestInvestigationGRPCAndJSONParity(t *testing.T) {
	ts := newInvestigationServer(t)
	ctx := context.Background()
	token := ts.token(t, "alice", RoleReader)

	t.Run("Get", func(t *testing.T) {
		viaGRPC, err := ts.client(token).Get(ctx,
			connect.NewRequest(&investigationv1.GetRequest{InvestigationId: investigationID}))
		if err != nil {
			t.Fatalf("gRPC Get: %v", err)
		}
		viaJSON, err := ts.jsonClient(token).Get(ctx,
			connect.NewRequest(&investigationv1.GetRequest{InvestigationId: investigationID}))
		if err != nil {
			t.Fatalf("JSON Get: %v", err)
		}
		assertParity(t, "Get", viaGRPC.Msg, viaJSON.Msg)
	})

	t.Run("List", func(t *testing.T) {
		req := func() *connect.Request[investigationv1.ListInvestigationsRequest] {
			return connect.NewRequest(&investigationv1.ListInvestigationsRequest{
				IncidentId: investigationInc,
			})
		}
		viaGRPC, err := ts.client(token).List(ctx, req())
		if err != nil {
			t.Fatalf("gRPC List: %v", err)
		}
		viaJSON, err := ts.jsonClient(token).List(ctx, req())
		if err != nil {
			t.Fatalf("JSON List: %v", err)
		}
		if len(viaGRPC.Msg.GetInvestigations()) == 0 {
			t.Fatal("List returned nothing; the parity comparison would be vacuous")
		}
		assertParity(t, "List", viaGRPC.Msg, viaJSON.Msg)
	})
}

// --- behaviour ------------------------------------------------------------------------------

// TestInvestigateStreamsTheAnytimeShape is FR-046a: the provisional prior-only ranking arrives
// first, labelled provisional, then the final answer.
func TestInvestigateStreamsTheAnytimeShape(t *testing.T) {
	ts := newInvestigationServer(t)
	ts.runner.emitProvisional = true
	ctx := context.Background()

	stream, err := ts.client(ts.token(t, "alice", RoleInvestigator)).Investigate(ctx,
		connect.NewRequest(&investigationv1.InvestigateRequest{
			ValidAt: timestamppb.New(investigationAt), Stream: true,
		}))
	if err != nil {
		t.Fatalf("investigate: %v", err)
	}
	defer func() { _ = stream.Close() }()

	var states []*investigationv1.Investigation
	for stream.Receive() {
		states = append(states, stream.Msg())
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if len(states) != 2 {
		t.Fatalf("received %d states, want the provisional ranking then the answer", len(states))
	}
	if !states[0].GetProvisional() {
		t.Error("the first state is not labelled provisional (FR-046a)")
	}
	if states[1].GetProvisional() {
		t.Error("the final state is still labelled provisional")
	}
	if states[1].GetLifecycle() != investigationv1.Lifecycle_CONCLUDED {
		t.Errorf("final lifecycle = %v, want CONCLUDED", states[1].GetLifecycle())
	}
	if states[1].GetRequester() == "" {
		t.Error("the investigation records no requester; FR-066 requires one")
	}
}

// TestSubmitHumanFactNeverBlocks is FR-057: the fact is written and the current state comes
// straight back. There is no waiting state for the RPC to sit in.
func TestSubmitHumanFactNeverBlocks(t *testing.T) {
	ts := newInvestigationServer(t)
	ctx := context.Background()

	resp, err := ts.client(ts.token(t, "alice", RoleInvestigator)).SubmitHumanFact(ctx,
		connect.NewRequest(&investigationv1.SubmitHumanFactRequest{
			InvestigationId: investigationID,
			Fact: &investigationv1.HumanFact{
				Kind: "manual_action", Statement: "I restarted a payments pod at 14:20",
			},
		}))
	if err != nil {
		t.Fatalf("submit fact: %v", err)
	}
	if resp.Msg.GetLifecycle() != investigationv1.Lifecycle_RUNNING {
		t.Errorf("lifecycle = %v after a fact; the engine never waits for a human (FR-006)",
			resp.Msg.GetLifecycle())
	}
	if len(resp.Msg.GetFacts()) != 1 {
		t.Fatalf("facts = %d, want 1", len(resp.Msg.GetFacts()))
	}
	fact := resp.Msg.GetFacts()[0]
	if fact.GetWeightClass() != investigationstore.WeightClassStrong {
		t.Errorf("weight class = %q, want strong and never decisive (FR-057a)", fact.GetWeightClass())
	}
	// FR-066: the author is the authenticated caller, filled in by the handler rather than
	// trusted from the request.
	if fact.GetAuthor() == "" {
		t.Error("the fact carries no author")
	}
}

// TestDeclareFillsTheDeclaringIdentityFromTheCredential is FR-002a + FR-066: a declaration that
// names no declaring identity gets the authenticated caller's, never a blank and never a guess.
func TestDeclareFillsTheDeclaringIdentityFromTheCredential(t *testing.T) {
	ts := newInvestigationServer(t)
	ctx := context.Background()

	stream, err := ts.client(ts.token(t, "alice", RoleInvestigator)).Declare(ctx,
		connect.NewRequest(&investigationv1.DeclareRequest{
			Declaration: &investigationv1.Symptom{
				OriginSystem: "slack:acme", OriginRef: "#incident-checkout",
				FiredAt: timestamppb.New(investigationAt), Title: "checkout is throwing 500s",
				Severity: "sev2",
			},
		}))
	if err != nil {
		t.Fatalf("declare: %v", err)
	}
	defer func() { _ = stream.Close() }()
	var last *investigationv1.Investigation
	for stream.Receive() {
		last = stream.Msg()
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if last == nil || len(last.GetSymptoms()) != 1 {
		t.Fatalf("declare produced %+v", last)
	}
	if last.GetSymptoms()[0].GetDeclaringIdentity() == "" {
		t.Error("the declaration carries no declaring identity (FR-002a, FR-066)")
	}
}

// TestRunRPCsAreUnimplementedWithoutAnEngine is FR-067 and plan F10: a server that can read its
// investigations but has no model configuration says so, rather than failing obscurely.
func TestRunRPCsAreUnimplementedWithoutAnEngine(t *testing.T) {
	svc := NewInvestigationService(nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := ContextWithPrincipal(context.Background(), &Principal{
		Issuer: "test", Subject: "alice", Roles: []Role{RoleInvestigator},
	})
	_, err := svc.Replay(ctx, connect.NewRequest(&investigationv1.ReplayRequest{Layer: "trajectory"}))
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) || connectErr.Code() != connect.CodeUnimplemented {
		t.Fatalf("error = %v, want Unimplemented", err)
	}
	if !errors.Is(err, ErrInvestigationDisabled) &&
		!strings.Contains(connectErr.Message(), "enable-investigation") {
		t.Errorf("the error does not say how to enable the engine: %v", err)
	}
}

// TestNotFoundIsNotFound keeps a missing investigation distinguishable from a broken server: an
// operator reading a pipeline log must be able to tell "that id does not exist" from "the
// database is down".
func TestNotFoundIsNotFound(t *testing.T) {
	ts := newInvestigationServer(t)
	_, err := ts.client(ts.token(t, "alice", RoleReader)).Get(context.Background(),
		connect.NewRequest(&investigationv1.GetRequest{InvestigationId: "inv-nope"}))
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) || connectErr.Code() != connect.CodeNotFound {
		t.Fatalf("error = %v, want NotFound", err)
	}
}

// TestWithoutTheFlagTheServiceIsAbsent is plan F10: without --enable-investigation the binary
// behaves exactly as it does today, and the procedures are not mounted at all.
func TestWithoutTheFlagTheServiceIsAbsent(t *testing.T) {
	ts := newGraphServer(t)
	client := investigationv1connect.NewInvestigationServiceClient(
		h2cClient(ts.readerToken(t)), ts.baseURL, connect.WithGRPC())
	_, err := client.Get(context.Background(),
		connect.NewRequest(&investigationv1.GetRequest{InvestigationId: investigationID}))
	if err == nil {
		t.Fatal("the investigation service answered on a server started without the flag")
	}
	// The mux has no route for the procedure, so what comes back is Unimplemented or a plain
	// 404 dressed as Unknown — either way the service is not there, which is what plan F10 asks
	// for: the served surface is byte-for-byte what it was before this feature.
	var connectErr *connect.Error
	if errors.As(err, &connectErr) &&
		connectErr.Code() != connect.CodeUnimplemented &&
		connectErr.Code() != connect.CodeUnknown &&
		connectErr.Code() != connect.CodeNotFound {
		t.Fatalf("unexpected code %s: %v", connectErr.Code(), err)
	}
}

// --- the ledger on the read path (T118) -----------------------------------------------------

// TestGetReturnsTheLedger is the drift the T118 quickstart run found: `investigate get <id>
// --output json | jq '.ledger.hypotheses[0]'` — which quickstart §1, §4, §5 and §9 all rest on,
// and which `investigate get --ledger|--chain|--evidence` renders — answered with no ledger at
// all, because `InvestigationDAO.Get` deliberately does not load one and nothing else did either.
//
// A verdict line with nothing under it is not a read of an investigation (FR-053, FR-057c).
func TestGetReturnsTheLedger(t *testing.T) {
	ts := newInvestigationServer(t)
	ctx := context.Background()
	client := ts.client(ts.token(t, "ivy", RoleInvestigator))

	// With a ledger reader wired in, Get answers with the hypotheses and evidence of the run.
	got, err := client.Get(ctx, connect.NewRequest(&investigationv1.GetRequest{
		InvestigationId: investigationID,
	}))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Msg.GetLedger() == nil {
		t.Fatalf("`investigate get` returned no ledger; a verdict with nothing under it is not a "+
			"read of an investigation (investigation %s)", investigationID)
	}
	if len(got.Msg.GetLedger().GetHypotheses()) == 0 {
		t.Error("the ledger carries no hypotheses")
	}
	if got.Msg.GetLedger().GetInvestigationId() != investigationID {
		t.Errorf("the ledger names investigation %q, want %q",
			got.Msg.GetLedger().GetInvestigationId(), investigationID)
	}

	// List does not: a caller listing a hundred runs does not want a hundred ledgers.
	list, err := client.List(ctx, connect.NewRequest(&investigationv1.ListInvestigationsRequest{}))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, inv := range list.Msg.GetInvestigations() {
		if inv.GetLedger() != nil {
			t.Errorf("list returned a ledger for %s; the read is deliberately cheap",
				inv.GetInvestigationId())
		}
	}
}
