// SPDX-License-Identifier: Apache-2.0

package emit

import (
	"context"
	"fmt"
	"sync"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	eventlog "github.com/Pierre-Theophile/aisre/internal/log"
	"github.com/Pierre-Theophile/aisre/internal/projector"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The in-process path.
//
// ProjectorEmitter skips the network and applies events to a graph in this process. Two
// callers want that: the conformance testkit, which needs real DUPLICATE_NOOP answers to prove
// a feeder is idempotent, and an offline run that replays a recording into a database directly
// — `aisre fixture record` is that, without a server.
//
// The projector it applies to is an internal type, so this constructor is usable from inside
// this repository and from nothing else. That is deliberate: an out-of-tree connector talks to
// a graph over its published RPC surface (ConnectEmitter), never to its storage layer.

// ProjectorEmitter applies a feeder's events directly to a graph in this process.
type ProjectorEmitter struct {
	desc feeder.Description
	p    *projector.Projector
	now  func() time.Time

	mu         sync.Mutex
	registered bool
	results    []*graphv1.IngestResult
	rejected   []*graphv1.IngestResult
}

var _ feeder.Emitter = (*ProjectorEmitter)(nil)

// ProjectorOption configures a ProjectorEmitter.
type ProjectorOption func(*ProjectorEmitter)

// WithProjectorClock pins the observed time stamped on each event. The default leaves it to
// the graph, which is what a live feeder always wants; a recording pins it so that replaying
// the recording reproduces the same observed intervals (FR-023).
func WithProjectorClock(now func() time.Time) ProjectorOption {
	return func(e *ProjectorEmitter) { e.now = now }
}

// NewProjectorEmitter returns an emitter over p. The feeder's source is registered on the
// first event.
func NewProjectorEmitter(p *projector.Projector, desc feeder.Description, opts ...ProjectorOption) *ProjectorEmitter {
	e := &ProjectorEmitter{desc: desc, p: p}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// Describe returns the description the emitter was built for.
func (e *ProjectorEmitter) Describe() feeder.Description { return e.desc }

// Register declares the feeder's contract to the graph (FR-018).
func (e *ProjectorEmitter) Register(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.registerLocked(ctx)
}

func (e *ProjectorEmitter) registerLocked(ctx context.Context) error {
	if e.registered {
		return nil
	}
	err := e.p.RegisterSource(ctx, eventlog.Source{
		SourceID:         e.desc.SourceID,
		Kind:             e.desc.Kind,
		Ordering:         e.desc.Ordering.String(),
		ReorderingWindow: e.desc.ReorderingWindow,
		SchemaVersion:    e.desc.Version(),
	})
	if err != nil {
		return fmt.Errorf("emit: register source %s: %w", e.desc.SourceID, err)
	}
	e.registered = true
	return nil
}

// Emit applies one event and returns the graph's real answer, rejections included.
func (e *ProjectorEmitter) Emit(ctx context.Context, ev *graphv1.EventEnvelope) (*graphv1.IngestResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.registerLocked(ctx); err != nil {
		return nil, err
	}

	var observedAt time.Time
	if e.now != nil {
		observedAt = e.now().UTC()
	}
	result, err := e.p.Apply(ctx, ev, observedAt)
	if err != nil {
		return nil, fmt.Errorf("emit: apply %s: %w", ev.GetEventId(), err)
	}
	e.results = append(e.results, result)
	if result.GetStatus() == graphv1.IngestResult_REJECTED {
		e.rejected = append(e.rejected, result)
	}
	return result, nil
}

// Checkpoint records the feeder's observed extent as a source_checkpoint event (FR-032).
func (e *ProjectorEmitter) Checkpoint(ctx context.Context, fact feeder.CheckpointFact) error {
	_, err := e.Emit(ctx, feeder.SourceCheckpoint(e.desc, feeder.CheckpointID(e.desc, fact.ExtentTo), fact))
	return err
}

// Flush is a no-op: every event was applied as it arrived, in its own transaction.
func (e *ProjectorEmitter) Flush(context.Context) error { return nil }

// Results returns one result per Emit call, in call order.
func (e *ProjectorEmitter) Results() []*graphv1.IngestResult {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]*graphv1.IngestResult(nil), e.results...)
}

// Rejected returns the refused results.
func (e *ProjectorEmitter) Rejected() []*graphv1.IngestResult {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]*graphv1.IngestResult(nil), e.rejected...)
}

// Reset forgets the recorded results, leaving the graph alone. A double-delivery check resets
// between the two runs so that the second run's results stand on their own.
func (e *ProjectorEmitter) Reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.results = nil
	e.rejected = nil
}
