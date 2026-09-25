// SPDX-License-Identifier: Apache-2.0

package record

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// Recording events (contracts/fixture-format.md §"Canonical serialization").
//
// An `events.jsonl` line is one EventEnvelope in canonical protobuf JSON — keys sorted, no
// insignificant whitespace, timestamps in protojson's spelling — plus two fields the schema
// does not have and the log does:
//
//	observedAt   the observed-time lower bound the graph assigned at acceptance. Replay applies
//	             the event with this value and never with the replay clock (FR-023), which is
//	             what makes a replayed graph have the observed intervals the live one had.
//	appendedSeq  the line number from 1.
//
// Only accepted events are written. A refused one goes to `rejected.jsonl` as a bare envelope
// — no observedAt, because the log never assigned one — and its reason code is remembered so
// that WriteManifest can state it under `expect_rejected`, which is what turns "this event was
// refused once" into "this event must always be refused" (SC-009). A DUPLICATE_NOOP is written
// nowhere: the event is already on the file, and a second copy would make the replay assert
// something the graph never did.

// EventsFile and RejectedFile are the two files an EventRecorder writes.
const (
	// EventsFile holds the accepted event stream.
	EventsFile = "events.jsonl"
	// RejectedFile holds the events that must be refused.
	RejectedFile = "rejected.jsonl"
)

// Log-added field names, which a loader strips before unmarshalling the envelope.
const (
	observedAtField  = "observedAt"
	appendedSeqField = "appendedSeq"
)

// EventRecorder is an Emitter that saves what the graph accepted.
type EventRecorder struct {
	inner feeder.Emitter
	dir   string

	mu sync.Mutex
	// seq is the LINE NUMBER of the last line written, which `appendedSeq` must equal
	// (contracts/fixture-format.md). It is seeded from the lines already in the file rather than
	// from zero — see Emitter.
	seq int64
	// written is how many lines THIS recorder wrote. It is separate from seq because a recorder
	// continuing an existing recording starts numbering above zero, and "did this run record
	// anything" is a different question from "how long is the file".
	written  int64
	rejected []Rejection
	err      error
}

var _ feeder.Emitter = (*EventRecorder)(nil)

// Rejection is one refused event, kept so that the manifest can require the refusal.
type Rejection struct {
	// EventID is the refused event's id.
	EventID string
	// ReasonCode is the published reason the graph gave.
	ReasonCode string
	// ReasonDetail names the offending field.
	ReasonDetail string
}

// Emitter tees em's accepted events into `<dir>/events.jsonl` and its refused ones into
// `<dir>/rejected.jsonl`, returning an Emitter a feeder can be run against unchanged.
//
// A write failure is returned from Emit, after the inner emitter has already seen the event: a
// recording that cannot be written must not silently become a run that was not recorded. So is
// ErrBatchingEmitter, on the first event an emitter answers nothing for: `em` must answer per
// event, which means a batch size of 1.
// A recorder CONTINUES an existing `events.jsonl` rather than restarting its numbering. That is
// what a multi-source fixture needs: two feeders recorded into one directory are one arrival stream,
// and `appendedSeq` is the line number of the whole stream (contracts/fixture-format.md). Restarting
// at 1 wrote a file the loader refuses — "appendedSeq is 1 but this is event 28" — which is the
// right refusal and was the only sign that the second recorder did not know about the first.
//
// A count failure is recorded rather than returned, because a constructor that could fail would make
// every call site handle an error for a file that usually does not exist yet. The first Emit reports
// it.
func Emitter(em feeder.Emitter, dir string) *EventRecorder {
	r := &EventRecorder{inner: em, dir: dir}
	lines, err := countLines(filepath.Join(dir, EventsFile))
	if err != nil {
		r.err = err
		return r
	}
	r.seq = lines
	return r
}

