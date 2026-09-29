// SPDX-License-Identifier: Apache-2.0

package datadog_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// twin is a structural twin of the Datadog read surface this backend uses: log search, log
// aggregation and the monitor read, answered from a fixed set of lines and one monitor. It keeps the
// request log, which is how "no span or APM call was made" is asserted rather than assumed (SC-023).
type twin struct {
	t       *testing.T
	lines   []twinLine // oldest first
	monitor string     // the monitor read's JSON body
	// ungroupable, when set, is an attribute the aggregate refuses to group by (a 400, as Datadog
	// does for an attribute it cannot group). Each line then also carries its version in a tag of that name.
	ungroupable string
	// spans are the APM half's data (apm_twin_test.go); with none, the span and metrics operations answer
	// empty, which a test that never enables apm_topology asserts is never asked.
	spans []twinSpan

	mu    sync.Mutex
	paths []string
}

func (tw *twin) requests() []string {
	tw.mu.Lock()
	defer tw.mu.Unlock()
	return append([]string(nil), tw.paths...)
}

type twinFilter struct {
	Query, From, To string
}

func (tw *twin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	tw.mu.Lock()
	tw.paths = append(tw.paths, r.Method+" "+r.URL.Path)
	tw.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case strings.HasPrefix(r.URL.Path, "/api/v1/monitor/"):
		_, _ = io.WriteString(w, tw.monitor)
	case r.URL.Path == "/api/v2/logs/events/search":
		tw.search(w, r)
	case r.URL.Path == "/api/v2/logs/analytics/aggregate":
		tw.aggregate(w, r)
	case r.URL.Path == "/api/v2/spans/analytics/aggregate":
		tw.spanAggregate(w, r)
	case r.URL.Path == "/api/v1/query":
		tw.metricsQuery(w, r)
	default:
		tw.t.Errorf("the twin was asked for %s %s, which this backend never declared", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

// match applies the filter: the window, and the query's exact `key:value` clauses. service and env
// are the twin's own, so they always match.
func (tw *twin) match(f twinFilter) []twinLine {
	from, _ := time.Parse(time.RFC3339Nano, f.From)
	to, _ := time.Parse(time.RFC3339Nano, f.To)
	var out []twinLine
	for _, l := range tw.lines {
		if l.at.Before(from) || !l.at.Before(to) || !clausesMatch(f.Query, l) {
			continue
		}
		out = append(out, l)
	}
	return out
}

func clausesMatch(query string, l twinLine) bool {
	for _, token := range splitQuery(query) {
		key, value, _ := strings.Cut(token, ":")
		value = strings.Trim(value, `"`)
		switch key {
		case "status":
			if l.status != value {
				return false
			}
		case "version", ungroupableFacet:
			if l.version != value {
				return false
			}
		}
	}
	return true
}

// splitQuery splits on spaces outside quotes.
func splitQuery(q string) []string {
	var out []string
	var cur strings.Builder
	quoted := false
	for _, r := range q {
		switch {
		case r == '"':
			quoted = !quoted
			cur.WriteRune(r)
		case r == ' ' && !quoted:
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// ungroupableFacet is the attribute a twin can be told to refuse: the version, stamped a second way.
const ungroupableFacet = "build.version"

func (tw *twin) search(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Filter twinFilter `json:"filter"`
		Page   struct {
			Limit  int    `json:"limit"`
			Cursor string `json:"cursor"`
		} `json:"page"`
	}
	tw.decode(r, &req)
	lines := tw.match(req.Filter)
	// Newest first, as the backend asks.
	sort.SliceStable(lines, func(i, j int) bool { return lines[i].at.After(lines[j].at) })
	offset, _ := strconv.Atoi(req.Page.Cursor)
	end := min(offset+req.Page.Limit, len(lines))
	data := make([]map[string]any, 0, end-offset)
	for i := offset; i < end; i++ {
		l := lines[i]
		tags := []string{"env:production", "service:checkout"}
		if l.version != "" {
			tags = append(tags, "version:"+l.version)
			if tw.ungroupable != "" {
				tags = append(tags, tw.ungroupable+":"+l.version)
			}
		}
		data = append(data, map[string]any{"id": fmt.Sprintf("ev-%s-%d", l.at.Format("150405"), i),
			"attributes": map[string]any{
				"timestamp": l.at.Format(time.RFC3339Nano), "message": l.msg, "status": l.status,
				"host": l.host, "service": "checkout", "tags": tags, "attributes": map[string]any{},
			}})
	}
	meta := map[string]any{"status": "done"}
	if end < len(lines) {
		meta["page"] = map[string]any{"after": strconv.Itoa(end)}
	}
	tw.encode(w, map[string]any{"data": data, "meta": meta})
}

func (tw *twin) aggregate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Compute []struct {
			Type     string `json:"type"`
			Interval string `json:"interval"`
		} `json:"compute"`
		Filter  twinFilter `json:"filter"`
		GroupBy []struct {
			Facet   string `json:"facet"`
			Missing string `json:"missing"`
		} `json:"group_by"`
	}
	tw.decode(r, &req)
	if len(req.GroupBy) > 0 && req.GroupBy[0].Facet == tw.ungroupable && tw.ungroupable != "" {
		w.WriteHeader(http.StatusBadRequest)
		tw.encode(w, map[string]any{"errors": []string{"cannot group by " + tw.ungroupable}})
		return
	}
	lines := tw.match(req.Filter)

	groups := map[string][]twinLine{}
	var facet string
	if len(req.GroupBy) > 0 {
		facet = req.GroupBy[0].Facet
		for _, l := range lines {
			key := map[string]string{"status": l.status, "version": l.version}[facet]
			if key == "" {
				key = req.GroupBy[0].Missing
			}
			groups[key] = append(groups[key], l)
		}
	} else {
		groups[""] = lines
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	buckets := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		by := map[string]any{}
		if facet != "" {
			by[facet] = k
		}
		var compute any = len(groups[k])
		if len(req.Compute) > 0 && req.Compute[0].Type == "timeseries" {
			perMinute := map[time.Time]int{}
			for _, l := range groups[k] {
				perMinute[l.at.Truncate(time.Minute)]++
			}
			minutes := make([]time.Time, 0, len(perMinute))
			for m := range perMinute {
				minutes = append(minutes, m)
			}
			sort.Slice(minutes, func(i, j int) bool { return minutes[i].Before(minutes[j]) })
			points := make([]map[string]any, 0, len(minutes))
			for _, m := range minutes {
				points = append(points, map[string]any{"time": m.Format(time.RFC3339), "value": perMinute[m]})
			}
			compute = points
		}
		buckets = append(buckets, map[string]any{"by": by, "computes": map[string]any{"c0": compute}})
	}
	tw.encode(w, map[string]any{"data": map[string]any{"buckets": buckets}, "meta": map[string]any{"status": "done"}})
}

func (tw *twin) decode(r *http.Request, v any) {
	body, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(body, v); err != nil {
		tw.t.Errorf("the twin could not read %s: %v", r.URL.Path, err)
	}
}

func (tw *twin) encode(w http.ResponseWriter, v any) {
	if err := json.NewEncoder(w).Encode(v); err != nil {
		tw.t.Errorf("the twin could not answer: %v", err)
	}
}
