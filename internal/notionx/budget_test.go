// SPDX-License-Identifier: Apache-2.0

package notionx_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/notionx"
)

// fakeClock advances only when the budget sleeps.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(_ context.Context, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	return nil
}

func budgetedTwin(t *testing.T, status func(n int) (int, string)) (*notionx.Client, *notionx.Budget, *fakeClock, *[]time.Time) {
	t.Helper()
	clock := &fakeClock{now: time.Date(2026, 10, 14, 12, 0, 0, 100_000_000, time.UTC)}
	var issued []time.Time
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		issued = append(issued, clock.Now())
		code, retry := status(n)
		n++
		if retry != "" {
			w.Header().Set("Retry-After", retry)
		}
		w.WriteHeader(code)
		_, _ = w.Write([]byte(`{"object":"user","id":"u1","type":"bot","bot":{"workspace_name":"acme"},"code":"rate_limited"}`))
	}))
	t.Cleanup(srv.Close)
	b := notionx.NewBudget(notionx.BudgetOptions{Now: clock.Now, Sleep: clock.Sleep})
	c, err := notionx.New(notionx.Options{Secret: secret(t), BaseURL: srv.URL, Budget: b, Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	return c, b, clock, &issued
}

// TestThreeCallsASecond is FR-044's static allowance: the fourth call in a second waits for the next.
func TestThreeCallsASecond(t *testing.T) {
	c, b, _, issued := budgetedTwin(t, func(int) (int, string) { return http.StatusOK, "" })
	for range 7 {
		if _, err := c.UsersMe(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	perSecond := map[int64]int{}
	for _, at := range *issued {
		perSecond[at.Unix()]++
	}
	for sec, n := range perSecond {
		if n > notionx.PerSecond {
			t.Errorf("%d calls in second %d", n, sec)
		}
	}
	if len(perSecond) != 3 {
		t.Errorf("7 calls spread over %d seconds, want 3", len(perSecond))
	}
	if r := b.Report(); !strings.Contains(r, "7 call(s)") || !strings.Contains(r, "static_fallback") {
		t.Errorf("report %q", r)
	}
}

// TestRetryAfterClosesTheBudget: a 429 closes the budget until the instant Notion names; a short wait
// is waited out, a long one cuts the poll short with a typed error.
func TestRetryAfterClosesTheBudget(t *testing.T) {
	c, _, clock, issued := budgetedTwin(t, func(n int) (int, string) {
		if n == 0 {
			return http.StatusTooManyRequests, "5"
		}
		return http.StatusOK, ""
	})
	ctx := context.Background()
	if _, err := c.UsersMe(ctx); err == nil {
		t.Fatal("the 429 was not reported")
	}
	before := clock.Now()
	if _, err := c.UsersMe(ctx); err != nil {
		t.Fatal(err)
	}
	if got := (*issued)[1].Sub(before); got < 4*time.Second {
		t.Errorf("the call after a 429 went out %s later, want after Retry-After", got)
	}

	c2, _, _, issued2 := budgetedTwin(t, func(int) (int, string) { return http.StatusTooManyRequests, "120" })
	_, _ = c2.UsersMe(ctx)
	_, err := c2.UsersMe(ctx)
	partial, ok := notionx.IsPartial(err)
	if !ok || partial.RetryAt.IsZero() {
		t.Fatalf("a 2-minute Retry-After gave %v, want a PartialPollError", err)
	}
	if len(*issued2) != 1 {
		t.Errorf("%d request(s) went out while the budget was closed", len(*issued2)-1)
	}
}
