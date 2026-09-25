// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"crypto/sha256"
	"encoding/base32"
	"strings"
)

// Deterministic identifiers (research §4).
//
// Nothing in the projection may be random: replaying the event log from empty must reproduce
// a graph that is byte-for-byte identical under canonical serialization (FR-023,
// constitution III). Every id below is therefore a pure function of values that come from the
// log.
//
// Construction, identical for every id kind:
//
//  1. join the inputs with a 0x00 byte. No identifier in this system may contain NUL — a
//     Postgres text column cannot store one either — so no two different input tuples can
//     produce the same byte string;
//  2. take the SHA-256 of that byte string;
//  3. encode the digest with **standard** base32 (RFC 4648 alphabet A-Z2-7), without padding,
//     lowercased;
//  4. truncate to 26 characters = 130 bits, ample against collisions at the reference
//     workload of 10k entities, and short enough to read in a terminal.
//
// Standard base32 is chosen over Crockford's variant because it is in the standard library,
// needs no alphabet table in this repository, and no id is ever typed by a human from a
// screen, which is the problem Crockford's alphabet solves.
//
// All four id kinds share this one construction, with no kind tag (research §4). Ids are
// scoped per column — an entity id is never compared against a version id — and the real
// inputs do not overlap, since a version id is hashed from a 26-character entity id and an
// event id while an entity id is hashed from a namespace and a value.
//
// The exact output strings are pinned by golden tests in ids_test.go. Changing the hash, the
// separator, the alphabet, the case or the length changes every id in every fixture and in
// every deployed database: it is a BREAKING CHANGE to the published schema (constitution IX)
// and requires a major version bump plus an event-log transformation.
const (
	// IDLength is the number of base32 characters in every deterministic id.
	IDLength = 26

	separator = "\x00"
)

var idEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// hashID implements the construction documented above.
func hashID(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, separator)))
	return strings.ToLower(idEncoding.EncodeToString(sum[:]))[:IDLength]
}

// EntityID returns the canonical id of the entity first claimed as (namespace, value), for
// example ("otel.service.name", "checkout").
//
// An entity's canonical id is derived from the *first* identity claim the log saw for it, so
// it depends on log order. That is why the shuffle check (FR-048) compares valid-time state up
// to entity-id relabeling, matching entities by alias set, while replay in log order is
// required to be byte-identical (research §4).
func EntityID(namespace, value string) string {
	return hashID(namespace, value)
}

// VersionID returns the id of the node version that the given event produced for the given
// entity. One event produces at most one version per entity, which makes the pair unique.
func VersionID(entityID, eventID string) string {
	return hashID(entityID, eventID)
}

// EdgeVersionID returns the id of the edge version that the given event produced for the given
// (source, destination, type) triple. edgeType is the canonical string form of EdgeType, so
// callers pass `string(EdgeTypeCalls)`, not the protobuf enum number, which could be renumbered
// only by a breaking schema change but reads less clearly in a hash input.
func EdgeVersionID(srcID, dstID, edgeType, eventID string) string {
	return hashID(srcID, dstID, edgeType, eventID)
}

// ClaimID returns the id of an identity claim. Claims are unique per
// (namespace, value, source_id) — the same claim re-asserted by the same source is the same
// claim, whichever event carried it, which is what makes re-delivery a no-op at the claim
// level too (data-model.md, graph.identity_claims).
func ClaimID(namespace, value, sourceID string) string {
	return hashID(namespace, value, sourceID)
}

// CorrelationID returns the deterministic id of one correlation key on one entity (004 T148).
//
// The ENTITY is part of it, and that is the whole difference from ClaimID: an identity claim's id
// cannot carry the entity, because an identifier names exactly one and the id is how that is enforced.
// A correlation key is shared, so the same (namespace, value, source) legitimately appears on many
// entities and each needs an id of its own — while a redelivered event still lands on the same row.
func CorrelationID(entityID, namespace, value, sourceID string) string {
	return hashID(entityID, namespace, value, sourceID)
}

// PairKey returns the order-independent key of an unordered pair of entity ids, `min|max`, as
// used by graph.suggestions and by the resolution layer to block a rejected pair regardless of
// which side a later rule presents first. The `|` separator cannot occur in an id, whose
// alphabet is a-z2-7.
func PairKey(a, b string) string {
	if a > b {
		a, b = b, a
	}
	return a + "|" + b
}
