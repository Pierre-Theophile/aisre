// SPDX-License-Identifier: Apache-2.0

package feeder

import (
	"strconv"
	"strings"
)

// measurementSuffixes are the units a label or tag value carries when it is a measurement. They are
// matched only after a leading number, so a service called `s3-proxy` is not a measurement.
var measurementSuffixes = []string{
	"ms", "us", "ns", "s", "m", "h", "d",
	"%", "b", "kb", "mb", "gb", "tb", "ki", "mi", "gi", "ti",
	"cpu", "core", "cores", "req", "rps", "qps",
}

// IsMeasurement reports whether a label value measures something rather than naming it (003 FR-126, 005 FR-067).
//
// The test is shape, not a list of keys, and that is the point: the next label nobody thought
// about will be `max-latency-target` rather than `replicas`, and a key list would miss it while a
// value shape catches it. A bare number, a number with a unit, or a percentage is a measurement.
func IsMeasurement(value string) bool {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return false
	}
	// A bare number: `8`, `2.5`, `-1`.
	if _, err := strconv.ParseFloat(trimmed, 64); err == nil {
		return true
	}
	// A number with a unit: `250ms`, `2Gi`, `99.9%`. Split at the first character that cannot be
	// part of a number, and require the whole remainder to be a known unit — so `v2` (a number
	// with no leading digits) and `8-east` (a number with an unrecognised remainder) are not
	// measurements, and `8x-large` is not either.
	digits := 0
	for digits < len(trimmed) && (trimmed[digits] >= '0' && trimmed[digits] <= '9' || trimmed[digits] == '.') {
		digits++
	}
	if digits == 0 {
		return false
	}
	unit := strings.ToLower(trimmed[digits:])
	for _, suffix := range measurementSuffixes {
		if unit == suffix {
			return true
		}
	}
	return false
}
