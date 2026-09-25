// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"fmt"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/internal/investigation/workers/metrics"
)

// `compare`: baseline against symptom, with the comparison stated (T096, T105; FR-092, FR-094).
//
// A before/after request returns **one digest carrying an explicit `Comparison`** — direction and
// magnitude — rather than two unrelated summaries the reader has to subtract. That is FR-092, and
// it is the difference between evidence and homework: two summaries side by side let two readers
// reach two conclusions about the same numbers.
//
// Both halves are aligned and reduced **in GCP**. What crosses the digest boundary is the
// statistics of the aligned series, never a point-by-point sample — which the aggregation makes
// structurally true rather than a rule this file has to remember (contract §3).

// compare answers the term.
func (b *Backend) compare(ctx context.Context, term *investigationv1.CompareTerm) (answer, error) {
	selector := term.GetPointer().GetSelector()
	facts := parseSelector(selector)
	statistic := term.GetStatistic()

	baseline, baselineNarrowed := narrow(term.GetWindows().GetBaseline(), WindowCapStandard)
	symptom, symptomNarrowed := narrow(term.GetWindows().GetSymptom(), WindowCapStandard)

	if b.transport == nil || b.transport.Metrics == nil {
		outcome, err := b.absent("cloud_monitoring:"+facts.MetricType,
			b.transport.absentSourceOf("Cloud Monitoring"), symptom)
		return answer{outcome: outcome, query: selector, vocabulary: VocabMonitoring}, err
	}

	groupBy := []string(nil)
	if statistic == investigationv1.Statistic_ERROR_RATE {
		// The error split comes from a metric label, so it is a grouping rather than a second
		// query: `request_count` grouped by `response_code_class` is both the numerator and the
		// denominator in one aggregation (contract §3.1).
		groupBy = []string{"metric.labels." + LabelResponseCodeClass}
	}
	agg := aggregationFor(statistic, symptom, groupBy)

	baselineSeries, err := b.queryMetrics(ctx, facts.scope(b.project), selector, baseline, agg)
	if err != nil {
		return b.queryFailed(err, selector, VocabMonitoring)
	}
	symptomSeries, err := b.queryMetrics(ctx, facts.scope(b.project), selector, symptom, agg)
	if err != nil {
		return b.queryFailed(err, selector, VocabMonitoring)
	}
	if len(baselineSeries) == 0 && len(symptomSeries) == 0 {
		return b.emptyMetricAnswer(ctx, facts, selector, symptom, agg)
	}

	key := termKeyOf(engine.Compare(term.GetPointer(), term.GetWindows(), statistic))
	baselineSummary, baselineValue, baselinePoints := b.summarise(
		baselineSeries, facts, baseline, agg, statistic, key, "baseline", selector)
	symptomSummary, symptomValue, symptomPoints := b.summarise(
		symptomSeries, facts, symptom, agg, statistic, key, "symptom", selector)

	criteria := b.metricCriteria(facts, statistic)
	if baselineNarrowed || symptomNarrowed {
		criteria = append(criteria, criterion(CriterionWindowCap, fmt.Sprintf(
			"a compare is capped at %s per window and the request was wider; the most recent %s "+
				"of each half was queried", WindowCapStandard, WindowCapStandard)))
	}

	lag, lagSource := b.metricLag(ctx, facts.MetricType)
	coverage, err := b.coverage(coverageInput{
		Entities:   entitiesOf(facts),
		DataSource: "cloud_monitoring:" + facts.MetricType,
		Window:     symptom,
		Volume:     baselinePoints + symptomPoints,
		Sampling:   agg.Describe,
		Criteria:   criteria,
		Lag:        lag,
		LagSource:  lagSource,
	})
	if err != nil {
		return answer{}, err
	}
	return answer{
		outcome: engine.DigestOutcome{Body: &investigationv1.Digest{
			Body: &investigationv1.Digest_Metric{Metric: &investigationv1.MetricDigest{
				Series:      append(baselineSummary, symptomSummary...),
				Comparisons: []*investigationv1.Comparison{comparisonOf(statistic, baselineValue, symptomValue)},
			}},
			Coverage: coverage,
		}},
		query:      selector,
		vocabulary: VocabMonitoring,
		deepLink:   b.deepLink(handleMetric, selector, symptom, facts),
	}, nil
}

