// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"fmt"
	"strings"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// Pointers (T059, FR-079, FR-080, FR-081, FR-085, contracts/pointer-vocabularies.md).
//
// A pointer says *where to look*, never what was found (constitution IV). Three properties of the
// ones minted here are contract rather than style:
//
//   - the **selector** is written in the vocabulary that will actually be executed — a Cloud
//     Monitoring filter for a metric, a Logging query for a log — because a selector in a vocabulary
//     nobody executes is a selector that has never been tested (FR-080);
//   - the **attributes** identifying which entity the pointer is about are OpenTelemetry semantic
//     conventions, so a reader or a future backend recognises the entity without understanding GCP's
//     grammar (FR-081);
//   - a pointer carries **no credential**, and no project-scoped secret. It names a project because
//     the project is part of the address, and nothing that authenticates.
//
// # resource.type is mandatory in every metric selector, and it is a correctness rule
//
// `run.googleapis.com/request_count` is written against **both** `cloud_run_revision` and
// `cloud_run_instance`, and `cloud_run_instance` carries **no `revision_name` label**. A selector
// that does not pin `resource.type` therefore silently picks up series with no revision — and
// `errors_by_version`, whose entire job is to say "the new revision is failing and the old one is
// not", then merges or drops groups without saying so. It would not error. It would answer, and the
// answer would be wrong in the direction that exonerates a bad deploy.
//
// So `MetricPointer` below refuses a selector with no `resource.type`, rather than documenting that
// callers should remember one.

// BackendKindGCP is the backend that executes these pointers: this feature's own telemetry backend.
const BackendKindGCP = "gcp"

// The Cloud Run monitored resource types. `cloud_run_revision` is the one every metric pointer here
// pins; `cloud_run_instance` is named so that the reason for pinning is greppable from the code that
// depends on it.
const (
	ResourceTypeCloudRunRevision = "cloud_run_revision"
	ResourceTypeCloudRunInstance = "cloud_run_instance"
)

// The published Cloud Run metrics FR-079 requires a pointer for: request rate, error rate and
// latency. They are Google's own metric type names, which is what the Monitoring filter matches on.
const (
	MetricRequestCount   = "run.googleapis.com/request_count"
	MetricRequestLatency = "run.googleapis.com/request_latencies"
	MetricInstanceCount  = "run.googleapis.com/container/instance_count"
)

// ErrNoResourceType is returned by MetricPointer for a selector that does not pin `resource.type`.
var ErrNoResourceType = fmt.Errorf("gcp: a Monitoring selector with no resource.type")

// MetricPointer builds a Cloud Monitoring metric pointer, refusing a selector that does not pin
// `resource.type` (see the file comment for why that is a correctness rule).
func MetricPointer(selector string, attrs map[string]string) (*graphv1.Pointer, error) {
	if !strings.Contains(selector, `resource.type=`) {
		return nil, fmt.Errorf("%w: %q. run.googleapis.com/request_count is written against both "+
			"cloud_run_revision and cloud_run_instance, and cloud_run_instance carries no "+
			"revision_name label — an unpinned selector picks up series with no revision and "+
			"errors_by_version then merges or drops groups without saying so", ErrNoResourceType, selector)
	}
	return feeder.WithJoinKeys(
		feeder.NewPointer(graphv1.PointerKind_METRIC, BackendKindGCP,
			feeder.VocabGCPMonitoringFilter, selector, attrs),
		joinKeysFor(selector)), nil
}

// joinKeysFor returns the join keys of the monitored resource the selector pins.
//
// It reads the resource type out of the selector rather than taking it as an argument because the
// pin is already there and is already mandatory — deriving the roles from the same clause that
// decides what the query returns is what keeps a pointer from declaring a field its own resource
// type does not have. A resource this feeder has no published mapping for gets no join keys, which
// is the honest answer rather than the convenient one.
func joinKeysFor(selector string) map[string]string {
	switch {
	case strings.Contains(selector, `resource.type="`+ResourceTypeCloudRunRevision+`"`):
		return cloudRunJoinKeys
	case strings.Contains(selector, `resource.type="`+ResourceTypeCloudSQLDatabase+`"`):
		return cloudSQLJoinKeys
	default:
		return nil
	}
}

// LogPointer builds a Cloud Logging pointer.
func LogPointer(selector string, attrs map[string]string) *graphv1.Pointer {
	return feeder.WithJoinKeys(
		feeder.NewPointer(graphv1.PointerKind_LOG, BackendKindGCP,
			feeder.VocabGCPLoggingQuery, selector, attrs),
		joinKeysFor(selector))
}

// ConsoleLink builds the SOURCE_LINK pointer back to the GCP console. Its vocabulary is a URL, so it
// is minted through the SDK's source-link helper with the console's own "vocabulary" name: the
// selector is the URL, and a reader follows it rather than executing it.
func ConsoleLink(url string, attrs map[string]string) *graphv1.Pointer {
	return feeder.SourceLinkPointer(BackendKindGCP, VocabGCPConsoleURL, url, attrs)
}

