// SPDX-License-Identifier: Apache-2.0

package datadogx

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	ddfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/datadog"
)

// The request and response shapes of the published operations, restricted to the fields the connector
// reads (research §2.1). A field Datadog adds is ignored, and a field it removes fails the fixture that
// depends on it, rather than silently producing less.

// LogFilter selects log events.
type LogFilter struct {
	Query   string   `json:"query"`
	From    string   `json:"from"`
	To      string   `json:"to"`
	Indexes []string `json:"indexes,omitempty"`
}

// NewLogFilter builds a filter over [from, to), in RFC 3339 — the form Datadog accepts and a recording
// can compare byte for byte.
func NewLogFilter(query string, from, to time.Time, indexes []string) LogFilter {
	return LogFilter{Query: query, From: from.UTC().Format(time.RFC3339Nano), To: to.UTC().Format(time.RFC3339Nano), Indexes: indexes}
}

// SearchRequest is the body of the search operation.
type SearchRequest struct {
	Filter LogFilter  `json:"filter"`
	Sort   string     `json:"sort,omitempty"`
	Page   SearchPage `json:"page"`
}

// SearchPage pages a search. Limit is at most 1000.
type SearchPage struct {
	Limit  int    `json:"limit"`
	Cursor string `json:"cursor,omitempty"`
}

// LogEvent is one log event as the search returns it.
type LogEvent struct {
	ID         string `json:"id"`
	Attributes struct {
		Timestamp  time.Time      `json:"timestamp"`
		Service    string         `json:"service"`
		Status     string         `json:"status"`
		Host       string         `json:"host"`
		Message    string         `json:"message"`
		Tags       []string       `json:"tags"`
		Attributes map[string]any `json:"attributes"`
	} `json:"attributes"`
}

// SearchResponse is one page.
type SearchResponse struct {
	Data []LogEvent `json:"data"`
	Meta Meta       `json:"meta"`
}

// Meta is the part of a response that says whether the answer is complete.
type Meta struct {
	Page struct {
		After string `json:"after"`
	} `json:"page"`
	// Status is "done" or "timeout"; a timeout is a partial answer.
	Status    string `json:"status"`
	RequestID string `json:"request_id"`
	// RawWarnings are kept whole: any warning means the answer may be partial.
	RawWarnings []json.RawMessage `json:"warnings"`
}

// Partial reports whether Datadog said the answer is incomplete.
func (m Meta) Partial() bool { return m.Status == "timeout" || len(m.RawWarnings) > 0 }

// SearchLogs issues one page of the search.
func (c *Client) SearchLogs(ctx context.Context, req SearchRequest) (*SearchResponse, *Response, error) {
	if req.Page.Limit <= 0 || req.Page.Limit > 1000 {
		return nil, nil, fmt.Errorf("datadogx: a search page limit must be 1–1000, not %d", req.Page.Limit)
	}
	resp, err := c.Do(ctx, ddfeeder.OpSearchLogs, "/api/v2/logs/events/search", nil, req)
	if err != nil {
		return nil, resp, err
	}
	var out SearchResponse
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return nil, resp, fmt.Errorf("datadogx: the search answer no longer parses: %w", err)
	}
	return &out, resp, nil
}

// Compute is one aggregation.
type Compute struct {
	Aggregation string `json:"aggregation"`
	Type        string `json:"type,omitempty"`     // "total" or "timeseries"
	Interval    string `json:"interval,omitempty"` // e.g. "1m", for a timeseries
}

// GroupBy groups by one facet. Missing names the bucket for events without it, so they form a named
// group rather than vanishing.
type GroupBy struct {
	Facet   string `json:"facet"`
	Limit   int    `json:"limit,omitempty"`
	Missing string `json:"missing,omitempty"`
}

// AggregateRequest is the body of the aggregate operation.
type AggregateRequest struct {
	Compute []Compute `json:"compute"`
	Filter  LogFilter `json:"filter"`
	GroupBy []GroupBy `json:"group_by,omitempty"`
}

