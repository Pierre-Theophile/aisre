// SPDX-License-Identifier: Apache-2.0

package event

import (
	"fmt"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
)

// Source is a feeder's declared contract, as registered in log.sources.
type Source struct {
	// SourceID is the feeder's identity, e.g. "k8s:prod-eu1".
	SourceID string
	// Kind is the connector family: "k8s", "otel", and later vendors.
	Kind string
	// Ordering is "per_source_sequence" when the feeder numbers its events, "none" otherwise.
	Ordering string
	// ReorderingWindow is how far out of order the feeder may deliver (research §5). The
	// projector never reorders; this is what the shuffle check in FR-048 permutes within.
	ReorderingWindow time.Duration
	// SchemaVersion is the event schema version the feeder emits.
	SchemaVersion string
}

// Type is the log.events.type value for an envelope: the snake_case name of the body
// oneof field, which is exactly the vocabulary the events_type_check constraint allows.
func Type(env *graphv1.EventEnvelope) string {
	switch env.GetBody().(type) {
	case *graphv1.EventEnvelope_UpsertNode:
		return "upsert_node"
	case *graphv1.EventEnvelope_UpsertEdge:
		return "upsert_edge"
	case *graphv1.EventEnvelope_RetractNode:
		return "retract_node"
	case *graphv1.EventEnvelope_RetractEdge:
		return "retract_edge"
	case *graphv1.EventEnvelope_ObserveChange:
		return "observe_change"
	case *graphv1.EventEnvelope_IdentityClaim:
		return "identity_claim"
	case *graphv1.EventEnvelope_CorrelateEntity:
		return "correlate_entity"
	case *graphv1.EventEnvelope_ConfirmMerge:
		return "confirm_merge"
	case *graphv1.EventEnvelope_RejectMerge:
		return "reject_merge"
	case *graphv1.EventEnvelope_SplitEntity:
		return "split_entity"
	case *graphv1.EventEnvelope_ManualMerge:
		return "manual_merge"
	case *graphv1.EventEnvelope_SourceCheckpoint:
		return "source_checkpoint"
	// The feature 001 change package for 002 (ADR-0005 D2/D3). The vocabulary the
	// events_type_check constraint accepts was widened to match in migration 0005.
	case *graphv1.EventEnvelope_RecordInvestigation:
		return "record_investigation"
	case *graphv1.EventEnvelope_SubmitHumanFact:
		return "submit_human_fact"
	case *graphv1.EventEnvelope_ReopenInvestigation:
		return "reopen_investigation"
	case *graphv1.EventEnvelope_LabelInvestigation:
		return "label_investigation"
	case *graphv1.EventEnvelope_AlertTransition:
		return "alert_transition"
	// 003 plan item 2 (FR-029). The vocabulary events_type_check accepts was widened to match
	// in migration 0009.
	case *graphv1.EventEnvelope_ProposeDependency:
		return "propose_dependency"
	case *graphv1.EventEnvelope_ConfirmDependency:
		return "confirm_dependency"
	case *graphv1.EventEnvelope_RejectDependency:
		return "reject_dependency"
	// ADR-0015 (feature 009): the service catalog and attribution. Migration 0015 widens
	// events_type_check to match.
	case *graphv1.EventEnvelope_ProposeCatalogEntry:
		return "propose_catalog_entry"
	case *graphv1.EventEnvelope_DeclareCatalogEntry:
		return "declare_catalog_entry"
	case *graphv1.EventEnvelope_DeclareAlias:
		return "declare_alias"
	case *graphv1.EventEnvelope_RetractAlias:
		return "retract_alias"
	case *graphv1.EventEnvelope_ProposeAlias:
		return "propose_alias"
	case *graphv1.EventEnvelope_RejectAlias:
		return "reject_alias"
	case *graphv1.EventEnvelope_AttributeSignal:
		return "attribute_signal"
	case *graphv1.EventEnvelope_CorrectAttribution:
		return "correct_attribution"
	case *graphv1.EventEnvelope_RevertCorrection:
		return "revert_correction"
	default:
		return ""
	}
}

