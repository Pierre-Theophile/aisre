// SPDX-License-Identifier: Apache-2.0

package datadogx_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/datadogx"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

var clientNow = time.Date(2026, 9, 27, 14, 0, 0, 0, time.UTC)

func newClient(t *testing.T, h http.HandlerFunc) (*datadogx.Client, *int64) {
	t.Helper()
	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	c, err := datadogx.New(datadogx.Options{
		BaseURL: srv.URL, APIKey: "api-key", AppKey: "app-key", Surface: datadogx.DefaultSurface(),
		Now: func() time.Time { return clientNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	return c, &calls
}

// An operation off the surface is refused before any request leaves the process.
func TestAnUnpublishedOperationNeverLeavesTheProcess(t *testing.T) {
	t.Parallel()
	c, calls := newClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	_, err := c.Do(context.Background(), "POST /api/v1/monitor/{monitor_id}/mute", "/api/v1/monitor/1/mute", nil, struct{}{})
	var unpublished *feeder.UnpublishedOperationError
	if !errors.As(err, &unpublished) {
		t.Fatalf("err = %v, want UnpublishedOperationError", err)
	}
	if *calls != 0 {
		t.Errorf("%d requests reached the server", *calls)
	}
}

// The keys travel as headers, the rate-limit reading is kept per bucket, and a 429 carries the retry
// delay from the relative reset.
func TestHeadersQuotaAndRateLimit(t *testing.T) {
	t.Parallel()
	c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("DD-API-KEY") != "api-key" || r.Header.Get("DD-APPLICATION-KEY") != "app-key" {
			t.Errorf("keys not sent as headers")
		}
		w.Header().Set("X-RateLimit-Name", "logs_search")
		w.Header().Set("X-RateLimit-Limit", "300")
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", "42")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"errors":["Rate limit exceeded"]}`))
	})
	_, _, err := c.SearchLogs(context.Background(), datadogx.SearchRequest{
		Filter: datadogx.NewLogFilter("service:a env:b", clientNow.Add(-time.Hour), clientNow, nil),
		Page:   datadogx.SearchPage{Limit: 10},
	})
	var status *datadogx.StatusError
	if !errors.As(err, &status) || status.Status != 429 || status.RetryAfter != 42*time.Second {
		t.Fatalf("err = %v, want a 429 retrying after 42s", err)
	}
	if r := c.Readings()["logs_search"]; r.Remaining != 0 || r.Limit != 300 {
		t.Errorf("reading %+v", r)
	}
}
