// SPDX-License-Identifier: Apache-2.0

package ledger

import (
	"fmt"
	"strings"
)

// The turn-scoped rendering (T057, FR-020b, FR-045).
//
// This is the text the engine puts back into the model's context at the start of every turn, and
// it is the whole of what the model knows about the state of the investigation. That gives it two
// jobs that pull against each other: it must be complete enough that nothing the model needs is
// outside it, and short enough that re-injecting it each turn is affordable. Everything below is
// a consequence of that trade:
//
//   - one line per hypothesis, in published rank order, carrying the id, kind, status, prior,
//     confidence *with its bucket range*, the counts of judgments each way, and the statement
//     truncated to a published width;
//   - the untested hypotheses listed again underneath with their reason and the exact next query,
//     because FR-031 says an untested hypothesis is reported with what would test it, and a
//     count in a table cannot carry a query;
//   - conflicts and non-separable groups spelled out, both sides each time (FR-024, FR-025);
//   - the budget remaining and the stop conditions in force, so the model can see the end coming
//     rather than being cut off by it (FR-045).
//
// The rendering is deterministic: same ledger, same bytes, on any machine. It is also *not* the
// export — a grader reads the canonical JSON, which carries everything; this is the working
// summary, and the truncation it applies is stated where it applies.

// VerdictRule is the sentence every rendering carries beside the buckets (Phase 8 K1, FR-022,
// FR-023, constitution V).
//
// It says the two halves of the rule in the order a reader needs them: what may be named, and what
// a confidence with nothing behind it is reported as. `unknown` and `unobserved` are the answers
// when nothing qualifies, and both are findings — the first says the run can still test, the
// second that it tested and nothing observed explains the symptom.
const VerdictRule = "verdict rule: prior-only mass is not confidence — a hypothesis is named only with a " +
	"supporting judgment from telemetry evidence or a human fact, and one without is reported no higher " +
	"than `moderate` whatever its posterior."

// statementWidth is the published width a statement is truncated to in the compact table. Long
// enough for "payments@rev7 rolled out at 14:28, two minutes before onset"; short enough that
// fifty hypotheses stay inside a screen and inside a turn's budget.
const statementWidth = 72

// BudgetLine is one budget's remaining headroom, as the budget manager reports it.
//
// Phase 6 owns the budget manager (`internal/investigation/budget`). This is the minimal shape
// the ledger's rendering needs from it, declared here so Phase 5 does not depend on a package
// that does not exist yet; when Phase 6 publishes its own, it maps onto this in one line.
type BudgetLine struct {
	// Name is the budget: "wall_time", "model_tokens", "worker_calls", "worker_calls:metrics".
	Name string
	// Remaining and Limit are in Unit. Remaining is what is left, not what was spent, because
	// what is left is the number that changes a decision.
	Remaining float64
	Limit     float64
	// Unit is "s", "tokens", "calls" or another published unit.
	Unit string
}

// String renders "wall_time 118/300 s".
func (b BudgetLine) String() string {
	unit := ""
	if b.Unit != "" {
		unit = " " + b.Unit
	}
	return fmt.Sprintf("%s %s/%s%s", b.Name, trimNumber(b.Remaining), trimNumber(b.Limit), unit)
}

// RenderContext is what the engine knows and the ledger does not: how much budget is left and
// which stop conditions are armed. Both are required by FR-045 to appear in the turn rendering,
// and both are empty in a unit test of the ledger alone, which renders without those two lines.
type RenderContext struct {
	// Budgets are the budgets in force, in the order the budget manager publishes them.
	Budgets []BudgetLine
	// StopConditions are the stop conditions armed, one sentence each: the reserve threshold,
	// the diminishing-returns threshold, a deadline.
	StopConditions []string
}

