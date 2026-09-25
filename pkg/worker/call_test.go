// SPDX-License-Identifier: Apache-2.0

package worker_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/pkg/worker"
)

// The call path (tasks.md T041; FR-010, FR-011, FR-015, FR-016, FR-018).
//
// The interesting cases are all failures: a worker that times out, a worker that returns
// nothing, a retry that succeeded on the third attempt. Each is an evidence item with a reason,
// never a silence — because a silence is indistinguishable from "we looked and there was
// nothing", which is the one conclusion this feature is most careful never to fake.

// callStub is a worker whose behaviour each test dictates.
type callStub struct {
	description worker.Description
	calls       int
	failures    int // how many of the first calls fail
	err         error
	outcome     investigationv1.TermOutcome
	delay       time.Duration
	mode        string // what it stamps on the response; "" means "whatever it was asked"
}

func (s *callStub) Describe() worker.Description { return s.description }

func (s *callStub) Call(ctx context.Context, req worker.Request) (worker.Response, error) {
	s.calls++
	if s.delay > 0 {
		select {
		case <-ctx.Done():
			return worker.Response{}, ctx.Err()
		case <-time.After(s.delay):
		}
	}
	if s.calls <= s.failures {
		return worker.Response{}, s.err
	}
	outcome := s.outcome
	if outcome == investigationv1.TermOutcome_TERM_OUTCOME_UNSPECIFIED {
		outcome = investigationv1.TermOutcome_DIGEST
	}
	mode := s.mode
	if mode == "" {
		mode = req.Mode.String()
	}
	return worker.Response{
		Worker:     s.description.Name,
		Capability: req.Capability,
		Mode:       req.Mode,
		Algebra: &investigationv1.AlgebraResponse{
			Outcome: outcome,
			Mode:    mode,
			TermKey: "a-term-key",
			Digest:  &investigationv1.Digest{Coverage: &investigationv1.Coverage{DataSource: "stub"}},
		},
	}, nil
}

func callDescription() worker.Description {
	return worker.Description{
		Name:          "metrics",
		SourceOfTruth: "synthetic:synthetic",
		Capabilities: []worker.Capability{
			{Name: "compare", ReadOnly: true, CostClass: worker.CostClassStandard},
		},
		Redaction: worker.RedactionPolicy{PolicyVersion: "1.0.0"},
		Modes:     []worker.Mode{worker.ModeLive, worker.ModeRecorded},
		Version:   "0.1.0",
	}
}

func callRequest(capability string, mode worker.Mode) worker.Request {
	return worker.Request{
		Capability: capability,
		Mode:       mode,
		Algebra: &investigationv1.AlgebraRequest{
			ServesHypothesisId:     "hyp-1",
			DiscriminatingQuestion: "did the error rate move with the deploy?",
		},
	}
}

func callerFor(t *testing.T, w worker.Worker, retry worker.RetryPolicy) *worker.Caller {
	t.Helper()
	registry := worker.NewRegistry()
	if err := registry.Register(w); err != nil {
		t.Fatalf("register: %v", err)
	}
	return worker.NewCaller(registry, retry)
}

func TestACallIsRecordedWithItsModeAndCost(t *testing.T) {
	t.Parallel()

	w := &callStub{description: callDescription()}
	_, record, err := callerFor(t, w, worker.RetryPolicy{}).
		Call(context.Background(), "metrics", callRequest("compare", worker.ModeRecorded))
	if err != nil {
		t.Fatalf("call: %v", err)
	}

	if record.Mode != worker.ModeRecorded {
		t.Errorf("mode = %q, want recorded; the mode is recorded per call (FR-015)", record.Mode)
	}
	if record.CostClass != worker.CostClassStandard {
		t.Errorf("cost class = %s, want standard; budgets are spent per cost class", record.CostClass)
	}
	if record.ServesHypothesisID != "hyp-1" || record.DiscriminatingQuestion == "" {
		t.Errorf("the call's purpose was not recorded: %+v", record)
	}
	if len(record.Attempts) != 1 {
		t.Errorf("attempts = %d, want 1", len(record.Attempts))
	}
	if record.ContainsModel {
		t.Error("the record claims a model ran in a worker that declares none")
	}
}

// TestAnUndeclaredCapabilityIsNotCallable: registration is the gate, and the call path applies
// it too (FR-016).
func TestAnUndeclaredCapabilityIsNotCallable(t *testing.T) {
	t.Parallel()

	w := &callStub{description: callDescription()}
	caller := callerFor(t, w, worker.RetryPolicy{})

	_, _, err := caller.Call(context.Background(), "metrics", callRequest("onset", worker.ModeRecorded))
	if worker.ReasonOf(err) != worker.ReasonUndeclaredCapability {
		t.Fatalf("reason = %q, want %q (%v)", worker.ReasonOf(err), worker.ReasonUndeclaredCapability, err)
	}
	if w.calls != 0 {
		t.Error("the worker was called for a capability it did not declare")
	}
	if !strings.Contains(err.Error(), "compare") {
		t.Errorf("the refusal does not name what is declared: %v", err)
	}

	if _, _, err := caller.Call(context.Background(), "logs", callRequest("compare", worker.ModeRecorded)); err == nil {
		t.Error("a call to an unregistered worker was accepted")
	}
}

// TestACallMustSayWhyItWasMade: a call that serves no hypothesis records
// `exploratory:<reason>`; a call that says nothing about why it was made is not made (FR-018a).
func TestACallMustSayWhyItWasMade(t *testing.T) {
	t.Parallel()

	w := &callStub{description: callDescription()}
	caller := callerFor(t, w, worker.RetryPolicy{})

	req := callRequest("compare", worker.ModeRecorded)
	req.Algebra.DiscriminatingQuestion = ""
	if _, _, err := caller.Call(context.Background(), "metrics", req); err == nil {
		t.Fatal("a call with no discriminating question was made")
	}

	req.Algebra.ServesHypothesisId = ""
	req.Algebra.DiscriminatingQuestion = "exploratory:widening the neighbourhood"
	if _, _, err := caller.Call(context.Background(), "metrics", req); err != nil {
		t.Fatalf("an exploratory call was refused: %v", err)
	}
}

