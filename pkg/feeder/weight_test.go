// SPDX-License-Identifier: Apache-2.0

package feeder_test

import (
	"math"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

func TestWeightClassBoundaries(t *testing.T) {
	t.Parallel()
	// research §8: 0 negligible, 1 <0.1, 2 <1, 3 <10, 4 <100, 5 >=100. Each boundary is
	// tested from both sides, because an off-by-one here silently reclassifies every edge in
	// the graph.
	tests := []struct {
		rps  float64
		want uint32
	}{
		{math.NaN(), 0},
		{-1, 0},
		{0, 0},
		{0.0001, 1},
		{0.099999, 1},
		{0.1, 2},
		{0.999999, 2},
		{1, 3},
		{9.999999, 3},
		{10, 4},
		{99.999999, 4},
		{100, 5},
		{100000, 5},
		{math.Inf(1), 5},
	}
	for _, tc := range tests {
		if got := feeder.WeightClass(tc.rps); got != tc.want {
			t.Errorf("WeightClass(%v) = %d, want %d", tc.rps, got, tc.want)
		}
	}
}

func TestWeightClassFromCount(t *testing.T) {
	t.Parallel()
	const window = 5 * time.Minute // the default aggregation window, research §11

	tests := []struct {
		name   string
		count  int
		window time.Duration
		want   uint32
	}{
		{name: "nothing observed", count: 0, window: window, want: 0},
		{name: "a negative count cannot happen but must not panic", count: -3, window: window, want: 0},
		{name: "a window of zero has no rate", count: 100, window: 0, want: 0},
		{name: "one call in five minutes is under 0.1 rps", count: 1, window: window, want: 1},
		{name: "thirty calls in five minutes is 0.1 rps", count: 30, window: window, want: 2},
		{name: "three hundred in five minutes is 1 rps", count: 300, window: window, want: 3},
		{name: "three thousand in five minutes is 10 rps", count: 3000, window: window, want: 4},
		{name: "thirty thousand in five minutes is 100 rps", count: 30000, window: window, want: 5},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := feeder.WeightClassFromCount(tc.count, tc.window); got != tc.want {
				t.Errorf("WeightClassFromCount(%d, %s) = %d, want %d", tc.count, tc.window, got, tc.want)
			}
		})
	}
}

func TestWeightClassPtr(t *testing.T) {
	t.Parallel()
	p := feeder.WeightClassPtr(feeder.WeightClass(42))
	if p == nil || *p != 4 {
		t.Fatalf("WeightClassPtr(WeightClass(42)) = %v, want a pointer to 4", p)
	}
}
