// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	backend "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/internal/investigation/ledger"
)

// Turning a tool call into an algebra term (T062, FR-042b, FR-018a).
//
// Everything the model can say is decoded here, once, into the published request messages. A
// field it did not send, a pointer it did not earn, an instant it spelled wrong: each produces a
// refusal that is returned to the model as a tool result naming what was wrong and what is
// available, never an error that ends the run. A model that mistypes a tool argument has made a
// recoverable mistake, and an engine that treats it as fatal has thrown away an investigation
// over a typo.

// Instants is the pair of time dimensions in force for this investigation. Every algebra request
// carries both, always, because an answer without them is an answer to a different question
// (constitution II).
type Instants struct {
	// ValidAt is the instant the world is asked about.
	ValidAt time.Time
	// ObservedAt is the instant the knowledge is pinned to.
	ObservedAt time.Time
}

// DecodeCall turns one tool call into an algebra request, resolving pointers and handles through
// the catalogue.
func DecodeCall(name string, input json.RawMessage, cat *Catalogue, at Instants) (*investigationv1.AlgebraRequest, error) {
	if _, ok := termTools[name]; !ok {
		return nil, &UnknownToolError{Name: name}
	}

	var common struct {
		ServesHypothesisID string `json:"serves_hypothesis_id"`
		Question           string `json:"discriminating_question"`
	}
	// The common fields are read leniently — unknown fields here are the term's own — and the
	// term fields strictly, below.
	if len(input) > 0 {
		if err := json.Unmarshal(input, &common); err != nil {
			return nil, fmt.Errorf("tool %s: input is not an object: %w", name, err)
		}
	}
	if strings.TrimSpace(common.Question) == "" {
		return nil, fmt.Errorf("tool %s: no discriminating question; a call that says nothing about "+
			"why it was made is not made (FR-018a). Use \"exploratory:<reason>\" where there is no hypothesis",
			name)
	}

	term, err := decodeTerm(name, input, cat, at)
	if err != nil {
		return nil, fmt.Errorf("tool %s: %w", name, err)
	}
	term.AlgebraVersion = AlgebraVersion

	return &investigationv1.AlgebraRequest{
		Term:                   term,
		ValidAt:                timestamppb.New(at.ValidAt),
		ObservedAt:             timestamppb.New(at.ObservedAt),
		ServesHypothesisId:     common.ServesHypothesisID,
		DiscriminatingQuestion: common.Question,
		WantExemplars:          name == "exemplars",
	}, nil
}

