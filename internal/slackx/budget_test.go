// SPDX-License-Identifier: Apache-2.0

package slackx

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The reader app's budget (006 T008, FR-014).

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// countingTwin answers every call ok, or 429 while limited, and counts what reached it.
type countingTwin struct {
	mu      sync.Mutex
	calls   map[string]int
	limited bool
}

func (tw *countingTwin) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tw.mu.Lock()
		tw.calls[r.URL.Path]++
		limited := tw.limited
		tw.mu.Unlock()
		if limited {
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"ok":false,"error":"ratelimited"}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true,"channels":[],"messages":[],"channel":{"id":"C1"}}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func budgetClient(t *testing.T, srv *httptest.Server, clock *fakeClock) (*Client, *Budget) {
	t.Helper()
	budget := NewBudget(clock.Now)
	c, err := New(Options{Token: "xoxb-reader", BaseURL: srv.URL, Now: clock.Now, History: true, Budget: budget})
	if err != nil {
		t.Fatal(err)
	}
	return c, budget
}

// A method's minute allowance is its tier's, refused before the network once spent, and refilled
// the next minute.
func TestTheBudgetHoldsEachMethodToItsTier(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 10, 14, 9, 0, 5, 0, time.UTC)}
	tw := &countingTwin{calls: map[string]int{}}
	c, budget := budgetClient(t, tw.server(t), clock)
	ctx := t.Context()
	var stopped error
	for range Tier2.PerMinute + 5 {
		if _, err := c.ListChannels(ctx, "", 200); err != nil {
			stopped = err
		}
	}
	if got := tw.calls["/api/conversations.list"]; got != Tier2.PerMinute {
		t.Errorf("%d list calls reached Slack in one minute; tier 2 allows %d", got, Tier2.PerMinute)
	}
	yield, ok := feeder.YieldedForQuota(stopped)
	if !ok || yield.Source != feeder.QuotaStatic || !yield.RetryAt.Equal(time.Date(2026, 10, 14, 9, 1, 0, 0, time.UTC)) {
		t.Errorf("the stop is %v", stopped)
	}
	// Another method has its own window.
	if _, err := c.ChannelInfo(ctx, "C1"); err != nil {
		t.Errorf("conversations.info was stopped by conversations.list's budget: %v", err)
	}
	clock.Add(time.Minute)
	if _, err := c.ListChannels(ctx, "", 200); err != nil {
		t.Errorf("the next minute is still closed: %v", err)
	}
	if report := budget.Report(); !strings.Contains(report, "static_fallback") ||
		!strings.Contains(report, "conversations.list: 1 call(s) this window of a 20/min allowance") {
		t.Errorf("report: %s", report)
	}
}

// Reading leaves a quarter of a shared method's window for intake.
func TestReadingLeavesRoomForIntake(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)}
	tw := &countingTwin{calls: map[string]int{}}
	c, _ := budgetClient(t, tw.server(t), clock)
	reading := WithPriority(t.Context(), PriorityReading)
	var deferred *DeferredError
	n := 0
	for ; n < Tier3.PerMinute; n++ {
		if _, err := c.ChannelInfo(reading, "C1"); err != nil {
			if !errors.As(err, &deferred) {
				t.Fatalf("call %d: %v", n, err)
			}
			break
		}
	}
	if want := Tier3.PerMinute - Tier3.PerMinute/4; n != want+1 && n != want {
		t.Errorf("reading spent %d of %d calls before deferring", n, Tier3.PerMinute)
	}
	for range Tier3.PerMinute - n {
		if _, err := c.ChannelInfo(t.Context(), "C1"); err != nil {
			t.Fatalf("intake was refused a call reading left for it: %v", err)
		}
	}
	if got := tw.calls["/api/conversations.info"]; got != Tier3.PerMinute {
		t.Errorf("%d info calls reached Slack; the window holds %d", got, Tier3.PerMinute)
	}
}

// A 429 closes the method until Retry-After has passed; nothing is sent in between.
func TestARateLimitClosesTheMethodUntilRetryAfter(t *testing.T) {
	clock := &fakeClock{t: time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)}
	tw := &countingTwin{calls: map[string]int{}, limited: true}
	c, budget := budgetClient(t, tw.server(t), clock)
	var apiErr *APIError
	if _, err := c.ListChannels(t.Context(), "", 200); !errors.As(err, &apiErr) || apiErr.RetryAfter != 30*time.Second {
		t.Fatalf("got %v", err)
	}
	tw.limited = false
	clock.Add(10 * time.Second)
	if _, err := c.ListChannels(t.Context(), "", 200); err == nil {
		t.Error("a call was sent inside Retry-After")
	}
	clock.Add(25 * time.Second)
	if _, err := c.ListChannels(t.Context(), "", 200); err != nil {
		t.Errorf("the method stayed closed after Retry-After: %v", err)
	}
	if got := tw.calls["/api/conversations.list"]; got != 2 {
		t.Errorf("%d calls reached Slack; want the limited one and the one after", got)
	}
	if !strings.Contains(budget.Report(), "rate limited by Slack 1 time(s)") {
		t.Errorf("report: %s", budget.Report())
	}
}
