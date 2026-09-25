// SPDX-License-Identifier: Apache-2.0

package render

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/investigation/ledger"
)

// The `unknown` outcome and the no-remediation guard (T086, FR-026, FR-028, FR-057, SC-007,
// SC-024).
//
// `unknown` is a *result*, not a failure to produce one. It says: here is everything that was
// considered, here is what each of them rests on, and here is at least one concrete thing that
// would settle it. The engine reaches it whether or not anyone answers — it is report-only and
// never blocks (FR-057) — so "what would resolve this" is a list of requests a person may act on
// at their leisure, not a question the run is waiting for.
//
// The five published resolution kinds are the five ways an investigation can be stuck:
// a source is not connected, an entity has no pointer of the needed kind, an identity is
// unconfirmed, the window is too narrow, or only a person knows.

// The published `Resolution.kind` values (FR-026, FR-045).
const (
	// ResolutionConnectSource: a telemetry source the answer needs is not connected.
	ResolutionConnectSource = "connect_source"
	// ResolutionAddPointer: the entity has no pointer of the kind the test needs.
	ResolutionAddPointer = "add_pointer"
	// ResolutionConfirmIdentity: an identifier resolved to nothing, or to more than one thing.
	ResolutionConfirmIdentity = "confirm_identity"
	// ResolutionWidenWindow: the window did not reach far enough back.
	ResolutionWidenWindow = "widen_window"
	// ResolutionAskHuman: only a person knows.
	ResolutionAskHuman = "ask_human"
)

// ResolutionKinds lists the published kinds, for a CLI that has to say what it accepts.
func ResolutionKinds() []string {
	return []string{ResolutionConnectSource, ResolutionAddPointer, ResolutionConfirmIdentity,
		ResolutionWidenWindow, ResolutionAskHuman}
}

// ErrUnknownWithoutResolution is an `unknown` outcome that names nothing that would resolve it.
// It is refused: "we do not know" with no way forward is the one shape of `unknown` FR-026
// forbids, because it leaves the reader with nothing to do and the corpus with nothing to learn.
var ErrUnknownWithoutResolution = errors.New(
	"an `unknown` outcome must name at least one concrete thing that would resolve it (FR-026)")

// Unknown renders the `unknown` terminal outcome: the hypotheses considered with their statuses
// and evidence, and what would resolve it (FR-026).
//
// It is the ordinary Human rendering with one extra guarantee — the resolutions section is
// present and non-empty — so an `unknown` report reads like every other report rather than like
// an error page.
func (r *Report) Unknown() (string, error) {
	if len(r.Resolutions) == 0 {
		return "", ErrUnknownWithoutResolution
	}
	return r.Human()
}

