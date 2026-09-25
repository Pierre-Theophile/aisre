// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"context"
	"errors"
	"fmt"
	"time"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
)

// The call path (tasks.md T041; FR-010, FR-011, FR-015, FR-016, FR-018, FR-057d).
//
// Registration decides what may be called; this file decides what is recorded about a call. The
// two are separate because the interesting failures are on this side: a worker that times out, a
// worker that returns nothing, a retry that succeeded on the third attempt. Each of those is an
// **evidence item with a reason**, never a silence — because a silence is indistinguishable from
// "we looked and there was nothing", which is the one conclusion this feature is most careful
// never to fake (FR-027, contracts/worker-sdk.md §Failures are evidence).
//
// What the engine does with a CallRecord is Phase 6's business; producing one, for every call,
// including the ones that went wrong, is this file's.

// Attempt is one try at one call, recorded individually. Retries are counted against budget
// individually too: three attempts at one question cost three calls, and a record that collapsed
// them would understate both the spend and the flakiness.
type Attempt struct {
	// Number is 1 for the first try.
	Number int
	// StartedAt and Duration bound the attempt.
	StartedAt time.Time
	// Duration is wall-clock time.
	Duration time.Duration
	// Outcome is the typed outcome the attempt produced, when it produced one.
	Outcome investigationv1.TermOutcome
	// Err is the transport-level failure, when the worker returned one rather than a typed
	// outcome. A worker that returns an error has failed to answer at all, which is different
	// from answering QUERY_FAILED.
	Err string
	// Reason is the published reason code this attempt failed under, when there is one.
	Reason string
}

// CallRecord is everything recorded about one call: which worker, which capability, which mode,
// every attempt, and the answer that was finally returned.
type CallRecord struct {
	// Worker and Capability are the call.
	Worker string
	// Capability is the algebra term.
	Capability string
	// Mode is live or recorded, recorded per call (FR-015).
	Mode Mode
	// CostClass is what the budget manager spends against.
	CostClass CostClass
	// ServesHypothesisID and DiscriminatingQuestion are why the call was made. A call that
	// serves no hypothesis records `exploratory:<reason>`; a call that says nothing about why
	// it was made is not made (FR-018a).
	ServesHypothesisID string
	// DiscriminatingQuestion is the question the call is meant to settle.
	DiscriminatingQuestion string
	// TermKey is the key the answer is filed under in a world.
	TermKey string
	// Attempts is every try, in order.
	Attempts []Attempt
	// Outcome is the outcome finally returned.
	Outcome investigationv1.TermOutcome
	// ContainsModel and ModelID are copied from the declaration, so a reader of the record can
	// see whether a model touched this answer without resolving the worker again (FR-009a).
	ContainsModel bool
	// ModelID is the model, when one ran.
	ModelID string
	// Empty says the answer carried no rows. It is recorded because an empty answer is an
	// evidence item with a reason, not a silence.
	Empty bool
}

// RetryPolicy is how many times a call is retried and how long it waits. Zero retries is the
// default: a retry is a budget decision, so the engine asks for one explicitly.
type RetryPolicy struct {
	// MaxAttempts is the total number of tries, including the first. Zero and one both mean
	// "try once".
	MaxAttempts int
	// Backoff is the wait between attempts.
	Backoff time.Duration
	// Timeout bounds one attempt. Zero means the context's own deadline governs.
	Timeout time.Duration
}

// Caller issues calls through a registry, applying the registration gate, the timeout and the
// retry policy, and recording everything about each one.
type Caller struct {
	registry *Registry
	retry    RetryPolicy
}

// NewCaller returns a Caller over a registry.
func NewCaller(registry *Registry, retry RetryPolicy) *Caller {
	return &Caller{registry: registry, retry: retry}
}

