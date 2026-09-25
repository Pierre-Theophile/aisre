// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	algebra "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/internal/investigation/replay"
	sdk "github.com/Pierre-Theophile/aisre/pkg/backend"
)

// The trajectory gate from the command line (T094, T096, T098).
//
// These are the assertions the CI job depends on, made here rather than in YAML: a gate whose
// exit code is only checked by a workflow file is a gate nobody can run locally.
//
// Nothing here opens a database or a socket. `fixture verify --trajectory-only` and
// `investigate replay --from` are both local operations over a directory, which is exactly why
// the gate can run in a job with no Postgres and no egress.

const incidentFixture = "../../fixtures/incidents/rollout-regression-01-incident"

// TestTrajectoryOnlyVerificationNeedsNoDatabase: the gate's happy path, and the property that
// makes it runnable in the locked-down job — it is invoked with no --db and no $PG_DSN read.
func TestTrajectoryOnlyVerificationNeedsNoDatabase(t *testing.T) {
	t.Setenv(EnvDSN, "")

	stdout, stderr, code := run(t, context.Background(),
		"fixture", "verify", "--trajectory-only", incidentFixture)
	if code != ExitOK {
		t.Fatalf("exit %d, want 0\nstdout %s\nstderr %s", code, stdout, stderr)
	}
	for _, want := range []string{"trajectory-replay", "identical"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the output does not mention %q:\n%s", want, stdout)
		}
	}
}

// TestAMutatedRecordingExitsFourAndNamesTheFirstDivergingRecord.
//
// The one-byte test, at the level an operator and a CI job see: exit code 4, and the message
// names the record that moved. Exit 4 is the published verification code (contracts/cli.md
// §Exit codes) — a replay that does not reproduce its recording is a failed check, not a
// transport problem.
func TestAMutatedRecordingExitsFourAndNamesTheFirstDivergingRecord(t *testing.T) {
	t.Setenv(EnvDSN, "")

	staged := stageFixtureWithMutatedTrajectory(t)
	stdout, stderr, code := run(t, context.Background(),
		"fixture", "verify", "--trajectory-only", staged)
	if code != ExitVerification {
		t.Fatalf("exit %d, want %d on a mutated recording\nstdout %s\nstderr %s",
			code, ExitVerification, stdout, stderr)
	}
	if !strings.Contains(stdout, "diverged at record") {
		t.Errorf("the failure does not name the diverging record:\n%s", stdout)
	}
}

// TestInvestigateReplayExitsFourOnADivergence: the same one-byte mutation through
// `investigate replay --from`, which is the command the gate and an operator both use.
func TestInvestigateReplayExitsFourOnADivergence(t *testing.T) {
	t.Setenv(EnvDSN, "")

	staged := stageFixtureWithMutatedTrajectory(t)
	stdout, stderr, code := run(t, context.Background(),
		"investigate", "replay", "--from", staged, "--layer", "trajectory")
	if code != ExitVerification {
		t.Fatalf("exit %d, want %d\nstdout %s\nstderr %s", code, ExitVerification, stdout, stderr)
	}
	if !strings.Contains(stderr, "diverged at") && !strings.Contains(stdout, "diverged at") {
		t.Errorf("the failure does not say where it diverged\nstdout %s\nstderr %s", stdout, stderr)
	}
}

// TestInvestigateReplayIsLocalAndExitsZeroOnTheCorpus.
func TestInvestigateReplayIsLocalAndExitsZeroOnTheCorpus(t *testing.T) {
	t.Setenv(EnvDSN, "")

	report := filepath.Join(t.TempDir(), "replay.json")
	stdout, stderr, code := run(t, context.Background(),
		"investigate", "replay", "--from", incidentFixture, "--report-json", report)
	if code != ExitOK {
		t.Fatalf("exit %d, want 0\nstdout %s\nstderr %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "identical=true") {
		t.Errorf("the output does not report identical=true:\n%s", stdout)
	}
	if _, err := os.Stat(report); err != nil {
		t.Errorf("--report-json wrote nothing: %v", err)
	}
}

// TestCalibrationRunsOverTheCorpusWithNoModelAndNoDatabase.
func TestCalibrationRunsOverTheCorpusWithNoModelAndNoDatabase(t *testing.T) {
	t.Setenv(EnvDSN, "")

	dir := t.TempDir()
	out := filepath.Join(dir, "calibration.json")
	summary := filepath.Join(dir, "calibration.md")
	stdout, stderr, code := run(t, context.Background(),
		"fixture", "calibration", "--fixtures", "../../fixtures/incidents",
		"--out", out, "--summary", summary)
	if code != ExitOK {
		t.Fatalf("exit %d, want 0\nstdout %s\nstderr %s", code, stdout, stderr)
	}
	for _, path := range []string{out, summary} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s was not written: %v", path, err)
		}
	}
	if !strings.Contains(stdout, "reliability table") {
		t.Errorf("the summary is not the reliability table:\n%s", stdout)
	}
}

