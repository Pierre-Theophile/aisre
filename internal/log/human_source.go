// SPDX-License-Identifier: Apache-2.0

package log

// The human source (001 FR-041, 002 FR-054, FR-055, FR-057a/b/e).
//
// A person who confirms a merge, pushes a fact at an investigation, reopens one or labels one is
// **one source**, whatever surface they act through. The identity that matters is the
// authenticated individual, which every such event carries in its own body; the source id says
// only "this came from a person rather than from a feeder", and splitting it per surface would
// make `log.sources` a list of user interfaces.
//
// It lives here, beside `Source`, because both the resolution path (`internal/server`) and the
// investigation's human channel (`internal/investigation/store`) register it and neither may
// import the other.

// HumanSourceID is the source id every human action is logged under.
const HumanSourceID = "human"

// HumanSource is its declaration: a person is not a feeder, delivers nothing in order, and needs
// no reordering window.
func HumanSource() Source {
	return Source{SourceID: HumanSourceID, Kind: "human", Ordering: "none"}
}
