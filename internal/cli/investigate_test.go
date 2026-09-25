// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/investigation/intake"
	investigationstore "github.com/Pierre-Theophile/aisre/internal/investigation/store"
	"github.com/Pierre-Theophile/aisre/internal/projector"
	"github.com/Pierre-Theophile/aisre/internal/server"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres/pgtest"
)

// `investigate …` at the CLI (T088, contracts/cli.md §Investigate).
//
// Two halves. The usage errors run with no server at all, because a command that refuses a
// malformed invocation before opening a connection is what makes exit 1 mean "you typed it
// wrong" rather than "the server disagreed". The end-to-end half runs `investigate declare`
// against a real database with a stub engine, which is the path an on-call actually takes.

const declaredAtRFC = "2026-09-01T14:35:00Z"

var cliDeclaredAt = time.Date(2026, 9, 1, 14, 35, 0, 0, time.UTC)

// --- usage errors and exit codes ------------------------------------------------------------

// TestInvestigateUsageErrorsExitOne is contracts/cli.md §Exit codes: a malformed invocation is 1,
// and it is 1 without a server, because nothing is sent.
func TestInvestigateUsageErrorsExitOne(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		args []string
		want string
	}{
		{
			"investigate with no instant",
			[]string{"investigate", "otel.service.name=checkout"},
			"--at is required",
		},
		{
			"investigate with a malformed reference",
			[]string{"investigate", "checkout", "--at", declaredAtRFC},
			"<namespace>=<value>",
		},
		{
			"investigate with a malformed instant",
			[]string{"investigate", "otel.service.name=checkout", "--at", "yesterday"},
			"RFC 3339",
		},
		{
			"declare with nothing",
			[]string{"investigate", "declare"},
			"--severity, --at, --title, --origin required",
		},
		{
			"declare with no origin",
			[]string{"investigate", "declare", "--severity", "sev2", "--at", declaredAtRFC,
				"--title", "checkout 500s"},
			"--origin required",
		},
		{
			"declare with an origin that is not <system>:<stable-id>",
			[]string{"investigate", "declare", "--severity", "sev2", "--at", declaredAtRFC,
				"--title", "checkout 500s", "--origin", "slack"},
			"<system>:<stable-id>",
		},
		{
			"get with no id",
			[]string{"investigate", "get"},
			"investigation id is required",
		},
		{
			"list with a status outside the published four",
			[]string{"investigate", "list", "--status", "waiting"},
			"the lifecycle is exactly",
		},
		{
			"fact with a kind outside the published set",
			[]string{"investigate", "fact", "inv-1", "--kind", "hunch", "--statement", "x"},
			"--kind",
		},
		{
			"fact with no statement",
			[]string{"investigate", "fact", "inv-1", "--kind", "manual_action"},
			"--statement is required",
		},
		{
			"review with no root cause",
			[]string{"investigate", "review", "inv-1", "--reason", "because"},
			"--root-cause is required",
		},
		{
			"review with no reason",
			[]string{"investigate", "review", "inv-1", "--root-cause", "unobserved"},
			"--reason is required",
		},
		{
			"review with a status the ledger does not publish",
			[]string{"investigate", "review", "inv-1", "--root-cause", "unobserved",
				"--reason", "because", "--amend", "h-1=maybe"},
			"not one the ledger publishes",
		},
		{
			"label with neither flag",
			[]string{"investigate", "label", "inv-1"},
			"exactly one of --right or --wrong",
		},
		{
			"label with both flags",
			[]string{"investigate", "label", "inv-1", "--right", "--wrong"},
			"exactly one of --right or --wrong",
		},
		{
			"export with no --out",
			[]string{"investigate", "export", "inv-1"},
			"--out <dir> is required",
		},
		{
			"replay with no --from",
			[]string{"investigate", "replay"},
			"--from <dir> is required",
		},
		{
			"replay with an unknown layer",
			[]string{"investigate", "replay", "--from", "/tmp", "--layer", "both"},
			"want trajectory",
		},
		{
			"to-incident with no --out",
			[]string{"investigate", "to-incident", "inv-1"},
			"--out <dir> is required",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// The server address is deliberately unreachable: a usage error must be detected
			// before any connection is attempted, or the exit code would be 2.
			args := append([]string{"--server", "http://127.0.0.1:1", "--token", "t"}, tc.args...)
			_, stderr, code := run(t, context.Background(), args...)
			if code != ExitUsage {
				t.Fatalf("exit %d, want %d (stderr %q)", code, ExitUsage, stderr)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr = %q, want it to mention %q", stderr, tc.want)
			}
		})
	}
}

