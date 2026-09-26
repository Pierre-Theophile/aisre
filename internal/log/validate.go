// SPDX-License-Identifier: Apache-2.0

package log

import (
	"fmt"
	"slices"
	"strings"

	"google.golang.org/protobuf/types/known/structpb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
)

// Event validation (FR-009, FR-024, data-model.md "Validation rules").
//
// Validate is a pure function of the envelope: it touches no database, so it can be run by a
// feeder SDK before the event leaves the process, by the ingest server before a transaction is
// opened, and by a fixture linter. Everything that needs to look something up — is the source
// registered, has this idempotency key been seen, does this retraction's ref resolve — is
// decided later, by the log and by the projector, and carries its own reason code.
//
// The rule the denylist enforces is constitution IV: the graph stores *where to look*, never
// the telemetry itself. A metric sample, a log line or a span in a property would turn the
// graph into a second-rate time-series database, so the shapes that telemetry takes are
// refused by name, by shape and by size.

// Reason codes recorded in log.rejected_events.reason_code. The vocabulary is published: a
// feeder matches on these strings, so they are part of the schema (constitution IX) and may
// only change with a version bump.
const (
	// ReasonUntyped is an envelope with no body: the oneof is unset, so there is no event.
	ReasonUntyped = "untyped"
	// ReasonUnknownSchemaVersion is an envelope whose schema_version is not in the accepted
	// set. The rejection detail names the versions that are accepted (edge case "schema
	// version drift").
	ReasonUnknownSchemaVersion = "unknown_schema_version"
	// ReasonMissingValidTime is an upsert, retraction or change with no source-asserted valid
	// time. FR-010: a missing interval is a rejection, and FR-011 forbids guessing one.
	ReasonMissingValidTime = "missing_valid_time"
	// ReasonTelemetryPayload is a property that carries telemetry rather than a pointer to it
	// (FR-009, constitution IV).
	ReasonTelemetryPayload = "telemetry_payload"
	// ReasonSecretValue is a configuration node of kind secret carrying the secret's value
	// instead of only its version identifier (edge case "secrets").
	ReasonSecretValue = "secret_value"
	// ReasonUnknownType is a node or edge type enum left unspecified, or a change with no kind.
	ReasonUnknownType = "unknown_type"
	// ReasonMissingRef is a body that omits an identity it needs: the subject of an upsert, an
	// endpoint of an edge, the claim of an identity assertion.
	ReasonMissingRef = "missing_ref"
	// ReasonCorrelationAsIdentity is an identity claim in a namespace published as a correlation key
	// (internal/graph/correlation.go). The two are different kinds and the storage enforces different
	// rules, so minting one as the other is refused at the log rather than found later as a graph whose
	// state depends on the order events arrived in (004 T148).
	ReasonCorrelationAsIdentity = "correlation_as_identity"
	// ReasonMissingSource is an envelope with no source_id.
	ReasonMissingSource = "missing_source"
	// ReasonMissingEventID is an envelope with no event_id; without one nothing can be
	// idempotent (FR-018).
	ReasonMissingEventID = "missing_event_id"
	// ReasonPropNamespace is a property on an investigation event outside the allow-listed
	// `sre.investigation.*` namespace (ADR-0005 D5, 002 FR-034). Added by the feature 001
	// change package; additive to the published vocabulary, since no existing feeder can
	// produce one of these bodies.
	ReasonPropNamespace = "prop_namespace"

	// ReasonUnknownSource is an envelope from a source that never registered. Decided by the
	// log, not by Validate, because it needs log.sources.
	ReasonUnknownSource = "unknown_source"
	// ReasonRefUnresolvable is a retraction naming an entity the graph has never seen. Decided
	// by the projector: upserts create what they name, retractions may not (data-model.md
	// "Validation rules").
	ReasonRefUnresolvable = "ref_unresolvable"
	// ReasonMissingPrincipal is a human resolution decision that names no authenticated
	// individual. FR-041: "the graph MUST NOT accept a resolution decision from an anonymous
	// or shared credential", so the event is refused rather than recorded without a name.
	// Decided by the projector, which is where the principal from ApplyOptions arrives.
	ReasonMissingPrincipal = "missing_principal"
)

