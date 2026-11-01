// SPDX-License-Identifier: Apache-2.0

package mcpserver_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1/graphv1connect"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1/investigationv1connect"
	"github.com/Pierre-Theophile/aisre/internal/mcpserver"
)

// The investigation tools and the HTTP transport are tested against a fake API: what matters is
// what the adapter sends (the request, the caller's own token) and what it does with the stream,
// not the engine, which has its own tests.

// fakeAPI is an identity service and an investigation service that record what they receive.
type fakeAPI struct {
	graphv1connect.UnimplementedIdentityServiceHandler
	investigationv1connect.UnimplementedInvestigationServiceHandler

	// roles maps a bearer token to the roles WhoAmI answers; an unknown token is unauthenticated.
	roles map[string][]string

	mu       sync.Mutex
	tokens   []string // every Authorization header received, in order
	symptoms []*investigationv1.Symptom
	declared []*investigationv1.DeclareRequest
	release  chan struct{} // closed to let running investigations finish
}

func (f *fakeAPI) seen(h http.Header) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokens = append(f.tokens, h.Get("Authorization"))
}

func (f *fakeAPI) WhoAmI(_ context.Context, req *connect.Request[graphv1.WhoAmIRequest]) (*connect.Response[graphv1.WhoAmIResponse], error) {
	f.seen(req.Header())
	token := strings.TrimPrefix(req.Header().Get("Authorization"), "Bearer ")
	roles, ok := f.roles[token]
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, nil)
	}
	return connect.NewResponse(&graphv1.WhoAmIResponse{Subject: token, Issuer: "test", Roles: roles}), nil
}

