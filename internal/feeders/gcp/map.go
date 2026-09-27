// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sort"
	"time"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// Emission: turning one poll into typed events (T046, T051–T056).
//
// This is where the mapping functions in cloudrun.go, rollout.go and labels.go meet the emitter.
// Keeping it separate from those files is not organisation for its own sake: the mappers are pure
// functions of a payload, which is what lets the recorded and the live path be the same code
// (FR-044), and this file is the only one that holds state between payloads.
//
// It holds exactly two kinds of memory, and both exist because a *poll* cannot state them:
//
//   - the last observed split per service, because a poll sees a split and not a transition;
//   - the last observed uid per service, because a recreation is only visible as a change in it.
//
// Neither is a cache of the graph. A feeder never reads the graph (constitution I); it remembers
// what it itself last saw.

// servicesPayload and revisionsPayload are the recorded shapes of the two list responses. They are
// `{"services": [...]}` rather than a bare array so that a recorder can add the paging metadata a
// later task needs without changing the fixtures already written.
type servicesPayload struct {
	Services []json.RawMessage `json:"services"`
}

type revisionsPayload struct {
	Revisions []json.RawMessage `json:"revisions"`
}

// applyServices maps one `projects.locations.services.list` response.
func (f *Feeder) applyServices(ctx context.Context, desc feeder.Description, em feeder.Emitter, raw []byte, at time.Time) error {
	var payload servicesPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return fmt.Errorf("gcp: decoding a %s payload: %w", PayloadServices, err)
	}
	for _, item := range payload.Services {
		var svc runpb.Service
		if err := protojson.Unmarshal(item, &svc); err != nil {
			return fmt.Errorf("gcp: decoding a Cloud Run service: %w", err)
		}
		if err := f.emitService(ctx, desc, em, &svc, at); err != nil {
			return err
		}
	}
	return nil
}

