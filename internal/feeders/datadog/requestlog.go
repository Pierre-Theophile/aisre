// SPDX-License-Identifier: Apache-2.0

package datadog

import "github.com/Pierre-Theophile/aisre/pkg/feeder"

// The published read-only operation surface (005 T007; FR-002–FR-004, SC-015).
//
// Every request the connector can issue is one of these constants, and a capability's operations are
// declared only when the capability is enabled (FR-008b), so a disabled capability cannot spend a
// call even by mistake. docs/connectors/datadog.md §2 publishes the default set and a test compares
// the two in both directions.
//
// The two logs operations are POST. They are named queries (ADR-0010 item 3): each is declared
// individually with the published reason it cannot change state, and the surface refuses every
// other POST at init.

const (
	// Startup: prove the credential before anything else runs (FR-003).
	OpValidateKeys feeder.ReadOperation = "GET /api/v2/validate_keys"
	OpOwnAppKey    feeder.ReadOperation = "GET /api/v2/current_user/application_keys/{id}"

	// Logs: the telemetry backend's log terms, version-stamp discovery, rollout detection.
	OpSearchLogs    feeder.ReadOperation = "POST /api/v2/logs/events/search"
	OpAggregateLogs feeder.ReadOperation = "POST /api/v2/logs/analytics/aggregate"

	// Monitors: definitions and per-group states.
	OpListMonitors feeder.ReadOperation = "GET /api/v1/monitor"
	OpGetMonitor   feeder.ReadOperation = "GET /api/v1/monitor/{monitor_id}"
)

// Platform is the connector's name, as it appears in a refusal.
const Platform = "datadog"

var startupOps = map[feeder.ReadOperation]feeder.ReadOperationSpec{
	OpValidateKeys: {
		Area: "startup",
		Why:  "check that the API and application keys are valid before anything else runs",
	},
	OpOwnAppKey: {
		Area: "startup",
		Why:  "read the application key's own scopes, so a write-capable key refuses the start",
	},
}

var capabilityOps = map[Capability]map[feeder.ReadOperation]feeder.ReadOperationSpec{
	CapLogs: {
		OpSearchLogs: {
			Area: "logs",
			Why: "named query: returns the log events matching the query in the body, paged; the " +
				"endpoint has no field that creates, updates or deletes anything",
			NamedQuery: true,
		},
		OpAggregateLogs: {
			Area: "logs",
			Why: "named query: returns counts of the log events matching the query in the body, " +
				"grouped by facet; the endpoint has no field that creates, updates or deletes anything",
			NamedQuery: true,
		},
	},
	CapMonitors: {
		OpListMonitors: {
			Area: "monitors",
			Why:  "list monitor definitions with their per-group states",
		},
		OpGetMonitor: {
			Area: "monitors",
			Why:  "read one monitor with its per-group states",
		},
	},
	// CapTags issues nothing of its own: tags arrive on the payloads above.
}

// SurfaceFor builds the surface for a set of enabled capabilities: the startup operations, plus each
// enabled capability's own. It panics on a malformed table, which is a build defect, not a runtime
// condition.
func SurfaceFor(caps Capabilities) *feeder.ReadOnlySurface {
	ops := map[feeder.ReadOperation]feeder.ReadOperationSpec{}
	for op, spec := range startupOps {
		ops[op] = spec
	}
	for _, name := range AllCapabilities {
		if !caps.Enabled(name) {
			continue
		}
		for op, spec := range capabilityOps[name] {
			ops[op] = spec
		}
	}
	return feeder.MustReadOnlySurface(Platform, ops)
}

// DefaultSurface is the surface of the default capabilities, which is what the published page lists.
var DefaultSurface = SurfaceFor(DefaultCapabilities())
