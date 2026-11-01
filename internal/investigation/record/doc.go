// SPDX-License-Identifier: Apache-2.0

// Package record is what an investigation writes down, without the database that keeps it: the
// incident and investigation it opens, the worker and model calls it makes, its conclusion and
// lifecycle states, the human facts, reviews and labels that bear on it, the decision record it
// emits to the graph, and the stored ledger rendered as the published message.
//
// It holds types, validation and the event envelopes, and nothing that runs SQL.
// internal/investigation/store is the Postgres adapter that persists these values; the runner and
// the server depend on this package and on their own ports, never on that adapter.
package record
