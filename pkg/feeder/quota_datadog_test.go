// SPDX-License-Identifier: Apache-2.0

package feeder_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// Datadog's rate-limit headers (005 T021–T022).

func datadogHeaders(name string, limit, remaining, reset, period string) http.Header {
	h := http.Header{}
	for k, v := range map[string]string{
		"X-RateLimit-Name": name, "X-RateLimit-Limit": limit, "X-RateLimit-Remaining": remaining,
		"X-RateLimit-Reset": reset, "X-RateLimit-Period": period,
	} {
		if v != "" {
			h.Set(k, v)
		}
	}
	return h
}

var datadogReceived = time.Date(2026, 9, 27, 14, 0, 30, 0, time.UTC)

// The reset is relative to the response, so it lands in the future — not in 1970, which is what the
// GitHub reader makes of the same header.
func TestADatadogResetIsSecondsFromTheResponse(t *testing.T) {
	t.Parallel()
	h := datadogHeaders("logs_search", "300", "120", "30", "3600")
	got, ok := feeder.DatadogReadingFromHeaders(h, datadogReceived)
	if !ok {
		t.Fatal("a response carrying the headers read as carrying none")
	}
	if want := datadogReceived.Add(30 * time.Second); !got.Reset.Equal(want) {
		t.Errorf("reset %s, want %s", got.Reset, want)
	}
	if got.Family != "logs_search" || got.Limit != 300 || got.Remaining != 120 || got.Used != 180 ||
		got.Period != time.Hour || got.Source != feeder.QuotaReported {
		t.Errorf("reading %+v", got)
	}
}

// Two endpoints sharing a rate-limit name are one bucket, and the reading says so.
func TestEndpointsSharingANameShareABucket(t *testing.T) {
	t.Parallel()
	a, _ := feeder.DatadogReadingFromHeaders(datadogHeaders("logs", "300", "10", "5", "60"), datadogReceived)
	b, _ := feeder.DatadogReadingFromHeaders(datadogHeaders("logs", "300", "9", "4", "60"), datadogReceived)
	if a.Family != b.Family {
		t.Errorf("two endpoints sharing a rate-limit name read as different buckets: %q and %q", a.Family, b.Family)
	}
}

// No headers is a first-class answer, never a zero reading: the caller falls back to the static
// budget and says so (FR-081a).
func TestNoDatadogHeadersIsNoReading(t *testing.T) {
	t.Parallel()
	if _, ok := feeder.DatadogReadingFromHeaders(http.Header{}, datadogReceived); ok {
		t.Error("a response with no rate-limit headers produced a reading")
	}
	if got, ok := feeder.DatadogReadingFromHeaders(datadogHeaders("", "300", "300", "", ""), datadogReceived); !ok ||
		got.Family != "unnamed" || !got.Reset.IsZero() {
		t.Errorf("an unnamed bucket with no reset read as %+v", got)
	}
}
