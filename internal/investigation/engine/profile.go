// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"fmt"
	"sort"
	"strings"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
)

// The search-space profile (FR-012a, T123, ADR-0019 D1).
//
// The investigator's first brief says what the scope holds and what can be asked about each part
// of it: the entities near the subject with their type and facets, the edges between them with
// the source that asserted each, which signal families have a pointer for each entity, and the
// blind spots — entities nothing can be queried about but their changes, sources with a gap or a
// lag inside the window, and a neighbourhood the graph itself truncated.
//
// It is the Grounder of the literature review (F3): an agent that is told what is reachable omits
// less of it, and the examination report (FR-022b) is computed against the same profile, so what
// was reachable and never queried is measured against what the investigator was shown.
//
// It is built from the neighbourhood read the provisional step already makes, and from nothing
// else: no new call, so nothing about the run's evidence, budget or recorded world changes. That
// read is a graph query, answered from the replayed event log in recorded mode, so the profile is
// re-rendered from a replay by rebuilding it from the same answer. BuildProfile is a pure function
// of that answer and the two instants, and its rendering is deterministic.
//
// Feature 012 adds each edge's provenance class (declared, observed, suggested) and its
// reconciliation state. Until it lands the graph records the source that asserted an edge and not
// its class, and the profile says so rather than leaving the reader to assume either.

// Profile returns the search-space profile, once the provisional step has built it.
func (e *Engine) Profile() *Profile {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.profile
}

// The published caps. A profile larger than this is truncated, and the truncation is stated in the
// profile, as FR-014 requires of a digest.
const (
	MaxProfileEntities = 40
	MaxProfileEdges    = 80
)

// ProfileLagThreshold is how far behind the observed instant a source's last checkpoint may be
// before the profile names it as lagging. Five minutes is one scrape-and-ingest cycle with margin;
// a source further behind than that is missing the part of the window the alert is about.
const ProfileLagThreshold = 5 * time.Minute

// The signal families a profile names, in published order.
const (
	FamilyMetrics   = "metrics"
	FamilyLogs      = "logs"
	FamilyTraces    = "traces"
	FamilyChanges   = "changes"
	FamilyKnowledge = "knowledge"
)

// Profile is the search space of one investigation.
type Profile struct {
	// Focus is the subject's reference.
	Focus string
	// Entities are the entities in scope, the subject first, then by reference.
	Entities []ProfileEntity
	// Edges are the edges between entities in scope, by source, destination and type.
	Edges []ProfileEdge
	// BlindSpots are the sentences naming what cannot be examined, in a stable order.
	BlindSpots []string
	// Truncations state every cap that cut the profile, the graph's own and this one's.
	Truncations []string
	// TotalEntities and TotalEdges are the counts before the caps.
	TotalEntities, TotalEdges int

	// all is every entity in scope, before the cap, and bySelector the entity each pointer
	// belongs to (kind and selector): what the examination report reads (FR-022b).
	all        []ProfileEntity
	allEdges   []ProfileEdge
	bySelector map[string]string
}

// InScope is every entity in scope, uncapped.
func (p *Profile) InScope() []ProfileEntity {
	if p == nil {
		return nil
	}
	return append([]ProfileEntity(nil), p.all...)
}

// AllEdges is every edge between entities in scope, uncapped.
func (p *Profile) AllEdges() []ProfileEdge {
	if p == nil {
		return nil
	}
	return append([]ProfileEdge(nil), p.allEdges...)
}

// EntityOfPointer is the entity in scope a pointer belongs to, by its kind and selector.
func (p *Profile) EntityOfPointer(ptr *graphv1.Pointer) (string, bool) {
	if p == nil || ptr == nil {
		return "", false
	}
	id, ok := p.bySelector[selectorKey(ptr)]
	return id, ok
}

func selectorKey(ptr *graphv1.Pointer) string {
	return ptr.GetKind().String() + "|" + ptr.GetSelector()
}