// Rejection is a machine-readable refusal: the published reason code plus the field path that
// caused it, so a feeder author can fix the event without reading this source (FR-024).
type Rejection struct {
	// ReasonCode is one of the Reason* constants.
	ReasonCode string
	// ReasonDetail names the offending field, in proto JSON path form, e.g.
	// "upsert_node.props.latency_samples". It is human-readable and stable enough to assert on.
	ReasonDetail string
}

// Error makes Rejection usable as an error where a caller wants one.
func (r *Rejection) Error() string {
	if r == nil {
		return ""
	}
	if r.ReasonDetail == "" {
		return r.ReasonCode
	}
	return r.ReasonCode + ": " + r.ReasonDetail
}

func reject(code, detailFormat string, args ...any) *Rejection {
	return &Rejection{ReasonCode: code, ReasonDetail: fmt.Sprintf(detailFormat, args...)}
}

// SchemaVersions is the set of event schema versions this build accepts (FR-025). A feeder
// emitting anything else is told which versions are accepted rather than being silently
// ignored.
type SchemaVersions []string

// DefaultSchemaVersions is what the graph accepts today. It is a var, not a const slice, so a
// deployment mid-migration can widen it; it is never narrowed without a major bump.
var DefaultSchemaVersions = SchemaVersions{"1.0.0"}

// Accepts reports whether v is in the set.
func (s SchemaVersions) Accepts(v string) bool { return slices.Contains(s, v) }

// String renders the set for a rejection detail.
func (s SchemaVersions) String() string { return strings.Join(s, ", ") }

// Telemetry denylist (constitution IV, FR-009).
//
// These are the property names a well-meaning feeder reaches for when it wants to "just
// attach the number". Each one is a telemetry payload under a different name, at any depth in
// a property value.
var deniedPropKeys = []string{
	"metric_value",
	"value",
	"log_body",
	"body",
	"span_id",
	"trace_id",
	"samples",
}

const (
	// maxPropBytes is the size above which a property is assumed to be carrying payload
	// rather than describing where payload lives. 4 KiB is generous for a selector and far
	// below anything worth storing as data.
	maxPropBytes = 4 << 10
	// minNumericListLen is the shortest list of numbers treated as a sample series. A pair of
	// numbers is already a series; a single number is a scalar property such as a replica
	// count or a port.
	minNumericListLen = 2
)

