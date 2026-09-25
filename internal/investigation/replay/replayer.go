// SPDX-License-Identifier: Apache-2.0

package replay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	backend "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/internal/investigation/ledger"
	"github.com/Pierre-Theophile/aisre/internal/investigation/model"
	sdk "github.com/Pierre-Theophile/aisre/pkg/backend"
)

// The re-issue path that re-issues nothing (tasks.md T094; FR-039, FR-040, FR-041, FR-042,
// SC-003, SC-018).
//
// Two layers, two gates, and they answer different questions.
//
// **Layer 1, `--layer trajectory`, the plumbing gate.** It takes a recorded run and puts every
// record back through the seam it came out of. A `model_request` is re-issued through
// `model.NewReplayingTransport` — a real `http.RoundTrip` against the strict replayer, which
// matches by canonical request digest against the *next* recorded exchange and refuses anything
// else. A `worker_request` is re-keyed through the published algebra (`backend.TermKey`) and
// served from the recording: the recorded `worker_response` that answered it, whose digest is
// recomputed from its content with `pkg/backend.ResponseDigest`, and — when a `world/` is present
// beside the trajectory — cross-checked against what the recorded world says about the same term
// key. A `ledger_update` has every judgment's ln LR re-derived from the published
// likelihood-ratio table. Nothing opens a socket and nothing needs a database, which is what lets
// this run on every pull request in a job with no egress and no Postgres.
//
// Why layer 1 does not re-run the engine's control flow: graph answers are deliberately **not**
// recorded (see `workers/graph`'s own note — they come from replaying `events.jsonl` into an
// empty database, and recording them as well would create a second source of truth for the same
// answer). An engine re-run therefore needs a database, and the gate that must run on every pull
// request must not. Re-running the engine is exactly what layer 2 is for.
//
// **Layer 2, `--layer world`, the reasoning gate.** It runs against the recorded world. With a
// `WorldRunner` — supplied by whoever knows how to build an engine for this artifact — it re-runs
// the investigation and reports what came out. Without one it re-issues the recorded worker
// requests against the recorded world and reports `not_recorded` and the miss rate, which is the
// number that gates a *fixture's* admission to the corpus and never the engine's score (FR-042c).
//
// What is impossible here, by construction rather than by policy: falling through to a live call
// (there is no client in any of these paths), returning empty (an unanswered request is an error
// that names the worker, the capability and the parameters), and substituting a similar recording
// (matching is by digest against the next record, never by lookup).

// Layer is which of the two recordings is being replayed.
type Layer string

// The two published layers, spelled as `investigate replay --layer` takes them.
const (
	// LayerTrajectory is the plumbing gate: the recorded sequence, re-issued through the seams.
	LayerTrajectory Layer = "trajectory"
	// LayerWorld is the reasoning gate: the engine over the recorded world.
	LayerWorld Layer = "world"
)

// ParseLayer reads a layer name, refusing anything but the published two.
func ParseLayer(value string) (Layer, error) {
	switch Layer(strings.TrimSpace(value)) {
	case "", LayerTrajectory:
		return LayerTrajectory, nil
	case LayerWorld:
		return LayerWorld, nil
	default:
		return "", fmt.Errorf("--layer %q: want %s (the plumbing gate) or %s (the reasoning gate)",
			value, LayerTrajectory, LayerWorld)
	}
}

// WorldRun is what a world-layer re-run produced.
type WorldRun struct {
	// Records is the trajectory the re-run produced, for comparison against the recorded one.
	Records []*investigationv1.TrajectoryRecord
	// Investigation is the decision record the re-run reached, when the runner produced one.
	Investigation *investigationv1.Investigation
}

// WorldRunner runs the engine over a recorded world.
//
// It is an interface rather than a function this package implements because building an engine
// needs a subject, instants, a prior, a budget profile and a worker set — and where those come
// from differs: a fixture harness reads them out of an `incident:` block, the server's runner
// reads them out of a stored investigation. The replay package owns the recording, the
// accounting and the divergence report; it does not own intake.
type WorldRunner interface {
	// RunWorld runs one investigation against the recorded world and returns what it produced.
	RunWorld(ctx context.Context, world *backend.Recorded) (*WorldRun, error)
}

