// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/investigation/budget"
	"github.com/Pierre-Theophile/aisre/internal/investigation/model"
	"github.com/Pierre-Theophile/aisre/internal/investigation/verify"
)

// Verification on every terminal path (FR-022a, FR-022c, T121, plan §"The 2026-11-01 amendment").
//
// The checker and the fresh-context verifier run inside Stop, after the verdict is settled and
// before the trajectory's stop record. Two consequences follow, and both are the reason for the
// placement:
//
//   - Every exit passes through Stop, so no path can skip the pass silently. A run that could not
//     be verified says so, with the reason, in a typed status.
//   - The verifier's exchange is an ordinary model request and response in the trajectory, so a
//     layer-1 replay reissues it like any investigator turn. Recordings made before this pass
//     existed carry no verifier exchange and replay unchanged, because a replay walks the recorded
//     records rather than re-running the engine.
//
// A claim is a sentence a model wrote that the report publishes. Today that is the statement of
// each hypothesis the investigator proposed, cited by the evidence its judgments rest on. A
// templated statement or a rationale derived from the ledger is not a claim: the engine composed
// it from typed values, and checking it against digests would test the template, not the model.

// Verification is the outcome of the pass, held on the engine once Stop has run.
type Verification struct {
	// Status is VERIFIED or NOT_VERIFIED. It is never unspecified on a run that stopped.
	Status investigationv1.VerificationStatus
	// Detail is one sentence: the counts, or why the claims were not verified.
	Detail string
	// VerifierModel is the model that ran the fresh-context pass, where one did.
	VerifierModel string
	// Findings is every claim's verdict, the checker's and the verifier's merged.
	Findings []verify.Finding
}

// Verification returns the pass's outcome, once the run has stopped.
func (e *Engine) Verification() *Verification {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.verification == nil {
		return nil
	}
	out := *e.verification
	out.Findings = append([]verify.Finding(nil), e.verification.Findings...)
	return &out
}

// Claims returns the checkable sentences of the run as it stands, in hypothesis-id order.
func (e *Engine) Claims() []verify.Claim {
	e.mu.Lock()
	proposed := make([]string, 0, len(e.modelHypotheses))
	for id := range e.modelHypotheses {
		proposed = append(proposed, id)
	}
	e.mu.Unlock()
	sort.Strings(proposed)

	claims := make([]verify.Claim, 0, len(proposed))
	for _, id := range proposed {
		h, ok := e.ledger.Hypothesis(id)
		if !ok || strings.TrimSpace(h.Statement) == "" {
			continue
		}
		judgments := e.ledger.JudgmentsFor(id)
		if len(judgments) == 0 {
			// Untested: published as a question with what would test it, never as an
			// assertion, so there is nothing for a citation check to hold (plan V2).
			continue
		}
		seen := map[string]struct{}{}
		var cited []string
		for _, j := range judgments {
			if j.EvidenceID == "" {
				continue
			}
			if _, dup := seen[j.EvidenceID]; dup {
				continue
			}
			seen[j.EvidenceID] = struct{}{}
			cited = append(cited, j.EvidenceID)
		}
		sort.Strings(cited)
		claims = append(claims, verify.Claim{
			Ref:         verify.HypothesisClaimRef(id),
			Kind:        verify.KindRanked,
			Text:        h.Statement,
			EvidenceIDs: cited,
		})
	}
	return claims
}