// Validate checks env against the published contract and returns nil when it may be appended.
//
// Checks run most-structural first, so the reason a feeder is told is the most useful one: an
// envelope with no body cannot have a bad property, and a configuration secret leaking its
// value is reported as secret_value rather than as the telemetry_payload its `value` key would
// otherwise trip.
func Validate(env *graphv1.EventEnvelope, accepted SchemaVersions) *Rejection {
	if env == nil {
		return reject(ReasonUntyped, "envelope")
	}
	if env.GetEventId() == "" {
		return reject(ReasonMissingEventID, "event_id")
	}
	if env.GetSourceId() == "" {
		return reject(ReasonMissingSource, "source_id")
	}
	if env.GetBody() == nil {
		return reject(ReasonUntyped, "body")
	}
	if len(accepted) > 0 && !accepted.Accepts(env.GetSchemaVersion()) {
		return reject(ReasonUnknownSchemaVersion,
			"schema_version %q; accepted: %s", env.GetSchemaVersion(), accepted)
	}

	switch body := env.GetBody().(type) {
	case *graphv1.EventEnvelope_UpsertNode:
		return validateUpsertNode(body.UpsertNode)
	case *graphv1.EventEnvelope_UpsertEdge:
		return validateUpsertEdge(body.UpsertEdge)
	case *graphv1.EventEnvelope_RetractNode:
		return validateRetractNode(body.RetractNode)
	case *graphv1.EventEnvelope_RetractEdge:
		return validateRetractEdge(body.RetractEdge)
	case *graphv1.EventEnvelope_ObserveChange:
		return validateObserveChange(body.ObserveChange)
	case *graphv1.EventEnvelope_IdentityClaim:
		return validateIdentityClaim(body.IdentityClaim)
	case *graphv1.EventEnvelope_CorrelateEntity:
		return validateCorrelateEntity(body.CorrelateEntity)
	case *graphv1.EventEnvelope_ConfirmMerge:
		return validatePair("confirm_merge", body.ConfirmMerge.GetEntityA(), body.ConfirmMerge.GetEntityB())
	case *graphv1.EventEnvelope_RejectMerge:
		return validatePair("reject_merge", body.RejectMerge.GetEntityA(), body.RejectMerge.GetEntityB())
	case *graphv1.EventEnvelope_ManualMerge:
		return validatePair("manual_merge", body.ManualMerge.GetEntityA(), body.ManualMerge.GetEntityB())
	case *graphv1.EventEnvelope_SplitEntity:
		return validateSplit(body.SplitEntity)
	case *graphv1.EventEnvelope_SourceCheckpoint:
		return nil
	// The feature 001 change package for 002 (ADR-0005 D2/D3).
	case *graphv1.EventEnvelope_RecordInvestigation:
		return validateRecordInvestigation(body.RecordInvestigation)
	case *graphv1.EventEnvelope_SubmitHumanFact:
		return validateSubmitHumanFact(body.SubmitHumanFact)
	case *graphv1.EventEnvelope_ReopenInvestigation:
		return validateReopenInvestigation(body.ReopenInvestigation)
	case *graphv1.EventEnvelope_LabelInvestigation:
		return validateLabelInvestigation(body.LabelInvestigation)
	case *graphv1.EventEnvelope_AlertTransition:
		return validateAlertTransition(body.AlertTransition)
	// 003 plan item 2 (FR-029, FR-122): a proposal that an edge exists, and the two decisions
	// about one.
	case *graphv1.EventEnvelope_ProposeDependency:
		return validateProposeDependency(body.ProposeDependency)
	case *graphv1.EventEnvelope_ConfirmDependency:
		return validateDependencyEnds("confirm_dependency",
			body.ConfirmDependency.GetSrc(), body.ConfirmDependency.GetDst(),
			body.ConfirmDependency.GetType())
	case *graphv1.EventEnvelope_RejectDependency:
		return validateDependencyEnds("reject_dependency",
			body.RejectDependency.GetSrc(), body.RejectDependency.GetDst(),
			body.RejectDependency.GetType())
	default:
		return reject(ReasonUntyped, "body")
	}
}

// validateProposeDependency checks a proposal's shape (003 FR-029).
//
// The score matters more here than it looks. A proposal exists to be reviewed by a person, and its
// score is how the queue is ordered — so a proposal with no score is not a weak proposal, it is an
// unrankable one that will sit at whichever end of the list the sort happens to put it. And a score
// outside [0,1] is a rule reporting in units nothing else in the graph uses.
func validateProposeDependency(p *graphv1.ProposeDependency) *Rejection {
	if r := validateDependencyEnds("propose_dependency", p.GetSrc(), p.GetDst(), p.GetType()); r != nil {
		return r
	}
	if p.GetRuleId() == "" {
		return reject(ReasonMissingRef, "propose_dependency.rule_id (a proposal names the "+
			"published rule that made it, so a reviewer can see what it matched on)")
	}
	if p.GetScore() <= 0 || p.GetScore() > 1 {
		return reject(ReasonUnknownType, "propose_dependency.score is %v; a proposal "+
			"is ranked for review by its score, so it must be in (0,1]", p.GetScore())
	}
	return nil
}

