// SPDX-License-Identifier: Apache-2.0

package replay_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	backend "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/internal/investigation/replay"
)

// Trajectory replay (T094, T098; FR-039, FR-040, FR-041, SC-003, SC-018).
//
// Every test here runs against the checked-in corpus with no network, no database and no model.

func corpusWorld(t *testing.T) *backend.Recorded {
	t.Helper()
	world, err := backend.NewRecorded(filepath.Join(fixtureDir, "world"))
	if err != nil {
		t.Fatalf("load the recorded world: %v", err)
	}
	return world
}

func corpusTrajectories(t *testing.T) []string {
	t.Helper()
	files, err := replay.Files(fixtureDir)
	if err != nil {
		t.Fatalf("list trajectories: %v", err)
	}
	if len(files) == 0 {
		t.Skip("the MVP fixture has no recorded trajectory in this checkout")
	}
	return files
}

// TestTheCorpusReplaysIdentically is the gate itself, run as a Go test.
func TestTheCorpusReplaysIdentically(t *testing.T) {
	t.Parallel()
	world := corpusWorld(t)

	for _, path := range corpusTrajectories(t) {
		result, err := replay.ReplayFile(context.Background(), path, world)
		if err != nil {
			t.Fatalf("%s: %v", filepath.Base(path), err)
		}
		if !result.Identical {
			t.Errorf("%s did not replay: %s", filepath.Base(path), result.Divergence.Error())
		}
		if result.Records == 0 {
			t.Errorf("%s replayed 0 records", filepath.Base(path))
		}
	}
}

// TestAModelBearingTrajectoryExercisesTheDigestMatchingPath.
//
// The whole reason the corpus ships a second, fake-model recording: without a `model_request` in
// it, the strict replaying transport is never asked to match anything and the path FR-041 is
// stated over is not covered by the gate at all.
func TestAModelBearingTrajectoryExercisesTheDigestMatchingPath(t *testing.T) {
	t.Parallel()
	world := corpusWorld(t)

	var exchanges int
	for _, path := range corpusTrajectories(t) {
		result, err := replay.ReplayFile(context.Background(), path, world)
		if err != nil {
			t.Fatalf("%s: %v", filepath.Base(path), err)
		}
		exchanges += result.ModelExchanges
	}
	if exchanges == 0 {
		t.Error("no recorded run in the corpus holds a model exchange, so the gate never matches a " +
			"model request digest; record one with `fixture record-trajectory --fake-model`")
	}
}

// TestOneMutatedByteIsADivergenceNamingTheFirstDivergingRecord.
//
// This is the test that says the gate is a gate. Each case changes exactly one thing in a
// recording — a digit of a recorded digest, a character of a recorded parameter, a likelihood
// ratio — and the replay must refuse it, at the record that changed, naming what moved. A gate
// that passed on a corrupted recording would certify nothing.
func TestOneMutatedByteIsADivergenceNamingTheFirstDivergingRecord(t *testing.T) {
	t.Parallel()

	world := corpusWorld(t)
	for _, path := range corpusTrajectories(t) {
		original, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		lines := strings.Split(strings.TrimRight(string(original), "\n"), "\n")

		mutations := map[string]func() (int, string, bool){
			"a worker answer's digest": func() (int, string, bool) {
				return mutateField(lines, "workerResponse", func(record map[string]any) bool {
					body, _ := record["workerResponse"].(map[string]any)
					resp, _ := body["response"].(map[string]any)
					digest, ok := resp["responseDigest"].(string)
					if !ok || digest == "" {
						return false
					}
					resp["responseDigest"] = flipHex(digest)
					return true
				})
			},
			"a worker request's term key": func() (int, string, bool) {
				return mutateField(lines, "workerRequest", func(record map[string]any) bool {
					body, _ := record["workerRequest"].(map[string]any)
					key, ok := body["termKey"].(string)
					if !ok || key == "" {
						return false
					}
					body["termKey"] = flipHex(key)
					return true
				})
			},
			"a recorded likelihood ratio": func() (int, string, bool) {
				return mutateField(lines, "ledgerUpdate", func(record map[string]any) bool {
					body, _ := record["ledgerUpdate"].(map[string]any)
					judgments, _ := body["judgments"].([]any)
					if len(judgments) == 0 {
						return false
					}
					first, _ := judgments[0].(map[string]any)
					first["lnLr"] = 9.5
					return true
				})
			},
			"a model response digest": func() (int, string, bool) {
				return mutateField(lines, "modelResponse", func(record map[string]any) bool {
					body, _ := record["modelResponse"].(map[string]any)
					digest, ok := body["responseDigest"].(string)
					if !ok || digest == "" {
						return false
					}
					body["responseDigest"] = flipHex(digest)
					return true
				})
			},
		}

		for name, mutate := range mutations {
			t.Run(filepath.Base(path)+"/"+name, func(t *testing.T) {
				t.Parallel()
				index, line, ok := mutate()
				if !ok {
					t.Skipf("this recording holds no %s", name)
				}
				mutated := append([]string(nil), lines...)
				mutated[index] = line

				dir := t.TempDir()
				target := filepath.Join(dir, filepath.Base(path))
				if err := os.WriteFile(target,
					[]byte(strings.Join(mutated, "\n")+"\n"), 0o600); err != nil {
					t.Fatalf("write the mutated recording: %v", err)
				}

				traj, readErr := replay.Read(target)
				if readErr != nil {
					// A mutation the reader itself refuses — a record that is no longer its own
					// canonical rendering — is also a refusal, and it is the right one.
					if !strings.Contains(readErr.Error(), "canonical") {
						t.Fatalf("read the mutated recording: %v", readErr)
					}
					return
				}
				result, err := replay.ReplayTrajectory(context.Background(), traj, world)
				if err != nil {
					t.Fatalf("replay: %v", err)
				}
				if result.Identical {
					t.Fatalf("the replay accepted a recording with %s changed at line %d", name, index+1)
				}
				d := result.Divergence
				if d.Seq != uint64(index+1) {
					t.Errorf("the divergence names record %d; the byte changed at record %d", d.Seq, index+1)
				}
				if d.Kind == "" || d.Detail == "" {
					t.Errorf("the divergence names neither a record type nor a detail: %+v", d)
				}
				if strings.HasPrefix(d.Kind, "worker") && d.Worker == "" {
					t.Errorf("a worker divergence that does not name the worker: %s", d.Error())
				}
			})
		}
	}
}

