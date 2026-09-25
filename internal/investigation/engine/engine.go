// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"errors"
	"fmt"
	"sync"
	"time"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/investigation/audit"
	"github.com/Pierre-Theophile/aisre/internal/investigation/budget"
	"github.com/Pierre-Theophile/aisre/internal/investigation/ledger"
	"github.com/Pierre-Theophile/aisre/internal/investigation/model"
	"github.com/Pierre-Theophile/aisre/internal/investigation/verify"
	"github.com/Pierre-Theophile/aisre/pkg/worker"
)

// The engine (T063, FR-009, FR-013, FR-013a, research §3, plan §Design Decisions 2).
//
// One investigation, one belief state, one loop. The three sentences that define the shape:
//
//   - **The engine is the ledger's writer of record.** Forced tool use returns 400 on this model
//     family, so the ledger cannot be kept current by compelling a tool call. It is kept current
//     by the engine: the deterministic first wave writes judgments with no model in the loop, and
//     a judgment the model proposes is validated, recorded and applied here.
//   - **History is append-only.** Editing an earlier turn invalidates the thinking blocks after
//     it. The ledger is therefore re-rendered by *appending* a turn-scoped system message, never
//     by patching the brief.
//   - **Exactly one belief state.** Evidence gathering is parallelised — the first wave issues its
//     calls concurrently — but reasoning is never forked. There is one ledger and one
//     conversation, so there is never a moment where two incompatible beliefs both exist and
//     something has to merge them.

// Subject is what the investigation is about: the entity, the symptom, the instants and the
// window. It is the shape the engine needs from intake; the intake package owns how an alert or a
// human declaration becomes one.
type Subject struct {
	// EntityRef is the resolved subject entity, as "namespace=value".
	EntityRef string
	// Statement is the symptom in one sentence, as the monitor or the person stated it.
	Statement string
	// FiredAt is the symptom instant: when the monitor transitioned, or when the incident was
	// declared.
	FiredAt time.Time
	// Window is the lookback the investigation searches over.
	Window *investigationv1.Window
	// Origin says where the question came from: "monitor:12345", "human:sam@example.com".
	Origin string
	// Priority is the alert priority the budget profile is selected from.
	Priority string
}

// Validate refuses a subject the engine could not investigate.
func (s Subject) Validate() error {
	switch {
	case s.EntityRef == "":
		return errors.New("engine: the investigation has no subject entity; resolution happens before reasoning (FR-003)")
	case s.Statement == "":
		return errors.New("engine: the investigation has no symptom statement")
	case s.FiredAt.IsZero():
		return errors.New("engine: the investigation has no symptom instant")
	case s.Window.GetStart() == nil || s.Window.GetEnd() == nil:
		return errors.New("engine: the investigation has no window")
	}
	return nil
}

// Config is everything the engine is wired with.
type Config struct {
	// InvestigationID is this run's id.
	InvestigationID string
	// Subject is what is being investigated.
	Subject Subject
	// Instants are the two time dimensions in force, pinned before anything is asked.
	Instants Instants
	// Prior is π₀ from the published coverage audit, with the audit it came from (ADR-0005 D9).
	Prior audit.PriorRecord
	// Workers is the worker set.
	Workers *Workers
	// Client is the model boundary. A nil client is legal and produces a model-free run: the
	// provisional ranking and the first wave, which is the MVP (tasks.md §MVP).
	Client *model.Client
	// Budget is the budget manager.
	Budget *budget.Manager
	// Mode is `live` or `recorded`, recorded per call (FR-015).
	Mode worker.Mode
	// Links turns a request into a deep link.
	Links DeepLinker
	// Clock is injectable so a test's timings are as deterministic as its digests.
	Clock func() time.Time
	// MaxTurns bounds the loop independently of the budget, as a defence against a model that
	// neither proposes nor concludes. Zero means the published default.
	MaxTurns int
	// OnState is the anytime stream (FR-046a). It is called after the provisional ranking, after
	// the deterministic first wave, and after every model turn, with the belief state as it
	// stands at that moment.
	//
	// It is a callback rather than a channel because the engine must not be able to block on a
	// consumer: a subscriber that stopped reading would stall the investigation, and an
	// investigation that stalls because nobody is watching it is worse than one nobody is
	// watching. It returns nothing for the same reason — a subscriber's failure to publish is
	// the subscriber's problem, not a reason to abandon the incident — so a caller that needs to
	// know about its own failures logs them itself.
	//
	// It is called from the run's own goroutine, in order, and never concurrently with itself.
	// Nil is legal and is the ordinary case for a recording.
	OnState func(*Snapshot)
}

