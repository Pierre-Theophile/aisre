// SPDX-License-Identifier: Apache-2.0

package datadog

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/datadogx"
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	sdk "github.com/Pierre-Theophile/aisre/pkg/backend"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The `apm_topology` capability's read side (T090; contract datadog-telemetry-backend.md §2, last
// paragraph; FR-040b, FR-049b).
//
// With the capability off none of this runs: `error_spans` answers the typed NO_DATA naming the absent
// span source, and `compare` refuses what a log count cannot state, byte for byte as before (SC-023,
// SC-024). With it on:
//
//   - `error_spans(src, dst)` reads the spans the source service emitted towards the destination
//     (`@peer.service:<dst>`) from the span aggregate, grouped by operation and error kind. The counts
//     are of the spans Datadog RETAINED, not of every request, and the coverage says so: an aggregate
//     over retained spans is a sample of the traffic, and a digest that hid that would read as the
//     whole of it;
//   - `compare` over a `datadog-apm-metric/v1` pointer states COUNT, RATE, ERROR_RATE and the P50, P95
//     and P99 latencies from Datadog's trace metrics, which count every ingested request, per version
//     where the service stamps one.
//
// Everything here is built against the published API shapes and has not been verified against a live
// organisation. The metric names are one table (apmMetrics) so that a correction is one line.

// The bounds on what one APM answer carries.
const (
	// spanOperationLimit and spanErrorKindLimit bound the groups asked of the span aggregate.
	spanOperationLimit = 20
	spanErrorKindLimit = 5
	// maxAPMVersions bounds the per-version series of a compare.
	maxAPMVersions = 10
)

// unspecifiedErrorKind names the errors of a span that states no error type.
const unspecifiedErrorKind = "error"

// apmMetrics are the trace metrics a statistic reads, by span name. Datadog derives them from the
// operation name: `http.request` yields `trace.http.request.hits`.
var apmMetrics = struct {
	hits, errors string
	percentile   map[investigationv1.Statistic]string
}{
	hits:   "trace.%s.hits",
	errors: "trace.%s.errors",
	percentile: map[investigationv1.Statistic]string{
		investigationv1.Statistic_P50: "trace.%s.duration.by.service.50p",
		investigationv1.Statistic_P95: "trace.%s.duration.by.service.95p",
		investigationv1.Statistic_P99: "trace.%s.duration.by.service.99p",
	},
}

// ---- the entity a span term names ------------------------------------------------------------------

// ServiceRef is the Datadog service an entity id stands for.
type ServiceRef struct{ Env, Service string }

// resolveEntity reads a graph entity id as the Datadog service it names. The backend holds no graph, so
// by default an id is read the way the feeder spells a service node's value — `<env>/<service>` — or as
// a bare service name in the configured environment; Options.EntityService replaces the reading for a
// caller that holds the graph.
func (b *Backend) resolveEntity(id string) (ServiceRef, bool) {
	if b.entityService != nil {
		return b.entityService(id)
	}
	env, service, ok := strings.Cut(strings.TrimSpace(id), "/")
	if !ok {
		env, service = b.apmEnv, strings.TrimSpace(id)
	}
	if service == "" || env == "" || strings.ContainsAny(env+service, " :*?()\"") {
		return ServiceRef{}, false
	}
	return ServiceRef{Env: env, Service: service}, true
}

// ---- error_spans -----------------------------------------------------------------------------------