// summarise turns the aligned series of one window into SeriesSummary rows, and returns the single
// value the comparison is made on.
//
// For an error rate that value is derived across the groups — 5xx over all classes — rather than
// read off one of them, which is what contract §3.1 means by *derived*. Where the selector pins a
// single class the denominator is not in the answer at all, and the value is that class's own rate
// with the coverage saying so.
func (b *Backend) summarise(series []*monitoringpb.TimeSeries, facts selectorFacts, window *engine.Window,
	agg aggregation, statistic investigationv1.Statistic, mintedBy, group, selector string,
) ([]*investigationv1.SeriesSummary, float64, int64) {
	rows := make([]*investigationv1.SeriesSummary, 0, len(series))
	var considered int64
	var errorTotal, allTotal float64
	var plain []float64

	for _, s := range series {
		values, instants := seriesValues(s)
		considered += int64(len(values))
		var firstSeen time.Time
		if len(instants) > 0 {
			firstSeen = instants[0]
		}
		tags := tagsOf(s)
		tags["window"] = group

		var sum float64
		for _, v := range values {
			sum = round6(sum + v)
		}
		class := s.GetMetric().GetLabels()[LabelResponseCodeClass]
		switch {
		case statistic != investigationv1.Statistic_ERROR_RATE:
			plain = append(plain, values...)
		case class == "5xx":
			errorTotal = round6(errorTotal + sum)
			allTotal = round6(allTotal + sum)
		default:
			allTotal = round6(allTotal + sum)
		}

		rows = append(rows, &investigationv1.SeriesSummary{
			Tags:              tags,
			PointCount:        int64(len(values)),
			IntervalCovered:   intervalOf(instants, window),
			ResolutionSeconds: int64(agg.Period / time.Second),
			Statistics:        statisticsOf(values),
			JoinKeys:          keysFromLabels(facts, s.GetResource().GetLabels(), s.GetMetric().GetLabels(), firstSeen),
			DrillDown:         b.mint(mintedBy, handleMetric, selector, group+":"+class, window, facts),
		})
	}

	if statistic == investigationv1.Statistic_ERROR_RATE {
		if facts.ResponseCodeClass != "" || allTotal == 0 {
			// The selector pins one class, so there is no total to divide by. The figure is
			// that class's own rate, and the coverage says which of the two this is.
			return rows, round6(errorTotal + nonErrorWhenPinned(facts, allTotal)), considered
		}
		return rows, round6(errorTotal / allTotal), considered
	}
	return rows, aggregateFor(statistic, plain), considered
}

// nonErrorWhenPinned returns the pinned class's own total when the selector restricts to a class
// that is not 5xx, so that a pointer pinning `4xx` reports the 4xx rate rather than zero.
func nonErrorWhenPinned(facts selectorFacts, allTotal float64) float64 {
	if facts.ResponseCodeClass == "5xx" {
		return 0
	}
	return allTotal
}

// aggregateFor reduces the aligned values of a window to the one number a comparison is made on,
// by the same statistic the caller asked for.
func aggregateFor(statistic investigationv1.Statistic, values []float64) float64 {
	stats := statisticsOf(values)
	if value, ok := stats[statistic.String()]; ok {
		return value
	}
	// RATE and the unspecified default are a mean of the aligned rate: the aligner has already
	// turned the counter into a rate, so the summary of it is its average.
	return stats[investigationv1.Statistic_MEAN.String()]
}

