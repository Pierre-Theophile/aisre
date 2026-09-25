// SPDX-License-Identifier: Apache-2.0

package model

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// The Mistral provider (Track L; FR-017, FR-022a, FR-041, FR-042a, FR-045b, FR-048).
//
// Mistral's `/v1/chat/completions` is an OpenAI-shaped API that also serves the GLM models, and
// GLM is the investigator this deployment runs: a cheap large thinking model with a 1M context.
// It is spoken here with plain `net/http` and `encoding/json` rather than through a vendor SDK.
// That is a deliberate choice, and the reason is the same one that made this feature hand-roll
// its loop: **the recorded request must be the request**. A client that is 200 lines of struct
// tags composes a body this repository can read, diff and digest; an SDK composes a body whose
// shape is the SDK's, and a version bump would silently invalidate every recorded digest in the
// corpus. There is also nothing to gain — no streaming accumulator, no retry policy and no
// credential chain is wanted here, because the engine owns all three.
//
// Everything below the body shape is shared with the Anthropic path: the same `Transport`, so a
// recording is the exact bytes and a replay is a canonical-digest match on them; the same
// `Response`/`Block`/`Usage` vocabulary; the same no-retry rule.

// mistralEndpoint is the chat-completions endpoint. It is a constant rather than a setting
// because a configurable base URL is a way for a recording to have been made against something
// other than what it says it was.
const mistralEndpoint = "https://api.mistral.ai/v1/chat/completions"

// mistralProvider speaks the Mistral chat-completions API.
type mistralProvider struct {
	config Config
	http   *http.Client
	apiKey string
	// replaying says the transport answers from a recording, so a missing credential is not a
	// problem: a replay makes no network call and requiring a key would make the CI gate depend
	// on a vendor account (FR-041).
	replaying bool
}

// newMistralProvider builds the client over the shared transport.
//
// The credential is read once, here, rather than per call: a run whose key is rotated halfway
// through would be a run where two turns were made under different credentials and the recording
// said nothing about it.
func newMistralProvider(config Config, transport Transport) *mistralProvider {
	return &mistralProvider{
		config:    config,
		http:      &http.Client{Transport: transport},
		apiKey:    os.Getenv(EnvMistralAPIKey),
		replaying: transport.Mode() == TransportReplaying,
	}
}

func (p *mistralProvider) name() Provider { return ProviderMistral }

// countTokens is the pre-flight admission estimate (FR-047a).
//
// Mistral publishes no `count_tokens` endpoint, so this is a local estimate over the exact body
// that is about to be sent: four bytes per token, the usual order of magnitude for English prose
// and JSON. Two properties matter and both hold. It makes **no network call**, so a replayed run
// admits without a vendor account and without consuming a recorded exchange. And it is a
// function of the request alone, so the same request admits the same way every time — which is
// what the loop's determinism needs. What it is not is a re-measurement of the tokenizer, and
// the admission check does not need one: it needs a stable, conservative number.
func (p *mistralProvider) countTokens(_ context.Context, rc RoleConfig, req Request) (int64, error) {
	body, err := p.body(rc, req)
	if err != nil {
		return 0, err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, fmt.Errorf("model: mistral: estimate tokens: %w", err)
	}
	return int64(len(raw) / bytesPerToken), nil
}

