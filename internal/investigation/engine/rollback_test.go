// SPDX-License-Identifier: Apache-2.0

package engine_test

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/investigation/engine"
	"github.com/Pierre-Theophile/aisre/internal/investigation/ledger"
	"github.com/Pierre-Theophile/aisre/internal/query"
)

// A platform-stated rollback away from a change is an operator's judgement about it (004 T155).

const rollbackChange = "k8s.change=shop/payments@rev8-rollback"

// rollbackGraph is the standard incident plus a rollback, after onset, away from the culprit — or, with
// `from` pointing elsewhere, away from something that is not a candidate.
type rollbackGraph struct {
	fakeGraph
	from      string
	namespace string
	at        time.Time
}

func (g *rollbackGraph) Diff(_ context.Context, req *graphv1.DiffRequest) (*graphv1.DiffResponse, error) {
	reference := req.GetT2().AsTime().UTC()
	if req.GetReferenceAt() != nil {
		reference = req.GetReferenceAt().AsTime().UTC()
	}
	params := query.RankParams{Reference: reference, Tau: query.DefaultTau, HopCap: 3}
	culprit := aliased(changeNode(rolloutChange, rolloutAt, graphv1.ChangeKind_ROLLOUT, graphv1.ActorKind_PERSON),
		"k8s.change", "shop/payments@rev7")
	rollback := aliased(changeNode(rollbackChange, g.at, graphv1.ChangeKind_ROLLOUT, graphv1.ActorKind_PERSON),
		g.namespace, "shop/payments@rev8-rollback")
	rollback.Change.Rollback = true
	rollback.Change.RolledBackFrom = g.from
	rollback.Change.RolledBackTo = "shop/payments@rev6"
	ranked, _ := query.Rank([]query.Candidate{
		{Change: culprit, TargetIDs: []string{paymentsRef}, Hop: 1, WeightClass: 1},
		{
			Change:    changeNode(scalingChange, scalingAt, graphv1.ChangeKind_SCALING, graphv1.ActorKind_CONTROLLER),
			TargetIDs: []string{storefrontRef}, Hop: 1, WeightClass: 1,
		},
		{Change: rollback, TargetIDs: []string{paymentsRef}, Hop: 1, WeightClass: 1},
	}, params)
	return &graphv1.DiffResponse{Changes: ranked, RankingFormula: query.RankingFormula(params)}, nil
}

func aliased(v *graphv1.NodeVersion, namespace, value string) *graphv1.NodeVersion {
	v.Aliases = append(v.Aliases, &graphv1.Ref{Namespace: namespace, Value: value})
	return v
}

func rollbackJudgments(l *ledger.Ledger, hypothesisID string) []ledger.Judgment {
	var out []ledger.Judgment
	for _, j := range l.JudgmentsFor(hypothesisID) {
		if j.Source == ledger.SourceRollback {
			out = append(out, j)
		}
	}
	return out
}

func runRollbackIncident(t *testing.T, g *rollbackGraph) *ledger.Ledger {
	t.Helper()
	h := newGraphHarness(t, g)
	stop, err := h.engine.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if stop.Reason != engine.StopCompleted {
		t.Fatalf("stop = %s: %s", stop.Reason, stop.Detail)
	}
	return h.engine.Ledger()
}

func TestARollbackAwayFromTheCulpritSupportsIt(t *testing.T) {
	t.Parallel()
	l := runRollbackIncident(t, &rollbackGraph{
		from: "shop/payments@rev7", namespace: "k8s.change", at: onsetAt.Add(8 * time.Minute),
	})

	culprit := hypothesisFor(t, l, rolloutChange)
	credited := rollbackJudgments(l, culprit.ID)
	if len(credited) != 1 {
		t.Fatalf("the culprit carries %d rollback judgments, want exactly one\nledger:\n%s",
			len(credited), l.Render(ledger.RenderContext{}))
	}
	if j := credited[0]; j.Direction != ledger.Supports || j.Strength != ledger.Strong {
		t.Errorf("the rollback judgment is %s/%s, want supports/strong: an operator's judgement, capped as a "+
			"human fact is", j.Direction, j.Strength)
	}
	if _, ok := l.EvidencedSupport(culprit.ID); !ok {
		t.Error("the rollback does not count as observed support; it is a person's judgement about this " +
			"incident, recorded by the platform")
	}

	// The rollback itself is the fix: credited with nothing, and exonerated by the timing rule.
	fix := hypothesisFor(t, l, rollbackChange)
	if len(rollbackJudgments(l, fix.ID)) != 0 {
		t.Error("the rollback credited itself")
	}
	if fix.Status != ledger.StatusExonerated {
		t.Errorf("the post-onset rollback is %s, want exonerated as a candidate effect", fix.Status)
	}

}

// Nothing is inferred: a rollback that names no candidate, names one in another source's namespace, or
// predates the change it claims to have left credits nobody.
func TestARollbackCreditsOnlyTheDeploymentItStates(t *testing.T) {
	t.Parallel()
	for name, g := range map[string]*rollbackGraph{
		"names no candidate":        {from: "shop/payments@rev5", namespace: "k8s.change", at: onsetAt.Add(8 * time.Minute)},
		"another source's name":     {from: "shop/payments@rev7", namespace: "vercel.change", at: onsetAt.Add(8 * time.Minute)},
		"before the change it left": {from: "shop/payments@rev7", namespace: "k8s.change", at: rolloutAt.Add(-time.Minute)},
		"states nothing":            {from: "", namespace: "k8s.change", at: onsetAt.Add(8 * time.Minute)},
	} {
		l := runRollbackIncident(t, g)
		for _, h := range l.Hypotheses() {
			if got := rollbackJudgments(l, h.ID); len(got) != 0 {
				t.Errorf("%s: %s (%s) was credited by a rollback", name, h.ID, h.CandidateChangeEntityID)
			}
		}
	}
}

// CreditRollbacks is also safe on an answer it has already credited: the ledger refuses a second
// judgment on one (hypothesis, evidence) pair, and the engine does not ask.
func TestCreditingTheSameAnswerTwiceIsANoOp(t *testing.T) {
	t.Parallel()
	g := &rollbackGraph{from: "shop/payments@rev7", namespace: "k8s.change", at: onsetAt.Add(8 * time.Minute)}
	h := newGraphHarness(t, g)
	if _, err := h.engine.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	l := h.engine.Ledger()
	culprit := hypothesisFor(t, l, rolloutChange)
	evidenceID := rollbackJudgments(l, culprit.ID)[0].EvidenceID

	diff, _ := g.Diff(context.Background(), &graphv1.DiffRequest{
		T2: timestamppb.New(firedAt), ReferenceAt: timestamppb.New(onsetAt),
	})
	credited, err := h.engine.CreditRollbacks(diff, evidenceID)
	if err != nil || len(credited) != 0 {
		t.Errorf("a second pass credited %v (err %v); the pair is already judged", credited, err)
	}
}
