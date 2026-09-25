// SPDX-License-Identifier: Apache-2.0

package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go/option"
)

// ErrNotYetImplemented is returned by an entry point this feature publishes in Phase 1 and
// implements in Phase 6.
var ErrNotYetImplemented = errors.New("model: not implemented yet")

// Client is the engine's boundary with the Anthropic Messages API: one place that builds a
// request, sets the cache breakpoints, records the exact request and response bodies, accounts
// for usage, and hands the recorder its trajectory records.
//
// The loop itself is not here and is not the SDK's. This feature owns it (plan §Design
// Decisions 2): byte-exact capture of every request and response, a re-issue path that replays
// recorded assistant turns without calling the API at all, anytime publication, the budget
// admission check before each call, and the diminishing-returns stop are the feature, and a
// harness that owns the loop cannot provide them.
//
// Everything crosses through a Transport (transport.go), which is what makes "no network" a
// property of the wiring rather than a promise: a test, a fixture replay and Phase 7's CI gate
// all hand this client a replaying transport and the code below does not know the difference.
type Client struct {
	providers map[Provider]provider
	config    Config
	prices    Prices
	transport Transport
}

// NewClient returns a Client for the given configuration. Request options are passed through to
// the SDK, which resolves credentials in its own published order — an unset ANTHROPIC_API_KEY
// does not mean there are none.
//
// The engine is inert without a model configuration, which is what makes an operator who runs
// only the graph unaffected by this feature existing (plan §Design Decisions 1).
func NewClient(config Config, prices Prices, opts ...option.RequestOption) (*Client, error) {
	return NewClientWithTransport(config, prices, NewLiveTransport(nil), opts...)
}

// NewClientWithTransport returns a Client whose every call crosses the given transport.
//
// This is the constructor the engine actually uses: a live run passes a recording transport, a
// fixture replay passes a replaying one built from the trajectory, and a unit test passes a
// canned one. There is no code path that reaches the network around it.
func NewClientWithTransport(config Config, prices Prices, transport Transport, opts ...option.RequestOption) (*Client, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if err := prices.Validate(); err != nil {
		return nil, err
	}
	if config.PriceTableVersion != prices.Version {
		return nil, fmt.Errorf(
			"model config %s names price table %s but was given version %s; the two are recorded together and must agree",
			config.Version, config.PriceTableVersion, prices.Version)
	}
	if transport == nil {
		return nil, errors.New("model: a transport is required; the model boundary is never reached around")
	}
	return &Client{
		providers: map[Provider]provider{
			ProviderAnthropic: newAnthropicProvider(config, transport, opts...),
			ProviderMistral:   newMistralProvider(config, transport),
		},
		config:    config,
		prices:    prices,
		transport: transport,
	}, nil
}

// Config returns the configuration this client runs under. Every investigation records a copy
// of it, so a historical run stays interpretable after the configuration changes (FR-061).
func (c *Client) Config() Config { return c.config }

// Prices returns the price table this client's cost figures are derived from. Spend is recorded
// both as raw token counts per model and class — the provider-independent unit — and as a
// monetary figure from this table (FR-048).
func (c *Client) Prices() Prices { return c.prices }

// Transport returns the transport every call crosses, so the engine can drain its exchanges into
// the trajectory.
func (c *Client) Transport() Transport { return c.transport }

// ModelFor returns the model id configured for a role.
func (c *Client) ModelFor(role Role) (string, error) {
	rc, ok := c.config.Role(role)
	if !ok {
		return "", fmt.Errorf("model config %s: role %s is not configured", c.config.Version, role)
	}
	return rc.Model, nil
}

// Request is one model call, expressed in this feature's terms rather than the SDK's.
//
// The field order below is the request's own order, and it is the order the cache depends on:
// tools, then system, then messages. Everything volatile lives in Messages, after the single
// cache breakpoint, which is what makes `usage.cache_read_input_tokens` non-zero from turn two
// (research §3, FR-061).
type Request struct {
	// Role picks the model, effort and thinking display from the configuration.
	Role Role
	// Tools is the tool set. It is part of the cached prefix, so it is the same set on every
	// turn of one investigation; a tool that appeared halfway through would invalidate the
	// prefix for every turn after it.
	Tools []Tool
	// System is the stable prefix. The cache breakpoint is placed on its last block.
	System string
	// Messages is the append-only conversation. Earlier entries are never edited: on this model
	// family editing a turn invalidates the thinking blocks after it (research §3).
	Messages []Message
	// MaxTokens bounds the response.
	MaxTokens int64
	// Format is the structured-output schema, for the two places a free-form answer would be a
	// defect: the judgment batch and the closing synthesis's machine form.
	Format *OutputFormat
	// Stream asks for a streamed response. The recorded body is then the raw event stream, and
	// a replay serves those same bytes back through the SDK's decoder.
	Stream bool
}