// errorSpansAPM answers `error_spans` from the span aggregate.
func (b *Backend) errorSpansAPM(ctx context.Context, term *investigationv1.ErrorSpansTerm) (answer, error) {
	src, srcOK := b.resolveEntity(term.GetSrcEntityId())
	dst, dstOK := b.resolveEntity(term.GetDstEntityId())
	window, narrowed := narrow(term.GetWindow(), WindowCapStandard)
	entities := []string{term.GetSrcEntityId(), term.GetDstEntityId()}
	if !srcOK || !dstOK {
		coverage, err := b.spanCoverage(entities, "datadog_apm:unresolved", window, 0, "none",
			"the entity id names no Datadog service, so no span query was made", nil)
		if err != nil {
			return answer{}, err
		}
		return answer{
			outcome: engine.NoData{Coverage: coverage, AbsentSource: fmt.Sprintf(
				"a Datadog service and environment for the edge %s -> %s, which the entity ids do not name",
				term.GetSrcEntityId(), term.GetDstEntityId())},
			query:      fmt.Sprintf("error_spans(%s -> %s): not executed; unresolved entity", term.GetSrcEntityId(), term.GetDstEntityId()),
			vocabulary: feeder.VocabDatadogSpans,
		}, nil
	}
	if term.GetEdgeType() != graphv1.EdgeType_CALLS && term.GetEdgeType() != graphv1.EdgeType_EDGE_TYPE_UNSPECIFIED {
		return answer{outcome: engine.QueryFailed{Reason: investigationv1.FailureReason_REJECTED_BY_BACKEND,
			Detail: fmt.Sprintf("datadog: error_spans reads the spans a service emits towards another; "+
				"a %s edge has none", term.GetEdgeType())},
			query: "error_spans", vocabulary: feeder.VocabDatadogSpans}, nil
	}

	query := fmt.Sprintf("service:%s env:%s @peer.service:%s", src.Service, src.Env, dst.Service)
	req := datadogx.SpanAggregateRequest{
		Compute: []datadogx.SpanCompute{
			{Aggregation: "count", Type: "total"},
			{Aggregation: "pc50", Metric: "@duration", Type: "total"},
			{Aggregation: "pc95", Metric: "@duration", Type: "total"},
			{Aggregation: "pc99", Metric: "@duration", Type: "total"},
		},
		Filter: datadogx.NewSpanFilter(query, window.GetStart().AsTime(), window.GetEnd().AsTime()),
		GroupBy: []datadogx.SpanGroupBy{
			{Facet: "resource_name", Limit: spanOperationLimit},
			{Facet: "status", Limit: 3},
			{Facet: "@error.type", Limit: spanErrorKindLimit, Missing: "__no_error_type__"},
		},
	}
	canon := canonical(req)
	out, resp, err := b.client.AggregateSpans(ctx, req)
	if err != nil {
		a, ferr := failed(err, canon)
		a.vocabulary = feeder.VocabDatadogSpans
		return a, ferr
	}

	groups := make([]*investigationv1.SpanGroup, 0, len(out.Data))
	var total int64
	for _, bucket := range out.Data {
		n, _ := bucket.Number(0)
		count := int64(n)
		total += count
		kind := ""
		if bucket.Key("status") == "error" {
			kind = bucket.Key("@error.type")
			if kind == "" || kind == "__no_error_type__" {
				kind = unspecifiedErrorKind
			}
		}
		latency := map[string]float64{}
		for i, name := range []string{"P50", "P95", "P99"} {
			if ns, ok := bucket.Number(i + 1); ok {
				latency[name] = round6(ns / 1e6) // spans state nanoseconds; digests state milliseconds
			}
		}
		groups = append(groups, &investigationv1.SpanGroup{
			Operation: bucket.Key("resource_name"), ErrorKind: kind, Count: count, Latency: latency,
			JoinKeys: &investigationv1.JoinKeys{Workload: src.Service},
		})
	}
	// Errors first, then by count, then by name: the reader's eye lands on what failed, and two
	// recordings of one world are byte-identical.
	sort.SliceStable(groups, func(i, j int) bool {
		a, c := groups[i], groups[j]
		if (a.ErrorKind != "") != (c.ErrorKind != "") {
			return a.ErrorKind != ""
		}
		if a.Count != c.Count {
			return a.Count > c.Count
		}
		if a.Operation != c.Operation {
			return a.Operation < c.Operation
		}
		return a.ErrorKind < c.ErrorKind
	})

	truncation := "retained_spans: counts are of the spans Datadog retained for this window (retention filters and " +
		"ingestion sampling apply), not of every request; the APM trace metrics count every ingested request"
	if narrowed {
		truncation = joinTruncation(truncation, narrowedText(sdk.TermErrorSpans, WindowCapStandard))
	}
	if len(out.Data) >= spanOperationLimit*3*spanErrorKindLimit {
		truncation = joinTruncation(truncation, fmt.Sprintf("group_cap: at most %d operations and %d error kinds per "+
			"operation are grouped; the rest are not in the answer", spanOperationLimit, spanErrorKindLimit))
	}
	if len(groups) > engine.MaxSpanGroups {
		truncation = joinTruncation(truncation, fmt.Sprintf("group_cap: the %d largest of %d operation and error "+
			"kind groups are in the answer", engine.MaxSpanGroups, len(groups)))
		groups = groups[:engine.MaxSpanGroups]
	}
	partial := out.Meta.Partial()
	if partial {
		truncation = joinTruncation(truncation, "partial: Datadog reported a timeout or warnings for this aggregation")
	}
	coverage, err := b.spanCoverage(entities, "datadog_apm:spans", window, total,
		"retained: an aggregate over the spans Datadog retained, exact for them and a sample of the requests",
		truncation, resp)
	if err != nil {
		return answer{}, err
	}
	digest := &investigationv1.Digest{
		Body:     &investigationv1.Digest_Trace{Trace: &investigationv1.TraceDigest{Groups: groups}},
		Coverage: coverage,
	}
	var outcome engine.Outcome = engine.DigestOutcome{Body: digest}
	switch {
	case partial:
		outcome = engine.Partial{Body: digest, Missing: "Datadog reported a timeout or warnings, so a count may be low"}
	case total == 0 && b.now().Sub(window.GetEnd().AsTime()) < lagHorizon:
		outcome = engine.NotYetIngested{Coverage: coverage, RetryAfter: window.GetEnd().AsTime().Add(lagHorizon)}
	case total == 0:
		outcome = engine.NoData{Coverage: coverage}
	}
	return answer{outcome: outcome, query: canon, vocabulary: feeder.VocabDatadogSpans,
		deepLink: b.spanLink(query, window)}, nil
}

