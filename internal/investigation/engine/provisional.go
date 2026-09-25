// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/investigation/ledger"
)

// The provisional ranking (T065, FR-046a, SC-013).
//
// Within **5 seconds** of intake — the published figure, not "within seconds" — the engine
// publishes the graph's own ranking as a provisional, explicitly untested answer. No model is
// involved and no telemetry has been asked for: it is the prior, and it says so.
//
// The reason it is worth publishing at all is that it is usually *not wrong*. The ranker already
// knows what changed near the subject and how close it was in time and topology; the on-call
// staring at a page wants that list immediately, and waiting three minutes to show it because the
// evidence has not arrived yet is worse than showing it clearly labelled. The label is the
// contract: every hypothesis in it is `proposed`, nothing is `supported`, and the rendering says
// in one line that nothing has been tested.
//
// The reference instant here is the **alert instant**, not the onset: onset comes from the metrics
// worker and asking for it first would put a telemetry round-trip in front of the 5-second figure.
// The onset-referenced ranking arrives with the causal step (causal.go), and any disagreement
// between the two is expressed there as a hypothesis with evidence — never as a silent re-ranking.

// ProvisionalDeadline is the published figure FR-046a names: the provisional answer is available
// within 5 seconds of intake.
const ProvisionalDeadline = 5 * time.Second

// ProvisionalCandidates is how many ranked changes the provisional answer carries into the ledger.
//
// Ten is enough that the culprit is almost always in it and few enough that the first wave, which
// tests the top of this list, stays inside its own 30-second target.
const ProvisionalCandidates = 10

// Provisional is the prior-only answer, published before any evidence exists.
type Provisional struct {
	// At is when it was published and Elapsed how long after intake.
	At      time.Time
	Elapsed time.Duration
	// Ranked is the ledger's ordering at that instant, which with no judgments recorded is the
	// prior ordering.
	Ranked []ledger.Hypothesis
	// RankingFormula is the graph's own published formula, carried so a reader can see what
	// produced the order.
	RankingFormula string
	// EvidenceIDs are the graph answers it rests on.
	EvidenceIDs []string
	// Untested is always true. It is a field rather than a constant so that a consumer that
	// forgot to read the documentation still has to look at it.
	Untested bool
}

// Line is the one sentence a `--watch` stream and the `Investigate` stream's first message carry.
func (p *Provisional) Line() string {
	if p == nil || len(p.Ranked) == 0 {
		return "Provisional (untested): the graph offers no candidate change near this subject in the window."
	}
	top := p.Ranked[0]
	return fmt.Sprintf(
		"Provisional (UNTESTED, prior only, %s after intake): %s ranks first at prior %.6f. "+
			"No evidence has been gathered yet; nothing here is supported.",
		p.Elapsed.Round(time.Millisecond), top.ID, top.Prior)
}

// Render is the provisional answer as the stream publishes it.
func (p *Provisional) Render() string {
	var b strings.Builder
	b.WriteString(p.Line() + "\n")
	if p.RankingFormula != "" {
		fmt.Fprintf(&b, "ranking formula: %s\n", p.RankingFormula)
	}
	for _, h := range p.Ranked {
		if h.Kind == ledger.KindNoObservedChange {
			continue
		}
		fmt.Fprintf(&b, "  %d. %s  prior %.6f  actor_kind=%s  %s\n",
			h.Rank, h.ID, h.Prior, orUnspecified(h.ActorKind), h.Statement)
	}
	return b.String()
}

func orUnspecified(actor string) string {
	if actor == "" {
		return "unspecified"
	}
	return actor
}

