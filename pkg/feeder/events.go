// SPDX-License-Identifier: Apache-2.0

package feeder

import (
	"time"

	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
)

// Event constructors (FR-017, FR-018).
//
// Every constructor takes the feeder's Description, the event id, and one struct describing
// the fact. The Description fills in what is the same on every event a feeder ever sends —
// source id, schema version — and the idempotency key defaults to the event id, which is the
// published convention (fixtures/README.md). The fact structs exist so that adding a field to
// the schema does not change a single call site: a new optional field is a new struct member,
// not a new positional argument.
//
// None of them validate. They build an envelope; the Emitter validates it and hands back a
// Rejection naming the field. That keeps one validator in the system rather than two that can
// disagree, and it means a constructor can never fail in the middle of a watch loop where a
// feeder has nowhere to put the error.
//
// Valid time always lives in the body — `validAt`, `validEnd` — never at the envelope level.
// It is the time the fact was true in the production system, as the source asserts it. The
// graph stamps observed time itself at acceptance and ignores anything a feeder says about it
// (FR-019); `Meta.SourceObservedAt` is kept as provenance only.

// Meta is the envelope-level information that is optional on every event.
type Meta struct {
	// Seq is the per-source sequence number, when the source has one (a Kubernetes
	// resourceVersion, a log offset). Zero means none, and is what a feeder declaring
	// OrderingNone always leaves it at.
	Seq int64
	// SourceObservedAt is when the *source* learned the fact. It is kept as provenance and
	// never replaces the observed time the graph assigns (FR-019).
	SourceObservedAt time.Time
	// IdempotencyKey overrides the default, which is the event id. Set it only to make two
	// different event ids collapse onto one delivery (see IdempotencyKey).
	IdempotencyKey string
}

// envelope builds the common half of every event.
func envelope(d Description, eventID string, m Meta) *graphv1.EventEnvelope {
	env := &graphv1.EventEnvelope{
		EventId:        eventID,
		IdempotencyKey: eventID,
		SourceId:       d.SourceID,
		SchemaVersion:  d.Version(),
	}
	if m.IdempotencyKey != "" {
		env.IdempotencyKey = m.IdempotencyKey
	}
	if m.Seq != 0 {
		seq := m.Seq
		env.SourceSeq = &seq
	}
	if !m.SourceObservedAt.IsZero() {
		env.SourceObservedAt = timestamppb.New(m.SourceObservedAt.UTC())
	}
	return env
}

// NodeFact is what a feeder asserts about one entity from one instant onwards.
type NodeFact struct {
	Meta
	// Ref is the identity the source knows the entity by, e.g.
	// Ref(NSK8sDeployment, "shop/checkout"). Required.
	Ref *graphv1.Ref
	// Type is the node type. Required: an unspecified type is refused (ReasonUnknownType).
	Type graphv1.NodeType
	// DisplayName is what an operator should see. Empty is allowed; the graph falls back to
	// the ref value.
	DisplayName string
	// Props are the entity's properties, from a Props builder. Nil is allowed.
	Props *structpb.Struct
	// Pointers say where the telemetry about this entity lives (constitution IV).
	Pointers []*graphv1.Pointer
	// ValidAt is when the asserted state became true in the production system. Required
	// unless ValidFromUnknown.
	ValidAt time.Time
	// ValidFromUnknown asserts that the state is true now but its start is genuinely unknown
	// — a workload that already existed when the feeder first listed it. It is the only
	// alternative to ValidAt; guessing a start is a defect (FR-011).
	ValidFromUnknown bool
}

// UpsertNode builds an assertion about an entity.
func UpsertNode(d Description, eventID string, fact NodeFact) *graphv1.EventEnvelope {
	env := envelope(d, eventID, fact.Meta)
	env.Body = &graphv1.EventEnvelope_UpsertNode{UpsertNode: &graphv1.UpsertNode{
		Ref:              fact.Ref,
		Type:             fact.Type,
		DisplayName:      fact.DisplayName,
		Props:            fact.Props,
		Pointers:         fact.Pointers,
		ValidAt:          timestampOrNil(fact.ValidAt),
		ValidFromUnknown: fact.ValidFromUnknown,
	}}
	return env
}

