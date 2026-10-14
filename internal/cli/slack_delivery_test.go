// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	slackfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/slack"
	"github.com/Pierre-Theophile/aisre/internal/investigation/render"
	"github.com/Pierre-Theophile/aisre/internal/investigation/render/slacksink"
	"github.com/Pierre-Theophile/aisre/internal/slackx"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The report in the incident channel, across a reopen (006 T025), and the two Slack credentials
// that must never stand in for each other (T024).

const (
	deliveryTeam    = "T0TWIN"
	deliveryChannel = "C0SEV2CHK"
	reporterToken   = "xoxb-reporter-app"
	readerToken     = "xoxb-reader-app"
)

// slackWorkspace is one twin of the Web API that both apps talk to, as a workspace is: each token
// is the app it belongs to, with that app's scopes, and every request is kept.
type slackWorkspace struct {
	t        *testing.T
	mu       sync.Mutex
	requests []string // "<app> <METHOD> <path>"
	exchange []exchangeLine
	messages map[string]bool
	next     int
}

// exchangeLine is one write as the fixture records it: the operation, the channel, the message —
// and never the text.
type exchangeLine struct {
	Op      string `json:"op"`
	Channel string `json:"channel"`
	TS      string `json:"ts"`
}

var appScopes = map[string]string{
	reporterToken: "chat:write,chat:write.public",
	readerToken:   "channels:read",
}

func newSlackWorkspace(t *testing.T) (*slackWorkspace, *httptest.Server) {
	t.Helper()
	ws := &slackWorkspace{t: t, messages: map[string]bool{}}
	srv := httptest.NewServer(http.HandlerFunc(ws.serve))
	t.Cleanup(srv.Close)
	return ws, srv
}

