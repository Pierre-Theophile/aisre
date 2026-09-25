// SPDX-License-Identifier: Apache-2.0

package ledger

import (
	"time"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
)

// The vocabulary of the ledger (T051, FR-019, FR-020, FR-020a, data-model §investigation.hypotheses
// and §investigation.judgments).
//
// Every string constant below is also a value the `investigation` schema's CHECK constraints
// accept, and a value in `api/sreagent/investigation/v1/investigation.proto` §4. They are spelled
// here as typed strings rather than as the generated enums for one reason: the database column is
// text, the render is text, and a round-trip through the enum would put a translation table in
// between where the constitution wants one vocabulary (IX). Conversion to the proto enums happens
// once, in export.go, where the wire form is actually needed.

// Kind is what a hypothesis is about.
type Kind string

// The three hypothesis kinds. `no_observed_change` is spelled exactly as data-model.md spells it,
// because the field name is published: research §8 uses `H₀` as prose shorthand and nowhere else.
const (
	// KindChange names a candidate change entity as the cause.
	KindChange Kind = "change"
	// KindCondition names a condition rather than a change (saturation, a dependency's state).
	KindCondition Kind = "condition"
	// KindNoObservedChange is the mandatory open hypothesis: no observed change explains this.
	KindNoObservedChange Kind = "no_observed_change"
)

// Status is where a hypothesis stands (data-model §State transitions).
type Status string

// The six statuses. `untested` is never scored as refuted and never dropped (FR-031);
// `exonerated` is as prominent as a support (FR-029c, FR-057c).
const (
	StatusProposed     Status = "proposed"
	StatusSupported    Status = "supported"
	StatusRefuted      Status = "refuted"
	StatusInconclusive Status = "inconclusive"
	StatusUntested     Status = "untested"
	StatusExonerated   Status = "exonerated"
)

// CausalRole distinguishes a candidate cause from a candidate effect (FR-029c). A change that
// starts after onset by more than the onset's uncertainty is a candidate *effect* of the incident,
// and exonerating it is a first-class finding rather than a silent drop.
type CausalRole string

// The two causal roles.
const (
	RoleCause           CausalRole = "cause"
	RoleCandidateEffect CausalRole = "candidate_effect"
)

// Direction is which way a judgment moves a hypothesis (FR-020a).
type Direction string

// The three directions. A neutral judgment is recorded and moves nothing: "we looked and it did
// not separate" is a different answer from "we did not look".
const (
	Supports Direction = "supports"
	Refutes  Direction = "refutes"
	Neutral  Direction = "neutral"
)

// Strength is the published five-point scale (research §8). It is a strength, not a probability:
// the ln LR it maps to is a published constant, so two engines that judge the same way agree.
type Strength string

// The four non-trivial strengths, plus the neutral one a Neutral judgment carries.
const (
	Weak     Strength = "weak"
	Moderate Strength = "moderate"
	Strong   Strength = "strong"
	Decisive Strength = "decisive"
)

// JudgmentSource is where a judgment came from, so that a ledger can be read back and the
// deterministic half of it separated from the model's half (data-model §investigation.judgments).
type JudgmentSource string

// The five judgment sources.
const (
	// SourceFirstWave is the deterministic first wave: no model was involved.
	SourceFirstWave JudgmentSource = "first_wave"
	// SourceModel is a judgment the investigator model proposed; the ledger still computes the
	// number (FR-023).
	SourceModel JudgmentSource = "model"
	// SourceHumanFact is a fact a person pushed. It enters at Strong, never Decisive (research §8).
	SourceHumanFact JudgmentSource = "human_fact"
	// SourceExoneration is the onset comparison that rules a change out (FR-029c).
	SourceExoneration JudgmentSource = "exoneration"
	// SourceRollback is a platform-stated rollback AWAY from the change a hypothesis names (004 T155).
	// It is an operator's judgement made during the incident, recorded by the platform rather than typed
	// into this system, so it is treated as a human fact is: observed support that may name a change, and
	// Strong at most — someone can roll back as a precaution, and one action must not end an
	// investigation.
	SourceRollback JudgmentSource = "rollback"
)

