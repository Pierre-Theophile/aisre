// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
)

// Server-side aggregation is the whole design (T096; contract §3).
//
// `timeSeries.list` takes `aggregation` and `secondaryAggregation`, each with `alignmentPeriod`,
// `perSeriesAligner`, `crossSeriesReducer` and `groupByFields`. **The summary is computed in GCP
// and the raw points never leave it.** That is what makes constitution IV natural here rather than
// effortful: a `compare` is two aligned-and-reduced queries, not a download and a loop, and "no
// point-by-point samples in the digest" is structurally true rather than a rule someone remembers.
//
// What comes back to this process is the aligned series — which is the *aggregate*, at the
// alignment period asked for — and the digest reports statistics over it. No sample of the
// underlying stream is retrieved, and none appears in any response, exemplar or coverage field.

// The bounding facts of `timeSeries.list`, stated in the coverage of every answer built on it
// (contract §3) rather than discovered by a reader wondering why a number looks the way it does.
const (
	// MaxPageSize is the API's own coercion point: a `pageSize` above 100,000 is coerced to
	// 100,000, so asking for more is asking for exactly this.
	MaxPageSize = 100_000
	// MinAlignmentPeriod is the API's minimum. Below it the request is rejected, not rounded.
	MinAlignmentPeriod = 60 * time.Second
	// TargetAlignedPoints is how many aligned points this backend aims a window at. It is what
	// keeps a wide window from returning a fine-grained series that the digest would then have
	// to truncate: the alignment period is widened until the window fits.
	TargetAlignedPoints = 240
)

// alignmentFor picks the alignment period for a window: at least the API's 60 s minimum, and wide
// enough that the window yields no more than TargetAlignedPoints. It is a function of the window
// alone, so the same question asked twice aggregates the same way and hashes to the same digest.
func alignmentFor(window *engine.Window) time.Duration {
	if window.GetStart() == nil || window.GetEnd() == nil {
		return MinAlignmentPeriod
	}
	width := window.GetEnd().AsTime().Sub(window.GetStart().AsTime())
	if width <= 0 {
		return MinAlignmentPeriod
	}
	period := time.Duration(math.Ceil(float64(width) / TargetAlignedPoints))
	// Rounded up to a whole minute, because an alignment period of 73 seconds is a number
	// nobody can reason about and a series nobody can line up against another.
	period = period.Round(time.Minute)
	if period < MinAlignmentPeriod {
		return MinAlignmentPeriod
	}
	return period
}

// aggregation is the server-side reduction one query asks for.
type aggregation struct {
	Aligner  monitoringpb.Aggregation_Aligner
	Reducer  monitoringpb.Aggregation_Reducer
	GroupBy  []string
	Period   time.Duration
	Describe string
}

// aggregationFor maps a published Statistic onto the aligner and reducer that computes it in GCP.
//
// The mapping is one-way on purpose: the algebra names a statistic, and this file decides how GCP
// computes it. A caller cannot ask for an aligner, which is the same rule as "caller-supplied
// query text is never executed" applied to the aggregation half.
func aggregationFor(statistic investigationv1.Statistic, window *engine.Window, groupBy []string) aggregation {
	period := alignmentFor(window)
	agg := aggregation{Period: period, GroupBy: groupBy}
	switch statistic {
	case investigationv1.Statistic_COUNT:
		agg.Aligner, agg.Reducer = monitoringpb.Aggregation_ALIGN_DELTA, monitoringpb.Aggregation_REDUCE_SUM
	case investigationv1.Statistic_RATE, investigationv1.Statistic_ERROR_RATE:
		agg.Aligner, agg.Reducer = monitoringpb.Aggregation_ALIGN_RATE, monitoringpb.Aggregation_REDUCE_SUM
	case investigationv1.Statistic_P50:
		agg.Aligner, agg.Reducer = monitoringpb.Aggregation_ALIGN_PERCENTILE_50, monitoringpb.Aggregation_REDUCE_MEAN
	case investigationv1.Statistic_P95:
		agg.Aligner, agg.Reducer = monitoringpb.Aggregation_ALIGN_PERCENTILE_95, monitoringpb.Aggregation_REDUCE_MEAN
	case investigationv1.Statistic_P99:
		agg.Aligner, agg.Reducer = monitoringpb.Aggregation_ALIGN_PERCENTILE_99, monitoringpb.Aggregation_REDUCE_MEAN
	case investigationv1.Statistic_MAX:
		agg.Aligner, agg.Reducer = monitoringpb.Aggregation_ALIGN_MAX, monitoringpb.Aggregation_REDUCE_MAX
	default: // MEAN and the unspecified default
		agg.Aligner, agg.Reducer = monitoringpb.Aggregation_ALIGN_MEAN, monitoringpb.Aggregation_REDUCE_MEAN
	}
	agg.Describe = agg.sampling()
	return agg
}

