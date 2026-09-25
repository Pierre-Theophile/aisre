// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
)

// SDKVersion is the semantic version of this SDK, recorded with every worker declaration.
// MINOR adds helpers; MAJOR changes the interfaces in this file.
const SDKVersion = "0.1.0"

// Mode is how a worker is running. Both modes are mandatory and must produce identical
// outputs for identical inputs — coverage block and join keys included, not only the summary
// statistics. The mode is recorded per call, so a replay can prove the two agree (SC-003).
type Mode string

const (
	// ModeLive calls the worker's source of truth.
	ModeLive Mode = "live"
	// ModeRecorded answers from a recording and makes no network call. It never falls through
	// to a live call to satisfy a miss: an in-algebra term the recording does not hold is
	// answered NOT_RECORDED with the term echoed back (FR-039, FR-040).
	ModeRecorded Mode = "recorded"
)

// String renders the mode as it is written in a worker_calls row and in a trajectory record.
func (m Mode) String() string { return string(m) }

// Valid reports whether m is one of the two published modes.
func (m Mode) Valid() bool { return m == ModeLive || m == ModeRecorded }

// CostClass is the published, closed set of cost classes a capability may declare. It is an
// alias of the generated enum rather than a second spelling of it, because budgets are
// expressed per backend and per cost class and both sides must mean the same thing (FR-047a).
type CostClass = investigationv1.CostClass

// The published cost classes. Anything else, including the unspecified zero value, is rejected
// at registration: a capability whose cost nobody declared cannot be budgeted for.
const (
	// CostClassUnspecified is the zero value and is never a valid declaration.
	CostClassUnspecified = investigationv1.CostClass_COST_CLASS_UNSPECIFIED
	// CostClassCheap is an indexed lookup or a monitor-state read.
	CostClassCheap = investigationv1.CostClass_CHEAP
	// CostClassStandard is one aggregation over one selector and one window pair.
	CostClassStandard = investigationv1.CostClass_STANDARD
	// CostClassExpensive is scan-shaped: a wide new_log_patterns, a long-window onset,
	// exemplars.
	CostClassExpensive = investigationv1.CostClass_EXPENSIVE
)

// Rejection reason codes, from 002 data-model.md §Validation rules. They are published strings
// rather than Go error values because they are recorded, rendered and asserted on in fixtures.
const (
	// ReasonWriteCapability is a worker declaring a capability that changes state in its
	// source (FR-016).
	ReasonWriteCapability = "write_capability"
	// ReasonUndeclaredCapability is a call to a capability the worker did not declare.
	ReasonUndeclaredCapability = "undeclared_capability"
	// ReasonUndeclaredRedaction is a worker emitting a field its redaction policy does not
	// cover (FR-038).
	ReasonUndeclaredRedaction = "undeclared_redaction"
	// ReasonOutsideAlgebra is a call whose term is not a published algebra term (FR-042b).
	ReasonOutsideAlgebra = "outside_algebra"
	// ReasonMissingCoverage is a response with no coverage block (FR-014a).
	ReasonMissingCoverage = "missing_coverage"
)

// Rejection is a refusal carrying the published reason code it was refused under, so that a
// rejection can be recorded and asserted on rather than only logged.
type Rejection struct {
	// Reason is one of the published reason codes above.
	Reason string
	// Detail says what was wrong, naming the worker and the capability.
	Detail string
}

// Error implements error.
func (r *Rejection) Error() string { return r.Reason + ": " + r.Detail }

// reject builds a Rejection with a formatted detail.
func reject(reason, format string, args ...any) error {
	return &Rejection{Reason: reason, Detail: fmt.Sprintf(format, args...)}
}

// Reject is the exported form, for a worker outside this package refusing under the same
// published reason codes. A worker written outside this repository refuses the same way the ones
// inside it do, or the codes a fixture asserts on mean nothing.
func Reject(reason, format string, args ...any) error { return reject(reason, format, args...) }

// ReasonOf returns the published reason code a rejection carries, or "" for any other error.
func ReasonOf(err error) string {
	var r *Rejection
	if errors.As(err, &r) {
		return r.Reason
	}
	return ""
}

