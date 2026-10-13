// SPDX-License-Identifier: Apache-2.0

package datadog_test

import (
	"net/http"
	"testing"
	"time"
)

// The live parity run waits for the reset Datadog states, not a guessed minute (T083: new_log_patterns
// stayed rate-limited through five 65-second waits). This runs without an organisation.
func TestParityWaitsForTheStatedReset(t *testing.T) {
	at := time.Date(2026, 9, 29, 23, 0, 0, 0, time.UTC)
	limited := func(reset string) recordedExchange {
		h := http.Header{}
		h.Set("X-RateLimit-Limit", "2")
		h.Set("X-RateLimit-Remaining", "0")
		h.Set("X-RateLimit-Period", "3600")
		h.Set("X-RateLimit-Name", "logs_list")
		h.Set("X-RateLimit-Reset", reset)
		return recordedExchange{status: http.StatusTooManyRequests, header: h, at: at}
	}
	ok := recordedExchange{status: http.StatusOK, header: http.Header{}, at: at}

	cases := []struct {
		name   string
		seen   map[string][]recordedExchange
		now    time.Time
		want   time.Duration
		stated bool
	}{
		{"a stated reset is waited for, past the boundary", map[string][]recordedExchange{
			"a": {ok}, "b": {limited("600")}}, at.Add(10 * time.Second), 591 * time.Second, true},
		{"the latest of two resets", map[string][]recordedExchange{
			"a": {limited("30")}, "b": {limited("900")}}, at, 901 * time.Second, true},
		{"a reset already passed waits a second", map[string][]recordedExchange{
			"a": {limited("5")}}, at.Add(time.Minute), time.Second, true},
		{"a day-long window is bounded", map[string][]recordedExchange{
			"a": {limited("86400")}}, at, parityMaxWait, true},
		{"no 429, no stated reset", map[string][]recordedExchange{"a": {ok}}, at, parityPace, false},
	}
	for _, c := range cases {
		got, stated := rateLimitWait(c.seen, c.now)
		if got != c.want || stated != c.stated {
			t.Errorf("%s: waited %s (stated %v), want %s (stated %v)", c.name, got, stated, c.want, c.stated)
		}
	}
}
