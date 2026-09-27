// SPDX-License-Identifier: Apache-2.0

package backend

import (
	"fmt"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	sdk "github.com/Pierre-Theophile/aisre/pkg/backend"
)

// The engine's view of the query algebra (tasks.md T033, FR-042b).
//
// The mechanics — the version constant, the term name, normalisation, `term_key =
// sha256(canonical(term))`, the `outside_algebra` refusal — are pkg/backend/algebra.go, because
// the key is part of the published contract and a backend written outside this repository has
// to compute it the same way. This file is what the engine and the workers hold: a typed
// constructor per published term, so that no caller anywhere assembles a oneof by hand, and the
// validation that decides whether a term is answerable at all.
//
// Everything a worker may ask is in this file. That is the point: the algebra is simultaneously
// the replay boundary, the vendor abstraction, the sanitisation point and the injection
// barrier, and a term that can be constructed only here is a term that can be enumerated,
// hashed, recorded and refused.

// Re-exports, so that engine code reads one package rather than two for one concept.
const (
	// AlgebraVersion is the version of the published algebra this build implements.
	AlgebraVersion = sdk.AlgebraVersion
	// ReasonOutsideAlgebra is a request whose term the algebra does not publish.
	ReasonOutsideAlgebra = sdk.ReasonOutsideAlgebra
	// ReasonMissingCoverage is a digest with no coverage block.
	ReasonMissingCoverage = sdk.ReasonMissingCoverage
	// ReasonUndeclaredRedaction is a field emitted that the declaration does not cover.
	ReasonUndeclaredRedaction = sdk.ReasonUndeclaredRedaction
	// ReasonUnknownAlgebraVersion is a term of a version this build does not implement.
	ReasonUnknownAlgebraVersion = sdk.ReasonUnknownAlgebraVersion
	// ReasonNotRecorded is an in-algebra term a world does not hold. It is a published
	// outcome rather than a rejection, and is spelled here so that the two vocabularies do
	// not drift.
	ReasonNotRecorded = "not_recorded"
)

// The two published modes, recorded per call. They are spelled here as well as in pkg/worker
// so that a backend can stamp a response without importing the worker SDK.
const (
	// ModeLive calls the vendor.
	ModeLive = "live"
	// ModeRecorded answers from a world and makes no network call.
	ModeRecorded = "recorded"
)

// Aliases of the published algebra types, so a worker imports this package alone.
type (
	// Term is one fully typed algebra term.
	Term = investigationv1.AlgebraTerm
	// Request is one term with both time dimensions, the hypothesis it serves and the
	// discriminating question it is meant to settle.
	Request = investigationv1.AlgebraRequest
	// Response is the typed outcome, the digest, the term key and the response digest.
	Response = investigationv1.AlgebraResponse
	// Window is a half-open interval.
	Window = investigationv1.Window
	// WindowPair is a baseline and a symptom window around a reference instant.
	WindowPair = investigationv1.WindowPair
	// Handle is minted by a previous answer and never constructed by a caller.
	Handle = investigationv1.Handle
	// Pointer is a telemetry selector published by feature 001.
	Pointer = graphv1.Pointer
)

