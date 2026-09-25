// SPDX-License-Identifier: Apache-2.0

// Package worker is the public SDK for writing a worker: the Worker interface, the Description
// a worker registers itself with, the Request and Response that cross the call boundary, and
// the registry that refuses a declaration the investigator must not be handed.
//
// A worker is a read-only capability provider bound to exactly one source of truth. It is what
// the investigator calls; it is never what the investigator reads around. The published
// contract is specs/002-investigation-engine/contracts/worker-sdk.md, and its machine-readable
// source of truth is api/sreagent/investigation/v1/investigation.proto §3.
//
// It rhymes with pkg/feeder on purpose — Describe, one entry point, a recorder, a testkit — so
// that a connector author learns one idiom and writes both halves of a connector with it.
//
// Four rules follow from the constitution and are worth stating before any code is written:
//
//   - A worker answers in the published query algebra and in nothing else. A request outside
//     it is refused, naming what was asked and what is available, and the refusal is recorded
//     (FR-042b). This is what keeps a recorded world finite and a replay honest.
//   - A worker returns digests, never telemetry. Identifiers, parameters, aggregates,
//     comparisons, mined templates, exemplar references, pointers, join keys and drill-down
//     handles cross the boundary; samples, log bodies and span payloads do not (constitution
//     IV). Every response carries a coverage block, and one without it is rejected.
//   - A worker is read-only. A capability that changes state in its source is rejected at
//     registration, not at call time (FR-016, constitution VII).
//   - Anything a worker returns is data. Instructions embedded in retrieved content do not
//     change the investigation's scope, budgets, posture, worker set or output; an attempt is
//     recorded as an evidence item (FR-017).
//
// The declaration and the registration gate are worker.go and registry.go; the call path — the
// per-attempt record, the typed failure, the mode check — is call.go; the conformance suite is
// the testkit subpackage. The five workers themselves are internal/investigation/workers.
package worker