// complete issues one chat-completions call and decodes it into this package's vocabulary.
func (p *mistralProvider) complete(ctx context.Context, rc RoleConfig, req Request) (*Response, error) {
	if req.Stream {
		return nil, errors.New(
			"model: mistral: streaming is not implemented on this provider; the engine's loop does " +
				"not stream and a streamed recording would be a different artefact from a JSON one")
	}
	if p.apiKey == "" && !p.replaying {
		return nil, fmt.Errorf(
			"model: mistral: $%s is not set; a live run needs a credential and this one would fail "+
				"on its first turn", EnvMistralAPIKey)
	}

	body, err := p.body(rc, req)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("model: mistral: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, mistralEndpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("model: mistral: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	resp, err := p.do(ctx, httpReq, raw)
	if err != nil {
		return nil, err
	}
	answer, readErr := io.ReadAll(resp.Body)
	closeErr := resp.Body.Close()
	if readErr != nil {
		return nil, fmt.Errorf("model: mistral: read response: %w", readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("model: mistral: close response: %w", closeErr)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &APIError{Provider: ProviderMistral, Status: resp.StatusCode, Body: truncate(string(answer))}
	}
	return decodeMistral(answer)
}

// transportAttempts is how many times one turn is put on the wire before the run gives up, and
// transportBackoff the pause between attempts.
//
// This is not a retry policy for the vendor's *answers* — a 429, a 500 and a refusal are all
// answers, and every one of them is recorded and surfaced rather than retried. It is a retry for
// the **socket**: a full corpus evaluation is an hour of requests, and a TCP connection reset on
// turn 40 of 180 ended the whole run with no report at all. That happened, on the second live
// evaluation of this corpus, and it is the only reason this exists.
//
// It is safe because it only ever fires when *no* response was read: either the request never
// reached the vendor or the answer never came back, and in both cases the turn has produced
// nothing the engine can record. The request body is byte-identical on every attempt, so a
// recording made through a retry is the recording of the attempt that answered.
const (
	transportAttempts = 3
	transportBackoff  = 2 * time.Second
)

// do issues the request, retrying a transport failure and nothing else.
func (p *mistralProvider) do(ctx context.Context, req *http.Request, body []byte) (*http.Response, error) {
	var last error
	for attempt := 1; attempt <= transportAttempts; attempt++ {
		if attempt > 1 {
			// A fresh body reader: the previous attempt consumed the old one.
			req.Body = io.NopCloser(bytes.NewReader(body))
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(transportBackoff):
			}
		}
		resp, err := p.http.Do(req)
		if err == nil {
			return resp, nil
		}
		if d := DivergenceOf(err); d != nil {
			// A replay that does not hold this exchange is a verdict, not a flaky socket.
			return nil, d
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("model: mistral: chat/completions: %w", err)
		}
		last = err
	}
	return nil, fmt.Errorf("model: mistral: chat/completions: %d transport attempts failed, last: %w",
		transportAttempts, last)
}

// maxErrorBody bounds what a failure quotes back. A vendor error page can be a whole HTML
// document, and an error nobody can read is an error nobody acts on.
const maxErrorBody = 2000

func truncate(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxErrorBody {
		return s
	}
	return s[:maxErrorBody] + "… (truncated)"
}

// ---------------------------------------------------------------------------------------------
// The request body.
// ---------------------------------------------------------------------------------------------

// mistralRequest is the exact body sent. Every field is explicit and every optional one is
// omitted when unset, so the recorded body says what was asked and nothing more.
type mistralRequest struct {
	Model    string           `json:"model"`
	Messages []mistralMessage `json:"messages"`
	// MaxTokens bounds the response, thinking included.
	MaxTokens int64 `json:"max_tokens"`
	// Tools is the tool set; it is the head of the cached prefix, so it is the same set on every
	// turn of one investigation.
	Tools []mistralTool `json:"tools,omitempty"`
	// ResponseFormat is the structured-output schema.
	ResponseFormat *mistralResponseFormat `json:"response_format,omitempty"`
	// ReasoningEffort is Mistral's spelling of the effort ladder. It is sent only when the role
	// configures one.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	// PromptCacheKey routes turns that share a prefix to the same cache. It is derived from the
	// stable prefix alone (see cacheKey), so it is a function of the schema version and carries
	// nothing volatile — a key with a timestamp in it would be a cache that never hits and a
	// recording that never reproduces.
	PromptCacheKey string `json:"prompt_cache_key,omitempty"`
}

// mistralMessage is one message. `content` is always present, including the empty string on an
// assistant turn that only called tools, because that is the shape the API documents.
type mistralMessage struct {
	Role       string            `json:"role"`
	Content    string            `json:"content"`
	ToolCalls  []mistralToolCall `json:"tool_calls,omitempty"`
	ToolCallID string            `json:"tool_call_id,omitempty"`
	Name       string            `json:"name,omitempty"`
}

type mistralToolCall struct {
	ID       string              `json:"id"`
	Type     string              `json:"type"`
	Function mistralCallFunction `json:"function"`
}

// mistralCallFunction carries the arguments as the API does: a JSON **string**, not an object.
type mistralCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type mistralTool struct {
	Type     string              `json:"type"`
	Function mistralToolFunction `json:"function"`
}

type mistralToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Strict      bool           `json:"strict"`
	Parameters  map[string]any `json:"parameters"`
}

