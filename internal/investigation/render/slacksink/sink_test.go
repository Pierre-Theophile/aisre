// SPDX-License-Identifier: Apache-2.0

package slacksink_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/investigation/render"
	"github.com/Pierre-Theophile/aisre/internal/investigation/render/slacksink"
)

const (
	team    = "T0TWIN"
	channel = "C0SEV2CHK"
	token   = "xoxb-test-reporter-token"
)

var at = time.Date(2026, 10, 14, 9, 30, 0, 0, time.UTC)

// fakeSlack is a recorded twin of the three operations the sink may use. It keeps every request so a
// test can assert what left the process, and the messages it holds so it can assert "edited in
// place".
type fakeSlack struct {
	t *testing.T

	mu       sync.Mutex
	scopes   string
	team     string
	calls    []string
	bodies   []map[string]any
	messages map[string]string // ts → text of the first section
	next     int
	failNext string // error code for the next write
}

func newFake(t *testing.T) (*fakeSlack, *httptest.Server) {
	f := &fakeSlack{t: t, scopes: "chat:write,chat:write.public", team: team, messages: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeSlack) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	op := r.Method + " " + r.URL.Path
	f.calls = append(f.calls, op)
	if r.Header.Get("Authorization") != "Bearer "+token {
		_, _ = io.WriteString(w, `{"ok":false,"error":"invalid_auth"}`)
		return
	}
	w.Header().Set("X-OAuth-Scopes", f.scopes)
	var body map[string]any
	if r.Body != nil {
		raw, _ := io.ReadAll(r.Body)
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &body); err != nil {
				f.t.Errorf("%s: body is not JSON: %v", op, err)
			}
		}
	}
	f.bodies = append(f.bodies, body)
	if f.failNext != "" && r.Method == http.MethodPost {
		code := f.failNext
		f.failNext = ""
		if code == "ratelimited" {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
		}
		_, _ = io.WriteString(w, `{"ok":false,"error":"`+code+`"}`)
		return
	}
	switch op {
	case "GET /api/auth.test":
		_, _ = io.WriteString(w, `{"ok":true,"team_id":"`+f.team+`","team":"twin","bot_id":"B0REPORT"}`)
	case "POST /api/chat.postMessage":
		f.next++
		ts := "1760434200.00000" + strconv.Itoa(f.next)
		f.messages[ts] = firstSection(body)
		_, _ = io.WriteString(w, `{"ok":true,"channel":"`+channel+`","ts":"`+ts+`"}`)
	case "POST /api/chat.update":
		ts, _ := body["ts"].(string)
		if _, ok := f.messages[ts]; !ok {
			_, _ = io.WriteString(w, `{"ok":false,"error":"message_not_found"}`)
			return
		}
		f.messages[ts] = firstSection(body)
		_, _ = io.WriteString(w, `{"ok":true,"channel":"`+channel+`","ts":"`+ts+`"}`)
	default:
		f.t.Errorf("the sink issued an unpublished request: %s", op)
		w.WriteHeader(http.StatusNotFound)
	}
}

func firstSection(body map[string]any) string {
	blocks, _ := body["blocks"].([]any)
	if len(blocks) == 0 {
		return ""
	}
	block, _ := blocks[0].(map[string]any)
	text, _ := block["text"].(map[string]any)
	s, _ := text["text"].(string)
	return s
}

func (f *fakeSlack) writes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, "POST ") {
			out = append(out, c)
		}
	}
	return out
}

func credFor(t *testing.T, system, secret string) *render.Credential {
	t.Helper()
	cred, err := render.NewCredential(system, render.DeliveryScope, secret)
	if err != nil {
		t.Fatal(err)
	}
	return cred
}

func request(rendering, previous string) render.DeliveryRequest {
	return render.DeliveryRequest{
		InvestigationID: "inv-01", TargetSystem: "slack:" + team, TargetRef: "slack:" + team + "/" + channel,
		Rendering: rendering, PreviousMessageRef: previous, At: at,
	}
}

func TestOperationsAreTheWholeSurface(t *testing.T) {
	want := []string{"GET /api/auth.test", "POST /api/chat.postMessage", "POST /api/chat.update"}
	if got := slacksink.Operations(); !slices.Equal(got, want) {
		t.Fatalf("operations = %v, want %v; a new one needs a review and a line in docs/connectors/slack.md", got, want)
	}
	if !slices.Equal(slacksink.AllowedScopes, []string{"chat:write", "chat:write.public"}) {
		t.Fatalf("allowed scopes = %v", slacksink.AllowedScopes)
	}
}

