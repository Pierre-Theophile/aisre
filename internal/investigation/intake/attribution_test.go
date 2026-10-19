// SPDX-License-Identifier: Apache-2.0

package intake_test

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/investigation/intake"
)

// The entries the triggering signal is attributed to join the subjects with provenance
// `attribution`, read at the investigation's own instants (009 T013, FR-021, research R7).

type attributedResolver struct {
	stubResolver
	current *graphv1.AttributionDecision
	asked   *graphv1.WhyAttributedRequest
}

func (a *attributedResolver) WhyAttributed(_ context.Context, req *graphv1.WhyAttributedRequest) (*graphv1.WhyAttributedResponse, error) {
	a.asked = req
	if a.current == nil {
		return nil, connect.NewError(connect.CodeNotFound, nil)
	}
	return &graphv1.WhyAttributedResponse{Current: a.current}, nil
}

func TestAnAttributedEntryJoinsTheSubjects(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	in, err := intake.NormaliseAlert(intake.MonitorIntake{SourceID: "datadog:prod", Transition: monitorBody(), Now: fakeNow})
	if err != nil {
		t.Fatalf("normalise: %v", err)
	}
	r := &attributedResolver{stubResolver: stubResolver{canonical: "e1"}, current: &graphv1.AttributionDecision{
		EventId: "sre.attributor:attribute:1", Kind: "attribute", Ruleset: "attribution/rules@1",
		Entries: []*graphv1.Attributed{{EntrySlug: "candidate-view", Layer: "alias", RuleId: "A1", Score: 1}},
	}}
	set, err := intake.Resolve(ctx, r, in)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := intake.Attribute(ctx, r, in, &set); err != nil {
		t.Fatalf("attribute: %v", err)
	}

	if got := r.asked.GetSignal(); got.GetNamespace() != "monitor" || got.GetValue() != "checkout-error-rate" {
		t.Errorf("asked about %v, want the monitor", got)
	}
	if !r.asked.GetAsOf().GetObservedAt().AsTime().Equal(in.ObservedAt) || !r.asked.GetAsOf().GetValidAt().AsTime().Equal(in.ValidAt) {
		t.Errorf("asked as of %v, want the investigation's instants", r.asked.GetAsOf())
	}
	if len(set.Attributions) != 1 || set.Attributions[0].Entry.String() != "sre.catalog=candidate-view" ||
		set.Attributions[0].RuleID != "attribution/rules@1/A1" {
		t.Fatalf("attributions = %+v", set.Attributions)
	}
	// Grouping is about environments; an entry spans them, so it is not a grouping entity.
	if len(set.EntityIDs) != 1 || set.EntityIDs[0] != "e1" {
		t.Errorf("entity ids = %v", set.EntityIDs)
	}
	last := in.Symptom.GetTargetRefs()[len(in.Symptom.GetTargetRefs())-1]
	if last.GetProvenance() != investigationv1.TargetRefProvenance_ATTRIBUTION ||
		last.GetDecisionEventId() != "sre.attributor:attribute:1" || last.GetRef().GetValue() != "candidate-view" {
		t.Errorf("target ref = %v", last)
	}
}

func TestAnUnattributedSignalAddsNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	in, err := intake.NormaliseAlert(intake.MonitorIntake{SourceID: "datadog:prod", Transition: monitorBody(), Now: fakeNow})
	if err != nil {
		t.Fatalf("normalise: %v", err)
	}
	before := len(in.Symptom.GetTargetRefs())
	for _, r := range []intake.Resolver{&attributedResolver{}, stubResolver{}} {
		var set intake.ResolutionSet
		if err := intake.Attribute(ctx, r, in, &set); err != nil || len(set.Attributions) != 0 {
			t.Errorf("%T: %v %v", r, set.Attributions, err)
		}
	}
	if len(in.Symptom.GetTargetRefs()) != before {
		t.Error("an unattributed signal changed the target references")
	}
}
