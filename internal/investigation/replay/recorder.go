// SPDX-License-Identifier: Apache-2.0

package replay

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/investigation/engine"
	"github.com/Pierre-Theophile/aisre/internal/investigation/model"
)

// The layer-1 trajectory on disk (tasks.md T093; FR-033, FR-036, FR-042a, FR-063,
// contracts/incident-format.md §trajectories).
//
// `engine.Trajectory` is the in-memory record and it already knows how to render itself: one
// canonical JSON document per line, in issue order, typed `model_request`, `model_response`,
// `worker_request`, `worker_response`, `ledger_update`, `human_fact`, `stop`. This file is the
// other half — where that rendering goes, what it is called, how it is digested, and how it is
// read back and checked. It deliberately does not re-implement the rendering: `Write` calls
// `Trajectory.JSONL()`, so there is exactly one definition of what a trajectory record looks
// like and a change to the record shape cannot drift between writer and reader.
//
// Four decisions the format rests on.
//
//   - **A file per run, named by content.** `trajectories/<run-id>.jsonl`, where the run id is
//     derived from the digest of the trajectory itself (`RunID`). Re-recording an unchanged run
//     therefore writes the same bytes to the same path: a re-record is a no-op a reviewer can
//     verify with `git status` rather than a diff nobody reads. A run id assigned from a clock or
//     a UUID would make every re-record a new file and the corpus would grow without saying
//     anything.
//   - **One line per record, in issue order.** Layer 1 is a *sequence* — FR-041 requires the
//     first diverging record to be named, which needs an order — and JSONL is the one format
//     where "the third line changed" is both a `git diff` and a machine's answer.
//   - **Canonical JSON, 001's serializer.** `graph.CanonicalJSON`: sorted keys, RFC 3339 UTC,
//     protobuf lowerCamel, no insignificant whitespace. Digests are taken over that rendering
//     rather than over the raw bytes, because protojson is entitled to order a JSON object
//     however it likes and a gate that broke when it did would be testing protojson.
//   - **The digest is over the whole file.** `Digest` is sha256 of the canonical JSONL, which is
//     what `recording_digest` on the decision record refers to (FR-033).

// TrajectoriesDirName is the directory a fixture and an export both hold their layer-1
// recordings in.
const TrajectoriesDirName = "trajectories"

// TrajectoryExt is the extension of one recorded run.
const TrajectoryExt = ".jsonl"

// runIDPrefix keeps a run id self-describing in a stack trace and in a filename.
const runIDPrefix = "run-"

// runIDDigestChars is how much of the digest a run id carries. Sixteen hex characters is 64 bits
// — far past collision range for a corpus of fixtures, and short enough that a person can say it
// out loud.
const runIDDigestChars = 16

// Trajectory is one recorded run as it is held in memory after being read back.
//
// It is the reader's counterpart of `engine.Trajectory`: the engine writes one, this package
// reads one, and both agree on the bytes because both go through `graph.CanonicalJSON`.
type Trajectory struct {
	// RunID is the content-derived id, which is also the file's base name.
	RunID string
	// Path is where it was read from, "" for one built in memory.
	Path string
	// Records are the records in issue order.
	Records []*investigationv1.TrajectoryRecord
	// Digest is sha256 of the canonical JSONL, hex-encoded.
	Digest string
}

// FinalLedger is the belief state the recorded run ended at, read off its stop record.
//
// It returns nil for a recording made before the field existed, which is the honest answer:
// "this run did not say what it ended believing" is different from "this run believed nothing",
// and a scorer that treated the two alike would report a corpus of old recordings as perfectly
// calibrated at zero confidence.
func (t *Trajectory) FinalLedger() []*investigationv1.FinalHypothesis {
	if t == nil {
		return nil
	}
	for _, record := range t.Records {
		if body, ok := record.GetRecord().(*investigationv1.TrajectoryRecord_Stop); ok {
			return body.Stop.GetFinalLedger()
		}
	}
	return nil
}