// ProfileEntity is one entity in scope.
type ProfileEntity struct {
	// ID is the canonical entity id and Ref the reference a read takes.
	ID, Ref string
	// Type is the node type and Facets its other types (layers), lower-case.
	Type   string
	Facets []string
	// Families are the signal families that can be queried about the entity, each with the
	// backends that answer it.
	Families []ProfileFamily
}

// ProfileFamily is one queryable signal family.
type ProfileFamily struct {
	// Name is one of the Family constants.
	Name string
	// Backends are the backend kinds that hold a pointer for it, sorted; `graph` for changes.
	Backends []string
}

// Has reports whether the entity can be queried for a family.
func (p ProfileEntity) Has(family string) bool {
	for _, f := range p.Families {
		if f.Name == family {
			return true
		}
	}
	return false
}

// ProfileEdge is one edge between two entities in scope.
type ProfileEdge struct {
	// Src and Dst are references; Type the published edge type, lower-case.
	Src, Dst, Type string
	// WeightClass is the CALLS weight class, -1 when the graph records none.
	WeightClass int
	// Source is the source that asserted it, "" when the graph records none.
	Source string
}

// profiledTypes are the node types the profile lists as entities. A change, an alert, an
// investigation and a knowledge document are things the graph records *about* entities; nobody
// queries a signal family on them.
var profiledTypes = map[graphv1.NodeType]bool{
	graphv1.NodeType_SERVICE:        true,
	graphv1.NodeType_WORKLOAD:       true,
	graphv1.NodeType_INFRA_RESOURCE: true,
	graphv1.NodeType_CONFIG:         true,
	graphv1.NodeType_FEATURE_FLAG:   true,
	graphv1.NodeType_DB_SCHEMA:      true,
	graphv1.NodeType_THIRD_PARTY:    true,
}

