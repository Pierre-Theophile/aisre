// SPDX-License-Identifier: Apache-2.0

package intake

import (
	"context"
	"fmt"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
)

// The catalog entries a triggering signal is attributed to are subjects too (009 FR-021, research
// R7). They are read as of the investigation's two instants, like everything else intake reads, so
// a correction made after the alert fired does not change an investigation pinned before it.
//
// They join the subjects and the symptom's target references; they do not join the entity ids the
// association rule groups on, because an entry spans environments and grouping does not.

// TargetProvenanceAttribution is a reference the triggering signal's attribution produced.
const TargetProvenanceAttribution = "attribution"

// AttributionReader is the catalog read intake asks. A resolver that does not offer it attributes
// nothing.
type AttributionReader interface {
	WhyAttributed(ctx context.Context, req *graphv1.WhyAttributedRequest) (*graphv1.WhyAttributedResponse, error)
}

// Attribution is one catalog entry the signal is attributed to.
type Attribution struct {
	// Entry is the entry's reference, `sre.catalog=<slug>`.
	Entry graph.Ref
	// RuleID is `<ruleset>/<rule>`, e.g. `attribution/rules@1/A1`.
	RuleID string
	// DecisionEventID is the attribution or correction it comes from.
	DecisionEventID string
}

// Attribute reads the signal's current attribution and records each entry on the set and on the
// symptom's target references. A signal nothing has attributed adds nothing.
func Attribute(ctx context.Context, r Resolver, in *Intake, set *ResolutionSet) error {
	reader, ok := r.(AttributionReader)
	if !ok || in == nil || in.Symptom == nil || in.Signal.IsZero() {
		return nil
	}
	resp, err := reader.WhyAttributed(ctx, &graphv1.WhyAttributedRequest{
		Signal: in.Signal.Proto(),
		AsOf:   &graphv1.AsOf{ValidAt: timestamppb.New(in.ValidAt), ObservedAt: timestamppb.New(in.ObservedAt)},
	})
	if connect.CodeOf(err) == connect.CodeNotFound {
		return nil
	}
	if err != nil {
		return fmt.Errorf("attribution of %s: %w", in.Signal, err)
	}
	current := resp.GetCurrent()
	if current == nil {
		return nil
	}
	for _, entry := range current.GetEntries() {
		if entry.GetEntrySlug() == "" {
			continue
		}
		a := Attribution{
			Entry:           graph.CatalogRef(entry.GetEntrySlug()),
			RuleID:          current.GetRuleset() + "/" + entry.GetRuleId(),
			DecisionEventID: current.GetEventId(),
		}
		set.Attributions = append(set.Attributions, a)
		in.Symptom.TargetRefs = append(in.Symptom.TargetRefs, &investigationv1.TargetRef{
			Ref:             a.Entry.Proto(),
			Provenance:      investigationv1.TargetRefProvenance_ATTRIBUTION,
			RuleId:          a.RuleID,
			DecisionEventId: a.DecisionEventID,
		})
	}
	return nil
}
