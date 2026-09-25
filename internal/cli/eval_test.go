// SPDX-License-Identifier: Apache-2.0

package cli_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// The gate, tested as the thing CI actually runs: `scripts/check-report.sh --investigation` over
// a rows file with a known answer, asserting the exit code and the lines it printed (T112).
//
// It is tested through the shell rather than by re-implementing the comparisons in Go, because
// the shell script *is* the gate: a Go test that agreed with a Go re-implementation of it would
// pass on the day somebody broke the jq.

// repoRoot is the checkout, two directories up from internal/cli.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

var (
	buildOnce sync.Once
	builtBin  string
	buildErr  error
)

// gateBinary builds `sre-agent` once for the whole test binary, so the gate's detection-power
// lines come from the real `eval power` rather than from a stub. A gate whose power statement
// was faked in its own test would be a gate with no power statement.
func gateBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		root := repoRoot(t)
		if info, err := os.Stat(filepath.Join(root, "bin", "aisre")); err == nil && !info.IsDir() {
			builtBin = filepath.Join(root, "bin", "aisre")
			return
		}
		out := filepath.Join(t.TempDir(), "aisre")
		cmd := exec.Command("go", "build", "-o", out, "./cmd/aisre")
		cmd.Dir = root
		if output, err := cmd.CombinedOutput(); err != nil {
			buildErr = err
			t.Logf("go build: %s", output)
			return
		}
		builtBin = out
	})
	if buildErr != nil {
		t.Fatalf("could not build sre-agent for the gate test: %v", buildErr)
	}
	return builtBin
}

// rowsFile writes a JSONL rows file from a list of {metric, scope, value} triples plus whatever
// else the test wants.
func rowsFile(t *testing.T, rows []map[string]any) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rows.jsonl")
	var out strings.Builder
	for _, row := range rows {
		if _, ok := row["scope"]; !ok {
			row["scope"] = "corpus"
		}
		encoded, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		out.Write(encoded)
		out.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(out.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// greenRows are a run where every gate in force holds.
func greenRows() []map[string]any {
	return []map[string]any{
		{"metric": "corpus", "value": nil, "n": 0, "detail": "public corpus, read from fixtures/incidents"},
		{"metric": "model_configuration", "value": nil, "n": 0, "detail": "MODEL-FREE"},
		{"metric": "fixtures", "value": 13, "n": 13},
		{"metric": "trials", "value": 39, "n": 39},
		{"metric": "pass_at_1", "value": 0.5, "n": 39},
		{"metric": "lift_over_prior", "value": 0.2, "n": 39},
		{"metric": "harm_rate", "value": 0.02, "n": 39},
		{"metric": "confidently_wrong", "value": 0.0, "n": 39},
		{"metric": "citation_validity", "value": 1.0, "n": 120},
		{"metric": "untraceable_conclusions", "value": 0, "n": 120},
		{"metric": "improvised_replays", "value": 0, "n": 39},
		{"metric": "replay_divergence_rate", "value": 0, "n": 39},
		{"metric": "metamorphic_verdict_changes", "value": 0, "n": 8},
		{"metric": "human_label_regressions", "value": 0, "n": 0},
		{"metric": "best_of_k", "value": 1, "n": 39, "detail": "reported, NEVER gated (FR-060)"},
		{"metric": "corpus_gaps", "value": 2, "n": 4, "detail": "corpus gap: 2 of 4 categories have no fixture"},
	}
}

// withMetric returns the rows with one metric's value replaced.
func withMetric(rows []map[string]any, metric string, value any) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		copied := map[string]any{}
		for key, each := range row {
			copied[key] = each
		}
		if copied["metric"] == metric {
			copied["value"] = value
		}
		out = append(out, copied)
	}
	return out
}