//nolint:gocyclo // one case per published algebra term; splitting it would hide the total.
func decodeTerm(name string, input json.RawMessage, cat *Catalogue, at Instants) (*investigationv1.AlgebraTerm, error) {
	switch name {
	case "subgraph":
		var in struct {
			EntityRef string `json:"entity_ref"`
			Hops      uint32 `json:"hops"`
			Direction string `json:"direction"`
			termCommon
		}
		if err := decodeInput(input, &in); err != nil {
			return nil, err
		}
		ref, err := parseRef(in.EntityRef)
		if err != nil {
			return nil, err
		}
		return backend.GraphSubgraph(&graphv1.SubgraphRequest{
			Focus:     ref,
			AsOf:      at.AsOf(),
			Hops:      clampHops(in.Hops, 3),
			Direction: parseDirection(in.Direction),
		}), nil

	case "diff":
		var in struct {
			EntityRef   string `json:"entity_ref"`
			Hops        uint32 `json:"hops"`
			T1          string `json:"t1"`
			T2          string `json:"t2"`
			ReferenceAt string `json:"reference_at"`
			termCommon
		}
		if err := decodeInput(input, &in); err != nil {
			return nil, err
		}
		ref, err := parseRef(in.EntityRef)
		if err != nil {
			return nil, err
		}
		t1, err := parseInstant("t1", in.T1)
		if err != nil {
			return nil, err
		}
		t2, err := parseInstant("t2", in.T2)
		if err != nil {
			return nil, err
		}
		req := &graphv1.DiffRequest{
			// The diff's own t1/t2 decide the window and `as_of.valid_at` is ignored for it
			// (graph.proto), but the *observed* instant is not: a diff computed against what is
			// known now, for an investigation pinned to what was known then, is a different diff.
			Subgraph:   &graphv1.SubgraphRequest{Focus: ref, AsOf: at.AsOf(), Hops: clampHops(in.Hops, 3)},
			T1:         timestamppb.New(t1),
			T2:         timestamppb.New(t2),
			ObservedAt: timestamppb.New(at.ObservedAt),
		}
		if in.ReferenceAt != "" {
			reference, err := parseInstant("reference_at", in.ReferenceAt)
			if err != nil {
				return nil, err
			}
			req.ReferenceAt = timestamppb.New(reference)
		}
		return backend.GraphDiff(req), nil

	case "impact":
		var in struct {
			EntityRef string `json:"entity_ref"`
			MaxHops   uint32 `json:"max_hops"`
			termCommon
		}
		if err := decodeInput(input, &in); err != nil {
			return nil, err
		}
		ref, err := parseRef(in.EntityRef)
		if err != nil {
			return nil, err
		}
		req := &graphv1.ImpactRequest{Focus: ref, AsOf: at.AsOf()}
		if in.MaxHops > 0 {
			hops := clampHops(in.MaxHops, 4)
			req.MaxHops = &hops
		}
		return backend.GraphImpact(req), nil

	case "pointers":
		ref, err := singleRef(input)
		if err != nil {
			return nil, err
		}
		return backend.GraphPointers(&graphv1.PointersRequest{Focus: ref, AsOf: at.AsOf()}), nil

	case "node_history":
		ref, err := singleRef(input)
		if err != nil {
			return nil, err
		}
		return backend.GraphNodeHistory(&graphv1.NodeHistoryRequest{Focus: ref}), nil

	case "resolution_audit":
		var in struct {
			A string `json:"entity_ref_a"`
			B string `json:"entity_ref_b"`
			termCommon
		}
		if err := decodeInput(input, &in); err != nil {
			return nil, err
		}
		a, err := parseRef(in.A)
		if err != nil {
			return nil, err
		}
		b, err := parseRef(in.B)
		if err != nil {
			return nil, err
		}
		return backend.GraphResolutionAudit(&graphv1.ResolutionAuditRequest{
			A: a, B: b, ObservedAt: timestamppb.New(at.ObservedAt),
		}), nil

	case "extent":
		return backend.GraphExtent(&graphv1.ExtentRequest{}), nil

	case "compare":
		var in struct {
			PointerID    string `json:"pointer_id"`
			ReferenceAt  string `json:"reference_at"`
			WidthSeconds int64  `json:"width_seconds"`
			Statistic    string `json:"statistic"`
			termCommon
		}
		if err := decodeInput(input, &in); err != nil {
			return nil, err
		}
		pointer, err := cat.Pointer(in.PointerID)
		if err != nil {
			return nil, err
		}
		reference, err := parseInstant("reference_at", in.ReferenceAt)
		if err != nil {
			return nil, err
		}
		if in.WidthSeconds <= 0 {
			return nil, fmt.Errorf("width_seconds is %d; a window of no width measures nothing", in.WidthSeconds)
		}
		statistic, err := parseStatistic(in.Statistic)
		if err != nil {
			return nil, err
		}
		return backend.Compare(pointer,
			backend.NewWindowPair(reference, time.Duration(in.WidthSeconds)*time.Second), statistic), nil

	case "onset":
		var in struct {
			PointerID   string `json:"pointer_id"`
			SearchStart string `json:"search_start"`
			SearchEnd   string `json:"search_end"`
			termCommon
		}
		if err := decodeInput(input, &in); err != nil {
			return nil, err
		}
		pointer, err := cat.Pointer(in.PointerID)
		if err != nil {
			return nil, err
		}
		window, err := parseWindow("search_start", in.SearchStart, "search_end", in.SearchEnd)
		if err != nil {
			return nil, err
		}
		return backend.Onset(pointer, window, investigationv1.OnsetMethod_SEASONAL_CUSUM), nil

	case "new_log_patterns":
		var in struct {
			PointerID     string `json:"pointer_id"`
			WindowStart   string `json:"window_start"`
			WindowEnd     string `json:"window_end"`
			BaselineStart string `json:"baseline_start"`
			BaselineEnd   string `json:"baseline_end"`
			termCommon
		}
		if err := decodeInput(input, &in); err != nil {
			return nil, err
		}
		pointer, err := cat.Pointer(in.PointerID)
		if err != nil {
			return nil, err
		}
		window, err := parseWindow("window_start", in.WindowStart, "window_end", in.WindowEnd)
		if err != nil {
			return nil, err
		}
		baseline, err := parseWindow("baseline_start", in.BaselineStart, "baseline_end", in.BaselineEnd)
		if err != nil {
			return nil, err
		}
		return backend.NewLogPatterns(pointer, window, baseline), nil

	case "error_spans":
		var in struct {
			Src         string `json:"src_entity_ref"`
			Dst         string `json:"dst_entity_ref"`
			EdgeType    string `json:"edge_type"`
			WindowStart string `json:"window_start"`
			WindowEnd   string `json:"window_end"`
			termCommon
		}
		if err := decodeInput(input, &in); err != nil {
			return nil, err
		}
		window, err := parseWindow("window_start", in.WindowStart, "window_end", in.WindowEnd)
		if err != nil {
			return nil, err
		}
		edge, err := parseEdgeType(in.EdgeType)
		if err != nil {
			return nil, err
		}
		// `ErrorSpansTerm` names its two ends by **canonical entity id**, which is what the graph
		// calls an edge's ends and what a recorded world is keyed by. The tool takes references,
		// because a reference is what a person and a model write. The translation is the
		// catalogue's, learned from the node versions the graph already returned: passing a
		// reference through under a field named `entity_id` would key the term to a string no
		// backend and no recording has ever used for that entity.
		return backend.ErrorSpans(cat.entityIDOr(in.Src), cat.entityIDOr(in.Dst), edge, window), nil

	case "errors_by_version":
		var in struct {
			PointerID   string `json:"pointer_id"`
			WindowStart string `json:"window_start"`
			WindowEnd   string `json:"window_end"`
			termCommon
		}
		if err := decodeInput(input, &in); err != nil {
			return nil, err
		}
		pointer, err := cat.Pointer(in.PointerID)
		if err != nil {
			return nil, err
		}
		window, err := parseWindow("window_start", in.WindowStart, "window_end", in.WindowEnd)
		if err != nil {
			return nil, err
		}
		attribute := pointer.GetJoinKeys()[JoinRoleVersion]
		if attribute == "" {
			return nil, fmt.Errorf("pointer %s declares no %q join key, so there is no attribute to "+
				"split the error rate by; splitting by the wrong tag produces a confident wrong answer",
				in.PointerID, JoinRoleVersion)
		}
		return backend.ErrorsByVersion(pointer, window, attribute), nil

	case "monitor_state":
		var in struct {
			PointerID   string `json:"pointer_id"`
			WindowStart string `json:"window_start"`
			WindowEnd   string `json:"window_end"`
			termCommon
		}
		if err := decodeInput(input, &in); err != nil {
			return nil, err
		}
		pointer, err := cat.Pointer(in.PointerID)
		if err != nil {
			return nil, err
		}
		window, err := parseWindow("window_start", in.WindowStart, "window_end", in.WindowEnd)
		if err != nil {
			return nil, err
		}
		return backend.MonitorState(pointer, window), nil

	case "exemplars":
		var in struct {
			HandleID string `json:"handle_id"`
			Limit    uint32 `json:"limit"`
			termCommon
		}
		if err := decodeInput(input, &in); err != nil {
			return nil, err
		}
		handle, err := cat.Handle(in.HandleID)
		if err != nil {
			return nil, err
		}
		limit := in.Limit
		if limit == 0 || limit > backend.MaxExemplars {
			limit = backend.MaxExemplars
		}
		return backend.Exemplars(handle, limit), nil

	case "drill_down":
		var in struct {
			HandleID string `json:"handle_id"`
			termCommon
		}
		if err := decodeInput(input, &in); err != nil {
			return nil, err
		}
		handle, err := cat.Handle(in.HandleID)
		if err != nil {
			return nil, err
		}
		return backend.DrillDown(handle), nil

	case "knowledge_search":
		var in struct {
			EntityRefs []string `json:"entity_refs"`
			QueryTerms []string `json:"query_terms"`
			Limit      uint32   `json:"limit"`
			termCommon
		}
		if err := decodeInput(input, &in); err != nil {
			return nil, err
		}
		ids := make([]string, 0, len(in.EntityRefs))
		for _, ref := range in.EntityRefs {
			if _, err := parseRef(ref); err != nil {
				return nil, err
			}
			ids = append(ids, ref)
		}
		limit := in.Limit
		if limit == 0 || limit > backend.MaxKnowledgeItems {
			limit = backend.MaxKnowledgeItems
		}
		return backend.KnowledgeSearch(ids, in.QueryTerms, limit), nil

	default:
		return nil, &UnknownToolError{Name: name}
	}
}

