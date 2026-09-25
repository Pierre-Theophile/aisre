// SPDX-License-Identifier: Apache-2.0

package model_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/investigation/model"
)

// The Mistral provider (Track L; FR-017, FR-041, FR-042a, FR-045b, FR-048).
//
// Every test here runs against a fake transport built from a hand-written response body — the
// exact body the API returns, taken from a live smoke call while the client was written. There
// is no API key in CI and there must never need to be: what is exercised is the real request
// build, the real decoder and the real usage accounting, with only the wire faked.

// mistralPair is the production configuration: Mistral, serving GLM as the investigator.
func mistralPair(t *testing.T) (model.Config, model.Prices) {
	t.Helper()
	config, prices, err := model.LoadPair(configPath, pricesPath)
	if err != nil {
		t.Fatalf("load the production configuration: %v", err)
	}
	return config, prices
}

func mistralClient(t *testing.T, transport model.Transport) *model.Client {
	t.Helper()
	config, prices := mistralPair(t)
	client, err := model.NewClientWithTransport(config, prices, transport)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return client
}

// canned builds a tolerant replaying transport over raw response bodies.
func canned(bodies ...string) model.Transport {
	exchanges := make([]model.Exchange, 0, len(bodies))
	for _, body := range bodies {
		exchanges = append(exchanges, model.Exchange{
			Method: "POST", Path: "/v1/chat/completions", Status: 200,
			ResponseBody: json.RawMessage(body),
		})
	}
	return model.NewReplayingTransportTolerant(exchanges)
}

// glmToolCall is a real GLM turn: reasoning as a `thinking` chunk, one tool call, and usage with
// `prompt_tokens_details`.
const glmToolCall = `{
  "id": "2f4aa782bfe84178a3032ed6a6f69c57",
  "object": "chat.completion",
  "model": "zai-glm-5-3",
  "choices": [{
    "index": 0,
    "finish_reason": "tool_calls",
    "message": {
      "role": "assistant",
      "content": [{"type": "thinking", "thinking": [{"type": "text", "text": "ptr-1 is the one to compare."}], "closed": true}],
      "tool_calls": [{
        "id": "chatcmpl-tool-a0a825d8dc333581",
        "type": "function",
        "function": {"name": "compare", "arguments": "{\"pointer_id\": \"ptr-1\"}"}
      }]
    }
  }],
  "usage": {"prompt_tokens": 11000, "completion_tokens": 640, "total_tokens": 11640,
            "prompt_tokens_details": {"cached_tokens": 9800}}
}`

// TestAMistralToolCallTurnDecodesIntoTheEnginesVocabulary (FR-042a, FR-048).
func TestAMistralToolCallTurnDecodesIntoTheEnginesVocabulary(t *testing.T) {
	t.Parallel()

	client := mistralClient(t, canned(glmToolCall))
	resp, err := client.Complete(context.Background(), request())
	if err != nil {
		t.Fatalf("complete: %v", err)
	}

	// `tool_calls` is the engine's `tool_use`. The loop compares against that spelling, and it
	// must get the same answer whoever produced the turn.
	if resp.StopReason != model.StopToolUse {
		t.Errorf("stop reason = %q, want %s", resp.StopReason, model.StopToolUse)
	}
	if resp.Model != "zai-glm-5-3" {
		t.Errorf("model = %q", resp.Model)
	}

	uses := resp.ToolUses()
	if len(uses) != 1 || uses[0].ToolName != "compare" {
		t.Fatalf("tool uses = %+v", uses)
	}
	if uses[0].ToolUseID != "chatcmpl-tool-a0a825d8dc333581" {
		t.Errorf("tool_use id = %q; the id is echoed verbatim, because the tool_result answers it",
			uses[0].ToolUseID)
	}
	var input map[string]any
	if err := json.Unmarshal(uses[0].ToolInput, &input); err != nil {
		t.Fatalf("tool input is not JSON: %v", err)
	}
	if input["pointer_id"] != "ptr-1" {
		t.Errorf("tool input = %v", input)
	}

	// The reasoning is recorded as a thinking block and stays out of the text the engine reasons
	// over — which is what `thinking_display: summarized` buys on the other provider.
	var thinking int
	for _, block := range resp.Blocks {
		if block.Kind == model.BlockThinking {
			thinking++
			if !strings.Contains(block.Thinking, "ptr-1") {
				t.Errorf("thinking block is empty or wrong: %q", block.Thinking)
			}
		}
	}
	if thinking != 1 {
		t.Errorf("thinking blocks = %d, want 1", thinking)
	}
	if strings.Contains(resp.Text(), "ptr-1 is the one") {
		t.Error("the model's reasoning leaked into the text the engine reasons over")
	}

	// `prompt_tokens` includes the cached tokens; the published classes must partition the spend
	// rather than double-count it (FR-048).
	if resp.Usage.Input != 1200 || resp.Usage.CacheRead != 9800 || resp.Usage.Output != 640 {
		t.Errorf("usage = %+v, want input 1200, cache_read 9800, output 640", resp.Usage)
	}
	if resp.Usage.CacheWrite != 0 {
		t.Errorf("cache_write = %d; this provider bills a cache populate as input and reports no "+
			"write class", resp.Usage.CacheWrite)
	}

	if resp.RequestDigest == "" || resp.ResponseDigest == "" {
		t.Error("the call was not digested; layer 1 matches on those digests")
	}
	if resp.Mode != model.TransportReplaying {
		t.Errorf("mode = %s, want the transport's own mode", resp.Mode)
	}
}