// validateDependencyEnds checks the (src, dst, type) triple every dependency body carries.
//
// The self-dependency check is here rather than only in the table constraint because a rejection
// names the field and the reason, while a constraint violation surfaces as a failed transaction —
// and a feeder reading the published reason codes can act on the first.
func validateDependencyEnds(field string, src, dst *graphv1.Ref, typ graphv1.EdgeType) *Rejection {
	if r := requireRef(field+".src", src); r != nil {
		return r
	}
	if r := requireRef(field+".dst", dst); r != nil {
		return r
	}
	if typ == graphv1.EdgeType_EDGE_TYPE_UNSPECIFIED {
		return reject(ReasonUnknownType, "%s.type", field)
	}
	if src.GetNamespace() == dst.GetNamespace() && src.GetValue() == dst.GetValue() {
		return reject(ReasonMissingRef, "%s names one entity at both ends; a self-dependency "+
			"carries no information", field)
	}
	return nil
}

func validateUpsertNode(n *graphv1.UpsertNode) *Rejection {
	if r := requireRef("upsert_node.ref", n.GetRef()); r != nil {
		return r
	}
	if n.GetType() == graphv1.NodeType_NODE_TYPE_UNSPECIFIED {
		return reject(ReasonUnknownType, "upsert_node.type")
	}
	if n.GetValidAt() == nil && !n.GetValidFromUnknown() {
		return reject(ReasonMissingValidTime,
			"upsert_node.valid_at (set valid_from_unknown to assert an unknown start, never a guessed one)")
	}
	if r := checkSecretValue("upsert_node", n.GetType(), n.GetProps()); r != nil {
		return r
	}
	if r := checkTelemetry("upsert_node.props", n.GetProps()); r != nil {
		return r
	}
	return checkPointers("upsert_node.pointers", n.GetPointers())
}

func validateUpsertEdge(e *graphv1.UpsertEdge) *Rejection {
	if r := requireRef("upsert_edge.src", e.GetSrc()); r != nil {
		return r
	}
	if r := requireRef("upsert_edge.dst", e.GetDst()); r != nil {
		return r
	}
	if e.GetType() == graphv1.EdgeType_EDGE_TYPE_UNSPECIFIED {
		return reject(ReasonUnknownType, "upsert_edge.type")
	}
	if e.GetValidAt() == nil && !e.GetValidFromUnknown() {
		return reject(ReasonMissingValidTime, "upsert_edge.valid_at")
	}
	return checkTelemetry("upsert_edge.props", e.GetProps())
}

func validateRetractNode(n *graphv1.RetractNode) *Rejection {
	if r := requireRef("retract_node.ref", n.GetRef()); r != nil {
		return r
	}
	if n.GetValidEnd() == nil {
		return reject(ReasonMissingValidTime, "retract_node.valid_end")
	}
	return nil
}

func validateRetractEdge(e *graphv1.RetractEdge) *Rejection {
	if r := requireRef("retract_edge.src", e.GetSrc()); r != nil {
		return r
	}
	if r := requireRef("retract_edge.dst", e.GetDst()); r != nil {
		return r
	}
	if e.GetType() == graphv1.EdgeType_EDGE_TYPE_UNSPECIFIED {
		return reject(ReasonUnknownType, "retract_edge.type")
	}
	if e.GetValidEnd() == nil {
		return reject(ReasonMissingValidTime, "retract_edge.valid_end")
	}
	return nil
}

