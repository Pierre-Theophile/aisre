// SPDX-License-Identifier: Apache-2.0

package projector

// A merge a person thought of themselves (FR-040).
//
// `manual_merge` and `confirm_merge` produce the same graph. They are separate event types so
// that the decisions table answers a question the project needs to keep asking: of the merges a
// person made, how many did the resolver propose first? A resolver whose suggestions are always
// rejected and whose merges are always manual is a resolver whose rules are wrong, and the only
// way to see that is to record which is which (constitution V, SC-007).
//
// The handler itself lives in confirm_merge.go, beside the code it shares, so that nothing about
// the two can drift apart.