// DefaultMaxTurns is the published ceiling on model turns in one investigation. It is not a
// budget — the budget manager owns those — but a termination guarantee: a loop whose only exit is
// a budget is a loop that can spin on a model that says nothing.
const DefaultMaxTurns = 24

// Engine is one investigation.
type Engine struct {
	mu sync.Mutex

	id      string
	subject Subject
	at      Instants

	ledger     *ledger.Ledger
	catalogue  *Catalogue
	trajectory *Trajectory
	budget     *budget.Manager
	workers    *Workers
	client     *model.Client
	links      DeepLinker
	mode       worker.Mode
	clock      func() time.Time
	maxTurns   int
	onState    func(*Snapshot)

	// history is the append-only conversation. Nothing in it is ever edited.
	history []model.Message
	// digests holds each evidence item's digest body, which the ledger does not carry and the
	// citation checker needs.
	digests  map[string]*investigationv1.Digest
	evidence []verify.Evidence

	evidenceSeq   int
	judgmentSeq   int
	hypothesisSeq int
	injections    []string

	// modelJudgments counts the judgments the model proposed and the engine applied. It is what
	// distinguishes "the investigator concluded" from "the investigator never said anything".
	modelJudgments int
	// cacheMiss records the turns on which the stable prefix did not hit the cache. A miss from
	// turn two means a silent invalidator crept into the prefix (FR-061).
	cacheMiss []int
	// rankerScores is the graph's own score per candidate change, kept so that a hypothesis the
	// model proposes about a change the ranker already ranked enters at the ranker's score rather
	// than at a number the engine invented (FR-020a).
	rankerScores map[string]float64
	// changeOffsets is each candidate change's signed distance from the ranking's reference
	// instant, in seconds, as the graph published it: positive before the reference, negative
	// after it. The first wave reads it to tell a change that is merely old from one that is a
	// candidate, and the causal step overwrites it once the ranking moves to the estimated onset,
	// so the number a decision is made on is always measured against the instant in force
	// (firstwave.go, StaleBefore).
	changeOffsets map[string]int64

	// onset is the estimated symptom onset and its evidence id, once the metrics worker has
	// produced one. Everything about causal ordering hangs off it (T081).
	onset *Onset

	// provisional is the prior-only ranking published within 5 s of intake (FR-046a).
	provisional *Provisional

	stopped *Stop
}

// New opens an investigation.
func New(cfg Config) (*Engine, error) {
	if cfg.InvestigationID == "" {
		return nil, errors.New("engine: an investigation id is required")
	}
	if err := cfg.Subject.Validate(); err != nil {
		return nil, err
	}
	if cfg.Workers == nil {
		return nil, errors.New("engine: a worker set is required; the engine reads nothing directly")
	}
	if cfg.Budget == nil {
		return nil, errors.New("engine: a budget is required; every call passes admission before it is issued (FR-047a)")
	}
	if !cfg.Mode.Valid() {
		return nil, fmt.Errorf("engine: mode %q is neither %q nor %q; the mode is recorded per call (FR-015)",
			cfg.Mode, worker.ModeLive, worker.ModeRecorded)
	}
	if cfg.Instants.ValidAt.IsZero() || cfg.Instants.ObservedAt.IsZero() {
		return nil, errors.New("engine: both instants are pinned before anything is asked (constitution II)")
	}
	clock := cfg.Clock
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	links := cfg.Links
	if links == nil {
		links = NoDeepLinks{}
	}
	maxTurns := cfg.MaxTurns
	if maxTurns <= 0 {
		maxTurns = DefaultMaxTurns
	}

	book, err := ledger.New(cfg.InvestigationID, cfg.Prior)
	if err != nil {
		return nil, err
	}

	return &Engine{
		id:            cfg.InvestigationID,
		subject:       cfg.Subject,
		at:            cfg.Instants,
		ledger:        book,
		catalogue:     NewCatalogue(),
		trajectory:    NewTrajectory(clock),
		budget:        cfg.Budget,
		workers:       cfg.Workers,
		client:        cfg.Client,
		links:         links,
		mode:          cfg.Mode,
		clock:         clock,
		maxTurns:      maxTurns,
		onState:       cfg.OnState,
		digests:       map[string]*investigationv1.Digest{},
		rankerScores:  map[string]float64{},
		changeOffsets: map[string]int64{},
	}, nil
}

