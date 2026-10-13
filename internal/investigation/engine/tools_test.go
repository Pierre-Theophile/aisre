// SPDX-License-Identifier: Apache-2.0

package engine_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/investigation/engine"
	"github.com/Pierre-Theophile/aisre/internal/investigation/ledger"
	sdk "github.com/Pierre-Theophile/aisre/pkg/backend"
)

// The tool set (tasks.md T062; FR-023, FR-020a, contracts/prompting.md).
//
// The contract is arithmetic: one tool per algebra term, plus exactly two engine tools. What the
// tests below actually protect is the *absence* of four tools — one that writes a confidence, one
// that re-ranks, one that reaches a source and one that changes a budget — because each of those
// is a tool somebody will eventually be tempted to add.

// TestTheToolSetIsOnePerTermPlusExactlyTwo.
func TestTheToolSetIsOnePerTermPlusExactlyTwo(t *testing.T) {
	t.Parallel()

	var terms []string
	for _, family := range []sdk.Family{sdk.FamilyGraph, sdk.FamilyTelemetry, sdk.FamilyKnowledge} {
		terms = append(terms, sdk.Terms(family)...)
	}

	names := engine.ToolNames()
	if len(names) != len(terms)+2 {
		t.Fatalf("tools = %d, want %d terms + exactly 2 engine tools; got %v",
			len(names), len(terms), names)
	}

	have := map[string]bool{}
	for _, name := range names {
		have[name] = true
	}
	for _, term := range terms {
		if !have[term] {
			t.Errorf("the algebra publishes %s and the tool set does not offer it", term)
		}
	}
	if !have[engine.ToolProposeJudgments] || !have[engine.ToolProposeHypothesis] {
		t.Errorf("the two engine tools are not both present: %v", names)
	}
	for _, forbidden := range []string{
		"set_confidence", "rank", "rerank", "query", "raw_query", "set_budget", "conclude_now",
	} {
		if have[forbidden] {
			t.Errorf("the tool set offers %s; no tool writes a confidence, re-ranks, reaches a "+
				"source or changes a budget", forbidden)
		}
	}
}

// TestTheToolSetIsStableAcrossCalls: the tool list is part of the cached prefix, so an unsorted
// map iteration here would invalidate the cache on every turn (FR-061).
func TestTheToolSetIsStableAcrossCalls(t *testing.T) {
	t.Parallel()

	first, err := json.Marshal(engine.ToolSet())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for i := 0; i < 20; i++ {
		next, err := json.Marshal(engine.ToolSet())
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if string(next) != string(first) {
			t.Fatalf("the tool set changed between call 1 and call %d", i+2)
		}
	}
}

// TestEveryToolSchemaIsClosed: `strict: true` needs `additionalProperties: false` and a required
// list, and every term tool requires its discriminating question (FR-018a).
func TestEveryToolSchemaIsClosed(t *testing.T) {
	t.Parallel()

	for _, tool := range engine.ToolSet() {
		schema := tool.Schema()
		if closed, ok := schema["additionalProperties"].(bool); !ok || closed {
			t.Errorf("%s: schema is not closed with additionalProperties: false", tool.Name)
		}
		if _, ok := schema["required"].([]string); !ok {
			t.Errorf("%s: schema has no required list", tool.Name)
		}
		if tool.Description == "" {
			t.Errorf("%s: no description", tool.Name)
		}
		if engine.IsEngineTool(tool.Name) {
			continue
		}
		required := map[string]bool{}
		for _, name := range tool.Required {
			required[name] = true
		}
		if !required["discriminating_question"] {
			t.Errorf("%s: the discriminating question is not required; a call that says nothing "+
				"about why it was made is not made (FR-018a)", tool.Name)
		}
		if _, ok := tool.Properties["serves_hypothesis_id"]; !ok {
			t.Errorf("%s: no serves_hypothesis_id property", tool.Name)
		}
	}
}