// OutputFormat is a structured-output schema (`output_config.format`).
type OutputFormat struct {
	// Name is what the schema is called, for the error message when it does not validate.
	Name string
	// Schema is the JSON schema object.
	Schema map[string]any
}

// Message is one entry in the append-only conversation.
//
// A system-role entry is the *operator channel*: the mid-conversation system message the ledger
// is re-rendered into each turn. Nothing a worker returns can ever be turned into one — worker
// output is only ever a tool result — which is the injection barrier stated in the API's own
// terms (FR-017, ADR-0003 D8).
type Message struct {
	// Role is "user", "assistant" or "system".
	Role string
	// ClearAt is "next_user_message" on a turn-scoped system message and empty everywhere else.
	// A turn-scoped message renders for one turn and then stays in the transcript cleared; it is
	// never deleted, because deleting it would edit history.
	ClearAt string
	// Blocks is the content.
	Blocks []Block
}

// Block is one content block.
type Block struct {
	// Kind is "text", "thinking", "tool_use" or "tool_result".
	Kind string
	// Text is the text of a text block, or the rendered result of a tool_result block.
	Text string
	// Thinking and Signature carry a thinking block back unchanged. They are echoed, never
	// rewritten: a thinking block that was edited is a thinking block the model will reject.
	Thinking  string
	Signature string
	// ToolUseID, ToolName and ToolInput are a tool_use block; ToolUseID alone identifies the
	// tool_result that answers it.
	ToolUseID string
	ToolName  string
	ToolInput json.RawMessage
	// IsError marks a tool_result that carries a failure. A failed worker call is an answer,
	// not a silence (FR-027).
	IsError bool
}

// The published block kinds.
const (
	BlockText       = "text"
	BlockThinking   = "thinking"
	BlockToolUse    = "tool_use"
	BlockToolResult = "tool_result"
)

// The published message roles. The system role is the operator channel.
const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleSystem    = "system"
)

// ClearAtNextUserMessage is the published `clear_at` value for a turn-scoped system message.
const ClearAtNextUserMessage = "next_user_message"

// Response is one model answer, plus everything the recording and the budget need.
type Response struct {
	// ID and Model are the vendor's own identifiers for the turn.
	ID    string
	Model string
	// StopReason is the API's stop reason, verbatim: "end_turn", "tool_use", "max_tokens",
	// "refusal", … A refusal is handled and recorded like any other terminal condition, never
	// silently retried (FR-045b).
	StopReason string
	// RefusalCategory and RefusalExplanation are populated only for a refusal.
	RefusalCategory    string
	RefusalExplanation string
	// Blocks is the content the model produced.
	Blocks []Block
	// Usage is the token accounting, by class.
	Usage Usage
	// Mode is the transport mode this answer came through.
	Mode TransportMode
	// RequestDigest, RequestBody, ResponseDigest and ResponseBody are the trajectory's raw
	// material: the exact bytes, canonicalised, and their digests (FR-042a).
	RequestDigest  string
	RequestBody    json.RawMessage
	ResponseDigest string
	ResponseBody   json.RawMessage
	// Betas is the beta set the request carried, read back off the wire rather than off the
	// configuration, so a recording shows what was actually sent.
	Betas []string
}

