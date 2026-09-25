// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/internal/investigation/budget"
	"github.com/Pierre-Theophile/aisre/internal/investigation/engine"
	"github.com/Pierre-Theophile/aisre/internal/investigation/ledger"
	"github.com/Pierre-Theophile/aisre/internal/investigation/model"
	"github.com/Pierre-Theophile/aisre/internal/investigation/replay"
	investigationstore "github.com/Pierre-Theophile/aisre/internal/investigation/store"
	sdk "github.com/Pierre-Theophile/aisre/pkg/backend"
	"github.com/Pierre-Theophile/aisre/pkg/worker"
)

// What a finished run leaves behind.
//
// Four artifacts, and the reason each exists is different:
//
//	the ledger rows      so a confidence is recomputable from the database alone (Invariant 1)
//	the call rows        so "we asked twice" survives, in order, with what came back (FR-033)
//	the spend row        so a historical cost says what it meant (FR-044, FR-048)
//	the trajectory file  so the run replays byte-for-byte with no network (FR-042a)
//
// The call rows are derived from the trajectory rather than from a second bookkeeping structure
// the engine would have to keep. That is not a shortcut: the trajectory *is* the record of what
// was issued and what came back, and deriving the rows from it means the two cannot disagree. A
// second list, kept alongside, would be a second thing to forget to append to.
//
// The pairing rule matters because evidence gathering is parallel: the first wave issues its
// telemetry calls concurrently, so two requests may be recorded before either answer is. A
// response is therefore paired with the oldest unanswered request carrying the same term key,
// falling back to the oldest unanswered request. Identical repeated terms pair in issue order,
// which is the only order that means anything when the terms are the same (Invariant 5).

// writtenRecording is where a run's trajectory landed.
type writtenRecording struct {
	key    string
	digest string
}

// writeTrajectory renders layer 1 under the recording root (FR-042a).
//
// A failure to write is logged and does not fail the investigation: the conclusion is the thing
// an on-call is waiting for, and losing the replay artifact is a smaller loss than losing the
// answer. The row then carries no recording key, which is the honest statement that there is no
// recording rather than a link to a file that is not there.
func (r *Runner) writeTrajectory(investigationID string, eng *engine.Engine) writtenRecording {
	if r.cfg.RecordingRoot == "" {
		return writtenRecording{}
	}
	dir := filepath.Join(r.cfg.RecordingRoot, investigationID, replay.TrajectoriesDirName)
	written, err := replay.Write(dir, eng.Trajectory())
	if err != nil {
		r.logger.Error("investigation runner: could not write the trajectory",
			"investigation_id", investigationID, "dir", dir, "error", err.Error())
		return writtenRecording{}
	}
	return writtenRecording{
		key:    filepath.Join(investigationID, replay.TrajectoriesDirName, written.RunID+replay.TrajectoryExt),
		digest: written.Digest,
	}
}

// PersistWindow bounds every write a finished run makes on a context detached from the caller's.
//
// A finished run's record is not the caller's to cancel — but nor may it block forever on a
// database that has stopped answering, because the goroutine holding it is the one the caller is
// no longer waiting on and nothing else will notice.
const PersistWindow = 30 * time.Second

// detachedWrite is the context a terminal-state write runs under: the caller's values, none of
// its cancellation, and a bound of its own (FR-007).
//
// The reason is FR-007. A row that has reached a terminal state is immutable, so a terminal state
// written in error is written for good: there is no second attempt that corrects a `failed` row
// for an investigation that in fact concluded. A client that hangs up in the second between the
// engine's last tool call and the ledger insert would otherwise leave exactly that — a
// permanently failed record of an answered question — and it is the commonest reason to be here,
// because the commonest reason a caller goes away is that it got bored waiting.
func detachedWrite(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), PersistWindow)
}

