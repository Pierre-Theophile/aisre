// SPDX-License-Identifier: Apache-2.0

package budget

import (
	"bytes"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
)

// Budget profiles (T067, FR-043, FR-047, research §11, plan §The budget manager).
//
// Two profiles, published with their targets and their hard stops, because an on-call being
// paged and a reviewer writing a post-incident document want opposite things from the same
// engine. `page` is time-bound: a first tested hypothesis inside two minutes, a substantive
// answer inside five, a hard stop at ten. `review` is quality-bound: no engine deadline at all,
// only the operator's own cap.
//
// A "substantive answer" is defined rather than felt (FR-047): at least one hypothesis at
// `supported` or `refuted`, together with a verdict line that passed the citation checker. That
// is what SubstantiveTarget is measured against, and it is why the target is reachable — the
// deterministic first wave produces the first tested hypothesis with no model in the loop
// (plan F2).
//
// Every dimension is a dimension, never a flat call count (FR-043): wall time, cost units,
// per-backend and per-cost-class calls, the widest window a query may span, the share of a
// vendor's remaining quota, concurrent investigations and daily global spend.

// AnyBackend is the key a per-backend budget uses for backends it does not name. A budget that
// only named the backends someone thought of is a budget with a hole in it.
const AnyBackend = "*"

// The published cost-class names. They are the keys of a profile's per-cost-class budget and
// match the algebra's declared classes.
const (
	ClassCheap     = "cheap"
	ClassStandard  = "standard"
	ClassExpensive = "expensive"
)

// Classes is the published cost-class set, in increasing cost order.
var Classes = []string{ClassCheap, ClassStandard, ClassExpensive}

// DefaultQuotaShare is the share of a backend's *remaining* quota one investigation may spend.
//
// A quarter, as an initial value to be calibrated with the corpus (research §20). The reviewer's
// point behind it is the reason this dimension exists at all: an agent that spends the on-call's
// vendor quota during the incident is worse than an agent that stops.
const DefaultQuotaShare = 0.25

// Profile is one published budget profile.
//
// A limit of zero means "this profile sets none; the operator cap governs" — which is what
// `review` says about wall time, and what makes it quality-bound rather than unbounded.
type Profile struct {
	// Name is `page` or `review`, or an operator's own name for a loaded profile.
	Name string
	// WallTime is the hard stop. Zero means the profile sets none.
	WallTime time.Duration
	// FirstTestedTarget and SubstantiveTarget are targets, not stops: missing one is recorded
	// and reported, never a reason to abandon the run.
	FirstTestedTarget time.Duration
	SubstantiveTarget time.Duration
	// CostUnits is the model-token budget, in tokens across every class and model. Tokens are
	// the primary, provider-independent unit (FR-048); the monetary figure is derived.
	CostUnits float64
	// CallsPerBackend caps worker calls per backend. The AnyBackend key is the default.
	CallsPerBackend map[string]int64
	// CallsPerCostClass caps worker calls per declared cost class.
	CallsPerCostClass map[string]int64
	// MaxWindow is the widest window a single query may span.
	MaxWindow time.Duration
	// QuotaShare is the share of a backend's remaining quota this investigation may spend.
	QuotaShare float64
	// SynthesisReserveFraction is held back for the closing synthesis. It is exactly 0.15 and
	// is not configurable; see reserve.go.
	SynthesisReserveFraction float64
	// MaxConcurrentInvestigations and DailyGlobalSpend are deployment-wide caps carried on the
	// profile so that a run records what it was held to.
	MaxConcurrentInvestigations uint32
	DailyGlobalSpend            float64
}

// PageProfile is the published `page` profile: first tested hypothesis inside 90–120 s, a
// substantive answer inside 5 minutes, hard stop at 10 minutes (FR-047).
func PageProfile() Profile {
	return Profile{
		Name:              "page",
		WallTime:          10 * time.Minute,
		FirstTestedTarget: 120 * time.Second,
		SubstantiveTarget: 5 * time.Minute,
		CostUnits:         400_000,
		CallsPerBackend:   map[string]int64{AnyBackend: 40},
		CallsPerCostClass: map[string]int64{
			ClassCheap: 60, ClassStandard: 30, ClassExpensive: 8,
		},
		MaxWindow:                   6 * time.Hour,
		QuotaShare:                  DefaultQuotaShare,
		SynthesisReserveFraction:    ReserveFraction,
		MaxConcurrentInvestigations: 4,
		DailyGlobalSpend:            250,
	}
}

