// SPDX-License-Identifier: Apache-2.0

package backend

import (
	"context"
	"errors"
	"fmt"
	"sort"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
)

// SDKVersion is the semantic version of this SDK, recorded with every backend declaration.
const SDKVersion = "0.1.0"

// The published contract, aliased rather than restated. A backend author and the engine read
// the same types out of the same generated package, so the two cannot drift; the aliases exist
// so that a backend implementation does not have to import the generated package by name.
//
// Messages are used through pointers, as generated protobuf messages always are, so a
// signature reads `*AlgebraRequest` rather than `AlgebraRequest`.
type (
	// AlgebraRequest is one term, both time dimensions, the hypothesis it serves and the
	// discriminating question it is meant to settle.
	AlgebraRequest = investigationv1.AlgebraRequest
	// AlgebraResponse is the typed outcome, the digest, the term key and the response digest.
	AlgebraResponse = investigationv1.AlgebraResponse
	// AlgebraTerm is the term itself, the only vocabulary a backend is ever asked in.
	AlgebraTerm = investigationv1.AlgebraTerm
	// Digest is everything a backend is allowed to return.
	Digest = investigationv1.Digest
	// Coverage is mandatory on every digest: what was searched, over which window, how much
	// was considered, what was sampled or truncated, the indexing lag and the remaining quota.
	// A digest without one is invalid (FR-014a).
	Coverage = investigationv1.Coverage
	// JoinKeys are what let separate answers be related to each other and to the graph.
	JoinKeys = investigationv1.JoinKeys
	// DrillDown is a handle a caller can present back, plus a human link. Never a payload.
	DrillDown = investigationv1.DrillDown
	// Handle is minted by a previous answer and never constructed by a caller — which is what
	// keeps the argument space of exemplars() and drill_down() finite, and therefore keeps a
	// recorded world finite.
	Handle = investigationv1.Handle
	// TermOutcome is the six typed outcomes, which are never collapsed into one another.
	TermOutcome = investigationv1.TermOutcome
	// FailureReason is the published set a QUERY_FAILED may cite.
	FailureReason = investigationv1.FailureReason
	// CostClass is the published, closed set budgets are expressed against.
	CostClass = investigationv1.CostClass
	// RedactionPolicy is what the backend removes or pseudonymises before anything leaves the
	// process.
	RedactionPolicy = investigationv1.RedactionPolicy
	// Window is a half-open interval [start, end).
	Window = investigationv1.Window
	// WindowPair is a baseline and a symptom window around a reference instant.
	WindowPair = investigationv1.WindowPair
)

// The six typed outcomes. Only OutcomeNoData is evidence that nothing happened; reporting any
// of the others as if it were is a defect, not a rounding (FR-027).
const (
	// OutcomeDigest is a bounded, structured answer.
	OutcomeDigest = investigationv1.TermOutcome_DIGEST
	// OutcomeNoData means the query was valid, the window was covered, and there is nothing
	// in it.
	OutcomeNoData = investigationv1.TermOutcome_NO_DATA
	// OutcomeNotYetIngested means the window falls inside the backend's indexing lag, so an
	// empty answer means nothing.
	OutcomeNotYetIngested = investigationv1.TermOutcome_NOT_YET_INGESTED
	// OutcomeQueryFailed carries a reason from the published set.
	OutcomeQueryFailed = investigationv1.TermOutcome_QUERY_FAILED
	// OutcomeNotRecorded is recorded mode only: in-algebra, not held by this world.
	OutcomeNotRecorded = investigationv1.TermOutcome_NOT_RECORDED
	// OutcomePartial means some of the answer was retrieved and what is missing is named.
	OutcomePartial = investigationv1.TermOutcome_PARTIAL
)

// The published cost classes. A declaration naming anything else, including the unspecified
// zero value, is rejected at registration (FR-047a).
const (
	// CostClassUnspecified is the zero value and is never a valid declaration.
	CostClassUnspecified = investigationv1.CostClass_COST_CLASS_UNSPECIFIED
	// CostClassCheap is an indexed lookup or a monitor-state read.
	CostClassCheap = investigationv1.CostClass_CHEAP
	// CostClassStandard is one aggregation over one selector and one window pair.
	CostClassStandard = investigationv1.CostClass_STANDARD
	// CostClassExpensive is scan-shaped.
	CostClassExpensive = investigationv1.CostClass_EXPENSIVE
)