// Stop is the typed stop the recorded run reached, and whether it holds one.
func (t *Trajectory) Stop() (*investigationv1.StopRecord, bool) {
	if t == nil {
		return nil, false
	}
	for _, record := range t.Records {
		if body, ok := record.GetRecord().(*investigationv1.TrajectoryRecord_Stop); ok {
			return body.Stop, true
		}
	}
	return nil, false
}

// Len is how many records the trajectory holds.
func (t *Trajectory) Len() int {
	if t == nil {
		return 0
	}
	return len(t.Records)
}

// Written is what a write produced.
type Written struct {
	// RunID is the content-derived run id.
	RunID string
	// Path is the file written.
	Path string
	// Digest is sha256 of the file's canonical JSONL.
	Digest string
	// Records is how many records it holds.
	Records int
	// Unchanged reports that the file already held exactly these bytes, which is what a
	// re-record of an unchanged run must produce.
	Unchanged bool
}

// Write renders an engine trajectory into dir/<run-id>.jsonl.
//
// dir is the `trajectories/` directory itself, created when absent. The rendering is
// `engine.Trajectory.JSONL()` verbatim — this function chooses the name and the digest and
// nothing else.
func Write(dir string, traj *engine.Trajectory) (Written, error) {
	if traj == nil {
		return Written{}, errors.New("replay: write: no trajectory")
	}
	raw, err := traj.JSONL()
	if err != nil {
		return Written{}, err
	}
	return WriteBytes(dir, raw)
}

// WriteRecords renders records that are already in hand — a trajectory read back and re-written,
// or one assembled by a test.
func WriteRecords(dir string, records []*investigationv1.TrajectoryRecord) (Written, error) {
	raw, err := JSONL(records)
	if err != nil {
		return Written{}, err
	}
	return WriteBytes(dir, raw)
}

// WriteBytes writes already-rendered canonical JSONL, validating it first. A recording nobody
// validated is a recording that fails at replay time, which is the one moment it is supposed to
// be trusted.
func WriteBytes(dir string, raw []byte) (Written, error) {
	records, err := Parse(raw)
	if err != nil {
		return Written{}, err
	}
	Normalize(records)
	if err := Validate(records); err != nil {
		return Written{}, err
	}
	// Re-render rather than trusting the caller's bytes: the file on disk is canonical by
	// construction, not by convention.
	canonical, err := JSONL(records)
	if err != nil {
		return Written{}, err
	}
	digest := digestOf(canonical)
	runID := runIDFor(digest)
	path := filepath.Join(dir, runID+TrajectoryExt)

	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, canonical) {
		return Written{RunID: runID, Path: path, Digest: digest, Records: len(records), Unchanged: true}, nil
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return Written{}, fmt.Errorf("replay: create %s: %w", dir, err)
	}
	if err := os.WriteFile(path, canonical, 0o600); err != nil {
		return Written{}, fmt.Errorf("replay: write %s: %w", path, err)
	}
	return Written{RunID: runID, Path: path, Digest: digest, Records: len(records)}, nil
}

