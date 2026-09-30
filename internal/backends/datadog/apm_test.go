// SPDX-License-Identifier: Apache-2.0

package datadog_test

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	ddbackend "github.com/Pierre-Theophile/aisre/internal/backends/datadog"
	ddfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/datadog"
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	sdk "github.com/Pierre-Theophile/aisre/pkg/backend"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// datadog-backend-apm-01: the Datadog backend's span and APM metric terms over a synthetic structural
// twin (T090; FR-040b, FR-049b, SC-019, SC-023, SC-024).
//
// The same story as datadog-backend-logs-01, seen from the tracing side. `checkout` calls `payments`; at
// 14:10 a new version ships and the calls start failing with a timeout, slowly. Datadog retains two of
// every three spans and counts every one in its trace metrics, so the two surfaces disagree on the count
// by design, and the coverage of each says which one answered.
//
// The world is the cross product of the terms the capability adds — compare over an APM metric pointer
// for every statistic it states, error_spans over the edge and over the empty reverse edge — plus the two
// log answers that must not move (a log compare, and the latency percentile a log pointer still refuses).
// Every run asserts that the recorded world answers each question exactly as the live twin does, and that
// with apm_topology off the same twin is never asked for a span or a metric.
//
// Built against the published API shapes and not verified against a live organisation.
// Synthetic structural twin: no identifier is derived from any organisation.

const (
	apmFixture     = "fixtures/datadog-backend-apm-01"
	apmSpanName    = "http.request"
	apmSourceEdge  = "checkout"
	apmTargetEdge  = "payments"
	apmFailureKind = "PaymentProviderTimeout"
)

// apmSpans is the twin's traffic: four calls a minute from checkout to payments, the same route and a
// status route, on the old version until the deploy and the new one after it.
func apmSpans() []twinSpan {
	var spans []twinSpan
	for m := backendFrom; m.Before(backendTo); m = m.Add(time.Minute) {
		version := backendOldCommit
		if !m.Before(backendDeploy) {
			version = backendNewCommit
		}
		for i := range 4 {
			s := twinSpan{at: m.Add(time.Duration(i*10) * time.Second), service: apmSourceEdge, peer: apmTargetEdge,
				resource: "POST /charge", status: "ok", version: version,
				duration: time.Duration(70+(m.Minute()+i)%20) * time.Millisecond,
				retained: (m.Minute()+i)%3 != 0}
			switch {
			case i == 3:
				s.resource, s.duration = "GET /charge/status", 30*time.Millisecond
			case !m.Before(backendDeploy) && i%2 == 0:
				s.status, s.errType = "error", apmFailureKind
				s.duration = time.Duration(2400+(m.Minute()*7+i)%600) * time.Millisecond
			}
			spans = append(spans, s)
		}
	}
	return spans
}

func apmPointer() *graphv1.Pointer {
	return &graphv1.Pointer{
		Kind: graphv1.PointerKind_METRIC, BackendKind: "datadog", Vocabulary: feeder.VocabDatadogAPMMetric,
		Selector: "service:checkout env:production span:" + apmSpanName,
		Attributes: map[string]string{feeder.AttrServiceName: apmSourceEdge,
			feeder.AttrDeploymentEnvironment: "production"},
		JoinKeys: map[string]string{"version": "version", "host": "host"},
	}
}

// apmTerms is the first level of the apm fixture's cross product.
func apmTerms() []*engine.Term {
	pair := engine.NewWindowPair(backendDeploy, 20*time.Minute)
	window := engine.NewWindow(backendDeploy.Add(-20*time.Minute), backendTo)
	terms := make([]*engine.Term, 0, 12)
	for _, s := range []investigationv1.Statistic{investigationv1.Statistic_COUNT, investigationv1.Statistic_RATE,
		investigationv1.Statistic_ERROR_RATE, investigationv1.Statistic_P50, investigationv1.Statistic_P95,
		investigationv1.Statistic_P99} {
		terms = append(terms, engine.Compare(apmPointer(), pair, s))
	}
	return append(terms,
		engine.Compare(backendLogPointer(), pair, investigationv1.Statistic_COUNT),
		engine.Compare(backendLogPointer(), pair, investigationv1.Statistic_P95),
		engine.ErrorSpans(apmSourceEdge, apmTargetEdge, graphv1.EdgeType_CALLS, window),
		engine.ErrorSpans(apmTargetEdge, apmSourceEdge, graphv1.EdgeType_CALLS, window),
	)
}

