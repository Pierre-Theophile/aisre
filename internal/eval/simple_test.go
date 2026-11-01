// SPDX-License-Identifier: Apache-2.0

package eval_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/eval"
	"github.com/Pierre-Theophile/aisre/internal/investigation/model"
	"github.com/Pierre-Theophile/aisre/internal/investigation/model/modeltest"
)

// The simple harness (002 T131, FR-059b, ADR-0019 D6): a general agent over feature 010's MCP
// server in its baseline profile, scored by the verdict-only rule. The model is scripted here
// through the same transport seam a live provider uses; the MCP server, the API and the fixture's
// replayed graph are the real ones.

// scriptedClient is a model boundary that answers with the given turns, in the configured
// investigator model's own body shape.
func scriptedClient(t *testing.T, turns ...modeltest.Turn) *model.Client {
	t.Helper()
	config, prices, err := model.LoadPair(modelYAML, pricesYAML)
	if err != nil {
		t.Fatalf("model config: %v", err)
	}
	rc, ok := config.Role(model.RoleInvestigator)
	if !ok {
		t.Fatal("the model configuration has no investigator role")
	}
	transport, err := modeltest.Transport(rc.Model, turns...)
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	client, err := model.NewClientWithTransport(config, prices, transport)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	return client
}

const simpleAt = "2026-09-01T14:32:00Z"

func TestTheSimpleHarnessReadsTheGraphThroughTheBaselineProfileAndIsScored(t *testing.T) {
	harness := newHarness(t, eval.KindModelFree)
	client := scriptedClient(t,
		modeltest.Turn{ToolUses: []modeltest.ToolUse{{
			ID: "toolu_1", Name: "diff",
			Input: map[string]any{"focus": "otel.service.name=checkout", "at": simpleAt, "window": "90m"},
		}}},
		modeltest.Turn{ToolUses: []modeltest.ToolUse{{
			ID: "toolu_2", Name: eval.SimpleAnswerTool,
			Input: map[string]any{
				"verdict": "k8s.change=shop/payments@rev7",
				"reason":  "the top-ranked change in the checkout neighbourhood before the alert",
			},
		}}},
	)

	out, err := harness.RunSimple(context.Background(), eval.SimpleOptions{Client: client})
	if err != nil {
		t.Fatalf("RunSimple: %v", err)
	}
	if out.Stop != eval.SimpleStopAnswered || out.Turns != 2 {
		t.Fatalf("stop %q after %d turn(s) (%s), want answered after 2", out.Stop, out.Turns, out.StopDetail)
	}
	if out.ToolCalls["diff"] != 1 {
		t.Errorf("tool calls %v, want one diff", out.ToolCalls)
	}
	if out.Verdict != "k8s.change=shop/payments@rev7" {
		t.Errorf("verdict %q", out.Verdict)
	}
	// The change was seen in the diff answer, so every spelling the graph published for it is
	// carried, as the engine's verdict carries them.
	hasID := false
	for _, ref := range out.VerdictRefs {
		hasID = hasID || strings.HasPrefix(ref, "id:")
	}
	if !hasID {
		t.Errorf("verdict refs %v carry no id: spelling of the change the diff showed", out.VerdictRefs)
	}
	if len(out.ModelIDs) == 0 {
		t.Error("no model id recorded")
	}

	// The tools the model was offered are the baseline profile's: graph and catalog, no
	// investigation, plus the answer tool.
	exchanges := client.Transport().Exchanges()
	if len(exchanges) != 2 {
		t.Fatalf("%d model exchanges, want 2", len(exchanges))
	}
	offered := offeredTools(t, exchanges[0].RequestBody)
	for _, want := range []string{"diff", "subgraph", "catalog_list", eval.SimpleAnswerTool} {
		if !offered[want] {
			t.Errorf("the first request offers no %s tool (offered %v)", want, offered)
		}
	}
	for _, unwanted := range []string{"investigation_start", "investigation_get", "investigation_list"} {
		if offered[unwanted] {
			t.Errorf("the baseline profile offered %s; the simple harness must run with no engine", unwanted)
		}
	}
	// The tool result went back to the model as a tool result, with the diff's answer in it.
	if second := string(exchanges[1].RequestBody); !strings.Contains(second, "shop/payments@rev7") {
		t.Error("the second request does not carry the diff answer back to the model")
	}

	// Scored by the verdict-only rule: right after the decisive evidence existed, and the same
	// verdict asked before it is wrong, because `unknown` was the answer then.
	m := harness.Manifest()
	truth := m.Incident.GroundTruth
	if !eval.VerdictRight(out.Outcome(), truth, m.Incident.Question.FiredAt) {
		t.Error("the simple harness named the culprit and was scored wrong")
	}
	if eval.VerdictRight(out.Outcome(), truth, truth.KnowabilityTime.Add(-time.Minute)) {
		t.Error("a change named before knowability was scored right")
	}
}

