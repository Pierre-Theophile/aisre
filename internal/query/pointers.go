// SPDX-License-Identifier: Apache-2.0

package query

import (
	"context"
	"slices"
	"strings"

	"connectrpc.com/connect"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
)

// Where to look for telemetry about a node (FR-030, FR-008, US5, constitution IV).
//
// The graph never stores a metric sample, a log line or a span. What it stores is the sentence
// that finds them — a selector, in a named vocabulary, against a named backend — and this query
// is how that sentence is read back out. Sam has a suspect node and wants the right dashboards
// open; the answer is that node's pointers, grouped by kind, and nothing else.
//
// # Pointers are versioned, so this is an as-of read
//
// A pointer is a property of a node version, not of an entity (FR-008: "Pointers MUST be
// versioned in time like any other property"). Asking for the pointers of `payments` at 13:00
// therefore returns the selectors that were true at 13:00 — the *old* ones, if the service was
// renamed at 14:00 — and asking at 14:32 returns the new ones. That is US5 scenario 2, and it
// is the only reason the query takes an instant at all: a pointer lookup that always answered
// "as of now" would send an engineer investigating 13:00 to a dashboard that did not exist then.
//
// The node returned alongside them carries the display name and the aliases the graph holds for
// the entity, so a consumer can see under which name the selectors were written.
//
// # Grouping
//
// `by_kind` is keyed by the PointerKind enum *name* — METRIC, LOG, TRACE, DASHBOARD,
// SOURCE_LINK — rather than by its number, because the map is read by people and by tools that
// have no copy of the enum. A kind with no pointers is absent rather than present and empty:
// "there is no log selector for this node" is said by the key not being there, and an empty list
// would say the same thing twice. A pointer whose kind a source left unset is grouped under
// POINTER_KIND_UNSPECIFIED rather than dropped — a selector nobody classified is still a place
// to look.
//
// Within a kind the order is (backend_kind, selector, vocabulary), which is total over what a
// pointer actually says, so the answer is a pure function of the graph and can be frozen as a
// golden.

// Pointers returns a node's telemetry pointers as of an instant, grouped by kind (FR-030).
//
// A focus that resolves to an entity with no version valid at the instant is an empty answer,
// not an error: "this thing did not exist then" is a fact about the graph, while a reference the
// graph has never heard of is NOT_FOUND. The distinction is the same one Subgraph makes.
func (e *Engine) Pointers(ctx context.Context, req *graphv1.PointersRequest) (*graphv1.PointersResponse, error) {
	asOf := AsOfFromProto(req.GetAsOf())
	if err := asOf.Validate(); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	focusRef := graph.RefFromProto(req.GetFocus())
	focusID, err := e.resolveFocus(ctx, focusRef)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	r, err := e.loadRedirects(ctx)
	if err != nil {
		return nil, err
	}

	// resolveFocus has already followed `merged_into`, so either alias of a merged entity
	// reaches the survivor and returns the survivor's pointers — which is what a person asking
	// about "the Kubernetes deployment" and a person asking about "the OpenTelemetry service"
	// both mean (FR-039).
	versions, err := e.loadNodeVersions(ctx, asOf, r, []string{focusID})
	if err != nil {
		return nil, err
	}
	resp := &graphv1.PointersResponse{}
	node, ok := versions[focusID]
	if !ok {
		return resp, nil
	}
	resp.Node = node
	resp.ByKind = groupPointers(node.GetPointers())
	return resp, nil
}

// groupPointers buckets a version's pointers by kind name, dropping nothing and inventing
// nothing. An empty result is returned as nil so that a node with no pointers serializes
// without an empty map.
func groupPointers(pointers []*graphv1.Pointer) map[string]*graphv1.PointerList {
	if len(pointers) == 0 {
		return nil
	}
	out := map[string]*graphv1.PointerList{}
	for _, pointer := range pointers {
		key := pointer.GetKind().String()
		list, ok := out[key]
		if !ok {
			list = &graphv1.PointerList{}
			out[key] = list
		}
		list.Pointers = append(list.Pointers, pointer)
	}
	for _, list := range out {
		slices.SortFunc(list.Pointers, comparePointers)
	}
	return out
}

// comparePointers is the published order within a kind: the backend first, because that is what
// an operator picks a tab by, then the selector, then the vocabulary it is written in. The three
// together are total over everything a pointer says that identifies it.
func comparePointers(a, b *graphv1.Pointer) int {
	if c := strings.Compare(a.GetBackendKind(), b.GetBackendKind()); c != 0 {
		return c
	}
	if c := strings.Compare(a.GetSelector(), b.GetSelector()); c != 0 {
		return c
	}
	return strings.Compare(a.GetVocabulary(), b.GetVocabulary())
}