func (ws *slackWorkspace) serve(w http.ResponseWriter, r *http.Request) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	app := map[string]string{reporterToken: "reporter", readerToken: "reader"}[token]
	if app == "" {
		app = "unknown"
	}
	op := r.Method + " " + r.URL.Path
	ws.requests = append(ws.requests, app+" "+op)
	scopes, known := appScopes[token]
	if !known {
		_, _ = io.WriteString(w, `{"ok":false,"error":"invalid_auth"}`)
		return
	}
	w.Header().Set("X-OAuth-Scopes", scopes)
	var body map[string]any
	if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
		_ = json.Unmarshal(raw, &body)
	}
	switch op {
	case "GET /api/auth.test":
		_, _ = io.WriteString(w, `{"ok":true,"team_id":"`+deliveryTeam+`","team":"twin"}`)
	case "GET /api/conversations.list":
		_, _ = io.WriteString(w, `{"ok":true,"channels":[]}`)
	case "POST /api/chat.postMessage":
		ws.next++
		ts := "1760434200.00000" + strconv.Itoa(ws.next)
		ws.messages[ts] = true
		ws.exchange = append(ws.exchange, exchangeLine{Op: "chat.postMessage", Channel: deliveryChannel, TS: ts})
		_, _ = io.WriteString(w, `{"ok":true,"channel":"`+deliveryChannel+`","ts":"`+ts+`"}`)
	case "POST /api/chat.update":
		ts, _ := body["ts"].(string)
		if !ws.messages[ts] {
			_, _ = io.WriteString(w, `{"ok":false,"error":"message_not_found"}`)
			return
		}
		ws.exchange = append(ws.exchange, exchangeLine{Op: "chat.update", Channel: deliveryChannel, TS: ts})
		_, _ = io.WriteString(w, `{"ok":true,"channel":"`+deliveryChannel+`","ts":"`+ts+`"}`)
	default:
		ws.t.Errorf("an unpublished request: %s", op)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (ws *slackWorkspace) seen() []string {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	return append([]string(nil), ws.requests...)
}

func slackInvestigation(id, reopens string) *investigationv1.Investigation {
	return &investigationv1.Investigation{
		InvestigationId:        id,
		ReopensInvestigationId: reopens,
		Symptoms: []*investigationv1.Symptom{{
			Origin:       investigationv1.IntakeOrigin_INTAKE_ORIGIN_DECLARED,
			OriginSystem: slackfeeder.SourceIDPrefix + deliveryTeam,
			OriginRef:    slackfeeder.OriginRef(deliveryTeam, deliveryChannel),
		}},
	}
}

// deliveryFixture is fixtures/slack-delivery-edit-01/delivery.json.
type deliveryFixture struct {
	Description string         `json:"description"`
	Exchange    []exchangeLine `json:"exchange"`
	Messages    int            `json:"messages_in_channel"`
}

// TestADeliveryAcrossAReopenEditsTheSameMessage is US2: an investigation concludes and its report is
// posted; it is reopened with new evidence, which is a new investigation record; that one concludes
// and the report in the channel is edited. One post, one update, one message — and the fixture holds
// the exchange, so a change to it is a reviewed diff.
func TestADeliveryAcrossAReopenEditsTheSameMessage(t *testing.T) {
	t.Setenv(EnvDeliveryToken, reporterToken)
	ws, srv := newSlackWorkspace(t)
	sink := slacksink.New(slacksink.Options{BaseURL: srv.URL})
	ctx := t.Context()

	parent := slackInvestigation("inv-parent", "")
	first := deliverReportWith(ctx, sink, parent, "*concluded*: no observed change explains this", "")
	if first.Outcome != render.OutcomeDelivered {
		t.Fatalf("first delivery %+v", first)
	}
	system, ref := deliverySplit(parent)
	parent.Deliveries = []*investigationv1.ReportDelivery{{
		TargetSystem: system, TargetRef: ref, ExternalMessageRef: first.ExternalMessageRef,
	}}

	child := slackInvestigation("inv-child", parent.GetInvestigationId())
	get := func(_ context.Context, id string) (*investigationv1.Investigation, error) {
		if id == parent.GetInvestigationId() {
			return parent, nil
		}
		return nil, errors.New("not found")
	}
	previous := previousMessageRef(ctx, get, child, system, ref)
	if previous != first.ExternalMessageRef {
		t.Fatalf("the reopened investigation edits %q, want its parent's message %q", previous, first.ExternalMessageRef)
	}
	second := deliverReportWith(ctx, sink, child, "*concluded*: payments@rev7, after the reopen", previous)
	if second.Outcome != render.OutcomeDelivered || second.ExternalMessageRef != first.ExternalMessageRef {
		t.Fatalf("second delivery %+v", second)
	}

	got := deliveryFixture{
		Description: "An investigation declared in a Slack channel concludes and its report is posted " +
			"there. It is reopened with new evidence, which opens a new investigation record linked to " +
			"its parent (FR-057b); that one concludes and the report in the channel is edited in place. " +
			"One chat.postMessage, one chat.update, one message in the channel (006 US2). Only the " +
			"operations and the message are recorded, never the report's text.",
		Exchange: ws.exchange,
		Messages: len(ws.messages),
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "fixtures", "slack-delivery-edit-01", "delivery.json")
	want, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, '\n')
	if os.Getenv("SRE_AGENT_GEN_FIXTURES") != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	recorded, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (regenerate with SRE_AGENT_GEN_FIXTURES=1)", err)
	}
	if string(recorded) != string(want) {
		t.Errorf("the exchange differs from %s:\n%s", path, want)
	}
	if got.Messages != 1 || len(got.Exchange) != 2 || got.Exchange[0].Op != "chat.postMessage" ||
		got.Exchange[1].Op != "chat.update" {
		t.Errorf("exchange %+v", got)
	}
}

