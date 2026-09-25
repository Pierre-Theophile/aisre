// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"sort"
	"sync"
)

// Registry is the set of workers an engine may call. Registration is the gate: a declaration
// that does not pass Description.Validate never reaches the investigator, so "this worker is
// read-only" and "this worker can be replayed" are properties of the set rather than promises
// made by each implementation.
//
// The zero Registry is not usable; call NewRegistry.
type Registry struct {
	mu      sync.RWMutex
	workers map[string]Worker
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{workers: make(map[string]Worker)}
}

// Register validates w's declaration and adds it. Registering a second worker under a name
// already taken is refused: two workers answering to one name is an answer whose provenance
// cannot be established.
func (r *Registry) Register(w Worker) error {
	if w == nil {
		return reject(ReasonUndeclaredCapability, "worker: Register(nil)")
	}
	d := w.Describe()
	if err := d.Validate(); err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.workers[d.Name]; dup {
		return reject(ReasonUndeclaredCapability,
			"worker %s is already registered; one name, one source of truth", d.Name)
	}
	r.workers[d.Name] = w
	return nil
}

// Lookup returns the worker registered under name.
func (r *Registry) Lookup(name string) (Worker, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	w, ok := r.workers[name]
	return w, ok
}

// Names returns the registered worker names in sorted order, so `worker list` and any
// recording built from it are stable.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.workers))
	for name := range r.workers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Descriptions returns every registered declaration, ordered by worker name. This is what
// `worker list` prints and what a run records as the worker set it had available.
func (r *Registry) Descriptions() []Description {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Description, 0, len(r.workers))
	for _, w := range r.workers {
		out = append(out, w.Describe())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Resolve finds the worker that declared a capability, and refuses the call when nobody did or
// when the named worker did not. It is the call-time half of the registration gate: an
// undeclared capability is not callable (FR-016).
//
// Phase 4 (tasks.md T041) adds the recording around the call — per-call mode, failures and
// timeouts as evidence items with their reason, retries recorded individually. This function
// is only the lookup those record against.
func (r *Registry) Resolve(worker, capability string) (Worker, Capability, error) {
	w, ok := r.Lookup(worker)
	if !ok {
		return nil, Capability{}, reject(ReasonUndeclaredCapability,
			"no worker named %s is registered; registered workers are %v", worker, r.Names())
	}
	d := w.Describe()
	c, ok := d.Capability(capability)
	if !ok {
		declared := make([]string, 0, len(d.Capabilities))
		for _, each := range d.Capabilities {
			declared = append(declared, each.Name)
		}
		return nil, Capability{}, reject(ReasonUndeclaredCapability,
			"worker %s did not declare capability %s; it declares %v", worker, capability, declared)
	}
	return w, c, nil
}
