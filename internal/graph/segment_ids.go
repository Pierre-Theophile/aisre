// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Deterministic ids for bitemporal *segments* (research §4, extended).
//
// research §4 specifies `version_id = sha256(entity_id || event_id)`, on the assumption that
// one event produces at most one version per entity. Valid-time splitting breaks that
// assumption, and it is not an edge case but the normal path: an upsert asserting a fact from
// 14:00 onwards, applied to an entity whose current version is valid from 13:00, produces
// *two* rows from one event — the remainder [13:00, 14:00) and the new fact [14:00, ∞) — and a
// retraction cascading to a node's edges produces one per edge. Two rows, one hash, primary
// key collision.
//
// The segment's own valid lower bound is therefore part of the id. It is the missing
// coordinate: within one event and one entity, at most one version can begin at a given
// instant, because the versions an event leaves current have pairwise disjoint valid ranges
// (the exclusion constraint in 0002_graph.sql enforces exactly that). The id stays a pure
// function of logged values, so a replay reproduces every id byte for byte (FR-023).
//
// The lower bound is rendered RFC 3339 in UTC with nanosecond precision, the same spelling
// canonical serialization uses, so an id does not depend on the caller's time zone or on how
// a driver happened to round-trip a timestamp.
//
// Changing any of this changes every version id in every fixture and every deployed database:
// a breaking change to the published schema (constitution IX).

// SegmentVersionID returns the id of the node version that eventID produced for entityID
// beginning at validFrom.
func SegmentVersionID(entityID, eventID string, validFrom time.Time) string {
	return hashID(entityID, eventID, formatBound(validFrom))
}

// EdgeSegmentVersionID returns the id of the edge version that eventID produced for the
// (source, destination, type) triple beginning at validFrom. edgeType is the canonical string
// form of EdgeType, as for EdgeVersionID.
func EdgeSegmentVersionID(srcID, dstID, edgeType, eventID string, validFrom time.Time) string {
	return hashID(srcID, dstID, edgeType, eventID, formatBound(validFrom))
}

// DecisionID returns the id of a resolution decision: one decision per entity pair per event,
// which is what makes re-applying an event produce the decision row it produced before rather
// than a second one.
func DecisionID(pairKey, eventID string) string {
	return hashID(pairKey, eventID)
}

// SplitEntityID is the id of the entity a split creates when the identifier it is minted from
// is already taken (FR-039).
//
// A split never resurrects the id of an entity that was merged away (data-model.md §State
// transitions): that id belongs to a row that still exists, carries its own history, and now
// redirects to the entity the split created. So when `EntityID(namespace, value)` of the first
// detached claim is already in use, the new entity gets this deterministic id instead — a hash
// of the id it would have taken and the decision event, so a replay mints the same one.
func SplitEntityID(entityID, eventID string) string {
	return hashID(entityID, eventID)
}

// HumanEventID is the deterministic event id of a human resolution decision:
//
//	human:<kind>:<subject>:<sha256(principal | rationale)[:12]>
//
// A decision is an ordinary event and so needs an idempotency key, but the request that carries
// one has no nonce to build it from (contracts/graph.proto: ConfirmMerge and friends are three
// strings). The id is therefore derived from the decision itself, which gives the behaviour an
// idempotency key is for: the same person deciding the same thing about the same pair with the
// same reason twice is a DUPLICATE_NOOP, whether the second delivery came from a double click,
// a retried request or a fixture being loaded again.
//
// The subject is sorted, so `confirm a b` and `confirm b a` are one decision. The digest covers
// the principal and the rationale, so two people deciding the same thing are two decisions —
// each attributable to one of them (FR-041) — and adding the reason you forgot is a second,
// visible decision rather than a silent overwrite.
func HumanEventID(kind string, subject []string, principal, rationale string) string {
	parts := slices.Clone(subject)
	slices.Sort(parts)
	digest := sha256.Sum256([]byte(principal + "|" + rationale))
	return fmt.Sprintf("human:%s:%s:%s", kind, strings.Join(parts, "|"),
		hex.EncodeToString(digest[:])[:12])
}

func formatBound(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}
