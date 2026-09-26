// SPDX-License-Identifier: Apache-2.0

package deploycoverage_test

import (
	"bufio"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/deploycoverage"
)

func fixtureDir(id string) string { return filepath.Join("..", "..", "fixtures", id) }

// Every deploy twin is complete against its own platform's answer: SC-001 and SC-002 measured on the
// public corpus. On the twins this is a regression gate rather than a finding — they were written
// against the shapes we expect — and the same measurement over the private corpus is T114's.
func TestEveryDeployTwinIsCompleteAgainstThePlatformsOwnAnswer(t *testing.T) {
	for _, id := range []string{
		"github-deployment-01", "github-status-states-01", "github-actor-kinds-01", "github-monorepo-01",
		"github-unattached-01", "github-rerun-01", "github-release-01", "github-doorbell-forged-01",
		"github-partial-poll-01", "vercel-promotion-01", "vercel-preview-excluded-01",
		"vercel-config-change-01", "deploy-cross-source-merge-01", "deploy-k8s-commit-merge-01",
		"deploy-rollback-01", "deploy-telemetry-rejection-01",
	} {
		reports, err := deploycoverage.Measure(fixtureDir(id), deploycoverage.Scope{})
		if err != nil {
			t.Errorf("%s: %v", id, err)
			continue
		}
		for _, r := range reports {
			if !r.Complete() {
				t.Errorf("%s is not complete:\n%s", id, r)
			}
		}
	}
}

// The differences are enumerated by id with a reason, never summarised: all four deployments that
// never succeeded are named with the states GitHub reported for them.
func TestTheExclusionsAreEnumeratedWithTheirReasons(t *testing.T) {
	reports, err := deploycoverage.Measure(fixtureDir("github-status-states-01"), deploycoverage.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 || reports[0].Platform != deploycoverage.GitHub {
		t.Fatalf("reports = %+v; want one GitHub report", reports)
	}
	r := reports[0]
	if r.Listed != 1 || r.Matched != 1 {
		t.Errorf("listed %d matched %d; want the one success, matched", r.Listed, r.Matched)
	}
	want := map[string]string{"2": "in_progress, queued", "3": "pending", "4": "failure", "5": "error"}
	if len(r.Excluded) != len(want) {
		t.Fatalf("excluded = %+v; want the four deployments that never succeeded", r.Excluded)
	}
	for _, d := range r.Excluded {
		if !strings.HasPrefix(d.Why, deploycoverage.WhyGitHubNotSucceeded) || !strings.Contains(d.Why, want[d.ID]) {
			t.Errorf("deployment %s excluded as %q; want the not-succeeded reason naming %q", d.ID, d.Why, want[d.ID])
		}
	}
}

// Previews are excluded and counted, and a stated rollback is reported as what it is rather than
// counted as an extra rollout.
func TestVercelExclusionsAndRollbacksAreNamed(t *testing.T) {
	reports, err := deploycoverage.Measure(fixtureDir("vercel-preview-excluded-01"), deploycoverage.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	r := reports[0]
	if r.Listed != 0 || len(r.Excluded) != 3 {
		t.Errorf("listed %d, excluded %+v; want nothing listed and all three non-production deployments named", r.Listed, r.Excluded)
	}
	for _, d := range r.Excluded {
		if !strings.HasPrefix(d.Why, deploycoverage.WhyVercelNotProduction) {
			t.Errorf("%s excluded as %q", d.ID, d.Why)
		}
	}

	reports, err = deploycoverage.Measure(fixtureDir("deploy-rollback-01"), deploycoverage.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	r = reports[0]
	if r.Listed != 3 || !r.Complete() || len(r.Other) != 1 || r.Other[0].Why != deploycoverage.WhyVercelRollback {
		t.Errorf("rollback fixture: %+v; want three deployments matched and the rollback under Other", r)
	}
}

// Probe: the measurement is not vacuous in either direction. A rollout removed from the events is
// named as missing; a rollout for a deployment the platform never listed as completed is named as
// extra.
func TestAGapIsNamedInBothDirections(t *testing.T) {
	dir := copyFixture(t, "github-status-states-01")
	events := filepath.Join(dir, "events.jsonl")
	lines := readLines(t, events)

	// Missing: drop the one rollout.
	kept := slices.DeleteFunc(slices.Clone(lines), func(l string) bool { return strings.Contains(l, `"observeChange"`) })
	writeLines(t, events, kept)
	r := measureOne(t, dir)
	if len(r.Missing) != 1 || r.Missing[0].ID != "1" || r.Missing[0].Why != deploycoverage.WhyMissing {
		t.Errorf("missing = %+v; want deployment 1 named as missing", r.Missing)
	}

	// Extra: restore it, and add a rollout for deployment 4, which only ever failed.
	var rollout string
	for _, l := range lines {
		if strings.Contains(l, `"observeChange"`) {
			rollout = l
		}
	}
	forged := strings.ReplaceAll(rollout, "/deployments/1/", "/deployments/4/")
	writeLines(t, events, append(slices.Clone(lines), forged))
	r = measureOne(t, dir)
	if len(r.Extra) != 1 || r.Extra[0].ID != "4" || r.Extra[0].Why != deploycoverage.WhyExtra {
		t.Errorf("extra = %+v; want deployment 4 named as extra", r.Extra)
	}
	if r.Complete() {
		t.Error("a report with an extra rollout claims to be complete")
	}
}

// A recording of anything else is refused rather than reported complete over nothing.
func TestARecordingWithNoDeployPlatformIsRefused(t *testing.T) {
	if _, err := deploycoverage.Measure(fixtureDir("baseline-topology-01"), deploycoverage.Scope{}); err == nil {
		t.Error("a Kubernetes and OpenTelemetry recording was measured; it holds no deploy platform")
	}
}

func measureOne(t *testing.T, dir string) deploycoverage.Report {
	t.Helper()
	reports, err := deploycoverage.Measure(dir, deploycoverage.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 {
		t.Fatalf("%d reports; want one", len(reports))
	}
	return reports[0]
}

func copyFixture(t *testing.T, id string) string {
	t.Helper()
	src, dst := fixtureDir(id), filepath.Join(t.TempDir(), id)
	err := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		if d.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), raw, 0o644)
	})
	if err != nil {
		t.Fatalf("copy %s: %v", id, err)
	}
	return dst
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var lines []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return lines
}

func writeLines(t *testing.T, path string, lines []string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}
