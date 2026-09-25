// SPDX-License-Identifier: Apache-2.0

package github

import (
	"fmt"
	"maps"
	"slices"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// One observed deployment becomes changes on the graph (004 US1; FR-012, FR-017, FR-022, FR-041).
//
// ---------------------------------------------------------------------------------------------
// What this file is, and what it deliberately is not
//
// Every decision a rollout needs has already been made, each in one place with its reason beside it:
// what a status means (status.go), who acted (actorkind.go), which environments count (allowlist.go),
// what it targeted (targets.go), what identifies it and what links to it (origin.go), and what it
// claims (claims.go). This file only **composes** them, and that is the point — a mapper that decided
// anything itself would be a second place a decision lives, and the second place is always the one
// nobody updates.
//
// So the ordering below is the contract: exclude, then read the status history, then resolve targets,
// then emit one change per target with its claims. A reader checking whether this feeder honours
// FR-023 looks at the allowlist; a reader checking whether it honours FR-022 looks at the status
// table. Neither has to read this file to find out.
//
// # An exclusion is not a silence
//
// Everything this function declines to emit is counted, under the filter that decided (FR-019). The
// returned Exclusions travel with the events rather than being logged, because a caller writing a
// checkpoint has to be able to say *why* a window that looked busy produced three changes.

// MapOptions is the operator's configuration, assembled once per cycle.
type MapOptions struct {
	Allowlist Allowlist
	Targets   TargetMap
	Actors    ActorPolicy
	Releases  ReleasePolicy
	// PollInterval is the cadence this connector reads GitHub at, recorded on every change as the
	// interval its history is sampled at (FR-052). Zero uses DefaultPollInterval.
	//
	// It is its OWN field and is deliberately not read off the reordering window, although
	// `DefaultReorderingWindow`'s comment says "the window is the poll interval rather than zero" and
	// the two default to the same number. They are different facts: the reordering window is how far
	// out of order this feeder may deliver its own events, and the poll interval is how often it
	// looks. They coincide today because the window is *derived* from the cadence — but an operator
	// who widened the window to accommodate a slow paginated read, without touching their cadence,
	// would silently change the sampling interval every change in the graph claims. One field, one
	// fact.
	PollInterval time.Duration
}

// Observation is one deployment as a cycle saw it.
type Observation struct {
	Repo Repo
	// RepositoryID is GitHub's numeric id, which every change identity is built from — a rename does
	// not touch it (Edge case 9).
	RepositoryID int64
	Deployment   Deployment
	// Statuses are the deployment's transitions **in the order GitHub returned them**. The order is
	// not normalised here or anywhere downstream: Edge case 11 turns on the platform's own.
	Statuses []DeploymentStatus
	// Run is the workflow run that produced the deployment, where the cycle could identify one. It is
	// what the actor and the origin link come from; a deployment created through the API directly has
	// none, and its creator is the actor instead.
	Run *WorkflowRun
	// OrderingAmbiguous marks a deployment that completed in the same second as another in the window
	// (TieAtSameInstant). Recorded as a property, never resolved.
	OrderingAmbiguous bool
}

// The change properties this feeder publishes.
const (
	// PropDeploymentEnvironment is the environment the rollout went to, as GitHub stated it.
	PropDeploymentEnvironment = "deployment.environment.name"
	// PropProductionEnvironment is GitHub's own flag, recorded as **evidence beside** the operator's
	// allowlist rather than as the thing that decided (FR-023). Absent where GitHub stated nothing,
	// because absent and false are different facts.
	PropProductionEnvironment = "sre.github.production_environment"
	// PropDeactivatedAt is a later `inactive`, recorded against the rollout it is a property of.
	PropDeactivatedAt = "sre.github.deactivated_at"
	// PropAttemptState and PropAttemptAt record a failed attempt that preceded the rollout.
	PropAttemptState = "sre.github.failed_attempt_state"
	PropAttemptAt    = "sre.github.failed_attempt_at"
	// PropActorRung and PropActorEvidence name which rung of the ladder decided and what it had to
	// work with. The evidence carries no login (actorkind.go).
	PropActorRung     = "sre.github.actor_rung"
	PropActorEvidence = "sre.github.actor_evidence"
	// PropRunAttempt is which attempt of the run this was, for a reader comparing two changes that
	// look alike (FR-028).
	PropRunAttempt = "sre.github.run_attempt"
	// PropUnrecognisedStates counts the status states the published table has not ruled on, by
	// spelling. It is on the change rather than only in telemetry because a change emitted from a
	// history containing one is a change somebody should look at.
	PropUnrecognisedStates = "sre.github.unrecognised_states"
	// PropSeveralSuccesses marks a deployment whose history carried more than one success.
	PropSeveralSuccesses = "sre.github.several_successes"
	// PropSampled and PropSampledEvery mark the history as POLLED rather than complete (FR-052).
	//
	// Both, rather than the interval alone: "sampled, at 5m" and "complete" are the two answers a
	// consumer needs, and leaving the first to be inferred from whether the second is set makes an
	// absent field mean "complete" — which is the wrong default for a connector that can only ever
	// poll. Polling is the source of truth for both deploy platforms (FR-052), so `sampled` is true
	// on every change either of them emits, and the day one gains a complete event stream the flag is
	// the thing that changes rather than a field quietly disappearing.
	PropSampled      = "sre.github.sampled"
	PropSampledEvery = "sre.github.sampled_every"
)

// MapDeployment turns one observed deployment into events.
//
// It returns the events, what it excluded and why, and an error only where an event could not be
// built at all. A deployment that is not a rollout is not an error: it is an exclusion, and the
// difference is FR-073's at the level of a single object.
func MapDeployment(desc feeder.Description, obs Observation, opts MapOptions, at time.Time) ([]*graphv1.EventEnvelope, Exclusions, error) {
	var excluded Exclusions

	// 1. The operator's allowlist, before anything is read. An environment nobody listed is not a
	// production change, and deciding that first means the status history of a preview deployment is
	// never even parsed.
	if ok, reason := opts.Allowlist.AllowsEnvironment(obs.Deployment.Environment); !ok {
		excluded.Exclude(reason)
		return nil, excluded, nil
	}

	// 2. What the platform said happened.
	rollout := ReadStatuses(obs.Statuses)
	switch {
	case rollout.DeactivatedWithoutRollingOut():
		// An `inactive` with no success has no rollout to be a property of, and attaching it to one
		// would invent the rollout (status.go).
		excluded.Exclude("the deployment was deactivated without ever succeeding")
		return nil, excluded, nil
	case !rollout.RolledOut:
		if rollout.Attempted {
			excluded.Exclude("the deploy was attempted and did not land: " + rollout.AttemptState)
		} else {
			excluded.Exclude("no status reports that anything reached production")
		}
		return nil, excluded, nil
	}

	// 3. What it changed. No match is Edge case 1 rather than an exclusion: the deploy happened.
	workflow := ""
	if obs.Run != nil {
		workflow = obs.Run.Name
	}
	targets := opts.Targets.TargetsFor(obs.Repo, obs.Deployment.Environment, workflow)

	base, ok := DeploymentChangeRef(obs.RepositoryID, obs.Deployment.ID)
	if !ok {
		return nil, excluded, fmt.Errorf("github: %s deployment %d has no identity; a change without "+
			"one cannot be recorded", obs.Repo, obs.Deployment.ID)
	}
	split, ok := SplitByTarget(base.GetValue(), targets)
	if !ok {
		return nil, excluded, fmt.Errorf("github: %s deployment %d could not be split by target",
			obs.Repo, obs.Deployment.ID)
	}

	// 4. Who acted, and how it is linked back.
	actor := opts.Actors.Classify(obs.actorEvidence())
	origin, _ := OriginLink(obs.originURL())
	pointers := obs.pointers()
	keys := obs.deployKeys()

	props, err := obs.props(rollout, actor, opts.pollInterval())
	if err != nil {
		return nil, excluded, err
	}

	// 5. One change per target, each with the same origin, the same deploy keys and its own identity.
	var out []*graphv1.EventEnvelope
	for _, change := range split.Changes {
		fact := feeder.ChangeFact{
			Meta:             feeder.Meta{SourceObservedAt: at},
			Ref:              change.Ref,
			Kind:             graphv1.ChangeKind_ROLLOUT,
			Summary:          obs.summary(change.Target),
			Actor:            actor.Login,
			ActorKind:        actor.Kind,
			OriginRef:        origin,
			ValidAt:          rollout.CompletedAt,
			ValidFromUnknown: rollout.ValidStartUnknown(),
			Props:            props,
			Pointers:         pointers,
		}
		if change.Target != nil {
			fact.Targets = []*graphv1.Ref{change.Target}
		}
		id := feeder.NewID(desc.SourceID, "change", change.Ref.GetValue())
		out = append(out, feeder.ObserveChange(desc, id, fact))

		// Correlation keys and not identity claims: one commit ships to every target in this split, so
		// the value is shared by construction and a claim could hold it for only one of them (claims.go,
		// 004 T148).
		for _, key := range keys {
			fact := feeder.CorrelationFact{
				Meta:       feeder.Meta{SourceObservedAt: at},
				Subject:    change.Ref,
				Key:        key.Ref(),
				Attributes: keyAttributes(key, obs.Deployment.Environment),
			}
			id := feeder.NewID(desc.SourceID, "correlation",
				change.Ref.GetValue(), key.Namespace, key.Value)
			out = append(out, feeder.Correlate(desc, id, fact))
		}
	}
	if split.Unattached {
		// Counted, not excluded: the change WAS emitted. A caller's checkpoint says how many deploys
		// it could not attach, which is a different number from how many it declined to record.
		excluded.Exclude("emitted unattached: no configured target for this repository and environment")
	}
	return out, excluded, nil
}

// actorEvidence is what the payload says about who acted: the run's trigger where a run produced the
// deployment, and the deployment's own creator otherwise.
//
// The run's **triggering** actor rather than its owner, because on a re-run they differ and FR-028's
// new change belongs to whoever asked for this attempt.
func (o Observation) actorEvidence() ActorEvidence {
	if o.Run == nil {
		return ActorEvidence{Account: o.Deployment.Creator}
	}
	account := o.Run.TriggeringActor
	if account.Login == "" && account.ID == 0 && account.Type == "" {
		account = o.Run.Actor
	}
	return ActorEvidence{Event: o.Run.Event, Account: account}
}

func (o Observation) originURL() string {
	if o.Run != nil && o.Run.HTMLURL != "" {
		return o.Run.HTMLURL
	}
	// A deployment created through the API has no page of its own that GitHub links to; the
	// SOURCE_LINK pointer addresses it, and the origin link is left empty rather than pointed at a
	// URL this code assembled.
	return ""
}

func (o Observation) pointers() []*graphv1.Pointer {
	var out []*graphv1.Pointer
	if pointer, ok := DeploymentSourceLink(o.Repo, o.Deployment.ID); ok {
		out = append(out, pointer)
	}
	if o.Run != nil {
		if pointer, ok := RunSourceLink(o.Repo, o.Run.ID); ok {
			out = append(out, pointer)
		}
		if pointer, ok := RunLogPointer(o.Run.LogsURL); ok {
			out = append(out, pointer)
		}
	}
	return out
}

// deployKeys are the correlation keys this rollout carries.
//
// The commit comes from the deployment where it states one and from the run's head otherwise, and
// which of the two is recorded as the key's evidence — a reviewer reading a merge can see whether
// the identifier came from the deployment record or from the pipeline that produced it.
func (o Observation) deployKeys() []feeder.CorrelationKey {
	sha, source := o.Deployment.SHA, SourceDeploymentSHA
	if sha == "" && o.Run != nil {
		sha, source = o.Run.HeadSHA, SourceRunHeadSHA
	}
	// No repository key: see claims.go's RepositoryProperty. It is a property of the change, added in
	// props.
	return DeployKeys(o.Repo, sha, source, "")
}

// keyAttributes renders a key's supporting facts, with the environment C8 compares.
//
// Sorted, so the same observation emits the same event bytes twice and a golden does not depend on
// Go's map iteration order.
func keyAttributes(key feeder.CorrelationKey, environment string) *structpb.Struct {
	props := feeder.NewProps().Str(AttrClaimSource, key.Why)
	if environment != "" {
		props = props.Str(PropDeploymentEnvironment, environment)
	}
	for _, name := range slices.Sorted(maps.Keys(key.Attrs)) {
		props = props.Str(name, key.Attrs[name])
	}
	built, err := props.Build()
	if err != nil {
		// Every value above is a string this package produced; a failure here is a programming error
		// rather than a fact about the payload, and an empty attribute set would make a rule silently
		// not fire.
		panic(fmt.Sprintf("github: building correlation attributes for %s: %v", key.Namespace, err))
	}
	return built
}

// pollInterval resolves the configured cadence, treating zero and anything negative as the default.
//
// A negative interval is treated as the default rather than refused here because this is a rendering
// path, and a mapper that panicked on configuration would turn an operator's typo into a lost cycle.
// Options.Validate is where a negative value is refused, at startup, before anything is read.
func (m MapOptions) pollInterval() time.Duration {
	if m.PollInterval <= 0 {
		return DefaultPollInterval
	}
	return m.PollInterval
}

// props are the change's published properties.
func (o Observation) props(rollout Rollout, actor Actor, pollInterval time.Duration) (*structpb.Struct, error) {
	props := feeder.NewProps().
		Str(PropDeploymentEnvironment, o.Deployment.Environment).
		Str(PropObjectKind, ObjectKindDeployment).
		Str(PropActorRung, actor.Rung).
		// FR-052: a polled history is marked sampled at the poll interval, so a consumer can tell a
		// complete history from a sampled one. It rides on the change as well as on the checkpoint
		// because the two have different readers: a consumer asking "what was production running at
		// 14:07?" holds a change and has no reason to join to a checkpoint, and the honest answer to
		// that question depends on how coarsely the history was read. The flag is stated rather than
		// implied by the interval's presence, because "sampled, at 5m" and "complete" are the two
		// answers and a reader should not have to infer which from whether a field is set.
		Bool(PropSampled, true).
		Str(PropSampledEvery, pollInterval.String())

	if repository, ok := RepositoryProperty(o.Repo); ok {
		// A property rather than a claim or a correlation key: several changes share one repository so a
		// claim cannot hold it, and no published rule correlates on it (claims.go).
		props = props.Str(AttrRepository, repository)
	}

	if len(actor.Evidence) > 0 {
		props = props.Strs(PropActorEvidence, actor.Evidence...)
	}
	if o.Deployment.ProductionEnvironment != nil {
		// GitHub's own flag, as evidence beside the operator's list. Absent where GitHub stated
		// nothing, because absent and false are different facts.
		props = props.Bool(PropProductionEnvironment, *o.Deployment.ProductionEnvironment)
	}
	if rollout.Deactivated {
		props = props.Time(PropDeactivatedAt, rollout.DeactivatedAt)
	}
	if rollout.Attempted {
		props = props.Str(PropAttemptState, rollout.AttemptState).Time(PropAttemptAt, rollout.AttemptedAt)
	}
	if rollout.Ambiguous() {
		props = props.Bool(PropSeveralSuccesses, true)
	}
	if o.OrderingAmbiguous {
		props = props.Bool(PropOrderingAmbiguous, true)
	}
	if len(rollout.Unrecognised) > 0 {
		states := slices.Sorted(maps.Keys(rollout.Unrecognised))
		props = props.Strs(PropUnrecognisedStates, states...)
	}
	if o.Run != nil && o.Run.RunAttempt > 0 {
		props = props.Int(PropRunAttempt, int64(o.Run.RunAttempt))
	}
	return props.Build()
}

// summary is the one human-readable line a change carries.
func (o Observation) summary(target *graphv1.Ref) string {
	if target == nil {
		return fmt.Sprintf("deployed %s to %s (no configured target)", o.Repo, o.Deployment.Environment)
	}
	return fmt.Sprintf("deployed %s to %s on %s", o.Repo, target.GetValue(), o.Deployment.Environment)
}
