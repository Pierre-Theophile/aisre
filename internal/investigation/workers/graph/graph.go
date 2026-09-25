// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/internal/investigation/workers"
	eventlog "github.com/Pierre-Theophile/aisre/internal/log"
	"github.com/Pierre-Theophile/aisre/internal/query"
	"github.com/Pierre-Theophile/aisre/pkg/worker"
)

// The graph worker (tasks.md T042; FR-009, FR-012, FR-042b).
//
// It is a typed wrapper over feature 001's published QueryService RPCs and nothing more. It
// holds no backend, contains no model, invents no answer, and — the part that matters for replay
// — is **never cross-producted into a recorded world**. Graph answers come from replaying the
// fixture's `events.jsonl` into an empty database, which 001 already guarantees is
// byte-reproducible; recording them as well would duplicate the event log and create a second
// source of truth for the same answer, which is a source of drift (research §9, plan F3).
//
// Both time dimensions travel through untouched. That is the whole reason this worker exists as
// a worker rather than as a direct call: every read of the graph carries a valid instant and an
// observed instant (constitution II), and a wrapper that dropped one would let an investigation
// silently ask "as known now" when it meant "as known then" — the exact confusion the bitemporal
// model was built to prevent.

// Name is the worker's published name.
const Name = "graph"

// Version is the worker's own version, recorded with every answer.
const Version = "0.1.0"

// SourceOfTruth is the one system this worker speaks for.
const SourceOfTruth = "graph:sreagent.graph.v1.QueryService"

// QueryService is the subset of feature 001's published service this worker calls. It is an
// interface so that the worker can run in-process against the query engine (which is what a
// fixture replay does) and over ConnectRPC against a server (which is what an operator does),
// with no difference in what it returns.
type QueryService interface {
	// Subgraph is the neighbourhood read.
	Subgraph(ctx context.Context, req *graphv1.SubgraphRequest) (*graphv1.SubgraphResponse, error)
	// Diff is the change-between-two-instants read.
	Diff(ctx context.Context, req *graphv1.DiffRequest) (*graphv1.DiffResponse, error)
	// Impact is the blast-radius read.
	Impact(ctx context.Context, req *graphv1.ImpactRequest) (*graphv1.ImpactResponse, error)
	// Pointers is the telemetry-selector read.
	Pointers(ctx context.Context, req *graphv1.PointersRequest) (*graphv1.PointersResponse, error)
	// NodeHistory is the version-history read.
	NodeHistory(ctx context.Context, req *graphv1.NodeHistoryRequest) (*graphv1.NodeHistoryResponse, error)
	// ResolutionAudit is the identity-resolution read.
	ResolutionAudit(ctx context.Context, req *graphv1.ResolutionAuditRequest) (*graphv1.ResolutionAuditResponse, error)
	// Extent is the coverage read — how much of reality this graph claims to have seen.
	Extent(ctx context.Context, req *graphv1.ExtentRequest) (*graphv1.Extent, error)
}

// Worker is the graph worker.
type Worker struct {
	service QueryService
}

var _ worker.Worker = (*Worker)(nil)

// New returns the graph worker over a query service.
func New(service QueryService) *Worker { return &Worker{service: service} }

// Describe returns the declaration. Every capability is read-only and cheap: a graph read is an
// indexed lookup against our own store, and pricing it as anything else would make the budget
// manager ration the one source that costs no vendor quota at all.
func (w *Worker) Describe() worker.Description {
	terms := []string{
		engine.TermSubgraph, engine.TermDiff, engine.TermImpact, engine.TermPointers,
		engine.TermNodeHistory, engine.TermResolutionAudit, engine.TermExtent,
	}
	capabilities := make([]worker.Capability, 0, len(terms))
	for _, term := range terms {
		capabilities = append(capabilities, worker.Capability{
			Name:      term,
			ReadOnly:  true,
			CostClass: worker.CostClassCheap,
		})
	}
	return worker.Description{
		Name:          Name,
		SourceOfTruth: SourceOfTruth,
		Capabilities:  capabilities,
		// No model. The graph worker's answers are the graph's own, and a model that
		// paraphrased them would be a second belief state (FR-013a).
		ContainsModel: false,
		Redaction:     workers.Redaction(),
		Modes:         workers.BothModes(),
		Version:       Version,
	}
}