// stageFixtureWithMutatedTrajectory copies the manifest, the world and the trajectories of the
// MVP fixture into a temporary directory and changes one recorded digest.
//
// It copies rather than editing in place for the obvious reason, and it copies the *whole*
// fixture rather than only the trajectory because the gate checks the recording against the
// recorded world beside it.
func stageFixtureWithMutatedTrajectory(t *testing.T) string {
	t.Helper()
	staged := t.TempDir()

	copyTree(t, incidentFixture, staged, func(rel string) bool {
		return rel == "manifest.yaml" ||
			strings.HasPrefix(rel, "world"+string(filepath.Separator)) ||
			strings.HasPrefix(rel, "trajectories"+string(filepath.Separator)) ||
			rel == "events.jsonl"
	})

	entries, err := os.ReadDir(filepath.Join(staged, "trajectories"))
	if err != nil {
		t.Fatalf("the staged fixture has no trajectories: %v", err)
	}
	if len(entries) == 0 {
		t.Skip("the MVP fixture has no recorded trajectory in this checkout")
	}
	path := filepath.Join(staged, "trajectories", entries[0].Name())
	raw, err := os.ReadFile(path) //nolint:gosec // the test's own temp dir
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	changed := false
	for i, line := range lines {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			continue
		}
		body, ok := record["workerResponse"].(map[string]any)
		if !ok {
			continue
		}
		resp, ok := body["response"].(map[string]any)
		if !ok {
			continue
		}
		digest, ok := resp["responseDigest"].(string)
		if !ok || digest == "" {
			continue
		}
		if digest[0] == '0' {
			resp["responseDigest"] = "1" + digest[1:]
		} else {
			resp["responseDigest"] = "0" + digest[1:]
		}
		encoded, err := json.Marshal(record)
		if err != nil {
			t.Fatalf("re-encode: %v", err)
		}
		lines[i] = string(encoded)
		changed = true
		break
	}
	if !changed {
		t.Skip("the recording holds no worker answer to mutate")
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write the mutated recording: %v", err)
	}
	return staged
}

// copyTree copies the files of src into dst for which keep returns true.
func copyTree(t *testing.T, src, dst string, keep func(rel string) bool) {
	t.Helper()
	err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if !keep(rel) {
			return nil
		}
		body, err := os.ReadFile(path) //nolint:gosec // inside the fixture the test named
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err
		}
		return os.WriteFile(target, body, 0o600)
	})
	if err != nil {
		t.Fatalf("stage %s: %v", src, err)
	}
}

