// SPDX-License-Identifier: Apache-2.0

package budget

import (
	"fmt"
	"time"
)

// Admission (T068, T069, T070, FR-043, FR-047a).
//
// Every model call and every worker call passes admission **before it is issued**. Not "is
// checked after"; not "is checked unless the caller is in a hurry". The reserve exists to
// guarantee that an exhausted investigation still writes an answer, and a reserve can be
// overrun by exactly one thing: a call that was already in flight when the check ran.
//
// So the check is pre-flight and it is pessimistic. A model call is admitted against its
// `count_tokens` pre-flight figure plus the output it is allowed to generate; a worker call
// against the cost class its capability declared and the width of the window it asks for. Both
// are known before the call leaves.
//
// A call that would breach a budget is **never issued**, and the refusal is not a silence: the
// intent is recorded with the hypothesis it would have served and the deep link a person can
// follow, and the hypothesis it would have tested is reported untested with that query attached
// (FR-031, FR-047a).

// Kind is what kind of call is asking to be admitted.
type Kind string

// The two kinds.
const (
	// KindModel is a call to the model.
	KindModel Kind = "model"
	// KindWorker is a call to a worker.
	KindWorker Kind = "worker"
)

// Request is one call asking to be admitted.
type Request struct {
	// Kind is model or worker.
	Kind Kind

	// ModelID and Tokens describe a model call: the model, and what `count_tokens` said the
	// request would cost plus the output it may generate.
	ModelID string
	Tokens  int64

	// Worker, Backend, CostClass and WindowWidth describe a worker call.
	Worker      string
	Backend     string
	CostClass   string
	WindowWidth time.Duration

	// ServesHypothesisID, Question and DeepLink are what a refusal records, so a refused call
	// becomes an untested hypothesis's next query rather than a gap.
	ServesHypothesisID string
	Question           string
	DeepLink           string

	// Synthesis marks a call that belongs to the closing synthesis — the verifier pass, the
	// render — and may therefore spend the reserve.
	Synthesis bool
}

// Decision is admission's answer.
type Decision struct {
	// Admitted says whether the call may be issued.
	Admitted bool
	// Budget names the dimension that refused, empty when admitted.
	Budget string
	// Reason is the sentence a person reads, naming the numbers.
	Reason string
	// EnteredReserve says this decision moved the engine into synthesis_only.
	EnteredReserve bool
	// Reservation is the booking an admitted call carries. It is nil on a refusal. The caller
	// hands it back to RecordWorkerCall, which reconciles it with what the call actually cost, or
	// releases it if the call is never issued.
	Reservation *Reservation
}

// Intent is a call admission refused, kept so it can be attached to the hypothesis it would have
// tested (FR-047a).
type Intent struct {
	// HypothesisID is what the call would have served.
	HypothesisID string
	// Question is the discriminating question it would have answered.
	Question string
	// DeepLink is the link a person can follow to ask it themselves.
	DeepLink string
	// Budget and Reason say why it was not issued.
	Budget string
	Reason string
	// At is when it was refused.
	At time.Time
}

// Intents returns every refused call, in the order they were refused.
func (m *Manager) Intents() []Intent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Intent(nil), m.intents...)
}

// Admit decides whether a call may be issued and **books it**, returning the reservation the
// caller reconciles once the call has been made.
//
// Deciding without booking is the check-then-act race the first wave exercises every run: two
// parallel admissions read the same counter, both see room for one more, and a cap of N admits
// N+1. The decision and the booking therefore happen under one lock, so the second of two
// concurrent admissions sees the first one's call already counted (reservation.go).
//
// A refusal in normal mode moves the engine into `synthesis_only` when the dimension that
// refused is one the reserve protects, which is every dimension. That is the transition, and it
// happens here rather than in a separate sweep so that the moment the budget binds is the moment
// the engine changes behaviour.
func (m *Manager) Admit(req Request) Decision {
	m.mu.Lock()
	defer m.mu.Unlock()

	decision := m.decide(req)
	if decision.Admitted {
		decision.Reservation = m.reserveLocked(req)
	}
	if !decision.Admitted {
		m.intents = append(m.intents, Intent{
			HypothesisID: req.ServesHypothesisID,
			Question:     req.Question,
			DeepLink:     req.DeepLink,
			Budget:       decision.Budget,
			Reason:       decision.Reason,
			At:           m.clock().UTC(),
		})
	}
	return decision
}

