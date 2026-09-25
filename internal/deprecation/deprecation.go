// SPDX-License-Identifier: Apache-2.0

// Package deprecation reads the `Deprecation` and `Sunset` response headers, from any HTTP client in
// this repository (T082, 003 FR-060, research §10).
//
// These two headers are **the only standardised deprecation source there is**. Everything else the
// vendor-notice feeder reads is a human writing prose in a medium of their choosing; RFC 9745
// (`Deprecation`, Standards Track) and RFC 8594 (`Sunset`, Informational) are a machine-readable
// statement, from the vendor, on the response to a call the integration was already making.
//
// So they are near-free: no new credential, no new cadence, no new budget — an observation on traffic
// that already flows. The cost of missing them is the same as the cost of missing a maintenance email,
// and the coverage audit is what says that cost is real.
//
// It is its own package because two callers need it and neither should depend on the other: the GCP
// layer installs the transport on the clients it builds, and the vendor-notice feeder installs it on
// the client its own sources poll with. A copy in each would be two RFC parsers, and the second one to
// be fixed would be the one nobody remembered.
//
// # Two spellings of `Deprecation`, and only one of `Sunset`
//
// RFC 9745 publishes `Deprecation` as a structured-field Date — `@1735689600`, seconds since the
// epoch. The draft that circulated for years before it used an HTTP-date, and deployed vendors still
// send that. Both are read, because refusing the older spelling would mean reading nothing from the
// APIs most likely to be deprecating something.
//
// `Sunset` is an HTTP-date and nothing else (RFC 8594 §3). A `@`-form `Sunset` is not a Sunset header,
// and reading one as a date would be inventing a removal instant from a field that does not carry one.
package deprecation

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Notice is one endpoint's stated deprecation, as its headers gave it.
type Notice struct {
	// Host is the vendor host the response came from, which is how an allowlist attributes it.
	Host string
	// URL is the resource that carried the header. It is a URL this integration already calls, so
	// it is configuration rather than content, and it is what a human opens to see the notice.
	URL string
	// Deprecated is when the resource became or becomes deprecated (RFC 9745). Zero when only a
	// sunset was stated.
	Deprecated time.Time
	// Sunset is when the resource stops responding (RFC 8594). Zero when none was stated.
	Sunset time.Time
}

// ParseDeprecation reads RFC 9745's structured-field Date, falling back to the HTTP-date of the
// earlier draft.
func ParseDeprecation(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	if rest, ok := strings.CutPrefix(value, "@"); ok {
		// A structured-field Date is an integer number of seconds. A fractional one is a Decimal,
		// which the field does not permit, so it is refused rather than truncated.
		seconds, err := strconv.ParseInt(strings.TrimSpace(rest), 10, 64)
		if err != nil {
			return time.Time{}, false
		}
		return time.Unix(seconds, 0).UTC(), true
	}
	if at, err := http.ParseTime(value); err == nil {
		return at.UTC(), true
	}
	// Some deployments send the literal `true`, from a draft that allowed it. It says the resource is
	// deprecated and says nothing about when — a window with an unknown start — and representing that
	// is the caller's job, not something to invent a date for here.
	return time.Time{}, false
}

// ParseSunset reads RFC 8594's HTTP-date.
func ParseSunset(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	at, err := http.ParseTime(value)
	if err != nil {
		return time.Time{}, false
	}
	return at.UTC(), true
}

// Log collects what the headers of every response said.
//
// It is a log rather than a callback because the headers arrive on requests made for other reasons,
// often on several responses from one host in one cycle, and a caller wants one notice per resource
// rather than one per request.
//
// The zero Log is usable, and a nil *Log discards — so a caller that does not care about deprecation
// headers passes nothing and every call site stays unconditional.
type Log struct {
	mu   sync.Mutex
	seen map[string]Notice
}

// NewLog builds an empty log.
func NewLog() *Log { return &Log{} }

// Observe records the headers of one response. A response carrying neither header is ignored, which
// is almost all of them.
func (l *Log) Observe(host, url string, header http.Header) {
	if l == nil {
		return
	}
	deprecated, hasDeprecation := ParseDeprecation(header.Get("Deprecation"))
	sunset, hasSunset := ParseSunset(header.Get("Sunset"))
	if !hasDeprecation && !hasSunset {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.seen == nil {
		l.seen = map[string]Notice{}
	}
	// Keyed by URL: one endpoint's headers are one notice however many times it was called, and two
	// endpoints of one host deprecating on different dates are two notices.
	notice := l.seen[url]
	notice.Host, notice.URL = host, url
	if hasDeprecation {
		notice.Deprecated = deprecated
	}
	if hasSunset {
		notice.Sunset = sunset
	}
	l.seen[url] = notice
}

// Take returns what was collected, in URL order, and clears the log for the next cycle.
func (l *Log) Take() []Notice {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Notice, 0, len(l.seen))
	for _, notice := range l.seen {
		out = append(out, notice)
	}
	l.seen = map[string]Notice{}
	sort.SliceStable(out, func(i, j int) bool { return out[i].URL < out[j].URL })
	return out
}

// Transport wraps a RoundTripper so that every response it carries is observed.
//
// It observes before anything decides what the response means, because a deprecation notice on a 404
// or a 429 is still a deprecation notice — and the endpoint most likely to carry one is the endpoint
// that has stopped working.
//
// It changes nothing about the request or the response: it is a read of a header on the way past, and
// a transport that altered either would be doing something a caller did not ask for on every call in
// the process.
func Transport(next http.RoundTripper, log *Log) http.RoundTripper {
	if next == nil {
		next = http.DefaultTransport
	}
	if log == nil {
		return next
	}
	return &observer{next: next, log: log}
}

type observer struct {
	next http.RoundTripper
	log  *Log
}

func (o *observer) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := o.next.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}
	o.log.Observe(req.URL.Host, req.URL.String(), resp.Header)
	return resp, nil
}