// termCommon is the two fields every term tool carries. It is embedded so that the strict decode
// of a term's own fields does not reject them.
type termCommon struct {
	ServesHypothesisID string `json:"serves_hypothesis_id"`
	Question           string `json:"discriminating_question"`
}

// JoinRoleVersion names the attribute carrying the deployed version, from the published role set
// (ADR-0005 D6, docs/schema/pointers.md).
const JoinRoleVersion = "version"

func singleRef(input json.RawMessage) (*graphv1.Ref, error) {
	var in struct {
		EntityRef string `json:"entity_ref"`
		termCommon
	}
	if err := decodeInput(input, &in); err != nil {
		return nil, err
	}
	return parseRef(in.EntityRef)
}

// parseRef reads "namespace=value", the published rendering of a Ref.
func parseRef(ref string) (*graphv1.Ref, error) {
	namespace, value, ok := strings.Cut(strings.TrimSpace(ref), "=")
	if !ok || namespace == "" || value == "" {
		return nil, fmt.Errorf("entity reference %q is not \"namespace=value\", "+
			"e.g. \"otel.service.name=checkout\"", ref)
	}
	return &graphv1.Ref{Namespace: namespace, Value: value}, nil
}

// RefString renders a Ref the way the graph and the fixtures spell it.
func RefString(ref *graphv1.Ref) string {
	if ref == nil {
		return ""
	}
	return ref.GetNamespace() + "=" + ref.GetValue()
}

