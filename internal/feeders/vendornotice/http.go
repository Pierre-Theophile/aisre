// SPDX-License-Identifier: Apache-2.0

package vendornotice

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/deprecation"
)

// The shared HTTP client (T082, T083, FR-075, research §10).
//
// Two sources read HTTP — the status page and the changelog feeds — and one more thing rides on the
// same transport: the `Deprecation` and `Sunset` response headers of **every** vendor API this
// integration already calls (see deprecation.go). That is why the client lives beside the sources
// rather than inside one of them.
//
// # It identifies itself honestly
//
// The User-Agent names the project and the feeder and carries a contact URL. It carries nothing about
// the organisation running it: a user agent is sent to every third party polled, and an organisation
// slug in one would tell a vendor who is watching them, which is not this feeder's information to
// give away. Honesty here is about being identifiable and reachable, not about being specific.
//
// # A publisher's stated limit is an instruction
//
// A `429` or a `503` with `Retry-After` is the publisher saying how often they will answer. The client
// waits it out once and retries, and beyond that gives up and lets the cycle record a gap. Polling
// through a stated limit gets the poller blocked, and a blocked source reads as a quiet week — the
// failure this whole feeder exists to make visible.
//
// # It reads a bounded body
//
// A feed is a few hundred kilobytes. A response that is not is either a mistake or a vendor's whole
// archive, and reading it into memory to parse it would turn one bad poll into an outage of the thing
// that is supposed to be watching for outages.

// UserAgent is what this feeder sends. It names the project, not the organisation running it.
const UserAgent = "sre-agent-vendor-notice/1.0 (+https://github.com/Pierre-Theophile/aisre)"

// MaxResponseBytes bounds every response this feeder reads.
const MaxResponseBytes = 8 << 20

// DefaultHTTPTimeout bounds one request.
const DefaultHTTPTimeout = 30 * time.Second

// MaxRetryAfter is the longest stated limit the client will wait out inside a cycle. A vendor asking
// for longer than this is asking to be polled next cycle, so the source records a gap and the
// checkpoint says the window was not read.
const MaxRetryAfter = 2 * time.Minute

// Client is the HTTP client the vendor sources share.
type Client struct {
	http    *http.Client
	agent   string
	sleep   func(ctx context.Context, d time.Duration) error
	headers *deprecation.Log
}

// ClientOptions configures it.
type ClientOptions struct {
	// HTTP is the underlying client. Nil builds one with DefaultHTTPTimeout.
	HTTP *http.Client
	// UserAgent overrides the published one. A deployment may want to add a contact address; it
	// must not add anything identifying the organisation's estate.
	UserAgent string
	// Sleep waits out a stated limit. Nil uses a context-aware sleep; a test supplies its own so
	// that the wait is asserted without spending it.
	Sleep func(ctx context.Context, d time.Duration) error
	// Deprecations collects the `Deprecation` and `Sunset` headers seen on every response. Nil
	// discards them, which is what a test that is not about them wants.
	Deprecations *deprecation.Log
}

// NewClient builds the shared client.
func NewClient(opts ClientOptions) *Client {
	c := &Client{
		http:    opts.HTTP,
		agent:   opts.UserAgent,
		sleep:   opts.Sleep,
		headers: opts.Deprecations,
	}
	if c.http == nil {
		// Its own transport rather than http.DefaultTransport. A shared transport shares its idle
		// connection pool with every other client in the process, so one of them calling
		// CloseIdleConnections breaks a request this one has in flight — which shows up as a vendor
		// being unreachable, and would be recorded as a gap in that vendor's coverage.
		transport := http.DefaultTransport.(*http.Transport).Clone()
		c.http = &http.Client{Timeout: DefaultHTTPTimeout, Transport: transport}
	}
	if c.agent == "" {
		c.agent = UserAgent
	}
	if c.sleep == nil {
		c.sleep = sleepContext
	}
	return c
}

// ErrStatedLimit reports that the publisher asked to be polled later than this cycle allows.
var ErrStatedLimit = errors.New("vendornotice: the publisher stated a limit longer than one cycle")

// Response is what Get returns: the body, already read and bounded, and the status.
type Response struct {
	Status int
	Body   []byte
	// Header is the response header, for an adapter that needs one. The body is the untrusted part;
	// so are these, and neither is ever stored as content.
	Header http.Header
}

// Get fetches one URL, waiting out one stated limit.
//
// It returns a Response for any status the server gave, including a 404 — deciding what a 404 means is
// the adapter's business, and for the status-page probe it means "not a Statuspage", which is a finding
// rather than an error (FR-075).
func (c *Client) Get(ctx context.Context, url string) (Response, error) {
	for attempt := 0; attempt < 2; attempt++ {
		resp, wait, err := c.get(ctx, url)
		if err != nil {
			return Response{}, err
		}
		if wait <= 0 {
			return resp, nil
		}
		if wait > MaxRetryAfter {
			return Response{}, fmt.Errorf("%w: %s asked for %s, longer than one cycle allows (%s); "+
				"the cycle records a gap rather than polling through a limit the publisher stated",
				ErrStatedLimit, url, wait, MaxRetryAfter)
		}
		if err := c.sleep(ctx, wait); err != nil {
			return Response{}, err
		}
	}
	return Response{}, fmt.Errorf("vendornotice: %s stated a limit twice in one cycle; it is asking "+
		"to be polled next cycle, so this one records a gap", url)
}

// get performs one request and reports any stated limit rather than acting on it.
func (c *Client) get(ctx context.Context, url string) (Response, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Response{}, 0, fmt.Errorf("vendornotice: building a request for %s: %w", url, err)
	}
	req.Header.Set("User-Agent", c.agent)
	req.Header.Set("Accept", "application/json, application/atom+xml, application/rss+xml, "+
		"application/feed+json, text/html;q=0.8, */*;q=0.1")

	resp, err := c.http.Do(req)
	if err != nil {
		return Response{}, 0, fmt.Errorf("vendornotice: fetching %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Recorded before anything decides what the response means, because a deprecation notice on a
	// 404 or a 429 is still a deprecation notice — and the endpoint most likely to carry one is the
	// endpoint that has stopped working.
	if c.headers != nil {
		c.headers.Observe(req.URL.Host, url, resp.Header)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		return Response{}, 0, fmt.Errorf("vendornotice: reading %s: %w", url, err)
	}
	if len(body) > MaxResponseBytes {
		return Response{}, 0, fmt.Errorf("vendornotice: %s returned more than %d bytes; a feed that "+
			"large is a vendor's whole archive, and reading it would turn one bad poll into an "+
			"outage of the thing watching for outages", url, MaxResponseBytes)
	}
	return Response{Status: resp.StatusCode, Body: body, Header: resp.Header.Clone()},
		statedLimit(resp), nil
}

// statedLimit reads a publisher's own instruction about when to come back.
func statedLimit(resp *http.Response) time.Duration {
	if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode != http.StatusServiceUnavailable {
		return 0
	}
	value := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if value == "" {
		// A limit with no stated interval is still a limit. One cycle is the answer, and the caller
		// turns the too-long wait into a gap rather than guessing a shorter one.
		return MaxRetryAfter + time.Second
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds < 0 {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		wait := time.Until(at)
		if wait < 0 {
			return 0
		}
		return wait
	}
	return MaxRetryAfter + time.Second
}