// Render returns the compact table the engine re-injects each turn (FR-020b).
func (l *Ledger) Render(ctx RenderContext) string {
	var b strings.Builder

	fmt.Fprintf(&b, "LEDGER %s · rule %s · π₀ %.6f", l.investigationID, LedgerRuleVersion, l.prior.Prior)
	if l.prior.AuditID != "" {
		fmt.Fprintf(&b, " (audit %s, ceiling %.6f, %d incidents)",
			l.prior.AuditID, l.prior.Ceiling, l.prior.IncidentCount)
	} else {
		// No audit means π₀ = 1 and it must say so: every candidate sits at prior 0 and the
		// engine's honest answer is `unknown` until a ceiling is measured (FR-069a).
		b.WriteString(" (no coverage audit published; the open hypothesis holds all the mass)")
	}
	b.WriteString("\nbuckets: ")
	for i, bucket := range buckets {
		if i > 0 {
			b.WriteString(" · ")
		}
		b.WriteString(bucket.String())
	}
	// The verdict rule, stated where the confidences are read rather than only in the
	// documentation: the model is about to reason over this table, and a number it reads as
	// `high` when nothing observed supports it is a number it will act on (Phase 8 K1).
	b.WriteString("\n" + VerdictRule + "\n\n")

	b.WriteString("  # id                    kind                status        prior      confidence  bucket      judgments  statement\n")
	for _, h := range l.Hypotheses() {
		supports, refutes, neutrals := l.counts(h.ID)
		judgments := fmt.Sprintf("+%d/-%d", supports, refutes)
		if neutrals > 0 {
			judgments += fmt.Sprintf("/~%d", neutrals)
		}
		fmt.Fprintf(&b, "%3d %-21s %-19s %-13s %-10.6f %-11.6f %-11s %-10s %s\n",
			h.Rank, h.ID, h.Kind, h.Status, h.Prior, h.Confidence, h.Bucket.Name, judgments,
			truncate(h.Statement, statementWidth))
	}

	l.renderUntested(&b)
	l.renderExonerated(&b)
	l.renderConflicts(&b)
	l.renderNonSeparable(&b)
	renderBudgets(&b, ctx)
	return b.String()
}

// counts is the per-hypothesis judgment tally the table's `+n/-n` column carries. Neutral
// judgments are counted separately and never folded into either side: "we looked and it did not
// separate" is a different answer from "we did not look" (FR-027's discipline, applied to
// judgments).
func (l *Ledger) counts(hypothesisID string) (supports, refutes, neutrals int) {
	for _, j := range l.judgments {
		if j.HypothesisID != hypothesisID {
			continue
		}
		switch j.Direction {
		case Supports:
			supports++
		case Refutes:
			refutes++
		case Neutral:
			neutrals++
		}
	}
	return supports, refutes, neutrals
}

