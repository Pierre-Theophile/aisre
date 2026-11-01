// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/event"
	"github.com/Pierre-Theophile/aisre/internal/investigation/intake"
	"github.com/Pierre-Theophile/aisre/internal/investigation/ledger"
	"github.com/Pierre-Theophile/aisre/internal/investigation/record"
)

// The runner's ports. The runner decides what an investigation does and what it writes down; where
// that is kept is the adapter's business. internal/investigation/pginvestigation implements every
// port below over Postgres and the projector, and the composition root hands the result in through
// Config.Persistence.

// Investigations keeps the investigation rows: the incident and its symptoms, the run, its spend
// and the reopen a later fact causes.
type Investigations interface {
	// Open records an incident (once), a new investigation of it and the symptoms that opened it,
	// and returns the investigation id.
	Open(ctx context.Context, incident record.Incident, inv record.NewInvestigation, symptoms ...*investigationv1.Symptom) (string, error)
	// Attach adds a symptom to an open incident and returns the investigation it joined.
	Attach(ctx context.Context, incidentID string, s *investigationv1.Symptom) (string, error)
	// Get reads one investigation with its children.
	Get(ctx context.Context, investigationID string) (*investigationv1.Investigation, error)
	// InvestigationForKey finds the investigation an idempotency key already opened.
	InvestigationForKey(ctx context.Context, key string) (string, bool, error)
	// OpenIncidents lists the incidents still open at at, within the association window.
	OpenIncidents(ctx context.Context, at time.Time, within time.Duration) ([]intake.OpenIncident, error)
	// ReopenWithFact opens child as the reopen of a concluded parent, with the fact that caused it.
	ReopenWithFact(ctx context.Context, parentID string, child record.NewInvestigation, fact *investigationv1.HumanFact) (*investigationv1.HumanFact, error)
	// SaveSpend records what a run consumed under its budget profile.
	SaveSpend(ctx context.Context, investigationID, profile string, spend *investigationv1.BudgetSpend) error
	// SpendOf reads what a run consumed.
	SpendOf(ctx context.Context, investigationID string) (*investigationv1.BudgetSpend, error)
}

// Ledgers keeps the ledger: hypotheses, judgments and evidence.
type Ledgers interface {
	Save(ctx context.Context, l *ledger.Ledger) error
	Load(ctx context.Context, investigationID string) (*record.LedgerRows, error)
}

// Lifecycle moves an investigation through its states. Both moves are guarded: an investigation
// that is not running returns record.ErrNotRunning.
type Lifecycle interface {
	Conclude(ctx context.Context, investigationID string, c record.Conclusion) error
	Fail(ctx context.Context, investigationID, detail string) error
}

// Evidence records the calls a run made and the evidence each one produced.
type Evidence interface {
	Record(ctx context.Context, investigationID string, call record.WorkerCall, item ledger.EvidenceItem) error
	RecordModelCall(ctx context.Context, investigationID string, call record.ModelCall) error
}

// DecisionLog is the graph's event log as the runner sees it: where the decision record goes, and
// what an export replays.
type DecisionLog interface {
	// RegisterSource makes a source appendable.
	RegisterSource(ctx context.Context, src event.Source) error
	// EmitDecisionRecord appends and projects the decision record once, and links the
	// investigation to it (FR-035).
	EmitDecisionRecord(ctx context.Context, in record.DecisionInput) (*graphv1.IngestResult, error)
	// Iterate calls fn for every event in the log, in append order.
	Iterate(ctx context.Context, fn func(env *graphv1.EventEnvelope) error) error
}

// Persistence is every port the runner writes through.
type Persistence struct {
	Investigations Investigations
	Ledgers        Ledgers
	Lifecycle      Lifecycle
	Evidence       Evidence
	Decisions      DecisionLog
}

func (p Persistence) complete() bool {
	return p.Investigations != nil && p.Ledgers != nil && p.Lifecycle != nil && p.Evidence != nil && p.Decisions != nil
}