// persist writes the ledger, the calls and the spend.
//
// It runs on a detached, bounded context: see detachedWrite. Everything it writes is what the run
// already established, so there is nothing here a cancellation could usefully stop.
func (r *Runner) persist(ctx context.Context, rn *run, eng *engine.Engine) error {
	ctx, cancel := detachedWrite(ctx)
	defer cancel()
	r.captureSpend(rn, eng)
	book := eng.Ledger()
	// Evidence, hypotheses and judgments go first and together: the hypothesis set is written in
	// one transaction so the deferred sum trigger sees a whole distribution (Invariant 10), and
	// `worker_calls.evidence_id` references the rows it writes.
	if err := r.ledgerDAO.Save(ctx, book); err != nil {
		return fmt.Errorf("save ledger: %w", err)
	}
	if err := r.saveCalls(ctx, rn.investigationID, eng); err != nil {
		return err
	}
	if err := r.saveSpend(ctx, rn); err != nil {
		return err
	}
	return nil
}

// persistQuietly is persist on the failure path: whatever the run established is still written,
// and a second failure while writing it is logged rather than replacing the first one.
//
// A failed run that lost its evidence is a failure nobody can diagnose, which is why this runs at
// all; a failed run whose *reported* cause is "the database was also unhappy" is a failure nobody
// can diagnose either, which is why it does not return.
func (r *Runner) persistQuietly(ctx context.Context, rn *run, eng *engine.Engine) {
	if err := r.persist(ctx, rn, eng); err != nil {
		r.logger.Error("investigation runner: could not persist a failed run",
			"investigation_id", rn.investigationID, "error", err.Error())
	}
}

// captureSpend renders the run's consumption once, so that the spend row, the decision record and
// anything that reads them back quote one figure (FR-044).
//
// A run with no model client is priced as zero tokens and no price table, which is what it spent:
// the monetary figure is derived from the versioned table and a run that had no table says so
// rather than reporting a zero it cannot justify (FR-048).
func (r *Runner) captureSpend(rn *run, eng *engine.Engine) {
	if rn.spend != nil {
		return
	}
	var client budget.Client
	if r.cfg.Model != nil {
		client = r.cfg.Model
	}
	spend, err := eng.Budget().Spend(client)
	if err != nil {
		r.logger.Warn("investigation runner: could not price the run",
			"investigation_id", rn.investigationID, "error", err.Error())
		return
	}
	rn.spend = spend
}

// saveCalls writes the worker and model call rows, in one interleaved sequence space (FR-033).
func (r *Runner) saveCalls(ctx context.Context, investigationID string, eng *engine.Engine) error {
	records := eng.Trajectory().Records()
	evidence := evidenceIndexOf(eng.Ledger())
	hypotheses := hypothesisIndexOf(eng.Ledger())

	var (
		waiting      []pendingCall
		modelPending *investigationv1.ModelRequestRecord
		seq          int
	)

	for _, record := range records {
		switch body := record.GetRecord().(type) {
		case *investigationv1.TrajectoryRecord_WorkerRequest:
			waiting = append(waiting, pendingCall{
				worker:  body.WorkerRequest.GetWorker(),
				request: body.WorkerRequest.GetRequest(),
				termKey: body.WorkerRequest.GetTermKey(),
			})
		case *investigationv1.TrajectoryRecord_WorkerResponse:
			resp := body.WorkerResponse.GetResponse()
			index := matchRequest(waiting, resp.GetTermKey())
			if index < 0 {
				// A response with nothing to answer is a truncated recording. It is skipped
				// rather than written as a call with no request, which would be a row nobody
				// can interpret.
				continue
			}
			req := waiting[index]
			waiting = append(waiting[:index], waiting[index+1:]...)

			call := investigationstore.WorkerCall{
				CallID:                 fmt.Sprintf("%s:call:%d", investigationID, seq),
				Seq:                    seq,
				Worker:                 req.worker,
				Capability:             capabilityOf(req.request),
				TermKey:                req.termKey,
				DiscriminatingQuestion: questionOf(req.request),
				Mode:                   modeOf(resp),
				Outcome:                callOutcomeOf(resp),
				Duration:               time.Duration(resp.GetDurationMs()) * time.Millisecond,
				CostClass:              costClassOf(resp),
				Backend:                r.workers.Backend(req.worker),
				RemainingQuota:         remainingQuotaOf(req.worker, resp),
				ResponseBytes:          len(resp.GetResponseDigest()),
				Truncated:              resp.GetDigest().GetTruncation().GetTruncated(),
				EvidenceID:             evidence.take(req.termKey),
			}
			if id := req.request.GetServesHypothesisId(); hypotheses[id] {
				call.ServesHypothesisID = id
			}
			if err := r.recorder.Record(ctx, investigationID, call, ledger.EvidenceItem{}); err != nil {
				return fmt.Errorf("record worker call %d: %w", seq, err)
			}
			seq++
		case *investigationv1.TrajectoryRecord_ModelRequest:
			modelPending = body.ModelRequest
		case *investigationv1.TrajectoryRecord_ModelResponse:
			if modelPending == nil {
				continue
			}
			resp := body.ModelResponse
			usage := resp.GetUsage()
			call := investigationstore.ModelCall{
				ModelCallID:      fmt.Sprintf("%s:model:%d", investigationID, seq),
				Seq:              seq,
				Role:             modelRoleName(modelPending.GetRole()),
				ModelID:          modelPending.GetModelId(),
				Effort:           r.effortFor(modelPending.GetRole()),
				Betas:            r.betas(),
				RequestDigest:    modelPending.GetRequestDigest(),
				ResponseDigest:   resp.GetResponseDigest(),
				InputTokens:      int(usage[model.ClassInput]),
				CacheWriteTokens: int(usage[model.ClassCacheWrite]),
				CacheReadTokens:  int(usage[model.ClassCacheRead]),
				OutputTokens:     int(usage[model.ClassOutput]),
				StopReason:       resp.GetStopReason(),
			}
			modelPending = nil
			if err := r.recorder.RecordModelCall(ctx, investigationID, call); err != nil {
				return fmt.Errorf("record model call %d: %w", seq, err)
			}
			seq++
		}
	}
	return nil
}