// emitService emits everything one service poll asserts.
func (f *Feeder) emitService(ctx context.Context, desc feeder.Description, em feeder.Emitter, svc *runpb.Service, at time.Time) error {
	obs, err := ObserveService(svc, f.opts.Labels)
	if err != nil {
		return err
	}

	// The recreation check runs *before* the node assertion, because a recreated service is a
	// different entity and asserting the node first would attach the new service's properties to
	// the old one's history (FR-025).
	current := Identity{UID: svc.GetUid(), CreateTime: obs.CreateTime, LastSeen: at}
	if obs.UpdateTime.After(obs.CreateTime) {
		current.StateFrom = obs.UpdateTime
	}
	f.mu.Lock()
	previous, seen := f.identities[obs.Service.Value()]
	f.identities[obs.Service.Value()] = current
	lastSplit, hadSplit := f.splits[obs.Service.Value()]
	f.splits[obs.Service.Value()] = obs.Split
	f.mu.Unlock()

	// Whether this poll is the first assertion of this uid. A recreated service is a new uid under
	// the old name, so its first poll is a first assertion exactly as a never-seen service's is.
	firstOfUID := !seen || previous.UID != current.UID
	if seen {
		if rec, recreated := DetectRecreation(obs.Service, previous, current); recreated {
			if err := f.emitRecreation(ctx, desc, em, *rec); err != nil {
				return err
			}
			// The old entity is retracted and the new one created, so the split it used to have is
			// not this entity's history: comparing against it would emit a traffic shift that never
			// happened.
			hadSplit = false
		}
	}

	props, err := obs.Props().Build()
	if err != nil {
		return err
	}
	pointers, err := ServicePointers(obs)
	if err != nil {
		return err
	}
	// FR-085: the absence of a trace pointer is a stated fact about the entity.
	if _, minted := firstOfKind(pointers, graphv1.PointerKind_TRACE); !minted {
		props.Fields[PropTracePointerAbsent] = structpb.NewStringValue(TracePointerAbsence)
	}

	// Two assertions, each named by the platform instant it is dated from (serviceEventID).
	//
	// The EXISTENCE assertion, from `createTime`, on a uid's first poll in this run. Its id is the
	// creation stamp, so a restarted feeder re-sending it repeats an id already sent and is a no-op:
	// it can never re-date, from the service's creation, a state that changed while the feeder was
	// down. That was the restart limit T066 left open and T184 closes.
	if firstOfUID {
		node := obs.NodeFact()
		node.Props, node.Pointers, node.SourceObservedAt = props, pointers, at
		if err := emit(ctx, em, feeder.UpsertNode(desc,
			serviceEventID(desc.SourceID, obs, current.UID, "created", obs.CreateTime), node)); err != nil {
			return err
		}
	}
	// The STATE assertion, from `updateTime`, whenever the platform says the resource took its
	// current form after it was created — on a first poll too, which is what makes a restart safe.
	// An unchanged re-poll repeats its id and is a no-op; a change, a change back and a restart after
	// a change are each a new updateTime and so a new assertion, dated where the platform dates it.
	// A later poll with no stated update instant has nothing to date a new state from. It asserts
	// one only when what it saw differs from what was last asserted, from the observation and marked
	// unknown (FR-011); a never-updated service re-polled is otherwise re-asserted at every poll, each
	// time as a new unknown start.
	digest, err := stateDigest(feeder.NodeFact{DisplayName: obs.NodeFact().DisplayName, Props: props, Pointers: pointers})
	if err != nil {
		return err
	}
	later := obs.LaterStateFact()
	switch {
	case !later.ValidFromUnknown:
		later.Props, later.Pointers, later.SourceObservedAt = props, pointers, at
		if err := emit(ctx, em, feeder.UpsertNode(desc,
			serviceEventID(desc.SourceID, obs, current.UID, "updated", obs.UpdateTime), later)); err != nil {
			return err
		}
	case !firstOfUID && digest != previous.Digest:
		later.ValidAt = at
		later.Props, later.Pointers, later.SourceObservedAt = props, pointers, at
		if err := emit(ctx, em, feeder.UpsertNode(desc,
			serviceEventID(desc.SourceID, obs, current.UID, "observed", at), later)); err != nil {
			return err
		}
		current.StateFrom = at
	}
	if !firstOfUID && current.StateFrom.IsZero() {
		current.StateFrom = previous.StateFrom
	}
	current.Digest = digest
	f.mu.Lock()
	f.identities[obs.Service.Value()] = current
	f.mu.Unlock()

	if err := f.emitClaims(ctx, desc, em, obs.Service.Ref(), obs.Service.Claims(obs.OTelServiceName, obs.EnvVarNames), obs.Labels.Environment, at); err != nil {
		return err
	}
	if err := f.emitOwner(ctx, desc, em, obs.Service.Ref(), obs.Labels, obs.CreateTime, at); err != nil {
		return err
	}

	// The split change. A poll sees a split, not a transition, so the first observation of a
	// service asserts the property and emits no change: there is nothing to have changed from, and
	// emitting one would claim a rollout at the instant the connector was first run.
	if !hadSplit {
		return nil
	}
	completion := f.completionFor(obs.Service, lastSplit, obs.Split)
	outcome, err := ShiftFromPoll(obs.Service, lastSplit, obs.Split, at, completion, f.opts.Actors)
	if err != nil {
		return err
	}
	switch {
	case outcome.Held != nil:
		f.HoldShift(*outcome.Held)
		return nil
	case outcome.Change != nil:
		// The entry that dated this shift is consumed, so it does not also become a general audit
		// change: two changes in the graph for one thing that happened is worse than either failure
		// the catch-all was meant to avoid (auditlog.go).
		if completion != nil {
			f.consumeAuditEntry(completion.OperationID)
		}
		change := *outcome.Change
		change.SourceObservedAt = at
		if outcome.Props != nil {
			props, err := outcome.Props.Build()
			if err != nil {
				return err
			}
			// The split either side of the shift, and the operation that dated it. A shift whose
			// before-and-after nobody can read is a shift nobody can recognise as a rollback.
			change.Props = props
		}
		return emit(ctx, em, feeder.ObserveChange(desc, feeder.NewID(desc.SourceID, "change", change.Ref.GetValue()), change))
	default:
		// The poll showed no change, so whatever the entry described is not something this path
		// models. It is deliberately NOT consumed: the catch-all is what covers it.
		return nil
	}
}

