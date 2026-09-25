// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"fmt"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	metricpb "google.golang.org/genproto/googleapis/api/metric"
)

// The transport seam for the telemetry half (contract §2, FR-106).
//
// It is the same idea as the feeder's internal/feeders/gcp/transport.go and for the same reason:
// everything above it works against these interfaces, so live mode and recorded mode are **one
// code path** rather than two implementations that agree until they do not. A term implementation
// holding one of these cannot tell which side it is on and has no client to reach around it with.
//
// It is deliberately NOT a second client-constructing file. internal/feeders/gcp/transport.go is
// the only place in this repository that builds a per-area Google Cloud client, and the readers it
// exposes satisfy these interfaces structurally — so wiring a live backend is handing it the
// feeder's transport, not constructing a parallel one that meters its calls against a different
// budget. The payload types are Google's decoded messages for the same reason they are there: a
// private mirror would be a second schema to keep in step, and the recorded corpus is a recording
// of what Google actually returned.

// MetricReader reads Cloud Monitoring (contract §2, §3).
//
// Two calls, and the second is not an accident of convenience. `ingestDelay` is what makes
// NOT_YET_INGESTED a contractual boundary rather than a guess (contract §8), so the descriptor
// read sits in the same interface as the series read and a recorded world holds both.
type MetricReader interface {
	// ListTimeSeries runs one aggregated query. The aggregation is server-side and the points
	// never leave GCP: what comes back is already aligned and reduced (contract §3).
	ListTimeSeries(ctx context.Context, req *monitoringpb.ListTimeSeriesRequest) ([]*monitoringpb.TimeSeries, error)
	// GetMetricDescriptor reads one metric type's descriptor, for its
	// `metadata.ingestDelay` — *"data points older than this age are guaranteed to be
	// ingested and available to be read"*.
	GetMetricDescriptor(ctx context.Context, project, metricType string) (*metricpb.MetricDescriptor, error)
}

// LogReader reads one page of Cloud Logging entries and the token for the next (contract §4).
//
// One page per call, never a drained iterator: at 60 calls/min per project the caller decides
// whether to spend another, and the pagination-correctness rule — an empty page WITH a token is an
// unfinished search, not an empty one — is only expressible if the token is visible to the caller.
type LogReader interface {
	ListEntries(ctx context.Context, req *loggingpb.ListLogEntriesRequest) ([]*loggingpb.LogEntry, string, error)
}

// Alert is one Cloud Monitoring alerting incident, reduced to the fields FR-046 and FR-098 need
// (contract §5).
//
// This one IS a private shape rather than the vendor's message, and the reason is in the contract:
// the only Go binding for `projects.alerts` is google.golang.org/api/monitoring/v3, whose package
// header says it is in maintenance mode, and the API itself is Public Preview with Google warning
// that *"the labels in the response are subject to change while this feature is in preview"*.
// Pinning the fields this feature reads means a label shuffle upstream is a compile error in one
// adapter rather than a silent change in every digest.
type Alert struct {
	// Name is `projects/P/alerts/ALERT_ID`, system-assigned.
	Name string
	// State is OPEN or CLOSED.
	State string
	// OpenTime and CloseTime are the transition instants Google reports — FR-046's valid time.
	// CloseTime is zero on an open incident.
	OpenTime  time.Time
	CloseTime time.Time
	// Resource and Metric are the labels preserved from the generating condition: what the
	// alert watches.
	Resource map[string]string
	Metric   map[string]string
	// PolicyName is the policy snapshot's name, so a transition joins the ALERT node the
	// feeder's GA alertPolicies.list half recorded.
	PolicyName string
}

// AlertReader reads alerting incidents over a window (contract §5).
//
// It sits behind a declared capability flag and a Transport may carry a nil one. That nil is a
// stated scope, not a missing dependency: with incident reads off, `monitor_state` still answers —
// NO_DATA with coverage naming the absent source, exactly as `error_spans` does — which is the
// contract's own answer for a source the organisation does not have.
type AlertReader interface {
	ListAlerts(ctx context.Context, project string, start, end time.Time) ([]*Alert, error)
}

// Transport is the set of readers one backend holds. A nil member is an area this backend does not
// read, and every term that would have used it answers NO_DATA naming the absent source rather
// than failing.
type Transport struct {
	// Metrics serves compare, errors_by_version and onset.
	Metrics MetricReader
	// Logs serves new_log_patterns and exemplars.
	Logs LogReader
	// Alerts serves monitor_state, and is nil unless the operator declared the capability.
	Alerts AlertReader
}

// absentSourceOf names the source a nil reader stands for, in the words a coverage block uses.
// A reader that is nil is not an error: it is a source the organisation has not given this
// backend, and saying which one is the difference between "this cannot be checked here" and an
// unexplained empty answer.
func (t *Transport) absentSourceOf(area string) string {
	return fmt.Sprintf("the %s source is not configured for this backend, so nothing was searched", area)
}