// saveSpend records consumption against every budget (FR-044).
func (r *Runner) saveSpend(ctx context.Context, rn *run) error {
	spend := r.spendOf(rn)
	if spend == nil {
		return nil
	}
	if err := r.dao.SaveSpend(ctx, rn.investigationID, rn.profile.Name, spend); err != nil {
		return fmt.Errorf("save spend: %w", err)
	}
	return nil
}

// spendOf is the run's consumption as captureSpend rendered it, or nil before it has run.
func (r *Runner) spendOf(rn *run) *investigationv1.BudgetSpend { return rn.spend }

// emitDecisionRecord appends the one event an investigation writes into the graph (FR-035).
func (r *Runner) emitDecisionRecord(
	ctx context.Context,
	rn *run,
	eng *engine.Engine,
	conclusion investigationstore.Conclusion,
) (string, error) {
	return r.emitDecisionRecordFrom(ctx, rn, eng.Ledger(), r.spendOf(rn), conclusion)
}

// emitDecisionRecordFrom is emitDecisionRecord over a ledger the caller holds, which is the
// unresolved-subject path: there is no engine, and there is still a conclusion to record.
//
// A rejected record is a result rather than an error (SC-010): the reason code is logged, the
// rejection is in `log.rejected_events`, and the investigation still concludes. Refusing to
// conclude because the graph refused the summary would lose the answer over its footnote.
func (r *Runner) emitDecisionRecordFrom(
	ctx context.Context,
	rn *run,
	book *ledger.Ledger,
	spend *investigationv1.BudgetSpend,
	conclusion investigationstore.Conclusion,
) (string, error) {
	input := investigationstore.DecisionInput{
		Ledger:          book,
		Subjects:        rn.subjects,
		StartedAt:       rn.startedAt,
		EndedAt:         conclusion.EndedAt,
		Outcome:         conclusion.Outcome,
		StopReason:      stopReasonNameOf(conclusion.StopReason),
		VerdictLine:     conclusion.VerdictLine,
		Requester:       rn.requester,
		Spend:           spendMap(spend),
		ModelConfig:     r.modelConfigMap(),
		RecordingKey:    conclusion.RecordingKey,
		RecordingDigest: conclusion.RecordingDigest,
	}
	result, err := investigationstore.EmitDecisionRecord(ctx, r.proj, input)
	if err != nil {
		return "", err
	}
	if result.GetStatus() == 0 || result.GetReasonCode() != "" {
		r.logger.Warn("investigation runner: the decision record was not applied",
			"investigation_id", rn.investigationID,
			"status", result.GetStatus().String(),
			"reason", result.GetReasonCode(),
			"detail", result.GetReasonDetail())
	}
	return result.GetEventId(), nil
}

