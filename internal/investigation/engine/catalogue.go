// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
)

// The catalogue of things the model may refer to but may never construct (T062, T081, FR-029).
//
// A pointer is the graph's description of how to find an entity in a telemetry backend, and a
// handle is a token a previous answer minted. Both are opaque and both are *earned*: the engine
// obtains them from answers and files them here under a short id, and the tool schemas take that
// id rather than the value. A model that writes its own selector therefore cannot: there is no
// field for one.
//
// That is FR-029's "never construct a selector of its own for an entity the graph can describe",
// enforced by the shape of the tools rather than by a check that could be forgotten.

// Catalogue holds the pointers and handles this investigation has earned.
//
// It is safe for concurrent use because the first wave calls workers in parallel, and each of
// those answers may mint handles.
type Catalogue struct {
	mu sync.Mutex

	pointers map[string]*graphv1.Pointer
	pointerO []string
	pointerK map[string]string // canonical selector → id, so the same pointer keeps one id

	handles map[string]*investigationv1.Handle
	handleO []string
	handleK map[string]string

	// owners records which entity a pointer describes, so a refusal can say what is available
	// for the entity the model was actually asking about.
	owners map[string]string

	// refByID and idByRef are the two directions of the one translation an investigation cannot
	// avoid: the graph names a change's targets and an edge's ends by **canonical entity id**,
	// while every read the engine issues — `pointers`, `subgraph`, `impact` — takes a **Ref**.
	// Both spellings are the graph's own; neither is invented here. They are learned from the
	// node versions the graph returns, which carry the entity id and the aliases together, so a
	// translation only exists for a node some answer actually described (T081, FR-016).
	refByID map[string]string
	idByRef map[string]string
	// edgeTypes is the published type of each edge in the neighbourhood, keyed "src\x00dst". A
	// span query over an edge has to name the edge's own type, and guessing `calls` for an edge
	// the graph calls `depends_on` asks a question no recording holds.
	edgeTypes map[string]graphv1.EdgeType
}

// NewCatalogue returns an empty catalogue.
func NewCatalogue() *Catalogue {
	return &Catalogue{
		pointers:  map[string]*graphv1.Pointer{},
		pointerK:  map[string]string{},
		handles:   map[string]*investigationv1.Handle{},
		handleK:   map[string]string{},
		owners:    map[string]string{},
		refByID:   map[string]string{},
		idByRef:   map[string]string{},
		edgeTypes: map[string]graphv1.EdgeType{},
	}
}

// NoteNode files the two-way translation between one node's canonical entity id and the
// references a person or an alert would write for it.
//
// The preferred reference is the OpenTelemetry service name where the node has one, because that
// is the vocabulary the alerts and the fixtures speak; any other alias is better than an opaque
// id. Every alias is indexed in the other direction, so a reference the graph ever published for
// this entity resolves to it.
//
// Filing is idempotent and first-writer-wins on the preferred direction, so the id a node was
// first described under keeps its rendering for the life of the investigation.
func (c *Catalogue) NoteNode(node *graphv1.NodeVersion) {
	if node == nil || node.GetEntityId() == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	id := node.GetEntityId()
	for _, alias := range node.GetAliases() {
		if alias.GetNamespace() == "" || alias.GetValue() == "" {
			continue
		}
		ref := alias.GetNamespace() + "=" + alias.GetValue()
		if _, seen := c.idByRef[ref]; !seen {
			c.idByRef[ref] = id
		}
		_, named := c.refByID[id]
		if !named || alias.GetNamespace() == preferredRefNamespace {
			c.refByID[id] = ref
		}
	}
}

// preferredRefNamespace is the alias namespace a rendering prefers: the OpenTelemetry service
// name, which is what the monitors, the manifests and the on-call all write.
const preferredRefNamespace = "otel.service.name"

// NoteRef records a reference the engine itself resolved — the subject's, from the `pointers`
// answer that named its node. It is the one case where the reference is known before any alias
// list is, because the request carried it.
func (c *Catalogue) NoteRef(ref, entityID string) {
	if ref == "" || entityID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, seen := c.idByRef[ref]; !seen {
		c.idByRef[ref] = entityID
	}
	if _, seen := c.refByID[entityID]; !seen {
		c.refByID[entityID] = ref
	}
}

// NoteEdge files one edge of the neighbourhood with the type the graph published for it.
func (c *Catalogue) NoteEdge(edge *graphv1.EdgeVersion) {
	if edge == nil || edge.GetSrcId() == "" || edge.GetDstId() == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, seen := c.edgeTypes[edgeKey(edge.GetSrcId(), edge.GetDstId())]; !seen {
		c.edgeTypes[edgeKey(edge.GetSrcId(), edge.GetDstId())] = edge.GetType()
	}
}

