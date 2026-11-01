// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/event"
	"github.com/Pierre-Theophile/aisre/internal/telemetry"
)

// The server's ports. The handlers authenticate, validate, translate errors and record metrics;
// what they read and write is behind the interfaces below. The projector, the query engine and
// the Postgres store implement them as they are, and internal/server/pgserver wires them together
// for the composition root.

// EventLog is the write side of the graph: the only way an event reaches it.
type EventLog interface {
	// RegisterSource makes a source appendable.
	RegisterSource(ctx context.Context, src event.Source) error
	// Apply appends env and projects it, observed at observedAt (zero means now).
	Apply(ctx context.Context, env *graphv1.EventEnvelope, observedAt time.Time) (*graphv1.IngestResult, error)
	// ApplyAs appends a human decision on behalf of the authenticated principal (FR-041).
	ApplyAs(ctx context.Context, env *graphv1.EventEnvelope, principal string) (*graphv1.IngestResult, error)
	// Extent reports the observed-time span of the log and each source's coverage.
	Extent(ctx context.Context) (*graphv1.Extent, error)
	// SchemaVersions are the event schema versions the log accepts, oldest first.
	SchemaVersions() event.SchemaVersions
	// RegisterObservables registers the gauges answered by reading the graph.
	RegisterObservables(m *telemetry.Metrics)
}

// GraphReader answers the graph's read questions.
type GraphReader interface {
	Subgraph(ctx context.Context, req *graphv1.SubgraphRequest) (*graphv1.SubgraphResponse, error)
	Diff(ctx context.Context, req *graphv1.DiffRequest) (*graphv1.DiffResponse, error)
	Impact(ctx context.Context, req *graphv1.ImpactRequest) (*graphv1.ImpactResponse, error)
	Pointers(ctx context.Context, req *graphv1.PointersRequest) (*graphv1.PointersResponse, error)
	NodeHistory(ctx context.Context, req *graphv1.NodeHistoryRequest) (*graphv1.NodeHistoryResponse, error)
	ResolutionAudit(ctx context.Context, req *graphv1.ResolutionAuditRequest) (*graphv1.ResolutionAuditResponse, error)
	Suggestions(ctx context.Context, req *graphv1.SuggestionsRequest) (*graphv1.SuggestionsResponse, error)
	ProposedDependencies(ctx context.Context, req *graphv1.ProposedDependenciesRequest) (*graphv1.ProposedDependenciesResponse, error)
}

// CatalogReader answers the service catalog's read questions (feature 009).
type CatalogReader interface {
	ListEntries(ctx context.Context, req *graphv1.ListEntriesRequest) (*graphv1.ListEntriesResponse, error)
	GetEntry(ctx context.Context, req *graphv1.GetEntryRequest) (*graphv1.GetEntryResponse, error)
	WhyAttributed(ctx context.Context, req *graphv1.WhyAttributedRequest) (*graphv1.WhyAttributedResponse, error)
	AttributedSignals(ctx context.Context, req *graphv1.AttributedSignalsRequest) (*graphv1.AttributedSignalsResponse, error)
	ListProposals(ctx context.Context, req *graphv1.ListProposalsRequest) (*graphv1.ListProposalsResponse, error)
}

// Database is what the health and readiness probes ask.
type Database interface {
	// Ping says the database answers.
	Ping(ctx context.Context) error
	// Migrated says the schema has been migrated.
	Migrated(ctx context.Context) (bool, error)
}

// Backends is everything the graph's handlers read and write through.
type Backends struct {
	Log        EventLog
	Graph      GraphReader
	Catalog    CatalogReader
	Principals PrincipalRecorder
	Database   Database
}

func (b Backends) complete() bool {
	return b.Log != nil && b.Graph != nil && b.Catalog != nil && b.Principals != nil && b.Database != nil
}