// The published term names, re-exported so engine code names a term through this package.
const (
	// TermSubgraph is the graph family's neighbourhood read.
	TermSubgraph = sdk.TermSubgraph
	// TermDiff is the graph family's change-between-two-instants read.
	TermDiff = sdk.TermDiff
	// TermImpact is the graph family's blast-radius read.
	TermImpact = sdk.TermImpact
	// TermPointers is the graph family's telemetry-selector read.
	TermPointers = sdk.TermPointers
	// TermNodeHistory is the graph family's version-history read.
	TermNodeHistory = sdk.TermNodeHistory
	// TermResolutionAudit is the graph family's identity-resolution read.
	TermResolutionAudit = sdk.TermResolutionAudit
	// TermExtent is the graph family's coverage read.
	TermExtent = sdk.TermExtent
	// TermCompare is baseline versus symptom over one selector.
	TermCompare = sdk.TermCompare
	// TermOnset is the estimated instant the symptom began.
	TermOnset = sdk.TermOnset
	// TermNewLogPatterns is mined templates with counts.
	TermNewLogPatterns = sdk.TermNewLogPatterns
	// TermErrorSpans is counts and latency statistics on one edge.
	TermErrorSpans = sdk.TermErrorSpans
	// TermErrorsByVersion is the error rate split by deployed version.
	TermErrorsByVersion = sdk.TermErrorsByVersion
	// TermMonitorState is transitions and per-group states.
	TermMonitorState = sdk.TermMonitorState
	// TermExemplars is bounded, sanitised exemplars behind a minted handle.
	TermExemplars = sdk.TermExemplars
	// TermDrillDown is the narrower answer behind a minted handle.
	TermDrillDown = sdk.TermDrillDown
	// TermKnowledgeSearch is graph-scoped retrieval over durable knowledge.
	TermKnowledgeSearch = sdk.TermKnowledgeSearch
)

// TermName returns the published name of a typed term, or "" when it names none.
func TermName(term *Term) string { return sdk.TermNameOf(term) }

// FamilyOf returns the family a published term belongs to.
func FamilyOf(name string) sdk.Family { return sdk.FamilyOf(name) }

// TermKey is the world's key for term: sha256 of its canonical, normalised encoding.
func TermKey(term *Term) (string, error) { return sdk.TermKey(term) }

// Normalise returns the canonical spelling of term.
func Normalise(term *Term) (*Term, error) { return sdk.Normalise(term) }

// OutsideAlgebra is the FR-042b refusal, naming what was asked and what is available.
func OutsideAlgebra(asked string) error { return sdk.OutsideAlgebra(asked) }

// Reject builds a refusal carrying a published reason code.
func Reject(reason, format string, args ...any) error { return sdk.Reject(reason, format, args...) }

// ReasonOf returns the published reason code an error carries.
func ReasonOf(err error) string { return sdk.ReasonOf(err) }

// NewWindow returns the half-open interval [start, end) with both instants truncated to whole
// seconds, which is the resolution the algebra keys on.
func NewWindow(start, end time.Time) *Window {
	return &Window{
		Start: timestamppb.New(start.UTC().Truncate(time.Second)),
		End:   timestamppb.New(end.UTC().Truncate(time.Second)),
	}
}

// NewWindowPair returns the symmetric baseline/symptom pair of width around reference: the
// baseline is [reference-width, reference) and the symptom is [reference, reference+width).
//
// The pair is written out explicitly rather than left implicit in reference_at and
// width_seconds, so that the term key names the exact windows the answer covers and a reader
// of a world file does not have to re-derive them.
func NewWindowPair(reference time.Time, width time.Duration) *WindowPair {
	ref := reference.UTC().Truncate(time.Second)
	return &WindowPair{
		ReferenceAt:  timestamppb.New(ref),
		WidthSeconds: int64(width / time.Second),
		Baseline:     NewWindow(ref.Add(-width), ref),
		Symptom:      NewWindow(ref, ref.Add(width)),
	}
}

// Compare is `compare(pointer, window pair, statistic)`: baseline versus symptom over one
// selector, with direction, magnitude and separability.
func Compare(pointer *Pointer, windows *WindowPair, statistic investigationv1.Statistic) *Term {
	return &Term{
		AlgebraVersion: AlgebraVersion,
		Term: &investigationv1.AlgebraTerm_Compare{Compare: &investigationv1.CompareTerm{
			Pointer:   pointer,
			Windows:   windows,
			Statistic: statistic,
		}},
	}
}

// Onset is `onset(pointer, search window, method)`: the estimated instant the symptom began,
// computed backend-side so the series never crosses the digest boundary.
func Onset(pointer *Pointer, search *Window, method investigationv1.OnsetMethod) *Term {
	return &Term{
		AlgebraVersion: AlgebraVersion,
		Term: &investigationv1.AlgebraTerm_Onset{Onset: &investigationv1.OnsetTerm{
			Pointer:      pointer,
			SearchWindow: search,
			Method:       method,
		}},
	}
}