// RefsFor is every reference the graph published for one entity, sorted, or nil when no answer
// has described it.
//
// All of them rather than the preferred one, because the caller that needs this is matching
// against a spelling somebody else chose — a ground truth, a runbook, an alert — and the one
// alias a rendering prefers is not necessarily the one they wrote.
func (c *Catalogue) RefsFor(entityID string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for ref, id := range c.idByRef {
		if id == entityID {
			out = append(out, ref)
		}
	}
	sort.Strings(out)
	return out
}

// RefFor renders a canonical entity id as the reference a read takes, or "" when no answer has
// described that entity yet.
func (c *Catalogue) RefFor(entityID string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.refByID[entityID]
}

// EntityIDFor resolves a reference to the canonical entity id the graph gave it, or "" when no
// answer has described it. A term whose fields are entity ids — `error_spans` is the only one —
// is built through this rather than by passing the reference through under a field named `id`.
func (c *Catalogue) EntityIDFor(ref string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.idByRef[ref]
}

// EdgeTypeBetween returns the type the graph published for an edge, and whether the
// neighbourhood holds one at all.
func (c *Catalogue) EdgeTypeBetween(srcEntityID, dstEntityID string) (graphv1.EdgeType, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.edgeTypes[edgeKey(srcEntityID, dstEntityID)]
	return t, ok
}

func edgeKey(src, dst string) string { return src + "\x00" + dst }

// AddPointer files a pointer under a stable short id and returns it. Filing the same pointer
// twice returns the same id: a model that saw `ptr-3` on turn two must still find it on turn
// nine.
func (c *Catalogue) AddPointer(entityRef string, pointer *graphv1.Pointer) string {
	if pointer == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := pointerKey(pointer)
	if id, ok := c.pointerK[key]; ok {
		return id
	}
	id := fmt.Sprintf("ptr-%d", len(c.pointerO)+1)
	c.pointers[id] = pointer
	c.pointerK[key] = id
	c.pointerO = append(c.pointerO, id)
	c.owners[id] = entityRef
	return id
}

// Pointer resolves a pointer id.
func (c *Catalogue) Pointer(id string) (*graphv1.Pointer, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.pointers[id]
	if !ok {
		return nil, fmt.Errorf("no pointer %q has been obtained in this investigation; "+
			"ask `pointers` for the entity first. Pointers so far: %s", id, strings.Join(c.pointerO, ", "))
	}
	return p, nil
}

// AddHandle files a handle minted by an answer.
func (c *Catalogue) AddHandle(handle *investigationv1.Handle) string {
	if handle == nil || handle.GetValue() == "" {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := handle.GetValue()
	if id, ok := c.handleK[key]; ok {
		return id
	}
	id := fmt.Sprintf("h-%d", len(c.handleO)+1)
	c.handles[id] = handle
	c.handleK[key] = id
	c.handleO = append(c.handleO, id)
	return id
}

// Handle resolves a handle id.
func (c *Catalogue) Handle(id string) (*investigationv1.Handle, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	h, ok := c.handles[id]
	if !ok {
		return nil, fmt.Errorf("no handle %q has been minted in this investigation; a handle comes "+
			"from a previous answer and is never composed. Handles so far: %s",
			id, strings.Join(c.handleO, ", "))
	}
	return h, nil
}

// PointerIDs returns every pointer id, in the order they were earned.
func (c *Catalogue) PointerIDs() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.pointerO...)
}

// PointersFor returns the pointer ids obtained for one entity, sorted.
func (c *Catalogue) PointersFor(entityRef string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for id, owner := range c.owners {
		if owner == entityRef {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// Render lists what the model may refer to, for the tool result of a `pointers` answer.
func (c *Catalogue) Render() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var b strings.Builder
	for _, id := range c.pointerO {
		p := c.pointers[id]
		fmt.Fprintf(&b, "%s  %s  kind=%s  backend=%s  selector=%s",
			id, c.owners[id], p.GetKind(), p.GetBackendKind(), p.GetSelector())
		if keys := p.GetJoinKeys(); len(keys) > 0 {
			roles := make([]string, 0, len(keys))
			for role, attribute := range keys {
				roles = append(roles, role+"→"+attribute)
			}
			sort.Strings(roles)
			fmt.Fprintf(&b, "  join_keys=%s", strings.Join(roles, ","))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func pointerKey(p *graphv1.Pointer) string {
	return fmt.Sprintf("%s\x00%s\x00%s", p.GetKind(), p.GetBackendKind(), p.GetSelector())
}
