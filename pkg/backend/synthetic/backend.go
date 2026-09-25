// SPDX-License-Identifier: Apache-2.0

package synthetic

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/internal/investigation/workers/logs"
	"github.com/Pierre-Theophile/aisre/internal/investigation/workers/metrics"
	sdk "github.com/Pierre-Theophile/aisre/pkg/backend"
	"github.com/Pierre-Theophile/aisre/pkg/backend/onset"
)

// Backend is the synthetic telemetry backend. It serves all eight telemetry terms from the
// scenario it was built with, and touches no network.
type Backend struct {
	scenario Scenario
	redactor *engine.Redactor
}

// New returns the backend for a scenario. The redaction key is derived from the scenario seed so
// that a pseudonym is stable within one fixture and different between two — the same property a
// real keyed HMAC gives, without a secret a fixture would have to carry.
func New(scenario Scenario) (*Backend, error) {
	redactor, err := engine.NewRedactor(engine.DefaultRedactionPolicy(), []byte("synthetic:"+scenario.Seed))
	if err != nil {
		return nil, err
	}
	return &Backend{scenario: scenario, redactor: redactor}, nil
}

// Describe returns the backend's declaration: all eight telemetry terms, priced exactly as a
// live backend prices them, so a budget spent against a synthetic world is the budget that would
// be spent against a vendor.
func (b *Backend) Describe() sdk.Description {
	terms := sdk.Terms(sdk.FamilyTelemetry)
	costs := make(map[string]sdk.CostClass, len(terms))
	capabilities := make([]sdk.Capability, 0, len(terms))
	for _, term := range terms {
		class := engine.PublishedCostClass(term)
		costs[term] = class
		capabilities = append(capabilities, sdk.Capability{Name: term, ReadOnly: true, CostClass: class})
	}
	return sdk.Description{
		Name:           Vendor,
		Vendor:         Vendor,
		Terms:          terms,
		CostClasses:    costs,
		Capabilities:   capabilities,
		Redaction:      b.redactor.Policy(),
		Version:        Version,
		AlgebraVersion: engine.AlgebraVersion,
	}
}

// Execute answers one term.
func (b *Backend) Execute(ctx context.Context, req *engine.Request) (*engine.Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	name := engine.TermName(req.GetTerm())
	if name == "" || engine.FamilyOf(name) != sdk.FamilyTelemetry {
		return b.fail(req, investigationv1.FailureReason_OUTSIDE_ALGEBRA, engine.OutsideAlgebra(name).Error())
	}
	if err := engine.Validate(req.GetTerm()); err != nil {
		return b.fail(req, investigationv1.FailureReason_OUTSIDE_ALGEBRA, err.Error())
	}

	// Nobody sees the future (constitution II; internal/investigation/backend/horizon.go). The
	// generator can produce a point for any instant, which is exactly why it must not: a window
	// running past the investigation's observed_at is cut back to it before a single sample is
	// generated, and a window lying entirely past it is NO_DATA naming the horizon. The clamped
	// request is what the rest of this call sees, so the term key an answer carries is the key of
	// the window that was actually answerable — the same key the recorded backend looks up.
	clamped, horizon, horizonState := engine.ClampRequest(req)
	if horizonState == engine.PastHorizon {
		return b.pastHorizon(req, horizon)
	}
	req = clamped

	var (
		outcome engine.Outcome
		query   string
		err     error
	)
	switch t := req.GetTerm().GetTerm().(type) {
	case *investigationv1.AlgebraTerm_Compare:
		outcome, query, err = b.compare(t.Compare)
	case *investigationv1.AlgebraTerm_Onset:
		outcome, query, err = b.onset(t.Onset)
	case *investigationv1.AlgebraTerm_NewLogPatterns:
		outcome, query, err = b.newLogPatterns(t.NewLogPatterns)
	case *investigationv1.AlgebraTerm_ErrorSpans:
		outcome, query, err = b.errorSpans(t.ErrorSpans)
	case *investigationv1.AlgebraTerm_ErrorsByVersion:
		outcome, query, err = b.errorsByVersion(t.ErrorsByVersion)
	case *investigationv1.AlgebraTerm_MonitorState:
		outcome, query, err = b.monitorState(t.MonitorState)
	case *investigationv1.AlgebraTerm_Exemplars:
		outcome, query, err = b.exemplars(t.Exemplars)
	case *investigationv1.AlgebraTerm_DrillDown:
		outcome, query, err = b.drillDown(t.DrillDown)
	default:
		return b.fail(req, investigationv1.FailureReason_OUTSIDE_ALGEBRA, engine.OutsideAlgebra(name).Error())
	}
	if err != nil {
		return nil, err
	}

	resp, err := engine.NewResponse(engine.ResponseInput{
		Request:        req,
		Outcome:        outcome,
		Mode:           engine.ModeLive,
		CostClass:      engine.PublishedCostClass(name),
		BackendVersion: Version,
		Vocabulary:     "synthetic/1",
		ExecutedQuery:  query,
		DeepLink:       "", // there is no console to open; a real backend fills this in
	})
	if err != nil {
		return nil, err
	}
	// The truncation is written into the answer it applies to rather than reported beside it
	// (FR-037), before redaction and before the digest is taken over what leaves the process.
	engine.AnnotateHorizon(resp, horizon, horizonState)
	if err := b.redactor.Apply(resp); err != nil {
		return nil, err
	}
	// Redaction can change the bytes, so the response digest is recomputed over what actually
	// leaves the process rather than over what was built inside it.
	digest, err := sdk.ResponseDigest(resp)
	if err != nil {
		return nil, err
	}
	resp.ResponseDigest = digest
	return resp, nil
}

