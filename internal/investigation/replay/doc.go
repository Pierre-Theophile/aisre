// SPDX-License-Identifier: Apache-2.0

// Package replay records and replays an investigation in two layers: the trajectory (layer 1,
// sequenced) and the world (layer 2, keyed by algebra term) — plan §Replay design.
//
// Three files, three jobs:
//
//   - recorder.go — the on-disk layer-1 format, `trajectories/<run-id>.jsonl`, and its reader.
//     The run id is derived from the trajectory's own digest, so re-recording an unchanged run
//     writes the same bytes to the same path (T093).
//   - replayer.go — the re-issue path that re-issues nothing: model requests matched by canonical
//     digest against the next recorded exchange, worker requests served from the recording,
//     divergence reported at the first diverging record, exit 4 at the command line (T094).
//   - export.go — the self-contained artifact: decision record, hypothesis ledger, both recording
//     layers and the graph events the investigation's graph queries need, bound by an export
//     digest (T095).
//
// Nothing in this package opens a socket, and nothing in it needs a database. That is the whole
// point: the trajectory gate runs on every pull request in a job with neither.
package replay