// NewLogPatterns is `new_log_patterns(pointer, window, baseline window)`: mined templates with
// counts, and which of them are new in the window.
func NewLogPatterns(pointer *Pointer, window, baseline *Window) *Term {
	return &Term{
		AlgebraVersion: AlgebraVersion,
		Term: &investigationv1.AlgebraTerm_NewLogPatterns{NewLogPatterns: &investigationv1.NewLogPatternsTerm{
			Pointer:        pointer,
			Window:         window,
			BaselineWindow: baseline,
		}},
	}
}

// ErrorSpans is `error_spans(edge, window)`: counts and latency statistics by operation and
// error kind on one graph edge.
func ErrorSpans(srcEntityID, dstEntityID string, edgeType graphv1.EdgeType, window *Window) *Term {
	return &Term{
		AlgebraVersion: AlgebraVersion,
		Term: &investigationv1.AlgebraTerm_ErrorSpans{ErrorSpans: &investigationv1.ErrorSpansTerm{
			SrcEntityId: srcEntityID,
			DstEntityId: dstEntityID,
			EdgeType:    edgeType,
			Window:      window,
		}},
	}
}

// ErrorsByVersion is `errors_by_version(pointer, window, version attribute)`: the error rate
// split by the deployed version tag named by Pointer.join_keys["version"] (ADR-0005 D4).
func ErrorsByVersion(pointer *Pointer, window *Window, versionAttribute string) *Term {
	return &Term{
		AlgebraVersion: AlgebraVersion,
		Term: &investigationv1.AlgebraTerm_ErrorsByVersion{ErrorsByVersion: &investigationv1.ErrorsByVersionTerm{
			Pointer:          pointer,
			Window:           window,
			VersionAttribute: versionAttribute,
		}},
	}
}

// MonitorState is `monitor_state(pointer, window)`: transitions, start and end state, and the
// per-group states.
func MonitorState(pointer *Pointer, window *Window) *Term {
	return &Term{
		AlgebraVersion: AlgebraVersion,
		Term: &investigationv1.AlgebraTerm_MonitorState{MonitorState: &investigationv1.MonitorStateTerm{
			Pointer: pointer,
			Window:  window,
		}},
	}
}

// Exemplars is `exemplars(handle, limit)`: bounded, sanitised exemplars, and only on explicit
// request. The handle is minted by a previous answer, never constructed by a caller — which is
// what keeps the argument space finite and therefore a recorded world finite.
func Exemplars(handle *Handle, limit uint32) *Term {
	return &Term{
		AlgebraVersion: AlgebraVersion,
		Term: &investigationv1.AlgebraTerm_Exemplars{Exemplars: &investigationv1.ExemplarsTerm{
			Handle: handle,
			Limit:  limit,
		}},
	}
}

// DrillDown is `drill_down(handle)`: the narrower answer behind a handle a previous digest
// minted.
func DrillDown(handle *Handle) *Term {
	return &Term{
		AlgebraVersion: AlgebraVersion,
		Term: &investigationv1.AlgebraTerm_DrillDown{DrillDown: &investigationv1.DrillDownTerm{
			Handle: handle,
		}},
	}
}

// KnowledgeSearch is `knowledge_search(entity ids, query terms, limit)`, scoped by the graph.
// The entity ids MUST be inside the investigation's subgraph; retrieval over telemetry is
// refused by the knowledge worker (FR-049).
func KnowledgeSearch(entityIDs, queryTerms []string, limit uint32) *Term {
	return &Term{
		AlgebraVersion: AlgebraVersion,
		Term: &investigationv1.AlgebraTerm_KnowledgeSearch{KnowledgeSearch: &investigationv1.KnowledgeSearchTerm{
			EntityIds:  entityIDs,
			QueryTerms: queryTerms,
			Limit:      limit,
		}},
	}
}

