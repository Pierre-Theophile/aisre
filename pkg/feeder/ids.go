// SPDX-License-Identifier: Apache-2.0

package feeder

import "strings"

// Deterministic event identifiers (FR-018, research §4).
//
// Nothing in this system may be random. An event id is the only thing that makes a
// re-delivery recognisable as one, and a feeder that mints a UUID per event turns every
// restart into a duplicate graph. So an id is always a pure function of source-native
// identifiers that are stable across restarts: an object UID plus its resourceVersion, an
// aggregation window start plus the pair of services it describes.
//
// The published convention is `<source_id>:<part>:<part>…` (fixtures/README.md), for example
// `k8s:demo:deploy:shop/checkout@rv1001` or `otel:demo:edge:checkout->payments@w1300`. The
// idempotency key equals the event id unless the feeder has a reason to separate them.

// idSeparator joins the parts of an event id. It is `:` because that is what every fixture and
// every log line in this project already uses.
const idSeparator = ":"

// NewID builds a deterministic event id: sourceID followed by each part, joined with `:`.
//
// Parts are sanitized, never rejected: a feeder minting an id in the middle of a watch stream
// has nowhere to report an error, and an id that silently loses a character is far less
// damaging than one that breaks the log's line format. The characters replaced with `_` are
// the ones that would make an id ambiguous or unparseable — NUL (which no identifier in this
// system may contain, research §4), newline and carriage return (the events file is one event
// per line), and `|` (the separator of a resolution pair key, graph.PairKey).
//
// Empty parts are dropped, so NewID("k8s:demo", "deploy", "") is `k8s:demo:deploy`.
func NewID(sourceID string, parts ...string) string {
	out := make([]string, 0, len(parts)+1)
	if clean := sanitizeIDPart(sourceID); clean != "" {
		out = append(out, clean)
	}
	for _, part := range parts {
		if clean := sanitizeIDPart(part); clean != "" {
			out = append(out, clean)
		}
	}
	return strings.Join(out, idSeparator)
}

// IdempotencyKey returns the key that makes re-delivery of an event a no-op (FR-020).
//
// Called with an event id alone it returns that id, which is the published default and what
// every fixture uses. Extra parts build a key that differs from the event id, for the one case
// that needs it: the same fact re-asserted under a new event id — a resync that re-reads an
// object it has already reported unchanged — should collapse onto the first delivery rather
// than appear as a second assertion.
func IdempotencyKey(eventID string, parts ...string) string {
	if len(parts) == 0 {
		return eventID
	}
	return NewID(eventID, parts...)
}

// sanitizeIDPart replaces the characters that would make an id ambiguous.
func sanitizeIDPart(part string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '\x00', '\n', '\r', '|':
			return '_'
		default:
			return r
		}
	}, strings.TrimSpace(part))
}
