// SPDX-License-Identifier: Apache-2.0

// Package modeltest is the in-repo fake model transport: canned assistant turns, returned in
// order, with no network and no credentials.
//
// It exists because every test in this feature has to run against a recorded or fake model
// transport — there is no API key in CI and there must never need to be (plan §Evaluation and
// CI). It is deliberately the *same seam* the fixture replay and Phase 7's CI gate use: a
// `model.Transport` built from a list of exchanges. A test that passes here is a test that
// exercised the real request build, the real SDK decoder and the real usage accounting; only the
// wire is fake.
package modeltest

import (
	"encoding/json"
	"fmt"

	"github.com/Pierre-Theophile/aisre/internal/investigation/model"
)

// Turn is one canned assistant turn.
type Turn struct {
	// ID is the message id; empty means a generated one.
	ID string
	// Model is the model id the response claims. Empty means the investigator's model.
	Model string
	// Text is an optional text block.
	Text string
	// ToolUses are the tool calls this turn makes, in order.
	ToolUses []ToolUse
	// StopReason overrides the derived stop reason ("tool_use" when there are tool calls,
	// "end_turn" otherwise). Set it to "refusal" to exercise the refusal path.
	StopReason string
	// RefusalCategory and RefusalExplanation populate `stop_details` on a refusal.
	RefusalCategory    string
	RefusalExplanation string
	// Usage is the token accounting the turn reports. A zero CacheRead on the second and later
	// turns is what the cache conformance check is looking for, so tests that care set it.
	Usage model.Usage
}

// ToolUse is one canned tool call.
type ToolUse struct {
	// ID is the tool_use id the tool_result will answer.
	ID string
	// Name is the tool.
	Name string
	// Input is the tool input; it is marshalled as-is, so a test can hand over a map or a
	// struct.
	Input any
}

// Transport returns a replaying transport that serves the given turns in order without checking
// what was asked.
//
// The body shape follows the model id: a canned turn for `claude-…` is rendered as an Anthropic
// message, one for `zai-…` / `mistral…` / `ministral…` as a Mistral chat completion. That is
// what keeps a canned transport honest across the provider seam — a test that hands the engine
// the configured model gets the body that model's provider would actually have to decode, and a
// test cannot accidentally decode one vendor's answer with the other's client.
//
// It is the tolerant replayer on purpose: a unit test of the loop is not a test of the replay
// gate, and making every test pin a request digest would make every prompt edit a mass rewrite.
// The strict replayer — the one Phase 7's gate uses — is model.NewReplayingTransport.
func Transport(defaultModel string, turns ...Turn) (model.Transport, error) {
	exchanges, err := Exchanges(defaultModel, turns...)
	if err != nil {
		return nil, err
	}
	return model.NewReplayingTransportTolerant(exchanges), nil
}

// Exchanges renders the turns as recorded exchanges, which a test can also feed to the strict
// replayer once it holds the matching requests.
func Exchanges(defaultModel string, turns ...Turn) ([]model.Exchange, error) {
	out := make([]model.Exchange, 0, len(turns))
	for i, turn := range turns {
		body, err := Body(defaultModel, i, turn)
		if err != nil {
			return nil, err
		}
		id := turn.Model
		if id == "" {
			id = defaultModel
		}
		out = append(out, model.Exchange{
			Method:       "POST",
			Path:         pathFor(id),
			Status:       200,
			ResponseBody: body,
		})
	}
	return out, nil
}

// pathFor is the endpoint the provider serving a model id answers on. It is recorded so that a
// canned exchange reads like the exchange it stands in for; nothing matches on it.
func pathFor(modelID string) string {
	if p, ok := model.ProviderFor(modelID); ok && p == model.ProviderMistral {
		return "/v1/chat/completions"
	}
	return "/v1/messages"
}

// Body renders one turn as the exact response body the provider serving the model id would have
// returned.
func Body(defaultModel string, index int, turn Turn) (json.RawMessage, error) {
	id := turn.Model
	if id == "" {
		id = defaultModel
	}
	if p, ok := model.ProviderFor(id); ok && p == model.ProviderMistral {
		return mistralBody(id, index, turn)
	}
	return anthropicBody(defaultModel, index, turn)
}

