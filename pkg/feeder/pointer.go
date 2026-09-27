// SPDX-License-Identifier: Apache-2.0

package feeder

import (
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
)

// Pointers: where to look, never what was seen (constitution IV, FR-012).
//
// A pointer is the graph's answer to "show me the latency of this service around 14:32". It
// names a backend, a vocabulary, and a selector in that vocabulary, plus the OpenTelemetry
// resource attributes that identify the entity. What it never carries is a sample, a log line
// or a span — those stay in the backend that owns them, and a property that holds one is
// refused (ReasonTelemetryPayload).
//
// `otel-semconv/1.30` is the canonical vocabulary: an attribute selector written in
// OpenTelemetry semantic conventions, which any backend can translate. `k8s-resource/v1` is a
// resource path for a SOURCE_LINK back into the origin system, and `url` is a link. Feature 003
// adds three GCP vocabularies whose selectors are in GCP's own query languages; see the constant
// block below.
//
// A pointer that cannot be expressed in OTel terms must document why (constitution IV), which in
// practice means a paragraph on the constant that names the vocabulary and a comment on the
// constructor that mints it. The registry, with each vocabulary's selector grammar, is
// docs/schema/pointers.md.

// Pointer vocabularies (fixtures/README.md §"Pointer vocabularies").
const (
	// VocabOTelSemconv is an attribute selector in OpenTelemetry semantic conventions 1.30.
	// It is the canonical vocabulary for METRIC, LOG and TRACE pointers, and what makes
	// backends interchangeable.
	VocabOTelSemconv = "otel-semconv/1.30"
	// VocabK8sResource is a Kubernetes API resource path, e.g.
	// `apps/v1/namespaces/shop/deployments/checkout`, used for SOURCE_LINK pointers.
	VocabK8sResource = "k8s-resource/v1"

	// The three GCP vocabularies (003 FR-079-FR-085,
	// specs/003-gcp-integration/contracts/pointer-vocabularies.md).
	//
	// All three make the same split, which looks contradictory until both halves are visible:
	// the SELECTOR is in GCP's own query language, because it is what will actually be
	// executed and a selector translated into OTel and back is a selector that can silently
	// stop matching; the ATTRIBUTES stay in OpenTelemetry semantic conventions, so a reader or
	// a future backend can recognise which ENTITY a pointer is about without understanding
	// GCP's grammar. The entity is portable, the query is not, and pretending otherwise is the
	// failure mode constitution IV's "document why" clause exists to catch.
	//
	// Neither GCP query language is independently versioned: each is versioned only by the
	// enclosing API surface, which is why these names carry the API version. The API version
	// IS the vocabulary version, and the pointer records it.

	// VocabGCPMonitoringFilter is a Cloud Monitoring filter plus its Aggregation, executed by
	// projects.timeSeries.list on Monitoring API v3.
	//
	// Not otel-semconv-expressible: the filter language carries right-hand functions
	// (starts_with, one_of, monitoring.regex.full_match), the substring/key-existence operator
	// `:`, and an Aggregation that is part of what the pointer selects rather than a property
	// of the entity. None of those is an attribute equality. Contract §2.
	//
	// Chosen over PromQL and MQL for one reason FR-083 implies: a pointer is STORED and read
	// back years later, so its vocabulary must be stable and versioned rather than merely
	// current. PromQL is Google's recommended language but is served on API v1 with no
	// first-party Go binding and upstream-governed semantics; MQL is deprecated. Contract §2.1.
	//
	// Every selector minted in this vocabulary MUST pin `resource.type`, and that is a
	// correctness rule rather than a convention: run.googleapis.com/request_count is written
	// against both cloud_run_revision and cloud_run_instance, and cloud_run_instance carries no
	// revision_name label. A selector that does not pin the type silently picks up series with
	// no revision, and errors_by_version then merges or drops groups without saying so.
	VocabGCPMonitoringFilter = "gcp-monitoring-filter/v3"

	// VocabGCPLoggingQuery is the Logging query language, executed by logging.entries.list on
	// Logging API v2.
	//
	// Not otel-semconv-expressible, and this is the clearest of the three: otel-semconv is an
	// attribute-equality vocabulary, while this is a query language — severity comparison
	// (`severity >= WARNING`), RE2 regular expressions (`=~`), field-existence tests, negation
	// and parenthesised boolean structure. The pointers this feature needs use all of them, and
	// flattening a severity range into an equality set would change what the pointer matches
	// the next time Google adds a severity level. Contract §3.1.
	VocabGCPLoggingQuery = "gcp-logging-query/v2"

	// VocabGCPTraceFilter is registered and deliberately UNMINTED.
	//
	// This organisation has no trace data source, and FR-085 requires that absence be a stated
	// fact about the entity rather than a fabricated pointer — so that a consumer can tell "no
	// trace pointer because there is no tracing" from "nobody wrote one". No feeder mints one;
	// pkg/feeder's own test asserts that.
	//
	// Registering the name now is what lets error_spans become a live operation with no
	// contract change if Cloud Trace is ever enabled. Until then the term is served, answering
	// NO_DATA with the absent source named. Contract §4.
	VocabGCPTraceFilter = "gcp-trace-filter/v1"

	// The two deploy vocabularies (004 FR-049, T026-T028).
	//
	// Both are SOURCE_LINK vocabularies and nothing else. That is what settles constitution IV's
	// "document why this is not expressible in OpenTelemetry semantic conventions": the question
	// does not arise in the form it does for a metric or a log pointer, because these do not
	// select a SERIES at all. They address one object — this deployment, this workflow run — by
	// its place in a REST API, and otel-semconv is a vocabulary of attributes on telemetry.
	// There is no attribute equality that means "the deployment with id 42 in this repository",
	// and inventing one would be a selector no backend could execute.
	//
	// It is the same argument the Kubernetes source-link vocabulary already rests on
	// (VocabK8sResource), and unlike the three GCP vocabularies it needs no split between an
	// untranslatable selector and portable attributes: a SOURCE_LINK carries no query.
	//
	// Neither platform versions these paths independently of the API, so the version here is the
	// vocabulary's own and starts at v1. A path whose API version changes — Vercel puts one in
	// every path — is a change to the SELECTOR, not to the vocabulary.

	// VocabGitHubResource is a path under GitHub's REST API identifying one object, without the
	// host: `repos/{owner}/{repo}/deployments/{id}`, `repos/{owner}/{repo}/actions/runs/{id}`.
	//
	// Without the host on purpose. A pointer is stored and read back years later, and a GitHub
	// Enterprise Server installation serves the same paths under a different host — so putting
	// the host in the selector would make one estate's pointers unreadable against another's,
	// for a fact that belongs to the connector's configuration rather than to the object.
	VocabGitHubResource = "github-resource/v1"

	// VocabVercelResource is a path under Vercel's REST API identifying one object, with its API
	// version and without the host: `v13/deployments/{idOrUrl}`, `v9/projects/{idOrName}`.
	//
	// WITH the API version, unlike the GitHub vocabulary, because Vercel versions per path and
	// the same object is served at different versions with different shapes. A selector that
	// dropped it would not identify what was read.
	VocabVercelResource = "vercel-resource/v1"
)