func validateObserveChange(c *graphv1.ObserveChange) *Rejection {
	if r := requireRef("observe_change.ref", c.GetRef()); r != nil {
		return r
	}
	kind := c.GetChange().GetKind()
	if kind == graphv1.ChangeKind_CHANGE_KIND_UNSPECIFIED {
		return reject(ReasonUnknownType, "observe_change.change.kind")
	}
	if kind == graphv1.ChangeKind_CHANGE_KIND_OTHER && c.GetChange().GetKindOther() == "" {
		return reject(ReasonUnknownType,
			"observe_change.change.kind_other (required when kind is CHANGE_KIND_OTHER)")
	}
	// `valid_from_unknown` is honoured here exactly as it is for a node and an edge, and it had not
	// been: the field existed on ObserveChange with 003 FR-069's reason beside it, `feeder.ChangeFact`
	// set it, the projector read it — and this check rejected every event that used it, so it could
	// never arrive. Nothing noticed because no feeder set it on a change until 004's GitHub connector
	// met the case the field was added for: a platform stating a rollout succeeded and not stating when.
	//
	// Guessing is the alternative and it is worse than an error. A change dated at the poll instant
	// reads as a fact about when production moved, and an operator diffing the window would see a
	// rollout at the moment we noticed rather than at the moment it happened.
	if c.GetValidAt() == nil && !c.GetValidFromUnknown() {
		return reject(ReasonMissingValidTime,
			"observe_change.valid_at (set valid_from_unknown to assert an unknown start, never a guessed one)")
	}
	for i, target := range c.GetTargets() {
		if r := requireRef(fmt.Sprintf("observe_change.targets[%d]", i), target); r != nil {
			return r
		}
	}
	// A change carries properties and pointers exactly as a node does, so it is held to the same
	// telemetry rule (constitution IV, FR-009). It was not: this body checked neither, and a change
	// could carry a canary's error-rate series or a failing step's log body and be accepted. A deploy
	// feeder is the connector most tempted to attach one — the platform hands it the measurement next
	// to the change — and 004's `deploy-telemetry-rejection-01` found the gap by trying (T110).
	if r := checkTelemetry("observe_change.props", c.GetProps()); r != nil {
		return r
	}
	return checkPointers("observe_change.pointers", c.GetPointers())
}

// ---------- the investigation bodies (ADR-0005 D2/D3, 002 FR-034, 002 SC-010) ----------
//
// An investigation's decision record is a fact about how the production system was operated, so
// it is an event like any other and it is validated like any other. What is new is *where* its
// properties may live.
//
// InvestigationPropNamespace is an allow-list, not an exemption. Every property on one of these
// bodies has to sit under `sre.investigation.`, and the telemetry denylist then applies inside
// it exactly as it applies everywhere else. The two halves do different jobs and both are
// needed:
//
//   - the namespace is what makes the record self-describing and bounded. A decision record is
//     written by a component that also holds metric digests, log patterns and span exemplars in
//     memory; the one thing stopping a future author attaching "just the series that proves it"
//     under a plausible new key is that there is no plausible new key. `sre.investigation.*` or
//     rejected.
//   - the denylist is what makes it safe inside the namespace: a list of numbers is a series
//     whatever it is called, a 4 KiB property is a payload whatever it is called, and
//     `sre.investigation.value` is refused for the same reason `value` always was.
//
// So a digest (`sre.investigation.recording_digest`, a hex string), a query parameter
// (`sre.investigation.model_config`, a small struct) and a confidence
// (`sre.investigation.hypotheses`, statuses and numbers per hypothesis id) all pass; a decision
// record carrying a series is rejected with `telemetry_payload`, which is what 002 SC-010
// measures.

// InvestigationPropNamespace is the allow-listed prefix. Published: 002 writes under it and
// this is the definition it writes against.
const InvestigationPropNamespace = "sre.investigation."