// consumeAuditEntry states that a specialised path emitted a change from an audit entry, so the entry
// must not also become a general one.
func (f *Feeder) consumeAuditEntry(operationID string) {
	f.mu.Lock()
	f.audit.ConsumeGeneral(operationID)
	f.mu.Unlock()
}

// applyRevisions maps one `projects.locations.services.revisions.list` response.
func (f *Feeder) applyRevisions(ctx context.Context, desc feeder.Description, em feeder.Emitter, raw []byte, at time.Time) error {
	var payload revisionsPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return fmt.Errorf("gcp: decoding a %s payload: %w", PayloadRevisions, err)
	}
	for _, item := range payload.Revisions {
		var rev runpb.Revision
		if err := protojson.Unmarshal(item, &rev); err != nil {
			return fmt.Errorf("gcp: decoding a Cloud Run revision: %w", err)
		}
		if err := f.emitRevision(ctx, desc, em, &rev, at); err != nil {
			return err
		}
	}
	return nil
}

// emitRevision emits a revision's node, its claims, its `deployed-by` edge and its creation change.
func (f *Feeder) emitRevision(ctx context.Context, desc feeder.Description, em feeder.Emitter, rev *runpb.Revision, at time.Time) error {
	obs, err := ObserveRevision(rev, f.opts.Labels)
	if err != nil {
		return err
	}
	props, err := obs.Props().Build()
	if err != nil {
		return err
	}
	pointers, err := RevisionPointers(obs, obs.Labels.Environment)
	if err != nil {
		return err
	}
	node := obs.NodeFact()
	node.Props = props
	node.Pointers = pointers
	node.SourceObservedAt = at
	if err := emit(ctx, em, feeder.UpsertNode(desc, feeder.NewID(desc.SourceID, "revision", obs.Revision.Value()), node)); err != nil {
		return err
	}

	if err := f.emitClaims(ctx, desc, em, obs.Revision.Ref(), obs.Revision.Claims(obs.Image, obs.ImageDigest), obs.Labels.Environment, at); err != nil {
		return err
	}

	// What this revision was deployed with, and the dependencies its configuration derives or
	// proposes (T134–T137). A Cloud Run revision is the immutable snapshot of the template, so the
	// configuration version is read out of this response rather than fetched separately.
	config, err := ObserveConfig(rev, f.opts.FingerprintKey)
	if err != nil {
		return err
	}
	if err := f.emitConfiguration(ctx, desc, em, config, at); err != nil {
		return err
	}

	// A revision runs the service's traffic, so the edge reads revision → service.
	if err := emit(ctx, em, feeder.UpsertEdge(desc,
		feeder.NewID(desc.SourceID, "edge", "runs-on", obs.Revision.Value()),
		feeder.EdgeFact{
			Meta:    feeder.Meta{SourceObservedAt: at},
			Src:     obs.Revision.Ref(),
			Dst:     obs.Revision.Service.Ref(),
			Type:    graphv1.EdgeType_RUNS_ON,
			ValidAt: obs.CreateTime,
		})); err != nil {
		return err
	}

	// The creation change. Its instant is the API's `createTime` and its principal comes from the
	// correlated audit entry, because there is no CreateRevision methodName in v2 (research §2).
	actor := f.opts.Actors.Classify(f.authFor(obs.Revision), false)
	change, err := RevisionCreated(obs.Revision, obs.CreateTime, actor, "", "")
	if err != nil {
		return err
	}
	changeProps, err := RevisionCreatedProps(obs.Revision, actor, "", "").Build()
	if err != nil {
		return err
	}
	// Carried on the change, which is new in 004: until then ObserveChange had nowhere to put a
	// property and this value was built and discarded. FR-016 requires the creation change to be
	// *marked* as not having moved production traffic — a positive statement — and contract §3.3
	// publishes that mark, so dropping it left a published clause unmet.
	change.Props = changeProps
	change.SourceObservedAt = at
	if err := emit(ctx, em, feeder.ObserveChange(desc, feeder.NewID(desc.SourceID, "change", change.Ref.GetValue()), change)); err != nil {
		return err
	}

	// The cross-source deploy keys, on the change and after it (004 T135, T148). The environment is
	// passed because C8 requires both sides to state one: a staging rollout of a commit and the
	// production rollout of the same commit are two rollouts, and on a certain rule treating the
	// two unstated environments as agreed would merge a promotion into its own staging deploy.
	return f.emitCorrelations(ctx, desc, em, change.Ref, RolloutDeployKeys(obs), obs.Labels.Environment, at)
}