// DeployVocabularies is the set registered by feature 004, in the order
// docs/schema/pointers.md lists them.
//
// Neither carries join keys, and that is a fact about what they are rather than an omission: join
// keys say which attribute of a SERIES plays which role, and a source link selects an object. A
// deployment has no version attribute to group by — it IS the version.
var DeployVocabularies = []string{VocabGitHubResource, VocabVercelResource}

// The Datadog vocabularies (005 T026; specs/005-datadog-connector/contracts/pointer-vocabularies.md).
const (
	// VocabDatadogLogs is a Datadog log-search query. Not otel-semconv-expressible: Datadog's grammar
	// distinguishes a TAG (`version:x`) from an ATTRIBUTE (`@version:x`), and the two are different
	// fields with different contents — a library's own JSON `version` stays `@version` and is not
	// the deployment's `version` tag (005 research §2.4). OpenTelemetry has no way to state that
	// distinction, so a translated selector could silently match the wrong field. Contract §1.
	VocabDatadogLogs = "datadog-logs/v1"
	// VocabDatadogMonitor is a Datadog monitor's own query, exactly as Datadog stores it, plus the
	// monitor id. Not otel-semconv-expressible: monitor queries span Datadog's metric, log and
	// composite grammars, each with its own aggregation and threshold syntax, and a monitor query is
	// only meaningful in its type's grammar. Contract §2.
	VocabDatadogMonitor = "datadog-monitor/v1"
)