// BodyMessage returns the envelope's body as a protobuf message, or nil when the oneof is
// unset. It is exported because every consumer of a log row needs the same switch.
func BodyMessage(env *graphv1.EventEnvelope) proto.Message {
	switch body := env.GetBody().(type) {
	case *graphv1.EventEnvelope_UpsertNode:
		return body.UpsertNode
	case *graphv1.EventEnvelope_UpsertEdge:
		return body.UpsertEdge
	case *graphv1.EventEnvelope_RetractNode:
		return body.RetractNode
	case *graphv1.EventEnvelope_RetractEdge:
		return body.RetractEdge
	case *graphv1.EventEnvelope_ObserveChange:
		return body.ObserveChange
	case *graphv1.EventEnvelope_IdentityClaim:
		return body.IdentityClaim
	case *graphv1.EventEnvelope_CorrelateEntity:
		return body.CorrelateEntity
	case *graphv1.EventEnvelope_ConfirmMerge:
		return body.ConfirmMerge
	case *graphv1.EventEnvelope_RejectMerge:
		return body.RejectMerge
	case *graphv1.EventEnvelope_SplitEntity:
		return body.SplitEntity
	case *graphv1.EventEnvelope_ManualMerge:
		return body.ManualMerge
	case *graphv1.EventEnvelope_SourceCheckpoint:
		return body.SourceCheckpoint
	case *graphv1.EventEnvelope_RecordInvestigation:
		return body.RecordInvestigation
	case *graphv1.EventEnvelope_SubmitHumanFact:
		return body.SubmitHumanFact
	case *graphv1.EventEnvelope_ReopenInvestigation:
		return body.ReopenInvestigation
	case *graphv1.EventEnvelope_LabelInvestigation:
		return body.LabelInvestigation
	case *graphv1.EventEnvelope_AlertTransition:
		return body.AlertTransition
	case *graphv1.EventEnvelope_ProposeDependency:
		return body.ProposeDependency
	case *graphv1.EventEnvelope_ConfirmDependency:
		return body.ConfirmDependency
	case *graphv1.EventEnvelope_RejectDependency:
		return body.RejectDependency
	case *graphv1.EventEnvelope_ProposeCatalogEntry:
		return body.ProposeCatalogEntry
	case *graphv1.EventEnvelope_DeclareCatalogEntry:
		return body.DeclareCatalogEntry
	case *graphv1.EventEnvelope_DeclareAlias:
		return body.DeclareAlias
	case *graphv1.EventEnvelope_RetractAlias:
		return body.RetractAlias
	case *graphv1.EventEnvelope_ProposeAlias:
		return body.ProposeAlias
	case *graphv1.EventEnvelope_RejectAlias:
		return body.RejectAlias
	case *graphv1.EventEnvelope_AttributeSignal:
		return body.AttributeSignal
	case *graphv1.EventEnvelope_CorrectAttribution:
		return body.CorrectAttribution
	case *graphv1.EventEnvelope_RevertCorrection:
		return body.RevertCorrection
	default:
		return nil
	}
}