// Call answers one graph term.
//
// Both modes return the same answer for the same input because there is only one path: the
// graph is replayed from the event log in recorded mode and read from the store in live mode,
// and 001's replay guarantee is that those are the same graph. The mode is stamped on the
// response, so a trajectory records which one ran.
func (w *Worker) Call(ctx context.Context, req worker.Request) (worker.Response, error) {
	if !w.Describe().Declares(req.Capability) {
		return worker.Response{}, worker.Reject(worker.ReasonUndeclaredCapability,
			"worker %s did not declare capability %s", Name, req.Capability)
	}
	term := req.Algebra.GetTerm().GetGraph()
	if term == nil {
		return worker.Response{}, worker.Reject(worker.ReasonOutsideAlgebra,
			"worker %s was called with a %s term, which is not in the graph family",
			Name, engine.TermName(req.Algebra.GetTerm()))
	}

	started := time.Now()
	answer, searched, err := w.answer(ctx, term)
	if err != nil {
		return worker.Response{}, err
	}

	coverage, err := w.coverage(ctx, searched, req)
	if err != nil {
		return worker.Response{}, err
	}
	// A graph answer is not one of the eight telemetry digest shapes: it is 001's own response
	// message, and re-expressing it as a metric digest would lose exactly the structure an
	// investigator needs. It travels as the digest's free-text-adjacent evidence — the
	// executed query — plus the coverage block, and the caller reads the typed response through
	// GraphAnswer below. The digest boundary is not crossed either way: a graph response is
	// already identifiers and structure, never samples.
	resp, err := engine.NewResponse(engine.ResponseInput{
		Request:        req.Algebra,
		Outcome:        engine.DigestOutcome{Body: &investigationv1.Digest{Coverage: coverage}},
		Mode:           req.Mode.String(),
		CostClass:      worker.CostClassCheap,
		Duration:       time.Since(started),
		BackendVersion: Version,
		Vocabulary:     "sreagent.graph.v1",
		ExecutedQuery:  executedQuery(term),
	})
	if err != nil {
		return worker.Response{}, err
	}
	return worker.Response{
		Worker:     Name,
		Capability: req.Capability,
		Mode:       req.Mode,
		Algebra:    resp,
		Graph:      answer,
	}, nil
}

// answer dispatches to the published RPC and returns the response together with the entity ids
// it was about, which is what the coverage block names as searched.
func (w *Worker) answer(ctx context.Context, term *investigationv1.GraphTerm) (proto.Message, []string, error) {
	switch t := term.GetTerm().(type) {
	case *investigationv1.GraphTerm_Subgraph:
		resp, err := w.service.Subgraph(ctx, t.Subgraph)
		return resp, []string{refString(t.Subgraph.GetFocus())}, err
	case *investigationv1.GraphTerm_Diff:
		resp, err := w.service.Diff(ctx, t.Diff)
		return resp, []string{refString(t.Diff.GetSubgraph().GetFocus())}, err
	case *investigationv1.GraphTerm_Impact:
		resp, err := w.service.Impact(ctx, t.Impact)
		return resp, []string{refString(t.Impact.GetFocus())}, err
	case *investigationv1.GraphTerm_Pointers:
		resp, err := w.service.Pointers(ctx, t.Pointers)
		return resp, []string{refString(t.Pointers.GetFocus())}, err
	case *investigationv1.GraphTerm_NodeHistory:
		resp, err := w.service.NodeHistory(ctx, t.NodeHistory)
		return resp, []string{refString(t.NodeHistory.GetFocus())}, err
	case *investigationv1.GraphTerm_ResolutionAudit:
		resp, err := w.service.ResolutionAudit(ctx, t.ResolutionAudit)
		return resp, []string{refString(t.ResolutionAudit.GetA()), refString(t.ResolutionAudit.GetB())}, err
	case *investigationv1.GraphTerm_Extent:
		resp, err := w.service.Extent(ctx, t.Extent)
		return resp, nil, err
	default:
		return nil, nil, engine.OutsideAlgebra("a graph term with no member set")
	}
}