// pastHorizon answers a window lying entirely at or after the investigation's observed_at:
// NO_DATA naming the horizon. The generator does not generate it and then discard it — it never
// generates it, because a sample after the horizon is not a sample that was suppressed, it is a
// sample that does not exist.
func (b *Backend) pastHorizon(req *engine.Request, horizon time.Time) (*engine.Response, error) {
	window := &engine.Window{
		Start: timestamppb.New(horizon.UTC()),
		End:   timestamppb.New(horizon.UTC()),
	}
	coverage, err := engine.CoverageInput{
		DataSource:               "synthetic",
		WindowCovered:            window,
		Sampling:                 "none",
		Truncation:               engine.HorizonReason(horizon, engine.PastHorizon),
		IngestionLag:             b.scenario.IngestionLag,
		IngestionLagUndetermined: b.scenario.IngestionLag == 0,
		ExecutedAt:               horizon.UTC(),
		QuotaUndetermined:        true,
	}.Coverage()
	if err != nil {
		return nil, err
	}
	engine.AnnotateCoverageHorizon(coverage, horizon, engine.PastHorizon)
	return engine.NewResponse(engine.ResponseInput{
		Request: req,
		Outcome: engine.NoData{
			Coverage: coverage,
			AbsentSource: "telemetry after the investigation's observed_at " +
				horizon.UTC().Format(time.RFC3339) + "; it does not exist at the instant this investigation observes the world",
		},
		Mode:           engine.ModeLive,
		CostClass:      engine.PublishedCostClass(engine.TermName(req.GetTerm())),
		BackendVersion: Version,
		Vocabulary:     "synthetic/1",
		ExecutedQuery:  "horizon:" + horizon.UTC().Format(time.RFC3339),
	})
}

func (b *Backend) fail(req *engine.Request, reason investigationv1.FailureReason, detail string) (*engine.Response, error) {
	return engine.NewResponse(engine.ResponseInput{
		Request:        req,
		Outcome:        engine.QueryFailed{Reason: reason, Detail: detail},
		Mode:           engine.ModeLive,
		BackendVersion: Version,
	})
}

// coverage builds the mandatory block. Everything the generator cannot know — a vendor quota it
// does not have — is stated as undetermined rather than omitted.
func (b *Backend) coverage(entities []string, source string, window *engine.Window, volume int64) (*investigationv1.Coverage, error) {
	return engine.CoverageInput{
		SearchedEntities:         entities,
		DataSource:               source,
		WindowCovered:            window,
		VolumeConsidered:         volume,
		Sampling:                 "none",
		IngestionLag:             b.scenario.IngestionLag,
		ExecutedAt:               b.scenario.End.UTC(),
		QuotaUndetermined:        true,
		IngestionLagUndetermined: b.scenario.IngestionLag == 0,
	}.Coverage()
}

// absent is the answer for a selector the fixture's graph never published: NO_DATA naming the
// absent source. It is the same answer a real backend gives when the organisation has no such
// data, and it is stable for the whole window, so a consumer concludes "this cannot be checked
// here" instead of retrying.
func (b *Backend) absent(selector, source string, window *engine.Window) (engine.Outcome, string, error) {
	coverage, err := b.coverage(nil, source, window, 0)
	if err != nil {
		return nil, "", err
	}
	return engine.NoData{
		Coverage:     coverage,
		AbsentSource: fmt.Sprintf("%s series for selector %q", source, selector),
	}, "absent:" + selector, nil
}

// ---- compare -------------------------------------------------------------------------------

func (b *Backend) compare(term *investigationv1.CompareTerm) (engine.Outcome, string, error) {
	selector := term.GetPointer().GetSelector()
	entity, ok := b.scenario.entityOfSelector(selector)
	if !ok {
		return b.absent(selector, "metrics", term.GetWindows().GetSymptom())
	}
	statistic := term.GetStatistic()
	baseline := b.series(entity, selector, term.GetWindows().GetBaseline(), statistic)
	symptom := b.series(entity, selector, term.GetWindows().GetSymptom(), statistic)

	baselineValue := aggregate(baseline, statistic)
	symptomValue := aggregate(symptom, statistic)
	comparison := comparisonOf(statistic, baselineValue, symptomValue)

	coverage, err := b.coverage([]string{entity.EntityID}, "metrics", term.GetWindows().GetSymptom(),
		int64(len(baseline)+len(symptom)))
	if err != nil {
		return nil, "", err
	}
	key := termKeyOfCompare(term)
	digest := &investigationv1.Digest{
		Body: &investigationv1.Digest_Metric{Metric: &investigationv1.MetricDigest{
			Series: []*investigationv1.SeriesSummary{
				b.summary(entity, selector, term.GetWindows().GetBaseline(), baseline, statistic, key, "baseline"),
				b.summary(entity, selector, term.GetWindows().GetSymptom(), symptom, statistic, key, "symptom"),
			},
			Comparisons: []*investigationv1.Comparison{comparison},
		}},
		Coverage: coverage,
	}
	return engine.DigestOutcome{Body: digest}, fmt.Sprintf("compare(%s, %s)", selector, statistic), nil
}

