// SPDX-License-Identifier: Apache-2.0

package render

import (
	"fmt"
	"sort"
	"strings"
	"time"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/investigation/ledger"
)

// The published rendering order (T085, FR-019, FR-028, FR-057c, FR-057d, SC-025).
//
// Four sections, in this order and no other:
//
//  1. **the verdict** — one line naming the decisive fact and the rollback candidate, or saying
//     there is none;
//  2. **the ranked hypotheses** — each with its evidence for and against, and exonerations
//     rendered as prominently as supports;
//  3. **the timeline** — the candidate changes and the estimated symptom onset;
//  4. **the narrative** — which never precedes the verdict or the list, and contains no claim
//     absent from them.
//
// The order is not a style choice. An on-call reading a pager at 03:00 reads the first line and
// acts; everything after it exists to be checked afterwards, by them or by a reviewer. A
// narrative first would bury the one line that matters under prose, and prose is where a model's
// unsupported claims live. Putting the narrative last, after the evidence, is what makes "it
// contains no claim absent from them" a checkable property rather than a hope.
//
// Two rules the renderer enforces rather than trusts:
//
// **Every evidence item carries a deep link or says why none exists** (FR-057d). RenderEvidence
// has no path that emits a bare citation.
//
// **No remediation** (FR-028). Guard below rejects a rendering that proposes, prepares or names
// an action against production. It deliberately does *not* reject naming the rollback candidate:
// FR-057c requires that, and "the change that could be rolled back is X" is a diagnosis. What is
// rejected is the imperative — "roll it back", "restart the pods", "run kubectl …" — and any
// prepared command. The line between them is the line between telling someone what is broken and
// telling them what to do about it.

// Section names, in published order. The renderer emits them in exactly this sequence and a test
// asserts it, because the order is the contract (FR-057c).
const (
	SectionVerdict   = "verdict"
	SectionRanked    = "ranked"
	SectionTimeline  = "timeline"
	SectionNarrative = "narrative"
)

// SectionOrder is the published order. Nothing may be rendered outside it.
var SectionOrder = []string{SectionVerdict, SectionRanked, SectionTimeline, SectionNarrative}

// TimelineEntry is one moment in the timeline: a candidate change, the estimated onset, or a
// symptom arriving.
type TimelineEntry struct {
	// At is the instant.
	At time.Time
	// Kind is `change`, `onset`, `symptom` or `human_fact`.
	Kind string
	// Statement is the line a person reads.
	Statement string
	// EntityID is what it concerns, where it concerns one.
	EntityID string
	// EvidenceID is the item that establishes it; DeepLink is that item's link.
	EvidenceID string
	DeepLink   string
	// DeepLinkAbsentReason is required when DeepLink is empty (FR-057d).
	DeepLinkAbsentReason string
	// Uncertainty is the onset estimate's tolerance, rendered beside it so that "this change
	// started after onset" can be read as the estimate it is.
	Uncertainty time.Duration
}

// The published timeline kinds.
const (
	TimelineChange    = "change"
	TimelineOnset     = "onset"
	TimelineSymptom   = "symptom"
	TimelineHumanFact = "human_fact"
)

// Report is everything one rendering needs. It is assembled by the engine and rendered here, so
// that the human form and the machine form come from one structure and cannot disagree
// (FR-064: "the same claims from the same run").
type Report struct {
	// Investigation is the run, with its lifecycle, outcome, verdict line and symptoms.
	Investigation *investigationv1.Investigation
	// Ledger is the projection of the hypothesis ledger the rendering reads. Required: the
	// ranked list is derived from it, never materialised (data-model §Derived structures).
	Ledger LedgerView
	// ledger is the engine's live ledger, when the report was built from one. It exists so that
	// Machine() can emit the canonical wire ledger rather than re-deriving it from a projection.
	ledger *ledger.Ledger
	// Timeline is the candidate changes and the estimated onset, in time order.
	Timeline []TimelineEntry
	// Narrative is the prose. It may be empty; an empty narrative is better than an unsupported
	// one, and the verifier removes claims it cannot cite (FR-022a).
	Narrative string
	// Resolutions are "what would resolve this" (FR-026, FR-045). Always rendered for an
	// `unknown` outcome; rendered whenever present otherwise.
	Resolutions []*investigationv1.Resolution
	// Notes are sentences the intake and the extent consultation produced — an invisible merge,
	// a coverage gap — that a reader must see to interpret the answer (FR-032, FR-003).
	Notes []string
}

