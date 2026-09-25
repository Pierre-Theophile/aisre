// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"fmt"

	"github.com/Pierre-Theophile/aisre/internal/sanitise"
)

// The recording boundary (T042, FR-137, contract §5 call site 1).
//
// FR-137 says sanitisation happens in the connector, before anything touches disk — and that it is
// a statement about the code path, not about a commit hook. A commit hook cannot help here: by the
// time a hook runs, the unsanitised bytes have been on a laptop's disk, in a temporary directory and
// possibly in a crash dump, and "including during a failed or aborted run" is precisely the case a
// hook never sees.
//
// So the boundary is a type. A Sink is where recorded bytes go; a SanitisedRecorder is the only
// thing in this package that holds one, and it cannot be constructed without a Sanitiser. There is
// no ordering in which a payload reaches a sink without passing the assertion, because there is no
// other route to the sink — the same shape the read-only gate uses for the credential, and for the
// same reason.

// Sink is where a recording is written: a file, a tar member, a test buffer. It is deliberately
// narrow — one method, bytes in — so that nothing that implements it can be mistaken for something
// that sanitises.
type Sink interface {
	Write(ctx context.Context, name string, payload []byte) error
}

// SanitisedRecorder is the only path from this feeder to a Sink.
type SanitisedRecorder struct {
	sanitiser *sanitise.Sanitiser
	sink      Sink
	written   int
}

// NewSanitisedRecorder returns a recorder over sink. A nil Sanitiser is refused rather than
// tolerated: a recorder that writes whatever it is handed is what FR-137 exists to prevent, and a
// campaign that is going to refuse should refuse before it reads its first payload.
func NewSanitisedRecorder(s *sanitise.Sanitiser, sink Sink) (*SanitisedRecorder, error) {
	if s == nil {
		return nil, fmt.Errorf("gcp: a recorder with no sanitiser; sanitisation happens in the " +
			"connector before anything touches disk, and a recording taken without it cannot be " +
			"cleaned afterwards — the unsanitised bytes were already written (FR-137)")
	}
	if sink == nil {
		return nil, fmt.Errorf("gcp: a recorder with no sink")
	}
	if err := s.Policy().Validate(); err != nil {
		return nil, err
	}
	return &SanitisedRecorder{sanitiser: s, sink: sink}, nil
}

// PolicyVersion returns the contract version this recording was made under, for the manifest
// (FR-134, FR-140).
func (r *SanitisedRecorder) PolicyVersion() string { return r.sanitiser.Policy().Version() }

// Dropped returns what the sanitiser dropped, for the manifest's "what was dropped" (FR-140).
func (r *SanitisedRecorder) Dropped() []sanitise.Drop { return r.sanitiser.Drops() }

// Written returns how many payloads reached the sink. It exists so that a campaign can assert the
// recording is non-empty: a sanitiser that refused everything and a source that returned nothing
// look identical from the outside, and they are very different situations.
func (r *SanitisedRecorder) Written() int { return r.written }

// Record sanitises a flattened payload and writes it. The assertion runs on the encoded bytes, not
// on the map, because the bytes are what the sink receives — an encoder that helpfully included a
// field the walk never saw is exactly the defect a check on the map would miss.
func (r *SanitisedRecorder) Record(ctx context.Context, name string, fields map[string]string, encode func(map[string]string) ([]byte, error)) error {
	clean, err := r.sanitiser.Fields(name, fields)
	if err != nil {
		// FR-140's "drop rather than promise": a payload that cannot be made safe is dropped from
		// the recording and the drop is documented. It is not written and then fixed later, because
		// that is the promise that is never kept.
		return fmt.Errorf("gcp: %s was dropped from the recording rather than written: %w", name, err)
	}
	payload, err := encode(clean)
	if err != nil {
		return fmt.Errorf("gcp: encoding %s: %w", name, err)
	}
	return r.RecordBytes(ctx, name, payload)
}

// RecordBytes asserts already-sanitised bytes and writes them. It is exported for the recorder of a
// payload this package does not flatten — a protobuf message written through its own marshaller —
// and the assertion is the same one, which is the point: there is one gate, and it is on the bytes.
func (r *SanitisedRecorder) RecordBytes(ctx context.Context, name string, payload []byte) error {
	if err := r.sanitiser.AssertArtifact(name, payload); err != nil {
		return err
	}
	if err := r.sink.Write(ctx, name, payload); err != nil {
		return err
	}
	r.written++
	return nil
}