// Normalize makes a recording a function of the run rather than of the machine that made it.
//
// Two things are normalised, and nothing else. In particular no instant is touched: a recording
// is made under a pinned clock, and normalising a timestamp would hide a run whose clock was not.
//
// **1. `duration_ms` is cleared on every worker answer.** It is the same rule
// `pkg/backend.ResponseDigest` already publishes — wall-clock time is not part of what "the same
// answer" means. Without it a re-record of an unchanged fixture differs in a field that says
// nothing, the content-derived run id moves, and "the recording did not change" stops being
// observable.
//
// **2. Every block of worker calls is put into canonical order.** A block is a maximal run of
// worker records uninterrupted by a model turn, a ledger update, a human fact or the stop, and
// **every** such block is re-emitted as request/answer pairs sorted by the canonical digest of
// the request — including one that already arrived strictly paired.
//
// Sorting all of them, rather than only the blocks that look concurrent, is what makes a
// recording reproducible. The first wave issues its calls in parallel (`engine.RunFirstWave`), so
// the order its records reach the trajectory in is the order the scheduler chose. That order is
// not stable *and it is not observable either*: a wave whose answers all arrive before the next
// request is written is indistinguishable, in the finished file, from a sequence of calls made
// one after another. A normaliser that sorted only the interleaved blocks would therefore sort a
// wave on a loaded machine and leave the same wave alone on an idle one — the run id would move
// between two runs of the same investigation, which is exactly the thing layer 1 exists to rule
// out. The cost is stated plainly: **the arrival order of worker calls inside a block is not
// preserved and a reordering of them is not a divergence.** What is preserved, and what FR-041
// needs, is where each block sits relative to the model turns and the ledger updates around it,
// which call was issued, with which parameters, and what came back.
//
// A block that cannot be paired cleanly — a request with no answer, a truncated recording — is
// left exactly as it arrived, so `Validate` reports it rather than the normaliser hiding it.
//
// `seq` is renumbered afterwards, because a sequence with a gap in it is not one.
func Normalize(records []*investigationv1.TrajectoryRecord) {
	for _, record := range records {
		if body, ok := record.GetRecord().(*investigationv1.TrajectoryRecord_WorkerResponse); ok {
			if resp := body.WorkerResponse.GetResponse(); resp != nil {
				resp.DurationMs = 0
			}
		}
	}
	reorderConcurrentWorkerBlocks(records)
	for i, record := range records {
		record.Seq = uint64(i + 1) //nolint:gosec // a sequence over this file's own records
	}
}

// workerPair is one worker call and the answer it got.
type workerPair struct {
	request  *investigationv1.TrajectoryRecord
	response *investigationv1.TrajectoryRecord
	key      string
}

// reorderConcurrentWorkerBlocks puts every maximal block of worker calls into canonical order.
//
// A **block** is a run of worker records uninterrupted by a model turn, a ledger update, a human
// fact or the stop. Inside a block the engine issues the first wave's calls concurrently
// (`engine.RunFirstWave`), so the order the records arrive in is the order the scheduler chose:
// re-running the same investigation over the same world produces the same *set* of calls in a
// different arrival order, and a recording that kept it would never replay twice. Each block is
// therefore re-emitted as request/answer pairs ordered by the canonical digest of the request.
//
// Every block, not only the interleaved ones. A wave whose answers happened to arrive in issue
// order produces a strictly paired block that is indistinguishable from a sequence of separate
// calls, so treating "strictly paired" as "sequential, leave alone" would make normalisation
// depend on how busy the machine was. See Normalize for what that costs and what it buys.
//
// What the sequence still carries — and it is the part FR-041 needs — is where each block sits
// relative to the model turns and the ledger updates around it. A call that moved from before a
// model turn to after it, a wave that gained or lost a query, a request whose parameters changed:
// all of those change the block and are reported at the first record that differs. What is
// deliberately *not* distinguishable is the arrival order of calls inside one block.
func reorderConcurrentWorkerBlocks(records []*investigationv1.TrajectoryRecord) {
	for start := 0; start < len(records); {
		if !isWorkerRecord(records[start]) {
			start++
			continue
		}
		end := start
		for end < len(records) && isWorkerRecord(records[end]) {
			end++
		}
		canonicaliseWorkerBlock(records[start:end])
		start = end
	}
}

func isWorkerRecord(record *investigationv1.TrajectoryRecord) bool {
	switch RecordKind(record) {
	case KindWorkerRequest, KindWorkerResponse:
		return true
	default:
		return false
	}
}

// canonicaliseWorkerBlock rewrites one block in place. A block it cannot pair cleanly is left
// exactly as it is, so that Validate reports a truncated recording rather than a normaliser
// hiding one.
func canonicaliseWorkerBlock(block []*investigationv1.TrajectoryRecord) {
	pairs, ok := pairUp(block)
	if !ok {
		return
	}
	sort.SliceStable(pairs, func(a, b int) bool { return pairs[a].key < pairs[b].key })
	at := 0
	for _, pair := range pairs {
		block[at] = pair.request
		block[at+1] = pair.response
		at += 2
	}
}