// NewReport builds a report from the engine's live ledger.
func NewReport(inv *investigationv1.Investigation, l *ledger.Ledger) *Report {
	return &Report{Investigation: inv, Ledger: FromLedger(l), ledger: l}
}

// FromProto builds a report from an Investigation that came back over the wire, so the CLI
// renders the published order from the same code the engine does (FR-064).
//
// It fails on a message with no ledger: the ranked list is derived from the ledger and a report
// that invented one would be a second set of claims.
func FromProto(inv *investigationv1.Investigation) (*Report, error) {
	if inv == nil {
		return nil, fmt.Errorf("render: no investigation")
	}
	report := &Report{
		Investigation: inv,
		Ledger:        FromProtoLedger(inv.GetLedger()),
		Resolutions:   inv.GetResolutions(),
	}
	report.Timeline = timelineFromProto(inv)
	return report, nil
}

// timelineFromProto derives the timeline entries a wire message already carries: the symptoms and
// the human facts. Candidate changes and the onset estimate are the engine's to supply — they
// rest on evidence items the message carries but does not order — so a CLI-side rendering shows
// what it can establish and says nothing it cannot.
func timelineFromProto(inv *investigationv1.Investigation) []TimelineEntry {
	var out []TimelineEntry
	for _, s := range inv.GetSymptoms() {
		if s.GetFiredAt() == nil {
			continue
		}
		statement := s.GetStatement()
		if s.GetAttachedAsAdditional() {
			statement += " (attached to this incident as an additional symptom)"
		}
		out = append(out, TimelineEntry{
			At: s.GetFiredAt().AsTime().UTC(), Kind: TimelineSymptom,
			Statement: statement, EvidenceID: s.GetGroupingEvidenceId(),
			DeepLinkAbsentReason: "a symptom is an intake record; `investigate get <id>` prints it",
		})
	}
	for _, f := range inv.GetFacts() {
		if f.GetSubmittedAt() == nil {
			continue
		}
		out = append(out, TimelineEntry{
			At: f.GetSubmittedAt().AsTime().UTC(), Kind: TimelineHumanFact,
			Statement:            fmt.Sprintf("%s (%s, by %s)", f.GetStatement(), f.GetKind(), f.GetAuthor()),
			EvidenceID:           f.GetEvidenceId(),
			DeepLinkAbsentReason: "a human fact is not a query; it was submitted by " + f.GetAuthor(),
		})
	}
	return out
}

// Human renders the published order (FR-057c).
//
// It returns an error when the guard rejects the output (FR-028): a rendering that proposes a
// remediation is not emitted with a warning, it is not emitted.
func (r *Report) Human() (string, error) {
	if r == nil {
		return "", fmt.Errorf("render: no report")
	}
	var b strings.Builder
	r.writeVerdict(&b)
	r.writeRanked(&b)
	r.writeTimeline(&b)
	r.writeNarrative(&b)

	out := b.String()
	if err := Guard(out); err != nil {
		return "", err
	}
	return out, nil
}

// Machine returns the machine-readable rendering: the wire Investigation with its ledger,
// resolutions and verdict filled in from the same structure the human form read (FR-064).
//
// It contains no claim absent from the human form, because both are projections of Report.
func (r *Report) Machine() *investigationv1.Investigation {
	if r == nil {
		return nil
	}
	out := r.Investigation
	if out == nil {
		out = &investigationv1.Investigation{}
	}
	if r.ledger != nil {
		out.Ledger = r.ledger.Proto()
	}
	if out.GetVerdictLine() == "" {
		out.VerdictLine = r.VerdictLine()
	}
	if len(r.Resolutions) > 0 {
		out.Resolutions = r.Resolutions
	}
	return out
}

// VerdictLine is the one line an on-call acts on (FR-057c).
//
// It names the decisive fact and the rollback candidate, or says there is none. "There is none"
// is a real answer and the commonest correct one when the open hypothesis leads — saying it
// plainly is what stops a reader inventing a culprit from a ranked list.
func (r *Report) VerdictLine() string {
	if line := strings.TrimSpace(r.Investigation.GetVerdictLine()); line != "" {
		return line
	}
	ranked := r.Ledger.Hypotheses
	if len(ranked) == 0 {
		return "no hypotheses were formed; there is no rollback candidate"
	}
	top := ranked[0]
	bucket := top.Bucket
	if bucket == "" {
		bucket = "no bucket"
	}

	if top.Kind == string(ledger.KindNoObservedChange) {
		return fmt.Sprintf("no observed change explains this (%s); there is no rollback candidate", bucket)
	}
	decisive := r.decisiveFact(top)
	candidate := top.CandidateChangeEntityID
	if candidate == "" {
		return fmt.Sprintf("%s (%s); %s; there is no rollback candidate",
			top.Statement, bucket, decisive)
	}
	// An operator already rolled production back from it (004 T155): the change is still the one the
	// evidence points at, but an on-call reading "rollback candidate" would reach for a button someone
	// already pressed, so the line says so.
	if r.rolledBack(top) {
		return fmt.Sprintf("%s (%s); %s; rollback candidate: %s (already rolled back by an operator)",
			top.Statement, bucket, decisive, candidate)
	}
	return fmt.Sprintf("%s (%s); %s; rollback candidate: %s",
		top.Statement, bucket, decisive, candidate)
}

