// SPDX-License-Identifier: Apache-2.0

// Package eval is the evaluation harness: the layer above `internal/fixture`, `internal/query`
// and the investigation engine that runs a corpus fixture and scores what came back.
//
// It exists because an engine built for a fixture needs three things that live in three
// different places — the `incident:` block (`internal/fixture`), feature 001's read surface
// (`internal/query`, which itself imports `internal/fixture` and therefore cannot be imported by
// it) and the engine — and nothing below all three may wire them together. The CLI used to hold
// that wiring, which made it unreachable from anything but a cobra command; this package is the
// same wiring with the cobra command taken off the front of it.
//
// What it publishes, and the reason each piece is where it is:
//
//   - `Harness` / `NewHarness` / `Run` — one fixture, replayed into a database of its own, with
//     its recorded world beside it. Byte-for-byte the wiring `fixture record-trajectory` used,
//     because a harness that investigated a *slightly* different question would make every
//     number below a measurement of the harness;
//   - `RunOutcome` — everything a scorer needs, gathered at the moment the run stops. A scorer
//     that had to re-run the engine to read a number would double the cost of the evaluation and
//     would measure a second run rather than the one it is reporting on;
//   - `RunPolicy` / `RunSet` — three runs, extended to seven only when the three disagree
//     (FR-061);
//   - `Grade` — the knowability rule and the decoy rules (FR-061b, SC-007);
//   - `CheckInvariance` — the metamorphic invariants (FR-062a, SC-020).
package eval

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
)

// The three verdict spellings that are not a change identifier.
//
// They are the vocabulary a ground truth is written in, so a verdict and a ground truth compare
// as strings without either side guessing at the other's spelling.
const (
	// VerdictUnknown is "the investigation did not reach a verdict it could evidence". Before a
	// fixture's `knowability_time` it is the *correct* answer (FR-061b).
	VerdictUnknown = "unknown"
	// VerdictUnobserved is "no observed change explains this symptom" (FR-071b, SC-023).
	VerdictUnobserved = "unobserved"
	// VerdictNotChangeInducedPrefix begins the verdict of an incident no change caused at all.
	// It always carries a category after the colon.
	VerdictNotChangeInducedPrefix = "not_change_induced"
)

