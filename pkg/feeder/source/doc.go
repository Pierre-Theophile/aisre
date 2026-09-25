// SPDX-License-Identifier: Apache-2.0

// Package source provides the two Sources every feeder needs.
//
// FileSource replays a recorded fixture's payloads; ChanSource is what a live feeder pushes
// into from its watch loop or its receiver. A feeder sees only feeder.Source, which is what
// makes FR-044 true by construction: live and recorded runs go through the same code and must
// therefore produce the same events.
package source