// --- small derivations ------------------------------------------------------------------------

// evidenceIndex hands out the evidence id a call produced, matched on the term key and consumed
// in ledger order so that a repeated identical term maps to its own row (Invariant 5).
type evidenceIndex map[string][]string

func evidenceIndexOf(book *ledger.Ledger) evidenceIndex {
	out := evidenceIndex{}
	for _, item := range book.EvidenceItems() {
		if item.ResponseKey == "" {
			continue
		}
		out[item.ResponseKey] = append(out[item.ResponseKey], item.ID)
	}
	return out
}

func (i evidenceIndex) take(termKey string) string {
	ids := i[termKey]
	if len(ids) == 0 {
		return ""
	}
	i[termKey] = ids[1:]
	return ids[0]
}

func hypothesisIndexOf(book *ledger.Ledger) map[string]bool {
	out := map[string]bool{}
	for _, h := range book.Hypotheses() {
		out[h.ID] = true
	}
	return out
}

// pendingCall is a worker request the trajectory has recorded and not yet answered.
type pendingCall struct {
	worker  string
	request *investigationv1.AlgebraRequest
	termKey string
}

// matchRequest returns the oldest unanswered request for a term key, or the oldest unanswered
// request of any key when none matches. -1 means the recording holds an answer to nothing.
func matchRequest(waiting []pendingCall, termKey string) int {
	for i, p := range waiting {
		if p.termKey == termKey {
			return i
		}
	}
	if len(waiting) > 0 {
		return 0
	}
	return -1
}

// capabilityOf is the algebra term a request asked for.
func capabilityOf(req *investigationv1.AlgebraRequest) string {
	return backend.TermName(req.GetTerm())
}

// questionOf is FR-018a's discriminating question. The column is NOT NULL and non-empty on
// purpose: a call that says nothing about what it was meant to settle is a call nobody can audit,
// and `exploratory:<reason>` is the published spelling of "we were looking around".
func questionOf(req *investigationv1.AlgebraRequest) string {
	if q := strings.TrimSpace(req.GetDiscriminatingQuestion()); q != "" {
		return q
	}
	return investigationstore.Exploratory("the recording carries no question for this call")
}

// remainingQuotaOf keys the vendor's own remaining-quota figure by the backend it came from, so
// that a per-backend quota share is measurable from the rows rather than only from the manager's
// in-memory state (FR-047a). A backend that could not determine it records nothing, which is a
// different statement from recording a zero.
func remainingQuotaOf(workerName string, resp *investigationv1.AlgebraResponse) map[string]int64 {
	coverage := resp.GetDigest().GetCoverage()
	if coverage == nil || coverage.GetQuotaUndetermined() {
		return nil
	}
	return map[string]int64{workerName: coverage.GetRemainingQuota()}
}

func modeOf(resp *investigationv1.AlgebraResponse) string {
	if mode := resp.GetMode(); mode != "" {
		return mode
	}
	return string(worker.ModeRecorded)
}

func costClassOf(resp *investigationv1.AlgebraResponse) string {
	if resp.GetCostClass() == investigationv1.CostClass_COST_CLASS_UNSPECIFIED {
		return ""
	}
	return sdk.CostClassName(resp.GetCostClass())
}

// callOutcomeOf maps a term outcome onto the six call outcomes, which are never collapsed into
// "no" (FR-027, Invariant 8).
//
// `not_yet_ingested` maps to `empty` rather than to `failed`: the call succeeded and the window
// held nothing *yet*, which the evidence item keeps distinct on its own row. A query refused for
// being outside the algebra is its own outcome, because it is a defect in the question rather
// than an answer about the world.
func callOutcomeOf(resp *investigationv1.AlgebraResponse) string {
	switch resp.GetOutcome() {
	case investigationv1.TermOutcome_DIGEST, investigationv1.TermOutcome_PARTIAL:
		return investigationstore.CallOutcomeAnswered
	case investigationv1.TermOutcome_NO_DATA, investigationv1.TermOutcome_NOT_YET_INGESTED:
		return investigationstore.CallOutcomeEmpty
	case investigationv1.TermOutcome_NOT_RECORDED:
		return investigationstore.CallOutcomeNotRecorded
	case investigationv1.TermOutcome_QUERY_FAILED:
		switch resp.GetFailureReason() {
		case investigationv1.FailureReason_TIMED_OUT:
			return investigationstore.CallOutcomeTimedOut
		case investigationv1.FailureReason_OUTSIDE_ALGEBRA:
			return investigationstore.CallOutcomeRefusedOutsideAlgebra
		default:
			return investigationstore.CallOutcomeFailed
		}
	default:
		return investigationstore.CallOutcomeFailed
	}
}