// RunOutcome is one investigation, as a scorer reads it.
//
// It is a flat, JSON-shaped record rather than a handle onto a live engine on purpose: the
// evaluation report (T109), the calibration table (T110) and the invariance checks (T108) all
// read it, they run in a different process from the one that produced it in `eval.yml`, and a
// number that had to be recomputed from an engine would be a number two of the three could not
// see.
//
// Fields are added, never changed: the exported shape here is what Track I's report writer is
// written against.
type RunOutcome struct {
	// FixtureID is the incident this run investigated, and Dir where it lives.
	FixtureID string `json:"fixture_id"`
	Dir       string `json:"dir,omitempty"`
	// RunIndex is 1-based: the run policy's first run is `pass@1` (FR-061).
	RunIndex int `json:"run_index"`
	// Kind is the model wiring the run used: `model-free` or `fake-model`, or a live
	// configuration's name.
	Kind string `json:"kind"`

	// ModelConfigDigest is the digest of the model configuration in force — config/model.yaml
	// as loaded — and ModelIDs the model ids that were actually called, sorted. FR-061 requires
	// both to be recorded on every evaluation run; a model-free run records the digest and no
	// ids, which is itself the statement that no model was called.
	ModelConfigDigest  string   `json:"model_config_digest"`
	ModelConfigVersion string   `json:"model_config_version,omitempty"`
	ModelIDs           []string `json:"model_ids,omitempty"`

	// Ledger is the belief state the run ended at, in the ledger's own rank order.
	Ledger []Hypothesis `json:"ledger"`

	// StopReason and StopDetail are the typed stop (FR-045b); Outcome is the investigation
	// outcome it implies.
	StopReason string `json:"stop_reason"`
	StopDetail string `json:"stop_detail,omitempty"`
	Outcome    string `json:"outcome"`

	// Verdict is what the run answered, in the ground truth's own vocabulary: a change
	// reference, `unobserved`, `not_change_induced: <category>` or `unknown`.
	Verdict string `json:"verdict"`
	// VerdictRefs are every spelling the graph publishes for the verdict's change, so a
	// hand-written ground truth matches whichever one it used.
	VerdictRefs []string `json:"verdict_refs,omitempty"`
	// VerdictConfidence and VerdictBucket are the top hypothesis's posterior and published
	// bucket.
	VerdictConfidence float64 `json:"verdict_confidence"`
	VerdictBucket     string  `json:"verdict_bucket,omitempty"`

	// CausalPath is the chain the engine reports from cause to symptom, so localisation,
	// attribution and mechanism can be scored separately and with partial credit (FR-061b).
	CausalPath []PathStep `json:"causal_path,omitempty"`

	// Citations are the evidence items the run rested on, each with the digest and pointer it
	// addresses, which is what citation validity is computed over (FR-061a).
	Citations []Citation `json:"citations,omitempty"`

	// Onset is the estimate the metrics worker produced, or nil where none was.
	Onset *OnsetEstimate `json:"onset,omitempty"`

	// The three latencies FR-059 publishes, taken from the trajectory's own instants. A
	// recording made under the constant clock a fixture replay uses reports zero for all three,
	// which is correct: a replayed run took no time in the world it is a recording of.
	TimeToProvisional         time.Duration `json:"time_to_provisional"`
	TimeToFirstTestedHypothis time.Duration `json:"time_to_first_tested_hypothesis"`
	TimeToConclusion          time.Duration `json:"time_to_conclusion"`

	// WorkerCalls counts the calls per `<worker>/<capability>`, and WorkerCallsTotal their sum.
	WorkerCalls      map[string]int `json:"worker_calls,omitempty"`
	WorkerCallsTotal int            `json:"worker_calls_total"`

	// Spend is the machine-readable consumption report (FR-044, SC-008).
	Spend *investigationv1.BudgetSpend `json:"spend,omitempty"`

	// NotRecorded is how many in-algebra questions the recorded world does not hold, and
	// MissRate their share of the questions asked of it. The miss rate gates the *fixture*, and
	// never the engine's score (FR-060).
	NotRecorded int     `json:"not_recorded"`
	MissRate    float64 `json:"miss_rate"`
	// MissedTerms names the published terms that missed, one entry per miss rather than one per
	// distinct name. The corpus report needs the counts: "compare missed forty times and
	// drill_down once" says the window grid is a notch too coarse, where the set of names alone
	// says only that both are thin.
	MissedTerms []string `json:"missed_terms,omitempty"`

	// ReplayIdentical says the run reproduced its own recording byte for byte, where a
	// recording existed to compare against. False with ReplayChecked false means "not checked",
	// which is not the same as "diverged", so both are published.
	ReplayIdentical bool `json:"replay_identical"`
	ReplayChecked   bool `json:"replay_checked"`
	// ReplayDivergence names the first diverging record when there was one.
	ReplayDivergence string `json:"replay_divergence,omitempty"`

	// TrajectoryDigest and RunID identify the recording this run produced.
	TrajectoryDigest string `json:"trajectory_digest"`
	RunID            string `json:"run_id"`
	// TrajectoryRecords is the recording's length, so a run that produced nothing is
	// distinguishable from one that was never recorded.
	TrajectoryRecords int `json:"trajectory_records"`
}

