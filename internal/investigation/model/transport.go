// SPDX-License-Identifier: Apache-2.0

package model

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/Pierre-Theophile/aisre/internal/graph"
)

// The model transport seam (T060, FR-041, FR-042a, research §3, plan §Replay design).
//
// Everything the engine and the vendor say to each other passes through exactly one place, and
// that place is an `http.RoundTripper`. That is a deliberate choice of altitude. A seam above
// the HTTP layer — a Go interface over "send these messages, get that message back" — would
// record a *rendering* of the request, and layer 1's whole claim is that the recorded request is
// the request: the exact body, the exact betas, the exact `output_config`. A seam at the HTTP
// layer records the bytes the SDK actually produced, which is the only thing a replay can be
// byte-identical to, and it lets a replayed response come back through the SDK's own decoder so
// that replay exercises the same parsing live traffic does.
//
// Three implementations, one interface:
//
//   - live — the ordinary transport, wrapped so the exchange is still observable;
//   - recording — live, plus every exchange appended to an ordered log;
//   - replaying — no network at all: the next recorded exchange is matched against the request
//     about to be issued, by canonical digest, and its recorded response is returned. A mismatch
//     fails loudly, naming the first diverging record (FR-041). It never falls through to the
//     network, because a replay that improvises is evidence of nothing.
//
// Phase 7's trajectory replay is this seam and nothing more: it loads the `model_request` /
// `model_response` pairs out of a trajectory file, hands them to NewReplayingTransport, and runs
// the same engine.

// TransportMode is how the model boundary is being served for this run. It is recorded on every
// model call, because a number produced under replay and a number produced live are different
// claims.
type TransportMode string

// The three published transport modes.
const (
	// TransportLive calls the vendor.
	TransportLive TransportMode = "live"
	// TransportRecording calls the vendor and keeps every exchange.
	TransportRecording TransportMode = "recording"
	// TransportReplaying answers from a recording and makes no network call.
	TransportReplaying TransportMode = "replaying"
)

// Exchange is one model call as it crossed the wire: the request body the SDK produced and the
// response body the vendor returned, both canonicalised, both digested.
//
// The digests are what a replay matches on. They are taken over the canonical JSON — sorted
// keys, normalised timestamps — rather than over the raw bytes, because the SDK is entitled to
// order a JSON object however it likes and a replay that broke when it did would be testing the
// SDK's map iteration rather than the engine.
type Exchange struct {
	// Seq is the 1-based position of this exchange in the run.
	Seq int `json:"seq"`
	// Method and Path are the HTTP call, kept so a divergence can say what was being asked.
	Method string `json:"method"`
	Path   string `json:"path"`
	// RequestBody is the canonical JSON of the exact request body.
	RequestBody json.RawMessage `json:"request_body"`
	// RequestDigest is sha256 of RequestBody, hex-encoded.
	RequestDigest string `json:"request_digest"`
	// Status is the HTTP status the vendor returned.
	Status int `json:"status"`
	// ResponseBody is the canonical JSON of the exact response body.
	ResponseBody json.RawMessage `json:"response_body"`
	// ResponseDigest is sha256 of ResponseBody, hex-encoded.
	ResponseDigest string `json:"response_digest"`
	// Betas is the `anthropic-beta` header the request carried. The beta set is a header rather
	// than a body field, so it is recorded here beside the body rather than inside it: the
	// digest stays a digest of the exact body, and a reader can still see which betas were in
	// force (FR-061).
	Betas []string `json:"betas,omitempty"`
}

// Transport is the seam between the engine and the Anthropic API. It is an http.RoundTripper so
// that the SDK can be handed it unchanged, and it publishes the exchanges it saw so that the
// engine can turn them into trajectory records without re-serialising anything.
type Transport interface {
	http.RoundTripper
	// Mode says which of the three implementations this is.
	Mode() TransportMode
	// Exchanges returns every exchange so far, in issue order.
	Exchanges() []Exchange
}

// DivergenceError is a replay that was asked a question its recording does not answer. It names
// the first diverging record, which is the whole point of sequencing layer 1 (FR-041).
type DivergenceError struct {
	// Seq is the position in the recording where the divergence was found.
	Seq int
	// Expected and Actual are the canonical request digests.
	Expected string
	Actual   string
	// ExpectedBody and ActualBody are the canonical request bodies, so a reader can see what
	// moved without re-running anything.
	ExpectedBody json.RawMessage
	ActualBody   json.RawMessage
	// Detail says what kind of divergence it is.
	Detail string
}

// Error renders the divergence.
func (e *DivergenceError) Error() string {
	return fmt.Sprintf(
		"model: trajectory replay diverged at record %d: %s (recorded request digest %s, request about to be issued %s)",
		e.Seq, e.Detail, short(e.Expected), short(e.Actual))
}