// TestDeliveryIsEditedInPlace is FR-020/FR-021 and ADR-0007 confinement 2: the first delivery posts,
// every later one edits that message, and the preflight runs once.
func TestDeliveryIsEditedInPlace(t *testing.T) {
	fake, srv := newFake(t)
	sink := slacksink.New(slacksink.Options{BaseURL: srv.URL})
	cred := credFor(t, "slack:"+team, token)
	ctx := context.Background()

	first := render.Deliver(ctx, sink, cred, request("*running*: checkout latency", ""))
	if first.Outcome != render.OutcomeDelivered || !strings.HasPrefix(first.ExternalMessageRef, channel+"/") {
		t.Fatalf("first delivery %+v", first)
	}
	second := render.Deliver(ctx, sink, cred, request("*concluded*: payments@rev7", first.ExternalMessageRef))
	if second.Outcome != render.OutcomeDelivered || second.ExternalMessageRef != first.ExternalMessageRef {
		t.Fatalf("second delivery %+v, want the same message %s", second, first.ExternalMessageRef)
	}
	if got := fake.writes(); !slices.Equal(got, []string{"POST /api/chat.postMessage", "POST /api/chat.update"}) {
		t.Errorf("writes = %v", got)
	}
	if len(fake.messages) != 1 {
		t.Errorf("%d messages in the channel, want 1", len(fake.messages))
	}
	for _, text := range fake.messages {
		if !strings.Contains(text, "concluded") {
			t.Errorf("the channel shows %q, want the latest rendering", text)
		}
	}
	if n := strings.Count(strings.Join(fake.calls, ","), "auth.test"); n != 1 {
		t.Errorf("auth.test issued %d times, want once", n)
	}
	for _, body := range fake.bodies {
		if body == nil {
			continue
		}
		if body["unfurl_links"] != false || body["parse"] != "none" || body["channel"] != channel {
			t.Errorf("body %v", body)
		}
	}
}

// TestADeletedMessageIsPostedAgain: the one message per target survives someone deleting it.
func TestADeletedMessageIsPostedAgain(t *testing.T) {
	fake, srv := newFake(t)
	sink := slacksink.New(slacksink.Options{BaseURL: srv.URL})
	got := sink.Deliver(context.Background(), credFor(t, "slack:"+team, token),
		request("report", channel+"/1700000000.000001"))
	if got.Outcome != render.OutcomeDelivered || got.ExternalMessageRef == channel+"/1700000000.000001" {
		t.Fatalf("delivery %+v", got)
	}
	if w := fake.writes(); !slices.Equal(w, []string{"POST /api/chat.update", "POST /api/chat.postMessage"}) {
		t.Errorf("writes = %v", w)
	}
}

// TestAReporterTokenThatCanDoMoreIsRefused is constitution VII: a token holding anything beyond
// chat:write fails closed before a byte of the report leaves.
func TestAReporterTokenThatCanDoMoreIsRefused(t *testing.T) {
	for _, scopes := range []string{"chat:write,channels:history", "chat:write,channels:join", "admin", ""} {
		fake, srv := newFake(t)
		fake.scopes = scopes
		got := slacksink.New(slacksink.Options{BaseURL: srv.URL}).
			Deliver(context.Background(), credFor(t, "slack:"+team, token), request("report", ""))
		if got.Outcome != render.OutcomeFailed {
			t.Errorf("scopes %q: outcome %s", scopes, got.Outcome)
		}
		if w := fake.writes(); len(w) != 0 {
			t.Errorf("scopes %q: the sink wrote %v", scopes, w)
		}
	}
}

// TestACredentialWritesOnlyToItsWorkspace: the token's workspace, the credential's system and the
// target must be one place.
func TestACredentialWritesOnlyToItsWorkspace(t *testing.T) {
	fake, srv := newFake(t)
	fake.team = "T0OTHER"
	sink := slacksink.New(slacksink.Options{BaseURL: srv.URL})
	if got := sink.Deliver(context.Background(), credFor(t, "slack:"+team, token), request("r", "")); got.Outcome != render.OutcomeFailed ||
		!strings.Contains(got.FailureDetail, "T0OTHER") {
		t.Errorf("a token from another workspace: %+v", got)
	}
	// A credential minted for another workspace.
	if got := render.Deliver(context.Background(), sink, credFor(t, "slack:T0OTHER", token), request("r", "")); got.Outcome == render.OutcomeDelivered {
		t.Errorf("a credential for another workspace delivered: %+v", got)
	}
	// A target that is not a channel of the credential's workspace.
	bad := request("r", "")
	bad.TargetRef = "slack:T0OTHER/" + channel
	if got := sink.Deliver(context.Background(), credFor(t, "slack:"+team, token), bad); got.Outcome != render.OutcomeNotConfigured {
		t.Errorf("a target in another workspace: %+v", got)
	}
	if w := fake.writes(); len(w) != 0 {
		t.Errorf("writes = %v", w)
	}
}