// Capability is one named algebra term a worker answers, with the cost of answering it.
type Capability struct {
	// Name is an algebra term name, e.g. "compare", "onset", "subgraph". A name that is not a
	// published term is rejected at registration: the algebra is the whole surface.
	Name string
	// ReadOnly must be true. A false value is rejected with ReasonWriteCapability rather than
	// tolerated and checked later — the point of declaring it is that nobody has to trust the
	// implementation (FR-016).
	ReadOnly bool
	// CostClass is exactly one of the published classes. The unspecified zero value is
	// rejected, because an unbudgetable capability is an unbounded one (FR-047a).
	CostClass CostClass
	// MaxWindow is the widest window this capability will answer over. Zero means the worker
	// declares no limit of its own and the profile's window cap governs.
	MaxWindow time.Duration
}

// Validate refuses a capability that could not be registered.
func (c Capability) Validate(worker string) error {
	if c.Name == "" {
		return reject(ReasonUndeclaredCapability,
			"worker %s declares a capability with no name; a capability is an algebra term and terms are named", worker)
	}
	if !c.ReadOnly {
		return reject(ReasonWriteCapability,
			"worker %s capability %s is not read-only; a worker may not change state in its source of truth", worker, c.Name)
	}
	switch c.CostClass {
	case CostClassCheap, CostClassStandard, CostClassExpensive:
	default:
		return reject(ReasonUndeclaredCapability,
			"worker %s capability %s declares cost class %s; it must be exactly one of cheap, standard or expensive",
			worker, c.Name, c.CostClass)
	}
	if c.MaxWindow < 0 {
		return reject(ReasonUndeclaredCapability,
			"worker %s capability %s declares a negative max window (%s)", worker, c.Name, c.MaxWindow)
	}
	return nil
}

// RedactionPolicy is what a worker removes or pseudonymises before anything leaves its
// process. It is applied in live mode as well as when recording, so a live investigation
// cannot surface what a recording would not be allowed to keep (FR-038, ADR-0003 D9).
type RedactionPolicy struct {
	// DroppedFields are removed outright. People identifiers belong here: they are dropped,
	// never hashed, because a stable hash of a person is still a person.
	DroppedFields []string
	// PseudonymisedFields are replaced with a keyed HMAC, consistently, so that joins survive.
	// A digest whose keys do not join is evidence about nothing.
	PseudonymisedFields []string
	// LogBodiesAsTemplates says log content is reduced to masked templates before it is
	// returned at all.
	LogBodiesAsTemplates bool
	// PolicyVersion is recorded with every recording, so what was applied to a fixture stays
	// knowable after the policy changes.
	PolicyVersion string
}

// Validate refuses a policy a recording could not be checked against.
func (p RedactionPolicy) Validate(worker string) error {
	if p.PolicyVersion == "" {
		return reject(ReasonUndeclaredRedaction,
			"worker %s declares no redaction policy version; a recording must say what was applied to it", worker)
	}
	return nil
}

// Description is everything the engine and the test harness need to know about a worker before
// it answers anything. It is the worker analogue of feeder.Description.
type Description struct {
	// Name is the worker's identity: "graph", "metrics", "logs", "traces", "knowledge".
	Name string
	// SourceOfTruth is the one system this worker speaks for. Exactly one — a worker bound to
	// two sources is two workers, and an answer whose provenance is ambiguous is not evidence.
	SourceOfTruth string
	// Capabilities are the named algebra terms it answers.
	Capabilities []Capability
	// ContainsModel is declared, not discovered (FR-009a). A worker whose answer a model
	// touched must say so, and its digest must state what the model added over the algorithm.
	ContainsModel bool
	// ModelID is required when ContainsModel, and recorded with every call.
	ModelID string
	// Redaction is what this worker removes before anything leaves the process.
	Redaction RedactionPolicy
	// Modes must contain both ModeLive and ModeRecorded. A worker that cannot be replayed
	// cannot be merged (constitution VIII).
	Modes []Mode
	// Version is the worker's own version, recorded with every answer so a digest stays
	// attributable after the worker changes.
	Version string
}

