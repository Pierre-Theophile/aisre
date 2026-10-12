// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/Pierre-Theophile/aisre/internal/sanitise"
)

// preparePayload is the half of the GCP pre-pass that needs no sanitiser, which a live recording runs before the sanitisation table (T187).
//
// Two fields need it, because the table can only keep, pseudonymise or drop a field whole.
//
// A poll marker's `reason`. As the poller writes it, it is `<operation> <project>/<region>: <error
// text>; …`, which quotes projects and free text. Dropping it is not an option: a partial checkpoint
// with no reason is refused (FR-012), so a recording that dropped it would replay as an error rather
// than as the partial cycle it was. So it is reduced to the operation names that failed, which are the
// published read-only operations and name nothing, and the table keeps that.
//
// A Cloud Run environment variable's `value`. The `env_var_value` derivation asserts a service's
// dependency on a Cloud SQL instance from a value that IS a connection name — Google's own connector
// libraries are configured that way, through `INSTANCE_CONNECTION_NAME`, with no volume — and a
// recording that dropped every value would lose those services' database dependency outright. So each
// value is reduced to the one connection name the feeder itself reads from it, by the feeder's own rule
// (ConnectionNameIn), and removed when there is none. Nothing else of the value survives: not the rest
// of a DSN, not a password next to the socket path, not a hash. The table then pseudonymises the
// connection name exactly as it does the `cloudSqlInstance` volume (FR-034 as amended, FR-135).
//
// Every other kind passes through unchanged.
func preparePayload(kind string, raw []byte) ([]byte, error) {
	switch kind {
	case PayloadPollMarker:
		var marker PollMarker
		if err := json.Unmarshal(raw, &marker); err != nil {
			return nil, fmt.Errorf("gcp: decoding a poll marker: %w", err)
		}
		if marker.Reason == "" {
			return raw, nil
		}
		marker.Reason = FailedReads(marker.Reason)
		return json.Marshal(marker)
	case PayloadServices, PayloadRevisions:
		return reduceEnvValues(kind, raw)
	default:
		return raw, nil
	}
}

// reduceEnvValues rewrites every `env[].value` in a services or revisions payload to the connection
// name it contains, or removes it.
func reduceEnvValues(kind string, raw []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber() // a port or a count must come out as the number it went in as
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		return nil, fmt.Errorf("gcp: decoding a %s payload: %w", kind, err)
	}
	items, _ := payload[kind].([]any)
	for _, item := range items {
		object, _ := item.(map[string]any)
		containers := object["containers"] // a revision
		if template, ok := object["template"].(map[string]any); ok {
			containers = template["containers"] // a service
		}
		list, _ := containers.([]any)
		for _, container := range list {
			c, _ := container.(map[string]any)
			env, _ := c["env"].([]any)
			for _, entry := range env {
				e, _ := entry.(map[string]any)
				value, ok := e["value"].(string)
				if !ok {
					continue
				}
				if name, found := ConnectionNameIn(value); found {
					e["value"] = name
				} else {
					delete(e, "value")
				}
			}
		}
	}
	return json.Marshal(payload)
}

// FailedReads reduces a poller reason to the sorted, distinct leading words of its parts: the
// operation each failed read issued.
func FailedReads(reason string) string {
	var ops []string
	for _, part := range strings.Split(reason, "; ") {
		if fields := strings.Fields(part); len(fields) > 0 {
			ops = append(ops, strings.TrimSuffix(fields[0], ":"))
		}
	}
	slices.Sort(ops)
	ops = slices.Compact(ops)
	return "failed reads: " + strings.Join(ops, ", ")
}

// RecordingPrepare is the pre-pass `feed gcp --record` installs: preparePayload, and for alert
// policies the filter reduction, which needs the recording's sanitiser.
//
// A recorded alert policy's condition filters are what an investigation starts from: the watched
// service, revision or Cloud SQL instance comes out of their equality clauses (watchesFromFilter),
// and a policy that watches nothing makes a replayed investigation conclude unresolved without
// running the engine. So the filters are not dropped. Each is reduced to the equality clauses on the
// fields below — the ones the feeder reads, and closed platform vocabularies — with every identifier
// pseudonymised as the table pseudonymises the same identifier elsewhere, so a recorded watch still
// names the recorded service. Every other clause is discarded: `!=`, substrings, regexes, OR, and
// above all `metric.labels.*`, which can carry a customer's id. A filter with nothing left is removed.
func RecordingPrepare(san *sanitise.Sanitiser) func(kind string, raw []byte) ([]byte, error) {
	return func(kind string, raw []byte) ([]byte, error) {
		if kind == PayloadAlertPolicies {
			return reduceAlertFilters(san, raw)
		}
		return preparePayload(kind, raw)
	}
}