func TestTheSimpleHarnessAnswersUnknownWhenItRunsOutOfTurns(t *testing.T) {
	harness := newHarness(t, eval.KindModelFree)
	look := modeltest.Turn{ToolUses: []modeltest.ToolUse{{
		ID: "toolu_x", Name: "extent", Input: map[string]any{},
	}}}
	client := scriptedClient(t, look, look, look)

	out, err := harness.RunSimple(context.Background(), eval.SimpleOptions{Client: client, MaxTurns: 3})
	if err != nil {
		t.Fatalf("RunSimple: %v", err)
	}
	if out.Stop != eval.SimpleStopTurnLimit || out.Verdict != eval.VerdictUnknown {
		t.Errorf("stop %q verdict %q, want turn_limit and unknown", out.Stop, out.Verdict)
	}
}

func TestTheSimpleHarnessNeedsAModel(t *testing.T) {
	harness := newHarness(t, eval.KindModelFree)
	if _, err := harness.RunSimple(context.Background(), eval.SimpleOptions{}); err == nil {
		t.Error("RunSimple ran with no model")
	}
}

// The report publishes the simple harness beside the floor, and `not run` without a model.
func TestTheReportPublishesTheSimpleHarnessOrSaysItWasNotRun(t *testing.T) {
	culprit := eval.GroundTruth{Culprit: "k8s.change=shop/payments@rev7"}
	engine := &eval.RunOutcome{
		FixtureID: "f", RunIndex: 1, Verdict: "k8s.change=shop/payments@rev7",
		FloorVerdict: "k8s.change=shop/other@2",
	}
	grade := eval.GradeResult{AskedAt: time.Date(2026, 9, 1, 14, 32, 0, 0, time.UTC)}
	set := &eval.RunSet{Outcomes: []*eval.RunOutcome{engine}}

	wrong := &eval.SimpleOutcome{FixtureID: "f", Verdict: eval.VerdictUnobserved}
	rows := eval.RunRowsFrom(set, []eval.GradeResult{grade}, culprit, eval.RowOptions{Simple: wrong})
	report := eval.Build(eval.Input{Corpus: "public", Runs: rows})
	if v := corpusValue(t, report, eval.MetricSimpleAccuracy); v == nil || *v != 0 {
		t.Errorf("simple accuracy %v, want 0", v)
	}
	if v := corpusValue(t, report, eval.MetricLiftOverSimple); v == nil || *v != 1 {
		t.Errorf("lift over the simple harness %v, want 1", v)
	}

	reason := "not run: the simple harness needs a model, and this run is model-free"
	rows = eval.RunRowsFrom(set, []eval.GradeResult{grade}, culprit, eval.RowOptions{SimpleNotRun: reason})
	report = eval.Build(eval.Input{Corpus: "public", Runs: rows})
	for _, metric := range []string{eval.MetricSimpleAccuracy, eval.MetricLiftOverSimple} {
		row := corpusRow(t, report, metric)
		if row.Measured() || row.Detail != reason {
			t.Errorf("%s: measured %v, detail %q; want not measured with the reason", metric, row.Measured(), row.Detail)
		}
	}
	if md := report.Markdown(); !strings.Contains(md, "| `"+eval.MetricSimpleAccuracy+"` | not run |") {
		t.Errorf("the summary does not say the simple harness was not run:\n%s", md)
	}
}

// offeredTools reads the tool names out of a recorded request body, in either provider's shape.
func offeredTools(t *testing.T, body []byte) map[string]bool {
	t.Helper()
	var req struct {
		Tools []struct {
			Name     string `json:"name"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("request body: %v", err)
	}
	out := map[string]bool{}
	for _, tool := range req.Tools {
		out[tool.Name+tool.Function.Name] = true
	}
	return out
}

func corpusRow(t *testing.T, report *eval.Report, metric string) eval.Row {
	t.Helper()
	for _, row := range report.Rows {
		if row.Metric == metric && row.Scope == eval.ScopeCorpus {
			return row
		}
	}
	t.Fatalf("no corpus row %s", metric)
	return eval.Row{}
}

func corpusValue(t *testing.T, report *eval.Report, metric string) *float64 {
	t.Helper()
	return corpusRow(t, report, metric).Value
}
