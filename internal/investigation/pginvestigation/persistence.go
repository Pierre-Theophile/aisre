// SPDX-License-Identifier: Apache-2.0

package pginvestigation

import (
	"context"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/event"
	"github.com/Pierre-Theophile/aisre/internal/investigation/record"
	"github.com/Pierre-Theophile/aisre/internal/investigation/runner"
	investigationstore "github.com/Pierre-Theophile/aisre/internal/investigation/store"
	eventlog "github.com/Pierre-Theophile/aisre/internal/log"
	"github.com/Pierre-Theophile/aisre/internal/projector"
	"github.com/Pierre-Theophile/aisre/internal/query"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
)

// Persistence builds the runner's ports over Postgres: the investigation store's DAOs for the
// rows, and the projector for the event log. The DAO is handed the projector so that a fact, a
// reopen and a label reach the event log in the transaction that writes their rows
// (FR-057a/b/e). A nil projector is built over store.
func Persistence(store *postgres.Store, proj *projector.Projector) runner.Persistence {
	if proj == nil {
		proj = projector.New(store)
	}
	dao := investigationstore.NewInvestigationDAO(store, investigationstore.WithEventLog(proj))
	return runner.Persistence{
		Investigations: dao,
		Ledgers:        investigationstore.NewLedgerDAO(store),
		Lifecycle:      investigationstore.NewLifecycleDAO(store),
		Evidence:       investigationstore.NewEvidenceRecorder(dao),
		Decisions:      decisionLog{proj: proj},
	}
}

// decisionLog is the projector and its log as the runner's DecisionLog.
type decisionLog struct{ proj *projector.Projector }

var _ runner.DecisionLog = decisionLog{}

func (d decisionLog) RegisterSource(ctx context.Context, src event.Source) error {
	return d.proj.RegisterSource(ctx, src)
}

func (d decisionLog) EmitDecisionRecord(ctx context.Context, in record.DecisionInput) (*graphv1.IngestResult, error) {
	return investigationstore.EmitDecisionRecord(ctx, d.proj, in)
}

func (d decisionLog) Iterate(ctx context.Context, fn func(*graphv1.EventEnvelope) error) error {
	return eventlog.New(d.proj.Store()).Iterate(ctx, 0, func(r *eventlog.Record) error { return fn(r.Envelope) })
}

// KnowledgeCorpusOver returns the knowledge worker's corpus over a store.
func KnowledgeCorpusOver(store *postgres.Store) KnowledgeCorpus {
	return KnowledgeCorpus{Engine: query.NewEngine(store)}
}
