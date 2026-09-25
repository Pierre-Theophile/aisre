// SPDX-License-Identifier: Apache-2.0

package ledger

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/Pierre-Theophile/aisre/internal/investigation/audit"
)

// The ledger itself (T051, T054, FR-019a, FR-020, FR-020b).
//
// A Ledger is the investigation's belief state, and it is the *only* belief state: the model is
// handed a rendering of it each turn and has no tool that writes a number into it. Everything a
// caller can do is here — add a hypothesis, add an evidence item, judge one against the other,
// set a status, widen a cut-short bucket — and every one of those recomputes the whole
// distribution, so there is no moment at which the set does not sum to one.
//
// It is deliberately not safe for concurrent use. An investigation is a sequential loop over one
// ledger; making it concurrent would invite two judgments to race, and the whole argument of
// FR-023 is that the ledger's arithmetic has no room for a race.

// ErrVocabulary is returned when a value is not in the published vocabulary — a strength that is
// not on the scale, a direction that is neither supports, refutes nor neutral.
var ErrVocabulary = errors.New("not in the published vocabulary")

// The published write-time reason codes (data-model §Validation rules). They are the ledger's
// half of the same vocabulary `internal/log` publishes for events: a caller matches on the
// string, so these are schema (constitution IX) and change only with a version bump.
const (
	// ReasonModelConfidence is an attempt to write a confidence the ledger rule did not produce
	// (FR-023). It is the code that makes "confidence is computed, never verbalised" enforceable
	// rather than aspirational.
	ReasonModelConfidence = "model_confidence"
	// ReasonUnsupportedStatus is `supported` with no qualifying judgment: no `supports` judgment
	// from an evidence item whose worker declares a source of truth (FR-022, FR-051).
	ReasonUnsupportedStatus = "unsupported_status"
	// ReasonDuplicateJudgment is a second judgment for the same (hypothesis, evidence) pair. The
	// same fact counted twice is the same fact believed twice (FR-020a).
	ReasonDuplicateJudgment = "duplicate_judgment"
	// ReasonMissingCoverage is an evidence item with no coverage block (FR-014a, Invariant 4).
	ReasonMissingCoverage = "missing_coverage"
)

// RejectionError is a refused write, carrying the published reason code and what was wrong.
type RejectionError struct {
	// ReasonCode is one of the Reason* constants above.
	ReasonCode string
	// Detail names the offending value, in a form a caller can put in a log line.
	Detail string
}

// Error renders the rejection.
func (e *RejectionError) Error() string {
	if e.Detail == "" {
		return "ledger: " + e.ReasonCode
	}
	return "ledger: " + e.ReasonCode + ": " + e.Detail
}

func rejectf(code, format string, args ...any) *RejectionError {
	return &RejectionError{ReasonCode: code, Detail: fmt.Sprintf(format, args...)}
}

// DefaultOpenHypothesisID is the id the mandatory open hypothesis takes unless the caller names
// another. It is stable across runs on purpose: a grader comparing two investigations of the same
// incident should find the open hypothesis under the same id in both.
const DefaultOpenHypothesisID = "h-no-observed-change"

// DefaultOpenStatement is what the open hypothesis says when the caller does not phrase it. It is
// the sentence FR-019a names, and it is rendered like any other hypothesis.
const DefaultOpenStatement = "no observed change explains this"

// Ledger is one investigation's hypotheses, evidence and judgments, with the computed posterior
// over them.
type Ledger struct {
	investigationID string
	prior           audit.PriorRecord
	openID          string

	hypotheses []Hypothesis
	index      map[string]int
	scores     map[string]float64

	judgments []Judgment
	pairs     map[string]string

	evidence      map[string]EvidenceItem
	evidenceOrder []string

	// exonerations holds each exoneration's rationale, by hypothesis id.
	//
	// It is a field rather than a suffix written once onto Hypothesis.Rationale because the
	// rationale is *derived*: every mutation calls recompute, and recompute rebuilds the
	// rationale from the rows. A reason written into the string would survive until the next
	// judgment landed anywhere in the ledger and then vanish — which is exactly the shape of bug
	// that only shows up on a busy investigation, where an exoneration is followed by more
	// evidence. Keeping the reason here means "exonerated: the change starts after onset" is
	// reproducible from the ledger at every instant, not only at the instant it was recorded.
	exonerations map[string]string

	// openStatement is how the open hypothesis is phrased, which WithOpenStatement sets.
	openStatement string
}

