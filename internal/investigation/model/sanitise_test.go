// SPDX-License-Identifier: Apache-2.0

package model_test

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/investigation/model"
	"github.com/Pierre-Theophile/aisre/internal/sanitise"
)

// The model provider boundary's own tests (T042, FR-141).

// recordingNext captures what actually reached the provider, which is the only thing this boundary
// is about.
type recordingNext struct {
	sawBody []byte
	calls   int
}

func (n *recordingNext) RoundTrip(req *http.Request) (*http.Response, error) {
	n.calls++
	if req.Body != nil {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		n.sawBody = body
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
		Header:     http.Header{},
		Request:    req,
	}, nil
}

func (n *recordingNext) Mode() model.TransportMode   { return model.TransportLive }
func (n *recordingNext) Exchanges() []model.Exchange { return nil }

func guard(t *testing.T) *sanitise.Sanitiser {
	t.Helper()
	material := make([]byte, sanitise.KeyBytes)
	for i := range material {
		material[i] = byte(i + 1)
	}
	key, err := sanitise.NewKey(material)
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}
	s, err := sanitise.New(sanitise.ContractPolicy(), key)
	if err != nil {
		t.Fatalf("sanitise.New: %v", err)
	}
	return s
}

func post(t *testing.T, body string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages",
		bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	return req
}

// A guard is required, not optional: an optional guard is off in exactly the configuration nobody
// reviewed.
func TestASanitisedTransportRequiresAGuard(t *testing.T) {
	if _, err := model.NewSanitisedTransport(&recordingNext{}, nil); err == nil {
		t.Fatal("a sanitised transport was built with no guard (FR-141)")
	}
	if _, err := model.NewSanitisedTransport(nil, guard(t)); err == nil {
		t.Fatal("a sanitised transport was built with nothing to wrap")
	}
	if _, err := model.NewSanitisedTransport(&recordingNext{}, guard(t)); err != nil {
		t.Fatalf("a valid sanitised transport was refused: %v", err)
	}
}

// A prompt carrying a principal address never reaches the provider, and the refusal is a refusal —
// not a warning with the request sent anyway.
func TestAPromptCarryingAPersonNeverReachesTheProvider(t *testing.T) {
	next := &recordingNext{}
	transport, err := model.NewSanitisedTransport(next, guard(t))
	if err != nil {
		t.Fatalf("NewSanitisedTransport: %v", err)
	}
	resp, err := transport.RoundTrip(post(t, `{"messages":[{"role":"user","content":"who deployed this? jane.doe@acme-corp.io did"}]}`))
	closeBody(t, resp)
	if err == nil {
		t.Fatal("a prompt carrying a principal address was sent to the provider (FR-141)")
	}
	if next.calls != 0 {
		t.Fatalf("the request reached the provider %d times despite the refusal", next.calls)
	}
	if strings.Contains(err.Error(), "jane.doe@acme-corp.io") {
		t.Fatalf("the refusal quotes the address it found, which puts it in a log: %q", err.Error())
	}
}

// The ordinary path still works, and the body reaches the provider intact — a RoundTripper that
// consumed the body without replacing it would break every retry above it.
func TestACleanPromptIsSentUnchangedAndTheBodyIsStillReadable(t *testing.T) {
	next := &recordingNext{}
	transport, err := model.NewSanitisedTransport(next, guard(t))
	if err != nil {
		t.Fatalf("NewSanitisedTransport: %v", err)
	}
	const body = `{"messages":[{"role":"user","content":"which revision of px_svc_abcdefghijkl is failing?"}]}`
	resp, err := transport.RoundTrip(post(t, body))
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if next.calls != 1 {
		t.Fatalf("the provider saw %d calls, want 1", next.calls)
	}
	if string(next.sawBody) != body {
		t.Fatalf("the provider saw %q, not the request body %q", next.sawBody, body)
	}
	if transport.Mode() != model.TransportLive {
		t.Fatalf("wrapping hid the transport mode: %q", transport.Mode())
	}
}

// A canary in a prompt is refused too, and by the same gate: a token seeded into the source data is
// asserted absent from what leaves towards a model, not only from what reaches disk.
func TestACanaryInAPromptIsRefused(t *testing.T) {
	material := make([]byte, sanitise.KeyBytes)
	for i := range material {
		material[i] = byte(i + 1)
	}
	key, err := sanitise.NewKey(material)
	if err != nil {
		t.Fatalf("NewKey: %v", err)
	}
	set := sanitise.NewCanarySet(key)
	seeded, err := set.Seed("a source", sanitise.CanaryInfrastructure)
	if err != nil {
		t.Fatalf("Seed: %v", err)
	}
	s, err := sanitise.New(sanitise.ContractPolicy(), key)
	if err != nil {
		t.Fatalf("sanitise.New: %v", err)
	}
	next := &recordingNext{}
	transport, err := model.NewSanitisedTransport(next, s.WithCanaries(set))
	if err != nil {
		t.Fatalf("NewSanitisedTransport: %v", err)
	}
	resp, err := transport.RoundTrip(post(t, `{"messages":[{"role":"user","content":"tell me about `+seeded[0].Token+`"}]}`))
	closeBody(t, resp)
	if err == nil {
		t.Fatal("a prompt carrying a canary was sent to the provider")
	}
	var survived *sanitise.CanarySurvivedError
	if !errors.As(err, &survived) {
		t.Fatalf("the refusal is %T, not *CanarySurvivedError: %v", err, err)
	}
	if next.calls != 0 {
		t.Fatalf("the request reached the provider %d times", next.calls)
	}
}

// closeBody closes a response body where the round trip produced one.
//
// On a refusal there is no response at all — the transport returns before calling the next one — so
// this is nil-tolerant by design rather than by accident. It exists because `bodyclose` cannot know
// that, and because a test that leaked a body would be modelling a caller that leaks one.
func closeBody(t *testing.T, resp *http.Response) {
	t.Helper()
	if resp == nil || resp.Body == nil {
		return
	}
	if err := resp.Body.Close(); err != nil {
		t.Errorf("closing the response body: %v", err)
	}
}

// A GET with no body passes straight through rather than failing on a nil read.
func TestARequestWithNoBodyPassesThrough(t *testing.T) {
	next := &recordingNext{}
	transport, err := model.NewSanitisedTransport(next, guard(t))
	if err != nil {
		t.Fatalf("NewSanitisedTransport: %v", err)
	}
	req, err := http.NewRequest(http.MethodGet, "https://api.anthropic.com/v1/models", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := transport.RoundTrip(req)
	closeBody(t, resp)
	if err != nil {
		t.Fatalf("a bodyless request was refused: %v", err)
	}
	if next.calls != 1 {
		t.Fatalf("the provider saw %d calls, want 1", next.calls)
	}
}
