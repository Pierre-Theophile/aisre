// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"fmt"
	"sort"
	"strings"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The two rollouts (T053–T056, FR-016, FR-017, FR-018, contracts/gcp-feeder.md §3.2 and §3.3).
//
// A Cloud Run deploy is **two** changes, and conflating them is the single most consequential error
// available in this feature:
//
//   - a **revision was created** — valid at `Revision.createTime`, and it did not move production
//     traffic;
//   - the **traffic split changed** — valid at the instant the new split took effect, and this is
//     the change whose instant is the instant what production served changed.
//
// They are minutes apart in a canary and hours apart in a held rollout. An investigation asking
// "what changed just before onset" gets the wrong answer from either instant used for both: the
// creation instant blames a revision that was not yet serving, and the shift instant hides a
// revision that was created broken and crashed on startup.
//
// # The instant the split took effect
//
// Research §2 establishes an awkward fact: **the Cloud Run v2 API has no field that stamps it.**
// What exists is a convergence *predicate* — `reconciling`, `traffic` vs `trafficStatuses`,
// `observedGeneration` vs `generation` — and `Service.updateTime`, which moves for any change to the
// resource and is therefore not a traffic-shift timestamp.
//
// So the instant comes from the Cloud Audit Logs. `UpdateService` is a long-running operation and
// writes two entries correlated by `LogEntry.operation.id`: a request entry with `operation.first`
// and a completion entry with `operation.last`. **The `timestamp` of the `operation.last` entry is
// the published definition of "the instant the split took effect"** (contract §3.2), which is what
// makes SC-002's "the instant GCP states" a defined quantity rather than an argument during review.
//
// Three consequences, all contract:
//
//  1. the poll detects *that* the split changed; the audit log supplies *when* and *who*. A split
//     with no completion entry yet is **held**, not emitted with a guessed instant;
//  2. `Service.updateTime` is never a change instant;
//  3. `Condition.type` values beyond `Ready` are not in the public contract and are not relied on.

// Properties that distinguish the two rollouts. The contract requires them to be distinguishable by
// a published property rather than by a summary string a reader has to parse.
const (
	// PropMovedTraffic says whether this change moved production traffic. False on a creation,
	// true on a shift. It is the property feature 002's ranking reads, and it is a boolean rather
	// than an inference from the change's targets because "did what production served change" is
	// the question, and a creation change targets the service too.
	PropMovedTraffic = "sre.gcp.moved_production_traffic"
	// PropRolloutKind names which of the two this is, for a reader and for a query.
	PropRolloutKind = "sre.gcp.rollout_kind"
	// PropSplitBefore and PropSplitAfter are the split either side of a shift. Both are recorded
	// because "traffic moved" is not a useful fact without them, and a rollback is only
	// recognisable as a rollback from the pair.
	PropSplitBefore = "sre.gcp.traffic_split_before"
	PropSplitAfter  = "sre.gcp.traffic_split_after"
	// PropOperationID is the audit `operation.id` that correlated the request and completion
	// entries. It is the evidence for the instant, so it is recorded with it (constitution V).
	PropOperationID = "sre.gcp.operation_id"
	// PropAuditMethod is the audit `methodName` that produced the change.
	PropAuditMethod = "sre.gcp.audit_method"
	// PropActorRung names which rung of the actor ladder decided the kind (actorkind.go).
	PropActorRung = "sre.gcp.actor_rung"
	// PropActorEvidence is what the ladder had to work with. It carries no principal address.
	PropActorEvidence = "sre.gcp.actor_evidence"
)

// The two published rollout kinds.
const (
	// RolloutRevisionCreated is a revision coming into existence. It did not move traffic.
	RolloutRevisionCreated = "revision_created"
	// RolloutTrafficShift is the split changing. It did.
	RolloutTrafficShift = "traffic_shift"
)

// ChangeRefRevisionCreated is the deterministic ref of a revision-creation change: the revision and
// the instant it was created.
//
// Both halves are needed. The revision alone would collide with nothing today, but a revision name
// is reused when a service is deleted and recreated (FR-025) — and that case must be two changes,
// because it was two revisions. The instant is rendered in UTC, RFC 3339 to the nanosecond, so the
// same creation read twice produces one ref (FR-076).
func ChangeRefRevisionCreated(rev Revision, at time.Time) *graphv1.Ref {
	return feeder.Ref(NSChange, "rollout-created/"+rev.Value()+"@"+instant(at))
}