func (b *Backend) summary(entity Entity, selector string, window *engine.Window, points []onset.Point,
	statistic investigationv1.Statistic, mintedBy, group string,
) *investigationv1.SeriesSummary {
	stats := map[string]float64{statistic.String(): aggregate(points, statistic)}
	for _, also := range []investigationv1.Statistic{
		investigationv1.Statistic_MEAN, investigationv1.Statistic_MAX,
	} {
		stats[also.String()] = aggregate(points, also)
	}
	return &investigationv1.SeriesSummary{
		Tags: map[string]string{
			"service": entity.Name,
			"window":  group,
		},
		PointCount:        int64(len(points)),
		IntervalCovered:   window,
		ResolutionSeconds: int64(b.scenario.resolution() / time.Second),
		Statistics:        stats,
		JoinKeys:          b.joinKeys(entity, window),
		DrillDown:         b.handle(mintedBy, handleMetric, selector, window, group),
	}
}

// series generates the samples for one selector over one window. Values are a pure function of
// (seed, selector, instant) and of the changes the graph holds, so the same fixture always
// produces the same series.
func (b *Backend) series(entity Entity, selector string, window *engine.Window, statistic investigationv1.Statistic) []onset.Point {
	if window.GetStart() == nil || window.GetEnd() == nil {
		return nil
	}
	start := window.GetStart().AsTime().UTC()
	end := window.GetEnd().AsTime().UTC()
	step := b.scenario.resolution()

	errorBase := round6(baselineErrorRateFloor + baselineErrorRateSpan*unitOf(b.scenario.Seed, selector, "error"))
	latencyBase := round6(baselineLatencyFloorMS + baselineLatencySpanMS*unitOf(b.scenario.Seed, selector, "latency"))
	rateBase := round6(baselineRateFloor + baselineRateSpan*unitOf(b.scenario.Seed, selector, "rate"))

	points := make([]onset.Point, 0, 128)
	for at := start; at.Before(end); at = at.Add(step) {
		wobble := wobbleOf(b.scenario.Seed, selector, at)
		degradation := b.scenario.effectiveDegradationAt(entity, at)
		var value float64
		switch statistic {
		case investigationv1.Statistic_ERROR_RATE:
			value = round6(errorBase * degradation * round6(1+wobble))
		case investigationv1.Statistic_COUNT:
			value = round6(round6(round6(rateBase*round6(1+wobble))*b.scenario.diurnalAt(at)) * float64(step/time.Second))
		case investigationv1.Statistic_RATE:
			value = round6(round6(rateBase*round6(1+wobble)) * b.scenario.diurnalAt(at))
		case investigationv1.Statistic_P95:
			value = round6(latencyBase * 2.1 * round6(1+wobble) * latencyFactor(degradation))
		case investigationv1.Statistic_P99:
			value = round6(latencyBase * 3.4 * round6(1+wobble) * latencyFactor(degradation))
		case investigationv1.Statistic_MAX:
			value = round6(latencyBase * 4.8 * round6(1+wobble) * latencyFactor(degradation))
		default: // P50, MEAN and the unspecified default are the central latency
			value = round6(latencyBase * round6(1+wobble) * latencyFactor(degradation))
		}
		points = append(points, onset.Point{At: at, Value: value})
	}
	return points
}

// latencyFactor turns an error-rate degradation into the smaller latency degradation that
// accompanies it. A regression that multiplies errors by 24 does not multiply latency by 24.
func latencyFactor(degradation float64) float64 {
	if degradation <= 1 {
		return 1
	}
	return round6(1 + (degradation-1)*0.08)
}

func aggregate(points []onset.Point, statistic investigationv1.Statistic) float64 {
	if len(points) == 0 {
		return 0
	}
	switch statistic {
	case investigationv1.Statistic_COUNT:
		var total float64
		for _, pt := range points {
			total = round6(total + pt.Value)
		}
		return total
	case investigationv1.Statistic_MAX:
		best := points[0].Value
		for _, pt := range points {
			if pt.Value > best {
				best = pt.Value
			}
		}
		return best
	case investigationv1.Statistic_P50:
		return quantile(points, 0.50)
	case investigationv1.Statistic_P95:
		return quantile(points, 0.95)
	case investigationv1.Statistic_P99:
		return quantile(points, 0.99)
	default:
		var total float64
		for _, pt := range points {
			total = round6(total + pt.Value)
		}
		return round6(total / float64(len(points)))
	}
}

