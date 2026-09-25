// SPDX-License-Identifier: Apache-2.0

package feeder

import (
	"math"
	"time"
)

// Traffic weight classes (research §8, FR-008).
//
// A `calls` edge carries how much traffic flows over it, as a class rather than a rate. The
// class is the whole point: a rate is a measurement and measurements belong in the telemetry
// backend (constitution IV), while "this call path carries roughly ten requests a second" is a
// structural fact about the graph that survives being a week old. Coarseness is also what
// makes sampling tolerable — halve every count and almost nothing changes class.
//
// The boundaries are published and fixed:
//
//	0  observed, but negligible traffic in the window
//	1  < 0.1 rps
//	2  < 1 rps
//	3  < 10 rps
//	4  < 100 rps
//	5  ≥ 100 rps
//
// A new edge version is emitted only when the class changes, and research §8 asks for one full
// window of hysteresis before a change is believed — that part is the feeder's job, since only
// it knows its own window history.

// MaxWeightClass is the highest class, `≥ 100 rps`.
const MaxWeightClass uint32 = 5

// weightClassBounds are the exclusive upper bounds of classes 1 to 4, in order. A rate below
// bounds[i] and at or above the previous bound is class i+1.
var weightClassBounds = [...]float64{0.1, 1, 10, 100}

// WeightClass maps a request rate to its published class.
//
// A rate that is zero, negative or not a number is class 0: "we watched this edge and saw
// effectively nothing", which is a different statement from not emitting the edge at all.
func WeightClass(rps float64) uint32 {
	if math.IsNaN(rps) || rps <= 0 {
		return 0
	}
	for i, bound := range weightClassBounds {
		if rps < bound {
			//nolint:gosec // i is bounded by len(weightClassBounds) = 4
			return uint32(i) + 1
		}
	}
	return MaxWeightClass
}

// WeightClassFromCount maps an observed count over an aggregation window to its class, which is
// the form a feeder actually has: spans are counted per window, never rated.
//
// A window of zero or less has no rate to speak of and yields class 0, as does a count of zero
// or less. Note that the count itself is never stored anywhere — only the class it produced
// (ADR-0001 D1).
func WeightClassFromCount(count int, window time.Duration) uint32 {
	if count <= 0 || window <= 0 {
		return 0
	}
	return WeightClass(float64(count) / window.Seconds())
}

// WeightClassPtr returns a pointer to class, for the optional `weight_class` field of an edge.
// Only `calls` edges carry one; leaving it nil is how every other edge type says "not
// applicable", which is not the same as class 0.
func WeightClassPtr(class uint32) *uint32 { return &class }