func stopReasonNameOf(reason investigationv1.StopReason) string {
	switch reason {
	case investigationv1.StopReason_COMPLETED:
		return string(engine.StopCompleted)
	case investigationv1.StopReason_BUDGET_EXHAUSTED_STOP:
		return string(engine.StopBudgetExhausted)
	case investigationv1.StopReason_DIMINISHING_RETURNS:
		return string(engine.StopDiminishingReturns)
	case investigationv1.StopReason_WORKER_UNAVAILABLE:
		return string(engine.StopWorkerUnavailable)
	case investigationv1.StopReason_REFUSED:
		return string(engine.StopRefused)
	case investigationv1.StopReason_ENGINE_FAILED:
		return string(engine.StopFailed)
	default:
		return ""
	}
}

// modelRoleName maps the engine's role onto the vocabulary the schema accepts. The logs labeller
// is spelled differently in the two places and mapping it here is cheaper than a migration that
// renames a published CHECK.
func modelRoleName(role string) string {
	if role == string(model.RoleLogsLabeller) {
		return "worker_logs_label"
	}
	return role
}

func (r *Runner) effortFor(role string) string {
	if r.cfg.Model == nil {
		return ""
	}
	rc, ok := r.cfg.Model.Config().Role(model.Role(role))
	if !ok {
		return ""
	}
	return rc.Effort
}

func (r *Runner) betas() []string {
	if r.cfg.Model == nil {
		return nil
	}
	return r.cfg.Model.Config().Betas
}

func (r *Runner) modelConfigMap() map[string]any {
	if r.modelConfig == nil {
		return nil
	}
	return r.modelConfig.AsMap()
}

// spendMap renders the spend for the decision record's allow-listed property.
//
// Every value is a scalar or a map of scalars. That is not tidiness: `internal/log`'s telemetry
// denylist reads a list of numbers as a series whatever it is called, and a spend report that
// carried one would have its whole decision record rejected (SC-010).
func spendMap(spend *investigationv1.BudgetSpend) map[string]any {
	if spend == nil {
		return nil
	}
	out := map[string]any{
		"wall_time_seconds":     float64(spend.GetWallTimeSeconds()),
		"cost_units":            spend.GetCostUnits(),
		"widest_window_seconds": float64(spend.GetWidestWindowSeconds()),
	}
	// `quota_share_used` is per backend, and structpb takes `map[string]any` and nothing else:
	// a typed map reaches it as an unsupported type and takes the whole decision record down
	// with it.
	if shares := spend.GetQuotaShareUsed(); len(shares) > 0 {
		converted := make(map[string]any, len(shares))
		for k, v := range shares {
			converted[k] = v
		}
		out["quota_share_used"] = converted
	}
	if v := spend.GetPriceTableVersion(); v != "" {
		out["price_table_version"] = v
	}
	if name := spend.GetLimits().GetName(); name != "" {
		out["profile"] = name
	}
	for key, values := range map[string]map[string]int64{
		"tokens_by_model_and_class": spend.GetTokensByModelAndClass(),
		"calls_by_worker":           spend.GetCallsByWorker(),
		"calls_by_backend":          spend.GetCallsByBackend(),
		"calls_by_cost_class":       spend.GetCallsByCostClass(),
		"remaining_quota_observed":  spend.GetRemainingQuotaObserved(),
	} {
		if len(values) == 0 {
			continue
		}
		converted := make(map[string]any, len(values))
		for k, v := range values {
			converted[k] = float64(v)
		}
		out[key] = converted
	}
	return out
}