// quantile is the nearest-rank quantile: no interpolation, so the answer is always a value that
// was actually observed and the arithmetic cannot diverge between architectures.
func quantile(points []onset.Point, q float64) float64 {
	values := make([]float64, 0, len(points))
	for _, pt := range points {
		values = append(values, pt.Value)
	}
	sort.Float64s(values)
	rank := int(round6(q*float64(len(values)-1)) + 0.5)
	if rank < 0 {
		rank = 0
	}
	if rank >= len(values) {
		rank = len(values) - 1
	}
	return values[rank]
}

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
		// A comparison separates a hypothesis when the two windows differ by more than the
		// generator's own wobble could account for, and the published rule for "more than" is
		// the metrics worker's, applied here so that the generator and the worker that reads it
		// never state two different things about the same comparison. `direction` is a coarser
		// description of the same number and is not that rule: a gentle ramp is "up" and still
		// does not discriminate, which is the shape slow-burn-01 records.
		Separable: metrics.Separates(relative),
	}
}

func (b *Backend) joinKeys(entity Entity, window *engine.Window) *investigationv1.JoinKeys {
	at := b.scenario.End
	if window.GetEnd() != nil {
		at = window.GetEnd().AsTime().UTC()
	}
	return &investigationv1.JoinKeys{
		Version:   b.scenario.versionAt(entity, at),
		Workload:  entity.Name,
		PodOrHost: entity.Name + "-" + shortOf(unitOf(b.scenario.Seed, entity.EntityID, "pod")),
		FirstSeen: timestamppb.New(b.scenario.Start.UTC()),
	}
}

func shortOf(unit float64) string {
	return fmt.Sprintf("%06d", int(unit*1_000_000))
}

// ---- onset ---------------------------------------------------------------------------------

func (b *Backend) onset(term *investigationv1.OnsetTerm) (engine.Outcome, string, error) {
	selector := term.GetPointer().GetSelector()
	entity, ok := b.scenario.entityOfSelector(selector)
	if !ok {
		return b.absent(selector, "metrics", term.GetSearchWindow())
	}
	window := term.GetSearchWindow()
	series := b.series(entity, selector, window, investigationv1.Statistic_ERROR_RATE)

	estimate := onset.EstimateOnset(series, onset.Window{
		Start: window.GetStart().AsTime().UTC(),
		End:   window.GetEnd().AsTime().UTC(),
	}, onset.Params{})

	coverage, err := b.coverage([]string{entity.EntityID}, "metrics", window, int64(len(series)))
	if err != nil {
		return nil, "", err
	}
	digest := &investigationv1.OnsetDigest{
		Method:           investigationv1.OnsetMethod_SEASONAL_CUSUM,
		MethodParameters: estimate.Params.Map(),
		Examined: &engine.Window{
			Start: timestamppb.New(estimate.Examined.Start),
			End:   timestamppb.New(estimate.Examined.End),
		},
	}
	if estimate.Unavailable {
		digest.Unavailable = true
		digest.UnavailableReason = estimate.Reason
	} else {
		digest.EstimatedOnset = timestamppb.New(estimate.At)
		digest.UncertaintySeconds = estimate.UncertaintySeconds
	}
	return engine.DigestOutcome{Body: &investigationv1.Digest{
		Body:     &investigationv1.Digest_Onset{Onset: digest},
		Coverage: coverage,
	}}, "onset(" + selector + ")", nil
}

// ---- new_log_patterns ----------------------------------------------------------------------

func (b *Backend) newLogPatterns(term *investigationv1.NewLogPatternsTerm) (engine.Outcome, string, error) {
	selector := term.GetPointer().GetSelector()
	entity, ok := b.scenario.entityOfSelector(selector)
	if !ok {
		return b.absent(selector, "logs", term.GetWindow())
	}
	windowLines := b.lines(entity, selector, term.GetWindow())
	baselineLines := b.lines(entity, selector, term.GetBaselineWindow())

	templates := logs.Diff(logs.Mine(windowLines), logs.Mine(baselineLines))
	key := termKeyOfLogPatterns(term)

	counts := make(map[string]int64, 3)
	patterns := make([]*investigationv1.LogPattern, 0, len(templates))
	for _, t := range templates {
		counts[t.Status] += int64(t.Count)
		patterns = append(patterns, &investigationv1.LogPattern{
			Template:      t.Text,
			Count:         int64(t.Count),
			BaselineCount: int64(t.BaselineCount),
			NewInWindow:   t.NewInWindow,
			Status:        t.Status,
			JoinKeys:      b.joinKeys(entity, term.GetWindow()),
			DrillDown:     b.handle(key, handleLog, selector, term.GetWindow(), t.Text),
		})
	}

	coverage, err := b.coverage([]string{entity.EntityID}, "logs", term.GetWindow(),
		int64(len(windowLines)+len(baselineLines)))
	if err != nil {
		return nil, "", err
	}
	return engine.DigestOutcome{Body: &investigationv1.Digest{
		Body: &investigationv1.Digest_Log{Log: &investigationv1.LogDigest{
			Patterns:       patterns,
			CountsByStatus: counts,
			MinerVersion:   logs.MinerVersion,
			ModelUsed:      false,
		}},
		Coverage: coverage,
	}}, "new_log_patterns(" + selector + ")", nil
}