// ReplayOptions is how a replay is asked for.
//
// is the price of a name that reads at the call site (`replay.Replay(ctx, dir, replay.ReplayOptions{…})`).
//
//nolint:revive // Replay* is the published API Track D's runner is written against; the stutter
type ReplayOptions struct {
	// Layer is which recording to replay. The zero value is LayerTrajectory.
	Layer Layer
	// RunID replays one recorded run. Empty replays every run in the directory, which is what
	// the CI gate does.
	RunID string
	// ReportJSON, when set, is a path the ReplayResponse is written to as JSON.
	ReportJSON string
	// World is the engine builder for the world layer. Nil is legal and documented above.
	World WorldRunner
}

// Divergence is a replay that did not reproduce its recording. It names the **first** diverging
// record, which is the whole reason layer 1 is a sequence (FR-041).
type Divergence struct {
	// Seq is the record the divergence was found at.
	Seq uint64
	// Kind is that record's published type.
	Kind string
	// Expected and Got are the canonical digests.
	Expected string
	Got      string
	// Detail says what moved.
	Detail string
	// Worker, Capability and Parameters are filled in for a worker divergence, because
	// "something in the recording changed" is not an actionable sentence and "metrics/compare
	// over this pointer and this window pair" is (FR-040).
	Worker     string
	Capability string
	Parameters string
	// Path is the trajectory file.
	Path string
}

// Error renders the divergence as the one line a CI log shows.
func (d *Divergence) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "replay diverged at record %d (%s)", d.Seq, d.Kind)
	if d.Worker != "" {
		fmt.Fprintf(&b, " %s/%s", d.Worker, d.Capability)
	}
	fmt.Fprintf(&b, ": %s", d.Detail)
	if d.Expected != "" || d.Got != "" {
		fmt.Fprintf(&b, " (recorded %s, replayed %s)", ShortDigest(d.Expected), ShortDigest(d.Got))
	}
	if d.Parameters != "" {
		fmt.Fprintf(&b, "; parameters %s", d.Parameters)
	}
	return b.String()
}

// DivergenceOf returns the divergence an error carries, or nil.
func DivergenceOf(err error) *Divergence {
	var d *Divergence
	if errors.As(err, &d) {
		return d
	}
	return nil
}

// Result is one trajectory's replay.
type Result struct {
	// Path and RunID identify the recording.
	Path  string
	RunID string
	// Identical says the replay reproduced it.
	Identical bool
	// Divergence is the first diverging record, when there is one.
	Divergence *Divergence
	// Records, ModelExchanges and WorkerCalls are what was replayed.
	Records        int
	ModelExchanges int
	WorkerCalls    int
	// WorldChecked is how many worker answers were cross-checked against the recorded world,
	// and NotRecorded how many of them the world does not hold.
	WorldChecked int
	NotRecorded  int
	// LedgerUpdates is how many ledger updates had their likelihood ratios re-derived.
	LedgerUpdates int
}

// MissRate is the share of the checked worker answers the recorded world does not hold.
func (r *Result) MissRate() float64 {
	if r.WorldChecked == 0 {
		return 0
	}
	return float64(r.NotRecorded) / float64(r.WorldChecked)
}

// Replay replays an exported or fixture directory and returns the published response.
//
// A divergence is **not** an error: it is a ReplayResponse with `identical` false and the first
// diverging record named, so that an RPC stays a 200 and the CLI turns it into exit 4. An error
// is reserved for a recording that could not be read at all.
func Replay(ctx context.Context, fromDir string, opts ReplayOptions) (*investigationv1.ReplayResponse, error) {
	layer, err := ParseLayer(string(opts.Layer))
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(fromDir) == "" {
		return nil, errors.New("replay: no directory to replay from")
	}
	var resp *investigationv1.ReplayResponse
	switch layer {
	case LayerWorld:
		resp, err = replayWorld(ctx, fromDir, opts)
	default:
		resp, err = replayTrajectories(ctx, fromDir, opts)
	}
	if err != nil {
		return nil, err
	}
	if opts.ReportJSON != "" {
		if err := writeReport(opts.ReportJSON, resp); err != nil {
			return nil, err
		}
	}
	return resp, nil
}