// TestInvestigateTransportFailureExitsTwo is the other half of the code contract: the command is
// well formed and the server is not there, which is 2 and not 1.
func TestInvestigateTransportFailureExitsTwo(t *testing.T) {
	t.Parallel()

	_, stderr, code := run(t, context.Background(),
		"--server", "http://127.0.0.1:1", "--token", "t",
		"investigate", "get", "inv-nope")
	if code != ExitTransport {
		t.Fatalf("exit %d, want %d (stderr %q)", code, ExitTransport, stderr)
	}
}

// --- end to end, against a real database with a stub engine -------------------------------

// cliStubRunner is the engine as the CLI needs it: it writes a real investigation through the
// DAO so that `get`, `list` and `report` have something to read, and answers immediately.
type cliStubRunner struct {
	dao *investigationstore.InvestigationDAO
}

func (r *cliStubRunner) Investigate(
	ctx context.Context, req *investigationv1.InvestigateRequest, requester string,
	emit func(*investigationv1.Investigation) error,
) (*investigationv1.Investigation, error) {
	return r.open(ctx, req.GetSymptom(), requester, emit)
}

func (r *cliStubRunner) Declare(
	ctx context.Context, req *investigationv1.DeclareRequest, requester string,
	emit func(*investigationv1.Investigation) error,
) (*investigationv1.Investigation, error) {
	return r.open(ctx, req.GetDeclaration(), requester, emit)
}

func (r *cliStubRunner) open(
	ctx context.Context, symptom *investigationv1.Symptom, requester string,
	emit func(*investigationv1.Investigation) error,
) (*investigationv1.Investigation, error) {
	at := symptom.GetFiredAt().AsTime()
	in := &intake.Intake{Symptom: symptom, ValidAt: at, ObservedAt: at}
	incidentID := intake.IncidentID(in, at)
	investigationID := "inv-" + strings.TrimPrefix(incidentID, "inc:")[:12]

	id, err := r.dao.Open(ctx,
		investigationstore.Incident{
			IncidentID: incidentID, CanonicalSubjectID: "e:checkout",
			OpenedAt: at, LastSymptomAt: at,
			AssociationRuleVersion: intake.AssociationRuleVersion,
		},
		investigationstore.NewInvestigation{
			InvestigationID: investigationID, IncidentID: incidentID,
			ValidAt: at, ObservedAt: at,
			WindowStart: at.Add(-90 * time.Minute), WindowEnd: at,
			Profile: "page", Requester: requester,
			AlgebraVersion: "1.0.0", LedgerRuleVersion: "1.0.0", SchemaVersion: "1.0.0",
			StartedAt: at,
		},
		symptom,
	)
	if err != nil {
		return nil, err
	}
	if err := emit(&investigationv1.Investigation{
		InvestigationId: id, Provisional: true,
		Lifecycle: investigationv1.Lifecycle_RUNNING,
	}); err != nil {
		return nil, err
	}
	return r.dao.Get(ctx, id)
}

func (r *cliStubRunner) Replay(context.Context, *investigationv1.ReplayRequest) (*investigationv1.ReplayResponse, error) {
	return &investigationv1.ReplayResponse{Identical: false, FirstDivergingRecord: "seq 12: metrics.compare"}, nil
}

