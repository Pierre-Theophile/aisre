// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// PreparePayload is the GCP pre-pass a live recording runs before the sanitisation table (T187).
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
func PreparePayload(kind string, raw []byte) ([]byte, error) {
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