// comparisonOf states the direction and magnitude, and whether the two windows separate the
// hypothesis at all.
//
// `Separable` is the metrics worker's published rule applied here rather than a second opinion
// formed in this file: the generator, the worker and this backend must not state three different
// things about the same two numbers. `direction` is a coarser description of the same number and
// is deliberately not that rule — a gentle ramp is "up" and still does not discriminate.
func comparisonOf(statistic investigationv1.Statistic, baseline, symptom float64) *investigationv1.Comparison {
	absolute := round6(symptom - baseline)
	relative := 0.0
	if baseline != 0 {
		relative = round6(absolute / baseline)
	}
	direction := "flat"
	switch {
	case relative > 0.1:
		direction = "up"
	case relative < -0.1:
		direction = "down"
	}
	return &investigationv1.Comparison{
		Statistic:     statistic,
		Baseline:      baseline,
		Symptom:       symptom,
		AbsoluteDelta: absolute,
		RelativeDelta: relative,
		Direction:     direction,
		Separable:     metrics.Separates(relative),
	}
}

// metricCriteria are the caveats every answer over this metric carries.
func (b *Backend) metricCriteria(facts selectorFacts, statistic investigationv1.Statistic) []string {
	var criteria []string
	if facts.derivedFromRequestCount() {
		criteria = append(criteria, requestCountCaveats()...)
	}
	if statistic == investigationv1.Statistic_ERROR_RATE && facts.ResponseCodeClass != "" {
		criteria = append(criteria, criterion(CriterionErrorRateDenominatorAbsent, fmt.Sprintf(
			"the selector pins metric.labels.response_code_class=%q, so the total to divide by "+
				"is not in this answer; the figure is that class's own aligned rate, not a ratio",
			facts.ResponseCodeClass)))
	}
	return criteria
}

// emptyMetricAnswer is the answer when a metric query returned no series at all. It is the cutoff
// of contract §8 written once:
//
//	window.End >  now − ingestDelay → NOT_YET_INGESTED: an empty answer means nothing yet
//	window.End <= now − ingestDelay → NO_DATA: the window was covered and held nothing
//
// Getting this backwards in either direction is a defect with a reader-visible consequence.
// Reporting NO_DATA inside the lag tells an investigation that nothing happened during the
// minutes it is actually about; reporting NOT_YET_INGESTED outside it withholds the one outcome
// that is evidence.
func (b *Backend) emptyMetricAnswer(ctx context.Context, facts selectorFacts, selector string,
	window *engine.Window, agg aggregation,
) (answer, error) {
	lag, lagSource := b.metricLag(ctx, facts.MetricType)
	coverage, err := b.coverage(coverageInput{
		Entities:   entitiesOf(facts),
		DataSource: "cloud_monitoring:" + facts.MetricType,
		Window:     window,
		Volume:     0,
		Sampling:   agg.Describe,
		Criteria:   b.metricCriteria(facts, investigationv1.Statistic_STATISTIC_UNSPECIFIED),
		Lag:        lag,
		LagSource:  lagSource,
	})
	if err != nil {
		return answer{}, err
	}
	out := answer{
		query:      selector,
		vocabulary: VocabMonitoring,
		deepLink:   b.deepLink(handleMetric, selector, window, facts),
	}
	if !ingested(b.now(), window, lag, lagSource) {
		out.outcome = engine.NotYetIngested{Coverage: coverage}
		return out, nil
	}
	out.outcome = engine.NoData{Coverage: coverage}
	return out, nil
}

// entitiesOf names the graph entities a selector is about, in the identifiers the graph uses. It
// is the coverage block's `searched_entities`, which is what lets a reader check that the answer
// is about the entity they asked about.
func entitiesOf(facts selectorFacts) []string {
	var out []string
	if facts.Service != "" {
		out = append(out, "gcp.cloudrun.service:"+facts.Project+"/"+facts.Region+"/"+facts.Service)
	}
	if facts.Revision != "" {
		out = append(out, "gcp.cloudrun.revision:"+facts.Project+"/"+facts.Region+"/"+facts.Revision)
	}
	return out
}

// termKeyOf is the key of the term an answer is minting handles from, so a handle records which
// answer minted it. A key that cannot be computed is empty rather than fabricated: a handle whose
// minted-by key is a guess would let a caller present it back against an answer that never
// existed.
func termKeyOf(term *engine.Term) string {
	key, err := engine.TermKey(term)
	if err != nil {
		return ""
	}
	return key
}
