// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"context"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/event"
	"github.com/Pierre-Theophile/aisre/internal/graph"
)

// The attributor's ports. The decision layers, discovery and the rules are this package; what the
// graph currently holds is read through Store, and what the attributor decides is appended through
// EventLog. internal/catalog/pgcatalog implements both over Postgres and the projector.
//
// Store returns rows, not decisions: props come back as the raw column so that decoding them (and
// every rule that reads them) stays here, and nothing in the adapter knows what a rule is.

// Store reads the graph and the catalog tables as they are now.
type Store interface {
	// CurrentSignals lists every current alert, document and change, by entity id.
	CurrentSignals(ctx context.Context) ([]string, error)
	// StandingDecision reads the latest attribution or correction of a signal nothing has
	// superseded or reverted. An empty eventID means there is none.
	StandingDecision(ctx context.Context, signalID string) (eventID string, entries, suggestions []Attributed, err error)
	// Correction reads the standing correction of a signal, if any.
	Correction(ctx context.Context, signalID string) (slugs []string, found bool, err error)
	// Aliases reads every current alias; ruleEntityClaim marks the ones a proposed entry carries.
	Aliases(ctx context.Context, ruleEntityClaim string) ([]Alias, error)
	// HiddenEntries lists the slugs of the entries a person hid.
	HiddenEntries(ctx context.Context) ([]string, error)
	// Entries reads every catalog entry as it is now.
	Entries(ctx context.Context) ([]EntryRow, error)
	// EntryOwners reads, per entry, the owners of the entities its aliases name (rule P4).
	EntryOwners(ctx context.Context) (map[string][]string, error)
	// Lookup resolves a ref to its canonical entity, without creating anything.
	Lookup(ctx context.Context, ref graph.Ref) (string, bool, error)
	// LogEnd is the event log's last appended sequence number.
	LogEnd(ctx context.Context) (int64, error)
	// AliasProposed says an alias proposal with this key was raised before.
	AliasProposed(ctx context.Context, key string) (bool, error)
	// Services lists the current SERVICE entities no catalog entry claims.
	Services(ctx context.Context) ([]EntityRow, error)
	// EntityNames reads an entity's identity claims, and every name it goes by with its
	// environment, following merges.
	EntityNames(ctx context.Context, entityID string) (claims []graph.Ref, names []Name, err error)
	// DocumentProps reads the current props of every KNOWLEDGE_DOC, in entity order.
	DocumentProps(ctx context.Context) ([][]byte, error)
	// ResolveEntity follows merges to the canonical entity and reads its type and first claim.
	ResolveEntity(ctx context.Context, entityID string) (canonical string, typ graph.NodeType, ref graph.Ref, found bool, err error)
	// LatestVersion reads an entity's latest believed version: display name and raw props.
	LatestVersion(ctx context.Context, entityID string) (display string, props []byte, err error)
	// Targets reads every name of the entities a signal's edges of typ reach. inbound reads edges
	// into the signal, at any instant; otherwise current edges out of it.
	Targets(ctx context.Context, signalID string, typ graph.EdgeType, inbound bool) ([]Name, error)
	// Neighbourhood reads the names of the entities the targets run on, depend on or call, and
	// the owners they are owned by.
	Neighbourhood(ctx context.Context, signalID string, typ graph.EdgeType, inbound bool) (related []Name, owners []string, err error)
}

// EntryRow is one catalog entry as stored.
type EntryRow struct {
	Slug    string
	Display string
	Props   []byte
}

// EntityRow is one entity and its display name.
type EntityRow struct {
	ID      string
	Display string
}

// EventLog is where the attributor's decisions go and where it reads what changed.
type EventLog interface {
	// RegisterSource records the attributor's source declaration.
	RegisterSource(ctx context.Context, src event.Source) error
	// Apply appends one event, observed at observedAt (zero lets the log stamp it).
	Apply(ctx context.Context, env *graphv1.EventEnvelope, observedAt time.Time) (*graphv1.IngestResult, error)
	// Page calls fn for up to limit events appended after cursor, in order.
	Page(ctx context.Context, cursor int64, limit int, fn func(seq int64, env *graphv1.EventEnvelope) error) error
}