// BuildProfile builds the profile from the neighbourhood read. window is the investigation's
// lookback, and observedAt the instant its knowledge is pinned to.
func BuildProfile(sub *graphv1.SubgraphResponse, start, end, observedAt time.Time) *Profile {
	p := &Profile{bySelector: map[string]string{}}
	if sub == nil {
		p.BlindSpots = append(p.BlindSpots, "the graph returned no neighbourhood: nothing near the "+
			"subject is known, so every family on every neighbour is unexamined by construction")
		return p
	}

	nodes := map[string]*graphv1.NodeVersion{}
	add := func(n *graphv1.NodeVersion) {
		if n != nil && n.GetEntityId() != "" {
			if _, seen := nodes[n.GetEntityId()]; !seen {
				nodes[n.GetEntityId()] = n
			}
		}
	}
	add(sub.GetFocus())
	for _, n := range sub.GetNodes() {
		add(n)
	}
	focusID := sub.GetFocus().GetEntityId()
	p.Focus = refOf(sub.GetFocus())

	// Knowledge: a document concerns an entity through a CONCERNS edge.
	knowledge := map[string]bool{}
	for _, e := range sub.GetEdges() {
		if e.GetType() == graphv1.EdgeType_CONCERNS {
			knowledge[e.GetDstId()] = true
		}
	}

	var entities []ProfileEntity
	for id, n := range nodes {
		if !profiledTypes[n.GetType()] {
			continue
		}
		entities = append(entities, profileEntity(id, n, knowledge[id]))
	}
	sort.Slice(entities, func(i, j int) bool {
		if (entities[i].ID == focusID) != (entities[j].ID == focusID) {
			return entities[i].ID == focusID
		}
		if entities[i].Ref != entities[j].Ref {
			return entities[i].Ref < entities[j].Ref
		}
		return entities[i].ID < entities[j].ID
	})
	p.TotalEntities = len(entities)
	p.all = append([]ProfileEntity(nil), entities...)
	for _, ent := range entities {
		for _, ptr := range nodes[ent.ID].GetPointers() {
			if ptr.GetSelector() != "" {
				p.bySelector[selectorKey(ptr)] = ent.ID
			}
		}
	}

	// Blind spots are computed over every entity in scope, before the cap: a cap on what is
	// listed is not a reason to stop saying what cannot be seen.
	for _, ent := range entities {
		if !ent.Has(FamilyMetrics) && !ent.Has(FamilyLogs) && !ent.Has(FamilyTraces) {
			p.BlindSpots = append(p.BlindSpots, fmt.Sprintf(
				"%s has no metric, log or trace pointer: only its changes can be examined", ent.Ref))
		}
	}

	if len(entities) > MaxProfileEntities {
		p.Truncations = append(p.Truncations, fmt.Sprintf(
			"entities: %d of %d listed (cap %d); the rest are in the neighbourhood read",
			MaxProfileEntities, len(entities), MaxProfileEntities))
		entities = entities[:MaxProfileEntities]
	}
	p.Entities = entities

	inScope := map[string]string{}
	for id, n := range nodes {
		if profiledTypes[n.GetType()] {
			inScope[id] = refOf(n)
		}
	}
	var edges []ProfileEdge
	for _, e := range sub.GetEdges() {
		src, okSrc := inScope[e.GetSrcId()]
		dst, okDst := inScope[e.GetDstId()]
		if !okSrc || !okDst {
			continue
		}
		weight := -1
		if e.WeightClass != nil {
			weight = int(e.GetWeightClass())
		}
		edges = append(edges, ProfileEdge{
			Src: src, Dst: dst, Type: lower(e.GetType().String()),
			WeightClass: weight, Source: e.GetProvenance().GetSourceId(),
		})
	}
	sort.Slice(edges, func(i, j int) bool {
		a, b := edges[i], edges[j]
		if a.Src != b.Src {
			return a.Src < b.Src
		}
		if a.Dst != b.Dst {
			return a.Dst < b.Dst
		}
		return a.Type < b.Type
	})
	p.TotalEdges = len(edges)
	p.allEdges = append([]ProfileEdge(nil), edges...)
	if len(edges) == 0 && len(entities) > 1 {
		p.BlindSpots = append(p.BlindSpots, "no edge links the entities in scope: the propagation "+
			"between them is unknown to the graph")
	}
	if len(edges) > MaxProfileEdges {
		p.Truncations = append(p.Truncations, fmt.Sprintf(
			"edges: %d of %d listed (cap %d)", MaxProfileEdges, len(edges), MaxProfileEdges))
		edges = edges[:MaxProfileEdges]
	}
	p.Edges = edges

	p.BlindSpots = append(p.BlindSpots, sourceBlindSpots(sub.GetExtent(), start, end, observedAt)...)
	if t := sub.GetTruncation(); t.GetTruncated() {
		p.Truncations = append(p.Truncations, fmt.Sprintf(
			"the graph truncated the neighbourhood (%s; per-hop cap %d, total cap %d)",
			orNone(t.GetReason()), t.GetPerHopCap(), t.GetTotalCap()))
	}
	return p
}

func profileEntity(id string, n *graphv1.NodeVersion, knowledge bool) ProfileEntity {
	ent := ProfileEntity{ID: id, Ref: refOf(n), Type: lower(n.GetType().String())}
	for _, f := range n.GetFacets() {
		if f != n.GetType() {
			ent.Facets = append(ent.Facets, lower(f.String()))
		}
	}
	sort.Strings(ent.Facets)

	backends := map[string]map[string]bool{}
	for _, ptr := range n.GetPointers() {
		family := ""
		switch ptr.GetKind() {
		case graphv1.PointerKind_METRIC:
			family = FamilyMetrics
		case graphv1.PointerKind_LOG:
			family = FamilyLogs
		case graphv1.PointerKind_TRACE:
			family = FamilyTraces
		default:
			continue
		}
		if backends[family] == nil {
			backends[family] = map[string]bool{}
		}
		backends[family][orUnknown(ptr.GetBackendKind())] = true
	}
	for _, family := range []string{FamilyMetrics, FamilyLogs, FamilyTraces} {
		if set := backends[family]; set != nil {
			ent.Families = append(ent.Families, ProfileFamily{Name: family, Backends: sortedKeys(set)})
		}
	}
	// Every entity's changes can be asked of the graph: `diff` and `node_history` read it.
	ent.Families = append(ent.Families, ProfileFamily{Name: FamilyChanges, Backends: []string{"graph"}})
	if knowledge {
		ent.Families = append(ent.Families, ProfileFamily{Name: FamilyKnowledge, Backends: []string{"graph"}})
	}
	return ent
}