// DivergenceOf returns the divergence an error carries, or nil. net/http wraps a RoundTripper's
// error in a *url.Error, so a caller that string-matched would be matching the wrapper.
func DivergenceOf(err error) *DivergenceError {
	var d *DivergenceError
	if errors.As(err, &d) {
		return d
	}
	return nil
}

func short(digest string) string {
	if len(digest) <= 12 {
		return digest
	}
	return digest[:12]
}

// baseTransport is the shared bookkeeping: read the request body, canonicalise it, digest it.
type baseTransport struct {
	mu        sync.Mutex
	exchanges []Exchange
}

func (b *baseTransport) append(x Exchange) {
	b.mu.Lock()
	defer b.mu.Unlock()
	x.Seq = len(b.exchanges) + 1
	b.exchanges = append(b.exchanges, x)
}

func (b *baseTransport) list() []Exchange {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Exchange, len(b.exchanges))
	copy(out, b.exchanges)
	return out
}

// readRequest drains a request body and returns it canonicalised and digested, leaving the
// request usable — a RoundTripper that consumed the body without replacing it would break any
// retry the SDK decided to make.
func readRequest(req *http.Request) (json.RawMessage, string, error) {
	if req.Body == nil {
		return json.RawMessage("null"), digestOf([]byte("null")), nil
	}
	raw, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, "", fmt.Errorf("model: read request body: %w", err)
	}
	if err := req.Body.Close(); err != nil {
		return nil, "", fmt.Errorf("model: close request body: %w", err)
	}
	req.Body = io.NopCloser(bytes.NewReader(raw))
	req.ContentLength = int64(len(raw))
	canonical, err := canonicalise(raw)
	if err != nil {
		return nil, "", err
	}
	return canonical, digestOf(canonical), nil
}

func canonicalise(raw []byte) (json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return json.RawMessage("null"), nil
	}
	out, err := graph.CanonicalJSON(json.RawMessage(raw))
	if err != nil {
		return nil, fmt.Errorf("model: canonicalise body: %w", err)
	}
	return out, nil
}

// betasOf reads the beta set off the wire rather than off the configuration, so a recording
// shows what was actually sent. The header may repeat or carry a comma-separated list; both
// spellings are flattened to one ordered slice.
func betasOf(req *http.Request) []string {
	values := req.Header.Values("anthropic-beta")
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// liveTransport calls the vendor and observes what crossed.
type liveTransport struct {
	baseTransport
	next      http.RoundTripper
	recording bool
}

// NewLiveTransport returns a Transport that calls the vendor through next (nil means
// http.DefaultTransport) and observes every exchange without keeping the bodies.
//
// It still fills in Exchanges: an investigation always records what it asked and what it was
// told, whether or not anyone intends to replay it (FR-042a). "Recording" in the mode sense is
// about whether that log is the artefact being produced, not about whether it exists.
func NewLiveTransport(next http.RoundTripper) Transport {
	return &liveTransport{next: orDefault(next)}
}

// NewRecordingTransport returns a Transport that calls the vendor through next and keeps every
// exchange for layer 1.
func NewRecordingTransport(next http.RoundTripper) Transport {
	return &liveTransport{next: orDefault(next), recording: true}
}

func orDefault(next http.RoundTripper) http.RoundTripper {
	if next != nil {
		return next
	}
	return http.DefaultTransport
}

func (t *liveTransport) Mode() TransportMode {
	if t.recording {
		return TransportRecording
	}
	return TransportLive
}

func (t *liveTransport) Exchanges() []Exchange { return t.list() }

// RoundTrip implements http.RoundTripper.
func (t *liveTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body, digest, err := readRequest(req)
	if err != nil {
		return nil, err
	}
	resp, err := t.next.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(resp.Body)
	closeErr := resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("model: read response body: %w", err)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("model: close response body: %w", closeErr)
	}
	resp.Body = io.NopCloser(bytes.NewReader(raw))

	canonical, err := canonicalise(raw)
	if err != nil {
		// A body that is not JSON is a vendor error page or a stream; it is recorded verbatim
		// rather than dropped, because a failure the recording cannot show is a failure nobody
		// can replay.
		canonical = json.RawMessage(raw)
	}
	t.append(Exchange{
		Method:         req.Method,
		Path:           req.URL.Path,
		RequestBody:    body,
		RequestDigest:  digest,
		Status:         resp.StatusCode,
		ResponseBody:   canonical,
		ResponseDigest: digestOf(canonical),
		Betas:          betasOf(req),
	})
	return resp, nil
}

// replayingTransport answers from a recording and makes no network call.
type replayingTransport struct {
	baseTransport
	recorded []Exchange
	next     int
	strict   bool
}

