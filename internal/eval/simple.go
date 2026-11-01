// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Pierre-Theophile/aisre/internal/investigation/model"
	"github.com/Pierre-Theophile/aisre/internal/mcpserver"
	"github.com/Pierre-Theophile/aisre/internal/server"
	"github.com/Pierre-Theophile/aisre/internal/server/pgserver"
)

// The simple harness (002 T131, FR-059b, ADR-0019 D6).
//
// It is the baseline that can beat the engine: a general agent, one model with tools, that reads
// the fixture's replayed graph through feature 010's read-only MCP server in its `baseline`
// profile (graph and catalog tools only) and answers the fixture's question. It has no engine, no
// first wave, no ledger and no prescribed step. Its answer is scored by `VerdictRight`, the same
// verdict-only rule the engine and the telemetry-free floor are held to, so the three numbers
// compare.
//
// Everything it reads comes through the same path a coding agent's does: the MCP server is the
// production one (`mcpserver.New`), and its tools call the production API (`server.New`) over the
// fixture's own database. The API is served in process, through a RoundTripper that hands each
// request straight to the handler, so nothing listens and nothing reaches a network; the only
// outbound call is the model's.
//
// It needs a model. Without one it is not run, and the report says so (`not run`), never 0.

// The simple harness's own vocabulary.
const (
	// SimpleAnswerTool is the tool the agent answers with. It is the only way to finish.
	SimpleAnswerTool = "answer"
	// DefaultSimpleMaxTurns bounds the conversation. A run that reaches it answers `unknown`.
	DefaultSimpleMaxTurns = 16
	// DefaultSimpleMaxResult bounds one tool result as the model sees it, in bytes.
	DefaultSimpleMaxResult = 24000

	// The stop reasons a simple run records.
	SimpleStopAnswered   = "answered"
	SimpleStopTurnLimit  = "turn_limit"
	SimpleStopEndTurn    = "ended_without_answer"
	SimpleStopRefusal    = "refusal"
	SimpleStopModelError = "model_error"

	// simpleSubject is the caller the harness's token names in the API's logs.
	simpleSubject = "simple-harness"
)

// SimpleOptions configure one simple-harness run.
type SimpleOptions struct {
	// Client is the model boundary (ADR-0017): any configured provider, called in the
	// investigator role. Nil means the harness is not run.
	Client *model.Client
	// MaxTurns bounds the conversation; zero means DefaultSimpleMaxTurns.
	MaxTurns int
	// MaxResult bounds one tool result in bytes; zero means DefaultSimpleMaxResult.
	MaxResult int
}

// SimpleOutcome is one simple-harness run, as the report reads it.
type SimpleOutcome struct {
	FixtureID string `json:"fixture_id"`
	// Verdict is the answer in the ground truth's vocabulary, and VerdictRefs every spelling the
	// graph published for the change it names, so a hand-written ground truth matches whichever
	// one it used (the rule the engine's verdict follows).
	Verdict     string   `json:"verdict"`
	VerdictRefs []string `json:"verdict_refs,omitempty"`
	// Reason is the agent's own one-paragraph justification, kept for a reader; it is not scored.
	Reason string `json:"reason,omitempty"`
	// Stop says how the conversation ended.
	Stop       string `json:"stop"`
	StopDetail string `json:"stop_detail,omitempty"`
	// Turns is the model calls made, and ToolCalls the MCP tool calls, by tool.
	Turns     int            `json:"turns"`
	ToolCalls map[string]int `json:"tool_calls,omitempty"`
	// ModelIDs are the models that answered, sorted; Usage the summed token accounting.
	ModelIDs []string    `json:"model_ids,omitempty"`
	Usage    model.Usage `json:"usage"`
}

// Outcome renders the answer as a RunOutcome, which is what VerdictRight scores.
func (s *SimpleOutcome) Outcome() *RunOutcome {
	if s == nil {
		return nil
	}
	return &RunOutcome{FixtureID: s.FixtureID, Verdict: s.Verdict, VerdictRefs: s.VerdictRefs}
}

// ErrSimpleNoGraph is returned when the harness was built over a caller's query service rather
// than its own database: there is no API to serve the MCP server from.
var ErrSimpleNoGraph = errors.New(
	"eval: the simple harness needs the fixture replayed into a database of its own; this harness was given a query service")