// ChangeRefTrafficShift is the deterministic ref of a traffic-shift change: the service and the
// instant the split took effect.
//
// It is keyed on the *service*, not the revision, and that is FR-017 read carefully: what changed is
// what the service serves. A rollback that moves traffic back to the old revision is a third change
// at its own instant, not a deletion of the second — and keying on the service plus the instant is
// what makes that fall out rather than needing a rule.
func ChangeRefTrafficShift(svc Service, at time.Time) *graphv1.Ref {
	return feeder.Ref(NSChange, "rollout-traffic/"+svc.Value()+"@"+instant(at))
}

// instant renders a change instant canonically: UTC, RFC 3339 with nanoseconds. A ref built from a
// local-zone time would differ from the same instant read in another zone, and the two would be two
// changes.
func instant(at time.Time) string { return at.UTC().Format(time.RFC3339Nano) }

// RevisionCreated builds the creation change (T053, FR-016).
//
// The valid instant is `Revision.createTime`, which is output-only and documented — so `unknown` is
// **not** accepted here, and a zero instant is an error rather than a ValidFromUnknown. That
// asymmetry with the node assertion is deliberate: a node may legitimately have an unknown start (it
// existed before the feeder looked), and a *change* with an unknown instant is not a change, it is a
// claim that something happened at no particular time.
//
// The actor comes from the correlated audit entry for the service mutation, because there is **no
// `CreateRevision` methodName in v2** (research §2) — revisions are created as a side effect of
// `CreateService`/`UpdateService`. So the instant comes from the API and the principal comes from the
// log, which is why this function takes both.
func RevisionCreated(rev Revision, createTime time.Time, actor Actor, operationID, auditMethod string) (feeder.ChangeFact, error) {
	if createTime.IsZero() {
		return feeder.ChangeFact{}, fmt.Errorf("gcp: revision %s has no createTime; it is output-only "+
			"and documented, so a missing one is an error rather than an unknown instant "+
			"(contracts/gcp-feeder.md §3.3)", rev.Value())
	}
	if err := rev.Validate(); err != nil {
		return feeder.ChangeFact{}, err
	}
	return feeder.ChangeFact{
		Ref:       ChangeRefRevisionCreated(rev, createTime),
		Kind:      graphv1.ChangeKind_ROLLOUT,
		Summary:   "revision " + rev.Revision + " created for " + rev.Name + " (no production traffic moved)",
		ActorKind: actor.Kind,
		Targets: []*graphv1.Ref{
			rev.Service.Ref(),
			rev.Ref(),
		},
		ValidAt: createTime,
	}, nil
}

// RevisionCreatedProps renders the creation change's properties.
func RevisionCreatedProps(rev Revision, actor Actor, operationID, auditMethod string) *feeder.Props {
	props := feeder.NewProps().
		Str(PropRolloutKind, RolloutRevisionCreated).
		// False, always, and recorded rather than omitted. An absent property reads as "we did not
		// say"; FR-016 requires the creation change to be *marked* as not having moved traffic,
		// which is a positive statement.
		Bool(PropMovedTraffic, false).
		Str(PropProject, rev.Project).
		Str(PropRegion, rev.Region).
		Str(PropRevisionOf, rev.Service.Value()).
		Str(PropActorRung, actor.Rung)
	if operationID != "" {
		props = props.Str(PropOperationID, operationID)
	}
	if auditMethod != "" {
		props = props.Str(PropAuditMethod, auditMethod)
	}
	if len(actor.Evidence) > 0 {
		props = props.Strs(PropActorEvidence, actor.Evidence...)
	}
	return props
}

