// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/sanitise"
)

// `feed github` (004 T072, FR-006, FR-008).
//
// The command's contract is mostly about what it does NOT do: a dry run that issues no operation from
// the published surface, a live run that refuses rather than half-reading, and a `--record` that
// refuses rather than writing an unsanitised response to disk.

// tokenServer answers the one non-read call this connector makes, and records every request so a test
// can assert the dry run made exactly one.
func tokenServer(t *testing.T, permissions, selection string) (*httptest.Server, *[]string) {
	t.Helper()
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"ghs_x","expires_at":"2026-03-01T10:00:00Z",` +
			`"permissions":` + permissions + `,"repository_selection":"` + selection + `"}`))
	}))
	t.Cleanup(server.Close)
	return server, &paths
}

// The dry run proves the credential and issues nothing from the published surface. For GitHub that is
// achievable because the permissions and the selection ride on the token's own response.
func TestFeedGitHubDryRunProvesTheCredentialAndReadsNothingElse(t *testing.T) {
	server, paths := tokenServer(t, `{"deployments":"read","metadata":"read"}`, "selected")

	stdout, stderr, code := run(t, context.Background(), "feed", "github", "--dry-run",
		"--installation", "42", "--api", server.URL, "--app-assertion", "an-assertion")
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	for _, want := range []string{
		"read-only gate: passed",
		"evidence: platform_reported",
		"deployments=read",
		"repository selection: selected (boundary enforced by the platform)",
		"no operation from the published surface was issued",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the dry run does not report %q:\n%s", want, stdout)
		}
	}
	// Exactly one request, and it is the mint. A dry run that read "just a little" would turn the one
	// command an operator trusts to be harmless into one they have to reason about.
	if len(*paths) != 1 || !strings.HasPrefix((*paths)[0], "POST /app/installations/42/access_tokens") {
		t.Errorf("the dry run made %v, want only the credential mint", *paths)
	}
}

// The evidence is printed rather than a bare verdict: GitHub's gate rests on a platform statement and
// Vercel's on an operator's assertion, and "read-only: yes" for both would flatten the stronger claim.
func TestFeedGitHubDryRunNamesWhoSaidTheCredentialIsReadOnly(t *testing.T) {
	server, _ := tokenServer(t, `{"metadata":"read"}`, "all")

	stdout, _, code := run(t, context.Background(), "feed", "github", "--dry-run",
		"--installation", "7", "--api", server.URL, "--app-assertion", "a")
	if code != 0 {
		t.Fatalf("exit %d, want 0: %s", code, stdout)
	}
	if !strings.Contains(stdout, "evidence: platform_reported") {
		t.Errorf("the dry run does not say the PLATFORM reported it:\n%s", stdout)
	}
	if strings.Contains(stdout, "operator_asserted") {
		t.Errorf("the dry run claims an operator assertion for a platform statement:\n%s", stdout)
	}
}

// A write permission refuses the dry run with the auth exit code, and names the grant.
func TestFeedGitHubDryRunRefusesAWriteCapableCredential(t *testing.T) {
	server, _ := tokenServer(t, `{"deployments":"write","metadata":"read"}`, "selected")

	stdout, stderr, code := run(t, context.Background(), "feed", "github", "--dry-run",
		"--installation", "42", "--api", server.URL, "--app-assertion", "a")
	if code != ExitAuth {
		t.Fatalf("exit %d, want ExitAuth (%d)\nstdout: %s\nstderr: %s", code, ExitAuth, stdout, stderr)
	}
	if !strings.Contains(stdout, "REFUSED") {
		t.Errorf("the refusal is not reported on stdout:\n%s", stdout)
	}
	if !strings.Contains(stdout, "deployments=write") {
		t.Errorf("the refusal does not name the grant that caused it:\n%s", stdout)
	}
}

// The dry run needs what the gate reads, and says which piece is missing rather than failing obscurely.
func TestFeedGitHubDryRunSaysWhatItIsMissing(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"no installation": {
			args: []string{"feed", "github", "--dry-run", "--app-assertion", "a"},
			want: "--installation is required",
		},
		"no assertion": {
			args: []string{"feed", "github", "--dry-run", "--installation", "42"},
			want: "--app-assertion is required",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, stderr, code := run(t, context.Background(), tc.args...)
			if code != ExitUsage {
				t.Errorf("exit %d, want ExitUsage (%d)", code, ExitUsage)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("the refusal does not say %q:\n%s", tc.want, stderr)
			}
		})
	}
}

// `--record` on a live run with no corpus key is refused rather than writing an unsanitised GitHub
// response to disk. FR-137 forbids that at any point INCLUDING during a failed or aborted run, which is
// why no commit hook can stand in: by the time a hook runs the bytes have already been written. With the
// key, the run records through the sanitiser (feed_github_live_test.go).
func TestFeedGitHubRefusesToRecordALiveRun(t *testing.T) {
	t.Setenv(sanitise.KeyEnv, "")
	_, stderr, code := run(t, context.Background(), "feed", "github",
		"--org", "acme", "--installation", "42", "--environments", "production",
		"--record", t.TempDir())
	if code != ExitUsage {
		t.Fatalf("exit %d, want ExitUsage (%d): %s", code, ExitUsage, stderr)
	}
	if !strings.Contains(stderr, "FR-137") {
		t.Errorf("the refusal does not name the requirement it is enforcing:\n%s", stderr)
	}
}

// Recording a replay would produce a fixture of a fixture.
func TestFeedGitHubRefusesRecordAndReplayTogether(t *testing.T) {
	_, stderr, code := run(t, context.Background(), "feed", "github",
		"--record", t.TempDir(), "--replay", t.TempDir())
	if code != ExitUsage {
		t.Fatalf("exit %d, want ExitUsage (%d): %s", code, ExitUsage, stderr)
	}
	if !strings.Contains(stderr, "mutually exclusive") {
		t.Errorf("the refusal does not say why:\n%s", stderr)
	}
}

// A live run with no environment list is refused rather than treating every preview deployment as a
// production change. There is deliberately no default (FR-023).
func TestFeedGitHubRefusesALiveRunWithNoEnvironmentList(t *testing.T) {
	_, stderr, code := run(t, context.Background(), "feed", "github",
		"--org", "acme", "--installation", "42")
	if code != ExitUsage {
		t.Fatalf("exit %d, want ExitUsage (%d): %s", code, ExitUsage, stderr)
	}
	if !strings.Contains(stderr, "--environments is required") {
		t.Errorf("the refusal does not name the missing configuration:\n%s", stderr)
	}
	if !strings.Contains(stderr, "FR-023") {
		t.Errorf("the refusal does not say which requirement makes it operator configuration:\n%s", stderr)
	}
}
