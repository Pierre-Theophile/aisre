// SPDX-License-Identifier: Apache-2.0

package backend

// SeparationThreshold is the published relative difference below which a comparison is reported
// as not separating the hypothesis. It is deliberately generous: reporting a real difference as
// non-separable costs one more query, and reporting noise as separable exonerates or convicts a
// change on nothing.
const SeparationThreshold = 0.20

// Separates is the rule itself, over the one number it reads: a relative difference of this
// magnitude discriminates, and anything smaller does not.
//
// It is exported because it must be applied in exactly one place by everyone who applies it. A
// backend that generates comparisons states
// `separable` on the comparisons it produces, and the metrics worker re-states it on every answer it
// passes on. Two spellings of the same rule is one spelling too many: where they disagree, the
// worker's annotation changes a digest that was already hashed, and the recording that results
// carries a `response_digest` its own content does not hash to.
func Separates(relativeDelta float64) bool {
	if relativeDelta < 0 {
		relativeDelta = -relativeDelta
	}
	return relativeDelta >= SeparationThreshold
}