func (r *cliStubRunner) Export(_ context.Context, req *investigationv1.ExportRequest) (*investigationv1.ExportResponse, error) {
	return &investigationv1.ExportResponse{OutDir: req.GetOutDir(), ExportDigest: "sha256:abc"}, nil
}

func (r *cliStubRunner) Reopen(_ context.Context, parentID string, fact *investigationv1.HumanFact, requester string) (*investigationv1.Investigation, error) {
	return &investigationv1.Investigation{
		InvestigationId: "inv-child", ReopensInvestigationId: parentID,
		Requester: requester, Facts: []*investigationv1.HumanFact{fact},
	}, nil
}

// investigationServerFixture is a running server with the investigation service mounted.
func investigationServerFixture(t *testing.T) (baseURL, token string) {
	t.Helper()
	baseURL, token, _ = investigationServerFixtureWithStore(t)
	return baseURL, token
}

// investigationServerFixtureWithStore is the same server, handing back the database as well, for
// a test that has to put a row into a state the stub runner does not produce — a concluded
// investigation, say.
func investigationServerFixtureWithStore(t *testing.T) (baseURL, token string, store *postgres.Store) {
	t.Helper()

	store = pgtest.Open(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	auth, err := server.NewDevAuthenticator(server.DevConfig{Enabled: true, Logger: logger})
	if err != nil {
		t.Fatalf("dev authenticator: %v", err)
	}
	dao := investigationstore.NewInvestigationDAO(store)
	srv, err := server.New(server.Config{
		Listen:    "127.0.0.1:0",
		Auth:      auth,
		Projector: projector.New(store),
		Logger:    logger,
		Investigation: server.NewInvestigationService(
			&cliStubRunner{dao: dao}, dao, logger),
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
		Name: "alice", Roles: []server.Role{server.RoleInvestigator, server.RoleDecider},
	})
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}
	return "http://" + srv.Addr(), minted, store
}

// TestInvestigateDeclareEndToEnd is the User Story 1a path: a person declares an incident with no
// monitor, and the investigation opens, is readable, is re-delivered as a no-op, and produces
// both renderings from the same run (FR-001a, FR-008b, FR-064).
func TestInvestigateDeclareEndToEnd(t *testing.T) {
	baseURL, token := investigationServerFixture(t)
	ctx := context.Background()

	declare := []string{
		"--server", baseURL, "--token", token,
		"investigate", "declare",
		"--severity", "sev2",
		"--at", declaredAtRFC,
		"--title", "checkout is throwing 500s",
		"--origin", "slack:C0123STABLE",
		"--service", "k8s.service=shop/checkout",
	}
	stdout, stderr, code := run(t, ctx, declare...)
	if code != ExitOK {
		t.Fatalf("declare: exit %d (stderr %q)", code, stderr)
	}
	// The human form is the published order (FR-057c).
	for _, section := range []string{"## verdict", "## ranked", "## timeline", "## narrative"} {
		if !strings.Contains(stdout, section) {
			t.Errorf("the human rendering has no %q section:\n%s", section, stdout)
		}
	}

	// The same run, the machine form.
	jsonOut, stderr, code := run(t, ctx, append([]string{"--output", "json"}, declare...)...)
	if code != ExitOK {
		t.Fatalf("declare --output json: exit %d (stderr %q)", code, stderr)
	}
	var inv map[string]any
	if err := json.Unmarshal([]byte(jsonOut), &inv); err != nil {
		t.Fatalf("the machine rendering is not canonical JSON: %v\n%s", err, jsonOut)
	}
	id, _ := inv["investigationId"].(string)
	if id == "" {
		t.Fatalf("the machine rendering carries no investigation id:\n%s", jsonOut)
	}

	// FR-008b: the identical declaration is a no-op returning the same investigation.
	second, stderr, code := run(t, ctx, append([]string{"--output", "json"}, declare...)...)
	if code != ExitOK {
		t.Fatalf("re-declare: exit %d (stderr %q)", code, stderr)
	}
	var again map[string]any
	if err := json.Unmarshal([]byte(second), &again); err != nil {
		t.Fatalf("unmarshal second declare: %v", err)
	}
	if again["investigationId"] != id {
		t.Errorf("re-delivery opened %v, want the same investigation %q (FR-008b)",
			again["investigationId"], id)
	}

	// The symptom carries what was declared, and nothing that was not.
	symptoms, _ := inv["symptoms"].([]any)
	if len(symptoms) != 1 {
		t.Fatalf("symptoms = %d, want 1", len(symptoms))
	}
	symptom, _ := symptoms[0].(map[string]any)
	if symptom["severity"] != "sev2" {
		t.Errorf("severity = %v, want the declared sev2", symptom["severity"])
	}
	if symptom["origin"] != "INTAKE_ORIGIN_DECLARED" {
		t.Errorf("origin = %v, want INTAKE_ORIGIN_DECLARED (FR-002a)", symptom["origin"])
	}
	if symptom["declaringIdentity"] == "" || symptom["declaringIdentity"] == nil {
		t.Error("the declaration carries no authenticated declaring identity (FR-066)")
	}
	// FR-004a: the observed instant is the instant of declaration.
	if inv["observedAt"] != declaredAtRFC {
		t.Errorf("observedAt = %v, want the declaration instant %s (FR-004a)",
			inv["observedAt"], declaredAtRFC)
	}

	// `get` reads it back, and `list` finds it.
	_, stderr, code = run(t, ctx, "--server", baseURL, "--token", token, "investigate", "get", id)
	if code != ExitOK {
		t.Fatalf("get: exit %d (stderr %q)", code, stderr)
	}
	listOut, stderr, code := run(t, ctx, "--server", baseURL, "--token", token,
		"investigate", "list", "--status", "running")
	if code != ExitOK {
		t.Fatalf("list: exit %d (stderr %q)", code, stderr)
	}
	if !strings.Contains(listOut, id) {
		t.Errorf("list did not find %s:\n%s", id, listOut)
	}
}

