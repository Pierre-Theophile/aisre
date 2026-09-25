// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"regexp"
	"strings"
)

// Reading a pointer's selector without rewriting it (FR-111, contract §2 of pointer-vocabularies).
//
// The backend **executes the selector as minted**. What this file does is read the facts a
// selector already states — which metric, which resource type, which project, region, service and
// revision — so the backend can fetch the right metric descriptor, mint the right console link and
// attach the right join keys. It never composes a different selector from them and sends that:
// a selector translated and back is a selector that can silently stop matching, which is the
// failure FR-080 exists to prevent.
//
// The grammar is narrow on purpose. A Monitoring filter and a Logging query both express the
// clauses this backend cares about as `field = "value"` equalities, and every selector the GCP
// feeder mints writes them that way. Anything else in the selector — a severity comparison, an RE2
// match, a parenthesised disjunction — is left entirely alone and travels to GCP untouched; this
// reader simply reports nothing about it rather than guessing.

// equality matches `field="value"` and `field = "value"`, which is the only shape this reader
// claims to understand. `field` is a dotted path; the value is a double-quoted literal.
var equality = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_.]*)\s*=\s*"([^"]*)"`)

// selectorFacts are the clauses a selector pins, as it pins them. A field left empty is a clause
// the selector does not state — never a default this code supplied.
type selectorFacts struct {
	// MetricType is `metric.type`, e.g. `run.googleapis.com/request_count`.
	MetricType string
	// ResourceType is `resource.type`. Every metric selector this feature mints pins
	// `cloud_run_revision`, and contract §3.1 explains why that is a correctness rule rather
	// than a style one.
	ResourceType string
	// Project, Region, Service and Revision are the Cloud Run resource labels.
	Project  string
	Region   string
	Service  string
	Revision string
	// ResponseCodeClass is `metric.labels.response_code_class` where the selector restricts to
	// one, e.g. the `5xx` the feeder's error-rate pointer pins.
	ResponseCodeClass string
}

// parseSelector reads the equality clauses of a selector. It never fails: a selector it
// understands nothing of yields an empty set of facts, and the caller states what it could not
// determine rather than inventing it.
func parseSelector(selector string) selectorFacts {
	var facts selectorFacts
	for _, match := range equality.FindAllStringSubmatch(selector, -1) {
		field, value := match[1], match[2]
		switch field {
		case "metric.type":
			facts.MetricType = value
		case "resource.type":
			facts.ResourceType = value
		case "resource.labels.project_id":
			facts.Project = value
		case "resource.labels.location":
			facts.Region = value
		case "resource.labels.service_name":
			facts.Service = value
		case "resource.labels.revision_name":
			facts.Revision = value
		case "metric.labels.response_code_class":
			facts.ResponseCodeClass = value
		}
	}
	return facts
}

// derivedFromRequestCount reports whether a selector's answer carries contract §3.1's two
// caveats. It is a property of the metric, not of the term: any digest built on `request_count`
// undercounts the ingress failures, whether it was asked for a rate, an error rate or a split by
// revision.
func (f selectorFacts) derivedFromRequestCount() bool {
	return strings.HasSuffix(f.MetricType, "/request_count")
}

// scope is the `projects/<project>` the Monitoring and Logging APIs take as the search root. It
// prefers the project the selector names — the selector is what GCP receives, so a selector about
// another project is about another project — and falls back to the backend's own.
func (f selectorFacts) scope(fallback string) string {
	if f.Project != "" {
		return f.Project
	}
	return fallback
}
