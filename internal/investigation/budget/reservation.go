// SPDX-License-Identifier: Apache-2.0

package budget

// Reserve on admit (T068, FR-030–FR-034, FR-047a).
//
// Admission decides whether a call may be issued. If that decision books nothing, the decision is
// worth nothing the moment two calls are decided at once: both read the same counter, both see
// headroom for one, both are admitted, and a cap of N admits N+1. That is not a theoretical race —
// the first wave issues its candidates' telemetry calls in parallel, and `rollout-regression-01`
// wants seven `expensive` calls against the six the reserve leaves of eight, so the run admitted
// six or seven depending on which goroutine reached the counter first.
//
// So admission books. `Admit` increments the dimensions the call will consume — its cost class,
// its backend, its worker, its share of the backend's reported quota, and for a model call the
// pre-flight token estimate — under the same lock that read them, and hands back a Reservation.
// The call is then issued against a budget that already knows about it, and the next admission
// sees a counter that includes every call in flight. A cap of N therefore admits exactly N, in
// the order the admissions were made.
//
// A reservation is an estimate, so it is reconciled rather than kept: `RecordWorkerCall` releases
// it and books what the call actually cost (a cheaper cost class, a different backend, the window
// it really spanned, the quota the coverage block really reported), and both halves happen under
// one lock so no admission can slip between them. A call that never leaves — a routing failure, a
// transport error — releases its reservation and the headroom returns.

// Reservation is the booking `Admit` made for a call it admitted.
//
// It is a handle, not a value: the counters it stands for live on the Manager, and releasing it
// twice releases it once. The zero handle and the nil handle are both safe to release, so a
// caller may `defer` the release without first asking whether there was one.
type Reservation struct {
	m    *Manager
	kind Kind

	// The worker-call identity as it was reserved. The actual call may differ — a worker that
	// routed elsewhere, a capability whose cost class is cheaper than the one admission priced —
	// which is why reconciliation releases this and books the actual rather than adjusting it.
	worker    string
	backend   string
	costClass string
	// quotaReserved records that one call was booked against the backend's reported quota share.
	quotaReserved bool

	// tokens is the pre-flight estimate a model call was admitted against.
	tokens int64

	// released is set by the first release; every later one is a no-op. Guarded by m.mu.
	released bool
}

// reserveLocked books an admitted call against every dimension it will consume. The caller holds
// m.mu.
func (m *Manager) reserveLocked(req Request) *Reservation {
	r := &Reservation{m: m, kind: req.Kind}
	switch req.Kind {
	case KindModel:
		r.tokens = req.Tokens
		m.pendingTokens += req.Tokens
	case KindWorker:
		r.worker, r.backend, r.costClass = req.Worker, req.Backend, req.CostClass
		incCount(m.callsByWorker, req.Worker)
		incCount(m.callsByBackend, req.Backend)
		incCount(m.callsByCostClass, req.CostClass)
		if req.Backend != "" {
			m.quota.reserve(req.Backend)
			r.quotaReserved = true
		}
	}
	return r
}

// Release returns a reservation's headroom to the budget.
//
// It is what a call that was admitted and then never issued owes the rest of the investigation: a
// worker that could not be routed, a transport that failed, a model request that was never sent.
// Releasing a nil or already-released reservation does nothing.
func (r *Reservation) Release() {
	if r == nil || r.m == nil {
		return
	}
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	r.releaseLocked()
}

// releaseLocked is Release with the lock already held, so that a reconciliation can release the
// estimate and book the actual without opening a window between them.
func (r *Reservation) releaseLocked() {
	if r == nil || r.m == nil || r.released {
		return
	}
	r.released = true
	switch r.kind {
	case KindModel:
		r.m.pendingTokens -= r.tokens
		if r.m.pendingTokens < 0 {
			r.m.pendingTokens = 0
		}
	case KindWorker:
		decCount(r.m.callsByWorker, r.worker)
		decCount(r.m.callsByBackend, r.backend)
		decCount(r.m.callsByCostClass, r.costClass)
		if r.quotaReserved {
			decCount(r.m.quota.spent, r.backend)
		}
	}
}

// incCount books one call against a counter.
func incCount(counts map[string]int64, key string) {
	if key == "" {
		return
	}
	counts[key]++
}

// decCount gives one call back, deleting the row when it reaches zero so that a released
// reservation leaves no trace in a spend report — a worker that made no call is absent from the
// call book rather than present with a zero.
func decCount(counts map[string]int64, key string) {
	if key == "" {
		return
	}
	if counts[key] <= 1 {
		delete(counts, key)
		return
	}
	counts[key]--
}