type mistralResponseFormat struct {
	Type       string             `json:"type"`
	JSONSchema *mistralJSONSchema `json:"json_schema,omitempty"`
}

type mistralJSONSchema struct {
	Name   string         `json:"name"`
	Schema map[string]any `json:"schema"`
	Strict bool           `json:"strict"`
}

// body composes the request. It is the only place a Mistral body is built, so "the recorded
// request is the request" is a property of one function rather than of a convention.
func (p *mistralProvider) body(rc RoleConfig, req Request) (*mistralRequest, error) {
	if req.System == "" {
		return nil, errors.New(
			"model: a request carries no system prefix; the prefix is the contract (contracts/prompting.md)")
	}
	if len(req.Messages) == 0 {
		return nil, errors.New("model: a request carries no messages")
	}

	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = DefaultMaxTokens
	}

	messages, err := mistralMessages(req)
	if err != nil {
		return nil, err
	}

	out := &mistralRequest{
		Model:           rc.Model,
		Messages:        messages,
		MaxTokens:       maxTokens,
		ReasoningEffort: rc.Effort,
		PromptCacheKey:  cacheKey(req),
	}
	for _, tool := range req.Tools {
		out.Tools = append(out.Tools, mistralTool{
			Type: "function",
			Function: mistralToolFunction{
				Name:        tool.Name,
				Description: tool.Description,
				Strict:      true,
				Parameters:  tool.Schema(),
			},
		})
	}
	if req.Format != nil {
		out.ResponseFormat = &mistralResponseFormat{
			Type: "json_schema",
			JSONSchema: &mistralJSONSchema{
				Name:   req.Format.Name,
				Schema: req.Format.Schema,
				Strict: true,
			},
		}
	}
	// No temperature, no top_p, no random_seed: this feature sends no sampling parameter to any
	// provider. The Anthropic models reject them outright; here they would merely be a knob
	// nobody evaluated, set to a value nobody recorded a reason for.
	return out, nil
}

