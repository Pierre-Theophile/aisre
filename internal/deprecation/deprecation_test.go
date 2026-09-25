// SPDX-License-Identifier: Apache-2.0

package deprecation_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/deprecation"
)

// The two headers (T082, 003 FR-060).

func TestBothSpellingsOfDeprecationAreRead(t *testing.T) {
	t.Parallel()

	want := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, header := range []string{
		"@1767225600",                   // RFC 9745's structured-field Date.
		"Thu, 01 Jan 2026 00:00:00 GMT", // The draft's HTTP-date, which deployed vendors still send.
		" @1767225600 ",                 // Whitespace around either is not a different header.
	} {
		got, ok := deprecation.ParseDeprecation(header)
		if !ok {
			t.Errorf("Deprecation: %q was not read; refusing the older spelling means reading "+
				"nothing from the APIs most likely to be deprecating something", header)
			continue
		}
		if !got.Equal(want) {
			t.Errorf("Deprecation: %q parsed to %s, want %s", header, got, want)
		}
	}
}

// A header that does not carry an instant must not produce one. `true` says the resource is deprecated
// and says nothing about when; representing that is the caller's job, and inventing today's date here
// would put a valid-from on a change the vendor never dated (FR-069).
func TestADeprecationHeaderWithNoInstantYieldsNoInstant(t *testing.T) {
	t.Parallel()

	for _, header := range []string{"", "  ", "true", "@", "@ ", "@1767225600.5", "@not-a-number", "soon"} {
		if at, ok := deprecation.ParseDeprecation(header); ok {
			t.Errorf("Deprecation: %q was read as %s", header, at)
		}
	}
}

// Sunset is an HTTP-date and nothing else (RFC 8594 §3). Reading an `@`-form as one would invent a
// removal instant from a field that does not carry it.
func TestSunsetIsAnHTTPDateAndNothingElse(t *testing.T) {
	t.Parallel()

	want := time.Date(2026, 3, 31, 23, 59, 59, 0, time.UTC)
	got, ok := deprecation.ParseSunset("Tue, 31 Mar 2026 23:59:59 GMT")
	if !ok || !got.Equal(want) {
		t.Fatalf("Sunset parsed to %s ok=%v, want %s", got, ok, want)
	}
	for _, header := range []string{"@1774915199", "2026-03-31T23:59:59Z", "soon", ""} {
		if at, ok := deprecation.ParseSunset(header); ok {
			t.Errorf("Sunset: %q was read as %s; RFC 8594 publishes an HTTP-date and nothing else",
				header, at)
		}
	}
}

// The log keys by resource, not by request: one endpoint called ten times is one notice, and two
// endpoints of one host deprecating on different dates are two.
func TestTheLogHoldsOneNoticePerResource(t *testing.T) {
	t.Parallel()

	log := deprecation.NewLog()
	first := http.Header{"Deprecation": []string{"@1767225600"}}
	second := http.Header{"Sunset": []string{"Tue, 31 Mar 2026 23:59:59 GMT"}}
	for range 3 {
		log.Observe("api.acme.test", "https://api.acme.test/v1/models", first)
	}
	// A second call to the same resource carrying the other header completes the same notice rather
	// than starting a new one: the pair is one interval.
	log.Observe("api.acme.test", "https://api.acme.test/v1/models", second)
	log.Observe("api.acme.test", "https://api.acme.test/v1/jobs", second)

	notices := log.Take()
	if len(notices) != 2 {
		t.Fatalf("the log holds %d notices, want one per resource: %+v", len(notices), notices)
	}
	if notices[0].URL != "https://api.acme.test/v1/jobs" {
		t.Errorf("notices are not in URL order: %+v", notices)
	}
	models := notices[1]
	if models.Deprecated.IsZero() || models.Sunset.IsZero() {
		t.Errorf("the two headers of one resource did not land on one notice: %+v", models)
	}
	if models.Host != "api.acme.test" {
		t.Errorf("the notice carries host %q", models.Host)
	}

	// And Take clears: a notice reported in one cycle must not be reported again in the next, or the
	// same deprecation would be announced every cycle for ever.
	if left := log.Take(); len(left) != 0 {
		t.Errorf("Take left %d notices behind", len(left))
	}
}

// A response with neither header is almost every response. It must not create an empty notice, or the
// log would grow one entry per URL the integration has ever called.
func TestAResponseWithNeitherHeaderIsNotANotice(t *testing.T) {
	t.Parallel()

	log := deprecation.NewLog()
	log.Observe("api.acme.test", "https://api.acme.test/v1/models", http.Header{"Etag": []string{"x"}})
	log.Observe("api.acme.test", "https://api.acme.test/v1/models", http.Header{})
	if notices := log.Take(); len(notices) != 0 {
		t.Errorf("a response carrying neither header produced %+v", notices)
	}
}

// The transport observes on the way past and changes nothing. It also observes an error status: the
// endpoint most likely to carry a sunset is the endpoint that has stopped working.
func TestTheTransportObservesWithoutChangingTheResponse(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Sunset", "Tue, 31 Mar 2026 23:59:59 GMT")
		w.Header().Set("Etag", "unchanged")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("gone"))
	}))
	defer server.Close()

	log := deprecation.NewLog()
	client := &http.Client{Transport: deprecation.Transport(nil, log)}
	resp, err := client.Get(server.URL + "/v1/models")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("the transport changed the status to %d", resp.StatusCode)
	}
	if resp.Header.Get("Etag") != "unchanged" {
		t.Error("the transport altered the response headers")
	}
	notices := log.Take()
	if len(notices) != 1 || notices[0].Sunset.IsZero() {
		t.Fatalf("the sunset on a 404 was not observed: %+v", notices)
	}
	if notices[0].Host == "" || notices[0].URL == "" {
		t.Errorf("the notice does not say where it came from: %+v", notices[0])
	}
}

// A nil log leaves the transport alone rather than wrapping it in a no-op, so a caller that does not
// want the capture pays nothing for it.
func TestANilLogIsNotWrapped(t *testing.T) {
	t.Parallel()

	base := http.DefaultTransport
	if got := deprecation.Transport(base, nil); got != base {
		t.Error("a nil log still wrapped the transport")
	}
	// And a nil *Log is safe to call, so a call site stays unconditional.
	var none *deprecation.Log
	none.Observe("api.acme.test", "https://api.acme.test", http.Header{"Sunset": []string{"x"}})
	if notices := none.Take(); notices != nil {
		t.Errorf("a nil log returned %+v", notices)
	}
}