// TestNoToolTakesAConfidence: there is nowhere for a model-stated number to go (FR-023).
func TestNoToolTakesAConfidence(t *testing.T) {
	t.Parallel()

	for _, tool := range engine.ToolSet() {
		for property, schema := range tool.Properties {
			lowered := strings.ToLower(property)
			for _, forbidden := range []string{"confidence", "probability", "posterior", "prior", "score", "ln_lr"} {
				if strings.Contains(lowered, forbidden) {
					t.Errorf("%s takes %q; the engine computes every number (FR-023)", tool.Name, property)
				}
			}
			if typed, ok := schema.(map[string]any); ok {
				if kind, _ := typed["type"].(string); kind == "number" {
					t.Errorf("%s takes a number in %q; strengths are a published scale, not a number",
						tool.Name, property)
				}
			}
		}
	}
}

// TestProposeJudgmentsTakesTheTypedTuple: direction and strength come from published enums, and
// each judgment names the evidence it rests on (FR-020a).
func TestProposeJudgmentsTakesTheTypedTuple(t *testing.T) {
	t.Parallel()

	tool, ok := engine.ToolByName(engine.ToolProposeJudgments)
	if !ok {
		t.Fatal("propose_judgments is not in the tool set")
	}
	judgments, ok := tool.Properties["judgments"].(map[string]any)
	if !ok {
		t.Fatalf("propose_judgments has no judgments array: %+v", tool.Properties)
	}
	item, ok := judgments["items"].(map[string]any)
	if !ok {
		t.Fatalf("judgments has no item schema")
	}
	properties, _ := item["properties"].(map[string]any)
	for _, field := range []string{"hypothesis_id", "evidence_id", "direction", "strength", "rationale"} {
		if _, ok := properties[field]; !ok {
			t.Errorf("a judgment has no %s", field)
		}
	}
	direction, _ := properties["direction"].(map[string]any)
	values, _ := direction["enum"].([]any)
	if len(values) != 3 {
		t.Errorf("direction enum = %v, want supports, refutes, neutral", values)
	}
}

// TestAPointerIsNeverComposed: a tool takes a pointer *id*, and an id the catalogue does not hold
// is refused naming what is available (FR-029).
func TestAPointerIsNeverComposed(t *testing.T) {
	t.Parallel()

	cat := engine.NewCatalogue()
	at := engine.Instants{ValidAt: time.Now().UTC(), ObservedAt: time.Now().UTC()}

	_, err := engine.DecodeCall("compare", jsonInput(map[string]any{
		"pointer_id":              "ptr-invented",
		"reference_at":            "2026-09-01T14:21:30Z",
		"width_seconds":           900,
		"statistic":               "error_rate",
		"discriminating_question": "did it move?",
	}), cat, at)
	if err == nil {
		t.Fatal("a pointer nobody obtained was accepted")
	}
	if !strings.Contains(err.Error(), "ask `pointers`") {
		t.Errorf("the refusal does not say how to obtain one: %v", err)
	}

	id := cat.AddPointer("otel.service.name=payments", &graphv1.Pointer{
		Kind:        graphv1.PointerKind_METRIC,
		BackendKind: "prometheus",
		Selector:    `http_errors{service="payments"}`,
		JoinKeys:    map[string]string{"version": "service.version"},
	})
	req, err := engine.DecodeCall("compare", jsonInput(map[string]any{
		"pointer_id":              id,
		"reference_at":            "2026-09-01T14:21:30Z",
		"width_seconds":           900,
		"statistic":               "error_rate",
		"discriminating_question": "did it move?",
	}), cat, at)
	if err != nil {
		t.Fatalf("a catalogued pointer was refused: %v", err)
	}
	if req.GetTerm().GetCompare().GetPointer().GetSelector() == "" {
		t.Error("the pointer did not travel into the term")
	}
	// A term carries the version that introduced it, so a 1.0.0 question keeps its 1.0.0 key
	// (ADR-0012).
	if req.GetTerm().GetAlgebraVersion() != sdk.BaseAlgebraVersion {
		t.Errorf("the term carries algebra version %q", req.GetTerm().GetAlgebraVersion())
	}
}

