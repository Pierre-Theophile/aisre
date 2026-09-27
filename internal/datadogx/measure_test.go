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
	if *calls != 9 {
		t.Errorf("%d calls, want 2 + 7 candidates", *calls)
	}
	for _, q := range queries {
		if !strings.HasPrefix(q, "service:checkout env:production") {
			t.Errorf("a discovery query not pinned to the source: %q", q)
		}
	}
}