// countLines returns how many lines a recording already holds. A missing file is zero, not an error:
// the ordinary case is a fresh directory.
func countLines(path string) (int64, error) {
	f, err := os.Open(path) //nolint:gosec // a fixture path the caller chose
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("record: count %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	var n int64
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), maxEventLine)
	for scanner.Scan() {
		if len(scanner.Bytes()) > 0 {
			n++
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("record: count %s: %w", path, err)
	}
	return n, nil
}

// maxEventLine bounds one recorded event. It is generous on purpose: an event carrying a large
// properties block is legitimate, and a scanner that gave up on one would report a short file and
// number the next line over an existing one.
const maxEventLine = 8 << 20

// Emit forwards the event and records the result.
func (r *EventRecorder) Emit(ctx context.Context, ev *graphv1.EventEnvelope) (*graphv1.IngestResult, error) {
	result, err := r.inner.Emit(ctx, ev)
	if writeErr := r.write(ev, result); writeErr != nil {
		if err == nil {
			return result, writeErr
		}
	}
	return result, err
}

// Checkpoint forwards the checkpoint; the event it produces is recorded like any other,
// because the inner emitter routes it back through Emit.
func (r *EventRecorder) Checkpoint(ctx context.Context, fact feeder.CheckpointFact) error {
	desc := r.describe()
	_, err := r.Emit(ctx, feeder.SourceCheckpoint(desc, feeder.CheckpointID(desc, fact.ExtentTo), fact))
	return err
}

// Flush forwards the flush, and makes sure `events.jsonl` EXISTS even when nothing was emitted.
//
// # Why an empty recording is a recording
//
// The file is otherwise created lazily, on the first write, so a cycle that correctly emitted nothing
// left no file at all — and `fixture verify` cannot read a fixture without one. That made a whole class
// of fixture impossible to express: "this source was read and produced nothing."
//
// It is a class worth having. A connector that over-emits is caught by a fixture whose event stream is
// empty and whose PAYLOAD count is not; without the file, the only way to record such a case was to give
// the fixture something to emit, which is the opposite of the assertion. `vercel-preview-excluded-01` is
// the first of them: three promoted deployments, every one excluded by its platform-stated target, and
// an empty stream that says so.
//
// An empty file and a missing file are different statements, which is the whole point: missing means
// nobody looked, empty means somebody looked and found nothing. Creating it here is what lets a reader
// tell those apart, and it changes nothing for any existing fixture — every one of them emits at least
// one event, so every one already had the file before this.
func (r *EventRecorder) Flush(ctx context.Context) error {
	if err := r.ensureFile(); err != nil {
		return err
	}
	return r.inner.Flush(ctx)
}

// ensureFile creates an empty events.jsonl when nothing has been written to one.
func (r *EventRecorder) ensureFile() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	path := filepath.Join(r.dir, EventsFile)
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return r.fail(fmt.Errorf("record: stat %s: %w", path, err))
	}
	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		return r.fail(fmt.Errorf("record: create %s: %w", r.dir, err))
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644) //nolint:gosec // a fixture path the caller chose
	if err != nil {
		return r.fail(fmt.Errorf("record: create %s: %w", path, err))
	}
	return f.Close()
}

// Accepted is how many events THIS recorder wrote to events.jsonl. A recorder continuing an
// existing recording does not count the lines that were already there.
func (r *EventRecorder) Accepted() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.written
}

// Lines is how long events.jsonl is now, including whatever another recorder wrote into the same
// directory before this one.
func (r *EventRecorder) Lines() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seq
}

// Rejections returns the refused events, in refusal order. WriteManifest turns them into
// `expect_rejected` entries.
func (r *EventRecorder) Rejections() []Rejection {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Rejection(nil), r.rejected...)
}

// Err returns the first write failure, or nil.
func (r *EventRecorder) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

// describe reads the inner emitter's description when it has one, so that Checkpoint can mint
// the same deterministic event id the inner emitter would have.
func (r *EventRecorder) describe() feeder.Description {
	if d, ok := r.inner.(interface{ Describe() feeder.Description }); ok {
		return d.Describe()
	}
	return feeder.Description{}
}