// lines generates the log lines for one selector over one window: a steady background of two
// templates, plus a third that appears only once a degrading change has landed. That third
// template is the lead a log worker is supposed to surface, and it exists in the generated data
// because it exists in the graph.
func (b *Backend) lines(entity Entity, selector string, window *engine.Window) []logs.Line {
	if window.GetStart() == nil || window.GetEnd() == nil {
		return nil
	}
	start := window.GetStart().AsTime().UTC()
	end := window.GetEnd().AsTime().UTC()
	step := b.scenario.resolution()

	out := make([]logs.Line, 0, 256)
	for at := start; at.Before(end); at = at.Add(step) {
		version := b.scenario.versionAt(entity, at)
		pod := entity.Name + "-" + shortOf(unitOf(b.scenario.Seed, entity.EntityID, "pod"))
		out = append(out,
			logs.Line{
				At:        at,
				Text:      fmt.Sprintf("GET /%s/health 200 in %dms", entity.Name, 3+int(unitOf(b.scenario.Seed, selector, at.Format(time.RFC3339))*5)),
				Status:    "info",
				PodOrHost: pod,
				Version:   version,
			},
			logs.Line{
				At:        at,
				Text:      fmt.Sprintf("handled request id=%d for %s", 100000+at.Unix()%900000, entity.Name),
				Status:    "info",
				PodOrHost: pod,
				Version:   version,
			},
		)
		if b.scenario.effectiveDegradationAt(entity, at) > 1 {
			out = append(out, logs.Line{
				At:        at,
				Text:      fmt.Sprintf("upstream call failed: deadline exceeded after %dms (service=%s)", 250+at.Unix()%50, entity.Name),
				Status:    "error",
				PodOrHost: pod,
				Version:   version,
			})
		}
	}
	return out
}

// ---- error_spans ---------------------------------------------------------------------------

func (b *Backend) errorSpans(term *investigationv1.ErrorSpansTerm) (engine.Outcome, string, error) {
	src, srcOK := b.scenario.entityByID(term.GetSrcEntityId())
	dst, dstOK := b.scenario.entityByID(term.GetDstEntityId())
	window := term.GetWindow()
	if !srcOK || !dstOK {
		coverage, err := b.coverage(nil, "traces", window, 0)
		if err != nil {
			return nil, "", err
		}
		return engine.NoData{
			Coverage: coverage,
			AbsentSource: fmt.Sprintf("trace data for the edge %s → %s, which this graph does not hold",
				term.GetSrcEntityId(), term.GetDstEntityId()),
		}, "error_spans(unknown edge)", nil
	}

	weight := b.weightOf(src.EntityID, dst.EntityID)
	total, errored := b.spanCounts(dst, window, weight)
	key := termKeyOfErrorSpans(term)

	groups := []*investigationv1.SpanGroup{
		{
			Operation: dst.Name + ".handle",
			ErrorKind: "",
			Count:     total - errored,
			Latency:   b.latencyStats(dst, window, false),
			JoinKeys:  b.joinKeys(dst, window),
			DrillDown: b.handle(key, handleSpan, dst.Name, window, "ok"),
		},
	}
	if errored > 0 {
		groups = append(groups, &investigationv1.SpanGroup{
			Operation: dst.Name + ".handle",
			ErrorKind: "deadline_exceeded",
			Count:     errored,
			Latency:   b.latencyStats(dst, window, true),
			JoinKeys:  b.joinKeys(dst, window),
			DrillDown: b.handle(key, handleSpan, dst.Name, window, "deadline_exceeded"),
		})
	}

	coverage, err := b.coverage([]string{src.EntityID, dst.EntityID}, "traces", window, total)
	if err != nil {
		return nil, "", err
	}
	return engine.DigestOutcome{Body: &investigationv1.Digest{
		Body:     &investigationv1.Digest_Trace{Trace: &investigationv1.TraceDigest{Groups: groups}},
		Coverage: coverage,
	}}, fmt.Sprintf("error_spans(%s -> %s)", src.Name, dst.Name), nil
}

func (b *Backend) weightOf(srcID, dstID string) uint32 {
	for _, edge := range b.scenario.Edges {
		if edge.SrcEntityID == srcID && edge.DstEntityID == dstID {
			if edge.WeightClass == 0 {
				return 3
			}
			return edge.WeightClass
		}
	}
	return 5
}