// replayTrajectories is the layer-1 gate over every recorded run in the directory.
func replayTrajectories(ctx context.Context, fromDir string, opts ReplayOptions) (*investigationv1.ReplayResponse, error) {
	files, err := Files(fromDir)
	if err != nil {
		return nil, err
	}
	if opts.RunID != "" {
		files = filterRun(files, opts.RunID)
		if len(files) == 0 {
			return nil, fmt.Errorf("replay: %s holds no recorded run %q", fromDir, opts.RunID)
		}
	}
	if len(files) == 0 {
		// A directory with neither a recording nor a world is not an artifact, and answering
		// "identical" for it would be a pass earned by having nothing to check.
		if _, err := os.Stat(filepath.Join(fromDir, "world", sdk.IndexFile)); err != nil {
			return nil, fmt.Errorf("replay: %s holds no trajectories/ and no world/; there is nothing "+
				"to replay there", fromDir)
		}
		// A fixture with a world but no recorded run is a normal state of the corpus: it has not
		// been run yet. The caller reports "0 trajectories"; it does not claim a replay succeeded.
		return &investigationv1.ReplayResponse{Identical: true}, nil
	}

	world, err := loadWorldBeside(fromDir)
	if err != nil {
		return nil, err
	}

	out := &investigationv1.ReplayResponse{Identical: true}
	var checked, missed int
	for _, path := range files {
		result, err := ReplayFile(ctx, path, world)
		if err != nil {
			return nil, err
		}
		checked += result.WorldChecked
		missed += result.NotRecorded
		if !result.Identical {
			out.Identical = false
			if out.FirstDivergingRecord == "" {
				out.FirstDivergingRecord = filepath.Base(path) + ": " + result.Divergence.Error()
			}
		}
	}
	out.NotRecordedCount = uint32(missed) //nolint:gosec // bounded by the recording's own length
	if checked > 0 {
		out.MissRate = float64(missed) / float64(checked)
	}
	return out, nil
}

func filterRun(files []string, runID string) []string {
	var out []string
	for _, path := range files {
		if strings.TrimSuffix(filepath.Base(path), TrajectoryExt) == runID {
			out = append(out, path)
		}
	}
	return out
}

// loadWorldBeside opens the `world/` recording next to a trajectory, when there is one. There
// need not be: a trajectory is replayable on its own, and the world is a cross-check rather than
// a dependency.
func loadWorldBeside(dir string) (*backend.Recorded, error) {
	worldDir := filepath.Join(dir, "world")
	if _, err := os.Stat(filepath.Join(worldDir, sdk.IndexFile)); err != nil {
		return nil, nil //nolint:nilnil // "there is no world here" is not an error
	}
	recorded, err := backend.NewRecorded(worldDir)
	if err != nil {
		return nil, fmt.Errorf("replay: load the recorded world beside %s: %w", dir, err)
	}
	return recorded, nil
}

// ReplayFile replays one recorded run. world may be nil.
//
//nolint:revive // see the note on ReplayOptions: the published spelling is deliberate.
func ReplayFile(ctx context.Context, path string, world *backend.Recorded) (*Result, error) {
	traj, err := Read(path)
	if err != nil {
		return nil, err
	}
	return ReplayTrajectory(ctx, traj, world)
}

