// SPDX-License-Identifier: Apache-2.0

package backend

import (
	"context"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	sdk "github.com/Pierre-Theophile/aisre/pkg/backend"
)

// The recorded telemetry backend (tasks.md T037; FR-039, FR-040, FR-042c, FR-067).
//
// It answers from `world/<term_key>.json` and from nowhere else. It opens no socket, holds no
// credential and has no live counterpart to fall through to — not as a policy the code checks,
// but as a fact about the type: there is no vendor client in it. That is what makes FR-067
// ("quickstart reproduces with no vendor account and no network to any telemetry backend") a
// property of the build rather than a hope about the configuration.
//
// A miss is **typed**. An in-algebra term this world does not hold is `NOT_RECORDED` with the
// term echoed back and a coverage block naming the world it missed in — never `NO_DATA`, which
// would say "nothing happened in production", and never a live call, because a replay that
// improvises is evidence of nothing (research §9). The miss is counted, and the count gates the
// *fixture's* admission to the corpus, never the engine's score (FR-042c).
//
// A term outside the algebra is refused the same way a live backend refuses it: `QUERY_FAILED /
// OUTSIDE_ALGEBRA`, naming what was asked and what is available. The recorded backend is not a
// laxer backend; it is the same contract answered from disk.

// RecordedBackendName is the published name of the recorded backend, as it appears in
// `backend list`, in a worker's source-of-truth field and in a worker_calls row.
const RecordedBackendName = "recorded"

// Recorded is a TelemetryBackend answering from one loaded world.
type Recorded struct {
	world   *sdk.World
	version string

	mu      sync.Mutex
	misses  map[string]*Term
	served  int
	refused int
}

// NewRecorded loads the world in dir and returns the backend that answers from it.
func NewRecorded(dir string) (*Recorded, error) {
	world, err := sdk.LoadWorld(dir)
	if err != nil {
		return nil, err
	}
	return NewRecordedFromWorld(world), nil
}

// NewRecordedFromWorld wraps an already-loaded world, which is what the fixture harness does
// when it has checked the index once and replays it many times.
func NewRecordedFromWorld(world *sdk.World) *Recorded {
	return &Recorded{world: world, version: sdk.SDKVersion, misses: make(map[string]*Term)}
}

// Describe returns the recorded backend's declaration. It serves all eight telemetry terms —
// that is what makes it a substitute for a vendor rather than a subset of one — and prices them
// exactly as a live backend does, so that a budget spent in replay is the budget that would be
// spent live.
func (r *Recorded) Describe() sdk.Description {
	terms := sdk.Terms(sdk.FamilyTelemetry)
	costs := make(map[string]sdk.CostClass, len(terms))
	capabilities := make([]sdk.Capability, 0, len(terms))
	for _, term := range terms {
		class := PublishedCostClass(term)
		costs[term] = class
		capabilities = append(capabilities, sdk.Capability{Name: term, ReadOnly: true, CostClass: class})
	}
	return sdk.Description{
		Name:           RecordedBackendName,
		Vendor:         RecordedBackendName,
		Terms:          terms,
		CostClasses:    costs,
		Capabilities:   capabilities,
		Redaction:      r.world.Index.RedactionPolicy(),
		Version:        r.version,
		AlgebraVersion: AlgebraVersion,
	}
}

// PublishedCostClass is the cost class of a term, from contracts/telemetry-backend.md §7. It is
// a property of the term rather than of the vendor, so that a budget written against one backend
// means the same thing against another.
func PublishedCostClass(term string) sdk.CostClass {
	switch term {
	case sdk.TermMonitorState, sdk.TermDrillDown:
		return sdk.CostClassCheap
	case sdk.TermCompare, sdk.TermErrorsByVersion, sdk.TermErrorSpans:
		return sdk.CostClassStandard
	case sdk.TermNewLogPatterns, sdk.TermOnset, sdk.TermExemplars:
		return sdk.CostClassExpensive
	default:
		return sdk.CostClassUnspecified
	}
}

// Execute answers one term from the world. It makes no network call and never falls through to
// a live call to satisfy a miss.
func (r *Recorded) Execute(ctx context.Context, req *Request) (*Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	term := req.GetTerm()
	name := TermName(term)
	if name == "" || FamilyOf(name) != sdk.FamilyTelemetry {
		r.countRefusal()
		return NewResponse(ResponseInput{
			Request: orEmptyRequest(req),
			Outcome: QueryFailed{
				Reason: investigationv1.FailureReason_OUTSIDE_ALGEBRA,
				Detail: outsideAlgebraDetail(name),
			},
			Mode:           string(ModeRecorded),
			BackendVersion: r.version,
		})
	}
	if err := Validate(term); err != nil {
		r.countRefusal()
		return NewResponse(ResponseInput{
			Request: req,
			Outcome: QueryFailed{
				Reason: investigationv1.FailureReason_OUTSIDE_ALGEBRA,
				Detail: err.Error(),
			},
			Mode:           string(ModeRecorded),
			CostClass:      PublishedCostClass(name),
			BackendVersion: r.version,
		})
	}

	// Nobody sees the future (constitution II; horizon.go). A world may hold an answer for a
	// window that runs past the investigation's observed_at — it was recorded by a generator or a
	// vendor that had no horizon to respect — and serving it would hand the investigator evidence
	// it could not have had. The clamp happens before the key is computed, so a replay looks up
	// the window that was actually answerable.
	clampedReq, horizon, horizonState := ClampRequest(req)
	if horizonState == PastHorizon {
		r.countServed()
		return r.pastHorizon(req, horizon)
	}
	term = clampedReq.GetTerm()

	key, err := TermKey(term)
	if err != nil {
		return nil, err
	}
	if answer, ok := r.world.Answer(key); ok {
		r.countServed()
		answer.Mode = string(ModeRecorded)
		if horizonState == TruncatedToHorizon {
			// The recording may pre-date the rule, or come from a backend that does not state
			// it. Either way the statement belongs in the answer rather than beside it (FR-037),
			// and the digest is recomputed over what actually leaves the process.
			AnnotateHorizon(answer, horizon, horizonState)
			digest, digestErr := sdk.ResponseDigest(answer)
			if digestErr != nil {
				return nil, digestErr
			}
			answer.ResponseDigest = digest
		}
		return answer, nil
	}

	r.countMiss(key, term)
	coverage, err := r.missCoverage(key)
	if err != nil {
		return nil, err
	}
	return NewResponse(ResponseInput{
		Request:        req,
		Outcome:        NotRecorded{Term: term, TermKey: key, Coverage: coverage},
		Mode:           string(ModeRecorded),
		CostClass:      PublishedCostClass(name),
		BackendVersion: r.version,
	})
}

