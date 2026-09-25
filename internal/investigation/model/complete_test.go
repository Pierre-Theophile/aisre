// SPDX-License-Identifier: Apache-2.0

package model_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/investigation/model"
	"github.com/Pierre-Theophile/aisre/internal/investigation/model/modeltest"
)

// The model boundary (tasks.md T060; FR-041, FR-042a, FR-045b, FR-048, research §2/§3).
//
// Every test here runs against a canned transport. There is no API key in CI and there must never
// need to be: the seam these tests exercise is the same one a fixture replay and Phase 7's gate
// use, so a test that passes here has exercised the real request build, the real SDK decoder and
// the real usage accounting.

const (
	investigatorModel = "claude-fable-5-1"
	verifierModel     = "claude-opus-5"
)

// configPair is the **Anthropic** checked-in configuration.
//
// The tests in this file assert the Anthropic request build to the byte — the cache breakpoint,
// `clear_at`, `output_config` — so they name the configuration that produces it rather than
// whichever one happens to be in force. That is also what keeps the alternative configuration
// alive: it is exercised by the suite, not merely checked in. The production configuration
// (Mistral, serving GLM) is exercised by mistral_test.go.
func configPair(t *testing.T) (model.Config, model.Prices) {
	t.Helper()
	config, prices, err := model.LoadPair(anthropicConfigPath, pricesPath)
	if err != nil {
		t.Fatalf("load the published configuration: %v", err)
	}
	return config, prices
}

func clientWith(t *testing.T, transport model.Transport) *model.Client {
	t.Helper()
	config, prices := configPair(t)
	client, err := model.NewClientWithTransport(config, prices, transport)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return client
}

func request() model.Request {
	return model.Request{
		Role:   model.RoleInvestigator,
		System: "# Role\n\nYou are the investigator.\n",
		Tools: []model.Tool{{
			Name:        "compare",
			Description: "compare a statistic across two windows",
			Properties:  map[string]any{"pointer_id": map[string]any{"type": "string"}},
			Required:    []string{"pointer_id"},
		}},
		Messages: []model.Message{{
			Role:   model.RoleUser,
			Blocks: []model.Block{{Kind: model.BlockText, Text: "investigate checkout"}},
		}},
	}
}

// TestACallIsRecordedExactlyAndAccountedByClass (FR-042a, FR-048).
func TestACallIsRecordedExactlyAndAccountedByClass(t *testing.T) {
	t.Parallel()

	transport, err := modeltest.Transport(investigatorModel, modeltest.Turn{
		Text: "looking at the version split",
		ToolUses: []modeltest.ToolUse{{
			ID: "toolu_1", Name: "compare", Input: map[string]any{"pointer_id": "ptr-1"},
		}},
		Usage: model.Usage{Input: 1200, CacheWrite: 9800, Output: 640},
	})
	if err != nil {
		t.Fatalf("canned transport: %v", err)
	}
	client := clientWith(t, transport)

	resp, err := client.Complete(context.Background(), request())
	if err != nil {
		t.Fatalf("complete: %v", err)
	}

	if resp.StopReason != "tool_use" {
		t.Errorf("stop reason = %q, want tool_use", resp.StopReason)
	}
	uses := resp.ToolUses()
	if len(uses) != 1 || uses[0].ToolName != "compare" {
		t.Fatalf("tool uses = %+v", uses)
	}
	var input map[string]any
	if err := json.Unmarshal(uses[0].ToolInput, &input); err != nil {
		t.Fatalf("tool input is not JSON: %v", err)
	}
	if input["pointer_id"] != "ptr-1" {
		t.Errorf("tool input = %v", input)
	}

	if resp.Usage.Input != 1200 || resp.Usage.CacheWrite != 9800 || resp.Usage.Output != 640 {
		t.Errorf("usage = %+v", resp.Usage)
	}
	byClass := resp.Usage.ByClass()
	if byClass[model.ClassCacheRead] != 0 {
		t.Errorf("a zero class was padded into the recorded map: %v", byClass)
	}
	if byClass[model.ClassCacheWrite] != 9800 {
		t.Errorf("cache_write = %d, want 9800", byClass[model.ClassCacheWrite])
	}

	if resp.RequestDigest == "" || resp.ResponseDigest == "" {
		t.Error("the call was not digested; layer 1 matches on those digests")
	}
	if len(resp.RequestBody) == 0 || len(resp.ResponseBody) == 0 {
		t.Error("the call recorded no bodies; layer 1's claim is byte-identical replay")
	}
	if resp.Mode != model.TransportReplaying {
		t.Errorf("mode = %s, want the transport's own mode on every recorded call", resp.Mode)
	}
}

