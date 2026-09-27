// SPDX-License-Identifier: Apache-2.0

package datadog_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ddfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/datadog"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/doorbell"
)

// pager serves pages of monitors, counting the reads.
type pager struct {
	pages [][]monitor
	fail  int // the page index that fails, or -1
	reads int64
}

func (p *pager) ListMonitorsPage(_ context.Context, _ string, page, _ int) ([]byte, error) {
	atomic.AddInt64(&p.reads, 1)
	if page == p.fail {
		return nil, errors.New("datadog answered 504")
	}
	if page >= len(p.pages) {
		return []byte("[]"), nil
	}
	return json.Marshal(p.pages[page])
}

type collected struct {
	mu       sync.Mutex
	payloads []feeder.Payload
}

func (c *collected) push(_ context.Context, p feeder.Payload) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.payloads = append(c.payloads, p)
	return nil
}

func (c *collected) kinds() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, p := range c.payloads {
		out = append(out, p.Kind)
	}
	return strings.Join(out, ",")
}

func marker(t *testing.T, p feeder.Payload) ddfeeder.PollMarker {
	t.Helper()
	var m ddfeeder.PollMarker
	if err := json.Unmarshal(p.Bytes, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// A poll pushes every page and a complete marker; a failed page ends it as partial, naming why.
func TestAPollPushesItsPagesThenItsMarker(t *testing.T) {
	t.Parallel()
	full := make([]monitor, 2)
	for i := range full {
		full[i] = errorsMonitor(map[string]group{"*": {Status: "OK"}})
	}
	var out collected
	p := &ddfeeder.Poller{Pager: &pager{pages: [][]monitor{full, full[:1]}, fail: -1}, PageSize: 2,
		Capabilities: ddfeeder.DefaultCapabilities(), Push: out.push}
	if err := p.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if out.kinds() != "monitors,monitors,poll" || marker(t, out.payloads[2]).Outcome != "complete" {
		t.Fatalf("payloads %s", out.kinds())
	}

	var partial collected
	p = &ddfeeder.Poller{Pager: &pager{pages: [][]monitor{full, full}, fail: 1}, PageSize: 2,
		Capabilities: ddfeeder.DefaultCapabilities(), Push: partial.push}
	if err := p.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	m := marker(t, partial.payloads[len(partial.payloads)-1])
	if m.Outcome != "partial" || !strings.Contains(m.Reason, "page 1") {
		t.Fatalf("marker %+v", m)
	}
}

// A forged ring is refused before the bell; a valid one asks for one poll; rings between two polls
// coalesce; the body is never read (contract §3.1, SC-020).
func TestTheDoorbellAsksForAPollAndNothingElse(t *testing.T) {
	t.Parallel()
	pg := &pager{fail: -1}
	var out collected
	p := &ddfeeder.Poller{Pager: pg, Capabilities: ddfeeder.DefaultCapabilities(), Push: out.push,
		Interval: time.Hour, DiscoveryInterval: time.Hour}
	bell, err := doorbell.New(doorbell.Options{Channel: "test", MinInterval: time.Hour, Burst: 1})
	if err != nil {
		t.Fatal(err)
	}
	h, err := doorbell.NewHTTP(doorbell.HTTPOptions{Bell: bell, Header: "X-Doorbell-Secret",
		Secret: "a-shared-secret-long-enough", OnPoll: p.PollNow})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	ring := func(secret, body string) int {
		req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(body))
		req.Header.Set("X-Doorbell-Secret", secret)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()
	waitFor(t, func() bool { return atomic.LoadInt64(&pg.reads) == 1 }) // the first poll

	if code := ring("forged", `{"ignored":true}`); code != http.StatusUnauthorized {
		t.Errorf("a forged ring answered %d", code)
	}
	if code := ring("a-shared-secret-long-enough", "not json at all {"); code != http.StatusAccepted {
		t.Errorf("a valid ring with a malformed body answered %d", code)
	}
	waitFor(t, func() bool { return atomic.LoadInt64(&pg.reads) == 2 })
	for range 5 { // a replayed ring, five times over: rate-limited
		ring("a-shared-secret-long-enough", "")
	}
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt64(&pg.reads); got != 2 {
		t.Errorf("%d polls, want the first and one for the rings", got)
	}
	r := bell.Report()
	if r.Refused != 1 || r.Honoured != 1 || r.RateLimited != 5 {
		t.Errorf("bell report %s", r)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