// apmTwin is the live twin: the logs of the logs fixture and the spans above.
func apmTwin(t *testing.T) *twin {
	t.Helper()
	return &twin{t: t, lines: backendLines(), monitor: backendMonitor(), spans: apmSpans()}
}

func apmLive(t *testing.T) (*ddbackend.Backend, *twin) {
	t.Helper()
	tw := apmTwin(t)
	b, _ := liveBackendWith(t, tw.ServeHTTP, true)
	return b, tw
}

func execute(t *testing.T, b *ddbackend.Backend, term *engine.Term) *engine.Response {
	t.Helper()
	resp, err := b.Execute(context.Background(), request(term))
	if err != nil {
		t.Fatalf("%s: %v", engine.TermName(term), err)
	}
	return resp
}

// countIn is how many of the twin's spans fall in [from, to), and how many of them are errors.
func countIn(spans []twinSpan, from, to time.Time, retainedOnly bool) (total, errs int) {
	for _, s := range spans {
		if s.at.Before(from) || !s.at.Before(to) || (retainedOnly && !s.retained) {
			continue
		}
		total++
		if s.status == "error" {
			errs++
		}
	}
	return total, errs
}

func TestErrorSpansAnswersFromTheRetainedSpans(t *testing.T) {
	t.Parallel()
	b, tw := apmLive(t)
	window := engine.NewWindow(backendDeploy.Add(-20*time.Minute), backendTo)
	resp := execute(t, b, engine.ErrorSpans(apmSourceEdge, apmTargetEdge, graphv1.EdgeType_CALLS, window))
	if resp.GetOutcome() != investigationv1.TermOutcome_DIGEST {
		t.Fatalf("got %v / %v %s", resp.GetOutcome(), resp.GetFailureReason(), resp.GetFailureDetail())
	}
	groups := resp.GetDigest().GetTrace().GetGroups()
	if len(groups) == 0 || groups[0].GetErrorKind() != apmFailureKind || groups[0].GetOperation() != "POST /charge" {
		t.Fatalf("the failing group is not first: %v", groups)
	}
	var total, errs int64
	for _, g := range groups {
		total += g.GetCount()
		if g.GetErrorKind() != "" {
			errs += g.GetCount()
		}
		if g.GetLatency()["P95"] <= 0 || g.GetJoinKeys().GetWorkload() == "" {
			t.Errorf("a group without latency or a workload join key: %v", g)
		}
	}
	wantTotal, wantErrs := countIn(tw.spans, window.GetStart().AsTime(), window.GetEnd().AsTime(), true)
	if total != int64(wantTotal) || errs != int64(wantErrs) {
		t.Errorf("counted %d spans / %d errors; the twin retained %d / %d", total, errs, wantTotal, wantErrs)
	}
	// The slow, failing group is slow: milliseconds, well above the healthy route.
	if groups[0].GetLatency()["P95"] < 2000 {
		t.Errorf("the failing group's P95 is %v ms", groups[0].GetLatency()["P95"])
	}
	// The coverage states the sample: retained spans, not requests (FR-048a).
	cov := resp.GetDigest().GetCoverage()
	if cov.GetDataSource() != "datadog_apm:spans" || !strings.Contains(cov.GetSampling(), "retained") ||
		!strings.Contains(cov.GetTruncation(), "retained_spans") {
		t.Errorf("the coverage does not state the sample: source %q, sampling %q, truncation %q",
			cov.GetDataSource(), cov.GetSampling(), cov.GetTruncation())
	}
	if cov.GetVolumeConsidered() != total {
		t.Errorf("volume %d, counted %d", cov.GetVolumeConsidered(), total)
	}
	if got := tw.requests(); len(got) != 1 || got[0] != "POST /api/v2/spans/analytics/aggregate" {
		t.Errorf("the requests were %v", got)
	}
}

