// SPDX-License-Identifier: Apache-2.0

package feeder

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// The published read-only operation surface a REST connector may issue (004 FR-004, SC-007;
// constitution VII).
//
// ---------------------------------------------------------------------------------------------
// Why the surface is a value and not a document
//
// "Read-only" is easy to claim and hard to prove. A credential that holds only read scopes answers a
// different question from the one that matters, which 004's FR-004 puts precisely: write-incapability
// must be verifiable **by inspecting the set of operations the feeder can issue, not only its
// behaviour on one run**. A run that happened not to write says nothing about the next run.
//
// So the set of issuable operations is a value in the program. Every call looks its operation up
// here, and an operation the surface does not carry is refused before any quota is spent. Three
// drifts stop being possible at once: a call site cannot issue an unpublished operation, because the
// constant would not exist; it cannot be metered against the wrong area, because it does not name
// one; and the published page cannot drift from the code, because a connector's test parses the page
// and compares it to the surface in both directions.
//
// # Why the write test here is stronger than a classifier
//
// internal/gcpx classifies IAM permissions by parsing Google's verb naming — `run.services.list` is
// a read, `run.services.setIamPolicy` is not. That is a heuristic over a vendor's spelling, and it is
// the right one there because an IAM permission is what a GCP operator grants.
//
// A REST operation carries its own answer. Spelled `"METHOD path"`, the method **is** the write test:
// `GET` and `HEAD` are reads and everything else is not, by the definition of the verbs rather than
// by any vendor's convention. There is no naming to guess at and no new verb to fail closed on.
//
// # Why it panics rather than returning an error
//
// MustReadOnlySurface is meant for a package-level var, so a table containing a write fails at
// **program start** rather than in a test that someone can forget to run. A binary whose published
// surface contains a write should not exist, and this is the cheapest way to make that true. The
// house already does this: internal/resolution.Register panics on a duplicate rule id.
//
// # Named query operations (005, ADR-0010 item 3)
//
// Some platforms put a read's query in a request body. Datadog's log search and log aggregation are
// `POST`, and under the method rule alone its telemetry backend could not answer a single log term.
// The rule is extended, not loosened: a `POST` is admissible **only** as an individual operation
// declared with NamedQuery set, and its Why is then the published justification — quoted from the
// platform's documentation — that the endpoint returns data and has no field that creates, updates or
// deletes anything. The reader of the published page approves each one individually. `PUT`, `PATCH`
// and `DELETE` are never admissible, NamedQuery or not, and an undeclared `POST` still panics at init.
//
// internal/gcpx predates this and is left alone: its operations are IAM permissions rather than
// method-and-path, so it is a different shape rather than an older one, and churning a shipped
// connector to share a type it does not fit would be change without improvement.

// ReadOperation is one REST operation a connector may issue, spelled `"METHOD path"` with the
// platform's own path template — for example `"GET /repos/{owner}/{repo}/deployments"`.
//
// The template rather than an interpolated path, because the surface is about **what may be
// issued**, and a surface listing every concrete URL would list the estate rather than the
// capability.
type ReadOperation string

// ReadOperationSpec is what the published page says about one operation.
type ReadOperationSpec struct {
	// Area is the platform area it belongs to, which groups the published page and the usage
	// report. Both are generated from the same table the gate enforces.
	Area string
	// Why is what the connector needs it for. It is required: an operation nobody can justify is
	// an operation to remove, and the published page is where an operator decides whether to
	// grant it. For a NamedQuery it is also the justification that the operation cannot change
	// state, and it must say so.
	Why string
	// NamedQuery admits a `POST` whose body is a query, and nothing else. See the file comment:
	// it is declared per operation, never inferred from a method.
	NamedQuery bool
}

// ReadOnlySurface is one connector's published set of issuable operations.
type ReadOnlySurface struct {
	platform string
	ops      map[ReadOperation]ReadOperationSpec
}

// readMethods are the HTTP methods that cannot change server state.
//
// `OPTIONS` and `TRACE` are deliberately absent: both are safe by the HTTP specification, and
// neither is an operation a connector has any reason to issue, so leaving them out keeps the set to
// what is actually used rather than to what is theoretically harmless.
var readMethods = []string{"GET", "HEAD"}