// ReplayTrajectory is the layer-1 replay of one recording.
//
//nolint:revive // see the note on ReplayOptions: the published spelling is deliberate.
func ReplayTrajectory(ctx context.Context, traj *Trajectory, world *backend.Recorded) (*Result, error) {
	exchanges, err := Exchanges(traj.Records)
	if err != nil {
		return nil, err
	}
	transport := model.NewReplayingTransport(exchanges)

	result := &Result{
		Path:      traj.Path,
		RunID:     traj.RunID,
		Identical: true,
		Records:   len(traj.Records),
	}

	var (
		pendingResponse string
		pendingWorker   *investigationv1.WorkerRequestRecord
		pendingKey      string
	)
	for _, record := range traj.Records {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		switch body := record.GetRecord().(type) {
		case *investigationv1.TrajectoryRecord_ModelRequest:
			digest, divergence := reissueModelRequest(ctx, transport, record, body.ModelRequest)
			if divergence != nil {
				return diverged(result, traj, divergence), nil
			}
			pendingResponse = digest
			result.ModelExchanges++

		case *investigationv1.TrajectoryRecord_ModelResponse:
			want := body.ModelResponse.GetResponseDigest()
			if want != "" && want != pendingResponse {
				return diverged(result, traj, &Divergence{
					Seq: record.GetSeq(), Kind: KindModelResponse,
					Expected: want, Got: pendingResponse,
					Detail: "the response the recording holds is not the response the replaying transport returned",
				}), nil
			}
			pendingResponse = ""

		case *investigationv1.TrajectoryRecord_WorkerRequest:
			key, divergence := rekeyWorkerRequest(record, body.WorkerRequest)
			if divergence != nil {
				return diverged(result, traj, divergence), nil
			}
			pendingWorker = body.WorkerRequest
			pendingKey = key
			result.WorkerCalls++

		case *investigationv1.TrajectoryRecord_WorkerResponse:
			if pendingWorker == nil {
				return diverged(result, traj, &Divergence{
					Seq: record.GetSeq(), Kind: KindWorkerResponse,
					Detail: "a worker response with no request before it",
				}), nil
			}
			divergence, notRecorded, checked := serveWorkerResponse(
				record, pendingWorker, pendingKey, body.WorkerResponse, world)
			if divergence != nil {
				return diverged(result, traj, divergence), nil
			}
			if checked {
				result.WorldChecked++
			}
			if notRecorded {
				result.NotRecorded++
			}
			pendingWorker = nil
			pendingKey = ""

		case *investigationv1.TrajectoryRecord_LedgerUpdate:
			if divergence := recheckLedgerUpdate(record, body.LedgerUpdate); divergence != nil {
				return diverged(result, traj, divergence), nil
			}
			result.LedgerUpdates++
		}
	}

	// The recording must be exhausted: a run that used fewer model calls than were recorded
	// stopped early, and reporting that as identical would hide exactly the regression the gate
	// exists to catch.
	if used := len(transport.Exchanges()); used != len(exchanges) {
		return diverged(result, traj, &Divergence{
			Seq: uint64(used + 1), Kind: KindModelRequest, //nolint:gosec // bounded by the recording
			Detail: fmt.Sprintf("the replay consumed %d of the %d recorded model exchanges", used, len(exchanges)),
		}), nil
	}

	// The whole-file check, last, because the record-level checks above are the ones that can name
	// *which* record moved and FR-041 asks for that name. This one catches everything they cannot:
	// the file's name is derived from the digest of its content (`replay.RunID`), so a byte edited
	// anywhere — an instant, a rationale, a field the algebra does not key on — renames the file it
	// belongs in. A recording that is not the recording its own name claims is refused.
	if strings.HasPrefix(traj.RunID, runIDPrefix) {
		derived, err := RunID(traj.Records)
		if err != nil {
			return nil, err
		}
		if derived != traj.RunID {
			return diverged(result, traj, &Divergence{
				Kind:     "trajectory",
				Expected: traj.RunID, Got: derived,
				Detail: "the file's name is derived from the digest of its content, and this content " +
					"hashes to a different name; the recording has been edited since it was made",
			}), nil
		}
	}
	return result, nil
}

func diverged(result *Result, traj *Trajectory, d *Divergence) *Result {
	d.Path = traj.Path
	result.Identical = false
	result.Divergence = d
	return result
}