// coverage is the graph's own coverage, taken from Extent: the observed-time span of the log and
// each source's gaps. "checkout called payments at 14:32" means one thing when the log covers
// 13:00 to 15:00 with no gaps and quite another when a feeder admits it was disconnected, and a
// graph answer that did not carry that distinction would be read as more certain than it is.
func (w *Worker) coverage(ctx context.Context, searched []string, req worker.Request) (*investigationv1.Coverage, error) {
	extent, err := w.service.Extent(ctx, &graphv1.ExtentRequest{})
	if err != nil {
		return nil, err
	}
	window := &investigationv1.Window{
		Start: extent.GetEarliestObserved(),
		End:   extent.GetLatestObserved(),
	}
	if window.GetStart() == nil || window.GetEnd() == nil || !window.GetEnd().AsTime().After(window.GetStart().AsTime()) {
		// An empty graph still answers, and says so: the window covered is the instant asked
		// about and nothing else.
		at := req.Algebra.GetValidAt()
		if at == nil {
			at = timestamppb.New(time.Unix(0, 0).UTC())
		}
		window = &investigationv1.Window{Start: at, End: timestamppb.New(at.AsTime().Add(time.Second))}
	}

	gaps := 0
	for _, source := range extent.GetSources() {
		gaps += len(source.GetGaps())
	}
	truncation := ""
	if gaps > 0 {
		// A gap is a statement about what was delivered, not about what happened, and it is
		// what turns "no change here" into "no change we were told about" (FR-032).
		truncation = fmt.Sprintf("source_gaps:%d", gaps)
	}
	return engine.CoverageInput{
		SearchedEntities:  searched,
		DataSource:        SourceOfTruth,
		WindowCovered:     window,
		VolumeConsidered:  int64(len(extent.GetSources())),
		Sampling:          "none",
		Truncation:        truncation,
		ExecutedAt:        window.GetEnd().AsTime(),
		QuotaUndetermined: true,
		// The graph has no indexing lag of its own: an event is queryable as soon as it is
		// appended. Where a feeder is behind, that is a source gap, not a lag, and it is
		// reported above.
		IngestionLag: 0,
	}.Coverage()
}

func refString(ref *graphv1.Ref) string {
	if ref == nil {
		return ""
	}
	return ref.GetNamespace() + "=" + ref.GetValue()
}

func executedQuery(term *investigationv1.GraphTerm) string {
	return "sreagent.graph.v1.QueryService/" + queryName(term)
}

func queryName(term *investigationv1.GraphTerm) string {
	switch term.GetTerm().(type) {
	case *investigationv1.GraphTerm_Subgraph:
		return "Subgraph"
	case *investigationv1.GraphTerm_Diff:
		return "Diff"
	case *investigationv1.GraphTerm_Impact:
		return "Impact"
	case *investigationv1.GraphTerm_Pointers:
		return "Pointers"
	case *investigationv1.GraphTerm_NodeHistory:
		return "NodeHistory"
	case *investigationv1.GraphTerm_ResolutionAudit:
		return "ResolutionAudit"
	case *investigationv1.GraphTerm_Extent:
		return "Extent"
	default:
		return "unknown"
	}
}

// EngineService adapts feature 001's in-process query engine to QueryService. It is what a
// fixture replay uses: the graph is replayed from events.jsonl into a database of its own and
// read directly, with no server and no network.
type EngineService struct {
	engine *query.Engine
	log    *eventlog.Log
}

var _ QueryService = (*EngineService)(nil)

// NewEngineService returns the in-process adapter.
func NewEngineService(e *query.Engine) *EngineService {
	return &EngineService{engine: e, log: eventlog.New(e.Store())}
}

// Subgraph implements QueryService.
func (s *EngineService) Subgraph(ctx context.Context, req *graphv1.SubgraphRequest) (*graphv1.SubgraphResponse, error) {
	return s.engine.Subgraph(ctx, req)
}

// Diff implements QueryService.
func (s *EngineService) Diff(ctx context.Context, req *graphv1.DiffRequest) (*graphv1.DiffResponse, error) {
	return s.engine.Diff(ctx, req)
}

// Impact implements QueryService.
func (s *EngineService) Impact(ctx context.Context, req *graphv1.ImpactRequest) (*graphv1.ImpactResponse, error) {
	return s.engine.Impact(ctx, req)
}

// Pointers implements QueryService.
func (s *EngineService) Pointers(ctx context.Context, req *graphv1.PointersRequest) (*graphv1.PointersResponse, error) {
	return s.engine.Pointers(ctx, req)
}

// NodeHistory implements QueryService.
func (s *EngineService) NodeHistory(ctx context.Context, req *graphv1.NodeHistoryRequest) (*graphv1.NodeHistoryResponse, error) {
	return s.engine.NodeHistory(ctx, req)
}

// ResolutionAudit implements QueryService.
func (s *EngineService) ResolutionAudit(ctx context.Context, req *graphv1.ResolutionAuditRequest) (*graphv1.ResolutionAuditResponse, error) {
	return s.engine.ResolutionAudit(ctx, req)
}

// Extent implements QueryService.
func (s *EngineService) Extent(ctx context.Context, _ *graphv1.ExtentRequest) (*graphv1.Extent, error) {
	return s.log.Extent(ctx)
}
