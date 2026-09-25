// SPDX-License-Identifier: Apache-2.0

package vercel_test

import (
	"strings"
	"testing"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	vercelfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/vercel"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// Mapping a Vercel deployment to a rollout (004 T082–T089).

const (
	mapCommit  = "9f8e7d6c5b4a39281706f5e4d3c2b1a098765432"
	mapProject = "prj_storefront"
)

var mapAt = time.Date(2026, 9, 21, 14, 30, 0, 0, time.UTC)

func mapper(t *testing.T) *vercelfeeder.Mapper {
	t.Helper()
	m := vercelfeeder.NewMapper(feeder.Description{SourceID: "vercel:twin"})
	m.ProjectTargets[mapProject] = feeder.Ref(feeder.NSK8sDeployment, "shop/storefront")
	m.Repositories[mapProject] = "acme/storefront"
	return m
}

func promoted() vercelfeeder.Deployment {
	return vercelfeeder.Deployment{
		UID: "dpl_42", Name: "storefront", ProjectID: mapProject,
		Target: "production", ReadyState: "READY", ReadySubstate: "PROMOTED",
		Source: "git", InspectorURL: "https://vercel.com/twin/storefront/dpl_42",
		Creator:     vercelfeeder.DeploymentCreator{UID: "usr_1", Type: "user"},
		Attribution: vercelfeeder.DeploymentAttribution{CommitMeta: map[string]string{"githubCommitSha": mapCommit}},
		ReadyMillis: mapAt.Add(-10 * time.Minute).UnixMilli(),
	}
}

func mapOne(t *testing.T, d vercelfeeder.Deployment) vercelfeeder.Rollout {
	t.Helper()
	got, err := mapper(t).MapDeployment(d, mapAt)
	if err != nil {
		t.Fatalf("MapDeployment: %v", err)
	}
	return got
}

// The refusal T083 names as the plausible wrong implementation.
//
// `READY` means built and available. Reading it as live would make every staged deployment a change
// node claiming production moved when it had not — and a staged build at the top of a ranked cause
// list is a plausible lie, which is worse than no answer.
func TestAReadyButStagedDeploymentIsNotARollout(t *testing.T) {
	t.Parallel()
	for _, substate := range []string{"STAGED", "ROLLING", "", "staged"} {
		d := promoted()
		d.ReadySubstate = substate
		got := mapOne(t, d)
		if got.Change != nil {
			t.Errorf("readySubstate=%q produced a ROLLOUT; READY is built and available, not serving",
				substate)
		}
		if got.Excluded != vercelfeeder.ExcludedNotPromoted {
			t.Errorf("readySubstate=%q was excluded as %q, want %q — the reason is counted, so it has "+
				"to be the right one", substate, got.Excluded, vercelfeeder.ExcludedNotPromoted)
		}
	}
	// And ROLLING is genuinely refused rather than accidentally: it is mid-shift, not promoted.
	if got := mapOne(t, promoted()); got.Change == nil {
		t.Fatal("PROMOTED produced no rollout, so the test above proves nothing")
	}
}

// A preview deployment is excluded by the platform-stated target, not by a heuristic (T085, FR-032).
func TestAPreviewDeploymentIsExcludedAndCounted(t *testing.T) {
	t.Parallel()
	for _, target := range []string{"", "staging", "preview"} {
		d := promoted()
		d.Target = target
		got := mapOne(t, d)
		if got.Change != nil {
			t.Errorf("target=%q produced a ROLLOUT", target)
		}
		if got.Excluded != vercelfeeder.ExcludedPreview {
			t.Errorf("target=%q excluded as %q, want %q", target, got.Excluded, vercelfeeder.ExcludedPreview)
		}
	}
}

// The exclusions are COUNTED, which is what SC-002 asks for — a log line nobody aggregates is not a
// measurement.
func TestExclusionsAreCountedByReason(t *testing.T) {
	t.Parallel()
	preview, staged := promoted(), promoted()
	preview.Target = ""
	staged.ReadySubstate = "STAGED"
	rollouts := []vercelfeeder.Rollout{
		mapOne(t, promoted()), mapOne(t, preview), mapOne(t, preview), mapOne(t, staged),
	}
	counts := vercelfeeder.CountExclusions(rollouts)
	want := map[string]int{vercelfeeder.ExcludedPreview: 2, vercelfeeder.ExcludedNotPromoted: 1}
	if len(counts) != len(want) {
		t.Fatalf("got %d reasons, want %d: %+v", len(counts), len(want), counts)
	}
	for _, c := range counts {
		if want[c.Reason] != c.Count {
			t.Errorf("%s counted %d, want %d", c.Reason, c.Count, want[c.Reason])
		}
	}
}

// Valid time is the PROMOTION instant, and where Vercel states none the start is UNKNOWN rather than
// the build's (T084, FR-041).
//
// Substituting `ready` would be silent and would err in the direction of "this rollout happened earlier
// than it did", which moves it up a causal ranking. The build instant is kept as a property, so
// refusing to misuse it loses nothing.
func TestAnUnstatedPromotionInstantIsUnknownRatherThanTheBuildInstant(t *testing.T) {
	t.Parallel()
	d := promoted()
	got := mapOne(t, d)
	body := got.Change.GetObserveChange()
	if !body.GetValidFromUnknown() {
		t.Error("the valid start is not marked unknown, so it was filled from something")
	}
	built, _ := time.Parse(time.RFC3339, body.GetProps().GetFields()["sre.vercel.built_at"].GetStringValue())
	if body.GetValidAt() != nil && body.GetValidAt().AsTime().Equal(built) {
		t.Error("the change is dated from its BUILD instant; a reader cannot then tell a promotion " +
			"dated from its build from one dated from its promotion")
	}
	if body.GetProps().GetFields()["sre.vercel.built_at"].GetStringValue() == "" {
		t.Error("the build instant was dropped rather than kept as a property; refusing to date the " +
			"change from it should lose nothing")
	}
}

// And when a promotion instant IS stated, it is used.
func TestAStatedPromotionInstantDatesTheChange(t *testing.T) {
	t.Parallel()
	want := mapAt.Add(-3 * time.Minute)
	d := promoted()
	d.Meta = map[string]string{"promotedAt": want.Format(time.RFC3339)}
	body := mapOne(t, d).Change.GetObserveChange()
	if body.GetValidFromUnknown() {
		t.Fatal("a stated promotion instant was ignored and the start marked unknown")
	}
	if !body.GetValidAt().AsTime().Equal(want) {
		t.Errorf("validAt = %s, want the promotion instant %s", body.GetValidAt().AsTime(), want)
	}
}

// The commit is minted through the SAME normaliser GitHub uses, which is what makes C8 able to join a
// Vercel promotion to a GitHub deployment of one commit (T086, SC-017).
func TestTheCommitIsByteIdenticalToGitHubs(t *testing.T) {
	t.Parallel()
	got := mapOne(t, promoted())
	if len(got.Correlations) != 1 {
		t.Fatalf("got %d correlations, want the commit", len(got.Correlations))
	}
	key := got.Correlations[0].GetCorrelateEntity().GetKey()
	if key.GetNamespace() != feeder.NSDeployCommitSHA {
		t.Errorf("namespace = %q, want %q", key.GetNamespace(), feeder.NSDeployCommitSHA)
	}
	shared, ok := feeder.CommitSHA(mapCommit)
	if !ok {
		t.Fatal("the shared normaliser refused the test commit; this test checks the wrong input")
	}
	if key.GetValue() != shared {
		t.Errorf("value = %q, the shared normaliser says %q; the two sources would not join",
			key.GetValue(), shared)
	}
	// The subject is the CHANGE: C8 compares two change observations.
	if s := feeder.RefString(got.Correlations[0].GetCorrelateEntity().GetSubject()); s != "vercel.change=dpl_42" {
		t.Errorf("subject = %q, want the rollout change", s)
	}
}

// An abbreviated sha is refused rather than padded: a 7-hex prefix is shared across an organisation's
// repositories, and merging on it would join rollouts of different code.
func TestAnAbbreviatedCommitMintsNoKey(t *testing.T) {
	t.Parallel()
	for _, sha := range []string{"9f8e7d6", "", "not-a-sha", mapCommit[:39]} {
		d := promoted()
		d.Attribution = vercelfeeder.DeploymentAttribution{CommitMeta: map[string]string{"githubCommitSha": sha}}
		got := mapOne(t, d)
		if got.Change == nil {
			t.Fatalf("sha %q stopped the rollout being a rollout; only the KEY should be refused", sha)
		}
		if len(got.Correlations) != 0 {
			t.Errorf("sha %q minted %q; an abbreviation is shared across repositories", sha,
				got.Correlations[0].GetCorrelateEntity().GetKey().GetValue())
		}
	}
}

// The repository rides as a PROPERTY, not as a claim or a correlation key (T087, resolving T148).
func TestTheRepositoryIsAPropertyAndNotAClaim(t *testing.T) {
	t.Parallel()
	got := mapOne(t, promoted())
	props := got.Change.GetObserveChange().GetProps().GetFields()
	if props["sre.vercel.repository"].GetStringValue() != "acme/storefront" {
		t.Errorf("the repository is not carried as a property: %v", props["sre.vercel.repository"])
	}
	for _, c := range got.Correlations {
		if ns := c.GetCorrelateEntity().GetKey().GetNamespace(); ns == "github.repo" {
			t.Error("the repository was minted as a correlation key; T148 established it is neither a " +
				"name for one entity nor a registered correlation namespace")
		}
	}
	if got.Change.GetObserveChange().GetRef().GetNamespace() == "github.repo" {
		t.Error("the change is addressed by the repository, which would mint an entity per repository")
	}
}

// The actor kind comes from `creator.type` and `source`, never from a login — which is not even
// decoded (T088, FR-037).
func TestActorKindComesFromTheAccountTypeAndTrigger(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		accountType string
		source      string
		want        graphv1.ActorKind
	}{
		{"an app is automation", "app", "git", graphv1.ActorKind_AUTOMATION},
		{"a bot is automation", "bot", "cli", graphv1.ActorKind_AUTOMATION},
		{"a deploy hook is automation", "user", "git-deploy-hook", graphv1.ActorKind_AUTOMATION},
		{"a redeploy is automation", "user", "redeploy", graphv1.ActorKind_AUTOMATION},
		{"a user pushing is a person", "user", "git", graphv1.ActorKind_PERSON},
		{"a user on the cli is a person", "user", "cli", graphv1.ActorKind_PERSON},
		{"an untyped account is unknown", "", "git", graphv1.ActorKind_ACTOR_KIND_UNKNOWN},
		{"nothing acted", "", "", graphv1.ActorKind_ACTOR_KIND_UNSPECIFIED},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := promoted()
			d.Creator = vercelfeeder.DeploymentCreator{Type: tc.accountType}
			d.Source = tc.source
			if got := vercelfeeder.ActorKind(d); got != tc.want {
				t.Errorf("ActorKind = %v, want %v", got, tc.want)
			}
		})
	}
}