//nolint:gocyclo // one branch per budget dimension; a shorter version would hide a dimension.
func (m *Manager) decide(req Request) Decision {
	synthesis := req.Synthesis || m.mode == ModeSynthesisOnly

	// Evidence gathering is over once the reserve is entered. This is checked first so that the
	// refusal names the mode rather than whichever dimension happens to be tightest.
	if m.mode == ModeSynthesisOnly && req.Kind == KindWorker && !req.Synthesis {
		return Decision{
			Budget: "synthesis_only",
			Reason: "the engine is in synthesis_only mode (" + m.reserveReason +
				"); no further evidence is gathered and there is no path back to normal (FR-045)",
		}
	}

	// Wall time. The hard stop is absolute: nothing, not even the synthesis, runs past it.
	if limit := m.wallTimeLimitLocked(); limit > 0 {
		elapsed := m.elapsedLocked()
		if elapsed >= limit {
			return Decision{
				Budget: "wall_time",
				Reason: fmt.Sprintf("wall time %s has reached the hard stop of %s", elapsed.Round(time.Second), limit),
			}
		}
		if !synthesis && elapsed.Seconds() >= unreserved(float64(limit/time.Second)) {
			return m.refuseIntoReserve("wall_time", fmt.Sprintf(
				"wall time %s has used the unreserved %.0f%% of the %s budget",
				elapsed.Round(time.Second), (1-ReserveFraction)*100, limit))
		}
	}

	switch req.Kind {
	case KindModel:
		spent := float64(m.tokens.Total().Total() + m.pendingTokens)
		limit := m.costUnitsLocked()
		ceiling := limit
		if !synthesis {
			ceiling = unreserved(limit)
		}
		if spent+float64(req.Tokens) > ceiling {
			if synthesis {
				return Decision{
					Budget: "model_tokens",
					Reason: fmt.Sprintf("a %d-token call would take the run to %.0f tokens, past the %.0f-token budget",
						req.Tokens, spent+float64(req.Tokens), limit),
				}
			}
			return m.refuseIntoReserve("model_tokens", fmt.Sprintf(
				"a %d-token call would take the run to %.0f tokens, past the unreserved %.0f of the %.0f-token budget",
				req.Tokens, spent+float64(req.Tokens), ceiling, limit))
		}

	case KindWorker:
		if req.CostClass == "" {
			return Decision{
				Budget: "cost_class",
				Reason: "the capability declares no cost class; an unbudgetable capability is an unbounded one (FR-047a)",
			}
		}
		if width := m.maxWindowLocked(); width > 0 && req.WindowWidth > width {
			return Decision{
				Budget: "max_window",
				Reason: fmt.Sprintf("a window of %s is wider than the %s cap this profile sets", req.WindowWidth, width),
			}
		}
		if limit := m.classLimitLocked(req.CostClass); limit > 0 {
			ceiling := limit
			if !synthesis {
				ceiling = unreservedCalls(limit)
			}
			if m.callsByCostClass[req.CostClass]+1 > ceiling {
				return m.refuseClassCap(req.CostClass, fmt.Sprintf(
					"%d %s calls have been made and the budget allows %d of %d before the reserve",
					m.callsByCostClass[req.CostClass], req.CostClass, ceiling, limit))
			}
		}
		if limit := m.backendLimitLocked(req.Backend); limit > 0 {
			ceiling := limit
			if !synthesis {
				ceiling = unreservedCalls(limit)
			}
			if m.callsByBackend[req.Backend]+1 > ceiling {
				return m.refuseIntoReserve("backend_calls:"+req.Backend, fmt.Sprintf(
					"%d calls have been made to %s and the budget allows %d of %d before the reserve",
					m.callsByBackend[req.Backend], req.Backend, ceiling, limit))
			}
		}
		if reason, ok := m.quota.wouldBreach(req.Backend); !ok {
			return Decision{Budget: "quota_share:" + req.Backend, Reason: reason}
		}

	default:
		return Decision{Budget: "kind", Reason: fmt.Sprintf("call kind %q is neither model nor worker", req.Kind)}
	}

	return Decision{Admitted: true}
}

// refuseIntoReserve refuses the call and enters synthesis_only, because the dimension that
// refused it is one the reserve protects.
func (m *Manager) refuseIntoReserve(budget, reason string) Decision {
	entered := m.mode == ModeNormal
	m.enterReserveLocked(budget + ": " + reason)
	return Decision{Budget: budget, Reason: reason, EnteredReserve: entered}
}

// refuseClassCap refuses a call whose cost class has spent its unreserved portion, and enters
// synthesis_only only when **no** class has any left.
//
// One class running out is not the investigation running out. The reserve exists to pay for the
// closing synthesis — the verifier pass, the render, the stop reason — and none of those spends an
// `expensive` worker call, so holding back 15% of the expensive class buys the synthesis nothing
// while flipping the whole engine into synthesis_only costs it everything: the cheap and standard
// questions it could still ask, and, when a model is configured, the turn in which it says what it
// found. FR-045 makes termination follow "exhaustion of any budget"; the unreserved portion of one
// class is not that budget's exhaustion, it is the point past which that class is reserved.
//
// So the refusal is local and typed — the call is not issued, the intent is filed, and the
// hypothesis it would have tested is reported untested with the exact next query (FR-031, FR-047a)
// — and the mode moves only when every class is in the same state, which is evidence gathering
// being genuinely over. Wall time and model tokens are unchanged: those the synthesis does spend,
// and their unreserved portion running out is the run running out.
func (m *Manager) refuseClassCap(class, reason string) Decision {
	budget := "worker_calls:" + class
	if m.anyClassHasHeadroomLocked() {
		return Decision{Budget: budget, Reason: reason}
	}
	return m.refuseIntoReserve(budget, reason)
}

// anyClassHasHeadroomLocked reports whether some cost class can still pay for an evidence call
// out of its unreserved portion.
func (m *Manager) anyClassHasHeadroomLocked() bool {
	for _, class := range Classes {
		limit := m.classLimitLocked(class)
		if limit <= 0 {
			// An unlimited class is headroom by definition.
			return true
		}
		if m.callsByCostClass[class] < unreservedCalls(limit) {
			return true
		}
	}
	return false
}
