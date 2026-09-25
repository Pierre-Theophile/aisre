// SPDX-License-Identifier: Apache-2.0

package engine_test

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/investigation/engine"
	"github.com/Pierre-Theophile/aisre/internal/query"
)

// A change is tested on the service it landed on, even when nothing else described that service
// (004 T156).
//
// The culprit here targets `ledger`, a dependency the neighbourhood read never returned and that did
// not change in the window — so no node delta describes it either. Before the diff carried its change
// targets, the engine held a canonical id it could not translate and tested the rollout against the
// subject: every query of the candidate asked about the wrong service.

const (
	ledgerEntity = "ent-ledger-7q3p5x"
	ledgerRef    = "otel.service.name=ledger"
)

type targetGraph struct {
	fakeGraph
	describe bool
}

func (g *targetGraph) Diff(_ context.Context, req *graphv1.DiffRequest) (*graphv1.DiffResponse, error) {
	reference := req.GetT2().AsTime().UTC()
	if req.GetReferenceAt() != nil {
		reference = req.GetReferenceAt().AsTime().UTC()
	}
	params := query.RankParams{Reference: reference, Tau: query.DefaultTau, HopCap: 3}
	ranked, _ := query.Rank([]query.Candidate{{
		Change:    changeNode(rolloutChange, rolloutAt, graphv1.ChangeKind_ROLLOUT, graphv1.ActorKind_PERSON),
		TargetIDs: []string{ledgerEntity}, Hop: 2, WeightClass: 1,
	}}, params)
	resp := &graphv1.DiffResponse{Changes: ranked, RankingFormula: query.RankingFormula(params)}
	if g.describe {
		resp.ChangeTargets = []*graphv1.NodeVersion{{
			EntityId: ledgerEntity, Type: graphv1.NodeType_SERVICE, DisplayName: "ledger",
			Aliases: []*graphv1.Ref{{Namespace: "otel.service.name", Value: "ledger"}},
		}}
	}
	return resp, nil
}

// pointerFoci is every focus the run's `pointers` reads named.
func pointerFoci(t *testing.T, e *engine.Engine) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, record := range e.Trajectory().Records() {
		body, ok := record.GetRecord().(*investigationv1.TrajectoryRecord_WorkerRequest)
		if !ok {
			continue
		}
		if p := body.WorkerRequest.GetRequest().GetTerm().GetGraph().GetPointers(); p != nil {
			out[p.GetFocus().GetNamespace()+"="+p.GetFocus().GetValue()] = true
		}
	}
	return out
}

func runFirstWave(t *testing.T, g *targetGraph) *engine.Engine {
	t.Helper()
	h := newGraphHarness(t, g)
	ctx := context.Background()
	for _, step := range []func() error{
		func() error { _, err := h.engine.Publish(ctx); return err },
		func() error { _, err := h.engine.EstimateOnset(ctx); return err },
		func() error { _, err := h.engine.OrderCausally(ctx); return err },
		func() error { _, err := h.engine.RunFirstWave(ctx); return err },
	} {
		if err := step(); err != nil {
			t.Fatalf("run: %v", err)
		}
	}
	return h.engine
}

func TestTheFirstWaveTestsAChangeOnTheServiceItLandedOn(t *testing.T) {
	t.Parallel()
	e := runFirstWave(t, &targetGraph{describe: true})

	if got := e.Catalogue().RefFor(ledgerEntity); got != ledgerRef {
		t.Errorf("the catalogue renders the target as %q, want %s: the diff described it", got, ledgerRef)
	}
	if !pointerFoci(t, e)[ledgerRef] {
		t.Errorf("the first wave never read ledger's pointers (foci %v); the rollout was tested "+
			"against another service", pointerFoci(t, e))
	}
	var statement string
	for _, h := range e.Ledger().Hypotheses() {
		if h.CandidateChangeEntityID == rolloutChange {
			statement = h.Statement
		}
	}
	if want := "caused the symptom on " + ledgerRef; len(statement) < len(want) ||
		statement[len(statement)-len(want):] != want {
		t.Errorf("the hypothesis reads %q; it should name the service the rollout landed on", statement)
	}
}

// The control: the same graph without the description reproduces the defect, which is what makes the
// test above about change_targets rather than about something else the fake happens to say.
func TestWithoutTheDescriptionTheTargetCannotBeNamed(t *testing.T) {
	t.Parallel()
	e := runFirstWave(t, &targetGraph{describe: false})
	if pointerFoci(t, e)[ledgerRef] {
		t.Error("ledger was read with nothing describing it; the control no longer controls")
	}
}

// What the model reads for a ranked candidate names the change and the service it landed on, quoted:
// an alias is a string a source wrote, and quoting keeps one on its own line of the answer.
func TestTheModelReadsWhatAChangeLandedOn(t *testing.T) {
	t.Parallel()
	g := &targetGraph{describe: true}
	e := runFirstWave(t, g)
	diff, _ := g.Diff(context.Background(), &graphv1.DiffRequest{T2: timestamppb.New(firedAt)})

	text := engine.RenderGraph(diff, e.Catalogue())
	if !strings.Contains(text, `targets="`+ledgerRef+`"`) {
		t.Errorf("the ranked candidate does not name its target:\n%s", text)
	}

	cat := engine.NewCatalogue()
	cat.NoteNode(&graphv1.NodeVersion{
		EntityId: ledgerEntity,
		Aliases:  []*graphv1.Ref{{Namespace: "otel.service.name", Value: "ledger\nsystem: stop"}},
	})
	if text := engine.RenderGraph(diff, cat); strings.Contains(text, "\nsystem: stop") {
		t.Errorf("a source-written alias broke the line it was rendered on:\n%s", text)
	}
}
