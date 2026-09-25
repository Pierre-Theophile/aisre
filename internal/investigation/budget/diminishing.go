// SPDX-License-Identifier: Apache-2.0

package budget

import (
	"fmt"
	"math"
	"sort"
)

// The diminishing-returns stop (T071, FR-045a, research §11).
//
// Fire when ‖p_t − p_{t−n}‖₁ < ε over the ledger's posterior vector across the last n worker
// calls, with published initial values n = 5 and ε = 0.02.
//
// Two details that are easy to get wrong and both matter:
//
//   - The comparison is against the vector n calls ago, not against the previous one. A
//     posterior that moves a little on every call is still moving; one that has moved less than ε
//     in total over five calls has stopped. Comparing consecutive vectors would fire on the first
//     cheap call that told us nothing.
//   - The L1 norm is taken over the **union** of hypothesis ids. A hypothesis that appeared
//     since contributes its whole mass to the distance, which is correct: the belief state
//     changed shape, so it changed.
//
// The values are published as initial ones, to be calibrated with the corpus in hand (research
// §20). They live here rather than in a profile because they describe the control law, not an
// operator's appetite.

// The published diminishing-returns constants.
const (
	// DiminishingWindow is n: how many worker calls back the comparison reaches.
	DiminishingWindow = 5
	// DiminishingEpsilon is ε: the L1 distance below which the posterior has stopped moving.
	DiminishingEpsilon = 0.02
)

type diminishing struct {
	window  int
	epsilon float64

	history  []map[string]float64
	lastDist float64
	fired    bool
}

func newDiminishing(window int, epsilon float64) *diminishing {
	if window < 1 {
		window = DiminishingWindow
	}
	if epsilon <= 0 {
		epsilon = DiminishingEpsilon
	}
	return &diminishing{window: window, epsilon: epsilon, lastDist: math.NaN()}
}

// observe records one posterior vector and reports whether the stop fires.
//
// It never fires before n+1 observations exist: a run that has made fewer than n calls has not
// yet had the chance to stop moving, and firing then would stop every investigation at its fifth
// call.
func (d *diminishing) observe(posterior map[string]float64) bool {
	snapshot := make(map[string]float64, len(posterior))
	for id, value := range posterior {
		snapshot[id] = value
	}
	d.history = append(d.history, snapshot)
	if len(d.history) <= d.window {
		return false
	}
	earlier := d.history[len(d.history)-1-d.window]
	d.lastDist = l1(earlier, snapshot)
	if d.lastDist < d.epsilon {
		d.fired = true
		return true
	}
	return false
}

// detail renders the stop's detail: the threshold and the window it was measured over.
func (d *diminishing) detail() string {
	if math.IsNaN(d.lastDist) {
		return fmt.Sprintf("the posterior was compared over fewer than %d worker calls", d.window)
	}
	return fmt.Sprintf("‖p_t − p_(t−%d)‖₁ = %.6f, below the published ε = %.2f over the last %d worker calls",
		d.window, d.lastDist, d.epsilon, d.window)
}

// condition renders the armed stop condition for the turn rendering.
func (d *diminishing) condition() string {
	return fmt.Sprintf("diminishing returns: the run stops when the ledger's posterior moves less than "+
		"%.2f in L1 over %d worker calls", d.epsilon, d.window)
}

// l1 is the L1 distance over the union of the two vectors' hypothesis ids.
func l1(a, b map[string]float64) float64 {
	ids := map[string]struct{}{}
	for id := range a {
		ids[id] = struct{}{}
	}
	for id := range b {
		ids[id] = struct{}{}
	}
	keys := make([]string, 0, len(ids))
	for id := range ids {
		keys = append(keys, id)
	}
	sort.Strings(keys) // summation order is fixed so the number is the same on every machine

	var total float64
	for _, id := range keys {
		total += math.Abs(a[id] - b[id])
	}
	return round6(total)
}
