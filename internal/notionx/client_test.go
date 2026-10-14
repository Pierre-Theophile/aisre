// SPDX-License-Identifier: Apache-2.0

package notionx_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/notionx"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/testkit"
)

func secret(t *testing.T) notionx.Secret {
	t.Helper()
	s, err := notionx.NewSecret("ntn_test")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestSurfaceIsTheFivePublishedReads is FR-001 in its cheapest form.
func TestSurfaceIsTheFivePublishedReads(t *testing.T) {
	want := []feeder.ReadOperation{
		notionx.OpDatabase, notionx.OpPage, notionx.OpPageProperty, notionx.OpUsersMe, notionx.OpQueryDataSource,
	}
	got := notionx.Surface.Operations()
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("surface = %v, want %v", got, want)
	}
	if n := notionx.Surface.StateChanges(); n != 0 {
		t.Errorf("surface carries %d state-changing operation(s)", n)
	}
	for _, write := range []feeder.ReadOperation{
		"POST /v1/pages", "PATCH /v1/pages/{id}", "PATCH /v1/blocks/{id}/children", "POST /v1/comments",
		"DELETE /v1/blocks/{id}", "POST /v1/search", "GET /v1/blocks/{id}/children",
	} {
		if _, err := notionx.Surface.Issuable(write); err == nil {
			t.Errorf("%s is issuable", write)
		}
	}
}

func TestSurfaceMatchesThePublishedPage(t *testing.T) {
	testkit.AssertSurfaceMatchesPage(t, notionx.Surface, "../../docs/connectors/notion.md")
}

func TestSecretsTheClientRefusesAndNeverPrints(t *testing.T) {
	for _, raw := range []string{"", "  ", "xoxb-1", "Bearer ntn_x"} {
		if _, err := notionx.NewSecret(raw); err == nil {
			t.Errorf("NewSecret(%q) accepted", raw)
		}
	}
	s := secret(t)
	for _, rendered := range []string{fmt.Sprint(s), fmt.Sprintf("%v %+v %#v", s, s, s)} {
		if strings.Contains(rendered, "ntn_test") {
			t.Errorf("the secret printed: %s", rendered)
		}
	}
}

func TestTheReadOnlyGateNeedsTheAcknowledgement(t *testing.T) {
	if _, err := notionx.CheckReadOnly("acme", nil); !errors.Is(err, notionx.ErrNotAcknowledged) {
		t.Errorf("no acknowledgement: %v", err)
	}
	evidence, err := notionx.CheckReadOnly("acme", []string{"notion.capabilities"})
	if err != nil || evidence != feeder.EvidenceOperatorAsserted {
		t.Errorf("acknowledged: %v, %v", evidence, err)
	}
}

