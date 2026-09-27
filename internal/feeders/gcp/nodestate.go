// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"crypto/sha256"
	"encoding/hex"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// Naming each state of a node the feeder re-reads every poll (003 T183).
//
// An event id is an idempotency key. A node emitted every poll under an id built from its ref alone
// is therefore asserted ONCE: every later poll is a DUPLICATE_NOOP and the node is frozen at its first
// observation. T066 found that for Cloud Run services, whose traffic split never moved on the node,
// and fixed it with Cloud Run's own version stamp (serviceEventID). The four node kinds here had the
// same id and the same defect — `gcp-cloudsql-flag-change-01`'s instance kept its old flags after the
// flag change the same fixture emits a change for — and have no stamp as convenient: a Cloud SQL
// instance, a GKE cluster and a forwarding rule state no update instant at all, and the recorded twins
// carry no etag or fingerprint to key on.
//
// So the state is named by what is asserted: a digest of the node's display name, properties and
// pointers. An unchanged re-poll produces the same digest and re-sends the id it already sent, which
// stays a no-op. A changed state is a new digest and a new id; the id also carries the observation
// instant, so a change BACK to an earlier state is not mistaken for the earlier assertion and dropped.
//
// # Dating a later state
//
// The first state a run sees of a node keeps the valid start its NodeFact gives it — creation where
// the platform states it, the rules each kind already documents where it does not. A LATER state did
// not hold from then, and asserting it from there would be a correction ("we were wrong about what it
// was all along") when what happened is that production moved. It is dated from the instant the
// platform says the resource last changed where there is one (an alert policy's mutation record), and
// otherwise marked unknown and begun at the observation — the honest answer, and the one a forwarding
// rule's first state already gives (FR-011).
//
// # Known limit
//
// The remembered states are per run. After a restart the first poll is a first state again, dated as
// one; a node that changed while the feeder was down is asserted from its original start, and loses to
// the previous run's later state for instants after it. That is the Cloud Run restart limit, recorded
// with its fix as T184.

// assertedState is the last state of one node this run emitted.
type assertedState struct {
	digest string
	id     string
}

// stateAssertion returns the event id to emit a node's state under, and the fact adjusted for a later
// state. kind is the id's kind segment ("sql_instance", …), value the node's ref value, and statedAt
// the instant the platform says the resource last changed, or zero where it states none.
func (f *Feeder) stateAssertion(sourceID, kind, value string, fact feeder.NodeFact, at, statedAt time.Time) (string, feeder.NodeFact, error) {
	digest, err := stateDigest(fact)
	if err != nil {
		return "", fact, err
	}
	key := kind + "\x00" + value

	f.mu.Lock()
	defer f.mu.Unlock()
	previous, seen := f.states[key]
	switch {
	case !seen:
		id := feeder.NewID(sourceID, kind, value+"@"+digest)
		f.states[key] = assertedState{digest: digest, id: id}
		return id, fact, nil
	case previous.digest == digest:
		// Unchanged: the id already sent, so the graph answers DUPLICATE_NOOP as it always did.
		return previous.id, fact, nil
	default:
		later := fact
		if !statedAt.IsZero() && statedAt.After(fact.ValidAt) {
			later.ValidAt, later.ValidFromUnknown = statedAt, false
		} else {
			later.ValidAt, later.ValidFromUnknown = at, true
		}
		id := feeder.NewID(sourceID, kind, value+"@"+digest+"@"+at.UTC().Format(time.RFC3339Nano))
		f.states[key] = assertedState{digest: digest, id: id}
		return id, later, nil
	}
}

// stateDigest is a short digest of what a node assertion says, with the valid time excluded: the
// same state asserted from a different instant is the same state.
func stateDigest(fact feeder.NodeFact) (string, error) {
	opts := proto.MarshalOptions{Deterministic: true}
	h := sha256.New()
	h.Write([]byte(fact.DisplayName))
	h.Write([]byte{0})
	if fact.Props != nil {
		raw, err := opts.Marshal(fact.Props)
		if err != nil {
			return "", err
		}
		h.Write(raw)
	}
	for _, pointer := range fact.Pointers {
		h.Write([]byte{0})
		raw, err := opts.Marshal(pointer)
		if err != nil {
			return "", err
		}
		h.Write(raw)
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}
