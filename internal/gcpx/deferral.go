// SPDX-License-Identifier: Apache-2.0

package gcpx

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// The published behaviour around the token buckets (T161–T164; FR-149, FR-150; contracts/budget.md).
//
// Phase 2B built the mechanism: one bucket per class per scope, a share this integration may hold and
// a reserve it must never touch. This file is the **policy** on top of it, and the policy is what the
// story is about — the mechanism alone would let the integration spend its whole share in the first
// second of every minute and then yield, which honours the reserve and still makes an incident worse.
//
// Three published things live here:
//
//  1. the **decay**: as the integration's own headroom falls toward the reserve, it paces itself
//     instead of sprinting to the floor;
//  2. the **deferral order**: when the budget is short, work is dropped in a stated order rather than
//     wherever the code happened to yield;
//  3. the **typed stop reason**: "we stopped for quota" and "we looked and found nothing" are opposite
//     conclusions, and an investigation that cannot tell them apart will report the second when the
//     first is true.

// DecayBand is the fraction of this integration's own headroom below which it starts pacing.
//
// Above the band it spends freely: the share exists to be used, and an integration that paced from the
// first call would simply be a smaller share. Inside it, the interval between calls grows in inverse
// proportion to the headroom left — at half the band, twice the interval — so the approach to the
// floor is gradual and a human querying Cloud Logging in the same project sees the integration back
// away rather than crowd them until it hits the wall.
//
// It is 0.5 because that is the point at which pacing can still do something useful: below a quarter
// there is too little left to slow down into, and above three quarters the integration would be
// pacing through the range it was given to use.
const DecayBand = 0.5

// YieldKind says which of the two refusals a yield is.
type YieldKind string

const (
	// YieldReserve is the floor: one more call would spend the humans' reserve. Nothing gets past it.
	YieldReserve YieldKind = "reserve"
	// YieldPaced is the decay: there is headroom left, and this call is too soon after the last one.
	// It is a delay rather than a refusal, and RetryAt is when it becomes allowed.
	YieldPaced YieldKind = "paced"
)

// StopReason is why a piece of work stopped, in the two kinds that must never be confused (FR-149).
//
// An investigation that ran out of quota and one that found nothing implicated are opposite
// conclusions: the first says "we do not know", the second says "we looked". Reporting the second when
// the first is true is the failure this type exists to prevent, and it is a *typed* reason rather than
// a message because a caller that had to read prose would get it wrong exactly once, silently.
type StopReason string

const (
	// StopQuota is "we stopped because we ran out of our share". It is never evidence of absence.
	StopQuota StopReason = "quota_exhausted"
	// StopEvidence is "we looked and the window was covered and nothing was implicated".
	StopEvidence StopReason = "no_evidence_found"
	// StopDeferred is "this work was dropped by the deferral order before it ran", which is neither
	// of the above: it is a scope statement, and the checkpoint carries it.
	StopDeferred StopReason = "deferred_for_budget"
)

// IsAbsence reports whether a stop reason is evidence that nothing happened. Only one of them is.
//
// It exists so a caller asks the question rather than comparing against a list it may not have kept
// current: adding a fourth reason that is *not* absence should not require finding every comparison.
func (r StopReason) IsAbsence() bool { return r == StopEvidence }

// Area is a unit of work the deferral order can drop, in the order it is dropped (FR-149).
type Area string

// The published areas, and the published order. `config/gcp-budget.yaml`'s `deferral_order` is the
// same list, and budget_test.go asserts the two agree — a documented order the code does not follow is
// worse than no order, because an operator plans around it.
const (
	// AreaLoadBalancersAndDNS is P3 and explicitly the first thing cut (FR-057).
	AreaLoadBalancersAndDNS Area = "load_balancers_and_dns"
	// AreaGeneralAuditStream is the catch-all beyond the deploy path.
	AreaGeneralAuditStream Area = "general_audit_stream"
	// AreaCloudSQLSettings is the Cloud SQL polling.
	AreaCloudSQLSettings Area = "cloudsql_settings_and_flags"
	// Below this line is what the P1 stories depend on, deferred only when the alternative is
	// failing outright.
	AreaCloudRunTopology Area = "cloud_run_topology"
	AreaAlerts           Area = "alert_policies_and_transitions"
)

// DeferralOrder is the published order, first dropped first.
func DeferralOrder() []Area {
	return []Area{
		AreaLoadBalancersAndDNS,
		AreaGeneralAuditStream,
		AreaCloudSQLSettings,
		AreaCloudRunTopology,
		AreaAlerts,
	}
}

// EssentialAreas are the ones the P1 stories depend on. Deferring one of them is a different event
// from deferring a P3 surface, and a report that did not distinguish them would let "we dropped the
// load balancers" and "we stopped reading the topology" look alike.
func EssentialAreas() []Area { return []Area{AreaCloudRunTopology, AreaAlerts} }

// Essential reports whether dropping this area costs a P1 story.
func (a Area) Essential() bool { return slices.Contains(EssentialAreas(), a) }

