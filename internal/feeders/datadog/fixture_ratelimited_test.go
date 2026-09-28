// SPDX-License-Identifier: Apache-2.0

package datadog_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/datadogx"
	ddfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/datadog"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/source"
)

// datadog-rate-limited-01: the budget under an incident (005 T084–T087; FR-081–FR-084, SC-008).
//
// The payloads are not written by hand. The live poller, the real client and its budget run against a
// structural twin of Datadog's HTTP API that answers with rate-limit headers, and the payloads they push
// are the recording. So a change to the budget, the deferral order or the 429 handling changes this
// fixture, and the test below fails until it is regenerated.
//
// The twin's log-analytics bucket opens the window with 40 calls left; with a share of one half and a
// reserve of 20, the connector's allowance is 19:
//
//   - 14:00 — discovery measures `checkout` (12 aggregates) and the rollout lookup, which must leave half
//     the allowance to the areas ahead of it, is deferred; the monitor poll is complete;
//   - 14:05 — the incident: people working it spend the bucket down to 24. Discovery reads until the
//     reserve is all that is left and is suspended, typed `quota`, until the bucket's stated reset at
//     15:00: its tick is not pushed half-measured; the monitor list answers 429 with a 90 s
//     reset, and the poll is partial, typed `rate_limited`, resuming 90 s after the 429 at 14:06:40;
//   - 14:06 — a doorbell ring inside the wait reads nothing and says so;
//   - 14:10 — discovery is still suspended and costs no call; the monitor window has reset, and the
//     poll is complete again.
//
// SC-008 is asserted to the exact call: the calls the twin served, the calls the budget counted and the
// calls the recorded usage report states are one number, and no call was served out of the reserve.

const rateLimitedFixture = "fixtures/datadog-rate-limited-01"

const (
	twinReserve = 20
	bucketLogs  = "logs_analytics"
	bucketMons  = "monitors_list"
)

// quotaTwin is Datadog's HTTP API as far as the poller reads it, with a remaining quota per bucket.
type quotaTwin struct {
	mu        sync.Mutex
	now       time.Time
	windowEnd time.Time
	remaining map[string]int
	limit     map[string]int
	throttle  time.Duration // non-zero: the monitor list answers 429 with this reset
	served    map[string]int
	reserve   []string // calls served with the reserve all that was left
	page      []byte
}

func (q *quotaTwin) clock() time.Time {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.now
}

func (q *quotaTwin) set(at time.Time, f func()) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.now = at
	if f != nil {
		f()
	}
}