// ID is the investigation id.
func (e *Engine) ID() string { return e.id }

// Ledger is the one belief state.
func (e *Engine) Ledger() *ledger.Ledger { return e.ledger }

// Trajectory is the layer-1 recording.
func (e *Engine) Trajectory() *Trajectory { return e.trajectory }

// Catalogue is the pointers and handles this investigation has earned.
func (e *Engine) Catalogue() *Catalogue { return e.catalogue }

// Budget is the budget manager.
func (e *Engine) Budget() *budget.Manager { return e.budget }

// Subject is what is being investigated.
func (e *Engine) Subject() Subject { return e.subject }

// Instants are the two time dimensions in force.
func (e *Engine) Instants() Instants { return e.at }

// EvidenceForVerification returns the checker's view of every evidence item recorded so far.
func (e *Engine) EvidenceForVerification() []verify.Evidence {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]verify.Evidence(nil), e.evidence...)
}

// History returns the append-only conversation as it stands.
func (e *Engine) History() []model.Message {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]model.Message(nil), e.history...)
}

// Stopped returns the typed stop, once there is one.
func (e *Engine) Stopped() *Stop {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.stopped
}

// Stop records the typed stop. Every run ends with exactly one, and it is written to the
// trajectory as well as held here (FR-045b).
func (e *Engine) Stop(stop Stop) error {
	if err := stop.Validate(); err != nil {
		return err
	}
	e.mu.Lock()
	if e.stopped != nil {
		e.mu.Unlock()
		return nil // the first stop is the one that bound; a second is bookkeeping noise
	}
	e.stopped = &stop
	e.mu.Unlock()
	// The verdict is settled before the final ledger is written, because the final ledger *is* the
	// answer: naming the open hypothesis afterwards would leave the record saying one thing and
	// the engine another (verdict.go).
	if err := e.concludeVerdict(stop); err != nil {
		return err
	}
	e.trajectory.Stop(stop, e.FinalLedger())
	return nil
}

// FinalLedger is the belief state as the run leaves it, in the ledger's own rank order: which
// hypothesis, how confident, which published bucket, what status.
//
// It is what a recorded run is scored on. A trajectory records the judgments a run applied, and
// the posterior is a function of those judgments and the published likelihood-ratio table — but
// re-deriving it from a recording would mean reimplementing the ledger in the scorer, and a
// scorer that reimplements the thing it scores measures its own reimplementation. So the engine
// states its own answer, once, on the record nothing may follow.
func (e *Engine) FinalLedger() []*investigationv1.FinalHypothesis {
	hypotheses := e.ledger.Hypotheses()
	out := make([]*investigationv1.FinalHypothesis, 0, len(hypotheses))
	for _, h := range hypotheses {
		out = append(out, &investigationv1.FinalHypothesis{
			HypothesisId: h.ID,
			Confidence:   h.Confidence,
			Bucket: &investigationv1.ConfidenceBucket{
				Name: h.Bucket.Name, RangeLow: h.Bucket.Low, RangeHigh: h.Bucket.High,
			},
			Status:                    ledger.StatusProto(h.Status),
			Kind:                      ledger.KindProto(h.Kind),
			CandidateChangeEntityId:   h.CandidateChangeEntityID,
			CandidateChangeEntityRefs: e.catalogue.RefsFor(h.CandidateChangeEntityID),
			Statement:                 h.Statement,
		})
	}
	return out
}

func (e *Engine) now() time.Time { return e.clock().UTC() }

func (e *Engine) nextEvidenceID() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.evidenceSeq++
	return fmt.Sprintf("e-%d", e.evidenceSeq)
}

func (e *Engine) nextJudgmentID() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.judgmentSeq++
	return fmt.Sprintf("j-%d", e.judgmentSeq)
}

func (e *Engine) nextHypothesisID() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.hypothesisSeq++
	return fmt.Sprintf("h-%d", e.hypothesisSeq)
}

