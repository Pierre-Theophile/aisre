// SPDX-License-Identifier: Apache-2.0

package budget

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Operator caps and the non-configurable set (T073, FR-043, FR-047b).
//
// Two lists, and the interesting one is the second.
//
// **Configurable** — because an operator's appetite for spend, latency and vendor quota is
// theirs: wall time, cost, per-backend and per-cost-class call budgets, the widest window a query
// may span, concurrent investigations, daily global spend.
//
// **Not configurable** — because each of them is a property the engine's claims rest on, and a
// deployment that turned one off would still be producing output that looked like this feature's
// output while meaning something else:
//
//   - the evidence requirement: a claim resting on nothing is not a claim this engine makes;
//   - the read-only posture: an engine that could act is a different product;
//   - the recording of calls: an investigation nobody can replay is an assertion, not a finding;
//   - the verification pass: the check exists because the investigator cannot catch this class of
//     error about itself;
//   - the synthesis reserve: it is what makes an exhausted run still produce an answer.
//
// An attempt to relax one is **refused with a reason**, not ignored. Silently ignoring it would
// let an operator believe they had turned something off.

// OperatorCaps is the operator's own ceilings. A zero field means "no cap of my own"; the
// profile's limit then governs. A cap only ever tightens — an operator cap larger than the
// profile's does not raise it.
type OperatorCaps struct {
	// WallTime is the operator's wall-clock ceiling. It is the only wall-clock limit the
	// `review` profile has.
	WallTime time.Duration
	// CostUnits is the operator's token ceiling.
	CostUnits float64
	// CallsPerBackend and CallsPerCostClass are the operator's call ceilings.
	CallsPerBackend   map[string]int64
	CallsPerCostClass map[string]int64
	// MaxWindow is the operator's ceiling on how wide a single query's window may be.
	MaxWindow time.Duration
	// MaxConcurrentInvestigations and DailyGlobalSpend are deployment-wide.
	MaxConcurrentInvestigations uint32
	DailyGlobalSpend            float64
}

// Validate refuses caps that could not be enforced.
func (c OperatorCaps) Validate() error {
	if c.WallTime < 0 || c.CostUnits < 0 || c.MaxWindow < 0 || c.DailyGlobalSpend < 0 {
		return errors.New("budget: an operator cap is a ceiling and cannot be negative; leave it zero to set none")
	}
	for class := range c.CallsPerCostClass {
		if !validClass(class) {
			return fmt.Errorf("budget: operator cap names cost class %q, which is not one of %v", class, Classes)
		}
	}
	return nil
}

// The published configurable settings, by name. They are strings because they are what a
// configuration file and a CLI flag spell.
const (
	SettingWallTime                    = "wall_time"
	SettingCostUnits                   = "cost_units"
	SettingCallsPerBackend             = "calls_per_backend"
	SettingCallsPerCostClass           = "calls_per_cost_class"
	SettingMaxWindow                   = "max_window"
	SettingMaxConcurrentInvestigations = "max_concurrent_investigations"
	SettingDailyGlobalSpend            = "daily_global_spend"
	SettingQuotaShare                  = "quota_share"
)

// The published non-configurable settings.
const (
	SettingEvidenceRequired = "evidence_required"
	SettingReadOnly         = "read_only"
	SettingRecordCalls      = "record_calls"
	SettingVerificationPass = "verification_pass"
	SettingSynthesisReserve = "synthesis_reserve_fraction"
)

// configurable is the published set an operator may set.
var configurable = map[string]struct{}{
	SettingWallTime: {}, SettingCostUnits: {}, SettingCallsPerBackend: {},
	SettingCallsPerCostClass: {}, SettingMaxWindow: {},
	SettingMaxConcurrentInvestigations: {}, SettingDailyGlobalSpend: {},
	SettingQuotaShare: {},
}

// nonConfigurable is the published set an operator may not, each with the reason the refusal
// carries. The reason is the point: a refusal that does not say why reads as a bug.
var nonConfigurable = map[string]string{
	SettingEvidenceRequired: "every claim resting on at least one evidence item is what the engine's " +
		"output means; a deployment without it would publish sentences nobody can check (FR-022)",
	SettingReadOnly: "the engine proposes and never acts; an engine that could act is a different " +
		"product with a different risk profile (constitution VII)",
	SettingRecordCalls: "an investigation whose calls were not recorded cannot be replayed, and an " +
		"investigation nobody can replay is an assertion rather than a finding (FR-042a)",
	SettingVerificationPass: "the verifier catches the class of error an investigator cannot catch " +
		"about itself — a number that drifted from the digest it came from (FR-022a)",
	SettingSynthesisReserve: "the reserve is exactly 15% and is what makes an exhausted investigation " +
		"still produce a written answer (FR-045, SC-005)",
}

// ErrNotConfigurable is returned when a configuration tries to relax something that is not
// configurable.
var ErrNotConfigurable = errors.New("budget: not configurable")

// RefusalError is a refused configuration change, carrying the setting and the reason.
type RefusalError struct {
	// Setting is what was being set.
	Setting string
	// Reason is why it may not be.
	Reason string
}

// Error renders the refusal.
func (e *RefusalError) Error() string {
	return fmt.Sprintf("budget: %s is not configurable: %s", e.Setting, e.Reason)
}

// Unwrap lets a caller match ErrNotConfigurable.
func (e *RefusalError) Unwrap() error { return ErrNotConfigurable }

// Configure checks one setting name before a configuration applies it, refusing the
// non-configurable set with its reason (FR-047b).
func Configure(setting string) error {
	name := strings.ToLower(strings.TrimSpace(setting))
	if _, ok := configurable[name]; ok {
		return nil
	}
	if reason, ok := nonConfigurable[name]; ok {
		return &RefusalError{Setting: name, Reason: reason}
	}
	return fmt.Errorf("budget: %q is not a published setting; the configurable ones are %s",
		setting, strings.Join(ConfigurableSettings(), ", "))
}

// ConfigurableSettings returns the published configurable set, sorted.
func ConfigurableSettings() []string {
	out := make([]string, 0, len(configurable))
	for name := range configurable {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// NonConfigurableSettings returns the published non-configurable set, sorted.
func NonConfigurableSettings() []string {
	out := make([]string, 0, len(nonConfigurable))
	for name := range nonConfigurable {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// ReasonNotConfigurable returns the published reason a setting may not be relaxed.
func ReasonNotConfigurable(setting string) (string, bool) {
	reason, ok := nonConfigurable[strings.ToLower(strings.TrimSpace(setting))]
	return reason, ok
}