// sourceBlindSpots names the sources whose coverage of the window has a hole: a gap that overlaps
// it, or a last checkpoint further behind the observed instant than ProfileLagThreshold.
func sourceBlindSpots(extent *graphv1.Extent, start, end, observedAt time.Time) []string {
	sources := append([]*graphv1.SourceExtent(nil), extent.GetSources()...)
	sort.Slice(sources, func(i, j int) bool { return sources[i].GetSourceId() < sources[j].GetSourceId() })
	var out []string
	for _, src := range sources {
		for _, gap := range src.GetGaps() {
			gapStart, gapEnd := gap.GetStart().AsTime(), gap.GetEnd().AsTime()
			if gap.GetEnd() == nil {
				gapEnd = observedAt
			}
			if gapStart.Before(end) && gapEnd.After(start) {
				out = append(out, fmt.Sprintf("source %s has a gap from %s to %s inside the window",
					src.GetSourceId(), gapStart.UTC().Format(time.RFC3339), gapEnd.UTC().Format(time.RFC3339)))
			}
		}
		if cp := src.GetLastCheckpoint(); cp != nil {
			if lag := observedAt.Sub(cp.AsTime()); lag > ProfileLagThreshold {
				out = append(out, fmt.Sprintf("source %s last checkpointed at %s, %s before the observed instant: nothing it saw since is in the graph",
					src.GetSourceId(), cp.AsTime().UTC().Format(time.RFC3339), lag.Round(time.Second)))
			}
		}
	}
	return out
}

// Render is the profile as the brief carries it and as the investigation records it.
func (p *Profile) Render() string {
	if p == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "focus: %s\n", orNone(p.Focus))
	fmt.Fprintf(&b, "entities in scope (%d):\n", p.TotalEntities)
	for _, ent := range p.Entities {
		fmt.Fprintf(&b, "  %s  [%s", ent.Ref, ent.Type)
		if len(ent.Facets) > 0 {
			fmt.Fprintf(&b, "; also %s", strings.Join(ent.Facets, ", "))
		}
		b.WriteString("]  queryable:")
		for i, f := range ent.Families {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, " %s (%s)", f.Name, strings.Join(f.Backends, "/"))
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "edges (%d; provenance class not yet recorded by the graph, source shown):\n", p.TotalEdges)
	for _, e := range p.Edges {
		fmt.Fprintf(&b, "  %s -%s-> %s", e.Src, e.Type, e.Dst)
		if e.WeightClass >= 0 {
			fmt.Fprintf(&b, "  weight_class=%d", e.WeightClass)
		}
		fmt.Fprintf(&b, "  source=%s\n", orNone(e.Source))
	}
	b.WriteString("blind spots:\n")
	if len(p.BlindSpots) == 0 {
		b.WriteString("  none known\n")
	}
	for _, s := range p.BlindSpots {
		fmt.Fprintf(&b, "  - %s\n", s)
	}
	for _, t := range p.Truncations {
		fmt.Fprintf(&b, "truncated: %s\n", t)
	}
	return b.String()
}

// refOf is the reference a rendering uses for a node: its OpenTelemetry service name where it has
// one, else its first alias, else its entity id. It matches the catalogue's preference.
func refOf(n *graphv1.NodeVersion) string {
	first := ""
	for _, alias := range n.GetAliases() {
		if alias.GetNamespace() == "" || alias.GetValue() == "" {
			continue
		}
		ref := alias.GetNamespace() + "=" + alias.GetValue()
		if alias.GetNamespace() == preferredRefNamespace {
			return ref
		}
		if first == "" {
			first = ref
		}
	}
	if first != "" {
		return first
	}
	return n.GetEntityId()
}

func lower(s string) string { return strings.ToLower(s) }

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
