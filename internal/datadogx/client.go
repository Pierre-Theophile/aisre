// SPDX-License-Identifier: Apache-2.0

// Package datadogx is the one read-only Datadog client both halves of the connector share: the feeder
// (internal/feeders/datadog) and the telemetry backend (internal/backends/datadog). One credential, one
// published operation surface, one place that reads the rate-limit headers (005 FR-001, FR-081).
//
// It is hand-written over net/http rather than a generated SDK on purpose (plan §Technical Context):
// the connector issues a handful of operations, and the published read-only list is only a proof while
// it is the literal set of requests this binary can make. Every request goes through Do, which refuses
// an operation the surface does not carry before anything leaves the process.
package datadogx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	ddfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/datadog"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// Options configures the client.
type Options struct {
	// Site is the Datadog site, e.g. "datadoghq.eu". The API host is `api.<site>`.
	Site string
	// BaseURL overrides the host, for tests against a recorded twin. Empty derives it from Site.
	BaseURL string
	// APIKey and AppKey are the credential. Never logged, never recorded, never in a pointer.
	APIKey, AppKey string
	// Surface is the published operation surface for the enabled capabilities.
	Surface *feeder.ReadOnlySurface
	// HTTP is the transport. Nil uses a client with a 30 s timeout.
	HTTP *http.Client
	// Now is the clock the rate-limit reset is measured from. Nil uses time.Now.
	Now func() time.Time
	// Budget, when set, paces every call (budget.go). Nil issues every call it is asked for.
	Budget *Budget
}

// Client issues the published operations.
type Client struct {
	base   string
	apiKey string
	appKey string
	surf   *feeder.ReadOnlySurface
	http   *http.Client
	now    func() time.Time
	budget *Budget

	mu       sync.Mutex
	readings map[string]feeder.Reading
}

// New returns a client, refusing an incomplete configuration.
func New(opts Options) (*Client, error) {
	switch {
	case opts.Surface == nil:
		return nil, errors.New("datadogx: a client with no published surface would issue anything")
	case strings.TrimSpace(opts.APIKey) == "" || strings.TrimSpace(opts.AppKey) == "":
		return nil, errors.New("datadogx: both the API key and the application key are required")
	case opts.BaseURL == "" && strings.TrimSpace(opts.Site) == "":
		return nil, errors.New("datadogx: no site; the API host is derived from it, e.g. datadoghq.eu")
	}
	base := strings.TrimRight(opts.BaseURL, "/")
	if base == "" {
		base = "https://api." + strings.TrimSpace(opts.Site)
	}
	client := opts.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Client{
		base: base, apiKey: opts.APIKey, appKey: opts.AppKey, surf: opts.Surface, http: client, now: now,
		readings: map[string]feeder.Reading{}, budget: opts.Budget,
	}, nil
}

// Response is one answer: the body, the rate-limit reading when Datadog sent one, and the request id.
type Response struct {
	Status    int
	Body      []byte
	Reading   feeder.Reading
	HasQuota  bool
	RequestID string
	At        time.Time
}

// StatusError is a non-2xx answer, typed so a caller can map it to a published failure reason.
type StatusError struct {
	Op         feeder.ReadOperation
	Status     int
	RetryAfter time.Duration
	Message    string
}

// HTTPStatus is the status Datadog answered.
func (e *StatusError) HTTPStatus() int { return e.Status }

// RetryAfterDuration is how long Datadog said to wait before retrying, zero when it said nothing.
func (e *StatusError) RetryAfterDuration() time.Duration { return e.RetryAfter }

func (e *StatusError) Error() string {
	return fmt.Sprintf("datadogx: %s answered %d: %s", e.Op, e.Status, e.Message)
}