// TestTheRecordedWorldIsASupersetOfWhatTheEngineAsks (Phase 7 Track F; FR-042b, FR-042c, SC-003).
//
// The property `fixture record-world` now holds by construction, asserted against the shipped
// corpus so that it stays true: **every telemetry term the deterministic engine issued is in the
// recorded world**, keyed the same way.
//
// It is worth an assertion of its own rather than leaning on the miss rate the verify step
// prints, because the two fail differently. A miss rate is computed over the calls a *recorded
// run* made, so a recording made before a regression and a world recorded after it agree with
// each other and say nothing; this reads the world's index directly and compares it against the
// term keys in the trajectory, which is the containment the recorder claims.
//
// The failure it exists to catch is the one Phase 7 opened with: the engine derived its onset
// search window from the question and the recorder derived its from the manifest's grid, the two
// never intersected, and the gate reported `15 not_recorded of 15 checked`. Nothing in either
// file was obviously wrong — each derivation was defensible on its own — which is exactly why
// the property has to be checked rather than reasoned about.
//
// **Scope, after Track L.** The claim is about the *deterministic* wiring, because that is the
// wiring `fixture record-world` reproduces: its engine pass runs `--model-free`, and the manifest
// grid supplements it. A recording with a **live model** in the loop is a different animal — the
// model asks questions the deterministic plan never derives, which is what a live investigator is
// for, and the world is a recording, so it can always be reached outside. Containment is
// therefore asserted strictly on every trajectory that holds no model record, and the weaker but
// still load-bearing property is asserted on the rest: a term the world does not hold must come
// back as a **typed `not_recorded` recorded in the trajectory**, never as a silence (FR-027).
//
// Chasing strict containment for a live run was tried and rejected. Widening the fixture's grid
// until the world held what one live run asked made the *next* term the model reached for the new
// gap, grew the world from 2.5 MB to 6.0 MB, and — because the trajectory had recorded those
// terms as `not_recorded` — put the recording and the world into direct disagreement. A fixed
// point there is not reachable by widening, and it is not what the guard is for.
func TestTheRecordedWorldIsASupersetOfWhatTheEngineAsks(t *testing.T) {
	t.Setenv(EnvDSN, "")

	world, err := sdk.LoadWorld(filepath.Join(incidentFixture, "world"))
	if err != nil {
		t.Fatalf("load the recorded world: %v", err)
	}
	held := make(map[string]struct{}, len(world.Keys()))
	for _, key := range world.Keys() {
		held[key] = struct{}{}
	}

	paths, err := replay.Files(incidentFixture)
	if err != nil {
		t.Fatalf("list the recorded trajectories: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("the fixture ships no recorded trajectory; there is nothing to check containment against")
	}

	var checked, deterministic int
	for _, path := range paths {
		traj, err := replay.Read(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		live := holdsModelRecord(traj)
		if !live {
			deterministic++
		}

		// A worker request is followed by its response, so the outcome that answered a term is
		// read off the record after it.
		var pending *investigationv1.AlgebraRequest
		for _, record := range traj.Records {
			switch body := record.GetRecord().(type) {
			case *investigationv1.TrajectoryRecord_WorkerRequest:
				pending = body.WorkerRequest.GetRequest()
			case *investigationv1.TrajectoryRecord_WorkerResponse:
				term := pending.GetTerm()
				outcome := body.WorkerResponse.GetResponse().GetOutcome()
				pending = nil
				// The graph family is answered by replaying events.jsonl and is never recorded
				// into a world, so it is not part of the containment claim.
				if term == nil || sdk.FamilyOf(algebra.TermName(term)) != sdk.FamilyTelemetry {
					continue
				}
				key, err := algebra.TermKey(term)
				if err != nil {
					t.Fatalf("key the recorded term: %v", err)
				}
				checked++
				if _, ok := held[key]; ok {
					continue
				}
				if !live {
					t.Errorf("%s: the deterministic engine issued %s (%s) and the recorded world "+
						"does not hold it; `fixture record-world` runs that same engine and records "+
						"what it asks, so a term outside the world means the two wirings have "+
						"drifted apart",
						filepath.Base(path), algebra.TermName(term), key)
					continue
				}
				// A live run may reach outside the world. What it may never do is reach outside
				// it silently: the miss has to be an answer with a reason (FR-027).
				if outcome != investigationv1.TermOutcome_NOT_RECORDED {
					t.Errorf("%s: a live run issued %s (%s), the recorded world does not hold it, "+
						"and it came back as %s rather than not_recorded; a term the recording "+
						"cannot answer must be answered with a reason, never a silence",
						filepath.Base(path), algebra.TermName(term), key, outcome)
				}
			}
		}
	}
	if checked == 0 {
		t.Error("no telemetry term was checked; the recorded runs asked the world nothing")
	}
	if deterministic == 0 {
		t.Error("no model-free trajectory was checked; the containment claim is about that wiring, " +
			"so a corpus with none of them asserts nothing")
	}
}

// holdsModelRecord reports whether a trajectory had a model in the loop. A recording with none is
// the deterministic engine end to end, and it is the one `fixture record-world` reproduces.
func holdsModelRecord(traj *replay.Trajectory) bool {
	for _, record := range traj.Records {
		if _, ok := record.GetRecord().(*investigationv1.TrajectoryRecord_ModelRequest); ok {
			return true
		}
	}
	return false
}
