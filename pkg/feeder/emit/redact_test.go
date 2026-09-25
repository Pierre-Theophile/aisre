// SPDX-License-Identifier: Apache-2.0

package emit_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/emit"
)

// A feeder's bearer token must not reach its own logs (T090, constitution VII). The canary is a
// string nothing legitimate contains, so any appearance is a leak.
const tokenCanary = "feeder-token-canary-9f31"

func redactTestDescription() feeder.Description {
	return feeder.Description{
		SourceID:         "flags:prod-eu1",
		Kind:             "flags",
		Ordering:         feeder.OrderingNone,
		ReorderingWindow: time.Minute,
		Namespaces:       []string{feeder.NSOTelService},
	}
}

func TestRedactAuthorization(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+tokenCanary)
	h.Set("Proxy-Authorization", "Basic "+tokenCanary)
	h.Set("Cookie", "session="+tokenCanary)
	h.Set("Content-Type", "application/json")

	got := emit.RedactAuthorization(h)
	for _, name := range []string{"Authorization", "Proxy-Authorization", "Cookie"} {
		if v := got.Get(name); v != emit.RedactedValue {
			t.Errorf("%s = %q, want %q", name, v, emit.RedactedValue)
		}
	}
	if got.Get("Content-Type") != "application/json" {
		t.Error("a harmless header was redacted")
	}
	// The original is untouched: the transport still has to send the real thing.
	if h.Get("Authorization") != "Bearer "+tokenCanary {
		t.Error("RedactAuthorization mutated its argument")
	}
	if emit.RedactAuthorization(nil) != nil {
		t.Error("a nil header should stay nil")
	}
}

// Everything the emitter logs on the unhappy path — a refusal, a retry, a failed delivery —
// goes through one logger. None of it may carry the credential.
func TestConnectEmitterNeverLogsTheToken(t *testing.T) {
	var logged bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// A server that refuses everything: the emitter retries what it can, gives up, and logs.
	// It also records what reached it, so the credential path is genuinely exercised.
	var seenAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	em, err := emit.NewConnectEmitter(srv.URL, tokenCanary, redactTestDescription(),
		emit.WithBatchSize(1),
		emit.WithMaxAttempts(2),
		emit.WithRetryBaseDelay(time.Millisecond),
		emit.WithLogger(logger),
	)
	if err != nil {
		t.Fatalf("NewConnectEmitter: %v", err)
	}
	defer func() { _ = em.Close(context.Background()) }()

	ev := feeder.UpsertNode(em.Describe(), "flags:prod-eu1:node:1", feeder.NodeFact{
		Ref:              feeder.Ref(feeder.NSOTelService, "checkout"),
		Type:             graphv1.NodeType_SERVICE,
		ValidFromUnknown: true,
	})
	// The error is the point of the test; what it says is what is being checked.
	emitErr := func() error { _, err := em.Emit(context.Background(), ev); return err }()
	if emitErr == nil {
		t.Fatal("want a delivery failure")
	}

	if strings.Contains(emitErr.Error(), tokenCanary) {
		t.Errorf("the returned error carries the token: %v", emitErr)
	}
	if out := logged.String(); strings.Contains(out, tokenCanary) {
		t.Errorf("the emitter logged the token:\n%s", out)
	}
	// The token did travel, so the test is about redaction and not about a missing credential.
	if seenAuth != "Bearer "+tokenCanary {
		t.Errorf("Authorization reaching the server = %q, want the bearer token", seenAuth)
	}
	// And a caller that logs the headers itself has a supported way not to leak them.
	h := http.Header{}
	h.Set("Authorization", seenAuth)
	if v := emit.RedactAuthorization(h).Get("Authorization"); strings.Contains(v, tokenCanary) {
		t.Errorf("RedactAuthorization left the token in %q", v)
	}
}

// A base URL written with userinfo would otherwise put a credential into every error message,
// log line and client span that names the endpoint.
func TestConnectEmitterStripsCredentialsFromTheBaseURL(t *testing.T) {
	var logged bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))

	em, err := emit.NewConnectEmitter("https://feeder:"+tokenCanary+"@graph.invalid",
		"", redactTestDescription(),
		emit.WithBatchSize(1),
		emit.WithMaxAttempts(1),
		emit.WithLogger(logger),
	)
	if err != nil {
		t.Fatalf("NewConnectEmitter: %v", err)
	}
	defer func() { _ = em.Close(context.Background()) }()

	ev := feeder.UpsertNode(em.Describe(), "flags:prod-eu1:node:1", feeder.NodeFact{
		Ref:              feeder.Ref(feeder.NSOTelService, "checkout"),
		Type:             graphv1.NodeType_SERVICE,
		ValidFromUnknown: true,
	})
	_, err = em.Emit(context.Background(), ev)
	if err == nil {
		t.Fatal("graph.invalid should not resolve")
	}
	if strings.Contains(err.Error(), tokenCanary) {
		t.Errorf("the error carries the URL password: %v", err)
	}
	if out := logged.String(); strings.Contains(out, tokenCanary) {
		t.Errorf("the emitter logged the URL password:\n%s", out)
	}
}