func TestErrorSpansOverAnEdgeWithNoSpansIsNoData(t *testing.T) {
	t.Parallel()
	b, _ := apmLive(t)
	window := engine.NewWindow(backendDeploy.Add(-20*time.Minute), backendTo)
	resp := execute(t, b, engine.ErrorSpans(apmTargetEdge, apmSourceEdge, graphv1.EdgeType_CALLS, window))
	if resp.GetOutcome() != investigationv1.TermOutcome_NO_DATA {
		t.Fatalf("got %v", resp.GetOutcome())
	}
	if resp.GetDigest().GetCoverage().GetDataSource() != "datadog_apm:spans" {
		t.Errorf("an empty read says it read nothing: %v", resp.GetDigest().GetCoverage())
	}
}

func TestErrorSpansOverAnEntityThatIsNoServiceSaysSo(t *testing.T) {
	t.Parallel()
	b, tw := apmLive(t)
	resp := execute(t, b, engine.ErrorSpans("not a service", apmTargetEdge, graphv1.EdgeType_CALLS,
		engine.NewWindow(backendDeploy, backendTo)))
	if resp.GetOutcome() != investigationv1.TermOutcome_NO_DATA ||
		!strings.Contains(resp.GetDigest().GetCoverage().GetTruncation(), "names no Datadog service") {
		t.Fatalf("got %v %q", resp.GetOutcome(), resp.GetDigest().GetCoverage().GetTruncation())
	}
	if len(tw.requests()) != 0 {
		t.Errorf("a span query was made for an entity that is no service: %v", tw.requests())
	}
}

// compare states the latency percentiles and the error rate from the trace metrics, which count every
// span, per version where the service stamps one.
func TestCompareStatesLatencyAndErrorRateFromTheTraceMetrics(t *testing.T) {
	t.Parallel()
	b, tw := apmLive(t)
	pair := engine.NewWindowPair(backendDeploy, 20*time.Minute)
	baseline, symptom := pair.GetBaseline(), pair.GetSymptom()

	p95 := execute(t, b, engine.Compare(apmPointer(), pair, investigationv1.Statistic_P95))
	if p95.GetOutcome() != investigationv1.TermOutcome_DIGEST {
		t.Fatalf("P95: %v / %v %s", p95.GetOutcome(), p95.GetFailureReason(), p95.GetFailureDetail())
	}
	cmp := p95.GetDigest().GetMetric().GetComparisons()[0]
	if cmp.GetDirection() != "up" || !cmp.GetSeparable() || cmp.GetSymptom() < 2000 || cmp.GetBaseline() > 200 {
		t.Errorf("P95 did not move as the story says (milliseconds): %v", cmp)
	}
	if got := p95.GetDigest().GetCoverage().GetTruncation(); !strings.Contains(got, "percentile_of_intervals") {
		t.Errorf("a mean of interval percentiles is not stated as one: %q", got)
	}

	errRate := execute(t, b, engine.Compare(apmPointer(), pair, investigationv1.Statistic_ERROR_RATE))
	cmp = errRate.GetDigest().GetMetric().GetComparisons()[0]
	total, errs := countIn(tw.spans, symptom.GetStart().AsTime(), symptom.GetEnd().AsTime(), false)
	if cmp.GetBaseline() != 0 || cmp.GetSymptom() != math.Round(float64(errs)/float64(total)*1e6)/1e6 {
		t.Errorf("ERROR_RATE %v; the twin has %d errors of %d spans", cmp, errs, total)
	}

	count := execute(t, b, engine.Compare(apmPointer(), pair, investigationv1.Statistic_COUNT))
	cmp = count.GetDigest().GetMetric().GetComparisons()[0]
	wantBaseline, _ := countIn(tw.spans, baseline.GetStart().AsTime(), baseline.GetEnd().AsTime(), false)
	if int(cmp.GetBaseline()) != wantBaseline || int(cmp.GetSymptom()) != total {
		t.Errorf("COUNT %v: the trace metrics count every span, retained or not (%d and %d)", cmp, wantBaseline, total)
	}
	retained, _ := countIn(tw.spans, symptom.GetStart().AsTime(), symptom.GetEnd().AsTime(), true)
	if retained == total {
		t.Fatal("the twin retains every span; the assertion above proves nothing")
	}

	// Per version: the old commit only in the baseline window, the new one only in the symptom's.
	versions := map[string]string{}
	for _, s := range count.GetDigest().GetMetric().GetSeries() {
		if s.GetTags()["grouping"] == "version" {
			versions[s.GetTags()["window"]] = s.GetJoinKeys().GetVersion()
			if s.GetJoinKeys().GetWorkload() == "" {
				t.Errorf("a per-version series without its workload join key: %v", s)
			}
		}
	}
	if versions["baseline"] != backendOldCommit || versions["symptom"] != backendNewCommit {
		t.Errorf("the versions by window are %v", versions)
	}
	if got := count.GetDigest().GetCoverage(); got.GetDataSource() != "datadog_apm:trace_metrics" || got.GetSampling() != "none" {
		t.Errorf("the coverage %v", got)
	}
	for _, p := range tw.requests() {
		if p != "GET /api/v1/query" {
			t.Errorf("an unexpected request %s", p)
		}
	}
}