// RunSimple runs the simple harness once on the harness's fixture.
func (h *Harness) RunSimple(ctx context.Context, opts SimpleOptions) (*SimpleOutcome, error) {
	if opts.Client == nil {
		return nil, errors.New("eval: the simple harness needs a model; without one it is not run")
	}
	if h.projector == nil {
		return nil, ErrSimpleNoGraph
	}
	if opts.MaxTurns <= 0 {
		opts.MaxTurns = DefaultSimpleMaxTurns
	}
	if opts.MaxResult <= 0 {
		opts.MaxResult = DefaultSimpleMaxResult
	}

	session, closeAll, err := h.simpleSession(ctx)
	if err != nil {
		return nil, err
	}
	defer closeAll()

	agent := &simpleAgent{
		opts:    opts,
		session: session,
		out: &SimpleOutcome{
			FixtureID: h.manifest.ID,
			Verdict:   VerdictUnknown,
			ToolCalls: map[string]int{},
		},
		spellings: map[string][]string{},
	}
	if err := agent.run(ctx, h.simpleQuestion()); err != nil {
		return nil, err
	}
	return agent.out, nil
}

// simpleSession serves the fixture's database through the production API and the production MCP
// server in its baseline profile, and connects an MCP client to it.
func (h *Harness) simpleSession(ctx context.Context) (*mcp.ClientSession, func(), error) {
	logger := slog.New(slog.DiscardHandler)
	auth, err := server.NewDevAuthenticator(server.DevConfig{Enabled: true, Logger: logger})
	if err != nil {
		return nil, nil, err
	}
	api, err := server.New(server.Config{
		Listen: "127.0.0.1:0", Auth: auth, Backends: pgserver.Backends(h.projector), Logger: logger,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("eval: simple harness API: %w", err)
	}
	token, err := auth.Issuer().Mint(simpleSubject, []server.Role{server.RoleReader})
	if err != nil {
		_ = api.Close()
		return nil, nil, err
	}

	clients := mcpserver.Connect("http://aisre.in-process", inProcess{api.Handler()},
		func() (string, error) { return token, nil })
	caller, err := clients.WhoAmI(ctx)
	if err != nil {
		_ = api.Close()
		return nil, nil, fmt.Errorf("eval: simple harness WhoAmI: %w", err)
	}
	srv, err := mcpserver.New(mcpserver.Options{
		Clients: clients.Clients, Roles: caller.Roles, Subject: caller.Subject,
		Profile: mcpserver.ProfileBaseline, Version: "eval",
	})
	if err != nil {
		_ = api.Close()
		return nil, nil, err
	}

	serverT, clientT := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, serverT, nil)
	if err != nil {
		_ = api.Close()
		return nil, nil, err
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "aisre-simple-harness", Version: "eval"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		_ = ss.Close()
		_ = api.Close()
		return nil, nil, err
	}
	return cs, func() {
		_ = cs.Close()
		_ = ss.Close()
		_ = api.Close()
	}, nil
}

// inProcess hands a request to the API's handler without a socket. Unary Connect calls are all
// the baseline profile makes, so a recorded response is the whole response.
type inProcess struct{ handler http.Handler }

func (t inProcess) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	t.handler.ServeHTTP(rec, req)
	res := rec.Result()
	res.Request = req
	return res, nil
}

// simpleQuestion is the fixture's question, as an on-call would hand it to a general agent.
func (h *Harness) simpleQuestion() string {
	q := h.manifest.Incident.Question
	var b strings.Builder
	b.WriteString("An incident question arrived.\n\n")
	if q.Transport != "" {
		fmt.Fprintf(&b, "- How it arrived: %s", q.Transport)
		if q.OriginSystem != "" {
			fmt.Fprintf(&b, " (%s)", q.OriginSystem)
		}
		b.WriteString("\n")
	}
	if q.Title != "" {
		fmt.Fprintf(&b, "- Title: %s\n", q.Title)
	}
	if q.Statement != "" {
		fmt.Fprintf(&b, "- Statement: %s\n", q.Statement)
	}
	switch {
	case q.Subject != "":
		fmt.Fprintf(&b, "- Subject: %s\n", q.Subject)
	case q.UnparseableSubject != "":
		fmt.Fprintf(&b, "- Subject, as written by the reporter: %s\n", q.UnparseableSubject)
	}
	if !q.FiredAt.IsZero() {
		fmt.Fprintf(&b, "- Fired at (this is now): %s\n", q.FiredAt.UTC().Format(time.RFC3339))
	}
	if q.Lookback != "" {
		fmt.Fprintf(&b, "- Look back: %s\n", q.Lookback)
	}
	b.WriteString("\nFind what caused it, then call `answer`.")
	return b.String()
}