// Validate is the registration gate: it refuses a declaration the investigator must not be
// handed. Every refusal carries a published reason code.
func (d Description) Validate() error {
	if d.Name == "" {
		return reject(ReasonUndeclaredCapability, "worker: Description.Name is required")
	}
	if d.SourceOfTruth == "" {
		return reject(ReasonUndeclaredCapability,
			"worker %s declares no source of truth; a worker speaks for exactly one system", d.Name)
	}
	if len(d.Capabilities) == 0 {
		return reject(ReasonUndeclaredCapability,
			"worker %s declares no capabilities; an undeclared capability is not callable, so this worker can never be called", d.Name)
	}
	seen := make(map[string]struct{}, len(d.Capabilities))
	for _, c := range d.Capabilities {
		if err := c.Validate(d.Name); err != nil {
			return err
		}
		if _, dup := seen[c.Name]; dup {
			return reject(ReasonUndeclaredCapability,
				"worker %s declares capability %s twice; one term, one cost class", d.Name, c.Name)
		}
		seen[c.Name] = struct{}{}
	}
	if d.ContainsModel && d.ModelID == "" {
		return reject(ReasonUndeclaredCapability,
			"worker %s declares ContainsModel with no ModelID; which model ran is recorded, not inferred (FR-009a)", d.Name)
	}
	if !d.ContainsModel && d.ModelID != "" {
		return reject(ReasonUndeclaredCapability,
			"worker %s names model %s but declares ContainsModel = false", d.Name, d.ModelID)
	}
	if err := d.Redaction.Validate(d.Name); err != nil {
		return err
	}
	var live, recorded bool
	for _, m := range d.Modes {
		if !m.Valid() {
			return reject(ReasonUndeclaredCapability,
				"worker %s declares mode %q, which is neither %q nor %q", d.Name, m, ModeLive, ModeRecorded)
		}
		live = live || m == ModeLive
		recorded = recorded || m == ModeRecorded
	}
	if !live || !recorded {
		return reject(ReasonUndeclaredCapability,
			"worker %s declares modes %v; both %q and %q are required, because a worker without recorded responses as its test is not merged",
			d.Name, d.Modes, ModeLive, ModeRecorded)
	}
	if d.Version == "" {
		return reject(ReasonUndeclaredCapability,
			"worker %s declares no version; a digest must stay attributable after the worker changes", d.Name)
	}
	return nil
}

// Declares reports whether the worker declared a capability by that name.
func (d Description) Declares(capability string) bool {
	for _, c := range d.Capabilities {
		if c.Name == capability {
			return true
		}
	}
	return false
}

// Capability returns the named declaration and whether it was declared.
func (d Description) Capability(name string) (Capability, bool) {
	for _, c := range d.Capabilities {
		if c.Name == name {
			return c, true
		}
	}
	return Capability{}, false
}

// Request is one call to a worker. It carries the hypothesis it serves and the discriminating
// question it is meant to answer — both inside Algebra, and both recorded with the call
// (FR-018a). A call that serves no hypothesis records "exploratory:<reason>"; a call that says
// nothing about why it was made is not made.
type Request struct {
	// Capability is the algebra term name being called. It must be one the worker declared.
	Capability string
	// Mode is live or recorded.
	Mode Mode
	// Algebra is the published request: the term, both time dimensions, the hypothesis served
	// and the discriminating question.
	Algebra *investigationv1.AlgebraRequest
}

// Response is one worker answer, echoing enough of the call to stand on its own as an evidence
// item. A failure, a timeout and an empty result are all answers: they become evidence items
// with their reason, never silences (contracts/worker-sdk.md §Failures are evidence).
type Response struct {
	// Worker and Capability are the call this answers.
	Worker     string
	Capability string
	// Mode is the mode it ran in, recorded per call.
	Mode Mode
	// Algebra is the published response: the typed outcome, the digest with its mandatory
	// coverage block, the term key and the response digest.
	Algebra *investigationv1.AlgebraResponse
	// Graph is the graph family's own answer — one of feature 001's published response
	// messages — and is set only by the graph worker.
	//
	// It travels beside the digest rather than inside it because the digest's eight bodies are
	// the shapes a *telemetry* answer takes, and re-expressing a subgraph as a metric digest
	// would lose exactly the structure an investigator needs. Nothing about the digest boundary
	// is weakened: a graph response is already identifiers, versions and structure, and 001
	// admits no sample into the graph in the first place (constitution IV).
	Graph proto.Message
}

// Worker turns one source of truth into digests the investigator can reason over.
type Worker interface {
	// Describe returns the worker's contract. It must be a pure function: the harness calls it
	// before any call and holds the worker to it.
	Describe() Description
	// Call answers one algebra term. An undeclared capability is refused
	// (ReasonUndeclaredCapability); a term outside the algebra is refused
	// (ReasonOutsideAlgebra), naming what was asked and what is available.
	Call(ctx context.Context, req Request) (Response, error)
}