func TestCompareOverAnAPMPointerRefusesWhatTraceMetricsDoNotCarry(t *testing.T) {
	t.Parallel()
	b, _ := apmLive(t)
	resp := execute(t, b, engine.Compare(apmPointer(), engine.NewWindowPair(backendDeploy, 20*time.Minute),
		investigationv1.Statistic_MAX))
	if resp.GetFailureReason() != investigationv1.FailureReason_REJECTED_BY_BACKEND {
		t.Fatalf("got %v / %v", resp.GetOutcome(), resp.GetFailureReason())
	}
	wild := apmPointer()
	wild.Selector = "service:* env:production span:http.request"
	resp = execute(t, b, engine.Compare(wild, engine.NewWindowPair(backendDeploy, 20*time.Minute), investigationv1.Statistic_COUNT))
	if resp.GetFailureReason() != investigationv1.FailureReason_UNSUPPORTED_POINTER {
		t.Fatalf("a wildcard on service: %v / %v", resp.GetOutcome(), resp.GetFailureReason())
	}
	spans := apmPointer()
	spans.Kind, spans.Vocabulary = graphv1.PointerKind_TRACE, feeder.VocabDatadogSpans
	resp = execute(t, b, engine.Compare(spans, engine.NewWindowPair(backendDeploy, 20*time.Minute), investigationv1.Statistic_COUNT))
	if resp.GetFailureReason() != investigationv1.FailureReason_UNSUPPORTED_POINTER {
		t.Fatalf("a span pointer is not a compare pointer: %v / %v", resp.GetOutcome(), resp.GetFailureReason())
	}
}

