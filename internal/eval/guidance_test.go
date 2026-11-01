// SPDX-License-Identifier: Apache-2.0

package eval_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/eval"
	"github.com/Pierre-Theophile/aisre/internal/investigation/model/modeltest"
)

// 002 T133, FR-013b: runbooks and postmortem lessons reach the investigator as optional guidance
// with their provenance, and nothing in them is a step or a gate. The investigator here reads the
// brief and calls no tool at all; the run still concludes, on the first wave's evidence, and the
// guidance it was offered asked nothing of it.
func TestGuidanceIsOfferedAndRequiresNoToolCall(t *testing.T) {
	silent := modeltest.Turn{Text: "The first wave settles it; I have nothing to add."}
	client := scriptedClient(t, silent, silent, silent, silent, silent, silent)
	harness, err := eval.NewHarness(context.Background(), eval.Options{
		Dir:        notionIncident,
		NewStore:   storeFactory(t),
		AuditPath:  auditPath,
		ModelYAML:  modelYAML,
		PricesYAML: pricesYAML,
		Client:     client,
	})
	if err != nil {
		t.Fatalf("NewHarness: %v", err)
	}
	t.Cleanup(harness.Close)

	outcome, err := harness.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if outcome.StopReason != "completed" {
		t.Fatalf("stop %s (%s): a run that read the guidance and called nothing must still conclude",
			outcome.StopReason, outcome.StopDetail)
	}

	exchanges := client.Transport().Exchanges()
	if len(exchanges) == 0 {
		t.Fatal("no model call was made")
	}
	brief := firstUserText(t, exchanges[0].RequestBody)
	_, guidance, ok := strings.Cut(brief, "# Guidance (optional)")
	if !ok {
		t.Fatalf("the brief offers no guidance section:\n%s", brief)
	}
	if !strings.Contains(guidance, "postmortem "+citedPostmortem) {
		t.Errorf("the guidance does not list the fixture's postmortem %s with its kind:\n%s", citedPostmortem, guidance)
	}
	for _, provenance := range []string{"authored ", "link provenance ", "(evidence "} {
		if !strings.Contains(guidance, provenance) {
			t.Errorf("the guidance carries no %q:\n%s", provenance, guidance)
		}
	}
	if !strings.Contains(guidance, "no call they suggest is required") {
		t.Errorf("the guidance does not say it requires nothing:\n%s", guidance)
	}
	// The barrier: the brief is a user message, so no document text is in it.
	if strings.Contains(brief, plantedInstruction) {
		t.Error("document text reached the brief; it may reach the model only as a tool result (FR-017)")
	}

	// No call was made because of the guidance, and none was demanded: every later request is
	// the same conversation with the reminders the loop sends to any silent investigator.
	for i, exchange := range exchanges {
		if strings.Contains(string(exchange.RequestBody), "knowledge_search\",\"arguments") {
			t.Errorf("request %d carries a knowledge_search call nobody made", i)
		}
	}
}

// firstUserText returns the first user message's text from a recorded request, in either
// provider's body shape.
func firstUserText(t *testing.T, body []byte) string {
	t.Helper()
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("request body: %v", err)
	}
	for _, m := range req.Messages {
		if m.Role != "user" {
			continue
		}
		var text string
		if json.Unmarshal(m.Content, &text) == nil {
			return text
		}
		var blocks []struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(m.Content, &blocks) == nil {
			var b strings.Builder
			for _, block := range blocks {
				b.WriteString(block.Text)
			}
			return b.String()
		}
	}
	t.Fatal("no user message in the request")
	return ""
}
