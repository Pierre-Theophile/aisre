// SPDX-License-Identifier: Apache-2.0

package verify

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/internal/investigation/ledger"
)

// The deterministic citation checker (T074, FR-022, FR-061a, SC-002, SC-017).
//
// It runs **first** and it is **cheap**, and both matter. Every claim the engine is about to
// publish is checked mechanically against the digests it cites, before a single token is spent on
// the model verifier. Two rules, and they catch different failures:
//
//  1. **Every claim resolves to at least one evidence id.** A sentence with no citation is not a
//     claim this engine makes. It is removed.
//  2. **Every number in a claim matches a field of a digest it cites.** This is the failure an
//     investigator cannot catch about itself: a number that drifted by a digit while a narrative
//     was being written around it. The claim is demoted — kept, with the offending value named —
//     rather than deleted, because the reader is better served by "this figure could not be
//     matched to its source" than by a sentence that quietly vanished.
//
// What the checker deliberately cannot see is the third failure — a citation that resolves and is
// about the right thing but does not support what the sentence says. That is what the model pass
// exists for (verifier.go), and it is why both run rather than either.
//
// The free-text field is excluded from citation resolution entirely (FR-014b, FR-017): it is data
// a source wrote, it is flagged unverified wherever it appears, and a number found only there is
// a number the engine has not established.

// Evidence is one evidence item as the checker and the verifier see it: the digest, its coverage
// block and the term that produced it, addressed by the evidence id a claim cites.
//
// It is the verify package's own shape rather than the ledger's, because the ledger holds what it
// needs to compute a posterior and this pass needs something else — the digest *body*, which is
// where the numbers a claim quotes actually live. The engine builds one of these beside every
// ledger evidence item it records.
type Evidence struct {
	// ID is the evidence id a claim cites.
	ID string
	// Kind is the published evidence kind.
	Kind string
	// Worker and Capability say where the answer came from.
	Worker     string
	Capability string
	// Term is the algebra term as called.
	Term *investigationv1.AlgebraTerm
	// Coverage is the mandatory coverage block.
	Coverage *investigationv1.Coverage
	// Digest is the answer's body. Its free-text field is excluded from citation resolution.
	Digest *investigationv1.Digest
	// Outcome is the typed outcome, so a claim resting on a `no_data` answer can be told from one
	// resting on a `query_failed` answer.
	Outcome string
	// FreeText is the one bounded unverified field, carried so the verifier prompt can show it
	// marked rather than hide it, and never used to resolve a citation (FR-014b).
	FreeText string
}

// EvidenceFromLedger builds a checker evidence item from a ledger item and the digest the answer
// carried. The ledger does not hold digest bodies — it holds what a posterior needs — so the
// digest is passed alongside.
func EvidenceFromLedger(item ledger.EvidenceItem, digest *investigationv1.Digest) Evidence {
	return Evidence{
		ID:         item.ID,
		Kind:       item.Kind,
		Worker:     item.Worker,
		Capability: item.Capability,
		Term:       item.Term,
		Coverage:   item.Coverage,
		Digest:     digest,
		Outcome:    item.Outcome,
		FreeText:   item.FreeText,
	}
}

// Kind is where in the report a claim sits. It decides what a failure costs: a verdict line that
// cannot be evidenced is a different problem from an unevidenced sentence in the narrative.
type Kind string

// The published claim kinds, in report order.
const (
	// KindVerdict is the one line the on-call acts on.
	KindVerdict Kind = "verdict"
	// KindRanked is a ranked hypothesis's line.
	KindRanked Kind = "ranked"
	// KindTimeline is one instant in the timeline.
	KindTimeline Kind = "timeline"
	// KindNarrative is a sentence of the argument.
	KindNarrative Kind = "narrative"
)