func validateRecordInvestigation(r *graphv1.RecordInvestigation) *Rejection {
	if r.GetInvestigationId() == "" {
		return reject(ReasonMissingRef, "record_investigation.investigation_id")
	}
	if r.GetStartedAt() == nil {
		return reject(ReasonMissingValidTime, "record_investigation.started_at")
	}
	if r.GetEndedAt() == nil {
		return reject(ReasonMissingValidTime, "record_investigation.ended_at")
	}
	for field, refs := range map[string][]*graphv1.Ref{
		"record_investigation.subjects":        r.GetSubjects(),
		"record_investigation.target_entities": r.GetTargetEntities(),
	} {
		if rejection := requireRefs(field, refs); rejection != nil {
			return rejection
		}
	}
	for field, value := range map[string]*structpb.Struct{
		"record_investigation.hypotheses":   r.GetHypotheses(),
		"record_investigation.spend":        r.GetSpend(),
		"record_investigation.model_config": r.GetModelConfig(),
	} {
		if rejection := checkInvestigationProps(field, value); rejection != nil {
			return rejection
		}
	}
	return nil
}

func validateSubmitHumanFact(f *graphv1.SubmitHumanFact) *Rejection {
	if f.GetInvestigationId() == "" {
		return reject(ReasonMissingRef, "submit_human_fact.investigation_id")
	}
	if strings.TrimSpace(f.GetAuthor()) == "" {
		// FR-041's rule, applied to the human channel: a fact nobody signed is not evidence,
		// and the graph refuses it rather than recording it unattributed.
		return reject(ReasonMissingPrincipal,
			"submit_human_fact.author (a human fact carries the authenticated individual who pushed it)")
	}
	if f.GetSubmittedAt() == nil {
		return reject(ReasonMissingValidTime, "submit_human_fact.submitted_at")
	}
	if strings.TrimSpace(f.GetStatement()) == "" {
		return reject(ReasonMissingRef, "submit_human_fact.statement")
	}
	return requireRefs("submit_human_fact.entities", f.GetEntities())
}

func validateReopenInvestigation(r *graphv1.ReopenInvestigation) *Rejection {
	if r.GetParentInvestigationId() == "" {
		return reject(ReasonMissingRef, "reopen_investigation.parent_investigation_id")
	}
	if r.GetChildInvestigationId() == "" {
		return reject(ReasonMissingRef, "reopen_investigation.child_investigation_id")
	}
	if r.GetChildInvestigationId() == r.GetParentInvestigationId() {
		return reject(ReasonMissingRef,
			"reopen_investigation.child_investigation_id equals the parent; a reopen is a NEW investigation")
	}
	return nil
}

func validateLabelInvestigation(l *graphv1.LabelInvestigation) *Rejection {
	if l.GetInvestigationId() == "" {
		return reject(ReasonMissingRef, "label_investigation.investigation_id")
	}
	if strings.TrimSpace(l.GetAuthor()) == "" {
		return reject(ReasonMissingPrincipal,
			"label_investigation.author (a label is a human judgement and carries the individual who made it)")
	}
	if l.GetLabelledAt() == nil {
		return reject(ReasonMissingValidTime, "label_investigation.labelled_at")
	}
	return nil
}

func validateAlertTransition(a *graphv1.AlertTransition) *Rejection {
	if r := requireRef("alert_transition.monitor", a.GetMonitor()); r != nil {
		return r
	}
	if a.GetTransitionAt() == nil {
		return reject(ReasonMissingValidTime, "alert_transition.transition_at")
	}
	if a.GetToState() == "" {
		return reject(ReasonUnknownType, "alert_transition.to_state")
	}
	return requireRefs("alert_transition.watches", a.GetWatches())
}

func requireRefs(field string, refs []*graphv1.Ref) *Rejection {
	for i, ref := range refs {
		if r := requireRef(fmt.Sprintf("%s[%d]", field, i), ref); r != nil {
			return r
		}
	}
	return nil
}

// checkInvestigationProps enforces the allow-list and then the denylist, in that order, so a
// feeder that put a series under a key of its own invention is told the more useful of the two
// things wrong with it.
func checkInvestigationProps(path string, props *structpb.Struct) *Rejection {
	for _, key := range sortedKeys(props.GetFields()) {
		if !strings.HasPrefix(key, InvestigationPropNamespace) {
			return reject(ReasonPropNamespace,
				"%s.%s is outside the allow-listed %s* namespace; an investigation event carries "+
					"only properties under it", path, key, InvestigationPropNamespace)
		}
	}
	return checkTelemetry(path, props)
}

