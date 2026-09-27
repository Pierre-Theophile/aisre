// SPDX-License-Identifier: Apache-2.0

package doorbell_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/pkg/feeder/doorbell"
)

// The HTTP doorbell (005 T024, FR-025b, SC-020): a forged, replayed or malformed ring costs at most one
// poll and writes nothing.

const secret = "0123456789abcdef-doorbell"

func newEndpoint(t *testing.T, now *time.Time) (*doorbell.HTTP, *doorbell.Bell, *int) {
	t.Helper()
	bell, err := doorbell.New(doorbell.Options{
		Channel: "/doorbell/datadog", ChannelLabel: "endpoint", MinInterval: 20 * time.Second,
		Now: func() time.Time { return *now },
	})
	if err != nil {
		t.Fatal(err)
	}
	polls := 0
	h, err := doorbell.NewHTTP(doorbell.HTTPOptions{
		Bell: bell, Header: "X-Doorbell-Secret", Secret: secret, OnPoll: func() { polls++ },
	})
	if err != nil {
		t.Fatal(err)
	}
	return h, bell, &polls
}

func ring(h http.Handler, method, secretValue, body string) int {
	req := httptest.NewRequest(method, "/doorbell/datadog", strings.NewReader(body))
	if secretValue != "" {
		req.Header.Set("X-Doorbell-Secret", secretValue)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

// A flood of valid rings inside one interval earns one poll; the next interval earns one more.
func TestAFloodCostsAtMostOnePollPerInterval(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 27, 14, 0, 0, 0, time.UTC)
	h, bell, polls := newEndpoint(t, &now)
	for range 50 {
		if code := ring(h, http.MethodPost, secret, `{"alert":"anything"}`); code != http.StatusAccepted {
			t.Fatalf("a valid ring answered %d", code)
		}
	}
	if *polls != 1 {
		t.Fatalf("50 rings in one interval earned %d polls, want 1", *polls)
	}
	now = now.Add(20 * time.Second)
	ring(h, http.MethodPost, secret, "")
	if *polls != 2 {
		t.Errorf("a ring after the interval earned %d polls in total, want 2", *polls)
	}
	if r := bell.Report(); r.Rings != 51 || r.Honoured != 2 || r.RateLimited != 49 {
		t.Errorf("report %+v", r)
	}
}

// Missing, wrong and truncated secrets are refused before the bucket, counted, and cost no poll.
func TestAForgedRingIsRefusedAndCounted(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 27, 14, 0, 0, 0, time.UTC)
	h, bell, polls := newEndpoint(t, &now)
	for _, value := range []string{"", "wrong-secret-of-some-length", secret[:len(secret)-1], secret + "x"} {
		if code := ring(h, http.MethodPost, value, `{"trust":"me"}`); code != http.StatusUnauthorized {
			t.Errorf("secret %q answered %d, want 401", value, code)
		}
	}
	if code := ring(h, http.MethodGet, secret, ""); code != http.StatusMethodNotAllowed {
		t.Errorf("a GET answered %d, want 405", code)
	}
	if *polls != 0 {
		t.Errorf("forged rings earned %d polls", *polls)
	}
	r := bell.Report()
	if r.Refused != 4 || r.Rings != 0 {
		t.Errorf("report %+v: want 4 refused and nothing reaching the bucket", r)
	}
	if strings.Contains(r.String(), secret) {
		t.Error("the report carries the secret")
	}
}

// The body is never needed: a malformed or enormous body rings exactly as an empty one does.
func TestTheBodyIsNeverData(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 27, 14, 0, 0, 0, time.UTC)
	h, _, polls := newEndpoint(t, &now)
	if code := ring(h, http.MethodPost, secret, strings.Repeat("{not json", 100_000)); code != http.StatusAccepted {
		t.Fatalf("a malformed body answered %d", code)
	}
	if *polls != 1 {
		t.Errorf("polls = %d", *polls)
	}
}

// An incomplete configuration is refused rather than served as an open door.
func TestAnIncompleteHTTPDoorbellIsRefused(t *testing.T) {
	t.Parallel()
	bell, err := doorbell.New(doorbell.Options{Channel: "/d", MinInterval: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	for name, opts := range map[string]doorbell.HTTPOptions{
		"no bell":      {Header: "X-S", Secret: secret},
		"no header":    {Bell: bell, Secret: secret},
		"short secret": {Bell: bell, Header: "X-S", Secret: "short"},
	} {
		if _, err := doorbell.NewHTTP(opts); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := doorbell.New(doorbell.Options{Channel: "/d"}); err == nil {
		t.Error("a bell with no rate limit was accepted")
	}
}