// sampling is what the coverage block states: the aggregation actually applied, plus the bounding
// facts of the surface it was applied on. A reader who knows the alignment period knows what one
// point means; a reader who knows `orderBy` is blank knows why the points arrive newest-first.
func (a aggregation) sampling() string {
	var b strings.Builder
	fmt.Fprintf(&b, "alignment_period=%s;per_series_aligner=%s;cross_series_reducer=%s",
		a.Period, a.Aligner, a.Reducer)
	if len(a.GroupBy) > 0 {
		fmt.Fprintf(&b, ";group_by_fields=%s", strings.Join(a.GroupBy, ","))
	}
	fmt.Fprintf(&b, ";view=FULL;page_size=%d;order_by=blank(points_most_recent_first)", MaxPageSize)
	return b.String()
}

// proto renders the aggregation as the API takes it.
func (a aggregation) proto() *monitoringpb.Aggregation {
	return &monitoringpb.Aggregation{
		AlignmentPeriod:    durationpb.New(a.Period),
		PerSeriesAligner:   a.Aligner,
		CrossSeriesReducer: a.Reducer,
		GroupByFields:      a.GroupBy,
	}
}

// queryMetrics runs one aggregated query and returns the aligned series.
//
// `orderBy` is deliberately not set: the API requires it to be blank when an aggregation is
// present, and points come back most-recent-first regardless. `view` is FULL because HEADERS
// returns no points at all, and a digest built on a headers-only read would report a point count
// of zero for a series that has data.
func (b *Backend) queryMetrics(ctx context.Context, project, filter string, window *engine.Window, agg aggregation) ([]*monitoringpb.TimeSeries, error) {
	if b.transport == nil || b.transport.Metrics == nil {
		return nil, errAbsentMetrics
	}
	return b.transport.Metrics.ListTimeSeries(ctx, &monitoringpb.ListTimeSeriesRequest{
		Name:   "projects/" + project,
		Filter: filter,
		Interval: &monitoringpb.TimeInterval{
			StartTime: window.GetStart(),
			EndTime:   window.GetEnd(),
		},
		Aggregation: agg.proto(),
		View:        monitoringpb.ListTimeSeriesRequest_FULL,
		PageSize:    MaxPageSize,
	})
}

// errAbsentMetrics is the sentinel for a backend given no metric reader. It is a sentinel rather
// than an error string because the caller turns it into NO_DATA naming the absent source, and a
// string comparison is how that turns into a QUERY_FAILED by accident one day.
var errAbsentMetrics = fmt.Errorf("gcp: no Cloud Monitoring reader configured")

