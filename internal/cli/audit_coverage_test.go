// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `audit coverage` end to end (T013, T014, T017, T018).
//
// Every test here types the command, exactly as contracts/cli.md publishes it, and reads the two
// renderings back. The numbers asserted are the synthetic fixture's, which reproduce the
// published September 2026 aggregate's shape: a ceiling of 8/13 and π₀ of 0.384615.

func syntheticList(name string) string {
	return filepath.Join("..", "..", "fixtures", "audits", name, "incidents.yaml")
}

// auditJSON runs `audit coverage --output json` over a synthetic list and decodes the result.
func auditJSON(t *testing.T, args ...string) map[string]any {
	t.Helper()
	stdout, stderr, code := run(t, context.Background(), args...)
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0\nstderr: %s", code, stderr)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
		t.Fatalf("decode output: %v\n%s", err, stdout)
	}
	return decoded
}

func TestAuditCoveragePublishesTheCeilingAndThePrior(t *testing.T) {
	result := auditJSON(t, "--output", "json", "audit", "coverage",
		"--input", syntheticList("synthetic-01"))

	if got, want := result["ceiling"], 0.615385; got != want {
		t.Errorf("ceiling = %v, want %v", got, want)
	}
	if got, want := result["prior"], 0.384615; got != want {
		t.Errorf("π₀ = %v, want %v", got, want)
	}
	if got, want := result["classifiable_count"], float64(13); got != want {
		t.Errorf("classifiable count = %v, want %v", got, want)
	}
	if got, want := result["feeder_set_in_force"], "+vendor-notice"; got != want {
		t.Errorf("feeder set in force = %v, want %v", got, want)
	}
	if items, ok := result["items"].([]any); !ok || len(items) != 13 {
		t.Errorf("items = %v, want 13", result["items"])
	}
}

func TestAuditCoverageTableCarriesEveryRung(t *testing.T) {
	stdout, stderr, code := run(t, context.Background(), "audit", "coverage",
		"--input", syntheticList("synthetic-01"))
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0\nstderr: %s", code, stderr)
	}
	for _, want := range []string{
		"ceiling 0.615385", "8 of 13", "0.384615",
		"feature-001", "+deploy", "+vendor-notice",
		"missing causes by category", "unobservable remainder",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("table output is missing %q\n%s", want, stdout)
		}
	}
}

// TestAuditCoverageFeederSetSelection: `--graph-config` is contracts/cli.md's spelling and
// `--feeder-set` is the format's; both select the same rung.
func TestAuditCoverageFeederSetSelection(t *testing.T) {
	for _, flag := range []string{"--feeder-set", "--graph-config"} {
		t.Run(flag, func(t *testing.T) {
			result := auditJSON(t, "--output", "json", "audit", "coverage",
				"--input", syntheticList("synthetic-01"), flag, "feature-001")
			if got, want := result["ceiling"], float64(0); got != want {
				t.Errorf("ceiling = %v, want %v", got, want)
			}
			if got, want := result["prior"], float64(1); got != want {
				t.Errorf("π₀ = %v, want %v", got, want)
			}
		})
	}
}

func TestAuditCoverageRejectsDisagreeingAliases(t *testing.T) {
	_, stderr, code := run(t, context.Background(), "audit", "coverage",
		"--input", syntheticList("synthetic-01"),
		"--feeder-set", "feature-001", "--graph-config", "+deploy")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(stderr, "disagree") {
		t.Errorf("stderr = %q, want it to say the flags disagree", stderr)
	}
}