// pairUp matches each request with its answer by term key, falling back to arrival order where a
// key repeats. It gives up — returning false, so the block is left alone — on anything it cannot
// pair cleanly, because a block a normaliser does not understand is a block it must not rewrite.
func pairUp(block []*investigationv1.TrajectoryRecord) ([]workerPair, bool) {
	var (
		requests  []*investigationv1.TrajectoryRecord
		responses []*investigationv1.TrajectoryRecord
	)
	for _, record := range block {
		if RecordKind(record) == KindWorkerRequest {
			requests = append(requests, record)
		} else {
			responses = append(responses, record)
		}
	}
	if len(requests) != len(responses) {
		return nil, false
	}
	taken := make([]bool, len(responses))
	pairs := make([]workerPair, 0, len(requests))
	for _, request := range requests {
		wantKey := request.GetWorkerRequest().GetTermKey()
		index := -1
		for i, response := range responses {
			if taken[i] {
				continue
			}
			gotKey := response.GetWorkerResponse().GetResponse().GetTermKey()
			if wantKey == "" || gotKey == "" || gotKey == wantKey {
				index = i
				break
			}
		}
		if index < 0 {
			return nil, false
		}
		taken[index] = true
		key, err := RecordDigest(request)
		if err != nil {
			return nil, false
		}
		pairs = append(pairs, workerPair{request: request, response: responses[index], key: key})
	}
	return pairs, true
}

// RunID is the run id of a set of records: `run-` plus the first 16 hex characters of the
// trajectory's own digest.
//
// Deriving the id from the content is what makes a re-record byte-identical. The alternative —
// a clock, a counter or a UUID — makes every re-record a new file, so "the recording did not
// change" stops being observable and the corpus grows without saying anything.
func RunID(records []*investigationv1.TrajectoryRecord) (string, error) {
	digest, err := Digest(records)
	if err != nil {
		return "", err
	}
	return runIDFor(digest), nil
}

func runIDFor(digest string) string {
	if len(digest) > runIDDigestChars {
		digest = digest[:runIDDigestChars]
	}
	return runIDPrefix + digest
}

// Digest is sha256 of the canonical JSONL of records, hex-encoded. It is the value a decision
// record carries as `recording_digest` (FR-033).
func Digest(records []*investigationv1.TrajectoryRecord) (string, error) {
	raw, err := JSONL(records)
	if err != nil {
		return "", err
	}
	return digestOf(raw), nil
}

// JSONL renders records the way `engine.Trajectory.JSONL` renders its own: one canonical JSON
// document per line, in issue order.
func JSONL(records []*investigationv1.TrajectoryRecord) ([]byte, error) {
	var out bytes.Buffer
	for i, record := range records {
		line, err := graph.CanonicalJSON(record)
		if err != nil {
			return nil, fmt.Errorf("replay: render record %d: %w", i+1, err)
		}
		out.Write(line)
		out.WriteByte('\n')
	}
	return out.Bytes(), nil
}

// Read parses one `trajectories/<run-id>.jsonl` and validates it.
func Read(path string) (*Trajectory, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // a fixture path the operator named
	if err != nil {
		return nil, fmt.Errorf("replay: read %s: %w", path, err)
	}
	records, err := Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("replay: %s: %w", path, err)
	}
	if err := Validate(records); err != nil {
		return nil, fmt.Errorf("replay: %s: %w", path, err)
	}
	canonical, err := JSONL(records)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(canonical, raw) {
		// A file that is not its own canonical rendering cannot be digested reproducibly, so
		// the gate would be measuring the writer's whitespace rather than the run.
		return nil, fmt.Errorf("replay: %s is not canonical JSONL; re-record it with `fixture record-trajectory` "+
			"(the digest a decision record carries is taken over the canonical rendering)", path)
	}
	digest := digestOf(canonical)
	return &Trajectory{
		RunID:   strings.TrimSuffix(filepath.Base(path), TrajectoryExt),
		Path:    path,
		Records: records,
		Digest:  digest,
	}, nil
}

