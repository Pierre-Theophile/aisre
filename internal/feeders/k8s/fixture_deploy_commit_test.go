// SPDX-License-Identifier: Apache-2.0

package k8s_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	githubfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/github"
	k8sfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/k8s"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/emit"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/record"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/source"
)

// The Kubernetes cross-source fixture: a deploy pipeline and a cluster observe one rollout (004 T149).
//
// ---------------------------------------------------------------------------------------------
// Why this fixture exists
//
// Until T149, C8 could not fire for a Kubernetes rollout at all: GitHub states a commit and no image,
// the cluster stated an image and no commit, so the pipeline's deployment an operator maps onto
// `k8s.deployment` had nothing to agree with. T149 lets the operator trust pod-template keys for the
// commit. This fixture is where that is measured rather than argued, the way `deploy-cross-source-
// merge-01` measures it for Cloud Run and Vercel.
//
// # What happens
//
//  1. The cluster lists `payments` at revision 7, its pod template stating commit OLD under the
//     operator-trusted `commit-sha` key. ADDED is not a change, so nothing merges yet.
//  2. The pipeline's deploy bumps the template to commit NEW: revision 8, a rollout carrying
//     `deploy.commit_sha=NEW`, because the operator trusts that key and this rollout changed it.
//  3. Someone restarts the workload: revision 9, a rollout whose template still states NEW. It did not
//     ship NEW — revision 8 did — so it carries NO commit key (rolloutCommit's "only when it changed").
//  4. The pipeline's own poll reports deployment 6001 of NEW to `prod`, mapped by the operator onto
//     `k8s.deployment=shop/payments`.
//
// C8 must merge (4) with (2): one commit, one target, one environment. It must NOT merge (4) with (3),
// which shares the target and the environment and would share the commit too if the feeder minted the
// unchanged value — which is the whole reason it does not.

const (
	k8sCommitFixtureDir = "fixtures/deploy-k8s-commit-merge-01"
	k8sCommitOld        = "af5bd3c0b0e4a1d2c3f4a5b6c7d8e9f0a1b2c3d4"
	k8sCommitNew        = "9f8e7d6c5b4a39281706f5e4d3c2b1a098765432"
	k8sCommitRepo       = "payments"
	k8sCommitRepoID     = 777
	k8sCommitDeployment = 6001
	k8sCommitTrustedKey = "commit-sha"
)

var (
	k8sCommitStart     = time.Date(2026, 9, 21, 14, 0, 0, 0, time.UTC)
	k8sCommitRolloutAt = k8sCommitStart.Add(10 * time.Minute)
	k8sCommitRestartAt = k8sCommitStart.Add(30 * time.Minute)
	k8sCommitPollAt    = k8sCommitStart.Add(40 * time.Minute)
	k8sCommitEnd       = k8sCommitStart.Add(time.Hour)
	k8sCommitPinnedAt  = k8sCommitStart.Add(6 * time.Hour)
)

// The two change refs the audits and the ground truth name. Spelled out rather than derived from the
// feeders' own id functions, so the ground truth is a statement about the graph rather than an echo of
// the code under test.
const (
	k8sCommitGitHubChange  = "repositories/777/deployments/6001/targets/k8s.deployment/shop/payments"
	k8sCommitRolloutChange = "shop/payments@rev8"
	k8sCommitRestartChange = "shop/payments@rev9"
)