// Family is which of the algebra's three families a term belongs to. It exists here because a
// backend serves exactly one of them.
type Family string

const (
	// FamilyGraph is answered by the graph worker against feature 001's published RPCs, and is
	// never cross-producted into a recorded world: those answers come from replaying the event
	// log into an empty database.
	FamilyGraph Family = "graph"
	// FamilyTelemetry is the eight terms a telemetry backend serves. The cross product of
	// these over the alert neighbourhood and the window grid is the recorded world.
	FamilyTelemetry Family = "telemetry"
	// FamilyKnowledge is graph-scoped retrieval over durable knowledge, served by the
	// knowledge worker.
	FamilyKnowledge Family = "knowledge"
	// FamilyUnknown is a term name the algebra does not publish.
	FamilyUnknown Family = ""
)

// termFamilies is the published algebra, by term name. A term not in this map is outside the
// algebra, and a request for it is refused QUERY_FAILED / OUTSIDE_ALGEBRA, naming what was
// asked and what is available. Adding a term is a published schema change and a corresponding
// extension of every recorded world — never something a backend improvises.
var termFamilies = map[string]Family{
	// graph: typed wrappers over feature 001's QueryService, both time dimensions passed
	// through.
	"subgraph":         FamilyGraph,
	"diff":             FamilyGraph,
	"impact":           FamilyGraph,
	"pointers":         FamilyGraph,
	"node_history":     FamilyGraph,
	"resolution_audit": FamilyGraph,
	"extent":           FamilyGraph,

	// telemetry: the eight terms a backend serves, and all a backend may serve.
	"compare":           FamilyTelemetry,
	"onset":             FamilyTelemetry,
	"new_log_patterns":  FamilyTelemetry,
	"error_spans":       FamilyTelemetry,
	"errors_by_version": FamilyTelemetry,
	"monitor_state":     FamilyTelemetry,
	"exemplars":         FamilyTelemetry,
	"drill_down":        FamilyTelemetry,

	// knowledge: scoped by the graph, never over telemetry.
	"knowledge_search": FamilyKnowledge,
}

// FamilyOf returns the family a published term belongs to, or FamilyUnknown when the algebra
// does not publish it.
func FamilyOf(term string) Family { return termFamilies[term] }

// Terms returns the published terms of one family, sorted, so a `backend list` rendering and
// any recording built from it are stable.
func Terms(family Family) []string {
	out := make([]string, 0, len(termFamilies))
	for term, f := range termFamilies {
		if f == family {
			out = append(out, term)
		}
	}
	sort.Strings(out)
	return out
}

// Description is what a backend registers itself with: the vendor it speaks for, the terms it
// serves, the cost of each, the redaction it applies, and the algebra version it implements.
//
// It is the backend analogue of feeder.Description and worker.Description. Registration
// validation — every term published and in the telemetry family, exactly one cost class per
// capability from the published set, no state-changing capability — is tasks.md T036.
type Description struct {
	// Name is this backend's identity, e.g. "datadog:prod", "recorded".
	Name string
	// Vendor is the system it speaks for, e.g. "datadog", "gcp", or "recorded" for the
	// world-backed implementation that makes a fixture replayable with no vendor account.
	Vendor string
	// Terms are the algebra terms it serves. All must be in the telemetry family: a backend
	// declaring a graph or knowledge term is rejected (contracts/telemetry-backend.md §1).
	Terms []string
	// Capabilities is the optional per-term declaration: read-only, cost class, widest window.
	// When it is present it must name exactly the terms above and price them the same way, and
	// every entry must be read-only — a state-changing capability is rejected
	// `write_capability` at registration (FR-016). Every backend in this repository declares
	// it; the field is optional only so that Terms plus CostClasses stays a valid minimal
	// declaration for a backend written before this field existed.
	Capabilities []Capability
	// CostClasses maps each declared term to exactly one published cost class. A term with no
	// entry, or an unspecified class, is rejected: an unbudgetable term is an unbounded one.
	CostClasses map[string]CostClass
	// Redaction is applied in live mode as well as when recording, so a live investigation
	// cannot surface what a recording would not be allowed to keep (FR-038).
	Redaction *RedactionPolicy
	// Version is the backend's own version, recorded in every digest it returns.
	Version string
	// AlgebraVersion is the algebra version it implements. A term of an unknown version is
	// refused rather than guessed at.
	AlgebraVersion string
}