// Do issues one published operation. path is the concrete path (the template with its parameters
// filled); query is the URL query; body, for a named query, is marshalled as JSON.
func (c *Client) Do(ctx context.Context, op feeder.ReadOperation, path string, query map[string]string, body any) (*Response, error) {
	if _, err := c.surf.Issuable(op); err != nil {
		return nil, err
	}
	if c.budget != nil {
		if err := c.budget.allow(ddfeeder.AreaOf(ctx), op, c.now()); err != nil {
			return nil, err
		}
	}
	method, _, _ := strings.Cut(string(op), " ")
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return nil, err
	}
	if len(query) > 0 {
		q := req.URL.Query()
		for k, v := range query {
			q.Set(k, v)
		}
		req.URL.RawQuery = q.Encode()
	}
	req.Header.Set("DD-API-KEY", c.apiKey)
	req.Header.Set("DD-APPLICATION-KEY", c.appKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if c.budget != nil {
			c.budget.sent(ddfeeder.AreaOf(ctx), op, nil)
		}
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	at := c.now().UTC()
	out := &Response{Status: resp.StatusCode, Body: raw, At: at, RequestID: resp.Header.Get("X-Datadog-Request-Id")}
	if reading, ok := feeder.DatadogReadingFromHeaders(resp.Header, at); ok {
		out.Reading, out.HasQuota = reading, true
		c.mu.Lock()
		c.readings[reading.Family] = reading
		c.mu.Unlock()
		if c.budget != nil {
			c.budget.observe(op, reading)
		}
	}
	if c.budget != nil {
		var charged *feeder.Reading
		if out.HasQuota {
			charged = &out.Reading
		}
		c.budget.sent(ddfeeder.AreaOf(ctx), op, charged)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		se := &StatusError{Op: op, Status: resp.StatusCode, Message: errorMessage(raw)}
		if out.HasQuota && !out.Reading.Reset.IsZero() {
			se.RetryAfter = out.Reading.Reset.Sub(at)
		}
		return out, se
	}
	return out, nil
}

// Readings returns the latest rate-limit reading per bucket, for the budget and the usage report.
func (c *Client) Readings() map[string]feeder.Reading {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]feeder.Reading, len(c.readings))
	for k, v := range c.readings {
		out[k] = v
	}
	return out
}

// errorMessage extracts Datadog's `errors` array, without echoing a body that might carry data.
func errorMessage(raw []byte) string {
	var body struct {
		Errors []string `json:"errors"`
	}
	if json.Unmarshal(raw, &body) == nil && len(body.Errors) > 0 {
		return strings.Join(body.Errors, "; ")
	}
	return "no error detail in the response"
}

// DefaultSurface is the surface of the default capabilities, re-exported so a caller needs one import.
func DefaultSurface() *feeder.ReadOnlySurface { return ddfeeder.DefaultSurface }

// DoRaw issues a body-less published operation and returns its status and body; a non-2xx status is an
// answer here rather than an error. It is how the feeder's startup gate reads, without importing this
// package.
func (c *Client) DoRaw(ctx context.Context, op, path string) (int, []byte, error) {
	resp, err := c.Do(ctx, feeder.ReadOperation(op), path, nil, nil)
	var status *StatusError
	if errors.As(err, &status) {
		return resp.Status, resp.Body, nil
	}
	if err != nil {
		return 0, nil, err
	}
	return resp.Status, resp.Body, nil
}

// ListMonitorsPage issues one page of the monitor list and returns the raw body, which is the payload a
// feeder replays.
func (c *Client) ListMonitorsPage(ctx context.Context, tags string, page, pageSize int) ([]byte, error) {
	_, resp, err := c.ListMonitors(ctx, tags, page, pageSize)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// ListEventsPage issues one page of the event stream over q's window and returns the raw body, which
// the poller filters to the configured scope and a feeder replays. The parameters are Datadog's
// published ones; the operation is a GET and has no body.
func (c *Client) ListEventsPage(ctx context.Context, q ddfeeder.EventsQuery) ([]byte, error) {
	resp, err := c.ListEvents(ctx, q)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// Usage is the usage report of the client's budget, empty without one. The feeder and the backend each
// build their own client and budget, so each reports its own calls (FR-084a).
func (c *Client) Usage() string {
	if c.budget == nil {
		return ""
	}
	return c.budget.Report()
}
