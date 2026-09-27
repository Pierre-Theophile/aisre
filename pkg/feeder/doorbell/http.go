// SPDX-License-Identifier: Apache-2.0

package doorbell

import (
	"crypto/subtle"
	"errors"
	"io"
	"net/http"
	"strings"
)

// An HTTP doorbell: a vendor webhook that means "poll now" (005 T024, FR-025b).
//
// It is stricter than a signed webhook, and can be, because the check is a shared-secret HEADER rather
// than a signature over the body: nothing about the body is needed to decide, so the body is never
// read into the process at all. It is drained to a bounded sink so the connection can be reused, and
// that is the only thing that touches it. There is no type in this package a payload could be decoded
// into.
//
// A request without the header, or with the wrong value, is refused before the bucket and counted, so
// "somebody is probing the doorbell" and "nobody is ringing it" stay different facts. The answer to a
// refused request does not say which of the two checks failed.

// maxDrain bounds how much of a body is discarded to keep the connection reusable; beyond it the
// connection is simply closed.
const maxDrain = 64 << 10

// HTTPOptions configures an HTTP doorbell.
type HTTPOptions struct {
	// Bell is the bucket rings go to. Required.
	Bell *Bell
	// Header is the request header carrying the shared secret, e.g. "X-Doorbell-Secret". Required.
	Header string
	// Secret is the value it must equal. Required, and never logged or reported.
	Secret string
	// OnPoll is called when a ring earns a poll. It must not block; a feeder typically does a
	// non-blocking send on a buffered channel its loop selects on.
	OnPoll func()
}

// HTTP serves one doorbell endpoint.
type HTTP struct {
	bell   *Bell
	header string
	secret []byte
	onPoll func()
}

// NewHTTP returns the endpoint, refusing an incomplete configuration rather than serving an open door.
func NewHTTP(opts HTTPOptions) (*HTTP, error) {
	switch {
	case opts.Bell == nil:
		return nil, errors.New("doorbell: an HTTP doorbell with no bell")
	case strings.TrimSpace(opts.Header) == "":
		return nil, errors.New("doorbell: an HTTP doorbell with no secret header named")
	case len(opts.Secret) < 16:
		return nil, errors.New("doorbell: an HTTP doorbell's secret must be at least 16 characters; " +
			"a guessable secret is an open door to unmetered polls")
	}
	return &HTTP{bell: opts.Bell, header: opts.Header, secret: []byte(opts.Secret), onPoll: opts.OnPoll}, nil
}

// ServeHTTP answers one ring. 202 for a ring that reached the bucket, whatever the bucket decided; 401
// for a refused one; 405 for anything but POST.
func (h *HTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Body != nil {
		defer func() {
			_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, maxDrain))
			_ = r.Body.Close()
		}()
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	got := []byte(r.Header.Get(h.header))
	if len(got) == 0 || subtle.ConstantTimeCompare(got, h.secret) != 1 {
		h.bell.refuse()
		http.Error(w, "unauthorised", http.StatusUnauthorized)
		return
	}
	if outcome := h.bell.Ring(1); outcome.PollNow && h.onPoll != nil {
		h.onPoll()
	}
	w.WriteHeader(http.StatusAccepted)
}