// spanCounts is the number of spans on an edge over a window and how many of them failed. The
// count scales with the edge's weight class, so a heavy path produces more spans than a light
// one — which is what makes a trace digest on the heavy path worth more than one on the light.
func (b *Backend) spanCounts(dst Entity, window *engine.Window, weight uint32) (total, errored int64) {
	if window.GetStart() == nil || window.GetEnd() == nil {
		return 0, 0
	}
	minutes := int64(window.GetEnd().AsTime().Sub(window.GetStart().AsTime()) / time.Minute)
	if minutes < 1 {
		minutes = 1
	}
	perMinute := 600 / int64(weight)
	total = minutes * perMinute

	// The failing minutes and the mean factor over them. A step to the package factor gives a
	// mean of exactly degradationFactor, which is what this loop used to assume outright; a ramp
	// or a load-coupled ceiling gives the factor it actually reached, so a partial degradation
	// produces partially failing spans rather than fully failing ones.
	var failing int64
	var factorTotal float64
	start := window.GetStart().AsTime().UTC()
	end := window.GetEnd().AsTime().UTC()
	for at := start; at.Before(end); at = at.Add(time.Minute) {
		if factor := b.scenario.effectiveDegradationAt(dst, at); factor > 1 {
			failing++
			factorTotal = round6(factorTotal + factor)
		}
	}
	meanFactor := float64(degradationFactor)
	if failing > 0 {
		meanFactor = round6(factorTotal / float64(failing))
	}
	base := round6(baselineErrorRateFloor + baselineErrorRateSpan*unitOf(b.scenario.Seed, dst.EntityID, "error"))
	errored = int64(round6(float64(total-failing*perMinute)*base)) +
		int64(round6(float64(failing*perMinute)*round6(base*meanFactor)))
	if errored > total {
		errored = total
	}
	return total, errored
}

func (b *Backend) latencyStats(dst Entity, window *engine.Window, failing bool) map[string]float64 {
	selector := dst.Name
	points := b.series(dst, firstSelector(dst, selector), window, investigationv1.Statistic_P50)
	stats := map[string]float64{
		"P50": aggregate(points, investigationv1.Statistic_P50),
		"P95": aggregate(b.series(dst, firstSelector(dst, selector), window, investigationv1.Statistic_P95), investigationv1.Statistic_P95),
		"P99": aggregate(b.series(dst, firstSelector(dst, selector), window, investigationv1.Statistic_P99), investigationv1.Statistic_P99),
	}
	if failing {
		// A failing span is a span that waited for a deadline, so its latency is the deadline
		// rather than the service's own distribution.
		for key := range stats {
			stats[key] = round6(stats[key] + 250)
		}
	}
	return stats
}

func firstSelector(entity Entity, fallback string) string {
	if len(entity.Selectors) > 0 {
		return entity.Selectors[0]
	}
	return fallback
}

// ---- errors_by_version ---------------------------------------------------------------------

func (b *Backend) errorsByVersion(term *investigationv1.ErrorsByVersionTerm) (engine.Outcome, string, error) {
	selector := term.GetPointer().GetSelector()
	entity, ok := b.scenario.entityOfSelector(selector)
	if !ok {
		return b.absent(selector, "metrics", term.GetWindow())
	}
	window := term.GetWindow()
	step := b.scenario.resolution()
	base := round6(baselineErrorRateFloor + baselineErrorRateSpan*unitOf(b.scenario.Seed, selector, "error"))

	type accumulator struct {
		total, errors float64
		firstSeen     time.Time
	}
	byVersion := map[string]*accumulator{}
	var order []string
	for at := window.GetStart().AsTime().UTC(); at.Before(window.GetEnd().AsTime().UTC()); at = at.Add(step) {
		version := b.scenario.versionAt(entity, at)
		acc, ok := byVersion[version]
		if !ok {
			acc = &accumulator{firstSeen: at}
			byVersion[version] = acc
			order = append(order, version)
		}
		requests := round6(baselineRateFloor + baselineRateSpan*unitOf(b.scenario.Seed, selector, "rate"))
		acc.total = round6(acc.total + requests)
		acc.errors = round6(acc.errors + round6(requests*round6(base*b.scenario.effectiveDegradationAt(entity, at))))
	}

	sort.Strings(order)
	key := termKeyOfErrorsByVersion(term)
	versions := make([]*investigationv1.VersionBreakdown, 0, len(order))
	for _, version := range order {
		acc := byVersion[version]
		rate := 0.0
		if acc.total > 0 {
			rate = round6(acc.errors / acc.total)
		}
		keys := b.joinKeys(entity, window)
		keys.Version = version
		keys.FirstSeen = timestamppb.New(acc.firstSeen)
		versions = append(versions, &investigationv1.VersionBreakdown{
			Version:   version,
			Errors:    int64(acc.errors),
			Total:     int64(acc.total),
			ErrorRate: rate,
			JoinKeys:  keys,
			DrillDown: b.handle(key, handleMetric, selector, window, version),
		})
	}

	coverage, err := b.coverage([]string{entity.EntityID}, "metrics", window, int64(len(order)))
	if err != nil {
		return nil, "", err
	}
	return engine.DigestOutcome{Body: &investigationv1.Digest{
		Body: &investigationv1.Digest_ErrorsByVersion{ErrorsByVersion: &investigationv1.ErrorsByVersionDigest{
			Versions:         versions,
			VersionAttribute: term.GetVersionAttribute(),
		}},
		Coverage: coverage,
	}}, "errors_by_version(" + selector + ")", nil
}

// ---- monitor_state -------------------------------------------------------------------------