func parseInstant(field, value string) (time.Time, error) {
	at, err := time.Parse(time.RFC3339, strings.TrimSpace(value))
	if err != nil {
		return time.Time{}, fmt.Errorf("%s = %q is not an RFC 3339 instant, e.g. \"2026-09-01T14:20:00Z\"",
			field, value)
	}
	return at.UTC(), nil
}

func parseWindow(startField, start, endField, end string) (*investigationv1.Window, error) {
	from, err := parseInstant(startField, start)
	if err != nil {
		return nil, err
	}
	to, err := parseInstant(endField, end)
	if err != nil {
		return nil, err
	}
	if !to.After(from) {
		return nil, fmt.Errorf("%s (%s) is not after %s (%s); a window is half-open [start, end)",
			endField, end, startField, start)
	}
	return backend.NewWindow(from, to), nil
}

func clampHops(hops, ceiling uint32) uint32 {
	if hops == 0 {
		return 1
	}
	if hops > ceiling {
		return ceiling
	}
	return hops
}

func parseDirection(name string) graphv1.Direction {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "upstream":
		return graphv1.Direction_UPSTREAM
	case "downstream":
		return graphv1.Direction_DOWNSTREAM
	default:
		return graphv1.Direction_BOTH
	}
}

var statistics = map[string]investigationv1.Statistic{
	"count":      investigationv1.Statistic_COUNT,
	"rate":       investigationv1.Statistic_RATE,
	"error_rate": investigationv1.Statistic_ERROR_RATE,
	"p50":        investigationv1.Statistic_P50,
	"p95":        investigationv1.Statistic_P95,
	"p99":        investigationv1.Statistic_P99,
	"mean":       investigationv1.Statistic_MEAN,
	"max":        investigationv1.Statistic_MAX,
}