func TestAuditCoverageWritesBothRenderings(t *testing.T) {
	dir := t.TempDir()
	stdout, stderr, code := run(t, context.Background(), "audit", "coverage",
		"--input", syntheticList("synthetic-01"), "--out", dir, "--aggregate-only")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0\nstderr: %s", code, stderr)
	}

	jsonPath := filepath.Join(dir, "coverage-audit-synthetic-01.json")
	markdownPath := filepath.Join(dir, "coverage-audit-synthetic-01.md")
	for _, path := range []string{jsonPath, markdownPath} {
		if !strings.Contains(stdout, path) {
			t.Errorf("output does not name %s", path)
		}
	}

	encoded, err := os.ReadFile(jsonPath) //nolint:gosec // a path this test just wrote.
	if err != nil {
		t.Fatalf("read %s: %v", jsonPath, err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode %s: %v", jsonPath, err)
	}
	if decoded["ceiling"] != 0.615385 {
		t.Errorf("written ceiling = %v, want 0.615385", decoded["ceiling"])
	}
	// --aggregate-only is what makes the written pair publishable (FR-069a).
	if _, ok := decoded["items"]; ok {
		t.Error("the written aggregate carries per-incident items")
	}
	if strings.Contains(string(encoded), "SYN-01") {
		t.Error("the written aggregate leaks an incident reference")
	}

	markdown, err := os.ReadFile(markdownPath) //nolint:gosec // a path this test just wrote.
	if err != nil {
		t.Fatalf("read %s: %v", markdownPath, err)
	}
	for _, want := range []string{"62 %", "8 of 13", "0.384615"} {
		if !strings.Contains(string(markdown), want) {
			t.Errorf("markdown is missing %q", want)
		}
	}
}

func TestAuditCoverageRefusals(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "no incident list",
			args: []string{"audit", "coverage"},
			want: "no incident list",
		},
		{
			name: "a list that does not exist",
			args: []string{"audit", "coverage", "--input", filepath.Join(t.TempDir(), "nope.yaml")},
			want: "read incident list",
		},
		{
			name: "a feeder set the list never declared",
			args: []string{"audit", "coverage", "--input", syntheticList("synthetic-01"),
				"--feeder-set", "+telepathy"},
			want: "not declared by this list",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, stderr, code := run(t, context.Background(), test.args...)
			if code != ExitUsage {
				t.Fatalf("exit = %d, want %d\nstderr: %s", code, ExitUsage, stderr)
			}
			if !strings.Contains(stderr, test.want) {
				t.Errorf("stderr = %q, want it to mention %q", stderr, test.want)
			}
		})
	}
}

// writeAudit runs the audit over a synthetic list and returns the path of the JSON it wrote.
func writeAudit(t *testing.T, dir, name string, extra ...string) string {
	t.Helper()
	args := append([]string{"audit", "coverage", "--input", syntheticList(name), "--out", dir}, extra...)
	_, stderr, code := run(t, context.Background(), args...)
	if code != ExitOK {
		t.Fatalf("audit %s: exit = %d\nstderr: %s", name, code, stderr)
	}
	return filepath.Join(dir, "coverage-audit-"+name+".json")
}

func TestAuditCompareAttributesTheMovement(t *testing.T) {
	dir := t.TempDir()
	before := writeAudit(t, dir, "synthetic-01")
	after := writeAudit(t, dir, "synthetic-02")

	stdout, stderr, code := run(t, context.Background(),
		"audit", "coverage", "compare", before, after)
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0\nstderr: %s", code, stderr)
	}
	for _, want := range []string{
		"the ceiling moved", "cdn.config_changes", "did not move", "new feeder set", "SYN-09",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("compare output is missing %q\n%s", want, stdout)
		}
	}
	// Every rung is reported, moved or not (FR-071a).
	for _, rung := range []string{"feature-001", "+deploy", "+vendor-notice", "+edge-cdn"} {
		if !strings.Contains(stdout, rung) {
			t.Errorf("compare output omits the rung %q", rung)
		}
	}
}

func TestAuditCompareOfIdenticalRunsSaysSo(t *testing.T) {
	dir := t.TempDir()
	path := writeAudit(t, dir, "synthetic-01")

	stdout, stderr, code := run(t, context.Background(), "audit", "coverage", "compare", path, path)
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0\nstderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "the ceiling did not move") {
		t.Errorf("output = %q, want it to report the ceiling as unmoved", stdout)
	}
}

func TestAuditCompareJSON(t *testing.T) {
	dir := t.TempDir()
	before := writeAudit(t, dir, "synthetic-01")
	after := writeAudit(t, dir, "synthetic-02")

	comparison := auditJSON(t, "--output", "json", "audit", "coverage", "compare", before, after)
	if got, want := comparison["ceiling_delta"], 0.076923; got != want {
		t.Errorf("ceiling delta = %v, want %v", got, want)
	}
	if comparison["comparable"] != true {
		t.Errorf("comparable = %v, want true", comparison["comparable"])
	}
}