// rolledBack reports whether a platform-stated rollback away from the hypothesis's change supports it.
func (r *Report) rolledBack(h HypothesisView) bool {
	for _, j := range r.Ledger.JudgmentsFor(h.ID) {
		if j.Source == string(ledger.SourceRollback) && j.Direction == string(ledger.Supports) {
			return true
		}
	}
	return false
}

// decisiveFact names the strongest supporting judgment's evidence, which is what "the decisive
// fact" means operationally: the single item that moved the answer most.
func (r *Report) decisiveFact(h HypothesisView) string {
	judgments := r.Ledger.JudgmentsFor(h.ID)
	var best *JudgmentView
	for i := range judgments {
		j := &judgments[i]
		if j.Direction != string(ledger.Supports) {
			continue
		}
		if best == nil || j.LnLR > best.LnLR {
			best = j
		}
	}
	if best == nil {
		return "no decisive fact: nothing tested separated it"
	}
	item, ok := r.Ledger.EvidenceByID(best.EvidenceID)
	if !ok {
		return "decisive fact: " + best.EvidenceID
	}
	// The verdict line is a rendering like any other, so the decisive fact it names carries its
	// deep link too (FR-057d): the one line an on-call acts on is the line they are most likely
	// to want to check by hand.
	return "decisive fact: " + EvidenceLine(item)
}

func (r *Report) writeVerdict(b *strings.Builder) {
	fmt.Fprintf(b, "## %s\n\n", SectionVerdict)
	fmt.Fprintf(b, "%s\n", r.VerdictLine())
	if outcome := r.Investigation.GetOutcome(); outcome != investigationv1.InvestigationOutcome_INVESTIGATION_OUTCOME_UNSPECIFIED {
		fmt.Fprintf(b, "outcome: %s", outcomeWord(outcome))
		if kind := r.Investigation.GetConclusionKind(); kind != investigationv1.ConclusionKind_CONCLUSION_KIND_UNSPECIFIED {
			fmt.Fprintf(b, " (%s conclusion)", strings.ToLower(strings.TrimPrefix(kind.String(), "CONCLUSION_")))
		}
		b.WriteString("\n")
	}
	if r.Investigation.GetProvisional() {
		b.WriteString("provisional: this is the prior-only ranking; nothing has been tested yet\n")
	}
	for _, note := range r.Notes {
		fmt.Fprintf(b, "note: %s\n", note)
	}
	b.WriteString("\n")
}

func (r *Report) writeRanked(b *strings.Builder) {
	fmt.Fprintf(b, "## %s\n\n", SectionRanked)
	for _, h := range r.Ledger.Hypotheses {
		// The id is printed beside the rank because it is what a reviewer types into
		// `investigate review --amend <id>=<status>`: a ranked list nobody can refer to is a
		// list nobody can correct (FR-054).
		fmt.Fprintf(b, "%d. [%s] %s — %s (%s)\n", h.Rank, h.Status, h.Statement, h.Bucket, h.ID)
		if h.Widened {
			fmt.Fprintf(b, "   bucket widened %s: %s\n", h.WidenedDirection, h.WidenedReason)
		}
		if h.Status == string(ledger.StatusUntested) && h.UntestedReason != "" {
			fmt.Fprintf(b, "   untested: %s\n", h.UntestedReason)
		}
		if h.Rationale != "" {
			fmt.Fprintf(b, "   %s\n", h.Rationale)
		}
		r.writeJudgments(b, h)
	}
	b.WriteString("\n")
}