// Option configures a new Ledger.
type Option func(*Ledger)

// WithOpenHypothesisID names the mandatory open hypothesis, for a caller that mints its own ids.
func WithOpenHypothesisID(id string) Option {
	return func(l *Ledger) {
		if id != "" {
			l.openID = id
		}
	}
}

// WithOpenStatement phrases the open hypothesis, which is how the symptom stays localised in an
// `unobserved` answer (Phase 8 K1, SC-023).
//
// "No observed change explains this" is true and useless on its own. "No observed change explains
// the error rate on checkout" is the same claim with the one fact an on-call still needs: where
// the symptom is. A run that ends on the open hypothesis is reported on this sentence, so the
// caller phrases it with the subject in it.
func WithOpenStatement(statement string) Option {
	return func(l *Ledger) {
		if statement != "" {
			l.openStatement = statement
		}
	}
}

// New opens a ledger for an investigation, with the mandatory `no_observed_change` hypothesis
// already in it (FR-019a, Invariant 2, T054).
//
// The open hypothesis is created here rather than by the caller because "always present" is not
// something a caller can be trusted to remember on the one incident where it matters. Its prior
// is π₀ from the coverage audit: `audit.PriorRecordFromAudit(nil)` gives 1.0, which is the
// correct prior for a deployment that has never measured its ceiling — no measurement says any
// cause is observable, so the whole mass belongs to *no observed change explains this*, and the
// engine reports `unknown` until the audit says otherwise.
func New(investigationID string, prior audit.PriorRecord, opts ...Option) (*Ledger, error) {
	if investigationID == "" {
		return nil, errors.New("ledger: new: an investigation id is required")
	}
	if prior.Prior < 0 || prior.Prior > 1 {
		return nil, fmt.Errorf("ledger: new: π₀ = %v is not a probability", prior.Prior)
	}

	l := &Ledger{
		investigationID: investigationID,
		prior:           prior,
		openID:          DefaultOpenHypothesisID,
		openStatement:   DefaultOpenStatement,
		index:           map[string]int{},
		scores:          map[string]float64{},
		pairs:           map[string]string{},
		evidence:        map[string]EvidenceItem{},
		exonerations:    map[string]string{},
	}
	for _, opt := range opts {
		opt(l)
	}

	l.hypotheses = append(l.hypotheses, Hypothesis{
		ID:         l.openID,
		Kind:       KindNoObservedChange,
		Statement:  l.openStatement,
		CausalRole: RoleCause,
		Status:     StatusProposed,
	})
	l.index[l.openID] = 0
	l.recompute()
	return l, nil
}

// InvestigationID is the run this ledger belongs to.
func (l *Ledger) InvestigationID() string { return l.investigationID }

// OpenHypothesisID is the id of the mandatory `no_observed_change` hypothesis.
func (l *Ledger) OpenHypothesisID() string { return l.openID }

// Prior is the π₀ record this ledger was opened with: the value, the audit it came from, and the
// ceiling and feeder set behind it (ADR-0005 D9). A confidence is only interpretable against it.
func (l *Ledger) Prior() audit.PriorRecord { return l.prior }

// RuleVersion is the published ledger rule in force, recorded on the investigation and carried in
// the export.
func (l *Ledger) RuleVersion() string { return LedgerRuleVersion }