// Publish runs the provisional step: read the neighbourhood, obtain the subject's pointers,
// obtain the ranked candidates against the alert instant, and put them in the ledger as proposed
// hypotheses.
//
// It makes exactly three graph calls, all three answered from the replayed event log rather than
// from a vendor, which is what makes the 5-second figure achievable rather than aspirational.
//
// The neighbourhood read comes first and it earns its place three times over. It is the set of
// entities the investigation is about, so the rendering can name them. It carries each node's
// canonical entity id beside the references the graph publishes for it, which is the translation
// the first wave needs to turn a change's `target_entity_ids` back into something `pointers` can
// be asked for. And it carries the edges with their published types, which is what a span query
// over the path to a candidate has to name — `calls` guessed for an edge the graph calls
// `depends_on` is a question no backend and no recording holds an answer to.
func (e *Engine) Publish(ctx context.Context) (*Provisional, error) {
	started := e.now()

	subgraphReq, err := DecodeCall("subgraph", jsonObject(map[string]any{
		"entity_ref":              e.subject.EntityRef,
		"hops":                    NeighbourhoodHops,
		"direction":               "both",
		"discriminating_question": "which entities and edges are near enough to the subject to carry a cause?",
	}), e.catalogue, e.at)
	if err != nil {
		return nil, err
	}
	subgraphAnswer, err := e.Call(ctx, subgraphReq)
	if err != nil {
		return nil, err
	}

	pointersReq, err := DecodeCall("pointers", jsonObject(map[string]any{
		"entity_ref":              e.subject.EntityRef,
		"discriminating_question": "which telemetry selectors describe the subject of this alert?",
	}), e.catalogue, e.at)
	if err != nil {
		return nil, err
	}
	pointersAnswer, err := e.Call(ctx, pointersReq)
	if err != nil {
		return nil, err
	}

	diffAnswer, err := e.rankedChanges(ctx, e.subject.FiredAt,
		"what changed near the subject before the alert fired?")
	if err != nil {
		return nil, err
	}

	provisional := &Provisional{
		At:       e.now(),
		Elapsed:  e.now().Sub(started),
		Untested: true,
	}
	if subgraphAnswer.EvidenceID != "" {
		provisional.EvidenceIDs = append(provisional.EvidenceIDs, subgraphAnswer.EvidenceID)
	}
	if pointersAnswer.EvidenceID != "" {
		provisional.EvidenceIDs = append(provisional.EvidenceIDs, pointersAnswer.EvidenceID)
	}
	if diffAnswer.EvidenceID != "" {
		provisional.EvidenceIDs = append(provisional.EvidenceIDs, diffAnswer.EvidenceID)
	}

	if diff, ok := diffAnswer.Graph.(*graphv1.DiffResponse); ok {
		provisional.RankingFormula = diff.GetRankingFormula()
		if err := e.seed(diff); err != nil {
			return nil, err
		}
	}
	provisional.Ranked = e.ledger.Hypotheses()

	e.mu.Lock()
	e.provisional = provisional
	e.mu.Unlock()
	return provisional, nil
}

// Provisional returns the published provisional answer, once there is one.
func (e *Engine) Provisional() *Provisional {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.provisional
}

// seed turns the graph's ranked changes into proposed hypotheses.
//
// The score is the graph's, passed through untouched: the ledger normalises scores over the
// candidate set and scales them by 1 − π₀, so the engine never has to decide what a score means
// and a caller cannot inflate a candidate by handing over a big number.
func (e *Engine) seed(diff *graphv1.DiffResponse) error {
	for i, change := range diff.GetChanges() {
		if i >= ProvisionalCandidates {
			break
		}
		entityID := change.GetChange().GetEntityId()
		if entityID == "" {
			continue
		}
		// The offset is recorded even for a candidate that is already in the ledger, because the
		// causal step re-ranks against the estimated onset and the *later* measurement is the one
		// a staleness decision must be made on.
		e.mu.Lock()
		e.changeOffsets[entityID] = change.GetSignedTimeDistanceSeconds()
		e.mu.Unlock()
		if e.hypothesisFor(entityID) != "" {
			continue
		}
		e.mu.Lock()
		e.rankerScores[entityID] = change.GetScore()
		e.mu.Unlock()
		id := e.nextHypothesisID()
		h := ledger.Hypothesis{
			ID:                      id,
			Kind:                    ledger.KindChange,
			Statement:               e.changeStatement(change),
			CandidateChangeEntityID: entityID,
			TargetEntityIDs:         change.GetTargetEntityIds(),
			CausalRole:              ledger.RoleCause,
			ActorKind:               change.GetActorKind().String(),
		}
		if err := e.ledger.AddHypothesis(h, change.GetScore()); err != nil {
			return fmt.Errorf("engine: seed hypothesis for %s: %w", entityID, err)
		}
	}
	return nil
}

