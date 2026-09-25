// SPDX-License-Identifier: Apache-2.0

// Package record turns a live feeder run into a replayable fixture (FR-044, FR-049,
// constitution VIII).
//
// Two tees and a manifest writer. Wrap sits between a feeder and its Source and saves every
// raw payload; Emitter sits between a feeder and its Emitter and saves every event the graph
// accepted, with the observed time the graph assigned it. Run a feeder once against the real
// system with both in place and the directory that comes out is a fixture: `payloads/` is the
// input, `events.jsonl` is the expected output, and `aisre fixture verify` replays one
// into the other for ever after.
//
// Recording is deliberately not clever. It writes files and nothing else — no filtering, no
// sanitisation, no rewriting. Sanitising a recording of a real cluster is a human's job,
// because only a human knows which host names are secrets, and a tool that promised to do it
// automatically would be believed.
package record