func TestGenerateDeployK8sCommitFixture(t *testing.T) {
	if os.Getenv("SRE_AGENT_GEN_FIXTURES") == "" {
		t.Skip("set SRE_AGENT_GEN_FIXTURES=1 to regenerate the Kubernetes cross-source fixture")
	}
	dir := filepath.Join(k8sRepoRoot(t), k8sCommitFixtureDir)
	// Only what a generator writes is cleared, so golden/ survives a regeneration (004 T079, T153).
	for _, generated := range []string{"payloads", "events.jsonl", "rejected.jsonl", "manifest.yaml"} {
		if err := os.RemoveAll(filepath.Join(dir, generated)); err != nil {
			t.Fatalf("clear %s: %v", generated, err)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create %s: %v", dir, err)
	}

	clock := &k8sArrivalClock{}
	k8sDesc := runK8sCommitHalf(t, dir, clock)
	githubDesc := runGitHubCommitHalf(t, dir, clock)

	if err := record.WriteManifest(dir, record.Manifest{
		Family: "deploy-cross-source",
		Description: "One rollout of a Kubernetes workload seen by the deploy pipeline and by the " +
			"cluster, joined on the commit the pod template states under a key the operator " +
			"trusts (004 T149). The rollout that shipped the commit merges with the pipeline's " +
			"deployment under C8; the restart that followed it states the same commit and did " +
			"not ship it, so it carries no commit key and stays its own change. Synthetic " +
			"structural twin: no identifier is derived from any organisation " +
			"(contracts/sanitisation.md §7).",
		Sources: []record.ManifestSource{record.SourceOf(k8sDesc), record.SourceOf(githubDesc)},
		Start:   k8sCommitStart,
		End:     k8sCommitEnd,
	}); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	appendK8sCommitQueries(t, dir)
}

// runK8sCommitHalf is the cluster: the list, the rollout that shipped NEW, and the restart.
func runK8sCommitHalf(t *testing.T, dir string, clock *k8sArrivalClock) feeder.Description {
	t.Helper()
	listed := commitDeployment("1003", "7", k8sCommitOld, "ghcr.io/twin/payments:v7", time.Time{}, "")
	rolled := commitDeployment("1050", "8", k8sCommitNew, "ghcr.io/twin/payments:v8", k8sCommitRolloutAt, "")
	restarted := commitDeployment("1080", "9", k8sCommitNew, "ghcr.io/twin/payments:v8", k8sCommitRestartAt,
		k8sCommitRestartAt.Format(time.RFC3339))

	ns := shopNamespace()
	payloads := []feeder.Payload{
		payloadOf(t, k8sfeeder.KindNamespaces, k8sfeeder.EventAdded, ns, ns.ResourceVersion, k8sCommitStart),
		payloadOf(t, k8sfeeder.KindDeployments, k8sfeeder.EventAdded, listed, listed.ResourceVersion, k8sCommitStart),
		{Kind: k8sfeeder.KindSync, At: k8sCommitStart.Add(time.Minute), Seq: 1100},
		payloadOf(t, k8sfeeder.KindDeployments, k8sfeeder.EventModified, rolled, rolled.ResourceVersion, k8sCommitRolloutAt),
		payloadOf(t, k8sfeeder.KindDeployments, k8sfeeder.EventModified, restarted, restarted.ResourceVersion, k8sCommitRestartAt),
	}

	opts := fixtureOptions()
	opts.CommitLabels = []string{k8sCommitTrustedKey}
	f, err := k8sfeeder.New(opts, nil)
	if err != nil {
		t.Fatalf("new k8s feeder: %v", err)
	}
	return recordHalf(t, "k8s half", dir, clock, f.Describe(), payloads, f.Run)
}

// runGitHubCommitHalf is the pipeline: one deployment of NEW to prod, mapped onto the workload.
func runGitHubCommitHalf(t *testing.T, dir string, clock *k8sArrivalClock) feeder.Description {
	t.Helper()
	at := k8sCommitPollAt
	payloads := []feeder.Payload{
		{Kind: githubfeeder.PayloadInstallationRepositories, At: at, Bytes: []byte(fmt.Sprintf(
			`{"total_count":1,"repository_selection":"selected","repositories":[`+
				`{"id":%d,"name":%q,"full_name":"acme/%s","private":true,"owner":{"login":"acme"}}]}`,
			k8sCommitRepoID, k8sCommitRepo, k8sCommitRepo))},
		{Kind: githubfeeder.PayloadDeployments, At: at, Bytes: []byte(fmt.Sprintf(
			`[{"id":%d,"sha":%q,"ref":"main","task":"deploy","environment":"prod",`+
				`"production_environment":true,`+
				`"created_at":"2026-09-21T14:08:00Z","updated_at":"2026-09-21T14:09:30Z",`+
				`"creator":{"login":"release-runner","id":7,"type":"Bot"},`+
				`"url":"https://api.github.com/repos/acme/%s/deployments/%d",`+
				`"statuses_url":"https://api.github.com/repos/acme/%s/deployments/%d/statuses"}]`,
			k8sCommitDeployment, k8sCommitNew, k8sCommitRepo, k8sCommitDeployment, k8sCommitRepo, k8sCommitDeployment))},
		{Kind: githubfeeder.PayloadDeploymentStatuses, At: at, Bytes: []byte(fmt.Sprintf(
			`{"repository":"acme/%s","deployment_id":%d,"statuses":[`+
				`{"id":%d,"state":"success","environment":"prod","created_at":"2026-09-21T14:09:30Z",`+
				`"creator":{"login":"release-runner","id":7,"type":"Bot"}}]}`,
			k8sCommitRepo, k8sCommitDeployment, k8sCommitDeployment*10))},
		{Kind: githubfeeder.PayloadPollMarker, At: at, Bytes: []byte(`{"outcome":"complete"}`)},
	}

	f, err := githubfeeder.New(githubfeeder.Options{
		OrgSlug: "twin",
		Map: githubfeeder.MapOptions{
			// `prod`, because that is what the cluster's namespace declares: C8 requires the environment
			// to AGREE, and a pipeline environment spelled differently from the cluster's is two
			// environments as far as a certain rule is concerned.
			Allowlist: githubfeeder.Allowlist{Environments: []string{"prod"}},
			Targets: githubfeeder.TargetMap{Repositories: map[string][]githubfeeder.TargetRule{
				"acme/" + k8sCommitRepo: {{
					Environment: "prod",
					Namespace:   feeder.NSK8sDeployment,
					Value:       "shop/payments",
				}},
			}},
			Actors: githubfeeder.ActorPolicy{DeploymentAutomation: []string{"release-runner"}},
		},
	})
	if err != nil {
		t.Fatalf("new github feeder: %v", err)
	}
	return recordHalf(t, "github half", dir, clock, f.Describe(), payloads, f.Run)
}

// commitDeployment is payments with the commit on its pod template — the only place rolloutCommit reads
// it from — and, for a rollout, the Progressing condition that dates it.
func commitDeployment(rv, revision, commit, image string, progressing time.Time, restartedAt string) *appsv1.Deployment {
	d := paymentsDeployment()
	d.ResourceVersion = rv
	d.Annotations[k8sfeeder.AnnotationRevision] = revision
	// The builder shares one label map between the workload and its template; this fixture writes the
	// commit on the template only, so the template gets its own copy.
	template := map[string]string{}
	for k, v := range d.Spec.Template.Labels {
		template[k] = v
	}
	template[k8sCommitTrustedKey] = commit
	d.Spec.Template.Labels = template
	d.Spec.Template.Spec.Containers[0].Image = image
	if restartedAt != "" {
		d.Spec.Template.Annotations["kubectl.kubernetes.io/restartedAt"] = restartedAt
	}
	if !progressing.IsZero() {
		d.Status.Conditions = []appsv1.DeploymentCondition{{
			Type:           appsv1.DeploymentProgressing,
			Status:         corev1.ConditionTrue,
			LastUpdateTime: metav1.NewTime(progressing),
		}}
	}
	return d
}

func recordHalf(t *testing.T, half, dir string, clock *k8sArrivalClock, desc feeder.Description,
	payloads []feeder.Payload, run func(context.Context, feeder.Source, feeder.Emitter) error) feeder.Description {
	t.Helper()
	src := record.Wrap(clock.wrap(source.NewSliceSource(payloads)), dir)
	memory := emit.NewMemoryEmitter(desc, emit.WithMemoryClock(clock.Now))
	events := record.Emitter(memory, dir)
	if err := run(t.Context(), src, events); err != nil {
		t.Fatalf("%s: run: %v", half, err)
	}
	if err := src.Err(); err != nil {
		t.Fatalf("%s: record payloads: %v", half, err)
	}
	if err := events.Err(); err != nil {
		t.Fatalf("%s: record events: %v", half, err)
	}
	if rejected := memory.Rejected(); len(rejected) > 0 {
		t.Fatalf("%s: %d events were refused; the first is %s (%s)",
			half, len(rejected), rejected[0].GetEventId(), rejected[0].GetReasonDetail())
	}
	if events.Accepted() == 0 {
		t.Fatalf("%s: recorded no events", half)
	}
	return desc
}

func appendK8sCommitQueries(t *testing.T, dir string) {
	t.Helper()
	pinned := k8sCommitPinnedAt.Format(time.RFC3339)
	queries := `queries:
  # The merge T149 exists for: the pipeline's deployment of NEW and the cluster's rollout that shipped
  # it, joined on the commit the pod template states under a trusted key. Its golden records rule C8
  # and the correlation keys it stood on.
  - name: why-the-pipeline-deploy-is-the-cluster-rollout
    kind: audit
    ref_a: github.change=` + k8sCommitGitHubChange + `
    ref_b: k8s.change=` + k8sCommitRolloutChange + `
    observed_at: ` + pinned + `
  # The RESTART. Same target, same environment, and its template states the same commit — but it did
  # not ship it, so the feeder minted no commit key and C8 has nothing to merge on.
  - name: why-the-restart-is-not-the-deploy
    kind: audit
    ref_a: github.change=` + k8sCommitGitHubChange + `
    ref_b: k8s.change=` + k8sCommitRestartChange + `
    observed_at: ` + pinned + `
  # What an investigation asks: what changed on payments in the hour? TWO rollouts: the deploy, carrying
  # the cluster's instant and revision and the pipeline's actor, because C8 merged the two observations;
  # and the restart, on its own.
  - name: what-changed-on-payments
    kind: diff
    focus: k8s.deployment=shop/payments
    t1: ` + k8sCommitStart.Format(time.RFC3339) + `
    t2: ` + k8sCommitEnd.Format(time.RFC3339) + `
    reference_at: ` + k8sCommitEnd.Format(time.RFC3339) + `
    observed_at: ` + pinned + `
    hops: 1
    direction: both

ground_truth:
  cross_source_pairs:
    - pair:
        - github.change=` + k8sCommitGitHubChange + `
        - k8s.change=` + k8sCommitRolloutChange + `
      same: true
      rule: C8
  distinct_pairs:
    # The restart: everything C8 compares would agree if the unchanged commit were minted.
    - pair:
        - github.change=` + k8sCommitGitHubChange + `
        - k8s.change=` + k8sCommitRestartChange + `
      same: false
    # And the two cluster rollouts of one workload are two rollouts.
    - pair:
        - k8s.change=` + k8sCommitRolloutChange + `
        - k8s.change=` + k8sCommitRestartChange + `
      same: false
`
	path := filepath.Join(dir, "manifest.yaml")
	existing, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := os.WriteFile(path, append(existing, []byte("\n"+queries)...), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// k8sArrivalClock hands out observed instants: each payload's arrival, then a microsecond per event,
// never going backwards. One clock across both halves, because observed time is monotonic over the
// fixture, not per source.
type k8sArrivalClock struct {
	base time.Time
	step int64
	last time.Time
}

func (c *k8sArrivalClock) Now() time.Time {
	at := c.base.Add(time.Duration(c.step) * time.Microsecond)
	c.step++
	if !c.last.IsZero() && !at.After(c.last) {
		at = c.last.Add(time.Microsecond)
	}
	c.last = at
	return at
}

func (c *k8sArrivalClock) wrap(src feeder.Source) feeder.Source { return k8sClockedSource{src, c} }

type k8sClockedSource struct {
	src   feeder.Source
	clock *k8sArrivalClock
}

func (s k8sClockedSource) Next(ctx context.Context) (feeder.Payload, error) {
	payload, err := s.src.Next(ctx)
	if err == nil && !payload.At.IsZero() {
		s.clock.base, s.clock.step = payload.At, 0
	}
	return payload, err
}

func k8sRepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the repository root")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "..")
}
