// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/investigation/ledger"
	"github.com/Pierre-Theophile/aisre/internal/investigation/model"
)

// The layer-1 trajectory (T063, FR-042a, research §9, contracts/incident-format.md).
//
// One canonical-JSON record per line, in issue order, typed `model_request`, `model_response`,
// `worker_request`, `worker_response`, `ledger_update`, `human_fact`, `stop`. The model records
// carry the exact request body and the exact response including `usage` and `stop_reason`; the
// worker records carry the published algebra request and response, term key and all.
//
// It is a *sequence*, not a map, and that is the whole reason layer 2 exists separately. FR-041
// requires a replay to name the **first** diverging request, which needs an order. A keyed
// recording could answer a question asked in the wrong order and hide exactly the regression the
// gate exists to catch.
//
// The digests are the hinge. `Exchanges` hands the model records straight back to
// `model.NewReplayingTransport`, which matches by canonical request digest and returns the
// recorded response with no network. Phase 7's CI gate is that function call and an assertion
// that the ledger digest at the end is the one recorded.

// Trajectory is one run's ordered record.
type Trajectory struct {
	mu      sync.Mutex
	records []*investigationv1.TrajectoryRecord
	clock   func() time.Time

	// wall is a real clock, and elapsed is how long after the first record each record was made
	// on it. Neither reaches the recording.
	//
	// The recorded instant is the *investigation's* clock, which a fixture run pins to the alert
	// instant so that a re-record is byte-identical — which means the recorded instants measure
	// nothing and the three published latencies (SC-013) read zero off them however long the run
	// actually took. They are two different quantities and this is the second one: how long the
	// run took in the world, kept beside the recording rather than inside it, so a timing can be
	// measured without a recording losing its determinism to the measurement.
	wall    func() time.Time
	start   time.Time
	elapsed []time.Duration
}

// NewTrajectory returns an empty trajectory. The clock is injectable so a test produces a
// trajectory whose instants are as deterministic as its digests.
func NewTrajectory(clock func() time.Time) *Trajectory {
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	return &Trajectory{clock: clock, wall: func() time.Time { return time.Now().UTC() }}
}

func (t *Trajectory) add(record *investigationv1.TrajectoryRecord) {
	t.mu.Lock()
	defer t.mu.Unlock()
	record.Seq = uint64(len(t.records) + 1) //nolint:gosec // a sequence over this run's own records
	record.At = timestamppb.New(t.clock().UTC())
	t.records = append(t.records, record)
	t.markLocked()
}

// markLocked stamps the wall-clock offset of the record just appended. The caller holds the lock.
func (t *Trajectory) markLocked() {
	now := t.wall().UTC()
	if t.start.IsZero() {
		t.start = now
	}
	t.elapsed = append(t.elapsed, now.Sub(t.start))
}

// Elapsed is how long after the first record each record was made, on a real clock, parallel to
// Records. It is what the published latencies are measured over.
func (t *Trajectory) Elapsed() []time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]time.Duration(nil), t.elapsed...)
}

// Concurrent returns an empty trajectory sharing this one's clock.
//
// It is for a caller that issues calls in parallel and must not let the scheduler decide the
// order they are recorded in: each parallel branch records into one of these, and the branches are
// spliced back in the caller's own plan order (`RunFirstWave`). The clock is shared, so a
// spliced record still carries the instant its call was actually made.
func (t *Trajectory) Concurrent() *Trajectory {
	t.mu.Lock()
	defer t.mu.Unlock()
	// The branch shares this trajectory's wall clock *and* its origin, so the offsets it stamps
	// are already offsets into the same run and Splice can carry them over unchanged. A branch
	// that started its own clock would report every call in the wave as having happened at zero.
	return &Trajectory{clock: t.clock, wall: t.wall, start: t.start}
}

// Splice appends another trajectory's records to this one, in that trajectory's order,
// renumbering them into this one's sequence.
//
// This is the second half of the ordering discipline `RunFirstWave` keeps. The first half put
// admission in plan order so that *which* call a binding cap refuses is a function of the plan;
// this one puts the records in plan order so that the trajectory the engine holds is a function
// of the plan too. Before it, the engine's own trajectory was in completion order and only became
// canonical when `replay.Normalize` sorted it on the way to disk — so everything that read the
// trajectory *without* writing it (a digest in the evaluation harness, an assertion in a test)
// was reading an order the Go scheduler chose, and saw it change under load.
func (t *Trajectory) Splice(other *Trajectory) {
	if other == nil {
		return
	}
	records, elapsed := other.recordsAndElapsed()
	t.mu.Lock()
	defer t.mu.Unlock()
	for i, record := range records {
		record.Seq = uint64(len(t.records) + 1) //nolint:gosec // a sequence over this run's own records
		t.records = append(t.records, record)
		if i < len(elapsed) {
			t.elapsed = append(t.elapsed, elapsed[i])
			continue
		}
		t.markLocked()
	}
}

func (t *Trajectory) recordsAndElapsed() ([]*investigationv1.TrajectoryRecord, []time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]*investigationv1.TrajectoryRecord(nil), t.records...),
		append([]time.Duration(nil), t.elapsed...)
}

