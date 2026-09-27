// SPDX-License-Identifier: Apache-2.0

package datadog_test

import (
	"context"
	"encoding/json"
	"fmt"
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
	"github.com/Pierre-Theophile/aisre/pkg/feeder/emit"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/record"
)

// datadog-backend-logs-01: the Datadog backend over a synthetic structural twin (005 T048, T051).
//
// One service, `checkout` in production, whose logs live in Datadog and are stamped with the deployed
// commit. At 14:10 a new commit ships and a payment error starts; from 14:15 a crash handler writes
// errors with no version stamp at all. The world is the cross product of the telemetry algebra over
// the service's two pointers (its logs and its error monitor) and one window pair around the deploy,
// plus every drill-down and exemplar request behind the handles those answers minted, followed once.
//
// Two things are asserted on every run, not only when the fixture is regenerated:
//
//   - the recorded world answers every question in the cross product, and answers it exactly as the
//     live twin does, field for field, coverage and join keys included (SC-019);
//   - the twin's request log holds no span or APM request, and a question outside the telemetry
//     algebra is refused naming what is available (SC-023, SC-021).
//
// The graph half is the connector's own LogSourceEvents for the service, and one event the graph must
// refuse: the same node carrying per-minute error counts in a property, which is telemetry, and which
// the zero-payload check (FR-078) must turn away with `telemetry_payload`.
//
// Synthetic structural twin: no identifier is derived from the organisation.

const (
	genFixturesEnv     = "SRE_AGENT_GEN_FIXTURES"
	backendFixture     = "fixtures/datadog-backend-logs-01"
	backendOrg         = "twin"
	backendMonitorID   = "7"
	backendOldCommit   = "3f1c9a0b7d2e4f6a8b0c1d2e3f4a5b6c7d8e9f01"
	backendNewCommit   = "9e8d7c6b5a4f3e2d1c0b9a8f7e6d5c4b3a2f1e0d"
	backendServiceName = "checkout"
)

var (
	backendDeploy = time.Date(2026, 9, 21, 14, 10, 0, 0, time.UTC)
	backendFrom   = backendDeploy.Add(-time.Hour)       // 13:10: the start of the onset search
	backendTo     = backendDeploy.Add(20 * time.Minute) // 14:30: the end of every window
)

// backendLines is the twin's log: a steady service on the old commit, then the new commit with its
// payment error, then the unstamped crash-handler lines.
func backendLines() []twinLine {
	hosts := []string{"web-1", "web-2"}
	var lines []twinLine
	for m := backendFrom; m.Before(backendTo); m = m.Add(time.Minute) {
		minute := m.Minute()
		host := hosts[minute%2]
		version := backendOldCommit
		if !m.Before(backendDeploy) {
			version = backendNewCommit
		}
		lines = append(lines, twinLine{at: m, msg: fmt.Sprintf("request served in %d ms", 40+minute%17),
			status: "info", host: host, version: version})
		if minute%3 == 0 {
			lines = append(lines, twinLine{at: m.Add(5 * time.Second), msg: fmt.Sprintf("cache miss for key user-%d", minute),
				status: "info", host: host, version: version})
		}
		if m.Before(backendDeploy) && minute%13 == 0 {
			lines = append(lines, twinLine{at: m.Add(20 * time.Second), msg: "upstream timeout after 3000 ms",
				status: "error", host: host, version: version})
		}
		if !m.Before(backendDeploy) {
			for i, offset := range []time.Duration{10 * time.Second, 40 * time.Second} {
				lines = append(lines, twinLine{at: m.Add(offset),
					msg:    fmt.Sprintf("payment provider refused order %d", 1000+minute*2+i),
					status: "error", host: hosts[i], version: version})
			}
		}
		if !m.Before(backendDeploy.Add(5*time.Minute)) && minute%4 == 0 {
			lines = append(lines, twinLine{at: m.Add(50 * time.Second), msg: "worker panicked: nil map write",
				status: "error", host: host}) // no version stamp: the crash handler's own logger
		}
	}
	return lines
}