// Call issues one call. It returns the response, the record of what happened, and an error only
// when the call could not be made at all — an undeclared capability, a term outside the algebra,
// a worker that is not registered. A worker that failed, timed out or returned nothing has
// *answered*: the response carries the typed outcome and the record carries the reason.
func (c *Caller) Call(ctx context.Context, workerName string, req Request) (Response, CallRecord, error) {
	record := CallRecord{
		Worker:                 workerName,
		Capability:             req.Capability,
		Mode:                   req.Mode,
		ServesHypothesisID:     req.Algebra.GetServesHypothesisId(),
		DiscriminatingQuestion: req.Algebra.GetDiscriminatingQuestion(),
	}
	if !req.Mode.Valid() {
		return Response{}, record, reject(ReasonUndeclaredCapability,
			"call to %s/%s declares mode %q, which is neither %q nor %q; the mode is recorded per call and cannot be inferred",
			workerName, req.Capability, req.Mode, ModeLive, ModeRecorded)
	}
	if req.Algebra.GetDiscriminatingQuestion() == "" {
		return Response{}, record, reject(ReasonUndeclaredCapability,
			"call to %s/%s carries no discriminating question; a call that says nothing about why it was made is not made (FR-018a). Record `exploratory:<reason>` where there is no hypothesis",
			workerName, req.Capability)
	}

	w, capability, err := c.registry.Resolve(workerName, req.Capability)
	if err != nil {
		return Response{}, record, err
	}
	declaration := w.Describe()
	record.CostClass = capability.CostClass
	record.ContainsModel = declaration.ContainsModel
	record.ModelID = declaration.ModelID

	attempts := max(c.retry.MaxAttempts, 1)
	var (
		resp    Response
		lastErr error
	)
	for attempt := 1; attempt <= attempts; attempt++ {
		if attempt > 1 && c.retry.Backoff > 0 {
			select {
			case <-ctx.Done():
				return resp, record, ctx.Err()
			case <-time.After(c.retry.Backoff):
			}
		}
		started := time.Now().UTC()
		callCtx := ctx
		var cancel context.CancelFunc
		if c.retry.Timeout > 0 {
			callCtx, cancel = context.WithTimeout(ctx, c.retry.Timeout)
		}
		resp, lastErr = w.Call(callCtx, req)
		if cancel != nil {
			cancel()
		}

		item := Attempt{
			Number:    attempt,
			StartedAt: started,
			Duration:  time.Since(started),
		}
		if lastErr != nil {
			item.Err = lastErr.Error()
			item.Reason = reasonFor(lastErr)
		} else {
			item.Outcome = resp.Algebra.GetOutcome()
		}
		record.Attempts = append(record.Attempts, item)

		if lastErr == nil {
			break
		}
	}

	if lastErr != nil {
		// The worker never answered. That is itself an evidence item: the record carries the
		// reason and every attempt, the affected hypotheses become untested with that reason,
		// and the investigation continues.
		record.Outcome = investigationv1.TermOutcome_QUERY_FAILED
		return Response{
			Worker:     workerName,
			Capability: req.Capability,
			Mode:       req.Mode,
			Algebra: &investigationv1.AlgebraResponse{
				Outcome:       investigationv1.TermOutcome_QUERY_FAILED,
				FailureReason: failureReasonFor(lastErr),
				FailureDetail: lastErr.Error(),
				Mode:          req.Mode.String(),
				CostClass:     capability.CostClass,
			},
		}, record, nil
	}

	record.Outcome = resp.Algebra.GetOutcome()
	record.TermKey = resp.Algebra.GetTermKey()
	record.Empty = isEmpty(resp.Algebra)
	if resp.Algebra.GetMode() == "" {
		resp.Algebra.Mode = req.Mode.String()
	}
	if resp.Algebra.GetMode() != req.Mode.String() {
		return resp, record, reject(ReasonUndeclaredCapability,
			"worker %s was called in mode %s and answered in mode %s; the mode is recorded per call and a worker that changes it has answered a different question",
			workerName, req.Mode, resp.Algebra.GetMode())
	}
	return resp, record, nil
}

