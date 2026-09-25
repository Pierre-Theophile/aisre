// SPDX-License-Identifier: Apache-2.0

package model

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
)

// The request build (T060, T061, research §2/§3, contracts/prompting.md §"Request layout").
//
// The layout is `tools → system → messages`, and that order is not a style choice: the vendor's
// cache is a prefix match, so everything stable has to come before everything volatile and the
// breakpoint has to sit exactly on the boundary. One breakpoint, on the last system block, which
// covers the tool set and the system prefix together — the ~10k tokens that are identical for
// the life of a schema version. Everything after it is the brief, the ledger render and the
// turns, all of which move every turn by design.
//
// Two parameters this feature never sends, recorded here so their absence is visible:
// `temperature` (and the other sampling parameters), which return 400 on this model family; and
// `tool_choice: {type: "any"|"tool"}`, which returns 400 too — forced tool use does not exist
// here, which is why the engine and not the model is the ledger's writer of record (research §3).

// DefaultMaxTokens is the response cap for an ordinary turn. It is generous enough that a turn
// proposing a batch of judgments is never truncated mid-argument and small enough to stay well
// inside the SDK's non-streaming timeout.
const DefaultMaxTokens int64 = 16000

// Tool is one tool the investigator may call, in this feature's terms.
//
// Every tool is `strict: true` and every schema closes itself with `additionalProperties:
// false`. That is what keeps a proposed judgment schema-valid without forcing the call, which is
// the only lever left once forced tool use is off the table.
type Tool struct {
	// Name is the tool name the model calls. For an algebra term it is the term name, so the
	// tool set and the published algebra are the same vocabulary (FR-042b).
	Name string
	// Description is what the tool does, in one or two sentences.
	Description string
	// Properties is the JSON-schema property map.
	Properties map[string]any
	// Required is the required property list, in a fixed order.
	Required []string
}

// Schema renders the tool's input schema as the closed object the API is sent.
func (t Tool) Schema() map[string]any {
	properties := t.Properties
	if properties == nil {
		properties = map[string]any{}
	}
	required := t.Required
	if required == nil {
		required = []string{}
	}
	return map[string]any{
		"type":                 "object",
		"properties":           properties,
		"required":             required,
		"additionalProperties": false,
	}
}

// params builds the exact SDK request. It is the only place an Anthropic request is composed, so
// "the recorded request is the request" is a property of one function rather than of a
// convention.
func (p *anthropicProvider) params(rc RoleConfig, req Request) (anthropic.BetaMessageNewParams, error) {
	if req.System == "" {
		return anthropic.BetaMessageNewParams{}, errors.New(
			"model: a request carries no system prefix; the prefix is the contract (contracts/prompting.md)")
	}
	if len(req.Messages) == 0 {
		return anthropic.BetaMessageNewParams{}, errors.New("model: a request carries no messages")
	}

	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = DefaultMaxTokens
	}

	out := anthropic.BetaMessageNewParams{
		Model:     rc.Model,
		MaxTokens: maxTokens,
		Betas:     betaSet(p.config.Betas),
	}

	for _, tool := range req.Tools {
		out.Tools = append(out.Tools, anthropic.BetaToolUnionParam{OfTool: &anthropic.BetaToolParam{
			Name:        tool.Name,
			Description: anthropic.String(tool.Description),
			Strict:      anthropic.Bool(true),
			InputSchema: anthropic.BetaToolInputSchemaParam{
				Properties: tool.Properties,
				Required:   tool.Required,
				ExtraFields: map[string]any{
					"additionalProperties": false,
				},
			},
		}})
	}

	// The single cache breakpoint. It is on the last (only) system block, which in the API's
	// render order covers `tools` and `system` together — exactly the stable prefix.
	out.System = []anthropic.BetaTextBlockParam{{
		Text:         req.System,
		CacheControl: anthropic.NewBetaCacheControlEphemeralParam(),
	}}

	if rc.Effort != "" {
		out.OutputConfig.Effort = anthropic.BetaOutputConfigEffort(rc.Effort)
	}
	if req.Format != nil {
		out.OutputConfig.Format = anthropic.BetaJSONOutputFormatParam{Schema: req.Format.Schema}
	}
	if rc.ThinkingDisplay != "" {
		adaptive := anthropic.BetaThinkingConfigAdaptiveParam{
			Display: anthropic.BetaThinkingConfigAdaptiveDisplay(rc.ThinkingDisplay),
		}
		out.Thinking = anthropic.BetaThinkingConfigParamUnion{OfAdaptive: &adaptive}
	}

	messages, err := messageParams(req.Messages)
	if err != nil {
		return anthropic.BetaMessageNewParams{}, err
	}
	out.Messages = messages
	return out, nil
}