// mistralBody renders one turn as a Mistral chat completion.
//
// One class does not survive the crossing: Mistral reports no cache *write*, because its prefix
// cache is automatic and populating it is billed as input. A canned turn that sets CacheWrite on
// a Mistral model therefore renders as input, which is what the provider itself would report.
func mistralBody(modelID string, index int, turn Turn) (json.RawMessage, error) {
	id := turn.ID
	if id == "" {
		id = fmt.Sprintf("msg_fake_%03d", index+1)
	}

	// A Mistral refusal has no `stop_details`: the finish reason *is* the category and whatever
	// the provider wants to say about it comes back as ordinary content. A canned refusal is
	// rendered that way rather than inventing a field the API does not have.
	text := turn.Text
	if text == "" && turn.StopReason == model.StopRefusal {
		text = turn.RefusalExplanation
	}

	content := make([]map[string]any, 0, 1)
	if text != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}

	message := map[string]any{"role": "assistant", "content": content}
	if len(turn.ToolUses) > 0 {
		calls := make([]map[string]any, 0, len(turn.ToolUses))
		for _, use := range turn.ToolUses {
			raw, err := json.Marshal(use.Input)
			if err != nil {
				return nil, fmt.Errorf("modeltest: tool use %s: %w", use.Name, err)
			}
			calls = append(calls, map[string]any{
				"id": use.ID, "type": "function",
				"function": map[string]any{"name": use.Name, "arguments": string(raw)},
			})
		}
		message["tool_calls"] = calls
	}

	body := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"model":   modelID,
		"created": 0,
		"choices": []map[string]any{{
			"index":         0,
			"finish_reason": finishReason(turn),
			"message":       message,
		}},
		"usage": map[string]any{
			"prompt_tokens":     turn.Usage.Input + turn.Usage.CacheWrite + turn.Usage.CacheRead,
			"completion_tokens": turn.Usage.Output,
			"total_tokens": turn.Usage.Input + turn.Usage.CacheWrite + turn.Usage.CacheRead +
				turn.Usage.Output,
			"prompt_tokens_details": map[string]any{"cached_tokens": turn.Usage.CacheRead},
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("modeltest: marshal turn %d: %w", index, err)
	}
	return raw, nil
}

// finishReason translates the engine's stop vocabulary back into Mistral's, which is the
// direction the provider itself never has to go — a canned transport is the one place the
// mapping runs backwards.
func finishReason(turn Turn) string {
	switch turn.StopReason {
	case "":
		if len(turn.ToolUses) > 0 {
			return "tool_calls"
		}
		return "stop"
	case model.StopToolUse:
		return "tool_calls"
	case model.StopEndTurn:
		return "stop"
	case model.StopMaxTokens:
		return "length"
	case model.StopRefusal:
		return "content_filter"
	default:
		return turn.StopReason
	}
}

// anthropicBody renders one turn as the exact Anthropic response body.
func anthropicBody(defaultModel string, index int, turn Turn) (json.RawMessage, error) {
	id := turn.ID
	if id == "" {
		id = fmt.Sprintf("msg_fake_%03d", index+1)
	}
	modelID := turn.Model
	if modelID == "" {
		modelID = defaultModel
	}

	content := make([]map[string]any, 0, len(turn.ToolUses)+1)
	if turn.Text != "" {
		content = append(content, map[string]any{"type": "text", "text": turn.Text})
	}
	for _, use := range turn.ToolUses {
		raw, err := json.Marshal(use.Input)
		if err != nil {
			return nil, fmt.Errorf("modeltest: tool use %s: %w", use.Name, err)
		}
		var input any
		if err := json.Unmarshal(raw, &input); err != nil {
			return nil, fmt.Errorf("modeltest: tool use %s: %w", use.Name, err)
		}
		content = append(content, map[string]any{
			"type": "tool_use", "id": use.ID, "name": use.Name, "input": input,
		})
	}

	stop := turn.StopReason
	if stop == "" {
		if len(turn.ToolUses) > 0 {
			stop = "tool_use"
		} else {
			stop = "end_turn"
		}
	}

	message := map[string]any{
		"id":          id,
		"type":        "message",
		"role":        "assistant",
		"model":       modelID,
		"content":     content,
		"stop_reason": stop,
		"usage": map[string]any{
			"input_tokens":                turn.Usage.Input,
			"output_tokens":               turn.Usage.Output,
			"cache_creation_input_tokens": turn.Usage.CacheWrite,
			"cache_read_input_tokens":     turn.Usage.CacheRead,
		},
	}
	if stop == "refusal" {
		message["stop_details"] = map[string]any{
			"type":        "refusal",
			"category":    turn.RefusalCategory,
			"explanation": turn.RefusalExplanation,
		}
	}

	raw, err := json.Marshal(message)
	if err != nil {
		return nil, fmt.Errorf("modeltest: marshal turn %d: %w", index, err)
	}
	return raw, nil
}