// EdgeFact is what a feeder asserts about one relationship from one instant onwards.
type EdgeFact struct {
	Meta
	// Src and Dst are the endpoints, in the direction the edge type reads: a `calls` edge
	// points from the caller to the callee, a `runs-on` edge from the workload to the
	// infrastructure, a `changed-by` edge from the target to the change.
	Src, Dst *graphv1.Ref
	// Type is the edge type. Required.
	Type graphv1.EdgeType
	// Props are the relationship's properties, typically PropWindowSeconds for an edge
	// derived from an aggregation window.
	Props *structpb.Struct
	// WeightClass is the traffic class, for `calls` edges only (research §8). Nil on every
	// other edge type: "not applicable" and "class 0" are different statements. Build it with
	// WeightClassPtr(WeightClass(rps)).
	WeightClass *uint32
	// ValidAt is when the relationship became true. Required unless ValidFromUnknown.
	ValidAt time.Time
	// ValidFromUnknown asserts a genuinely unknown start (FR-011).
	ValidFromUnknown bool
}

// UpsertEdge builds an assertion about a relationship.
func UpsertEdge(d Description, eventID string, fact EdgeFact) *graphv1.EventEnvelope {
	env := envelope(d, eventID, fact.Meta)
	env.Body = &graphv1.EventEnvelope_UpsertEdge{UpsertEdge: &graphv1.UpsertEdge{
		Src:              fact.Src,
		Dst:              fact.Dst,
		Type:             fact.Type,
		Props:            fact.Props,
		WeightClass:      fact.WeightClass,
		ValidAt:          timestampOrNil(fact.ValidAt),
		ValidFromUnknown: fact.ValidFromUnknown,
	}}
	return env
}

// NodeRetraction ends an entity's valid interval: the thing stopped existing.
type NodeRetraction struct {
	Meta
	// Ref is the entity. The graph refuses a retraction of something it has never seen
	// (ReasonRefUnresolvable), so retract only what you have upserted.
	Ref *graphv1.Ref
	// ValidEnd is when it stopped being true. Required; retraction is a statement about
	// valid time, not about the feeder noticing.
	ValidEnd time.Time
}

// RetractNode builds the end of an entity's valid interval. Nothing is deleted: the entity
// stays queryable as of any instant inside its interval (constitution II).
func RetractNode(d Description, eventID string, fact NodeRetraction) *graphv1.EventEnvelope {
	env := envelope(d, eventID, fact.Meta)
	env.Body = &graphv1.EventEnvelope_RetractNode{RetractNode: &graphv1.RetractNode{
		Ref:      fact.Ref,
		ValidEnd: timestampOrNil(fact.ValidEnd),
	}}
	return env
}

// EdgeRetraction ends a relationship's valid interval.
type EdgeRetraction struct {
	Meta
	// Src, Dst and Type identify the relationship, exactly as the UpsertEdge that asserted it
	// did.
	Src, Dst *graphv1.Ref
	Type     graphv1.EdgeType
	// ValidEnd is when the relationship stopped being true. For an edge derived from
	// aggregation windows this is the end of the last window it was observed in, never the
	// moment the feeder gave up waiting (FR-042).
	ValidEnd time.Time
}

// RetractEdge builds the end of a relationship's valid interval.
func RetractEdge(d Description, eventID string, fact EdgeRetraction) *graphv1.EventEnvelope {
	env := envelope(d, eventID, fact.Meta)
	env.Body = &graphv1.EventEnvelope_RetractEdge{RetractEdge: &graphv1.RetractEdge{
		Src:      fact.Src,
		Dst:      fact.Dst,
		Type:     fact.Type,
		ValidEnd: timestampOrNil(fact.ValidEnd),
	}}
	return env
}

