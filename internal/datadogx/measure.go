// SPDX-License-Identifier: Apache-2.0

package datadogx

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	ddfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/datadog"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/versionstamp"
)

// Measurer is the discovery reads (005 T064; version-stamping.md §3): for one log source over one
// window, how many lines and error lines there are, how many carry a host, and how many carry each
// version-stamp candidate. Each is one aggregate grouped by status, so a source costs two calls plus one
// per candidate — bounded, and drawn at the discovery interval, never inside an investigation.
//
// It reads counts of PRESENCE only: `field:*`, never a field's values and never a line.
type Measurer struct {
	Client  *Client
	Indexes []string
	// Cache, when set, keeps the answers of the current discovery window, so a discovery the budget
	// stopped resumes where it stopped instead of spending again what it already read (005: a bucket
	// of 2 calls per window cannot afford a measurement that starts over).
	Cache *MeasureCache
}

// MeasureCache holds one discovery window's answers, keyed by request. A request for another window
// empties it: answers are never reused across windows.
type MeasureCache struct {
	mu     sync.Mutex
	window string
	agg    map[string]*AggregateResponse
	search map[string]*SearchResponse
}

// NewMeasureCache returns an empty cache.
func NewMeasureCache() *MeasureCache { return &MeasureCache{} }

func (c *MeasureCache) key(window string, req any) string {
	raw, _ := json.Marshal(req)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.window != window {
		c.window, c.agg, c.search = window, map[string]*AggregateResponse{}, map[string]*SearchResponse{}
	}
	return string(raw)
}

func windowKey(f LogFilter) string { return f.From + "/" + f.To }

func (m Measurer) aggregate(ctx context.Context, req AggregateRequest) (*AggregateResponse, error) {
	if m.Cache == nil {
		out, _, err := m.Client.AggregateLogs(ctx, req)
		return out, err
	}
	key := m.Cache.key(windowKey(req.Filter), req)
	m.Cache.mu.Lock()
	hit, ok := m.Cache.agg[key]
	m.Cache.mu.Unlock()
	if ok {
		return hit, nil
	}
	out, _, err := m.Client.AggregateLogs(ctx, req)
	if err != nil {
		return nil, err
	}
	m.Cache.mu.Lock()
	m.Cache.agg[key] = out
	m.Cache.mu.Unlock()
	return out, nil
}

func (m Measurer) search(ctx context.Context, window string, req SearchRequest) (*SearchResponse, error) {
	if m.Cache == nil {
		out, _, err := m.Client.SearchLogs(ctx, req)
		return out, err
	}
	key := m.Cache.key(window, req)
	m.Cache.mu.Lock()
	hit, ok := m.Cache.search[key]
	m.Cache.mu.Unlock()
	if ok {
		return hit, nil
	}
	out, _, err := m.Client.SearchLogs(ctx, req)
	if err != nil {
		return nil, err
	}
	m.Cache.mu.Lock()
	m.Cache.search[key] = out
	m.Cache.mu.Unlock()
	return out, nil
}

// Measure measures one source.
func (m Measurer) Measure(ctx context.Context, src ddfeeder.LogSource, override string, from, to time.Time) (ddfeeder.SourceMeasurement, error) {
	base := src.Query()
	indexes := m.Indexes
	if src.Index != "" {
		indexes = []string{src.Index}
	}
	count := func(extra string) (lines, errorLines int64, err error) {
		query := base
		if extra != "" {
			query += " " + extra
		}
		out, err := m.aggregate(ctx, AggregateRequest{
			Compute: []Compute{{Aggregation: "count", Type: "total"}},
			Filter:  NewLogFilter(query, from, to, indexes),
			GroupBy: []GroupBy{{Facet: "status", Limit: 20, Missing: "__no_status__"}},
		})
		if err != nil {
			return 0, 0, err
		}
		for _, b := range out.Data.Buckets {
			n, _ := b.Count(0)
			lines += n
			if b.Key("status") == "error" {
				errorLines += n
			}
		}
		return lines, errorLines, nil
	}
	var out ddfeeder.SourceMeasurement
	var err error
	if out.Lines, out.ErrorLines, err = count(""); err != nil {
		return out, err
	}
	if out.Lines == 0 && src.Env != "" {
		// Nothing carries this environment. One more count says whether the service logs without any
		// `env` tag, which is a configuration to fix rather than a silent service.
		n, _, err := m.countWithout(ctx, src, from, to, indexes)
		if err != nil {
			return out, err
		}
		out.LinesWithoutEnv = n
	}
	if out.HostLines, _, err = count("host:*"); err != nil {
		return out, err
	}
	candidates, err := ddfeeder.CandidatesFor(override)
	if err != nil {
		return out, err
	}
	for _, c := range candidates {
		lines, errorLines, err := count(presence(c))
		if err != nil {
			return out, err
		}
		out.Candidates = append(out.Candidates, ddfeeder.CandidateCount{Label: c.Label(), Lines: lines, ErrorLines: errorLines})
	}
	tags, err := m.tags(ctx, base, from, to, indexes)
	if err != nil {
		return out, err
	}
	out.Tags = tags
	return out, nil
}

