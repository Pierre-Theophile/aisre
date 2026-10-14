// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `feed notion` (007 T009, T025): the gates that stop a run before it reads anything.

const notionConfig = `
workspace: twin
principal: oidc:operator-twin
%s
databases:
  - database: 0a1b2c3d4e5f60718293a4b5c6d7e8f9
    kind: postmortem
    entity_property: Services
    entities:
      Checkout: otel.service.name=checkout
`

func writeNotionConfig(t *testing.T, acknowledge string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "notion.yaml")
	if err := os.WriteFile(path, []byte(strings.Replace(notionConfig, "%s", acknowledge, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// notionTwin answers users/me for the given workspace and counts every request.
func notionTwin(t *testing.T, workspace string, calls *int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		if r.Method+" "+r.URL.Path != "GET /v1/users/me" {
			t.Errorf("feed notion issued %s %s before its gates passed", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, `{"object":"user","id":"u1","type":"bot","bot":{"workspace_name":"`+workspace+`"}}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFeedNotionRefusesWithoutTheAcknowledgement(t *testing.T) {
	t.Setenv(EnvNotionToken, "ntn_twin")
	calls := 0
	srv := notionTwin(t, "twin", &calls)
	_, stderr, code := run(t, t.Context(), "feed", "notion", "--config", writeNotionConfig(t, ""), "--notion-api", srv.URL)
	if code != ExitAuth || !strings.Contains(stderr, "notion.capabilities") {
		t.Errorf("exit %d: %s", code, stderr)
	}
	if calls != 0 {
		t.Errorf("%d request(s) reached Notion before the acknowledgement was checked", calls)
	}
}

func TestFeedNotionRefusesAnotherWorkspacesSecret(t *testing.T) {
	t.Setenv(EnvNotionToken, "ntn_twin")
	calls := 0
	srv := notionTwin(t, "someone-else", &calls)
	_, stderr, code := run(t, t.Context(), "feed", "notion",
		"--config", writeNotionConfig(t, "acknowledge_read_only: [notion.capabilities]"), "--notion-api", srv.URL)
	if code != ExitAuth || !strings.Contains(stderr, `belongs to "someone-else"`) {
		t.Errorf("exit %d: %s", code, stderr)
	}
	if calls != 1 {
		t.Errorf("%d request(s); only users/me may be issued before the workspace is checked", calls)
	}
}

func TestFeedNotionRefusesASecretThatIsNotAnIntegrations(t *testing.T) {
	t.Setenv(EnvNotionToken, "xoxb-not-notion")
	_, stderr, code := run(t, t.Context(), "feed", "notion",
		"--config", writeNotionConfig(t, "acknowledge_read_only: [notion.capabilities]"))
	if code != ExitAuth || strings.Contains(stderr, "xoxb-not-notion") {
		t.Errorf("exit %d: %s", code, stderr)
	}
}

func TestFeedNotionRefusesABadConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notion.yaml")
	if err := os.WriteFile(path, []byte("workspace: twin\nprincipal: x\ndatabases: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, stderr, code := run(t, t.Context(), "feed", "notion", "--config", path)
	if code != ExitUsage || !strings.Contains(stderr, "no databases") {
		t.Errorf("exit %d: %s", code, stderr)
	}
}

// `--check` (007 T032, US5): everything is read, nothing is sent, and each page not registered is
// named with the clause that excluded it.

func TestFeedNotionCheckOverTheTwinRecording(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notion.yaml")
	if err := os.WriteFile(path, []byte(`
workspace: twin
principal: oidc:operator-twin
acknowledge_read_only: [notion.capabilities]
databases:
  - database: 0a1b2c3d-4e5f-4071-8293-a4b5c6d7e8f9
    kind: postmortem
    entity_property: Services
    entities:
      Checkout: otel.service.name=checkout
      Payments: otel.service.name=payments
    summary_properties: [Summary, Root cause, Resolution]
    authored_property: Incident date
  - database: 1b2c3d4e-5f60-4182-93a4-b5c6d7e8f90a
    kind: runbook
    entity_property: Service
    entities:
      Checkout: otel.service.name=checkout
    summary_properties: [Purpose]
`), 0o600); err != nil {
		t.Fatal(err)
	}
	// No --server: a check that tried to reach the graph would fail here.
	stdout, stderr, code := run(t, t.Context(), "feed", "notion", "--config", path, "--check",
		"--replay", "../../fixtures/notion-knowledge-twin-01")
	if code != ExitOK {
		t.Fatalf("exit %d: %s\n%s", code, stderr, stdout)
	}
	for _, want := range []string{
		"nothing was sent to the graph",
		"acknowledges notion.capabilities",
		"this check read a recording",
		"database 0a1b2c3d-4e5f-4071-8293-a4b5c6d7e8f9 (postmortem)",
		`property "Services" (entities): found`,
		`property "Resolution" (summary): NOT FOUND on any page`,
		`"Vendor bridge outage": unmapped value Search`,
		`"Payments pool exhaustion"`,
		"database 1b2c3d4e-5f60-4182-93a4-b5c6d7e8f90a (runbook)",
		`"Checkout rollback runbook"`,
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("--check does not say %q:\n%s", want, stdout)
		}
	}
	// Titles are printed; summaries never are.
	if strings.Contains(stdout, "Ignore previous instructions") || strings.Contains(stdout, "slower than planned") {
		t.Errorf("--check printed a summary:\n%s", stdout)
	}
}

func TestFeedNotionCheckAgainstNotionWritesNothing(t *testing.T) {
	t.Setenv(EnvNotionToken, "ntn_twin")
	const (
		db       = "0a1b2c3d-4e5f-6071-8293-a4b5c6d7e8f9"
		ds       = "9a1b2c3d-4e5f-6071-8293-a4b5c6d7e8f9"
		linkedDS = "8a1b2c3d-4e5f-6071-8293-a4b5c6d7e8f9"
	)
	page := func(id, parent, title, service string) string {
		return `{"object":"page","id":"` + id + `","created_time":"2026-05-14T11:00:00.000Z",` +
			`"last_edited_time":"2026-05-15T09:00:00.000Z","parent":{"type":"data_source_id","data_source_id":"` + parent + `"},` +
			`"properties":{"Name":{"id":"title","type":"title","title":[{"type":"text","plain_text":"` + title + `"}]},` +
			`"Services":{"id":"sv","type":"multi_select","multi_select":[{"name":"` + service + `"}]}}}`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/users/me":
			_, _ = io.WriteString(w, `{"object":"user","id":"u1","type":"bot","bot":{"workspace_name":"twin"}}`)
		case "GET /v1/databases/" + db:
			_, _ = io.WriteString(w, `{"object":"database","id":"`+db+`","title":[{"plain_text":"Postmortems"}],`+
				`"data_sources":[{"id":"`+ds+`"}]}`)
		case "POST /v1/data_sources/" + ds + "/query":
			_, _ = io.WriteString(w, `{"object":"list","has_more":false,"results":[`+
				page("c1000000-0000-4000-8000-000000000001", ds, "Checkout outage", "Checkout")+","+
				page("c1000000-0000-4000-8000-000000000002", ds, "Search outage", "Search")+","+
				page("c1000000-0000-4000-8000-000000000003", linkedDS, "Linked view page", "Checkout")+`]}`)
		default:
			t.Errorf("--check issued %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	config := filepath.Join(dir, "notion.yaml")
	if err := os.WriteFile(config, []byte(strings.Replace(notionConfig, "%s",
		"acknowledge_read_only: [notion.capabilities]", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := run(t, t.Context(), "feed", "notion", "--config", config, "--check", "--notion-api", srv.URL)
	if code != ExitOK {
		t.Fatalf("exit %d: %s\n%s", code, stderr, stdout)
	}
	for _, want := range []string{
		`workspace: "twin", the integration's own`,
		"data sources: " + ds,
		"registered: 2",
		`"Search outage": unmapped value Search`,
		"outside the configured databases: 1",
		"notion budget: 3 call(s)",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("--check does not say %q:\n%s", want, stdout)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "notion-state.json")); !os.IsNotExist(err) {
		t.Errorf("--check wrote the state file: %v", err)
	}
}

func TestFeedNotionCheckRecordsNothing(t *testing.T) {
	_, stderr, code := run(t, t.Context(), "feed", "notion", "--config",
		writeNotionConfig(t, "acknowledge_read_only: [notion.capabilities]"), "--check", "--record", t.TempDir())
	if code != ExitUsage || !strings.Contains(stderr, "nothing for --record") {
		t.Errorf("exit %d: %s", code, stderr)
	}
}
