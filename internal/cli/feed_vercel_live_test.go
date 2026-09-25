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
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/sanitise"
)

// `feed vercel` live (004 T158): one cycle against a Vercel stand-in, into a real graph.

func vercelStandIn(t *testing.T) *httptest.Server {
	t.Helper()
	body := func(kind string) []byte {
		raw, err := os.ReadFile(filepath.Join("..", "..", "fixtures", "vercel-config-change-01", "payloads", kind, "000001.json"))
		if err != nil {
			t.Fatalf("read %s: %v", kind, err)
		}
		return raw
	}
	var listing struct {
		Projects []json.RawMessage `json:"projects"`
	}
	if err := json.Unmarshal(body("projects"), &listing); err != nil {
		t.Fatalf("decode projects: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer vercel-token" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		switch r.URL.Path {
		case "/v9/projects/prj_storefront":
			_, _ = w.Write(listing.Projects[0])
		case "/v9/projects/prj_storefront/env":
			_, _ = w.Write(body("project-env"))
		case "/v7/deployments":
			_, _ = w.Write(body("deployments"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A live cycle reads the mapped project, its configuration and its production deployments, and the
// promoted rollout is in the graph with its commit.
func TestFeedVercelLiveCyclePutsTheRolloutInTheGraph(t *testing.T) {
	graphURL := emptyGraph(t)
	srv := vercelStandIn(t)
	_, stderr, code := run(t, t.Context(), "--server", graphURL,
		"--token", devToken(t, "--user", "vercel-feeder", "--roles", "f", "--source-id", "vercel:twin"),
		"feed", "vercel", "--org", "twin", "--api", srv.URL, "--vercel-token", "vercel-token",
		"--projects", "prj_storefront=k8s.deployment:shop/storefront", "--assert-read-only", "--once",
		"--lookback", "9000h")
	if code != ExitOK {
		t.Fatalf("feed vercel --once: exit %d\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "evidence=operator_asserted") {
		t.Errorf("the gate did not record that read-only rests on the operator's word:\n%s", stderr)
	}
	stdout, stderr, code := run(t, t.Context(), "--server", graphURL,
		"--token", devToken(t, "--user", "alice", "--roles", "r"),
		"query", "diff", "vercel.project=prj_storefront",
		// To now: Vercel states PROMOTED as a state with no instant, so a promotion is dated from the
		// poll that saw it (research §5.2) — and a live poll runs now, not on the fixture's day.
		"--from", "2026-09-20T00:00:00Z", "--to", time.Now().UTC().Add(time.Hour).Format(time.RFC3339))
	if code != ExitOK {
		t.Fatalf("query diff: exit %d\n%s", code, stderr)
	}
	// The promoted rollout with its commit, and the configuration change dated by the platform.
	for _, want := range []string{"ROLLOUT", "9f8e7d6c5b4a", "CONFIG_CHANGE", "FEATURE_NEW_CHECKOUT"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the graph does not hold the live cycle's %q:\n%s", want, stdout)
		}
	}
}

// Vercel reports nothing about write capability, so a live run without the operator's assertion is
// refused before any read.
func TestFeedVercelLiveNeedsTheOperatorsAssertion(t *testing.T) {
	_, stderr, code := run(t, t.Context(), "feed", "vercel", "--org", "twin", "--vercel-token", "t",
		"--projects", "prj_storefront=k8s.deployment:shop/storefront")
	if code != ExitAuth || !strings.Contains(stderr, "--assert-read-only") {
		t.Errorf("exit %d; a live run with no assertion must be refused and say what is missing:\n%s", code, stderr)
	}
}

// A live Vercel run records through the sanitiser (004 T104): no project, repository, person, commit
// or deployment of the estate anywhere under the recording, and the derived events attach the rollout.
func TestFeedVercelLiveRecordingIsSanitisedBeforeAnythingTouchesDisk(t *testing.T) {
	key, err := sanitise.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(sanitise.KeyEnv, key.Hex())
	graphURL := emptyGraph(t)
	srv := vercelStandIn(t)
	dir := filepath.Join(t.TempDir(), "campaign")
	_, stderr, code := run(t, t.Context(), "--server", graphURL,
		"--token", devToken(t, "--user", "vercel-feeder", "--roles", "f", "--source-id", "vercel:twin"),
		"feed", "vercel", "--org", "twin", "--api", srv.URL, "--vercel-token", "vercel-token",
		"--projects", "prj_storefront=k8s.deployment:shop/storefront", "--assert-read-only", "--once",
		"--lookback", "9000h", "--record", dir)
	if code != ExitOK {
		t.Fatalf("feed vercel --record: exit %d\n%s", code, stderr)
	}
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, _ := os.ReadFile(path)
		for _, leak := range []string{"prj_storefront", "storefront", "acme", "usr_ada", "usr_grace",
			"dpl_storefront_77", "9f8e7d6c5b4a39281706f5e4d3c2b1a098765432", "shop/"} {
			if strings.Contains(string(raw), leak) {
				t.Errorf("%s holds %q", strings.TrimPrefix(path, dir), leak)
			}
		}
		return nil
	})
	events, _ := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	for _, want := range []string{`"ROLLOUT"`, `"CONFIG_CHANGE"`, "PAYMENTS_API_URL", "k8s.deployment"} {
		if !strings.Contains(string(events), want) {
			t.Errorf("the recording's events lack %s:\n%s", want, events)
		}
	}
}
