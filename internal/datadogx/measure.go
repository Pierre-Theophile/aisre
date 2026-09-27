// SPDX-License-Identifier: Apache-2.0

package datadogx

import (
	"context"
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
}

// Measure measures one source.
func (m Measurer) Measure(ctx context.Context, src ddfeeder.LogSource, override string, from, to time.Time) (ddfeeder.SourceMeasurement, error) {
	base := "service:" + src.Service + " env:" + src.Env
	indexes := m.Indexes
	if src.Index != "" {
		indexes = []string{src.Index}
	}
	count := func(extra string) (lines, errorLines int64, err error) {
		query := base
		if extra != "" {
			query += " " + extra
		}
		out, _, err := m.Client.AggregateLogs(ctx, AggregateRequest{
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