func (b *Backend) monitorState(term *investigationv1.MonitorStateTerm) (engine.Outcome, string, error) {
	selector := term.GetPointer().GetSelector()
	entity, ok := b.scenario.entityOfSelector(selector)
	if !ok {
		return b.absent(selector, "monitors", term.GetWindow())
	}
	window := term.GetWindow()
	step := b.scenario.resolution()

	transitions := make([]*investigationv1.MonitorTransition, 0, 4)
	state := "ok"
	startState := ""
	for at := window.GetStart().AsTime().UTC(); at.Before(window.GetEnd().AsTime().UTC()); at = at.Add(step) {
		want := "ok"
		if b.scenario.effectiveDegradationAt(entity, at) > 1 {
			want = "alert"
		}
		if startState == "" {
			startState = want
			state = want
			continue
		}
		if want != state {
			transitions = append(transitions, &investigationv1.MonitorTransition{
				At:        timestamppb.New(at),
				FromState: state,
				ToState:   want,
				GroupKey:  "service:" + entity.Name,
			})
			state = want
		}
	}

	coverage, err := b.coverage([]string{entity.EntityID}, "monitors", window, int64(len(transitions)))
	if err != nil {
		return nil, "", err
	}
	return engine.DigestOutcome{Body: &investigationv1.Digest{
		Body: &investigationv1.Digest_MonitorState{MonitorState: &investigationv1.MonitorStateDigest{
			Transitions:   transitions,
			StateAtStart:  startState,
			StateAtEnd:    state,
			PerGroupState: map[string]string{"service:" + entity.Name: state},
		}},
		Coverage: coverage,
	}}, "monitor_state(" + selector + ")", nil
}

// ---- exemplars and drill_down ---------------------------------------------------------------

func (b *Backend) exemplars(term *investigationv1.ExemplarsTerm) (engine.Outcome, string, error) {
	payload, err := parseHandle(term.GetHandle())
	if err != nil {
		return engine.QueryFailed{
			Reason: investigationv1.FailureReason_UNSUPPORTED_POINTER,
			Detail: err.Error(),
		}, "exemplars(bad handle)", nil
	}
	if payload.Kind != handleLog {
		// An exemplar is a bounded, sanitised *log line*. The exemplar of a metric series is a
		// sample and the exemplar of a span group is a span payload, and neither may cross the
		// digest boundary in any quantity (constitution IV, contracts/telemetry-backend.md §3).
		// So this is a refusal with a reason rather than a smaller violation.
		return engine.QueryFailed{
			Reason: investigationv1.FailureReason_UNSUPPORTED_POINTER,
			Detail: "exemplars are bounded, sanitised log lines; handle " + term.GetHandle().GetValue() +
				" was minted by a " + payload.Kind + " answer, whose exemplars would be raw samples or span payloads — and those never cross the digest boundary",
		}, "exemplars(" + payload.Kind + " handle)", nil
	}
	entity, ok := b.scenario.entityOfSelector(payload.Selector)
	if !ok {
		entity = Entity{EntityID: payload.Selector, Name: payload.Selector}
	}
	window := payload.window()
	limit := int(term.GetLimit())
	if limit <= 0 || limit > engine.MaxExemplars {
		limit = engine.MaxExemplars
	}

	lines := b.lines(entity, payload.Selector, window)
	exemplars := make([]*investigationv1.Exemplar, 0, limit)
	// Redaction reduces a log line to its masked template before it leaves the process, so two
	// lines that mask to the same text and carry the same join keys are the same exemplar. They
	// are de-duplicated here rather than returned ten times: repeating one sanitised line adds
	// nothing a reader can use and costs the response's whole size budget.
	seen := make(map[string]struct{}, limit)
	for _, line := range lines {
		masked := engine.MaskLine(line.Text)
		if payload.Group != "" && masked != payload.Group {
			continue
		}
		keys := b.joinKeys(entity, window)
		fingerprint := masked + "\x00" + keys.GetPodOrHost() + "\x00" + keys.GetVersion()
		if _, dup := seen[fingerprint]; dup {
			continue
		}
		seen[fingerprint] = struct{}{}
		exemplars = append(exemplars, &investigationv1.Exemplar{Text: line.Text, JoinKeys: keys})
		if len(exemplars) >= limit {
			break
		}
	}

	coverage, err := b.coverage([]string{entity.EntityID}, "logs", window, int64(len(lines)))
	if err != nil {
		return nil, "", err
	}
	return engine.DigestOutcome{Body: &investigationv1.Digest{
		Body: &investigationv1.Digest_Exemplars{Exemplars: &investigationv1.ExemplarDigest{
			Exemplars: exemplars,
			Cap:       uint32(limit),
		}},
		Coverage: coverage,
	}}, "exemplars(" + payload.Group + ")", nil
}

