// SPDX-License-Identifier: Apache-2.0

package datadog_test

import (
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
)

// datadog-small-bucket-01: discovery against a logs bucket of 2 calls per window (005, found on the
// first live run).
//
// The organisation's logs aggregate bucket allows 2 calls per window. Against the published reserve of
// 20 the connector's allowance was zero in every window, and a source was never measured. Two changes
// make it measurable without taking the bucket from the people who share it:
//
//   - the reserve a small bucket is held to is half its limit (pkg/feeder, QuotaBudget.reserveFor): one
//     call per window is the connector's, the other stays unspent;
//   - a measurement the budget stops is suspended until the reset Datadog stated and resumes over the
//     same window, the measurer's cache answering what was already read (poller.go, suspend).
//
// So the twelve aggregates of one measurement are spread over twelve windows, one each, the tick is
// pushed once and complete, and no call is ever served with only the reserve left.

const smallBucketFixture = "fixtures/datadog-small-bucket-01"

const smallBucketName = "logs_public_analytics_aggregate"

// smallBucketTwin answers the logs aggregate with a bucket of 2 calls per 10-second window.
type smallBucketTwin struct {
	mu         sync.Mutex
	now        time.Time
	windowEnd  time.Time
	remaining  int
	served     []time.Time
	fromLast   []string // calls served with only the reserve (1 call) left
	perWindow  map[time.Time]int
	windowSize time.Duration
}

func (q *smallBucketTwin) clock() time.Time {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.now
}

func (q *smallBucketTwin) advance(d time.Duration) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.now = q.now.Add(d)
	for !q.now.Before(q.windowEnd) {
		q.windowEnd, q.remaining = q.windowEnd.Add(q.windowSize), 2
	}
}

func (q *smallBucketTwin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.remaining <= 1 {
		q.fromLast = append(q.fromLast, q.now.Format(time.TimeOnly))
	}
	q.served = append(q.served, q.now)
	q.perWindow[q.windowEnd]++
	if q.remaining > 0 {
		q.remaining--
	}
	w.Header().Set("X-RateLimit-Name", smallBucketName)
	w.Header().Set("X-RateLimit-Limit", "2")
	w.Header().Set("X-RateLimit-Remaining", fmt.Sprint(q.remaining))
	w.Header().Set("X-RateLimit-Reset", fmt.Sprint(int(q.windowEnd.Sub(q.now).Seconds())))
	w.Header().Set("X-RateLimit-Period", "10")
	var req struct {
		Filter struct{ Query string } `json:"filter"`
	}
	body, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(body, &req)
	info, errs := 5000, 40 // every line carries the `version` tag and a host
	if strings.Contains(req.Filter.Query, ":*") && !strings.Contains(req.Filter.Query, " version:*") &&
		!strings.Contains(req.Filter.Query, "host:*") {
		info, errs = 0, 0
	}
	_, _ = fmt.Fprintf(w, `{"data":{"buckets":[{"by":{"status":"info"},"computes":{"c0":%d}},`+
		`{"by":{"status":"error"},"computes":{"c0":%d}}]},"meta":{"status":"done"}}`, info, errs)
}

func smallBucketRun(t *testing.T) ([]feeder.Payload, *smallBucketTwin) {
	t.Helper()
	start := hm(14, 0)
	twin := &smallBucketTwin{now: start, windowEnd: start.Add(10 * time.Second), remaining: 2,
		perWindow: map[time.Time]int{}, windowSize: 10 * time.Second}
	srv := httptest.NewServer(twin)
	t.Cleanup(srv.Close)
	budget, err := datadogx.NewBudget(feeder.QuotaPolicy{Share: 0.5, Reserve: 20, StaticAllowance: 30})
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
		LogSources:   []ddfeeder.LogSource{{Env: "production", Service: "checkout", EnvField: "env"}},
		Capabilities: ddfeeder.DefaultCapabilities(),
		Measurer:     datadogx.Measurer{Client: client, Cache: datadogx.NewMeasureCache()},
		Now:          twin.clock, Push: out.push,
		Sleep: func(_ context.Context, d time.Duration) error {
			twin.advance(d)
			return nil
		},
	}
	if err := p.DiscoverAndWait(context.Background()); err != nil {
		t.Fatal(err)
	}
	return out.payloads, twin
}