// spanCoverage is the coverage block of an APM answer. The indexing lag is undetermined: an aggregate
// carries no newest event to measure it from.
func (b *Backend) spanCoverage(entities []string, source string, window *engine.Window, volume int64, sampling, truncation string, resp *datadogx.Response) (*investigationv1.Coverage, error) {
	in := engine.CoverageInput{
		SearchedEntities:         entities,
		DataSource:               source,
		WindowCovered:            window,
		VolumeConsidered:         volume,
		Sampling:                 sampling,
		Truncation:               truncation,
		IngestionLagUndetermined: true,
		ExecutedAt:               b.now().UTC(),
		QuotaUndetermined:        true,
	}
	if resp != nil && resp.HasQuota {
		in.QuotaUndetermined = false
		in.RemainingQuota = int64(resp.Reading.Remaining)
		in.QuotaWindow = resp.Reading.Period
	}
	return in.Coverage()
}

// spanLink opens the same span query over the same window in Datadog for a person.
func (b *Backend) spanLink(query string, window *engine.Window) string {
	if b.site == "" {
		return ""
	}
	v := url.Values{}
	v.Set("query", query)
	v.Set("start", strconv.FormatInt(window.GetStart().AsTime().UnixMilli(), 10))
	v.Set("end", strconv.FormatInt(window.GetEnd().AsTime().UnixMilli(), 10))
	return "https://app." + b.site + "/apm/traces?" + v.Encode()
}

// ---- compare over APM trace metrics ------------------------------------------------------------------

// apmSelector is a parsed `datadog-apm-metric/v1` selector.
type apmSelector struct{ Service, Env, Span string }

// parseAPMSelector reads and checks the selector: exactly `service`, `env` and `span`, each an exact
// term. Anything else — a wildcard, a fourth term, a metric name — is refused rather than executed.
func parseAPMSelector(raw string) (apmSelector, error) {
	var s apmSelector
	for _, token := range strings.Fields(raw) {
		key, value, ok := strings.Cut(token, ":")
		switch {
		case !ok || key == "" || value == "":
			return apmSelector{}, fmt.Errorf("datadog: %q is not a `key:value` term; free text is not in the "+
				"published %s subset", token, feeder.VocabDatadogAPMMetric)
		case strings.ContainsAny(value, "*?(){}\",: ") || strings.ContainsAny(key, "*?(){}\","):
			return apmSelector{}, fmt.Errorf("datadog: %q uses a wildcard, a group or a quote, which the "+
				"published %s subset does not admit", token, feeder.VocabDatadogAPMMetric)
		}
		switch key {
		case "service":
			s.Service = value
		case "env":
			s.Env = value
		case "span":
			s.Span = value
		default:
			return apmSelector{}, fmt.Errorf("datadog: %q is not a term of %s (service, env, span)", token,
				feeder.VocabDatadogAPMMetric)
		}
	}
	if s.Service == "" || s.Env == "" || s.Span == "" {
		return apmSelector{}, fmt.Errorf("datadog: an APM metric selector must pin service, env and span; %q "+
			"does not", raw)
	}
	return s, nil
}