// drillDown answers the narrower question behind a handle: the same family, split by instance.
// Depth is carried on the handle and checked, because a world records depth 1 and a handle minted
// by a depth-1 answer is exactly what "depth 2 is not_recorded by design" means.
func (b *Backend) drillDown(term *investigationv1.DrillDownTerm) (engine.Outcome, string, error) {
	payload, err := parseHandle(term.GetHandle())
	if err != nil {
		return engine.QueryFailed{
			Reason: investigationv1.FailureReason_UNSUPPORTED_POINTER,
			Detail: err.Error(),
		}, "drill_down(bad handle)", nil
	}
	entity, ok := b.scenario.entityOfSelector(payload.Selector)
	if !ok {
		return b.absent(payload.Selector, "metrics", payload.window())
	}
	window := payload.window()

	// One row per instance. The generator runs a fixed, small replica set so that the narrower
	// answer is genuinely narrower rather than a copy of the wider one.
	series := make([]*investigationv1.SeriesSummary, 0, 3)
	for replica := range 3 {
		instance := fmt.Sprintf("%s-%d", entity.Name, replica)
		points := b.series(entity, payload.Selector+"/"+instance, window, investigationv1.Statistic_ERROR_RATE)
		keys := b.joinKeys(entity, window)
		keys.PodOrHost = instance
		series = append(series, &investigationv1.SeriesSummary{
			Tags:              map[string]string{"service": entity.Name, "pod": instance, "group": payload.Group},
			PointCount:        int64(len(points)),
			IntervalCovered:   window,
			ResolutionSeconds: int64(b.scenario.resolution() / time.Second),
			Statistics: map[string]float64{
				investigationv1.Statistic_ERROR_RATE.String(): aggregate(points, investigationv1.Statistic_ERROR_RATE),
			},
			JoinKeys: keys,
			// No handle: this answer is at the world's recorded depth, and a handle it minted
			// would be a depth-2 question the world does not hold. Minting one would invite a
			// caller into a NOT_RECORDED it could not have predicted.
		})
	}

	coverage, err := b.coverage([]string{entity.EntityID}, "metrics", window, int64(len(series)))
	if err != nil {
		return nil, "", err
	}
	return engine.DigestOutcome{Body: &investigationv1.Digest{
		Body:     &investigationv1.Digest_Metric{Metric: &investigationv1.MetricDigest{Series: series}},
		Coverage: coverage,
	}}, "drill_down(" + payload.Group + ")", nil
}

// ---- handles -------------------------------------------------------------------------------

// The handle kinds. A handle carries what the narrower question is about, so that presenting it
// back is a complete question and the caller never has to compose a selector.
const (
	handleMetric = "metric"
	handleLog    = "log"
	handleSpan   = "span"
)

type handlePayload struct {
	Kind     string `json:"k"`
	Selector string `json:"s"`
	Start    int64  `json:"a"`
	End      int64  `json:"b"`
	Group    string `json:"g"`
}

func (p handlePayload) window() *engine.Window {
	return &engine.Window{
		Start: timestamppb.New(time.Unix(p.Start, 0).UTC()),
		End:   timestamppb.New(time.Unix(p.End, 0).UTC()),
	}
}

// handle mints a drill-down handle. The value is a deterministic encoding of the narrower
// question, opaque to the caller and decodable by the backend: that is what keeps the argument
// space of drill_down and exemplars finite and therefore a recorded world finite.
func (b *Backend) handle(mintedBy, kind, selector string, window *engine.Window, group string) *investigationv1.DrillDown {
	payload := handlePayload{
		Kind:     kind,
		Selector: selector,
		Start:    window.GetStart().AsTime().Unix(),
		End:      window.GetEnd().AsTime().Unix(),
		Group:    group,
	}
	encoded, err := json.Marshal(payload)
	if err != nil { // unreachable: the payload is four strings and two integers
		return nil
	}
	return &investigationv1.DrillDown{
		Handle: &investigationv1.Handle{
			Value:           base64.RawURLEncoding.EncodeToString(encoded),
			MintedByTermKey: mintedBy,
			Depth:           1,
		},
		// A synthetic backend has no console to open. A real backend puts the vendor URL here,
		// and the absence is stated rather than faked.
		HumanLink: "",
	}
}

func parseHandle(h *investigationv1.Handle) (handlePayload, error) {
	var payload handlePayload
	raw, err := base64.RawURLEncoding.DecodeString(h.GetValue())
	if err != nil {
		return payload, fmt.Errorf("handle %q was not minted by this backend; a handle is presented back, never composed", h.GetValue())
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return payload, fmt.Errorf("handle %q does not decode to a question", h.GetValue())
	}
	if payload.Selector == "" || payload.End <= payload.Start {
		return payload, fmt.Errorf("handle %q decodes to an incomplete question", h.GetValue())
	}
	return payload, nil
}

// termKeyOf* compute the key of the term an answer is minting handles from, so that a handle
// records which answer minted it.
func termKeyOfCompare(term *investigationv1.CompareTerm) string {
	key, _ := engine.TermKey(engine.Compare(term.GetPointer(), term.GetWindows(), term.GetStatistic()))
	return key
}

func termKeyOfLogPatterns(term *investigationv1.NewLogPatternsTerm) string {
	key, _ := engine.TermKey(engine.NewLogPatterns(term.GetPointer(), term.GetWindow(), term.GetBaselineWindow()))
	return key
}

func termKeyOfErrorSpans(term *investigationv1.ErrorSpansTerm) string {
	key, _ := engine.TermKey(engine.ErrorSpans(term.GetSrcEntityId(), term.GetDstEntityId(), term.GetEdgeType(), term.GetWindow()))
	return key
}

func termKeyOfErrorsByVersion(term *investigationv1.ErrorsByVersionTerm) string {
	key, _ := engine.TermKey(engine.ErrorsByVersion(term.GetPointer(), term.GetWindow(), term.GetVersionAttribute()))
	return key
}