func TestGenerateDatadogSmallBucketFixture(t *testing.T) {
	if os.Getenv(genFixturesEnv) == "" {
		t.Skipf("set %s=1 to regenerate %s", genFixturesEnv, smallBucketFixture)
	}
	payloads, _ := smallBucketRun(t)
	generateFixture(t, smallBucketFixture, "datadog-budget",
		"Discovery against a logs aggregate bucket of 2 calls per 10-second window, the shape the first live "+
			"run met. The reserve a small bucket is held to is half its limit, so one call per window is the "+
			"connector's and the other stays unspent; a measurement the budget stops is suspended until the "+
			"stated reset and resumes over the same window, its answers cached. The twelve aggregates of "+
			"`checkout`'s measurement take twelve windows, one each, and the tick is pushed once, complete: "+
			"the `version` tag is accepted and the pointer carries its join key. The rollout lookup that "+
			"follows finds the window's call spent and says so, typed `quota`; it looks again next interval.",
		ddfeeder.Options{OrgSlug: "twin", LogSources: []ddfeeder.LogSource{{Env: "production", Service: "checkout", EnvField: "env"}}},
		payloads, hm(13, 59), hm(14, 5), `
queries:
  # The measurement completed over twelve windows: the pointer carries the version join key.
  - name: checkout-pointer-after-a-paced-discovery
    kind: pointers
    focus: datadog.service=production/checkout
    valid_at: 2026-09-21T14:04:00Z
    observed_at: 2026-09-21T14:05:00Z
`)
}

// One call per window, never the reserve, and one complete tick.
func TestASmallBucketIsPacedNotAbandoned(t *testing.T) {
	t.Parallel()
	payloads, twin := smallBucketRun(t)

	if len(twin.fromLast) > 0 {
		t.Errorf("calls served with only the reserve left: %v", twin.fromLast)
	}
	for end, n := range twin.perWindow {
		if n > 1 {
			t.Errorf("window ending %s served %d calls; the connector's share of a bucket of 2 is 1", end.Format(time.TimeOnly), n)
		}
	}
	if len(twin.served) != 12 {
		t.Errorf("%d calls served, want the 12 aggregates of one measurement", len(twin.served))
	}
	if len(payloads) != 1 || payloads[0].Kind != ddfeeder.PayloadDiscovery {
		t.Fatalf("payloads %d, want one discovery tick pushed once, complete", len(payloads))
	}
	var tick ddfeeder.DiscoveryTick
	if err := json.Unmarshal(payloads[0].Bytes, &tick); err != nil {
		t.Fatal(err)
	}
	m := tick.Measurements[0]
	if m.Failed != "" || m.Lines != 5040 || m.Candidates[0].Lines != 5040 {
		t.Errorf("measurement %+v, want a complete one", m)
	}
	if m.ValuesFailed != ddfeeder.StopQuota {
		t.Errorf("values_failed = %q; the rollout lookup found the window's call spent and must say so", m.ValuesFailed)
	}
	if !tick.Window.To.Equal(hm(14, 0)) {
		t.Errorf("window %v; a resumed measurement keeps the window it started over", tick.Window)
	}
	if elapsed := twin.clock().Sub(hm(14, 0)); elapsed < 110*time.Second || elapsed > 120*time.Second {
		t.Errorf("the measurement took %s, want eleven waits of one window", elapsed)
	}
	if replay := recordedPayloadBytes(t, smallBucketFixture); replay != string(payloads[0].Bytes) {
		t.Errorf("the run differs from the recording (regenerate with %s=1)", genFixturesEnv)
	}
}

func recordedPayloadBytes(t *testing.T, fixture string) string {
	t.Helper()
	raw, err := os.ReadFile(repoRoot(t) + "/" + fixture + "/payloads/discovery/000001.json")
	if err != nil {
		t.Fatalf("%v (regenerate with %s=1)", err, genFixturesEnv)
	}
	return strings.TrimSpace(string(raw))
}
