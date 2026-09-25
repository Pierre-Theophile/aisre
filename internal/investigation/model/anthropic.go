// SPDX-License-Identifier: Apache-2.0

package model

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// The Anthropic provider (Track L; formerly the whole of client.go).
//
// Nothing below changed when the seam was introduced, and that is the point: the request this
// builds is byte-for-byte the request this feature has always built, so the two checked-in
// fake-model trajectories replay unchanged and every recorded digest still matches. The only
// edit was the receiver.
//
// The request layout, the single cache breakpoint and the parameters this path deliberately
// never sends are documented in params.go, which is where the body is composed.

// anthropicProvider speaks the Anthropic Messages API through the SDK.
type anthropicProvider struct {
	api    anthropic.Client
	config Config
}

// newAnthropicProvider builds the SDK client over the shared transport.
//
// Request options are passed through to the SDK, which resolves credentials in its own published
// order — an unset ANTHROPIC_API_KEY does not mean there are none.
func newAnthropicProvider(config Config, transport Transport, opts ...option.RequestOption) *anthropicProvider {
	if transport.Mode() == TransportReplaying {
		// A replay makes no network call, so requiring a key would make the CI gate depend on a
		// vendor account (FR-041); and it never retries, because a retry would re-issue a request
		// the recording has already answered and turn one divergence into three.
		opts = append([]option.RequestOption{
			option.WithAPIKey("replay-no-network"),
			option.WithMaxRetries(0),
		}, opts...)
	}
	opts = append(opts, option.WithHTTPClient(&http.Client{Transport: transport}))
	return &anthropicProvider{api: anthropic.NewClient(opts...), config: config}
}

func (p *anthropicProvider) name() Provider { return ProviderAnthropic }

// countTokens asks the vendor's own tokenizer what this request would cost to send.
func (p *anthropicProvider) countTokens(ctx context.Context, rc RoleConfig, req Request) (int64, error) {
	params, err := p.params(rc, req)
	if err != nil {
		return 0, err
	}
	count, err := p.api.Beta.Messages.CountTokens(ctx, anthropic.BetaMessageCountTokensParams{
		Model:    params.Model,
		Messages: params.Messages,
		System: anthropic.BetaMessageCountTokensParamsSystemUnion{
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

// complete issues one Messages call and decodes it into this package's vocabulary.
func (p *anthropicProvider) complete(ctx context.Context, rc RoleConfig, req Request) (*Response, error) {
	params, err := p.params(rc, req)
	if err != nil {
		return nil, err
	}

	var message *anthropic.BetaMessage
	if req.Stream {
		stream := p.api.Beta.Messages.NewStreaming(ctx, params)
		accumulated := anthropic.BetaMessage{}
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

	out := &Response{
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
func (p *anthropicProvider) wrap(err error) error {
	if d := DivergenceOf(err); d != nil {
		return d
	}
	return fmt.Errorf("model: messages.new: %w", err)
}

func blocksOf(content []anthropic.BetaContentBlockUnion) []Block {
	out := make([]Block, 0, len(content))
	for _, block := range content {
		switch variant := block.AsAny().(type) {
		case anthropic.BetaTextBlock:
			out = append(out, Block{Kind: BlockText, Text: variant.Text})
		case anthropic.BetaThinkingBlock:
			out = append(out, Block{
				Kind:      BlockThinking,
				Thinking:  variant.Thinking,
				Signature: variant.Signature,
			})
		case anthropic.BetaToolUseBlock:
			out = append(out, Block{
				Kind:      BlockToolUse,
				ToolUseID: variant.ID,
				ToolName:  variant.Name,
				ToolInput: json.RawMessage(variant.JSON.Input.Raw()),
			})
		}
	}
	return out
}
