// SPDX-License-Identifier: Apache-2.0

package vercel

import (
	"sort"
	"strconv"
	"strings"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// Turning a Vercel deployment into a rollout (004 T082–T089; FR-032, FR-035, FR-037, FR-040, FR-041).
//
// ---------------------------------------------------------------------------------------------
// Ready is not live
//
// The single most important line in this file is the one that refuses. Vercel reports `readyState`
// (`READY` once built) and, separately, `readySubstate` (`STAGED`, `ROLLING`, `PROMOTED`) which is
// documented as tracking whether the deployment has seen production traffic. `READY` means built and
// available. It does not mean serving.
//
// So a ROLLOUT is `target=production` AND `readySubstate=PROMOTED`, and nothing else. Reading `READY`
// as live would make every staged deployment a change node claiming production moved when it had not —
// the confident-wrong-answer class this project keeps finding, and the one an investigation punishes
// hardest, because a staged build sitting at the top of a ranked cause list is a plausible lie.
//
// `isRollbackCandidate` is refused for the same reason and is recorded as a property instead: it says a
// deployment CAN be rolled back to, not that one happened (research §5.2).
//
// # The promotion instant, and what is done when Vercel does not state one
//
// Valid time is when the world changed, so for a rollout it is the PROMOTION instant — not the build's.
// Research §5.2 established that `readySubstate=PROMOTED` is a state and not a timestamp: the API
// states `createdAt`, `buildingAt` and `ready`, and `ready` is when the build finished.
//
// Where no promotion instant is stated the valid start is marked UNKNOWN rather than filled with
// `ready`. That is FR-041 again: `ready` is a real instant of a real event, which makes it exactly the
// wrong thing to substitute — a reader cannot tell a promotion dated from its build from one dated from
// its promotion, and the error is silent and in the direction of "this rollout happened earlier than it
// did", which moves it up a causal ranking.
//
// The build instant is kept as a property, so nothing is lost by refusing to misuse it.

// Rollout is one deployment's mapped form, or a refusal with the reason.
type Rollout struct {
	// Change is the ROLLOUT event, or nil when this deployment is not one.
	Change *graphv1.EventEnvelope
	// Correlations are the cross-source keys C8 joins on, empty when the deployment states no commit.
	Correlations []*graphv1.EventEnvelope
	// Excluded is the reason this deployment produced no change, empty when it produced one. It is a
	// value rather than a log line because FR-032 and SC-002 ask for the exclusions to be COUNTED.
	Excluded string
}

// Exclusion reasons, as a closed set so a caller can count them per cycle without parsing prose.
const (
	// ExcludedPreview is a deployment with no production target: a preview build.
	ExcludedPreview = "preview"
	// ExcludedNotPromoted is `target=production` that has not taken production traffic. The case
	// T083 exists for.
	ExcludedNotPromoted = "not-promoted"
	// ExcludedUnusable is a payload that names no deployment, so nothing can be said about it.
	ExcludedUnusable = "unusable"
	// ExcludedRollbackNotCompleted is a rollback the platform states but whose job has not succeeded:
	// pending, in progress, failed or skipped. Production did not move, so there is no change yet.
	ExcludedRollbackNotCompleted = "rollback-not-completed"
)

// Mapper turns deployments into graph events.
type Mapper struct {
	desc feeder.Description
	// ProjectTargets maps a Vercel project id to the entity its rollouts change, in the graph's own
	// vocabulary. It is the operator's, for the reason internal/feeders/github/targets.go sets out at
	// length: the platform has no name for the thing that runs in production, and a mapping read from
	// the observed system would let that system choose what its changes attach to.
	ProjectTargets map[string]*graphv1.Ref
	// Repositories maps a project id to the connected repository, where the platform reported one.
	// Carried as a PROPERTY of the change; see ProjectLink for why it is not a claim.
	Repositories map[string]string
}

// NewMapper builds a mapper for one source.
func NewMapper(desc feeder.Description) *Mapper {
	return &Mapper{
		desc:           desc,
		ProjectTargets: map[string]*graphv1.Ref{},
		Repositories:   map[string]string{},
	}
}

// MapDeployment turns one deployment into a rollout, or states why it is not one.
func (m *Mapper) MapDeployment(d Deployment, at time.Time) (Rollout, error) {
	uid := strings.TrimSpace(d.UID)
	if uid == "" {
		return Rollout{Excluded: ExcludedUnusable}, nil
	}
	if !strings.EqualFold(strings.TrimSpace(d.Target), "production") {
		return Rollout{Excluded: ExcludedPreview}, nil
	}
	if !strings.EqualFold(strings.TrimSpace(d.ReadySubstate), "PROMOTED") {
		// READY and STAGED lands here, which is the whole point of reading the substate.
		return Rollout{Excluded: ExcludedNotPromoted}, nil
	}

	props := feeder.NewProps().
		Str(feeder.AttrDeploymentEnvironment, "production").
		Str("sre.vercel.project_id", d.ProjectID).
		Str("sre.vercel.ready_substate", strings.ToUpper(strings.TrimSpace(d.ReadySubstate))).
		Str("sre.vercel.source", strings.TrimSpace(d.Source)).
		Bool("sre.vercel.rollback_candidate", d.IsRollbackCandidate).
		Strs("sre.vercel.actor_evidence", ActorEvidence(d)...)
	// The repository, as a property. Several deployments ship from one repository, so it describes
	// this change rather than naming it — see ProjectLink.
	if repo := strings.TrimSpace(m.Repositories[d.ProjectID]); repo != "" {
		props = props.Str("sre.vercel.repository", repo)
	}
	// The build instant, kept so that refusing to date the change from it loses nothing.
	if built, ok := millis(d.ReadyMillis); ok {
		props = props.Str("sre.vercel.built_at", built.Format(time.RFC3339))
	}
	built, err := props.Build()
	if err != nil {
		return Rollout{}, err
	}

	fact := feeder.ChangeFact{
		Meta:      feeder.Meta{SourceObservedAt: at},
		Ref:       feeder.Ref(feeder.NSVercelChange, uid),
		Kind:      graphv1.ChangeKind_ROLLOUT,
		Summary:   m.summary(d),
		ActorKind: ActorKind(d),
		OriginRef: strings.TrimSpace(d.InspectorURL),
		Props:     built,
	}
	fact.Targets = m.targets(d.ProjectID)
	// The promotion instant, or an unknown start. Never the build's.
	if promoted, ok := m.promotedAt(d); ok {
		fact.ValidAt = promoted
	} else {
		fact.ValidFromUnknown = true
	}

	change := feeder.ObserveChange(m.desc, m.id("change", uid), fact)
	return Rollout{Change: change, Correlations: m.correlations(change, d, at)}, nil
}

// targets is what a change to one project changes: the project itself, and the operator's target for
// it where the mapping names one (004 T093).
//
// The project comes first and always. Vercel is the platform that SERVES the project, so unlike a
// deploy pipeline it does have a name for the thing that runs in production — the project — and this
// feeder describes it (MapProject). Until T093 the change targeted only the operator's ref, which no
// source described, so the change was permanently unattached and C8, which reads targets from the
// graph, could never find a target it shared with another source's observation of the same rollout.
//
// The operator's target is kept beside it rather than replacing it. It says which entity ELSEWHERE in
// the graph this project's rollouts change — typically the OpenTelemetry service a telemetry feeder
// describes — and a rollout that attaches there too is one an investigation starting from that
// service's alert finds.
func (m *Mapper) targets(projectID string) []*graphv1.Ref {
	var out []*graphv1.Ref
	if project, ok := feeder.VercelProjectRef(projectID); ok {
		out = append(out, project)
	}
	if target, ok := m.ProjectTargets[projectID]; ok && target != nil {
		duplicate := false
		for _, have := range out {
			if have.GetNamespace() == target.GetNamespace() && have.GetValue() == target.GetValue() {
				duplicate = true
			}
		}
		if !duplicate {
			out = append(out, target)
		}
	}
	return out
}

// MapProject describes one project as a SERVICE node (004 T093), or reports false when the project
// names no usable id.
//
// # Why the project, and why SERVICE
//
// A project is what Vercel builds, promotes and serves: every deployment belongs to exactly one, a
// promotion moves the project's production traffic, and its id survives a rename. It is the Vercel
// analogue of a Cloud Run service, which the GCP feeder describes as SERVICE, and it is the entity a
// Vercel rollout changes. A domain or an alias was the other candidate and is the wrong one: several
// point at one project, they are reassigned independently of deployments, and an incident on "the web
// app" is an incident on the project whichever hostname it was reached through.
//
// # When it began
//
// `createdAt`, where the platform states it. Otherwise the start is UNKNOWN and dated from the read, as
// FR-011 requires — a project that already existed when this feeder first listed it did not begin then.
func (m *Mapper) MapProject(p Project, at time.Time) (*graphv1.EventEnvelope, bool, error) {
	ref, ok := feeder.VercelProjectRef(p.ID)
	if !ok {
		return nil, false, nil
	}
	name := strings.TrimSpace(p.Name)
	if name == "" {
		name = ref.GetValue()
	}
	props := feeder.NewProps().Str("sre.vercel.project_name", name)
	if p.Link.Org != "" && p.Link.Repo != "" {
		props = props.Str("sre.vercel.repository", p.Link.Org+"/"+p.Link.Repo)
	}
	built, err := props.Build()
	if err != nil {
		return nil, false, err
	}
	fact := feeder.NodeFact{
		Meta:        feeder.Meta{SourceObservedAt: at},
		Ref:         ref,
		Type:        graphv1.NodeType_SERVICE,
		DisplayName: name,
		Props:       built,
		Pointers: []*graphv1.Pointer{feeder.SourceLinkPointer(Kind, feeder.VocabVercelResource,
			"v9/projects/"+ref.GetValue(), nil)},
	}
	if created, stated := millis(p.CreatedAt); stated {
		fact.ValidAt = created
	} else {
		fact.ValidFromUnknown = true
	}
	return feeder.UpsertNode(m.desc, m.id("project", ref.GetValue()), fact), true, nil
}

// MapAliasRequest turns a project's last alias request into a ROLLBACK, when the platform states one
// (004 T117–T121, FR-016).
//
// # Flagged because the platform said so, and only then
//
// `rollback` is set from `lastAliasRequest.type == "rollback"` and from nothing else. Not from a redeploy
// of an older commit, not from `isRollbackCandidate`, not from a deployment being older than the one it
// replaced, not from a revert-shaped commit message (FR-016, SC-009). Every one of those is an inference,
// and a wrong rollback flag is the expensive kind of wrong: it tells an investigation that someone already
// stopped the bleeding.
//
// A `promote` is not a rollback, whatever it promotes. Promoting an older deployment is what a rollback
// LOOKS like, and reading it as one is the inference the rule above forbids; the platform has a word for
// rollback and did not use it.
//
// # Only when production moved
//
// A job that is pending, in progress, failed or skipped moved nothing, so it is counted as an exclusion
// rather than emitted. A later read that finds it succeeded emits it then, under the same ref.
//
// # When
//
// `requestedAt`, the platform's instant for the request. It is the rollback's own event — unlike the
// build instant a promotion must not be dated from — and an instant rollback is an alias swap, so the
// request and the move are the same moment as far as anything published says. It is recorded as a
// property as well, so a reader can see where the instant came from. With no `requestedAt` the start is
// unknown and dated from the read (FR-011).
//
// # Identity
//
// The ref carries the project, the restored deployment and the request instant, so re-reading the same
// request is the same change (the log answers DUPLICATE_NOOP), and a second rollback to the same deployment
// later is a second change.
func (m *Mapper) MapAliasRequest(p Project, at time.Time) (Rollout, error) {
	req := p.LastAliasRequest
	if req == nil || !strings.EqualFold(strings.TrimSpace(req.Type), "rollback") {
		return Rollout{}, nil
	}
	to := strings.TrimSpace(req.ToDeploymentID)
	project, ok := feeder.VercelProjectRef(p.ID)
	if to == "" || !ok {
		return Rollout{Excluded: ExcludedUnusable}, nil
	}
	if !strings.EqualFold(strings.TrimSpace(req.JobStatus), "succeeded") {
		return Rollout{Excluded: ExcludedRollbackNotCompleted}, nil
	}

	props := feeder.NewProps().
		Str(feeder.AttrDeploymentEnvironment, "production").
		Str("sre.vercel.project_id", project.GetValue()).
		Str("sre.vercel.alias_request_type", "rollback").
		Str("sre.vercel.alias_job_status", "succeeded")
	requested, stated := millis(req.RequestedAt)
	if stated {
		props = props.Str("sre.vercel.alias_requested_at", requested.Format(time.RFC3339))
	}
	built, err := props.Build()
	if err != nil {
		return Rollout{}, err
	}

	name := strings.TrimSpace(p.Name)
	if name == "" {
		name = project.GetValue()
	}
	value := "rollback/" + project.GetValue() + "/" + to + "@" + strconv.FormatInt(req.RequestedAt, 10)
	fact := feeder.ChangeFact{
		Meta:         feeder.Meta{SourceObservedAt: at},
		Ref:          feeder.Ref(feeder.NSVercelChange, value),
		Kind:         graphv1.ChangeKind_ROLLOUT,
		Summary:      "rolled back " + name + " to " + to,
		Rollback:     true,
		RolledBackTo: to,
		// Spelled as this feeder names its rollout of that deployment (`vercel.change=dpl_…`), which is
		// the convention that lets the engine credit the hypothesis naming it (004 T155).
		RolledBackFrom: strings.TrimSpace(req.FromDeploymentID),
		Props:          built,
		Targets:        m.targets(project.GetValue()),
	}
	if stated {
		fact.ValidAt = requested
	} else {
		fact.ValidFromUnknown = true
	}
	return Rollout{Change: feeder.ObserveChange(m.desc, m.id("rollback", value), fact)}, nil
}

// promotedAt is the instant the deployment became production, and whether Vercel stated one.
//
// Today the API states no promotion timestamp (research §5.2), so this reports false for every real
// payload and the change carries an unknown valid start. It is a function rather than a constant
// `false` because the shape of the answer is the contract: when Vercel adds the field, this is the one
// place that changes, and every caller already handles both branches.
func (m *Mapper) promotedAt(d Deployment) (time.Time, bool) {
	for _, stated := range []string{
		strings.TrimSpace(d.Meta["promotedAt"]),
		strings.TrimSpace(d.Attribution.CommitMeta["promotedAt"]),
	} {
		if stated == "" {
			continue
		}
		if parsed, err := time.Parse(time.RFC3339, stated); err == nil {
			return parsed.UTC(), true
		}
	}
	return time.Time{}, false
}

// correlations are the cross-source keys this rollout carries (T086).
//
// Exactly one today: the commit, normalised by `feeder.CommitSHA` — the SAME normaliser the GitHub
// feeder uses, which is what makes a Vercel promotion and a GitHub deployment of one commit joinable
// by C8 (SC-017). An abbreviated sha is refused rather than padded, because a 7-hex prefix is shared
// across an organisation's repositories and merging on it would join rollouts of different code.
func (m *Mapper) correlations(change *graphv1.EventEnvelope, d Deployment, at time.Time) []*graphv1.EventEnvelope {
	commit, source, ok := commitOf(d)
	if !ok {
		return nil
	}
	attrs, err := feeder.NewProps().
		Str(feeder.AttrDeploymentEnvironment, "production").
		Str("sre.vercel.correlation_source", source).
		Build()
	if err != nil {
		return nil
	}
	subject := change.GetObserveChange().GetRef()
	return []*graphv1.EventEnvelope{feeder.Correlate(m.desc,
		feeder.NewID(m.desc.SourceID, "correlation", subject.GetValue(), feeder.NSDeployCommitSHA, commit),
		feeder.CorrelationFact{
			Meta:       feeder.Meta{SourceObservedAt: at},
			Subject:    subject,
			Key:        feeder.Ref(feeder.NSDeployCommitSHA, commit),
			Attributes: attrs,
		})}
}

// commitOf reads the commit from the two places Vercel states it, preferring `attribution.commitMeta`
// as the newer and more explicit of the two, and says which one it came from.
//
// The keys are tried in a fixed order rather than by ranging a map, so the same payload always yields
// the same evidence string and a golden does not depend on Go's map iteration.
func commitOf(d Deployment) (commit, source string, ok bool) {
	type candidate struct{ where, key string }
	for _, c := range []candidate{
		{"attribution.commitMeta", "githubCommitSha"},
		{"attribution.commitMeta", "gitlabCommitSha"},
		{"attribution.commitMeta", "bitbucketCommitSha"},
		{"attribution.commitMeta", "commitSha"},
		{"meta", "githubCommitSha"},
		{"meta", "gitlabCommitSha"},
		{"meta", "bitbucketCommitSha"},
		{"meta", "commitSha"},
	} {
		var stated string
		if c.where == "meta" {
			stated = d.Meta[c.key]
		} else {
			stated = d.Attribution.CommitMeta[c.key]
		}
		if value, valid := feeder.CommitSHA(stated); valid {
			return value, c.where + "." + c.key, true
		}
	}
	return "", "", false
}

// summary is what an operator reads on the change node.
func (m *Mapper) summary(d Deployment) string {
	name := strings.TrimSpace(d.Name)
	if name == "" {
		name = strings.TrimSpace(d.ProjectID)
	}
	if commit, _, ok := commitOf(d); ok {
		return "promoted " + name + " to production (" + commit[:7] + ")"
	}
	return "promoted " + name + " to production"
}

// id is this feeder's deterministic event id.
func (m *Mapper) id(kind string, parts ...string) string {
	return feeder.NewID(m.desc.SourceID, append([]string{kind}, parts...)...)
}

// CountExclusions tallies a cycle's refusals by reason, which is what FR-032 and SC-002 ask to be
// reported rather than merely logged. Sorted, so a report reads the same twice.
func CountExclusions(rollouts []Rollout) []ExclusionCount {
	tally := map[string]int{}
	for _, r := range rollouts {
		if r.Excluded != "" {
			tally[r.Excluded]++
		}
	}
	out := make([]ExclusionCount, 0, len(tally))
	for reason, n := range tally {
		out = append(out, ExclusionCount{Reason: reason, Count: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Reason < out[j].Reason })
	return out
}

// ExclusionCount is one reason and how often it fired this cycle.
type ExclusionCount struct {
	Reason string
	Count  int
}