// ChangeFact is something that happened to the production system. Change is a first-class node
// type, not an annotation (constitution, Definitions): it is what a diff ranks and what an
// investigation is looking for.
type ChangeFact struct {
	Meta
	// Ref identifies the change itself in its origin system, e.g.
	// Ref(NSK8sChange, "shop/payments@rev7"). Required, and deterministic: the same rollout
	// observed twice must produce the same ref.
	Ref *graphv1.Ref
	// Kind is the change kind. Required. Use graphv1.ChangeKind_CHANGE_KIND_OTHER with
	// KindOther for a kind the schema does not name yet.
	Kind graphv1.ChangeKind
	// KindOther names the kind when Kind is CHANGE_KIND_OTHER. Required in that case.
	KindOther string
	// Summary is one human-readable line: "rollout inventory to revision 8 (0.9.8)".
	Summary string
	// Actor is who or what performed it, when known: a CI bot, a named operator.
	Actor string
	// ActorKind is the typed reading of Actor (ADR-0005 D1). Leave it
	// ACTOR_KIND_UNSPECIFIED — the zero value — when the source said nothing about who acted:
	// the field is then omitted by canonical serialisation and the graph records no claim
	// about it. ACTOR_KIND_UNKNOWN is a different statement, and a stronger one: the feeder
	// *observed* an actor and could not classify it. Never guess either (plan §I1).
	ActorKind graphv1.ActorKind
	// OriginRef links back to the change in its origin system — a pipeline run, a commit, a
	// ticket.
	OriginRef string
	// Targets are the entities the change was applied to. They become `changed-by` edges from
	// the target to the change, which is what puts a rollout at hop 0 of the service it
	// changed.
	Targets []*graphv1.Ref
	// ValidAt is when the change happened. Required unless ValidFromUnknown.
	ValidAt time.Time
	// ValidFromUnknown asserts that the change is real and its start is genuinely unknown — a vendor
	// announcing a deprecation "in a future release" (003 FR-069). The graph then starts the interval
	// at the observation and marks it unknown, which is the earliest instant anybody can show.
	//
	// It is the only alternative to ValidAt. Leaving both unset is a change dated at the zero instant,
	// which is worse than an error: an ancient change ranks as maximally distant, is never excluded as
	// a future announcement, and looks like a fact.
	ValidFromUnknown bool
	// ValidEnd bounds a change that had a duration — a cloud maintenance window, a migration.
	// Leave it zero for a point-in-time change; the graph gives those the shortest non-empty
	// interval so they do not intersect every future diff window (research §4).
	ValidEnd time.Time
	// AnnouncementState is set only on a change that is an **announcement** — a fact whose valid
	// time begins after its observed time. Leave it ANNOUNCEMENT_STATE_UNSPECIFIED — the zero value
	// — for an ordinary observed change: unspecified is not a synonym for ANNOUNCED, canonical
	// serialisation omits it, and every change recorded before this field existed keeps serialising
	// exactly as it did.
	//
	// A notice defaults to ANNOUNCED and is never promoted to CONFIRMED without an observation that
	// the window happened (003 FR-066). A cancellation or a reschedule is a *correction of the same
	// change*, so it carries CANCELLED or SUPERSEDED on the same ref rather than a second node.
	AnnouncementState graphv1.AnnouncementState
	// Rollback marks a rollout that restored a previous version (004 FR-016).
	//
	// It is a marker on a ROLLOUT and not a change kind of its own, because a rollback is a rollout
	// in every other respect: it moves production to a version, it has an actor and it has an
	// instant. A separate kind would make every consumer that ranks rollouts remember to include it,
	// and the first one to forget would stop ranking the change an incident is most often about.
	//
	// Set it only on an OBSERVATION that a rollback happened. A platform reporting that a deployment
	// *could* be rolled back to — Vercel's `isRollbackCandidate` — is not one: candidacy says the
	// button exists, not that anybody pressed it.
	Rollback bool
	// RolledBackTo names the deployment or version production was restored to, in the source's own
	// spelling. Leave it empty when the platform says a rollback happened without saying what it
	// went back to; empty with Rollback set reads as "a rollback whose target we have not been told",
	// which is a different statement from "not a rollback" and stays distinguishable from it.
	RolledBackTo string
	// RolledBackFrom names the deployment or version production was moved AWAY from, spelled as this
	// source names its rollout of that deployment, and empty when the platform does not say (004 T155).
	// It is what lets an investigation treat the rollback as an operator's judgement about that
	// deployment. Leave it empty on anything that is not a rollback.
	RolledBackFrom string
	// Props are what is true of the CHANGE rather than of the thing it changed: which environment a
	// rollout went to, whether it moved production traffic, which rung of an actor ladder decided and
	// on what evidence, the instant a rollout was later superseded.
	//
	// An identifier another source might also know the change by does NOT belong here — that is an
	// IdentityFact, so a published resolution rule can key on it. A property no rule can read is a
	// property no rule can merge on.
	//
	// Build it with NewProps, which refuses a value the graph cannot store.
	Props *structpb.Struct
	// Pointers are where to look for this change in the system it came from: the pipeline run, the
	// deployment record, the job logs. Never the telemetry itself (constitution IV).
	Pointers []*graphv1.Pointer
}

