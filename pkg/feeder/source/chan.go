// SPDX-License-Identifier: Apache-2.0

package source

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// ChanSource is the live half of FR-044: a watch loop, an OTLP receiver or a webhook handler
// pushes payloads in, and the feeder — which cannot tell the difference — reads them out.
//
// Closing it is how a live feeder shuts down cleanly: Next drains whatever is buffered and
// then returns io.EOF, so Run finishes the work it already has rather than abandoning it.
type ChanSource struct {
	ch chan feeder.Payload

	closeOnce sync.Once
	closed    chan struct{}
}

var _ feeder.Source = (*ChanSource)(nil)

// ErrSourceClosed is returned by Push after Close.
var ErrSourceClosed = errors.New("source: closed")

// NewChanSource returns a channel-backed Source with the given buffer. A buffer of zero makes
// Push block until the feeder is ready, which is the right default for a watch loop that must
// not run ahead of the graph.
func NewChanSource(buffer int) *ChanSource {
	if buffer < 0 {
		buffer = 0
	}
	return &ChanSource{ch: make(chan feeder.Payload, buffer), closed: make(chan struct{})}
}

// Push hands one payload to the feeder. It blocks until the feeder takes it, ctx is done, or
// the source is closed.
func (s *ChanSource) Push(ctx context.Context, p feeder.Payload) error {
	select {
	case <-s.closed:
		return ErrSourceClosed
	default:
	}
	select {
	case s.ch <- p:
		return nil
	case <-s.closed:
		return ErrSourceClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Next returns the next pushed payload. It blocks until one arrives, and returns io.EOF once
// the source is closed and drained.
//
// The buffered channel is read first and without blocking, so that a payload already pushed is
// always preferred to the close signal: closing must not throw away work the feeder was given.
func (s *ChanSource) Next(ctx context.Context) (feeder.Payload, error) {
	select {
	case p := <-s.ch:
		return p, nil
	default:
	}
	select {
	case p := <-s.ch:
		return p, nil
	case <-s.closed:
		// A payload may have been pushed between the two selects; take it before ending.
		select {
		case p := <-s.ch:
			return p, nil
		default:
			return feeder.Payload{}, io.EOF
		}
	case <-ctx.Done():
		return feeder.Payload{}, ctx.Err()
	}
}

// Close stops the source. Payloads already pushed are still delivered; Next returns io.EOF
// after them, and a Push blocked on a full buffer returns ErrSourceClosed. Close is safe to
// call more than once.
//
// The payload channel itself is never closed: a producer blocked in Push would then send on a
// closed channel and panic. Closing the separate signal channel unblocks both sides instead.
func (s *ChanSource) Close() {
	s.closeOnce.Do(func() { close(s.closed) })
}