// verifyClaims runs the checker, then the verifier over what the checker kept.
func (e *Engine) verifyClaims(ctx context.Context, stop Stop) Verification {
	if stop.Reason == StopFailed {
		return Verification{
			Status: investigationv1.VerificationStatus_NOT_VERIFIED,
			Detail: "the run failed before its claims could be verified (FR-022c)",
		}
	}

	claims := e.Claims()
	if len(claims) == 0 {
		return Verification{
			Status: investigationv1.VerificationStatus_VERIFIED,
			Detail: "no model-authored claim to verify; every published statement was composed by the engine",
		}
	}

	checked := verify.Check(claims, e.EvidenceForVerification())
	out := Verification{Findings: checked.Findings}
	if len(checked.Kept) == 0 {
		out.Status = investigationv1.VerificationStatus_VERIFIED
		out.Detail = summarise(out.Findings, "the citation checker condemned every claim, so the verifier had nothing to check")
		return out
	}

	if e.verifier == nil {
		out.Status = investigationv1.VerificationStatus_NOT_VERIFIED
		out.Detail = "the citation checker ran; the fresh-context verifier did not: " + e.verifierUnavailable
		return out
	}
	verifierModel, err := e.verifier.Model()
	if err != nil {
		out.Status = investigationv1.VerificationStatus_NOT_VERIFIED
		out.Detail = "the fresh-context verifier has no model: " + err.Error()
		return out
	}
	out.VerifierModel = verifierModel

	request, err := verify.RequestFor(checked.Kept, e.EvidenceForVerification())
	if err != nil {
		out.Status = investigationv1.VerificationStatus_NOT_VERIFIED
		out.Detail = "the verifier's request could not be built: " + err.Error()
		return out
	}
	tokens, err := e.client.CountTokens(ctx, request)
	if err != nil {
		out.Status = investigationv1.VerificationStatus_NOT_VERIFIED
		out.Detail = "the verifier's request could not be priced for admission: " + err.Error()
		return out
	}
	decision := e.budget.Admit(budget.Request{
		Kind:     budget.KindModel,
		ModelID:  verifierModel,
		Tokens:   tokens + model.DefaultMaxTokens,
		Question: "the verification pass over the run's claims (FR-022c)",
	})
	if !decision.Admitted {
		out.Status = investigationv1.VerificationStatus_NOT_VERIFIED
		out.Detail = "the budget did not admit the verifier: " + decision.Budget + " — " + decision.Reason
		return out
	}
	defer decision.Reservation.Release()

	verdicts, resp, err := e.verifier.Verify(ctx, checked.Kept, e.EvidenceForVerification())
	if resp != nil {
		if recErr := e.trajectory.ModelRequest(model.RoleVerifier, resp.Model, resp.RequestBody, resp.RequestDigest); recErr != nil {
			err = joinErr(err, recErr)
		} else if recErr := e.trajectory.ModelResponse(resp); recErr != nil {
			err = joinErr(err, recErr)
		}
		e.budget.RecordModelCall(resp.Model, resp.Usage)
	}
	if err != nil {
		out.Status = investigationv1.VerificationStatus_NOT_VERIFIED
		out.Detail = "the citation checker ran; the fresh-context verifier failed: " + err.Error()
		return out
	}

	out.Findings = merge(checked.Findings, verdicts)
	out.Status = investigationv1.VerificationStatus_VERIFIED
	out.Detail = summarise(out.Findings, "checked by the citation checker and by "+verifierModel)
	return out
}

// merge takes the checker's findings and lets the verifier's replace them claim by claim, except
// where the checker already demoted or removed a claim: a figure the checker could not match stays
// unmatched whatever the model says about the sentence around it.
func merge(checker, verifier []verify.Finding) []verify.Finding {
	byRef := make(map[string]verify.Finding, len(checker)+len(verifier))
	order := make([]string, 0, len(checker))
	for _, f := range checker {
		byRef[f.ClaimRef] = f
		order = append(order, f.ClaimRef)
	}
	for _, f := range verifier {
		previous, ok := byRef[f.ClaimRef]
		if !ok {
			continue // a verdict on a claim that was not sent is not a verdict about this report
		}
		if previous.ActionTaken != verify.ActionKept && f.ActionTaken == verify.ActionKept {
			continue
		}
		byRef[f.ClaimRef] = f
	}
	sort.Strings(order)
	out := make([]verify.Finding, 0, len(order))
	for _, ref := range order {
		out = append(out, byRef[ref])
	}
	return out
}

func summarise(findings []verify.Finding, how string) string {
	var kept, demoted, removed int
	for _, f := range findings {
		switch f.ActionTaken {
		case verify.ActionKept:
			kept++
		case verify.ActionDemoted:
			demoted++
		case verify.ActionRemoved:
			removed++
		}
	}
	return fmt.Sprintf("%d claim(s): %d kept, %d demoted, %d removed; %s",
		len(findings), kept, demoted, removed, how)
}

func joinErr(err, other error) error {
	if err == nil {
		return other
	}
	return fmt.Errorf("%w; %w", err, other)
}