// TestTheCacheBreakpointSitsAfterToolsAndSystem: the prefix is `tools → system`, with one
// breakpoint on the last system block, and everything volatile after it (research §3, FR-061).
func TestTheCacheBreakpointSitsAfterToolsAndSystem(t *testing.T) {
	t.Parallel()

	transport, err := modeltest.Transport(investigatorModel, modeltest.Turn{Text: "done"})
	if err != nil {
		t.Fatalf("canned transport: %v", err)
	}
	client := clientWith(t, transport)
	resp, err := client.Complete(context.Background(), request())
	if err != nil {
		t.Fatalf("complete: %v", err)
	}

	var body struct {
		System []struct {
			Text         string          `json:"text"`
			CacheControl json.RawMessage `json:"cache_control"`
		} `json:"system"`
		Tools []struct {
			Name   string `json:"name"`
			Strict bool   `json:"strict"`
			Schema struct {
				AdditionalProperties *bool `json:"additionalProperties"`
			} `json:"input_schema"`
		} `json:"tools"`
		Temperature *float64        `json:"temperature"`
		TopP        *float64        `json:"top_p"`
		TopK        *int            `json:"top_k"`
		ToolChoice  json.RawMessage `json:"tool_choice"`
		Thinking    struct {
			Type string `json:"type"`
		} `json:"thinking"`
		OutputConfig struct {
			Effort string `json:"effort"`
		} `json:"output_config"`
	}
	if err := json.Unmarshal(resp.RequestBody, &body); err != nil {
		t.Fatalf("recorded request body: %v", err)
	}

	if len(body.System) != 1 || len(body.System[0].CacheControl) == 0 {
		t.Fatalf("no cache breakpoint on the system prefix: %+v", body.System)
	}
	if len(body.Tools) != 1 || !body.Tools[0].Strict {
		t.Errorf("the tool is not strict: %+v", body.Tools)
	}
	if body.Tools[0].Schema.AdditionalProperties == nil || *body.Tools[0].Schema.AdditionalProperties {
		t.Errorf("the tool schema does not close itself with additionalProperties: false: %+v", body.Tools[0])
	}

	// Sampling parameters return 400 on this model family and forced tool use returns 400; both
	// must be absent from the wire, not merely unused in the code.
	if body.Temperature != nil || body.TopP != nil || body.TopK != nil {
		t.Error("a sampling parameter was sent to a model family that rejects them")
	}
	if len(body.ToolChoice) != 0 {
		t.Errorf("tool_choice was sent: %s; forced tool use returns 400 on this model", body.ToolChoice)
	}
	if body.Thinking.Type != "adaptive" {
		t.Errorf("thinking type = %q, want adaptive", body.Thinking.Type)
	}
	if body.OutputConfig.Effort != "high" {
		t.Errorf("effort = %q, want the configured high", body.OutputConfig.Effort)
	}
	if len(resp.Betas) == 0 || resp.Betas[0] != "mid-conversation-system-clear-at-2026-08-21" {
		t.Errorf("betas = %v, want the configured set recorded off the wire", resp.Betas)
	}
}