// Graph wraps one of feature 001's published query requests as a graph-family term. Both time
// dimensions travel inside the request, exactly as 001 published them.
func Graph(term *investigationv1.GraphTerm) *Term {
	return &Term{
		AlgebraVersion: AlgebraVersion,
		Term:           &investigationv1.AlgebraTerm_Graph{Graph: term},
	}
}

// GraphSubgraph, GraphDiff, GraphImpact, GraphPointers, GraphNodeHistory, GraphResolutionAudit
// and GraphExtent are the seven members of the graph family, one constructor each.
func GraphSubgraph(req *graphv1.SubgraphRequest) *Term {
	return Graph(&investigationv1.GraphTerm{Term: &investigationv1.GraphTerm_Subgraph{Subgraph: req}})
}

// GraphDiff wraps 001's DiffRequest.
func GraphDiff(req *graphv1.DiffRequest) *Term {
	return Graph(&investigationv1.GraphTerm{Term: &investigationv1.GraphTerm_Diff{Diff: req}})
}

// GraphImpact wraps 001's ImpactRequest.
func GraphImpact(req *graphv1.ImpactRequest) *Term {
	return Graph(&investigationv1.GraphTerm{Term: &investigationv1.GraphTerm_Impact{Impact: req}})
}

// GraphPointers wraps 001's PointersRequest.
func GraphPointers(req *graphv1.PointersRequest) *Term {
	return Graph(&investigationv1.GraphTerm{Term: &investigationv1.GraphTerm_Pointers{Pointers: req}})
}

// GraphNodeHistory wraps 001's NodeHistoryRequest.
func GraphNodeHistory(req *graphv1.NodeHistoryRequest) *Term {
	return Graph(&investigationv1.GraphTerm{Term: &investigationv1.GraphTerm_NodeHistory{NodeHistory: req}})
}

// GraphResolutionAudit wraps 001's ResolutionAuditRequest.
func GraphResolutionAudit(req *graphv1.ResolutionAuditRequest) *Term {
	return Graph(&investigationv1.GraphTerm{Term: &investigationv1.GraphTerm_ResolutionAudit{ResolutionAudit: req}})
}

// GraphExtent wraps 001's ExtentRequest.
func GraphExtent(req *graphv1.ExtentRequest) *Term {
	return Graph(&investigationv1.GraphTerm{Term: &investigationv1.GraphTerm_Extent{Extent: req}})
}