// Parse decodes canonical JSONL into records without validating the sequence.
func Parse(raw []byte) ([]*investigationv1.TrajectoryRecord, error) {
	var (
		out  []*investigationv1.TrajectoryRecord
		line int
	)
	scanner := bytes.Split(raw, []byte("\n"))
	unmarshal := protojson.UnmarshalOptions{DiscardUnknown: false}
	for _, text := range scanner {
		line++
		if len(bytes.TrimSpace(text)) == 0 {
			continue
		}
		record := &investigationv1.TrajectoryRecord{}
		if err := unmarshal.Unmarshal(text, record); err != nil {
			return nil, fmt.Errorf("line %d is not a trajectory record: %w", line, err)
		}
		out = append(out, record)
	}
	return out, nil
}

// Validate refuses a recording a replay could not trust.
//
// The four rules are the ones a divergence report depends on. Sequence numbers must be 1..N in
// order, or "the first diverging record" is not a thing that can be named. Every record must
// carry one of the seven published types, because an untyped record is one the replayer would
// silently skip. A model request must be answered, and a worker request must be answered, or the
// recording is truncated and a replay that ran out halfway would report success. And a run ends
// with exactly one `stop`, because every run ends with exactly one typed stop (FR-045b).
func Validate(records []*investigationv1.TrajectoryRecord) error {
	if len(records) == 0 {
		return errors.New("the trajectory holds no records; an empty recording replays nothing and proves nothing")
	}
	var (
		pendingModel  *investigationv1.TrajectoryRecord
		pendingWorker *investigationv1.TrajectoryRecord
		stops         int
	)
	for i, record := range records {
		want := uint64(i + 1) //nolint:gosec // a sequence over this file's own records
		if record.GetSeq() != want {
			return fmt.Errorf("record %d carries seq %d; layer 1 is a sequence and its numbers are 1..N in order",
				i+1, record.GetSeq())
		}
		if record.GetAt() == nil {
			return fmt.Errorf("record %d carries no instant", record.GetSeq())
		}
		kind := RecordKind(record)
		if kind == "" {
			return fmt.Errorf("record %d carries none of the seven published record types", record.GetSeq())
		}
		switch kind {
		case KindModelRequest:
			if pendingModel != nil {
				return fmt.Errorf("record %d is a model request but record %d was never answered; "+
					"the recording is truncated", record.GetSeq(), pendingModel.GetSeq())
			}
			pendingModel = record
		case KindModelResponse:
			if pendingModel == nil {
				return fmt.Errorf("record %d is a model response with no request before it", record.GetSeq())
			}
			pendingModel = nil
		case KindWorkerRequest:
			if pendingWorker != nil {
				return fmt.Errorf("record %d is a worker request but record %d was never answered; "+
					"the recording is truncated", record.GetSeq(), pendingWorker.GetSeq())
			}
			pendingWorker = record
		case KindWorkerResponse:
			if pendingWorker == nil {
				return fmt.Errorf("record %d is a worker response with no request before it", record.GetSeq())
			}
			pendingWorker = nil
		case KindStop:
			stops++
		}
	}
	if pendingModel != nil {
		return fmt.Errorf("record %d is a model request that was never answered; the recording is truncated",
			pendingModel.GetSeq())
	}
	if pendingWorker != nil {
		return fmt.Errorf("record %d is a worker request that was never answered; the recording is truncated",
			pendingWorker.GetSeq())
	}
	if stops != 1 {
		return fmt.Errorf("the trajectory holds %d stop records; every run ends with exactly one typed stop (FR-045b)",
			stops)
	}
	if RecordKind(records[len(records)-1]) != KindStop {
		return errors.New("the trajectory's last record is not the stop; a run that recorded anything after " +
			"it stopped is a run whose stop did not stop it")
	}
	return nil
}

// The seven published record types, spelled as they appear in a divergence report and in the
// `record` oneof of `TrajectoryRecord`.
const (
	KindModelRequest   = "model_request"
	KindModelResponse  = "model_response"
	KindWorkerRequest  = "worker_request"
	KindWorkerResponse = "worker_response"
	KindLedgerUpdate   = "ledger_update"
	KindHumanFact      = "human_fact"
	KindStop           = "stop"
)