// Claim is one checkable sentence of the report.
type Claim struct {
	// Ref is a stable identifier for this claim, e.g. "verdict" or "narrative.3". It is what a
	// finding names, so it must survive a re-render.
	Ref string
	// Kind is where in the report it sits.
	Kind Kind
	// Text is the sentence as it would be published.
	Text string
	// EvidenceIDs are the evidence items it cites.
	EvidenceIDs []string
}

// The published verdicts a finding carries. They are the same three the model verifier returns,
// plus `uncited`, which only the deterministic checker can produce because only it knows whether
// a claim carried a citation at all.
const (
	// VerdictSupported is a claim that resolved and whose numbers matched.
	VerdictSupported = "supported"
	// VerdictUnsupported is a claim the evidence does not support.
	VerdictUnsupported = "unsupported"
	// VerdictNumberMismatch is a number in the claim that no cited digest carries.
	VerdictNumberMismatch = "number_mismatch"
	// VerdictUncited is a claim with no evidence id at all.
	VerdictUncited = "uncited"
)

// The published actions a finding records.
const (
	// ActionKept is a claim that passed.
	ActionKept = "kept"
	// ActionDemoted is a claim kept with its unmatched figure named.
	ActionDemoted = "demoted"
	// ActionRemoved is a claim that is not published.
	ActionRemoved = "removed"
)

// Finding is one claim's verdict, in the shape the investigation records (FR-022a).
type Finding struct {
	// ClaimRef is the claim.
	ClaimRef string
	// Verdict is one of the four above.
	Verdict string
	// OffendingValue is the number that did not match, for a number_mismatch.
	OffendingValue string
	// CitedEvidenceID is the evidence the claim cited, where there was one.
	CitedEvidenceID string
	// ActionTaken is kept, demoted or removed.
	ActionTaken string
	// Detail is the sentence a reviewer reads.
	Detail string
}

// Proto renders the finding as the published message.
func (f Finding) Proto() *investigationv1.VerifierFinding {
	return &investigationv1.VerifierFinding{
		ClaimRef:        f.ClaimRef,
		Verdict:         f.Verdict,
		OffendingValue:  f.OffendingValue,
		CitedEvidenceId: f.CitedEvidenceID,
		ActionTaken:     f.ActionTaken,
	}
}

// Result is what the checker produced: the claims that survive, and every finding.
type Result struct {
	// Kept are the claims to publish, in the order they were given, with a demoted claim
	// carrying its caveat.
	Kept []Claim
	// Findings is every claim's verdict, including the ones that passed, so that "100% of claims
	// were checked" is checkable rather than asserted (SC-002).
	Findings []Finding
}

// Failed reports whether any claim failed, which is what the citation-validity gate reads.
func (r Result) Failed() bool {
	for _, f := range r.Findings {
		if f.Verdict != VerdictSupported {
			return true
		}
	}
	return false
}

// CitationValidity is the share of claims that resolved and whose numbers matched, at six
// decimals. It is a 100% gate (research §17).
func (r Result) CitationValidity() float64 {
	if len(r.Findings) == 0 {
		return 1
	}
	var ok int
	for _, f := range r.Findings {
		if f.Verdict == VerdictSupported {
			ok++
		}
	}
	return round6(float64(ok) / float64(len(r.Findings)))
}

// Check runs the deterministic pass over a set of claims against the evidence they cite.
func Check(claims []Claim, evidence []Evidence) Result {
	index := make(map[string]Evidence, len(evidence))
	for _, item := range evidence {
		index[item.ID] = item
	}
	values := make(map[string]*numberSet, len(evidence))

	var out Result
	for _, claim := range claims {
		finding, keep := checkOne(claim, index, values)
		out.Findings = append(out.Findings, finding)
		if keep {
			if finding.ActionTaken == ActionDemoted {
				claim.Text = demote(claim.Text, finding.OffendingValue)
			}
			out.Kept = append(out.Kept, claim)
		}
	}
	return out
}

