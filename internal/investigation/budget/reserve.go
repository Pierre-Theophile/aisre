// SPDX-License-Identifier: Apache-2.0

package budget

import "math"

// The synthesis reserve (T070, FR-045, SC-005).
//
// **Exactly fifteen per cent.** Not "about 15%", not "a configurable fraction defaulting to
// 0.15": a published constant, asserted by a fixture, and refused by Profile.Validate when a
// configuration tries to move it. The reason it cannot be an operator setting is that the thing
// it buys is not a preference — it is the guarantee that an investigation which runs out of
// budget still produces a written answer, with a verifier pass behind it and a typed stop reason
// on it (SC-005). A deployment that could set it to zero could produce a run that spent
// everything gathering evidence and then had nothing left to say what it found.
//
// The mode machine has two states and one transition:
//
//	normal ──(the unreserved portion of any dimension is exhausted)──► synthesis_only
//
// There is no path back. Once the engine has decided it is closing, evidence gathering is over;
// a run that oscillated would spend its reserve on the oscillation. And there is no third state
// for "give up": continuing past a budget and discarding partial results are both impossible, so
// `synthesis_only` is what exhaustion means rather than what failure means.

// ReserveFraction is the published reserve: exactly 15% of every dimension, held back for the
// closing synthesis — the verifier pass, the render and the stop reason.
const ReserveFraction = 0.15

// Mode is whether the engine is still gathering evidence.
type Mode string

// The two modes.
const (
	// ModeNormal gathers evidence.
	ModeNormal Mode = "normal"
	// ModeSynthesisOnly runs the verifier, the render and the stop reason from the reserve. No
	// new evidence is gathered and there is no path back to normal.
	ModeSynthesisOnly Mode = "synthesis_only"
)

// unreserved is how much of a limit may be spent before the reserve is entered.
//
// It floors rather than rounds, so a budget of 3 expensive calls gives 2 before the reserve
// rather than 3: rounding up would let the last call be issued out of the reserve, which is
// exactly what the reserve exists to prevent.
func unreserved(limit float64) float64 {
	if limit <= 0 {
		return 0
	}
	return limit * (1 - ReserveFraction)
}

// unreservedCalls is unreserved for an integer dimension, floored.
func unreservedCalls(limit int64) int64 {
	if limit <= 0 {
		return 0
	}
	return int64(math.Floor(float64(limit) * (1 - ReserveFraction)))
}