// SetBody rebuilds the oneof from the type and canonical payload of a stored event. The type column and
// the payload are written together in one INSERT, so a row that does not round-trip here is a
// corrupted log, not a schema drift; it is reported as such rather than skipped.
func SetBody(env *graphv1.EventEnvelope, eventType string, payload []byte) error {
	unmarshal := protojson.UnmarshalOptions{DiscardUnknown: false}
	decode := func(m proto.Message) error {
		if err := unmarshal.Unmarshal(payload, m); err != nil {
			return fmt.Errorf("event: decode %s payload of %s: %w", eventType, env.GetEventId(), err)
		}
		return nil
	}
	switch eventType {
	case "upsert_node":
		body := &graphv1.UpsertNode{}
		if err := decode(body); err != nil {
			return err
		}
		env.Body = &graphv1.EventEnvelope_UpsertNode{UpsertNode: body}
	case "upsert_edge":
		body := &graphv1.UpsertEdge{}
		if err := decode(body); err != nil {
			return err
		}
		env.Body = &graphv1.EventEnvelope_UpsertEdge{UpsertEdge: body}
	case "retract_node":
		body := &graphv1.RetractNode{}
		if err := decode(body); err != nil {
			return err
		}
		env.Body = &graphv1.EventEnvelope_RetractNode{RetractNode: body}
	case "retract_edge":
		body := &graphv1.RetractEdge{}
		if err := decode(body); err != nil {
			return err
		}
		env.Body = &graphv1.EventEnvelope_RetractEdge{RetractEdge: body}
	case "observe_change":
		body := &graphv1.ObserveChange{}
		if err := decode(body); err != nil {
			return err
		}
		env.Body = &graphv1.EventEnvelope_ObserveChange{ObserveChange: body}
	case "identity_claim":
		body := &graphv1.IdentityClaim{}
		if err := decode(body); err != nil {
			return err
		}
		env.Body = &graphv1.EventEnvelope_IdentityClaim{IdentityClaim: body}
	case "confirm_merge":
		body := &graphv1.ConfirmMerge{}
		if err := decode(body); err != nil {
			return err
		}
		env.Body = &graphv1.EventEnvelope_ConfirmMerge{ConfirmMerge: body}
	case "reject_merge":
		body := &graphv1.RejectMerge{}
		if err := decode(body); err != nil {
			return err
		}
		env.Body = &graphv1.EventEnvelope_RejectMerge{RejectMerge: body}
	case "split_entity":
		body := &graphv1.SplitEntity{}
		if err := decode(body); err != nil {
			return err
		}
		env.Body = &graphv1.EventEnvelope_SplitEntity{SplitEntity: body}
	case "manual_merge":
		body := &graphv1.ManualMerge{}
		if err := decode(body); err != nil {
			return err
		}
		env.Body = &graphv1.EventEnvelope_ManualMerge{ManualMerge: body}
	case "source_checkpoint":
		body := &graphv1.SourceCheckpoint{}
		if err := decode(body); err != nil {
			return err
		}
		env.Body = &graphv1.EventEnvelope_SourceCheckpoint{SourceCheckpoint: body}
	// The feature 001 change package for 002 (ADR-0005 D2/D3). A replay reads every row back
	// through here, so a type missing from this switch is a type that cannot be replayed —
	// which is the same thing as a type that cannot be stored (constitution III).
	case "record_investigation":
		body := &graphv1.RecordInvestigation{}
		if err := decode(body); err != nil {
			return err
		}
		env.Body = &graphv1.EventEnvelope_RecordInvestigation{RecordInvestigation: body}
	case "submit_human_fact":
		body := &graphv1.SubmitHumanFact{}
		if err := decode(body); err != nil {
			return err
		}
		env.Body = &graphv1.EventEnvelope_SubmitHumanFact{SubmitHumanFact: body}
	case "reopen_investigation":
		body := &graphv1.ReopenInvestigation{}
		if err := decode(body); err != nil {
			return err
		}
		env.Body = &graphv1.EventEnvelope_ReopenInvestigation{ReopenInvestigation: body}
	case "label_investigation":
		body := &graphv1.LabelInvestigation{}
		if err := decode(body); err != nil {
			return err
		}
		env.Body = &graphv1.EventEnvelope_LabelInvestigation{LabelInvestigation: body}
	case "alert_transition":
		body := &graphv1.AlertTransition{}
		if err := decode(body); err != nil {
			return err
		}
		env.Body = &graphv1.EventEnvelope_AlertTransition{AlertTransition: body}
	// The 003 dependency package (FR-029, plan item 2). All three are here for the reason stated
	// above: a type this switch does not know is a type that cannot be replayed, which is the same
	// thing as a type that cannot be stored. A proposal that could be appended and not read back
	// would make a replay produce a different graph from the live one — and the decisions are
	// durable precisely so that they survive a replay (FR-122).
	case "propose_dependency":
		body := &graphv1.ProposeDependency{}
		if err := decode(body); err != nil {
			return err
		}
		env.Body = &graphv1.EventEnvelope_ProposeDependency{ProposeDependency: body}
	case "confirm_dependency":
		body := &graphv1.ConfirmDependency{}
		if err := decode(body); err != nil {
			return err
		}
		env.Body = &graphv1.EventEnvelope_ConfirmDependency{ConfirmDependency: body}
	case "reject_dependency":
		body := &graphv1.RejectDependency{}
		if err := decode(body); err != nil {
			return err
		}
		env.Body = &graphv1.EventEnvelope_RejectDependency{RejectDependency: body}
	// 004 T148: a value several entities share. Here for the reason the two comments above give, and this
	// is the case that proved the reason is not rhetorical: the type was added to Validate and to the
	// append path and missed here, so every recorded fixture appended cleanly and then failed to replay.
	// A type this switch does not know is a type that cannot be stored.
	case "correlate_entity":
		body := &graphv1.CorrelateEntity{}
		if err := decode(body); err != nil {
			return err
		}
		env.Body = &graphv1.EventEnvelope_CorrelateEntity{CorrelateEntity: body}
	// ADR-0015 (feature 009). Here for the reason every case above is: a type this switch does
	// not know is a type that cannot be replayed.
	case "propose_catalog_entry":
		body := &graphv1.ProposeCatalogEntry{}
		if err := decode(body); err != nil {
			return err
		}
		env.Body = &graphv1.EventEnvelope_ProposeCatalogEntry{ProposeCatalogEntry: body}
	case "declare_catalog_entry":
		body := &graphv1.DeclareCatalogEntry{}
		if err := decode(body); err != nil {
			return err
		}
		env.Body = &graphv1.EventEnvelope_DeclareCatalogEntry{DeclareCatalogEntry: body}
	case "declare_alias":
		body := &graphv1.DeclareAlias{}
		if err := decode(body); err != nil {
			return err
		}
		env.Body = &graphv1.EventEnvelope_DeclareAlias{DeclareAlias: body}
	case "retract_alias":
		body := &graphv1.RetractAlias{}
		if err := decode(body); err != nil {
			return err
		}
		env.Body = &graphv1.EventEnvelope_RetractAlias{RetractAlias: body}
	case "propose_alias":
		body := &graphv1.ProposeAlias{}
		if err := decode(body); err != nil {
			return err
		}
		env.Body = &graphv1.EventEnvelope_ProposeAlias{ProposeAlias: body}
	case "reject_alias":
		body := &graphv1.RejectAlias{}
		if err := decode(body); err != nil {
			return err
		}
		env.Body = &graphv1.EventEnvelope_RejectAlias{RejectAlias: body}
	case "attribute_signal":
		body := &graphv1.AttributeSignal{}
		if err := decode(body); err != nil {
			return err
		}
		env.Body = &graphv1.EventEnvelope_AttributeSignal{AttributeSignal: body}
	case "correct_attribution":
		body := &graphv1.CorrectAttribution{}
		if err := decode(body); err != nil {
			return err
		}
		env.Body = &graphv1.EventEnvelope_CorrectAttribution{CorrectAttribution: body}
	case "revert_correction":
		body := &graphv1.RevertCorrection{}
		if err := decode(body); err != nil {
			return err
		}
		env.Body = &graphv1.EventEnvelope_RevertCorrection{RevertCorrection: body}
	default:
		return fmt.Errorf("log: event %s has unknown type %q", env.GetEventId(), eventType)
	}
	return nil
}