// DatadogVocabularies is the set registered by feature 005, in the order the page documents them.
var DatadogVocabularies = []string{VocabDatadogLogs, VocabDatadogMonitor}

// VocabURL is a dashboard's URL or identifier in its backend. There is no OpenTelemetry vocabulary
// for "a dashboard", which is the documented reason DashboardPointer does not use one.
const VocabURL = "url"

// Vocabularies is every vocabulary this SDK publishes, in the order docs/schema/pointers.md
// documents them.
//
// It exists so the page and the registry can be compared in both directions. A vocabulary on the page
// that nothing registers promises a selector grammar no connector mints; one registered that the page
// does not document is a selector a backend may be handed with nothing to read it by — and
// constitution IV's "document why" clause is only a guarantee while the document is the whole list.
var Vocabularies = []string{
	VocabOTelSemconv,
	VocabK8sResource,
	VocabURL,
	VocabGitHubResource,
	VocabVercelResource,
	VocabGCPMonitoringFilter,
	VocabGCPLoggingQuery,
	VocabGCPTraceFilter,
	VocabDatadogLogs,
	VocabDatadogMonitor,
}

// GCPVocabularies is the set registered by feature 003, in the order
// specs/003-gcp-integration/contracts/pointer-vocabularies.md documents them.
//
// It exists so the telemetry backend can answer FR-111 precisely: it accepts the pointers the
// feeders emit WITHOUT translation, and a pointer it cannot execute is reported
// QUERY_FAILED / UNSUPPORTED_POINTER naming the kind and vocabulary — never executed as
// something else, which is the failure that would return a confident answer to a question
// nobody asked.
var GCPVocabularies = []string{
	VocabGCPMonitoringFilter, VocabGCPLoggingQuery, VocabGCPTraceFilter,
}

// AttrMetricName is the selector key naming which metric a METRIC pointer selects. It is not
// an OpenTelemetry resource attribute — it names the instrument, not the entity — so it
// appears in the selector and not in a pointer's attributes.
const AttrMetricName = "metric.name"

// NewPointer builds a pointer of any kind. The four helpers below are the shapes the reference
// feeders emit; use this one for a backend or a kind they do not cover.
func NewPointer(kind graphv1.PointerKind, backendKind, vocabulary, selector string, attrs map[string]string) *graphv1.Pointer {
	return &graphv1.Pointer{
		Kind:        kind,
		BackendKind: backendKind,
		Vocabulary:  vocabulary,
		Selector:    selector,
		Attributes:  copyAttrs(attrs),
	}
}

// TracePointer selects the spans of an entity in a tracing backend ("tempo", "jaeger",
// "datadog"). The selector is an OTel attribute expression; build it with OTelSelector.
func TracePointer(backendKind, selector string, attrs map[string]string) *graphv1.Pointer {
	return NewPointer(graphv1.PointerKind_TRACE, backendKind, VocabOTelSemconv, selector, attrs)
}

// MetricPointer selects one metric of an entity in a metrics backend ("prometheus",
// "datadog"). The selector identifies both the entity and the instrument, which is why
// OTelSelector takes a metric name.
func MetricPointer(backendKind, selector string, attrs map[string]string) *graphv1.Pointer {
	return NewPointer(graphv1.PointerKind_METRIC, backendKind, VocabOTelSemconv, selector, attrs)
}

