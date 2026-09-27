// SPDX-License-Identifier: Apache-2.0

package datadog_test

import (
	"context"
	"os"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
)

// datadog-errors-by-version-01: what each version group names (005 T049; FR-040b–FR-040d).
//
// The same `checkout` twin as datadog-backend-logs-01 — the old commit, the new commit whose payment
// error starts at 14:10, the crash-handler lines with no stamp — plus a batch worker built with
// `git rev-parse --short`, whose lines carry an abbreviated sha. The world is errors_by_version over
// the window around the deploy and the drill-downs and exemplars behind its groups. It is the golden
// for the rule every backend applies: a full commit names `deploy.commit_sha`; an abbreviated one names
// nothing and says ABBREVIATED_SHA, because the full form cannot be recovered and guessing it would join
// the errors to the wrong change; lines without a stamp are a named group of their own.

const (
	ebvFixture     = "fixtures/datadog-errors-by-version-01"
	ebvShortCommit = "4c2e9ab"
)

func ebvLines() []twinLine {
	lines := backendLines()
	for m := backendDeploy.Add(-20 * time.Minute); m.Before(backendTo); m = m.Add(2 * time.Minute) {
		lines = append(lines, twinLine{at: m.Add(30 * time.Second), msg: "batch settled 12 invoices",
			status: "info", host: "worker-1", version: ebvShortCommit})
		if m.Minute()%10 == 0 {
			lines = append(lines, twinLine{at: m.Add(35 * time.Second), msg: "batch retry scheduled",
				status: "error", host: "worker-1", version: ebvShortCommit})
		}
	}
	return lines
}

func ebvTerms() []*engine.Term {
	return []*engine.Term{engine.ErrorsByVersion(backendLogPointer(),
		engine.NewWindow(backendDeploy.Add(-20*time.Minute), backendTo), "version")}
}

func TestGenerateDatadogErrorsByVersionFixture(t *testing.T) {
	if os.Getenv(genFixturesEnv) == "" {
		t.Skipf("set %s=1 to regenerate %s", genFixturesEnv, ebvFixture)
	}
	generateBackendFixture(t, ebvFixture, ebvLines(), ebvTerms(), graphHalf{
		family: "datadog-backend",
		description: "errors_by_version's groups and what each names. `checkout` stamps the deployed " +
			"commit on its logs; a new commit ships at 14:10 and a payment error starts; a crash handler " +
			"writes errors with no stamp; and a batch worker built with `git rev-parse --short` stamps an " +
			"abbreviated sha. world/ is errors_by_version over the window around the deploy, and the " +
			"drill-downs and exemplars behind its groups, followed once. Each full commit's group names " +
			"deploy.commit_sha; the abbreviated group names nothing and says ABBREVIATED_SHA, because its " +
			"full form cannot be recovered; the unstamped lines are a group of their own, " +
			"NOT_A_STABLE_IDENTIFIER. internal/backends/datadog asserts all of it against the world on every " +
			"run, and that the world answers as the live twin does. Synthetic structural twin: no " +
			"identifier is derived from the organisation.",
	})
}

// Every group names its deploy reference or the reason it has none, read off the recorded world, and
// the world answers as the live twin does.
func TestEachVersionGroupNamesItsDeployReference(t *testing.T) {
	t.Parallel()
	recorded := backendRecorded(t, ebvFixture)
	live, _ := backendLive(t, ebvLines())
	liveReqs, liveResps := crossProduct(t, live, ebvTerms())
	_, recordedResps := crossProduct(t, recorded, ebvTerms())
	if len(recordedResps) != len(liveResps) {
		t.Fatalf("%d recorded answers, %d live", len(recordedResps), len(liveResps))
	}
	for i := range liveResps {
		if liveResps[i].GetResponseDigest() != recordedResps[i].GetResponseDigest() ||
			!proto.Equal(liveResps[i].GetDigest(), recordedResps[i].GetDigest()) {
			t.Errorf("%d %s: recorded differs from live", i, engine.TermName(liveReqs[i].GetTerm()))
		}
	}

	resp, err := recorded.Execute(context.Background(), &engine.Request{Term: ebvTerms()[0]})
	if err != nil {
		t.Fatal(err)
	}
	groups := map[string]*investigationv1.VersionBreakdown{}
	for _, row := range resp.GetDigest().GetErrorsByVersion().GetVersions() {
		groups[row.GetVersion()] = row
	}
	for _, commit := range []string{backendOldCommit, backendNewCommit} {
		ref := groups[commit].GetDeployRef()
		if ref.GetNamespace() != "deploy.commit_sha" || ref.GetValue() != commit {
			t.Errorf("commit %s: deploy_ref %v", commit, ref)
		}
	}
	if short := groups[ebvShortCommit]; short.GetDeployRef() != nil ||
		short.GetDeployRefAbsentReason() != investigationv1.DeployRefAbsentReason_ABBREVIATED_SHA {
		t.Errorf("the abbreviated group: %v", short)
	}
	if missing := groups[""]; missing.GetTotal() == 0 ||
		missing.GetDeployRefAbsentReason() != investigationv1.DeployRefAbsentReason_NOT_A_STABLE_IDENTIFIER {
		t.Errorf("the unstamped group: %v", missing)
	}
	if len(groups) != 4 {
		t.Errorf("%d groups, want 4: %v", len(groups), groups)
	}
}