// And the evidence is recorded on the change, without the login.
func TestTheActorEvidenceIsRecordedAndNamesNoLogin(t *testing.T) {
	t.Parallel()
	got := mapOne(t, promoted())
	evidence := got.Change.GetObserveChange().GetProps().GetFields()["sre.vercel.actor_evidence"].GetListValue()
	if evidence == nil || len(evidence.GetValues()) == 0 {
		t.Fatal("no actor evidence on the change; a verdict a reader cannot check is not an audit trail")
	}
	for _, v := range evidence.GetValues() {
		s := v.GetStringValue()
		if s == "" {
			continue
		}
		for _, forbidden := range []string{"username", "githubLogin", "login"} {
			if strings.Contains(s, forbidden) {
				t.Errorf("the evidence names %q (%q); a line naming the login invites the next reader "+
					"to draw a conclusion from it", forbidden, s)
			}
		}
	}
}

// The origin reference is the inspector URL and carries no credential (T089, FR-014, FR-040).
func TestTheOriginReferenceIsTheInspectorURLAndCarriesNoCredential(t *testing.T) {
	t.Parallel()
	got := mapOne(t, promoted())
	origin := got.Change.GetObserveChange().GetChange().GetOriginRef()
	if origin != "https://vercel.com/twin/storefront/dpl_42" {
		t.Errorf("originRef = %q, want the inspectorUrl", origin)
	}
	for _, secretish := range []string{"token", "key=", "secret", "Bearer", "?access"} {
		if strings.Contains(origin, secretish) {
			t.Errorf("the origin reference carries %q: %s", secretish, origin)
		}
	}
}

