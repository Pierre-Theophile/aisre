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

// The `apm_topology` capability's operations (section B; T090): the span aggregate, the metrics query
// and the service dependencies. Like the events read, each is built from the published API shape and not
// yet verified against a live organisation: the structs read only the fields the connector uses, a field
// Datadog adds is ignored, and one it removes fails the test that depends on it rather than producing
// less. Every call is declared only under the capability (requestlog.go), so with it off none can leave.

// SpanFilter selects spans.
type SpanFilter struct {
	Query string `json:"query"`
	From  string `json:"from"`
	To    string `json:"to"`
}

// NewSpanFilter builds a filter over [from, to), in RFC 3339 like the log filter.
func NewSpanFilter(query string, from, to time.Time) SpanFilter {
	return SpanFilter{Query: query, From: from.UTC().Format(time.RFC3339Nano), To: to.UTC().Format(time.RFC3339Nano)}
}

// SpanCompute is one aggregation: `count`, or a percentile (`pc50` ... `pc99`) of a numeric metric such
// as `@duration`, which spans state in nanoseconds.
type SpanCompute struct {
	Aggregation string `json:"aggregation"`
	Metric      string `json:"metric,omitempty"`
	Type        string `json:"type,omitempty"`
}

// SpanGroupBy groups by one facet.
type SpanGroupBy struct {
	Facet   string `json:"facet"`
	Limit   int    `json:"limit,omitempty"`
	Missing string `json:"missing,omitempty"`
}

// SpanAggregateRequest is what the aggregate is asked. The wire body wraps it in Datadog's
// `data.attributes` envelope.
type SpanAggregateRequest struct {
	Compute []SpanCompute
	Filter  SpanFilter
	GroupBy []SpanGroupBy
}

type spanAggregateAttributes struct {
	Compute []SpanCompute `json:"compute"`
	Filter  SpanFilter    `json:"filter"`
	GroupBy []SpanGroupBy `json:"group_by,omitempty"`
}

type spanAggregateData struct {
	Attributes spanAggregateAttributes `json:"attributes"`
	Type       string                  `json:"type"`
}

// MarshalJSON writes the published envelope.
func (r SpanAggregateRequest) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Data spanAggregateData `json:"data"`
	}{Data: spanAggregateData{
		Attributes: spanAggregateAttributes(r),
		Type:       "aggregate_request",
	}})
}

// SpanBucket is one group of the span aggregate.
type SpanBucket struct {
	Attributes struct {
		By      map[string]any             `json:"by"`
		Compute map[string]json.RawMessage `json:"compute"`
	} `json:"attributes"`
}

// Key reads one group value as a string.
func (b SpanBucket) Key(facet string) string {
	switch v := b.Attributes.By[facet].(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

// Number reads compute `c<index>` as a number.
func (b SpanBucket) Number(index int) (float64, bool) {
	raw, ok := b.Attributes.Compute["c"+strconv.Itoa(index)]
	if !ok {
		return 0, false
	}
	var f float64
	if json.Unmarshal(raw, &f) != nil {
		return 0, false
	}
	return f, true
}

// SpanAggregateResponse is the aggregate answer.
type SpanAggregateResponse struct {
	Data []SpanBucket `json:"data"`
	Meta Meta         `json:"meta"`
}

// AggregateSpans issues the span aggregate.
func (c *Client) AggregateSpans(ctx context.Context, req SpanAggregateRequest) (*SpanAggregateResponse, *Response, error) {
	resp, err := c.Do(ctx, ddfeeder.OpAggregateSpans, "/api/v2/spans/analytics/aggregate", nil, req)
	if err != nil {
		return nil, resp, err
	}
	var out SpanAggregateResponse
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return nil, resp, fmt.Errorf("datadogx: the span aggregate answer no longer parses: %w", err)
	}
	return &out, resp, nil
}

// MetricSeries is one series of a metrics query.
type MetricSeries struct {
	Metric string `json:"metric"`
	Scope  string `json:"scope"`
	// Pointlist is [[epoch milliseconds, value or null], ...].
	Pointlist [][]*float64 `json:"pointlist"`
}

// Values are the series' non-null values, in time order.
func (s MetricSeries) Values() []float64 {
	var out []float64
	for _, p := range s.Pointlist {
		if len(p) == 2 && p[1] != nil {
			out = append(out, *p[1])
		}
	}
	return out
}

// MetricsResponse is the answer to a metrics query.
type MetricsResponse struct {
	Status string         `json:"status"`
	Series []MetricSeries `json:"series"`
}

// QueryMetrics issues one metrics query over [from, to).
func (c *Client) QueryMetrics(ctx context.Context, query string, from, to time.Time) (*MetricsResponse, *Response, error) {
	resp, err := c.Do(ctx, ddfeeder.OpQueryMetrics, "/api/v1/query", map[string]string{
		"query": query, "from": strconv.FormatInt(from.UTC().Unix(), 10), "to": strconv.FormatInt(to.UTC().Unix(), 10),
	}, nil)
	if err != nil {
		return nil, resp, err
	}
	var out MetricsResponse
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return nil, resp, fmt.Errorf("datadogx: the metrics answer no longer parses: %w", err)
	}
	if out.Status != "" && out.Status != "ok" {
		return nil, resp, fmt.Errorf("datadogx: the metrics query answered status %q", out.Status)
	}
	return &out, resp, nil
}

// ServiceDependencies reads the services of an environment and the services each calls, as
// service → callees. Datadog answers an object keyed by service name.
func (c *Client) ServiceDependencies(ctx context.Context, env string, from, to time.Time) (map[string][]string, *Response, error) {
	resp, err := c.Do(ctx, ddfeeder.OpServiceDependencies, "/api/v1/service_dependencies", map[string]string{
		"env": env, "start": strconv.FormatInt(from.UTC().Unix(), 10), "end": strconv.FormatInt(to.UTC().Unix(), 10),
	}, nil)
	if err != nil {
		return nil, resp, err
	}
	var raw map[string]struct {
		Calls []string `json:"calls"`
	}
	if err := json.Unmarshal(resp.Body, &raw); err != nil {
		return nil, resp, fmt.Errorf("datadogx: the service dependencies answer no longer parses: %w", err)
	}
	out := make(map[string][]string, len(raw))
	for service, dep := range raw {
		out[service] = dep.Calls
	}
	return out, resp, nil
}
