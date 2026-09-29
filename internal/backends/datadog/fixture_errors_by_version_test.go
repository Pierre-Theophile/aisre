// SPDX-License-Identifier: Apache-2.0

package datadog_test

import (
	"context"
	"os"
	"strings"
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
	window := engine.NewWindow(backendDeploy.Add(-20*time.Minute), backendTo)
	return []*engine.Term{
		engine.ErrorsByVersion(backendLogPointer(), window, "version"),
		// The same question over an attribute the aggregate refuses to group by (research §5 O1): the
		// answer comes from the newest lines, grouped here.
		engine.ErrorsByVersion(backendLogPointer(), window, ungroupableFacet),
	}
}

func TestGenerateDatadogErrorsByVersionFixture(t *testing.T) {
	if os.Getenv(genFixturesEnv) == "" {
		t.Skipf("set %s=1 to regenerate %s", genFixturesEnv, ebvFixture)
	}
	generateBackendFixtureRefusing(t, ebvFixture, ebvLines(), ebvTerms(), graphHalf{
		family: "datadog-backend",
		description: "errors_by_version's groups and what each names. `checkout` stamps the deployed " +
			"commit on its logs; a new commit ships at 14:10 and a payment error starts; a crash handler " +
			"writes errors with no stamp; and a batch worker built with `git rev-parse --short` stamps an " +
			"abbreviated sha. world/ is errors_by_version over the window around the deploy, and the " +
			"drill-downs and exemplars behind its groups, followed once. Each full commit's group names " +
			"deploy.commit_sha; the abbreviated group names nothing and says ABBREVIATED_SHA, because its " +
			"full form cannot be recovered; the unstamped lines are a group of their own, " +
			"NOT_A_STABLE_IDENTIFIER. A second term asks the same question over an attribute the aggregate " +
			"API refuses to group by (a 400, as for an attribute Datadog cannot group), and is answered " +
			"from the newest lines grouped client-side, its coverage stating the sample. " +
			"internal/backends/datadog asserts all of it against the world on every " +
			"run, and that the world answers as the live twin does. Synthetic structural twin: no " +
			"identifier is derived from the organisation.",
	}, ungroupableFacet)
}

// Every group names its deploy reference or the reason it has none, read off the recorded world, and
// the world answers as the live twin does.
func TestEachVersionGroupNamesItsDeployReference(t *testing.T) {
	t.Parallel()
	recorded := backendRecorded(t, ebvFixture)
	live, _ := backendLiveRefusing(t, ebvLines(), ungroupableFacet)
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

// Over the attribute the aggregate refuses, the recorded answer is the sampled one: the same groups
// and the same deploy references as the aggregated answer, with coverage saying they were read from
// lines and grouped here rather than counted by Datadog (contract §3.1).
func TestTheSampledFallbackIsRecordedAndSaysSo(t *testing.T) {
	t.Parallel()
	recorded := backendRecorded(t, ebvFixture)
	terms := ebvTerms()
	exact, err := recorded.Execute(context.Background(), &engine.Request{Term: terms[0]})
	if err != nil {
		t.Fatal(err)
	}
	sampled, err := recorded.Execute(context.Background(), &engine.Request{Term: terms[1]})
	if err != nil {
		t.Fatal(err)
	}
	if sampled.GetOutcome() != investigationv1.TermOutcome_DIGEST {
		t.Fatalf("outcome %v / %v", sampled.GetOutcome(), sampled.GetFailureReason())
	}
	cov := sampled.GetDigest().GetCoverage()
	if cov.GetSampling() == "none" || !strings.Contains(cov.GetSampling(), "would not group by "+ungroupableFacet) ||
		cov.GetVolumeConsidered() == 0 {
		t.Errorf("coverage does not state the sample: %d lines, %q", cov.GetVolumeConsidered(), cov.GetSampling())
	}
	if exact.GetDigest().GetCoverage().GetSampling() != "none" {
		t.Errorf("the aggregated answer states a sample: %q", exact.GetDigest().GetCoverage().GetSampling())
	}
	want, got := exact.GetDigest().GetErrorsByVersion().GetVersions(), sampled.GetDigest().GetErrorsByVersion().GetVersions()
	if len(got) != len(want) {
		t.Fatalf("%d sampled groups, %d aggregated", len(got), len(want))
	}
	for i := range want {
		if got[i].GetVersion() != want[i].GetVersion() || got[i].GetErrors() != want[i].GetErrors() ||
			got[i].GetTotal() != want[i].GetTotal() ||
			!proto.Equal(got[i].GetDeployRef(), want[i].GetDeployRef()) ||
			got[i].GetDeployRefAbsentReason() != want[i].GetDeployRefAbsentReason() {
			t.Errorf("group %d: sampled %v, aggregated %v", i, got[i], want[i])
		}
	}
}