// runGate runs the gate over a rows file with a thresholds file of the test's choosing.
func runGate(t *testing.T, rows []map[string]any, thresholds string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("the investigation gate needs jq")
	}
	root := repoRoot(t)
	path := rowsFile(t, rows)

	argv := append([]string{"scripts/check-report.sh", "--investigation"}, args...)
	argv = append(argv, path)
	cmd := exec.Command("bash", argv...) //nolint:gosec // a fixed script in this checkout.
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"INVESTIGATION_THRESHOLDS="+thresholds,
		"SRE_AGENT_BIN="+gateBinary(t))
	var out, errOut strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err := cmd.Run()
	code = 0
	var exitErr *exec.ExitError
	if err != nil {
		if !asExitError(err, &exitErr) {
			t.Fatalf("running the gate: %v", err)
		}
		code = exitErr.ExitCode()
	}
	return out.String(), errOut.String(), code
}

func asExitError(err error, target **exec.ExitError) bool {
	exitErr, ok := err.(*exec.ExitError) //nolint:errorlint // the only wrapping here is ours.
	if ok {
		*target = exitErr
	}
	return ok
}

// thresholdsFile writes a thresholds document with the given published pass@1 (nil for unset).
func thresholdsFile(t *testing.T, passAt1 any) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "thresholds.json")
	doc := map[string]any{
		"pass_at_1": passAt1, "set_by_run": nil, "coverage_ceiling": 0.615385,
		"harm_rate_max": 0.05, "confidently_wrong_max": 0.05, "citation_validity_min": 1.0,
	}
	encoded, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestAnUnsetThresholdIsPrintedAsUnsetAndDoesNotPassSilently is FR-060's central instruction.
func TestAnUnsetThresholdIsPrintedAsUnsetAndDoesNotPassSilently(t *testing.T) {
	stdout, _, code := runGate(t, greenRows(), thresholdsFile(t, nil))

	if code != 0 {
		t.Fatalf("a run with every gate in force holding exited %d:\n%s", code, stdout)
	}
	if !strings.Contains(stdout, "gate pass_at_1 value=0.5 threshold=unset status=unset") {
		t.Fatalf("the machine-readable line does not say the threshold is unset:\n%s", stdout)
	}
	if !strings.Contains(stdout, "UNSET") {
		t.Fatalf("the table does not print the threshold as unset:\n%s", stdout)
	}
	if !strings.Contains(stdout, "::warning title=Investigation pass@1 threshold unset::") {
		t.Fatalf("no GitHub annotation: a collapsed job would show nothing:\n%s", stdout)
	}
	// It must say, in words, that it did not gate — not merely exit 0.
	if !strings.Contains(stdout, "UNSET and did not gate this build") {
		t.Fatalf("the closing line reads as an ordinary pass:\n%s", stdout)
	}
}

// TestEveryOtherGateIsInForceFromTheFirstRun.
func TestEveryOtherGateIsInForceFromTheFirstRun(t *testing.T) {
	cases := []struct {
		name, metric string
		value        any
		want         string
	}{
		{"lift at zero", "lift_over_prior", 0, "did not beat the deterministic ranker"},
		{"lift below zero", "lift_over_prior", -0.1, "did not beat the deterministic ranker"},
		{"harm above five percent", "harm_rate", 0.06, "harm rate = 0.06"},
		{"confidently wrong above five percent", "confidently_wrong", 0.2, "confidently wrong = 0.2"},
		{"citation validity below one", "citation_validity", 0.999, "citation validity = 0.999"},
		{"an untraceable conclusion", "untraceable_conclusions", 1, "untraceable conclusions = 1"},
		{"an improvised replay", "improvised_replays", 1, "improvised replays = 1"},
		{"a replay divergence", "replay_divergence_rate", 0.02, "replay divergence rate = 0.02"},
		{"a metamorphic verdict change", "metamorphic_verdict_changes", 1, "metamorphic verdict changes = 1"},
		{"a human-label regression", "human_label_regressions", 1, "human-label regressions = 1"},
	}

	for _, each := range cases {
		t.Run(each.name, func(t *testing.T) {
			_, stderr, code := runGate(t,
				withMetric(greenRows(), each.metric, each.value), thresholdsFile(t, nil))
			if code != 1 {
				t.Fatalf("%s exited %d, want 1; the gate is in force from the first run", each.name, code)
			}
			if !strings.Contains(stderr, each.want) {
				t.Fatalf("the failure does not name what went wrong (%q):\n%s", each.want, stderr)
			}
		})
	}
}

