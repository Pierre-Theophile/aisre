// SPDX-License-Identifier: Apache-2.0

package emit

import (
	"context"
	"fmt"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/knowledge"
	"github.com/Pierre-Theophile/aisre/pkg/knowledge/memory"
)

// MemoryEmitter is a graph that only remembers (contracts/feeder-sdk.md §Recording and
// testing).
//
// It answers like the real one: it validates every event with the same validator the server
// runs, refuses a second delivery of a seen idempotency key with DUPLICATE_NOOP, and stamps an
// observed time. What it does not do is project anything, which is exactly why a feeder's unit
// tests can use it — a connector author gets the whole rejection vocabulary with no database,
// no container and no network.
//
// It is strict by default: a refused event comes back as both a REJECTED result and an error.
// The server returns only the result, because a live feeder must survive one bad event in ten
// thousand; a test must not. WithStrict(false) restores the server's behaviour for a feeder
// whose tests deliberately exercise the rejection path.

// MemoryEmitter collects events in memory, validating each one.
type MemoryEmitter struct {
	desc   feeder.Description
	strict bool
	now    func() time.Time

	mu       sync.Mutex
	events   []*graphv1.EventEnvelope
	results  []*graphv1.IngestResult
	rejected []*graphv1.IngestResult
	seenKeys map[string]bool
	flushes  int
	sections *memory.Store
	puts     []SectionsPut
}

var (
	_ feeder.Emitter         = (*MemoryEmitter)(nil)
	_ feeder.SectionsEmitter = (*MemoryEmitter)(nil)
)

// SectionsPut is one PutSections call a MemoryEmitter received.
type SectionsPut struct {
	DocumentRef string
	Sections    []knowledge.Section
	// AfterEvents is how many events had been accepted when the sections arrived, so that a test can
	// assert a feeder sends a document's sections after its events.
	AfterEvents int
	Result      knowledge.PutResult
}

// MemoryOption configures a MemoryEmitter.
type MemoryOption func(*MemoryEmitter)

// WithStrict sets whether a refused event also returns an error. Default true; see the type's
// documentation for why.
func WithStrict(strict bool) MemoryOption {
	return func(m *MemoryEmitter) { m.strict = strict }
}

// WithMemoryClock replaces the source of observed time, so that a test comparing results does
// not have to tolerate a moving clock.
func WithMemoryClock(now func() time.Time) MemoryOption {
	return func(m *MemoryEmitter) { m.now = now }
}