func (f *fakeAPI) Investigate(ctx context.Context, req *connect.Request[investigationv1.InvestigateRequest], stream *connect.ServerStream[investigationv1.Investigation]) error {
	f.seen(req.Header())
	f.mu.Lock()
	f.symptoms = append(f.symptoms, req.Msg.GetSymptom())
	id := "inv-" + string(rune('a'+len(f.symptoms)-1))
	f.mu.Unlock()
	if err := stream.Send(&investigationv1.Investigation{InvestigationId: id, Provisional: true,
		Lifecycle: investigationv1.Lifecycle_RUNNING}); err != nil {
		return err
	}
	select {
	case <-f.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return stream.Send(&investigationv1.Investigation{InvestigationId: id, Lifecycle: investigationv1.Lifecycle_CONCLUDED,
		VerdictLine: "payments rev 7"})
}

func (f *fakeAPI) Declare(_ context.Context, req *connect.Request[investigationv1.DeclareRequest], stream *connect.ServerStream[investigationv1.Investigation]) error {
	f.seen(req.Header())
	f.mu.Lock()
	f.declared = append(f.declared, req.Msg)
	f.mu.Unlock()
	return stream.Send(&investigationv1.Investigation{InvestigationId: "inv-declared",
		Lifecycle: investigationv1.Lifecycle_RUNNING, Provisional: true})
}

func (f *fakeAPI) Get(_ context.Context, req *connect.Request[investigationv1.GetRequest]) (*connect.Response[investigationv1.Investigation], error) {
	f.seen(req.Header())
	if req.Msg.GetInvestigationId() != "inv-a" {
		return nil, connect.NewError(connect.CodeNotFound, nil)
	}
	return connect.NewResponse(&investigationv1.Investigation{InvestigationId: "inv-a",
		Lifecycle: investigationv1.Lifecycle_CONCLUDED, VerdictLine: "payments rev 7 broke checkout"}), nil
}

func newFakeAPI(t *testing.T) (*fakeAPI, string) {
	t.Helper()
	f := &fakeAPI{
		roles: map[string][]string{
			"tok-reader": {"reader"}, "tok-inv": {"investigator"}, "tok-feeder": {"feeder"},
		},
		release: make(chan struct{}),
	}
	mux := http.NewServeMux()
	mux.Handle(graphv1connect.NewIdentityServiceHandler(f))
	mux.Handle(investigationv1connect.NewInvestigationServiceHandler(f))
	ts := httptest.NewServer(mux)
	t.Cleanup(func() {
		select {
		case <-f.release:
		default:
			close(f.release)
		}
		ts.Close()
	})
	return f, ts.URL
}

func fakeSession(t *testing.T, baseURL, token string, limit *mcpserver.RunningLimit) *mcp.ClientSession {
	t.Helper()
	api := mcpserver.Connect(baseURL, nil, func() (string, error) { return token, nil })
	caller, err := api.WhoAmI(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	srv, err := mcpserver.New(mcpserver.Options{Clients: mcpserver.Clients{Investigation: api.Clients.Investigation},
		Roles: caller.Roles, Subject: caller.Subject, Running: limit})
	if err != nil {
		t.Fatal(err)
	}
	return connectSession(t, srv)
}

// TestInvestigationStartReturnsAtOnceAndIsBounded is US3, FR-050–FR-054: the tool returns the
// first, provisional state while the run continues; the symptom says it came over MCP; the
// per-caller bound refuses a fourth run and frees a slot when a run ends.
func TestInvestigationStartReturnsAtOnceAndIsBounded(t *testing.T) {
	f, baseURL := newFakeAPI(t)
	limit := mcpserver.NewRunningLimit(3)
	cs := fakeSession(t, baseURL, "tok-inv", limit)

	for i := 0; i < 3; i++ {
		start := time.Now()
		res := call(t, cs, "investigation_start", map[string]any{"reference": "otel.service.name=checkout", "at": "2026-09-01T14:32:00Z"})
		if res.IsError {
			t.Fatalf("start %d: %s", i, text(res))
		}
		if time.Since(start) > 5*time.Second {
			t.Fatalf("start %d waited for the run to finish", i)
		}
		if !strings.Contains(text(res), "provisional") {
			t.Errorf("start %d: summary does not say provisional: %q", i, text(res))
		}
	}
	if got := limit.Running("tok-inv"); got != 3 {
		t.Fatalf("running = %d, want 3", got)
	}
	refused := call(t, cs, "investigation_start", map[string]any{"reference": "otel.service.name=checkout", "at": "now"})
	if !refused.IsError || !strings.Contains(text(refused), "bound is 3") {
		t.Fatalf("fourth start not refused with the bound: %s", text(refused))
	}

	f.mu.Lock()
	for _, s := range f.symptoms {
		if s.GetOriginSystem() != mcpserver.OriginSystem || s.GetTransport() != "explicit_reference" {
			t.Errorf("symptom origin %q transport %q, want %q explicit_reference", s.GetOriginSystem(), s.GetTransport(), mcpserver.OriginSystem)
		}
	}
	f.mu.Unlock()

	close(f.release)
	deadline := time.Now().Add(5 * time.Second)
	for limit.Running("tok-inv") > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := limit.Running("tok-inv"); got != 0 {
		t.Fatalf("running = %d after the runs ended, want 0", got)
	}
}

// TestDeclarationKeepsTheCLIsIdempotencyKey is FR-052: a declaration over MCP carries the key
// `aisre investigate declare` computes, so declaring twice is one investigation.
func TestDeclarationKeepsTheCLIsIdempotencyKey(t *testing.T) {
	f, baseURL := newFakeAPI(t)
	cs := fakeSession(t, baseURL, "tok-inv", mcpserver.NewRunningLimit(3))
	args := map[string]any{"at": "2026-09-01T14:00:00Z", "declaration": map[string]any{
		"title": "checkout 5xx", "severity": "sev2", "origin": "slack:C0123", "services": []string{"otel.service.name=checkout"},
	}}
	for i := 0; i < 2; i++ {
		if res := call(t, cs, "investigation_start", args); res.IsError {
			t.Fatalf("declare: %s", text(res))
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.declared) != 2 {
		t.Fatalf("%d declarations reached the API, want 2", len(f.declared))
	}
	a, b := f.declared[0].GetDeclaration(), f.declared[1].GetDeclaration()
	if a.GetIdempotencyKey() == "" || a.GetIdempotencyKey() != b.GetIdempotencyKey() {
		t.Errorf("idempotency keys %q and %q: want one non-empty key", a.GetIdempotencyKey(), b.GetIdempotencyKey())
	}
	if len(a.GetTargetRefs()) != 1 || a.GetTargetRefs()[0].GetProvenance() != investigationv1.TargetRefProvenance_SUPPLIED_BY_HUMAN {
		t.Errorf("targets %v: want one supplied by a person", a.GetTargetRefs())
	}

	missing := call(t, cs, "investigation_start", map[string]any{"at": "now", "declaration": map[string]any{"title": "x"}})
	if !missing.IsError {
		t.Errorf("a declaration without severity and origin was not refused: %s", text(missing))
	}
}

// TestInvestigationGetCarriesTheVerdict is US2 at the adapter's boundary, and FR-034 for a
// missing investigation.
func TestInvestigationGetCarriesTheVerdict(t *testing.T) {
	_, baseURL := newFakeAPI(t)
	cs := fakeSession(t, baseURL, "tok-reader", nil)
	if slices.Contains(toolNames(t, cs), "investigation_start") {
		t.Fatal("a reader is offered investigation_start")
	}
	res := call(t, cs, "investigation_get", map[string]any{"investigation_id": "inv-a"})
	if !strings.Contains(string(structured(t, res)), "payments rev 7 broke checkout") {
		t.Errorf("verdict missing: %s", structured(t, res))
	}
	missing := call(t, cs, "investigation_get", map[string]any{"investigation_id": "nope"})
	if !missing.IsError || missing.Meta["aisre.code"] != "not_found" {
		t.Errorf("missing investigation: error=%v code=%v", missing.IsError, missing.Meta["aisre.code"])
	}
}

// TestHTTPForwardsEachRequestsOwnToken is FR-012 and SC-003 over streamable HTTP: no token is
// 401, an unknown token is 401, a feeder token is 403, and each request reaches the API with its
// own token and no other.
func TestHTTPForwardsEachRequestsOwnToken(t *testing.T) {
	f, baseURL := newFakeAPI(t)
	h := mcpserver.NewHTTPHandler(mcpserver.HTTPOptions{APIURL: baseURL, Running: mcpserver.NewRunningLimit(3)})
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)

	post := func(token string) int {
		req, _ := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	for _, c := range []struct {
		token string
		want  int
	}{{"", 401}, {"tok-unknown", 401}, {"tok-feeder", 403}} {
		if got := post(c.token); got != c.want {
			t.Errorf("token %q: status %d, want %d", c.token, got, c.want)
		}
	}

	for _, token := range []string{"tok-reader", "tok-inv"} {
		client := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "0"}, nil)
		transport := &mcp.StreamableClientTransport{Endpoint: ts.URL, HTTPClient: &http.Client{
			Transport: headerTransport{token: token},
		}}
		cs, err := client.Connect(context.Background(), transport, nil)
		if err != nil {
			t.Fatalf("%s: connect: %v", token, err)
		}
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "investigation_get",
			Arguments: map[string]any{"investigation_id": "inv-a"}})
		if err != nil || res.IsError {
			t.Fatalf("%s: investigation_get: %v %v", token, err, res)
		}
		_ = cs.Close()
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	for _, got := range f.tokens {
		if got == "" {
			t.Error("a request reached the API without a token")
		}
	}
	if !slices.Contains(f.tokens, "Bearer tok-reader") || !slices.Contains(f.tokens, "Bearer tok-inv") {
		t.Errorf("tokens seen by the API: %v", f.tokens)
	}
}

type headerTransport struct{ token string }

func (h headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+h.token)
	return http.DefaultTransport.RoundTrip(req)
}

// TestCheckListen is FR-003.
func TestCheckListen(t *testing.T) {
	for _, c := range []struct {
		addr string
		tls  bool
		ok   bool
	}{
		{"127.0.0.1:8090", false, true}, {"localhost:8090", false, true}, {"[::1]:8090", false, true},
		{"0.0.0.0:8090", false, false}, {":8090", false, false}, {"10.0.0.5:8090", false, false},
		{"0.0.0.0:8090", true, true},
	} {
		if err := mcpserver.CheckListen(c.addr, c.tls); (err == nil) != c.ok {
			t.Errorf("CheckListen(%q, tls=%v) = %v, want ok=%v", c.addr, c.tls, err, c.ok)
		}
	}
}
