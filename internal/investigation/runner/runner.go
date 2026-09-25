// SPDX-License-Identifier: Apache-2.0

// Package runner is the investigation, productised (Phase 7 Track D; FR-064, FR-067).
//
// Everything this package does exists already, somewhere: intake normalises the two front doors,
// the engine reasons, the store persists, the recorder writes the trajectory. What did not exist
// is the *assembly* — the single object that takes an `InvestigateRequest` and leaves behind a
// concluded row, a saved ledger, an evidence chain, a trajectory on disk and exactly one decision
// record in the graph. Without it `serve --enable-investigation` could read investigations and
// could not run one, which is the state `NewInvestigationService(nil, …)` left the binary in.
//
// Three properties are worth stating up front, because each is a decision rather than a detail.
//
// **A nil model client is a supported configuration, not a degraded one.** With no client the run
// is the provisional prior-only ranking, the onset estimate, the causal ordering and the
// deterministic first wave — the MVP — and it needs no API key, no network to a vendor and no
// credential of any kind (FR-067). Every test in this package runs that way.
//
// **The runner never writes to a production system** (constitution VII). Its only writes are
// schema `investigation`, one `record_investigation` event in the graph's own log, and files
// under the recording root. It reads the graph and it reads recorded or synthetic telemetry; it
// has no path to a vendor's write API because it holds no client that has one.
//
// **A human is never waited for** (FR-057). There is no state in which the loop blocks: a fact
// pushed at a running investigation is evidence it may or may not reach in time, and a fact
// pushed at a concluded one reopens it into a *new linked row* (FR-057b). Context cancellation is
// therefore the only thing that can stop a run from outside, and it lands as `failed` with a
// typed detail rather than as a row left `running` forever.
package runner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/Pierre-Theophile/aisre/internal/investigation/audit"
	"github.com/Pierre-Theophile/aisre/internal/investigation/budget"
	"github.com/Pierre-Theophile/aisre/internal/investigation/engine"
	"github.com/Pierre-Theophile/aisre/internal/investigation/intake"
	"github.com/Pierre-Theophile/aisre/internal/investigation/ledger"
	"github.com/Pierre-Theophile/aisre/internal/investigation/model"
	investigationstore "github.com/Pierre-Theophile/aisre/internal/investigation/store"
	graphworker "github.com/Pierre-Theophile/aisre/internal/investigation/workers/graph"
	eventlog "github.com/Pierre-Theophile/aisre/internal/log"
	"github.com/Pierre-Theophile/aisre/internal/projector"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
	"github.com/Pierre-Theophile/aisre/pkg/worker"
)

// SchemaVersion is the version of schema `investigation` a run records on its own row, so that a
// historical investigation stays interpretable after the schema next moves
// (data-model §Schema investigation).
const SchemaVersion = "1.0.0"

// DefaultProfileName is the budget profile a request that names none runs under. `page` is the
// right default for the same reason the pager is: a run nobody qualified is a run somebody is
// waiting on.
const DefaultProfileName = "page"