// TestInvestigateHumanChannelEndToEnd walks a fact, a review and a label through the CLI, which
// is the FR-054/FR-057a/FR-057e path as a person types it.
func TestInvestigateHumanChannelEndToEnd(t *testing.T) {
	baseURL, token := investigationServerFixture(t)
	ctx := context.Background()

	declareOut, stderr, code := run(t, ctx, "--output", "json",
		"--server", baseURL, "--token", token,
		"investigate", "declare",
		"--severity", "sev1", "--at", declaredAtRFC, "--title", "payments is down",
		"--origin", "slack:C0999HUMAN", "--service", "k8s.service=shop/payments")
	if code != ExitOK {
		t.Fatalf("declare: exit %d (stderr %q)", code, stderr)
	}
	var inv map[string]any
	if err := json.Unmarshal([]byte(declareOut), &inv); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	id, _ := inv["investigationId"].(string)

	factOut, stderr, code := run(t, ctx, "--server", baseURL, "--token", token,
		"investigate", "fact", id,
		"--kind", "manual_action",
		"--statement", "I restarted a payments pod by hand at 14:20",
		"--entity", "k8s.service=shop/payments",
		"--from", "2026-09-01T14:20:00Z")
	if code != ExitOK {
		t.Fatalf("fact: exit %d (stderr %q)", code, stderr)
	}
	if !strings.Contains(factOut, "fact recorded") {
		t.Errorf("fact output = %q", factOut)
	}

	reviewOut, stderr, code := run(t, ctx, "--server", baseURL, "--token", token,
		"investigate", "review", id,
		"--root-cause", "not_change_induced:client config",
		"--reason", "a client rolled out a bad timeout")
	if code != ExitOK {
		t.Fatalf("review: exit %d (stderr %q)", code, stderr)
	}
	if !strings.Contains(reviewOut, "review recorded") {
		t.Errorf("review output = %q", reviewOut)
	}

	labelOut, stderr, code := run(t, ctx, "--server", baseURL, "--token", token,
		"investigate", "label", id, "--wrong")
	if code != ExitOK {
		t.Fatalf("label: exit %d (stderr %q)", code, stderr)
	}
	if !strings.Contains(labelOut, "wrong") {
		t.Errorf("label output = %q", labelOut)
	}

	// Everything is additive and readable back on the same investigation.
	getOut, stderr, code := run(t, ctx, "--output", "json",
		"--server", baseURL, "--token", token, "investigate", "get", id)
	if code != ExitOK {
		t.Fatalf("get: exit %d (stderr %q)", code, stderr)
	}
	var after map[string]any
	if err := json.Unmarshal([]byte(getOut), &after); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, field := range []string{"facts", "reviews", "labels"} {
		items, _ := after[field].([]any)
		if len(items) != 1 {
			t.Errorf("%s = %d, want 1", field, len(items))
		}
	}
}