// emitClaims emits one identity claim per identifier, including the addressing ref (FR-115).
func (f *Feeder) emitClaims(ctx context.Context, desc feeder.Description, em feeder.Emitter, subject *graphv1.Ref, claims []Claim, environment string, at time.Time) error {
	for _, claim := range claims {
		attrs := feeder.NewProps().Str("sre.gcp.claim_source", claim.Why)
		if environment != "" && environment != EnvironmentUnknown {
			attrs = attrs.Str(feeder.AttrDeploymentEnvironment, environment)
		}
		// The supporting attributes the published rules read (Claim.Attrs). Sorted, so that the
		// same observation emits the same event bytes twice and a golden does not depend on Go's
		// map iteration.
		for _, key := range slices.Sorted(maps.Keys(claim.Attrs)) {
			attrs = attrs.Str(key, claim.Attrs[key])
		}
		built, err := attrs.Build()
		if err != nil {
			return err
		}
		fact := feeder.IdentityFact{
			Meta:       feeder.Meta{SourceObservedAt: at},
			Subject:    subject,
			Claim:      claim.Ref(),
			Attributes: built,
		}
		id := feeder.NewID(desc.SourceID, "claim", subject.GetValue(), claim.Namespace, claim.Value)
		if err := emit(ctx, em, feeder.IdentityClaim(desc, id, fact)); err != nil {
			return err
		}
	}
	return nil
}

// emitCorrelations emits one correlation key per shared value (004 T148).
//
// It is emitClaims with one word changed, and the duplication is deliberate rather than a missed
// refactor: the two events mean different things, they are stored under different rules, and the event
// log refuses one where the other belongs. A single helper taking a flag would be a helper whose caller
// has to remember which kind it asked for — and getting that wrong in the identity direction is the
// expensive mistake, because a shared value stored as a name lands on whichever entity was processed
// first.
func (f *Feeder) emitCorrelations(ctx context.Context, desc feeder.Description, em feeder.Emitter, subject *graphv1.Ref, keys []feeder.CorrelationKey, environment string, at time.Time) error {
	for _, key := range keys {
		attrs := feeder.NewProps().Str("sre.gcp.claim_source", key.Why)
		if environment != "" && environment != EnvironmentUnknown {
			attrs = attrs.Str(feeder.AttrDeploymentEnvironment, environment)
		}
		// Sorted, so that the same observation emits the same event bytes twice and a golden does not
		// depend on Go's map iteration.
		for _, name := range slices.Sorted(maps.Keys(key.Attrs)) {
			attrs = attrs.Str(name, key.Attrs[name])
		}
		built, err := attrs.Build()
		if err != nil {
			return err
		}
		fact := feeder.CorrelationFact{
			Meta:       feeder.Meta{SourceObservedAt: at},
			Subject:    subject,
			Key:        key.Ref(),
			Attributes: built,
		}
		id := feeder.NewID(desc.SourceID, "correlation", subject.GetValue(), key.Namespace, key.Value)
		if err := emit(ctx, em, feeder.Correlate(desc, id, fact)); err != nil {
			return err
		}
	}
	return nil
}