// A chain of reopens finds the first message; a parent that cannot be read, or a loop, means a new
// message rather than a wrong edit.
func TestPreviousMessageRefWalksTheReopenChain(t *testing.T) {
	ctx := t.Context()
	root := slackInvestigation("a", "")
	system, ref := deliverySplit(root)
	root.Deliveries = []*investigationv1.ReportDelivery{
		{TargetSystem: "slack:OTHER", TargetRef: "slack:OTHER/C1", ExternalMessageRef: "C1/1.0"},
		{TargetSystem: system, TargetRef: ref, ExternalMessageRef: deliveryChannel + "/1.5"},
	}
	byID := map[string]*investigationv1.Investigation{"a": root, "b": slackInvestigation("b", "a")}
	get := func(_ context.Context, id string) (*investigationv1.Investigation, error) {
		if inv, ok := byID[id]; ok {
			return inv, nil
		}
		return nil, errors.New("not found")
	}
	if got := previousMessageRef(ctx, get, slackInvestigation("c", "b"), system, ref); got != deliveryChannel+"/1.5" {
		t.Errorf("chain: %q", got)
	}
	if got := previousMessageRef(ctx, get, slackInvestigation("d", "missing"), system, ref); got != "" {
		t.Errorf("unreadable parent: %q", got)
	}
	byID["x"], byID["y"] = slackInvestigation("x", "y"), slackInvestigation("y", "x")
	if got := previousMessageRef(ctx, get, byID["x"], system, ref); got != "" {
		t.Errorf("loop: %q", got)
	}
	if got := previousMessageRef(ctx, nil, slackInvestigation("e", "a"), system, ref); got != "" {
		t.Errorf("no getter: %q", got)
	}
}

// TestTheTwoSlackCredentialsNeverStandInForEachOther is US2 scenario 3. The reporter's token reads
// nothing and the reader's delivers nothing, and every refusal comes before the request it refuses:
// the only call either makes with the other's token is auth.test, which is how it learns whose token
// it holds. An app-level token reaches neither.
func TestTheTwoSlackCredentialsNeverStandInForEachOther(t *testing.T) {
	ctx := t.Context()

	t.Run("the reader issues no write", func(t *testing.T) {
		ws, srv := newSlackWorkspace(t)
		reader, err := slackx.New(slackx.Options{Token: readerToken, BaseURL: srv.URL})
		if err != nil {
			t.Fatal(err)
		}
		for _, op := range []string{"POST /api/chat.postMessage", "POST /api/chat.update", "GET /api/conversations.join"} {
			if _, err := reader.Do(ctx, feeder.ReadOperation(op), nil); err == nil {
				t.Errorf("the reader issued %s", op)
			}
		}
		if got := ws.seen(); len(got) != 0 {
			t.Errorf("requests left the process: %v", got)
		}
	})

	t.Run("the reporter's token does not read", func(t *testing.T) {
		ws, srv := newSlackWorkspace(t)
		misplaced, err := slackx.New(slackx.Options{Token: reporterToken, BaseURL: srv.URL})
		if err != nil {
			t.Fatal(err)
		}
		id, err := misplaced.AuthTest(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := slackx.CheckReadOnly(id); err == nil {
			t.Fatal("the reporter's token passed the reader's read-only gate")
		}
		if got := ws.seen(); !slices.Equal(got, []string{"reporter GET /api/auth.test"}) {
			t.Errorf("requests: %v; the gate must stop before any channel is read", got)
		}
	})

	t.Run("the reader's token does not deliver", func(t *testing.T) {
		ws, srv := newSlackWorkspace(t)
		t.Setenv(EnvDeliveryToken, readerToken)
		sink := slacksink.New(slacksink.Options{BaseURL: srv.URL})
		result := deliverReportWith(ctx, sink, slackInvestigation("inv-01", ""), "report", "")
		if result.Outcome == render.OutcomeDelivered {
			t.Fatalf("the reader's token delivered: %+v", result)
		}
		if got := ws.seen(); !slices.Equal(got, []string{"reader GET /api/auth.test"}) {
			t.Errorf("requests: %v; the sink must stop before any write", got)
		}
	})

	t.Run("a doorbell token reaches neither", func(t *testing.T) {
		ws, srv := newSlackWorkspace(t)
		if _, err := slackx.New(slackx.Options{Token: "xapp-1-doorbell", BaseURL: srv.URL}); err == nil {
			t.Error("the reader accepted an app-level token")
		}
		t.Setenv(EnvDeliveryToken, "xapp-1-doorbell")
		result := deliverReportWith(ctx, slacksink.New(slacksink.Options{BaseURL: srv.URL}),
			slackInvestigation("inv-01", ""), "report", "")
		if result.Outcome == render.OutcomeDelivered {
			t.Errorf("an app-level token delivered: %+v", result)
		}
		if got := ws.seen(); len(got) != 0 {
			t.Errorf("requests left the process: %v", got)
		}
	})
}