// The published stop reasons, in the engine's own vocabulary.
//
// They are Anthropic's spellings because that is what the engine, the trajectory schema and the
// checked-in corpus were written against; a second provider translates into them rather than
// adding its own, so a stop reason means one thing whoever produced it (provider.go).
const (
	// StopEndTurn is a turn the model ended of its own accord.
	StopEndTurn = "end_turn"
	// StopToolUse is a turn that ended by calling tools.
	StopToolUse = "tool_use"
	// StopMaxTokens is a turn truncated by the response cap.
	StopMaxTokens = "max_tokens"
	// StopSequence is a turn ended by a stop sequence.
	StopSequence = "stop_sequence"
	// StopRefusal is a request the provider declined. It is a terminal condition recorded like
	// any other and never silently retried (FR-045b).
	StopRefusal = "refusal"
)

// Refused reports whether the API declined the request.
func (r *Response) Refused() bool { return r.StopReason == StopRefusal }

// ToolUses returns the tool_use blocks in the order the model produced them.
func (r *Response) ToolUses() []Block {
	out := make([]Block, 0, len(r.Blocks))
	for _, b := range r.Blocks {
		if b.Kind == BlockToolUse {
			out = append(out, b)
		}
	}
	return out
}

// Text returns the concatenated text blocks.
func (r *Response) Text() string {
	var parts []string
	for _, b := range r.Blocks {
		if b.Kind == BlockText && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// AssistantMessage renders the response as the next entry of the append-only history, with the
// thinking blocks carried back unchanged.
func (r *Response) AssistantMessage() Message {
	return Message{Role: RoleAssistant, Blocks: r.Blocks}
}

// CountTokens is the pre-flight admission check: what this request would cost to send, asked
// before it is sent, so a reserve is never overrun by a call already in flight (FR-047a).
//
// It dispatches on the role's provider, because the two vendors answer it very differently: one
// publishes a `count_tokens` endpoint that tokenises the exact request, the other publishes
// none and is estimated locally. What the loop needs from either is a stable, conservative
// number to admit against — not a re-measurement of the tokenizer.
func (c *Client) CountTokens(ctx context.Context, req Request) (int64, error) {
	rc, p, err := c.resolve(req.Role)
	if err != nil {
		return 0, err
	}
	return p.countTokens(ctx, rc, req)
}

// Complete issues one model call and returns the answer together with the bytes that produced
// it.
//
// It does not retry. A refusal, a timeout and a vendor error are all terminal conditions the
// engine records and stops on with a typed reason; a retry here would be a call nobody budgeted
// for and a turn nobody recorded (FR-045b).
//
// The provider decodes the vendor's answer into this package's vocabulary; everything below the
// body shape — which exchange crossed the wire, its canonical bodies and digests, the transport
// mode — is filled in here, identically for every provider, because that is what makes a
// recording replayable without knowing who answered it (FR-042a).
func (c *Client) Complete(ctx context.Context, req Request) (*Response, error) {
	rc, p, err := c.resolve(req.Role)
	if err != nil {
		return nil, err
	}

	before := len(c.transport.Exchanges())
	out, err := p.complete(ctx, rc, req)
	if err != nil {
		return nil, err
	}

	exchanges := c.transport.Exchanges()
	if len(exchanges) <= before {
		return nil, errors.New("model: the transport recorded no exchange for a completed call; " +
			"a model call that left no trace cannot be replayed (FR-042a)")
	}
	exchange := exchanges[len(exchanges)-1]

	out.Mode = c.transport.Mode()
	out.RequestDigest = exchange.RequestDigest
	out.RequestBody = exchange.RequestBody
	out.ResponseDigest = exchange.ResponseDigest
	out.ResponseBody = exchange.ResponseBody
	out.Betas = exchange.Betas
	return out, nil
}

// resolve turns a role into the configuration and the provider that serve it.
func (c *Client) resolve(role Role) (RoleConfig, provider, error) {
	rc, ok := c.config.Role(role)
	if !ok {
		return RoleConfig{}, nil, fmt.Errorf(
			"model config %s: role %s is not configured", c.config.Version, role)
	}
	p, err := c.providerFor(rc)
	if err != nil {
		return RoleConfig{}, nil, err
	}
	return rc, p, nil
}

// Provider returns the provider serving a role, which is what a command reports at startup and
// what FR-061's recorded configuration carries.
func (c *Client) Provider(role Role) (Provider, error) {
	rc, ok := c.config.Role(role)
	if !ok {
		return "", fmt.Errorf("model config %s: role %s is not configured", c.config.Version, role)
	}
	return rc.ProviderOrDefault(), nil
}
