// SPDX-License-Identifier: Apache-2.0

package sanitise

// The Datadog sanitisation table (005 T079, FR-072–FR-075; pulled into Phase 3 because the telemetry
// backend cannot answer live without it — FR-052 applies the same boundary to a live investigation).
//
// It is its own table, built with NewPolicy as the package comment promised, because Datadog's
// payloads are not GCP's: the paths differ, and a shared table would be one whose unassigned-field
// refusal fires on the other vendor's fields. Every rule carries its reason, and Validate enforces the
// guarantees the contract makes: people identifiers are dropped, never hashed; every pseudonym is
// typed; an unlisted tag key is dropped.

// DatadogPolicyVersion is the version recorded in every Datadog manifest and digest.
const DatadogPolicyVersion = "datadog-1.0.0"

// The Datadog field paths the connector hands to Sanitiser.Field. They are paths in the connector's own
// reading of a payload, dotted, and they are constants so a backend and a feeder cannot spell one
// field two ways.
const (
	DatadogPathService      = "datadog.service"
	DatadogPathEnvironment  = "datadog.env"
	DatadogPathHost         = "datadog.host"
	DatadogPathVersion      = "datadog.version"
	DatadogPathMonitorName  = "datadog.monitor.name"
	DatadogPathMonitorGroup = "datadog.monitor.group"
	DatadogPathMonitorQuery = "datadog.monitor.query"
	DatadogPathMonitorBody  = "datadog.monitor.message"
	DatadogPathMonitorOwner = "datadog.monitor.creator.email"
	DatadogPathUserEmail    = "datadog.log.usr.email"
	DatadogPathUserName     = "datadog.log.usr.name"
	DatadogPathClientIP     = "datadog.log.network.client.ip"
)

// DatadogPolicy returns the table.
func DatadogPolicy() *Policy {
	fields := map[string]Rule{
		// §1 dropped: never recorded, in any form.
		DatadogPathMonitorBody: {Disposition: Dropped, Why: "a monitor notification body routinely " +
			"carries webhook targets, handles and occasionally credentials; dropped whole, not " +
			"redacted in place (FR-074)"},
		DatadogPathMonitorOwner: {Disposition: Dropped, Why: "a person"},
		DatadogPathUserEmail:    {Disposition: Dropped, Why: "a person"},
		DatadogPathUserName:     {Disposition: Dropped, Why: "a person"},
		DatadogPathClientIP: {Disposition: Dropped, Why: "where a person or a customer connected " +
			"from, not an address that serves traffic"},

		// §2 pseudonymised: infrastructure identifiers, keyed HMAC, consistent across the corpus so
		// joins survive (FR-072a, FR-073).
		DatadogPathService:      {Disposition: Pseudonym, Kind: KindService, Why: "a service name"},
		DatadogPathEnvironment:  {Disposition: Pseudonym, Kind: KindEnvironment, Why: "an environment name"},
		DatadogPathHost:         {Disposition: Pseudonym, Kind: KindHost, Why: "a host or worker name"},
		DatadogPathMonitorName:  {Disposition: Pseudonym, Kind: KindAlertPolicy, Why: "a monitor's name"},
		DatadogPathMonitorGroup: {Disposition: Pseudonym, Kind: KindResource, Why: "a monitor group names the resources it alerts on"},

		// §3 verbatim, each with the reason it is safe.
		DatadogPathVersion: {Disposition: Verbatim, Why: "the deployed version a service stamps on " +
			"its logs — a commit sha, an image digest or a release — is not a people or " +
			"infrastructure identifier under FR-072a, and pseudonymising it would break the join " +
			"to the deploy feeders' changes it exists for (FR-040d)"},
		DatadogPathMonitorQuery: {Disposition: Verbatim, Why: "the monitor's query, kept because " +
			"it is what the graph needs (FR-074); identifiers inside it are sanitised by the " +
			"template pass"},
	}
	labels := map[string]Rule{
		"env":     {Disposition: Pseudonym, Kind: KindEnvironment, Why: "an environment name"},
		"service": {Disposition: Pseudonym, Kind: KindService, Why: "a service name"},
		"host":    {Disposition: Pseudonym, Kind: KindHost, Why: "a host name"},
		"team":    {Disposition: Pseudonym, Kind: KindTeam, Why: "an owning team"},
		"version": {Disposition: Verbatim, Why: "the deployed version; see " + DatadogPathVersion},
	}
	return NewPolicy(DatadogPolicyVersion, fields, labels)
}
