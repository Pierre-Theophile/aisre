// SPDX-License-Identifier: Apache-2.0

package backend

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
)

// The query algebra's mechanics: the version constant, the term name of a typed term, the
// normalisation every term goes through before it is hashed, and the term key a recorded world
// is indexed by (tasks.md T033, contracts/telemetry-backend.md §1, docs/schema/algebra.md).
//
// It lives in the public SDK rather than in the engine because the key is part of the published
// contract: a world recorded by this repository must be readable by a backend written outside
// it, and `sha256(canonical(term))` is only a contract if both sides compute it the same way.
// The engine's own surface — typed constructors, the cross product, the recorded refusal —
// is internal/investigation/backend/algebra.go, which is layered on top of this file.
//
// Two rules make the key well defined:
//
//   - Canonical JSON is feature 001's serializer, unmodified: sorted keys, RFC 3339 UTC,
//     unpopulated fields omitted. The same bytes on every architecture, which is what lets a
//     golden be compared byte for byte.
//   - Instants are normalised to whole seconds and set-valued arguments are sorted and
//     de-duplicated before hashing, so that two spellings of the same question are one key.
//     A world keyed by the spelling rather than the question would answer NOT_RECORDED to a
//     request it in fact holds.

// AlgebraVersion is the version of the published algebra this build implements. It is carried
// on every term, recorded in every world index, and checked on load: a term of an unknown
// version is refused rather than guessed at.
//
// Adding a term is a MINOR bump and obliges a corresponding extension of every recorded world;
// changing the meaning or the encoding of an existing term is a MAJOR bump, because it changes
// every term key and therefore invalidates every world.
const AlgebraVersion = "1.0.0"

// The published term names. They are the wire spelling used in a declaration, a `worker call`
// invocation, a capability and a cost-class table, so they are constants rather than literals
// scattered across the tree.
const (
	// TermSubgraph is the graph family's neighbourhood read.
	TermSubgraph = "subgraph"
	// TermDiff is the graph family's change-between-two-instants read.
	TermDiff = "diff"
	// TermImpact is the graph family's blast-radius read.
	TermImpact = "impact"
	// TermPointers is the graph family's telemetry-selector read.
	TermPointers = "pointers"
	// TermNodeHistory is the graph family's version history read.
	TermNodeHistory = "node_history"
	// TermResolutionAudit is the graph family's identity-resolution read.
	TermResolutionAudit = "resolution_audit"
	// TermExtent is the graph family's coverage read.
	TermExtent = "extent"

	// TermCompare is baseline versus symptom over one selector.
	TermCompare = "compare"
	// TermOnset is the estimated instant the symptom began, computed backend-side.
	TermOnset = "onset"
	// TermNewLogPatterns is mined templates with counts and which are new in the window.
	TermNewLogPatterns = "new_log_patterns"
	// TermErrorSpans is counts and latency statistics on one graph edge.
	TermErrorSpans = "error_spans"
	// TermErrorsByVersion is the error rate split by the deployed version tag.
	TermErrorsByVersion = "errors_by_version"
	// TermMonitorState is transitions, start and end state, per-group states.
	TermMonitorState = "monitor_state"
	// TermExemplars is bounded, sanitised exemplars behind a minted handle.
	TermExemplars = "exemplars"
	// TermDrillDown is the narrower answer behind a minted handle.
	TermDrillDown = "drill_down"

	// TermKnowledgeSearch is graph-scoped retrieval over durable knowledge.
	TermKnowledgeSearch = "knowledge_search"
)

// Rejection reason codes, published so that a refusal can be recorded, rendered and asserted
// on in a fixture rather than only logged. They are the backend half of the codes
// pkg/worker publishes.
const (
	// ReasonOutsideAlgebra is a request whose term the algebra does not publish (FR-042b).
	ReasonOutsideAlgebra = "outside_algebra"
	// ReasonMissingCoverage is a digest with no coverage block (FR-014a).
	ReasonMissingCoverage = "missing_coverage"
	// ReasonUndeclaredRedaction is a backend emitting a field its declaration does not cover
	// (FR-038).
	ReasonUndeclaredRedaction = "undeclared_redaction"
	// ReasonWriteCapability is a backend declaring a capability that changes state in its
	// vendor (FR-016).
	ReasonWriteCapability = "write_capability"
	// ReasonUnknownAlgebraVersion is a term carrying a version this build does not implement.
	ReasonUnknownAlgebraVersion = "unknown_algebra_version"
)

