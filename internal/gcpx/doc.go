// SPDX-License-Identifier: Apache-2.0

// Package gcpx is the one place this repository talks to Google Cloud.
//
// Its single responsibility is **policy**: credential discovery and scopes, the three-layer
// read-only gate that must pass before anything else may run, the call budget, quota and backoff,
// and the usage report every run is audited by.
//
// Where the line with the feeder's transport seam falls, since both are about talking to GCP:
// this package touches only the APIs that answer questions **about the credential** —
// `cloudresourcemanager.testIamPermissions` and the token endpoint — and never an area the feeder
// reads. The per-area clients (run, logging, monitoring, sqladmin, pubsub) are imported in exactly
// one file, `internal/feeders/gcp/transport.go`, which is what keeps live and recorded mode on one
// code path.
//
// The ordering that matters is enforced by the type system rather than by convention: a
// `*Credential` is minted only by a gate that passed, and a per-area client cannot be constructed
// without one. So there is no ordering of this program in which GCP is read before the check that
// makes reading it safe.
//
// It is deliberately not a facade over the whole of Google Cloud. It exposes only the operations
// the published read-only operation list names, because a helper that can call a mutating method
// is a helper that one day will.
//
// Contract: specs/003-gcp-integration/contracts/gcp-feeder.md §"Startup refusal" and
// specs/003-gcp-integration/contracts/budget.md. Vendor-API facts, including the four assumptions
// that turned out to be wrong, are specs/003-gcp-integration/research.md.
package gcpx