// ModelRequest records the exact request the engine issued.
func (t *Trajectory) ModelRequest(role model.Role, modelID string, body json.RawMessage, digest string) error {
	value, err := structOf(body)
	if err != nil {
		return fmt.Errorf("trajectory: model request: %w", err)
	}
	t.add(&investigationv1.TrajectoryRecord{
		Record: &investigationv1.TrajectoryRecord_ModelRequest{
			ModelRequest: &investigationv1.ModelRequestRecord{
				Role:          string(role),
				ModelId:       modelID,
				RequestDigest: digest,
				Body:          value,
			},
		},
	})
	return nil
}

// ModelResponse records the exact response, including `usage` and `stop_reason`. A refusal is
// recorded here like any other terminal condition, never swallowed (FR-045b).
func (t *Trajectory) ModelResponse(resp *model.Response) error {
	value, err := structOf(resp.ResponseBody)
	if err != nil {
		return fmt.Errorf("trajectory: model response: %w", err)
	}
	t.add(&investigationv1.TrajectoryRecord{
		Record: &investigationv1.TrajectoryRecord_ModelResponse{
			ModelResponse: &investigationv1.ModelResponseRecord{
				ResponseDigest: resp.ResponseDigest,
				Body:           value,
				Usage:          resp.Usage.ByClass(),
				StopReason:     resp.StopReason,
			},
		},
	})
	return nil
}

// WorkerRequest records one worker call before it is issued.
func (t *Trajectory) WorkerRequest(worker string, req *investigationv1.AlgebraRequest, termKey string) {
	t.add(&investigationv1.TrajectoryRecord{
		Record: &investigationv1.TrajectoryRecord_WorkerRequest{
			WorkerRequest: &investigationv1.WorkerRequestRecord{
				Worker: worker, Request: req, TermKey: termKey,
			},
		},
	})
}

// WorkerResponse records what came back — including a failure, a timeout and an empty answer,
// each of which is an answer with a reason rather than a silence (FR-027).
func (t *Trajectory) WorkerResponse(resp *investigationv1.AlgebraResponse) {
	// `duration_ms` is cleared on the way *in*, not on the way to disk.
	//
	// How long a call took is wall-clock time, and wall-clock time is not part of what "the same
	// answer" means — the rule `pkg/backend.ResponseDigest` publishes and `replay.Normalize`
	// applies when a recording is written. Applying it only at the writer left the trajectory the
	// engine itself holds carrying a field that changes on every run, so its digest was stable on
	// an idle machine and not under load: the same investigation over the same world produced two
	// different digests because one call took 41 µs and the other 63 µs. Everything that reads
	// the trajectory without writing it — the evaluation harness's `trajectory_digest`, the
	// engine's own determinism tests — was reading that.
	//
	// The response is cloned rather than edited: the caller still holds it, and an answer that
	// lost its duration because it was recorded would be an observation changing the thing
	// observed.
	recorded := resp
	if resp.GetDurationMs() != 0 {
		if clone, ok := proto.Clone(resp).(*investigationv1.AlgebraResponse); ok {
			clone.DurationMs = 0
			recorded = clone
		}
	}
	t.add(&investigationv1.TrajectoryRecord{
		Record: &investigationv1.TrajectoryRecord_WorkerResponse{
			WorkerResponse: &investigationv1.WorkerResponseRecord{Response: recorded},
		},
	})
}

// LedgerUpdate records the judgments applied and the ledger digest they produced, so a replay can
// assert that the same evidence produced the same belief state.
func (t *Trajectory) LedgerUpdate(judgments []ledger.Judgment, digest string) {
	record := &investigationv1.LedgerUpdateRecord{LedgerDigest: digest}
	for _, j := range judgments {
		record.Judgments = append(record.Judgments, judgmentProto(j))
	}
	t.add(&investigationv1.TrajectoryRecord{
		Record: &investigationv1.TrajectoryRecord_LedgerUpdate{LedgerUpdate: record},
	})
}

// HumanFact records a fact a person pushed mid-run.
func (t *Trajectory) HumanFact(fact *investigationv1.HumanFact) {
	t.add(&investigationv1.TrajectoryRecord{
		Record: &investigationv1.TrajectoryRecord_HumanFact{HumanFact: fact},
	})
}

// Stop records the typed stop and the belief state the run ended at. Every run ends with exactly
// one of these.
//
// The final ledger travels on the stop rather than in a record of its own for a reason worth
// stating: it is not an event that happened during the run, it is the run's *answer*, and a
// recording whose answer sat in an ordinary record could have another one appended after it. The
// stop is already the record nothing may follow (`Validate` enforces that), so the answer rides
// on the one record that cannot be followed by a different answer.
func (t *Trajectory) Stop(stop Stop, final []*investigationv1.FinalHypothesis) {
	t.add(&investigationv1.TrajectoryRecord{
		Record: &investigationv1.TrajectoryRecord_Stop{
			Stop: &investigationv1.StopRecord{
				Reason: stop.Reason.Proto(), Detail: stop.Detail, FinalLedger: final,
			},
		},
	})
}