// mistralMessages is the whole of the mapping, and the two interesting cases are the turn-scoped
// system message and the tool result.
//
// **The turn-scoped ledger.** Anthropic publishes `clear_at: "next_user_message"`: the ledger is
// re-rendered by *appending* a system message that renders for one turn and is then cleared in
// place, so history is never edited. Mistral has no equivalent. The mapping is to render the
// stable prefix as the **first** message and whichever turn-scoped renders are still live as a
// **trailing** system message — a render is live until a user message follows it, which is
// exactly what `next_user_message` means. Cleared renders are dropped rather than accumulated, so
// the transcript does not fill up with stale ledgers arguing with each other.
//
// **The render goes at the end, and that is the whole of what makes the prefix cache work here.**
// The first version of this mapping rewrote the single leading system message each turn — stable
// prefix, then the live renders — on the reasoning that the shared *token* prefix was still the
// prefix plus the tools, which is the region Anthropic's single breakpoint covers. That reasoning
// is wrong on this platform, and it was measured wrong: over three recorded live turns whose
// leading system messages shared their first 10,340 of 10,911 characters,
// `prompt_tokens_details.cached_tokens` was **0 on every turn**. Re-issuing those same recorded
// bodies with nothing changed but the leading system message held byte-identical turned that into
// 6,848 cached tokens of 10,761 — so the vendor caches for this model, and it matches on a prefix
// of *whole messages*: a message that differs anywhere invalidates itself and everything after
// it, however long the common head inside it is. Moving the render to the end makes every message
// before it byte-identical from one turn to the next, which is what the cache needs.
//
// The **injection barrier** (FR-017) is untouched by either arrangement: a worker's output reaches
// the model only ever as a `tool` message, and nothing a worker returns can become a system
// message, because both system messages here are composed out of the prompt prefix and the
// engine's own ledger render.
//
// **Tool results.** Anthropic carries them as `tool_result` blocks inside one user message, which
// is what keeps parallel tool calling healthy. Mistral carries one `tool` message per result. The
// expansion is mechanical and order-preserving. `is_error` has no field on a Mistral `tool`
// message, so a failed call is marked in the content itself — a failed worker call is an answer
// and the model must be able to tell it from a successful one (FR-027).
func mistralMessages(req Request) ([]mistralMessage, error) {
	lastUser := -1
	for i, message := range req.Messages {
		if message.Role == RoleUser {
			lastUser = i
		}
	}

	// head is the stable prefix, which must stay byte-identical from turn to turn; live is the
	// turn-scoped render, which by definition does not, and therefore goes last.
	head := []string{req.System}
	var live []string
	out := make([]mistralMessage, 0, len(req.Messages)+2)

	for i, message := range req.Messages {
		switch message.Role {
		case RoleSystem:
			text := blockText(message.Blocks)
			switch {
			case text == "":
			case message.ClearAt == "":
				// An unscoped system message is always live and is a standing instruction, so it
				// belongs with the prefix.
				head = append(head, text)
			case i > lastUser:
				// A render is live until a user message follows it. A turn-scoped one that has
				// been cleared is dropped.
				live = append(live, text)
			}
		case RoleUser, RoleAssistant:
			if message.ClearAt != "" {
				return nil, fmt.Errorf(
					"model: message %d is a %s message with clear_at %q; only a system message is turn-scoped",
					i, message.Role, message.ClearAt)
			}
			rendered, err := mistralTurn(i, message)
			if err != nil {
				return nil, err
			}
			out = append(out, rendered...)
		default:
			return nil, fmt.Errorf("model: message %d: role %q is not one of user, assistant, system",
				i, message.Role)
		}
	}

	messages := append([]mistralMessage{{
		Role:    "system",
		Content: strings.Join(head, "\n\n"),
	}}, out...)
	if len(live) > 0 {
		messages = append(messages, mistralMessage{
			Role:    "system",
			Content: strings.Join(live, "\n\n"),
		})
	}
	return messages, nil
}

// mistralTurn renders one user or assistant message as the one or more Mistral messages it maps
// onto.
func mistralTurn(index int, message Message) ([]mistralMessage, error) {
	var (
		text      strings.Builder
		toolCalls []mistralToolCall
		results   []mistralMessage
	)
	for _, block := range message.Blocks {
		switch block.Kind {
		case BlockText:
			if block.Text == "" {
				continue
			}
			if text.Len() > 0 {
				text.WriteString("\n\n")
			}
			text.WriteString(block.Text)
		case BlockThinking:
			// Deliberately dropped. GLM and the magistral models return their reasoning as a
			// `thinking` chunk; it is recorded in the trajectory (the response body is captured
			// verbatim, and the block survives on the Response) but it is not sent back. There is
			// no signature to echo and no append-only constraint to honour here — the constraint
			// that made Anthropic thinking blocks non-negotiable does not exist on this API — and
			// re-sending a page of unsigned reasoning every turn is tokens spent on nothing.
			continue
		case BlockToolUse:
			arguments := "{}"
			if len(block.ToolInput) > 0 {
				compact, err := compactJSON(block.ToolInput)
				if err != nil {
					return nil, fmt.Errorf("model: message %d: tool_use %s: input is not JSON: %w",
						index, block.ToolUseID, err)
				}
				arguments = compact
			}
			toolCalls = append(toolCalls, mistralToolCall{
				ID:       block.ToolUseID,
				Type:     "function",
				Function: mistralCallFunction{Name: block.ToolName, Arguments: arguments},
			})
		case BlockToolResult:
			content := block.Text
			if block.IsError {
				content = toolErrorPrefix + content
			}
			results = append(results, mistralMessage{
				Role:       "tool",
				Content:    content,
				ToolCallID: block.ToolUseID,
			})
		default:
			return nil, fmt.Errorf("model: message %d: block kind %q is not one of text, thinking, tool_use, tool_result",
				index, block.Kind)
		}
	}

	out := make([]mistralMessage, 0, len(results)+1)
	if text.Len() > 0 || len(toolCalls) > 0 {
		out = append(out, mistralMessage{
			Role:      message.Role,
			Content:   text.String(),
			ToolCalls: toolCalls,
		})
	}
	// The results follow the message that may have carried text with them, in issue order. A
	// `tool` message has no role of its own in the engine's vocabulary: it is always the answer
	// to a `tool_use` the assistant made.
	return append(out, results...), nil
}