// VocabGCPConsoleURL is the vocabulary of a console deep link: an absolute https URL. It is named
// rather than left empty because a pointer whose vocabulary is empty is a pointer a backend has to
// guess at, and the whole point of FR-080 is that it never has to.
const VocabGCPConsoleURL = "gcp-console-url/v1"

// otelAttrs builds the OpenTelemetry attributes that say which entity a pointer is about (FR-081).
//
// `cloud.region` and `service.name` are conventions; the project and the Cloud Run revision have no
// OTel convention, so they carry this project's `sre.` names — the split FR-007 asks for, visible in
// the key rather than explained in a comment.
func otelAttrs(svc Service, revision, environment string) map[string]string {
	attrs := map[string]string{
		feeder.AttrCloudRegion: svc.Region,
		feeder.AttrServiceName: svc.Name,
		PropProject:            svc.Project,
	}
	if revision != "" {
		attrs[PropRevisionLabel] = revision
	}
	if environment != "" && environment != EnvironmentUnknown {
		attrs[feeder.AttrDeploymentEnvironment] = environment
	}
	return attrs
}

// PropRevisionLabel is the attribute naming which Cloud Run revision a pointer is about. It matches
// the `revision_name` label Monitoring groups by, which is what lets a join key line up with a
// digest's grouping without a translation table.
const PropRevisionLabel = "sre.gcp.revision_name"

// The Monitoring and Logging filter fields that carry a join role (ADR-0005 D6, FR-102).
//
// They are the **filter-field** spelling — `resource.labels.revision_name`, not the bare
// `revision_name` a returned series' label map is keyed by — because a join key names the
// attribute in *this pointer's* vocabulary, and this pointer's vocabulary is a Monitoring filter
// whose filterable fields are `resource.labels.*`. The same spelling is what
// internal/backends/gcp groups by, and a term naming any other field is refused there rather than
// answered; a test in that package asserts the two constants are one string, because two spellings
// that drifted apart would leave a pointer advertising a field the query never groups by, with one
// merged row as the only symptom.
const (
	// FieldRevisionName carries the JoinRoleVersion role: for Cloud Run, the deployed version
	// *is* the revision. It is the field `errors_by_version` groups by.
	FieldRevisionName = "resource.labels.revision_name"
	// FieldServiceName carries the JoinRoleWorkload role: the Cloud Run service.
	FieldServiceName = "resource.labels.service_name"
	// FieldDatabaseID carries the JoinRoleHost role for Cloud SQL — the instance connection
	// name `<project>:<instance>`, which is what a Cloud SQL series has in place of a replica.
	FieldDatabaseID = "resource.labels.database_id"
)

// cloudRunJoinKeys is the role -> field map every `cloud_run_revision` pointer here carries, metric
// and log alike.
//
// It is a package-level value rather than a literal per pointer because the map is a statement
// about the vocabulary and the monitored resource, not about the individual pointer: every
// selector pinned to `cloud_run_revision` spells these two roles the same way.
//
// Three of the five published roles are **absent, and each absence is a fact about GCP rather than
// an omission** — a role nothing in the pointer's backend can express is left out rather than
// guessed at (pkg/feeder, ADR-0005 D6):
//
//   - `pod` — the per-instance label is `instance_id`, and it exists only on
//     `cloud_run_instance`. Every selector here pins `cloud_run_revision` precisely because
//     `cloud_run_instance` carries no `revision_name`, so the two are mutually exclusive: a
//     pointer cannot carry both a revision and an instance. Declaring `pod` would name a field
//     the selector's own resource type does not have;
//   - `host` — Cloud Run is serverless. There is no node, and no label that names one;
//   - `trace` — this estate has no trace data source (FR-085); the trace vocabulary is
//     registered and deliberately unminted.
var cloudRunJoinKeys = map[string]string{
	feeder.JoinRoleVersion:  FieldRevisionName,
	feeder.JoinRoleWorkload: FieldServiceName,
}