// Validate refuses a declaration that could not be registered. It is the shape of the
// registration gate; tasks.md T036 wires it into `pkg/backend/registry.go` and adds the
// state-changing-capability refusal, which needs the capability declaration T036 introduces.
//
// Every refusal names what was wrong and what was expected, because a backend author reading
// this message is usually reading it instead of the contract.
func (d Description) Validate() error {
	if d.Name == "" {
		return errors.New("backend: Description.Name is required")
	}
	if d.Vendor == "" {
		return fmt.Errorf("backend %s names no vendor; a backend speaks for exactly one", d.Name)
	}
	if len(d.Terms) == 0 {
		return fmt.Errorf("backend %s declares no terms; it can never be called", d.Name)
	}
	seen := make(map[string]struct{}, len(d.Terms))
	for _, term := range d.Terms {
		if _, dup := seen[term]; dup {
			return fmt.Errorf("backend %s declares term %s twice; one term, one cost class", d.Name, term)
		}
		seen[term] = struct{}{}

		switch FamilyOf(term) {
		case FamilyTelemetry:
		case FamilyUnknown:
			return fmt.Errorf(
				"backend %s declares %s, which the algebra does not publish; the telemetry terms are %v",
				d.Name, term, Terms(FamilyTelemetry))
		default:
			return fmt.Errorf(
				"backend %s declares %s, which is a %s term; a telemetry backend serves the telemetry family only, and the graph and knowledge families are the workers' own",
				d.Name, term, FamilyOf(term))
		}

		switch d.CostClasses[term] {
		case CostClassCheap, CostClassStandard, CostClassExpensive:
		default:
			return fmt.Errorf(
				"backend %s declares term %s with cost class %s; it must be exactly one of cheap, standard or expensive, because budgets are spent per backend and per cost class and never as a flat call count",
				d.Name, term, d.CostClasses[term])
		}
	}
	for term := range d.CostClasses {
		if _, declared := seen[term]; !declared {
			return fmt.Errorf("backend %s prices term %s but does not serve it", d.Name, term)
		}
	}
	if d.Redaction.GetPolicyVersion() == "" {
		return fmt.Errorf(
			"backend %s declares no redaction policy version; a recording must say what was applied to it", d.Name)
	}
	if d.Version == "" {
		return fmt.Errorf("backend %s declares no version; a digest must stay attributable after the backend changes", d.Name)
	}
	if d.AlgebraVersion == "" {
		return fmt.Errorf("backend %s declares no algebra version; a term of an unknown version is refused, not guessed at", d.Name)
	}
	if d.AlgebraVersion != AlgebraVersion {
		return Reject(ReasonUnknownAlgebraVersion,
			"backend %s implements algebra version %s; this build publishes %s, and a world recorded against another version could not be replayed against this one",
			d.Name, d.AlgebraVersion, AlgebraVersion)
	}
	return d.validateCapabilities()
}

// TelemetryBackend executes one algebra term against one vendor and returns one bounded
// digest (contracts/telemetry-backend.md §8).
//
// Two modes, one output: a backend runs live against its vendor and recorded against a
// recording, and for the same request the two must produce identical digests — coverage block
// and join keys included. In recorded mode it makes no network call and never falls through to
// a live call to satisfy a miss.
type TelemetryBackend interface {
	// Describe returns the backend's contract: its terms, their cost classes and its
	// redaction. It must be a pure function.
	Describe() Description
	// Execute answers one term. Executing a term changes nothing in the vendor and creates no
	// vendor object of any kind — no saved views, no notebooks, no scheduled queries.
	//
	// A request outside the algebra is refused with QUERY_FAILED / OUTSIDE_ALGEBRA, naming
	// what was asked for and what is available, and the refusal is recorded. A term whose data
	// source the organisation does not have is still answered: NO_DATA with a coverage block
	// naming the absent source, identically in both modes and stable for the whole window, so
	// a consumer can conclude "this cannot be checked here" instead of retrying.
	Execute(ctx context.Context, req *AlgebraRequest) (*AlgebraResponse, error)
}
