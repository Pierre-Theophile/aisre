// SPDX-License-Identifier: Apache-2.0

// Package deployrecord records a live deploy-feeder run without writing an unsanitised byte
// (004 T104, FR-062, FR-137).
//
// ---------------------------------------------------------------------------------------------
// The shape
//
// Tee sits between the live poller and the feeder. The feeder receives every payload exactly as the
// platform sent it — the graph is built from the real estate. The recording receives the SANITISED
// payload and nothing else: each one goes through sanitise.Sanitiser.JSON before any byte is written,
// under the contract table and the corpus key. There is no code path in which the platform's bytes
// reach the recording's writer, because the writer is only ever handed the sanitiser's output.
//
// A payload the sanitiser refuses — a field no row names — is not written at all and is counted, with
// the field that stopped it, for the manifest (FR-064's "a payload that cannot be made safe MUST be
// dropped and the drop documented"). A canary reaching an output is not a payload problem, it is a
// broken gate, and it ends the run.
//
// # Why the recording's events are derived rather than tee'd
//
// The events the live feeder emits carry the real identifiers, so recording them would be recording
// the estate. Finish derives `events.jsonl` instead by running a second feeder over the sanitised
// payloads on disk, with the operator's mapping pseudonymised under the same key. So the recording's
// events are exactly what its payloads replay to: "the graph built from the recording" is a graph built
// from nothing but sanitised bytes.
package deployrecord

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/Pierre-Theophile/aisre/internal/sanitise"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/emit"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/record"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/source"
)

// Tee is a Source that records the sanitised form of everything it passes on unchanged.
type Tee struct {
	inner    feeder.Source
	platform string
	san      *sanitise.Sanitiser
	out      *record.PayloadRecorder

	// prepare, when set, rewrites a payload before the policy pass: for a vendor whose identifiers
	// also live inside free text (a Datadog monitor's query, its group keys and tags) and must be
	// pseudonymised with the same key as the fields that name them, so the recording still joins.
	prepare func(kind string, raw []byte) ([]byte, error)

	mu      sync.Mutex
	dropped map[string]int
}

// WithPrepare sets a pre-pass run on each payload before the policy's; its output, never the input,
// goes on to the sanitiser. A pre-pass that fails drops the payload like a policy refusal.
func (t *Tee) WithPrepare(prepare func(kind string, raw []byte) ([]byte, error)) *Tee {
	t.prepare = prepare
	return t
}

var _ feeder.Source = (*Tee)(nil)

// NewTee builds the tee. A nil sanitiser is refused: a recorder that writes whatever it is handed is
// what FR-137 exists to prevent.
func NewTee(inner feeder.Source, platform string, san *sanitise.Sanitiser, dir string) (*Tee, error) {
	switch {
	case inner == nil:
		return nil, errors.New("deployrecord: a tee with no source")
	case san == nil:
		return nil, errors.New("deployrecord: a recording with no sanitiser; sanitisation happens in the " +
			"connector before anything touches disk, and a recording taken without it cannot be cleaned " +
			"afterwards (FR-062, FR-137)")
	case platform == "":
		return nil, errors.New("deployrecord: a tee with no platform; the policy rows are rooted at it")
	case dir == "":
		return nil, errors.New("deployrecord: a tee with no recording directory")
	}
	return &Tee{inner: inner, platform: platform, san: san, out: record.Writer(dir), dropped: map[string]int{}}, nil
}

// Next hands the live feeder the platform's payload, having recorded its sanitised form.
func (t *Tee) Next(ctx context.Context) (feeder.Payload, error) {
	p, err := t.inner.Next(ctx)
	if err != nil {
		return p, err
	}
	raw := p.Bytes
	if t.prepare != nil {
		if raw, err = t.prepare(p.Kind, raw); err != nil {
			t.mu.Lock()
			t.dropped["the pre-pass refused it: "+err.Error()]++
			t.mu.Unlock()
			return p, nil
		}
	}
	clean, err := t.san.JSON(t.platform+"."+p.Kind, raw)
	if err != nil {
		var canary *sanitise.CanarySurvivedError
		if errors.As(err, &canary) {
			return p, err
		}
		t.mu.Lock()
		t.dropped[droppedReason(err)]++
		t.mu.Unlock()
		return p, nil
	}
	if err := t.out.Record(feeder.Payload{Kind: p.Kind, At: p.At, Seq: p.Seq, Bytes: clean}); err != nil {
		return p, err
	}
	return p, nil
}

func droppedReason(err error) string {
	var unassigned *sanitise.UnassignedError
	if errors.As(err, &unassigned) {
		return "field " + unassigned.Field + " has no disposition"
	}
	return err.Error()
}

// Written is how many sanitised payloads reached the recording.
func (t *Tee) Written() int { return t.out.Count() }

// Dropped is the payloads refused, by reason, sorted.
func (t *Tee) Dropped() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, 0, len(t.dropped))
	for reason, n := range t.dropped {
		out = append(out, fmt.Sprintf("%d payload(s): %s", n, reason))
	}
	sort.Strings(out)
	return out
}

// Err is the first write failure, or nil.
func (t *Tee) Err() error { return t.out.Err() }

// Finish derives the recording's events from its sanitised payloads with shadow, a feeder configured
// with the pseudonymised mapping, and writes the manifest. It is safe on a recording that holds no
// payload, which it reports rather than writing an empty fixture.
func (t *Tee) Finish(ctx context.Context, dir, family string, shadow feeder.Feeder) (events int64, err error) {
	if err := t.out.Err(); err != nil {
		return 0, err
	}
	if t.out.Count() == 0 {
		return 0, fmt.Errorf("deployrecord: no payload was recorded (%d refused); nothing to derive events from",
			len(t.Dropped()))
	}
	src, err := source.NewFileSource(dir)
	if err != nil {
		return 0, err
	}
	desc := shadow.Describe()
	recorder := record.Emitter(emit.NewMemoryEmitter(desc), dir)
	if err := shadow.Run(ctx, src, recorder); err != nil {
		return 0, fmt.Errorf("deployrecord: deriving the recording's events: %w", err)
	}
	if err := recorder.Err(); err != nil {
		return 0, err
	}
	description := fmt.Sprintf("%d %s payloads recorded live and sanitised in the connector under policy %s "+
		"(corpus key %s) before any byte was written; the events are derived from the sanitised payloads, "+
		"not recorded from the live run.", t.out.Count(), t.platform, t.san.Policy().Version(), t.san.Key().Fingerprint())
	if dropped := t.Dropped(); len(dropped) > 0 {
		description += fmt.Sprintf(" Payloads refused rather than written: %v.", dropped)
	}
	if err := record.WriteManifest(dir, record.Manifest{
		Family:         family,
		Description:    description + " Add a queries: list and record the goldens before committing.",
		Sources:        []record.ManifestSource{record.SourceOf(desc)},
		ExpectRejected: recorder.Rejections(),
	}); err != nil {
		return 0, err
	}
	return recorder.Accepted(), nil
}
