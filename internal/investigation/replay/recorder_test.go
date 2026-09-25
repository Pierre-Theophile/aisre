// SPDX-License-Identifier: Apache-2.0

package replay_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/investigation/engine"
	"github.com/Pierre-Theophile/aisre/internal/investigation/ledger"
	"github.com/Pierre-Theophile/aisre/internal/investigation/model"
	"github.com/Pierre-Theophile/aisre/internal/investigation/replay"
)

// The on-disk trajectory format (T093).
//
// Nothing here touches a network, a database or a model. That is the point: the format is
// supposed to be readable and checkable by anything that can open a file.

const fixtureDir = "../../../fixtures/incidents/rollout-regression-01-incident"

// pinned is the constant clock a recording is made under.
var pinned = time.Date(2026, 9, 1, 14, 32, 0, 0, time.UTC)

// sample builds a small in-memory trajectory through the engine's own recorder, so the test
// exercises the same writer a run does rather than a hand-built approximation.
func sample(t *testing.T) *engine.Trajectory {
	t.Helper()
	traj := engine.NewTrajectory(func() time.Time { return pinned })

	if err := traj.ModelRequest(model.RoleInvestigator, "claude-fable-5-1",
		json.RawMessage(`{"model":"claude-fable-5-1","messages":[{"role":"user","content":"hello"}]}`),
		"aaaa"); err != nil {
		t.Fatalf("model request: %v", err)
	}
	if err := traj.ModelResponse(&model.Response{
		ResponseBody:   json.RawMessage(`{"id":"msg_1","stop_reason":"end_turn"}`),
		ResponseDigest: "bbbb",
		StopReason:     "end_turn",
	}); err != nil {
		t.Fatalf("model response: %v", err)
	}
	traj.LedgerUpdate([]ledger.Judgment{{
		ID: "j-1", HypothesisID: "h-1", EvidenceID: "e-1",
		Direction: ledger.Supports, Strength: ledger.Moderate,
		LnLR: mustLnLR(t, ledger.Supports, ledger.Moderate), Source: ledger.SourceFirstWave,
		RecordedAt: pinned,
	}}, "cccc")
	traj.Stop(engine.Stop{Reason: engine.StopCompleted, Detail: "done"}, nil)
	return traj
}

func mustLnLR(t *testing.T, d ledger.Direction, s ledger.Strength) float64 {
	t.Helper()
	ln, err := ledger.LnLR(d, s)
	if err != nil {
		t.Fatalf("ln LR: %v", err)
	}
	return ln
}