func backendMonitor() string {
	return fmt.Sprintf(`{"id":%s,"name":"checkout errors","type":"log alert","overall_state":"Alert",`+
		`"state":{"groups":{"env:production":{"status":"Alert","last_triggered_ts":%d,"last_resolved_ts":%d},`+
		`"env:staging":{"status":"OK","last_resolved_ts":%d}}}}`, backendMonitorID,
		backendDeploy.Add(4*time.Minute).Unix(), backendFrom.Add(-5*time.Minute).Unix(),
		backendFrom.Add(-24*time.Hour).Unix())
}

func backendLogPointer() *graphv1.Pointer {
	return &graphv1.Pointer{
		Kind: graphv1.PointerKind_LOG, BackendKind: "datadog", Vocabulary: feeder.VocabDatadogLogs,
		Selector: "service:checkout env:production", JoinKeys: map[string]string{"version": "version"},
	}
}

func backendMonitorPointer() *graphv1.Pointer {
	return &graphv1.Pointer{
		Kind: graphv1.PointerKind_LOG, BackendKind: "datadog", Vocabulary: feeder.VocabDatadogMonitor,
		Selector:   `logs("service:checkout env:production status:error").index("*").rollup("count").last("5m") > 10`,
		Attributes: map[string]string{feeder.AttrDatadogMonitorID: backendMonitorID},
	}
}

// backendTerms is the first level of the cross product: every telemetry term over every pointer it
// applies to and the window grid. The grid is one pair, symmetric around the deploy.
func backendTerms() []*engine.Term {
	pair := engine.NewWindowPair(backendDeploy, 20*time.Minute)
	window := engine.NewWindow(backendDeploy.Add(-20*time.Minute), backendTo)
	logs := backendLogPointer()
	terms := make([]*engine.Term, 0, 12)
	for _, s := range []investigationv1.Statistic{investigationv1.Statistic_COUNT, investigationv1.Statistic_RATE,
		investigationv1.Statistic_ERROR_RATE, investigationv1.Statistic_P95} {
		terms = append(terms, engine.Compare(logs, pair, s))
	}
	return append(terms,
		engine.Onset(logs, engine.NewWindow(backendFrom, backendTo), investigationv1.OnsetMethod_SEASONAL_CUSUM),
		engine.NewLogPatterns(logs, pair.GetSymptom(), pair.GetBaseline()),
		engine.ErrorsByVersion(logs, window, "version"),
		engine.MonitorState(backendMonitorPointer(), window),
		engine.ErrorSpans("checkout", "payments", graphv1.EdgeType_CALLS, window),
	)
}

// handlesIn are the drill-down handles an answer minted.
func handlesIn(resp *engine.Response) []*investigationv1.Handle {
	var out []*investigationv1.Handle
	d := resp.GetDigest()
	for _, v := range d.GetErrorsByVersion().GetVersions() {
		if h := v.GetDrillDown().GetHandle(); h != nil {
			out = append(out, h)
		}
	}
	for _, p := range d.GetLog().GetPatterns() {
		if h := p.GetDrillDown().GetHandle(); h != nil {
			out = append(out, h)
		}
	}
	return out
}

// crossProduct runs the whole world against b: the first level, then each minted handle's drill-down
// and exemplars, once. It returns every request with its answer, in a stable order.
func crossProduct(t *testing.T, b *ddbackend.Backend, terms []*engine.Term) ([]*engine.Request, []*engine.Response) {
	t.Helper()
	var reqs []*engine.Request
	var resps []*engine.Response
	run := func(req *engine.Request) *engine.Response {
		resp, err := b.Execute(context.Background(), req)
		if err != nil {
			t.Fatalf("%s: %v", engine.TermName(req.GetTerm()), err)
		}
		reqs, resps = append(reqs, req), append(resps, resp)
		return resp
	}
	var handles []*investigationv1.Handle
	for _, term := range terms {
		handles = append(handles, handlesIn(run(&engine.Request{Term: term}))...)
	}
	for _, h := range handles {
		run(&engine.Request{Term: engine.DrillDown(h)})
		run(&engine.Request{Term: engine.Exemplars(h, 3), WantExemplars: true})
	}
	return reqs, resps
}