// TestAnEditedRecordingIsRefusedByItsOwnName.
//
// The record-level checks above catch a change to something the algebra keys on. This catches
// everything else — an instant, a rationale, a field nobody hashes — because the file's name is
// the digest of its content, so editing any byte of it renames the file it belongs in.
func TestAnEditedRecordingIsRefusedByItsOwnName(t *testing.T) {
	t.Parallel()
	world := corpusWorld(t)

	for _, path := range corpusTrajectories(t) {
		original, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		lines := strings.Split(strings.TrimRight(string(original), "\n"), "\n")
		index, line, ok := mutateField(lines, "workerRequest", func(record map[string]any) bool {
			body, _ := record["workerRequest"].(map[string]any)
			req, _ := body["request"].(map[string]any)
			if req == nil {
				return false
			}
			// An instant the algebra does not key on: invisible to every record-level check.
			req["observedAt"] = "2026-09-01T14:33:00Z"
			return true
		})
		if !ok {
			t.Skip("this recording holds no worker request")
		}
		mutated := append([]string(nil), lines...)
		mutated[index] = line

		dir := t.TempDir()
		target := filepath.Join(dir, filepath.Base(path))
		if err := os.WriteFile(target, []byte(strings.Join(mutated, "\n")+"\n"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		traj, err := replay.Read(target)
		if err != nil {
			if strings.Contains(err.Error(), "canonical") {
				continue // the reader refused it first, which is also a refusal
			}
			t.Fatalf("read: %v", err)
		}
		result, err := replay.ReplayTrajectory(context.Background(), traj, world)
		if err != nil {
			t.Fatalf("replay: %v", err)
		}
		if result.Identical {
			t.Errorf("%s: an edited recording replayed as identical", filepath.Base(path))
			continue
		}
		if !strings.Contains(result.Divergence.Detail, "edited since it was made") {
			t.Errorf("%s: the divergence does not say the recording was edited: %s",
				filepath.Base(path), result.Divergence.Error())
		}
	}
}

// mutateField finds the first record of a kind, applies a mutation to its decoded form and
// returns the line index and the re-encoded line.
func mutateField(lines []string, kind string, apply func(map[string]any) bool) (int, string, bool) {
	for i, line := range lines {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			continue
		}
		if _, ok := record[kind]; !ok {
			continue
		}
		if !apply(record) {
			continue
		}
		encoded, err := json.Marshal(record)
		if err != nil {
			return 0, "", false
		}
		return i, string(encoded), true
	}
	return 0, "", false
}

// flipHex changes exactly one hex character of a digest.
func flipHex(digest string) string {
	out := []byte(digest)
	if out[0] == '0' {
		out[0] = '1'
	} else {
		out[0] = '0'
	}
	return string(out)
}

// TestReplayOfADirectoryReportsTheMissRate: the published response a caller gets.
func TestReplayOfADirectoryReportsTheMissRate(t *testing.T) {
	t.Parallel()
	corpusTrajectories(t)

	resp, err := replay.Replay(context.Background(), fixtureDir, replay.ReplayOptions{
		Layer: replay.LayerTrajectory,
	})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !resp.GetIdentical() {
		t.Fatalf("the corpus did not replay: %s", resp.GetFirstDivergingRecord())
	}
	if resp.GetMissRate() < 0 || resp.GetMissRate() > 1 {
		t.Errorf("miss rate %v is not a rate", resp.GetMissRate())
	}
}

// TestTheWorldLayerReportsNotRecordedWithoutRunningAModel: `--layer world` with no engine builder
// re-issues the recorded worker requests against the recorded world and reports the miss rate. It
// is the number that gates a *fixture's* admission to the corpus, never the engine's score
// (FR-042c).
func TestTheWorldLayerReportsNotRecordedWithoutRunningAModel(t *testing.T) {
	t.Parallel()
	corpusTrajectories(t)

	report := filepath.Join(t.TempDir(), "replay.json")
	resp, err := replay.Replay(context.Background(), fixtureDir, replay.ReplayOptions{
		Layer: replay.LayerWorld, ReportJSON: report,
	})
	if err != nil {
		t.Fatalf("world replay: %v", err)
	}
	body, err := os.ReadFile(report)
	if err != nil {
		t.Fatalf("the report was not written: %v", err)
	}
	if !strings.Contains(string(body), "missRate") && !strings.Contains(string(body), "notRecordedCount") {
		t.Errorf("the report carries neither the miss rate nor the not_recorded count:\n%s", body)
	}
	if resp.GetNotRecordedCount() == 0 && resp.GetMissRate() != 0 {
		t.Errorf("not_recorded is 0 but the miss rate is %v", resp.GetMissRate())
	}
}

// TestAnUnknownLayerIsRefused: the layer is a closed set of two.
func TestAnUnknownLayerIsRefused(t *testing.T) {
	t.Parallel()
	if _, err := replay.ParseLayer("ledger"); err == nil {
		t.Error("ParseLayer accepted a layer that does not exist")
	}
}