func (s apmSelector) scope() string { return "{service:" + s.Service + ",env:" + s.Env + "}" }

func (s apmSelector) String() string {
	return "service:" + s.Service + " env:" + s.Env + " span:" + s.Span
}

func (s apmSelector) entity() string { return s.Service + "@" + s.Env }

// metricQuery renders one trace-metric query. byVersion adds the grouping that yields a series per
// version; counts are read as counts, and a percentile as the gauge Datadog states.
func (s apmSelector) metricQuery(pattern, aggregation string, count, byVersion bool) string {
	q := aggregation + ":" + fmt.Sprintf(pattern, s.Span) + s.scope()
	if byVersion {
		q += " by {version}"
	}
	if count {
		q += ".as_count()"
	}
	return q
}

// apmHalf is what one window's metric queries returned: totals for the whole scope, and per version.
type apmHalf struct {
	hits, errors float64
	// percentile is the mean of Datadog's per-interval percentile over the window, in seconds, for a
	// percentile statistic; zero otherwise.
	percentile float64
	versions   map[string]*apmVersion
	points     int
}

type apmVersion struct {
	hits, errors, percentile float64
	points                   int
}

// apmCompare answers `compare` over an APM metric pointer.
func (b *Backend) apmCompare(ctx context.Context, term *investigationv1.CompareTerm) (answer, error) {
	raw := term.GetPointer().GetSelector()
	sel, err := parseAPMSelector(raw)
	if err != nil {
		return answer{outcome: engine.QueryFailed{Reason: investigationv1.FailureReason_UNSUPPORTED_POINTER,
			Detail: err.Error()}, query: raw, vocabulary: feeder.VocabDatadogAPMMetric}, nil
	}
	statistic := term.GetStatistic()
	_, isPercentile := apmMetrics.percentile[statistic]
	switch {
	case isPercentile:
	case statistic == investigationv1.Statistic_COUNT, statistic == investigationv1.Statistic_RATE,
		statistic == investigationv1.Statistic_ERROR_RATE:
	default:
		return answer{outcome: engine.QueryFailed{Reason: investigationv1.FailureReason_REJECTED_BY_BACKEND,
			Detail: fmt.Sprintf("datadog: compare over an APM metric pointer states COUNT, RATE, ERROR_RATE, P50, "+
				"P95 or P99; %s is not one Datadog's trace metrics carry", statistic)},
			query: sel.String(), vocabulary: feeder.VocabDatadogAPMMetric}, nil
	}
	baseline, baselineNarrowed := narrow(term.GetWindows().GetBaseline(), WindowCapStandard)
	symptom, symptomNarrowed := narrow(term.GetWindows().GetSymptom(), WindowCapStandard)

	var queries []string
	var halves [2]apmHalf
	var last *datadogx.Response
	for i, w := range []*engine.Window{baseline, symptom} {
		half, resp, qs, err := b.apmHalfOf(ctx, sel, statistic, w)
		queries = append(queries, qs...)
		if err != nil {
			a, ferr := failed(err, canonical(queries))
			a.vocabulary = feeder.VocabDatadogAPMMetric
			return a, ferr
		}
		halves[i] = half
		if resp != nil {
			last = resp
		}
	}

	value := func(hits, errors, percentile float64, window *engine.Window) float64 {
		switch statistic {
		case investigationv1.Statistic_RATE:
			seconds := window.GetEnd().AsTime().Sub(window.GetStart().AsTime()).Seconds()
			if seconds <= 0 {
				return 0
			}
			return round6(hits / seconds)
		case investigationv1.Statistic_ERROR_RATE:
			if hits == 0 {
				return 0
			}
			return round6(errors / hits)
		case investigationv1.Statistic_COUNT:
			return round6(hits)
		default:
			return round6(percentile * 1000) // trace durations are seconds; digests state milliseconds
		}
	}
	summary := func(h apmHalf, window *engine.Window, name string) []*investigationv1.SeriesSummary {
		stats := func(hits, errors, percentile float64) map[string]float64 {
			m := map[string]float64{investigationv1.Statistic_COUNT.String(): round6(hits)}
			m[statistic.String()] = value(hits, errors, percentile, window)
			return m
		}
		points := int64(h.points)
		out := []*investigationv1.SeriesSummary{{
			Tags:            map[string]string{"window": name, "source": "apm_trace_metrics"},
			PointCount:      points,
			IntervalCovered: window,
			Statistics:      stats(h.hits, h.errors, h.percentile),
			JoinKeys:        &investigationv1.JoinKeys{Workload: sel.Service},
		}}
		names := make([]string, 0, len(h.versions))
		for v := range h.versions {
			names = append(names, v)
		}
		sort.Strings(names)
		for _, v := range names {
			ver := h.versions[v]
			out = append(out, &investigationv1.SeriesSummary{
				// The version is the join key, not a tag: a tag value is masked as a possible identifier,
				// and the join key is the one place the table keeps a version verbatim.
				Tags:            map[string]string{"window": name, "source": "apm_trace_metrics", "grouping": "version"},
				PointCount:      int64(ver.points),
				IntervalCovered: window,
				Statistics:      stats(ver.hits, ver.errors, ver.percentile),
				JoinKeys:        &investigationv1.JoinKeys{Version: v, Workload: sel.Service},
			})
		}
		return out
	}

	truncation := "apm_trace_metrics: requests, errors and durations as Datadog computes them from every ingested " +
		"trace, not from the retained spans"
	if isPercentile {
		truncation = joinTruncation(truncation, "percentile_of_intervals: Datadog states a percentile per rollup "+
			"interval and none for a window, so the value is the mean of the intervals' percentiles")
	}
	if baselineNarrowed || symptomNarrowed {
		truncation = joinTruncation(truncation, narrowedText(sdk.TermCompare, WindowCapStandard))
	}
	for _, h := range halves {
		if len(h.versions) >= maxAPMVersions {
			truncation = joinTruncation(truncation, fmt.Sprintf("version_cap: at most %d versions are summarised "+
				"per window; the rest are in the totals only", maxAPMVersions))
			break
		}
	}
	considered := int64(halves[0].points + halves[1].points)
	coverage, err := b.spanCoverage([]string{sel.entity()}, "datadog_apm:trace_metrics", symptom, considered, "none",
		truncation, last)
	if err != nil {
		return answer{}, err
	}
	digest := &investigationv1.Digest{
		Body: &investigationv1.Digest_Metric{Metric: &investigationv1.MetricDigest{
			Series: append(summary(halves[0], baseline, "baseline"), summary(halves[1], symptom, "symptom")...),
			Comparisons: []*investigationv1.Comparison{comparisonOf(statistic,
				value(halves[0].hits, halves[0].errors, halves[0].percentile, baseline),
				value(halves[1].hits, halves[1].errors, halves[1].percentile, symptom))},
		}},
		Coverage: coverage,
	}
	var outcome engine.Outcome = engine.DigestOutcome{Body: digest}
	if considered == 0 {
		outcome = engine.NoData{Coverage: coverage}
	}
	return answer{outcome: outcome, query: canonical(queries), vocabulary: feeder.VocabDatadogAPMMetric,
		deepLink: b.metricLink(queries[len(queries)-1], symptom)}, nil
}