// pastHorizon is the answer for a window lying entirely at or after the investigation's
// observed_at: NO_DATA naming the horizon. It is answered without consulting the world, because
// the world is not what makes it empty — a window in the future holds nothing in any recording,
// and a NOT_RECORDED here would blame the fixture for a fact about time.
func (r *Recorded) pastHorizon(req *Request, horizon time.Time) (*Response, error) {
	coverage, err := CoverageInput{
		DataSource: "world:" + r.world.Dir,
		WindowCovered: &Window{
			Start: timestamppb.New(horizon),
			End:   timestamppb.New(horizon),
		},
		VolumeConsidered:         0,
		Sampling:                 "none",
		Truncation:               HorizonReason(horizon, PastHorizon),
		IngestionLagUndetermined: true,
		QuotaUndetermined:        true,
		ExecutedAt:               horizon,
	}.Coverage()
	if err != nil {
		return nil, err
	}
	AnnotateCoverageHorizon(coverage, horizon, PastHorizon)
	return NewResponse(ResponseInput{
		Request: req,
		Outcome: NoData{
			Coverage: coverage,
			AbsentSource: "telemetry after the investigation's observed_at " +
				horizon.Format(time.RFC3339) + "; it does not exist at the instant this investigation observes the world",
		},
		Mode:           string(ModeRecorded),
		CostClass:      PublishedCostClass(TermName(req.GetTerm())),
		BackendVersion: r.version,
	})
}

func outsideAlgebraDetail(name string) string {
	if name == "" {
		return OutsideAlgebra("").Error()
	}
	if FamilyOf(name) == sdk.FamilyGraph {
		return "term " + name + " is a graph term; it is answered by replaying the event log, not by a telemetry backend, and is never recorded into a world"
	}
	if FamilyOf(name) == sdk.FamilyKnowledge {
		return "term " + name + " is a knowledge term; it is answered by the knowledge worker against documents the graph links, not by a telemetry backend"
	}
	return OutsideAlgebra(name).Error()
}

// missCoverage says what the world does hold, so that NOT_RECORDED reads as a statement about
// the recording rather than about production.
func (r *Recorded) missCoverage(key string) (*investigationv1.Coverage, error) {
	return CoverageInput{
		DataSource: "world:" + r.world.Dir,
		WindowCovered: &Window{
			Start: timestamppb.New(time.Unix(0, 0).UTC()),
			End:   timestamppb.New(time.Unix(0, 0).UTC().Add(time.Second)),
		},
		VolumeConsidered:         int64(r.world.Len()),
		Sampling:                 "none",
		Truncation:               ReasonNotRecorded + ":" + key,
		IngestionLagUndetermined: true,
		QuotaUndetermined:        true,
		ExecutedAt:               time.Unix(0, 0).UTC(),
	}.Coverage()
}

func orEmptyRequest(req *Request) *Request {
	if req.GetTerm() != nil {
		return req
	}
	// A refusal still has to be keyed, recorded and replayed. Keying it on a well-formed
	// placeholder is what lets "the engine asked for something outside the algebra" be an
	// evidence item rather than an error nobody can point at.
	return &Request{Term: KnowledgeSearch([]string{"outside-algebra"}, []string{"outside-algebra"}, 1)}
}

func (r *Recorded) countServed() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.served++
}

func (r *Recorded) countRefusal() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refused++
}

func (r *Recorded) countMiss(key string, term *Term) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, seen := r.misses[key]; !seen {
		r.misses[key] = term
	}
}

// Misses returns the accounting of this backend's answers: served, missed and the distinct
// in-algebra terms that missed. It is what the verifier publishes as the fixture's miss rate.
func (r *Recorded) Misses() MissReport {
	r.mu.Lock()
	defer r.mu.Unlock()
	report := MissReport{
		Served:    r.served,
		Refused:   r.refused,
		WorldDir:  r.world.Dir,
		WorldSize: r.world.Len(),
	}
	for key, term := range r.misses {
		report.Missed = append(report.Missed, Miss{TermKey: key, Term: TermName(term)})
	}
	sortMisses(report.Missed)
	return report
}

// World returns the loaded world, for a caller that wants to enumerate what is there.
func (r *Recorded) World() *sdk.World { return r.world }