// ReviewProfile is the published `review` profile: quality-bound, with no engine deadline. The
// operator's cap is the only wall-clock limit, which is what "operator cap only" means (FR-047).
func ReviewProfile() Profile {
	return Profile{
		Name:              "review",
		WallTime:          0,
		FirstTestedTarget: 0,
		SubstantiveTarget: 0,
		CostUnits:         2_000_000,
		CallsPerBackend:   map[string]int64{AnyBackend: 200},
		CallsPerCostClass: map[string]int64{
			ClassCheap: 300, ClassStandard: 150, ClassExpensive: 40,
		},
		MaxWindow:                   7 * 24 * time.Hour,
		QuotaShare:                  DefaultQuotaShare,
		SynthesisReserveFraction:    ReserveFraction,
		MaxConcurrentInvestigations: 2,
		DailyGlobalSpend:            250,
	}
}

// Published returns the two published profiles by name.
func Published() map[string]Profile {
	return map[string]Profile{"page": PageProfile(), "review": ReviewProfile()}
}

// ForPriority selects a profile from an alert's priority. Anything urgent enough to wake someone
// gets `page`; everything else is reviewed at leisure. The mapping is published so that an
// operator can predict which budget a given monitor will run under.
func ForPriority(priority string) Profile {
	switch strings.ToLower(strings.TrimSpace(priority)) {
	case "p1", "p2", "critical", "high", "page", "":
		return PageProfile()
	default:
		return ReviewProfile()
	}
}

// Validate refuses a profile that could not be enforced or could not be reported.
func (p Profile) Validate() error {
	if strings.TrimSpace(p.Name) == "" {
		return fmt.Errorf("budget: a profile with no name cannot be recorded on an investigation (FR-047)")
	}
	if p.CostUnits <= 0 {
		return fmt.Errorf("budget profile %s: cost_units is %v; an unbounded token budget is not a budget", p.Name, p.CostUnits)
	}
	if p.QuotaShare <= 0 || p.QuotaShare > 1 {
		return fmt.Errorf("budget profile %s: quota_share is %v; it is a share of what the vendor says "+
			"is left, so it lies in (0, 1]", p.Name, p.QuotaShare)
	}
	if p.SynthesisReserveFraction != ReserveFraction {
		return fmt.Errorf("budget profile %s: synthesis_reserve_fraction is %v; the reserve is exactly "+
			"%v and is not configurable (FR-045)", p.Name, p.SynthesisReserveFraction, ReserveFraction)
	}
	if p.MaxWindow <= 0 {
		return fmt.Errorf("budget profile %s: max_window is %v; an uncapped window is how one query "+
			"spends a day's quota", p.Name, p.MaxWindow)
	}
	if len(p.CallsPerCostClass) == 0 {
		return fmt.Errorf("budget profile %s: no per-cost-class budget; a flat call count prices a "+
			"cheap monitor read the same as an expensive span search (FR-043)", p.Name)
	}
	for _, class := range Classes {
		if _, ok := p.CallsPerCostClass[class]; !ok {
			return fmt.Errorf("budget profile %s: cost class %s has no budget; an unbudgeted class is "+
				"an unbounded one", p.Name, class)
		}
	}
	for class := range p.CallsPerCostClass {
		if !validClass(class) {
			return fmt.Errorf("budget profile %s: cost class %q is not one of %v", p.Name, class, Classes)
		}
	}
	if _, ok := p.CallsPerBackend[AnyBackend]; !ok {
		return fmt.Errorf("budget profile %s: no %q entry in calls_per_backend; a budget that only names "+
			"the backends someone thought of has a hole in it", p.Name, AnyBackend)
	}
	return nil
}

func validClass(class string) bool {
	for _, each := range Classes {
		if each == class {
			return true
		}
	}
	return false
}

// CallsForBackend returns the per-backend cap in force for one backend.
func (p Profile) CallsForBackend(backend string) int64 {
	if limit, ok := p.CallsPerBackend[backend]; ok {
		return limit
	}
	return p.CallsPerBackend[AnyBackend]
}

// Proto renders the profile as the published message, which every investigation records.
func (p Profile) Proto() *investigationv1.BudgetProfile {
	return &investigationv1.BudgetProfile{
		Name:                        p.Name,
		WallTimeSeconds:             int64(p.WallTime / time.Second),
		FirstTestedTargetSeconds:    int64(p.FirstTestedTarget / time.Second),
		SubstantiveTargetSeconds:    int64(p.SubstantiveTarget / time.Second),
		CostUnits:                   p.CostUnits,
		CallsPerBackend:             copyInt64Map(p.CallsPerBackend),
		CallsPerCostClass:           copyInt64Map(p.CallsPerCostClass),
		MaxWindowSeconds:            int64(p.MaxWindow / time.Second),
		QuotaShare:                  p.QuotaShare,
		SynthesisReserveFraction:    p.SynthesisReserveFraction,
		MaxConcurrentInvestigations: p.MaxConcurrentInvestigations,
		DailyGlobalSpend:            p.DailyGlobalSpend,
	}
}