func backendLive(t *testing.T, lines []twinLine) (*ddbackend.Backend, *twin) {
	t.Helper()
	tw := &twin{t: t, lines: lines, monitor: backendMonitor()}
	b, _ := liveBackend(t, tw.ServeHTTP)
	return b, tw
}

// backendRecorded answers from the committed world, over a client whose every call fails the test:
// recorded mode must never fall through to the network.
func backendRecorded(t *testing.T, fixture string) *ddbackend.Backend {
	t.Helper()
	world, err := sdk.LoadWorld(filepath.Join(repoRoot(t), fixture, "world"))
	if err != nil {
		t.Fatalf("load the world (regenerate with %s=1): %v", genFixturesEnv, err)
	}
	b, err := ddbackend.New(ddbackend.Options{OrgSlug: backendOrg, World: world, Now: func() time.Time { return at }})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestGenerateDatadogBackendLogsFixture(t *testing.T) {
	if os.Getenv(genFixturesEnv) == "" {
		t.Skipf("set %s=1 to regenerate %s", genFixturesEnv, backendFixture)
	}
	generateBackendFixture(t, backendFixture, backendLines(), backendTerms(), graphHalf{
		family: "datadog-backend", refusal: true,
		description: "The Datadog telemetry backend over a synthetic structural twin. `checkout` in " +
			"production stamps the deployed commit on its logs; at 14:10 a new commit ships and a payment " +
			"error starts, and from 14:15 a crash handler writes errors with no stamp. world/ is the cross " +
			"product of the telemetry algebra over the service's log and monitor pointers and one window " +
			"pair around the deploy, plus every drill-down and exemplar behind the handles those answers " +
			"minted, followed once; internal/backends/datadog asserts on every run that it answers every " +
			"question exactly as the live twin does and that no span or APM call was made. The graph half " +
			"is the connector's own log-source events and one event the graph must refuse: the service node " +
			"carrying per-minute error counts in a property, which is telemetry (FR-078). Synthetic " +
			"structural twin: no identifier is derived from the organisation.",
	})
}

// graphHalf is what a backend fixture's events and manifest say.
type graphHalf struct {
	family, description string
	// refusal adds the telemetry-carrying event the graph must refuse.
	refusal bool
}

// generateBackendFixture writes a backend fixture: the graph half, then the world recorded from the
// live twin over terms and the handles they mint.
func generateBackendFixture(t *testing.T, fixture string, lines []twinLine, terms []*engine.Term, half graphHalf) {
	t.Helper()
	dir := filepath.Join(repoRoot(t), fixture)
	for _, generated := range []string{"world", "events.jsonl", "rejected.jsonl", "manifest.yaml"} {
		if err := os.RemoveAll(filepath.Join(dir, generated)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeBackendGraphHalf(t, dir, filepath.Base(fixture), half)

	live, _ := backendLive(t, lines)
	recorder, err := sdk.NewRecorderWithOptions(filepath.Join(dir, "world"), sdk.RecordOptions{
		DrillDownDepth:         1,
		WindowGrid:             []*investigationv1.WindowPair{engine.NewWindowPair(backendDeploy, 20*time.Minute)},
		RedactionPolicyVersion: live.Describe().Redaction.GetPolicyVersion(),
		Focus:                  ddfeeder.NSService + "=production/" + backendServiceName,
	})
	if err != nil {
		t.Fatal(err)
	}
	reqs, resps := crossProduct(t, live, terms)
	for i := range reqs {
		if err := recorder.Record(context.Background(), reqs[i], resps[i]); err != nil {
			t.Fatalf("record %s: %v", engine.TermName(reqs[i].GetTerm()), err)
		}
	}
	if _, err := recorder.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Logf("%s: %d recorded answers", fixture, len(reqs))
}

// writeBackendGraphHalf writes the events, the refusal and the manifest.
func writeBackendGraphHalf(t *testing.T, dir, id string, half graphHalf) {
	t.Helper()
	desc, err := ddfeeder.Describe(backendOrg)
	if err != nil {
		t.Fatal(err)
	}
	observed := backendFrom.Add(-10 * time.Minute)
	memory := emit.NewMemoryEmitter(desc, emit.WithMemoryClock(func() time.Time { return observed }))
	events := record.Emitter(memory, dir)
	src := ddfeeder.LogSource{Env: "production", Service: backendServiceName}
	batch, err := ddfeeder.LogSourceEvents(desc, src, observed)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range batch {
		if _, err := events.Emit(t.Context(), ev); err != nil {
			t.Fatalf("emit %s: %v", ev.GetEventId(), err)
		}
	}
	if err := events.Err(); err != nil {
		t.Fatal(err)
	}

	refusal := map[string]any{
		"eventId": desc.SourceID + ":telemetry-error-counts-1", "idempotencyKey": desc.SourceID + ":telemetry-error-counts-1",
		"schemaVersion": feeder.SchemaVersion, "sourceId": desc.SourceID,
		"sourceObservedAt": observed.Add(time.Minute).Format(time.RFC3339),
		"upsertNode": map[string]any{
			"displayName": backendServiceName, "type": "SERVICE", "validAt": observed.Format(time.RFC3339),
			"ref": map[string]any{"namespace": ddfeeder.NSService, "value": src.Ref().GetValue()},
			"props": map[string]any{
				"service.name":                 backendServiceName,
				"sre.datadog.errors_by_minute": []int{0, 1, 0, 42, 45, 44},
			},
		},
	}
	encoded, err := json.Marshal(refusal)
	if err != nil {
		t.Fatal(err)
	}
	pinned := backendTo.Add(time.Hour).Format(time.RFC3339)
	manifest := record.Manifest{
		ID: id, Family: half.family, Description: half.description,
		Sources: []record.ManifestSource{record.SourceOf(desc)},
		Start:   observed.Add(-time.Minute),
		End:     backendTo.Add(time.Hour),
	}
	if half.refusal {
		manifest.ExpectRejected = []record.Rejection{
			{EventID: desc.SourceID + ":telemetry-error-counts-1", ReasonCode: "telemetry_payload"},
		}
	}
	if err := record.WriteManifest(dir, manifest); err != nil {
		t.Fatal(err)
	}
	extra := ""
	if half.refusal {
		if err := os.WriteFile(filepath.Join(dir, "rejected.jsonl"), append(encoded, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		extra = "rejected_events: rejected.jsonl\n"
	}
	appendToManifest(t, dir, extra+`
queries:
  # The service node as the connector states it: its log-service correlation, and no property of it
  # holding a measurement.
  - name: checkout-log-service
    kind: subgraph
    focus: `+ddfeeder.NSService+"="+src.Ref().GetValue()+`
    valid_at: `+pinned+`
    observed_at: `+pinned+`
    hops: 1
    direction: both
`)
}

func appendToManifest(t *testing.T, dir, text string) {
	t.Helper()
	path := filepath.Join(dir, "manifest.yaml")
	existing, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(existing, []byte(text)...), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The recorded world answers the whole cross product, and answers it as the live twin does, field for
// field (SC-019). A missing answer is NOT_RECORDED, which fails here rather than in an investigation.
func TestTheRecordedWorldAnswersLikeTheLiveTwin(t *testing.T) {
	t.Parallel()
	live, tw := backendLive(t, backendLines())
	liveReqs, liveResps := crossProduct(t, live, backendTerms())
	recordedReqs, recordedResps := crossProduct(t, backendRecorded(t, backendFixture), backendTerms())

	if len(recordedReqs) != len(liveReqs) {
		t.Fatalf("the recorded cross product has %d questions, the live one %d", len(recordedReqs), len(liveReqs))
	}
	outcomes := map[string]int{}
	for i := range liveResps {
		name := engine.TermName(liveReqs[i].GetTerm())
		l, r := liveResps[i], recordedResps[i]
		outcomes[name+"/"+l.GetOutcome().String()]++
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
	// The story the fixture tells must still be in it, or the identity above proves nothing useful.
	for _, want := range []string{"errors_by_version/DIGEST", "new_log_patterns/DIGEST", "onset/DIGEST",
		"monitor_state/DIGEST", "error_spans/NO_DATA", "compare/QUERY_FAILED", "drill_down/DIGEST", "exemplars/DIGEST"} {
		if outcomes[want] == 0 {
			t.Errorf("no %s answer in the cross product: %v", want, outcomes)
		}
	}

	// SC-023: no span or APM request in the whole run.
	for _, p := range tw.requests() {
		if strings.Contains(p, "/spans") || strings.Contains(p, "/apm") || strings.Contains(p, "/trace") {
			t.Errorf("a span or APM request was made: %s", p)
		}
	}
}

// The story's facts, read off the recorded answers: the new commit's group is the worst and names its
// commit; the unstamped crash-handler lines are their own group; the payment error is a new pattern.
func TestTheWorldTellsTheDeployStory(t *testing.T) {
	t.Parallel()
	b := backendRecorded(t, backendFixture)
	window := engine.NewWindow(backendDeploy.Add(-20*time.Minute), backendTo)
	resp, err := b.Execute(context.Background(), &engine.Request{Term: engine.ErrorsByVersion(backendLogPointer(), window, "version")})
	if err != nil {
		t.Fatal(err)
	}
	rows := resp.GetDigest().GetErrorsByVersion().GetVersions()
	if len(rows) != 3 {
		t.Fatalf("groups %v", rows)
	}
	if rows[0].GetVersion() != "" || rows[0].GetDeployRefAbsentReason() != investigationv1.DeployRefAbsentReason_NOT_A_STABLE_IDENTIFIER {
		t.Errorf("the unstamped group (every line an error) is not first: %v", rows[0])
	}
	if rows[1].GetVersion() != backendNewCommit || rows[1].GetDeployRef().GetValue() != backendNewCommit {
		t.Errorf("the new commit's group: %v", rows[1])
	}
	if rows[2].GetErrorRate() >= rows[1].GetErrorRate() {
		t.Errorf("the old commit's error rate is not below the new one's: %v", rows)
	}

	pair := engine.NewWindowPair(backendDeploy, 20*time.Minute)
	resp, err = b.Execute(context.Background(), &engine.Request{Term: engine.NewLogPatterns(backendLogPointer(), pair.GetSymptom(), pair.GetBaseline())})
	if err != nil {
		t.Fatal(err)
	}
	first := resp.GetDigest().GetLog().GetPatterns()[0]
	if !first.GetNewInWindow() || !strings.HasPrefix(first.GetTemplate(), "payment provider refused order") {
		t.Errorf("the first pattern is not the new payment error: %v", first)
	}
}

// SC-021: a question outside the telemetry algebra is refused naming what is available.
func TestAnOutOfAlgebraQuestionNamesBothLists(t *testing.T) {
	t.Parallel()
	b, tw := backendLive(t, backendLines())
	resp, err := b.Execute(context.Background(), &engine.Request{Term: engine.GraphSubgraph(&graphv1.SubgraphRequest{})})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetFailureReason() != investigationv1.FailureReason_OUTSIDE_ALGEBRA {
		t.Fatalf("got %v / %v", resp.GetOutcome(), resp.GetFailureReason())
	}
	for _, want := range []string{"subgraph", "compare", "errors_by_version"} {
		if !strings.Contains(resp.GetFailureDetail(), want) {
			t.Errorf("the refusal does not name %q: %s", want, resp.GetFailureDetail())
		}
	}
	if len(tw.requests()) != 0 {
		t.Errorf("an out-of-algebra question reached Datadog: %v", tw.requests())
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test")
		}
		dir = parent
	}
}