// SC-023, SC-024: with apm_topology off nothing changes. error_spans names the absent source, an APM
// pointer is refused as one minted for another surface, and the twin is never asked for a span or a metric.
func TestWithAPMTopologyOffTheSameQuestionsStayAsTheyWere(t *testing.T) {
	t.Parallel()
	tw := apmTwin(t)
	b, calls := liveBackend(t, tw.ServeHTTP)
	pair := engine.NewWindowPair(backendDeploy, 20*time.Minute)

	spans := execute(t, b, engine.ErrorSpans(apmSourceEdge, apmTargetEdge, graphv1.EdgeType_CALLS, pair.GetSymptom()))
	if spans.GetOutcome() != investigationv1.TermOutcome_NO_DATA || spans.GetDigest().GetCoverage().GetDataSource() != "datadog_apm:absent" {
		t.Errorf("error_spans: %v %v", spans.GetOutcome(), spans.GetDigest().GetCoverage().GetDataSource())
	}
	metric := execute(t, b, engine.Compare(apmPointer(), pair, investigationv1.Statistic_P95))
	if metric.GetFailureReason() != investigationv1.FailureReason_UNSUPPORTED_POINTER ||
		!strings.Contains(metric.GetFailureDetail(), feeder.VocabDatadogAPMMetric) {
		t.Errorf("an APM pointer with the capability off: %v / %v %s", metric.GetOutcome(), metric.GetFailureReason(), metric.GetFailureDetail())
	}
	logs := execute(t, b, engine.Compare(backendLogPointer(), pair, investigationv1.Statistic_P95))
	if logs.GetOutcome() != investigationv1.TermOutcome_QUERY_FAILED || !strings.Contains(logs.GetFailureDetail(), "not a property of a log count") {
		t.Errorf("a log latency percentile: %v %s", logs.GetOutcome(), logs.GetFailureDetail())
	}
	if *calls != 0 {
		t.Errorf("%d Datadog calls: %v", *calls, tw.requests())
	}
}

// A backend that declared no APM operation cannot answer an APM term: the metered transport refuses the
// call before it leaves the process (FR-008b), and the backend does not turn that into an empty answer.
func TestAnAPMTermWithoutTheDeclaredOperationIsAnError(t *testing.T) {
	t.Parallel()
	tw := apmTwin(t)
	b, _ := liveBackendFull(t, tw.ServeHTTP, ddfeeder.DefaultSurface, true)
	_, err := b.Execute(context.Background(), request(engine.ErrorSpans(apmSourceEdge, apmTargetEdge,
		graphv1.EdgeType_CALLS, engine.NewWindow(backendDeploy, backendTo))))
	if err == nil {
		t.Fatal("the span aggregate was issued without being declared")
	}
	if len(tw.requests()) != 0 {
		t.Errorf("requests left the process: %v", tw.requests())
	}
}

// ---- the world ------------------------------------------------------------------------------------