// The change states when the SOURCE saw it, which is what makes its valid time replay-stable (004).
//
// A Vercel promotion carries no promotion instant, so the change's valid start is marked unknown and the
// projector dates it from the observation. There are two candidates for "the observation" and only one
// is stable: the ingest instant is assigned per event as events are applied, so two events delivered in
// one arrival window get different ones depending on the order they arrived in.
//
// This connector emits a change and its correlation key together on EVERY promotion, so that window
// always holds two events. Without this field the rollout's valid interval shifts by a microsecond under
// permutation, which `vercel-promotion-01`'s shuffle step failed on before the field was set.
//
// It is asserted here rather than left to the fixture because a fixture 59 directories away is a slow
// and indirect way to learn that a struct field went missing.
func TestTheChangeStatesWhenTheSourceSawIt(t *testing.T) {
	t.Parallel()
	got := mapOne(t, promoted())
	stated := got.Change.GetSourceObservedAt()
	if stated == nil {
		t.Fatal("the change states no source instant, so the projector dates its unknown start from " +
			"the ingest instant — which depends on the order events arrived in (FR-021)")
	}
	if !stated.AsTime().Equal(mapAt) {
		t.Errorf("source instant = %s, want the poll's %s", stated.AsTime(), mapAt)
	}
	// The correlation states it too, for the same reason.
	if len(got.Correlations) > 0 {
		if c := got.Correlations[0].GetSourceObservedAt(); c == nil || !c.AsTime().Equal(mapAt) {
			t.Errorf("the correlation's source instant is %v, want %s", c, mapAt)
		}
	}
}