// Hypothesis is one row of the final ledger.
type Hypothesis struct {
	// ID is the ledger id and Rank its 1-based position in the published order.
	ID   string `json:"id"`
	Rank int    `json:"rank"`
	// Kind is `change`, `condition` or `no_observed_change`.
	Kind string `json:"kind"`
	// Statement is the plain-language claim.
	Statement string `json:"statement,omitempty"`
	// Posterior is the ledger-computed confidence, never model-stated (FR-023), and Bucket the
	// published bucket it is reported in.
	Posterior float64 `json:"posterior"`
	Bucket    string  `json:"bucket"`
	BucketLow float64 `json:"bucket_low"`
	BucketHi  float64 `json:"bucket_high"`
	// Status is where it stands: proposed, supported, refuted, inconclusive, untested,
	// exonerated.
	Status string `json:"status"`
	// CandidateChangeEntityID names the change a `change` hypothesis is about, and
	// CandidateChangeRefs every spelling the graph publishes for it.
	CandidateChangeEntityID string   `json:"candidate_change_entity_id,omitempty"`
	CandidateChangeRefs     []string `json:"candidate_change_refs,omitempty"`
	// Rationale is derived from the ledger, never from a model's prose. For an exonerated
	// hypothesis it carries the reason, which is what decoy grading reads.
	Rationale string `json:"rationale,omitempty"`
	// CausalRole is `cause` or `candidate_effect` (FR-029c), and is the other half of decoy
	// grading: a decoy exonerated for the wrong reason is not a pass.
	CausalRole string `json:"causal_role,omitempty"`
	// UntestedReason is required of an untested hypothesis.
	UntestedReason string `json:"untested_reason,omitempty"`
	// SupportingEvidenceIDs, RefutingEvidenceIDs and JudgmentIDs are what was brought against
	// it. They are what "refuted by evidence on its own target" is checked with: a decoy whose
	// ledger row carries no judgment was never tested, whatever its status reads.
	SupportingEvidenceIDs []string `json:"supporting_evidence_ids,omitempty"`
	RefutingEvidenceIDs   []string `json:"refuting_evidence_ids,omitempty"`
	JudgmentIDs           []string `json:"judgment_ids,omitempty"`
}

// Tested reports whether any judgment was applied to this hypothesis.
func (h Hypothesis) Tested() bool { return len(h.JudgmentIDs) > 0 }

// NamesChange reports whether this hypothesis is about the given change, in any spelling the
// graph publishes for it.
func (h Hypothesis) NamesChange(ref string) bool {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return false
	}
	if h.CandidateChangeEntityID == ref {
		return true
	}
	for _, each := range h.CandidateChangeRefs {
		if each == ref {
			return true
		}
	}
	return false
}

// PathStep is one hop of the causal path the engine reports, in the same shape a fixture's
// `ground_truth.causal_path` is written in, so the two compare directly.
type PathStep struct {
	// Entity is the node, `<namespace>=<value>` where the graph publishes such a reference.
	Entity string `json:"entity"`
	// Via is the edge to the next step; the last step names none.
	Via string `json:"via,omitempty"`
	// Role is `cause`, `mechanism` or `symptom`.
	Role string `json:"role,omitempty"`
}

// Citation is one evidence item as a scorer reads it: the digest it addresses and the pointer it
// was taken over. It is never telemetry (constitution IV).
type Citation struct {
	// EvidenceID is the ledger's id for it.
	EvidenceID string `json:"evidence_id"`
	// Worker and Capability say where the answer came from.
	Worker     string `json:"worker,omitempty"`
	Capability string `json:"capability,omitempty"`
	// ResponseDigest and ResponseKey address the recording beside the graph: the first is
	// sha256 of the canonical response, the second the world's own key for the term.
	ResponseDigest string `json:"response_digest,omitempty"`
	ResponseKey    string `json:"response_key,omitempty"`
	// Digest is the bounded structured answer itself, as canonical JSON — what constitution IV
	// calls a digest, and what a `decisive_evidence` predicate is evaluated against.
	//
	// It is carried on the outcome rather than looked up later because a predicate that had to
	// be re-answered from the world would be evaluated against what the world holds rather than
	// against what the run actually cited, and those are different claims: the first says the
	// evidence exists, the second says the investigation used it.
	Digest json.RawMessage `json:"digest,omitempty"`
	// Pointer is the telemetry pointer the answer was taken over, where there was one.
	Pointer string `json:"pointer,omitempty"`
	// Term is the algebra family, e.g. `compare`, `onset`, `errors_by_version`.
	Term string `json:"term,omitempty"`
	// Outcome is one of the six published term outcomes, never collapsed (FR-027).
	Outcome string `json:"outcome,omitempty"`
	// Mode is `live` or `recorded`.
	Mode string `json:"mode,omitempty"`
	// DeepLink resolves to the exact query, where one exists.
	DeepLink string `json:"deep_link,omitempty"`
}