// Rejection is a refusal carrying its published reason code.
type Rejection struct {
	// Reason is one of the published codes above.
	Reason string
	// Detail says what was wrong and, for a refusal a caller can act on, what is available
	// instead.
	Detail string
}

// Error implements error.
func (r *Rejection) Error() string { return r.Reason + ": " + r.Detail }

// Reject builds a Rejection with a formatted detail. It is exported because a backend outside
// this repository refuses under the same published codes.
func Reject(reason, format string, args ...any) error {
	return &Rejection{Reason: reason, Detail: fmt.Sprintf(format, args...)}
}

// ReasonOf returns the published reason code an error carries, or "" for any other error.
func ReasonOf(err error) string {
	var r *Rejection
	if errors.As(err, &r) {
		return r.Reason
	}
	return ""
}

// OutsideAlgebra is the refusal of FR-042b: it names what was asked for and what is available,
// because the caller reading this message is usually reading it instead of the contract. The
// CLI maps it to exit 1 and the engine records it as an evidence item.
func OutsideAlgebra(asked string) error {
	if asked == "" {
		asked = "(no term)"
	}
	return Reject(ReasonOutsideAlgebra,
		"%s is not a published algebra term; the algebra is telemetry %v, graph %v, knowledge %v, and nothing else may be asked of a worker",
		asked, Terms(FamilyTelemetry), Terms(FamilyGraph), Terms(FamilyKnowledge))
}

// TermNameOf returns the published name of a typed term, or "" when the term is empty. A term
// naming a member of the graph oneof returns that member's name, so that "subgraph" and
// "compare" are spelled the same way whichever family they come from.
func TermNameOf(term *investigationv1.AlgebraTerm) string {
	switch t := term.GetTerm().(type) {
	case *investigationv1.AlgebraTerm_Graph:
		return graphTermNameOf(t.Graph)
	case *investigationv1.AlgebraTerm_Compare:
		return TermCompare
	case *investigationv1.AlgebraTerm_Onset:
		return TermOnset
	case *investigationv1.AlgebraTerm_NewLogPatterns:
		return TermNewLogPatterns
	case *investigationv1.AlgebraTerm_ErrorSpans:
		return TermErrorSpans
	case *investigationv1.AlgebraTerm_ErrorsByVersion:
		return TermErrorsByVersion
	case *investigationv1.AlgebraTerm_MonitorState:
		return TermMonitorState
	case *investigationv1.AlgebraTerm_Exemplars:
		return TermExemplars
	case *investigationv1.AlgebraTerm_DrillDown:
		return TermDrillDown
	case *investigationv1.AlgebraTerm_KnowledgeSearch:
		return TermKnowledgeSearch
	default:
		return ""
	}
}

func graphTermNameOf(term *investigationv1.GraphTerm) string {
	switch term.GetTerm().(type) {
	case *investigationv1.GraphTerm_Subgraph:
		return TermSubgraph
	case *investigationv1.GraphTerm_Diff:
		return TermDiff
	case *investigationv1.GraphTerm_Impact:
		return TermImpact
	case *investigationv1.GraphTerm_Pointers:
		return TermPointers
	case *investigationv1.GraphTerm_NodeHistory:
		return TermNodeHistory
	case *investigationv1.GraphTerm_ResolutionAudit:
		return TermResolutionAudit
	case *investigationv1.GraphTerm_Extent:
		return TermExtent
	default:
		return ""
	}
}

