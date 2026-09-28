// SPDX-License-Identifier: Apache-2.0

package datadogx_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/datadogx"
	ddfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/datadog"
)

// The discovery reads count presence per candidate, grouped by status: two calls plus one per
// candidate, never a field's values (005 T064).
func TestTheMeasurerCountsPresencePerCandidate(t *testing.T) {
	t.Parallel()
	var queries []string
	c, calls := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Filter struct{ Query string } `json:"filter"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		queries = append(queries, req.Filter.Query)
		info, errs := 1000, 10 // every line…
		switch {
		case strings.Contains(req.Filter.Query, "@version:*"):
			info, errs = 3, 0 // …except the SDK's own @version, on a few start-up lines
		case strings.Contains(req.Filter.Query, " version:*"):
			info, errs = 1000, 10
		case strings.Contains(req.Filter.Query, ":*") && !strings.Contains(req.Filter.Query, "host:*"):
			info, errs = 0, 0
		}
		_, _ = fmt.Fprintf(w, `{"data":{"buckets":[{"by":{"status":"info"},"computes":{"c0":%d}},`+
			`{"by":{"status":"error"},"computes":{"c0":%d}}]},"meta":{"status":"done"}}`, info, errs)
	})
	m, err := datadogx.Measurer{Client: c}.Measure(context.Background(),
		ddfeeder.LogSource{Env: "production", Service: "checkout"}, "", clientNow.Add(-3600e9), clientNow)
	if err != nil {
		t.Fatal(err)
	}
	if m.Lines != 1010 || m.ErrorLines != 10 || m.HostLines != 1010 || len(m.Candidates) != 7 {
		t.Fatalf("measurement %+v", m)
	}
	if got := m.Candidates[0]; got.Label != "version (tag)" || got.Lines != 1010 {
		t.Errorf("version (tag): %+v", got)
	}
	if got := m.Candidates[2]; got.Label != "version (attribute)" || got.Lines != 3 {
		t.Errorf("version (attribute): %+v", got)
	}
	if *calls != 12 {
		t.Errorf("%d calls, want 2 + 7 candidates + 3 tag keys", *calls)
	}
	for _, q := range queries {
		if !strings.HasPrefix(q, "service:checkout env:production") {
			t.Errorf("a discovery query not pinned to the source: %q", q)
		}
	}
}

// When no line carries the source's environment, one more count says whether the service logs with no
// `env` at all: a configuration to fix, not a silent service (005, the first live run). A source that
// has lines, or names no environment, never pays for it.
func TestAnEmptyEnvironmentIsProbedForLogsWithoutOne(t *testing.T) {
	t.Parallel()
	var queries []string
	attribute := false
	c, calls := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Filter struct{ Query string } `json:"filter"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		queries = append(queries, req.Filter.Query)
		n := 0
		if strings.Contains(req.Filter.Query, "-env:*") {
			n = 3000
		}
		if strings.Contains(req.Filter.Query, "@env:") && attribute {
			n = 2000
		}
		_, _ = fmt.Fprintf(w, `{"data":{"buckets":[{"by":{"status":"info"},"computes":{"c0":%d}}]},"meta":{"status":"done"}}`, n)
	})
	m, err := datadogx.Measurer{Client: c}.Measure(context.Background(),
		ddfeeder.LogSource{Env: "production", Service: "billing"}, "", clientNow.Add(-3600e9), clientNow)
	if err != nil {
		t.Fatal(err)
	}
	if m.Lines != 0 || m.LinesWithoutEnv != 3000 {
		t.Errorf("measurement %+v, want 0 lines in production and 3000 without an env", m)
	}
	if *calls != 14 || queries[1] != "service:billing @env:production" || queries[2] != "service:billing -env:*" {
		t.Errorf("%d calls, queries %q; want the 12 aggregates plus the attribute probe and the env-less probe", *calls, queries[:3])
	}

	// JSON logs that carry the environment as the `@env` attribute: that probe finds them, and the second
	// is not paid for.
	attribute = true
	queries = nil
	*calls = 0
	m, err = datadogx.Measurer{Client: c}.Measure(context.Background(),
		ddfeeder.LogSource{Env: "production", Service: "billing"}, "", clientNow.Add(-3600e9), clientNow)
	if err != nil {
		t.Fatal(err)
	}
	if m.LinesWithEnvAttribute != 2000 || m.LinesWithoutEnv != 0 || *calls != 13 {
		t.Errorf("measurement %+v after %d calls; want 2000 lines with @env and one probe", m, *calls)
	}

	// Watched with --env-field @env, the source is measured on the attribute and never probed.
	attribute = false
	queries = nil
	*calls = 0
	if _, err = (datadogx.Measurer{Client: c}).Measure(context.Background(),
		ddfeeder.LogSource{Env: "production", Service: "billing", EnvField: "@env"}, "", clientNow.Add(-3600e9), clientNow); err != nil {
		t.Fatal(err)
	}
	if *calls != 12 || queries[0] != "service:billing @env:production" {
		t.Errorf("%d calls, first query %q; want the source measured on its @env attribute", *calls, queries[0])
	}

	// An environment-less source is measured on its service alone, and is never probed.
	queries = nil
	*calls = 0
	m, err = datadogx.Measurer{Client: c}.Measure(context.Background(),
		ddfeeder.LogSource{Service: "billing"}, "", clientNow.Add(-3600e9), clientNow)
	if err != nil {
		t.Fatal(err)
	}
	if *calls != 12 || queries[0] != "service:billing" || m.LinesWithoutEnv != 0 {
		t.Errorf("%d calls, first query %q: an environment-less source is measured on its service alone", *calls, queries[0])
	}
}