// simpleSystem is the agent's whole instruction. It describes the task and the answer's
// vocabulary, and prescribes no step: which tools to call, and in what order, is the agent's.
const simpleSystem = "You are an on-call SRE investigating an incident with read-only access to " +
	"aisre, a bitemporal graph of the production system, through the tools you are given. Ask the " +
	"graph only about instants at or before the moment the question fired: that is now, and nothing " +
	"later exists yet.\n\n" +
	"Finish by calling `answer` exactly once, with one verdict:\n" +
	"- the change that caused the incident, written as one of its references from the graph " +
	"(`<namespace>=<value>`, for example k8s.change=shop/payments@rev7, or `id:<entity_id>`);\n" +
	"- `unobserved` when no change the graph records explains the symptom;\n" +
	"- `unknown` when what you can see does not yet support either answer.\n" +
	"Naming a change you cannot support is worse than `unknown`."

// simpleAgent is one conversation.
type simpleAgent struct {
	opts    SimpleOptions
	session *mcp.ClientSession
	tools   []model.Tool
	out     *SimpleOutcome
	models  map[string]bool
	// spellings maps every reference the graph published for a change seen in a tool result to
	// all of that change's spellings.
	spellings map[string][]string
}

func (a *simpleAgent) run(ctx context.Context, question string) error {
	system, err := a.prepare(ctx)
	if err != nil {
		return err
	}
	a.models = map[string]bool{}
	messages := []model.Message{{
		Role: model.RoleUser, Blocks: []model.Block{{Kind: model.BlockText, Text: question}},
	}}
	defer a.finish()

	for a.out.Turns < a.opts.MaxTurns {
		a.out.Turns++
		resp, err := a.opts.Client.Complete(ctx, model.Request{
			Role: model.RoleInvestigator, Tools: a.tools, System: system,
			Messages: messages, MaxTokens: model.DefaultMaxTokens,
		})
		if err != nil {
			a.out.Stop, a.out.StopDetail = SimpleStopModelError, err.Error()
			return nil
		}
		a.addUsage(resp)
		if resp.Refused() {
			a.out.Stop, a.out.StopDetail = SimpleStopRefusal, resp.RefusalExplanation
			return nil
		}
		uses := resp.ToolUses()
		if len(uses) == 0 {
			a.out.Stop = SimpleStopEndTurn
			return nil
		}
		messages = append(messages, resp.AssistantMessage())
		results := make([]model.Block, 0, len(uses))
		for _, use := range uses {
			if use.ToolName == SimpleAnswerTool {
				a.answer(use.ToolInput)
				return nil
			}
			results = append(results, a.call(ctx, use))
		}
		messages = append(messages, model.Message{Role: model.RoleUser, Blocks: results})
	}
	a.out.Stop = SimpleStopTurnLimit
	return nil
}

// prepare lists the MCP server's tools and reads its guide, as a general agent connecting to it
// would, and returns the system prompt.
func (a *simpleAgent) prepare(ctx context.Context) (string, error) {
	listed, err := a.session.ListTools(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("eval: simple harness: list tools: %w", err)
	}
	for _, tool := range listed.Tools {
		mt, err := modelTool(tool)
		if err != nil {
			return "", err
		}
		a.tools = append(a.tools, mt)
	}
	a.tools = append(a.tools, model.Tool{
		Name:        SimpleAnswerTool,
		Description: "Give your verdict and finish. Call it once.",
		Properties: map[string]any{
			"verdict": map[string]any{
				"type":        "string",
				"description": "a change reference (`<namespace>=<value>` or `id:<entity_id>`), `unobserved` or `unknown`",
			},
			"reason": map[string]any{
				"type":        "string",
				"description": "what in the graph supports the verdict, in a few sentences",
			},
		},
		Required: []string{"verdict", "reason"},
	})

	system := simpleSystem
	if init := a.session.InitializeResult(); init != nil && init.Instructions != "" {
		system += "\n\nThe aisre server's own instructions:\n" + init.Instructions
	}
	if guide, err := a.session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "aisre://guide"}); err == nil {
		for _, c := range guide.Contents {
			if c.Text != "" {
				system += "\n\n" + c.Text
			}
		}
	}
	return system, nil
}