// RecordKind names a record's type, or "" when it carries none.
func RecordKind(record *investigationv1.TrajectoryRecord) string {
	switch record.GetRecord().(type) {
	case *investigationv1.TrajectoryRecord_ModelRequest:
		return KindModelRequest
	case *investigationv1.TrajectoryRecord_ModelResponse:
		return KindModelResponse
	case *investigationv1.TrajectoryRecord_WorkerRequest:
		return KindWorkerRequest
	case *investigationv1.TrajectoryRecord_WorkerResponse:
		return KindWorkerResponse
	case *investigationv1.TrajectoryRecord_LedgerUpdate:
		return KindLedgerUpdate
	case *investigationv1.TrajectoryRecord_HumanFact:
		return KindHumanFact
	case *investigationv1.TrajectoryRecord_Stop:
		return KindStop
	default:
		return ""
	}
}

// RecordDigest is the canonical digest of one record with its own bookkeeping removed.
//
// `seq` and `at` are stripped before hashing. They are properties of the recording rather than of
// what happened — two runs that asked the same question a millisecond apart are the same request
// — and a digest that carried them would make every comparison a comparison of clocks.
func RecordDigest(record *investigationv1.TrajectoryRecord) (string, error) {
	body, err := recordBody(record)
	if err != nil {
		return "", err
	}
	return digestOf(body), nil
}

func recordBody(record *investigationv1.TrajectoryRecord) ([]byte, error) {
	raw, err := graph.CanonicalJSON(record)
	if err != nil {
		return nil, fmt.Errorf("replay: digest record %d: %w", record.GetSeq(), err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, fmt.Errorf("replay: digest record %d: %w", record.GetSeq(), err)
	}
	delete(object, "seq")
	delete(object, "at")
	return graph.CanonicalJSON(object)
}

// Exchanges pairs the model records into the exchanges a replaying transport serves.
//
// `engine.Trajectory.Exchanges` does the same for a trajectory still in memory; this is its
// counterpart for one read back off disk, and the two produce the same exchanges from the same
// records — the round-trip is asserted in recorder_test.go rather than assumed.
func Exchanges(records []*investigationv1.TrajectoryRecord) ([]model.Exchange, error) {
	var (
		out     []model.Exchange
		pending *model.Exchange
	)
	for _, record := range records {
		switch body := record.GetRecord().(type) {
		case *investigationv1.TrajectoryRecord_ModelRequest:
			raw, err := graph.CanonicalJSON(body.ModelRequest.GetBody())
			if err != nil {
				return nil, fmt.Errorf("replay: record %d: %w", record.GetSeq(), err)
			}
			pending = &model.Exchange{
				Method:        "POST",
				Path:          "/v1/messages",
				RequestBody:   raw,
				RequestDigest: body.ModelRequest.GetRequestDigest(),
			}
		case *investigationv1.TrajectoryRecord_ModelResponse:
			if pending == nil {
				return nil, fmt.Errorf("replay: record %d is a model response with no request before it",
					record.GetSeq())
			}
			raw, err := graph.CanonicalJSON(body.ModelResponse.GetBody())
			if err != nil {
				return nil, fmt.Errorf("replay: record %d: %w", record.GetSeq(), err)
			}
			pending.Status = 200
			pending.ResponseBody = raw
			pending.ResponseDigest = body.ModelResponse.GetResponseDigest()
			out = append(out, *pending)
			pending = nil
		}
	}
	if pending != nil {
		return nil, errors.New("replay: the last model request was never answered; the recording is truncated")
	}
	return out, nil
}

// Files lists dir/trajectories/*.jsonl, sorted. An absent directory yields no files and no
// error: a fixture with no recorded run is a normal fixture, not a broken one.
func Files(dir string) ([]string, error) {
	trajectories := filepath.Join(dir, TrajectoriesDirName)
	entries, err := os.ReadDir(trajectories)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("replay: read %s: %w", trajectories, err)
	}
	var files []string
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != TrajectoryExt {
			continue
		}
		files = append(files, filepath.Join(trajectories, entry.Name()))
	}
	sort.Strings(files)
	return files, nil
}

func digestOf(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// ShortDigest is the 12-character form a divergence report prints.
func ShortDigest(digest string) string {
	if len(digest) <= 12 {
		return digest
	}
	return digest[:12]
}