// namedQueryMethod is the one method a NamedQuery may use.
const namedQueryMethod = "POST"

// namedQueryMarker is what a NamedQuery's Why must contain, so the published justification cannot be
// a reason that forgets to say why the operation is a read.
const namedQueryMarker = "named query:"

// isReadMethod reports whether a method cannot change server state. Shared with issue.go, which asks
// the same question of a cycle's log rather than of a table.
func isReadMethod(method string) bool { return slices.Contains(readMethods, method) }

// admissible reports whether an operation with this spec is a read: a read method, or a POST declared
// as a named query. It is the one rule the constructor, Issuable, StateChanges and the issuer's log
// all apply, so no two of them can disagree about what counts as a write.
func admissible(op ReadOperation, spec ReadOperationSpec) bool {
	method, _, ok := splitOperation(op)
	if !ok {
		return false
	}
	if isReadMethod(method) {
		return !spec.NamedQuery // a GET declared as a named query is a mistaken declaration
	}
	return method == namedQueryMethod && spec.NamedQuery &&
		strings.Contains(strings.ToLower(spec.Why), namedQueryMarker)
}

// MustReadOnlySurface builds a surface, panicking if any operation is malformed or is not a read.
//
// It is meant for a package-level var. See the file comment: a binary whose published surface
// contains a write should not be buildable, and a panic at init is how that is enforced rather than
// hoped for.
func MustReadOnlySurface(platform string, ops map[ReadOperation]ReadOperationSpec) *ReadOnlySurface {
	surface, err := NewReadOnlySurface(platform, ops)
	if err != nil {
		panic(err)
	}
	return surface
}

// NewReadOnlySurface builds a surface, returning the first reason it is not read-only.
func NewReadOnlySurface(platform string, ops map[ReadOperation]ReadOperationSpec) (*ReadOnlySurface, error) {
	if strings.TrimSpace(platform) == "" {
		return nil, fmt.Errorf("feeder: a read-only surface with no platform name; the refusals it " +
			"raises would not say whose operation was refused")
	}
	if len(ops) == 0 {
		return nil, fmt.Errorf("feeder: %s declares no operation, so the gate would hold over "+
			"nothing and every call would be refused", platform)
	}
	for op, spec := range ops {
		method, path, ok := splitOperation(op)
		switch {
		case !ok:
			return nil, fmt.Errorf("feeder: %s declares %q, which is not `METHOD path`; the method "+
				"is what decides whether an operation can write, so an operation that does not "+
				"state one cannot be admitted", platform, op)
		case spec.NamedQuery && method != namedQueryMethod:
			return nil, fmt.Errorf("feeder: %s declares %q as a named query, but only %s carries a "+
				"query in its body; a %s is either a read already or never admissible",
				platform, op, namedQueryMethod, method)
		case spec.NamedQuery && !strings.Contains(strings.ToLower(spec.Why), namedQueryMarker):
			return nil, fmt.Errorf("feeder: %s declares %q as a named query with a reason that does "+
				"not begin %q and justify it; a POST is admitted only on a published statement that "+
				"it cannot change state (ADR-0010 item 3)", platform, op, namedQueryMarker)
		case !spec.NamedQuery && !isReadMethod(method):
			return nil, fmt.Errorf("feeder: %s declares %q, whose method %s is not one of %v and "+
				"which is not a named query; constitution VII permits no write to any production "+
				"system, and FR-004 requires that be verifiable from the set of operations the "+
				"connector can issue", platform, op, method, readMethods)
		case !strings.HasPrefix(path, "/"):
			return nil, fmt.Errorf("feeder: %s declares %q, whose path does not begin with `/`",
				platform, op)
		case strings.TrimSpace(spec.Area) == "":
			return nil, fmt.Errorf("feeder: %s declares %q with no area; the published page and the "+
				"usage report are grouped by it", platform, op)
		case strings.TrimSpace(spec.Why) == "":
			return nil, fmt.Errorf("feeder: %s declares %q with no reason; an operator reads the "+
				"published page to decide whether to grant it, and an operation nobody can justify "+
				"is one to remove", platform, op)
		}
	}
	copied := make(map[ReadOperation]ReadOperationSpec, len(ops))
	for op, spec := range ops {
		copied[op] = spec
	}
	return &ReadOnlySurface{platform: platform, ops: copied}, nil
}