func copyInt64Map(in map[string]int64) map[string]int64 {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]int64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// Describe renders the profile's targets and hard stop in the sentence the render and the turn's
// stop-condition line use.
func (p Profile) Describe() string {
	var parts []string
	if p.FirstTestedTarget > 0 {
		parts = append(parts, fmt.Sprintf("first tested hypothesis within %s", p.FirstTestedTarget))
	}
	if p.SubstantiveTarget > 0 {
		parts = append(parts, fmt.Sprintf("substantive answer within %s", p.SubstantiveTarget))
	}
	if p.WallTime > 0 {
		parts = append(parts, fmt.Sprintf("hard stop at %s", p.WallTime))
	} else {
		parts = append(parts, "no engine deadline; the operator cap governs")
	}
	return p.Name + ": " + strings.Join(parts, ", ")
}

// ProfileFile is `--budget-profiles <file>`: an operator's own profiles, replacing the published
// ones by name.
//
// It decodes into its own struct rather than into Profile, for one reason that is worth the
// duplication: durations are written the way an operator writes them — `10m`, `6h`, `120s` — and
// a struct that decoded straight into time.Duration would silently read `10` as ten nanoseconds.
// A budget misread by nine orders of magnitude is the kind of defect that only surfaces during an
// incident.
type ProfileFile struct {
	// Version is recorded with the investigation, so a historical budget stays interpretable.
	Version string `yaml:"version"`
	// Profiles are the profiles this file publishes.
	Profiles []profileYAML `yaml:"profiles"`
}

type profileYAML struct {
	Name                        string           `yaml:"name"`
	WallTime                    string           `yaml:"wall_time"`
	FirstTestedTarget           string           `yaml:"first_tested_target"`
	SubstantiveTarget           string           `yaml:"substantive_target"`
	CostUnits                   float64          `yaml:"cost_units"`
	CallsPerBackend             map[string]int64 `yaml:"calls_per_backend"`
	CallsPerCostClass           map[string]int64 `yaml:"calls_per_cost_class"`
	MaxWindow                   string           `yaml:"max_window"`
	QuotaShare                  float64          `yaml:"quota_share"`
	SynthesisReserveFraction    float64          `yaml:"synthesis_reserve_fraction"`
	MaxConcurrentInvestigations uint32           `yaml:"max_concurrent_investigations"`
	DailyGlobalSpend            float64          `yaml:"daily_global_spend"`
}

// profile converts a decoded document into a Profile, refusing a duration nobody can read back.
func (p profileYAML) profile() (Profile, error) {
	wall, err := parseDuration(p.Name, "wall_time", p.WallTime)
	if err != nil {
		return Profile{}, err
	}
	first, err := parseDuration(p.Name, "first_tested_target", p.FirstTestedTarget)
	if err != nil {
		return Profile{}, err
	}
	substantive, err := parseDuration(p.Name, "substantive_target", p.SubstantiveTarget)
	if err != nil {
		return Profile{}, err
	}
	window, err := parseDuration(p.Name, "max_window", p.MaxWindow)
	if err != nil {
		return Profile{}, err
	}
	return Profile{
		Name:                        p.Name,
		WallTime:                    wall,
		FirstTestedTarget:           first,
		SubstantiveTarget:           substantive,
		CostUnits:                   p.CostUnits,
		CallsPerBackend:             p.CallsPerBackend,
		CallsPerCostClass:           p.CallsPerCostClass,
		MaxWindow:                   window,
		QuotaShare:                  p.QuotaShare,
		SynthesisReserveFraction:    p.SynthesisReserveFraction,
		MaxConcurrentInvestigations: p.MaxConcurrentInvestigations,
		DailyGlobalSpend:            p.DailyGlobalSpend,
	}, nil
}

// parseDuration reads "10m", "6h", "120s" or "0".
func parseDuration(profile, field, value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "0" {
		return 0, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("budget profile %s: %s = %q is not a duration such as \"10m\", \"6h\" or \"120s\"",
			profile, field, value)
	}
	if d < 0 {
		return 0, fmt.Errorf("budget profile %s: %s is negative", profile, field)
	}
	return d, nil
}

// LoadProfiles reads `--budget-profiles <file>` and returns the profiles by name, starting from
// the published defaults so that a file naming only `page` still leaves `review` usable.
func LoadProfiles(path string) (map[string]Profile, error) {
	out := Published()
	if strings.TrimSpace(path) == "" {
		return out, nil
	}
	raw, err := os.ReadFile(path) //nolint:gosec // an operator-supplied configuration path
	if err != nil {
		return nil, fmt.Errorf("budget: read %s: %w", path, err)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true) // an unknown key is a typo, and an ignored typo is a default nobody chose
	var file ProfileFile
	if err := decoder.Decode(&file); err != nil {
		return nil, fmt.Errorf("budget: parse %s: %w", path, err)
	}
	for _, document := range file.Profiles {
		profile, err := document.profile()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if err := profile.Validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		out[profile.Name] = profile
	}
	return out, nil
}

// Names returns the profile names in a set, sorted.
func Names(profiles map[string]Profile) []string {
	out := make([]string, 0, len(profiles))
	for name := range profiles {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