// toolErrorPrefix marks a failed tool result on a provider whose `tool` message has no
// `is_error`. It is a prefix rather than a wrapper object so the result still reads as the text
// the worker produced.
const toolErrorPrefix = "[tool error] "

func blockText(blocks []Block) string {
	var b strings.Builder
	for _, block := range blocks {
		if block.Kind != BlockText || block.Text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(block.Text)
	}
	return b.String()
}

func compactJSON(raw json.RawMessage) (string, error) {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// cacheKey derives the prompt cache key from the stable prefix alone: the system prefix and the
// tool names, in order. It is therefore a function of the schema version — stable for the life of
// one, different the moment the prefix changes — which is exactly the routing hint a prefix cache
// wants, and it puts nothing volatile into a recorded request body.
func cacheKey(req Request) string {
	h := sha256.New()
	h.Write([]byte(req.System))
	for _, tool := range req.Tools {
		h.Write([]byte{0})
		h.Write([]byte(tool.Name))
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// ---------------------------------------------------------------------------------------------
// The response.
// ---------------------------------------------------------------------------------------------

type mistralCompletion struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Index        int    `json:"index"`
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Role      string            `json:"role"`
			Content   json.RawMessage   `json:"content"`
			ToolCalls []mistralToolCall `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens        int64 `json:"prompt_tokens"`
		CompletionTokens    int64 `json:"completion_tokens"`
		TotalTokens         int64 `json:"total_tokens"`
		PromptTokensDetails *struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

// mistralChunk is one element of a structured `content` array.
type mistralChunk struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Thinking []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"thinking"`
}

// decodeMistral turns the vendor's answer into this package's vocabulary.
func decodeMistral(raw []byte) (*Response, error) {
	var completion mistralCompletion
	if err := json.Unmarshal(raw, &completion); err != nil {
		return nil, fmt.Errorf("model: mistral: decode response: %w", err)
	}
	if len(completion.Choices) == 0 {
		return nil, errors.New("model: mistral: the response carries no choices; " +
			"a turn with no content is a turn the engine cannot record")
	}
	choice := completion.Choices[0]

	out := &Response{
		ID:         completion.ID,
		Model:      completion.Model,
		StopReason: mistralStopReason(choice.FinishReason),
		Usage:      mistralUsage(completion),
	}
	if out.Refused() {
		out.RefusalCategory = choice.FinishReason
		out.RefusalExplanation = strings.TrimSpace(textOf(choice.Message.Content))
		if out.RefusalExplanation == "" {
			out.RefusalExplanation = "the provider declined the request and returned finish_reason " +
				choice.FinishReason
		}
	}

	blocks, err := mistralBlocks(choice.Message.Content)
	if err != nil {
		return nil, err
	}
	for _, call := range choice.Message.ToolCalls {
		arguments := strings.TrimSpace(call.Function.Arguments)
		if arguments == "" {
			arguments = "{}"
		}
		if !json.Valid([]byte(arguments)) {
			return nil, fmt.Errorf(
				"model: mistral: tool call %s (%s) carries arguments that are not JSON; the engine "+
					"decodes a tool call before it dispatches it", call.ID, call.Function.Name)
		}
		blocks = append(blocks, Block{
			Kind:      BlockToolUse,
			ToolUseID: call.ID,
			ToolName:  call.Function.Name,
			ToolInput: json.RawMessage(arguments),
		})
	}
	out.Blocks = blocks
	return out, nil
}

// mistralBlocks decodes `content`, which is either a plain string or an array of typed chunks.
//
// A `thinking` chunk becomes a thinking block, which keeps the model's reasoning out of the text
// the engine reasons over — `Response.Text()` concatenates text blocks only — while still putting
// it in the trajectory, which is what `thinking_display: summarized` buys on the other provider.
func mistralBlocks(content json.RawMessage) ([]Block, error) {
	if len(bytes.TrimSpace(content)) == 0 || string(bytes.TrimSpace(content)) == "null" {
		return nil, nil
	}
	var text string
	if err := json.Unmarshal(content, &text); err == nil {
		if strings.TrimSpace(text) == "" {
			return nil, nil
		}
		return []Block{{Kind: BlockText, Text: text}}, nil
	}

	var chunks []mistralChunk
	if err := json.Unmarshal(content, &chunks); err != nil {
		return nil, fmt.Errorf("model: mistral: decode content: %w", err)
	}
	out := make([]Block, 0, len(chunks))
	for _, chunk := range chunks {
		switch chunk.Type {
		case "text":
			if chunk.Text != "" {
				out = append(out, Block{Kind: BlockText, Text: chunk.Text})
			}
		case "thinking":
			var b strings.Builder
			for _, part := range chunk.Thinking {
				b.WriteString(part.Text)
			}
			if b.Len() > 0 {
				out = append(out, Block{Kind: BlockThinking, Thinking: b.String()})
			}
		default:
			// A chunk type this build has never seen — an image, a citation — is skipped rather
			// than guessed at. The exact bytes are in the trajectory either way.
			continue
		}
	}
	return out, nil
}

// textOf is the text of a `content` value whatever shape it took, for a refusal explanation.
func textOf(content json.RawMessage) string {
	blocks, err := mistralBlocks(content)
	if err != nil {
		return ""
	}
	var parts []string
	for _, block := range blocks {
		if block.Kind == BlockText {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// mistralStopReason translates a finish reason into the engine's vocabulary.
//
// The translation is the point of the seam: the loop asks `resp.Refused()` and compares against
// `tool_use`, and it must get the same answer whoever produced the turn. An unrecognised reason
// is passed through verbatim rather than mapped onto a guess — a stop reason nobody published is
// something a reader must be able to see.
func mistralStopReason(finish string) string {
	switch finish {
	case "stop":
		return StopEndTurn
	case "tool_calls":
		return StopToolUse
	case "length", "model_length":
		return StopMaxTokens
	case "content_filter", "guardrail", "refusal":
		return StopRefusal
	case "":
		return StopEndTurn
	default:
		return finish
	}
}

// mistralUsage maps the usage block onto the four published classes.
//
// Two adjustments, both of which exist so that a Mistral number and an Anthropic number mean the
// same thing in `tokens_by_model_and_class` (FR-048):
//
//   - `prompt_tokens` **includes** the cached tokens, where Anthropic's `input_tokens` excludes
//     them. The cached count is subtracted so the classes partition the spend instead of
//     double-counting it.
//   - there is no cache *write* class. Mistral's prefix cache is automatic and a write is billed
//     at the ordinary input rate, so cache_write is always zero here and the price table says so.
func mistralUsage(completion mistralCompletion) Usage {
	cached := int64(0)
	if completion.Usage.PromptTokensDetails != nil {
		cached = completion.Usage.PromptTokensDetails.CachedTokens
	}
	input := completion.Usage.PromptTokens - cached
	if input < 0 {
		input = 0
	}
	return Usage{
		Input:     input,
		CacheRead: cached,
		Output:    completion.Usage.CompletionTokens,
	}
}
