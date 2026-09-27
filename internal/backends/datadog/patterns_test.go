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

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	ddbackend "github.com/Pierre-Theophile/aisre/internal/backends/datadog"
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
)

// twinLine is one log line the search twin serves.
type twinLine struct {
	at      time.Time
	msg     string
	status  string
	host    string
	version string
}

// searchTwin serves the search operation over lines, honouring the filter's window and a
// `version:` clause, newest first, one page per call. forever makes every page claim a next one.
func searchTwin(t *testing.T, lines []twinLine, forever bool) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Filter struct{ Query, From, To string } `json:"filter"`
			Page   struct{ Limit int }              `json:"page"`
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("search body: %v", err)
		}
		from, _ := time.Parse(time.RFC3339Nano, req.Filter.From)
		to, _ := time.Parse(time.RFC3339Nano, req.Filter.To)
		var data []map[string]any
		for i := len(lines) - 1; i >= 0; i-- {
			l := lines[i]
			if l.at.Before(from) || !l.at.Before(to) {
				continue
			}
			if strings.Contains(req.Filter.Query, "version:") && !strings.Contains(req.Filter.Query, "version:"+l.version) {
				continue
			}
			data = append(data, map[string]any{"id": fmt.Sprint(i), "attributes": map[string]any{
				"timestamp": l.at.Format(time.RFC3339Nano), "message": l.msg, "status": l.status,
				"host": l.host, "service": "voice-agent", "tags": []string{"version:" + l.version},
				"attributes": map[string]any{},
			}})
		}
		out := map[string]any{"data": data, "meta": map[string]any{"status": "done"}}
		if forever {
			out["meta"] = map[string]any{"status": "done", "page": map[string]any{"after": "next"}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}
}

// A window with a template its baseline never had: the incident's lines.
func incidentLines() []twinLine {
	var lines []twinLine
	for i := range 20 { // baseline: two hours before, healthy
		lines = append(lines, twinLine{at: at.Add(-3*time.Hour + time.Duration(i)*time.Minute),
			msg: fmt.Sprintf("request served in %d ms", 10+i), status: "info", host: "h1", version: "v1"})
	}
	for i := range 6 { // window: the new failure on two hosts, from the new version
		host := []string{"h1", "h2"}[i%2]
		lines = append(lines, twinLine{at: at.Add(-50*time.Minute + time.Duration(i)*time.Minute),
			msg: fmt.Sprintf("payment provider refused order %d", 1000+i), status: "error", host: host, version: "v2"})
		lines = append(lines, twinLine{at: at.Add(-50*time.Minute + time.Duration(i)*time.Minute + time.Second),
			msg: fmt.Sprintf("request served in %d ms", 12+i), status: "info", host: host, version: "v2"})
	}
	return lines
}

func patternsTerm() *engine.Term {
	return engine.NewLogPatterns(logPointer(), engine.NewWindow(at.Add(-time.Hour), at),
		engine.NewWindow(at.Add(-3*time.Hour), at.Add(-2*time.Hour)))
}

