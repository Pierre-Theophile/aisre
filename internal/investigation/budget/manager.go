// SPDX-License-Identifier: Apache-2.0

package budget

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/investigation/model"
)

// The budget manager (T067–T073, FR-043, FR-044, FR-045, FR-047, FR-047a, research §11).
//
// One object holds every dimension, and every call — model or worker — passes through Admit
// before it is issued. That ordering is the whole design: a check made *after* a call has been
// sent is a reserve that a call in flight has already overrun, and the reserve is the thing that
// guarantees an exhausted investigation still writes an answer.
//
// The control law is three sentences and no more (research §11): spend against every dimension,
// hold back exactly 15% for the synthesis, stop when the posterior stops moving. A PID-style
// controller over spend was considered and rejected as cleverness; this is explainable to an
// operator in one breath, which is what makes it operable at 03:00.

// Manager is one investigation's budget.
//
// It is safe for concurrent use because the first wave issues worker calls in parallel and each
// of them must pass admission. Admission and the spend it authorises happen under one lock, so
// two parallel calls cannot both be admitted against the last unit of a budget.
type Manager struct {
	mu sync.Mutex

	profile Profile
	caps    OperatorCaps
	clock   func() time.Time
	started time.Time

	mode             Mode
	reserveEntered   time.Time
	reserveReason    string
	firstTestedAt    time.Time
	substantiveAt    time.Time
	widestWindowSecs int64

	tokens *model.TokenLedger
	// pendingTokens is what admission has reserved for model calls that have been admitted and
	// not yet recorded. A ceiling is read against tokens + pendingTokens, so a call in flight
	// counts against the next admission (reservation.go).
	pendingTokens int64

	callsByWorker    map[string]int64
	callsByBackend   map[string]int64
	callsByCostClass map[string]int64

	quota *quotaTracker
	dr    *diminishing

	// intents are the calls admission refused, kept so each becomes an untested hypothesis's
	// next query with its deep link rather than a silence (FR-047a).
	intents []Intent
}

// New opens a budget for one investigation.
func New(profile Profile, caps OperatorCaps, clock func() time.Time) (*Manager, error) {
	if err := profile.Validate(); err != nil {
		return nil, err
	}
	if err := caps.Validate(); err != nil {
		return nil, err
	}
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	m := &Manager{
		profile:          profile,
		caps:             caps,
		clock:            clock,
		mode:             ModeNormal,
		tokens:           model.NewTokenLedger(),
		callsByWorker:    map[string]int64{},
		callsByBackend:   map[string]int64{},
		callsByCostClass: map[string]int64{},
		quota:            newQuotaTracker(profile.QuotaShare),
		dr:               newDiminishing(DiminishingWindow, DiminishingEpsilon),
	}
	m.started = m.clock().UTC()
	return m, nil
}

// Profile is the profile in force, recorded on every investigation (FR-047).
func (m *Manager) Profile() Profile { return m.profile }

// Caps are the operator caps in force.
func (m *Manager) Caps() OperatorCaps { return m.caps }

// Mode reports whether the engine is still gathering evidence.
func (m *Manager) Mode() Mode {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mode
}

// Elapsed is the wall time this investigation has used.
func (m *Manager) Elapsed() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.elapsedLocked()
}

func (m *Manager) elapsedLocked() time.Duration { return m.clock().UTC().Sub(m.started) }

// StartedAt is when the budget was opened.
func (m *Manager) StartedAt() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.started
}

// EnterSynthesisOnly switches to the reserve, recording why. It is idempotent and there is no
// path back: the first reason is the one kept, because the first dimension to run out is the one
// that bound.
func (m *Manager) EnterSynthesisOnly(reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.enterReserveLocked(reason)
}

func (m *Manager) enterReserveLocked(reason string) {
	if m.mode == ModeSynthesisOnly {
		return
	}
	m.mode = ModeSynthesisOnly
	m.reserveReason = reason
	m.reserveEntered = m.clock().UTC()
}

// ReserveEnteredAt is when the reserve was entered, zero if it was not.
func (m *Manager) ReserveEnteredAt() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reserveEntered
}

// ReserveReason is the dimension that ran out, empty if the reserve was not entered.
func (m *Manager) ReserveReason() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reserveReason
}

// MarkFirstTested records the instant the first hypothesis reached `supported` or `refuted`,
// which is what the profile's first-tested target is measured against (SC-013).
func (m *Manager) MarkFirstTested() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.firstTestedAt.IsZero() {
		m.firstTestedAt = m.clock().UTC()
	}
}

