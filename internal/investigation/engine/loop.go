// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/investigation/budget"
	"github.com/Pierre-Theophile/aisre/internal/investigation/engine/prompt"
	"github.com/Pierre-Theophile/aisre/internal/investigation/ledger"
	"github.com/Pierre-Theophile/aisre/internal/investigation/model"
)

// The hand-rolled agent loop (T063, FR-009, FR-013, FR-013a, FR-045b, research §3).
//
// The loop is the feature. Three requirements are requirements *on the loop itself*, which is why
// neither the Claude Agent SDK nor the SDK's tool runner can own it (plan §Design Decisions 2):
// byte-exact recording of the model boundary, a re-issue path that calls nothing, and a budget
// admission check made before a request is composed.
//
// Its shape, in the order the code below takes it:
//
//	publish the provisional ranking      (no model; within 5 s)
//	estimate onset                       (the metrics worker; no model)
//	order candidates causally             (the graph, referenced to onset; no model)
//	run the first wave                   (five queries per top candidate, parallel; no model)
//	then, per turn, until a typed stop:
//	  append the ledger as a turn-scoped system message   clear_at: next_user_message
//	  count_tokens, then admission                        before the request is composed
//	  one request, recorded exactly                       no forced tool use
//	  execute the tool calls, append tool_results         the only channel from a worker
//	  apply the proposed judgments through the ledger     the engine is the writer of record
//	  evaluate diminishing returns over the posterior
//
// Two things the loop never does. It never edits an earlier turn — on this model family that
// invalidates the thinking blocks after it, and the ledger is therefore re-rendered by appending.
// And it never retries a refusal: a refusal is a terminal condition recorded like any other
// (FR-045b).

// MaxSilentTurns is how many turns that propose nothing the loop answers with a turn-scoped
// reminder before concluding.
//
// Two. The first may be the model orienting itself; the second is a pattern, and a third reminder
// would be the engine arguing with a model that has nothing to say — at the operator's expense.
const MaxSilentTurns = 2

// Run carries the investigation from intake to a typed stop.
func (e *Engine) Run(ctx context.Context) (Stop, error) {
	if _, err := e.Publish(ctx); err != nil {
		return e.fail(err)
	}
	e.publishState(PhaseProvisional, 0)
	if _, err := e.EstimateOnset(ctx); err != nil {
		return e.fail(err)
	}
	if _, err := e.OrderCausally(ctx); err != nil {
		return e.fail(err)
	}
	wave, err := e.RunFirstWave(ctx)
	if err != nil {
		return e.fail(err)
	}
	e.publishState(PhaseFirstWave, 0)

	if e.client == nil {
		// The model-free run: the provisional ranking, the causal ordering and the first wave.
		// It is a complete investigation with a smaller claim, and it is the MVP.
		stop := Stop{
			Reason: StopCompleted,
			Detail: "no model is configured; the investigation is the prior-only ranking, the causal " +
				"ordering and the deterministic first wave",
		}
		return stop, e.Stop(stop)
	}

	e.appendMessage(model.Message{
		Role:   model.RoleUser,
		Blocks: []model.Block{{Kind: model.BlockText, Text: e.brief(wave)}},
	})

	var silent int
	for turn := 1; turn <= e.maxTurns; turn++ {
		if stop, done := e.budgetStop(); done {
			return stop, e.Stop(stop)
		}

		resp, stop, err := e.turn(ctx, turn)
		if err != nil {
			return e.fail(err)
		}
		if stop != nil {
			return *stop, e.Stop(*stop)
		}

		uses := resp.ToolUses()
		if len(uses) == 0 {
			if e.hasModelJudgment() || silent >= MaxSilentTurns {
				concluded := Stop{
					Reason: StopCompleted,
					Detail: "the investigator concluded",
				}
				if !e.hasModelJudgment() {
					concluded.Detail = fmt.Sprintf(
						"the investigator proposed nothing across %d turns and was reminded %d times; "+
							"the answer is what the deterministic first wave established", turn, silent)
				}
				return concluded, e.Stop(concluded)
			}
			silent++
			e.remind(silent)
			e.publishState(PhaseTurn, turn)
			continue
		}
		silent = 0

		results, stop, err := e.execute(ctx, uses)
		if err != nil {
			return e.fail(err)
		}
		if len(results) > 0 {
			e.appendMessage(model.Message{Role: model.RoleUser, Blocks: results})
		}
		if stop != nil {
			return *stop, e.Stop(*stop)
		}
		// One snapshot per turn, taken once the turn's tool results are in, so what a subscriber
		// sees for turn n is the ledger as that turn left it rather than as it found it.
		e.publishState(PhaseTurn, turn)
	}

	stop := Stop{
		Reason: StopCompleted,
		Detail: fmt.Sprintf("the loop reached its published ceiling of %d turns", e.maxTurns),
	}
	return stop, e.Stop(stop)
}