// OnsetEstimate is the symptom onset as the run estimated it.
type OnsetEstimate struct {
	// Available says an estimate was produced; Reason says why not when it was not.
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
	// At is the estimate and Uncertainty its own uncertainty, which is the tolerance an
	// exoneration has to clear (FR-029b).
	At          time.Time     `json:"at,omitempty"`
	Uncertainty time.Duration `json:"uncertainty,omitempty"`
	// Method is the published method, and EvidenceID the `onset_estimate` item an exoneration
	// rests on (Invariant 9).
	Method     string `json:"method,omitempty"`
	EvidenceID string `json:"evidence_id,omitempty"`
	// ReferenceUsed is the instant the ranking was actually taken against, and FellBackToAlert
	// says it was the alert instant because no onset could be estimated.
	ReferenceUsed   time.Time `json:"reference_used,omitempty"`
	FellBackToAlert bool      `json:"fell_back_to_alert"`
}

// Top is the highest-ranked hypothesis, or the zero value when the ledger is empty.
func (o *RunOutcome) Top() Hypothesis {
	if o == nil || len(o.Ledger) == 0 {
		return Hypothesis{}
	}
	best := o.Ledger[0]
	for _, h := range o.Ledger[1:] {
		if h.Rank > 0 && (best.Rank == 0 || h.Rank < best.Rank) {
			best = h
		}
	}
	return best
}

// NamedChanges is every change the run named at `supported` — the answers it committed to.
func (o *RunOutcome) NamedChanges() []string {
	if o == nil {
		return nil
	}
	seen := map[string]struct{}{}
	var out []string
	for _, h := range o.Ledger {
		if h.Status != statusSupported {
			continue
		}
		for _, ref := range append([]string{h.CandidateChangeEntityID}, h.CandidateChangeRefs...) {
			if ref == "" {
				continue
			}
			if _, dup := seen[ref]; dup {
				continue
			}
			seen[ref] = struct{}{}
			out = append(out, ref)
		}
	}
	sort.Strings(out)
	return out
}

// Hypothesis returns the ledger row naming the given change, in any spelling.
func (o *RunOutcome) Hypothesis(changeRef string) (Hypothesis, bool) {
	if o == nil {
		return Hypothesis{}, false
	}
	for _, h := range o.Ledger {
		if h.NamesChange(changeRef) {
			return h, true
		}
	}
	return Hypothesis{}, false
}

// The ledger status spellings this package compares against, named once.
const (
	statusSupported    = "supported"
	statusExonerated   = "exonerated"
	statusRefuted      = "refuted"
	statusUntested     = "untested"
	statusInconclusive = "inconclusive"
)

// IsUnknown reports whether the run answered `unknown`.
func (o *RunOutcome) IsUnknown() bool { return o != nil && o.Verdict == VerdictUnknown }

// VerdictClass collapses the verdict to what it is *about*: a named change, the unobserved
// remainder, or nothing. Grading compares classes first and identifiers second, because
// `unobserved` and `not_change_induced: saturation` are the same answer to the question "did an
// observed change cause this" and differ only in the category the fixture labels.
func (o *RunOutcome) VerdictClass() string {
	switch {
	case o == nil || o.Verdict == "" || o.Verdict == VerdictUnknown:
		return VerdictUnknown
	case o.Verdict == VerdictUnobserved,
		strings.HasPrefix(o.Verdict, VerdictNotChangeInducedPrefix):
		return VerdictUnobserved
	default:
		return "change_induced"
	}
}