// TestInvestigateReportDeliversWithoutFailingTheCommand is FR-057f at the CLI: a delivery that
// cannot happen is reported and the command still exits 0, because the investigation is complete
// whatever the connector did.
func TestInvestigateReportDeliversWithoutFailingTheCommand(t *testing.T) {
	baseURL, token := investigationServerFixture(t)
	ctx := context.Background()

	declareOut, stderr, code := run(t, ctx, "--output", "json",
		"--server", baseURL, "--token", token,
		"investigate", "declare",
		"--severity", "sev2", "--at", declaredAtRFC, "--title", "checkout 500s",
		"--origin", "slack:C0777REPORT")
	if code != ExitOK {
		t.Fatalf("declare: exit %d (stderr %q)", code, stderr)
	}
	var inv map[string]any
	if err := json.Unmarshal([]byte(declareOut), &inv); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	id, _ := inv["investigationId"].(string)

	// No credential configured: the delivery is `not_configured`, printed, and exit 0.
	t.Setenv(EnvDeliveryToken, "")
	out, stderr, code := run(t, ctx, "--server", baseURL, "--token", token,
		"investigate", "report", id, "--deliver")
	if code != ExitOK {
		t.Fatalf("report --deliver: exit %d, want 0: a delivery is never a precondition of "+
			"anything (FR-057f) (stderr %q)", code, stderr)
	}
	if !strings.Contains(out, "# investigation "+id) {
		t.Errorf("the report does not name the investigation:\n%s", out)
	}
	for _, section := range []string{"## verdict", "## ranked", "## timeline", "## narrative"} {
		if !strings.Contains(out, section) {
			t.Errorf("the delivered report has no %q section (FR-057f carries the FR-057c order)", section)
		}
	}

	// With a credential the log sink delivers and says what it edited in place.
	t.Setenv(EnvDeliveryToken, "s3cr3t")
	out, stderr, code = run(t, ctx, "--server", baseURL, "--token", token,
		"investigate", "report", id, "--deliver")
	if code != ExitOK {
		t.Fatalf("report --deliver: exit %d (stderr %q)", code, stderr)
	}
	if !strings.Contains(out, "delivered to slack:") {
		t.Errorf("the delivery was not reported:\n%s", out)
	}
}

// TestInvestigateReplayDivergenceExitsFour is contracts/cli.md: a replay that does not reproduce
// its recording is a verification failure, exit 4, naming the first diverging record.
func TestInvestigateReplayDivergenceExitsFour(t *testing.T) {
	baseURL, token := investigationServerFixture(t)

	// --remote: `investigate replay` replays locally by default (a recording on disk needs no
	// server), and this case is about what the *server* reports when it diverges.
	_, stderr, code := run(t, context.Background(), "--server", baseURL, "--token", token,
		"investigate", "replay", "--remote", "--from", t.TempDir(), "--layer", "trajectory")
	if code != ExitVerification {
		t.Fatalf("exit %d, want %d (stderr %q)", code, ExitVerification, stderr)
	}
	if !strings.Contains(stderr, "seq 12") {
		t.Errorf("the failure does not name the first diverging record: %q", stderr)
	}
}

