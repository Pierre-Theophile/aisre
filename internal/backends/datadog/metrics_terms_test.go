// SPDX-License-Identifier: Apache-2.0

package datadog_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// compare states the error rate of each window from one aggregate grouped by status, and the
// comparison between them (contract §2).
func TestCompareStatesTheLogErrorRate(t *testing.T) {
	t.Parallel()
	symptomStart := at.Add(-time.Hour)
	b, calls := liveBackend(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Filter struct{ From string } `json:"filter"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		from, _ := time.Parse(time.RFC3339Nano, req.Filter.From)
		errs, infos := 1, 99 // the baseline
		if !from.Before(symptomStart) {
			errs, infos = 30, 70
		}
		_, _ = fmt.Fprintf(w, `{"data":{"buckets":[{"by":{"status":"error"},"computes":{"c0":%d}},`+
			`{"by":{"status":"info"},"computes":{"c0":%d}}]},"meta":{"status":"done"}}`, errs, infos)
	})
	resp, err := b.Execute(context.Background(), request(engine.Compare(logPointer(),
		engine.NewWindowPair(symptomStart, time.Hour), investigationv1.Statistic_ERROR_RATE)))
	if err != nil {
		t.Fatal(err)
	}
	c := resp.GetDigest().GetMetric().GetComparisons()
	if resp.GetOutcome() != investigationv1.TermOutcome_DIGEST || len(c) != 1 {
		t.Fatalf("outcome %v / %v %v", resp.GetOutcome(), resp.GetFailureReason(), c)
	}
	if c[0].GetBaseline() != 0.01 || c[0].GetSymptom() != 0.3 || c[0].GetDirection() != "up" || !c[0].GetSeparable() {
		t.Errorf("comparison %v", c[0])
	}
	if *calls != 2 {
		t.Errorf("%d calls, want one aggregate per window", *calls)
	}
	if !strings.Contains(resp.GetDigest().GetCoverage().GetTruncation(), "log_derived") {
		t.Errorf("coverage does not say the counts are log-derived")
	}
}

// A latency percentile is not a property of a log count and is refused, not approximated.
func TestCompareRefusesALatencyPercentileOverLogs(t *testing.T) {
	t.Parallel()
	b, calls := liveBackend(t, func(http.ResponseWriter, *http.Request) {})
	resp, err := b.Execute(context.Background(), request(engine.Compare(logPointer(),
		engine.NewWindowPair(at.Add(-time.Hour), time.Hour), investigationv1.Statistic_P95)))
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetOutcome() != investigationv1.TermOutcome_QUERY_FAILED || *calls != 0 {
		t.Fatalf("got %v with %d calls", resp.GetOutcome(), *calls)
	}
}

// onset finds the minute error lines began to rise, from a zero-filled per-minute count; the series
// itself never leaves the backend.
func TestOnsetFindsWhenErrorLinesRose(t *testing.T) {
	t.Parallel()
	start := at.Add(-3 * time.Hour)
	rise := at.Add(-40 * time.Minute)
	b, _ := liveBackend(t, func(w http.ResponseWriter, _ *http.Request) {
		var points []string
		for m := start; m.Before(at); m = m.Add(time.Minute) {
			v := 0 // quiet minutes are absent, as Datadog omits them
			switch {
			case !m.Before(rise):
				v = 40 + m.Minute()%3
			case m.Minute()%7 == 0:
				v = 1
			}
			if v > 0 {
				points = append(points, fmt.Sprintf(`{"time":%q,"value":%d}`, m.Format(time.RFC3339), v))
			}
		}
		_, _ = io.WriteString(w, `{"data":{"buckets":[{"by":{},"computes":{"c0":[`+strings.Join(points, ",")+
			`]}}]},"meta":{"status":"done"}}`)
	})
	resp, err := b.Execute(context.Background(), request(engine.Onset(logPointer(),
		engine.NewWindow(start, at), investigationv1.OnsetMethod_SEASONAL_CUSUM)))
	if err != nil {
		t.Fatal(err)
	}
	o := resp.GetDigest().GetOnset()
	if resp.GetOutcome() != investigationv1.TermOutcome_DIGEST || o.GetUnavailable() {
		t.Fatalf("outcome %v, onset %v", resp.GetOutcome(), o)
	}
	if got := o.GetEstimatedOnset().AsTime(); got.Sub(rise).Abs() > 2*time.Minute {
		t.Errorf("onset %s, want about %s", got, rise)
	}
}

func monitorPointer(id string) *graphv1.Pointer {
	return &graphv1.Pointer{
		Kind: graphv1.PointerKind_LOG, BackendKind: "datadog", Vocabulary: feeder.VocabDatadogMonitor,
		Selector:   `logs("service:voice-agent status:error").index("*").rollup("count").last("5m") > 10`,
		Attributes: map[string]string{feeder.AttrDatadogMonitorID: id},
	}
}

// monitor_state reports the stated instants inside the window, and a state at an edge only where
// it can be derived (contract §3.3).
func TestMonitorStateReportsStatedInstantsOnly(t *testing.T) {
	t.Parallel()
	triggered, resolved := at.Add(-30*time.Minute).Unix(), at.Add(time.Hour).Unix()
	b, _ := liveBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/api/v1/monitor/42") || r.URL.Query().Get("group_states") != "all" {
			t.Errorf("request %s", r.URL)
		}
		_, _ = fmt.Fprintf(w, `{"id":42,"name":"errors","type":"log alert","overall_state":"OK","state":{"groups":{`+
			`"env:production":{"status":"OK","last_triggered_ts":%d,"last_resolved_ts":%d},`+
			`"env:staging":{"status":"OK","last_resolved_ts":%d}}}}`, triggered, resolved, at.Add(-48*time.Hour).Unix())
	})
	resp, err := b.Execute(context.Background(), request(engine.MonitorState(monitorPointer("42"),
		engine.NewWindow(at.Add(-time.Hour), at))))
	if err != nil {
		t.Fatal(err)
	}
	d := resp.GetDigest().GetMonitorState()
	if resp.GetOutcome() != investigationv1.TermOutcome_DIGEST {
		t.Fatalf("outcome %v / %v", resp.GetOutcome(), resp.GetFailureReason())
	}
	if len(d.GetTransitions()) != 1 || d.GetTransitions()[0].GetToState() != "Alert" {
		t.Fatalf("transitions %v", d.GetTransitions())
	}
	// Production resolved after the window, so its state at the end is not derivable; staging's is.
	if d.GetStateAtEnd() != "unknown" || len(d.GetPerGroupState()) != 2 {
		t.Errorf("edges %q, groups %v", d.GetStateAtEnd(), d.GetPerGroupState())
	}
	if !strings.Contains(resp.GetDigest().GetCoverage().GetSampling(), "not a history") {
		t.Errorf("coverage does not say the history is partial: %q", resp.GetDigest().GetCoverage().GetSampling())
	}
}

// A monitor pointer without its id is refused naming the attribute.
func TestAMonitorPointerWithoutAnIDIsRefused(t *testing.T) {
	t.Parallel()
	b, calls := liveBackend(t, func(http.ResponseWriter, *http.Request) {})
	resp, err := b.Execute(context.Background(), request(engine.MonitorState(monitorPointer(""),
		engine.NewWindow(at.Add(-time.Hour), at))))
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetFailureReason() != investigationv1.FailureReason_UNSUPPORTED_POINTER || *calls != 0 ||
		!strings.Contains(resp.GetFailureDetail(), feeder.AttrDatadogMonitorID) {
		t.Fatalf("got %v %q with %d calls", resp.GetFailureReason(), resp.GetFailureDetail(), *calls)
	}
}
