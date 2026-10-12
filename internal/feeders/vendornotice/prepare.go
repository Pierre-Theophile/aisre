// SPDX-License-Identifier: Apache-2.0

package vendornotice

import (
	"encoding/json"
	"fmt"
)

// RecordedGapReason is what a cycle gap's `why` becomes in a recording.
const RecordedGapReason = "the source could not be read; the error text is not recorded"

// PreparePayload is the pre-pass a live recording runs before the sanitisation table (T188).
//
// A cycle marker's gap reason is an error message, and an error from a mailbox or a fetch can quote
// the mailbox, a host or a credential's name. The gap itself must survive — a gap is what says a
// source was unreachable rather than quiet (FR-078) — so its reason is replaced rather than dropped.
// Every other payload passes through: announcements are typed fields the extractor produced, with no
// body to remove.
func PreparePayload(kind string, raw []byte) ([]byte, error) {
	if kind != PayloadCycleMarker {
		return raw, nil
	}
	var marker cycleMarker
	if err := json.Unmarshal(raw, &marker); err != nil {
		return nil, fmt.Errorf("vendornotice: decoding a cycle marker: %w", err)
	}
	if len(marker.Gaps) == 0 {
		return raw, nil
	}
	for i := range marker.Gaps {
		marker.Gaps[i].Why = RecordedGapReason
	}
	return json.Marshal(marker)
}