// TestTheChatFamilyIsOfferedOnlyWhenAsked (006 FR-030, ADR-0012): without the chat family the
// tool set is the 1.0.0 set byte for byte, which is what keeps every recorded trajectory
// replaying; with it, the two chat tools are added and nothing else moves.
func TestTheChatFamilyIsOfferedOnlyWhenAsked(t *testing.T) {
	t.Parallel()

	base, _ := json.Marshal(engine.ToolSet())
	off, _ := json.Marshal(engine.ToolSetFor(false))
	if string(base) != string(off) {
		t.Fatal("ToolSetFor(false) is not ToolSet()")
	}
	names := map[string]bool{}
	for _, tool := range engine.ToolSetFor(true) {
		names[tool.Name] = true
		if _, takesChannel := tool.Properties["channel_ref"]; takesChannel {
			t.Errorf("%s lets the model name a channel; the engine fills it", tool.Name)
		}
	}
	for _, term := range sdk.Terms(sdk.FamilyChat) {
		if !names[term] {
			t.Errorf("the chat tool set does not offer %s", term)
		}
	}
	if len(names) != len(engine.ToolNames())+len(sdk.Terms(sdk.FamilyChat)) {
		t.Errorf("the chat tool set has %d tools, want the base set plus the chat family", len(names))
	}
	for _, name := range engine.ToolNames() {
		if !names[name] {
			t.Errorf("offering chat dropped %s", name)
		}
	}
}

// TestAChatCallReadsOnlyTheDeclaredChannel (006 FR-031): a chat call is unknown to an
// investigation with no declared channel, names the declared one otherwise, and cannot name
// another.
func TestAChatCallReadsOnlyTheDeclaredChannel(t *testing.T) {
	t.Parallel()

	at := engine.Instants{
		ValidAt:    time.Date(2026, 10, 14, 9, 30, 0, 0, time.UTC),
		ObservedAt: time.Date(2026, 10, 14, 9, 30, 0, 0, time.UTC),
	}
	call := map[string]any{
		"term_class":              "rollback",
		"window_start":            "2026-10-14T09:00:00Z",
		"window_end":              "2026-10-14T09:30:00Z",
		"discriminating_question": "did anyone roll back?",
	}

	cat := engine.NewCatalogue()
	_, err := engine.DecodeCall("chat_mentions", jsonInput(call), cat, at)
	var unknown *engine.UnknownToolError
	if !errors.As(err, &unknown) {
		t.Fatalf("a chat call without a declared channel: %v, want an unknown tool", err)
	}

	cat.SetChatChannel("slack:T0TWIN/C0SEV2")
	req, err := engine.DecodeCall("chat_mentions", jsonInput(call), cat, at)
	if err != nil {
		t.Fatalf("chat_mentions: %v", err)
	}
	if got := req.GetTerm().GetChatMentions().GetChannelRef(); got != "slack:T0TWIN/C0SEV2" {
		t.Errorf("channel %q, want the declared one", got)
	}
	if req.GetTerm().GetAlgebraVersion() != sdk.AlgebraVersion {
		t.Errorf("a chat term carries %q, want %s", req.GetTerm().GetAlgebraVersion(), sdk.AlgebraVersion)
	}

	call["channel_ref"] = "slack:T0TWIN/C0OTHER"
	if _, err := engine.DecodeCall("chat_mentions", jsonInput(call), cat, at); err == nil {
		t.Error("the model named a channel and the call was accepted")
	}

	timeline, err := engine.DecodeCall("chat_timeline", jsonInput(map[string]any{
		"window_start":            "2026-10-14T09:00:00Z",
		"window_end":              "2026-10-14T09:30:00Z",
		"bucket_seconds":          300,
		"discriminating_question": "when did people notice?",
	}), cat, at)
	if err != nil {
		t.Fatalf("chat_timeline: %v", err)
	}
	if timeline.GetTerm().GetChatTimeline().GetChannelRef() != "slack:T0TWIN/C0SEV2" {
		t.Errorf("timeline channel %q", timeline.GetTerm().GetChatTimeline().GetChannelRef())
	}
}

// TestACallWithoutItsPurposeIsRefused (FR-018a).
func TestACallWithoutItsPurposeIsRefused(t *testing.T) {
	t.Parallel()

	cat := engine.NewCatalogue()
	at := engine.Instants{ValidAt: time.Now().UTC(), ObservedAt: time.Now().UTC()}

	_, err := engine.DecodeCall("pointers", jsonInput(map[string]any{
		"entity_ref": "otel.service.name=checkout",
	}), cat, at)
	if err == nil || !strings.Contains(err.Error(), "discriminating question") {
		t.Fatalf("a call with no discriminating question was accepted: %v", err)
	}

	if _, err := engine.DecodeCall("pointers", jsonInput(map[string]any{
		"entity_ref":              "otel.service.name=checkout",
		"discriminating_question": "exploratory: widening the neighbourhood",
	}), cat, at); err != nil {
		t.Fatalf("an exploratory call was refused: %v", err)
	}
}

