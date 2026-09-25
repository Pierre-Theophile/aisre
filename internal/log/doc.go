// SPDX-License-Identifier: Apache-2.0

// Package log owns the append-only event log: it validates and appends incoming events,
// deduplicates them by idempotency key, stamps observed time, and reads them back in replay
// order.
package log