// Bucket is one group.
type Bucket struct {
	By       map[string]any             `json:"by"`
	Computes map[string]json.RawMessage `json:"computes"`
}

// AggregateResponse is the aggregate answer.
type AggregateResponse struct {
	Data struct {
		Buckets []Bucket `json:"buckets"`
	} `json:"data"`
	Meta Meta `json:"meta"`
}

// Count reads a total compute, `c<index>`, as an integer count.
func (b Bucket) Count(index int) (int64, bool) {
	raw, ok := b.Computes["c"+strconv.Itoa(index)]
	if !ok {
		return 0, false
	}
	var f float64
	if json.Unmarshal(raw, &f) != nil {
		return 0, false
	}
	return int64(f), true
}

// Point is one timeseries point.
type Point struct {
	Time  time.Time `json:"time"`
	Value float64   `json:"value"`
}

// Series reads a timeseries compute.
func (b Bucket) Series(index int) ([]Point, bool) {
	raw, ok := b.Computes["c"+strconv.Itoa(index)]
	if !ok {
		return nil, false
	}
	var points []Point
	if json.Unmarshal(raw, &points) != nil {
		return nil, false
	}
	return points, true
}

// Key reads one group value as a string.
func (b Bucket) Key(facet string) string {
	switch v := b.By[facet].(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

// AggregateLogs issues the aggregate operation.
func (c *Client) AggregateLogs(ctx context.Context, req AggregateRequest) (*AggregateResponse, *Response, error) {
	resp, err := c.Do(ctx, ddfeeder.OpAggregateLogs, "/api/v2/logs/analytics/aggregate", nil, req)
	if err != nil {
		return nil, resp, err
	}
	var out AggregateResponse
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return nil, resp, fmt.Errorf("datadogx: the aggregate answer no longer parses: %w", err)
	}
	return &out, resp, nil
}

// GroupState is one monitor group's state (research §2.1).
type GroupState struct {
	Status          string `json:"status"`
	LastTriggeredTS int64  `json:"last_triggered_ts"`
	LastResolvedTS  int64  `json:"last_resolved_ts"`
	LastNoDataTS    int64  `json:"last_nodata_ts"`
}

// Monitor is a monitor definition with its group states.
type Monitor struct {
	ID           int64    `json:"id"`
	Name         string   `json:"name"`
	Type         string   `json:"type"`
	Query        string   `json:"query"`
	Tags         []string `json:"tags"`
	Priority     *int     `json:"priority"`
	OverallState string   `json:"overall_state"`
	State        struct {
		Groups map[string]GroupState `json:"groups"`
	} `json:"state"`
}

// GetMonitor reads one monitor with every group state.
func (c *Client) GetMonitor(ctx context.Context, id int64) (*Monitor, *Response, error) {
	resp, err := c.Do(ctx, ddfeeder.OpGetMonitor, fmt.Sprintf("/api/v1/monitor/%d", id),
		map[string]string{"group_states": "all"}, nil)
	if err != nil {
		return nil, resp, err
	}
	var out Monitor
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return nil, resp, fmt.Errorf("datadogx: the monitor answer no longer parses: %w", err)
	}
	return &out, resp, nil
}

// ListMonitors reads one page of monitors, filtered by tags.
func (c *Client) ListMonitors(ctx context.Context, tags string, page, pageSize int) ([]Monitor, *Response, error) {
	query := map[string]string{
		"group_states": "all", "page": strconv.Itoa(page), "page_size": strconv.Itoa(pageSize),
	}
	if tags != "" {
		query["monitor_tags"] = tags
	}
	resp, err := c.Do(ctx, ddfeeder.OpListMonitors, "/api/v1/monitor", query, nil)
	if err != nil {
		return nil, resp, err
	}
	var out []Monitor
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return nil, resp, fmt.Errorf("datadogx: the monitor list no longer parses: %w", err)
	}
	return out, resp, nil
}