// betaSet copies the configured beta names so that a caller mutating the configuration after a
// client was built cannot change what a request in flight declares.
func betaSet(names []string) []anthropic.AnthropicBeta {
	out := make([]anthropic.AnthropicBeta, 0, len(names))
	out = append(out, names...)
	return out
}

func messageParams(messages []Message) ([]anthropic.BetaMessageParam, error) {
	out := make([]anthropic.BetaMessageParam, 0, len(messages))
	for i, message := range messages {
		blocks, err := blockParams(message.Blocks)
		if err != nil {
			return nil, fmt.Errorf("model: message %d: %w", i, err)
		}
		param := anthropic.BetaMessageParam{
			Role:    anthropic.BetaMessageParamRole(message.Role),
			Content: blocks,
		}
		switch message.Role {
		case RoleUser, RoleAssistant:
			if message.ClearAt != "" {
				return nil, fmt.Errorf(
					"model: message %d is a %s message with clear_at %q; only a system message is turn-scoped",
					i, message.Role, message.ClearAt)
			}
		case RoleSystem:
			param.ClearAt = anthropic.BetaMessageParamClearAt(message.ClearAt)
		default:
			return nil, fmt.Errorf("model: message %d: role %q is not one of user, assistant, system",
				i, message.Role)
		}
		out = append(out, param)
	}
	return out, nil
}

func blockParams(blocks []Block) ([]anthropic.BetaContentBlockParamUnion, error) {
	out := make([]anthropic.BetaContentBlockParamUnion, 0, len(blocks))
	for _, block := range blocks {
		switch block.Kind {
		case BlockText:
			out = append(out, anthropic.NewBetaTextBlock(block.Text))
		case BlockThinking:
			// Echoed back unchanged. Rewriting one is what the append-only discipline exists to
			// prevent (research §3).
			out = append(out, anthropic.NewBetaThinkingBlock(block.Signature, block.Thinking))
		case BlockToolUse:
			var input any
			if len(block.ToolInput) > 0 {
				if err := json.Unmarshal(block.ToolInput, &input); err != nil {
					return nil, fmt.Errorf("tool_use %s: input is not JSON: %w", block.ToolUseID, err)
				}
			}
			out = append(out, anthropic.NewBetaToolUseBlock(block.ToolUseID, input, block.ToolName))
		case BlockToolResult:
			out = append(out, anthropic.NewBetaToolResultBlock(block.ToolUseID, block.Text, block.IsError))
		default:
			return nil, fmt.Errorf("block kind %q is not one of text, thinking, tool_use, tool_result", block.Kind)
		}
	}
	return out, nil
}

// countTokensTools re-expresses the tool set for the count_tokens endpoint, which takes its own
// union type over the same tools.
func countTokensTools(tools []anthropic.BetaToolUnionParam) []anthropic.BetaMessageCountTokensParamsToolUnion {
	out := make([]anthropic.BetaMessageCountTokensParamsToolUnion, 0, len(tools))
	for _, tool := range tools {
		if tool.OfTool == nil {
			continue
		}
		out = append(out, anthropic.BetaMessageCountTokensParamsToolUnion{OfTool: tool.OfTool})
	}
	return out
}