// NewReplayingTransport returns a Transport that serves the recorded exchanges in order and
// never reaches the network.
//
// Matching is by canonical request digest against the *next* record, not by lookup: layer 1 is a
// sequence, and FR-041 requires the first diverging request to be named. A lookup would answer a
// question asked in the wrong order and hide exactly the regression the gate exists to catch.
func NewReplayingTransport(recorded []Exchange) Transport {
	return &replayingTransport{recorded: append([]Exchange(nil), recorded...), strict: true}
}

// NewReplayingTransportTolerant is NewReplayingTransport without the digest check: the recorded
// responses are returned in order whatever is asked.
//
// It exists for one purpose — a unit test that wants canned model turns and is not testing the
// replay gate — and it is named so that nobody reaches for it by accident. A fixture replay
// always uses the strict one.
func NewReplayingTransportTolerant(recorded []Exchange) Transport {
	return &replayingTransport{recorded: append([]Exchange(nil), recorded...)}
}

func (t *replayingTransport) Mode() TransportMode { return TransportReplaying }

func (t *replayingTransport) Exchanges() []Exchange { return t.list() }

// countTokensPath is the pre-flight token count. It is not a model turn: it produces no content,
// it is not part of the trajectory, and a recording therefore holds no entry for it. A replay
// answers it locally rather than consuming a recorded turn or reaching the network — the number
// only has to be a stable, conservative estimate for the admission check to be meaningful
// (FR-047a).
const countTokensPath = "/v1/messages/count_tokens"

// bytesPerToken is the estimate a replay counts with. Four bytes per token is the usual
// order-of-magnitude for English prose and JSON, and a replay's admission check is a check that
// the *loop* still admits, not a re-measurement of the tokenizer.
const bytesPerToken = 4

// RoundTrip implements http.RoundTripper without touching the network.
func (t *replayingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body, digest, err := readRequest(req)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(req.URL.Path, countTokensPath) {
		return jsonResponse(req, fmt.Sprintf(`{"input_tokens":%d}`, len(body)/bytesPerToken)), nil
	}

	// The index advances only on a match. A divergence must name the record it diverged *at*, and
	// an index that advanced on failure would name a different record on every retry the HTTP
	// client made — turning one divergence into a moving target.
	t.mu.Lock()
	index := t.next
	t.mu.Unlock()

	if index >= len(t.recorded) {
		return nil, &DivergenceError{
			Seq:        index + 1,
			Actual:     digest,
			ActualBody: body,
			Detail: fmt.Sprintf(
				"the engine issued a %d%s model call but the recording holds %d; the run asked a question the recording does not answer",
				index+1, ordinal(index+1), len(t.recorded)),
		}
	}
	want := t.recorded[index]
	if t.strict && want.RequestDigest != "" && want.RequestDigest != digest {
		return nil, &DivergenceError{
			Seq:          index + 1,
			Expected:     want.RequestDigest,
			Actual:       digest,
			ExpectedBody: want.RequestBody,
			ActualBody:   body,
			Detail:       "the request the engine was about to issue is not the request that was recorded",
		}
	}

	t.mu.Lock()
	t.next++
	t.mu.Unlock()

	// A recording made by this package always carries a canonical body and its digest. A canned
	// one — a unit test's fixture — carries neither, so both are derived here rather than being
	// required of every caller that wants to hand the engine a scripted turn.
	responseBody, err := canonicalise(want.ResponseBody)
	if err != nil {
		responseBody = want.ResponseBody
	}
	responseDigest := want.ResponseDigest
	if responseDigest == "" {
		responseDigest = digestOf(responseBody)
	}

	t.append(Exchange{
		Method:         req.Method,
		Path:           req.URL.Path,
		RequestBody:    body,
		RequestDigest:  digest,
		Status:         statusOr(want.Status),
		ResponseBody:   responseBody,
		ResponseDigest: responseDigest,
		Betas:          betasOf(req),
	})

	return &http.Response{
		Status:        http.StatusText(statusOr(want.Status)),
		StatusCode:    statusOr(want.Status),
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{"Content-Type": []string{"application/json"}},
		Body:          io.NopCloser(bytes.NewReader(responseBody)),
		ContentLength: int64(len(responseBody)),
		Request:       req,
	}, nil
}

// jsonResponse builds a synthetic 200 for a call a replay answers itself.
func jsonResponse(req *http.Request, body string) *http.Response {
	return &http.Response{
		Status:        http.StatusText(http.StatusOK),
		StatusCode:    http.StatusOK,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{"Content-Type": []string{"application/json"}},
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}

func statusOr(status int) int {
	if status == 0 {
		return http.StatusOK
	}
	return status
}

// ordinal renders the English suffix for a call number, so a divergence reads as a sentence.
func ordinal(n int) string {
	if n%100 >= 11 && n%100 <= 13 {
		return "th"
	}
	switch n % 10 {
	case 1:
		return "st"
	case 2:
		return "nd"
	case 3:
		return "rd"
	default:
		return "th"
	}
}