// TestBestOfKIsPrintedAndNeverGated (FR-060's one absolute).
func TestBestOfKIsPrintedAndNeverGated(t *testing.T) {
	// best_of_k is 0 — the worst it can be — and every other gate holds.
	stdout, _, code := runGate(t, withMetric(greenRows(), "best_of_k", 0), thresholdsFile(t, nil))
	if code != 0 {
		t.Fatalf("best-of-k moved the exit code to %d; it must never be gated on:\n%s", code, stdout)
	}
	if !strings.Contains(stdout, "NEVER gated") {
		t.Fatalf("best_of_k is printed without the sentence that protects it:\n%s", stdout)
	}
}

// TestAPublishedThresholdGatesAndPrintsItsDetectionPower.
func TestAPublishedThresholdGatesAndPrintsItsDetectionPower(t *testing.T) {
	thresholds := thresholdsFile(t, 0.6)

	_, stderr, code := runGate(t, greenRows(), thresholds)
	if code != 1 {
		t.Fatalf("pass@1 of 0.5 against a published 0.6 exited %d, want 1", code)
	}
	if !strings.Contains(stderr, "aggregate pass@1 = 0.5, want min 0.6") {
		t.Fatalf("the failure does not name the threshold it missed:\n%s", stderr)
	}

	stdout, _, code := runGate(t, withMetric(greenRows(), "pass_at_1", 0.61), thresholds)
	if code != 0 {
		t.Fatalf("pass@1 of 0.61 against 0.6 exited %d, want 0:\n%s", code, stdout)
	}
	// Every gate prints what it can actually detect at this n, with its method.
	if !strings.Contains(stdout, "normal approximation to the binomial") {
		t.Fatalf("a threshold was printed with no stated method:\n%s", stdout)
	}
	if !strings.Contains(stdout, "Bernoulli trials detects a drop from 60 %") {
		t.Fatalf("the pass@1 gate printed no detection power:\n%s", stdout)
	}
	// A max gate's regression is a RISE, and its statement has to say so or it is nonsense.
	if !strings.Contains(stdout, "detects a rise from 5.0 %") {
		t.Fatalf("the harm-rate gate's detection power is stated in the wrong direction:\n%s", stdout)
	}
}

// TestAThresholdAboveTheCoverageCeilingIsRefused (FR-071).
func TestAThresholdAboveTheCoverageCeilingIsRefused(t *testing.T) {
	// The published audit's ceiling is 0.615385; 0.9 is above it.
	_, stderr, code := runGate(t, withMetric(greenRows(), "pass_at_1", 0.95), thresholdsFile(t, 0.9))
	if code != 1 {
		t.Fatalf("a threshold above the coverage ceiling exited %d, want 1", code)
	}
	if !strings.Contains(stderr, "exceeds the coverage ceiling") {
		t.Fatalf("the failure does not cite the ceiling:\n%s", stderr)
	}
}

// TestSetThresholdPublishesTheRunsOwnPassAtOneAndRefusesToDoItTwice.
func TestSetThresholdPublishesTheRunsOwnPassAtOneAndRefusesToDoItTwice(t *testing.T) {
	thresholds := thresholdsFile(t, nil)

	stdout, stderr, code := runGate(t, greenRows(), thresholds, "--set-threshold")
	if code != 0 {
		t.Fatalf("--set-threshold exited %d: %s\n%s", code, stderr, stdout)
	}
	if !strings.Contains(stdout, "published pass_at_1 = 0.5") {
		t.Fatalf("--set-threshold did not publish the run's own pass@1:\n%s", stdout)
	}

	raw, err := os.ReadFile(thresholds) //nolint:gosec // a path this test made.
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["pass_at_1"] != 0.5 {
		t.Fatalf("thresholds.json holds pass_at_1 = %v, want 0.5", doc["pass_at_1"])
	}
	if doc["set_by_run"] == nil {
		t.Fatal("the threshold was published without recording which run set it (FR-060)")
	}

	// It is the FIRST full corpus run that sets it, and only that one.
	_, stderr, code = runGate(t, greenRows(), thresholds, "--set-threshold")
	if code != 1 {
		t.Fatalf("re-setting a published threshold exited %d, want 1", code)
	}
	if !strings.Contains(stderr, "already published") {
		t.Fatalf("the refusal does not say the threshold is already published:\n%s", stderr)
	}
}