// Validate refuses a term that could not be answered, naming what is wrong. It is the check a
// worker runs before it calls its backend and the check `worker call` runs before it issues
// anything, so a malformed term is refused in one place rather than misinterpreted in several.
//
// A term outside the algebra is refused with `outside_algebra`; a published term with a missing
// argument is refused with the same code, because a term that cannot be keyed cannot be
// recorded and a request that cannot be recorded cannot be replayed.
func Validate(term *Term) error {
	name := TermName(term)
	if name == "" {
		return OutsideAlgebra("")
	}
	switch t := term.GetTerm().(type) {
	case *investigationv1.AlgebraTerm_Graph:
		if graphTermName(t.Graph) == "" {
			return OutsideAlgebra("a graph term with no member set")
		}
	case *investigationv1.AlgebraTerm_Compare:
		if err := requirePointer(name, t.Compare.GetPointer()); err != nil {
			return err
		}
		if err := requireWindowPair(name, t.Compare.GetWindows()); err != nil {
			return err
		}
		if t.Compare.GetStatistic() == investigationv1.Statistic_STATISTIC_UNSPECIFIED {
			return badTerm(name, "names no statistic; compare answers one statistic at a time so that the comparison is unambiguous")
		}
	case *investigationv1.AlgebraTerm_Onset:
		if err := requirePointer(name, t.Onset.GetPointer()); err != nil {
			return err
		}
		if err := requireWindow(name, "search_window", t.Onset.GetSearchWindow()); err != nil {
			return err
		}
		if t.Onset.GetMethod() == investigationv1.OnsetMethod_ONSET_METHOD_UNSPECIFIED {
			return badTerm(name, "names no method; the estimate is evidence and evidence states how it was produced (FR-029a)")
		}
	case *investigationv1.AlgebraTerm_NewLogPatterns:
		if err := requirePointer(name, t.NewLogPatterns.GetPointer()); err != nil {
			return err
		}
		if err := requireWindow(name, "window", t.NewLogPatterns.GetWindow()); err != nil {
			return err
		}
		if err := requireWindow(name, "baseline_window", t.NewLogPatterns.GetBaselineWindow()); err != nil {
			return err
		}
	case *investigationv1.AlgebraTerm_ErrorSpans:
		if t.ErrorSpans.GetSrcEntityId() == "" || t.ErrorSpans.GetDstEntityId() == "" {
			return badTerm(name, "names no edge; error_spans is asked about one edge of the graph, not about a service")
		}
		if err := requireWindow(name, "window", t.ErrorSpans.GetWindow()); err != nil {
			return err
		}
	case *investigationv1.AlgebraTerm_ErrorsByVersion:
		if err := requirePointer(name, t.ErrorsByVersion.GetPointer()); err != nil {
			return err
		}
		if err := requireWindow(name, "window", t.ErrorsByVersion.GetWindow()); err != nil {
			return err
		}
		// No version attribute is not a malformed term: it means the pointer's logs carry no
		// version stamp, and the engine answers NO_DATA naming what was searched (unstamped.go;
		// 005 FR-040b, ADR-0010 item 2). It never reaches a backend.
	case *investigationv1.AlgebraTerm_MonitorState:
		if err := requirePointer(name, t.MonitorState.GetPointer()); err != nil {
			return err
		}
		if err := requireWindow(name, "window", t.MonitorState.GetWindow()); err != nil {
			return err
		}
	case *investigationv1.AlgebraTerm_Exemplars:
		if t.Exemplars.GetHandle().GetValue() == "" {
			return badTerm(name, "carries no handle; an exemplar request names a handle a previous answer minted, never a selector a caller composed")
		}
	case *investigationv1.AlgebraTerm_DrillDown:
		if t.DrillDown.GetHandle().GetValue() == "" {
			return badTerm(name, "carries no handle; a drill-down names a handle a previous answer minted")
		}
	case *investigationv1.AlgebraTerm_KnowledgeSearch:
		if len(t.KnowledgeSearch.GetEntityIds()) == 0 {
			return badTerm(name, "names no entities; retrieval is scoped by the graph and an unscoped search would retrieve documents linked to nothing (FR-049)")
		}
	}
	return nil
}

func graphTermName(term *investigationv1.GraphTerm) string {
	return sdk.TermNameOf(Graph(term))
}

func badTerm(name, detail string) error {
	return Reject(ReasonOutsideAlgebra, "term %s %s", name, detail)
}

func requirePointer(name string, p *Pointer) error {
	if p == nil || p.GetSelector() == "" {
		return badTerm(name, "names no pointer; a telemetry term is asked about a selector the graph published, never about free-form query text")
	}
	return nil
}

func requireWindow(name, field string, w *Window) error {
	if w.GetStart() == nil || w.GetEnd() == nil {
		return badTerm(name, fmt.Sprintf("has no %s; every telemetry answer states the window it covers, so every request states the window it asks about", field))
	}
	if !w.GetEnd().AsTime().After(w.GetStart().AsTime()) {
		return badTerm(name, fmt.Sprintf("has an empty or inverted %s [%s, %s); a window is half-open and non-empty",
			field, w.GetStart().AsTime().UTC().Format(time.RFC3339), w.GetEnd().AsTime().UTC().Format(time.RFC3339)))
	}
	return nil
}

func requireWindowPair(name string, pair *WindowPair) error {
	if pair == nil {
		return badTerm(name, "has no window pair; compare is baseline against symptom and needs both")
	}
	if err := requireWindow(name, "baseline", pair.GetBaseline()); err != nil {
		return err
	}
	return requireWindow(name, "symptom", pair.GetSymptom())
}