// MarkSubstantive records the instant a substantive answer existed: a tested hypothesis plus a
// verdict line that passed the citation checker (FR-047).
func (m *Manager) MarkSubstantive() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.substantiveAt.IsZero() {
		m.substantiveAt = m.clock().UTC()
	}
}

// FirstTestedAfter and SubstantiveAfter are how long each took, zero when it has not happened.
func (m *Manager) FirstTestedAfter() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.firstTestedAt.IsZero() {
		return 0
	}
	return m.firstTestedAt.Sub(m.started)
}

// SubstantiveAfter is how long the substantive answer took, zero when there was none.
func (m *Manager) SubstantiveAfter() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.substantiveAt.IsZero() {
		return 0
	}
	return m.substantiveAt.Sub(m.started)
}

// RecordModelCall books a model call's tokens against the cost-unit budget.
func (m *Manager) RecordModelCall(modelID string, usage model.Usage) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tokens.Record(modelID, usage)
}

// WorkerCall is one worker call as it was actually issued, for the books.
type WorkerCall struct {
	// Worker and Backend name who was called and which source of truth answered.
	Worker  string
	Backend string
	// CostClass is the class the capability declared.
	CostClass string
	// WindowWidth is the width the query actually spanned.
	WindowWidth time.Duration
	// RemainingQuota and QuotaWindow are what the backend's coverage block reported. A backend
	// that reports nothing sets QuotaUndetermined, and the fallback to the absolute per-backend
	// budget is recorded (FR-047a).
	RemainingQuota    int64
	QuotaWindow       time.Duration
	QuotaUndetermined bool

	// Reservation is the handle Admit returned when it booked this call. Recording reconciles it:
	// the estimate is released and the actual is booked, both under one lock. A call recorded
	// without one is booked outright, which is what a caller that never passed admission means.
	Reservation *Reservation
}

// RecordWorkerCall reconciles a call's reservation with what the call actually cost, and books it
// against every dimension it touches.
//
// Release and book happen under one lock so that no admission can observe the moment between
// them, in which the budget would have forgotten a call that had already been made.
func (m *Manager) RecordWorkerCall(call WorkerCall) {
	m.mu.Lock()
	defer m.mu.Unlock()
	call.Reservation.releaseLocked()
	incCount(m.callsByWorker, call.Worker)
	incCount(m.callsByBackend, call.Backend)
	incCount(m.callsByCostClass, call.CostClass)
	if secs := int64(call.WindowWidth / time.Second); secs > m.widestWindowSecs {
		m.widestWindowSecs = secs
	}
	m.quota.observe(call)
}

// Tokens is the accumulated token ledger, the provider-independent unit (FR-048).
func (m *Manager) Tokens() *model.TokenLedger {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tokens
}

// CallsByWorker, CallsByBackend and CallsByCostClass are the call books.
func (m *Manager) CallsByWorker() map[string]int64 { return m.copyCounts(m.callsByWorker) }

// CallsByBackend is the per-backend call book.
func (m *Manager) CallsByBackend() map[string]int64 { return m.copyCounts(m.callsByBackend) }

// CallsByCostClass is the per-cost-class call book.
func (m *Manager) CallsByCostClass() map[string]int64 { return m.copyCounts(m.callsByCostClass) }

func (m *Manager) copyCounts(in map[string]int64) map[string]int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]int64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// Observe hands the ledger's posterior vector to the diminishing-returns test, once per worker
// call. It returns true when the stop should fire (FR-045a).
func (m *Manager) Observe(posterior map[string]float64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.dr.observe(posterior)
}

// DiminishingDetail renders the threshold and the window the stop was measured over, for the
// typed stop reason's detail.
func (m *Manager) DiminishingDetail() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.dr.detail()
}

// Line is one budget's remaining headroom, in the shape the ledger render takes.
type Line struct {
	// Name is the budget: "wall_time", "model_tokens", "worker_calls:metrics", …
	Name string
	// Remaining and Limit are in Unit.
	Remaining float64
	Limit     float64
	// Unit is "s", "tokens" or "calls".
	Unit string
}