// Config is everything the runner is wired with.
//
// Only Store, Graph and Workers are required. Everything else has a published default, and the
// defaults are chosen so that a runner built with the minimum is a runner that works with no
// vendor account and no network (FR-067).
type Config struct {
	// Store is the database holding schema `investigation` and the graph. Required.
	Store *postgres.Store
	// Projector writes the one event an investigation emits (FR-035). Built over Store when nil.
	Projector *projector.Projector
	// Graph is feature 001's published read surface. It answers intake's resolution audit and
	// extent consultation and backs the graph worker. Required — an engine that cannot read the
	// graph has no subject and no candidates. GraphOver builds one from a store.
	Graph graphworker.QueryService
	// Workers is the worker registry the engine calls. Required. The graph worker is registered
	// over Graph when the registry does not already declare one, so a caller only has to supply
	// the telemetry workers.
	Workers *worker.Registry
	// Profiles are the budget profiles by name (budget.LoadProfiles). Empty means the published
	// two.
	Profiles map[string]budget.Profile
	// OperatorCaps are the deployment's own ceilings, applied over whichever profile is chosen.
	OperatorCaps budget.OperatorCaps
	// Model is the model boundary. **Nil is legal and is the MVP**: the run is the provisional
	// ranking, the causal ordering and the deterministic first wave, with no model in the loop
	// and no API key anywhere (FR-067, tasks.md §MVP).
	Model *model.Client
	// RecordingRoot is the directory trajectories are written under, as
	// `<root>/<investigation-id>/trajectories/<run-id>.jsonl` (FR-042a). Empty writes none and
	// says so on the row rather than pretending one exists.
	RecordingRoot string
	// WorldDir is the recorded world the telemetry backend answers from, where the deployment
	// serves one (`serve --recording-root`). It is not where anything is *written*: it is
	// recorded so that an export can carry the layer 2 the run actually read (FR-042). Empty
	// means "work it out from RecordingRoot", which is what every caller that serves a world
	// out of the recording root gets for free.
	WorldDir string
	// Prior is π₀ from the published coverage audit (ADR-0005 D9). The zero value means "no
	// audit has been published", which the ledger renders as the open hypothesis holding all the
	// mass — the only honest reading (FR-069a).
	Prior audit.PriorRecord
	// Neighbourhood answers the association rule's graph-distance question (FR-008c). Nil means
	// intake.SameEntityNeighbourhood: two symptoms group when they resolved to the same entity
	// and not otherwise, which is the conservative reading and the one that never merges two
	// unrelated outages.
	Neighbourhood intake.Neighbourhood
	// GroupingRule is the association rule in force. The zero value means the published one.
	GroupingRule intake.GroupingRule
	// Mode is stamped on every worker call (FR-015). Empty means `recorded`, because this build
	// ships no live telemetry backend; feature 003's GCP backend is the first that sets `live`.
	Mode worker.Mode
	// Links turns an algebra request into a link a person can follow (FR-057d). Nil means
	// engine.NoDeepLinks, which states why there is none rather than leaving the field blank.
	Links engine.DeepLinker
	// MaxTurns bounds the model loop. Zero means engine.DefaultMaxTurns.
	MaxTurns int
	// Clock is injectable so a test's instants are as deterministic as its digests.
	Clock func() time.Time
	// Logger is where the runner's own notices go. Nil means slog.Default().
	Logger *slog.Logger
}

// Runner implements server.Runner.
type Runner struct {
	cfg       Config
	dao       *investigationstore.InvestigationDAO
	ledgerDAO *investigationstore.LedgerDAO
	lifecycle *investigationstore.LifecycleDAO
	recorder  *investigationstore.EvidenceRecorder
	proj      *projector.Projector
	workers   *engine.Workers
	profiles  map[string]budget.Profile
	clock     func() time.Time
	logger    *slog.Logger
	// modelConfig is the recorded production model configuration (FR-061), built once because it
	// does not change between runs.
	modelConfig *structpb.Struct
}