// emitOwner emits the OWNER node and its `owned-by` edge (FR-125, revised 2026-09-21).
//
// The owner's valid start is a **bound**, stated rather than left unknown: the earliest instant the
// labelled service state is known to have held, which is the service's own `createTime`. The node
// carries PropOwnerValidFromIsABound so that a reader does not mistake the bound for the instant the
// team came to own the service — which is not a fact a label carries, because a label has no history.
//
// FR-125 originally required an *unknown* start, and that turned out to be unsatisfiable alongside the
// graph's placeholder rule. `markPlaceholder` gives an edge-minted endpoint **the edge's**
// `valid_from_unknown`, deliberately: a placeholder minted with the flag clear would keep it clear
// once the real node arrived, and the entity would then read differently according to whether the edge
// or its endpoint reached the graph first. So an `owned-by` edge has to agree with both of its
// endpoints — and the Cloud Run service at one end has a documented `createTime`, so its start is
// known and claiming otherwise would be false. Whichever flag the edge carried, one endpoint
// disagreed, and the shuffle check found the permutation that exposed which. US1's fixtures surfaced
// it; the decision was to take the bound and say it is one.
func (f *Feeder) emitOwner(ctx context.Context, desc feeder.Description, em feeder.Emitter, subject *graphv1.Ref, labels Labels, createTime, at time.Time) error {
	if labels.Owner == "" {
		return nil
	}
	ownerRef := feeder.Ref(feeder.NSOwnerTeam, labels.Owner)
	props, err := feeder.NewProps().
		Str(feeder.PropOwnerKind, "team").
		Str(feeder.PropOwnerTeam, labels.Owner).
		Str(feeder.PropOwnerSourceLabel, f.opts.Labels.OwnerLabel).
		// The bound, said out loud. A reader seeing `valid from 2026-08-22` on an owner would
		// otherwise read it as the day the team took the service on, and it is only the earliest
		// instant we can show the labelled state held.
		Str(PropOwnerValidFromIsABound, OwnerValidFromBoundReason).
		Build()
	if err != nil {
		return err
	}
	if err := emit(ctx, em, feeder.UpsertNode(desc, feeder.NewID(desc.SourceID, "owner", labels.Owner), feeder.NodeFact{
		Meta:        feeder.Meta{SourceObservedAt: at},
		Ref:         ownerRef,
		Type:        graphv1.NodeType_OWNER,
		DisplayName: labels.Owner,
		Props:       props,
		ValidAt:     createTime,
	})); err != nil {
		return err
	}
	// The edge's id carries the instant it is asserted from, which is the subject's createTime. A
	// service recreated under its name has its owner edge closed by the retraction's cascade, and
	// the successor's edge — same subject, same owner — was the same id, so it was dropped as a
	// duplicate and the recreated service had no owner (003 T066). The createTime is what differs.
	stamp := "unstamped"
	if !createTime.IsZero() {
		stamp = createTime.UTC().Format(time.RFC3339Nano)
	}
	return emit(ctx, em, feeder.UpsertEdge(desc,
		feeder.NewID(desc.SourceID, "edge", "owned-by", subject.GetValue(), labels.Owner, stamp),
		feeder.EdgeFact{
			Meta:    feeder.Meta{SourceObservedAt: at},
			Src:     subject,
			Dst:     ownerRef,
			Type:    graphv1.EdgeType_OWNED_BY,
			ValidAt: createTime,
		}))
}