// recordedFilterFields are the clauses a reduced filter may keep, and how each value is recorded.
var recordedFilterFields = map[string]func(*sanitise.Sanitiser, string) (string, bool, error){
	"resource.type": closedValue(`^[a-z][a-z0-9_]*$`),
	"metric.type":   metricType,
	"resource.labels.project_id": func(s *sanitise.Sanitiser, v string) (string, bool, error) {
		out, err := s.LabelIdentifier(sanitise.KindProject, v)
		return out, err == nil, err
	},
	"resource.labels.service_name":  tokenOf(sanitise.KindService),
	"resource.labels.revision_name": tokenOf(sanitise.KindRevision),
	"resource.labels.location":      closedValue(`^[a-z]+(-[a-z]+[0-9]+(-[a-z])?)?$`),
	"resource.labels.region":        closedValue(`^[a-z]+(-[a-z]+[0-9]+(-[a-z])?)?$`),
	"resource.labels.database_id": func(s *sanitise.Sanitiser, v string) (string, bool, error) {
		project, instance, ok := strings.Cut(v, ":")
		if !ok || strings.Contains(instance, ":") {
			return "", false, nil
		}
		p, err := s.LabelIdentifier(sanitise.KindProject, project)
		if err != nil {
			return "", false, err
		}
		i, err := s.LabelIdentifier(sanitise.KindInstance, instance)
		return p + ":" + i, err == nil, err
	},
	"metric.labels.response_code_class": closedValue(`^[0-9a-z]+$`),
	// An uptime check's id names a check an operator created. Both spellings occur in real policies.
	"metric.labels.check_id": tokenOf(sanitise.KindResource),
	"metric.label.check_id":  tokenOf(sanitise.KindResource),
}

// googleMetric is a metric type Google defines: `<service>.googleapis.com/<path>`.
var googleMetric = regexp.MustCompile(`^[a-z]+\.googleapis\.com/[a-z0-9_/.]+$`)

// metricType keeps a Google-defined metric type and pseudonymises the operator-chosen half of one an
// organisation defined: a log-based metric (`logging.googleapis.com/user/<name>`) or a custom one
// (`custom.googleapis.com/<name>`, `external.googleapis.com/<name>`). The prefix stays, because it says
// which kind of metric the condition watches; the name goes, because the operator chose it.
func metricType(s *sanitise.Sanitiser, v string) (string, bool, error) {
	for _, prefix := range []string{"logging.googleapis.com/user/", "custom.googleapis.com/", "external.googleapis.com/",
		"workload.googleapis.com/", "prometheus.googleapis.com/"} {
		if name, ok := strings.CutPrefix(v, prefix); ok {
			out, err := s.Identifier(sanitise.KindResource, name)
			return prefix + out, err == nil && name != "", err
		}
	}
	return v, googleMetric.MatchString(v), nil
}

func closedValue(pattern string) func(*sanitise.Sanitiser, string) (string, bool, error) {
	re := regexp.MustCompile(pattern)
	return func(_ *sanitise.Sanitiser, v string) (string, bool, error) { return v, re.MatchString(v), nil }
}

func tokenOf(kind sanitise.Kind) func(*sanitise.Sanitiser, string) (string, bool, error) {
	return func(s *sanitise.Sanitiser, v string) (string, bool, error) {
		out, err := s.Identifier(kind, v)
		return out, err == nil && out != "", err
	}
}

// ReducedFilter is one filter after the reduction.
//
// A filter with no clause to keep — a log condition whose filter is free text, the commonest case —
// becomes one opaque token of the whole filter. The condition keeps its pointer, so the policy still
// says it can be executed against something, the same filter still records as the same token, and
// nothing of the text survives. It watched nothing live either: watches come from equality clauses.
func ReducedFilter(san *sanitise.Sanitiser, filter string) (string, error) {
	reduced, err := reducedClauses(san, filter)
	if err != nil || reduced != "" || strings.TrimSpace(filter) == "" {
		return reduced, err
	}
	return san.Identifier(sanitise.KindResource, filter)
}

func reducedClauses(san *sanitise.Sanitiser, filter string) (string, error) {
	var clauses []string
	for _, match := range equalityClause.FindAllStringSubmatch(filter, -1) {
		record, listed := recordedFilterFields[match[1]]
		if !listed {
			continue
		}
		value, ok, err := record(san, match[2])
		if err != nil {
			return "", err
		}
		if ok {
			clauses = append(clauses, match[1]+`="`+value+`"`)
		}
	}
	return strings.Join(clauses, " AND "), nil
}

// reduceAlertFilters rewrites every condition filter in an alert_policies payload.
func reduceAlertFilters(san *sanitise.Sanitiser, raw []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		return nil, fmt.Errorf("gcp: decoding an %s payload: %w", PayloadAlertPolicies, err)
	}
	policies, _ := payload["alertPolicies"].([]any)
	for _, policy := range policies {
		p, _ := policy.(map[string]any)
		conditions, _ := p["conditions"].([]any)
		for _, condition := range conditions {
			c, _ := condition.(map[string]any)
			for _, block := range []string{"conditionThreshold", "conditionMatchedLog", "conditionAbsent"} {
				b, _ := c[block].(map[string]any)
				for _, field := range []string{"filter", "denominatorFilter"} {
					filter, ok := b[field].(string)
					if !ok {
						continue
					}
					reduced, err := ReducedFilter(san, filter)
					if err != nil {
						return nil, err
					}
					if reduced == "" {
						delete(b, field)
					} else {
						b[field] = reduced
					}
				}
			}
		}
	}
	return json.Marshal(payload)
}