// Normalise returns the canonical spelling of term: the algebra version filled in where the
// caller left it empty, every instant truncated to a whole second, and every set-valued
// argument sorted and de-duplicated.
//
// It never mutates its argument. Normalisation is what makes the term key a function of the
// *question* rather than of the spelling, which is the property a recorded world depends on: a
// consumer that asks the same question a different way must hit the same key rather than be
// told NOT_RECORDED about an answer the world holds.
func Normalise(term *investigationv1.AlgebraTerm) (*investigationv1.AlgebraTerm, error) {
	if term == nil || term.GetTerm() == nil {
		return nil, OutsideAlgebra("")
	}
	name := TermNameOf(term)
	if name == "" {
		return nil, OutsideAlgebra("a term with no member set")
	}

	out, ok := proto.Clone(term).(*investigationv1.AlgebraTerm)
	if !ok { // unreachable: proto.Clone returns the dynamic type it was given.
		return nil, fmt.Errorf("backend: normalise %s: clone returned a foreign type", name)
	}
	switch out.GetAlgebraVersion() {
	case "":
		out.AlgebraVersion = AlgebraVersion
	case AlgebraVersion:
	default:
		return nil, Reject(ReasonUnknownAlgebraVersion,
			"term %s carries algebra version %s; this build implements %s, and a term of an unknown version is refused rather than guessed at",
			name, out.GetAlgebraVersion(), AlgebraVersion)
	}

	switch t := out.GetTerm().(type) {
	case *investigationv1.AlgebraTerm_Graph:
		normaliseGraphTerm(t.Graph)
	case *investigationv1.AlgebraTerm_Compare:
		normaliseWindowPair(t.Compare.GetWindows())
	case *investigationv1.AlgebraTerm_Onset:
		normaliseWindow(t.Onset.GetSearchWindow())
	case *investigationv1.AlgebraTerm_NewLogPatterns:
		normaliseWindow(t.NewLogPatterns.GetWindow())
		normaliseWindow(t.NewLogPatterns.GetBaselineWindow())
	case *investigationv1.AlgebraTerm_ErrorSpans:
		normaliseWindow(t.ErrorSpans.GetWindow())
	case *investigationv1.AlgebraTerm_ErrorsByVersion:
		normaliseWindow(t.ErrorsByVersion.GetWindow())
	case *investigationv1.AlgebraTerm_MonitorState:
		normaliseWindow(t.MonitorState.GetWindow())
	case *investigationv1.AlgebraTerm_Exemplars:
	case *investigationv1.AlgebraTerm_DrillDown:
	case *investigationv1.AlgebraTerm_KnowledgeSearch:
		t.KnowledgeSearch.EntityIds = sortedSet(t.KnowledgeSearch.GetEntityIds())
		t.KnowledgeSearch.QueryTerms = sortedSet(lowered(t.KnowledgeSearch.GetQueryTerms()))
	}
	return out, nil
}

func normaliseGraphTerm(term *investigationv1.GraphTerm) {
	// The graph family carries feature 001's own request messages, whose instants live in an
	// AsOf. They are normalised the same way, so that a graph term is keyed like any other even
	// though it is never recorded into a world.
	switch t := term.GetTerm().(type) {
	case *investigationv1.GraphTerm_Subgraph:
		normaliseTimestamp(t.Subgraph.GetAsOf().GetValidAt())
		normaliseTimestamp(t.Subgraph.GetAsOf().GetObservedAt())
	case *investigationv1.GraphTerm_Diff:
		normaliseTimestamp(t.Diff.GetT1())
		normaliseTimestamp(t.Diff.GetT2())
		normaliseTimestamp(t.Diff.GetObservedAt())
		normaliseTimestamp(t.Diff.GetReferenceAt())
		normaliseTimestamp(t.Diff.GetSubgraph().GetAsOf().GetValidAt())
		normaliseTimestamp(t.Diff.GetSubgraph().GetAsOf().GetObservedAt())
	case *investigationv1.GraphTerm_Impact:
		normaliseTimestamp(t.Impact.GetAsOf().GetValidAt())
		normaliseTimestamp(t.Impact.GetAsOf().GetObservedAt())
	case *investigationv1.GraphTerm_Pointers:
		normaliseTimestamp(t.Pointers.GetAsOf().GetValidAt())
		normaliseTimestamp(t.Pointers.GetAsOf().GetObservedAt())
	case *investigationv1.GraphTerm_NodeHistory:
		// NodeHistoryRequest carries no instant: it is the whole history of one node.
	case *investigationv1.GraphTerm_ResolutionAudit:
		normaliseTimestamp(t.ResolutionAudit.GetObservedAt())
	case *investigationv1.GraphTerm_Extent:
	}
}

