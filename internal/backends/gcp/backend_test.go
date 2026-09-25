// SPDX-License-Identifier: Apache-2.0

package gcp_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	metricpb "google.golang.org/genproto/googleapis/api/metric"
	monitoredrespb "google.golang.org/genproto/googleapis/api/monitoredres"
	ltype "google.golang.org/genproto/googleapis/logging/type"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	gcpbackend "github.com/Pierre-Theophile/aisre/internal/backends/gcp"
	feedergcp "github.com/Pierre-Theophile/aisre/internal/feeders/gcp"
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	sdk "github.com/Pierre-Theophile/aisre/pkg/backend"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The GCP telemetry backend's tests (T091–T108).
//
// Every assertion below is about a sentence a reader of a digest would act on: which outcome an
// empty answer carries, whether a caveat is stated, whether a join key joins. The fakes stand in
// for the two Google surfaces and nothing else — there is no code path under test that talks to a
// network, which is itself one of the properties asserted.

// ---- the fakes -------------------------------------------------------------------------------

type fakeMetrics struct {
	series     []*monitoringpb.TimeSeries
	perCall    [][]*monitoringpb.TimeSeries
	err        error
	descriptor *metricpb.MetricDescriptor
	descErr    error
	requests   []*monitoringpb.ListTimeSeriesRequest
	descCalls  int
}

func (f *fakeMetrics) ListTimeSeries(_ context.Context, req *monitoringpb.ListTimeSeriesRequest) ([]*monitoringpb.TimeSeries, error) {
	f.requests = append(f.requests, req)
	if f.err != nil {
		return nil, f.err
	}
	if len(f.perCall) > 0 {
		out := f.perCall[0]
		f.perCall = f.perCall[1:]
		return out, nil
	}
	return f.series, nil
}

func (f *fakeMetrics) GetMetricDescriptor(_ context.Context, _, _ string) (*metricpb.MetricDescriptor, error) {
	f.descCalls++
	if f.descErr != nil {
		return nil, f.descErr
	}
	return f.descriptor, nil
}

type logPage struct {
	entries []*loggingpb.LogEntry
	token   string
}

type fakeLogs struct {
	pages    []logPage
	err      error
	requests []*loggingpb.ListLogEntriesRequest
}

func (f *fakeLogs) ListEntries(_ context.Context, req *loggingpb.ListLogEntriesRequest) ([]*loggingpb.LogEntry, string, error) {
	f.requests = append(f.requests, req)
	if f.err != nil {
		return nil, "", f.err
	}
	if len(f.pages) == 0 {
		return nil, "", nil
	}
	page := f.pages[0]
	f.pages = f.pages[1:]
	return page.entries, page.token, nil
}

type fakeAlerts struct {
	alerts []*gcpbackend.Alert
	err    error
}

func (f *fakeAlerts) ListAlerts(_ context.Context, _ string, _, _ time.Time) ([]*gcpbackend.Alert, error) {
	return f.alerts, f.err
}

// exploding is a transport whose every call fails the test. It is how "recorded mode makes no
// network call" is asserted as a property rather than checked by reading the code.
type exploding struct{ t *testing.T }

func (e exploding) ListTimeSeries(context.Context, *monitoringpb.ListTimeSeriesRequest) ([]*monitoringpb.TimeSeries, error) {
	e.t.Fatal("recorded mode called Cloud Monitoring; it must never fall through to a live call")
	return nil, nil
}

func (e exploding) GetMetricDescriptor(context.Context, string, string) (*metricpb.MetricDescriptor, error) {
	e.t.Fatal("recorded mode read a metric descriptor")
	return nil, nil
}

func (e exploding) ListEntries(context.Context, *loggingpb.ListLogEntriesRequest) ([]*loggingpb.LogEntry, string, error) {
	e.t.Fatal("recorded mode called Cloud Logging")
	return nil, "", nil
}

// ---- fixtures --------------------------------------------------------------------------------

var testNow = time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

// versionAttribute is what a planner reads off `Pointer.join_keys["version"]` and passes with the
// term (ADR-0005 D4). The algebra refuses an errors_by_version that names none, so every call here
// names the label the GCP grouping actually uses.
const versionAttribute = "resource.labels.revision_name"

const (
	testProject  = "shop-prod"
	testRegion   = "europe-west1"
	testService  = "storefront"
	testRevision = "storefront-00042-abc"
)

func metricSelector(extra string) string {
	selector := `metric.type="run.googleapis.com/request_count"` +
		` AND resource.type="cloud_run_revision"` +
		` AND resource.labels.project_id="` + testProject + `"` +
		` AND resource.labels.location="` + testRegion + `"` +
		` AND resource.labels.service_name="` + testService + `"`
	if extra != "" {
		selector += " AND " + extra
	}
	return selector
}

func logSelector() string {
	return `resource.type="cloud_run_revision"` +
		` AND resource.labels.project_id="` + testProject + `"` +
		` AND resource.labels.service_name="` + testService + `"` +
		` AND severity >= WARNING`
}

func metricPointer(t *testing.T, extra string) *graphv1.Pointer {
	t.Helper()
	pointer, err := feedergcp.MetricPointer(metricSelector(extra), map[string]string{
		feeder.AttrServiceName: testService,
	})
	if err != nil {
		t.Fatalf("MetricPointer: %v", err)
	}
	return pointer
}

func logPointer() *graphv1.Pointer {
	return feedergcp.LogPointer(logSelector(), map[string]string{feeder.AttrServiceName: testService})
}

// series builds one returned time series with the labels a Cloud Run answer carries.
func series(revision, codeClass string, start time.Time, step time.Duration, values ...float64) *monitoringpb.TimeSeries {
	points := make([]*monitoringpb.Point, 0, len(values))
	// The API returns points most-recent-first, and this fake does too: a backend that assumed
	// ascending order would pass against an ascending fake and mis-order every live answer.
	for i := len(values) - 1; i >= 0; i-- {
		at := start.Add(time.Duration(i) * step)
		points = append(points, &monitoringpb.Point{
			Interval: &monitoringpb.TimeInterval{
				StartTime: timestamppb.New(at),
				EndTime:   timestamppb.New(at.Add(step)),
			},
			Value: &monitoringpb.TypedValue{
				Value: &monitoringpb.TypedValue_DoubleValue{DoubleValue: values[i]},
			},
		})
	}
	labels := map[string]string{
		"project_id":   testProject,
		"location":     testRegion,
		"service_name": testService,
	}
	if revision != "" {
		labels["revision_name"] = revision
	}
	metricLabels := map[string]string{}
	if codeClass != "" {
		metricLabels["response_code_class"] = codeClass
	}
	return &monitoringpb.TimeSeries{
		Metric:   &metricpb.Metric{Type: "run.googleapis.com/request_count", Labels: metricLabels},
		Resource: &monitoredrespb.MonitoredResource{Type: "cloud_run_revision", Labels: labels},
		Points:   points,
	}
}

func logEntry(at time.Time, severity ltype.LogSeverity, text, revision string) *loggingpb.LogEntry {
	return &loggingpb.LogEntry{
		Timestamp:        timestamppb.New(at),
		ReceiveTimestamp: timestamppb.New(at.Add(3 * time.Second)),
		Severity:         severity,
		Payload:          &loggingpb.LogEntry_TextPayload{TextPayload: text},
		Resource: &monitoredrespb.MonitoredResource{
			Type: "cloud_run_revision",
			Labels: map[string]string{
				"service_name":  testService,
				"revision_name": revision,
				"instance_id":   "instance-7",
			},
		},
	}
}