// turn issues one model request: the ledger render, the admission check, the call, the recording.
func (e *Engine) turn(ctx context.Context, number int) (*model.Response, *Stop, error) {
	e.appendMessage(e.ledgerMessage())

	request := model.Request{
		Role:     model.RoleInvestigator,
		Tools:    ToolSet(),
		System:   prompt.Prefix(),
		Messages: e.History(),
	}

	// Admission before the request is issued, on the pre-flight token count plus the output it is
	// allowed to generate (FR-047a).
	tokens, err := e.client.CountTokens(ctx, request)
	if err != nil {
		return nil, nil, err
	}
	decision := e.budget.Admit(budget.Request{
		Kind:     budget.KindModel,
		ModelID:  e.modelID(),
		Tokens:   tokens + model.DefaultMaxTokens,
		Question: fmt.Sprintf("turn %d of the investigation", number),
	})
	if !decision.Admitted {
		return nil, &Stop{Reason: StopBudgetExhausted, Detail: decision.Budget + " — " + decision.Reason}, nil
	}
	// Admission booked the pre-flight estimate, so a turn in flight counts against the next one's
	// ceiling. The booking is released once the turn has recorded what it actually spent; a turn
	// that never reaches the model releases it on the way out.
	defer decision.Reservation.Release()

	resp, err := e.client.Complete(ctx, request)
	if err != nil {
		return nil, nil, err
	}

	if err := e.trajectory.ModelRequest(model.RoleInvestigator, resp.Model, resp.RequestBody, resp.RequestDigest); err != nil {
		return nil, nil, err
	}
	if err := e.trajectory.ModelResponse(resp); err != nil {
		return nil, nil, err
	}
	e.budget.RecordModelCall(resp.Model, resp.Usage)

	if resp.Refused() {
		// Handled and recorded like any other terminal condition, never silently retried.
		stop := RefusalStop(resp.RefusalCategory, resp.RefusalExplanation)
		return resp, &stop, nil
	}

	// The cache conformance check: a zero cache read from turn two means a silent invalidator has
	// crept into the stable prefix (FR-061, quickstart §11). It is a recorded observation rather
	// than a failure — the investigation is still valid, it is merely costing more than it should.
	if number >= 2 && resp.Usage.CacheRead == 0 && resp.Usage.Input > 0 {
		e.cacheMiss = append(e.cacheMiss, number)
	}

	e.appendMessage(resp.AssistantMessage())
	return resp, nil, nil
}

// execute runs the tool calls of one turn and returns the tool_result blocks that answer them.
//
// Every result is returned in a *single* user message by the caller, because splitting them across
// messages trains the model out of calling tools in parallel. A failed call still gets a result:
// dropping one leaves a tool_use nothing answered, which is a malformed conversation.
func (e *Engine) execute(ctx context.Context, uses []model.Block) ([]model.Block, *Stop, error) {
	results := make([]model.Block, 0, len(uses))
	var stop *Stop

	for _, use := range uses {
		text, err := e.dispatch(ctx, use)
		if err != nil {
			results = append(results, model.Block{
				Kind: model.BlockToolResult, ToolUseID: use.ToolUseID,
				Text: err.Error(), IsError: true,
			})
			continue
		}
		results = append(results, model.Block{
			Kind: model.BlockToolResult, ToolUseID: use.ToolUseID, Text: text,
		})

		if !IsEngineTool(use.ToolName) {
			if e.budget.Observe(e.Posterior()) && stop == nil {
				stop = &Stop{Reason: StopDiminishingReturns, Detail: e.budget.DiminishingDetail()}
			}
		}
	}
	if stop == nil {
		if budgetStop, done := e.budgetStop(); done {
			stop = &budgetStop
		}
	}
	return results, stop, nil
}

