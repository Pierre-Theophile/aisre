// SPDX-License-Identifier: Apache-2.0

package budget

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// Quota share (T069, FR-047a, research §11).
//
// The binding constraint on an investigation is usually not the model bill; it is the vendor
// quota the on-call also needs. An agent that spends the team's remaining Datadog quota during
// the incident is worse than an agent that stops. So the share of a backend's *remaining* quota
// is a primary dimension rather than a decoration.
//
// Every digest's coverage block carries `remaining_quota` and `quota_window_seconds` where the
// vendor reports them. The engine reads both, records what it observed, and spends at most its
// configured share of what is left. Where the vendor reports nothing the engine falls back to the
// absolute per-backend budget — **and records that it did**, because "we had no quota signal" and
// "we had plenty of quota" are different things to find in a spend report six months later.

type quotaTracker struct {
	share float64

	// remaining is the most recent remaining-quota figure the backend reported.
	remaining map[string]int64
	// window is the quota window the backend reported.
	window map[string]time.Duration
	// spent is how many calls this investigation has made against the reported quota.
	spent map[string]int64
	// fallback records backends that reported nothing, so the report can say the absolute
	// budget governed rather than the share.
	fallback map[string]bool
	// observed records whether a backend has ever reported a quota at all.
	observed map[string]bool
}

func newQuotaTracker(share float64) *quotaTracker {
	return &quotaTracker{
		share:     share,
		remaining: map[string]int64{},
		window:    map[string]time.Duration{},
		spent:     map[string]int64{},
		fallback:  map[string]bool{},
		observed:  map[string]bool{},
	}
}

// reserve books one call against a backend's quota share at admission time, before the call is
// issued and therefore before its coverage block is known. The figure the call comes back with is
// booked by observe, after the reservation has been released (reservation.go).
func (q *quotaTracker) reserve(backend string) {
	if backend == "" {
		return
	}
	q.spent[backend]++
}

// observe books one call and records what its coverage block said about the quota.
func (q *quotaTracker) observe(call WorkerCall) {
	if call.Backend == "" {
		return
	}
	q.spent[call.Backend]++
	if call.QuotaUndetermined || call.RemainingQuota <= 0 {
		q.fallback[call.Backend] = true
		return
	}
	q.observed[call.Backend] = true
	q.remaining[call.Backend] = call.RemainingQuota
	if call.QuotaWindow > 0 {
		q.window[call.Backend] = call.QuotaWindow
	}
}

// wouldBreach reports whether one more call to this backend would take the investigation past its
// share of the quota the backend last reported remaining.
//
// A backend that has never reported a quota is admitted here and governed by the absolute
// per-backend budget instead; the fallback is recorded rather than assumed.
func (q *quotaTracker) wouldBreach(backend string) (string, bool) {
	if backend == "" || !q.observed[backend] {
		return "", true
	}
	allowance := int64(math.Floor(float64(q.remaining[backend]) * q.share))
	if allowance < 1 {
		allowance = 1 // one call is always allowed against a reported quota, or a low quota is a deadlock
	}
	if q.spent[backend]+1 > allowance {
		window := q.window[backend]
		return fmt.Sprintf(
			"%s reported %d calls of quota remaining over %s; this investigation may spend %.0f%% of that "+
				"(%d calls) and has made %d",
			backend, q.remaining[backend], window, q.share*100, allowance, q.spent[backend]), false
	}
	return "", true
}

// QuotaShareUsed is the share of each backend's reported remaining quota this investigation
// consumed, recorded per backend (FR-047a).
func (m *Manager) QuotaShareUsed() map[string]float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]float64{}
	for backend, remaining := range m.quota.remaining {
		if remaining <= 0 {
			continue
		}
		out[backend] = round6(float64(m.quota.spent[backend]) / float64(remaining))
	}
	return out
}

// RemainingQuotaObserved is the last remaining-quota figure each backend reported.
func (m *Manager) RemainingQuotaObserved() map[string]int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]int64, len(m.quota.remaining))
	for backend, remaining := range m.quota.remaining {
		out[backend] = remaining
	}
	return out
}

// QuotaFallbacks names the backends that reported no quota, so the spend report can say the
// absolute per-backend budget governed rather than the share.
func (m *Manager) QuotaFallbacks() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.quota.fallback))
	for backend, fell := range m.quota.fallback {
		if fell && !m.quota.observed[backend] {
			out = append(out, backend)
		}
	}
	sort.Strings(out)
	return out
}

func round6(v float64) float64 {
	return math.Round(v*1e6) / 1e6
}