// Platform is the connector's name, as it appears in a refusal.
func (s *ReadOnlySurface) Platform() string { return s.platform }

// Issuable decides whether an operation may be issued at all, before any quota is spent.
//
// It is independent of any budget: an unmetered run — a replay from disk, a unit test — is never a
// reason to stop checking what may be called.
func (s *ReadOnlySurface) Issuable(op ReadOperation) (ReadOperationSpec, error) {
	spec, ok := s.ops[op]
	if !ok {
		return ReadOperationSpec{}, &UnpublishedOperationError{Platform: s.platform, Op: op}
	}
	// Defence in depth behind the constructor. Reaching this means the map was mutated after
	// construction, which the copy above is meant to prevent.
	if !admissible(op, spec) {
		return ReadOperationSpec{}, &WriteOperationError{Platform: s.platform, Op: op}
	}
	return spec, nil
}

// Operations returns the published set, sorted, so the page, the report and the gate are generated
// from one source.
func (s *ReadOnlySurface) Operations() []ReadOperation {
	out := make([]ReadOperation, 0, len(s.ops))
	for op := range s.ops {
		out = append(out, op)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// SpecOf returns one operation's published entry.
func (s *ReadOnlySurface) SpecOf(op ReadOperation) (ReadOperationSpec, bool) {
	spec, ok := s.ops[op]
	return spec, ok
}

// StateChanges reports how many published operations change state. For a connector that only reads
// it is zero, and a test asserting that is the cheapest form SC-007 takes.
//
// It is a count rather than a boolean so the failure message can say how many, and it is here rather
// than in each connector so no connector can define it away.
func (s *ReadOnlySurface) StateChanges() int {
	var n int
	for op, spec := range s.ops {
		if !admissible(op, spec) {
			n++
		}
	}
	return n
}

// isRead reports whether an operation, published or not, is a read under this surface's rules: a
// published operation by its declaration, an unpublished one by its method alone — an undeclared POST
// is never a named query.
func (s *ReadOnlySurface) isRead(op ReadOperation) bool {
	if spec, ok := s.ops[op]; ok {
		return admissible(op, spec)
	}
	method, _, ok := splitOperation(op)
	return ok && isReadMethod(method)
}

// splitOperation reads `"METHOD path"` into its two halves.
func splitOperation(op ReadOperation) (method, path string, ok bool) {
	method, path, found := strings.Cut(string(op), " ")
	if !found || method == "" || path == "" || strings.Contains(path, " ") {
		return "", "", false
	}
	return method, path, true
}

// UnpublishedOperationError is the refusal FR-004 turns on: a call named an operation the published
// surface does not carry, so it was not issued.
//
// It is unreachable through a connector's own constants — that is the point. It exists for an
// operation added to a call site by name, or by a caller outside the connector, and it makes the
// failure a refusal at the call rather than a review finding afterwards.
type UnpublishedOperationError struct {
	Platform string
	Op       ReadOperation
}

func (e *UnpublishedOperationError) Error() string {
	return fmt.Sprintf("feeder: %q is not on %s's published read-only operation surface, so it was "+
		"not issued (FR-004, SC-007)", e.Op, e.Platform)
}

// WriteOperationError is defence in depth: an operation on the surface whose method is not a read.
// The constructor refuses to build such a surface, so reaching this means the map was mutated after
// construction.
type WriteOperationError struct {
	Platform string
	Op       ReadOperation
}

func (e *WriteOperationError) Error() string {
	return fmt.Sprintf("feeder: %q is on %s's published surface but its method is not a read; this "+
		"connector issues no mutating operation (FR-004, SC-007)", e.Op, e.Platform)
}