// TestAGateWithNoRowsIsNotAGateThatPassed.
func TestAGateWithNoRowsIsNotAGateThatPassed(t *testing.T) {
	_, stderr, code := runGate(t, nil, thresholdsFile(t, nil))
	if code != 1 {
		t.Fatalf("an empty rows file exited %d, want 1", code)
	}
	if !strings.Contains(stderr, "no investigation report rows") {
		t.Fatalf("the failure does not say the report was empty:\n%s", stderr)
	}
}

// TestAnExcludedFixtureIsListedAndNeverFailsTheBuild: the miss rate gates the fixture's
// admission, never the engine's score (FR-060).
func TestAnExcludedFixtureIsListedAndNeverFailsTheBuild(t *testing.T) {
	rows := append(greenRows(), map[string]any{
		"metric": "fixture_admitted", "scope": "fixture", "fixture": "thin-01",
		"value": 0, "n": 3, "detail": "EXCLUDED: miss rate 0.2000 > 0.05",
	})
	stdout, _, code := runGate(t, rows, thresholdsFile(t, nil))
	if code != 0 {
		t.Fatalf("an excluded fixture failed the build (exit %d); it gates its own admission only:\n%s",
			code, stdout)
	}
	if !strings.Contains(stdout, "thin-01") || !strings.Contains(stdout, "EXCLUDED") {
		t.Fatalf("the excluded fixture is not listed by name:\n%s", stdout)
	}
}

// TestTheCorpusGapLineIsPrintedAndNeverGates (FR-071b).
func TestTheCorpusGapLineIsPrintedAndNeverGates(t *testing.T) {
	stdout, _, code := runGate(t, withMetric(greenRows(), "corpus_gaps", 4), thresholdsFile(t, nil))
	if code != 0 {
		t.Fatalf("corpus gaps failed the build (exit %d); an incomplete corpus is a fact, not a "+
			"regression:\n%s", code, stdout)
	}
	if !strings.Contains(stdout, "corpus gaps: 4") {
		t.Fatalf("the corpus-gap line was not printed:\n%s", stdout)
	}
}

// TestTwoCorpusRowsForOneMetricIsRefused is the gate refusing a file it cannot read safely.
//
// Every lookup in the script ends in `head -1`, which is right for a well-formed report — one
// corpus row per metric, by construction — and silently wrong for two reports catted together:
// a red run pasted after a green one would be gated on the green one, and the table would print
// the other run's n beside it. There is no reading of such a file that is safe to guess at, so
// the gate refuses it and names the metric.
func TestTwoCorpusRowsForOneMetricIsRefused(t *testing.T) {
	rows := append(greenRows(), map[string]any{
		"metric": "pass_at_1", "value": 0.1, "n": 39,
	})

	stdout, stderr, code := runGate(t, rows, thresholdsFile(t, nil))

	if code == 0 {
		t.Fatalf("a rows file with two corpus pass_at_1 rows passed the gate:\n%s", stdout)
	}
	if !strings.Contains(stderr, "pass_at_1") {
		t.Errorf("the refusal does not name the duplicated metric:\n%s", stderr)
	}
	if !strings.Contains(stderr, "concatenated reports are not a report") {
		t.Errorf("the refusal does not say what to do about it:\n%s", stderr)
	}
	if !strings.Contains(stdout, "::error title=Duplicate corpus rows::") {
		t.Errorf("no GitHub annotation: a collapsed job would show nothing:\n%s", stdout)
	}
	// A fixture-scoped metric appearing once per fixture is the ordinary shape and must still
	// pass: the rule is one row per *corpus* metric, not one row per metric.
	fixtureRows := append(greenRows(),
		map[string]any{"metric": "pass_at_1", "scope": "fixture", "fixture": "a", "value": 1, "n": 3},
		map[string]any{"metric": "pass_at_1", "scope": "fixture", "fixture": "b", "value": 0, "n": 3})
	if _, _, code := runGate(t, fixtureRows, thresholdsFile(t, nil)); code != 0 {
		t.Errorf("per-fixture rows for one metric were refused; only corpus rows must be unique")
	}
}