// modelTool renders an MCP tool as the model boundary's tool.
func modelTool(tool *mcp.Tool) (model.Tool, error) {
	raw, err := json.Marshal(tool.InputSchema)
	if err != nil {
		return model.Tool{}, fmt.Errorf("eval: simple harness: tool %s schema: %w", tool.Name, err)
	}
	var schema struct {
		Properties map[string]any `json:"properties"`
		Required   []string       `json:"required"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		return model.Tool{}, fmt.Errorf("eval: simple harness: tool %s schema: %w", tool.Name, err)
	}
	description := tool.Description
	if description == "" && tool.Annotations != nil {
		description = tool.Annotations.Title
	}
	return model.Tool{
		Name: tool.Name, Description: description, Properties: schema.Properties, Required: schema.Required,
	}, nil
}

// call answers one tool_use through the MCP session.
func (a *simpleAgent) call(ctx context.Context, use model.Block) model.Block {
	a.out.ToolCalls[use.ToolName]++
	result := model.Block{Kind: model.BlockToolResult, ToolUseID: use.ToolUseID}
	var args map[string]any
	if len(use.ToolInput) > 0 {
		if err := json.Unmarshal(use.ToolInput, &args); err != nil {
			result.IsError, result.Text = true, "the tool input is not a JSON object: "+err.Error()
			return result
		}
	}
	res, err := a.session.CallTool(ctx, &mcp.CallToolParams{Name: use.ToolName, Arguments: args})
	if err != nil {
		result.IsError, result.Text = true, err.Error()
		return result
	}
	result.IsError = res.IsError
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
			b.WriteString("\n")
		}
	}
	if res.StructuredContent != nil {
		if raw, err := json.Marshal(res.StructuredContent); err == nil {
			a.learn(raw)
			b.Write(raw)
		}
	}
	result.Text = clip(b.String(), a.opts.MaxResult)
	return result
}

// clip bounds a tool result and says that it did.
func clip(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit] + fmt.Sprintf(
		"\n[truncated: %d of %d bytes shown; narrow the question to see the rest]", limit, len(text))
}

// learn records every change the result shows, with all its spellings.
func (a *simpleAgent) learn(raw []byte) {
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return
	}
	walkChanges(doc, func(spellings []string) {
		for _, s := range spellings {
			merged := append(append([]string(nil), a.spellings[s]...), spellings...)
			sort.Strings(merged)
			a.spellings[s] = dedupe(merged)
		}
	})
}

// walkChanges calls fn with the spellings of every change entity in a canonical answer: an object
// whose type is CHANGE, named by `id:<entityId>`, its bare entity id and each alias.
func walkChanges(node any, fn func([]string)) {
	switch v := node.(type) {
	case map[string]any:
		if t, _ := v["type"].(string); t == "CHANGE" {
			if id, _ := v["entityId"].(string); id != "" {
				spellings := []string{id, "id:" + id}
				if aliases, ok := v["aliases"].([]any); ok {
					for _, alias := range aliases {
						m, _ := alias.(map[string]any)
						ns, _ := m["namespace"].(string)
						val, _ := m["value"].(string)
						if ns != "" && val != "" {
							spellings = append(spellings, ns+"="+val)
						}
					}
				}
				fn(spellings)
			}
		}
		for _, child := range v {
			walkChanges(child, fn)
		}
	case []any:
		for _, child := range v {
			walkChanges(child, fn)
		}
	}
}

// answer records the agent's verdict in the ground truth's vocabulary.
func (a *simpleAgent) answer(input json.RawMessage) {
	a.out.Stop = SimpleStopAnswered
	var in struct {
		Verdict string `json:"verdict"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		a.out.StopDetail = "the answer was not the declared shape: " + err.Error()
		return
	}
	a.out.Reason = in.Reason
	verdict := strings.TrimSpace(in.Verdict)
	switch lower := strings.ToLower(verdict); {
	case verdict == "", lower == VerdictUnknown:
		a.out.Verdict = VerdictUnknown
	case lower == VerdictUnobserved:
		a.out.Verdict = VerdictUnobserved
	case strings.HasPrefix(lower, VerdictNotChangeInducedPrefix):
		a.out.Verdict = verdict
	default:
		a.out.Verdict = verdict
		refs := append([]string{verdict}, a.spellings[verdict]...)
		sort.Strings(refs)
		a.out.VerdictRefs = dedupe(refs)
	}
}

func (a *simpleAgent) addUsage(resp *model.Response) {
	a.out.Usage.Input += resp.Usage.Input
	a.out.Usage.CacheWrite += resp.Usage.CacheWrite
	a.out.Usage.CacheRead += resp.Usage.CacheRead
	a.out.Usage.Output += resp.Usage.Output
	if resp.Model != "" {
		a.models[resp.Model] = true
	}
}

func (a *simpleAgent) finish() {
	a.out.ModelIDs = sortedKeys(a.models)
	if len(a.out.ToolCalls) == 0 {
		a.out.ToolCalls = nil
	}
}
