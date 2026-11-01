// SPDX-License-Identifier: Apache-2.0

// Package anthropic is the Anthropic Messages API adapter of the model boundary.
//
// It is the only package that links the Anthropic SDK. It registers itself as the `anthropic`
// provider when imported, so a binary that wants Anthropic imports it (the CLI does), and a
// package that only reasons over the model boundary — the engine, the verifier — never links
// the vendor SDK.
//
// The request this builds is byte-for-byte the request the engine has always built, so the
// checked-in fake-model trajectories replay unchanged and every recorded digest still matches.
// The request layout, the single cache breakpoint and the parameters this path deliberately
// never sends are documented on params, which is where the body is composed.
package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/Pierre-Theophile/aisre/internal/investigation/model"
)

func init() {
	model.RegisterProvider(model.ProviderAnthropic, model.ProviderSpec{
		New: func(config model.Config, transport model.Transport) (model.Adapter, error) {
			return New(config, transport), nil
		},
		Efforts:       []string{"low", "medium", "high", "xhigh", "max"},
		CredentialEnv: model.EnvAnthropicAPIKey,
	})
}

// Adapter speaks the Anthropic Messages API through the SDK.
type Adapter struct {
	api    sdk.Client
	config model.Config
}

var _ model.Adapter = (*Adapter)(nil)

// New builds the SDK client over the shared transport. Request options are passed through to the
// SDK, which resolves credentials in its own published order — an unset ANTHROPIC_API_KEY does
// not mean there are none.
func New(config model.Config, transport model.Transport, opts ...option.RequestOption) *Adapter {
	if transport.Mode() == model.TransportReplaying {
		// A replay makes no network call, so requiring a key would make the CI gate depend on a
		// vendor account (FR-041); and it never retries, because a retry would re-issue a request
		// the recording has already answered and turn one divergence into three.
		opts = append([]option.RequestOption{
			option.WithAPIKey("replay-no-network"),
			option.WithMaxRetries(0),
		}, opts...)
	}
	opts = append(opts, option.WithHTTPClient(&http.Client{Transport: transport}))
	return &Adapter{api: sdk.NewClient(opts...), config: config}
}

// CountTokens asks the vendor's own tokenizer what this request would cost to send.
func (p *Adapter) CountTokens(ctx context.Context, rc model.RoleConfig, req model.Request) (int64, error) {
	params, err := p.params(rc, req)
	if err != nil {
		return 0, err
	}
	count, err := p.api.Beta.Messages.CountTokens(ctx, sdk.BetaMessageCountTokensParams{
		Model:    params.Model,
		Messages: params.Messages,
		System: sdk.BetaMessageCountTokensParamsSystemUnion{
			OfBetaTextBlockArray: params.System,
		},
		Tools:        countTokensTools(params.Tools),
		Thinking:     params.Thinking,
		OutputConfig: params.OutputConfig,
		Betas:        params.Betas,
	})
	if err != nil {
		return 0, fmt.Errorf("model: count_tokens: %w", err)
	}
	return count.InputTokens, nil
}

// Complete issues one Messages call and decodes it into this package's vocabulary.
func (p *Adapter) Complete(ctx context.Context, rc model.RoleConfig, req model.Request) (*model.Response, error) {
	params, err := p.params(rc, req)
	if err != nil {
		return nil, err
	}

	var message *sdk.BetaMessage
	if req.Stream {
		stream := p.api.Beta.Messages.NewStreaming(ctx, params)
		accumulated := sdk.BetaMessage{}
		for stream.Next() {
			if err := accumulated.Accumulate(stream.Current()); err != nil {
				return nil, fmt.Errorf("model: accumulate stream: %w", err)
			}
		}
		if err := stream.Err(); err != nil {
			return nil, p.wrap(err)
		}
		message = &accumulated
	} else {
		message, err = p.api.Beta.Messages.New(ctx, params)
		if err != nil {
			return nil, p.wrap(err)
		}
	}

	out := &model.Response{
		ID:         message.ID,
		Model:      message.Model,
		StopReason: string(message.StopReason),
		Usage:      usageOf(message.Usage),
	}
	if out.Refused() {
		out.RefusalCategory = string(message.StopDetails.Category)
		out.RefusalExplanation = message.StopDetails.Explanation
	}
	out.Blocks = blocksOf(message.Content)
	return out, nil
}

// wrap turns an SDK error into something a caller can act on, keeping a replay divergence
// visible through net/http's own wrapping.
func (p *Adapter) wrap(err error) error {
	if d := model.DivergenceOf(err); d != nil {
		return d
	}
	return fmt.Errorf("model: messages.new: %w", err)
}

func blocksOf(content []sdk.BetaContentBlockUnion) []model.Block {
	out := make([]model.Block, 0, len(content))
	for _, block := range content {
		switch variant := block.AsAny().(type) {
		case sdk.BetaTextBlock:
			out = append(out, model.Block{Kind: model.BlockText, Text: variant.Text})
		case sdk.BetaThinkingBlock:
			out = append(out, model.Block{
				Kind:      model.BlockThinking,
				Thinking:  variant.Thinking,
				Signature: variant.Signature,
			})
		case sdk.BetaToolUseBlock:
			out = append(out, model.Block{
				Kind:      model.BlockToolUse,
				ToolUseID: variant.ID,
				ToolName:  variant.Name,
				ToolInput: json.RawMessage(variant.JSON.Input.Raw()),
			})
		}
	}
	return out
}

