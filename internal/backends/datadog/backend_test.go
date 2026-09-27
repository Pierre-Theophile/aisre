// SPDX-License-Identifier: Apache-2.0

package datadog_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	ddbackend "github.com/Pierre-Theophile/aisre/internal/backends/datadog"
	"github.com/Pierre-Theophile/aisre/internal/datadogx"
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/internal/sanitise"
	sdk "github.com/Pierre-Theophile/aisre/pkg/backend"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

var at = time.Date(2026, 9, 21, 14, 40, 0, 0, time.UTC)

// liveBackend builds a live backend over a Datadog twin served by h, counting the requests it gets.
func liveBackend(t *testing.T, h http.HandlerFunc) (*ddbackend.Backend, *int64) {
	t.Helper()
	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	client, err := datadogx.New(datadogx.Options{
		BaseURL: srv.URL, APIKey: "k", AppKey: "a", Surface: datadogx.DefaultSurface(),
		Now: func() time.Time { return at },
	})
	if err != nil {
		t.Fatal(err)
	}
	material := make([]byte, sanitise.KeyBytes)
	for i := range material {
		material[i] = byte(i * 7)
	}
	key, err := sanitise.NewKey(material)
	if err != nil {
		t.Fatal(err)
	}
	s, err := sanitise.New(sanitise.DatadogPolicy(), key)
	if err != nil {
		t.Fatal(err)
	}
	policy := engine.DefaultRedactionPolicy()
	policy.PolicyVersion = sanitise.DatadogPolicyVersion
	r, err := engine.NewRedactor(policy, []byte("a corpus key for the digest side"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := ddbackend.New(ddbackend.Options{
		OrgSlug: "twin", Client: client, Sanitiser: s, Redactor: r, Now: func() time.Time { return at },
	})
	if err != nil {
		t.Fatal(err)
	}
	return b, &calls
}

func logPointer() *graphv1.Pointer {
	return &graphv1.Pointer{
		Kind: graphv1.PointerKind_LOG, BackendKind: "datadog", Vocabulary: feeder.VocabDatadogLogs,
		Selector: "service:voice-agent env:production", JoinKeys: map[string]string{"version": "version"},
	}
}

func request(term *engine.Term) *engine.Request { return &engine.Request{Term: term} }

// The declaration serves all eight telemetry terms, every one read-only (contract §1).
func TestTheDeclarationServesTheWholeTelemetryFamily(t *testing.T) {
	t.Parallel()
	d := ddbackend.Declaration("twin")
	if strings.Join(d.Terms, ",") != strings.Join(sdk.Terms(sdk.FamilyTelemetry), ",") {
		t.Errorf("terms %v", d.Terms)
	}
	for _, c := range d.Capabilities {
		if !c.ReadOnly || c.MaxWindow <= 0 {
			t.Errorf("capability %+v", c)
		}
	}
}

// A backend that could not answer honestly is refused at construction.
func TestNewRefusesABackendThatCouldNotAnswerHonestly(t *testing.T) {
	t.Parallel()
	for name, opts := range map[string]ddbackend.Options{
		"no organisation": {Now: time.Now},
		"no clock":        {OrgSlug: "twin"},
		"live, no client": {OrgSlug: "twin", Now: time.Now},
	} {
		if _, err := ddbackend.New(opts); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// error_spans answers NO_DATA naming the absent span source, with no Datadog call (FR-049b, SC-023).
func TestErrorSpansNamesTheAbsentSpanSource(t *testing.T) {
	t.Parallel()
	b, calls := liveBackend(t, func(http.ResponseWriter, *http.Request) {})
	resp, err := b.Execute(context.Background(), request(engine.ErrorSpans("svc-a", "svc-b",
		graphv1.EdgeType_CALLS, engine.NewWindow(at.Add(-time.Hour), at))))
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetOutcome() != investigationv1.TermOutcome_NO_DATA ||
		!strings.Contains(resp.GetDigest().GetCoverage().GetTruncation(), "span data source") {
		t.Fatalf("got %v %q", resp.GetOutcome(), resp.GetDigest().GetCoverage().GetTruncation())
	}
	if *calls != 0 {
		t.Errorf("%d Datadog calls for an absent source", *calls)
	}
}

// A pointer minted for another backend is refused naming its vocabulary, never executed.
func TestAForeignPointerIsRefused(t *testing.T) {
	t.Parallel()
	b, calls := liveBackend(t, func(http.ResponseWriter, *http.Request) {})
	foreign := logPointer()
	foreign.Vocabulary = feeder.VocabGCPLoggingQuery
	resp, err := b.Execute(context.Background(), request(engine.ErrorsByVersion(foreign,
		engine.NewWindow(at.Add(-time.Hour), at), "version")))
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetFailureReason() != investigationv1.FailureReason_UNSUPPORTED_POINTER || *calls != 0 {
		t.Fatalf("got %v / %v with %d calls", resp.GetOutcome(), resp.GetFailureReason(), *calls)
	}
}

// A window older than the indexes hold is OUTSIDE_RETENTION with the horizon, never NO_DATA.
func TestAWindowOutsideRetentionIsNotNoData(t *testing.T) {
	t.Parallel()
	b, _ := liveBackend(t, func(http.ResponseWriter, *http.Request) {})
	old := at.Add(-30 * 24 * time.Hour)
	resp, err := b.Execute(context.Background(), request(engine.ErrorsByVersion(logPointer(),
		engine.NewWindow(old, old.Add(time.Hour)), "version")))
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetFailureReason() != investigationv1.FailureReason_OUTSIDE_RETENTION || resp.GetRetentionHorizon() == nil {
		t.Fatalf("got %v / %v", resp.GetOutcome(), resp.GetFailureReason())
	}
}

// An unstamped errors_by_version gets the engine's answer here too.
func TestAnUnstampedPointerGetsTheEnginesAnswer(t *testing.T) {
	t.Parallel()
	b, calls := liveBackend(t, func(http.ResponseWriter, *http.Request) {})
	p := logPointer()
	p.JoinKeys = nil
	resp, err := b.Execute(context.Background(), request(engine.ErrorsByVersion(p,
		engine.NewWindow(at.Add(-time.Hour), at), "")))
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetOutcome() != investigationv1.TermOutcome_NO_DATA || *calls != 0 {
		t.Fatalf("got %v with %d calls", resp.GetOutcome(), *calls)
	}
}