// AddHypothesis adds a candidate hypothesis with its published ranker score and recomputes the
// whole distribution (FR-019, FR-020).
//
// `score` is the ranker score from `docs/schema/ranking.md` for a change hypothesis, and the
// engine's weight on the same scale for a named condition. Scores are normalised over the
// candidate set and scaled by `1 − π₀`, so their absolute size never matters and a caller cannot
// inflate a hypothesis by handing over a big number.
//
// Confidence, Bucket and Rank on the argument are refused rather than ignored: an engine that
// tries to set one is doing the thing FR-023 exists to prevent, and being told so with
// `model_confidence` is more useful than having it silently dropped.
func (l *Ledger) AddHypothesis(h Hypothesis, score float64) error {
	switch {
	case h.ID == "":
		return errors.New("ledger: add hypothesis: an id is required")
	case h.ID == l.openID:
		return fmt.Errorf("ledger: add hypothesis: %s is the open hypothesis, which already exists", h.ID)
	case h.Kind == KindNoObservedChange:
		return rejectf(ReasonUnsupportedStatus,
			"hypothesis %s is a second no_observed_change; exactly one exists per investigation (FR-019a)", h.ID)
	case h.Kind != KindChange && h.Kind != KindCondition:
		return fmt.Errorf("ledger: add hypothesis %s: %w: kind %q", h.ID, ErrVocabulary, h.Kind)
	case h.Statement == "":
		return fmt.Errorf("ledger: add hypothesis %s: a statement in plain language is required (FR-020)", h.ID)
	case h.Confidence != 0 || h.Bucket.Name != "" || h.Rank != 0 || h.Prior != 0:
		return rejectf(ReasonModelConfidence,
			"hypothesis %s arrived with a confidence, bucket, prior or rank; the ledger computes those (FR-023)", h.ID)
	case score < 0:
		return fmt.Errorf("ledger: add hypothesis %s: score %v is negative", h.ID, score)
	}
	if _, exists := l.index[h.ID]; exists {
		return fmt.Errorf("ledger: add hypothesis: %s is already in the ledger", h.ID)
	}
	if h.CausalRole == "" {
		h.CausalRole = RoleCause
	}
	if h.CausalRole != RoleCause && h.CausalRole != RoleCandidateEffect {
		return fmt.Errorf("ledger: add hypothesis %s: %w: causal role %q", h.ID, ErrVocabulary, h.CausalRole)
	}
	if h.Kind != KindChange && h.CandidateChangeEntityID != "" {
		return fmt.Errorf("ledger: add hypothesis %s: only a change hypothesis names a candidate change", h.ID)
	}
	if h.Status == "" {
		h.Status = StatusProposed
	}
	if h.Status != StatusProposed {
		return fmt.Errorf("ledger: add hypothesis %s: a hypothesis enters as proposed, not %q", h.ID, h.Status)
	}

	h.TargetEntityIDs = slices.Clone(h.TargetEntityIDs)
	l.hypotheses = append(l.hypotheses, h)
	l.index[h.ID] = len(l.hypotheses) - 1
	l.scores[h.ID] = score
	l.recompute()
	return nil
}

// AddEvidence records an evidence item the ledger may rest on (FR-021, Invariant 4).
//
// The coverage block is required and the row is refused without one, because a digest with no
// coverage is a claim about an unknown amount of data — the same refusal the schema makes, made
// here so a caller learns it before the transaction rather than during it.
func (l *Ledger) AddEvidence(e EvidenceItem) error {
	switch {
	case e.ID == "":
		return errors.New("ledger: add evidence: an id is required")
	case e.Kind == "":
		return fmt.Errorf("ledger: add evidence %s: a kind is required (FR-021)", e.ID)
	case e.Outcome == "":
		return fmt.Errorf("ledger: add evidence %s: an outcome is required; the four negative "+
			"outcomes are never collapsed (FR-027)", e.ID)
	case e.Coverage == nil:
		return rejectf(ReasonMissingCoverage,
			"evidence item %s has no coverage block (FR-014a)", e.ID)
	case e.DeepLink == "" && e.DeepLinkAbsentReason == "":
		return fmt.Errorf("ledger: add evidence %s: a missing deep link is explained, never silent (FR-057d)", e.ID)
	case e.Truncated && e.TruncationNote == "":
		return fmt.Errorf("ledger: add evidence %s: truncation is stated in the response it applies to (FR-037)", e.ID)
	}
	if _, exists := l.evidence[e.ID]; exists {
		return fmt.Errorf("ledger: add evidence: %s is already in the ledger", e.ID)
	}
	e.GraphEventIDs = slices.Clone(e.GraphEventIDs)
	l.evidence[e.ID] = e
	l.evidenceOrder = append(l.evidenceOrder, e.ID)
	return nil
}

// Evidence returns the evidence item with this id.
func (l *Ledger) Evidence(id string) (EvidenceItem, bool) {
	e, ok := l.evidence[id]
	return e, ok
}

// EvidenceItems returns every evidence item, in the order it was added.
func (l *Ledger) EvidenceItems() []EvidenceItem {
	out := make([]EvidenceItem, 0, len(l.evidenceOrder))
	for _, id := range l.evidenceOrder {
		out = append(out, l.evidence[id])
	}
	return out
}

