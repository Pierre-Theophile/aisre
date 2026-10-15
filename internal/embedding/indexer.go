// SPDX-License-Identifier: Apache-2.0

package embedding

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Pierre-Theophile/aisre/pkg/knowledge"
)

// The indexer (feature 008 FR-028, SC-007, tasks T032).
//
// A loop in `serve` that keeps the knowledge store's vectors current for the configured model: it
// drains Store.Pending, embeds each section as knowledge.EmbeddingText spells it, and stores the
// vectors. Sections arrive from feeders at any time; the indexer is what turns them into something
// the semantic ranking can see, and its lag is what SC-007's 20 minutes is spent on beyond the poll.
//
// **Ready** is the rule FR-028 needs: a model whose backfill has not completed once does not serve.
// Vectors are keyed by model, so a new model starts with none, and until the indexer has drained
// every pending section for it at least once the knowledge worker runs lexical only and says why.
// A search therefore never ranks by a half-built index of one model, and never mixes two: each
// search asks Semantic for exactly one model. Once ready, a section that arrives later is embedded
// on the next tick and is matched lexically until then — the same as any section between polls.

// The indexer's published defaults.
const (
	// DefaultInterval is how often the indexer looks for new sections once it has caught up.
	DefaultInterval = 30 * time.Second
	// drainBatch is how many pending sections one round embeds.
	drainBatch = 4 * BatchSize
	// pendingCountLimit bounds the pending count a status reports.
	pendingCountLimit = 100_000
)

// IndexerStatus is what the indexer reports: for a log line, and for whoever asks whether the
// semantic path may serve.
type IndexerStatus struct {
	// Model is the model vectors are kept for.
	Model string
	// Ready says the backfill for Model has completed at least once.
	Ready bool
	// Pending is how many current sections have no vector yet, as of the last round.
	Pending int
	// Lag is how long sections have been waiting: the time since the indexer was last caught up,
	// zero when nothing is pending.
	Lag time.Duration
	// Embedded counts the vectors stored since start.
	Embedded int
	// LastError is the last round's failure, empty when it succeeded.
	LastError string
}

// Indexer keeps one model's vectors current.
type Indexer struct {
	store    knowledge.Store
	embedder knowledge.Embedder
	logger   *slog.Logger
	interval time.Duration
	now      func() time.Time

	mu         sync.Mutex
	status     IndexerStatus
	caughtUpAt time.Time
}

// IndexerOption configures an Indexer.
type IndexerOption func(*Indexer)

// WithInterval sets how often the indexer looks for new sections.
func WithInterval(d time.Duration) IndexerOption { return func(i *Indexer) { i.interval = d } }

// WithClock replaces time.Now, for tests.
func WithClock(now func() time.Time) IndexerOption { return func(i *Indexer) { i.now = now } }

// NewIndexer returns an indexer of store's sections by embedder's model.
func NewIndexer(store knowledge.Store, embedder knowledge.Embedder, logger *slog.Logger, opts ...IndexerOption) *Indexer {
	if logger == nil {
		logger = slog.Default()
	}
	i := &Indexer{store: store, embedder: embedder, logger: logger, interval: DefaultInterval, now: time.Now}
	for _, opt := range opts {
		opt(i)
	}
	i.status.Model = embedder.Model().String()
	i.caughtUpAt = i.now()
	return i
}

// Ready reports whether the semantic path may serve: the backfill for this model has completed.
func (i *Indexer) Ready() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.status.Ready
}

// Status returns the last round's figures.
func (i *Indexer) Status() IndexerStatus {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.status
}

// Run drains the pending sections every interval until ctx ends. A failed round is logged and
// retried at the next tick; it never stops the loop.
func (i *Indexer) Run(ctx context.Context) {
	i.logger.Info("knowledge: semantic indexer started", "model", i.status.Model, "interval", i.interval.String())
	ticker := time.NewTicker(i.interval)
	defer ticker.Stop()
	for {
		if err := i.Drain(ctx); err != nil && ctx.Err() == nil {
			status := i.Status()
			i.logger.Warn("knowledge: semantic indexing round failed; retrying at the next tick",
				"model", status.Model, "pending", status.Pending, "lag", status.Lag.Round(time.Second), "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Drain embeds every pending section, in rounds, until none is left.
func (i *Indexer) Drain(ctx context.Context) error {
	model := i.embedder.Model()
	wasReady := i.Ready()
	for {
		pending, err := i.store.Pending(ctx, model, pendingCountLimit)
		if err != nil {
			return i.fail(fmt.Errorf("knowledge: pending sections for %s: %w", model, err))
		}
		i.observe(len(pending))
		if len(pending) == 0 {
			i.mu.Lock()
			i.status.Ready, i.status.LastError = true, ""
			i.mu.Unlock()
			if !wasReady {
				i.logger.Info("knowledge: semantic index ready; hybrid retrieval serves from now on", "model", model.String())
			}
			return nil
		}
		batch := pending[:min(drainBatch, len(pending))]
		texts := make([]string, len(batch))
		for j, p := range batch {
			texts[j] = knowledge.EmbeddingText(p.Label, p.Text)
		}
		vectors, err := i.embedder.Embed(ctx, texts, knowledge.PurposeDocument)
		if err != nil {
			return i.fail(err)
		}
		out := make([]knowledge.DigestVector, len(batch))
		for j, p := range batch {
			out[j] = knowledge.DigestVector{Digest: p.Digest, Vector: vectors[j]}
		}
		if err := i.store.PutVectors(ctx, model, out); err != nil {
			return i.fail(fmt.Errorf("knowledge: store vectors for %s: %w", model, err))
		}
		i.mu.Lock()
		i.status.Embedded += len(out)
		i.mu.Unlock()
		i.logger.Debug("knowledge: sections embedded", "model", model.String(), "count", len(out), "pending", len(pending)-len(out))
	}
}

func (i *Indexer) observe(pending int) {
	i.mu.Lock()
	defer i.mu.Unlock()
	now := i.now()
	i.status.Pending = pending
	if pending == 0 {
		i.caughtUpAt = now
		i.status.Lag = 0
		return
	}
	i.status.Lag = now.Sub(i.caughtUpAt)
}

func (i *Indexer) fail(err error) error {
	i.mu.Lock()
	i.status.LastError = err.Error()
	i.mu.Unlock()
	return err
}