// pointValue reads one aligned point's value. A distribution is reduced to its mean, because an
// aligner that returns one is an aligner asked for a distribution-valued metric without a
// percentile — and the mean is the only scalar a distribution states without a quantile.
func pointValue(point *monitoringpb.Point) (float64, bool) {
	switch v := point.GetValue().GetValue().(type) {
	case *monitoringpb.TypedValue_DoubleValue:
		return v.DoubleValue, true
	case *monitoringpb.TypedValue_Int64Value:
		return float64(v.Int64Value), true
	case *monitoringpb.TypedValue_DistributionValue:
		if v.DistributionValue.GetCount() == 0 {
			return 0, false
		}
		return v.DistributionValue.GetMean(), true
	case *monitoringpb.TypedValue_BoolValue:
		if v.BoolValue {
			return 1, false
		}
		return 0, false
	default:
		// A string-valued point is not a measurement. Reporting it as zero would put a number
		// in a statistic that has none.
		return 0, false
	}
}

// statisticsOf summarises one aligned series. The keys are from the published Statistic set, so a
// reader joining two digests from two vendors reads the same names.
//
// Every accumulation goes through round6 for the same reason the onset estimator's does: goldens
// are compared byte for byte and CI runs on both amd64 and arm64, and Go permits a compiler to
// fuse `a*b + c` on one and not the other. Rounding at each step means both architectures do
// arithmetic on the same six-decimal quantities.
func statisticsOf(values []float64) map[string]float64 {
	if len(values) == 0 {
		return map[string]float64{}
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)

	var total float64
	max := sorted[len(sorted)-1]
	for _, v := range values {
		total = round6(total + v)
	}
	return map[string]float64{
		investigationv1.Statistic_COUNT.String(): round6(total),
		investigationv1.Statistic_MEAN.String():  round6(total / float64(len(values))),
		investigationv1.Statistic_MAX.String():   round6(max),
		investigationv1.Statistic_P50.String():   nearestRank(sorted, 0.50),
		investigationv1.Statistic_P95.String():   nearestRank(sorted, 0.95),
		investigationv1.Statistic_P99.String():   nearestRank(sorted, 0.99),
	}
}

// nearestRank is the nearest-rank quantile: no interpolation, so the answer is always a value that
// was actually returned and the arithmetic cannot diverge between architectures.
func nearestRank(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(round6(q*float64(len(sorted)-1)) + 0.5)
	if rank < 0 {
		rank = 0
	}
	if rank >= len(sorted) {
		rank = len(sorted) - 1
	}
	return sorted[rank]
}

func round6(v float64) float64 { return math.Round(v*1e6) / 1e6 }

// seriesValues extracts the aligned values of one series, newest-first as the API returns them,
// reversed into chronological order because everything downstream — the onset estimator, the
// first-seen instant, the interval covered — reads a series forwards.
func seriesValues(series *monitoringpb.TimeSeries) ([]float64, []time.Time) {
	points := series.GetPoints()
	values := make([]float64, 0, len(points))
	instants := make([]time.Time, 0, len(points))
	for i := len(points) - 1; i >= 0; i-- {
		value, ok := pointValue(points[i])
		if !ok {
			continue
		}
		values = append(values, round6(value))
		instants = append(instants, points[i].GetInterval().GetEndTime().AsTime().UTC())
	}
	return values, instants
}

// intervalOf is the window a series actually covers, which is the first and last aligned point
// rather than the window that was asked for. Reporting the asked-for window would claim coverage
// GCP did not return.
func intervalOf(instants []time.Time, asked *engine.Window) *engine.Window {
	if len(instants) == 0 {
		return asked
	}
	return &engine.Window{
		Start: timestamppb.New(instants[0]),
		End:   timestamppb.New(instants[len(instants)-1]),
	}
}

// tagsOf is the identifying label set of one series, resource labels and metric labels together.
// It is what tells two rows of a grouped answer apart, and it is sorted into a map the canonical
// encoder orders, so two identical answers hash identically.
func tagsOf(series *monitoringpb.TimeSeries) map[string]string {
	tags := make(map[string]string, 8)
	for key, value := range series.GetResource().GetLabels() {
		tags[key] = value
	}
	for key, value := range series.GetMetric().GetLabels() {
		tags[key] = value
	}
	if t := series.GetResource().GetType(); t != "" {
		tags["resource_type"] = t
	}
	return tags
}