func TestAuditCompareRefusesAMissingFile(t *testing.T) {
	dir := t.TempDir()
	path := writeAudit(t, dir, "synthetic-01")

	_, stderr, code := run(t, context.Background(), "audit", "coverage", "compare",
		path, filepath.Join(dir, "absent.json"))
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d\nstderr: %s", code, ExitUsage, stderr)
	}
	if !strings.Contains(stderr, "read result") {
		t.Errorf("stderr = %q, want it to name the unreadable result", stderr)
	}
}

// writeCriteria writes an evaluation report publishing the given criteria.
func writeCriteria(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "eval.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write criteria: %v", err)
	}
	return path
}

func TestAuditGuardPassesTargetsAtOrBelowTheCeiling(t *testing.T) {
	dir := t.TempDir()
	auditPath := writeAudit(t, dir, "synthetic-01", "--aggregate-only")
	report := writeCriteria(t, dir, `{"criteria":[
		{"id":"SC-001","metric":"pass@1","target":0.55,"annotation":"[ceiling-bounded]","audit":"synthetic-01"},
		{"id":"SC-005","metric":"time to provisional","target":0.95,"annotation":"[not ceiling-bounded: latency]"}
	]}`)

	stdout, stderr, code := run(t, context.Background(), "audit", "coverage", "guard",
		"--report", report, "--audit", auditPath)
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0\nstderr: %s\n%s", code, stderr, stdout)
	}
	if !strings.Contains(stdout, "checked 1, passed 1, failed 0, not checked 1") {
		t.Errorf("counts line missing from:\n%s", stdout)
	}
	if !strings.Contains(stdout, "NOT scaled down") {
		t.Errorf("the latency criterion is not reported as unscaled:\n%s", stdout)
	}
}

func TestAuditGuardFailsATargetAboveTheCeiling(t *testing.T) {
	dir := t.TempDir()
	auditPath := writeAudit(t, dir, "synthetic-01", "--aggregate-only")
	report := writeCriteria(t, dir, `{"criteria":[
		{"id":"SC-006","metric":"top hypothesis","target":0.90,"annotation":"[ceiling-bounded]","audit":"synthetic-01"}
	]}`)

	stdout, stderr, code := run(t, context.Background(), "audit", "coverage", "guard",
		"--report", report, "--audit", auditPath)
	if code != ExitVerification {
		t.Fatalf("exit = %d, want %d (a gate failure)\nstderr: %s", code, ExitVerification, stderr)
	}
	if !strings.Contains(stdout, "exceeds the ceiling") {
		t.Errorf("output does not explain the failure:\n%s", stdout)
	}
	if !strings.Contains(stderr, "FR-071") {
		t.Errorf("stderr = %q, want it to cite FR-071", stderr)
	}
}

// TestAuditGuardFailsAnUnannotatedCriterion is the loud failure T017 asks for by name.
func TestAuditGuardFailsAnUnannotatedCriterion(t *testing.T) {
	dir := t.TempDir()
	auditPath := writeAudit(t, dir, "synthetic-01", "--aggregate-only")
	report := writeCriteria(t, dir, `{"criteria":[
		{"id":"SC-099","metric":"something new","target":0.99}
	]}`)

	stdout, _, code := run(t, context.Background(), "audit", "coverage", "guard",
		"--report", report, "--audit", auditPath)
	if code != ExitVerification {
		t.Fatalf("exit = %d, want %d", code, ExitVerification)
	}
	if !strings.Contains(stdout, "ceiling annotation") {
		t.Errorf("output does not name the missing annotation:\n%s", stdout)
	}
}

func TestAuditGuardNeedsBothInputs(t *testing.T) {
	dir := t.TempDir()
	auditPath := writeAudit(t, dir, "synthetic-01", "--aggregate-only")

	for _, args := range [][]string{
		{"audit", "coverage", "guard"},
		{"audit", "coverage", "guard", "--audit", auditPath},
		{"audit", "coverage", "guard", "--report", writeCriteria(t, dir, `{"criteria":[]}`)},
	} {
		_, stderr, code := run(t, context.Background(), args...)
		if code != ExitUsage {
			t.Errorf("%v: exit = %d, want %d\nstderr: %s", args, code, ExitUsage, stderr)
		}
	}
}

