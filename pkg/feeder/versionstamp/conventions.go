// SPDX-License-Identifier: Apache-2.0

// Package versionstamp finds the attribute that carries a service's deployed version on its logs,
// and turns each version value into the platform-neutral deploy vocabulary (005 FR-040c–FR-040e;
// specs/005-datadog-connector/contracts/version-stamping.md).
//
// It is deliberately not any vendor's. A service stamps its own logs — only the running process knows
// which version wrote a line — and the stamp is joined to the graph through deploy.commit_sha,
// deploy.image and deploy.release, never through a platform-specific identifier the logs happen to
// carry. So "did errors start with this version?" is answered the same way whether Cloud Run,
// Kubernetes, Vercel, a vendor-hosted runtime or a VM deployed the service, and every log backend
// uses this package rather than re-deriving the list — the drift 003's C4 showed two sources spelling
// one join differently produces.
package versionstamp

// ConventionsVersion is the version of the published convention list. A change to the list or its
// order is a new version, recorded in every checkpoint and verdict that used it.
const ConventionsVersion = "1.0.0"

// Form is how a candidate is carried on a log line. A tag and an attribute of the same name are
// different fields with different contents — a library's own JSON `version` field stays the attribute
// `@version` and is not the unified-service-tagging tag `version` (research §2.4) — so they are
// separate candidates.
type Form string

const (
	// FormTag is a tag, e.g. Datadog's `version` set by DD_VERSION.
	FormTag Form = "tag"
	// FormAttribute is an attribute of the log record.
	FormAttribute Form = "attribute"
	// FormAttributePair is two attributes read together: an image name and its digest.
	FormAttributePair Form = "attribute_pair"
)

// Candidate is one place a version may be stamped.
type Candidate struct {
	// Name is the attribute or tag name as the convention spells it. For FormAttributePair it is the
	// name attribute, and Pair is the digest attribute.
	Name string
	Form Form
	Pair string
	// SetBy says what typically sets it, for the verdict and the stamping guide.
	SetBy string
}

// Label is the candidate's stable spelling in a verdict, e.g. `version (tag)`.
func (c Candidate) Label() string {
	if c.Form == FormAttributePair {
		return c.Name + " + " + c.Pair + " (" + string(c.Form) + ")"
	}
	return c.Name + " (" + string(c.Form) + ")"
}

// Conventions is the published list, in the order candidates are tried (contract §2). The first one
// that passes the share test wins.
var Conventions = []Candidate{
	{Name: "version", Form: FormTag,
		SetBy: "unified service tagging: DD_VERSION, tags.datadoghq.com/version, com.datadoghq.tags.version; OpenTelemetry service.version as mapped by the backend"},
	{Name: "service.version", Form: FormAttribute,
		SetBy: "OpenTelemetry log records whose resource attributes are kept as attributes"},
	{Name: "version", Form: FormAttribute,
		SetBy: "the application's own structured logger — and libraries logging their own version, which the share test rejects"},
	{Name: "git.commit.sha", Form: FormTag,
		SetBy: "source-code integration: DD_GIT_COMMIT_SHA, the OCI org.opencontainers.image.revision label"},
	{Name: "git.commit.sha", Form: FormAttribute,
		SetBy: "source-code integration, where carried as an attribute"},
	{Name: "container.image.name", Form: FormAttributePair, Pair: "container.image.digest",
		SetBy: "container runtimes and collectors enriching logs with image metadata"},
	{Name: "faas.version", Form: FormAttribute,
		SetBy: "the version role of a registered platform vocabulary, e.g. a Cloud Run revision"},
}

// ConventionLabels returns every candidate's label in order, which is what a NO_DATA answer names as
// "searched".
func ConventionLabels() []string {
	out := make([]string, 0, len(Conventions))
	for _, c := range Conventions {
		out = append(out, c.Label())
	}
	return out
}