// Judge records a typed judgment and recomputes the distribution (FR-020a).
//
// This is the only way a confidence moves. The ln LR is taken from the published table for the
// (direction, strength) pair and written onto the judgment; a caller that supplies its own
// different value is refused with `model_confidence`, because a likelihood ratio nobody published
// is a confidence nobody can reproduce.
//
// The returned judgment is the one recorded, with its ln LR filled in, ready for the DAO.
func (l *Ledger) Judge(j Judgment) (Judgment, error) {
	if j.ID == "" {
		return Judgment{}, errors.New("ledger: judge: a judgment id is required")
	}
	if _, ok := l.index[j.HypothesisID]; !ok {
		return Judgment{}, fmt.Errorf("ledger: judge %s: hypothesis %s is not in the ledger", j.ID, j.HypothesisID)
	}
	if _, ok := l.evidence[j.EvidenceID]; !ok {
		return Judgment{}, fmt.Errorf("ledger: judge %s: evidence item %s is not in the ledger; a judgment "+
			"rests on recorded evidence (FR-022)", j.ID, j.EvidenceID)
	}
	switch j.Source {
	case SourceFirstWave, SourceModel, SourceHumanFact, SourceExoneration, SourceRollback:
	default:
		return Judgment{}, fmt.Errorf("ledger: judge %s: %w: source %q", j.ID, ErrVocabulary, j.Source)
	}
	// A human fact is strong evidence, not truth (research §8, FR-057a). Letting it in at
	// Decisive would let one assertion end an investigation, which is the failure mode the
	// human channel is explicitly not allowed to have.
	if j.Source == SourceHumanFact && j.Strength == Decisive {
		return Judgment{}, fmt.Errorf("ledger: judge %s: a human fact enters at %s, never %s (FR-057a)",
			j.ID, Strong, Decisive)
	}
	// The same cap for the same reason: a rollback is a person's judgement, recorded by a platform.
	if j.Source == SourceRollback && j.Strength == Decisive {
		return Judgment{}, fmt.Errorf("ledger: judge %s: a rollback enters at %s at most, never %s (004 T155)",
			j.ID, Strong, Decisive)
	}
	if j.Direction == Neutral {
		j.Strength = ""
	}
	ln, err := LnLR(j.Direction, j.Strength)
	if err != nil {
		return Judgment{}, fmt.Errorf("ledger: judge %s: %w", j.ID, err)
	}
	if j.LnLR != 0 && round6(j.LnLR) != ln {
		return Judgment{}, rejectf(ReasonModelConfidence,
			"judgment %s carries ln_lr %v; the published table says %v for %s/%s (FR-023)",
			j.ID, j.LnLR, ln, j.Direction, j.Strength)
	}
	j.LnLR = ln

	key := pairKey(j.HypothesisID, j.EvidenceID)
	if existing, ok := l.pairs[key]; ok {
		return Judgment{}, rejectf(ReasonDuplicateJudgment,
			"evidence item %s already moved hypothesis %s, in judgment %s (FR-020a)",
			j.EvidenceID, j.HypothesisID, existing)
	}
	l.pairs[key] = j.ID
	l.judgments = append(l.judgments, j)
	l.recompute()
	return j, nil
}

func pairKey(hypothesisID, evidenceID string) string { return hypothesisID + "\x00" + evidenceID }

// Judgments returns every judgment, in the order it was recorded. The posterior does not depend
// on that order — this is for rendering and for the DAO, not for the arithmetic.
func (l *Ledger) Judgments() []Judgment { return slices.Clone(l.judgments) }

// JudgmentsFor returns the judgments bearing on one hypothesis, in recording order.
func (l *Ledger) JudgmentsFor(hypothesisID string) []Judgment {
	out := make([]Judgment, 0, 4)
	for _, j := range l.judgments {
		if j.HypothesisID == hypothesisID {
			out = append(out, j)
		}
	}
	return out
}

// Hypothesis returns one hypothesis by id, with its computed confidence.
func (l *Ledger) Hypothesis(id string) (Hypothesis, bool) {
	i, ok := l.index[id]
	if !ok {
		return Hypothesis{}, false
	}
	return l.hypotheses[i], true
}

// Hypotheses returns every hypothesis in published rank order: most believed first, the open
// hypothesis among them and never special-cased (FR-019a).
func (l *Ledger) Hypotheses() []Hypothesis {
	out := slices.Clone(l.hypotheses)
	slices.SortFunc(out, byRank)
	return out
}

