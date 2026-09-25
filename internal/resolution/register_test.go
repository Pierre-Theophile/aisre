// SPDX-License-Identifier: Apache-2.0

package resolution_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/resolution"
)

// What Register refuses (004 T148).
//
// The registry is open on purpose — a feature adds rules without editing rules.go — so what it refuses
// is the whole of what "a well-formed rule" means, and it was refusing three things with nothing
// asserting any of them. That is the shape this project keeps finding: a guard published, reachable and
// never exercised, so a later edit removes it and every test still passes.
//
// Each case panics rather than returning an error, deliberately: a malformed rule is a programming
// error in a `func init()`, discovered at process start, and a registry that accepted one would publish
// a rule that cannot work.

func registerPanics(t *testing.T, rule resolution.Rule) (message string) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			message, _ = r.(string)
		}
	}()
	resolution.Register(rule)
	t.Error("Register accepted the rule")
	return ""
}

// A rule with no evaluator is a rule that is published, listed by `aisre rules`, counted by the
// calibration report — and never fires. It is the exact failure C4, C5, C7 and `Skew` shipped with,
// which is why the registry refuses to be the place it can happen again.
func TestRegisterRefusesARuleWithNoEvaluator(t *testing.T) {
	got := registerPanics(t, resolution.Rule{ID: "ZZ-no-evaluator", Certain: true, Score: 1.0})
	if !strings.Contains(got, "no evaluator") {
		t.Errorf("the panic was %q; it should say the rule would be published and never fire", got)
	}
}

// And a rule with BOTH evaluators would run twice on one pair — once when a claim arrives and once when
// a correlation does — and record two decisions for one reason.
func TestRegisterRefusesARuleWithBothEvaluators(t *testing.T) {
	got := registerPanics(t, resolution.Rule{
		ID: "ZZ-both", Certain: true, Score: 1.0,
		Eval: func(context.Context, resolution.ClaimStore, resolution.Claim) ([]resolution.Match, error) {
			return nil, nil
		},
		EvalCorrelation: func(context.Context, resolution.ClaimStore, resolution.Correlation) ([]resolution.Match, error) {
			return nil, nil
		},
	})
	if !strings.Contains(got, "both evaluators") {
		t.Errorf("the panic was %q; it should say the rule would record two decisions for one reason", got)
	}
}

// A duplicate id makes a recorded decision unreadable: `rule_id = "C8"` would name two rules, and the
// audit's whole job is to say which one merged.
func TestRegisterRefusesADuplicateID(t *testing.T) {
	got := registerPanics(t, resolution.Rule{
		ID: "C8", Certain: true, Score: 1.0,
		Eval: func(context.Context, resolution.ClaimStore, resolution.Claim) ([]resolution.Match, error) {
			return nil, nil
		},
	})
	if !strings.Contains(got, "already registered") {
		t.Errorf("the panic was %q; it should say the id is taken", got)
	}
}

// And a rule with no id at all, which would be listed as "" and recorded as "".
func TestRegisterRefusesARuleWithNoID(t *testing.T) {
	got := registerPanics(t, resolution.Rule{Certain: true, Score: 1.0})
	if !strings.Contains(got, "needs an id") {
		t.Errorf("the panic was %q; it should say the rule needs an id", got)
	}
}

// The registry is not damaged by a refusal. Each case above panics before appending, so a process that
// recovered from one — a test, or a plugin loader — must not be left with a half-registered rule.
func TestARefusedRuleIsNotRegistered(t *testing.T) {
	for _, id := range []string{"ZZ-no-evaluator", "ZZ-both", ""} {
		for _, rule := range resolution.Rules() {
			if rule.ID == id {
				t.Errorf("the refused rule %q is in the published registry", id)
			}
		}
	}
	// And C8 was registered exactly once, so the duplicate case above did not add a second.
	var c8 int
	for _, rule := range resolution.Rules() {
		if rule.ID == "C8" {
			c8++
		}
	}
	if c8 != 1 {
		t.Errorf("C8 appears %d times in the registry, want once", c8)
	}
}