// LogPointer selects the logs of an entity in a logging backend ("loki", "elasticsearch").
func LogPointer(backendKind, selector string, attrs map[string]string) *graphv1.Pointer {
	return NewPointer(graphv1.PointerKind_LOG, backendKind, VocabOTelSemconv, selector, attrs)
}

// SourceLinkPointer links back to the entity in the system the feeder read it from: the
// Kubernetes object, the pipeline run, the repository. backendKind names that system ("k8s",
// "github") and vocabulary says how to read the selector — VocabK8sResource for a Kubernetes
// resource path, a URL scheme name for a link.
func SourceLinkPointer(backendKind, vocabulary, selector string, attrs map[string]string) *graphv1.Pointer {
	return NewPointer(graphv1.PointerKind_SOURCE_LINK, backendKind, vocabulary, selector, attrs)
}

// DashboardPointer links to a prepared view of an entity. The selector is the dashboard's URL
// or identifier in its backend; there is no OTel vocabulary for "a dashboard", which is the
// documented reason this one is not expressed in semconv terms (constitution IV).
func DashboardPointer(backendKind, selector string, attrs map[string]string) *graphv1.Pointer {
	return NewPointer(graphv1.PointerKind_DASHBOARD, backendKind, VocabURL, selector, attrs)
}

// OTelSelector renders an attribute selector in the `otel-semconv/1.30` vocabulary:
// `key="value" AND key="value"`, keys in the order given, or sorted when none are named.
//
// Naming the keys is the normal case, because the selector is usually narrower than the
// attributes: a log selector picks the Kubernetes namespace and workload while the pointer's
// attributes also carry the environment, so that a reader knows which environment the pointer
// refers to without the selector having to filter on it.
//
// Values are quoted and backslash-escaped. A key named but absent from attrs is skipped, so a
// caller may list optional keys without checking them first.
func OTelSelector(attrs map[string]string, keys ...string) string {
	if len(keys) == 0 {
		keys = slices.Collect(maps.Keys(attrs))
		sort.Strings(keys)
	}
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		value, ok := attrs[key]
		if !ok {
			continue
		}
		parts = append(parts, key+"="+strconv.Quote(value))
	}
	return strings.Join(parts, " AND ")
}

// MetricSelector renders OTelSelector with `metric.name="<metric>"` appended, which is the
// shape a METRIC pointer's selector takes.
func MetricSelector(metric string, attrs map[string]string, keys ...string) string {
	selector := OTelSelector(attrs, keys...)
	instrument := AttrMetricName + "=" + strconv.Quote(metric)
	if selector == "" {
		return instrument
	}
	return selector + " AND " + instrument
}

// copyAttrs defensively copies a caller's map, so that a feeder reusing one attribute map
// across many pointers cannot mutate an event it has already emitted.
func copyAttrs(attrs map[string]string) map[string]string {
	if len(attrs) == 0 {
		return nil
	}
	return maps.Clone(attrs)
}

// Join keys: which attribute carries which role, in this pointer's own vocabulary (ADR-0005
// D6, 002 FR-014b).
//
// Two answers from two backends are only relatable if something says how. "The error rate by
// version" is expressible only if a pointer names the attribute that carries the deployed
// version, and that attribute is spelled differently in every vocabulary: `service.version` in
// OpenTelemetry semantic conventions, `version` as a Datadog tag, `labels.version` in a Cloud
// Logging filter. The role is the same; the spelling is the backend's.
//
// So a pointer may carry a small map from a **published role** to the attribute or tag name
// that plays it here. The role set is closed on purpose — five roles, listed below — because a
// vocabulary of roles that anyone may extend is not a vocabulary, it is a comment. A role
// nothing in the pointer's backend can express is simply left out; an empty map is omitted by
// canonical serialisation exactly as every other empty map is, so adding join keys moves no
// golden that has none to emit.
//
// The registry of pointer vocabularies and the roles each of them can express lives in
// docs/schema/pointers.md.
const (
	// JoinRoleVersion is the deployed version of the thing being observed. It is what makes
	// `errors_by_version(pointer, window)` expressible at all.
	JoinRoleVersion = "version"
	// JoinRoleWorkload is the workload, deployment or service the telemetry belongs to.
	JoinRoleWorkload = "workload"
	// JoinRolePod is the individual replica.
	JoinRolePod = "pod"
	// JoinRoleHost is the node or host the replica runs on.
	JoinRoleHost = "host"
	// JoinRoleTrace is the trace identifier, so an exemplar can be followed from a metric
	// digest into a trace backend.
	JoinRoleTrace = "trace"
)

