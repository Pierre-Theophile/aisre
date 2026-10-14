// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	slackfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/slack"
	"github.com/Pierre-Theophile/aisre/internal/investigation/intake"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// `feed slack` (006 US1, T013–T015).

// slackTwin answers auth.test with the given scopes and lists four channels created minutes ago; it
// refuses everything else, so a test that reaches any other operation — a message read, a write —
// fails loudly.
func slackTwin(t *testing.T, scopes string) *httptest.Server {
	t.Helper()
	created := time.Now().Add(-10 * time.Minute).Unix()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-OAuth-Scopes", scopes)
		switch r.Method + " " + r.URL.Path {
		case "GET /api/auth.test":
			_, _ = io.WriteString(w, `{"ok":true,"team_id":"T0TWIN","team":"twin","bot_id":"B0READ"}`)
		case "GET /api/conversations.list":
			_, _ = fmt.Fprintf(w, `{"ok":true,"channels":[`+
				`{"id":"C1","name":"sev2-2026-10-14-checkout-latency","created":%[1]d},`+
				`{"id":"C2","name":"sev2-2026-10-14-test-paging","created":%[1]d},`+
				`{"id":"C3","name":"random","created":%[1]d},`+
				`{"id":"C4","name":"sev1-2026-10-14-vendor-bridge","created":%[1]d,"is_ext_shared":true}]}`, created)
		default:
			t.Errorf("feed slack --check issued %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestFeedSlackCheckShowsWhatTheRuleMatches is US5: each credential, the rule and its version, and
// the channels matched and skipped, each skipped one with the clause that excluded it — from one read
// of the list, and nothing emitted.
func TestFeedSlackCheckShowsWhatTheRuleMatches(t *testing.T) {
	t.Setenv(EnvSlackReaderToken, "xoxb-reader")
	t.Setenv(EnvDeliveryToken, "")
	t.Setenv(EnvSlackAppToken, "")
	srv := slackTwin(t, "channels:read")
	stdout, stderr, code := run(t, t.Context(), "feed", "slack", "--check", "--slack-api", srv.URL,
		"--service", "checkout=service.name=checkout")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	for _, want := range []string{"workspace T0TWIN", "read-only", "reporter token: not set", "doorbell: off",
		"source id: slack:T0TWIN", "rule: slack/sev-channel@1", "services mapped: 1",
		"1 matched", "matched: sev2-2026-10-14-checkout-latency",
		"skipped: sev2-2026-10-14-test-paging — excluded marker test",
		"skipped: sev1-2026-10-14-vendor-bridge — shared with another organisation",
		"skipped: 1 channel(s) — no rule match (not named)", "no message was read and no event was emitted"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("--check does not say %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "random") {
		t.Errorf("--check named a channel the rule does not match:\n%s", stdout)
	}
}

// --check refuses a reporter token that can do more than post the report, and a doorbell token that
// is not an app-level token, after saying everything else.
func TestFeedSlackCheckRefusesAWrongCredential(t *testing.T) {
	t.Setenv(EnvSlackReaderToken, "xoxb-reader")
	t.Setenv(EnvDeliveryToken, "")
	t.Setenv(EnvSlackAppToken, "xoxb-not-an-app-token")
	srv := slackTwin(t, "channels:read")
	stdout, stderr, code := run(t, t.Context(), "feed", "slack", "--check", "--slack-api", srv.URL)
	if code != ExitUsage || !strings.Contains(stdout, "doorbell: REFUSED") || !strings.Contains(stderr, "doorbell token") {
		t.Errorf("exit %d\n%s\n%s", code, stdout, stderr)
	}
}

// The same check over a recording: fixtures/slack-intake-01 (T036).
func TestFeedSlackCheckOverARecording(t *testing.T) {
	stdout, stderr, code := run(t, t.Context(), "feed", "slack", "--check", "--team", "T0TWIN",
		"--replay", "../../fixtures/slack-intake-01", "--horizon", "876000h")
	if code != ExitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	for _, want := range []string{"matched: sev2-2026-10-14-checkout-latency",
		"skipped: sev2-2026-10-14-test-paging — excluded marker test",
		"skipped: sev1-2026-10-14-vendor-bridge — shared with another organisation",
		"no rule match (not named)"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("--check --replay does not say %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "reader token") {
		t.Errorf("a replay checked a credential:\n%s", stdout)
	}
}

func TestFeedSlackRefusesAnUnpublishedRule(t *testing.T) {
	_, stderr, code := run(t, t.Context(), "feed", "slack", "--check", "--team", "T0TWIN",
		"--replay", "../../fixtures/slack-intake-01", "--rule", "slack/sev-channel@9")
	if code != ExitUsage || !strings.Contains(stderr, "not a published naming rule") {
		t.Errorf("exit %d: %s", code, stderr)
	}
}

// TestFeedSlackRefusesAReaderThatCanWrite is FR-002: the reader token holding a write scope stops
// the run before a channel is read.
func TestFeedSlackRefusesAReaderThatCanWrite(t *testing.T) {
	t.Setenv(EnvSlackReaderToken, "xoxb-reader")
	for _, scopes := range []string{"channels:read,chat:write", "channels:read,channels:join"} {
		srv := slackTwin(t, scopes)
		_, stderr, code := run(t, t.Context(), "feed", "slack", "--check", "--slack-api", srv.URL)
		if code != ExitUsage || !strings.Contains(stderr, "not read-only") {
			t.Errorf("%s: exit %d: %s", scopes, code, stderr)
		}
	}
}

func TestFeedSlackRefusesTheWrongKindOfToken(t *testing.T) {
	for _, token := range []string{"", "xoxp-a-person", "xapp-1-doorbell"} {
		t.Setenv(EnvSlackReaderToken, token)
		_, stderr, code := run(t, t.Context(), "feed", "slack", "--check", "--slack-api", "http://127.0.0.1:1")
		if code != ExitUsage || !strings.Contains(stderr, EnvSlackReaderToken) {
			t.Errorf("token %q: exit %d: %s", token, code, stderr)
		}
	}
}

// TestFeedSlackNeedsTheIdentityKeyLive is FR-052: a live run that could only record a creator as a raw
// Slack user id does not start.
func TestFeedSlackNeedsTheIdentityKeyLive(t *testing.T) {
	t.Setenv(EnvSlackReaderToken, "xoxb-reader")
	t.Setenv(EnvSlackIdentityKey, "")
	srv := slackTwin(t, "channels:read")
	_, stderr, code := run(t, t.Context(), "feed", "slack", "--slack-api", srv.URL, "--cycles", "1")
	if code != ExitUsage || !strings.Contains(stderr, EnvSlackIdentityKey) {
		t.Errorf("exit %d: %s", code, stderr)
	}
	t.Setenv(EnvSlackIdentityKey, "abcd")
	_, stderr, code = run(t, t.Context(), "feed", "slack", "--slack-api", srv.URL, "--cycles", "1")
	if code != ExitUsage || !strings.Contains(stderr, "32 hex-encoded bytes") {
		t.Errorf("short key: exit %d: %s", code, stderr)
	}
}

// TestSlackDeclareRequestIsTheFeedersDeclaration: the investigation and the event are one incident —
// same key, same instant, same place — and late discovery is review mode.
func TestSlackDeclareRequestIsTheFeedersDeclaration(t *testing.T) {
	at := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	d := slackfeeder.Declared{
		SourceID: "slack:T0TWIN", ChannelID: "C0SEV2", OriginRef: slackfeeder.OriginRef("T0TWIN", "C0SEV2"),
		DeclaredAt: at, ReviewMode: true, Severity: "sev2", Title: "sev2-2026-10-14-checkout",
		Declarer: "slack:T0TWIN/u_0123456789abcdef", RuleRef: "slack/sev-channel@1",
		Targets:        []*graphv1.Ref{feeder.Ref("service.name", "checkout")},
		IdempotencyKey: slackfeeder.DeclarationKey("slack:T0TWIN", "C0SEV2", at),
	}
	req := SlackDeclareRequest(d)
	s := req.GetDeclaration()
	if want := intake.DeclarationKey("slack:T0TWIN", "C0SEV2", at); s.GetIdempotencyKey() != want {
		t.Errorf("key %s, want the engine's %s", s.GetIdempotencyKey(), want)
	}
	if s.GetOriginSystem() != "slack:T0TWIN" || s.GetOriginRef() != "slack:T0TWIN/C0SEV2" ||
		!s.GetFiredAt().AsTime().Equal(at) || s.GetDeclaringIdentity() != d.Declarer {
		t.Errorf("declaration %v", s)
	}
	if req.GetProfile() != "review" {
		t.Errorf("profile %s, want review for a late discovery", req.GetProfile())
	}
	if len(s.GetTargetRefs()) != 1 || s.GetTargetRefs()[0].GetProvenance() != investigationv1.TargetRefProvenance_PARSED_FROM_DECLARATION ||
		s.GetTargetRefs()[0].GetRuleId() != "slack/sev-channel@1" {
		t.Errorf("targets %v", s.GetTargetRefs())
	}
	if system, ref := deliverySplit(&investigationv1.Investigation{Symptoms: []*investigationv1.Symptom{s}}); system != "slack:T0TWIN" || ref != "slack:T0TWIN/C0SEV2" {
		t.Errorf("the report would go to %s %s", system, ref)
	}
}

func TestParseSlackServices(t *testing.T) {
	got, err := parseSlackServices([]string{"Checkout=service.name=checkout", "pay=service.name=payments"})
	if err != nil || got["checkout"].GetValue() != "checkout" || got["pay"].GetValue() != "payments" {
		t.Errorf("%v %v", got, err)
	}
	for _, bad := range [][]string{{"checkout"}, {"=service.name=x"}, {"a=service.name=x", "A=service.name=y"}, {"a=novalue"}} {
		if _, err := parseSlackServices(bad); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

// --doorbell is for a live run, and needs the app-level token: a bot token is not one.
func TestFeedSlackDoorbellNeedsAnAppLevelToken(t *testing.T) {
	_, stderr, code := run(t, t.Context(), "feed", "slack", "--doorbell", "--team", "T0TWIN",
		"--replay", "../../fixtures/slack-intake-01")
	if code != ExitUsage || !strings.Contains(stderr, "live run") {
		t.Errorf("replay: exit %d: %s", code, stderr)
	}
	t.Setenv(EnvSlackReaderToken, "xoxb-reader")
	t.Setenv(EnvSlackIdentityKey, strings.Repeat("ab", 32))
	t.Setenv(EnvSlackAppToken, "xoxb-reader")
	srv := slackTwin(t, "channels:read")
	_, stderr, code = run(t, t.Context(), "feed", "slack", "--doorbell", "--slack-api", srv.URL, "--cycles", "1",
		"--investigate=false")
	if code != ExitUsage || !strings.Contains(stderr, EnvSlackAppToken) {
		t.Errorf("bot token as the doorbell's: exit %d: %s", code, stderr)
	}
}