func validateIdentityClaim(c *graphv1.IdentityClaim) *Rejection {
	if r := requireRef("identity_claim.subject", c.GetSubject()); r != nil {
		return r
	}
	if r := requireRef("identity_claim.claim", c.GetClaim()); r != nil {
		return r
	}
	// A correlation key is not a name, and the two are stored under different rules: identity is
	// unique per (namespace, value, source) and correlation is not. Asserting one as the other is
	// refused here rather than accepted and found later — as an identity it would put a value several
	// entities share on whichever entity was processed first, and C1 would then merge on it
	// (004 T148, internal/graph/correlation.go).
	if ns := c.GetClaim().GetNamespace(); graph.IsCorrelationNamespace(ns) {
		return reject(ReasonCorrelationAsIdentity,
			"identity_claim.claim.namespace %q is published as a correlation key, not a name: "+
				"several entities share one of its values, so emit CorrelateEntity instead", ns)
	}
	return checkTelemetry("identity_claim.attributes", c.GetAttributes())
}

// validateCorrelateEntity checks a correlation (004 T148).
//
// It deliberately does NOT refuse a namespace outside the published correlation list. A new connector
// correlates on things this schema has never heard of — a build id, a vendor's incident number — and a
// registry that had to be edited before a feeder could ship would make the list the bottleneck rather
// than the vocabulary. The refusal runs the other way, on identity claims, because that is the
// direction with a wrong answer: a correlation stored as a name corrupts resolution, and a name stored
// as a correlation merely fails to merge.
func validateCorrelateEntity(c *graphv1.CorrelateEntity) *Rejection {
	if r := requireRef("correlate_entity.subject", c.GetSubject()); r != nil {
		return r
	}
	if r := requireRef("correlate_entity.key", c.GetKey()); r != nil {
		return r
	}
	return checkTelemetry("correlate_entity.attributes", c.GetAttributes())
}

func validatePair(field, a, b string) *Rejection {
	if a == "" {
		return reject(ReasonMissingRef, "%s.entity_a", field)
	}
	if b == "" {
		return reject(ReasonMissingRef, "%s.entity_b", field)
	}
	return nil
}

func validateSplit(s *graphv1.SplitEntity) *Rejection {
	if s.GetEntityId() == "" {
		return reject(ReasonMissingRef, "split_entity.entity_id")
	}
	if len(s.GetDetachClaims()) == 0 {
		return reject(ReasonMissingRef, "split_entity.detach_claims")
	}
	for i, ref := range s.GetDetachClaims() {
		if r := requireRef(fmt.Sprintf("split_entity.detach_claims[%d]", i), ref); r != nil {
			return r
		}
	}
	return nil
}

func requireRef(field string, ref *graphv1.Ref) *Rejection {
	if ref.GetNamespace() == "" || ref.GetValue() == "" {
		return reject(ReasonMissingRef, "%s (namespace and value are both required)", field)
	}
	return nil
}

// checkSecretValue refuses a configuration node that carries the secret itself.
//
// A secret node exists so that "payments depends on this secret, which rotated at 14:02" is
// answerable; the value is the one thing the graph must never learn. The marker is the
// documented property `sre.config.kind = "secret"`, and the offence is any property that would
// hold the material: `data` or `value`, plain or namespaced.
func checkSecretValue(prefix string, typ graphv1.NodeType, props *structpb.Struct) *Rejection {
	if typ != graphv1.NodeType_CONFIG || props == nil {
		return nil
	}
	if !strings.EqualFold(stringProp(props, "sre.config.kind"), "secret") {
		return nil
	}
	for key := range props.GetFields() {
		switch base := key[strings.LastIndex(key, ".")+1:]; base {
		case "data", "value":
			return reject(ReasonSecretValue,
				"%s.props.%s (a secret node carries only its version identifier)", prefix, key)
		default:
		}
	}
	return nil
}

