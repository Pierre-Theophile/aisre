// SPDX-License-Identifier: Apache-2.0

package datadog_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	ddbackend "github.com/Pierre-Theophile/aisre/internal/backends/datadog"
	"github.com/Pierre-Theophile/aisre/internal/datadogx"
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/internal/sanitise"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// Live digest parity against a real organisation (005 SC-009, FR-143 second half; T083).
//
// Every term of the algebra runs twice over one settled window: once against Datadog, recording each
// response body as it arrives, and once against a local server that serves those recorded bodies back.
// The two digests must be the same bytes. A backend that derived anything from the connection rather
// than from the response (FR-008) produces identical requests and different digests, which is what
// this catches and a graph comparison cannot.
//
// It runs only when asked, with the keys in the environment and the organisation's names given as
// variables, so no identifier of the organisation is written into this file:
//
//	DD_LIVE_PARITY=1 DD_SITE=datadoghq.eu \
//	DD_PARITY_SELECTOR='service:<service> @env:<env>' DD_PARITY_MONITOR=<monitor id> \
//	SRE_AGENT_CORPUS_KEY=<hex> go test ./internal/backends/datadog/ -run LiveDigestParity -v
//
// The report names terms and digest hashes, never a digest's content.

// parityPace is how long a rate-limited term waits before asking again when Datadog's 429 did not say
// when its window resets. Where it did, the term waits for that reset instead: the first live run's
// fixed 65 seconds was a guess at a one-minute window, and five of them need not reach the reset of a
// logs bucket whose period Datadog states and which is calendar-aligned (research §2.3).
const parityPace = 65 * time.Second

// parityMaxWait bounds one wait for a stated reset, so a header that names a day-long window ends the
// run with a named failure rather than holding it.
const parityMaxWait = 61 * time.Minute

// parityAttempts is how many times a rate-limited term asks before the run records it as unanswered.
const parityAttempts = 5

type recordedExchange struct {
	status int
	header http.Header
	body   []byte
	// at is when the answer arrived; Datadog's X-RateLimit-Reset counts from it.
	at time.Time
}

// exchangeKey is what makes two requests the same request: the method, the path with its query, and
// the body. The keys are never part of it.
func exchangeKey(method, uri string, body []byte) string {
	return method + " " + uri + " " + string(body)
}

// recordingTransport forwards to Datadog and keeps every answer, in order, per request.
type recordingTransport struct {
	next http.RoundTripper
	mu   sync.Mutex
	seen map[string][]recordedExchange
}

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		body = b
		req.Body = io.NopCloser(bytes.NewReader(b))
	}
	resp, err := r.next.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	got, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(got))
	r.mu.Lock()
	key := exchangeKey(req.Method, req.URL.RequestURI(), body)
	r.seen[key] = append(r.seen[key], recordedExchange{status: resp.StatusCode, header: resp.Header.Clone(), body: got,
		at: time.Now()})
	r.mu.Unlock()
	return resp, nil
}