// JoinRoles is the published role set, in the order docs/schema/pointers.md lists it.
var JoinRoles = []string{
	JoinRoleVersion, JoinRoleWorkload, JoinRolePod, JoinRoleHost, JoinRoleTrace,
}

// ValidJoinRole reports whether role is one of the published five.
func ValidJoinRole(role string) bool { return slices.Contains(JoinRoles, role) }

// WithJoinKeys returns p carrying the given join keys, skipping any role outside the published
// set — silently, because a feeder that names a role this build does not know is a feeder built
// against a later schema, and dropping the key it cannot express is better than refusing the
// pointer that would otherwise be perfectly usable.
//
// It mutates and returns p, so it composes with the constructors above:
//
//	feeder.WithJoinKeys(feeder.MetricPointer(...), map[string]string{
//	    feeder.JoinRoleVersion:  feeder.AttrServiceVersion,
//	    feeder.JoinRoleWorkload: feeder.AttrK8sDeploymentName,
//	})
func WithJoinKeys(p *graphv1.Pointer, joinKeys map[string]string) *graphv1.Pointer {
	if p == nil || len(joinKeys) == 0 {
		return p
	}
	out := make(map[string]string, len(joinKeys))
	for role, attribute := range joinKeys {
		if attribute == "" || !ValidJoinRole(role) {
			continue
		}
		out[role] = attribute
	}
	if len(out) == 0 {
		return p
	}
	p.JoinKeys = out
	return p
}

// PointerCompat replays a corpus recorded before a pointer field existed.
//
// A recording is evidence of what a feeder emitted on a day, and the conformance harness asks
// the strongest question there is of a connector: replay these recorded payloads through this
// feeder and produce, byte for byte, the events the recording ships. An additive field breaks
// that question — not because the feeder got worse, but because the recording is older than the
// field.
//
// There are two honest answers and the project has already chosen between them once. The first
// is to re-record, which is right when the recording is cheap; the second is to replay the
// corpus under the output shape it was recorded with, which is what `IDFormat` already does for
// the `@w1300` window keys of the hand-authored baseline. This is the second answer for
// `Pointer.join_keys` (ADR-0005 D6), and it is the right one here for a specific reason: the
// shipped fixtures are re-recorded by feature 002's tasks under a rule that names exactly which
// three may move (002 tasks.md T025, T032), and a recording re-derived as a side effect of this
// field would move four more and falsify the change package's central claim that it is additive.
//
// So: a live run and every recording made from now on carry join keys; the recordings made
// before ADR-0005 D6 are replayed with PointerCompatNoJoinKeys and keep reproducing themselves.
// The flag is on the *test's* configuration, never on a deployment's.
type PointerCompat int

const (
	// PointerCompatCurrent emits every pointer field this SDK knows. It is the zero value, so
	// a feeder that says nothing gets the current shape.
	PointerCompatCurrent PointerCompat = iota
	// PointerCompatNoJoinKeys omits Pointer.join_keys, reproducing a corpus recorded before
	// ADR-0005 D6.
	PointerCompatNoJoinKeys
)

// JoinKeys returns the join keys to emit under this compatibility level: the map as given, or
// nil when the level predates the field.
func (c PointerCompat) JoinKeys(joinKeys map[string]string) map[string]string {
	if c == PointerCompatNoJoinKeys {
		return nil
	}
	return joinKeys
}