// isEmpty reports whether a digest carried no rows. It is recorded rather than inferred later,
// because an empty DIGEST and a NO_DATA read alike in a summary and mean different things.
func isEmpty(resp *investigationv1.AlgebraResponse) bool {
	switch resp.GetOutcome() {
	case investigationv1.TermOutcome_NO_DATA, investigationv1.TermOutcome_NOT_RECORDED,
		investigationv1.TermOutcome_NOT_YET_INGESTED, investigationv1.TermOutcome_QUERY_FAILED:
		return true
	}
	switch body := resp.GetDigest().GetBody().(type) {
	case *investigationv1.Digest_Metric:
		return len(body.Metric.GetSeries()) == 0 && len(body.Metric.GetComparisons()) == 0
	case *investigationv1.Digest_Log:
		return len(body.Log.GetPatterns()) == 0
	case *investigationv1.Digest_Trace:
		return len(body.Trace.GetGroups()) == 0
	case *investigationv1.Digest_MonitorState:
		return len(body.MonitorState.GetTransitions()) == 0 && body.MonitorState.GetStateAtStart() == ""
	case *investigationv1.Digest_ErrorsByVersion:
		return len(body.ErrorsByVersion.GetVersions()) == 0
	case *investigationv1.Digest_Exemplars:
		return len(body.Exemplars.GetExemplars()) == 0
	case *investigationv1.Digest_Knowledge:
		return len(body.Knowledge.GetItems()) == 0
	case *investigationv1.Digest_Onset:
		return body.Onset.GetUnavailable()
	default:
		// No body at all. For a telemetry answer that is as empty as it gets. The graph
		// worker's answers legitimately land here — a subgraph travels in Response.Graph
		// rather than in one of the eight digest bodies — so the engine reads emptiness for a
		// graph call from that message rather than from this flag.
		return resp.GetDigest().GetBody() == nil
	}
}

func reasonFor(err error) string {
	if reason := ReasonOf(err); reason != "" {
		return reason
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timed_out"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	return "worker_error"
}

func failureReasonFor(err error) investigationv1.FailureReason {
	switch reasonFor(err) {
	case ReasonOutsideAlgebra:
		return investigationv1.FailureReason_OUTSIDE_ALGEBRA
	case "timed_out":
		return investigationv1.FailureReason_TIMED_OUT
	case ReasonUndeclaredCapability:
		return investigationv1.FailureReason_NOT_PERMITTED
	default:
		return investigationv1.FailureReason_REJECTED_BY_BACKEND
	}
}

// Summary is the one-line rendering of a record, for `worker call` and for a turn's evidence
// list. It names the mode and, where the answer was empty or failed, why.
func (r CallRecord) Summary() string {
	base := fmt.Sprintf("%s/%s [%s, %s] → %s", r.Worker, r.Capability, r.Mode,
		lowerCostClass(r.CostClass), r.Outcome)
	if len(r.Attempts) > 1 {
		base += fmt.Sprintf(" after %d attempts", len(r.Attempts))
	}
	if r.Empty && r.Outcome == investigationv1.TermOutcome_DIGEST {
		base += " (no rows — an empty digest, not evidence that nothing happened)"
	}
	if last := r.lastFailure(); last != "" {
		base += " (" + last + ")"
	}
	return base
}

func (r CallRecord) lastFailure() string {
	for i := len(r.Attempts) - 1; i >= 0; i-- {
		if r.Attempts[i].Err != "" {
			return r.Attempts[i].Reason + ": " + r.Attempts[i].Err
		}
	}
	return ""
}

func lowerCostClass(class CostClass) string {
	switch class {
	case CostClassCheap:
		return "cheap"
	case CostClassStandard:
		return "standard"
	case CostClassExpensive:
		return "expensive"
	default:
		return "unspecified"
	}
}