// replayServer serves the recorded answers back in the order they were given. A request that was
// never made live fails the test: the replay must ask exactly what the live run asked.
func replayServer(t *testing.T, seen map[string][]recordedExchange) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	next := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		key := exchangeKey(req.Method, req.URL.RequestURI(), body)
		mu.Lock()
		i := next[key]
		next[key]++
		mu.Unlock()
		answers := seen[key]
		if i >= len(answers) {
			t.Errorf("the replay asked a request the live run did not: %s %s", req.Method, req.URL.Path)
			w.WriteHeader(http.StatusTeapot)
			return
		}
		for k, v := range answers[i].header {
			w.Header()[k] = v
		}
		w.WriteHeader(answers[i].status)
		_, _ = w.Write(answers[i].body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func paritySanitiserAndRedactor(t *testing.T) (*sanitise.Sanitiser, *engine.Redactor) {
	t.Helper()
	key, err := sanitise.KeyFromEnv()
	if err != nil {
		t.Fatalf("the live parity run sanitises under the corpus key: %v", err)
	}
	s, err := sanitise.New(sanitise.DatadogPolicy(), key)
	if err != nil {
		t.Fatal(err)
	}
	policy := engine.DefaultRedactionPolicy()
	policy.PolicyVersion = sanitise.DatadogPolicyVersion
	material, err := hex.DecodeString(strings.TrimSpace(os.Getenv(sanitise.KeyEnv)))
	if err != nil {
		t.Fatal(err)
	}
	r, err := engine.NewRedactor(policy, material)
	if err != nil {
		t.Fatal(err)
	}
	return s, r
}

func parityBackend(t *testing.T, opts datadogx.Options, now time.Time) *ddbackend.Backend {
	t.Helper()
	opts.Surface = datadogx.DefaultSurface()
	opts.APIKey, opts.AppKey = os.Getenv("DD_API_KEY"), os.Getenv("DD_APP_KEY")
	opts.Now = func() time.Time { return now }
	client, err := datadogx.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	s, r := paritySanitiserAndRedactor(t)
	b, err := ddbackend.New(ddbackend.Options{
		OrgSlug: "parity", Client: client, Sanitiser: s, Redactor: r, Site: os.Getenv("DD_SITE"),
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// parityTerms is the algebra over the given log and monitor pointers, one window pair ending an hour
// before the pinned clock, so the window has settled (SC-009 "over a settled window").
//
// new_log_patterns comes first. It pages raw events for the window and again for the baseline, and on
// the first live run it met a logs allowance the three compares, the onset and errors_by_version had
// already drawn down, and never answered (T083). First, it asks on a full allowance.
func parityTerms(selector, monitorID string, now time.Time) []*engine.Term {
	logs := &graphv1.Pointer{
		Kind: graphv1.PointerKind_LOG, BackendKind: "datadog", Vocabulary: feeder.VocabDatadogLogs,
		Selector: selector, JoinKeys: map[string]string{"version": "version"},
	}
	split := now.Add(-90 * time.Minute)
	pair := engine.NewWindowPair(split, 20*time.Minute)
	window := engine.NewWindow(split.Add(-20*time.Minute), split.Add(20*time.Minute))
	terms := []*engine.Term{engine.NewLogPatterns(logs, pair.GetSymptom(), pair.GetBaseline())}
	for _, s := range []investigationv1.Statistic{investigationv1.Statistic_COUNT, investigationv1.Statistic_RATE,
		investigationv1.Statistic_ERROR_RATE} {
		terms = append(terms, engine.Compare(logs, pair, s))
	}
	terms = append(terms,
		engine.Onset(logs, window, investigationv1.OnsetMethod_SEASONAL_CUSUM),
		engine.ErrorsByVersion(logs, window, "version"),
	)
	if monitorID != "" {
		monitor := &graphv1.Pointer{
			Kind: graphv1.PointerKind_LOG, BackendKind: "datadog", Vocabulary: feeder.VocabDatadogMonitor,
			Selector: "recorded: ", Attributes: map[string]string{feeder.AttrDatadogMonitorID: monitorID},
		}
		terms = append(terms, engine.MonitorState(monitor, window))
	}
	return terms
}

func TestLiveDigestParity(t *testing.T) {
	if os.Getenv("DD_LIVE_PARITY") == "" {
		t.Skip("set DD_LIVE_PARITY=1, with the keys, DD_SITE and DD_PARITY_SELECTOR, to run against an organisation")
	}
	selector := os.Getenv("DD_PARITY_SELECTOR")
	if selector == "" || os.Getenv("DD_SITE") == "" {
		t.Fatal("DD_PARITY_SELECTOR and DD_SITE are required")
	}
	now := time.Now().UTC().Truncate(time.Minute)
	terms := parityTerms(selector, os.Getenv("DD_PARITY_MONITOR"), now)

	rec := &recordingTransport{next: http.DefaultTransport, seen: map[string][]recordedExchange{}}
	live := parityBackend(t, datadogx.Options{Site: os.Getenv("DD_SITE"), HTTP: &http.Client{
		Transport: rec, Timeout: 60 * time.Second}}, now)
	liveDigests := make([][]byte, len(terms))
	liveOutcomes := make([]string, len(terms))
	unanswered := make([]bool, len(terms))
	kept := map[string][]recordedExchange{}
	for i, term := range terms {
		// The organisation's logs allowance can be as small as two calls per window. A term that meets
		// it waits for the reset Datadog stated and asks again; only the attempt that answered is kept,
		// so the replay is asked exactly what that answer used.
		var resp *engine.Response
		for attempt := 1; ; attempt++ {
			rec.mu.Lock()
			rec.seen = map[string][]recordedExchange{}
			rec.mu.Unlock()
			var err error
			resp, err = live.Execute(context.Background(), &engine.Request{Term: term})
			if err != nil {
				t.Fatalf("live %s: %v", engine.TermName(term), err)
			}
			if resp.GetFailureReason() != investigationv1.FailureReason_RATE_LIMITED || attempt == parityAttempts {
				break
			}
			rec.mu.Lock()
			wait, stated := rateLimitWait(rec.seen, time.Now())
			rec.mu.Unlock()
			why := "for Datadog's stated reset"
			if !stated {
				why = "with no reset stated"
			}
			t.Logf("%s: rate-limited (attempt %d of %d), waiting %s %s", engine.TermName(term), attempt,
				parityAttempts, wait.Round(time.Second), why)
			time.Sleep(wait)
		}
		for k, v := range rec.seen {
			kept[k] = append(kept[k], v...)
		}
		liveDigests[i] = canonicalDigest(t, resp)
		liveOutcomes[i] = outcomeOf(resp)
		unanswered[i] = resp.GetFailureReason() == investigationv1.FailureReason_RATE_LIMITED
	}

	srv := replayServer(t, kept)
	replayed := parityBackend(t, datadogx.Options{BaseURL: srv.URL}, now)
	for i, term := range terms {
		resp, err := replayed.Execute(context.Background(), &engine.Request{Term: term})
		if err != nil {
			t.Fatalf("replayed %s: %v", engine.TermName(term), err)
		}
		got := canonicalDigest(t, resp)
		name := engine.TermName(term)
		if !bytes.Equal(got, liveDigests[i]) {
			t.Errorf("%s: the digest from the recorded response differs from the live one (%d vs %d bytes)",
				name, len(got), len(liveDigests[i]))
			continue
		}
		sum := sha256.Sum256(got)
		t.Logf("%s: %s, identical, %d bytes, sha256 %x", name, liveOutcomes[i], len(got), sum[:8])
	}
	// A term Datadog never answered replays its refusal identically, which proves the refusal is a
	// function of the response and nothing about the term's content. The first live run reported such a
	// term among "7 of 7 identical" (T083); it is a failure of the run, named, not a pass.
	for i, term := range terms {
		if unanswered[i] {
			t.Errorf("%s: still rate-limited after %d attempts, so its parity on real content is not established",
				engine.TermName(term), parityAttempts)
		}
	}
	calls := 0
	for _, answers := range kept {
		calls += len(answers)
	}
	t.Logf("%d terms, %d Datadog calls recorded and served back", len(terms), calls)
}

// rateLimitWait is how long to wait after a rate-limited attempt: until the latest 429's stated reset,
// plus a second for the calendar boundary, bounded by parityMaxWait. stated is false when no 429 said,
// and the wait is then parityPace.
func rateLimitWait(seen map[string][]recordedExchange, now time.Time) (wait time.Duration, stated bool) {
	var reset time.Time
	for _, answers := range seen {
		for _, a := range answers {
			if a.status != http.StatusTooManyRequests {
				continue
			}
			if reading, ok := feeder.DatadogReadingFromHeaders(a.header, a.at); ok && reading.Reset.After(reset) {
				reset = reading.Reset
			}
		}
	}
	if reset.IsZero() {
		return parityPace, false
	}
	return min(max(reset.Sub(now), 0)+time.Second, parityMaxWait), true
}

// canonicalDigest is the whole answer as bytes — outcome, failure reason and detail, digest, coverage
// and the response hash — with only the call's wall-clock duration removed, which is the transport's
// and not the response's.
func canonicalDigest(t *testing.T, resp *engine.Response) []byte {
	t.Helper()
	c := proto.Clone(resp).(*engine.Response)
	c.DurationMs = 0
	b, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(c)
	if err != nil {
		t.Fatal(fmt.Errorf("marshal the answer: %w", err))
	}
	return b
}

// outcomeOf names what a term answered, for the report: the outcome and, if it failed, why.
func outcomeOf(resp *engine.Response) string {
	if r := resp.GetFailureReason(); r != investigationv1.FailureReason_FAILURE_REASON_UNSPECIFIED {
		return resp.GetOutcome().String() + " " + r.String()
	}
	return resp.GetOutcome().String()
}
