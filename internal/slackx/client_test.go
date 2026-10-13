// SPDX-License-Identifier: Apache-2.0

package slackx_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/slackx"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// TestSurfaceIsTheFourPublishedReads is FR-002/FR-003 in its cheapest form: the reader can issue
// exactly the operations contracts/slack-connector.md §2 lists for it, none of which changes state.
func TestSurfaceIsTheFourPublishedReads(t *testing.T) {
	want := []feeder.ReadOperation{
		slackx.OpAuthTest, slackx.OpConversationsHistory, slackx.OpConversationsInfo, slackx.OpConversationsList,
	}
	if got := slackx.Surface.Operations(); !slices.Equal(got, want) {
		t.Errorf("surface = %v, want %v", got, want)
	}
	if n := slackx.Surface.StateChanges(); n != 0 {
		t.Errorf("surface carries %d state-changing operation(s)", n)
	}
	for _, write := range []feeder.ReadOperation{
		"POST /api/chat.postMessage", "POST /api/chat.update", "GET /api/conversations.join",
		"POST /api/conversations.invite", "POST /api/conversations.archive",
	} {
		if _, err := slackx.Surface.Issuable(write); err == nil {
			t.Errorf("%s is issuable by the reader", write)
		}
	}
}

func TestTokenKindsTheReaderRefuses(t *testing.T) {
	for _, token := range []string{"", "  ", "xapp-1-A0-123", "xoxp-123-456"} {
		if _, err := slackx.New(slackx.Options{Token: token}); err == nil {
			t.Errorf("New(%q) accepted a token that is not a bot token", token)
		}
	}
}

func TestReadOnlyGateRulesOnTheReportedScopes(t *testing.T) {
	ok := slackx.Identity{TeamID: "T1", Scopes: []string{"channels:history", "channels:read"}}
	if evidence, err := slackx.CheckReadOnly(ok); err != nil || evidence != feeder.EvidencePlatformReported {
		t.Errorf("read scopes: %v, %v", evidence, err)
	}
	for _, scope := range []string{"chat:write", "channels:join", "channels:manage", "chat:write.public", "reactions:write"} {
		id := slackx.Identity{TeamID: "T1", Scopes: []string{"channels:read", scope}}
		_, err := slackx.CheckReadOnly(id)
		var refused *feeder.WriteCapableError
		if !errors.As(err, &refused) || !strings.Contains(err.Error(), scope) {
			t.Errorf("scope %s: %v, want a refusal naming it", scope, err)
		}
	}
	if _, err := slackx.CheckReadOnly(slackx.Identity{TeamID: "T1"}); err == nil {
		t.Error("a token reporting no scope was accepted")
	}
}

func TestHistoryIsRefusedUnlessEnabled(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"ok":true,"messages":[]}`))
	}))
	defer srv.Close()
	c, err := slackx.New(slackx.Options{Token: "xoxb-1", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.History(context.Background(), "C1", time.Time{}, time.Time{}, ""); !errors.Is(err, slackx.ErrHistoryDisabled) {
		t.Errorf("History with the reader off = %v, want ErrHistoryDisabled", err)
	}
	if calls != 0 {
		t.Errorf("%d request(s) left the process for a refused read", calls)
	}
}

func TestAPIErrorsAndRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer xoxb-1" {
			t.Errorf("authorization header %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/api/conversations.info":
			_, _ = w.Write([]byte(`{"ok":false,"error":"channel_not_found"}`))
		default:
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"ok":false,"error":"ratelimited"}`))
		}
	}))
	defer srv.Close()
	c, _ := slackx.New(slackx.Options{Token: "xoxb-1", BaseURL: srv.URL})
	_, err := c.ChannelInfo(context.Background(), "C9")
	var apiErr *slackx.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "channel_not_found" {
		t.Errorf("info = %v", err)
	}
	_, err = c.ListChannels(context.Background(), "", 0)
	if !errors.As(err, &apiErr) || !apiErr.RateLimited() || apiErr.RetryAfter != 7*time.Second {
		t.Errorf("list = %v", err)
	}
}

func TestListAndAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-OAuth-Scopes", "channels:read, channels:history")
		switch r.URL.Path {
		case "/api/auth.test":
			_, _ = w.Write([]byte(`{"ok":true,"team_id":"T1","team":"acme","bot_id":"B1"}`))
		case "/api/conversations.list":
			if r.URL.Query().Get("exclude_archived") != "false" || r.URL.Query().Get("types") != "public_channel" {
				t.Errorf("list query %v", r.URL.Query())
			}
			_, _ = w.Write([]byte(`{"ok":true,"channels":[{"id":"C1","name":"sev2-2026-10-14-checkout","created":1791976323,"creator":"U1"}],"response_metadata":{"next_cursor":"abc"}}`))
		}
	}))
	defer srv.Close()
	c, _ := slackx.New(slackx.Options{Token: "xoxb-1", BaseURL: srv.URL})
	id, err := c.AuthTest(context.Background())
	if err != nil || id.TeamID != "T1" || !slices.Equal(id.Scopes, []string{"channels:history", "channels:read"}) {
		t.Fatalf("auth = %+v, %v", id, err)
	}
	page, err := c.ListChannels(context.Background(), "", 0)
	if err != nil || len(page.Channels) != 1 || page.NextCursor != "abc" {
		t.Fatalf("page = %+v, %v", page, err)
	}
	if got := page.Channels[0].CreatedAt(); !got.Equal(time.Unix(1791976323, 0)) {
		t.Errorf("created = %s", got)
	}
}

func TestMessageInstant(t *testing.T) {
	m := slackx.Message{TS: "1791976323.000250"}
	if got, want := m.At(), time.Unix(1791976323, 250_000).UTC(); !got.Equal(want) {
		t.Errorf("At = %s, want %s", got, want)
	}
	if got := slackx.SlackTS(time.Unix(1791976323, 250_000)); got != "1791976323.000250" {
		t.Errorf("SlackTS = %s", got)
	}
}