func (l *Ledger) renderUntested(b *strings.Builder) {
	var lines []string
	for _, h := range l.Hypotheses() {
		if h.Status != StatusUntested {
			continue
		}
		line := fmt.Sprintf("  %s — %s", h.ID, h.UntestedReason)
		if h.NextQuery != nil {
			line += "; next query: " + termSummary(h)
		}
		if h.NextQueryDeepLink != "" {
			line += "; link: " + h.NextQueryDeepLink
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return
	}
	b.WriteString("\nuntested (never scored as refuted, never dropped — FR-031):\n")
	b.WriteString(strings.Join(lines, "\n") + "\n")
}

func (l *Ledger) renderExonerated(b *strings.Builder) {
	var lines []string
	for _, h := range l.Hypotheses() {
		if h.Status != StatusExonerated {
			continue
		}
		line := fmt.Sprintf("  %s — %s; onset evidence: %s",
			h.ID, truncate(h.Statement, statementWidth), h.OnsetEvidenceID)
		// The reason is what makes an exoneration reviewable: "not the cause" is a verdict, and
		// "starts 5m after the onset, outside its ±90s uncertainty" is the argument for it.
		if reason := l.exonerations[h.ID]; reason != "" {
			line += "; " + reason
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return
	}
	b.WriteString("\nexonerated (a candidate effect, not a cause — FR-029c):\n")
	b.WriteString(strings.Join(lines, "\n") + "\n")
}

func (l *Ledger) renderConflicts(b *strings.Builder) {
	conflicts := l.Conflicts()
	if len(conflicts) == 0 {
		return
	}
	b.WriteString("\nconflicting evidence (both sides reported, both sides in the number — FR-024):\n")
	for _, c := range conflicts {
		fmt.Fprintf(b, "  %s — supporting: %s; refuting: %s; net ln LR %+.6f → %.6f (%s)\n",
			c.HypothesisID, judgmentList(c.Supporting), judgmentList(c.Refuting),
			c.NetLnLR, c.Confidence, c.Bucket.Name)
	}
}

func (l *Ledger) renderNonSeparable(b *strings.Builder) {
	groups := l.NonSeparable()
	if len(groups) == 0 {
		return
	}
	b.WriteString("\nnot separable by the evidence (FR-025):\n")
	for _, g := range groups {
		ids := make([]string, 0, len(g.Hypotheses))
		for _, h := range g.Hypotheses {
			ids = append(ids, fmt.Sprintf("%s at %.6f", h.ID, h.Confidence))
		}
		fmt.Fprintf(b, "  %s (spread %.6f)\n", strings.Join(ids, ", "), g.Spread)
		for _, s := range g.Separators {
			what := "none recorded — nothing this deployment can ask would tell them apart"
			switch {
			case s.DeepLink != "":
				what = s.DeepLink
			case s.Known:
				what = "the recorded next query"
			}
			fmt.Fprintf(b, "    what would separate %s: %s\n", s.HypothesisID, what)
		}
	}
}

func renderBudgets(b *strings.Builder, ctx RenderContext) {
	if len(ctx.Budgets) > 0 {
		lines := make([]string, 0, len(ctx.Budgets))
		for _, budget := range ctx.Budgets {
			lines = append(lines, budget.String())
		}
		fmt.Fprintf(b, "\nbudget remaining: %s\n", strings.Join(lines, " · "))
	}
	if len(ctx.StopConditions) > 0 {
		fmt.Fprintf(b, "stop conditions in force: %s\n", strings.Join(ctx.StopConditions, "; "))
	}
}

// judgmentList renders "e2 (moderate), e5 (strong)" — evidence ids, never digest contents.
func judgmentList(judgments []Judgment) string {
	parts := make([]string, 0, len(judgments))
	for _, j := range judgments {
		parts = append(parts, fmt.Sprintf("%s (%s)", j.EvidenceID, j.Strength))
	}
	return strings.Join(parts, ", ")
}

// termSummary names the next query without inlining the whole term: the term travels in the
// export and in the deep link, and the turn rendering says which capability it would call.
func termSummary(h Hypothesis) string {
	term := h.NextQuery
	if term == nil {
		return ""
	}
	switch {
	case term.GetCompare() != nil:
		return "compare"
	case term.GetOnset() != nil:
		return "onset"
	case term.GetNewLogPatterns() != nil:
		return "new_log_patterns"
	case term.GetErrorSpans() != nil:
		return "error_spans"
	case term.GetErrorsByVersion() != nil:
		return "errors_by_version"
	case term.GetMonitorState() != nil:
		return "monitor_state"
	case term.GetExemplars() != nil:
		return "exemplars"
	case term.GetDrillDown() != nil:
		return "drill_down"
	case term.GetGraph() != nil:
		return "graph"
	case term.GetKnowledgeSearch() != nil:
		return "knowledge_search"
	default:
		return "term"
	}
}

func truncate(s string, width int) string {
	if len(s) <= width {
		return s
	}
	if width <= 1 {
		return s[:width]
	}
	return s[:width-1] + "…"
}

// trimNumber renders a budget figure without trailing zeros, so "118" stays "118" and "0.5"
// stays "0.5".
func trimNumber(v float64) string {
	s := fmt.Sprintf("%.2f", v)
	s = strings.TrimRight(s, "0")
	return strings.TrimSuffix(s, ".")
}