// byRank is the published order: confidence descending, then prior descending, then id ascending
// so that two hypotheses the evidence cannot separate still come out in the same order on every
// machine and in every replay.
func byRank(a, b Hypothesis) int {
	if a.Confidence != b.Confidence {
		if a.Confidence > b.Confidence {
			return -1
		}
		return 1
	}
	if a.Prior != b.Prior {
		if a.Prior > b.Prior {
			return -1
		}
		return 1
	}
	return strings.Compare(a.ID, b.ID)
}

// recompute rebuilds priors, posteriors, buckets, ranks and rationales from the rows.
//
// Every mutation calls it, which is what makes "the posterior is a function of the rows" true at
// every instant rather than only after some explicit commit the caller might forget.
func (l *Ledger) recompute() {
	scores := make([]PriorInput, 0, len(l.hypotheses))
	for _, h := range l.hypotheses {
		if h.Kind == KindNoObservedChange {
			continue
		}
		scores = append(scores, PriorInput{HypothesisID: h.ID, Score: l.scores[h.ID]})
	}
	priors, err := Priors(scores, l.prior.Prior, l.openID)
	if err != nil {
		// Priors only fails on inputs New and AddHypothesis already refused, so this is
		// unreachable; leaving the priors as they were is the safe response if it ever is not.
		return
	}
	for i := range l.hypotheses {
		l.hypotheses[i].Prior = priors[l.hypotheses[i].ID]
	}

	posteriors := Posteriors(l.hypotheses, l.judgments)
	for i := range l.hypotheses {
		h := &l.hypotheses[i]
		h.Confidence = posteriors[h.ID]
		h.Bucket = l.reportedBucket(*h)
		h.Rationale = l.rationale(*h)
	}

	order := slices.Clone(l.hypotheses)
	slices.SortFunc(order, byRank)
	for rank, h := range order {
		l.hypotheses[l.index[h.ID]].Rank = rank + 1
	}
}

// rationale is the ledger's own account of a confidence: the prior, the judgments that moved it
// and the number they produced. It is derived, never a model's prose (FR-020), and it is what
// makes a confidence explainable line by line — "prior 0.310, two supports at moderate, one
// refute at strong → 0.214".
func (l *Ledger) rationale(h Hypothesis) string {
	var supports, refutes, neutrals []string
	for _, j := range l.judgments {
		if j.HypothesisID != h.ID {
			continue
		}
		switch j.Direction {
		case Supports:
			supports = append(supports, string(j.Strength))
		case Refutes:
			refutes = append(refutes, string(j.Strength))
		case Neutral:
			neutrals = append(neutrals, j.EvidenceID)
		}
	}

	parts := []string{fmt.Sprintf("prior %.6f", h.Prior)}
	if len(supports) > 0 {
		parts = append(parts, fmt.Sprintf("%s supporting", countByStrength(supports)))
	}
	if len(refutes) > 0 {
		parts = append(parts, fmt.Sprintf("%s refuting", countByStrength(refutes)))
	}
	if len(neutrals) > 0 {
		parts = append(parts, fmt.Sprintf("%d neutral (tested, did not separate)", len(neutrals)))
	}
	if len(supports) == 0 && len(refutes) == 0 && len(neutrals) == 0 {
		parts = append(parts, "no judgments yet")
	}
	line := strings.Join(parts, ", ") + fmt.Sprintf(" → %.6f (%s)", h.Confidence, h.Bucket)
	if h.Widened {
		line += fmt.Sprintf("; bucket widened %s: %s", h.WidenedDirection, h.WidenedReason)
	}
	if reason := l.exonerations[h.ID]; reason != "" {
		line += "; exonerated: " + reason
	}
	return line
}

// ExonerationReason returns why a hypothesis was exonerated, or the empty string when it was not.
//
// It is read by the rendering and by a reader who has the ledger rather than the rows. The rows
// keep the same sentence on the hypothesis's rationale, which is what an export and a reload see.
func (l *Ledger) ExonerationReason(hypothesisID string) string { return l.exonerations[hypothesisID] }

// countByStrength renders "2 moderate, 1 strong" in published scale order, so two ledgers with
// the same judgments produce the same sentence.
func countByStrength(strengths []string) string {
	counts := map[string]int{}
	for _, s := range strengths {
		counts[s]++
	}
	parts := make([]string, 0, len(counts))
	for _, s := range []Strength{Weak, Moderate, Strong, Decisive} {
		if n := counts[string(s)]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, s))
		}
	}
	return strings.Join(parts, ", ")
}