// new_log_patterns mines the sample, marks what the baseline never had, and mints a handle per
// template; drill_down and exemplars answer those handles (contract §3.2, §6).
func TestNewLogPatternsThenDrillDownAndExemplars(t *testing.T) {
	t.Parallel()
	b, _ := liveBackend(t, searchTwin(t, incidentLines(), false))
	resp, err := b.Execute(context.Background(), request(patternsTerm()))
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetOutcome() != investigationv1.TermOutcome_DIGEST {
		t.Fatalf("outcome %v / %v", resp.GetOutcome(), resp.GetFailureReason())
	}
	log := resp.GetDigest().GetLog()
	if log.GetMinerVersion() == "" || len(log.GetPatterns()) != 2 {
		t.Fatalf("patterns %v", log.GetPatterns())
	}
	first := log.GetPatterns()[0]
	if !first.GetNewInWindow() || first.GetCount() != 6 || first.GetStatus() != "error" ||
		!strings.HasPrefix(first.GetTemplate(), "payment provider refused order") {
		t.Fatalf("the new template is not first: %v", first)
	}
	if strings.Contains(first.GetTemplate(), "1000") {
		t.Errorf("a template carries a raw value: %q", first.GetTemplate())
	}
	if log.GetCountsByStatus()["error"] != 6 {
		t.Errorf("counts %v", log.GetCountsByStatus())
	}
	if resp.GetDigest().GetCoverage().GetSampling() == "" {
		t.Errorf("coverage states no sampling")
	}
	handle := first.GetDrillDown().GetHandle()
	if handle.GetValue() == "" {
		t.Fatal("the new template has no handle")
	}

	drill, err := b.Execute(context.Background(), request(engine.DrillDown(handle)))
	if err != nil {
		t.Fatal(err)
	}
	rows := drill.GetDigest().GetLog().GetPatterns()
	if drill.GetOutcome() != investigationv1.TermOutcome_DIGEST || len(rows) != 2 ||
		rows[0].GetJoinKeys().GetPodOrHost() == rows[1].GetJoinKeys().GetPodOrHost() ||
		rows[0].GetCount()+rows[1].GetCount() != 6 || rows[0].GetDrillDown() != nil {
		t.Fatalf("drill-down by host: %v %v", drill.GetOutcome(), rows)
	}

	refused, err := b.Execute(context.Background(), request(engine.Exemplars(handle, 3)))
	if err != nil {
		t.Fatal(err)
	}
	if refused.GetOutcome() != investigationv1.TermOutcome_QUERY_FAILED {
		t.Fatalf("exemplars served without want_exemplars: %v", refused.GetOutcome())
	}
	req := request(engine.Exemplars(handle, 3))
	req.WantExemplars = true
	ex, err := b.Execute(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	got := ex.GetDigest().GetExemplars().GetExemplars()
	if ex.GetOutcome() != investigationv1.TermOutcome_DIGEST || len(got) == 0 || len(got) > 3 {
		t.Fatalf("exemplars %v %v", ex.GetOutcome(), got)
	}
	for _, e := range got {
		if e.GetJoinKeys().GetVersion() != "v2" || e.GetJoinKeys().GetPodOrHost() == "" {
			t.Errorf("exemplar join keys %v", e.GetJoinKeys())
		}
	}
}

// A version group's handle drills into that version's lines only.
func TestAVersionHandleDrillsIntoThatVersion(t *testing.T) {
	t.Parallel()
	lines := incidentLines()
	b, _ := liveBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "aggregate") {
			versionTwin(buckets(bucket("v2", 6)), buckets(bucket("v2", 12), bucket("v1", 20)))(w, r)
			return
		}
		searchTwin(t, lines, false)(w, r)
	})
	resp, err := b.Execute(context.Background(), request(engine.ErrorsByVersion(logPointer(),
		engine.NewWindow(at.Add(-time.Hour), at), "version")))
	if err != nil {
		t.Fatal(err)
	}
	v2 := resp.GetDigest().GetErrorsByVersion().GetVersions()[0]
	if v2.GetVersion() != "v2" {
		t.Fatalf("first group %v", v2)
	}
	drill, err := b.Execute(context.Background(), request(engine.DrillDown(v2.GetDrillDown().GetHandle())))
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, p := range drill.GetDigest().GetLog().GetPatterns() {
		total += p.GetCount()
		if p.GetJoinKeys().GetVersion() != "v2" {
			t.Errorf("a v1 line in the v2 drill-down: %v", p)
		}
	}
	if total != 12 {
		t.Errorf("%d lines behind v2, want 12", total)
	}
}

// The line cap stops the read and the answer is PARTIAL naming it, never a complete digest.
func TestTheLineCapMakesTheAnswerPartial(t *testing.T) {
	t.Parallel()
	var lines []twinLine
	for i := range 1000 {
		lines = append(lines, twinLine{at: at.Add(-30*time.Minute + time.Duration(i)*time.Second),
			msg: "tick", status: "info", host: "h1", version: "v1"})
	}
	search := searchTwin(t, lines, true)
	b, calls := liveBackend(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "aggregate") {
			_, _ = io.WriteString(w, `{"data":{"buckets":[{"by":{},"computes":{"c0":123456}}]},"meta":{"status":"done"}}`)
			return
		}
		search(w, r)
	})
	resp, err := b.Execute(context.Background(), request(patternsTerm()))
	if err != nil {
		t.Fatal(err)
	}
	cov := resp.GetDigest().GetCoverage()
	if resp.GetOutcome() != investigationv1.TermOutcome_PARTIAL || !strings.Contains(cov.GetTruncation(), "line_cap") {
		t.Fatalf("got %v %q", resp.GetOutcome(), cov.GetTruncation())
	}
	if !strings.Contains(cov.GetSampling(), "of 123456 lines in the window") {
		t.Errorf("the sample is not stated against the window's total: %q", cov.GetSampling())
	}
	// The window's pages, one baseline page, and the one count stating the sample's fraction.
	want := int64(ddbackend.LineCap/1000) + 2
	if *calls != want {
		t.Errorf("%d calls, want %d", *calls, want)
	}
}

// A handle this backend did not mint is refused, never executed.
func TestAComposedHandleIsRefused(t *testing.T) {
	t.Parallel()
	b, calls := liveBackend(t, func(http.ResponseWriter, *http.Request) {})
	resp, err := b.Execute(context.Background(), request(engine.DrillDown(&engine.Handle{Value: "service:x", Depth: 1})))
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetOutcome() != investigationv1.TermOutcome_QUERY_FAILED || *calls != 0 {
		t.Fatalf("got %v with %d calls", resp.GetOutcome(), *calls)
	}
}