// apmHalfOf runs the queries one window needs for the statistic. Counts are additive across versions,
// so the per-version query also yields the totals; a percentile is not, and gets its own unversioned
// query for the whole scope.
func (b *Backend) apmHalfOf(ctx context.Context, sel apmSelector, statistic investigationv1.Statistic, window *engine.Window) (apmHalf, *datadogx.Response, []string, error) {
	half := apmHalf{versions: map[string]*apmVersion{}, percentile: math.NaN()}
	from, to := window.GetStart().AsTime(), window.GetEnd().AsTime()
	var last *datadogx.Response
	var queries []string
	run := func(q string) (*datadogx.MetricsResponse, error) {
		queries = append(queries, q)
		out, resp, err := b.client.QueryMetrics(ctx, q, from, to)
		if resp != nil {
			last = resp
		}
		return out, err
	}
	version := func(series datadogx.MetricSeries) string { return tagValue(series.Scope, "version") }
	needErrors := statistic == investigationv1.Statistic_ERROR_RATE
	pattern, isPercentile := apmMetrics.percentile[statistic]

	if isPercentile {
		out, err := run(sel.metricQuery(pattern, "avg", false, false))
		if err != nil {
			return apmHalf{}, last, queries, err
		}
		half.percentile, _ = meanOf(out.Series)
	}
	hits, err := run(sel.metricQuery(apmMetrics.hits, "sum", true, true))
	if err != nil {
		return apmHalf{}, last, queries, err
	}
	for _, series := range hits.Series {
		v := half.version(version(series))
		sum, n := sumOf(series.Values())
		v.hits += sum
		v.points += n
	}
	if needErrors {
		errs, err := run(sel.metricQuery(apmMetrics.errors, "sum", true, true))
		if err != nil {
			return apmHalf{}, last, queries, err
		}
		for _, series := range errs.Series {
			sum, _ := sumOf(series.Values())
			half.version(version(series)).errors += sum
		}
	}
	if isPercentile {
		out, err := run(sel.metricQuery(pattern, "avg", false, true))
		if err != nil {
			return apmHalf{}, last, queries, err
		}
		for _, series := range out.Series {
			mean, _ := meanOf([]datadogx.MetricSeries{series})
			half.version(version(series)).percentile = mean
		}
	}
	// The totals are the sums of the versions, the unversioned group (a service that stamps none) included;
	// that group is part of them and is not a version.
	for _, v := range half.versions {
		half.hits += v.hits
		half.errors += v.errors
		half.points += v.points
	}
	if !isPercentile || math.IsNaN(half.percentile) {
		half.percentile = 0
	}
	delete(half.versions, "")
	half.trimVersions()
	return half, last, queries, nil
}