func checkOne(claim Claim, index map[string]Evidence, values map[string]*numberSet) (Finding, bool) {
	cited := make([]string, 0, len(claim.EvidenceIDs))
	for _, id := range claim.EvidenceIDs {
		if _, ok := index[id]; ok {
			cited = append(cited, id)
		}
	}
	if len(cited) == 0 {
		detail := "the claim cites no evidence item"
		if len(claim.EvidenceIDs) > 0 {
			detail = fmt.Sprintf("the claim cites %s, which is not an evidence item of this investigation",
				strings.Join(claim.EvidenceIDs, ", "))
		}
		return Finding{
			ClaimRef: claim.Ref, Verdict: VerdictUncited, ActionTaken: ActionRemoved,
			Detail: detail + "; a sentence with no evidence behind it is not a claim this engine makes (FR-022)",
		}, false
	}

	for _, number := range numbersIn(claim.Text) {
		matched := false
		for _, id := range cited {
			set := values[id]
			if set == nil {
				set = numbersOf(index[id])
				values[id] = set
			}
			if set.has(number.value) {
				matched = true
				break
			}
		}
		if !matched {
			action := ActionDemoted
			if claim.Kind == KindVerdict {
				// The one line the on-call acts on is removed rather than demoted: a verdict
				// carrying an unmatched number is worse than no verdict, because it will be acted
				// on before the caveat is read.
				action = ActionRemoved
			}
			return Finding{
				ClaimRef:        claim.Ref,
				Verdict:         VerdictNumberMismatch,
				OffendingValue:  number.text,
				CitedEvidenceID: cited[0],
				ActionTaken:     action,
				Detail: fmt.Sprintf("%q appears in the claim but in no field of the digests it cites (%s); "+
					"the free-text field is excluded from citation resolution (FR-014b)",
					number.text, strings.Join(cited, ", ")),
			}, action != ActionRemoved
		}
	}

	return Finding{
		ClaimRef: claim.Ref, Verdict: VerdictSupported, CitedEvidenceID: cited[0],
		ActionTaken: ActionKept,
		Detail:      "every number in the claim matches a field of a digest it cites",
	}, true
}

// demote annotates a kept claim with the figure that could not be matched, so the reader sees the
// caveat beside the number rather than in a footnote.
func demote(text, value string) string {
	return text + fmt.Sprintf(" [unverified figure: %s could not be matched to a cited digest]", value)
}

// numberPattern matches an integer, a decimal, a percentage or a thousands-separated figure. It
// deliberately does not match an RFC 3339 instant: instants are checked as strings, and splitting
// one into digits would produce a dozen spurious numbers per timeline line.
var numberPattern = regexp.MustCompile(`-?\d[\d,]*(?:\.\d+)?%?`)

