// SPDX-License-Identifier: Apache-2.0

package datadog

import (
	"fmt"
	"sort"
	"strings"
)

// The `datadog-logs/v1` selector subset (contract pointer-vocabularies.md §1).
//
// A selector is executed as Datadog's search query, so the subset is checked here before it is: the
// feeder mints `service:<v> env:<v>` and optionally `index:<v>` and a few exact tag or attribute terms,
// and anything else — free text, a wildcard on service, a boolean operator — is refused rather than
// executed. A backend that ran whatever it was handed would be a query proxy (FR-040a), and a
// wildcard on service is the widest query a caller could smuggle in.

// selector is a parsed log selector.
type selector struct {
	Service, Env string
	// EnvKey is the field the environment was pinned with: `env` (the tag) or an attribute such as
	// `@env`, for logs that carry the environment as a JSON field (005).
	EnvKey string
	Index  string
	// Terms are the remaining exact `key:value` terms, sorted, spelled as the grammar spells them.
	Terms []string
}

// parseSelector reads and checks a `datadog-logs/v1` selector.
func parseSelector(raw string) (selector, error) {
	var s selector
	for _, token := range strings.Fields(raw) {
		key, value, ok := strings.Cut(token, ":")
		switch {
		case !ok || key == "" || value == "":
			return selector{}, fmt.Errorf("datadog: %q is not a `key:value` term; free text is not in "+
				"the published selector subset", token)
		case strings.ContainsAny(value, "*?()\"") || strings.ContainsAny(key, "*?()\""):
			return selector{}, fmt.Errorf("datadog: %q uses a wildcard, a group or a quoted phrase, "+
				"which the published selector subset does not admit", token)
		case strings.EqualFold(key, "AND") || strings.EqualFold(key, "OR") || strings.HasPrefix(key, "-"):
			return selector{}, fmt.Errorf("datadog: %q is a boolean operator; the subset is a "+
				"conjunction of exact terms", token)
		}
		switch key {
		case "service":
			s.Service = value
		case "env", "@env", "@environment", "@deployment.environment.name":
			// The published environment fields (the feeder's envfield.go): the tag, or the attribute the
			// feeder discovered the environment in.
			s.Env, s.EnvKey = value, key
		case "index":
			s.Index = value
		default:
			s.Terms = append(s.Terms, key+":"+value)
		}
	}
	if s.Service == "" {
		return selector{}, fmt.Errorf("datadog: a log selector must pin the service; %q does not, and a "+
			"query over every service is not what the feeder minted", raw)
	}
	// No env term is the feeder's pointer for a service whose logs carry no environment (005): the
	// selector is exactly what it minted, and the backend searches what the logs actually carry.
	sort.Strings(s.Terms)
	return s, nil
}

// query renders the selector as the search query sent, deterministically, with extra clauses appended
// — e.g. the error-level clause.
func (s selector) query(extra ...string) string {
	parts := []string{"service:" + s.Service}
	if s.Env != "" {
		parts = append(parts, s.EnvKey+":"+s.Env)
	}
	parts = append(parts, s.Terms...)
	parts = append(parts, extra...)
	return strings.Join(parts, " ")
}

// indexes returns the index list a request names: the selector's own, else the backend's configured.
func (s selector) indexes(configured []string) []string {
	if s.Index != "" {
		return []string{s.Index}
	}
	return configured
}

// entity names what was searched: `<service>@<env>`, or the service alone when the logs carry no env.
func (s selector) entity() string {
	if s.Env == "" {
		return s.Service
	}
	return s.Service + "@" + s.Env
}