// TrafficShift builds the traffic-shift change (T054, FR-017).
//
// `at` is the `operation.last = true` completion entry's `timestamp` and nothing else. The signature
// takes it as a plain instant, so this function cannot reach for `Service.updateTime` — the
// prohibition is enforced by what is not in scope rather than by a comment asking the caller to
// behave, and `ShiftFromPoll` below is the only thing that decides where `at` comes from.
func TrafficShift(svc Service, at time.Time, before, after TrafficSplit, actor Actor, operationID, auditMethod string) (feeder.ChangeFact, error) {
	if at.IsZero() {
		return feeder.ChangeFact{}, fmt.Errorf("gcp: a traffic shift for %s with no instant; the instant "+
			"is the operation.last audit entry's timestamp, and a shift with no completion entry is "+
			"held rather than emitted with a guess (contracts/gcp-feeder.md §3.2)", svc.Value())
	}
	if err := svc.Validate(); err != nil {
		return feeder.ChangeFact{}, err
	}

	targets := []*graphv1.Ref{svc.Ref()}
	// Every revision either side of the shift is a target: the one traffic arrived at and the one
	// it left. Both are hop 0 of this change, because "what changed about this revision" must find
	// the shift that took its traffic away as well as the one that gave it some.
	for _, name := range revisionsInvolved(before, after) {
		targets = append(targets, Revision{Service: svc, Revision: name}.Ref())
	}

	return feeder.ChangeFact{
		Ref:       ChangeRefTrafficShift(svc, at),
		Kind:      graphv1.ChangeKind_ROLLOUT,
		Summary:   "traffic split for " + svc.Name + " changed to " + after.String(),
		ActorKind: actor.Kind,
		Targets:   targets,
		ValidAt:   at,
	}, nil
}

// TrafficShiftProps renders the shift's properties.
func TrafficShiftProps(svc Service, before, after TrafficSplit, actor Actor, operationID, auditMethod string) *feeder.Props {
	props := feeder.NewProps().
		Str(PropRolloutKind, RolloutTrafficShift).
		Bool(PropMovedTraffic, true).
		Str(PropProject, svc.Project).
		Str(PropRegion, svc.Region).
		Str(PropTrafficSplitSource, TrafficSplitSourceObserved).
		Str(PropActorRung, actor.Rung)
	if entries := before.Entries(); len(entries) > 0 {
		props = props.Strs(PropSplitBefore, entries...)
	}
	if entries := after.Entries(); len(entries) > 0 {
		props = props.Strs(PropSplitAfter, entries...)
	}
	if operationID != "" {
		props = props.Str(PropOperationID, operationID)
	}
	if auditMethod != "" {
		props = props.Str(PropAuditMethod, auditMethod)
	}
	if len(actor.Evidence) > 0 {
		props = props.Strs(PropActorEvidence, actor.Evidence...)
	}
	return props
}