// instantPattern matches an RFC 3339 instant, which is excluded from the number scan and checked
// whole.
var instantPattern = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})`)

type number struct {
	text  string
	value float64
}

// numbersIn extracts the checkable numbers from a claim, in order, without duplicates.
//
// A digit that is part of an identifier is not a number a claim is quoting: "rev7" and "v1.2" and
// "http/2" are names. The check is therefore on what precedes the match — a letter, an underscore
// or a slash means this is a name with digits in it, not a figure that came from a digest. Getting
// this wrong in the other direction would be worse than useless: every claim naming a version tag
// would fail its own citation check.
func numbersIn(text string) []number {
	stripped := instantPattern.ReplaceAllString(text, " ")
	var out []number
	seen := map[string]struct{}{}
	for _, span := range numberPattern.FindAllStringIndex(stripped, -1) {
		if partOfAName(stripped, span[0]) {
			continue
		}
		match := stripped[span[0]:span[1]]
		if _, dup := seen[match]; dup {
			continue
		}
		seen[match] = struct{}{}
		value, ok := parseNumber(match)
		if !ok {
			continue
		}
		out = append(out, number{text: match, value: value})
	}
	return out
}

// partOfAName reports whether the character before a match makes it an identifier rather than a
// figure.
func partOfAName(text string, start int) bool {
	if start == 0 {
		return false
	}
	previous := rune(text[start-1])
	return unicode.IsLetter(previous) || previous == '_' || previous == '/'
}

func parseNumber(text string) (float64, bool) {
	trimmed := strings.ReplaceAll(text, ",", "")
	percent := strings.HasSuffix(trimmed, "%")
	trimmed = strings.TrimSuffix(trimmed, "%")
	value, err := strconv.ParseFloat(trimmed, 64)
	if err != nil {
		return 0, false
	}
	if percent {
		// A claim that says "24%" is citing a digest field that holds 0.24 — or one that holds
		// 24. Both are accepted; numberSet holds each digest number under both readings.
		return value / 100, true
	}
	return value, true
}

// numberSet is every number a digest carries, plus the readings a claim might state them in.
type numberSet struct {
	values []float64
	texts  map[string]struct{}
}

func (s *numberSet) has(value float64) bool {
	for _, each := range s.values {
		if math.Abs(each-value) <= tolerance(each, value) {
			return true
		}
		// A claim may state a ratio as a percentage or the other way round; both readings of the
		// digest's own number are accepted rather than requiring the renderer and the checker to
		// agree on a convention.
		if math.Abs(each*100-value) <= tolerance(each*100, value) {
			return true
		}
		if math.Abs(each/100-value) <= tolerance(each/100, value) {
			return true
		}
	}
	return false
}

// tolerance is the six-decimal agreement the whole project uses, widened proportionally for large
// numbers so that a count of 1 234 567 does not fail on a float round-trip.
func tolerance(a, b float64) float64 {
	scale := math.Max(math.Abs(a), math.Abs(b))
	if scale < 1 {
		scale = 1
	}
	return scale * 1e-6
}

// numbersOf collects every number a digest carries, excluding the free-text field and exemplar
// text — the two places a source's own words cross the boundary, and therefore the two places a
// number may never be established from (FR-014b, FR-017).
func numbersOf(item Evidence) *numberSet {
	set := &numberSet{texts: map[string]struct{}{}}
	if item.Coverage != nil {
		collectNumbers(item.Coverage, set)
	}
	if item.Term != nil {
		collectNumbers(item.Term, set)
	}
	if item.Digest != nil {
		collectNumbers(item.Digest, set)
	}
	return set
}

// collectNumbers walks any JSON-able value and records every number in it.
func collectNumbers(value any, set *numberSet) {
	if value == nil {
		return
	}
	raw, err := graph.CanonicalJSON(value)
	if err != nil {
		return
	}
	var decoded any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil {
		return
	}
	walk(decoded, "", set)
}

func walk(value any, key string, set *numberSet) {
	switch v := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if excludedKey(k) {
				continue
			}
			walk(v[k], k, set)
		}
	case []any:
		for _, each := range v {
			walk(each, key, set)
		}
	case json.Number:
		if f, err := v.Float64(); err == nil {
			set.values = append(set.values, f)
			set.texts[v.String()] = struct{}{}
		}
	case string:
		// A number carried as a string — a version tag, a duration — is still a number a claim may
		// cite, so it is parsed rather than skipped. An RFC 3339 instant is kept whole.
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			set.values = append(set.values, f)
		}
		set.texts[v] = struct{}{}
	}
}

// excludedKey is the free-text and exemplar-text boundary: whatever a source wrote is data, never
// a source of established numbers.
func excludedKey(key string) bool {
	switch key {
	case "freeText", "free_text":
		return true
	case "exemplars":
		return true
	case "text":
		return true
	default:
		return false
	}
}

// FreeTextPrefix is the published marker the excluded field carries wherever it is rendered.
const FreeTextPrefix = backend.FreeTextPrefix

func round6(v float64) float64 { return math.Round(v*1e6) / 1e6 }
