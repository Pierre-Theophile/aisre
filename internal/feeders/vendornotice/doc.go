// SPDX-License-Identifier: Apache-2.0

// Package vendornotice turns provider announcements into change nodes with a vendor actor.
//
// Its single responsibility is the one the coverage audit paid for: **the most repeated cause
// across the thirteen incidents was a vendor notice nobody read** — a maintenance window, a
// deprecation or a provider incident, announced weeks in advance and rediscovered at 02:10 under a
// page. This feeder puts that announcement in the graph on the day it arrives.
//
// Two things make it unlike every other feeder, and both are why it is its own package:
//
//  1. **The facts it emits are about the future.** A notice is an announced fact — valid time
//     begins after observed time — so cancellation and rescheduling are corrections that close an
//     observation and open a new one, and never rewrite a valid interval (FR-062–FR-068).
//  2. **Its input is untrusted text from outside the organisation**, which must never be stored.
//     It extracts typed fields and a message pointer; the body is never written to disk at all.
//
// Notice sources sit behind pkg/feeder.Source, so a mailbox, a status page and a changelog are
// three implementations of one seam. No mailbox address, domain or sender is hard-coded anywhere
// in this package (FR-132b), and per-user delivery is a first-class path rather than a fallback
// (FR-132a) because Google addresses platform notices to each project's Essential Contacts and to
// its billing and owner principals, not to a shared address.
//
// Contract: specs/003-gcp-integration/contracts/vendor-notice-feeder.md.
package vendornotice