// ErrBatchingEmitter is returned by the first Emit whose inner emitter answered nothing.
//
// A recorder writes a line per event *the graph answered for*, because the observed time it
// records is the one the graph assigned at acceptance (FR-023). A batching emitter has not
// asked yet, so it returns a nil result and there is nothing to write — and the run then
// produces payloads, an empty `events.jsonl` and a directory that looks like a recording. That
// happened on the live run of 2026-09-16 (finding 1) with the batch size the SDK defaults to,
// and cost a cluster run to notice, so it is now an error on the first event rather than a
// surprise at the end.
var ErrBatchingEmitter = errors.New(
	"record: the emitter answered nothing for this event, so there is no observed time to record. " +
		"It is batching: a recording needs one answer per event. Wrap an emitter built with " +
		"emit.WithBatchSize(1) (`--batch-size=1` on the command line), or record closer to the graph")

func (r *EventRecorder) write(ev *graphv1.EventEnvelope, result *graphv1.IngestResult) error {
	if ev == nil {
		return nil
	}
	if result == nil {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.fail(fmt.Errorf("%w (first seen on %s)", ErrBatchingEmitter, ev.GetEventId()))
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	switch result.GetStatus() {
	case graphv1.IngestResult_APPLIED:
		line, err := EventLine(ev, observedAtOf(ev, result), r.seq+1)
		if err != nil {
			return r.fail(err)
		}
		if err := appendLine(filepath.Join(r.dir, EventsFile), line); err != nil {
			return r.fail(err)
		}
		r.seq++
		r.written++
	case graphv1.IngestResult_REJECTED:
		line, err := graph.CanonicalJSON(ev)
		if err != nil {
			return r.fail(fmt.Errorf("record: encode rejected event %s: %w", ev.GetEventId(), err))
		}
		if err := appendLine(filepath.Join(r.dir, RejectedFile), line); err != nil {
			return r.fail(err)
		}
		r.rejected = append(r.rejected, Rejection{
			EventID:      result.GetEventId(),
			ReasonCode:   result.GetReasonCode(),
			ReasonDetail: result.GetReasonDetail(),
		})
	case graphv1.IngestResult_DUPLICATE_NOOP, graphv1.IngestResult_STATUS_UNSPECIFIED:
	}
	return nil
}

func (r *EventRecorder) fail(err error) error {
	if r.err == nil {
		r.err = err
	}
	return err
}

// EventLine renders one events.jsonl line: the canonical envelope plus observedAt and
// appendedSeq. It is exported because a fixture written by any other route must produce
// exactly these bytes.
func EventLine(ev *graphv1.EventEnvelope, observedAt time.Time, appendedSeq int64) ([]byte, error) {
	canonical, err := graph.CanonicalJSON(ev)
	if err != nil {
		return nil, fmt.Errorf("record: encode event %s: %w", ev.GetEventId(), err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(canonical, &fields); err != nil {
		return nil, fmt.Errorf("record: re-read event %s: %w", ev.GetEventId(), err)
	}
	// The canonicalizer normalizes any RFC 3339 string to protojson's timestamp spelling, so
	// writing RFC3339Nano here produces the same bytes a protobuf Timestamp would have.
	stamp, err := json.Marshal(observedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	seq, err := json.Marshal(appendedSeq)
	if err != nil {
		return nil, err
	}
	fields[observedAtField] = stamp
	fields[appendedSeqField] = seq
	return graph.CanonicalJSON(fields)
}

// observedAtOf picks the observed time to record. The graph's own stamp is the only correct
// answer; the fallbacks exist so that an emitter which does not return one — a hand-rolled
// test double — still produces a loadable file rather than one that fails at the first line.
func observedAtOf(ev *graphv1.EventEnvelope, result *graphv1.IngestResult) time.Time {
	if ts := result.GetObservedAt(); ts != nil {
		return ts.AsTime().UTC()
	}
	if ts := ev.GetSourceObservedAt(); ts != nil {
		return ts.AsTime().UTC()
	}
	return time.Now().UTC()
}