// twin is a recorded double of the five operations.
func twin(t *testing.T, handle func(w http.ResponseWriter, r *http.Request, body map[string]any)) *notionx.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ntn_test" || r.Header.Get("Notion-Version") != notionx.APIVersion {
			t.Errorf("headers %v", r.Header)
		}
		var body map[string]any
		if r.Method == http.MethodPost {
			raw, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("body %s: %v", raw, err)
			}
		}
		handle(w, r, body)
	}))
	t.Cleanup(srv.Close)
	c, err := notionx.New(notionx.Options{Secret: secret(t), BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestTheOperationsReadWhatTheConnectorNeeds(t *testing.T) {
	c := twin(t, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		switch r.URL.Path {
		case "/v1/users/me":
			_, _ = w.Write([]byte(`{"object":"user","id":"u1","type":"bot","bot":{"workspace_name":"acme"}}`))
		case "/v1/databases/db1":
			_, _ = w.Write([]byte(`{"object":"database","id":"db1","title":[{"type":"text","plain_text":"Postmortems"}],` +
				`"data_sources":[{"id":"ds1","name":"Postmortems"}]}`))
		case "/v1/data_sources/ds1/query":
			if r.Method != http.MethodPost || body["filter"] == nil || body["start_cursor"] != "c1" {
				t.Errorf("query %s %v", r.Method, body)
			}
			_, _ = w.Write([]byte(`{"object":"list","results":[{"object":"page","id":"p1","url":"https://www.notion.so/p1",` +
				`"created_time":"2026-05-14T11:00:00.000Z","last_edited_time":"2026-05-15T09:00:00.000Z",` +
				`"created_by":{"object":"user","id":"SHOULD-NOT-BE-DECODED"},` +
				`"parent":{"type":"data_source_id","data_source_id":"ds1","database_id":"db1"},"properties":{` +
				`"Name":{"id":"title","type":"title","title":[{"type":"text","plain_text":"Checkout outage"}]},` +
				`"Summary":{"id":"s1","type":"rich_text","rich_text":[{"type":"text","plain_text":"Pool exhausted, "},` +
				`{"type":"mention","plain_text":"@Dana","mention":{"type":"user"}},{"type":"text","plain_text":" rolled back"}]},` +
				`"Services":{"id":"sv","type":"multi_select","multi_select":[{"name":"Checkout"},{"name":"Payments"}]},` +
				`"Owner":{"id":"ow","type":"people","people":[{"object":"user","id":"SHOULD-NOT-BE-DECODED"}]}}}],` +
				`"has_more":false,"next_cursor":null}`))
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	ctx := context.Background()
	me, err := c.UsersMe(ctx)
	if err != nil || me.Workspace != "acme" {
		t.Fatalf("me = %+v, %v", me, err)
	}
	db, err := c.Database(ctx, "db1")
	if err != nil || db.Title != "Postmortems" || len(db.DataSources) != 1 || db.DataSources[0].ID != "ds1" {
		t.Fatalf("db = %+v, %v", db, err)
	}
	q, err := c.QueryDataSource(ctx, "ds1", notionx.QueryFilter{EditedOnOrAfter: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)}, "c1")
	if err != nil || len(q.Pages) != 1 {
		t.Fatalf("query = %+v, %v", q, err)
	}
	p := q.Pages[0]
	if p.Properties["Name"].Text() != "Checkout outage" {
		t.Errorf("title %q", p.Properties["Name"].Text())
	}
	if got := p.Properties["Summary"].Text(); got != "Pool exhausted,  rolled back" {
		t.Errorf("summary %q: a user mention must be removed", got)
	}
	if got := p.Properties["Services"].Values(); !slices.Equal(got, []string{"Checkout", "Payments"}) {
		t.Errorf("values %v", got)
	}
	if got := p.Properties["Owner"].Values(); len(got) != 0 || p.Properties["Owner"].Text() != "" {
		t.Errorf("a people property was read: %v", got)
	}
	raw, _ := json.Marshal(p)
	if strings.Contains(string(raw), "SHOULD-NOT-BE-DECODED") {
		t.Errorf("a person reached the decoded page: %s", raw)
	}
}

func TestAPIErrors(t *testing.T) {
	c := twin(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"object":"error","status":404,"code":"object_not_found"}`))
	})
	_, err := c.Page(context.Background(), "p9")
	var apiErr *notionx.APIError
	if !errors.As(err, &apiErr) || !apiErr.NotFound() {
		t.Errorf("page = %v", err)
	}
	if _, err := c.Page(context.Background(), "../users"); err == nil {
		t.Error("an id that changes the path was issued")
	}
}

func TestPropertyItemsPaginate(t *testing.T) {
	calls := 0
	c := twin(t, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		calls++
		if r.URL.Path != "/v1/pages/p1/properties/sv" {
			t.Errorf("path %s", r.URL.Path)
		}
		if r.URL.Query().Get("start_cursor") == "" {
			_, _ = w.Write([]byte(`{"object":"list","results":[{"type":"relation","relation":{"id":"r1"}}],"has_more":true,"next_cursor":"n2"}`))
			return
		}
		_, _ = w.Write([]byte(`{"object":"list","results":[{"type":"relation","relation":{"id":"r2"}}],"has_more":false}`))
	})
	prop, err := c.PropertyItems(context.Background(), "p1", "sv", nil)
	if err != nil || !slices.Equal(prop.Values(), []string{"r1", "r2"}) || calls != 2 {
		t.Errorf("items = %v, %v after %d call(s)", prop.Values(), err, calls)
	}
}
