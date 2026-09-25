// SPDX-License-Identifier: Apache-2.0

package model

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
)

// The model provider boundary (T042, FR-141, contract §5 call site 3).
//
// FR-141: nothing may be sent to a model that would not be permitted into a recording under the same
// contract and policy version. It is the call site people forget, and the reason is that it does not
// feel like storage — but a prompt leaves the building, is retained by the provider under its own
// terms, and may be logged at three hops this project does not control. A principal address in a
// prompt is a disclosure with a wider blast radius than the same address in a private fixture.
//
// The seam is the one feature 002 already established: everything the engine and the vendor say to
// each other passes through exactly one `http.RoundTripper`, and the recorded request is the request.
// A guard at that altitude sees the exact bytes, including the ones an SDK added — which is the whole
// argument for the seam being there in the first place.
//
// The guard is an interface rather than an import of internal/sanitise so that this package stays
// free of the sanitisation contract's dependencies; the wiring point supplies a *sanitise.Sanitiser,
// which satisfies it.

// OutboundGuard refuses bytes that must not leave the process. *sanitise.Sanitiser satisfies it.
type OutboundGuard interface {
	AssertArtifact(where string, data []byte) error
}

// sanitisedTransport asserts every outbound request body before it reaches the next transport.
type sanitisedTransport struct {
	next  Transport
	guard OutboundGuard
}

// NewSanitisedTransport wraps next so that no request body reaches the provider unasserted.
//
// A nil guard is refused rather than treated as "no checking". That is the difference between a
// boundary and a decoration: an optional guard is off in exactly the configuration nobody reviewed.
//
// What this does **not** do is remove the unguarded constructors beside it. NewLiveTransport and
// NewRecordingTransport are feature 002's published seam and still exist, so FR-141 holds for the
// paths that go through here and is a claim about wiring elsewhere. The wiring is asserted where it
// is made, not here.
func NewSanitisedTransport(next Transport, guard OutboundGuard) (Transport, error) {
	if next == nil {
		return nil, fmt.Errorf("model: a sanitised transport with nothing to wrap")
	}
	if guard == nil {
		return nil, fmt.Errorf("model: a sanitised transport with no guard; nothing may be sent to a " +
			"model that would not be permitted into a recording under the same contract and policy " +
			"version (FR-141), and an optional guard is off in the configuration nobody reviewed")
	}
	return &sanitisedTransport{next: next, guard: guard}, nil
}

// RoundTrip implements http.RoundTripper. It reads the body, asserts it, and replaces it — a
// RoundTripper that consumed the body without replacing it would break any retry above it, which is
// the same care the recording transport takes for the same reason.
func (t *sanitisedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body == nil {
		return t.next.RoundTrip(req)
	}
	body, err := io.ReadAll(req.Body)
	closeErr := req.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("model: reading a request body for the sanitisation boundary: %w", err)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("model: closing a request body for the sanitisation boundary: %w", closeErr)
	}
	req.Body = io.NopCloser(bytes.NewReader(body))
	if err := t.guard.AssertArtifact("a model request", body); err != nil {
		// The refusal is returned rather than logged and continued. A prompt that carries a person is
		// not a prompt to send with a warning attached; there is no version of this with a degraded
		// mode, because the degraded mode is the disclosure.
		return nil, fmt.Errorf("model: refusing to send a request to the provider: %w", err)
	}
	return t.next.RoundTrip(req)
}

// Mode reports the mode of the wrapped transport, so wrapping does not hide what a run was.
func (t *sanitisedTransport) Mode() TransportMode { return t.next.Mode() }

// Exchanges returns the wrapped transport's exchanges.
func (t *sanitisedTransport) Exchanges() []Exchange { return t.next.Exchanges() }