// TestErrorsByVersionRefusesAPointerWithNoVersionJoinKey: splitting by the wrong tag produces a
// confident wrong answer, which is worse than no answer (ADR-0005 D4).
func TestErrorsByVersionRefusesAPointerWithNoVersionJoinKey(t *testing.T) {
	t.Parallel()

	cat := engine.NewCatalogue()
	at := engine.Instants{ValidAt: time.Now().UTC(), ObservedAt: time.Now().UTC()}
	id := cat.AddPointer("otel.service.name=payments", &graphv1.Pointer{
		Kind: graphv1.PointerKind_METRIC, Selector: "errors", BackendKind: "prometheus",
	})

	_, err := engine.DecodeCall("errors_by_version", jsonInput(map[string]any{
		"pointer_id":              id,
		"window_start":            "2026-09-01T14:20:00Z",
		"window_end":              "2026-09-01T14:35:00Z",
		"discriminating_question": "is it one version?",
	}), cat, at)
	if err == nil || !strings.Contains(err.Error(), "join key") {
		t.Fatalf("a version split with no version tag was accepted: %v", err)
	}
}

// TestAJudgmentDecodesToThePublishedVocabulary.
func TestAJudgmentDecodesToThePublishedVocabulary(t *testing.T) {
	t.Parallel()

	proposed, err := engine.DecodeJudgments(jsonInput(map[string]any{
		"judgments": []map[string]any{
			{"hypothesis_id": "h-1", "evidence_id": "e-4", "direction": "supports",
				"strength": "strong", "rationale": "rev7 carries 96% of the errors"},
			{"hypothesis_id": "h-2", "evidence_id": "e-5", "direction": "neutral",
				"strength": "none", "rationale": "the windows do not separate it"},
		},
	}))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(proposed) != 2 {
		t.Fatalf("judgments = %d, want 2", len(proposed))
	}
	direction, err := proposed[0].LedgerDirection()
	if err != nil || direction != ledger.Supports {
		t.Fatalf("direction = %v (%v)", direction, err)
	}
	strength, err := proposed[0].LedgerStrength(direction)
	if err != nil || strength != ledger.Strong {
		t.Fatalf("strength = %v (%v)", strength, err)
	}

	neutral, err := proposed[1].LedgerDirection()
	if err != nil || neutral != ledger.Neutral {
		t.Fatalf("neutral direction = %v (%v)", neutral, err)
	}
	if s, err := proposed[1].LedgerStrength(neutral); err != nil || s != "" {
		t.Errorf("a neutral judgment carries strength %q; it moves nothing", s)
	}

	if _, err := engine.DecodeJudgments(jsonInput(map[string]any{"judgments": []any{}})); err == nil {
		t.Error("an empty batch was accepted")
	}
}

// TestAProposedHypothesisCarriesNoPrior (FR-020a).
func TestAProposedHypothesisCarriesNoPrior(t *testing.T) {
	t.Parallel()

	_, err := engine.DecodeHypothesis(jsonInput(map[string]any{
		"kind": "change", "statement": "the rollout did it",
		"candidate_change_entity_ref": "k8s.change=shop/payments@rev7",
		"target_entity_refs":          []string{"otel.service.name=payments"},
		"prior":                       0.9,
	}))
	if err == nil {
		t.Fatal("a hypothesis carrying a prior was accepted; the engine assigns it")
	}

	proposed, err := engine.DecodeHypothesis(jsonInput(map[string]any{
		"kind": "condition", "statement": "the connection pool is saturated",
		"target_entity_refs": []string{"otel.service.name=payments"},
	}))
	if err != nil {
		t.Fatalf("a condition hypothesis was refused: %v", err)
	}
	if proposed.Kind != string(ledger.KindCondition) {
		t.Errorf("kind = %q", proposed.Kind)
	}
}

func jsonInput(fields map[string]any) json.RawMessage {
	raw, err := json.Marshal(fields)
	if err != nil {
		panic(err)
	}
	return raw
}