// ObserveChange builds a change observation.
func ObserveChange(d Description, eventID string, fact ChangeFact) *graphv1.EventEnvelope {
	env := envelope(d, eventID, fact.Meta)
	env.Body = &graphv1.EventEnvelope_ObserveChange{ObserveChange: &graphv1.ObserveChange{
		Ref: fact.Ref,
		Change: &graphv1.Change{
			Kind:              fact.Kind,
			KindOther:         fact.KindOther,
			Summary:           fact.Summary,
			Actor:             fact.Actor,
			ActorKind:         fact.ActorKind,
			OriginRef:         fact.OriginRef,
			AnnouncementState: fact.AnnouncementState,
			Rollback:          fact.Rollback,
			RolledBackTo:      fact.RolledBackTo,
			RolledBackFrom:    fact.RolledBackFrom,
		},
		Targets:          fact.Targets,
		ValidAt:          timestampOrNil(fact.ValidAt),
		ValidFromUnknown: fact.ValidFromUnknown,
		ValidEnd:         timestampOrNil(fact.ValidEnd),
		Props:            fact.Props,
		Pointers:         fact.Pointers,
	}}
	return env
}

// CorrelationFact is one value a feeder knows several entities may share (004 T148).
//
// The counterpart of IdentityFact, and see CorrelationKey for which of the two a given value is. In
// one line: a name identifies, a correlation describes.
type CorrelationFact struct {
	Meta
	// Subject is the entity the key is on.
	Subject *graphv1.Ref
	// Key is the shared value.
	Key *graphv1.Ref
	// Attributes are what a rule corroborates with — the deployment environment, what the value was
	// read from. A correlation alone never merges anything.
	Attributes *structpb.Struct
}

// Correlate emits one correlation key.
func Correlate(d Description, eventID string, fact CorrelationFact) *graphv1.EventEnvelope {
	env := envelope(d, eventID, fact.Meta)
	env.Body = &graphv1.EventEnvelope_CorrelateEntity{CorrelateEntity: &graphv1.CorrelateEntity{
		Subject:    fact.Subject,
		Key:        fact.Key,
		Attributes: fact.Attributes,
	}}
	return env
}

// IdentityFact is one external identifier a feeder knows an entity by.
//
// Emit one for *every* identifier you know about an entity, including the one you address it
// by, and let the graph resolve (contracts/feeder-sdk.md rule 4). A feeder must never merge
// two identities itself: it has no way to record why, and constitution VI requires every merge
// to carry its rule, its score and its rationale.
type IdentityFact struct {
	Meta
	// Subject is the entity, in the namespace this feeder addresses it by.
	Subject *graphv1.Ref
	// Claim is the identifier being asserted about it — possibly the same ref as Subject,
	// which is how a feeder registers its own naming with the resolver.
	Claim *graphv1.Ref
	// Attributes are the supporting facts a rule needs to decide: environment, Kubernetes
	// namespace, service namespace, and PropK8sClaimKey when the claim came from a label or
	// an annotation. Build them with a Props builder.
	Attributes *structpb.Struct
}

// IdentityClaim builds an identity assertion.
func IdentityClaim(d Description, eventID string, fact IdentityFact) *graphv1.EventEnvelope {
	env := envelope(d, eventID, fact.Meta)
	env.Body = &graphv1.EventEnvelope_IdentityClaim{IdentityClaim: &graphv1.IdentityClaim{
		Subject:    fact.Subject,
		Claim:      fact.Claim,
		Attributes: fact.Attributes,
	}}
	return env
}

// CheckpointFact records the extent of what a feeder has observed.
//
// It is what distinguishes "nothing happened" from "nobody was watching" (FR-032), and a
// feeder emits one on start, on resync and after any gap. Prefer Emitter.Checkpoint, which
// mints the deterministic event id for you.
type CheckpointFact struct {
	Meta
	// ExtentFrom and ExtentTo bound the observed window, half-open.
	ExtentFrom, ExtentTo time.Time
	// GapBefore marks that the feeder was not watching immediately before ExtentFrom — a
	// restart, a dropped watch, an expired resource version.
	GapBefore bool
	// Note is a human-readable explanation, e.g. "initial informer list complete for
	// namespace shop".
	Note string
}

