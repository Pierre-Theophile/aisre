// SPDX-License-Identifier: Apache-2.0

// Package mask holds the published log-masking rules: the single definition of what a masked
// template is. It is a leaf package with no dependency on the investigation engine, so that the
// sanitiser, the log miner and the backend redactor can all apply the same rules.
package mask

import (
	"regexp"
	"sort"
	"strings"
)

// The masking rules. They are the single definition of what a masked template is: the Drain
// miner masks with them before it clusters, the exemplar path masks with them before it returns
// a line, and the redactor's CheckDeclared uses their inverse to catch anything that got through.
//
// Order matters: the most specific pattern first, so an email is an <email> rather than a
// <word>@<word> and a UUID is a <uuid> rather than five <hex> runs.
var maskRules = []struct {
	name        string
	pattern     *regexp.Regexp
	placeholder string
}{
	{"email", regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`), "<email>"},
	{"url", regexp.MustCompile(`\b[a-zA-Z][a-zA-Z0-9+.\-]*://[^\s"']+`), "<url>"},
	{"uuid", regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`), "<uuid>"},
	{"ip", regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`), "<ip>"},
	{"timestamp", regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+\-]\d{2}:?\d{2})?`), "<ts>"},
	{"hex", regexp.MustCompile(`\b(?:0x)?[0-9a-fA-F]{16,}\b`), "<hex>"},
	{"duration", regexp.MustCompile(`\b\d+(?:\.\d+)?(?:ns|us|ms|s|m|h)\b`), "<dur>"},
	{"path", regexp.MustCompile(`(?:^|\s)/(?:[A-Za-z0-9._\-]+/)+[A-Za-z0-9._\-]*`), " <path>"},
	{"number", regexp.MustCompile(`\b\d+(?:\.\d+)?\b`), "<num>"},
}

// Line reduces one line to its masked form: every variable the published rules recognise is
// replaced by its placeholder. It is idempotent — masking a masked line changes nothing — which
// is what lets the redactor run over a digest a miner already produced.
func Line(line string) string {
	if line == "" {
		return ""
	}
	out := line
	for _, rule := range maskRules {
		out = rule.pattern.ReplaceAllString(out, rule.placeholder)
	}
	return strings.TrimSpace(strings.Join(strings.Fields(out), " "))
}

// UnmaskedLiterals names the first class of unmasked variable it finds in text, or "" when the
// text is fully masked. It is the inverse of Line and the check the backend redactor's CheckDeclared runs.
func UnmaskedLiterals(text string) string {
	if text == "" {
		return ""
	}
	names := make([]string, 0, 2)
	for _, rule := range maskRules {
		if rule.name == "number" || rule.name == "path" {
			// A masked template legitimately keeps small structural numbers in a status code
			// or a field name, and <path> replacement leaves a leading space the round trip
			// normalises; both are covered by re-masking rather than by refusal.
			continue
		}
		if rule.pattern.MatchString(text) {
			names = append(names, rule.name)
		}
	}
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	return names[0]
}