// params builds the exact SDK request. It is the only place an Anthropic request is composed, so
// "the recorded request is the request" is a property of one function rather than of a
// convention.
func (p *Adapter) params(rc model.RoleConfig, req model.Request) (sdk.BetaMessageNewParams, error) {
	if req.System == "" {
		return sdk.BetaMessageNewParams{}, errors.New(
			"model: a request carries no system prefix; the prefix is the contract (contracts/prompting.md)")
	}
	if len(req.Messages) == 0 {
		return sdk.BetaMessageNewParams{}, errors.New("model: a request carries no messages")
	}

	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = model.DefaultMaxTokens
	}

	out := sdk.BetaMessageNewParams{
		Model:     rc.Model,
		MaxTokens: maxTokens,
		Betas:     betaSet(p.config.Betas),
	}

	for _, tool := range req.Tools {
		out.Tools = append(out.Tools, sdk.BetaToolUnionParam{OfTool: &sdk.BetaToolParam{
			Name:        tool.Name,
			Description: sdk.String(tool.Description),
			Strict:      sdk.Bool(true),
			InputSchema: sdk.BetaToolInputSchemaParam{
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
	out.System = []sdk.BetaTextBlockParam{{
		Text:         req.System,
		CacheControl: sdk.NewBetaCacheControlEphemeralParam(),
	}}

	if rc.Effort != "" {
		out.OutputConfig.Effort = sdk.BetaOutputConfigEffort(rc.Effort)
	}
	if req.Format != nil {
		out.OutputConfig.Format = sdk.BetaJSONOutputFormatParam{Schema: req.Format.Schema}
	}
	if rc.ThinkingDisplay != "" {
		adaptive := sdk.BetaThinkingConfigAdaptiveParam{
			Display: sdk.BetaThinkingConfigAdaptiveDisplay(rc.ThinkingDisplay),
		}
		out.Thinking = sdk.BetaThinkingConfigParamUnion{OfAdaptive: &adaptive}
	}

	messages, err := messageParams(req.Messages)
	if err != nil {
		return sdk.BetaMessageNewParams{}, err
	}
	out.Messages = messages
	return out, nil
}

// betaSet copies the configured beta names so that a caller mutating the configuration after a
// client was built cannot change what a request in flight declares.
func betaSet(names []string) []sdk.AnthropicBeta {
	out := make([]sdk.AnthropicBeta, 0, len(names))
	out = append(out, names...)
	return out
}

func messageParams(messages []model.Message) ([]sdk.BetaMessageParam, error) {
	out := make([]sdk.BetaMessageParam, 0, len(messages))
	for i, message := range messages {
		blocks, err := blockParams(message.Blocks)
		if err != nil {
			return nil, fmt.Errorf("model: message %d: %w", i, err)
		}
		param := sdk.BetaMessageParam{
			Role:    sdk.BetaMessageParamRole(message.Role),
			Content: blocks,
		}
		switch message.Role {
		case model.RoleUser, model.RoleAssistant:
			if message.ClearAt != "" {
				return nil, fmt.Errorf(
					"model: message %d is a %s message with clear_at %q; only a system message is turn-scoped",
					i, message.Role, message.ClearAt)
			}
		case model.RoleSystem:
			param.ClearAt = sdk.BetaMessageParamClearAt(message.ClearAt)
		default:
			return nil, fmt.Errorf("model: message %d: role %q is not one of user, assistant, system",
				i, message.Role)
		}
		out = append(out, param)
	}
	return out, nil
}

func blockParams(blocks []model.Block) ([]sdk.BetaContentBlockParamUnion, error) {
	out := make([]sdk.BetaContentBlockParamUnion, 0, len(blocks))
	for _, block := range blocks {
		switch block.Kind {
		case model.BlockText:
			out = append(out, sdk.NewBetaTextBlock(block.Text))
		case model.BlockThinking:
			// Echoed back unchanged. Rewriting one is what the append-only discipline exists to
			// prevent (research §3).
			out = append(out, sdk.NewBetaThinkingBlock(block.Signature, block.Thinking))
		case model.BlockToolUse:
			var input any
			if len(block.ToolInput) > 0 {
				if err := json.Unmarshal(block.ToolInput, &input); err != nil {
					return nil, fmt.Errorf("tool_use %s: input is not JSON: %w", block.ToolUseID, err)
				}
			}
			out = append(out, sdk.NewBetaToolUseBlock(block.ToolUseID, input, block.ToolName))
		case model.BlockToolResult:
			out = append(out, sdk.NewBetaToolResultBlock(block.ToolUseID, block.Text, block.IsError))
		default:
			return nil, fmt.Errorf("block kind %q is not one of text, thinking, tool_use, tool_result", block.Kind)
		}
	}
	return out, nil
}

// countTokensTools re-expresses the tool set for the count_tokens endpoint, which takes its own
// union type over the same tools.
func countTokensTools(tools []sdk.BetaToolUnionParam) []sdk.BetaMessageCountTokensParamsToolUnion {
	out := make([]sdk.BetaMessageCountTokensParamsToolUnion, 0, len(tools))
	for _, tool := range tools {
		if tool.OfTool == nil {
			continue
		}
		out = append(out, sdk.BetaMessageCountTokensParamsToolUnion{OfTool: tool.OfTool})
	}
	return out
}

func usageOf(u sdk.BetaUsage) model.Usage {
	return model.Usage{
		Input:      u.InputTokens,
		CacheWrite: u.CacheCreationInputTokens,
		CacheRead:  u.CacheReadInputTokens,
		Output:     u.OutputTokens,
	}
}