// tags counts the allowlisted tag values on the source's lines: one aggregate per owner key and one
// for the Kubernetes pair (005 T076). Only allowlisted keys are ever asked for (FR-066).
func (m Measurer) tags(ctx context.Context, base string, from, to time.Time, indexes []string) ([]ddfeeder.TagCount, error) {
	var out []ddfeeder.TagCount
	for _, key := range ddfeeder.TagKeysMeasured() {
		facets := strings.Split(key, "/")
		group := make([]GroupBy, 0, len(facets))
		for _, f := range facets {
			group = append(group, GroupBy{Facet: f, Limit: 10})
		}
		res, err := m.aggregate(ctx, AggregateRequest{
			Compute: []Compute{{Aggregation: "count", Type: "total"}},
			Filter:  NewLogFilter(base, from, to, indexes),
			GroupBy: group,
		})
		if err != nil {
			return nil, err
		}
		for _, b := range res.Data.Buckets {
			values := make([]string, 0, len(facets))
			for _, f := range facets {
				values = append(values, b.Key(f))
			}
			value := strings.Join(values, "/")
			if strings.Contains("/"+value+"/", "//") || value == "" {
				continue // a line without the key (or half the pair) names nothing
			}
			n, _ := b.Count(0)
			out = append(out, ddfeeder.TagCount{Key: key, Value: value, Lines: n})
		}
	}
	return out, nil
}

// presence is the query clause "the candidate is on the line".
func presence(c versionstamp.Candidate) string {
	switch c.Form {
	case versionstamp.FormTag:
		return c.Name + ":*"
	case versionstamp.FormAttributePair:
		return "@" + c.Name + ":* @" + c.Pair + ":*"
	default:
		return "@" + c.Name + ":*"
	}
}

// DefaultFirstSeenHorizon is how far back a new value's first line is looked for (contract §4).
const DefaultFirstSeenHorizon = 7 * 24 * time.Hour

// Sightings lists the values of facet seen in [from, to) that are not in known, each with its first
// indexed line within the horizon (005 T069). A value with lines in the day before the horizon is
// marked BeyondHorizon: it was deployed before the connector could see it. Costs one aggregate, then two
// searches per new value.
func (m Measurer) Sightings(ctx context.Context, src ddfeeder.LogSource, facet string, from, to time.Time, horizon time.Duration, known map[string]bool) ([]ddfeeder.ValueSighting, error) {
	if horizon <= 0 {
		horizon = DefaultFirstSeenHorizon
	}
	base := src.Query()
	indexes := m.Indexes
	if src.Index != "" {
		indexes = []string{src.Index}
	}
	out, err := m.aggregate(ctx, AggregateRequest{
		Compute: []Compute{{Aggregation: "count", Type: "total"}},
		Filter:  NewLogFilter(base, from, to, indexes),
		GroupBy: []GroupBy{{Facet: facet, Limit: 50}},
	})
	if err != nil {
		return nil, err
	}
	first := func(clause string, a, b time.Time) (time.Time, bool, error) {
		// Keyed on the discovery window, not the search's own: the horizon searches look further back.
		page, err := m.search(ctx, windowKey(NewLogFilter(base, from, to, indexes)), SearchRequest{
			Filter: NewLogFilter(base+" "+clause, a, b, indexes), Sort: "timestamp", Page: SearchPage{Limit: 1},
		})
		if err != nil || len(page.Data) == 0 {
			return time.Time{}, false, err
		}
		return page.Data[0].Attributes.Timestamp.UTC(), true, nil
	}
	var sightings []ddfeeder.ValueSighting
	for _, b := range out.Data.Buckets {
		value := b.Key(facet)
		if value == "" || known[value] {
			continue
		}
		clause := facetClause(facet, value)
		seen, ok, err := first(clause, to.Add(-horizon), to)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		_, older, err := first(clause, to.Add(-horizon-24*time.Hour), to.Add(-horizon))
		if err != nil {
			return nil, err
		}
		sightings = append(sightings, ddfeeder.ValueSighting{Value: value, FirstSeen: seen, BeyondHorizon: older})
	}
	return sightings, nil
}

// facetClause spells `facet:value`, quoting a value the query syntax would split.
func facetClause(facet, value string) string {
	if strings.ContainsAny(value, ` :"()\*?`) {
		value = `"` + strings.ReplaceAll(value, `"`, `\"`) + `"`
	}
	return facet + ":" + value
}

// countWithout counts a source's service lines that carry no `env` tag at all.
func (m Measurer) countWithout(ctx context.Context, src ddfeeder.LogSource, from, to time.Time, indexes []string) (int64, int64, error) {
	out, err := m.aggregate(ctx, AggregateRequest{
		Compute: []Compute{{Aggregation: "count", Type: "total"}},
		Filter:  NewLogFilter("service:"+src.Service+" -env:*", from, to, indexes),
		GroupBy: []GroupBy{{Facet: "status", Limit: 20, Missing: "__no_status__"}},
	})
	if err != nil {
		return 0, 0, err
	}
	var lines, errorLines int64
	for _, b := range out.Data.Buckets {
		n, _ := b.Count(0)
		lines += n
		if b.Key("status") == "error" {
			errorLines += n
		}
	}
	return lines, errorLines, nil
}