func stringProp(props *structpb.Struct, key string) string {
	v, ok := props.GetFields()[key]
	if !ok {
		return ""
	}
	return v.GetStringValue()
}

// checkTelemetry walks a property struct and refuses anything that looks like telemetry:
// a denylisted key at any depth, a series of numbers, or a value too large to be a selector.
func checkTelemetry(path string, props *structpb.Struct) *Rejection {
	if props == nil {
		return nil
	}
	for _, key := range sortedKeys(props.GetFields()) {
		value := props.GetFields()[key]
		child := path + "." + key
		if r := checkDeniedKey(child, key); r != nil {
			return r
		}
		if size := valueSize(value); size > maxPropBytes {
			return reject(ReasonTelemetryPayload,
				"%s is %d bytes, above the %d byte property limit; store a pointer, not the payload",
				child, size, maxPropBytes)
		}
		if r := checkTelemetryValue(child, value); r != nil {
			return r
		}
	}
	return nil
}

func checkDeniedKey(path, key string) *Rejection {
	base := key[strings.LastIndex(key, ".")+1:]
	if slices.Contains(deniedPropKeys, base) || slices.Contains(deniedPropKeys, key) {
		return reject(ReasonTelemetryPayload,
			"%s is a telemetry payload key; the graph stores pointers, not samples", path)
	}
	return nil
}

// checkTelemetryValue recurses into structs and lists. A list of two or more numbers is a
// sample series whatever it is called, which is what catches the `latency_samples` case the
// baseline fixture ships as its rejection example.
func checkTelemetryValue(path string, value *structpb.Value) *Rejection {
	switch kind := value.GetKind().(type) {
	case *structpb.Value_StructValue:
		for _, key := range sortedKeys(kind.StructValue.GetFields()) {
			child := path + "." + key
			if r := checkDeniedKey(child, key); r != nil {
				return r
			}
			if r := checkTelemetryValue(child, kind.StructValue.GetFields()[key]); r != nil {
				return r
			}
		}
	case *structpb.Value_ListValue:
		items := kind.ListValue.GetValues()
		numbers := 0
		for _, item := range items {
			if _, ok := item.GetKind().(*structpb.Value_NumberValue); ok {
				numbers++
			}
		}
		if numbers >= minNumericListLen && numbers == len(items) {
			return reject(ReasonTelemetryPayload,
				"%s is a list of %d numeric samples; the graph stores pointers, not samples",
				path, numbers)
		}
		for i, item := range items {
			if r := checkTelemetryValue(fmt.Sprintf("%s[%d]", path, i), item); r != nil {
				return r
			}
		}
	default:
	}
	return nil
}

// checkPointers bounds the one field whose whole purpose is to be a query expression. A
// selector longer than the property limit is a payload someone pasted into a selector slot.
func checkPointers(path string, pointers []*graphv1.Pointer) *Rejection {
	for i, p := range pointers {
		if len(p.GetSelector()) > maxPropBytes {
			return reject(ReasonTelemetryPayload,
				"%s[%d].selector is %d bytes, above the %d byte limit",
				path, i, len(p.GetSelector()), maxPropBytes)
		}
	}
	return nil
}

// valueSize is the encoded size of one property value, used as the size limit. It counts the
// JSON shape rather than the Go representation so the limit means the same thing to a feeder
// author reading the event they sent.
func valueSize(v *structpb.Value) int {
	raw, err := v.MarshalJSON()
	if err != nil {
		return 0
	}
	return len(raw)
}

// sortedKeys walks a property map in a fixed order, so the reason a feeder is told for an
// event with two offending keys does not depend on Go's map iteration order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