// SourceCheckpoint builds a checkpoint event.
func SourceCheckpoint(d Description, eventID string, fact CheckpointFact) *graphv1.EventEnvelope {
	env := envelope(d, eventID, fact.Meta)
	env.Body = &graphv1.EventEnvelope_SourceCheckpoint{SourceCheckpoint: &graphv1.SourceCheckpoint{
		ExtentFrom: timestampOrNil(fact.ExtentFrom),
		ExtentTo:   timestampOrNil(fact.ExtentTo),
		GapBefore:  fact.GapBefore,
		Note:       fact.Note,
	}}
	return env
}

// CheckpointID is the deterministic event id of a checkpoint whose extent ends at to:
// `<source_id>:ckpt:<YYYYMMDDTHHMMSSZ>`. Every Emitter in this SDK uses it, so two checkpoints
// covering the same instant are one event however they were produced.
func CheckpointID(d Description, to time.Time) string {
	return NewID(d.SourceID, "ckpt", to.UTC().Format("20060102T150405Z"))
}

// timestampOrNil renders a time, leaving an unset one absent rather than encoding the zero
// instant — which the schema reads as "no valid time asserted" and refuses.
func timestampOrNil(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t.UTC())
}

// AlertFact is one alert transition: a monitor entering a state at an instant.
//
// It is the published `alert.transition` body (001 event body 25), and the reason it is in this
// SDK rather than in one connector is the property the whole intake rests on: a vendor's poll, a
// vendor's webhook and a human declaration in a chat channel are **one event shape and one
// idempotency key**, so the engine sees one trigger per incident rather than three shapes to
// reconcile (ADR-0003 D10, ADR-0004 D4, ADR-0005 D2).
//
// The key is derived from `(source, monitor ref, group key, transition instant)` and is not a
// field here, because a connector that had to compute it could compute it differently. Set
// Meta.IdempotencyKey only to override; leaving it empty is the normal case and the right one.
//
// Two rules a connector must apply before it calls this, both published in internal/log/alert.go
// so that every connector applies the same one:
//
//   - **filter flapping and no_data** with SuppressAlertTransition. A monitor oscillating on its
//     threshold is noise, and a monitor that stopped reporting is a fact about the monitor rather
//     than about the system it watches;
//   - **keep recoveries.** `alert → ok` bounds the outage and is what distinguishes "this is
//     still happening" from "this healed itself at 14:31". A connector that drops it leaves an
//     investigation unable to tell whether the change it is looking at was the fix.
type AlertFact struct {
	Meta
	// Monitor is the alert entity, on its STABLE identifier — a policy id, never a
	// per-notification id, which changes on every delivery. For a human declaration it is the
	// place the incident lives. Required.
	Monitor *graphv1.Ref
	// GroupKey is the alerting group within the monitor, empty when it has none. A policy that
	// opens one incident per grouped resource produces one transition per group, and the group
	// key is what keeps them separate — including in the idempotency key.
	GroupKey string
	// TransitionAt is the instant the vendor reports for the transition, never the instant this
	// connector learned of it. Required: it is a third of the idempotency key, and a transition
	// dated at the poll would make two transports produce two events for one fact.
	TransitionAt time.Time
	// FromState and ToState are from the published vocabulary: ok | warn | alert | no_data |
	// declared. A state a connector cannot map is `no_data`, which the filter drops.
	FromState string
	ToState   string
	// Watches are the entities the monitor watches. They become WATCHES edges from the alert,
	// and one naming an entity the graph has not seen yet is kept and marked unattached rather
	// than dropped or given a placeholder (003 FR-048).
	Watches []*graphv1.Ref
	// Transport is how this delivery arrived: poll | webhook | human_declared. It is recorded
	// because it is what says whether the history around it is complete.
	Transport string
	// OriginRef links back to the incident in the vendor's console.
	OriginRef string
	// ActorKind is PERSON for a human declaration and unspecified for a monitor.
	ActorKind graphv1.ActorKind
	// Severity and Title are as the vendor or the declarer stated them, never inferred.
	Severity string
	Title    string
	// DeclaringIdentity is the authenticated principal behind a human declaration.
	DeclaringIdentity string
	// Sampled says this history is a sequence of observations at SampledInterval rather than a
	// complete record (003 FR-051). A webhook-delivered transition is complete; a polled one is
	// not, and any transition that opened and closed between two polls is invisible. Without
	// the marker a gap in a polled history reads as "the alert did not fire" when it means "we
	// were not looking at the moment it did".
	Sampled bool
	// SampledInterval is the poll interval in force when this transition was read. It is unset
	// for a webhook or a declaration, where the history is complete.
	SampledInterval time.Duration
}

