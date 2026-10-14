// SPDX-License-Identifier: Apache-2.0

package slack_test

import (
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/feeders/slack"
)

func TestDefaultRule(t *testing.T) {
	if err := slack.DefaultRule.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := slack.DefaultRule.Ref(); got != "slack/sev-channel@1" {
		t.Errorf("Ref = %s", got)
	}
	cases := []struct {
		name     string
		matched  bool
		severity string
		tokens   []string
		reason   string
	}{
		{name: "sev2-2026-10-14-checkout-latency", matched: true, severity: "sev2", tokens: []string{"checkout", "latency"}},
		{name: "SEV1-2026-10-14-payments", matched: true, severity: "sev1", tokens: []string{"payments"}},
		{name: "sev3-2026-10-14", matched: true, severity: "sev3"},
		{name: "sev2-2026-10-14-test-paging", reason: "excluded marker test"},
		{name: "sev2-2026-10-14-gameday", reason: "excluded marker gameday"},
		{name: "sev2-2026-10-14-latest", matched: true, severity: "sev2", tokens: []string{"latest"}},
		{name: "sev2-2026-13-40-checkout", reason: slack.SkipBadDate},
		{name: "sev-2-2026-10-14-checkout", reason: slack.SkipNoMatch},
		{name: "inc-2026-10-14-checkout", reason: slack.SkipNoMatch},
		{name: "general", reason: slack.SkipNoMatch},
		{name: "sev2-26-10-14-checkout", reason: slack.SkipNoMatch},
	}
	for _, c := range cases {
		m := slack.DefaultRule.Match(c.name)
		if m.Matched != c.matched {
			t.Errorf("%s: matched = %v (%s), want %v", c.name, m.Matched, m.Reason, c.matched)
			continue
		}
		if !c.matched {
			if m.Reason != c.reason {
				t.Errorf("%s: reason = %q, want %q", c.name, m.Reason, c.reason)
			}
			continue
		}
		if m.Severity != c.severity || !slices.Equal(m.Tokens, c.tokens) {
			t.Errorf("%s: %+v", c.name, m)
		}
		if m.Date.Format("2006-01-02") != "2026-10-14" {
			t.Errorf("%s: date %s", c.name, m.Date)
		}
	}
}

func TestRuleValidateRefusesAnUnrecordableRule(t *testing.T) {
	bad := []slack.Rule{
		{ID: "", Version: "1", Pattern: slack.DefaultRule.Pattern, SeverityFormat: "sev%s"},
		{ID: "a@b", Version: "1", Pattern: slack.DefaultRule.Pattern, SeverityFormat: "sev%s"},
		{ID: "x", Version: "", Pattern: slack.DefaultRule.Pattern, SeverityFormat: "sev%s"},
		{ID: "x", Version: "1", Pattern: regexp.MustCompile(`^sev(?P<sev>\d)$`), SeverityFormat: "sev%s"},
		{ID: "x", Version: "1", Pattern: slack.DefaultRule.Pattern, SeverityFormat: "sev"},
	}
	for i, r := range bad {
		if err := r.Validate(); err == nil {
			t.Errorf("rule %d validated", i)
		}
	}
	if err := (slack.Rule{ID: "x", Version: "1"}).Validate(); err == nil || !strings.Contains(err.Error(), "pattern") {
		t.Errorf("no pattern: %v", err)
	}
}
