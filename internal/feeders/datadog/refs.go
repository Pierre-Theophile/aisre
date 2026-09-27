// SPDX-License-Identifier: Apache-2.0

package datadog

import "github.com/Pierre-Theophile/aisre/pkg/feeder"

// The Datadog namespaces (005 T014, FR-058). They are local, as feature 003's gcp.* are, because only
// this connector mints them; internal/resolution carries its own copy of the one a rule reads, and a
// test asserts the two spellings agree.
const (
	// NSService addresses a watched log source's service node, valued `<env>/<service>`. The
	// environment is in the value because a service name is unique only within one environment.
	NSService = "datadog.service"
	// NSLogService is the claim C9 reads: the service name as the logs state it, with the
	// environment as a supporting attribute. It is NOT an identifying namespace: two organisations,
	// or two environments, may state the same name for different services (FR-062), so equality of
	// the value alone must never merge anything — only C9, which also requires equal environments.
	NSLogService = "datadog.log_service"
	// NSMonitor addresses a monitor, valued by its id; a grouped monitor's alert adds the group.
	NSMonitor = "datadog.monitor"
	// NSChange addresses a log-observed rollout, valued `<env>/<service>@<version>@<first-seen>`.
	NSChange = "datadog.change"
)

// AttrEnvironment is the supporting attribute every environment-scoped claim carries.
const AttrEnvironment = feeder.AttrDeploymentEnvironment