// TestAWrittenTrajectoryReadsBackIdentically is the round trip: write, read, and the records,
// the digest and the bytes all agree.
func TestAWrittenTrajectoryReadsBackIdentically(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	written, err := replay.Write(dir, sample(t))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if written.Records != 4 {
		t.Errorf("wrote %d records, want 4", written.Records)
	}
	if !strings.HasPrefix(filepath.Base(written.Path), "run-") {
		t.Errorf("the file is called %s; a run id is content-derived and prefixed run-",
			filepath.Base(written.Path))
	}

	back, err := replay.Read(written.Path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if back.Digest != written.Digest {
		t.Errorf("digest %s on write, %s on read", written.Digest, back.Digest)
	}
	if back.Len() != written.Records {
		t.Errorf("read %d records, wrote %d", back.Len(), written.Records)
	}
}

// TestRewritingAnUnchangedTrajectoryIsAByteIdenticalNoOp is the property the whole naming scheme
// exists for: a re-record of an unchanged run writes the same bytes to the same path, so a
// re-record is a `git status` a reviewer can read rather than a diff nobody reads.
func TestRewritingAnUnchangedTrajectoryIsAByteIdenticalNoOp(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	first, err := replay.Write(dir, sample(t))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	before, err := os.ReadFile(first.Path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}

	second, err := replay.Write(dir, sample(t))
	if err != nil {
		t.Fatalf("re-write: %v", err)
	}
	if !second.Unchanged {
		t.Error("a re-record of an unchanged run reported itself as changed")
	}
	if second.Path != first.Path || second.Digest != first.Digest {
		t.Errorf("the re-record landed at %s (%s), the first at %s (%s)",
			second.Path, second.Digest, first.Path, first.Digest)
	}
	after, err := os.ReadFile(second.Path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(before) != string(after) {
		t.Error("the bytes changed between two recordings of the same run")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("the directory holds %d files after two identical recordings; a content-derived "+
			"run id means there is exactly one", len(entries))
	}
}

// TestTheReaderRefusesARecordingItCannotTrust: the four rules a divergence report depends on.
func TestTheReaderRefusesARecordingItCannotTrust(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		records []*investigationv1.TrajectoryRecord
		want    string
	}{
		"empty": {records: nil, want: "holds no records"},
		"out of sequence": {
			records: []*investigationv1.TrajectoryRecord{
				record(2, stopRecord()),
			},
			want: "1..N in order",
		},
		"no stop": {
			records: []*investigationv1.TrajectoryRecord{
				record(1, func(r *investigationv1.TrajectoryRecord) {
					r.Record = &investigationv1.TrajectoryRecord_LedgerUpdate{
						LedgerUpdate: &investigationv1.LedgerUpdateRecord{LedgerDigest: "x"},
					}
				}),
			},
			want: "0 stop records",
		},
		"truncated model exchange": {
			records: []*investigationv1.TrajectoryRecord{
				record(1, func(r *investigationv1.TrajectoryRecord) {
					r.Record = &investigationv1.TrajectoryRecord_ModelRequest{
						ModelRequest: &investigationv1.ModelRequestRecord{RequestDigest: "a"},
					}
				}),
				record(2, stopRecord()),
			},
			want: "never answered",
		},
		"untyped record": {
			records: []*investigationv1.TrajectoryRecord{
				record(1, nil),
				record(2, stopRecord()),
			},
			want: "none of the seven published record types",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := replay.Validate(tc.records)
			if err == nil {
				t.Fatalf("Validate accepted %s", name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Validate said %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// record builds one record. The oneof wrapper types are unexported interfaces in the generated
// code, so the body is set on a fresh message rather than passed as a value.
func record(seq uint64, apply func(*investigationv1.TrajectoryRecord)) *investigationv1.TrajectoryRecord {
	out := &investigationv1.TrajectoryRecord{Seq: seq, At: timestamppb.New(pinned)}
	if apply != nil {
		apply(out)
	}
	return out
}

func stopRecord() func(*investigationv1.TrajectoryRecord) {
	return func(r *investigationv1.TrajectoryRecord) {
		r.Record = &investigationv1.TrajectoryRecord_Stop{
			Stop: &investigationv1.StopRecord{Reason: investigationv1.StopReason_COMPLETED},
		}
	}
}

// TestTheOnDiskReaderAndTheInMemoryRecorderAgreeOnExchanges: `replay.Exchanges` over records read
// off disk produces what `engine.Trajectory.Exchanges` produces in memory. The two are separate
// functions only because one side has a Trajectory and the other has a slice; a corpus where they
// disagreed would replay differently from the run that recorded it.
func TestTheOnDiskReaderAndTheInMemoryRecorderAgreeOnExchanges(t *testing.T) {
	t.Parallel()

	traj := sample(t)
	inMemory, err := traj.Exchanges()
	if err != nil {
		t.Fatalf("engine exchanges: %v", err)
	}
	dir := t.TempDir()
	written, err := replay.Write(dir, traj)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	back, err := replay.Read(written.Path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	onDisk, err := replay.Exchanges(back.Records)
	if err != nil {
		t.Fatalf("replay exchanges: %v", err)
	}
	if len(onDisk) != len(inMemory) {
		t.Fatalf("on disk %d exchanges, in memory %d", len(onDisk), len(inMemory))
	}
	for i := range onDisk {
		if onDisk[i].RequestDigest != inMemory[i].RequestDigest ||
			string(onDisk[i].RequestBody) != string(inMemory[i].RequestBody) {
			t.Errorf("exchange %d differs between the on-disk reader and the in-memory recorder", i+1)
		}
	}
}

// TestTheCorpusTrajectoriesAreCanonicalAndValid reads what is actually checked in, because a
// format nobody reads back is a format that drifts.
func TestTheCorpusTrajectoriesAreCanonicalAndValid(t *testing.T) {
	t.Parallel()

	files, err := replay.Files(fixtureDir)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(files) == 0 {
		t.Skip("the MVP fixture has no recorded trajectory in this checkout")
	}
	for _, path := range files {
		traj, err := replay.Read(path)
		if err != nil {
			t.Errorf("%s: %v", filepath.Base(path), err)
			continue
		}
		runID, err := replay.RunID(traj.Records)
		if err != nil {
			t.Fatalf("run id: %v", err)
		}
		if runID != traj.RunID {
			t.Errorf("%s is named %s but its content hashes to %s; the file name is derived from the "+
				"content, so a mismatch means the file was edited by hand",
				filepath.Base(path), traj.RunID, runID)
		}
	}
}

// TestAStrictlyPairedWorkerBlockIsSortedToo pins what `Normalize` actually does, because for a
// while its own doc comment said the opposite.
//
// The comment claimed a strictly paired block — every request immediately followed by its own
// answer — was left exactly as it arrived, and that only interleaved blocks were sorted. The code
// sorts every block, and the code is right: a wave whose answers all arrive before the next
// request is written is indistinguishable, in the finished file, from a sequence of calls made
// one after another. Sorting only the interleaved ones would make normalisation depend on how
// busy the machine was — the same investigation over the same world would produce one run id on a
// loaded machine and another on an idle one, which is exactly what layer 1 exists to rule out.
//
// The assertion is the property rather than a literal order, so it cannot be satisfied by
// accident: two arrival orders of the same paired calls normalise to the *same* order, and that
// order is the request digests ascending.
func TestAStrictlyPairedWorkerBlockIsSortedToo(t *testing.T) {
	t.Parallel()

	first := pairedWorkerTrajectory(t, "otel.service.name=payments", "otel.service.name=checkout")
	second := pairedWorkerTrajectory(t, "otel.service.name=checkout", "otel.service.name=payments")

	replay.Normalize(first)
	replay.Normalize(second)

	got := termKeysOf(t, first)
	if other := termKeysOf(t, second); !equalOrder(got, other) {
		t.Fatalf("two arrival orders of the same strictly paired calls normalised differently:\n"+
			"  %v\n  %v\nA recording that depends on the scheduler never replays twice", got, other)
	}
	if want := sortedDigests(t, first); !equalOrder(digestsOf(t, first), want) {
		t.Errorf("the block is ordered %v, want the request digests ascending %v",
			digestsOf(t, first), want)
	}
	// And the normaliser did not quietly drop the pairing: request, answer, request, answer.
	for i, record := range first {
		kind := replay.RecordKind(record)
		want := replay.KindWorkerRequest
		if i%2 == 1 {
			want = replay.KindWorkerResponse
		}
		if i >= 4 {
			break
		}
		if kind != want {
			t.Errorf("record %d is %s, want %s: a sorted block is still pairs", i+1, kind, want)
		}
	}
}

// pairedWorkerTrajectory records two worker calls in the order given, each answered immediately —
// the strictly paired shape the old comment said was left alone.
func pairedWorkerTrajectory(t *testing.T, subjects ...string) []*investigationv1.TrajectoryRecord {
	t.Helper()
	traj := engine.NewTrajectory(func() time.Time { return pinned })
	for _, subject := range subjects {
		key := "term-" + subject
		traj.WorkerRequest("metrics", &investigationv1.AlgebraRequest{
			Term: &investigationv1.AlgebraTerm{
				AlgebraVersion: "1.0.0",
				Term: &investigationv1.AlgebraTerm_MonitorState{
					MonitorState: &investigationv1.MonitorStateTerm{
						Pointer: &graphv1.Pointer{
							Kind:        graphv1.PointerKind_METRIC,
							BackendKind: "synthetic",
							Vocabulary:  "promql",
							Selector:    subject,
						},
					},
				},
			},
			ValidAt:    timestamppb.New(pinned),
			ObservedAt: timestamppb.New(pinned),
		}, key)
		traj.WorkerResponse(&investigationv1.AlgebraResponse{
			Outcome:        investigationv1.TermOutcome_DIGEST,
			TermKey:        key,
			ResponseDigest: "digest-" + subject,
			Mode:           "recorded",
		})
	}
	traj.Stop(engine.Stop{Reason: engine.StopCompleted, Detail: "done"}, nil)
	return traj.Records()
}

func termKeysOf(t *testing.T, records []*investigationv1.TrajectoryRecord) []string {
	t.Helper()
	out := make([]string, 0, len(records))
	for _, record := range records {
		if replay.RecordKind(record) == replay.KindWorkerRequest {
			out = append(out, record.GetWorkerRequest().GetTermKey())
		}
	}
	if len(out) != 2 {
		t.Fatalf("the trajectory holds %d worker requests, want 2", len(out))
	}
	return out
}

func digestsOf(t *testing.T, records []*investigationv1.TrajectoryRecord) []string {
	t.Helper()
	out := make([]string, 0, len(records))
	for _, record := range records {
		if replay.RecordKind(record) != replay.KindWorkerRequest {
			continue
		}
		digest, err := replay.RecordDigest(record)
		if err != nil {
			t.Fatalf("digest a worker request: %v", err)
		}
		out = append(out, digest)
	}
	return out
}

func sortedDigests(t *testing.T, records []*investigationv1.TrajectoryRecord) []string {
	t.Helper()
	out := append([]string(nil), digestsOf(t, records)...)
	sort.Strings(out)
	return out
}

func equalOrder(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