// remember files an answer's digest and checker view. It is called for every answer, including
// the ones that failed: a failed call is an evidence item with a reason (FR-027).
func (e *Engine) remember(answer *Answer) {
	if answer == nil || answer.EvidenceID == "" {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.digests[answer.EvidenceID] = answer.Response.GetDigest()
	e.evidence = append(e.evidence, answer.Evidence)
}

// Judge applies one judgment through the ledger and records the update in the trajectory.
//
// This is the only path a confidence moves along. The model never calls it; the model proposes,
// and the engine — here — validates and applies (FR-023).
func (e *Engine) Judge(hypothesisID, evidenceID string, direction ledger.Direction, strength ledger.Strength,
	source ledger.JudgmentSource, workerCallID string,
) (ledger.Judgment, error) {
	judgment, err := e.ledger.Judge(ledger.Judgment{
		ID:           e.nextJudgmentID(),
		HypothesisID: hypothesisID,
		EvidenceID:   evidenceID,
		Direction:    direction,
		Strength:     strength,
		Source:       source,
		WorkerCallID: workerCallID,
		RecordedAt:   e.now(),
	})
	if err != nil {
		return ledger.Judgment{}, err
	}
	digest, err := e.ledger.Digest()
	if err != nil {
		return ledger.Judgment{}, err
	}
	e.trajectory.LedgerUpdate([]ledger.Judgment{judgment}, digest)
	return judgment, nil
}

// Posterior is the ledger's confidence vector, which the diminishing-returns test is measured
// over (FR-045a).
func (e *Engine) Posterior() map[string]float64 {
	out := map[string]float64{}
	for _, h := range e.ledger.Hypotheses() {
		out[h.ID] = h.Confidence
	}
	return out
}

// The anytime stream (FR-046a).
//
// FR-046a asks for an answer that improves while the investigation runs rather than one that
// appears at the end: the prior-only ranking within five seconds, the deterministic first wave
// within thirty, and the belief state after every turn the model takes. The engine owns the phase
// order, so the engine is what says when a phase ended; a consumer that polled the ledger would
// be guessing at boundaries the engine already knows.

// Phase names the step a snapshot was taken after.
type Phase string

// The published phases, in the order a run passes through them.
const (
	// PhaseProvisional is the prior-only ranking, before any telemetry has been asked for.
	PhaseProvisional Phase = "provisional"
	// PhaseFirstWave is the deterministic first wave, with no model in the loop.
	PhaseFirstWave Phase = "first_wave"
	// PhaseTurn is one model turn.
	PhaseTurn Phase = "turn"
)

// Snapshot is the belief state at the end of one phase.
type Snapshot struct {
	// Phase is the step that just completed.
	Phase Phase
	// Turn is the model turn number, 1-based. Zero for the two phases before the loop.
	Turn int
	// Untested is true while nothing in the ledger has been tested — that is, for the
	// provisional phase and no other. A consumer that renders a snapshot must say so.
	Untested bool
	// Hypotheses is the ledger's ordering at that instant.
	Hypotheses []ledger.Hypothesis
	// EvidenceCount is how many evidence items have been recorded. It is monotone
	// non-decreasing across the snapshots of one run, which is the property that makes the
	// stream an improving answer rather than a changing one.
	EvidenceCount int
	// LedgerDigest binds the snapshot to the belief state it describes.
	LedgerDigest string
	// Onset is the onset estimate once there is one, so a consumer can state the reference
	// instant the ranking was taken against.
	Onset *Onset
}

// publishState calls the anytime callback, if there is one.
func (e *Engine) publishState(phase Phase, turn int) {
	if e.onState == nil {
		return
	}
	digest, err := e.ledger.Digest()
	if err != nil {
		// A digest that cannot be computed is a bug in the ledger, not a reason to stop
		// investigating. The snapshot goes out without one rather than not at all.
		digest = ""
	}
	e.mu.Lock()
	evidence := len(e.evidence)
	e.mu.Unlock()
	e.onState(&Snapshot{
		Phase:         phase,
		Turn:          turn,
		Untested:      phase == PhaseProvisional,
		Hypotheses:    e.ledger.Hypotheses(),
		EvidenceCount: evidence,
		LedgerDigest:  digest,
		Onset:         e.Onset(),
	})
}
