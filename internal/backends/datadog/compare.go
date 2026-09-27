// SPDX-License-Identifier: Apache-2.0

package datadog

import (
	"context"
	"fmt"
	"math"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/datadogx"
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/internal/investigation/workers/metrics"
	sdk "github.com/Pierre-Theophile/aisre/pkg/backend"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// `compare`: baseline against symptom over log counts, one digest with an explicit Comparison
// (contract §2; FR-040b).
//
// A log pointer can state three statistics honestly: a COUNT of lines, their RATE per second, and the
// ERROR_RATE — error-level lines over all lines. Each window is one aggregate grouped by status, so
// the numerator and the denominator come from the same call. A latency percentile is not a property
// of a log count, and is refused rather than approximated; with apm_topology it gains the APM metric
// surface (contract §2, last paragraph).

// errorStatus is the status an error-level line carries: the same clause errors_by_version uses.
const errorStatus = "error"

func (b *Backend) compare(ctx context.Context, term *investigationv1.CompareTerm) (answer, error) {
	raw := term.GetPointer().GetSelector()
	sel, err := parseSelector(raw)
	if err != nil {
		return unsupported(raw, err), nil
	}
	statistic := term.GetStatistic()
	switch statistic {
	case investigationv1.Statistic_COUNT, investigationv1.Statistic_RATE, investigationv1.Statistic_ERROR_RATE:
	default:
		return answer{outcome: engine.QueryFailed{Reason: investigationv1.FailureReason_REJECTED_BY_BACKEND,
			Detail: fmt.Sprintf("datadog: compare over a log pointer states COUNT, RATE or ERROR_RATE; %s is not "+
				"a property of a log count and is not approximated", statistic)},
			query: sel.query(), vocabulary: feeder.VocabDatadogLogs}, nil
	}
	baseline, baselineNarrowed := narrow(term.GetWindows().GetBaseline(), WindowCapStandard)
	symptom, symptomNarrowed := narrow(term.GetWindows().GetSymptom(), WindowCapStandard)

	group := []datadogx.GroupBy{{Facet: "status", Limit: 20, Missing: "__no_status__"}}
	count := []datadogx.Compute{{Aggregation: "count", Type: "total"}}
	requests := []datadogx.AggregateRequest{
		{Compute: count, Filter: filterOf(sel, baseline, b.indexes), GroupBy: group},
		{Compute: count, Filter: filterOf(sel, symptom, b.indexes), GroupBy: group},
	}
	query := canonical(requests)
	var halves [2]struct{ errors, total int64 }
	var last *datadogx.Response
	partial := false
	for i, req := range requests {
		out, resp, err := b.client.AggregateLogs(ctx, req)
		if err != nil {
			return failed(err, query)
		}
		last = resp
		partial = partial || out.Meta.Partial()
		for _, bucket := range out.Data.Buckets {
			n, _ := bucket.Count(0)
			halves[i].total += n
			if bucket.Key("status") == errorStatus {
				halves[i].errors += n
			}
		}
	}

	value := func(i int, window *engine.Window) float64 {
		h := halves[i]
		switch statistic {
		case investigationv1.Statistic_RATE:
			seconds := window.GetEnd().AsTime().Sub(window.GetStart().AsTime()).Seconds()
			if seconds <= 0 {
				return 0
			}
			return round6(float64(h.total) / seconds)
		case investigationv1.Statistic_ERROR_RATE:
			if h.total == 0 {
				return 0
			}
			return round6(float64(h.errors) / float64(h.total))
		default:
			return float64(h.total)
		}
	}
	summary := func(i int, window *engine.Window, name string) *investigationv1.SeriesSummary {
		return &investigationv1.SeriesSummary{
			Tags:            map[string]string{"window": name, "source": "log_count"},
			PointCount:      1,
			IntervalCovered: window,
			Statistics: map[string]float64{
				investigationv1.Statistic_COUNT.String(): float64(halves[i].total),
				statistic.String():                       value(i, window),
			},
			JoinKeys: &investigationv1.JoinKeys{Workload: sel.Service},
		}
	}

	truncation := "log_derived: counts of indexed log lines, error-level meaning status:" + errorStatus
	if baselineNarrowed || symptomNarrowed {
		truncation = joinTruncation(truncation, narrowedText(sdk.TermCompare, WindowCapStandard))
	}
	if partial {
		truncation = joinTruncation(truncation, "partial: Datadog reported a timeout or warnings for this aggregation")
	}
	considered := halves[0].total + halves[1].total
	coverage, err := b.coverageOf(sel, symptom, considered, "none", truncation, last)
	if err != nil {
		return answer{}, err
	}
	digest := &investigationv1.Digest{
		Body: &investigationv1.Digest_Metric{Metric: &investigationv1.MetricDigest{
			Series:      []*investigationv1.SeriesSummary{summary(0, baseline, "baseline"), summary(1, symptom, "symptom")},
			Comparisons: []*investigationv1.Comparison{comparisonOf(statistic, value(0, baseline), value(1, symptom))},
		}},
		Coverage: coverage,
	}
	var outcome engine.Outcome = engine.DigestOutcome{Body: digest}
	switch {
	case partial:
		outcome = engine.Partial{Body: digest, Missing: "Datadog reported a timeout or warnings, so a count may be low"}
	case considered == 0:
		outcome = engine.NoData{Coverage: coverage}
	}
	return answer{outcome: outcome, query: query, vocabulary: feeder.VocabDatadogLogs,
		deepLink: b.deepLink(sel.query(), symptom)}, nil
}

func comparisonOf(statistic investigationv1.Statistic, baseline, symptom float64) *investigationv1.Comparison {
	absolute := round6(symptom - baseline)
	relative := 0.0
	if baseline != 0 {
		relative = round6(absolute / baseline)
	}
	direction := "flat"
	switch {
	case relative > 0.1 || (baseline == 0 && symptom > 0):
		direction = "up"
	case relative < -0.1:
		direction = "down"
	}
	return &investigationv1.Comparison{
		Statistic: statistic, Baseline: baseline, Symptom: symptom,
		AbsoluteDelta: absolute, RelativeDelta: relative, Direction: direction,
		Separable: metrics.Separates(relative) || (baseline == 0 && symptom > 0),
	}
}

func round6(v float64) float64 { return math.Round(v*1e6) / 1e6 }
