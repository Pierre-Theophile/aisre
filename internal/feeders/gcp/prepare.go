// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// PreparePayload is the GCP pre-pass a live recording runs before the sanitisation table (T187).
//
// One field needs it: a poll marker's `reason`. As the poller writes it, it is
// `<operation> <project>/<region>: <error text>; …`, which quotes projects and free text. The table
// cannot pseudonymise inside it, and dropping it is not an option either: a partial checkpoint with no
// reason is refused (FR-012), so a recording that dropped it would replay as an error rather than as
// the partial cycle it was. So the reason is reduced to the operation names that failed, which are the
// published read-only operations and name nothing, and the table keeps that.
//
// Every other kind passes through unchanged.
func PreparePayload(kind string, raw []byte) ([]byte, error) {
	if kind != PayloadPollMarker {
		return raw, nil
	}
	var marker PollMarker
	if err := json.Unmarshal(raw, &marker); err != nil {
		return nil, fmt.Errorf("gcp: decoding a poll marker: %w", err)
	}
	if marker.Reason == "" {
		return raw, nil
	}
	marker.Reason = FailedReads(marker.Reason)
	return json.Marshal(marker)
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