// reissueModelRequest puts the recorded request body back through the strict replaying transport
// and returns the digest of the response that came back.
//
// It is a real RoundTrip rather than a map lookup on purpose: the transport is the seam the
// engine uses, its matching rule is the rule FR-041 publishes, and a gate that reimplemented the
// matching would be testing its own reimplementation.
func reissueModelRequest(
	ctx context.Context,
	transport model.Transport,
	record *investigationv1.TrajectoryRecord,
	body *investigationv1.ModelRequestRecord,
) (string, *Divergence) {
	raw, err := graph.CanonicalJSON(body.GetBody())
	if err != nil {
		return "", &Divergence{Seq: record.GetSeq(), Kind: KindModelRequest, Detail: err.Error()}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.anthropic.com/v1/messages", bytes.NewReader(raw))
	if err != nil {
		return "", &Divergence{Seq: record.GetSeq(), Kind: KindModelRequest, Detail: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := transport.RoundTrip(req)
	if err != nil {
		if d := model.DivergenceOf(err); d != nil {
			return "", &Divergence{
				Seq: record.GetSeq(), Kind: KindModelRequest,
				Expected: d.Expected, Got: d.Actual, Detail: d.Detail,
			}
		}
		return "", &Divergence{Seq: record.GetSeq(), Kind: KindModelRequest, Detail: err.Error()}
	}
	answer, err := io.ReadAll(resp.Body)
	closeErr := resp.Body.Close()
	if err != nil {
		return "", &Divergence{Seq: record.GetSeq(), Kind: KindModelRequest, Detail: err.Error()}
	}
	if closeErr != nil {
		return "", &Divergence{Seq: record.GetSeq(), Kind: KindModelRequest, Detail: closeErr.Error()}
	}
	canonical, err := graph.CanonicalJSON(json.RawMessage(answer))
	if err != nil {
		canonical = answer
	}
	// The recorded request digest is re-derived too, so a hand edit to `request_digest` alone
	// cannot make a mutated body pass: the transport matched on the body, and this checks that
	// the recording's own claim about that body is true.
	if want := body.GetRequestDigest(); want != "" && want != digestOf(raw) {
		return "", &Divergence{
			Seq: record.GetSeq(), Kind: KindModelRequest,
			Expected: want, Got: digestOf(raw),
			Detail: "the record claims a request digest its own body does not hash to",
		}
	}
	return digestOf(canonical), nil
}

// rekeyWorkerRequest re-derives the term key of a recorded worker request through the published
// algebra. A mutated parameter changes the key, which is how a one-byte edit is caught.
func rekeyWorkerRequest(
	record *investigationv1.TrajectoryRecord,
	body *investigationv1.WorkerRequestRecord,
) (string, *Divergence) {
	term := body.GetRequest().GetTerm()
	capability := backend.TermName(term)
	if capability == "" {
		return "", &Divergence{
			Seq: record.GetSeq(), Kind: KindWorkerRequest, Worker: body.GetWorker(),
			Detail: "the recorded request carries no algebra term; a replay cannot serve a question nobody can name",
		}
	}
	key, err := backend.TermKey(term)
	if err != nil {
		return "", &Divergence{
			Seq: record.GetSeq(), Kind: KindWorkerRequest, Worker: body.GetWorker(),
			Capability: capability, Parameters: renderParameters(body.GetRequest()),
			Detail: "the recorded request cannot be keyed: " + err.Error(),
		}
	}
	if want := body.GetTermKey(); want != "" && want != key {
		return "", &Divergence{
			Seq: record.GetSeq(), Kind: KindWorkerRequest, Worker: body.GetWorker(),
			Capability: capability, Parameters: renderParameters(body.GetRequest()),
			Expected: want, Got: key,
			Detail: "the recorded term key is not the key this build derives from the recorded parameters",
		}
	}
	return key, nil
}

// serveWorkerResponse serves the answer from the recording and cross-checks it against the world.
//
// The rule, stated once so nobody has to guess it: **the recorded `worker_response` is the
// answer**, and the `world/` is a cross-check on it. A trajectory is a sequence and its answers
// are the ones the run actually saw; the world is keyed and holds the same answers filed by term
// key. Where both are present they must agree, and a disagreement is a divergence rather than a
// preference. Where the world does not hold the term, that is counted as `not_recorded` and
// reported — it is the number that gates a fixture's admission to the corpus (FR-042c) — and the
// recorded answer still serves. There is no third source: no live call, no empty answer, no
// similar recording.
func serveWorkerResponse(
	record *investigationv1.TrajectoryRecord,
	request *investigationv1.WorkerRequestRecord,
	termKey string,
	body *investigationv1.WorkerResponseRecord,
	world *backend.Recorded,
) (*Divergence, bool, bool) {
	resp := body.GetResponse()
	capability := backend.TermName(request.GetRequest().GetTerm())
	if resp == nil {
		return &Divergence{
			Seq: record.GetSeq(), Kind: KindWorkerResponse, Worker: request.GetWorker(),
			Capability: capability, Parameters: renderParameters(request.GetRequest()),
			Detail: "the recording holds no answer for this request; returning empty is not an option a replay has",
		}, false, false
	}
	recomputed, err := sdk.ResponseDigest(resp)
	if err != nil {
		return &Divergence{
			Seq: record.GetSeq(), Kind: KindWorkerResponse, Worker: request.GetWorker(),
			Capability: capability, Detail: err.Error(),
		}, false, false
	}
	if want := resp.GetResponseDigest(); want != "" && want != recomputed {
		return &Divergence{
			Seq: record.GetSeq(), Kind: KindWorkerResponse, Worker: request.GetWorker(),
			Capability: capability, Parameters: renderParameters(request.GetRequest()),
			Expected: want, Got: recomputed,
			Detail: "the recorded answer claims a digest its own content does not hash to",
		}, false, false
	}
	// The key an answer is *served* under is the key of the request **after the horizon rule has
	// been applied to it**, which is not always the key of the request as asked.
	//
	// A window running past the investigation's `observed_at` is cut back to it before the key is
	// computed (constitution II; backend/horizon.go), so the answer a recorded backend returns is
	// filed under the clamped term while the request record honestly carries the term the engine
	// asked. Keying the answer from the unclamped request compares two different questions: it
	// reported a sound recording as a divergence, and it looked in the world under a key no
	// recording holds. The request record is still checked against its own unclamped key above —
	// that check is about whether this build derives the same key from the same parameters, and
	// it must not be softened.
	served := termKey
	var horizon time.Time
	var truncated bool
	if clamped, at, state := backend.ClampRequest(request.GetRequest()); state == backend.TruncatedToHorizon {
		horizon, truncated = at, true
		key, err := backend.TermKey(clamped.GetTerm())
		if err != nil {
			return &Divergence{
				Seq: record.GetSeq(), Kind: KindWorkerResponse, Worker: request.GetWorker(),
				Capability: capability, Parameters: renderParameters(request.GetRequest()),
				Detail: "the recorded request cannot be keyed after the horizon clamp: " + err.Error(),
			}, false, false
		}
		served = key
	}
	if want := resp.GetTermKey(); want != "" && served != "" && want != served {
		return &Divergence{
			Seq: record.GetSeq(), Kind: KindWorkerResponse, Worker: request.GetWorker(),
			Capability: capability, Parameters: renderParameters(request.GetRequest()),
			Expected: want, Got: served,
			Detail: "the recorded answer is filed under a term key its own request does not produce",
		}, false, false
	}

	// The graph family is never recorded into a world by design (a graph answer is replayed from
	// the event log), so it is not a miss when the world does not hold it.
	if world == nil || sdk.FamilyOf(capability) != sdk.FamilyTelemetry {
		return nil, false, false
	}
	answer, ok := world.World().Answer(served)
	if !ok {
		return nil, true, true
	}
	// The world's digest is compared against the trajectory's after the same horizon statement
	// has been stamped into it, for the same reason the key above is the clamped one: a recorded
	// backend serving a truncated answer annotates it and re-digests it, so the world file's own
	// digest is the digest of an answer that was never sent. Comparing the two directly reported
	// the world and the trajectory as disagreeing about an answer they agree on exactly.
	expected := answer.GetResponseDigest()
	if truncated {
		annotated, ok := proto.Clone(answer).(*investigationv1.AlgebraResponse)
		if !ok {
			return &Divergence{
				Seq: record.GetSeq(), Kind: KindWorkerResponse, Worker: request.GetWorker(),
				Capability: capability, Detail: "the recorded world's answer could not be cloned",
			}, false, true
		}
		backend.AnnotateHorizon(annotated, horizon, backend.TruncatedToHorizon)
		digest, err := sdk.ResponseDigest(annotated)
		if err != nil {
			return &Divergence{
				Seq: record.GetSeq(), Kind: KindWorkerResponse, Worker: request.GetWorker(),
				Capability: capability, Detail: err.Error(),
			}, false, true
		}
		expected = digest
	}
	if expected != recomputed {
		return &Divergence{
			Seq: record.GetSeq(), Kind: KindWorkerResponse, Worker: request.GetWorker(),
			Capability: capability, Parameters: renderParameters(request.GetRequest()),
			Expected: expected, Got: recomputed,
			Detail: "the recorded world and the recorded trajectory disagree about the answer to the same term",
		}, false, true
	}
	return nil, false, true
}

// recheckLedgerUpdate re-derives each judgment's likelihood ratio from the published table.
//
// The engine is the ledger's writer of record and every number in it comes from that table
// (FR-023). A recording whose ln LR does not match what the table says for its own direction and
// strength is a recording made by a build whose table has moved, and replaying it as though
// nothing had changed would silently re-baseline every confidence in the corpus.
func recheckLedgerUpdate(
	record *investigationv1.TrajectoryRecord,
	body *investigationv1.LedgerUpdateRecord,
) *Divergence {
	for _, judgment := range body.GetJudgments() {
		direction := directionOf(judgment.GetDirection())
		strength := strengthOf(judgment.GetStrength())
		want, err := ledger.LnLR(direction, strength)
		if err != nil {
			return &Divergence{
				Seq: record.GetSeq(), Kind: KindLedgerUpdate,
				Detail: fmt.Sprintf("judgment %s: %v", judgment.GetJudgmentId(), err),
			}
		}
		if !nearlyEqual(want, judgment.GetLnLr()) {
			return &Divergence{
				Seq: record.GetSeq(), Kind: KindLedgerUpdate,
				Expected: fmt.Sprintf("%+.6f", want), Got: fmt.Sprintf("%+.6f", judgment.GetLnLr()),
				Detail: fmt.Sprintf("judgment %s (%s %s) carries a likelihood ratio the published table does not produce",
					judgment.GetJudgmentId(), direction, strength),
			}
		}
	}
	return nil
}

// lnLRTolerance is the slack a six-decimal canonical rendering leaves.
const lnLRTolerance = 1e-6

func nearlyEqual(a, b float64) bool {
	diff := a - b
	if diff < 0 {
		diff = -diff
	}
	return diff <= lnLRTolerance
}

func directionOf(d investigationv1.JudgmentDirection) ledger.Direction {
	switch d {
	case investigationv1.JudgmentDirection_SUPPORTS:
		return ledger.Supports
	case investigationv1.JudgmentDirection_REFUTES:
		return ledger.Refutes
	default:
		return ledger.Neutral
	}
}

func strengthOf(s investigationv1.JudgmentStrength) ledger.Strength {
	switch s {
	case investigationv1.JudgmentStrength_WEAK:
		return ledger.Weak
	case investigationv1.JudgmentStrength_MODERATE:
		return ledger.Moderate
	case investigationv1.JudgmentStrength_STRONG:
		return ledger.Strong
	case investigationv1.JudgmentStrength_DECISIVE:
		return ledger.Decisive
	default:
		return ledger.Strength("")
	}
}

// renderParameters is the one-line rendering of a request a divergence names it by: the term, the
// two instants and the hypothesis it serves. It is what turns "something diverged" into a thing
// an operator can go and run.
func renderParameters(req *investigationv1.AlgebraRequest) string {
	if req == nil {
		return ""
	}
	raw, err := graph.CanonicalJSON(req.GetTerm())
	if err != nil {
		raw = []byte("<unrenderable>")
	}
	out := string(raw)
	const cap = 400
	if len(out) > cap {
		out = out[:cap] + "…"
	}
	return fmt.Sprintf("valid_at=%s observed_at=%s serves=%s term=%s",
		req.GetValidAt().AsTime().UTC().Format("2006-01-02T15:04:05Z07:00"),
		req.GetObservedAt().AsTime().UTC().Format("2006-01-02T15:04:05Z07:00"),
		orNone(req.GetServesHypothesisId()), out)
}

func orNone(value string) string {
	if value == "" {
		return "(exploratory)"
	}
	return value
}

// replayWorld is the layer-2 gate.
func replayWorld(ctx context.Context, fromDir string, opts ReplayOptions) (*investigationv1.ReplayResponse, error) {
	worldDir := filepath.Join(fromDir, "world")
	recorded, err := backend.NewRecorded(worldDir)
	if err != nil {
		return nil, fmt.Errorf("replay: %s has no recorded world to replay: %w", fromDir, err)
	}

	if opts.World != nil {
		run, err := opts.World.RunWorld(ctx, recorded)
		if err != nil {
			return nil, err
		}
		return worldResponse(recorded, run), nil
	}

	// No engine builder: re-issue the recorded worker requests against the recorded world. It is
	// model-free, it is the miss-rate measurement FR-042c asks for, and it says out loud that it
	// did not re-reason.
	files, err := Files(fromDir)
	if err != nil {
		return nil, err
	}
	out := &investigationv1.ReplayResponse{Identical: true}
	var asked, missed int
	for _, path := range files {
		traj, err := Read(path)
		if err != nil {
			return nil, err
		}
		for _, record := range traj.Records {
			body, ok := record.GetRecord().(*investigationv1.TrajectoryRecord_WorkerRequest)
			if !ok {
				continue
			}
			term := body.WorkerRequest.GetRequest().GetTerm()
			if sdk.FamilyOf(backend.TermName(term)) != sdk.FamilyTelemetry {
				continue
			}
			asked++
			resp, err := recorded.Execute(ctx, body.WorkerRequest.GetRequest())
			if err != nil {
				return nil, err
			}
			if resp.GetOutcome() == investigationv1.TermOutcome_NOT_RECORDED {
				missed++
			}
		}
	}
	out.NotRecordedCount = uint32(missed) //nolint:gosec // bounded by the recording's own length
	if asked > 0 {
		out.MissRate = float64(missed) / float64(asked)
	}
	return out, nil
}

func worldResponse(recorded *backend.Recorded, run *WorldRun) *investigationv1.ReplayResponse {
	misses := recorded.Misses()
	out := &investigationv1.ReplayResponse{
		Identical:        true,
		NotRecordedCount: uint32(len(misses.Missed)), //nolint:gosec // bounded by the world's own size
	}
	if total := misses.Served + len(misses.Missed); total > 0 {
		out.MissRate = float64(len(misses.Missed)) / float64(total)
	}
	if run != nil {
		out.Investigation = run.Investigation
	}
	return out
}

func writeReport(path string, resp *investigationv1.ReplayResponse) error {
	// EmitUnpopulated, because the two numbers a reader comes to this report for — the miss rate
	// and the not-recorded count — are most interesting when they are zero, and a report that
	// omitted them would say nothing in exactly the case the gate is meant to certify.
	raw, err := protojson.MarshalOptions{Multiline: true, Indent: "  ", EmitUnpopulated: true}.Marshal(resp)
	if err != nil {
		return fmt.Errorf("replay: render the report: %w", err)
	}
	canonical, err := graph.CanonicalJSON(json.RawMessage(raw))
	if err != nil {
		canonical = raw
	}
	if err := os.WriteFile(path, append(canonical, '\n'), 0o600); err != nil {
		return fmt.Errorf("replay: write %s: %w", path, err)
	}
	return nil
}