// Hypothesis is one row of the ledger: what is claimed, what it rests on, and what the ledger
// rule computed for it.
//
// Confidence and Bucket are outputs, not inputs. Nothing outside this package sets them — the
// write-time rule that says so is ReasonModelConfidence, and Set exists so the compiler agrees.
type Hypothesis struct {
	// ID is the hypothesis id, unique within the investigation.
	ID string
	// Kind is `change`, `condition` or `no_observed_change`.
	Kind Kind
	// Statement is the plain-language claim (FR-020).
	Statement string
	// CandidateChangeEntityID names the change a `change` hypothesis is about; empty otherwise,
	// which the schema enforces as well.
	CandidateChangeEntityID string
	// TargetEntityIDs are the entities the hypothesis concerns.
	TargetEntityIDs []string
	// CausalRole is `cause` or `candidate_effect` (FR-029c).
	CausalRole CausalRole
	// ActorKind is the actor kind carried over from the ranked change (ADR-0005 D3).
	ActorKind string
	// Prior is the hypothesis's prior: the normalised ranker score scaled by 1 − π₀, or π₀ itself
	// for the open hypothesis.
	Prior float64
	// Status is where it stands (FR-020).
	Status Status
	// UntestedReason says why it was not tested; required when Status is StatusUntested.
	UntestedReason string
	// OnsetEvidenceID is the onset estimate an exoneration rests on; required when Status is
	// StatusExonerated (Invariant 9).
	OnsetEvidenceID string
	// Confidence is the computed posterior, rounded to six decimals. Never model-stated (FR-023).
	Confidence float64
	// Bucket is the published coarse bucket the confidence is reported in, widened where a test
	// was cut short (FR-045). It is the *reported* bucket: BucketFor(Confidence) when the
	// hypothesis was not widened, one step outward when it was.
	Bucket Bucket
	// Widened records that the reported bucket is not the computed one, and Widened* say why.
	Widened bool
	// WidenedReason is required when Widened (FR-045).
	WidenedReason string
	// WidenedDirection is which way the bucket moved: WidenUp or WidenDown.
	WidenedDirection WidenDirection
	// Rank is the position in the published order, 1-based, assigned by the ledger.
	Rank int
	// Rationale is derived from the ledger, never from a model's prose.
	Rationale string
	// KnowledgeDerived marks a hypothesis resting only on a document; KnowledgeConfirmed marks
	// one a source-of-truth worker has since confirmed (FR-051).
	KnowledgeDerived   bool
	KnowledgeConfirmed bool
	// NextQuery is the exact term that would test an untested hypothesis, and NextQueryDeepLink
	// the link a person can follow (FR-031, FR-045).
	NextQuery         *investigationv1.AlgebraTerm
	NextQueryDeepLink string
}

// EvidenceItem is what a judgment may rest on (FR-021, data-model §investigation.evidence_items).
//
// The ledger holds evidence to render and export it and to answer one question at write time —
// does this judgment come from a source-of-truth worker — so the fields here are the ones the
// ledger reads plus the ones the DAO writes. It is never telemetry: a digest, a coverage block
// and join keys, which is the whole of constitution IV at this boundary.
type EvidenceItem struct {
	// ID is the evidence id.
	ID string
	// Kind is one of the published evidence kinds (`algebra_answer`, `onset_estimate`, …).
	Kind string
	// Worker and Capability name where the answer came from.
	Worker     string
	Capability string
	// SourceOfTruth is the worker's declared source of truth — the system it is authoritative
	// for — and is empty for a worker that is not one.
	//
	// It is a field on the evidence rather than a lookup into a worker registry because the
	// ledger must be readable years later from its rows alone: "supported by a source-of-truth
	// worker" has to stay checkable when the registry has moved on. Phase 4's
	// WorkerDescription.source_of_truth is where the engine reads it from.
	SourceOfTruth string
	// Term is the algebra term as called, canonicalised.
	Term *investigationv1.AlgebraTerm
	// ValidAt and ObservedAt are the instants in force; CalledAt is when the call was made.
	ValidAt    time.Time
	ObservedAt time.Time
	CalledAt   time.Time
	// Mode is `live` or `recorded`.
	Mode string
	// Outcome is one of the six published outcomes, never collapsed (FR-027, Invariant 8).
	Outcome string
	// ResponseDigest and ResponseKey address the recording beside the graph.
	ResponseDigest string
	ResponseKey    string
	// Coverage is the mandatory coverage block; an item without one is rejected (Invariant 4).
	Coverage *investigationv1.Coverage
	// JoinKeys are the identifiers that let two answers be joined (FR-014b).
	JoinKeys *investigationv1.JoinKeys
	// GraphEventIDs are the events a graph answer rests on (Invariant 11).
	GraphEventIDs []string
	// DeepLink resolves to the exact query; DeepLinkAbsentReason is required when it does not
	// exist (FR-057d).
	DeepLink             string
	DeepLinkAbsentReason string
	// Truncated and TruncationNote state truncation in the response it applies to (FR-037).
	Truncated      bool
	TruncationNote string
	// FreeText is the single bounded unverified field, never citable on its own (FR-014b).
	FreeText string
}

// Judgment is the only mechanism by which evidence moves a confidence (FR-020a).
//
// LnLR is stored rather than recomputed so the posterior is reproducible from the rows alone,
// which is what makes Invariant 1 checkable years after the LR table is next revised.
type Judgment struct {
	// ID is the judgment id.
	ID string
	// HypothesisID and EvidenceID are the pair. At most one judgment exists per pair
	// (ReasonDuplicateJudgment, FR-020a).
	HypothesisID string
	EvidenceID   string
	// Direction and Strength are the typed judgment.
	Direction Direction
	Strength  Strength
	// LnLR is the published constant for (Direction, Strength), signed by the direction.
	LnLR float64
	// Source is where the judgment came from.
	Source JudgmentSource
	// WorkerCallID is the call it came from, where there was one.
	WorkerCallID string
	// RecordedAt is when it was recorded.
	RecordedAt time.Time
}

// Judged reports whether the judgment moves anything. A neutral judgment is recorded and inert.
func (j Judgment) Judged() bool { return j.Direction != Neutral && j.LnLR != 0 }