func TestOnlyABotTokenIsAccepted(t *testing.T) {
	fake, srv := newFake(t)
	sink := slacksink.New(slacksink.Options{BaseURL: srv.URL})
	for _, secret := range []string{"xapp-1-doorbell", "xoxp-a-person", "plain"} {
		if got := sink.Deliver(context.Background(), credFor(t, "slack:"+team, secret), request("r", "")); got.Outcome != render.OutcomeNotConfigured {
			t.Errorf("%s: %+v", secret, got)
		}
	}
	if len(fake.calls) != 0 {
		t.Errorf("calls = %v", fake.calls)
	}
}

func TestSlackFailuresAreResults(t *testing.T) {
	fake, srv := newFake(t)
	sink := slacksink.New(slacksink.Options{BaseURL: srv.URL})
	fake.failNext = "ratelimited"
	got := sink.Deliver(context.Background(), credFor(t, "slack:"+team, token), request("r", ""))
	if got.Outcome != render.OutcomeFailed || !strings.Contains(got.FailureDetail, "ratelimited") ||
		!strings.Contains(got.FailureDetail, "7s") {
		t.Errorf("rate limited: %+v", got)
	}
	fake.failNext = "channel_not_found"
	got = sink.Deliver(context.Background(), credFor(t, "slack:"+team, token), request("r", ""))
	if got.Outcome != render.OutcomeFailed || !strings.Contains(got.FailureDetail, "channel_not_found") {
		t.Errorf("no channel: %+v", got)
	}
}

// TestAReportPagesNobody is FR-023: nothing in a rendering becomes a mention.
func TestAReportPagesNobody(t *testing.T) {
	in := "ping <!here> and <!channel|channel> @Channel @everyone <!subteam^S0ONCALL> <@U0BOB> & done"
	out := slacksink.Neutralise(in)
	for _, bad := range []string{"<!", "<@", "@channel", "@Channel", "@everyone", "@here"} {
		if strings.Contains(out, bad) {
			t.Errorf("%q survives in %q", bad, out)
		}
	}
	if !strings.Contains(out, "&amp; done") {
		t.Errorf("ampersand not escaped: %q", out)
	}
}

func TestALongReportIsCutWithANote(t *testing.T) {
	fake, srv := newFake(t)
	line := strings.Repeat("é", 99) + "\n"
	long := strings.Repeat(line, 3000) // 300 000 runes
	got := slacksink.New(slacksink.Options{BaseURL: srv.URL}).
		Deliver(context.Background(), credFor(t, "slack:"+team, token), request(long, ""))
	if got.Outcome != render.OutcomeDelivered {
		t.Fatalf("%+v", got)
	}
	body := fake.bodies[len(fake.bodies)-1]
	blocks, _ := body["blocks"].([]any)
	if len(blocks) > 50 {
		t.Fatalf("%d blocks; Slack allows 50", len(blocks))
	}
	last, _ := blocks[len(blocks)-1].(map[string]any)
	if last["type"] != "context" {
		t.Errorf("no truncation note: last block %v", last)
	}
	for _, b := range blocks[:len(blocks)-1] {
		section, _ := b.(map[string]any)
		text, _ := section["text"].(map[string]any)
		if s, _ := text["text"].(string); len([]rune(s)) > 3000 {
			t.Errorf("a section of %d runes; Slack allows 3000", len([]rune(s)))
		}
	}
	if text, _ := body["text"].(string); len([]rune(text)) > 3000 {
		t.Errorf("fallback text of %d runes", len([]rune(text)))
	}
}

func TestSplitTarget(t *testing.T) {
	gotTeam, gotChannel, err := slacksink.SplitTarget("slack:T1", "slack:T1/C9")
	if err != nil || gotTeam != "T1" || gotChannel != "C9" {
		t.Errorf("SplitTarget = %s %s %v", gotTeam, gotChannel, err)
	}
	for _, c := range [][2]string{{"slack:T1", "slack:T2/C9"}, {"pagerduty:x", "pagerduty:x/C9"}, {"slack:T1", "slack:T1/"}, {"slack:T1", "slack:T1/C9/x"}} {
		if _, _, err := slacksink.SplitTarget(c[0], c[1]); err == nil {
			t.Errorf("SplitTarget(%s, %s) accepted", c[0], c[1])
		}
	}
}