// NewMemoryEmitter returns an emitter that keeps everything it is given.
func NewMemoryEmitter(desc feeder.Description, opts ...MemoryOption) *MemoryEmitter {
	m := &MemoryEmitter{
		desc:     desc,
		strict:   true,
		now:      func() time.Time { return time.Now().UTC() },
		seenKeys: map[string]bool{},
		sections: memory.New(),
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Describe returns the description the emitter was built for.
func (m *MemoryEmitter) Describe() feeder.Description { return m.desc }

// Emit validates the event and records it.
//
// The result is APPLIED for a new, valid event; DUPLICATE_NOOP for one whose idempotency key
// has been seen; REJECTED, with the reason code the server would have used, for one that does
// not validate. In strict mode a REJECTED result is also returned as an error.
func (m *MemoryEmitter) Emit(_ context.Context, ev *graphv1.EventEnvelope) (*graphv1.IngestResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if r := feeder.Validate(ev); r != nil {
		result := &graphv1.IngestResult{
			EventId:      ev.GetEventId(),
			Status:       graphv1.IngestResult_REJECTED,
			ReasonCode:   r.ReasonCode,
			ReasonDetail: r.ReasonDetail,
		}
		m.results = append(m.results, result)
		m.rejected = append(m.rejected, result)
		if m.strict {
			return result, r
		}
		return result, nil
	}
	// FR-046 in miniature: an emitter belongs to one feeder, and an event claiming another
	// source id is the mistake a shared emitter makes. The server refuses it with a
	// permission error; here it is a plain error, because there is no token to blame.
	if m.desc.SourceID != "" && ev.GetSourceId() != m.desc.SourceID {
		return nil, &feeder.Rejection{
			ReasonCode: feeder.ReasonUnknownSource,
			ReasonDetail: "event " + ev.GetEventId() + " claims source " + ev.GetSourceId() +
				" but this emitter is scoped to " + m.desc.SourceID,
		}
	}

	key := ev.GetIdempotencyKey()
	if key == "" {
		key = ev.GetEventId()
	}
	observedAt := timestamppb.New(m.now().UTC())
	if m.seenKeys[key] {
		result := &graphv1.IngestResult{
			EventId:    ev.GetEventId(),
			Status:     graphv1.IngestResult_DUPLICATE_NOOP,
			ObservedAt: observedAt,
		}
		m.results = append(m.results, result)
		return result, nil
	}
	m.seenKeys[key] = true

	// The envelope is cloned: a feeder that reuses a message across loop iterations would
	// otherwise leave this emitter holding a slice of the same mutated pointer.
	stored, ok := proto.Clone(ev).(*graphv1.EventEnvelope)
	if !ok { // unreachable: Clone preserves the dynamic type
		stored = ev
	}
	m.events = append(m.events, stored)
	result := &graphv1.IngestResult{
		EventId:    ev.GetEventId(),
		Status:     graphv1.IngestResult_APPLIED,
		ObservedAt: observedAt,
	}
	m.results = append(m.results, result)
	return result, nil
}

// Checkpoint records the feeder's observed extent as a source_checkpoint event.
func (m *MemoryEmitter) Checkpoint(ctx context.Context, fact feeder.CheckpointFact) error {
	_, err := m.Emit(ctx, feeder.SourceCheckpoint(m.desc, feeder.CheckpointID(m.desc, fact.ExtentTo), fact))
	return err
}

// Flush is a no-op: nothing is buffered. It counts calls so that a test can assert its feeder
// flushes before returning from Run.
func (m *MemoryEmitter) Flush(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.flushes++
	return nil
}

// PutSections validates and keeps a document's sections in an in-memory knowledge store, refusing
// what the server would: invalid sections, and a document outside the emitter's source's namespace.
func (m *MemoryEmitter) PutSections(ctx context.Context, documentRef string, sections []knowledge.Section) (knowledge.PutResult, error) {
	if !knowledge.OwnsDocument(m.desc.SourceID, documentRef) {
		return knowledge.PutResult{}, fmt.Errorf("emit: source %q does not own document %q", m.desc.SourceID, documentRef)
	}
	if err := knowledge.ValidateFeederSections(sections); err != nil {
		return knowledge.PutResult{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	result, err := m.sections.Put(ctx, documentRef, sections, m.now())
	if err != nil {
		return knowledge.PutResult{}, err
	}
	m.puts = append(m.puts, SectionsPut{DocumentRef: documentRef, Sections: append([]knowledge.Section(nil), sections...),
		AfterEvents: len(m.events), Result: result})
	return result, nil
}

// SectionsPuts returns the PutSections calls, in call order.
func (m *MemoryEmitter) SectionsPuts() []SectionsPut {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]SectionsPut(nil), m.puts...)
}

// KnowledgeStore is the in-memory store the sections were put in.
func (m *MemoryEmitter) KnowledgeStore() *memory.Store { return m.sections }

// Events returns the accepted events, in emit order. A duplicate is not among them: it was
// answered DUPLICATE_NOOP and changed nothing, exactly as the graph would have.
func (m *MemoryEmitter) Events() []*graphv1.EventEnvelope {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*graphv1.EventEnvelope(nil), m.events...)
}

// Results returns one result per Emit call, in call order.
func (m *MemoryEmitter) Results() []*graphv1.IngestResult {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*graphv1.IngestResult(nil), m.results...)
}

// Rejected returns the refused results, which a test asserts is empty.
func (m *MemoryEmitter) Rejected() []*graphv1.IngestResult {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*graphv1.IngestResult(nil), m.rejected...)
}

// Flushes is how many times Flush was called.
func (m *MemoryEmitter) Flushes() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.flushes
}

// Reset forgets everything, including the seen idempotency keys. Use it between two runs that
// must each start from nothing; to check double delivery, keep the emitter instead.
func (m *MemoryEmitter) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = nil
	m.results = nil
	m.rejected = nil
	m.seenKeys = map[string]bool{}
	m.flushes = 0
	m.sections = memory.New()
	m.puts = nil
}
