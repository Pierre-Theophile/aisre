// SPDX-License-Identifier: Apache-2.0

package vercel_test

import (
	"testing"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	vercelfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/vercel"
)

// A rollback is flagged because the platform said so (004 T117–T121, FR-016, SC-009).
//
// Vercel states it on the project's `lastAliasRequest`; nothing else in either platform's published
// surface does. Each case below is a way a rollback flag could be wrong, and a wrong one tells an
// investigation that somebody already stopped the bleeding.

var rollbackRequested = time.Date(2026, 9, 21, 14, 40, 0, 0, time.UTC)

func projectWithAliasRequest(req *vercelfeeder.AliasRequest) vercelfeeder.Project {
	return vercelfeeder.Project{ID: mapProject, Name: "storefront", LastAliasRequest: req}
}

func succeededRollback() *vercelfeeder.AliasRequest {
	return &vercelfeeder.AliasRequest{
		Type: "rollback", JobStatus: "succeeded",
		FromDeploymentID: "dpl_bad", ToDeploymentID: "dpl_good",
		RequestedAt: rollbackRequested.UnixMilli(),
	}
}

func TestAStatedRollbackIsARolloutFlaggedAsOne(t *testing.T) {
	t.Parallel()
	rollout, err := mapper(t).MapAliasRequest(projectWithAliasRequest(succeededRollback()), mapAt)
	if err != nil || rollout.Change == nil {
		t.Fatalf("no change for a succeeded, platform-stated rollback (err=%v, excluded=%q)", err, rollout.Excluded)
	}
	body := rollout.Change.GetObserveChange()
	change := body.GetChange()
	if !change.GetRollback() || change.GetRolledBackTo() != "dpl_good" {
		t.Errorf("rollback=%v rolled_back_to=%q, want true and the deployment production was moved to",
			change.GetRollback(), change.GetRolledBackTo())
	}
	if change.GetKind() != graphv1.ChangeKind_ROLLOUT {
		t.Errorf("kind = %v; a rollback is a ROLLOUT in every other respect, marked rather than kinded", change.GetKind())
	}
	if body.GetValidFromUnknown() || !body.GetValidAt().AsTime().Equal(rollbackRequested) {
		t.Errorf("valid at %v (unknown=%v), want the platform's requestedAt %v",
			body.GetValidAt().AsTime(), body.GetValidFromUnknown(), rollbackRequested)
	}
	if targets := body.GetTargets(); len(targets) == 0 || targets[0].GetNamespace() != "vercel.project" ||
		targets[0].GetValue() != mapProject {
		t.Errorf("targets = %v, want the project first", targets)
	}
	if from := change.GetRolledBackFrom(); from != "dpl_bad" {
		t.Errorf("rolled_back_from = %q, want the deployment production was moved away from, spelled as "+
			"this feeder names its rollout of it", from)
	}
}

// Only a succeeded job moved production. Everything else is counted and emits nothing.
func TestARollbackThatHasNotSucceededIsNotAChange(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"pending", "in-progress", "failed", "skipped"} {
		req := succeededRollback()
		req.JobStatus = status
		rollout, err := mapper(t).MapAliasRequest(projectWithAliasRequest(req), mapAt)
		if err != nil {
			t.Fatalf("%s: %v", status, err)
		}
		if rollout.Change != nil || rollout.Excluded != vercelfeeder.ExcludedRollbackNotCompleted {
			t.Errorf("%s: change=%v excluded=%q; a %s rollback moved nothing", status,
				rollout.Change != nil, rollout.Excluded, status)
		}
	}
}

// A promotion is not a rollback, even of an older deployment: the platform has a word for rollback and
// did not use it (FR-016). And a project with no alias request states nothing at all.
func TestAPromotionIsNeverReadAsARollback(t *testing.T) {
	t.Parallel()
	promote := succeededRollback()
	promote.Type = "promote"
	for name, project := range map[string]vercelfeeder.Project{
		"a promotion":      projectWithAliasRequest(promote),
		"no alias request": projectWithAliasRequest(nil),
		"an unknown type":  projectWithAliasRequest(&vercelfeeder.AliasRequest{Type: "revert", JobStatus: "succeeded", ToDeploymentID: "dpl_good"}),
	} {
		rollout, err := mapper(t).MapAliasRequest(project, mapAt)
		if err != nil || rollout.Change != nil || rollout.Excluded != "" {
			t.Errorf("%s: change=%v excluded=%q err=%v; nothing here is a stated rollback",
				name, rollout.Change != nil, rollout.Excluded, err)
		}
	}
}

// A deployment that CAN be rolled back to, or that redeploys an older commit, is an ordinary rollout
// (T119, T120): candidacy is not history, and an older commit is not a rollback the platform stated.
func TestCandidacyAndRedeploysAreNotRollbacks(t *testing.T) {
	t.Parallel()
	redeploy := promoted()
	redeploy.Source = "redeploy"
	redeploy.IsRollbackCandidate = true
	rollout, err := mapper(t).MapDeployment(redeploy, mapAt)
	if err != nil || rollout.Change == nil {
		t.Fatalf("the redeploy is a promoted production deployment and should be a rollout: %v", err)
	}
	if change := rollout.Change.GetObserveChange().GetChange(); change.GetRollback() || change.GetRolledBackTo() != "" {
		t.Errorf("a redeploy that is a rollback CANDIDATE was flagged rollback=%v to %q; neither the "+
			"candidacy nor the redeploy is the platform stating a rollback", change.GetRollback(), change.GetRolledBackTo())
	}
}

// Without requestedAt the start is unknown and dated from the read, never invented (FR-011).
func TestARollbackWithNoRequestInstantHasAnUnknownStart(t *testing.T) {
	t.Parallel()
	req := succeededRollback()
	req.RequestedAt = 0
	rollout, err := mapper(t).MapAliasRequest(projectWithAliasRequest(req), mapAt)
	if err != nil || rollout.Change == nil {
		t.Fatalf("no change: %v", err)
	}
	if body := rollout.Change.GetObserveChange(); !body.GetValidFromUnknown() || body.GetValidAt() != nil {
		t.Errorf("valid_at=%v unknown=%v; with no stated instant the start is unknown", body.GetValidAt(), body.GetValidFromUnknown())
	}
}