// serviceEventID names one dated assertion about a service, not the service.
//
// It carries the uid and the instant the assertion is dated from — `createTime` for the existence
// assertion, `updateTime` for a stated later state — which together act as Cloud Run's version stamp,
// as `resourceVersion` does for Kubernetes (internal/feeders/k8s names its events `<kind>:<key>@rv<n>`
// for the same reason). It used to be the ref alone, and an event id is an idempotency key: every poll
// after the first re-sent the same id, the graph answered each with DUPLICATE_NOOP, and a service's
// node was frozen at its first observation (003 T066).
func serviceEventID(sourceID string, obs ServiceObservation, uid, label string, instant time.Time) string {
	stamp := label + "-unstated"
	if !instant.IsZero() {
		stamp = label + "-" + instant.UTC().Format(time.RFC3339Nano)
	}
	return feeder.NewID(sourceID, "service", obs.Service.Value()+"@"+uid+"@"+stamp)
}

// emitRecreation retracts the old service and records the link as claims, never as a merge (FR-025).
//
// `RetractNode` carries no reason field — a retraction is a statement about valid time and the SDK
// deliberately gives it nowhere to editorialise — so the explanation goes where a reader will
// actually look for it: the next checkpoint's note. Without that, a reader seeing a service retracted
// and immediately recreated has no way to tell a recreation from a deletion followed by a coincidence.
func (f *Feeder) emitRecreation(ctx context.Context, desc feeder.Description, em feeder.Emitter, rec Recreation) error {
	f.mu.Lock()
	f.recreations = append(f.recreations, rec)
	f.mu.Unlock()
	return emit(ctx, em, feeder.RetractNode(desc,
		feeder.NewID(desc.SourceID, "retract", rec.Service.Value(), rec.OldUID),
		feeder.NodeRetraction{
			Ref:      rec.Service.Ref(),
			ValidEnd: rec.RetractOldAt,
		}))
}

// completionFor returns the audit completion entry that dates a split change, or nil.
//
// nil is the **held** case, and it is the ordinary state of a rollout in progress rather than a
// failure: a poll running between the request entry and the completion entry has seen the split
// change and cannot yet say when. Holding says exactly that; emitting at the poll time would put a
// change in the graph at an instant GCP never stated and break SC-002 silently.
func (f *Feeder) completionFor(svc Service, _, _ TrafficSplit) *Completion {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.audit.Take(svc)
}

// authFor returns the authentication evidence for a revision's creation, from the audit entry for the
// service mutation that created it.
//
// There is no `CreateRevision` methodName in v2 (research §2) — revisions are created as a side
// effect of `CreateService`/`UpdateService` — so the *instant* comes from the API's `createTime` and
// the *principal* comes from the log. An empty result yields ACTOR_KIND_UNSPECIFIED, "the source said
// nothing", rather than UNKNOWN, which would claim the feeder looked and could not tell.
func (f *Feeder) authFor(rev Revision) AuthenticationInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.audit.AuthFor(rev.Service)
}

// emit sends one event and turns a rejection into an error naming it, so a fixture that stops
// verifying says which event the graph refused rather than producing a silently short golden.
func emit(ctx context.Context, em feeder.Emitter, ev *graphv1.EventEnvelope) error {
	result, err := em.Emit(ctx, ev)
	if err != nil {
		return err
	}
	if result.GetStatus() == graphv1.IngestResult_REJECTED {
		return fmt.Errorf("gcp: the graph refused event %s: %s %s",
			ev.GetEventId(), result.GetReasonCode(), result.GetReasonDetail())
	}
	return nil
}

// firstOfKind returns the first pointer of a kind, and whether there is one.
func firstOfKind(pointers []*graphv1.Pointer, kind graphv1.PointerKind) (*graphv1.Pointer, bool) {
	for _, pointer := range pointers {
		if pointer.GetKind() == kind {
			return pointer, true
		}
	}
	return nil, false
}

// ObservedServices returns the service identifiers this feeder has seen, sorted. It is what a poll
// marker hands the silence tracker, and what the coverage audit compares against GCP's own list.
func (f *Feeder) ObservedServices() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.splits))
	for value := range f.splits {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