// writeJudgments renders supports, refutations and exonerations under one hypothesis.
//
// **Exonerations are rendered as prominently as supports** (FR-029c, FR-057c): same indentation,
// same shape, same deep link. A rendering that pushed "this change is ruled out" into a footnote
// would be telling a reader what the engine found interesting rather than what it found.
func (r *Report) writeJudgments(b *strings.Builder, h HypothesisView) {
	judgments := r.Ledger.JudgmentsFor(h.ID)
	sort.SliceStable(judgments, func(i, j int) bool {
		return directionOrder(judgments[i]) < directionOrder(judgments[j])
	})
	for _, j := range judgments {
		label := j.Direction
		switch j.Source {
		case string(ledger.SourceExoneration):
			label = "exonerates"
		case string(ledger.SourceRollback):
			// Said in words: the evidence line is the graph answer, and "supports: graph.diff" would hide
			// that the support is an operator having rolled production back from this change (004 T155).
			label = "supports — production was rolled back away from this change"
		}
		item, ok := r.Ledger.EvidenceByID(j.EvidenceID)
		if !ok {
			fmt.Fprintf(b, "   %s (%s): %s\n", label, j.Strength, j.EvidenceID)
			continue
		}
		fmt.Fprintf(b, "   %s (%s): %s\n", label, j.Strength, EvidenceLine(item))
	}
	if h.Status == string(ledger.StatusExonerated) && h.OnsetEvidenceID != "" {
		if onset, ok := r.Ledger.EvidenceByID(h.OnsetEvidenceID); ok {
			fmt.Fprintf(b, "   exonerated against the onset estimate: %s\n", EvidenceLine(onset))
		} else {
			fmt.Fprintf(b, "   exonerated against the onset estimate [%s] "+
				"(no deep link: the onset estimate is not in this ledger)\n", h.OnsetEvidenceID)
		}
	}
}

func directionOrder(j JudgmentView) int {
	switch {
	case j.Source == string(ledger.SourceExoneration):
		return 1
	case j.Direction == string(ledger.Supports):
		return 0
	case j.Direction == string(ledger.Refutes):
		return 2
	default:
		return 3
	}
}

// EvidenceLine renders one evidence item with its deep link, or with the reason there is none
// (FR-057d). It is the only way an item reaches a rendering, so there is no path to a bare
// citation.
func EvidenceLine(item EvidenceView) string {
	detail := item.FreeText
	if detail == "" {
		detail = fmt.Sprintf("%s.%s → %s", item.Worker, item.Capability, item.Outcome)
	}
	link := item.DeepLink
	if link == "" {
		link = "no deep link: " + item.DeepLinkAbsentReason
	}
	line := fmt.Sprintf("%s [%s] (%s)", detail, item.ID, link)
	if item.Truncated {
		line += " — truncated: " + item.TruncationNote
	}
	return line
}

func (r *Report) writeTimeline(b *strings.Builder) {
	fmt.Fprintf(b, "## %s\n\n", SectionTimeline)
	if len(r.Timeline) == 0 {
		b.WriteString("no candidate change and no onset estimate were established\n\n")
		return
	}
	entries := append([]TimelineEntry(nil), r.Timeline...)
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].At.Before(entries[j].At) })
	for _, e := range entries {
		fmt.Fprintf(b, "%s  %-10s %s", e.At.UTC().Format(time.RFC3339), e.Kind, e.Statement)
		if e.Kind == TimelineOnset && e.Uncertainty > 0 {
			fmt.Fprintf(b, " (±%s)", e.Uncertainty)
		}
		if e.EntityID != "" {
			fmt.Fprintf(b, " [%s]", e.EntityID)
		}
		switch {
		case e.DeepLink != "":
			fmt.Fprintf(b, " (%s)", e.DeepLink)
		case e.DeepLinkAbsentReason != "":
			fmt.Fprintf(b, " (no deep link: %s)", e.DeepLinkAbsentReason)
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")
}

func (r *Report) writeNarrative(b *strings.Builder) {
	fmt.Fprintf(b, "## %s\n\n", SectionNarrative)
	narrative := strings.TrimSpace(r.Narrative)
	if narrative == "" {
		narrative = "no narrative was produced; the ranked list above is the whole of the answer."
	}
	b.WriteString(narrative)
	b.WriteString("\n")

	if len(r.Resolutions) > 0 {
		b.WriteString("\n" + WhatWouldResolveThis(r.Resolutions))
	}
}

func outcomeWord(o investigationv1.InvestigationOutcome) string {
	switch o {
	case investigationv1.InvestigationOutcome_RANKED:
		return "ranked"
	case investigationv1.InvestigationOutcome_UNKNOWN:
		return "unknown"
	case investigationv1.InvestigationOutcome_BUDGET_EXHAUSTED:
		return "budget_exhausted"
	case investigationv1.InvestigationOutcome_INVESTIGATION_FAILED:
		return "failed"
	default:
		return "unspecified"
	}
}