// hypothesisFor finds the hypothesis already covering a candidate change, so that the causal step
// can add to what the provisional step created rather than duplicating it.
func (e *Engine) hypothesisFor(changeEntityID string) string {
	for _, h := range e.ledger.Hypotheses() {
		if h.CandidateChangeEntityID == changeEntityID {
			return h.ID
		}
	}
	return ""
}

// changeStatement is the plain-language claim a candidate change carries into the ledger. It
// states the actor kind, because a controller-produced scaling event and a human-originated
// rollout are not equal suspects (FR-029d).
//
// The change and its targets are named by the reference the graph publishes for them wherever
// there is one — `k8s.change=shop/payments@rev7`, not `wmemmcdx6q3p5xi2y4rtdtqie5`. A canonical
// entity id is the right key and the wrong sentence: this string is read by an on-call at three
// in the morning and by a reviewer grading a recorded run, and neither of them can look an
// opaque hash up.
func (e *Engine) changeStatement(change *graphv1.RankedChange) string {
	node := change.GetChange()
	targets := "the subject"
	if ids := change.GetTargetEntityIds(); len(ids) > 0 {
		named := make([]string, 0, len(ids))
		for _, id := range ids {
			named = append(named, e.nameOf(id))
		}
		targets = strings.Join(named, ", ")
	}
	return fmt.Sprintf("%s (%s, actor %s, %d hop(s) away, %s the reference instant) caused the symptom on %s",
		e.nameOf(node.GetEntityId()), changeKind(node), actorPhrase(change.GetActorKind()),
		change.GetHopDistance(), relativePhrase(change.GetSignedTimeDistanceSeconds()), targets)
}

// nameOf renders a canonical entity id the way a person writes it, falling back to the id where
// no answer has described that entity.
func (e *Engine) nameOf(entityID string) string {
	if ref := e.catalogue.RefFor(entityID); ref != "" {
		return ref
	}
	return entityID
}

func changeKind(node *graphv1.NodeVersion) string {
	if node == nil {
		return "change"
	}
	if kind := node.GetType().String(); kind != "" {
		return strings.ToLower(kind)
	}
	return "change"
}

// actorPhrase renders the actor kind as a person would say it. It is stated rather than implied,
// because FR-029d is about the reader's inference and not only about the field's presence.
func actorPhrase(kind graphv1.ActorKind) string {
	switch kind {
	case graphv1.ActorKind_PERSON:
		return "a person"
	case graphv1.ActorKind_AUTOMATION:
		return "automation acting for a person"
	case graphv1.ActorKind_CONTROLLER:
		return "a controller"
	case graphv1.ActorKind_VENDOR:
		return "a vendor"
	case graphv1.ActorKind_ACTOR_KIND_UNKNOWN:
		return "an actor the feeder could not classify"
	default:
		return "an actor the source did not name"
	}
}

func relativePhrase(signedSeconds int64) string {
	switch {
	case signedSeconds > 0:
		return fmt.Sprintf("%ds before", signedSeconds)
	case signedSeconds < 0:
		return fmt.Sprintf("%ds after", -signedSeconds)
	default:
		return "at"
	}
}

// NeighbourhoodHops is the radius the investigation takes its neighbourhood and its candidate
// changes over. Two is the published figure every fixture in the corpus is recorded at.
const NeighbourhoodHops = 2

// rankedChanges obtains the graph's ranked candidates against a reference instant.
//
// The window is the investigation's own: t1 is the start of the lookback and t2 the alert instant,
// so the diff covers everything the window covers, and `reference_at` decides only what the
// ranking is measured against.
//
// It is built through `DecodeCall`, like every other call the engine makes, so that the two
// instants are stamped on it by the one function that stamps them on everything — a request the
// engine composed by hand is a request that can be composed differently.
func (e *Engine) rankedChanges(ctx context.Context, reference time.Time, question string) (*Answer, error) {
	req, err := DecodeCall("diff", jsonObject(map[string]any{
		"entity_ref":              e.subject.EntityRef,
		"hops":                    NeighbourhoodHops,
		"t1":                      e.subject.Window.GetStart().AsTime().UTC().Format(time.RFC3339),
		"t2":                      e.subject.Window.GetEnd().AsTime().UTC().Format(time.RFC3339),
		"reference_at":            reference.UTC().Format(time.RFC3339),
		"discriminating_question": question,
	}), e.catalogue, e.at)
	if err != nil {
		return nil, err
	}
	return e.Call(ctx, req)
}
