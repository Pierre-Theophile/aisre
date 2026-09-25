// SPDX-License-Identifier: Apache-2.0

package fixture

import (
	"math/rand/v2"
	"slices"
	"time"
)

// Shuffling within the declared window (FR-021, FR-048, SC-004).
//
// A feeder declares how far out of order it may deliver; that declaration is a promise the
// graph must be robust to. The shuffle check takes it literally: inside a source's reordering
// window the events may arrive in any order, and the valid-time state afterwards must be the
// same.
//
// Two invariants make the permutation a fair test rather than an arbitrary one:
//
//   - Events are permuted per source. A source's window says nothing about another source's
//     delivery, and the projector is allowed to see interleavings only as observed time permits.
//   - The observed-time *slots* stay where they are: the i-th event delivered by a source keeps
//     the i-th observed time, whichever event that now is. Observed time therefore still only
//     moves forwards, which is the real constraint — a feeder may deliver late, never early.

// shuffleSeed is mixed into every permutation's seed so that a fixture's shuffles are
// reproducible but not the same stream as any other seeded use of the generator.
const shuffleSeed uint64 = 0x5E3A6E17

// shuffleWithinWindow returns a permutation of events that respects every source's declared
// reordering window.
func shuffleWithinWindow(events []Event, windows map[string]time.Duration, rng *rand.Rand) []Event {
	bySource := map[string][]Event{}
	var order []string
	for _, event := range events {
		id := event.Envelope.GetSourceId()
		if _, ok := bySource[id]; !ok {
			order = append(order, id)
		}
		bySource[id] = append(bySource[id], event)
	}

	out := make([]Event, 0, len(events))
	for _, sourceID := range order {
		queue := slices.Clone(bySource[sourceID])
		slots := make([]time.Time, len(queue))
		for i, event := range queue {
			slots[i] = event.ObservedAt
		}
		window := windows[sourceID]

		permuted := make([]Event, 0, len(queue))
		for len(queue) > 0 {
			// Anything observed within the window of the queue's head may be delivered
			// first; nothing beyond it may jump the queue.
			limit := 1
			for limit < len(queue) && queue[limit].ObservedAt.Sub(queue[0].ObservedAt) <= window {
				limit++
			}
			pick := rng.IntN(limit)
			permuted = append(permuted, queue[pick])
			queue = append(queue[:pick], queue[pick+1:]...)
		}
		for i := range permuted {
			permuted[i].ObservedAt = slots[i]
		}
		out = append(out, permuted...)
	}

	// A stable sort by observed time interleaves the sources the way the log would have seen
	// them, without disturbing the order chosen within a source.
	slices.SortStableFunc(out, func(a, b Event) int { return a.ObservedAt.Compare(b.ObservedAt) })
	return out
}