// ServicePointers returns the pointers every Cloud Run service node carries (FR-079).
//
// A trace pointer is **deliberately absent**, and that absence is a stated fact rather than an
// oversight: this organisation has no trace data source, FR-085 requires the absence be recorded so
// a consumer can tell "no trace pointer because there is no tracing" from "nobody wrote one", and
// `gcp-trace-filter/v1` is registered and unminted for exactly that reason. `TracePointerAbsence`
// below is how the fact is carried.
func ServicePointers(obs ServiceObservation) ([]*graphv1.Pointer, error) {
	svc := obs.Service
	attrs := otelAttrs(svc, "", obs.Labels.Environment)
	base := fmt.Sprintf(`resource.type="%s" AND resource.labels.project_id="%s" AND `+
		`resource.labels.location="%s" AND resource.labels.service_name="%s"`,
		ResourceTypeCloudRunRevision, svc.Project, svc.Region, svc.Name)

	var out []*graphv1.Pointer
	for _, metric := range []string{MetricRequestCount, MetricRequestLatency, MetricInstanceCount} {
		pointer, err := MetricPointer(fmt.Sprintf(`metric.type="%s" AND %s`, metric, base), attrs)
		if err != nil {
			return nil, err
		}
		out = append(out, pointer)
	}

	// The error rate is the request count restricted to 5xx. It is a separate pointer rather than
	// an aggregation note on the request-count one because `response_code_class` is a metric label
	// and belongs in the selector — the thing that is executed — not in a comment a backend would
	// have to interpret.
	errRate, err := MetricPointer(fmt.Sprintf(
		`metric.type="%s" AND metric.labels.response_code_class="5xx" AND %s`, MetricRequestCount, base), attrs)
	if err != nil {
		return nil, err
	}
	out = append(out, errRate)

	out = append(out, LogPointer(fmt.Sprintf(
		`resource.type="%s" AND resource.labels.project_id="%s" AND resource.labels.location="%s" `+
			`AND resource.labels.service_name="%s" AND severity >= WARNING`,
		ResourceTypeCloudRunRevision, svc.Project, svc.Region, svc.Name), attrs))

	out = append(out, ConsoleLink(fmt.Sprintf(
		"https://console.cloud.google.com/run/detail/%s/%s/metrics?project=%s",
		svc.Region, svc.Name, svc.Project), attrs))

	return out, nil
}

// RevisionPointers returns a revision's pointers: the same signals, pinned to the revision.
//
// The revision-scoped selectors are what make `errors_by_version` answerable at all: without
// `resource.labels.revision_name` in the selector, "is the new revision failing and the old one not"
// has no query behind it.
func RevisionPointers(obs RevisionObservation, environment string) ([]*graphv1.Pointer, error) {
	rev := obs.Revision
	attrs := otelAttrs(rev.Service, rev.Revision, environment)
	base := fmt.Sprintf(`resource.type="%s" AND resource.labels.project_id="%s" AND `+
		`resource.labels.location="%s" AND resource.labels.service_name="%s" AND `+
		`resource.labels.revision_name="%s"`,
		ResourceTypeCloudRunRevision, rev.Project, rev.Region, rev.Name, rev.Revision)

	var out []*graphv1.Pointer
	for _, metric := range []string{MetricRequestCount, MetricRequestLatency} {
		pointer, err := MetricPointer(fmt.Sprintf(`metric.type="%s" AND %s`, metric, base), attrs)
		if err != nil {
			return nil, err
		}
		out = append(out, pointer)
	}
	out = append(out, LogPointer(base+" AND severity >= WARNING", attrs))
	out = append(out, ConsoleLink(fmt.Sprintf(
		"https://console.cloud.google.com/run/detail/%s/%s/revisions?project=%s",
		rev.Region, rev.Name, rev.Project), attrs))
	return out, nil
}

// PropTracePointerAbsent records why a node carries no trace pointer (FR-085). It is a property
// rather than a silence: "there is no tracing in this estate" and "nobody wrote a trace pointer" are
// different facts, and only the first is a fact about production.
const PropTracePointerAbsent = "sre.gcp.trace_pointer_absent"

// TracePointerAbsence is the stated reason a node carries no trace pointer.
//
// It deliberately does **not** reference the trace vocabulary's constant, and not out of fussiness:
// pkg/feeder's own test asserts that nothing outside its declaration mentions that identifier, on the
// grounds that a reference is how something eventually mints a pointer in it. Naming it here — even in
// a message — would make that guard fire, and the right response to a strict guard about fabricating
// telemetry pointers is to stay inside it rather than to widen it for a string constant. The
// registration, with the name, is in pkg/feeder/pointer.go, which is where a reader looking for what
// *would* be minted goes anyway.
const TracePointerAbsence = "no trace data source in this estate; the GCP trace vocabulary is " +
	"registered in pkg/feeder and deliberately unminted (FR-085)"

// PointersCarryNoCredential reports whether any pointer in the set carries something that
// authenticates. It is the assertion behind FR-079's "carrying no credential", and it is a function
// rather than a review item because a console link is a URL and a URL is where a token ends up.
//
// It looks for the shapes a credential takes in a URL or a filter — a bearer token, an api key, a
// signature parameter — and returns the first offending pointer's index and the class found.
func PointersCarryNoCredential(pointers []*graphv1.Pointer) (int, string) {
	markers := map[string]string{
		"access_token":     "an access token",
		"id_token":         "an id token",
		"apikey":           "an api key",
		"api_key":          "an api key",
		"key=aiza":         "a Google API key",
		"authorization":    "an authorization header",
		"bearer ":          "a bearer token",
		"x-goog-signature": "a signed-URL signature",
		"signature=":       "a signature parameter",
	}
	for i, pointer := range pointers {
		haystack := strings.ToLower(pointer.GetSelector())
		for _, value := range pointer.GetAttributes() {
			haystack += "\n" + strings.ToLower(value)
		}
		for marker, class := range markers {
			if strings.Contains(haystack, marker) {
				return i, class
			}
		}
	}
	return -1, ""
}