// TestAModeMustBeDeclared: the mode is recorded per call and cannot be inferred.
func TestAModeMustBeDeclared(t *testing.T) {
	t.Parallel()

	w := &callStub{description: callDescription()}
	caller := callerFor(t, w, worker.RetryPolicy{})

	if _, _, err := caller.Call(context.Background(), "metrics", callRequest("compare", "guess")); err == nil {
		t.Fatal("a call in an unpublished mode was made")
	}

	// A worker that answers in a mode other than the one it was asked has answered a different
	// question.
	lying := &callStub{description: callDescription(), mode: "live"}
	_, _, err := callerFor(t, lying, worker.RetryPolicy{}).
		Call(context.Background(), "metrics", callRequest("compare", worker.ModeRecorded))
	if err == nil {
		t.Fatal("a worker that changed mode was accepted")
	}
}

// TestFailuresAreEvidence: a worker failure is an answer — the typed outcome and the reason are
// returned rather than an error the caller has to interpret.
func TestFailuresAreEvidence(t *testing.T) {
	t.Parallel()

	w := &callStub{description: callDescription(), failures: 99, err: errors.New("the vendor hung up")}
	resp, record, err := callerFor(t, w, worker.RetryPolicy{}).
		Call(context.Background(), "metrics", callRequest("compare", worker.ModeLive))
	if err != nil {
		t.Fatalf("a worker failure was returned as an error rather than as evidence: %v", err)
	}
	if resp.Algebra.GetOutcome() != investigationv1.TermOutcome_QUERY_FAILED {
		t.Errorf("outcome = %s, want QUERY_FAILED", resp.Algebra.GetOutcome())
	}
	if !strings.Contains(resp.Algebra.GetFailureDetail(), "hung up") {
		t.Errorf("failure detail = %q, which does not say what went wrong", resp.Algebra.GetFailureDetail())
	}
	if record.Outcome != investigationv1.TermOutcome_QUERY_FAILED {
		t.Errorf("the record's outcome = %s, want QUERY_FAILED", record.Outcome)
	}
	if !strings.Contains(record.Summary(), "hung up") {
		t.Errorf("the summary does not carry the reason: %s", record.Summary())
	}
}

// TestRetriesAreRecordedIndividually: three attempts at one question cost three calls, and a
// record that collapsed them would understate both the spend and the flakiness.
func TestRetriesAreRecordedIndividually(t *testing.T) {
	t.Parallel()

	w := &callStub{description: callDescription(), failures: 2, err: errors.New("rate limited")}
	resp, record, err := callerFor(t, w, worker.RetryPolicy{MaxAttempts: 3}).
		Call(context.Background(), "metrics", callRequest("compare", worker.ModeLive))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if resp.Algebra.GetOutcome() != investigationv1.TermOutcome_DIGEST {
		t.Errorf("outcome = %s, want DIGEST after the third attempt succeeded", resp.Algebra.GetOutcome())
	}
	if len(record.Attempts) != 3 {
		t.Fatalf("attempts = %d, want 3 recorded individually", len(record.Attempts))
	}
	for i, attempt := range record.Attempts[:2] {
		if attempt.Err == "" {
			t.Errorf("attempt %d records no failure", i+1)
		}
		if attempt.Number != i+1 {
			t.Errorf("attempt %d is numbered %d", i+1, attempt.Number)
		}
	}
	if record.Attempts[2].Err != "" {
		t.Error("the successful attempt records a failure")
	}
	if !strings.Contains(record.Summary(), "after 3 attempts") {
		t.Errorf("the summary does not say how many attempts it took: %s", record.Summary())
	}
}

// TestATimeoutIsRecordedAsOne: "the worker timed out" and "the worker found nothing" are
// different sentences.
func TestATimeoutIsRecordedAsOne(t *testing.T) {
	t.Parallel()

	w := &callStub{description: callDescription(), delay: 200 * time.Millisecond, failures: 99, err: errors.New("unused")}
	resp, record, err := callerFor(t, w, worker.RetryPolicy{Timeout: 10 * time.Millisecond}).
		Call(context.Background(), "metrics", callRequest("compare", worker.ModeLive))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if resp.Algebra.GetFailureReason() != investigationv1.FailureReason_TIMED_OUT {
		t.Errorf("failure reason = %s, want TIMED_OUT", resp.Algebra.GetFailureReason())
	}
	if record.Attempts[0].Reason != "timed_out" {
		t.Errorf("attempt reason = %q, want timed_out", record.Attempts[0].Reason)
	}
}

// TestAnEmptyDigestIsRecordedAsEmpty: an empty DIGEST and a NO_DATA read alike in a summary and
// mean different things, so the record says which it was.
func TestAnEmptyDigestIsRecordedAsEmpty(t *testing.T) {
	t.Parallel()

	w := &callStub{description: callDescription(), outcome: investigationv1.TermOutcome_DIGEST}
	_, record, err := callerFor(t, w, worker.RetryPolicy{}).
		Call(context.Background(), "metrics", callRequest("compare", worker.ModeRecorded))
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !record.Empty {
		t.Error("a digest with no rows was not recorded as empty")
	}
	if !strings.Contains(record.Summary(), "not evidence that nothing happened") {
		t.Errorf("the summary lets an empty digest read as NO_DATA: %s", record.Summary())
	}
}