// Remaining returns every budget's headroom, in a fixed order, for the turn rendering (FR-045).
//
// The numbers are against the *unreserved* portion while the engine is in normal mode, because
// that is the headroom the model actually has: showing it the full budget would show it 15% it
// is not allowed to spend on evidence.
func (m *Manager) Remaining() []Line {
	m.mu.Lock()
	defer m.mu.Unlock()

	reserve := m.mode == ModeNormal
	out := []Line{}
	if limit := m.wallTimeLimitLocked(); limit > 0 {
		ceiling := float64(limit / time.Second)
		if reserve {
			ceiling = unreserved(ceiling)
		}
		out = append(out, Line{
			Name:      "wall_time",
			Remaining: maxZero(ceiling - m.elapsedLocked().Seconds()),
			Limit:     ceiling,
			Unit:      "s",
		})
	}
	tokenCeiling := m.costUnitsLocked()
	if reserve {
		tokenCeiling = unreserved(tokenCeiling)
	}
	out = append(out, Line{
		Name:      "model_tokens",
		Remaining: maxZero(tokenCeiling - float64(m.tokens.Total().Total())),
		Limit:     tokenCeiling,
		Unit:      "tokens",
	})

	classes := append([]string(nil), Classes...)
	for _, class := range classes {
		limit := m.classLimitLocked(class)
		ceiling := float64(limit)
		if reserve {
			ceiling = float64(unreservedCalls(limit))
		}
		out = append(out, Line{
			Name:      "worker_calls:" + class,
			Remaining: maxZero(ceiling - float64(m.callsByCostClass[class])),
			Limit:     ceiling,
			Unit:      "calls",
		})
	}

	backends := make([]string, 0, len(m.callsByBackend))
	for backend := range m.callsByBackend {
		backends = append(backends, backend)
	}
	sort.Strings(backends)
	for _, backend := range backends {
		limit := m.backendLimitLocked(backend)
		ceiling := float64(limit)
		if reserve {
			ceiling = float64(unreservedCalls(limit))
		}
		out = append(out, Line{
			Name:      "backend_calls:" + backend,
			Remaining: maxZero(ceiling - float64(m.callsByBackend[backend])),
			Limit:     ceiling,
			Unit:      "calls",
		})
	}
	return out
}

// StopConditions renders the stop conditions armed, one sentence each, for the turn rendering.
func (m *Manager) StopConditions() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []string{
		fmt.Sprintf("reserve: %.0f%% of every budget is held back for the closing synthesis; "+
			"on exhaustion of the rest the engine stops gathering evidence and writes what it has",
			ReserveFraction*100),
		m.dr.condition(),
	}
	if limit := m.wallTimeLimitLocked(); limit > 0 {
		out = append(out, fmt.Sprintf("hard stop at %s wall time (profile %s)", limit, m.profile.Name))
	}
	if m.mode == ModeSynthesisOnly {
		out = append(out, "synthesis_only: "+m.reserveReason+"; no further evidence will be gathered")
	}
	return out
}

// String renders the manager's state in one line, for a log.
func (m *Manager) String() string {
	lines := m.Remaining()
	parts := make([]string, 0, len(lines))
	for _, line := range lines {
		parts = append(parts, fmt.Sprintf("%s %.0f/%.0f%s", line.Name, line.Remaining, line.Limit, unitSuffix(line.Unit)))
	}
	return string(m.Mode()) + " · " + strings.Join(parts, " · ")
}

func unitSuffix(unit string) string {
	if unit == "" {
		return ""
	}
	return " " + unit
}

func maxZero(v float64) float64 {
	if v < 0 {
		return 0
	}
	return v
}

// The effective limits: the tighter of the profile's and the operator's, because an operator cap
// is a cap and never a licence to spend more than the profile allows.
func (m *Manager) wallTimeLimitLocked() time.Duration {
	return tighterDuration(m.profile.WallTime, m.caps.WallTime)
}

func (m *Manager) costUnitsLocked() float64 {
	return tighterFloat(m.profile.CostUnits, m.caps.CostUnits)
}

func (m *Manager) maxWindowLocked() time.Duration {
	return tighterDuration(m.profile.MaxWindow, m.caps.MaxWindow)
}

func (m *Manager) classLimitLocked(class string) int64 {
	return tighterInt(m.profile.CallsPerCostClass[class], m.caps.CallsPerCostClass[class])
}

func (m *Manager) backendLimitLocked(backend string) int64 {
	profile := m.profile.CallsForBackend(backend)
	cap, ok := m.caps.CallsPerBackend[backend]
	if !ok {
		cap = m.caps.CallsPerBackend[AnyBackend]
	}
	return tighterInt(profile, cap)
}

func tighterDuration(a, b time.Duration) time.Duration {
	switch {
	case a <= 0:
		return b
	case b <= 0:
		return a
	case b < a:
		return b
	default:
		return a
	}
}

func tighterFloat(a, b float64) float64 {
	switch {
	case a <= 0:
		return b
	case b <= 0:
		return a
	case b < a:
		return b
	default:
		return a
	}
}

func tighterInt(a, b int64) int64 {
	switch {
	case a <= 0:
		return b
	case b <= 0:
		return a
	case b < a:
		return b
	default:
		return a
	}
}