// dispatch runs one tool call.
func (e *Engine) dispatch(ctx context.Context, use model.Block) (string, error) {
	switch use.ToolName {
	case ToolProposeJudgments:
		return e.applyJudgments(use.ToolInput)
	case ToolProposeHypothesis:
		return e.applyHypothesis(use.ToolInput)
	}

	req, err := DecodeCall(use.ToolName, use.ToolInput, e.catalogue, e.at)
	if err != nil {
		return "", err
	}
	answer, err := e.Call(ctx, req)
	if err != nil {
		return "", err
	}
	if answer.Refused != nil {
		if err := e.recordUntested(req.GetServesHypothesisId(), req, answer.Refused); err != nil {
			return "", err
		}
		return fmt.Sprintf("not issued: the %s budget would have been breached (%s). "+
			"The hypothesis it would have tested is recorded untested with this query attached.",
			answer.Refused.Budget, answer.Refused.Reason), nil
	}
	return answer.ToolResult, nil
}

// applyJudgments validates a proposed batch and applies it through the ledger.
//
// The engine is the writer of record: the model proposes a direction and a strength, and every
// number comes from the published likelihood-ratio table (FR-023). A judgment that names a
// hypothesis or an evidence item that does not exist is refused back to the model as a tool
// result, because that is a mistake it can correct on the next turn.
func (e *Engine) applyJudgments(input []byte) (string, error) {
	proposed, err := DecodeJudgments(input)
	if err != nil {
		return "", err
	}

	var (
		applied  []ledger.Judgment
		refusals []string
	)
	for i, p := range proposed {
		direction, err := p.LedgerDirection()
		if err != nil {
			refusals = append(refusals, fmt.Sprintf("judgment %d: %v", i+1, err))
			continue
		}
		strength, err := p.LedgerStrength(direction)
		if err != nil {
			refusals = append(refusals, fmt.Sprintf("judgment %d: %v", i+1, err))
			continue
		}
		judgment, err := e.Judge(p.HypothesisID, p.EvidenceID, direction, strength, ledger.SourceModel, "")
		if err != nil {
			refusals = append(refusals, fmt.Sprintf("judgment %d (%s ← %s): %v",
				i+1, p.HypothesisID, p.EvidenceID, err))
			continue
		}
		applied = append(applied, judgment)
		e.mu.Lock()
		e.modelJudgments++
		e.mu.Unlock()
	}

	for _, judgment := range applied {
		if err := e.settleStatus(judgment.HypothesisID); err != nil {
			return "", err
		}
	}
	if len(applied) > 0 {
		e.budget.MarkFirstTested()
	}

	var b strings.Builder
	fmt.Fprintf(&b, "applied %d of %d judgments; the ledger recomputed every confidence.\n",
		len(applied), len(proposed))
	for _, judgment := range applied {
		fmt.Fprintf(&b, "  %s: %s %s on %s (ln LR %+.6f)\n",
			judgment.ID, judgment.Direction, judgment.Strength, judgment.HypothesisID, judgment.LnLR)
	}
	for _, refusal := range refusals {
		fmt.Fprintf(&b, "  refused — %s\n", refusal)
	}
	return b.String(), nil
}

// applyHypothesis adds a proposed hypothesis, with the prior the engine assigns.
//
// A hypothesis the model proposes that the ranker did not rank has no ranker score, so it takes
// the published condition prior: it is a candidate worth testing, not one the graph vouched for.
func (e *Engine) applyHypothesis(input []byte) (string, error) {
	proposed, err := DecodeHypothesis(input)
	if err != nil {
		return "", err
	}
	if proposed.CandidateChangeEntityRef != "" {
		if existing := e.hypothesisFor(proposed.CandidateChangeEntityRef); existing != "" {
			return fmt.Sprintf("%s is already in the ledger for %s; propose judgments on it rather than "+
				"a second hypothesis about the same change.", existing, proposed.CandidateChangeEntityRef), nil
		}
	}
	id := e.nextHypothesisID()
	score := ConditionPrior
	if proposed.Kind == string(ledger.KindChange) {
		score = e.rankerScore(proposed.CandidateChangeEntityRef)
	}
	h := ledger.Hypothesis{
		ID:                      id,
		Kind:                    ledger.Kind(proposed.Kind),
		Statement:               proposed.Statement,
		CandidateChangeEntityID: proposed.CandidateChangeEntityRef,
		TargetEntityIDs:         proposed.TargetEntityRefs,
		CausalRole:              ledger.RoleCause,
	}
	if err := e.ledger.AddHypothesis(h, score); err != nil {
		return "", err
	}
	updated, _ := e.ledger.Hypothesis(id)
	return fmt.Sprintf("%s added with prior %.6f, assigned by the engine from %s. Its confidence will move "+
		"only through judgments resting on evidence ids.",
		id, updated.Prior, priorSource(proposed.Kind)), nil
}