// TestATurnScopedSystemMessageCarriesClearAt: the ledger is re-rendered by appending a turn-scoped
// system message, never by editing an earlier turn (research §3, plan §Design Decisions 4).
func TestATurnScopedSystemMessageCarriesClearAt(t *testing.T) {
	t.Parallel()

	transport, err := modeltest.Transport(investigatorModel, modeltest.Turn{Text: "ok"})
	if err != nil {
		t.Fatalf("canned transport: %v", err)
	}
	client := clientWith(t, transport)

	req := request()
	req.Messages = append(req.Messages, model.Message{
		Role:    model.RoleSystem,
		ClearAt: model.ClearAtNextUserMessage,
		Blocks:  []model.Block{{Kind: model.BlockText, Text: "LEDGER …"}},
	})
	resp, err := client.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}

	var body struct {
		Messages []struct {
			Role    string `json:"role"`
			ClearAt string `json:"clear_at"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(resp.RequestBody, &body); err != nil {
		t.Fatalf("recorded request body: %v", err)
	}
	last := body.Messages[len(body.Messages)-1]
	if last.Role != "system" || last.ClearAt != "next_user_message" {
		t.Fatalf("the ledger message is %+v, want a turn-scoped system message", last)
	}
}

// TestAUserMessageMayNotBeTurnScoped: only a system message is, and a caller that tried is told so
// rather than having the field quietly dropped.
func TestAUserMessageMayNotBeTurnScoped(t *testing.T) {
	t.Parallel()

	transport, err := modeltest.Transport(investigatorModel, modeltest.Turn{Text: "ok"})
	if err != nil {
		t.Fatalf("canned transport: %v", err)
	}
	client := clientWith(t, transport)

	req := request()
	req.Messages[0].ClearAt = model.ClearAtNextUserMessage
	if _, err := client.Complete(context.Background(), req); err == nil {
		t.Fatal("a turn-scoped user message was accepted")
	}
}

// TestARefusalIsSurfacedAndNeverRetried (FR-045b).
func TestARefusalIsSurfacedAndNeverRetried(t *testing.T) {
	t.Parallel()

	transport, err := modeltest.Transport(investigatorModel, modeltest.Turn{
		StopReason:         "refusal",
		RefusalCategory:    "cyber",
		RefusalExplanation: "the request looks like an intrusion attempt",
		Usage:              model.Usage{Input: 900},
	})
	if err != nil {
		t.Fatalf("canned transport: %v", err)
	}
	client := clientWith(t, transport)

	resp, err := client.Complete(context.Background(), request())
	if err != nil {
		t.Fatalf("a refusal was returned as an error rather than a recorded terminal condition: %v", err)
	}
	if !resp.Refused() {
		t.Fatalf("stop reason = %q, want refusal", resp.StopReason)
	}
	if resp.RefusalCategory != "cyber" || resp.RefusalExplanation == "" {
		t.Errorf("the refusal's category and explanation were not carried: %+v", resp)
	}
	if len(transport.Exchanges()) != 1 {
		t.Errorf("exchanges = %d; a refusal is never silently retried", len(transport.Exchanges()))
	}
}

// TestCostIsDerivedFromTheVersionedPriceTable (FR-048).
func TestCostIsDerivedFromTheVersionedPriceTable(t *testing.T) {
	t.Parallel()

	transport, err := modeltest.Transport(investigatorModel, modeltest.Turn{Text: "ok"})
	if err != nil {
		t.Fatalf("canned transport: %v", err)
	}
	client := clientWith(t, transport)

	// 1M input at $10/MTok plus 1M output at $50/MTok on the investigator's model.
	cost, err := client.Cost(investigatorModel, model.Usage{Input: 1_000_000, Output: 1_000_000})
	if err != nil {
		t.Fatalf("cost: %v", err)
	}
	if cost != 60 {
		t.Errorf("cost = %v, want 60.0 from the published rates", cost)
	}
	if _, err := client.Cost("claude-invented-9", model.Usage{Input: 10}); err == nil {
		t.Error("an unpriced model costed silently; an unpriced class is the one answer that is never true")
	}

	ledger := model.NewTokenLedger()
	ledger.Record(investigatorModel, model.Usage{Input: 1_000_000})
	ledger.Record(verifierModel, model.Usage{Input: 1_000_000})
	total, err := ledger.Cost(client)
	if err != nil {
		t.Fatalf("ledger cost: %v", err)
	}
	if total != 15 {
		t.Errorf("total = %v, want 15.0 ($10 + $5)", total)
	}
	flat := ledger.Flat()
	if flat[investigatorModel+":input"] != 1_000_000 {
		t.Errorf("the flat form is not keyed by model and class: %v", flat)
	}
	if client.PriceTableVersion() == "" {
		t.Error("the price table version is not exposed; a cost with no table behind it is uninterpretable")
	}
}

// TestTheVerifierRunsOnADifferentModel: the published configuration puts the verifier on
// claude-opus-5 and the investigator on claude-fable-5-1 (FR-022a).
func TestTheVerifierRunsOnADifferentModel(t *testing.T) {
	t.Parallel()

	transport, err := modeltest.Transport(investigatorModel, modeltest.Turn{Text: "ok"})
	if err != nil {
		t.Fatalf("canned transport: %v", err)
	}
	client := clientWith(t, transport)

	investigator, err := client.ModelFor(model.RoleInvestigator)
	if err != nil {
		t.Fatalf("investigator: %v", err)
	}
	verifier, err := client.ModelFor(model.RoleVerifier)
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	if investigator == verifier {
		t.Fatalf("both roles run %s; the verifier's blind spots must not be the investigator's", verifier)
	}
	if verifier != verifierModel {
		t.Errorf("verifier = %s, want %s", verifier, verifierModel)
	}
}

// TestAStructuredOutputSchemaTravelsInOutputConfig: `output_config.format`, not the deprecated
// top-level parameter.
func TestAStructuredOutputSchemaTravelsInOutputConfig(t *testing.T) {
	t.Parallel()

	transport, err := modeltest.Transport(verifierModel, modeltest.Turn{Text: `{"findings":[]}`})
	if err != nil {
		t.Fatalf("canned transport: %v", err)
	}
	client := clientWith(t, transport)

	req := request()
	req.Role = model.RoleVerifier
	req.Format = &model.OutputFormat{Name: "verdicts", Schema: map[string]any{
		"type": "object", "additionalProperties": false,
		"properties": map[string]any{"findings": map[string]any{"type": "array"}},
		"required":   []string{"findings"},
	}}
	resp, err := client.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if !strings.Contains(string(resp.RequestBody), `"format"`) {
		t.Errorf("the schema did not travel in output_config.format: %s", resp.RequestBody)
	}
	if strings.Contains(string(resp.RequestBody), `"output_format"`) {
		t.Error("the deprecated output_format parameter was sent")
	}
}
