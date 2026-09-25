// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/sanitise"
)

// `feed vercel` (004 T090).

// The dry run REPORTS the token's regime and refuses to declare one.
//
// This is the assertion T034 turns on. GitHub's dry run can say "boundary enforced by the platform"
// because GitHub's response states the repository selection in a documented field. Vercel's position is
// weaker: the platform CAN pin a token to one project, but the spelling that scope takes when the token
// is read back is not established. Printing the strong sentence from a guessed field would claim more
// than is known, and flattening the two platforms into one output would erase the difference
// (contracts/read-only-operations.md §3.1).
func TestFeedVercelDryRunReportsTheRegimeRatherThanDeclaringOne(t *testing.T) {
	stdout, _, code := run(t, t.Context(), "feed", "vercel", "--dry-run", "--vercel-token", "tok")
	if code != 0 {
		t.Fatalf("exit %d, want 0: %s", code, stdout)
	}
	if !strings.Contains(stdout, "NOT ESTABLISHED") {
		t.Errorf("the dry run does not say the regime is unestablished:\n%s", stdout)
	}
	// It must NOT claim the stronger thing.
	if strings.Contains(stdout, "boundary enforced by the platform") {
		t.Errorf("the dry run claims a platform-enforced boundary from a field nobody has established:\n%s",
			stdout)
	}
	// And it states what it read instead: the surface, every operation a GET.
	if !strings.Contains(stdout, "every one a GET") {
		t.Errorf("the dry run does not state the surface:\n%s", stdout)
	}
	for _, op := range []string{"GET /v7/deployments", "GET /v9/projects"} {
		if !strings.Contains(stdout, op) {
			t.Errorf("the dry run omits %s:\n%s", op, stdout)
		}
	}
	// The decrypt refusal is named, because it is the one a reader most wants confirmed.
	if !strings.Contains(stdout, "FR-038") {
		t.Errorf("the dry run does not name the environment-variable refusal:\n%s", stdout)
	}
	if !strings.Contains(stdout, "no operation from the published surface was issued") {
		t.Errorf("the dry run does not state that it issued nothing:\n%s", stdout)
	}
}

// A dry run with no token has nothing to report, and says so rather than printing a verdict.
func TestFeedVercelDryRunNeedsAToken(t *testing.T) {
	_, stderr, code := run(t, t.Context(), "feed", "vercel", "--dry-run")
	if code == 0 {
		t.Error("a dry run with no credential exited zero")
	}
	if !strings.Contains(stderr, "--vercel-token is required") {
		t.Errorf("stderr does not say why: %s", stderr)
	}
}

// `--record` on a live run with no corpus key is refused (FR-137): the sanitiser cannot be built, and
// nothing is written unsanitised in its place. With the key, the run records through it
// (feed_vercel_live_test.go).
func TestRecordingALiveVercelRunWithoutTheKeyIsRefused(t *testing.T) {
	t.Setenv(sanitise.KeyEnv, "")
	dir := filepath.Join(t.TempDir(), "campaign")
	_, stderr, code := run(t, t.Context(), "feed", "vercel",
		"--org", "twin", "--vercel-token", "tok", "--record", dir)
	if code == 0 {
		t.Fatal("`feed vercel --record` on a live run exited zero; recording a live Vercel response " +
			"would write it to disk unsanitised")
	}
	if !strings.Contains(stderr, "FR-137") {
		t.Errorf("stderr does not cite the requirement: %s", stderr)
	}
}

// --record and --replay together would record a fixture of a fixture.
func TestFeedVercelRefusesRecordAndReplayTogether(t *testing.T) {
	_, stderr, code := run(t, t.Context(), "feed", "vercel",
		"--org", "twin", "--record", t.TempDir(), "--replay", t.TempDir())
	if code == 0 {
		t.Error("--record with --replay exited zero")
	}
	if !strings.Contains(stderr, "mutually exclusive") {
		t.Errorf("stderr does not say why: %s", stderr)
	}
}

// A live run with no --org is refused: an empty suffix makes every team's events one source.
func TestFeedVercelNeedsAnOrg(t *testing.T) {
	_, stderr, code := run(t, t.Context(), "feed", "vercel")
	if code == 0 {
		t.Error("a run with no --org exited zero")
	}
	if !strings.Contains(stderr, "--org is required") {
		t.Errorf("stderr does not say why: %s", stderr)
	}
}

// The operator's mapping is parsed strictly. A silently-dropped rule is a rollout attached to nothing,
// and the operator would see a working run with a quietly emptier graph.
func TestFeedVercelRefusesAMalformedProjectMapping(t *testing.T) {
	for _, spec := range []string{
		"prj_abc",                         // no target at all
		"prj_abc=k8s.deployment",          // no value
		"=k8s.deployment:shop/storefront", // no project
		"prj_abc=:shop/storefront",        // no namespace
		"prj_abc=k8s.deployment:",         // empty value
	} {
		_, stderr, code := run(t, t.Context(), "feed", "vercel",
			"--org", "twin", "--replay", t.TempDir(), "--projects", spec)
		if code == 0 {
			t.Errorf("--projects %q was accepted; the rollout would attach to nothing", spec)
			continue
		}
		if !strings.Contains(stderr, "projectId=namespace:value") {
			t.Errorf("--projects %q: stderr does not state the shape: %s", spec, stderr)
		}
	}
}

// And a project named twice is refused rather than silently keeping the last rule.
func TestFeedVercelRefusesADuplicateProjectMapping(t *testing.T) {
	_, stderr, code := run(t, t.Context(), "feed", "vercel",
		"--org", "twin", "--replay", t.TempDir(),
		"--projects", "prj_abc=k8s.deployment:shop/a",
		"--projects", "prj_abc=k8s.deployment:shop/b")
	if code == 0 {
		t.Fatal("a duplicate project mapping was accepted")
	}
	if !strings.Contains(stderr, "twice") {
		t.Errorf("stderr does not say the project was named twice: %s", stderr)
	}
}