func parseStatistic(name string) (investigationv1.Statistic, error) {
	s, ok := statistics[strings.ToLower(strings.TrimSpace(name))]
	if !ok {
		return investigationv1.Statistic_STATISTIC_UNSPECIFIED,
			fmt.Errorf("statistic %q is not published; the set is count, rate, error_rate, p50, p95, p99, mean, max", name)
	}
	return s, nil
}

var edgeTypes = map[string]graphv1.EdgeType{
	"calls":       graphv1.EdgeType_CALLS,
	"depends_on":  graphv1.EdgeType_DEPENDS_ON,
	"runs_on":     graphv1.EdgeType_RUNS_ON,
	"deployed_by": graphv1.EdgeType_DEPLOYED_BY,
	"exposed_via": graphv1.EdgeType_EXPOSED_VIA,
	"changed_by":  graphv1.EdgeType_CHANGED_BY,
}

func parseEdgeType(name string) (graphv1.EdgeType, error) {
	e, ok := edgeTypes[strings.ToLower(strings.TrimSpace(name))]
	if !ok {
		return graphv1.EdgeType_EDGE_TYPE_UNSPECIFIED,
			fmt.Errorf("edge type %q is not one the tool publishes", name)
	}
	return e, nil
}

// ProposedJudgment is one judgment the model proposed, before the engine validated it.
//
// It carries no number and there is nowhere to put one: the engine maps (direction, strength) to
// the published likelihood ratio and recomputes the posterior itself (FR-023).
type ProposedJudgment struct {
	// HypothesisID and EvidenceID are the pair.
	HypothesisID string `json:"hypothesis_id"`
	EvidenceID   string `json:"evidence_id"`
	// Direction is supports, refutes or neutral.
	Direction string `json:"direction"`
	// Strength is on the published five-point scale, or "none" for a neutral judgment.
	Strength string `json:"strength"`
	// Rationale is one sentence about what in the cited digest moves this hypothesis.
	Rationale string `json:"rationale"`
}

// DecodeJudgments reads a `propose_judgments` call.
func DecodeJudgments(input json.RawMessage) ([]ProposedJudgment, error) {
	var in struct {
		Judgments []ProposedJudgment `json:"judgments"`
	}
	if err := decodeInput(input, &in); err != nil {
		return nil, fmt.Errorf("tool %s: %w", ToolProposeJudgments, err)
	}
	if len(in.Judgments) == 0 {
		return nil, fmt.Errorf("tool %s: an empty batch of judgments; a turn that proposes nothing is "+
			"recorded as such rather than sent as an empty call", ToolProposeJudgments)
	}
	return in.Judgments, nil
}

// LedgerDirection maps the proposed direction onto the ledger's vocabulary.
func (p ProposedJudgment) LedgerDirection() (ledger.Direction, error) {
	switch strings.ToLower(strings.TrimSpace(p.Direction)) {
	case string(ledger.Supports):
		return ledger.Supports, nil
	case string(ledger.Refutes):
		return ledger.Refutes, nil
	case string(ledger.Neutral):
		return ledger.Neutral, nil
	default:
		return "", fmt.Errorf("direction %q is not supports, refutes or neutral", p.Direction)
	}
}