// TestInvestigateWithoutARoleExitsThree is FR-008/FR-066 at the CLI: a credential that cannot run
// an investigation is an auth failure, not a transport one.
func TestInvestigateWithoutARoleExitsThree(t *testing.T) {
	baseURL, _ := investigationServerFixture(t)

	// A reader may read but may not run.
	readerToken := mintReaderToken(t)
	_, stderr, code := run(t, context.Background(), "--server", baseURL, "--token", readerToken,
		"investigate", "declare", "--severity", "sev2", "--at", declaredAtRFC,
		"--title", "checkout 500s", "--origin", "slack:C0123")
	if code != ExitAuth {
		t.Fatalf("exit %d, want %d (stderr %q)", code, ExitAuth, stderr)
	}

	// And an anonymous caller is refused everywhere (FR-066).
	_, _, code = run(t, context.Background(), "--server", baseURL, "--token", "",
		"investigate", "get", "inv-1")
	if code != ExitAuth {
		t.Fatalf("anonymous get: exit %d, want %d", code, ExitAuth)
	}
}

// TestToIncidentRefusesAnUnreviewedInvestigation is FR-055: a corpus incident's ground truth is a
// human's validated root cause, so an investigation nobody reviewed cannot become one.
func TestToIncidentRefusesAnUnreviewedInvestigation(t *testing.T) {
	baseURL, token := investigationServerFixture(t)
	ctx := context.Background()

	declareOut, _, code := run(t, ctx, "--output", "json",
		"--server", baseURL, "--token", token,
		"investigate", "declare",
		"--severity", "sev2", "--at", declaredAtRFC, "--title", "checkout 500s",
		"--origin", "slack:C0555CORPUS")
	if code != ExitOK {
		t.Fatalf("declare: exit %d", code)
	}
	var inv map[string]any
	if err := json.Unmarshal([]byte(declareOut), &inv); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	id, _ := inv["investigationId"].(string)

	_, stderr, code := run(t, ctx, "--server", baseURL, "--token", token,
		"investigate", "to-incident", id, "--out", t.TempDir())
	if code != ExitUsage {
		t.Fatalf("exit %d, want %d (stderr %q)", code, ExitUsage, stderr)
	}
	if !strings.Contains(stderr, "no review recorded") {
		t.Errorf("stderr = %q, want it to say a review is required (FR-055)", stderr)
	}
}

// TestParseOriginKeysOnTheStableIdentifier is FR-008b at the flag: the split is on the first
// colon, so an identifier containing colons survives.
func TestParseOriginKeysOnTheStableIdentifier(t *testing.T) {
	t.Parallel()

	system, id, err := parseOrigin("jira:PROJ-123:comment:9")
	if err != nil {
		t.Fatalf("parse origin: %v", err)
	}
	if system != "jira" || id != "PROJ-123:comment:9" {
		t.Errorf("got (%q, %q), want (jira, PROJ-123:comment:9)", system, id)
	}

	// The key derived from it is the published 4-tuple with an empty group.
	want := intake.DeclarationKey("jira", "PROJ-123:comment:9", cliDeclaredAt)
	if got := intake.DeclarationKey(system, id, cliDeclaredAt); got != want {
		t.Errorf("key = %q, want %q", got, want)
	}
	if _, _, err := parseOrigin("slack"); ExitCode(err) != ExitUsage {
		t.Errorf("a bare system is a usage error")
	}
}

// TestRenderHumanRefusesARemediation keeps FR-028 true at the CLI boundary too: a server that
// returned a rendering proposing an action is a verification failure, not something the CLI
// prints with a warning.
func TestRenderHumanRefusesARemediation(t *testing.T) {
	t.Parallel()

	_, err := renderHuman(&investigationv1.Investigation{
		InvestigationId: "inv-1",
		VerdictLine:     "Next steps: roll back shop/payments to rev6.",
		StartedAt:       timestamppb.New(cliDeclaredAt),
	})
	if err == nil {
		t.Fatal("the CLI printed a rendering that proposes a remediation (FR-028)")
	}
}