// ConditionPrior is the published weight a named condition enters with, on the same scale as a
// ranker score. It is deliberately modest: a condition nobody ranked is a candidate worth testing,
// not one the graph vouched for.
const ConditionPrior = 0.25

func priorSource(kind string) string {
	if kind == string(ledger.KindChange) {
		return "the published ranker score"
	}
	return "the published condition prior"
}

// rankerScore returns the graph's score for a change, where the diff ranked it. A change the diff
// did not rank enters at the condition prior, because the engine has no score of its own to give
// it and inventing one would be the re-ranking FR-029a forbids.
func (e *Engine) rankerScore(changeEntityID string) float64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	if score, ok := e.rankerScores[changeEntityID]; ok {
		return score
	}
	return ConditionPrior
}

// recordUntested attaches a refused call to the hypothesis it would have tested, with the exact
// next query and its deep link (FR-031, FR-047a).
//
// This is what turns "we ran out of budget" from a silence into a finding: the hypothesis is
// reported untested, with the reason and with the query a person can run themselves. It is never
// scored as refuted and never dropped.
func (e *Engine) recordUntested(hypothesisID string, req *investigationv1.AlgebraRequest, decision *budget.Decision) error {
	if hypothesisID == "" {
		return nil
	}
	if _, ok := e.ledger.Hypothesis(hypothesisID); !ok {
		return nil
	}
	link, _ := e.links.Link(req)
	return e.ledger.SetStatus(hypothesisID, ledger.StatusUntested, ledger.StatusUpdate{
		Reason: fmt.Sprintf("the query that would test it was not issued: the %s budget would have been "+
			"breached (%s)", decision.Budget, decision.Reason),
		NextQuery:         req.GetTerm(),
		NextQueryDeepLink: link,
	})
}

// hasModelJudgment reports whether the model has proposed any judgment yet.
func (e *Engine) hasModelJudgment() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.modelJudgments > 0
}

// budgetStop maps the budget manager's state onto a typed stop.
func (e *Engine) budgetStop() (Stop, bool) {
	if e.budget.Mode() != budget.ModeSynthesisOnly {
		return Stop{}, false
	}
	return Stop{Reason: StopBudgetExhausted, Detail: e.budget.ReserveReason()}, true
}

// remind appends the turn-scoped reminder a silent turn is answered with.
//
// It is a *new* system message with `clear_at: "next_user_message"`, not an edit of anything
// earlier: rewriting history to force a retry would invalidate every thinking block after the
// edit, which is the failure mode the append-only discipline exists to avoid (research §3).
func (e *Engine) remind(count int) {
	e.appendMessage(model.Message{
		Role:    model.RoleSystem,
		ClearAt: model.ClearAtNextUserMessage,
		Blocks: []model.Block{{Kind: model.BlockText, Text: fmt.Sprintf(
			"That turn proposed nothing and it has been recorded as such (reminder %d of %d).\n\n"+
				"The ledger above is the only belief state and it moves only through `%s`. If the evidence "+
				"you have moves a hypothesis either way, propose the judgments now, each naming the evidence "+
				"id it rests on. If it does not, say so by proposing a neutral judgment — tested and did not "+
				"separate is a real finding — or call a tool that would settle the question. If nothing "+
				"further can be established, write the verdict and stop.",
			count, MaxSilentTurns, ToolProposeJudgments)}},
	})
}