// Records returns every record in issue order.
func (t *Trajectory) Records() []*investigationv1.TrajectoryRecord {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]*investigationv1.TrajectoryRecord(nil), t.records...)
}

// Len is how many records there are.
func (t *Trajectory) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.records)
}

// JSONL renders the trajectory as the file `trajectories/<run-id>.jsonl` holds: one canonical
// JSON document per line, in issue order.
func (t *Trajectory) JSONL() ([]byte, error) {
	var out bytes.Buffer
	for i, record := range t.Records() {
		line, err := graph.CanonicalJSON(record)
		if err != nil {
			return nil, fmt.Errorf("trajectory: record %d: %w", i+1, err)
		}
		out.Write(line)
		out.WriteByte('\n')
	}
	return out.Bytes(), nil
}

// Exchanges pairs the model records back into the exchanges a replaying transport serves.
//
// This is the function Phase 7's gate is built on: read a trajectory, call this, hand the result
// to model.NewReplayingTransport, run the engine, compare. A `model_request` with no following
// `model_response` is a truncated recording and is refused rather than replayed, because a replay
// that ran out of recording halfway is not a replay.
func (t *Trajectory) Exchanges() ([]model.Exchange, error) {
	var (
		out     []model.Exchange
		pending *model.Exchange
	)
	for _, record := range t.Records() {
		switch body := record.GetRecord().(type) {
		case *investigationv1.TrajectoryRecord_ModelRequest:
			if pending != nil {
				return nil, fmt.Errorf("trajectory: record %d is a model request but the previous one "+
					"was never answered; the recording is truncated", record.GetSeq())
			}
			raw, err := jsonOf(body.ModelRequest.GetBody())
			if err != nil {
				return nil, err
			}
			pending = &model.Exchange{
				Method:        "POST",
				Path:          "/v1/messages",
				RequestBody:   raw,
				RequestDigest: body.ModelRequest.GetRequestDigest(),
			}
		case *investigationv1.TrajectoryRecord_ModelResponse:
			if pending == nil {
				return nil, fmt.Errorf("trajectory: record %d is a model response with no request before it",
					record.GetSeq())
			}
			raw, err := jsonOf(body.ModelResponse.GetBody())
			if err != nil {
				return nil, err
			}
			pending.Status = 200
			pending.ResponseBody = raw
			pending.ResponseDigest = body.ModelResponse.GetResponseDigest()
			out = append(out, *pending)
			pending = nil
		}
	}
	if pending != nil {
		return nil, fmt.Errorf("trajectory: the last model request was never answered; the recording is truncated")
	}
	return out, nil
}

func structOf(raw json.RawMessage) (*structpb.Struct, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var value structpb.Struct
	if err := value.UnmarshalJSON(raw); err != nil {
		return nil, fmt.Errorf("body is not a JSON object: %w", err)
	}
	return &value, nil
}

func jsonOf(value *structpb.Struct) (json.RawMessage, error) {
	if value == nil {
		return json.RawMessage("null"), nil
	}
	raw, err := graph.CanonicalJSON(value)
	if err != nil {
		return nil, fmt.Errorf("trajectory: render body: %w", err)
	}
	return raw, nil
}

// judgmentProto renders a ledger judgment for the trajectory. The ledger package's own exporter
// is unexported, and a trajectory needs only the five fields that make a judgment reproducible.
func judgmentProto(j ledger.Judgment) *investigationv1.Judgment {
	return &investigationv1.Judgment{
		JudgmentId:   j.ID,
		HypothesisId: j.HypothesisID,
		EvidenceId:   j.EvidenceID,
		Direction:    directionProto(j.Direction),
		Strength:     strengthProto(j.Strength),
		LnLr:         j.LnLR,
		Source:       string(j.Source),
		WorkerCallId: j.WorkerCallID,
		RecordedAt:   timestamppb.New(j.RecordedAt),
	}
}

func directionProto(d ledger.Direction) investigationv1.JudgmentDirection {
	switch d {
	case ledger.Supports:
		return investigationv1.JudgmentDirection_SUPPORTS
	case ledger.Refutes:
		return investigationv1.JudgmentDirection_REFUTES
	case ledger.Neutral:
		return investigationv1.JudgmentDirection_NEUTRAL
	default:
		return investigationv1.JudgmentDirection_JUDGMENT_DIRECTION_UNSPECIFIED
	}
}

func strengthProto(s ledger.Strength) investigationv1.JudgmentStrength {
	switch s {
	case ledger.Weak:
		return investigationv1.JudgmentStrength_WEAK
	case ledger.Moderate:
		return investigationv1.JudgmentStrength_MODERATE
	case ledger.Strong:
		return investigationv1.JudgmentStrength_STRONG
	case ledger.Decisive:
		return investigationv1.JudgmentStrength_DECISIVE
	default:
		return investigationv1.JudgmentStrength_JUDGMENT_STRENGTH_UNSPECIFIED
	}
}
