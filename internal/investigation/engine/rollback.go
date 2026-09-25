// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"fmt"
	"strings"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/investigation/ledger"
)

// A stated rollback is evidence about the deployment it moved away from (004 T155).
//
// ---------------------------------------------------------------------------------------------
// Why this is evidence at all
//
// An operator who rolls production back from deployment X in the middle of an incident has judged,
// with more context than this engine will ever have, that X is the problem. Before T155 the engine saw
// the rollback only as a change that started after onset — a candidate EFFECT, correctly exonerated —
// and let the judgement it records go unused. That is the most informative single fact an incident
// timeline can contain, discarded.
//
// # What is credited, and how strongly
//
// The hypothesis naming X's change gets a `supports` judgment at **Strong**, sourced `rollback`, resting on
// the graph answer that carried both changes. Strong and not Decisive, for the reason a human fact is capped
// the same way: people roll back as a precaution, or roll back the wrong thing under pressure, and one
// action must not end an investigation. The ledger refuses a Decisive rollback outright.
//
// # How X is found — and what is refused
//
// Only from what the platform stated. `rolled_back_from` is spelled the way the same source names its
// rollout of that deployment (graph.proto documents the convention), so X is the candidate carrying an
// alias in one of the rollback's own namespaces whose value is `rolled_back_from`. Nothing is inferred
// from commit order, deploy order or which change looks most recent: a rollback whose source does not
// say what it rolled back from credits nobody.
//
// Two more refusals. The rollback must start after X did — a rollback "away from" a deployment that did
// not exist yet is a stale or mismatched record, not a judgement about it. And the rollback itself is
// never credited: it is the fix, and causal.go's timing rule already types it as an effect.

// CreditRollbacks applies the rule above to one ranked-candidates answer and returns the hypothesis ids it
// credited, in candidate order.
func (e *Engine) CreditRollbacks(diff *graphv1.DiffResponse, evidenceID string) ([]string, error) {
	if diff == nil || evidenceID == "" {
		return nil, nil
	}
	var credited []string
	for _, rollback := range diff.GetChanges() {
		version := rollback.GetChange()
		from := strings.TrimSpace(version.GetChange().GetRolledBackFrom())
		if !version.GetChange().GetRollback() || from == "" {
			continue
		}
		namespaces := map[string]bool{}
		for _, alias := range version.GetAliases() {
			namespaces[alias.GetNamespace()] = true
		}
		for _, candidate := range diff.GetChanges() {
			target := candidate.GetChange()
			if target.GetEntityId() == version.GetEntityId() || !namesDeployment(target, namespaces, from) {
				continue
			}
			if !startsBefore(target, version) {
				continue
			}
			hypothesisID := e.hypothesisFor(target.GetEntityId())
			if hypothesisID == "" || e.alreadyJudged(hypothesisID, evidenceID) {
				continue
			}
			if _, err := e.Judge(hypothesisID, evidenceID, ledger.Supports, ledger.Strong,
				ledger.SourceRollback, ""); err != nil {
				return nil, fmt.Errorf("engine: credit rollback of %s to %s: %w", from, hypothesisID, err)
			}
			credited = append(credited, hypothesisID)
		}
	}
	return credited, nil
}

// namesDeployment reports whether a candidate is the rollout of `deployment` in one of the rollback's own
// namespaces.
func namesDeployment(change *graphv1.NodeVersion, namespaces map[string]bool, deployment string) bool {
	for _, alias := range change.GetAliases() {
		if namespaces[alias.GetNamespace()] && alias.GetValue() == deployment {
			return true
		}
	}
	return false
}

// startsBefore reports whether a's valid start is strictly earlier than b's. A missing start is not
// earlier than anything: the ordering cannot be established, so nothing is credited on it.
func startsBefore(a, b *graphv1.NodeVersion) bool {
	sa, sb := a.GetValid().GetStart(), b.GetValid().GetStart()
	if sa == nil || sb == nil {
		return false
	}
	return sa.AsTime().Before(sb.AsTime())
}

// alreadyJudged reports whether this evidence item already moved this hypothesis. The ledger refuses a
// second judgment on one pair, and the causal step can see the same rollback twice when two answers
// carry it.
func (e *Engine) alreadyJudged(hypothesisID, evidenceID string) bool {
	for _, j := range e.ledger.JudgmentsFor(hypothesisID) {
		if j.EvidenceID == evidenceID {
			return true
		}
	}
	return false
}