// New builds the runner.
//
// It registers the decision-record source eagerly rather than at conclusion. A source must be
// registered before it may append, and discovering that at the end of a five-minute
// investigation would throw the run's conclusion away over a configuration problem that was
// knowable at startup.
func New(cfg Config) (*Runner, error) {
	switch {
	case cfg.Store == nil:
		return nil, errors.New("runner: a store is required")
	case cfg.Graph == nil:
		return nil, errors.New("runner: a graph read surface is required; intake resolves the subject " +
			"through it before anything is asked (FR-003)")
	case cfg.Workers == nil:
		return nil, errors.New("runner: a worker registry is required; the engine reads nothing directly")
	}
	if cfg.Clock == nil {
		cfg.Clock = func() time.Time { return time.Now().UTC() }
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Mode == "" {
		cfg.Mode = worker.ModeRecorded
	}
	if !cfg.Mode.Valid() {
		return nil, fmt.Errorf("runner: mode %q is neither %q nor %q (FR-015)",
			cfg.Mode, worker.ModeLive, worker.ModeRecorded)
	}
	if cfg.Links == nil {
		cfg.Links = engine.NoDeepLinks{}
	}
	if cfg.Neighbourhood == nil {
		cfg.Neighbourhood = intake.SameEntityNeighbourhood{}
	}
	proj := cfg.Projector
	if proj == nil {
		proj = projector.New(cfg.Store)
	}
	profiles := cfg.Profiles
	if len(profiles) == 0 {
		profiles = budget.Published()
	}

	// The graph worker is the one worker the runner can build itself, because it is a typed
	// wrapper over the read surface the config already carries. A caller that registered its own
	// keeps it.
	if _, ok := cfg.Workers.Lookup(graphworker.Name); !ok {
		if err := cfg.Workers.Register(graphworker.New(cfg.Graph)); err != nil {
			return nil, fmt.Errorf("runner: register the graph worker: %w", err)
		}
	}
	workers, err := engine.NewWorkers(cfg.Workers, worker.RetryPolicy{})
	if err != nil {
		return nil, fmt.Errorf("runner: index the worker registry: %w", err)
	}

	// The DAO is handed the projector so that a fact, a reopen and a label reach the event log
	// in the transaction that writes their rows (FR-057a/b/e, human_events.go).
	dao := investigationstore.NewInvestigationDAO(cfg.Store, investigationstore.WithEventLog(proj))
	r := &Runner{
		cfg:         cfg,
		dao:         dao,
		ledgerDAO:   investigationstore.NewLedgerDAO(cfg.Store),
		lifecycle:   investigationstore.NewLifecycleDAO(cfg.Store),
		recorder:    investigationstore.NewEvidenceRecorder(dao),
		proj:        proj,
		workers:     workers,
		profiles:    profiles,
		clock:       cfg.Clock,
		logger:      cfg.Logger,
		modelConfig: modelConfigStruct(cfg.Model),
	}
	if err := r.registerDecisionSource(context.Background()); err != nil {
		return nil, err
	}
	if cfg.Model == nil {
		cfg.Logger.Info("investigation runner: no model configured; every run is the prior-only " +
			"ranking, the causal ordering and the deterministic first wave (FR-067)")
	}
	return r, nil
}

// DAO exposes the investigation DAO, for a caller that has to read what a run left behind
// without building a second one.
func (r *Runner) DAO() *investigationstore.InvestigationDAO { return r.dao }

// registerDecisionSource makes `investigation:engine` an appendable source. It is idempotent —
// the insert is ON CONFLICT DO NOTHING — so building two runners over one database is not an
// error.
func (r *Runner) registerDecisionSource(ctx context.Context) error {
	err := r.proj.RegisterSource(ctx, eventlog.Source{
		SourceID:      investigationstore.DecisionRecordSourceID,
		Kind:          "investigation",
		Ordering:      "none",
		SchemaVersion: investigationstore.DecisionRecordSchemaVersion,
	})
	if err != nil {
		return fmt.Errorf("runner: register the decision-record source: %w", err)
	}
	return nil
}

func (r *Runner) now() time.Time { return r.clock().UTC() }

// profileFor selects the budget profile a request runs under: the name it asked for, the
// priority's published mapping when it asked for none, and the published `page` when neither
// names one the deployment has.
func (r *Runner) profileFor(name, priority string) budget.Profile {
	if name != "" {
		if p, ok := r.profiles[name]; ok {
			return p
		}
		r.logger.Warn("investigation runner: unknown budget profile; falling back to the published default",
			"asked", name, "using", DefaultProfileName)
	}
	if priority != "" {
		byPriority := budget.ForPriority(priority)
		if p, ok := r.profiles[byPriority.Name]; ok {
			return p
		}
		return byPriority
	}
	if p, ok := r.profiles[DefaultProfileName]; ok {
		return p
	}
	return budget.PageProfile()
}

// modelConfigStruct renders the model configuration the run records (FR-061). A model-free run
// records that it was model-free rather than recording nothing, so a reader of the row can tell
// "no model" from "we forgot to write it down".
func modelConfigStruct(client *model.Client) *structpb.Struct {
	if client == nil {
		out, err := structpb.NewStruct(map[string]any{
			"model_free": true,
			"note": "no model was configured; the run is the prior-only ranking, the causal " +
				"ordering and the deterministic first wave (FR-067)",
		})
		if err != nil {
			return nil
		}
		return out
	}
	config := client.Config()
	roles := map[string]any{}
	for role, rc := range config.Roles {
		roles[string(role)] = map[string]any{
			"model":            rc.Model,
			"effort":           rc.Effort,
			"thinking_display": rc.ThinkingDisplay,
			"purpose":          rc.Purpose,
		}
	}
	betas := make([]any, 0, len(config.Betas))
	for _, beta := range config.Betas {
		betas = append(betas, beta)
	}
	out, err := structpb.NewStruct(map[string]any{
		"version":             config.Version,
		"roles":               roles,
		"betas":               betas,
		"price_table_version": config.PriceTableVersion,
	})
	if err != nil {
		return nil
	}
	return out
}

// ledgerRuleVersion is the published ledger rule in force, recorded on every row.
const ledgerRuleVersion = ledger.LedgerRuleVersion
