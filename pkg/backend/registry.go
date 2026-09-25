// SPDX-License-Identifier: Apache-2.0

package backend

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// The backend registration gate (tasks.md T036; FR-016, FR-047a;
// contracts/telemetry-backend.md §1 and §7).
//
// Registration is where three promises become properties of the set rather than claims made by
// each implementation:
//
//   - **A telemetry backend serves the telemetry family and nothing else.** A backend declaring
//     `subgraph` or `knowledge_search` has mistaken itself for a worker, and the boundary it
//     would blur is simultaneously the replay boundary, the sanitisation point and the injection
//     barrier. Refused here, not reviewed later.
//   - **Every capability declares exactly one published cost class.** Budgets are expressed per
//     backend *and* per cost class, never as a flat call count, so an unpriced term is an
//     unbudgetable one and an unbudgetable term is unbounded.
//   - **No capability changes state.** Executing a term creates no vendor object of any kind —
//     no saved views, no notebooks, no scheduled queries. A declaration that says otherwise is
//     refused `write_capability`.

// Capability is one declared term with the cost of answering it and the promise that answering
// it changes nothing. It is optional on a Description — a backend may declare `Terms` plus
// `CostClasses` and nothing else — but a backend that declares capabilities gets the read-only
// check, and every backend in this repository declares them.
type Capability struct {
	// Name is a published telemetry term name.
	Name string
	// ReadOnly must be true. False is rejected with ReasonWriteCapability at registration
	// rather than tolerated and checked at call time: the point of declaring it is that nobody
	// has to trust the implementation.
	ReadOnly bool
	// CostClass is exactly one of the published classes.
	CostClass CostClass
	// MaxWindow is the widest window this capability answers over. Zero means the backend
	// declares no limit of its own and the profile's window cap governs.
	MaxWindow time.Duration
}

// Validate refuses a capability that could not be registered.
func (c Capability) Validate(backend string) error {
	if c.Name == "" {
		return Reject(ReasonOutsideAlgebra,
			"backend %s declares a capability with no name; a capability is an algebra term and terms are named", backend)
	}
	if !c.ReadOnly {
		return Reject(ReasonWriteCapability,
			"backend %s capability %s is not read-only; executing a term changes nothing in the vendor and creates no vendor object of any kind",
			backend, c.Name)
	}
	switch c.CostClass {
	case CostClassCheap, CostClassStandard, CostClassExpensive:
	default:
		return Reject(ReasonWriteCapability,
			"backend %s capability %s declares cost class %s; it must be exactly one of cheap, standard or expensive, because budgets are spent per backend and per cost class",
			backend, c.Name, c.CostClass)
	}
	if c.MaxWindow < 0 {
		return Reject(ReasonWriteCapability,
			"backend %s capability %s declares a negative max window (%s)", backend, c.Name, c.MaxWindow)
	}
	return nil
}

// validateCapabilities checks the optional capability declaration against the terms and the cost
// classes, so the two spellings of the same statement cannot disagree.
func (d Description) validateCapabilities() error {
	if len(d.Capabilities) == 0 {
		return nil
	}
	declared := make(map[string]struct{}, len(d.Terms))
	for _, term := range d.Terms {
		declared[term] = struct{}{}
	}
	seen := make(map[string]struct{}, len(d.Capabilities))
	for _, c := range d.Capabilities {
		if err := c.Validate(d.Name); err != nil {
			return err
		}
		if _, dup := seen[c.Name]; dup {
			return fmt.Errorf("backend %s declares capability %s twice; one term, one cost class", d.Name, c.Name)
		}
		seen[c.Name] = struct{}{}
		if _, ok := declared[c.Name]; !ok {
			return fmt.Errorf("backend %s declares capability %s but does not list it among its terms", d.Name, c.Name)
		}
		if priced := d.CostClasses[c.Name]; priced != c.CostClass {
			return fmt.Errorf(
				"backend %s prices term %s as %s in CostClasses and %s in Capabilities; one term has one cost class",
				d.Name, c.Name, priced, c.CostClass)
		}
	}
	for term := range declared {
		if _, ok := seen[term]; !ok {
			return fmt.Errorf(
				"backend %s serves term %s but declares no capability for it; a term nobody declared read-only is a term nobody promised is read-only",
				d.Name, term)
		}
	}
	return nil
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

// Serves reports whether the backend declared the named term.
func (d Description) Serves(term string) bool {
	for _, t := range d.Terms {
		if t == term {
			return true
		}
	}
	return false
}

// Registry is the set of telemetry backends an engine may execute against. The zero Registry is
// not usable; call NewRegistry.
type Registry struct {
	mu       sync.RWMutex
	backends map[string]TelemetryBackend
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{backends: make(map[string]TelemetryBackend)}
}

// Register validates b's declaration and adds it. Two backends under one name is refused: an
// answer whose provenance cannot be established is not evidence.
func (r *Registry) Register(b TelemetryBackend) error {
	if b == nil {
		return fmt.Errorf("backend: Register(nil)")
	}
	d := b.Describe()
	if err := d.Validate(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.backends[d.Name]; dup {
		return fmt.Errorf("backend %s is already registered; one name, one vendor", d.Name)
	}
	r.backends[d.Name] = b
	return nil
}

// Lookup returns the backend registered under name.
func (r *Registry) Lookup(name string) (TelemetryBackend, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	b, ok := r.backends[name]
	return b, ok
}

// Names returns the registered backend names in sorted order, so `backend list` and any
// recording built from it are stable.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.backends))
	for name := range r.backends {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Descriptions returns every registered declaration, ordered by name. This is what
// `backend list` prints and what a run records as the backend set it had available.
func (r *Registry) Descriptions() []Description {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Description, 0, len(r.backends))
	for _, b := range r.backends {
		out = append(out, b.Describe())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Resolve finds the backend that declared a term and refuses the call when nobody did or when
// the named backend did not. It is the call-time half of the registration gate.
func (r *Registry) Resolve(backend, term string) (TelemetryBackend, CostClass, error) {
	b, ok := r.Lookup(backend)
	if !ok {
		return nil, CostClassUnspecified, fmt.Errorf(
			"no backend named %s is registered; registered backends are %v", backend, r.Names())
	}
	d := b.Describe()
	if !d.Serves(term) {
		if FamilyOf(term) != FamilyTelemetry {
			return nil, CostClassUnspecified, OutsideAlgebra(term)
		}
		return nil, CostClassUnspecified, fmt.Errorf(
			"backend %s does not serve %s; it serves %v", backend, term, d.Terms)
	}
	return b, d.CostClasses[term], nil
}
