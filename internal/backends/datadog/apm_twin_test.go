// SPDX-License-Identifier: Apache-2.0

package datadog_test

import (
	"fmt"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The APM half of the structural twin (T090): the span aggregate and the metrics query, answered from a
// fixed set of spans. Built from the published response shapes, like the code that reads them, and not
// verified against a live organisation.
//
// The twin keeps the distinction the backend states in its coverage: the trace metrics count EVERY span,
// and the span aggregate counts only the RETAINED ones, so an answer that mixed the two up would read the
// wrong number here.

// twinSpan is one client span the source service emitted towards a destination.
type twinSpan struct {
	at       time.Time
	service  string
	peer     string
	resource string
	status   string // "ok" or "error"
	errType  string
	version  string
	duration time.Duration
	// retained is whether Datadog kept the span; the trace metrics count it either way.
	retained bool
}

// spanQuery is a parsed span-search query: exact `key:value` clauses.
func spanClauses(query string) map[string]string {
	out := map[string]string{}
	for _, token := range splitQuery(query) {
		key, value, _ := strings.Cut(token, ":")
		out[key] = strings.Trim(value, `"`)
	}
	return out
}

func (tw *twin) spansIn(from, to time.Time, service, peer string, retainedOnly bool) []twinSpan {
	var out []twinSpan
	for _, s := range tw.spans {
		if s.at.Before(from) || !s.at.Before(to) || (service != "" && s.service != service) ||
			(peer != "" && s.peer != peer) || (retainedOnly && !s.retained) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// percentile is the nearest-rank percentile of the durations, in the unit given.
func percentile(durations []time.Duration, p float64) time.Duration {
	if len(durations) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), durations...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	rank := int(math.Ceil(p / 100 * float64(len(sorted))))
	return sorted[max(rank, 1)-1]
}

func (tw *twin) spanAggregate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Data struct {
			Type       string `json:"type"`
			Attributes struct {
				Compute []struct {
					Aggregation string `json:"aggregation"`
					Metric      string `json:"metric"`
				} `json:"compute"`
				Filter  twinFilter `json:"filter"`
				GroupBy []struct {
					Facet   string `json:"facet"`
					Missing string `json:"missing"`
				} `json:"group_by"`
			} `json:"attributes"`
		} `json:"data"`
	}
	tw.decode(r, &req)
	if req.Data.Type != "aggregate_request" {
		w.WriteHeader(http.StatusBadRequest)
		tw.encode(w, map[string]any{"errors": []string{"the body is not an aggregate_request"}})
		return
	}
	attrs := req.Data.Attributes
	from, _ := time.Parse(time.RFC3339Nano, attrs.Filter.From)
	to, _ := time.Parse(time.RFC3339Nano, attrs.Filter.To)
	clauses := spanClauses(attrs.Filter.Query)
	spans := tw.spansIn(from, to, clauses["service"], clauses["@peer.service"], true)

	facetOf := func(s twinSpan, facet, missing string) string {
		var v string
		switch facet {
		case "resource_name":
			v = s.resource
		case "status":
			v = s.status
		case "@error.type":
			v = s.errType
		}
		if v == "" {
			return missing
		}
		return v
	}
	groups := map[string][]twinSpan{}
	keyOf := func(s twinSpan) string {
		parts := make([]string, len(attrs.GroupBy))
		for i, g := range attrs.GroupBy {
			parts[i] = facetOf(s, g.Facet, g.Missing)
		}
		return strings.Join(parts, "\x00")
	}
	for _, s := range spans {
		groups[keyOf(s)] = append(groups[keyOf(s)], s)
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	data := make([]map[string]any, 0, len(keys))
	for i, k := range keys {
		by := map[string]any{}
		for j, part := range strings.Split(k, "\x00") {
			by[attrs.GroupBy[j].Facet] = part
		}
		durations := make([]time.Duration, 0, len(groups[k]))
		for _, s := range groups[k] {
			durations = append(durations, s.duration)
		}
		compute := map[string]any{}
		for j, c := range attrs.Compute {
			switch c.Aggregation {
			case "count":
				compute["c"+strconv.Itoa(j)] = len(groups[k])
			case "pc50", "pc95", "pc99":
				p, _ := strconv.ParseFloat(strings.TrimPrefix(c.Aggregation, "pc"), 64)
				compute["c"+strconv.Itoa(j)] = float64(percentile(durations, p).Nanoseconds())
			}
		}
		data = append(data, map[string]any{"id": fmt.Sprintf("bucket-%d", i), "type": "bucket",
			"attributes": map[string]any{"by": by, "compute": compute}})
	}
	tw.encode(w, map[string]any{"data": data, "meta": map[string]any{"status": "done", "request_id": "twin"}})
}

var metricQuery = regexp.MustCompile(`^(avg|sum):(trace\.[A-Za-z0-9_.]+)\{service:([^,}]+),env:([^}]+)\}( by \{version\})?(\.as_count\(\))?$`)

// metricValue is one minute's value of a trace metric for the spans of one version.
func metricValue(metric string, spans []twinSpan) (float64, bool) {
	if len(spans) == 0 {
		return 0, false
	}
	var errs int
	durations := make([]time.Duration, 0, len(spans))
	for _, s := range spans {
		if s.status == "error" {
			errs++
		}
		durations = append(durations, s.duration)
	}
	switch {
	case strings.HasSuffix(metric, ".hits"):
		return float64(len(spans)), true
	case strings.HasSuffix(metric, ".errors"):
		return float64(errs), true
	case strings.HasSuffix(metric, ".duration.by.service.50p"):
		return percentile(durations, 50).Seconds(), true
	case strings.HasSuffix(metric, ".duration.by.service.95p"):
		return percentile(durations, 95).Seconds(), true
	case strings.HasSuffix(metric, ".duration.by.service.99p"):
		return percentile(durations, 99).Seconds(), true
	}
	return 0, false
}

func (tw *twin) metricsQuery(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	m := metricQuery.FindStringSubmatch(q.Get("query"))
	if m == nil {
		w.WriteHeader(http.StatusBadRequest)
		tw.encode(w, map[string]any{"errors": []string{"the twin does not read the query " + q.Get("query")}})
		return
	}
	fromSec, _ := strconv.ParseInt(q.Get("from"), 10, 64)
	toSec, _ := strconv.ParseInt(q.Get("to"), 10, 64)
	from, to := time.Unix(fromSec, 0).UTC(), time.Unix(toSec, 0).UTC()
	metric, service, env, byVersion := m[2], m[3], m[4], m[5] != ""
	// The trace metrics carry every span, retained or not.
	spans := tw.spansIn(from, to, service, "", false)

	byKey := map[string][]twinSpan{}
	for _, s := range spans {
		key := ""
		if byVersion {
			key = s.version
		}
		byKey[key] = append(byKey[key], s)
	}
	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	series := make([]map[string]any, 0, len(keys))
	for _, key := range keys {
		perMinute := map[time.Time][]twinSpan{}
		for _, s := range byKey[key] {
			perMinute[s.at.Truncate(time.Minute)] = append(perMinute[s.at.Truncate(time.Minute)], s)
		}
		minutes := make([]time.Time, 0, len(perMinute))
		for minute := range perMinute {
			minutes = append(minutes, minute)
		}
		sort.Slice(minutes, func(i, j int) bool { return minutes[i].Before(minutes[j]) })
		points := make([][]any, 0, len(minutes))
		for _, minute := range minutes {
			if v, ok := metricValue(metric, perMinute[minute]); ok {
				points = append(points, []any{minute.UnixMilli(), v})
			}
		}
		scope := "env:" + env + ",service:" + service
		if byVersion && key != "" {
			scope += ",version:" + key
		}
		series = append(series, map[string]any{"metric": metric, "scope": scope, "pointlist": points})
	}
	tw.encode(w, map[string]any{"status": "ok", "series": series, "query": q.Get("query")})
}
