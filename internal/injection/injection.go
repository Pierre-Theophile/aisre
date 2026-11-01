// SPDX-License-Identifier: Apache-2.0

// Package injection is the published prompt-injection detector: a small set of named patterns
// that recognise retrieved text trying to instruct the investigator. The engine records what it
// finds as evidence; the chat worker counts it without quoting. Both apply the same table, which
// is why it lives in a leaf package rather than inside the engine.
package injection

import (
	"fmt"
	"regexp"
	"strings"
)

// Injection is a detected attempt to instruct the investigator through retrieved content. It is
// recorded as an evidence item of kind `injection_attempt` and changes nothing else.
type Injection struct {
	// Field is where it was seen: `free_text` or `exemplar`.
	Field string
	// Pattern is the published pattern name that matched, so a fixture can assert on it.
	Pattern string
	// Excerpt is a bounded quotation of what matched, kept so a reviewer can see it.
	Excerpt string
}

// Statement renders the finding as the evidence item's statement.
func (i Injection) Statement() string {
	return fmt.Sprintf(
		"retrieved content in the %s field reads as an instruction (%s): %q. It is data about the "+
			"systems under investigation, it was recorded, and it changed nothing about this "+
			"investigation's scope, budgets, worker set, read-only posture or output (FR-017).",
		i.Field, i.Pattern, i.Excerpt)
}

// injectionPatterns is the published detector. It is deliberately a small set of named patterns
// rather than a cleverness: the detector's job is to *record* an attempt, not to be the thing
// that stops one, because what stops one is the structure above. A pattern that misses costs a
// recorded observation; it cannot cost the investigation its integrity.
var injectionPatterns = []struct {
	name    string
	pattern *regexp.Regexp
}{
	{"override_instructions", regexp.MustCompile(`(?i)\b(ignore|disregard|forget)\b[^.]{0,40}\b(previous|prior|above|earlier|all)\b[^.]{0,20}\b(instruction|prompt|rule|direction)`)},
	{"assert_conclusion", regexp.MustCompile(`(?i)\b(the\s+)?root\s+cause\s+is\b|\bconclude\s+that\b|\bstop\s+investigating\b|\bno\s+further\s+investigation\b`)},
	{"redirect_tooling", regexp.MustCompile(`(?i)\b(call|fetch|curl|POST|GET)\b[^.]{0,30}\b(https?://|endpoint|webhook|api\s+key)`)},
	{"raise_privilege", regexp.MustCompile(`(?i)\b(raise|increase|remove|disable)\b[^.]{0,30}\b(budget|limit|guardrail|verification|read-only|readonly)`)},
	{"impersonate_operator", regexp.MustCompile(`(?i)\b(system|operator|administrator)\s*[:>]\s|\bnew\s+instructions?\b|\byou\s+are\s+now\b`)},
}

// Patterns names the published injection patterns text matches, in a fixed order. A
// worker whose source's prose never crosses the boundary — the chat worker reads messages and
// returns only counts — uses it to count what it would otherwise have had to quote.
func Patterns(text string) []string {
	var out []string
	for _, found := range Scan("", text) {
		out = append(out, found.Pattern)
	}
	return out
}

// Scan returns every published pattern text matches, recording where it was seen as field.
func Scan(field, text string) []Injection {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	var out []Injection
	for _, candidate := range injectionPatterns {
		if match := candidate.pattern.FindString(text); match != "" {
			out = append(out, Injection{
				Field:   field,
				Pattern: candidate.name,
				Excerpt: excerpt(text, match),
			})
		}
	}
	return out
}

// excerpt bounds the quotation so a recorded attempt cannot itself be a payload of unbounded size.
func excerpt(text, match string) string {
	const width = 120
	index := strings.Index(text, match)
	if index < 0 {
		index = 0
	}
	start := index - 20
	if start < 0 {
		start = 0
	}
	end := start + width
	if end > len(text) {
		end = len(text)
	}
	out := strings.TrimSpace(text[start:end])
	if end < len(text) {
		out += "…"
	}
	return out
}