// Deferral is one area dropped, with why.
type Deferral struct {
	Area Area
	// Why names the class that ran short, so the report says which quota caused it.
	Class EndpointClass
	// Reason is the sentence a checkpoint carries.
	Reason string
}

// String renders a deferral for a checkpoint note.
func (d Deferral) String() string {
	return fmt.Sprintf("%s (deferred for %s: %s)", d.Area, d.Class, d.Reason)
}

// Defer returns the areas to drop, in the published order, to free `needed` proportion of a class.
//
// It is a pure function of the order and the number asked for, and it deliberately does **not** take
// the budget: which areas are dropped must not depend on which one happened to ask for a call first.
// A deferral order that varied with arrival order would be unreproducible, and an operator planning
// around it would be planning around nothing.
//
// It stops before the essential areas unless `includeEssential` is set, because dropping one of those
// costs a P1 story and the caller has to say it means to.
func Defer(count int, class EndpointClass, includeEssential bool) []Deferral {
	var out []Deferral
	for _, area := range DeferralOrder() {
		if len(out) >= count {
			break
		}
		if area.Essential() && !includeEssential {
			continue
		}
		reason := "the published deferral order drops this before anything a P1 story depends on"
		if area.Essential() {
			reason = "the published order had nothing cheaper left to drop, and the alternative was " +
				"failing outright — a P1 story's coverage is reduced and the checkpoint says so"
		}
		out = append(out, Deferral{Area: area, Class: class, Reason: reason})
	}
	return out
}

// SummariseDeferrals renders deferrals for a checkpoint note, in the published order.
func SummariseDeferrals(deferrals []Deferral) string {
	if len(deferrals) == 0 {
		return ""
	}
	notes := make([]string, 0, len(deferrals))
	for _, area := range DeferralOrder() {
		for _, deferral := range deferrals {
			if deferral.Area == area {
				notes = append(notes, deferral.String())
			}
		}
	}
	return strings.Join(notes, "; ")
}

// ---------------------------------------------------------------------------
// Window caps per operation and cost class (T163, FR-150)
// ---------------------------------------------------------------------------
//
// A window cap is a *budget* rule expressed in time: a wider window costs more calls, more pages and
// more bytes, so the cap is what stops one question spending a class's whole share. It lives here as
// well as in the telemetry backend because the two have to agree — the backend caps per **term**, this
// caps per **cost class**, and a term whose cap exceeded its class's would be a cap that does nothing.
//
// The tightest cap is on the class that spends `logging.read`, which is the binding one: 60 calls per
// minute per project, not hierarchical, shared with every human querying Cloud Logging in that
// project.

// The published window caps per cost class (config/gcp-budget.yaml, contracts/budget.md §3).
const (
	// WindowCapLoggingRead is the cap on anything that spends `logging.read`. It is the tightest, and
	// `new_log_patterns` — the one term that mines log bodies — is capped tighter still by the
	// backend.
	WindowCapLoggingRead = 6 * time.Hour
	// WindowCapMonitoringQuery is the cap on a time-series read. A metric read is one call whatever
	// the window, so the cap here is about the *points* it returns and the alignment they force.
	WindowCapMonitoringQuery = 7 * 24 * time.Hour
	// WindowCapCheap is the cap on the classes whose cost does not scale with the window.
	WindowCapCheap = 90 * 24 * time.Hour
)

// WindowCapFor returns the published cap for a cost class, and whether one is published.
//
// A class with no published cap returns false rather than a zero duration: "no cap" and "a cap of
// zero" are opposite statements, and returning the second would refuse every request.
func WindowCapFor(class EndpointClass) (time.Duration, bool) {
	switch class {
	case ClassLoggingRead:
		return WindowCapLoggingRead, true
	case ClassMonitoringQuery:
		return WindowCapMonitoringQuery, true
	case ClassRunRead, ClassSQLAdminRead, ClassMonitoringPolicies, ClassComputeRead,
		ClassDNSRead, ClassContainerRead:
		return WindowCapCheap, true
	default:
		return 0, false
	}
}

// ErrWindowTooWide is the refusal for a request wider than its cap that the caller asked not to be
// narrowed. It names the cap, because a refusal a caller cannot act on is a refusal that becomes a
// retry loop.
type ErrWindowTooWide struct {
	Class     EndpointClass
	Requested time.Duration
	Cap       time.Duration
}

func (e *ErrWindowTooWide) Error() string {
	return fmt.Sprintf("gcpx: a window of %s on %s is wider than the published cap of %s. A request "+
		"wider than its cap is narrowed to the cap or refused with the cap named, never issued as "+
		"asked (FR-150)", e.Requested, e.Class, e.Cap)
}

// CapWindow narrows a duration to its class's published cap, and reports whether it did.
//
// It narrows rather than refuses, because the caller that asked is an investigation and a narrower
// answer with the narrowing **stated** is worth more than no answer. The refusal exists for the caller
// that cannot use a narrowed answer at all, and that caller asks for it by checking the second return.
func CapWindow(class EndpointClass, requested time.Duration) (time.Duration, bool) {
	cap, published := WindowCapFor(class)
	if !published || requested <= cap {
		return requested, false
	}
	return cap, true
}