// revisionsInvolved returns every revision named on either side of a shift whose share changed,
// sorted. A revision whose percentage is the same before and after was not involved: including it
// would put every long-lived revision at hop 0 of every shift.
func revisionsInvolved(before, after TrafficSplit) []string {
	changed := map[string]bool{}
	for _, share := range after.Shares {
		if was, mentioned := before.Percent(share.Revision); !mentioned || was != share.Percent {
			changed[share.Revision] = true
		}
	}
	for _, share := range before.Shares {
		if now, mentioned := after.Percent(share.Revision); !mentioned || now != share.Percent {
			changed[share.Revision] = true
		}
	}
	out := make([]string, 0, len(changed))
	for name := range changed {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// HeldSplit is a split the poll observed with no audit completion entry to date it (T055).
//
// It is a first-class value rather than a nil-and-a-comment because the checkpoint has to state it:
// "the split changed and we do not yet know when" is a real, reportable state of the world, and the
// two alternatives are both worse. Emitting it with the poll time as the instant would put a change
// in the graph at an instant GCP never stated, breaking SC-002 silently for every shift whose
// completion entry was still in flight. Dropping it would lose the shift entirely if the entry never
// arrives.
type HeldSplit struct {
	Service Service
	// Before and After are the splits either side, so the shift can be emitted unchanged once the
	// completion entry arrives.
	Before, After TrafficSplit
	// ObservedAt is when the poll saw it. It is **not** a candidate valid time; it is recorded so
	// the checkpoint can say how long the shift has been held.
	ObservedAt time.Time
	// Why is the reason it is held, for the checkpoint.
	Why string
}

// HeldNoCompletionEntry is the reason a shift is held.
const HeldNoCompletionEntry = "the split changed and no operation.last audit entry has arrived to date it"

// Hold records a split change that cannot yet be dated.
func Hold(svc Service, before, after TrafficSplit, observedAt time.Time) HeldSplit {
	return HeldSplit{
		Service:    svc,
		Before:     before,
		After:      after,
		ObservedAt: observedAt,
		Why:        HeldNoCompletionEntry,
	}
}

// String describes a held split for the checkpoint, without implying an instant.
func (h HeldSplit) String() string {
	return fmt.Sprintf("%s: %s -> %s, held since %s (%s)",
		h.Service.Value(), h.Before.String(), h.After.String(), instant(h.ObservedAt), h.Why)
}

// Completion is the audit evidence that dates a traffic shift: the `operation.last = true` entry.
type Completion struct {
	// At is that entry's `timestamp`. This is the published definition of the instant.
	At time.Time
	// OperationID is `LogEntry.operation.id`, which correlated it to its request entry.
	OperationID string
	// MethodName is the audit `methodName`, e.g. `google.cloud.run.v2.Services.UpdateService`.
	MethodName string
	// Auth is the caller, for the actor ladder.
	Auth AuthenticationInfo
}

// ShiftOutcome is what a poll's observation of a changed split resolves to: either a change ready to
// emit, or a hold. Exactly one of the two is set, which is what stops a caller from emitting a
// change *and* recording a hold for the same shift.
type ShiftOutcome struct {
	Change *feeder.ChangeFact
	Props  *feeder.Props
	Held   *HeldSplit
}

// ShiftFromPoll turns an observed split change into either a dated change or a hold (T054, T055).
//
// This is the only function that decides where a traffic shift's instant comes from, and it has
// exactly two answers: the completion entry's timestamp, or nothing. `Service.updateTime` is not a
// parameter, so the prohibited instant is not reachable from here.
//
// A nil completion is the held case, and it is not an error: a poll that runs between the request
// and the completion entry is the ordinary state of a rollout in progress, not a failure.
func ShiftFromPoll(svc Service, before, after TrafficSplit, observedAt time.Time, completion *Completion, policy ActorPolicy) (ShiftOutcome, error) {
	if before.Equal(after) {
		// Nothing changed. Returning an empty outcome rather than an error is what makes the
		// caller's loop a plain "for every service, reconcile" without a pre-check that would
		// duplicate this comparison.
		return ShiftOutcome{}, nil
	}
	if completion == nil || completion.At.IsZero() {
		held := Hold(svc, before, after, observedAt)
		return ShiftOutcome{Held: &held}, nil
	}
	actor := policy.Classify(completion.Auth, false)
	change, err := TrafficShift(svc, completion.At, before, after, actor, completion.OperationID, completion.MethodName)
	if err != nil {
		return ShiftOutcome{}, err
	}
	props := TrafficShiftProps(svc, before, after, actor, completion.OperationID, completion.MethodName)
	return ShiftOutcome{Change: &change, Props: props}, nil
}

// ZeroTrafficRevision is the rule of FR-018 (T056), stated as a function so that it has a name a
// test can call and a reviewer can find.
//
// A revision holding 0% of traffic:
//
//   - still exists as a node — it was created, it consumed a build, it may be about to serve;
//   - still produced its creation change, at its own instant;
//   - is **not** presented as serving: it is absent from PropServing, which is derived from the
//     split rather than stored beside it;
//   - and its creation change is **never reinterpreted** when traffic later arrives. The later
//     movement is its own change, at its own instant.
//
// The last is the one that needs enforcing rather than intending. The tempting implementation, on
// seeing traffic arrive at a revision, is to "complete" the creation change by stamping it with the
// moment it started serving — which destroys the two-instant distinction this whole file is about
// and does it retroactively, to a change an investigation may already have read.
//
// It is enforced structurally: the creation change's ref is keyed on the creation instant
// (ChangeRefRevisionCreated), so an attempt to re-emit it at a later instant produces a *different*
// change rather than an update to that one — and its PropMovedTraffic is a constant false rather
// than a field any later observation feeds.
func ZeroTrafficRevision(obs RevisionObservation, split TrafficSplit) (exists bool, serving bool, producedCreationChange bool) {
	percent, _ := split.Percent(obs.Revision.Revision)
	return true, percent > 0, true
}

// AssertCreationChangeNotReinterpreted is the guard behind FR-018's last clause. It returns an error
// if a caller tries to build a creation change for a revision at an instant that is not the
// revision's `createTime` — which is the shape every "let me just update the creation change"
// attempt takes.
func AssertCreationChangeNotReinterpreted(rev Revision, createTime, at time.Time) error {
	if at.Equal(createTime) {
		return nil
	}
	return fmt.Errorf("gcp: a creation change for revision %s was built at %s rather than its "+
		"createTime %s. A creation change is never reinterpreted when traffic later arrives — the "+
		"later movement is its own change at its own instant (FR-018)",
		rev.Value(), instant(at), instant(createTime))
}

// SummariseHeld renders held splits for a checkpoint note, oldest first, so the note is stable
// across runs and a reader sees the longest-held shift first.
func SummariseHeld(held []HeldSplit) string {
	if len(held) == 0 {
		return ""
	}
	sorted := append([]HeldSplit(nil), held...)
	sort.Slice(sorted, func(i, j int) bool {
		if !sorted[i].ObservedAt.Equal(sorted[j].ObservedAt) {
			return sorted[i].ObservedAt.Before(sorted[j].ObservedAt)
		}
		return sorted[i].Service.Value() < sorted[j].Service.Value()
	})
	lines := make([]string, 0, len(sorted))
	for _, h := range sorted {
		lines = append(lines, h.String())
	}
	return strings.Join(lines, "; ")
}

// RolloutDeployKeys are the cross-source correlation keys a revision-created rollout carries
// (004 T135, T148, FR-045, SC-004).
//
// They are CORRELATION KEYS and not identity claims, which is the correction T148 made. A commit does
// not name a rollout: a monorepo run ships one commit to three services and a redeploy ships it again,
// so several changes legitimately carry the value and `graph.identity_claims` — unique per
// (namespace, value, source) — could hold it for only one of them. The event log now refuses these
// namespaces as claims outright (internal/log, ReasonCorrelationAsIdentity), so this is not a style
// preference.
//
// They are on the CHANGE and not on the revision, which is the other half of the point. SC-004 is "one
// rollout, not two": certain rule C8 compares two **change** observations, and it requires both sides
// to be changes precisely so that a deploy identifier cannot put a change together with a service. The
// revision's image already reaches the graph as a property (PropImage, PropImageDigest); a second,
// inert copy on the revision would be noise, because nothing reads it there.
//
// What is available, established from the vendored Cloud Run v2 surface rather than assumed:
//
//   - `deploy.image` comes from the image reference the revision runs, and only where the deployer
//     **pinned a digest**. Cloud Run v2's `Revision` has no resolved-digest field — `ContainerStatus`
//     with its `imageDigest` hangs off `Instance`, not off a revision — so a service deployed with
//     `--image=repo:tag` states no digest anywhere, and a tag is not claimed: two rollouts running
//     `:latest` are not one rollout.
//   - `deploy.commit_sha` comes from a **label**, because the API states no commit at all. That makes
//     it conditional on the operator's deploy tooling and on config/gcp.yaml's `commit_from_labels`,
//     and it is the only identifier a GitHub rollout and a Cloud Run rollout can share: GitHub
//     deployments state a commit and no image digest, Cloud Run states an image and no commit.
//
// A missing value is an absent key and never an invented one (FR-041).
func RolloutDeployKeys(obs RevisionObservation) []feeder.CorrelationKey {
	var keys []feeder.CorrelationKey
	if image, ok := feeder.Image(obs.Image); ok {
		keys = append(keys, feeder.CorrelationKey{
			Namespace: feeder.NSDeployImage, Value: image,
			Why: "the digest-pinned image this rollout deployed",
		})
	}
	if obs.Labels.Commit != "" {
		keys = append(keys, feeder.CorrelationKey{
			Namespace: feeder.NSDeployCommitSHA, Value: obs.Labels.Commit,
			Why: "the commit this rollout shipped, declared by the label " + obs.Labels.CommitSource,
		})
	}
	return keys
}