func (q *quotaTwin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q.mu.Lock()
	defer q.mu.Unlock()
	bucket := bucketLogs
	if r.URL.Path == "/api/v1/monitor" {
		bucket = bucketMons
	}
	if q.remaining[bucket] <= twinReserve {
		q.reserve = append(q.reserve, fmt.Sprintf("%s at %s with %d left", bucket, q.now.Format(time.TimeOnly), q.remaining[bucket]))
	}
	q.served[bucket]++
	reset := q.windowEnd.Sub(q.now)
	status := http.StatusOK
	if bucket == bucketMons && q.throttle > 0 {
		q.remaining[bucket], reset, status = 0, q.throttle, http.StatusTooManyRequests
	} else if q.remaining[bucket] > 0 {
		q.remaining[bucket]--
	}
	w.Header().Set("X-RateLimit-Name", bucket)
	w.Header().Set("X-RateLimit-Limit", fmt.Sprint(q.limit[bucket]))
	w.Header().Set("X-RateLimit-Remaining", fmt.Sprint(q.remaining[bucket]))
	w.Header().Set("X-RateLimit-Reset", fmt.Sprint(int(reset.Seconds())))
	w.Header().Set("X-RateLimit-Period", "3600")
	if status != http.StatusOK {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"errors":["Rate limit exceeded"]}`))
		return
	}
	if bucket == bucketMons {
		_, _ = w.Write(q.page)
		return
	}
	var req struct {
		Filter struct{ Query string } `json:"filter"`
	}
	body, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(body, &req)
	info, errs := 5000, 40 // every line carries the service's `version` tag and a host
	if strings.Contains(req.Filter.Query, ":*") && !strings.Contains(req.Filter.Query, " version:*") &&
		!strings.Contains(req.Filter.Query, "host:*") {
		info, errs = 0, 0
	}
	_, _ = fmt.Fprintf(w, `{"data":{"buckets":[{"by":{"status":"info"},"computes":{"c0":%d}},`+
		`{"by":{"status":"error"},"computes":{"c0":%d}}]},"meta":{"status":"done"}}`, info, errs)
}

// rateLimitedRun plays the scenario and returns the payloads pushed, the twin and the budget.
func rateLimitedRun(t *testing.T) ([]feeder.Payload, *quotaTwin, *datadogx.Budget) {
	t.Helper()
	page, err := json.Marshal([]monitor{twinMonitor{
		id: 7001, name: "checkout error rate", kind: "log alert", priority: 2,
		query:  `logs("service:checkout env:production status:error").index("*").rollup("count").last("5m") > 50`,
		tags:   []string{"service:checkout", "env:production"},
		groups: map[string]group{"*": {Status: "OK"}},
	}.json()})
	if err != nil {
		t.Fatal(err)
	}
	twin := &quotaTwin{
		now: hm(14, 0), windowEnd: hm(15, 0), page: page,
		remaining: map[string]int{bucketLogs: 40, bucketMons: 900},
		limit:     map[string]int{bucketLogs: 300, bucketMons: 1000},
		served:    map[string]int{},
	}
	srv := httptest.NewServer(twin)
	t.Cleanup(srv.Close)
	budget, err := datadogx.NewBudget(feeder.QuotaPolicy{Share: 0.5, Reserve: twinReserve, StaticAllowance: 30})
	if err != nil {
		t.Fatal(err)
	}
	client, err := datadogx.New(datadogx.Options{
		BaseURL: srv.URL, APIKey: "api-key", AppKey: "app-key", Surface: datadogx.DefaultSurface(),
		Now: twin.clock, Budget: budget,
	})
	if err != nil {
		t.Fatal(err)
	}
	var out collected
	p := &ddfeeder.Poller{
		Pager: client, Tags: []string{"env:production"}, LogSources: []ddfeeder.LogSource{{Env: "production", Service: "checkout"}},
		Capabilities: ddfeeder.DefaultCapabilities(), Measurer: datadogx.Measurer{Client: client},
		Now: twin.clock, Push: out.push, Usage: budget.Report,
	}
	ctx := context.Background()
	cycle := func(discover bool) {
		if discover {
			if err := p.Discover(ctx); err != nil {
				t.Fatal(err)
			}
		}
		twin.set(twin.clock().Add(10*time.Second), nil)
		if err := p.PollOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	cycle(true)
	twin.set(hm(14, 5), func() { twin.remaining[bucketLogs], twin.throttle = 24, 90*time.Second })
	cycle(true)
	twin.set(hm(14, 6), func() { twin.throttle = 0 })
	cycle(false)
	// The 429's window has reset: the monitor bucket has refilled.
	twin.set(hm(14, 10), func() { twin.remaining[bucketMons] = 880 })
	cycle(true)
	return out.payloads, twin, budget
}

func TestGenerateDatadogRateLimitedFixture(t *testing.T) {
	if os.Getenv(genFixturesEnv) == "" {
		t.Skipf("set %s=1 to regenerate %s", genFixturesEnv, rateLimitedFixture)
	}
	payloads, _, _ := rateLimitedRun(t)
	opts := ddfeeder.Options{OrgSlug: "twin", Site: "datadoghq.eu", MonitorTags: []string{"env:production"},
		LogSources: []ddfeeder.LogSource{{Env: "production", Service: "checkout"}}}
	generateFixture(t, rateLimitedFixture, "datadog-budget",
		"The connector's budget under an incident, recorded from the live poller, the real client and its "+
			"budget against a twin that answers with rate-limit headers. At 14:00 discovery measures `checkout` "+
			"and the rollout lookup is deferred to leave half the share to the areas ahead of it; at 14:05 the "+
			"people working an incident spend the log bucket down, discovery stops at the reserve (typed "+
			"`quota`) and is suspended until the bucket's stated reset rather than pushed half-measured, and "+
			"the monitor list answers 429, so the poll is partial (typed `rate_limited`) and "+
			"resumes at the stated reset; a doorbell ring inside the wait reads nothing; at 14:10 the poll is "+
			"complete again and discovery is still suspended. No monitor is retracted by a partial poll.",
		opts, payloads, hm(13, 59), hm(14, 30), `
queries:
  # A partial poll retracts nothing: the monitor is still an alert after the 429 and the unread ring.
  - name: the-monitor-survives-the-rate-limit
    kind: subgraph
    focus: datadog.monitor=7001
    valid_at: 2026-09-21T14:08:00Z
    observed_at: 2026-09-21T14:30:00Z
    hops: 1
    direction: both
  # The log source's pointer and verdict as the 14:00 measurement left them: the quota stop at 14:05
  # changed nothing it could not read.
  - name: checkout-pointer-through-the-quota-stop
    kind: pointers
    focus: datadog.service=production/checkout
    valid_at: 2026-09-21T14:20:00Z
    observed_at: 2026-09-21T14:30:00Z
`)
}

// T087, SC-008: to the exact call.
func TestTheBudgetHoldsToTheExactCall(t *testing.T) {
	t.Parallel()
	payloads, twin, budget := rateLimitedRun(t)

	// The recording is this run: a change to the budget changes the fixture.
	src, err := source.NewFileSource(repoRoot(t) + "/" + rateLimitedFixture)
	if err != nil {
		t.Fatalf("%v (regenerate with %s=1)", err, genFixturesEnv)
	}
	for i, want := range payloads {
		got, err := src.Next(context.Background())
		if err != nil {
			t.Fatalf("payload %d: %v; the recording holds fewer payloads than the run (regenerate with %s=1)", i, err, genFixturesEnv)
		}
		if got.Kind != want.Kind || !got.At.Equal(want.At) || !bytes.Equal(got.Bytes, want.Bytes) {
			t.Fatalf("payload %d differs from the recording (regenerate with %s=1):\n got %s %s\nwant %s %s",
				i, genFixturesEnv, got.Kind, got.Bytes, want.Kind, want.Bytes)
		}
	}

	// No call was served out of the reserve.
	if len(twin.reserve) > 0 {
		t.Errorf("calls served out of the human reserve: %v", twin.reserve)
	}

	// The twin served, the budget counted and the recorded usage states the same calls.
	served := twin.served[bucketLogs] + twin.served[bucketMons]
	counted := 0
	for _, n := range budget.Calls() {
		counted += n
	}
	if served != counted {
		t.Errorf("the twin served %d calls and the budget counted %d", served, counted)
	}
	// 12 + 1 at 14:00, 4 + 1 (the 429) at 14:05, none at 14:06, 1 at 14:10.
	if served != 19 || twin.served[bucketLogs] != 16 || twin.served[bucketMons] != 3 {
		t.Errorf("served %v, want 16 log-analytics and 3 monitor-list calls", twin.served)
	}
	var markers []ddfeeder.PollMarker
	for _, p := range payloads {
		if p.Kind == ddfeeder.PayloadPoll {
			markers = append(markers, marker(t, p))
		}
	}
	if len(markers) != 4 {
		t.Fatalf("%d poll markers, want 4", len(markers))
	}
	last := markers[3]
	for area, n := range budget.Calls() {
		if !strings.Contains(last.Usage, string(area)+":") {
			t.Errorf("the recorded usage %q does not state area %s (%d calls)", last.Usage, area, n)
		}
	}
	for _, line := range []string{
		fmt.Sprintf("discovery: 16 call(s) on %s", bucketLogs),
		"monitors: 3 call(s) on ",
	} {
		if !strings.Contains(last.Usage, line) {
			t.Errorf("the recorded usage %q does not state %q", last.Usage, line)
		}
	}

	// The deferral order, the typed stops and the resume.
	want := []struct {
		outcome, stop string
		deferred      string
		resume        bool
	}{
		{"complete", "", "rollouts", false},
		{"partial", ddfeeder.StopRateLimited, "discovery", true},
		{"partial", ddfeeder.StopRateLimited, "", true},
		{"complete", "", "discovery", false},
	}
	for i, w := range want {
		m := markers[i]
		if m.Outcome != w.outcome || m.StopReason != w.stop || strings.Join(m.Deferred, ",") != w.deferred || (m.ResumeAt != nil) != w.resume {
			t.Errorf("poll %d: %+v, want outcome %s, stop %q, deferred %q, resume %v", i, m, w.outcome, w.stop, w.deferred, w.resume)
		}
	}
	if r := markers[1].ResumeAt; r == nil || !r.Equal(hms(14, 6, 40)) {
		t.Errorf("resume at %v, want 14:06:40, the 429's stated reset", r)
	}
}