// TestAuditGapsOverTheShippedCorpusReportsNone: T102 landed a fixture for every category of
// the unobservable remainder, so the command has nothing to warn about and says so. Exit 0
// either way — a corpus gap is reported on the evaluation run, never gated (T018).
func TestAuditGapsOverTheShippedCorpusReportsNone(t *testing.T) {
	dir := t.TempDir()
	auditPath := writeAudit(t, dir, "synthetic-01", "--aggregate-only")

	stdout, stderr, code := run(t, context.Background(), "audit", "coverage", "gaps",
		"--audit", auditPath, "--fixtures", filepath.Join("..", "..", "fixtures", "incidents"))
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0\nstderr: %s", code, stderr)
	}
	if strings.Contains(stdout, "warning:") || strings.Contains(stdout, "GAP") {
		t.Errorf("the shipped corpus covers every remainder category, but:\n%s", stdout)
	}
	for _, category := range []string{"latent_bug", "client_side_configuration", "business_data_change"} {
		if !strings.Contains(stdout, category) {
			t.Errorf("gap output omits %q\n%s", category, stdout)
		}
	}
	if !strings.Contains(stdout, "covered") {
		t.Errorf("the covered categories are not marked as covered:\n%s", stdout)
	}
}

// TestAuditGapsWarnsAndExitsZero is the other half: pointed at a corpus that has none of the
// fixtures, every remainder category is a gap, each is named with what would close it, and the
// exit code is still zero. Failing the build over an incomplete corpus would only teach people
// to stop measuring it.
func TestAuditGapsWarnsAndExitsZero(t *testing.T) {
	dir := t.TempDir()
	auditPath := writeAudit(t, dir, "synthetic-01", "--aggregate-only")

	stdout, stderr, code := run(t, context.Background(), "audit", "coverage", "gaps",
		"--audit", auditPath, "--fixtures", t.TempDir())
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0 — a corpus gap is reported, not gated\nstderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "warning:") {
		t.Errorf("no warning in:\n%s", stdout)
	}
	for _, category := range []string{"latent_bug", "client_side_configuration", "business_data_change"} {
		if !strings.Contains(stdout, category) {
			t.Errorf("gap output omits %q\n%s", category, stdout)
		}
	}
	if !strings.Contains(stdout, "GAP") {
		t.Errorf("gap output does not mark the gaps:\n%s", stdout)
	}
}

func TestAuditGapsJSON(t *testing.T) {
	dir := t.TempDir()
	auditPath := writeAudit(t, dir, "synthetic-01", "--aggregate-only")

	shipped := auditJSON(t, "--output", "json", "audit", "coverage", "gaps",
		"--audit", auditPath, "--fixtures", filepath.Join("..", "..", "fixtures", "incidents"))
	if gaps, ok := shipped["gaps"].([]any); !ok || len(gaps) != 0 {
		t.Fatalf("gaps over the shipped corpus = %v, want none", shipped["gaps"])
	}
	covered, ok := shipped["covered"].([]any)
	if !ok || len(covered) == 0 {
		t.Fatalf("covered = %v, want the remainder categories", shipped["covered"])
	}
	if warning, present := shipped["warning"]; present && warning != "" {
		t.Errorf("a corpus with no gaps produced the warning %q", warning)
	}

	empty := auditJSON(t, "--output", "json", "audit", "coverage", "gaps",
		"--audit", auditPath, "--fixtures", t.TempDir())
	if gaps, ok := empty["gaps"].([]any); !ok || len(gaps) != 3 {
		t.Fatalf("gaps over an empty corpus = %v, want 3", empty["gaps"])
	}
	if empty["warning"] == nil || empty["warning"] == "" {
		t.Error("no warning in the machine-readable gap report")
	}
}

func TestAuditGapsNeedsAnAudit(t *testing.T) {
	_, stderr, code := run(t, context.Background(), "audit", "coverage", "gaps")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d\nstderr: %s", code, ExitUsage, stderr)
	}
	if !strings.Contains(stderr, "--audit") {
		t.Errorf("stderr = %q, want it to name the missing flag", stderr)
	}
}

// TestAuditCommandIsRegistered keeps the one line added to root.go honest.
func TestAuditCommandIsRegistered(t *testing.T) {
	root := NewRootCommand()
	for _, cmd := range root.Commands() {
		if cmd.Name() == "audit" {
			return
		}
	}
	t.Error("the audit command is not registered on the root command")
}