// TestReportDeliveryIsRecordedInTheLedger is the other half of FR-057f: "a delivery that fails is
// recorded and the investigation still concludes".
//
// The delivery path never wrote `investigation.report_deliveries`, so the table only ever moved in
// the store's own unit tests and an operator asking "did the on-call ever see this, and how many
// times has it been edited?" had nowhere to look. What this asserts is the invariant the table
// exists for: a report is **edited in place**, so two deliveries of one investigation to one
// target are one row with `update_count` 2 — never two rows, which would make the ledger the
// stream of messages FR-057f forbids.
func TestReportDeliveryIsRecordedInTheLedger(t *testing.T) {
	baseURL, token, store := investigationServerFixtureWithStore(t)
	ctx := context.Background()
	dsn := store.Pool().Config().ConnString()

	declareOut, stderr, code := run(t, ctx, "--output", "json",
		"--server", baseURL, "--token", token,
		"investigate", "declare",
		"--severity", "sev2", "--at", declaredAtRFC, "--title", "checkout 500s",
		"--origin", "slack:C0888LEDGER")
	if code != ExitOK {
		t.Fatalf("declare: exit %d (stderr %q)", code, stderr)
	}
	var inv map[string]any
	if err := json.Unmarshal([]byte(declareOut), &inv); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	id, _ := inv["investigationId"].(string)
	if id == "" {
		t.Fatal("declare returned no investigation id")
	}

	t.Setenv(EnvDeliveryToken, "s3cr3t")
	for attempt := 1; attempt <= 2; attempt++ {
		out, stderr, code := run(t, ctx, "--server", baseURL, "--token", token,
			"investigate", "report", id, "--deliver", "--db", dsn)
		if code != ExitOK {
			t.Fatalf("report --deliver (attempt %d): exit %d (stderr %q)", attempt, code, stderr)
		}
		if !strings.Contains(out, "recorded in the delivery ledger") {
			t.Errorf("attempt %d did not say it recorded the delivery:\n%s", attempt, out)
		}
	}

	rows, err := store.Pool().Query(ctx, `
		SELECT delivery_id, update_count, outcome, coalesce(external_message_ref, '')
		FROM investigation.report_deliveries WHERE investigation_id = $1`, id)
	if err != nil {
		t.Fatalf("read the delivery ledger: %v", err)
	}
	defer rows.Close()

	var (
		found       int
		deliveryID  string
		updateCount int32
		outcome     string
		externalRef string
	)
	for rows.Next() {
		if err := rows.Scan(&deliveryID, &updateCount, &outcome, &externalRef); err != nil {
			t.Fatalf("scan: %v", err)
		}
		found++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the delivery ledger: %v", err)
	}
	if found != 1 {
		t.Fatalf("the ledger holds %d row(s) for %s; a report is edited in place, so two "+
			"deliveries to one place are one row (FR-057f)", found, id)
	}
	// The target ref is the declaration's origin ref as it was given, prefix and all: the place
	// the incident lives is `slack:C0888LEDGER`, and the id is derived from it rather than from
	// a re-split of it.
	if want := investigationstore.DeliveryIDFor(id, "slack", "slack:C0888LEDGER"); deliveryID != want {
		t.Errorf("delivery id = %q, want the canonical %q", deliveryID, want)
	}
	if updateCount != 2 {
		t.Errorf("update_count = %d after two deliveries, want 2", updateCount)
	}
	if outcome != investigationstore.DeliveryDelivered {
		t.Errorf("outcome = %q, want %q", outcome, investigationstore.DeliveryDelivered)
	}
	if externalRef == "" {
		t.Error("no external message reference was recorded, so the next delivery has nothing " +
			"to edit in place")
	}
}