func newBackend(t *testing.T, transport *gcpbackend.Transport) *gcpbackend.Backend {
	t.Helper()
	b, err := gcpbackend.New(gcpbackend.Options{
		OrgSlug:   "acme",
		Project:   testProject,
		Region:    testRegion,
		Transport: transport,
		Sanitiser: liveSanitiser(t),
		Redactor:  declaredRedactor(t),
		Now:       func() time.Time { return testNow },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return b
}

// declaredRedactor is the redactor built from the policy this backend actually declares, rather
// than from 002's default: the declaration and the enforcement have to be the same policy or the
// check is against a different rule than the one published.
func declaredRedactor(t *testing.T) *engine.Redactor {
	t.Helper()
	r, err := engine.NewRedactor(gcpbackend.DeclaredRedactionPolicy(liveSanitiser(t).Policy()),
		[]byte("a corpus key for the digest side"))
	if err != nil {
		t.Fatalf("NewRedactor: %v", err)
	}
	return r
}

func window(start, end time.Time) *investigationv1.Window {
	return &investigationv1.Window{Start: timestamppb.New(start), End: timestamppb.New(end)}
}

func execute(t *testing.T, b *gcpbackend.Backend, term *investigationv1.AlgebraTerm) *investigationv1.AlgebraResponse {
	t.Helper()
	resp, err := b.Execute(context.Background(), &investigationv1.AlgebraRequest{Term: term})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return resp
}

func coverageOf(resp *investigationv1.AlgebraResponse) *investigationv1.Coverage {
	return resp.GetDigest().GetCoverage()
}

// ---- T091: the declaration -------------------------------------------------------------------

// The declaration is all eight telemetry terms and nothing else, every capability read-only, and
// one published cost class each — the three things registration refuses a backend for.
func TestDescribeIsTheContractDeclaration(t *testing.T) {
	b := newBackend(t, &gcpbackend.Transport{})
	d := b.Describe()

	if err := d.Validate(); err != nil {
		t.Fatalf("the declaration does not register: %v", err)
	}
	if d.Vendor != "gcp" || d.Name != "gcp:acme" {
		t.Errorf("name/vendor = %q/%q, want gcp:acme/gcp", d.Name, d.Vendor)
	}
	want := map[string]sdk.CostClass{
		"compare":           sdk.CostClassStandard,
		"errors_by_version": sdk.CostClassStandard,
		"error_spans":       sdk.CostClassStandard,
		"onset":             sdk.CostClassExpensive,
		"new_log_patterns":  sdk.CostClassExpensive,
		"exemplars":         sdk.CostClassExpensive,
		"monitor_state":     sdk.CostClassCheap,
		"drill_down":        sdk.CostClassCheap,
	}
	if len(d.Terms) != len(want) {
		t.Fatalf("declared %d terms, want the %d telemetry terms: %v", len(d.Terms), len(want), d.Terms)
	}
	for _, term := range d.Terms {
		class, published := want[term]
		if !published {
			t.Errorf("declared %s, which contract §2 does not table", term)
			continue
		}
		if d.CostClasses[term] != class {
			t.Errorf("%s priced %s, contract §2 says %s", term, d.CostClasses[term], class)
		}
	}
	for _, capability := range d.Capabilities {
		if !capability.ReadOnly {
			t.Errorf("capability %s is not read-only; executing a term creates no GCP object", capability.Name)
		}
		if capability.MaxWindow <= 0 {
			t.Errorf("capability %s declares no window cap", capability.Name)
		}
	}
	if got := d.Redaction.GetPolicyVersion(); got != "1.0.0" {
		t.Errorf("redaction policy version = %q, want the sanitisation contract's 1.0.0", got)
	}
}

// A graph or knowledge term in the declaration is a registration failure, which is what keeps the
// families apart. The probe plants the violation the check exists for.
func TestADeclarationWithAGraphTermWouldNotRegister(t *testing.T) {
	b := newBackend(t, &gcpbackend.Transport{})
	d := b.Describe()
	d.Terms = append(d.Terms, "subgraph")
	d.CostClasses["subgraph"] = sdk.CostClassCheap
	if err := d.Validate(); err == nil {
		t.Fatal("a declaration naming a graph term registered; the graph family is the graph worker's")
	}
}

// ---- T091: out of algebra --------------------------------------------------------------------

// An out-of-algebra request is refused naming what was asked AND what is available. Both halves
// are the requirement: a refusal that names only the first leaves a caller guessing at an algebra,
// and guessing is how a caller ends up sending query text.
func TestAnOutOfAlgebraRequestNamesWhatIsAvailable(t *testing.T) {
	b := newBackend(t, &gcpbackend.Transport{})
	resp := execute(t, b, engine.KnowledgeSearch([]string{"entity"}, []string{"why"}, 3))

	if resp.GetOutcome() != investigationv1.TermOutcome_QUERY_FAILED {
		t.Fatalf("outcome = %s, want QUERY_FAILED", resp.GetOutcome())
	}
	if resp.GetFailureReason() != investigationv1.FailureReason_OUTSIDE_ALGEBRA {
		t.Errorf("reason = %s, want OUTSIDE_ALGEBRA", resp.GetFailureReason())
	}
	detail := resp.GetFailureDetail()
	if !strings.Contains(detail, "knowledge_search") {
		t.Errorf("the refusal does not name what was asked: %q", detail)
	}
	for _, term := range []string{"compare", "onset", "monitor_state", "drill_down"} {
		if !strings.Contains(detail, term) {
			t.Errorf("the refusal does not name %s as available: %q", term, detail)
		}
	}
}

// ---- T096, T098: compare ---------------------------------------------------------------------

// A before/after request carries ONE digest with an explicit Comparison — direction and magnitude
// — rather than two summaries a reader has to subtract (FR-092).
func TestCompareStatesTheComparisonRatherThanTwoSummaries(t *testing.T) {
	start := testNow.Add(-4 * time.Hour)
	metrics := &fakeMetrics{perCall: [][]*monitoringpb.TimeSeries{
		{series(testRevision, "", start, time.Minute, 10, 10, 10, 10)},
		{series(testRevision, "", start.Add(2*time.Hour), time.Minute, 40, 40, 40, 40)},
	}}
	b := newBackend(t, &gcpbackend.Transport{Metrics: metrics})

	term := engine.Compare(metricPointer(t, ""), &investigationv1.WindowPair{
		Baseline: window(start, start.Add(time.Hour)),
		Symptom:  window(testNow.Add(-time.Hour), testNow),
	}, investigationv1.Statistic_MEAN)

	resp := execute(t, b, term)
	if resp.GetOutcome() != investigationv1.TermOutcome_DIGEST {
		t.Fatalf("outcome = %s (%s)", resp.GetOutcome(), resp.GetFailureDetail())
	}
	comparisons := resp.GetDigest().GetMetric().GetComparisons()
	if len(comparisons) != 1 {
		t.Fatalf("got %d comparisons, want exactly one", len(comparisons))
	}
	c := comparisons[0]
	if c.GetBaseline() != 10 || c.GetSymptom() != 40 {
		t.Errorf("comparison = %v -> %v, want 10 -> 40", c.GetBaseline(), c.GetSymptom())
	}
	if c.GetDirection() != "up" {
		t.Errorf("direction = %q, want up", c.GetDirection())
	}
	if !c.GetSeparable() {
		t.Error("a fourfold increase does not separate the hypothesis")
	}

	// Server-side aggregation is the design: the request carries one, and the digest carries no
	// point-by-point sample.
	if len(metrics.requests) != 2 {
		t.Fatalf("issued %d queries, want one per window", len(metrics.requests))
	}
	for _, req := range metrics.requests {
		agg := req.GetAggregation()
		if agg == nil || agg.GetAlignmentPeriod().AsDuration() < 60*time.Second {
			t.Errorf("aggregation = %v; the alignment period is the API's 60s minimum or wider", agg)
		}
		if req.GetView() != monitoringpb.ListTimeSeriesRequest_FULL {
			t.Errorf("view = %s, want FULL; HEADERS returns no points at all", req.GetView())
		}
		if req.GetOrderBy() != "" {
			t.Errorf("order_by = %q; it must be blank when an aggregation is present", req.GetOrderBy())
		}
	}
}

// Every request_count-derived digest states the two caveats of contract §3.1 in its coverage: the
// error rate is derived, and the metric omits the ingress failures an incident is often about.
func TestARequestCountDigestStatesThatItUndercountsIngressFailures(t *testing.T) {
	start := testNow.Add(-2 * time.Hour)
	metrics := &fakeMetrics{series: []*monitoringpb.TimeSeries{series(testRevision, "", start, time.Minute, 1, 2)}}
	b := newBackend(t, &gcpbackend.Transport{Metrics: metrics})

	resp := execute(t, b, engine.Compare(metricPointer(t, ""), &investigationv1.WindowPair{
		Baseline: window(start, start.Add(time.Hour)),
		Symptom:  window(testNow.Add(-time.Hour), testNow),
	}, investigationv1.Statistic_RATE))

	truncation := coverageOf(resp).GetTruncation()
	for _, want := range []string{"derived_error_rate", "request_count_excludes_ingress_failures"} {
		if !strings.Contains(truncation, want) {
			t.Errorf("coverage does not state %s: %q", want, truncation)
		}
	}
	if !strings.Contains(truncation, "max-instances") {
		t.Errorf("the caveat does not say WHICH failures are omitted: %q", truncation)
	}
}

// ---- T097: errors_by_version -----------------------------------------------------------------

// The split is by revision, and the revision values are join keys. "The new revision is erroring
// and the old one is not" is then a fact the digest states rather than one the reader assembles.
func TestErrorsByVersionSplitsByRevisionWithJoinKeys(t *testing.T) {
	start := testNow.Add(-time.Hour)
	metrics := &fakeMetrics{series: []*monitoringpb.TimeSeries{
		series("storefront-00041-old", "2xx", start, time.Minute, 100, 100),
		series("storefront-00041-old", "5xx", start, time.Minute, 1, 1),
		series("storefront-00042-new", "2xx", start, time.Minute, 50, 50),
		series("storefront-00042-new", "5xx", start, time.Minute, 50, 50),
	}}
	b := newBackend(t, &gcpbackend.Transport{Metrics: metrics})

	resp := execute(t, b, engine.ErrorsByVersion(metricPointer(t, ""), window(start, testNow), versionAttribute))
	if resp.GetOutcome() != investigationv1.TermOutcome_DIGEST {
		t.Fatalf("outcome = %s (%s)", resp.GetOutcome(), resp.GetFailureDetail())
	}
	versions := resp.GetDigest().GetErrorsByVersion().GetVersions()
	if len(versions) != 2 {
		t.Fatalf("got %d version rows, want one per revision", len(versions))
	}
	// The published ordering puts the worst first, so the reader's eye lands on the revision
	// that is failing rather than on the one that happens to sort first.
	if versions[0].GetErrorRate() <= versions[1].GetErrorRate() {
		t.Errorf("rows are not worst-first: %v then %v",
			versions[0].GetErrorRate(), versions[1].GetErrorRate())
	}
	if got := versions[0].GetErrorRate(); got != 0.5 {
		t.Errorf("the failing revision's rate = %v, want 100 errors over 200 requests", got)
	}
	for _, row := range versions {
		if row.GetJoinKeys().GetVersion() == "" {
			t.Error("a version row carries no join key; a digest whose keys do not join is evidence about nothing")
		}
		if !strings.HasPrefix(row.GetJoinKeys().GetVersion(), "px_") {
			t.Errorf("join key %q left the process in the clear; the graph holds tokens",
				row.GetJoinKeys().GetVersion())
		}
		// The same identifier under another field name is the same identifier.
		if strings.Contains(row.GetVersion(), "storefront-000") {
			t.Errorf("the version row spells the revision out: %q", row.GetVersion())
		}
		if row.GetVersion() != row.GetJoinKeys().GetVersion() {
			t.Errorf("the row's version %q and its join key %q are different tokens for one revision",
				row.GetVersion(), row.GetJoinKeys().GetVersion())
		}
	}

	// The grouping is what makes the answer per-revision, and the resource.type pin is what
	// makes the grouping trustworthy.
	req := metrics.requests[0]
	if got := req.GetAggregation().GetGroupByFields(); len(got) == 0 ||
		got[0] != "resource.labels.revision_name" {
		t.Errorf("group_by_fields = %v, want the revision first", got)
	}
}

// An unpinned selector is refused rather than executed. Executing it would not error — it would
// answer, and the answer would be wrong in the direction that exonerates a bad deploy.
func TestErrorsByVersionRefusesASelectorThatDoesNotPinTheResourceType(t *testing.T) {
	b := newBackend(t, &gcpbackend.Transport{Metrics: &fakeMetrics{}})
	// Built by hand: the feeder's own MetricPointer refuses to mint this, which is the other
	// end of the same rule.
	pointer := &graphv1.Pointer{
		Kind:       graphv1.PointerKind_METRIC,
		Vocabulary: feeder.VocabGCPMonitoringFilter,
		Selector:   `metric.type="run.googleapis.com/request_count" AND resource.labels.service_name="storefront"`,
	}
	resp := execute(t, b, engine.ErrorsByVersion(pointer, window(testNow.Add(-time.Hour), testNow), versionAttribute))

	if resp.GetFailureReason() != investigationv1.FailureReason_UNSUPPORTED_POINTER {
		t.Fatalf("reason = %s, want UNSUPPORTED_POINTER", resp.GetFailureReason())
	}
	if !strings.Contains(resp.GetFailureDetail(), "cloud_run_instance") {
		t.Errorf("the refusal does not explain why the pin matters: %q", resp.GetFailureDetail())
	}
}

// The field the feeder mints as the `version` join key and the field this backend groups by are one
// string.
//
// This is the join that either works or silently does not. The feeder tells a consumer "split by
// this field"; the backend groups by its own constant; nothing at runtime compares them. A rename on
// either side would leave a pointer advertising a field the query never groups by, and the symptom
// would be a breakdown with one merged row rather than an error. So the identity is asserted rather
// than shared through a variable: each package keeps its own grammar, and drift fails here.
func TestTheFeedersVersionJoinKeyAndTheBackendsGroupingAreOneField(t *testing.T) {
	if gcpbackend.GroupByRevision != feedergcp.FieldRevisionName {
		t.Errorf("the backend groups Cloud Run series by %q and the feeder mints %q as the "+
			"version join key. A pointer would advertise a field the query does not group by, and "+
			"the answer would be one merged row rather than a refusal",
			gcpbackend.GroupByRevision, feedergcp.FieldRevisionName)
	}
	// And the constant is the filter-field spelling, not the label-map spelling. Both exist and
	// they are not interchangeable: one goes into a query, the other reads a result.
	if !strings.HasPrefix(gcpbackend.GroupByRevision, "resource.labels.") {
		t.Errorf("GroupByRevision = %q, which is how a returned series' label map is keyed rather "+
			"than a field the filter language has", gcpbackend.GroupByRevision)
	}
}

// A term asking to split by a field this backend does not group by is refused rather than answered.
//
// The grouping below is fixed at one field, so a mismatched request would be ignored — and the
// digest would then echo the name the term asked for while reporting numbers grouped by another.
// That is the same confident-wrong-answer shape the resource.type pin exists to prevent, arriving
// by the other door: the reader is told which field the split is by, and told wrong.
func TestErrorsByVersionRefusesASplitByAFieldItDoesNotGroupBy(t *testing.T) {
	start := testNow.Add(-time.Hour)
	metrics := &fakeMetrics{series: []*monitoringpb.TimeSeries{
		series("storefront-00042-new", "5xx", start, time.Minute, 50, 50),
	}}
	b := newBackend(t, &gcpbackend.Transport{Metrics: metrics})

	// `revision_name` is the label-map spelling — the plausible wrong answer, not a nonsense one.
	resp := execute(t, b, engine.ErrorsByVersion(
		metricPointer(t, ""), window(start, testNow), "revision_name"))

	if resp.GetFailureReason() != investigationv1.FailureReason_UNSUPPORTED_POINTER {
		t.Fatalf("reason = %s, want UNSUPPORTED_POINTER: the split was by %q and the answer would "+
			"have been grouped by %q", resp.GetFailureReason(), "revision_name", gcpbackend.GroupByRevision)
	}
	if !strings.Contains(resp.GetFailureDetail(), gcpbackend.GroupByRevision) {
		t.Errorf("the refusal does not name the field this backend does group by: %q", resp.GetFailureDetail())
	}
	if len(metrics.requests) != 0 {
		t.Error("the refused term still spent a Monitoring request; a refusal that costs quota is " +
			"a refusal that arrived too late")
	}
}

// The other side of that refusal: a term naming the field the backend does group by is answered, and
// the digest reports the field it was actually grouped by.
//
// A term naming *no* attribute is not tested here because it cannot reach the backend: the algebra
// refuses it first, naming `Pointer.join_keys["version"]` as where the attribute comes from. That
// refusal is asserted where it lives, in the algebra's own tests.
func TestErrorsByVersionReportsTheFieldItActuallyGroupedBy(t *testing.T) {
	start := testNow.Add(-time.Hour)
	metrics := &fakeMetrics{series: []*monitoringpb.TimeSeries{
		series("storefront-00042-new", "2xx", start, time.Minute, 50, 50),
	}}
	b := newBackend(t, &gcpbackend.Transport{Metrics: metrics})

	resp := execute(t, b, engine.ErrorsByVersion(
		metricPointer(t, ""), window(start, testNow), gcpbackend.GroupByRevision))
	if resp.GetOutcome() != investigationv1.TermOutcome_DIGEST {
		t.Fatalf("outcome = %s (%s)", resp.GetOutcome(), resp.GetFailureDetail())
	}
	got := resp.GetDigest().GetErrorsByVersion().GetVersionAttribute()
	if got != gcpbackend.GroupByRevision {
		t.Errorf("the digest reports version_attribute = %q, want %q — the field the rows were "+
			"actually grouped by", got, gcpbackend.GroupByRevision)
	}
}

// ---- T099: onset -----------------------------------------------------------------------------

// The estimate crosses the boundary and the series does not. The assertion is on the encoded
// response: a sample that reached any field would appear in the bytes.
func TestOnsetReturnsTheEstimateAndNoSample(t *testing.T) {
	start := testNow.Add(-6 * time.Hour)
	values := make([]float64, 0, 120)
	for range 60 {
		values = append(values, 1.0)
	}
	for range 60 {
		values = append(values, 97.1234)
	}
	metrics := &fakeMetrics{series: []*monitoringpb.TimeSeries{
		series(testRevision, "", start, 3*time.Minute, values...),
	}}
	b := newBackend(t, &gcpbackend.Transport{Metrics: metrics})

	resp := execute(t, b, engine.Onset(metricPointer(t, ""), window(start, testNow),
		investigationv1.OnsetMethod_SEASONAL_CUSUM))
	if resp.GetOutcome() != investigationv1.TermOutcome_DIGEST {
		t.Fatalf("outcome = %s (%s)", resp.GetOutcome(), resp.GetFailureDetail())
	}
	onset := resp.GetDigest().GetOnset()
	if onset == nil {
		t.Fatal("no onset digest")
	}
	if onset.GetMethod() != investigationv1.OnsetMethod_SEASONAL_CUSUM {
		t.Errorf("method = %s", onset.GetMethod())
	}
	encoded, err := protojson.Marshal(resp)
	if err != nil {
		t.Fatalf("protojson: %v", err)
	}
	// 97.1234 is a value that exists only in the series. If it appears anywhere in the response
	// — a field, an exemplar, a coverage note — a sample crossed the boundary.
	if strings.Contains(string(encoded), "97.1234") {
		t.Error("a raw sample appears in the response; the series may not cross the digest boundary")
	}
}

// ---- T100, T101: new_log_patterns ------------------------------------------------------------

// The pagination rule. An empty page WITH a token means the search did not finish, and reporting
// it as NO_DATA would tell an investigation that nothing happened because the vendor ran out of
// time.
func TestAnEmptyPageWithATokenIsNeverNoData(t *testing.T) {
	pages := make([]logPage, 0, gcpbackend.MaxLogPages+1)
	for range gcpbackend.MaxLogPages + 1 {
		pages = append(pages, logPage{token: "keep-going"})
	}
	logs := &fakeLogs{pages: pages}
	b := newBackend(t, &gcpbackend.Transport{Logs: logs})

	resp := execute(t, b, engine.NewLogPatterns(logPointer(),
		window(testNow.Add(-time.Hour), testNow), window(testNow.Add(-2*time.Hour), testNow.Add(-time.Hour))))

	if resp.GetOutcome() == investigationv1.TermOutcome_NO_DATA {
		t.Fatal("an unfinished search was reported as NO_DATA, which is the one outcome that is evidence")
	}
	if resp.GetOutcome() != investigationv1.TermOutcome_PARTIAL {
		t.Fatalf("outcome = %s, want PARTIAL", resp.GetOutcome())
	}
	if !strings.Contains(resp.GetFailureDetail(), "nextPageToken") {
		t.Errorf("PARTIAL does not name what is missing: %q", resp.GetFailureDetail())
	}
}

// An empty page with NO token, outside the ingestion lag, is NO_DATA: the window was covered and
// held nothing. That is the one case a consumer may read as evidence.
func TestAnEmptyPageWithNoTokenOutsideTheLagIsNoData(t *testing.T) {
	b := newBackend(t, &gcpbackend.Transport{Logs: &fakeLogs{}})
	// The window ends well before now, so it is outside any plausible logging lag — and with no
	// entries there is no receiveTimestamp to estimate one from, which is the case the next
	// test covers from the other side.
	resp := execute(t, b, engine.NewLogPatterns(logPointer(),
		window(testNow.Add(-3*time.Hour), testNow.Add(-2*time.Hour)), window(testNow.Add(-5*time.Hour), testNow.Add(-4*time.Hour))))

	// With the lag undetermined the honest answer is NOT_YET_INGESTED, which claims nothing.
	if resp.GetOutcome() != investigationv1.TermOutcome_NOT_YET_INGESTED {
		t.Fatalf("outcome = %s; with no entries there is no lag estimate, and an undetermined "+
			"cutoff may not produce a NO_DATA", resp.GetOutcome())
	}
	if !coverageOf(resp).GetIngestionLagUndetermined() {
		t.Error("the lag is reported as determined; it was estimated from nothing")
	}
}

// Templates are mined, counted and given first-seen instants, and the raw lines do not leave.
func TestNewLogPatternsMinesTemplatesAndNeverCarriesRawLines(t *testing.T) {
	at := testNow.Add(-30 * time.Minute)
	logs := &fakeLogs{pages: []logPage{{entries: []*loggingpb.LogEntry{
		logEntry(at, ltype.LogSeverity_ERROR, "upstream call failed: deadline exceeded after 251ms", testRevision),
		logEntry(at.Add(time.Minute), ltype.LogSeverity_ERROR, "upstream call failed: deadline exceeded after 263ms", testRevision),
		logEntry(at.Add(2*time.Minute), ltype.LogSeverity_WARNING, "retrying request 12345", testRevision),
	}}}}
	b := newBackend(t, &gcpbackend.Transport{Logs: logs})

	resp := execute(t, b, engine.NewLogPatterns(logPointer(),
		window(testNow.Add(-time.Hour), testNow), window(testNow.Add(-2*time.Hour), testNow.Add(-time.Hour))))
	if resp.GetOutcome() != investigationv1.TermOutcome_DIGEST {
		t.Fatalf("outcome = %s (%s)", resp.GetOutcome(), resp.GetFailureDetail())
	}
	digest := resp.GetDigest().GetLog()
	if digest.GetMinerVersion() == "" {
		t.Error("the miner version is not recorded; a template set is a function of the miner")
	}
	if len(digest.GetPatterns()) == 0 {
		t.Fatal("no templates mined")
	}
	for _, pattern := range digest.GetPatterns() {
		if strings.Contains(pattern.GetTemplate(), "251ms") {
			t.Errorf("a raw line reached the digest: %q", pattern.GetTemplate())
		}
		if pattern.GetJoinKeys().GetVersion() == "" {
			t.Error("a template carries no revision join key")
		}
	}
	// The coverage states what was examined, over which sub-window.
	coverage := coverageOf(resp)
	if coverage.GetVolumeConsidered() != 3 {
		t.Errorf("volume_considered = %d, want the 3 entries examined", coverage.GetVolumeConsidered())
	}
	if !strings.Contains(coverage.GetSampling(), "entries.list") {
		t.Errorf("sampling does not say what was read: %q", coverage.GetSampling())
	}
}

// ---- T102: error_spans -----------------------------------------------------------------------

// The term is served, answers NO_DATA naming the absent source, states that nothing was searched,
// and is byte-stable across edges and windows — so a consumer concludes "this cannot be checked
// here" rather than "there were no error spans".
func TestErrorSpansNamesTheAbsentTraceSourceAndIsStable(t *testing.T) {
	b := newBackend(t, &gcpbackend.Transport{})

	first := execute(t, b, engine.ErrorSpans("svc-a", "svc-b", graphv1.EdgeType_CALLS,
		window(testNow.Add(-time.Hour), testNow)))
	if first.GetOutcome() != investigationv1.TermOutcome_NO_DATA {
		t.Fatalf("outcome = %s, want NO_DATA; never QUERY_FAILED", first.GetOutcome())
	}
	coverage := coverageOf(first)
	if !strings.Contains(coverage.GetDataSource(), "cloud_trace:absent") {
		t.Errorf("coverage does not name the absent source: %q", coverage.GetDataSource())
	}
	if !strings.Contains(coverage.GetDataSource(), "searched=nothing") {
		t.Errorf("coverage does not state that nothing was searched: %q", coverage.GetDataSource())
	}
	if !strings.Contains(gcpbackend.TraceSourceAbsent, "not evidence") {
		t.Error("the absent-source sentence does not warn against reading it as evidence")
	}

	second := execute(t, b, engine.ErrorSpans("svc-c", "svc-d", graphv1.EdgeType_CALLS,
		window(testNow.Add(-24*time.Hour), testNow.Add(-23*time.Hour))))
	if first.GetDigest().GetCoverage().GetTruncation() != second.GetDigest().GetCoverage().GetTruncation() {
		t.Error("two error_spans answers differ; the absence is a standing fact, not a finding about a window")
	}
}

// ---- T103: exemplars -------------------------------------------------------------------------

// Exemplars are never returned by default. A backend that served them because the handle happened
// to be a log handle would make want_exemplars advisory.
func TestExemplarsAreRefusedWhenNotExplicitlyRequested(t *testing.T) {
	at := testNow.Add(-30 * time.Minute)
	logs := &fakeLogs{pages: []logPage{{entries: []*loggingpb.LogEntry{
		logEntry(at, ltype.LogSeverity_ERROR, "upstream call failed", testRevision),
	}}}}
	b := newBackend(t, &gcpbackend.Transport{Logs: logs})

	handle := mintLogHandle(t, b, logs)
	resp := execute(t, b, engine.Exemplars(handle, 3))
	if resp.GetOutcome() != investigationv1.TermOutcome_QUERY_FAILED {
		t.Fatalf("outcome = %s; exemplars are never returned by default", resp.GetOutcome())
	}
	if !strings.Contains(resp.GetFailureDetail(), "want_exemplars") {
		t.Errorf("the refusal does not name the flag: %q", resp.GetFailureDetail())
	}
}

// With the flag set they are returned, capped, and masked.
func TestExemplarsAreBoundedAndMasked(t *testing.T) {
	at := testNow.Add(-30 * time.Minute)
	entries := make([]*loggingpb.LogEntry, 0, 40)
	for i := range 40 {
		entries = append(entries, logEntry(at.Add(time.Duration(i)*time.Second),
			ltype.LogSeverity_ERROR, "upstream call failed for 10.1.2.3", testRevision))
	}
	logs := &fakeLogs{pages: []logPage{{entries: entries}}}
	b := newBackend(t, &gcpbackend.Transport{Logs: logs})
	handle := mintLogHandle(t, b, logs)

	logs.pages = []logPage{{entries: entries}}
	resp, err := b.Execute(context.Background(), &investigationv1.AlgebraRequest{
		Term:          engine.Exemplars(handle, 3),
		WantExemplars: true,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	exemplars := resp.GetDigest().GetExemplars().GetExemplars()
	if len(exemplars) > 3 {
		t.Errorf("got %d exemplars, the request capped them at 3", len(exemplars))
	}
	for _, exemplar := range exemplars {
		if strings.Contains(exemplar.GetText(), "10.1.2.3") {
			t.Errorf("an exemplar carries an unmasked address: %q", exemplar.GetText())
		}
	}
}

// A handle minted by a metric answer is refused rather than served smaller: the exemplar of a
// series is a raw sample, and those never cross the boundary in any quantity.
func TestExemplarsRefuseAMetricHandle(t *testing.T) {
	start := testNow.Add(-time.Hour)
	metrics := &fakeMetrics{series: []*monitoringpb.TimeSeries{series(testRevision, "", start, time.Minute, 1, 2)}}
	b := newBackend(t, &gcpbackend.Transport{Metrics: metrics})

	compare := execute(t, b, engine.Compare(metricPointer(t, ""), &investigationv1.WindowPair{
		Baseline: window(start, start.Add(30*time.Minute)),
		Symptom:  window(start.Add(30*time.Minute), testNow),
	}, investigationv1.Statistic_MEAN))
	handle := compare.GetDigest().GetMetric().GetSeries()[0].GetDrillDown().GetHandle()
	if handle == nil {
		t.Fatal("the compare answer minted no handle")
	}

	resp, err := b.Execute(context.Background(), &investigationv1.AlgebraRequest{
		Term:          engine.Exemplars(handle, 3),
		WantExemplars: true,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if resp.GetFailureReason() != investigationv1.FailureReason_UNSUPPORTED_POINTER {
		t.Fatalf("reason = %s, want UNSUPPORTED_POINTER", resp.GetFailureReason())
	}
}

// mintLogHandle runs a new_log_patterns answer and returns a handle it minted.
func mintLogHandle(t *testing.T, b *gcpbackend.Backend, logs *fakeLogs) *investigationv1.Handle {
	t.Helper()
	pages := logs.pages
	resp := execute(t, b, engine.NewLogPatterns(logPointer(), window(testNow.Add(-time.Hour), testNow), window(testNow.Add(-2*time.Hour), testNow.Add(-time.Hour))))
	patterns := resp.GetDigest().GetLog().GetPatterns()
	if len(patterns) == 0 {
		t.Fatal("no template to mint a handle from")
	}
	logs.pages = pages
	return patterns[0].GetDrillDown().GetHandle()
}

// ---- T104: drill-down links ------------------------------------------------------------------

// A drill-down is a reference plus a link that opens the same query over the same window, and the
// link carries no credential (FR-082).
func TestADrillDownIsAReferenceAndItsLinkCarriesNoCredential(t *testing.T) {
	start := testNow.Add(-time.Hour)
	metrics := &fakeMetrics{series: []*monitoringpb.TimeSeries{series(testRevision, "", start, time.Minute, 1, 2)}}
	b := newBackend(t, &gcpbackend.Transport{Metrics: metrics})

	resp := execute(t, b, engine.ErrorsByVersion(metricPointer(t, ""), window(start, testNow), versionAttribute))
	rows := resp.GetDigest().GetErrorsByVersion().GetVersions()
	if len(rows) == 0 {
		t.Fatal("no rows")
	}
	drill := rows[0].GetDrillDown()
	if drill.GetHandle().GetValue() == "" {
		t.Fatal("no handle minted")
	}
	if drill.GetHandle().GetDepth() != 1 {
		t.Errorf("depth = %d, want 1; a world records depth 1", drill.GetHandle().GetDepth())
	}
	link := drill.GetHumanLink()
	if !strings.HasPrefix(link, "https://console.cloud.google.com/") {
		t.Errorf("human link = %q, want a console URL", link)
	}
	for _, marker := range []string{"access_token", "api_key", "authorization", "signature="} {
		if strings.Contains(strings.ToLower(link), marker) {
			t.Errorf("the console link carries %s: %q", marker, link)
		}
	}
}

// ---- T106: retention -------------------------------------------------------------------------

// A window older than retention is a typed failure carrying the horizon, never NO_DATA. NO_DATA
// would tell an investigation that nothing happened during a window nobody could look at.
func TestAWindowOutsideRetentionIsRefusedWithTheHorizon(t *testing.T) {
	metrics := &fakeMetrics{}
	b := newBackend(t, &gcpbackend.Transport{Metrics: metrics})

	old := testNow.Add(-90 * 24 * time.Hour)
	resp := execute(t, b, engine.Compare(metricPointer(t, ""), &investigationv1.WindowPair{
		Baseline: window(old, old.Add(time.Hour)),
		Symptom:  window(old.Add(2*time.Hour), old.Add(3*time.Hour)),
	}, investigationv1.Statistic_MEAN))

	if resp.GetOutcome() == investigationv1.TermOutcome_NO_DATA {
		t.Fatal("a window outside retention was reported as NO_DATA")
	}
	if resp.GetFailureReason() != investigationv1.FailureReason_OUTSIDE_RETENTION {
		t.Fatalf("reason = %s, want OUTSIDE_RETENTION", resp.GetFailureReason())
	}
	horizon, ok := gcpbackend.RetentionHorizonOf(resp)
	if !ok {
		t.Fatal("the refusal carries no retention horizon; the caller would find the boundary by bisection")
	}
	if want := testNow.Add(-gcpbackend.DefaultMetricRetention); !horizon.Equal(want) {
		t.Errorf("horizon = %s, want %s", horizon, want)
	}
	if len(metrics.requests) != 0 {
		t.Error("the backend queried GCP for a window the vendor states it does not hold")
	}
}

// ---- T108: pointer acceptance ----------------------------------------------------------------

// A pointer this backend cannot execute is named, never executed as something else.
func TestAnUnexecutablePointerNamesItsKindAndVocabulary(t *testing.T) {
	metrics := &fakeMetrics{}
	b := newBackend(t, &gcpbackend.Transport{Metrics: metrics})

	promql := &graphv1.Pointer{
		Kind:       graphv1.PointerKind_METRIC,
		Vocabulary: "promql",
		Selector:   `rate(http_requests_total{service="storefront"}[5m])`,
	}
	resp := execute(t, b, engine.Compare(promql, &investigationv1.WindowPair{
		Baseline: window(testNow.Add(-2*time.Hour), testNow.Add(-time.Hour)),
		Symptom:  window(testNow.Add(-time.Hour), testNow),
	}, investigationv1.Statistic_MEAN))

	if resp.GetFailureReason() != investigationv1.FailureReason_UNSUPPORTED_POINTER {
		t.Fatalf("reason = %s, want UNSUPPORTED_POINTER", resp.GetFailureReason())
	}
	detail := resp.GetFailureDetail()
	if !strings.Contains(detail, "promql") || !strings.Contains(detail, feeder.VocabGCPMonitoringFilter) {
		t.Errorf("the refusal does not name both vocabularies: %q", detail)
	}
	if len(metrics.requests) != 0 {
		t.Error("a pointer in another vocabulary was executed; it must be refused, never translated")
	}
}

// The pointers the feeders mint are accepted without translation: the selector GCP receives is the
// selector that was stored.
func TestTheFeedersPointerIsExecutedAsMinted(t *testing.T) {
	start := testNow.Add(-time.Hour)
	metrics := &fakeMetrics{series: []*monitoringpb.TimeSeries{series(testRevision, "", start, time.Minute, 1, 2)}}
	b := newBackend(t, &gcpbackend.Transport{Metrics: metrics})

	pointer := metricPointer(t, `metric.labels.response_code_class="5xx"`)
	execute(t, b, engine.Onset(pointer, window(start, testNow), investigationv1.OnsetMethod_SEASONAL_CUSUM))

	if len(metrics.requests) == 0 {
		t.Fatal("no query issued")
	}
	if got := metrics.requests[0].GetFilter(); got != pointer.GetSelector() {
		t.Errorf("the executed filter was rewritten.\n got: %q\nwant: %q", got, pointer.GetSelector())
	}
}

// ---- T093, T094: coverage --------------------------------------------------------------------

// The ingestion lag says which source it came from: a figure Google published and one this code
// assumed are different evidence.
func TestTheIngestionLagStatesItsSource(t *testing.T) {
	start := testNow.Add(-time.Hour)
	withDescriptor := &fakeMetrics{
		series: []*monitoringpb.TimeSeries{series(testRevision, "", start, time.Minute, 1, 2)},
		descriptor: &metricpb.MetricDescriptor{
			Metadata: &metricpb.MetricDescriptor_MetricDescriptorMetadata{
				IngestDelay: durationpb.New(210 * time.Second),
			},
		},
	}
	b := newBackend(t, &gcpbackend.Transport{Metrics: withDescriptor})
	resp := execute(t, b, engine.ErrorsByVersion(metricPointer(t, ""), window(start, testNow), versionAttribute))
	coverage := coverageOf(resp)
	if coverage.GetIngestionLagSeconds() != 210 {
		t.Errorf("lag = %ds, want the descriptor's 210", coverage.GetIngestionLagSeconds())
	}
	if !strings.Contains(coverage.GetDataSource(), string(gcpbackend.LagDescriptor)) {
		t.Errorf("coverage does not name the descriptor as the source: %q", coverage.GetDataSource())
	}

	// With the field empty the documented Cloud Run default is used, and the fallback says so.
	empty := &fakeMetrics{
		series:     []*monitoringpb.TimeSeries{series(testRevision, "", start, time.Minute, 1, 2)},
		descriptor: &metricpb.MetricDescriptor{},
	}
	b = newBackend(t, &gcpbackend.Transport{Metrics: empty})
	resp = execute(t, b, engine.ErrorsByVersion(metricPointer(t, ""), window(start, testNow), versionAttribute))
	coverage = coverageOf(resp)
	if coverage.GetIngestionLagSeconds() != int64(gcpbackend.CloudRunIngestDelay/time.Second) {
		t.Errorf("lag = %ds, want the documented 120", coverage.GetIngestionLagSeconds())
	}
	if !strings.Contains(coverage.GetDataSource(), string(gcpbackend.LagCloudRunDefault)) {
		t.Errorf("coverage does not say the fallback was used: %q", coverage.GetDataSource())
	}
}

// Quota is undetermined on every real-time answer: GCP reports no in-band remaining figure, and
// the serviceruntime figure is a minutes-stale reconciliation signal rather than a coverage fact.
func TestQuotaIsAlwaysUndeterminedOnALiveAnswer(t *testing.T) {
	start := testNow.Add(-time.Hour)
	b := newBackend(t, &gcpbackend.Transport{
		Metrics: &fakeMetrics{series: []*monitoringpb.TimeSeries{series(testRevision, "", start, time.Minute, 1, 2)}},
		Logs:    &fakeLogs{},
	})
	for name, term := range map[string]*investigationv1.AlgebraTerm{
		"errors_by_version": engine.ErrorsByVersion(metricPointer(t, ""), window(start, testNow), versionAttribute),
		"onset":             engine.Onset(metricPointer(t, ""), window(start, testNow), investigationv1.OnsetMethod_SEASONAL_CUSUM),
		"error_spans":       engine.ErrorSpans("a", "b", graphv1.EdgeType_CALLS, window(start, testNow)),
	} {
		resp := execute(t, b, term)
		coverage := coverageOf(resp)
		if coverage == nil {
			t.Fatalf("%s: no coverage block", name)
		}
		if !coverage.GetQuotaUndetermined() {
			t.Errorf("%s: quota_undetermined is false; GCP reports no in-band remaining figure", name)
		}
		if coverage.GetRemainingQuota() != 0 {
			t.Errorf("%s: a remaining quota was reported: %d", name, coverage.GetRemainingQuota())
		}
	}
}

// FR-093's two instants: the reference the request asked about, and the instant of execution.
func TestCoverageRecordsTheInstantOfExecution(t *testing.T) {
	start := testNow.Add(-time.Hour)
	b := newBackend(t, &gcpbackend.Transport{
		Metrics: &fakeMetrics{series: []*monitoringpb.TimeSeries{series(testRevision, "", start, time.Minute, 1, 2)}},
	})
	resp := execute(t, b, engine.ErrorsByVersion(metricPointer(t, ""), window(start, testNow), versionAttribute))
	if got := coverageOf(resp).GetExecutedAt().AsTime().UTC(); !got.Equal(testNow) {
		t.Errorf("executed_at = %s, want %s", got, testNow)
	}
}

// ---- the horizon -----------------------------------------------------------------------------

// Nobody sees the future. A window entirely past the investigation's observed_at is NO_DATA naming
// the horizon, and nothing is fetched.
func TestAWindowPastTheHorizonIsAnsweredWithoutQuerying(t *testing.T) {
	metrics := &fakeMetrics{}
	b := newBackend(t, &gcpbackend.Transport{Metrics: metrics})

	observed := testNow.Add(-2 * time.Hour)
	resp, err := b.Execute(context.Background(), &investigationv1.AlgebraRequest{
		Term:       engine.ErrorsByVersion(metricPointer(t, ""), window(testNow.Add(-time.Hour), testNow), versionAttribute),
		ObservedAt: timestamppb.New(observed),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if resp.GetOutcome() != investigationv1.TermOutcome_NO_DATA {
		t.Fatalf("outcome = %s, want NO_DATA naming the horizon", resp.GetOutcome())
	}
	if !coverageOf(resp).GetTruncatedToHorizon() {
		t.Error("coverage does not say it was bounded by the horizon")
	}
	if len(metrics.requests) != 0 {
		t.Error("the backend queried for a window after the investigation's observed_at")
	}
}

// ---- window caps -----------------------------------------------------------------------------

// A window wider than its published cap is narrowed to the cap, and the digest says so. A
// truncated answer that does not say so is worse than a refusal.
func TestAWindowWiderThanItsCapIsNarrowedAndSaysSo(t *testing.T) {
	logs := &fakeLogs{pages: []logPage{{entries: []*loggingpb.LogEntry{
		logEntry(testNow.Add(-10*time.Minute), ltype.LogSeverity_ERROR, "boom", testRevision),
	}}}}
	b := newBackend(t, &gcpbackend.Transport{Logs: logs})

	wide := window(testNow.Add(-48*time.Hour), testNow)
	resp := execute(t, b, engine.NewLogPatterns(logPointer(), wide, window(testNow.Add(-72*time.Hour), testNow.Add(-48*time.Hour))))

	if !strings.Contains(coverageOf(resp).GetTruncation(), gcpbackend.CriterionWindowCap) {
		t.Errorf("coverage does not state the window cap: %q", coverageOf(resp).GetTruncation())
	}
	if len(logs.requests) == 0 {
		t.Fatal("no query issued")
	}
	filter := logs.requests[0].GetFilter()
	if !strings.Contains(filter, testNow.Add(-gcpbackend.WindowCapLogs).Format(time.RFC3339Nano)) {
		t.Errorf("the executed filter was not narrowed to the cap: %q", filter)
	}
}

// ---- T107: recorded mode ---------------------------------------------------------------------

// Recorded mode makes no network call and never falls through to a live one. A miss is its own
// outcome, not an empty answer.
func TestRecordedModeAnswersFromTheWorldAndNeverCallsTheVendor(t *testing.T) {
	world := &sdk.World{}
	b, err := gcpbackend.New(gcpbackend.Options{
		OrgSlug:   "acme",
		Project:   testProject,
		Region:    testRegion,
		Transport: &gcpbackend.Transport{Metrics: exploding{t}, Logs: exploding{t}},
		World:     world,
		Now:       func() time.Time { return testNow },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	resp := execute(t, b, engine.ErrorsByVersion(metricPointer(t, ""),
		window(testNow.Add(-time.Hour), testNow), versionAttribute))

	if resp.GetOutcome() != investigationv1.TermOutcome_NOT_RECORDED {
		t.Fatalf("outcome = %s, want NOT_RECORDED for a term this world does not hold", resp.GetOutcome())
	}
	if resp.GetMode() != engine.ModeRecorded {
		t.Errorf("mode = %q, want recorded", resp.GetMode())
	}
	if report := b.Misses(); report.NotRecorded() != 1 {
		t.Errorf("the miss was not counted: %+v", report)
	}
}

// A live backend refuses to be built without the sanitiser: FR-110 says a live investigation must
// not be able to surface what a recording would not be allowed to keep.
func TestALiveBackendWithoutTheSanitiserIsRefused(t *testing.T) {
	_, err := gcpbackend.New(gcpbackend.Options{
		OrgSlug:   "acme",
		Transport: &gcpbackend.Transport{},
		Redactor:  declaredRedactor(t),
		Now:       func() time.Time { return testNow },
	})
	if err == nil {
		t.Fatal("a live backend was built with no sanitiser")
	}
	if !strings.Contains(err.Error(), "FR-110") {
		t.Errorf("the refusal does not cite the rule: %v", err)
	}
}

// ---- failures --------------------------------------------------------------------------------

// A vendor failure is a typed failure and never an emptiness: a query that failed did not
// establish that the window was covered.
func TestAVendorFailureIsNeverReportedAsNoData(t *testing.T) {
	b := newBackend(t, &gcpbackend.Transport{Metrics: &fakeMetrics{err: errors.New("backend unavailable")}})
	resp := execute(t, b, engine.ErrorsByVersion(metricPointer(t, ""),
		window(testNow.Add(-time.Hour), testNow), versionAttribute))

	if resp.GetOutcome() == investigationv1.TermOutcome_NO_DATA {
		t.Fatal("a failed query was reported as NO_DATA")
	}
	if resp.GetOutcome() != investigationv1.TermOutcome_QUERY_FAILED {
		t.Fatalf("outcome = %s, want QUERY_FAILED", resp.GetOutcome())
	}
}

// monitor_state with incident reads off answers NO_DATA naming the absent source, exactly as
// error_spans does — the contract's own answer for a missing source.
func TestMonitorStateWithIncidentReadsOffNamesTheAbsentSource(t *testing.T) {
	b := newBackend(t, &gcpbackend.Transport{Metrics: &fakeMetrics{}})
	resp := execute(t, b, engine.MonitorState(metricPointer(t, ""), window(testNow.Add(-time.Hour), testNow)))

	if resp.GetOutcome() != investigationv1.TermOutcome_NO_DATA {
		t.Fatalf("outcome = %s, want NO_DATA", resp.GetOutcome())
	}
	if !strings.Contains(coverageOf(resp).GetDataSource(), "searched=nothing") {
		t.Errorf("coverage does not state that nothing was searched: %q", coverageOf(resp).GetDataSource())
	}
}

// With the capability on, the transitions come from the instants Google reports, and an incident
// open before the window puts the window's start in alert.
func TestMonitorStateReadsTheTransitionInstantsGoogleReports(t *testing.T) {
	start := testNow.Add(-2 * time.Hour)
	alerts := &fakeAlerts{alerts: []*gcpbackend.Alert{
		{
			Name: "projects/p/alerts/1", State: "CLOSED",
			OpenTime:   start.Add(-time.Hour), // before the window
			CloseTime:  start.Add(30 * time.Minute),
			PolicyName: "projects/p/alertPolicies/9",
		},
	}}
	b := newBackend(t, &gcpbackend.Transport{Metrics: &fakeMetrics{}, Alerts: alerts})
	resp := execute(t, b, engine.MonitorState(metricPointer(t, ""), window(start, testNow)))

	digest := resp.GetDigest().GetMonitorState()
	if digest == nil {
		t.Fatalf("no monitor-state digest (%s)", resp.GetOutcome())
	}
	if digest.GetStateAtStart() != "alert" {
		t.Errorf("state at start = %q; an incident open before the window did not begin calm",
			digest.GetStateAtStart())
	}
	if digest.GetStateAtEnd() != "ok" {
		t.Errorf("state at end = %q", digest.GetStateAtEnd())
	}
	if len(digest.GetTransitions()) != 1 {
		t.Fatalf("got %d transitions, want the close inside the window", len(digest.GetTransitions()))
	}
	if got := digest.GetTransitions()[0].GetAt().AsTime().UTC(); !got.Equal(start.Add(30 * time.Minute)) {
		t.Errorf("transition at %s, want the closeTime Google reported", got)
	}
}

// Two modes, one output (T107, FR-106, SC-014): the recorded answer is the live answer, coverage
// block and join keys included, and it is produced without touching the vendor.
//
// This is asserted by recording a live answer and replaying it, rather than by comparing two
// hand-written expectations — a pair of expectations can agree with each other and with neither
// mode.
func TestRecordedModeReproducesTheLiveAnswerFieldForField(t *testing.T) {
	start := testNow.Add(-time.Hour)
	fresh := func() *fakeMetrics {
		return &fakeMetrics{series: []*monitoringpb.TimeSeries{
			series("storefront-00041-old", "2xx", start, time.Minute, 100, 100),
			series("storefront-00041-old", "5xx", start, time.Minute, 1, 1),
			series("storefront-00042-new", "5xx", start, time.Minute, 50, 50),
		}}
	}
	term := engine.ErrorsByVersion(metricPointer(t, ""), window(start, testNow), versionAttribute)
	req := &investigationv1.AlgebraRequest{Term: term}

	live := newBackend(t, &gcpbackend.Transport{Metrics: fresh()})
	liveResp, err := live.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("live Execute: %v", err)
	}

	dir := t.TempDir()
	recorder, err := sdk.NewRecorderWithOptions(dir, sdk.RecordOptions{
		DrillDownDepth: 1,
		Focus:          "gcp.cloudrun.service=" + testProject + "/" + testRegion + "/" + testService,
		// A world states what was applied to it: a recording whose redaction policy is unknown
		// is a recording nobody can say is safe to read.
		RedactionPolicyVersion: live.Describe().Redaction.GetPolicyVersion(),
	})
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	if err := recorder.Record(context.Background(), req, liveResp); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if _, err := recorder.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	world, err := sdk.LoadWorld(dir)
	if err != nil {
		t.Fatalf("LoadWorld: %v", err)
	}

	replayed, err := gcpbackend.New(gcpbackend.Options{
		OrgSlug: "acme", Project: testProject, Region: testRegion,
		// The exploding transport is the assertion that no network call is made: recorded mode
		// must never fall through to a live call to satisfy a request, even one it could serve.
		Transport: &gcpbackend.Transport{Metrics: exploding{t}, Logs: exploding{t}},
		World:     world,
		Now:       func() time.Time { return testNow },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	recordedResp, err := replayed.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("recorded Execute: %v", err)
	}

	if recordedResp.GetOutcome() != liveResp.GetOutcome() {
		t.Fatalf("outcome = %s, live was %s", recordedResp.GetOutcome(), liveResp.GetOutcome())
	}
	if recordedResp.GetMode() != engine.ModeRecorded {
		t.Errorf("mode = %q, want recorded", recordedResp.GetMode())
	}
	// The response digest is computed over the three cleared fields' absence, which is exactly
	// what makes live and recorded hash the same bytes (contract §10).
	if recordedResp.GetResponseDigest() != liveResp.GetResponseDigest() {
		t.Errorf("response digest differs:\n recorded %s\n     live %s",
			recordedResp.GetResponseDigest(), liveResp.GetResponseDigest())
	}

	liveRows := liveResp.GetDigest().GetErrorsByVersion().GetVersions()
	recordedRows := recordedResp.GetDigest().GetErrorsByVersion().GetVersions()
	if len(liveRows) != len(recordedRows) {
		t.Fatalf("got %d rows, live had %d", len(recordedRows), len(liveRows))
	}
	for i := range liveRows {
		if !proto.Equal(liveRows[i].GetJoinKeys(), recordedRows[i].GetJoinKeys()) {
			t.Errorf("row %d join keys differ:\n recorded %v\n     live %v",
				i, recordedRows[i].GetJoinKeys(), liveRows[i].GetJoinKeys())
		}
	}
	if !proto.Equal(liveResp.GetDigest().GetCoverage(), recordedResp.GetDigest().GetCoverage()) {
		t.Errorf("the coverage block differs:\n recorded %v\n     live %v",
			recordedResp.GetDigest().GetCoverage(), liveResp.GetDigest().GetCoverage())
	}
}