// A project is described as a SERVICE node dated from its creation where the platform states it, and a
// project with no usable id describes nothing (004 T093).
func TestAProjectIsAServiceDatedFromItsCreation(t *testing.T) {
	t.Parallel()
	m := vercelfeeder.NewMapper(feeder.Description{SourceID: "vercel:twin"})
	created := time.Date(2025, 3, 1, 9, 0, 0, 0, time.UTC)
	env, ok, err := m.MapProject(vercelfeeder.Project{
		ID: mapProject, Name: "storefront", CreatedAt: created.UnixMilli(),
		Link: vercelfeeder.ProjectLink{Org: "acme", Repo: "storefront"},
	}, mapAt)
	if err != nil || !ok {
		t.Fatalf("MapProject: ok=%v err=%v", ok, err)
	}
	node := env.GetUpsertNode()
	if node.GetType() != graphv1.NodeType_SERVICE || node.GetRef().GetValue() != mapProject {
		t.Fatalf("got %v, want the project as a SERVICE addressed by its id", node)
	}
	if node.GetValidFromUnknown() || !node.GetValidAt().AsTime().Equal(created) {
		t.Errorf("valid start %v (unknown=%v); the platform stated createdAt %v and that is when the project "+
			"began", node.GetValidAt().AsTime(), node.GetValidFromUnknown(), created)
	}
	if repo := node.GetProps().GetFields()["sre.vercel.repository"].GetStringValue(); repo != "acme/storefront" {
		t.Errorf("repository = %q, want the connected repository", repo)
	}
	if len(node.GetPointers()) != 1 || node.GetPointers()[0].GetSelector() != "v9/projects/"+mapProject {
		t.Errorf("pointers = %v, want the project's API path", node.GetPointers())
	}

	if _, ok, err := m.MapProject(vercelfeeder.Project{Name: "nameless"}, mapAt); ok || err != nil {
		t.Errorf("a project with no id was described (ok=%v, err=%v); a name is not stable across a rename", ok, err)
	}
}