func TestGenerateDatadogBackendAPMFixture(t *testing.T) {
	if os.Getenv(genFixturesEnv) == "" {
		t.Skipf("set %s=1 to regenerate %s", genFixturesEnv, apmFixture)
	}
	dir := filepath.Join(repoRoot(t), apmFixture)
	for _, generated := range []string{"world", "events.jsonl", "rejected.jsonl", "manifest.yaml"} {
		if err := os.RemoveAll(filepath.Join(dir, generated)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeBackendGraphHalf(t, dir, filepath.Base(apmFixture), graphHalf{
		family: "datadog-backend",
		description: "The Datadog telemetry backend's span and APM metric terms over a synthetic structural " +
			"twin. `checkout` calls `payments`; at 14:10 a new version ships and the calls start failing " +
			"with a timeout, slowly. Datadog retains two of every three spans and counts every one in its " +
			"trace metrics, so error_spans (retained spans) and compare (trace metrics) disagree on a count " +
			"by design and each coverage block says which surface answered. world/ is the cross product of " +
			"the terms the apm_topology capability adds — compare over an APM metric pointer for every " +
			"statistic it states, error_spans over the edge and over the empty reverse edge — and the two " +
			"log answers that must not move; internal/backends/datadog asserts on every run that it " +
			"answers every question exactly as the live twin does, and that with the capability off the " +
			"twin is never asked for a span or a metric. Built against the published API shapes, not " +
			"verified against a live organisation. Synthetic structural twin: no identifier is derived " +
			"from any organisation.",
	})
	live, _ := apmLive(t)
	recorder, err := sdk.NewRecorderWithOptions(filepath.Join(dir, "world"), sdk.RecordOptions{
		DrillDownDepth:         1,
		WindowGrid:             []*investigationv1.WindowPair{engine.NewWindowPair(backendDeploy, 20*time.Minute)},
		RedactionPolicyVersion: live.Describe().Redaction.GetPolicyVersion(),
		Focus:                  ddfeeder.NSService + "=production/" + backendServiceName,
	})
	if err != nil {
		t.Fatal(err)
	}
	reqs, resps := crossProduct(t, live, apmTerms())
	for i := range reqs {
		if err := recorder.Record(context.Background(), reqs[i], resps[i]); err != nil {
			t.Fatalf("record %s: %v", engine.TermName(reqs[i].GetTerm()), err)
		}
	}
	if _, err := recorder.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Logf("%s: %d recorded answers", apmFixture, len(reqs))
}

// The recorded world answers every question the live twin does, field for field (SC-019), and the story
// the fixture tells is still in it.
func TestTheRecordedAPMWorldAnswersLikeTheLiveTwin(t *testing.T) {
	t.Parallel()
	live, tw := apmLive(t)
	liveReqs, liveResps := crossProduct(t, live, apmTerms())
	recordedReqs, recordedResps := crossProduct(t, backendRecorded(t, apmFixture), apmTerms())
	if len(recordedReqs) != len(liveReqs) {
		t.Fatalf("the recorded cross product has %d questions, the live one %d", len(recordedReqs), len(liveReqs))
	}
	outcomes := map[string]int{}
	for i := range liveResps {
		name := engine.TermName(liveReqs[i].GetTerm())
		l, r := liveResps[i], recordedResps[i]
		outcomes[fmt.Sprintf("%s/%s", name, l.GetOutcome())]++
		if r.GetOutcome() == investigationv1.TermOutcome_NOT_RECORDED {
			t.Errorf("%d %s: the world does not hold it", i, name)
			continue
		}
		if r.GetMode() != engine.ModeRecorded {
			t.Errorf("%d %s: mode %q", i, name, r.GetMode())
		}
		if l.GetResponseDigest() != r.GetResponseDigest() || !proto.Equal(l.GetDigest(), r.GetDigest()) {
			t.Errorf("%d %s: recorded differs from live\n recorded %v\n     live %v", i, name, r.GetDigest(), l.GetDigest())
		}
	}
	for _, want := range []string{"compare/DIGEST", "compare/QUERY_FAILED", "error_spans/DIGEST", "error_spans/NO_DATA"} {
		if outcomes[want] == 0 {
			t.Errorf("no %s answer in the cross product: %v", want, outcomes)
		}
	}
	// Both surfaces were asked, and only the declared ones.
	var spans, metrics, other int
	for _, p := range tw.requests() {
		switch {
		case p == "POST /api/v2/spans/analytics/aggregate":
			spans++
		case p == "GET /api/v1/query":
			metrics++
		case strings.Contains(p, "/logs/"):
		default:
			other++
		}
	}
	if spans == 0 || metrics == 0 || other != 0 {
		t.Errorf("spans %d, metrics %d, other %d in %v", spans, metrics, other, tw.requests())
	}
}

// SC-024, last clause: every log term's answer is identical with apm_topology on and off, digest for
// digest. Only error_spans, whose answer the capability replaces, may differ.
func TestEveryLogTermIsIdenticalWithAPMTopologyOnAndOff(t *testing.T) {
	t.Parallel()
	offBackend, _ := liveBackendWith(t, apmTwin(t).ServeHTTP, false)
	onBackend, _ := liveBackendWith(t, apmTwin(t).ServeHTTP, true)
	compared := 0
	for _, term := range backendTerms() {
		name := engine.TermName(term)
		if name == sdk.TermErrorSpans {
			continue
		}
		off, on := execute(t, offBackend, term), execute(t, onBackend, term)
		compared++
		if off.GetResponseDigest() != on.GetResponseDigest() || !proto.Equal(off.GetDigest(), on.GetDigest()) ||
			off.GetOutcome() != on.GetOutcome() {
			t.Errorf("%s differs with apm_topology on:\n off %v\n  on %v", name, off.GetDigest(), on.GetDigest())
		}
	}
	if compared < 6 {
		t.Errorf("only %d terms compared", compared)
	}
}