func normaliseWindowPair(pair *investigationv1.WindowPair) {
	if pair == nil {
		return
	}
	normaliseTimestamp(pair.GetReferenceAt())
	normaliseWindow(pair.GetBaseline())
	normaliseWindow(pair.GetSymptom())
}

func normaliseWindow(w *investigationv1.Window) {
	if w == nil {
		return
	}
	normaliseTimestamp(w.GetStart())
	normaliseTimestamp(w.GetEnd())
}

// normaliseTimestamp truncates ts to a whole second in place. Sub-second precision in a term
// is spurious: no backend resolves a window that finely, and keeping it would split one
// question into arbitrarily many keys.
func normaliseTimestamp(ts *timestamppb.Timestamp) {
	if ts == nil {
		return
	}
	ts.Nanos = 0
}

func lowered(values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		out = append(out, strings.ToLower(strings.TrimSpace(v)))
	}
	return out
}

// sortedSet sorts, de-duplicates and drops empties. Set-valued arguments are sets: asking about
// entities {a, b} and {b, a} is one question and must be one key.
func sortedSet(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v == "" {
			continue
		}
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil
	}
	return out
}

// CanonicalTerm renders the normalised term in feature 001's canonical JSON — the bytes the
// term key hashes and the bytes a diff of a world shows.
func CanonicalTerm(term *investigationv1.AlgebraTerm) ([]byte, error) {
	normalised, err := Normalise(term)
	if err != nil {
		return nil, err
	}
	return graph.CanonicalJSON(normalised)
}

// TermKey is `sha256(canonical(term))` in lowercase hex: the key a recorded world is indexed
// by and the file name under world/ (contracts/incident-format.md).
func TermKey(term *investigationv1.AlgebraTerm) (string, error) {
	canonical, err := CanonicalTerm(term)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// NormaliseForDigest clears the three fields a response's digest is deliberately blind to, and
// is the single published definition of what "the same answer" means.
//
// The three, and why each one is out:
//
//   - `response_digest` itself, so that a response's digest is a function of the answer rather
//     than of itself;
//   - `duration_ms`, because wall-clock time differs between two runs that agree on every fact;
//   - `mode`, because `live` and `recorded` are statements about *how the call was served*, not
//     about the answer. The telemetry-backend contract §5 requires the two modes to produce
//     identical digests for the same term, and a digest that hashed the mode could not: the
//     recording side clears it before hashing (`Recorder.Record`) while every reader stamps its
//     own afterwards (`workers.Base.Call`, `backend.Recorded.Execute`), so the two sides would
//     hash different bytes for the one answer.
//
// It mutates in place and is exported because a third-party backend that computes the digest
// itself must normalise exactly as this does or its recordings will not replay.
func NormaliseForDigest(resp *investigationv1.AlgebraResponse) {
	if resp == nil {
		return
	}
	resp.ResponseDigest = ""
	resp.DurationMs = 0
	resp.Mode = ""
}

// ResponseDigest is `sha256(canonical(response))` over the response normalised by
// NormaliseForDigest, whose comment publishes what is excluded and why.
func ResponseDigest(resp *investigationv1.AlgebraResponse) (string, error) {
	if resp == nil {
		return "", errors.New("backend: response digest of a nil response")
	}
	clone, ok := proto.Clone(resp).(*investigationv1.AlgebraResponse)
	if !ok { // unreachable
		return "", errors.New("backend: response digest: clone returned a foreign type")
	}
	NormaliseForDigest(clone)
	canonical, err := graph.CanonicalJSON(clone)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}
