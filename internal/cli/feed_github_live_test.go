// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/projector"
	"github.com/Pierre-Theophile/aisre/internal/sanitise"
	"github.com/Pierre-Theophile/aisre/internal/server"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres/pgtest"
)

// `feed github` live (004 T157): one cycle against a GitHub stand-in, into a real graph.

// emptyGraph serves an empty graph and returns its URL.
func emptyGraph(t *testing.T) string {
	t.Helper()
	store := pgtest.Open(t)
	srv, err := server.New(server.Config{
		Listen: "127.0.0.1:0", Auth: devAuthenticator(t), Projector: projector.New(store), Logger: discardLogger(),
	})
	if err != nil {
		t.Fatalf("build server: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	h := httptest.NewServer(srv.Handler())
	t.Cleanup(h.Close)
	return h.URL
}

func devToken(t *testing.T, args ...string) string {
	t.Helper()
	stdout, stderr, code := run(t, t.Context(), append([]string{"dev-token", "--dev"}, args...)...)
	if code != ExitOK {
		t.Fatalf("dev-token: exit %d (stderr %q)", code, stderr)
	}
	return strings.TrimSpace(stdout)
}

// githubStandIn serves the installation-token mint and github-deployment-01's recorded bodies at the
// API's own paths, and records the assertions it was handed.
func githubStandIn(t *testing.T, permissions string) (*httptest.Server, func() []string) {
	t.Helper()
	body := func(kind string) []byte {
		raw, err := os.ReadFile(filepath.Join("..", "..", "fixtures", "github-deployment-01", "payloads", kind, "000001.json"))
		if err != nil {
			t.Fatalf("read %s: %v", kind, err)
		}
		return raw
	}
	var statuses struct {
		Statuses json.RawMessage `json:"statuses"`
	}
	if err := json.Unmarshal(body("deployment-statuses"), &statuses); err != nil {
		t.Fatalf("decode statuses: %v", err)
	}
	var mu sync.Mutex
	var assertions []string
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/42/access_tokens":
			mu.Lock()
			assertions = append(assertions, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"ghs_live","expires_at":"2099-01-01T00:00:00Z","permissions":` +
				permissions + `,"repository_selection":"selected"}`))
		case "/rate_limit":
			_, _ = w.Write([]byte(`{"resources":{"core":{"limit":5000,"remaining":4990,"used":10}}}`))
		case "/installation/repositories":
			_, _ = w.Write(body("installation-repositories"))
		case "/repos/acme/storefront/actions/runs":
			_, _ = w.Write(body("workflow-runs"))
		case "/repos/acme/storefront/deployments":
			_, _ = w.Write(body("deployments"))
		case "/repos/acme/storefront/deployments/4321/statuses":
			_, _ = w.Write(statuses.Statuses)
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	t.Cleanup(gh.Close)
	return gh, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), assertions...)
	}
}

func githubMap(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "github.yaml")
	if err := os.WriteFile(path, []byte(`environments: [production]
deploy_workflows: [Deploy]
automation_accounts: [release-runner]
targets:
  acme/storefront:
    - namespace: k8s.deployment
      value: shop/storefront
`), 0o600); err != nil {
		t.Fatalf("write map: %v", err)
	}
	return path
}

// A live cycle reads GitHub, proves the credential it got from the helper, and puts the rollout in the
// graph, where a query finds it with its commit and its run.
func TestFeedGitHubLiveCyclePutsTheRolloutInTheGraph(t *testing.T) {
	graphURL := emptyGraph(t)
	gh, assertions := githubStandIn(t, `{"deployments":"read","actions":"read","metadata":"read"}`)

	_, stderr, code := run(t, t.Context(), "--server", graphURL,
		"--token", devToken(t, "--user", "github-feeder", "--roles", "f", "--source-id", "github:twin"),
		"feed", "github", "--org", "twin", "--installation", "42", "--api", gh.URL,
		"--app-assertion-command", "printf 'helper-assertion\\n'", "--map", githubMap(t), "--once",
		// A backfill: the recorded deployment is from March, and the default lookback reads ten minutes.
		"--lookback", "9000h")
	if code != ExitOK {
		t.Fatalf("feed github --once: exit %d\n%s", code, stderr)
	}
	if got := assertions(); len(got) != 1 || got[0] != "helper-assertion" {
		t.Errorf("the token mint was handed %q; want the helper's stdout, trimmed, once", got)
	}

	stdout, stderr, code := run(t, t.Context(), "--server", graphURL,
		"--token", devToken(t, "--user", "alice", "--roles", "r"),
		"--output", "json", "query", "subgraph",
		"github.change=repositories/555/deployments/4321/targets/k8s.deployment/shop/storefront",
		"--as-of", "2026-03-01T14:03:12Z", "--hops", "1")
	if code != ExitOK {
		t.Fatalf("query subgraph: exit %d\n%s", code, stderr)
	}
	// Nothing describes shop/storefront in this graph, so the rollout stands unattached (FR-028); what
	// matters is that it is there, dated at the platform's completion instant, with its actor and run.
	for _, want := range []string{
		"deployed acme/storefront to shop/storefront on production", `"actor":"ada"`,
		"2026-03-01T14:03:12Z", "https://github.com/acme/monorepo/actions/runs/99",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the graph does not hold the live rollout's %q:\n%s", want, stdout)
		}
	}
}

// A credential that can write is refused at startup, before any read, and the exit says it was the
// credential.
func TestFeedGitHubLiveRefusesAWriteCapableCredential(t *testing.T) {
	gh, _ := githubStandIn(t, `{"deployments":"write","metadata":"read"}`)
	_, stderr, code := run(t, t.Context(), "--server", "http://127.0.0.1:1", "--token", "x",
		"feed", "github", "--org", "twin", "--installation", "42", "--api", gh.URL,
		"--app-assertion", "a", "--map", githubMap(t), "--once")
	if code == ExitOK || !strings.Contains(stderr, "read-only gate refused") {
		t.Errorf("exit %d; a write-capable credential must stop the run at the gate:\n%s", code, stderr)
	}
}

// A mapping file with a misspelt key is refused: it would otherwise load as an empty allowlist, which
// emits nothing and looks exactly like a quiet estate.
func TestFeedGitHubRefusesAMappingWithAnUnknownKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "github.yaml")
	if err := os.WriteFile(path, []byte("environment: [production]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, stderr, code := run(t, t.Context(), "feed", "github", "--org", "twin", "--installation", "42",
		"--app-assertion", "a", "--map", path)
	if code != ExitUsage || !strings.Contains(stderr, "environment") {
		t.Errorf("exit %d; a misspelt mapping key must be refused and named:\n%s", code, stderr)
	}
}

// A live run with no credential says where one comes from.
func TestFeedGitHubLiveNeedsACredential(t *testing.T) {
	_, stderr, code := run(t, t.Context(), "feed", "github",
		"--org", "acme", "--installation", "42", "--environments", "production")
	if code != ExitUsage || !strings.Contains(stderr, "--app-assertion-command") {
		t.Errorf("exit %d; a live run with no credential must say where one comes from:\n%s", code, stderr)
	}
}

// --replay runs the same feeder over a recording: the command that could only dry-run now replays too.
func TestFeedGitHubReplaysARecording(t *testing.T) {
	graphURL := emptyGraph(t)
	_, stderr, code := run(t, t.Context(), "--server", graphURL,
		"--token", devToken(t, "--user", "github-feeder", "--roles", "f", "--source-id", "github:twin"),
		"feed", "github", "--org", "twin", "--map", githubMap(t),
		"--replay", filepath.Join("..", "..", "fixtures", "github-deployment-01"))
	if code != ExitOK {
		t.Fatalf("feed github --replay: exit %d\n%s", code, stderr)
	}
	stdout, stderr, code := run(t, t.Context(), "--server", graphURL,
		"--token", devToken(t, "--user", "alice", "--roles", "r"),
		"--output", "json", "query", "subgraph",
		"github.change=repositories/555/deployments/4321/targets/k8s.deployment/shop/storefront",
		"--as-of", "2026-03-01T14:03:12Z", "--hops", "1")
	if code != ExitOK || !strings.Contains(stdout, "deployed acme/storefront to shop/storefront on production") {
		t.Errorf("query subgraph after a replay: exit %d\n%s\n%s", code, stdout, stderr)
	}
}

// A live run records (004 T104): the recording holds only sanitised payloads and the events derived
// from them — no organisation, repository, person, commit or deployer URL of the estate anywhere under
// the directory — while the graph the run fed holds the real rollout.
func TestFeedGitHubLiveRecordingIsSanitisedBeforeAnythingTouchesDisk(t *testing.T) {
	key, err := sanitise.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(sanitise.KeyEnv, key.Hex())
	graphURL := emptyGraph(t)
	gh, _ := githubStandIn(t, `{"deployments":"read","actions":"read","metadata":"read"}`)
	dir := filepath.Join(t.TempDir(), "campaign")

	_, stderr, code := run(t, t.Context(), "--server", graphURL,
		"--token", devToken(t, "--user", "github-feeder", "--roles", "f", "--source-id", "github:twin"),
		"feed", "github", "--org", "twin", "--installation", "42", "--api", gh.URL,
		"--app-assertion", "a", "--map", githubMap(t), "--once", "--lookback", "9000h", "--record", dir)
	if code != ExitOK {
		t.Fatalf("feed github --record: exit %d\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "recorded 5 sanitised payload(s)") {
		t.Errorf("the run did not report the recording it made:\n%s", stderr)
	}

	var files int
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		files++
		raw, _ := os.ReadFile(path)
		// The login is looked for as a JSON string, quotes included: bare, `ada` is three hex
		// digits, and the pseudonyms a random key produces contained it about one run in thirty.
		for _, leak := range []string{"acme", "storefront", `"ada"`, "8f5bd3c0b0e4a1d2c3f4a5b6c7d8e9f0a1b2c3d4",
			"SHOULD-NEVER-BE-STORED", "shop/"} {
			if strings.Contains(string(raw), leak) {
				t.Errorf("%s holds %q", strings.TrimPrefix(path, dir), leak)
			}
		}
		return nil
	})
	if files < 7 {
		t.Errorf("%d files in the recording; want five payloads, their index, the events and the manifest", files)
	}
	events, _ := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if !strings.Contains(string(events), `"ROLLOUT"`) || !strings.Contains(string(events), "k8s.deployment") {
		t.Errorf("the recording's events hold no attached rollout; the pseudonymised mapping did not match "+
			"the pseudonymised payloads:\n%s", events)
	}

	// And the graph the run fed is the real one.
	stdout, _, code := run(t, t.Context(), "--server", graphURL,
		"--token", devToken(t, "--user", "alice", "--roles", "r"),
		"--output", "json", "query", "subgraph",
		"github.change=repositories/555/deployments/4321/targets/k8s.deployment/shop/storefront",
		"--as-of", "2026-03-01T14:03:12Z", "--hops", "1")
	if code != ExitOK || !strings.Contains(stdout, "deployed acme/storefront") {
		t.Errorf("the live graph does not hold the real rollout (exit %d)", code)
	}
}

// Without the corpus key a live recording is refused before a directory exists or anything is read.
func TestFeedGitHubLiveRecordingNeedsTheCorpusKey(t *testing.T) {
	t.Setenv(sanitise.KeyEnv, "")
	dir := filepath.Join(t.TempDir(), "campaign")
	_, stderr, code := run(t, t.Context(), "feed", "github", "--org", "twin", "--installation", "42",
		"--app-assertion", "a", "--environments", "production", "--record", dir)
	if code != ExitUsage || !strings.Contains(stderr, sanitise.KeyEnv) || !strings.Contains(stderr, "FR-137") {
		t.Errorf("exit %d; a recording without the key must be refused, naming the key and the rule:\n%s", code, stderr)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("the refused recording left a directory behind")
	}
}