// WhatWouldResolveThis renders the resolving actions, each with the evidence it would change
// (FR-026, FR-057).
func WhatWouldResolveThis(resolutions []*investigationv1.Resolution) string {
	if len(resolutions) == 0 {
		return ""
	}
	sorted := append([]*investigationv1.Resolution(nil), resolutions...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].GetKind() < sorted[j].GetKind() })

	var b strings.Builder
	b.WriteString("what would resolve this:\n")
	for _, res := range sorted {
		fmt.Fprintf(&b, "- [%s] %s", res.GetKind(), res.GetStatement())
		if id := res.GetHypothesisId(); id != "" {
			fmt.Fprintf(&b, " — would change %s", id)
		}
		if link := res.GetDeepLink(); link != "" {
			fmt.Fprintf(&b, " (%s)", link)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// ResolutionsFor derives the resolving actions an `unknown` outcome carries, from the ledger
// itself (FR-026, FR-031, FR-045).
//
// Every untested hypothesis is a thing that would resolve it: the reason it was untested names
// what is missing, and its next query names the exact term that would test it. That is the whole
// derivation — the engine does not invent requests, it reports the ones its own ledger already
// implies. `extra` are the requests intake produced (an unresolved subject, a declaration with no
// target), which the ledger cannot know about.
func ResolutionsFor(l *ledger.Ledger, extra ...*investigationv1.Resolution) []*investigationv1.Resolution {
	out := append([]*investigationv1.Resolution(nil), extra...)
	if l == nil {
		return out
	}
	for _, h := range l.Hypotheses() {
		if h.Status != ledger.StatusUntested {
			continue
		}
		res := &investigationv1.Resolution{
			Statement:    resolutionStatement(h),
			Kind:         resolutionKindFor(h),
			NextQuery:    h.NextQuery,
			DeepLink:     h.NextQueryDeepLink,
			HypothesisId: h.ID,
		}
		out = append(out, res)
	}
	return out
}

func resolutionStatement(h ledger.Hypothesis) string {
	reason := h.UntestedReason
	if reason == "" {
		reason = "it was not tested and no reason was recorded"
	}
	return fmt.Sprintf("test %q: %s", h.Statement, reason)
}

// resolutionKindFor maps an untested hypothesis's reason onto one of the five published kinds.
// It reads the reason's own words rather than a separate enum, because the reason is what the
// engine recorded and a second field would be a second thing to keep in step.
func resolutionKindFor(h ledger.Hypothesis) string {
	reason := strings.ToLower(h.UntestedReason)
	switch {
	case strings.Contains(reason, "pointer"):
		return ResolutionAddPointer
	case strings.Contains(reason, "window"):
		return ResolutionWidenWindow
	case strings.Contains(reason, "identity"), strings.Contains(reason, "resolve"):
		return ResolutionConfirmIdentity
	case strings.Contains(reason, "not connected"), strings.Contains(reason, "no source"),
		strings.Contains(reason, "not_recorded"), strings.Contains(reason, "backend"):
		return ResolutionConnectSource
	default:
		return ResolutionAskHuman
	}
}

// --- the no-remediation guard (FR-028, SC-025) --------------------------------------------

// ErrRemediationProposed is a rendering that proposes, prepares or names an action against a
// production system. The rendering is refused, not trimmed: a report that has to be edited to be
// safe was not safe to produce.
var ErrRemediationProposed = errors.New(
	"the engine does not propose, prepare or execute remediation; a statement about what to do " +
		"next is limited to what to investigate or observe (FR-028)")

// remediationPatterns are the shapes of an instruction to change production.
//
// They are imperatives and prepared commands, not nouns. "rollback candidate: shop/payments@rev7"
// is a diagnosis and FR-057c requires it; "roll back shop/payments@rev7" is an instruction and
// FR-028 forbids it. The patterns below are anchored on the imperative form and on the shell, so
// that the distinction survives a model that is trying to be helpful.
var remediationPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?im)^\s*(?:you should|please|next step[s]?:?|action:?|remediation:?|fix:?|to fix)\b`),
	regexp.MustCompile(`(?im)\b(?:roll ?back|revert|redeploy|re-?deploy|restart|reboot|scale (?:up|down|out|in)|failover|fail over|drain|cordon|evict|delete|disable|enable|apply|patch|rotate|purge|flush|truncate) (?:the |this |that |your )?\w`),
	regexp.MustCompile(`(?im)\b(?:kubectl|helm|terraform|gcloud|aws|az|systemctl|docker|argocd|flux)\s+\w`),
}

// What the guard deliberately does NOT catch: a **past-tense** statement that something was
// changed. "I restarted a payments pod by hand at 14:20" is a human fact and one of the most
// useful pieces of evidence an investigation can hold (FR-057a); "rev7 was rolled back at 14:40"
// is a fact about the world. FR-028 governs *what to do next*, so the guard is anchored on the
// imperative. A model that fabricates "we have rolled back rev7" is caught by the verifier
// instead (FR-022a), which checks every claim against the evidence it cites — which is the right
// instrument for a false statement, where this one is the right instrument for an unsafe one.

// allowedNounPhrases are the noun phrases FR-057c *requires* the verdict to carry. They contain
// a word that is also a remediation verb — "rollback candidate" is the obvious one — so they are
// removed from a line before the imperative patterns run. Removing rather than exempting keeps
// the check honest: "rollback candidate: X; now roll it back" still fails, because only the noun
// phrase is stripped and the imperative that follows it is not.
var allowedNounPhrases = regexp.MustCompile(`(?i)\b(?:no )?rollback (?:candidate|target)\b`)

// investigationVerbs are the statements FR-028 explicitly permits: what to investigate or
// observe. A line that matches one of these is exempt from the imperative patterns, because
// "check the errors_by_version digest" is an instruction to *look*, not to change.
var investigationVerbs = regexp.MustCompile(
	`(?i)\b(?:investigate|observe|check|inspect|look at|review|examine|confirm|compare|widen|connect (?:a |the )?source|add (?:a |the )?pointer|ask)\b`)

// Guard rejects a rendering that proposes, prepares or names a remediation (FR-028).
//
// It works line by line so that a report can say what a person should look at without the whole
// document being condemned by one word. A line that both proposes an action and names something
// to investigate is still rejected: the ambiguity is the problem.
func Guard(rendering string) error {
	var offending []string
	for _, line := range strings.Split(rendering, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		scrubbed := allowedNounPhrases.ReplaceAllString(trimmed, "")
		for _, pattern := range remediationPatterns {
			if !pattern.MatchString(scrubbed) {
				continue
			}
			// An instruction to look is not an instruction to act. The exemption is narrow: the
			// line must name an investigative verb and must not carry a shell command.
			if investigationVerbs.MatchString(scrubbed) && !remediationPatterns[2].MatchString(scrubbed) {
				continue
			}
			offending = append(offending, trimmed)
			break
		}
	}
	if len(offending) == 0 {
		return nil
	}
	return fmt.Errorf("%w; offending line(s): %s", ErrRemediationProposed,
		strings.Join(offending, " | "))
}
