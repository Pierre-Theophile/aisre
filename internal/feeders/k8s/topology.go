// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"sort"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
)

// The listed topology (FR-043, FR-048, research §12).
//
// What an informer's initial list delivers is not a stream of things happening, it is one
// answer to one question: what exists right now. The feeder therefore accumulates the list and
// emits it here, as a single ordered batch, and that order is a pure function of the object set
// — grouped by kind, sorted inside each group. Nothing about the emitted stream depends on
// which watch replied first, which is what lets testkit.Shuffle permute the whole list and get
// the same bytes back, and it is the order fixtures/baseline-topology-01 records.
//
// The grouping is also what makes two facts derivable that a payload-at-a-time feeder cannot
// state honestly: how many machines a node pool has, and which workloads a Service selects.
// Both are properties of the set, and a feeder that computed them from "everything seen so
// far" would answer differently depending on when it was asked.
//
// After the list, the feeder is incremental and one payload produces one object's events
// (feeder.go). That asymmetry is the shape of a watch, not an inconsistency: a list is a set
// and a watch is a sequence.

// topologyEvents renders everything the feeder has listed, in canonical order. The caller holds
// the lock.
//
// Every fact takes the valid time of the payload that is its evidence, through Feeder.validFor:
// initial state carries FR-011's unknown start, and anything the informers delivered after the
// list completed carries the instant it happened. The list is emitted here rather than
// payload-by-payload for a different reason — node counts and `exposed-via` are properties of
// the set — and the two are independent: nothing here reads a single "when the list happened"
// instant any more, because there is no such instant that is not a function of arrival order.
func (f *Feeder) topologyEvents() ([]*graphv1.EventEnvelope, error) {
	workloads := f.sortedWorkloads("")
	var events []*graphv1.EventEnvelope

	// 1. The cluster, its node pools, and the pools' place in it.
	cluster, err := f.mapper.ClusterEvents(f.knownNodes(), f.listValid())
	if err != nil {
		return nil, err
	}
	events = append(events, cluster...)

	// 2. The workloads, then where each of them runs. Nodes before the edges that reference
	// them is not required by the graph — it creates a placeholder for an unresolved ref — but
	// it is what a person reading the event log expects, and costs nothing.
	for _, w := range workloads {
		env := f.environmentOf(w.labels, w.annotations, w.namespace)
		node, err := f.mapper.WorkloadNode(w, env, f.validFor(w.observedAt))
		if err != nil {
			return nil, err
		}
		events = append(events, node)
	}
	for _, w := range workloads {
		if edge := f.mapper.RunsOnEdge(w, f.validFor(w.observedAt)); edge != nil {
			events = append(events, edge)
		}
	}

	// 3. How the workloads are reached: Services, Ingresses, and the exposures through them.
	for _, s := range f.sortedServices("") {
		env := f.environmentOf(s.labels, s.annotations, s.namespace)
		svc, err := f.mapper.ServiceEvents(s, env, f.validFor(s.observedAt))
		if err != nil {
			return nil, err
		}
		events = append(events, svc...)
	}
	for _, i := range f.sortedIngresses("") {
		env := f.environmentOf(i.labels, i.annotations, i.namespace)
		ing, err := f.mapper.IngressEvents(i, env, f.validFor(i.observedAt))
		if err != nil {
			return nil, err
		}
		events = append(events, ing...)
	}
	for _, w := range workloads {
		exposing := map[string]bool{}
		for _, s := range f.sortedServices(w.namespace) {
			if !matchesSelector(s.selector, w.podLabels) {
				continue
			}
			exposing[s.name] = true
			events = append(events, f.mapper.ExposedViaService(w, s, f.validFor(s.observedAt)))
		}
		for _, i := range f.sortedIngresses(w.namespace) {
			for _, backend := range i.backends {
				if exposing[backend] {
					events = append(events, f.mapper.ExposedViaIngress(w, i, f.validFor(i.observedAt)))
					break
				}
			}
		}
	}

	// 4. The configuration the workloads consume. ConfigMaps before Secrets, because that is
	// the order fixtures/baseline-topology-01 records and there is no better reason to prefer.
	for _, c := range f.sortedConfigs(kindConfigMap) {
		env := f.environmentOf(c.labels, c.annotations, c.namespace)
		cfg, err := f.mapper.ConfigEvents(c, env, f.validFor(c.observedAt))
		if err != nil {
			return nil, err
		}
		events = append(events, cfg...)
	}
	for _, c := range f.sortedConfigs(kindSecret) {
		env := f.environmentOf(c.labels, c.annotations, c.namespace)
		cfg, err := f.mapper.ConfigEvents(c, env, f.validFor(c.observedAt))
		if err != nil {
			return nil, err
		}
		events = append(events, cfg...)
	}
	for _, w := range workloads {
		edges, err := f.mapper.DependsOnEdges(w, f.validFor(w.observedAt))
		if err != nil {
			return nil, err
		}
		events = append(events, edges...)
	}

	// 5. Who owns them. An owning team is asserted once, by the first workload that names it
	// in this ordering, and the rest are DUPLICATE_NOOPs the graph never sees.
	seenOwner := map[string]bool{}
	for _, w := range workloads {
		owner, label := f.mapper.OwnerOf(w)
		if owner == "" || seenOwner[owner] {
			continue
		}
		seenOwner[owner] = true
		node, err := f.mapper.OwnerNode(owner, label, f.environmentOf(w.labels, w.annotations, w.namespace), f.listValid())
		if err != nil {
			return nil, err
		}
		events = append(events, node)
	}
	for _, w := range workloads {
		if owner, _ := f.mapper.OwnerOf(w); owner != "" {
			events = append(events, f.mapper.OwnedByEdge(w, owner, f.validFor(w.observedAt)))
		}
	}

	// 6. The names each workload is known by, which is the whole of this feeder's contribution
	// to entity resolution: claims, never merges (constitution VI).
	for _, w := range workloads {
		claims, err := f.mapper.ClaimEvents(w, f.environmentOf(w.labels, w.annotations, w.namespace))
		if err != nil {
			return nil, err
		}
		events = append(events, claims...)
	}
	return events, nil
}

// environmentOf resolves an object's environment against the namespaces the feeder has listed.
func (f *Feeder) environmentOf(labels, annotations map[string]string, namespace string) string {
	return f.opts.environmentOf(labels, annotations, f.state.namespace[namespace])
}

// sortedConfigs is every listed ConfigMap or Secret, in a stable order.
func (f *Feeder) sortedConfigs(kind resourceKind) []configView {
	out := make([]configView, 0, len(f.state.configs))
	for _, c := range f.state.configs {
		if c.kind.payload == kind.payload {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key() < out[j].key() })
	return out
}
