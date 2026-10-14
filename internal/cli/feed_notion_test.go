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
