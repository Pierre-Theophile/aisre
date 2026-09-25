// SPDX-License-Identifier: Apache-2.0

// Package backend is the public SDK for writing a telemetry backend: the TelemetryBackend
// interface, the Description it registers itself with, the published telemetry terms it may
// serve, the Recorder that writes a world, and the testkit that replays one back through it.
//
// A telemetry backend executes one algebra term against one vendor and returns one bounded
// digest. It is simultaneously four things, which is why it is a published contract rather
// than an internal seam (ADR-0003 D8):
//
//   - the replay boundary — a recorded world is keyed by the canonicalised term;
//   - the vendor abstraction — features 003 (GCP) and 005 (Datadog) implement these messages,
//     they do not define their own;
//   - the sanitisation point — redaction happens here, in live mode as well as when recording;
//   - the prompt-injection barrier — worker output reaches the model only as a tool result.
//
// The prose contract is specs/002-investigation-engine/contracts/telemetry-backend.md; its
// machine-readable source of truth is api/sreagent/investigation/v1/investigation.proto §1–§3.
// This package aliases those generated types rather than restating them, so that a backend
// author and the engine cannot disagree about what a digest is.
//
// A backend emits no graph events — that is the feeder half of a connector, and the two halves
// share a credential and a quota, and nothing else. It serves the telemetry family and nothing
// else: a backend declaring a graph or knowledge term is rejected at registration.
//
// The algebra's mechanics are algebra.go, the registration gate registry.go, the declaration's
// renderings describe.go, the world on disk record.go, and the conformance suite the testkit
// subpackage. The recorded backend that reads a world is internal/investigation/backend.
package backend