// ObserveAlertTransition builds an alert transition.
func ObserveAlertTransition(d Description, eventID string, fact AlertFact) *graphv1.EventEnvelope {
	env := envelope(d, eventID, fact.Meta)
	env.Body = &graphv1.EventEnvelope_AlertTransition{AlertTransition: &graphv1.AlertTransition{
		Monitor:                fact.Monitor,
		GroupKey:               fact.GroupKey,
		TransitionAt:           timestampOrNil(fact.TransitionAt),
		FromState:              fact.FromState,
		ToState:                fact.ToState,
		Watches:                fact.Watches,
		Transport:              fact.Transport,
		OriginRef:              fact.OriginRef,
		ActorKind:              fact.ActorKind,
		Severity:               fact.Severity,
		Title:                  fact.Title,
		DeclaringIdentity:      fact.DeclaringIdentity,
		Sampled:                fact.Sampled,
		SampledIntervalSeconds: int64(fact.SampledInterval / time.Second),
	}}
	return env
}

// The published transports of an alert transition. They are spelled here as well as in the
// engine's intake so that a connector does not have to import the engine to name one.
const (
	// TransportPoll is a transition read by polling the vendor. Its history is SAMPLED.
	TransportPoll = "poll"
	// TransportWebhook is a transition the vendor pushed. Its history is complete.
	TransportWebhook = "webhook"
	// TransportHumanDeclared is a person declaring an incident.
	TransportHumanDeclared = "human_declared"
)

// DependencyProposal is suggestive-but-insufficient evidence that one entity depends on another
// (003 FR-029, event body 26).
//
// It exists because the alternatives are both worse than a stated gap. A feeder that finds a Cloud
// SQL instance nothing in a service's configuration names, but which the service plausibly uses,
// can invent a `depends-on` edge — poisoning every blast radius that traverses it — or drop the
// evidence, in which case nobody ever sees what it found. A proposal is neither: it is recorded,
// ranked and reviewable, and **it is never an edge until a human confirms it**.
//
// The discipline that makes the queue readable is the feeder's, not the graph's: raise a proposal
// **once per cycle** and do not re-raise it while it is pending. A connector that emits the same
// weak evidence every minute produces a list nobody can read, which is indistinguishable from not
// having raised anything.
type DependencyProposal struct {
	Meta
	// Src and Dst are the two entities. They are different entities — that is what separates this
	// from an identity claim, which says two refs name one entity. Required, and a proposal naming
	// one entity at both ends is refused.
	Src *graphv1.Ref
	Dst *graphv1.Ref
	// Type is the edge being proposed, and never created here. Required.
	Type graphv1.EdgeType
	// Score is the proposing rule's published confidence, in (0,1]. Required: the score is how the
	// review queue is ordered, so a proposal without one is not a weak proposal but an unrankable
	// one that sits at whichever end of the list the sort happens to put it.
	Score float64
	// RuleID names the published rule that proposed it, so a reviewer can look up what it matched
	// on rather than taking the score on trust. Required.
	RuleID string
	// Rationale is one line a human can act on.
	Rationale string
	// Evidence is the material the rule saw, so a reviewer decides from the same material rather
	// than from the score alone (constitution V: a claim resting on nothing is not a claim).
	Evidence *structpb.Struct
}

// ProposeDependency builds a dependency proposal.
func ProposeDependency(d Description, eventID string, fact DependencyProposal) *graphv1.EventEnvelope {
	env := envelope(d, eventID, fact.Meta)
	env.Body = &graphv1.EventEnvelope_ProposeDependency{ProposeDependency: &graphv1.ProposeDependency{
		Src:       fact.Src,
		Dst:       fact.Dst,
		Type:      fact.Type,
		Score:     fact.Score,
		RuleId:    fact.RuleID,
		Rationale: fact.Rationale,
		Evidence:  fact.Evidence,
	}}
	return env
}