func (h *apmHalf) version(name string) *apmVersion {
	if v, ok := h.versions[name]; ok {
		return v
	}
	v := &apmVersion{}
	h.versions[name] = v
	return v
}

// trimVersions keeps the maxAPMVersions busiest versions, ties by name.
func (h *apmHalf) trimVersions() {
	if len(h.versions) <= maxAPMVersions {
		return
	}
	names := make([]string, 0, len(h.versions))
	for name := range h.versions {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		a, c := h.versions[names[i]], h.versions[names[j]]
		if a.hits != c.hits {
			return a.hits > c.hits
		}
		return names[i] < names[j]
	})
	for _, name := range names[maxAPMVersions:] {
		delete(h.versions, name)
	}
}

// tagValue reads one tag out of a series' scope, `env:production,service:checkout,version:v2`.
func tagValue(scope, key string) string {
	for _, tag := range strings.Split(scope, ",") {
		if k, v, ok := strings.Cut(strings.TrimSpace(tag), ":"); ok && k == key {
			return v
		}
	}
	return ""
}

func sumOf(values []float64) (float64, int) {
	var sum float64
	for _, v := range values {
		sum += v
	}
	return sum, len(values)
}

// meanOf is the mean of every non-null point of the series, and how many there were.
func meanOf(series []datadogx.MetricSeries) (float64, int) {
	var sum float64
	var n int
	for _, s := range series {
		for _, v := range s.Values() {
			sum += v
			n++
		}
	}
	if n == 0 {
		return math.NaN(), 0
	}
	return sum / float64(n), n
}

// metricLink opens the same metric query over the same window in Datadog for a person.
func (b *Backend) metricLink(query string, window *engine.Window) string {
	if b.site == "" {
		return ""
	}
	v := url.Values{}
	v.Set("query", query)
	v.Set("from_ts", strconv.FormatInt(window.GetStart().AsTime().UnixMilli(), 10))
	v.Set("to_ts", strconv.FormatInt(window.GetEnd().AsTime().UnixMilli(), 10))
	return "https://app." + b.site + "/metric/explorer?" + v.Encode()
}