// LedgerStrength maps the proposed strength onto the ledger's published scale.
func (p ProposedJudgment) LedgerStrength(direction ledger.Direction) (ledger.Strength, error) {
	name := strings.ToLower(strings.TrimSpace(p.Strength))
	if direction == ledger.Neutral || name == "none" || name == "" {
		return "", nil
	}
	switch name {
	case string(ledger.Weak):
		return ledger.Weak, nil
	case string(ledger.Moderate):
		return ledger.Moderate, nil
	case string(ledger.Strong):
		return ledger.Strong, nil
	case string(ledger.Decisive):
		return ledger.Decisive, nil
	default:
		return "", fmt.Errorf("strength %q is not on the published scale weak, moderate, strong, decisive", p.Strength)
	}
}

// ProposedHypothesis is one hypothesis the model proposed. It carries no prior: the engine
// assigns it from the published ranker score or the published condition prior (FR-020a).
type ProposedHypothesis struct {
	// Kind is "change" or "condition".
	Kind string `json:"kind"`
	// Statement is the claim in plain language.
	Statement string `json:"statement"`
	// CandidateChangeEntityRef names the change a change hypothesis is about.
	CandidateChangeEntityRef string `json:"candidate_change_entity_ref"`
	// TargetEntityRefs are the entities it concerns.
	TargetEntityRefs []string `json:"target_entity_refs"`
}

// DecodeHypothesis reads a `propose_hypothesis` call.
func DecodeHypothesis(input json.RawMessage) (ProposedHypothesis, error) {
	var in ProposedHypothesis
	if err := decodeInput(input, &in); err != nil {
		return ProposedHypothesis{}, fmt.Errorf("tool %s: %w", ToolProposeHypothesis, err)
	}
	switch strings.ToLower(strings.TrimSpace(in.Kind)) {
	case string(ledger.KindChange):
		in.Kind = string(ledger.KindChange)
		if in.CandidateChangeEntityRef == "" {
			return ProposedHypothesis{}, fmt.Errorf(
				"tool %s: a change hypothesis names the change entity it is about", ToolProposeHypothesis)
		}
	case string(ledger.KindCondition):
		in.Kind = string(ledger.KindCondition)
	default:
		return ProposedHypothesis{}, fmt.Errorf("tool %s: kind %q is neither change nor condition",
			ToolProposeHypothesis, in.Kind)
	}
	if strings.TrimSpace(in.Statement) == "" {
		return ProposedHypothesis{}, fmt.Errorf("tool %s: a hypothesis carries a statement in plain language",
			ToolProposeHypothesis)
	}
	return in, nil
}

// jsonObject marshals a tool-input map for the engine's own calls. The engine builds the same
// requests the model builds, through the same decoder, so that a deterministic first-wave call and
// a model-proposed call cannot diverge in how they are formed.
func jsonObject(fields map[string]any) json.RawMessage {
	raw, err := json.Marshal(fields)
	if err != nil {
		// The engine builds these maps from its own typed values; a failure here would be a
		// programming error, and an empty object produces a refusal that names the missing field.
		return json.RawMessage("{}")
	}
	return raw
}

// AsOf is the pair of instants as the graph's own reads take them. Every graph read carries both:
// a read with no `valid_at` is a read of "now" answering a question about "then", which is the
// confusion the bitemporal model exists to prevent, and feature 001's graph refuses it outright.
func (i Instants) AsOf() *graphv1.AsOf {
	return &graphv1.AsOf{
		ValidAt:    timestamppb.New(i.ValidAt),
		ObservedAt: timestamppb.New(i.ObservedAt),
	}
}

// entityIDOr resolves a reference to the canonical entity id the graph gave it, falling back to
// the reference itself when no answer has described that entity yet.
//
// The fallback is deliberate and it is not a silent guess: an unresolved reference produces a
// term the backend answers NO_DATA for, naming the edge it does not hold, which is the honest
// answer to "what do the spans on this edge say" when the graph has never described the edge.
// Refusing the call instead would deny the model a question it is entitled to ask.
func (c *Catalogue) entityIDOr(ref string) string {
	if id := c.EntityIDFor(ref); id != "" {
		return id
	}
	return ref
}
