// SPDX-License-Identifier: Apache-2.0

package datadog

import (
	"fmt"
	"sort"
	"strings"
)

// Capabilities are the independently enabled parts of the connector (FR-008a). The set in force is
// recorded in every checkpoint and fixture manifest, because a disabled capability must read as "we
// were not looking", never as "there was nothing there" (FR-008b).
type Capabilities map[Capability]bool

// Capability is one named part of the connector.
type Capability string

// The published capabilities.
const (
	// CapLogs is the log-search telemetry backend, the watched log sources and their rollouts.
	CapLogs Capability = "logs"
	// CapMonitors is monitor and alert intake.
	CapMonitors Capability = "monitors"
	// CapTags is ownership and identity claims from allowlisted tags.
	CapTags Capability = "tags"
	// CapAPMTopology is the APM service map, metrics and spans. Off by default, and not built until an
	// organisation with tracing needs it.
	CapAPMTopology Capability = "apm_topology"
	// CapChanges is Datadog's event stream as a change source (section D). Off by default: changes come
	// from the platforms themselves, and this is for an organisation that also posts deployment,
	// configuration or infrastructure events into Datadog.
	CapChanges Capability = "changes"
)

// AllCapabilities is the published set, in the order the page lists them.
var AllCapabilities = []Capability{CapLogs, CapMonitors, CapTags, CapAPMTopology, CapChanges}

// DefaultCapabilities is the configuration an operator gets by naming nothing.
func DefaultCapabilities() Capabilities {
	return Capabilities{CapLogs: true, CapMonitors: true, CapTags: true}
}

// builtCapabilities are the ones this build can run. Enabling one that is specified but not built is
// refused at configuration rather than half-enabled, which is the failure FR-008b guards against.
// `changes` is built (T092) and off by default; `apm_topology` is specified and not built.
var builtCapabilities = map[Capability]bool{CapLogs: true, CapMonitors: true, CapTags: true, CapChanges: true}

// ParseCapabilities reads a comma-separated list, refusing an unknown name and a capability this build
// does not implement.
func ParseCapabilities(list string) (Capabilities, error) {
	out := Capabilities{}
	for _, raw := range strings.Split(list, ",") {
		name := Capability(strings.TrimSpace(raw))
		if name == "" {
			continue
		}
		known := false
		for _, c := range AllCapabilities {
			if c == name {
				known = true
			}
		}
		switch {
		case !known:
			return nil, fmt.Errorf("datadog: unknown capability %q; the published set is %v", name, AllCapabilities)
		case !builtCapabilities[name]:
			return nil, fmt.Errorf("datadog: capability %q is specified but not built in this release; "+
				"enabling it would request its scopes and issue nothing, which is the half-enabled state "+
				"FR-008b forbids", name)
		}
		out[name] = true
	}
	return out, nil
}

// Enabled reports whether a capability is on.
func (c Capabilities) Enabled(name Capability) bool { return c[name] }

// String is the stable, sorted spelling recorded in checkpoints: every published capability with its
// state, so an off capability is stated rather than absent.
func (c Capabilities) String() string {
	names := make([]string, 0, len(AllCapabilities))
	for _, name := range AllCapabilities {
		state := "off"
		if c[name] {
			state = "on"
		}
		names = append(names, string(name)+"="+state)
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}