// TestTheMistralRequestCarriesTheToolSetAndNoSamplingParameters is the request half of the
// mapping: the tool schema, `max_tokens`, the effort the role configures, and nothing else.
func TestTheMistralRequestCarriesTheToolSetAndNoSamplingParameters(t *testing.T) {
	t.Parallel()

	client := mistralClient(t, canned(glmToolCall))
	resp, err := client.Complete(context.Background(), request())
	if err != nil {
		t.Fatalf("complete: %v", err)
	}

	var body struct {
		Model     string `json:"model"`
		MaxTokens int64  `json:"max_tokens"`
		Tools     []struct {
			Type     string `json:"type"`
			Function struct {
				Name       string         `json:"name"`
				Strict     bool           `json:"strict"`
				Parameters map[string]any `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
		ReasoningEffort string `json:"reasoning_effort"`
		PromptCacheKey  string `json:"prompt_cache_key"`
		Temperature     *any   `json:"temperature"`
		TopP            *any   `json:"top_p"`
		RandomSeed      *any   `json:"random_seed"`
		ToolChoice      *any   `json:"tool_choice"`
	}
	if err := json.Unmarshal(resp.RequestBody, &body); err != nil {
		t.Fatalf("the recorded request is not JSON: %v", err)
	}

	if body.Model != "zai-glm-5-3" {
		t.Errorf("model = %q", body.Model)
	}
	if body.MaxTokens != model.DefaultMaxTokens {
		t.Errorf("max_tokens = %d, want %d", body.MaxTokens, model.DefaultMaxTokens)
	}
	if len(body.Tools) != 1 || body.Tools[0].Type != "function" || body.Tools[0].Function.Name != "compare" {
		t.Fatalf("tools = %+v", body.Tools)
	}
	if !body.Tools[0].Function.Strict {
		t.Error("the tool is not strict; a schema-invalid judgment is the failure strict: true prevents")
	}
	if body.Tools[0].Function.Parameters["additionalProperties"] != false {
		t.Errorf("the tool schema does not close itself: %v", body.Tools[0].Function.Parameters)
	}
	// The configured effort travels as Mistral spells it.
	if body.ReasoningEffort != "high" {
		t.Errorf("reasoning_effort = %q, want the role's configured effort", body.ReasoningEffort)
	}
	// No sampling parameter is sent to any provider, and forced tool use is never asked for.
	for name, value := range map[string]*any{
		"temperature": body.Temperature, "top_p": body.TopP,
		"random_seed": body.RandomSeed, "tool_choice": body.ToolChoice,
	} {
		if value != nil {
			t.Errorf("the request carries %s; this feature sends no sampling parameter and never "+
				"forces a tool call", name)
		}
	}
	// The cache key is a function of the stable prefix, so it is stable for the life of a schema
	// version and carries nothing volatile into a recorded body.
	if len(body.PromptCacheKey) != 32 {
		t.Errorf("prompt_cache_key = %q, want a 32-character digest of the stable prefix", body.PromptCacheKey)
	}
}

// TestTheLedgerRendersAfterTheStablePrefixAndNeverInsideIt is the mapping that has no equivalent:
// Anthropic's `clear_at: "next_user_message"` against a provider with no such field.
//
// A turn-scoped render is live until a user message follows it. The live ones render as a
// **trailing** system message; the cleared ones are dropped rather than accumulated. The leading
// system message is the stable prefix and nothing else, which is what this test pins: this
// provider's prefix cache matches on whole messages, so a render folded in behind the prefix
// invalidates the message it is in and every message after it, and `cached_tokens` comes back 0
// on every turn of every run (measured; see mistral.go's note on mistralMessages).
//
// And the injection barrier holds under either arrangement: a worker's output is a `tool` message
// and nothing else (FR-017).
func TestTheLedgerRendersAfterTheStablePrefixAndNeverInsideIt(t *testing.T) {
	t.Parallel()

	req := request()
	req.Messages = []model.Message{
		{Role: model.RoleUser, Blocks: []model.Block{{Kind: model.BlockText, Text: "investigate checkout"}}},
		{Role: model.RoleSystem, ClearAt: model.ClearAtNextUserMessage,
			Blocks: []model.Block{{Kind: model.BlockText, Text: "LEDGER TURN 1 — stale"}}},
		{Role: model.RoleAssistant, Blocks: []model.Block{
			{Kind: model.BlockThinking, Thinking: "a page of reasoning nobody has to pay for twice"},
			{Kind: model.BlockText, Text: "comparing"},
			{Kind: model.BlockToolUse, ToolUseID: "call_1", ToolName: "compare",
				ToolInput: json.RawMessage(`{"pointer_id":"ptr-1"}`)},
		}},
		{Role: model.RoleUser, Blocks: []model.Block{
			{Kind: model.BlockToolResult, ToolUseID: "call_1", Text: "before 0.2% after 7.9%"},
			{Kind: model.BlockToolResult, ToolUseID: "call_2", Text: "the backend refused the term", IsError: true},
		}},
		{Role: model.RoleSystem, ClearAt: model.ClearAtNextUserMessage,
			Blocks: []model.Block{{Kind: model.BlockText, Text: "LEDGER TURN 2 — live"}}},
	}

	client := mistralClient(t, canned(glmToolCall))
	resp, err := client.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}

	var body struct {
		Messages []struct {
			Role       string `json:"role"`
			Content    string `json:"content"`
			ToolCallID string `json:"tool_call_id"`
			ToolCalls  []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(resp.RequestBody, &body); err != nil {
		t.Fatalf("the recorded request is not JSON: %v", err)
	}

	if len(body.Messages) == 0 || body.Messages[0].Role != "system" {
		t.Fatalf("the first message is not the system message: %+v", body.Messages)
	}
	// The leading system message is the stable prefix and *nothing else*. Equality, not a prefix
	// check: anything appended to it here is a byte that changes from turn to turn inside the
	// first message, and this provider's cache matches whole messages.
	if head := body.Messages[0].Content; head != req.System {
		t.Errorf("the leading system message is not the stable prefix alone:\n got %q\nwant %q",
			head, req.System)
	}

	// The live render arrives last, where it changes nothing before it.
	last := body.Messages[len(body.Messages)-1]
	if last.Role != "system" {
		t.Fatalf("the last message is %q, want the turn-scoped system render", last.Role)
	}
	if !strings.Contains(last.Content, "LEDGER TURN 2 — live") {
		t.Error("the live ledger render did not reach the trailing system message")
	}
	for _, m := range body.Messages {
		if strings.Contains(m.Content, "LEDGER TURN 1 — stale") {
			t.Error("a cleared ledger render was carried forward; clear_at means it renders once")
		}
	}

	// Two system messages and no more: the standing instruction and the turn's render. There is
	// no third channel for an instruction.
	var systems int
	for _, m := range body.Messages {
		if m.Role == "system" {
			systems++
		}
	}
	if systems != 2 {
		t.Errorf("system messages = %d, want exactly 2 (the stable prefix and the turn's render)", systems)
	}

	// Every worker result arrived as a `tool` message and nothing else (FR-017).
	roles := make([]string, 0, len(body.Messages))
	for _, m := range body.Messages {
		roles = append(roles, m.Role)
	}
	want := []string{"system", "user", "assistant", "tool", "tool", "system"}
	if strings.Join(roles, ",") != strings.Join(want, ",") {
		t.Fatalf("message roles = %v, want %v", roles, want)
	}

	assistant := body.Messages[2]
	if len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].ID != "call_1" {
		t.Fatalf("assistant tool calls = %+v", assistant.ToolCalls)
	}
	if assistant.ToolCalls[0].Function.Arguments != `{"pointer_id":"ptr-1"}` {
		t.Errorf("arguments = %q; the API carries them as a JSON string, not an object",
			assistant.ToolCalls[0].Function.Arguments)
	}
	if strings.Contains(assistant.Content, "nobody has to pay for twice") {
		t.Error("a thinking block was echoed back; there is no signature to carry and no " +
			"append-only constraint on this API")
	}

	if body.Messages[3].ToolCallID != "call_1" || body.Messages[3].Content != "before 0.2% after 7.9%" {
		t.Errorf("the first tool result did not survive: %+v", body.Messages[3])
	}
	// A failed worker call is an answer, and the model must be able to tell it from a successful
	// one on a provider whose `tool` message has no is_error (FR-027).
	if !strings.HasPrefix(body.Messages[4].Content, "[tool error] ") {
		t.Errorf("a failed tool result is indistinguishable from a successful one: %q",
			body.Messages[4].Content)
	}
}

// TestAMistralStructuredOutputTurnTravelsInResponseFormat (the verifier's path, FR-022a).
func TestAMistralStructuredOutputTurnTravelsInResponseFormat(t *testing.T) {
	t.Parallel()

	req := request()
	req.Role = model.RoleVerifier
	req.Tools = nil
	req.Format = &model.OutputFormat{
		Name: "verifier_verdicts",
		Schema: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{"verdict": map[string]any{"type": "string"}},
			"required":             []string{"verdict"},
			"additionalProperties": false,
		},
	}

	const verdict = `{"id":"v1","object":"chat.completion","model":"mistral-medium-latest",
	  "choices":[{"index":0,"finish_reason":"stop",
	    "message":{"role":"assistant","content":"{\"verdict\":\"supported\"}"}}],
	  "usage":{"prompt_tokens":2000,"completion_tokens":40,"total_tokens":2040}}`

	client := mistralClient(t, canned(verdict))
	resp, err := client.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}

	if resp.StopReason != model.StopEndTurn {
		t.Errorf("stop reason = %q, want %s", resp.StopReason, model.StopEndTurn)
	}
	if resp.Text() != `{"verdict":"supported"}` {
		t.Errorf("text = %q; a plain-string content must decode as one text block", resp.Text())
	}
	// A response with no `prompt_tokens_details` is not a response with a negative input count.
	if resp.Usage.Input != 2000 || resp.Usage.CacheRead != 0 || resp.Usage.Output != 40 {
		t.Errorf("usage = %+v", resp.Usage)
	}

	var body struct {
		Model          string `json:"model"`
		ResponseFormat struct {
			Type       string `json:"type"`
			JSONSchema struct {
				Name   string         `json:"name"`
				Strict bool           `json:"strict"`
				Schema map[string]any `json:"schema"`
			} `json:"json_schema"`
		} `json:"response_format"`
	}
	if err := json.Unmarshal(resp.RequestBody, &body); err != nil {
		t.Fatalf("the recorded request is not JSON: %v", err)
	}
	// FR-022a: the verifier is a different model from the investigator, and the role picks it.
	if body.Model != "mistral-medium-latest" {
		t.Errorf("the verifier ran on %q", body.Model)
	}
	if body.ResponseFormat.Type != "json_schema" {
		t.Errorf("response_format.type = %q", body.ResponseFormat.Type)
	}
	if body.ResponseFormat.JSONSchema.Name != "verifier_verdicts" || !body.ResponseFormat.JSONSchema.Strict {
		t.Errorf("json_schema = %+v", body.ResponseFormat.JSONSchema)
	}
	if body.ResponseFormat.JSONSchema.Schema["additionalProperties"] != false {
		t.Errorf("the output schema does not close itself: %v", body.ResponseFormat.JSONSchema.Schema)
	}
}

// TestAMistralRefusalIsSurfacedAndNeverRetried (FR-045b).
func TestAMistralRefusalIsSurfacedAndNeverRetried(t *testing.T) {
	t.Parallel()

	const refusal = `{"id":"r1","object":"chat.completion","model":"zai-glm-5-3",
	  "choices":[{"index":0,"finish_reason":"content_filter",
	    "message":{"role":"assistant","content":"this reads as an intrusion attempt"}}],
	  "usage":{"prompt_tokens":900,"completion_tokens":0,"total_tokens":900}}`

	transport := canned(refusal)
	client := mistralClient(t, transport)
	resp, err := client.Complete(context.Background(), request())
	if err != nil {
		t.Fatalf("a refusal was returned as an error rather than a recorded terminal condition: %v", err)
	}
	if !resp.Refused() {
		t.Fatalf("stop reason = %q, want %s", resp.StopReason, model.StopRefusal)
	}
	if resp.RefusalCategory != "content_filter" {
		t.Errorf("refusal category = %q; on this provider the finish reason is the category",
			resp.RefusalCategory)
	}
	if !strings.Contains(resp.RefusalExplanation, "intrusion attempt") {
		t.Errorf("refusal explanation = %q", resp.RefusalExplanation)
	}
	if len(transport.Exchanges()) != 1 {
		t.Errorf("the refused call was retried: %d exchanges", len(transport.Exchanges()))
	}
}

// TestAMistralHTTPErrorIsTyped: a vendor status is a typed error, not a formatted string, so a
// caller can tell a 429 from a 400 without parsing prose.
func TestAMistralHTTPErrorIsTyped(t *testing.T) {
	t.Parallel()

	transport := model.NewReplayingTransportTolerant([]model.Exchange{{
		Method: "POST", Path: "/v1/chat/completions", Status: 429,
		ResponseBody: json.RawMessage(`{"object":"error","message":"rate limited"}`),
	}})
	client := mistralClient(t, transport)

	_, err := client.Complete(context.Background(), request())
	if err == nil {
		t.Fatal("a 429 was treated as a successful turn")
	}
	var apiErr *model.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error %v is not a *model.APIError", err)
	}
	if apiErr.Status != 429 || apiErr.Provider != model.ProviderMistral {
		t.Errorf("api error = %+v", apiErr)
	}
	if !apiErr.Retryable() {
		t.Error("a 429 is not reported as retryable")
	}
	if !strings.Contains(apiErr.Error(), "rate limited") {
		t.Errorf("the error does not quote the vendor's own explanation: %v", apiErr)
	}
}

// TestAMistralRecordingReplaysWithNoNetwork is the property the whole seam exists for: a Mistral
// exchange recorded through the shared transport is matched back by canonical request digest,
// exactly as an Anthropic one is (FR-041, FR-042a).
func TestAMistralRecordingReplaysWithNoNetwork(t *testing.T) {
	t.Parallel()

	// "Recording" here is a tolerant replay standing in for the vendor; what matters is that the
	// exchanges it produced go back through the *strict* replayer and match.
	config, prices := mistralPair(t)
	recording, err := model.NewClientWithTransport(config, prices, canned(glmToolCall, glmToolCall))
	if err != nil {
		t.Fatalf("recording client: %v", err)
	}
	for i := range 2 {
		if _, err := recording.Complete(context.Background(), request()); err != nil {
			t.Fatalf("complete %d: %v", i+1, err)
		}
	}

	replay, err := model.NewClientWithTransport(config, prices,
		model.NewReplayingTransport(recording.Transport().Exchanges()))
	if err != nil {
		t.Fatalf("replay client: %v", err)
	}
	for i := range 2 {
		resp, err := replay.Complete(context.Background(), request())
		if err != nil {
			t.Fatalf("replay %d: %v", i+1, err)
		}
		if got, want := resp.RequestDigest, recording.Transport().Exchanges()[i].RequestDigest; got != want {
			t.Errorf("replay %d: request digest %s, recorded %s", i+1, got, want)
		}
	}

	// A request the recording does not answer names the diverging record rather than improvising.
	diverged := request()
	diverged.System = "a prefix nobody recorded"
	if _, err := replay.Complete(context.Background(), diverged); err == nil {
		t.Fatal("a replay answered a question its recording does not hold")
	} else if d := model.DivergenceOf(err); d == nil {
		t.Fatalf("error %v is not a divergence", err)
	}
}

// TestCountTokensOnMistralMakesNoCall: the admission estimate is local, because a replayed run
// must admit without a vendor account and without consuming a recorded exchange (FR-047a).
func TestCountTokensOnMistralMakesNoCall(t *testing.T) {
	t.Parallel()

	transport := canned(glmToolCall)
	client := mistralClient(t, transport)

	tokens, err := client.CountTokens(context.Background(), request())
	if err != nil {
		t.Fatalf("count tokens: %v", err)
	}
	if tokens <= 0 {
		t.Errorf("count = %d; an admission check against zero admits everything", tokens)
	}
	if len(transport.Exchanges()) != 0 {
		t.Errorf("the estimate crossed the wire: %d exchanges", len(transport.Exchanges()))
	}

	// It is a function of the request alone, which is what the loop's determinism needs.
	again, err := client.CountTokens(context.Background(), request())
	if err != nil {
		t.Fatalf("count tokens: %v", err)
	}
	if again != tokens {
		t.Errorf("the same request estimated %d then %d", tokens, again)
	}
}

// TestTheClientDispatchesPerRole: the seam's own claim. One client, two providers, chosen by the
// role's configuration — which is what FR-022a needs when "a different model" means "a different
// vendor".
func TestTheClientDispatchesPerRole(t *testing.T) {
	t.Parallel()

	config, prices := mistralPair(t)
	config.Version = "test-mixed"
	config.Roles[model.RoleVerifier] = model.RoleConfig{
		Provider: model.ProviderAnthropic,
		Model:    "claude-opus-5",
		Purpose:  "a verifier on the other vendor",
		Effort:   "high",
	}

	// Two canned answers in one recording: the investigator's, in Mistral's shape, then the
	// verifier's, in Anthropic's.
	const opusVerdict = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5",
	  "content":[{"type":"text","text":"supported"}],"stop_reason":"end_turn",
	  "usage":{"input_tokens":100,"output_tokens":10}}`
	transport := model.NewReplayingTransportTolerant([]model.Exchange{
		{Method: "POST", Path: "/v1/chat/completions", Status: 200, ResponseBody: json.RawMessage(glmToolCall)},
		{Method: "POST", Path: "/v1/messages", Status: 200, ResponseBody: json.RawMessage(opusVerdict)},
	})
	client, err := model.NewClientWithTransport(config, prices, transport)
	if err != nil {
		t.Fatalf("client: %v", err)
	}

	investigator, err := client.Complete(context.Background(), request())
	if err != nil {
		t.Fatalf("investigator turn: %v", err)
	}
	if investigator.Model != "zai-glm-5-3" {
		t.Errorf("investigator ran on %q", investigator.Model)
	}

	verifierReq := request()
	verifierReq.Role = model.RoleVerifier
	verifier, err := client.Complete(context.Background(), verifierReq)
	if err != nil {
		t.Fatalf("verifier turn: %v", err)
	}
	if verifier.Model != "claude-opus-5" {
		t.Errorf("verifier ran on %q", verifier.Model)
	}
	if verifier.Text() != "supported" {
		t.Errorf("verifier text = %q", verifier.Text())
	}

	// The two bodies are the two vendors' own shapes, from one client, in one recording.
	exchanges := transport.Exchanges()
	if len(exchanges) != 2 {
		t.Fatalf("exchanges = %d", len(exchanges))
	}
	if !strings.Contains(string(exchanges[0].RequestBody), `"reasoning_effort"`) {
		t.Errorf("the investigator's body is not a Mistral body: %s", exchanges[0].RequestBody)
	}
	if !strings.Contains(string(exchanges[1].RequestBody), `"cache_control"`) {
		t.Errorf("the verifier's body is not an Anthropic body: %s", exchanges[1].RequestBody)
	}
}

// TestAMismatchedProviderAndModelIsRefusedAtLoadTime: the pair check, at the seam this time
// rather than in the configuration loader, because a client built from a configuration nobody
// validated is the path this would otherwise sneak through.
func TestAMismatchedProviderAndModelIsRefusedAtLoadTime(t *testing.T) {
	t.Parallel()

	config, prices := mistralPair(t)
	rc := config.Roles[model.RoleInvestigator]
	rc.Provider = model.ProviderAnthropic
	config.Roles[model.RoleInvestigator] = rc

	_, err := model.NewClientWithTransport(config, prices, canned(glmToolCall))
	if err == nil {
		t.Fatal("a client was built for a provider/model pair that would 404 on its first turn")
	}
	if !strings.Contains(err.Error(), `served by "mistral"`) {
		t.Errorf("error %q does not name the provider that actually serves the model", err)
	}
}