// ledgerMessage is the ledger re-rendered as a turn-scoped mid-conversation system message.
//
// `clear_at: "next_user_message"` means it renders for this turn and then stays in the transcript
// cleared. Earlier copies are never deleted: deleting one would be an edit to history.
func (e *Engine) ledgerMessage() model.Message {
	render := e.ledger.Render(ledger.RenderContext{
		Budgets:        budgetLines(e.budget.Remaining()),
		StopConditions: e.budget.StopConditions(),
	})
	if onset := e.Onset(); onset != nil {
		render = onset.Line() + "\n\n" + render
	}
	return model.Message{
		Role:    model.RoleSystem,
		ClearAt: model.ClearAtNextUserMessage,
		Blocks:  []model.Block{{Kind: model.BlockText, Text: render}},
	}
}

func budgetLines(lines []budget.Line) []ledger.BudgetLine {
	out := make([]ledger.BudgetLine, 0, len(lines))
	for _, line := range lines {
		out = append(out, ledger.BudgetLine{
			Name: line.Name, Remaining: line.Remaining, Limit: line.Limit, Unit: line.Unit,
		})
	}
	return out
}

// brief is the investigation brief: the subject, the instants, the window, the budget profile, the
// provisional ranking and the first wave's digests. It is the first user message and it is the
// only one the run needs.
func (e *Engine) brief(wave *FirstWave) string {
	var b strings.Builder
	b.WriteString("# The investigation\n\n")
	fmt.Fprintf(&b, "subject: %s\nsymptom: %s\norigin: %s\n", e.subject.EntityRef, e.subject.Statement,
		orNone(e.subject.Origin))
	fmt.Fprintf(&b, "symptom instant: %s\nvalid_at: %s\nobserved_at: %s\n",
		e.subject.FiredAt.Format(time.RFC3339),
		e.at.ValidAt.Format(time.RFC3339), e.at.ObservedAt.Format(time.RFC3339))
	fmt.Fprintf(&b, "window: %s .. %s\n",
		e.subject.Window.GetStart().AsTime().Format(time.RFC3339),
		e.subject.Window.GetEnd().AsTime().Format(time.RFC3339))
	fmt.Fprintf(&b, "budget profile: %s\n\n", e.budget.Profile().Describe())

	if provisional := e.Provisional(); provisional != nil {
		b.WriteString("# The provisional ranking (prior only, nothing tested)\n\n")
		b.WriteString(provisional.Render())
		b.WriteString("\n")
	}
	if onset := e.Onset(); onset != nil {
		b.WriteString("# Onset\n\n" + onset.Line() + "\n\n")
	}
	if wave != nil {
		b.WriteString("# The first wave\n\n")
		b.WriteString(wave.Render())
		b.WriteString("\nThe digests it produced are evidence items in the ledger above; their ids are the " +
			"ones your judgments must cite. Read the ledger, then either propose judgments on what the " +
			"wave established or call the tools that would settle what it did not.\n")
	}
	return b.String()
}

func orNone(value string) string {
	if value == "" {
		return "(none recorded)"
	}
	return value
}

func (e *Engine) appendMessage(message model.Message) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.history = append(e.history, message)
}

func (e *Engine) modelID() string {
	if e.client == nil {
		return ""
	}
	id, err := e.client.ModelFor(model.RoleInvestigator)
	if err != nil {
		return ""
	}
	return id
}

// fail records an engine failure as a typed stop. It is the only stop that is a defect rather than
// an outcome, and it still produces a recorded, readable investigation.
func (e *Engine) fail(err error) (Stop, error) {
	if divergence := model.DivergenceOf(err); divergence != nil {
		// A replay divergence is not an engine defect; it is the gate doing its job, and it must
		// surface with the diverging record named rather than as a generic failure.
		return Stop{Reason: StopFailed, Detail: divergence.Error()}, err
	}
	stop := Stop{Reason: StopFailed, Detail: err.Error()}
	if stopErr := e.Stop(stop); stopErr != nil {
		return stop, errors.Join(err, stopErr)
	}
	return stop, err
}

// CacheMisses returns the turns on which the stable prefix did not hit the cache. A non-empty list
// from turn two means a silent invalidator crept into the prefix (FR-061).
func (e *Engine) CacheMisses() []int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]int(nil), e.cacheMiss...)
}
