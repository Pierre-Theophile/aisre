// SPDX-License-Identifier: Apache-2.0

package datadog

import (
	"fmt"
	"strings"
)

// Environment-field discovery (005, found on the first live run).
//
// Unified service tagging puts a log line's environment in the `env` tag. Many organisations' logs carry
// it elsewhere: the first live organisation's JSON logs carry `{"env": "production"}`, which Datadog
// indexes as the `@env` attribute, and Datadog's `env:` search reads only the tag. An agent that required
// the tag would measure nothing there, and one that required the operator to know which field to name
// would fail everyone who has not read this comment. So the connector discovers the field the way it
// discovers the version stamp: a published list, tried in order, each accepted only when it is present on
// the service's lines at the published share. `--env-field` overrides the list, as `--version-override`
// overrides the version conventions.
//
// The discovery costs one count of the service's lines and one per field tried, and it is made once per
// source and poller: the field is remembered, and looked for again only when the source stops matching.

// EnvFieldCandidate is one place an environment is carried.
type EnvFieldCandidate struct {
	// Field is spelled as the search grammar spells it: `env` for the tag, `@name` for an attribute.
	Field string
}

// Label is the candidate as a verdict names it.
func (c EnvFieldCandidate) Label() string {
	if strings.HasPrefix(c.Field, "@") {
		return c.Field + " (attribute)"
	}
	return c.Field + " (tag)"
}

// EnvFields is the published list, in the order tried.
var EnvFields = []EnvFieldCandidate{
	{Field: "env"},                          // unified service tagging's tag
	{Field: "@env"},                         // a JSON logger's `env` field
	{Field: "@environment"},                 // the same, spelled out
	{Field: "@deployment.environment.name"}, // OpenTelemetry's resource attribute, kept as an attribute
}

// EnvFieldShare is the share of the service's lines a field must be present on to be the environment.
const EnvFieldShare = 0.95

// EnvDiscovery is what the measurer found about where a service's environment is carried.
type EnvDiscovery struct {
	// ServiceLines counts the service's lines over the window, whatever their environment.
	ServiceLines int64 `json:"service_lines"`
	// Fields are the candidates tried, in order, with the lines each is present on.
	Fields []CandidateCount `json:"fields,omitempty"`
	// Values are the environments the chosen field carries, with their lines: read when the source
	// names no environment, or names one no line carries.
	Values []EnvValue `json:"values,omitempty"`
}

// EnvValue is one environment and the lines that carry it.
type EnvValue struct {
	Value string `json:"value"`
	Lines int64  `json:"lines"`
}

// DecideEnvField is the first candidate present on EnvFieldShare of the service's lines, or "".
func DecideEnvField(serviceLines int64, present map[string]int64) string {
	if serviceLines <= 0 {
		return ""
	}
	for _, c := range EnvFields {
		if float64(present[c.Field]) >= EnvFieldShare*float64(serviceLines) {
			return c.Field
		}
	}
	return ""
}

// IsEnvField reports whether a selector key is one of the published environment fields.
func IsEnvField(key string) bool {
	for _, c := range EnvFields {
		if c.Field == key {
			return true
		}
	}
	return false
}

// envNote is what the checkpoint says about a source's environment, and whether it is a warning (a
// configuration to fix) or a statement (how the connector read it). Empty when there is nothing to say.
func envNote(src LogSource, m SourceMeasurement, measured bool) (note string, warning bool) {
	d := m.EnvDiscovery
	switch {
	case src.Env == "" && d != nil && len(d.Values) > 0:
		return fmt.Sprintf("%s: its logs carry the environment in %s: %s. Watch <env>/%s (for example --watch %s/%s) "+
			"so the service is matched to the same service on other platforms (C9 needs the environment)",
			src.Key(), m.EnvField, valuesText(d.Values), src.Service, d.Values[0].Value, src.Service), true
	case src.Env == "":
		return src.Key() + ": " + MissingEnvWarning(src.Service), true
	case !measured:
		return "", false
	case m.Lines == 0 && d != nil && len(d.Values) > 0:
		return fmt.Sprintf("%s: no line carries %s:%s, and the service's lines carry %s: %s. Check the environment's "+
			"name: --watch %s/%s", src.Key(), m.EnvField, src.Env, m.EnvField, valuesText(d.Values),
			d.Values[0].Value, src.Service), true
	case m.EnvField == "" && d != nil && d.ServiceLines > 0:
		return fmt.Sprintf("%s: none of %s is on the service's lines. %s", src.Key(), envFieldList(),
			MissingEnvWarning(src.Service)), true
	case m.EnvField != "" && m.EnvField != "env":
		share := ""
		if d != nil && d.ServiceLines > 0 {
			for _, f := range d.Fields {
				if f.Label == (EnvFieldCandidate{Field: m.EnvField}).Label() {
					share = fmt.Sprintf(", present on %.4g%% of the service's lines", 100*float64(f.Lines)/float64(d.ServiceLines))
				}
			}
		}
		return fmt.Sprintf("%s: the environment is read from %s%s. A Datadog log pipeline Remapper from %s to the "+
			"env tag would let monitors and dashboards read it too", src.Key(), m.EnvField, share, m.EnvField), false
	}
	return "", false
}

func valuesText(values []EnvValue) string {
	parts := make([]string, 0, len(values))
	for _, v := range values {
		parts = append(parts, fmt.Sprintf("%s (%d lines)", v.Value, v.Lines))
	}
	return strings.Join(parts, ", ")
}

func envFieldList() string {
	parts := make([]string, 0, len(EnvFields))
	for _, c := range EnvFields {
		parts = append(parts, c.Field)
	}
	return strings.Join(parts, ", ")
}
