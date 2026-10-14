// SPDX-License-Identifier: Apache-2.0

package slack

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// The naming rule: which channel names are a declaration, and what they declare (006 FR-005,
// FR-008, research R5).
//
// A rule is versioned data rather than a code path. Every field it parses is recorded with
// `slack/<id>@<version>` (intake.RuleRef), so a reader of an investigation can find the exact rule
// that turned a channel name into a severity, and a replay uses the version on the event rather than
// whichever version is compiled in today. A change to what a rule matches is a new version, never an
// edit of an old one.
//
// It parses nothing it cannot see in the name. Targets come from the operator's service map, never
// from a guess about what a slug token might mean (FR-007): a channel called `…-checkout-latency`
// becomes a target `checkout` only if an operator said that `checkout` is a service and which one.

// RuleNamespace prefixes every rule id this connector owns (ADR-0011).
const RuleNamespace = "slack/"

// Rule is one published naming rule.
type Rule struct {
	// ID and Version identify the rule; the pair is what every parsed field records.
	ID      string
	Version string
	// Pattern matches a whole channel name. It must name the groups `sev` and `date`, and may
	// name `slug`.
	Pattern *regexp.Regexp
	// SeverityFormat renders the `sev` group as the declared severity, e.g. "sev%s".
	SeverityFormat string
	// Exclusions are slug tokens that mark a channel that matches but is not an incident: a test,
	// a drill, a game day. A channel carrying one is skipped and reported, never declared.
	Exclusions []string
}

// DefaultRule is `slack/sev-channel@1`: `sev<N>-<YYYY>-<MM>-<DD>[-<slug>]`, with the three
// exclusion markers the coverage audit found in practice.
var DefaultRule = Rule{
	ID:             "sev-channel",
	Version:        "1",
	Pattern:        regexp.MustCompile(`^sev(?P<sev>[0-9])-(?P<date>[0-9]{4}-[0-9]{2}-[0-9]{2})(?:-(?P<slug>[a-z0-9][a-z0-9_-]*))?$`),
	SeverityFormat: "sev%s",
	Exclusions:     []string{"test", "drill", "gameday"},
}

// PublishedRules are the rules a deployment may name, by reference. A rule is added here, never
// edited: a replay names the version its events carry.
var PublishedRules = map[string]Rule{DefaultRule.Ref(): DefaultRule}

// RuleByRef returns a published rule.
func RuleByRef(ref string) (Rule, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return DefaultRule, nil
	}
	rule, ok := PublishedRules[ref]
	if !ok {
		refs := make([]string, 0, len(PublishedRules))
		for r := range PublishedRules {
			refs = append(refs, r)
		}
		sort.Strings(refs)
		return Rule{}, fmt.Errorf("slack: %q is not a published naming rule; the published ones are %s",
			ref, strings.Join(refs, ", "))
	}
	return rule, nil
}

// Ref is the rule's published reference, `slack/<id>@<version>`.
func (r Rule) Ref() string { return RuleNamespace + r.ID + "@" + r.Version }

// Validate refuses a rule that could not be recorded or could not parse a severity.
func (r Rule) Validate() error {
	switch {
	case strings.TrimSpace(r.ID) == "" || strings.Contains(r.ID, "@"):
		return fmt.Errorf("slack: a rule id must be non-empty and contain no '@' (got %q)", r.ID)
	case strings.TrimSpace(r.Version) == "":
		return fmt.Errorf("slack: rule %s has no version; a parsed field must name the version that parsed it", r.ID)
	case r.Pattern == nil:
		return fmt.Errorf("slack: rule %s has no pattern", r.ID)
	case r.Pattern.SubexpIndex("sev") < 0 || r.Pattern.SubexpIndex("date") < 0:
		return fmt.Errorf("slack: rule %s must name the groups sev and date", r.ID)
	case !strings.Contains(r.SeverityFormat, "%s"):
		return fmt.Errorf("slack: rule %s's severity format must contain %%s", r.ID)
	}
	return nil
}

// Skip reasons, as the report and `--check` name them (US5).
const (
	SkipNoMatch       = "no rule match"
	SkipExcluded      = "excluded marker"
	SkipBadDate       = "date part is not a date"
	SkipOutsideWindow = "created outside the backfill horizon"
	SkipPrivate       = "private channel"
	SkipShared        = "shared with another organisation"
)

// Match is what a rule read from one channel name.
type Match struct {
	// Matched is false when the name is not a declaration; Reason then says why.
	Matched bool
	Reason  string
	// Severity is as the name declares it, rendered by the rule's SeverityFormat.
	Severity string
	// Date is the date part of the name. It is a claim beside the pin, never the pin: a channel
	// created on the 15th for an incident on the 14th is pinned to the 15th (US1 scenario 5).
	Date time.Time
	// Slug is the free part after the date, and Tokens its words.
	Slug   string
	Tokens []string
}

// Match reads one channel name.
func (r Rule) Match(name string) Match {
	groups := r.Pattern.FindStringSubmatch(strings.ToLower(strings.TrimSpace(name)))
	if groups == nil {
		return Match{Reason: SkipNoMatch}
	}
	date, err := time.Parse("2006-01-02", groups[r.Pattern.SubexpIndex("date")])
	if err != nil {
		return Match{Reason: SkipBadDate}
	}
	m := Match{
		Matched:  true,
		Severity: fmt.Sprintf(r.SeverityFormat, groups[r.Pattern.SubexpIndex("sev")]),
		Date:     date,
	}
	if i := r.Pattern.SubexpIndex("slug"); i >= 0 {
		m.Slug = groups[i]
	}
	m.Tokens = tokens(m.Slug)
	for _, token := range m.Tokens {
		for _, marker := range r.Exclusions {
			if token == marker {
				return Match{Reason: SkipExcluded + " " + marker}
			}
		}
	}
	return m
}

func tokens(slug string) []string {
	return strings.FieldsFunc(slug, func(r rune) bool { return r == '-' || r == '_' })
}
