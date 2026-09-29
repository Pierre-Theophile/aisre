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
		ddfeeder.LogSource{Env: "production", Service: "checkout", EnvField: "env"}, "", clientNow.Add(-3600e9), clientNow)
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

// envTwin answers counts the way a Datadog organisation whose JSON logs carry `{"env": "production"}`
// would: the environment is the `@env` attribute on 998 of the service's 1000 lines, never the tag.
func envTwin(t *testing.T, attribute bool) (*datadogx.Client, *int64, *[]string) {
	t.Helper()
	var queries []string
	c, calls := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Filter  struct{ Query string }   `json:"filter"`
			GroupBy []struct{ Facet string } `json:"group_by"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		q := req.Filter.Query
		queries = append(queries, q)
		if len(req.GroupBy) > 0 && req.GroupBy[0].Facet == "@env" {
			_, _ = fmt.Fprint(w, `{"data":{"buckets":[{"by":{"@env":"production"},"computes":{"c0":998}},`+
				`{"by":{"@env":"staging"},"computes":{"c0":2}}]},"meta":{"status":"done"}}`)
			return
		}
		n := 0
		switch {
		case q == "service:billing":
			n = 1000
		case strings.HasSuffix(q, " @env:*"):
			if attribute {
				n = 998
			}
		case strings.HasSuffix(q, ":*") && strings.Count(q, " ") == 1: // another environment field
			n = 0
		case strings.Contains(q, "@env:production") && attribute:
			n = 998
		}
		_, _ = fmt.Fprintf(w, `{"data":{"buckets":[{"by":{"status":"info"},"computes":{"c0":%d}}]},"meta":{"status":"done"}}`, n)
	})
	return c, calls, &queries
}

// The environment field is discovered, not required (005): the tag first, then `@env`, each accepted
// only on 95 % of the service's lines, and the source is measured on the field found.
func TestTheEnvironmentFieldIsDiscovered(t *testing.T) {
	t.Parallel()
	from := clientNow.Add(-3600e9)

	c, calls, queries := envTwin(t, true)
	m, err := datadogx.Measurer{Client: c}.Measure(context.Background(),
		ddfeeder.LogSource{Env: "production", Service: "billing"}, "", from, clientNow)
	if err != nil {
		t.Fatal(err)
	}
	if m.EnvField != "@env" || m.Lines != 998 || m.EnvDiscovery == nil || m.EnvDiscovery.ServiceLines != 1000 ||
		len(m.EnvDiscovery.Fields) != 2 || len(m.EnvDiscovery.Values) != 0 {
		t.Errorf("measurement %+v, discovery %+v; want @env found second and the source measured on it", m, m.EnvDiscovery)
	}
	if want := []string{"service:billing", "service:billing env:*", "service:billing @env:*", "service:billing @env:production"}; strings.Join((*queries)[:4], "|") != strings.Join(want, "|") {
		t.Errorf("queries %q, want %q", (*queries)[:4], want)
	}
	if *calls != 15 {
		t.Errorf("%d calls, want the 12 aggregates plus a total and two fields", *calls)
	}

	// The name the operator gave is not the one the logs carry: the values say which is.
	c, _, _ = envTwin(t, true)
	m, err = datadogx.Measurer{Client: c}.Measure(context.Background(),
		ddfeeder.LogSource{Env: "prod", Service: "billing"}, "", from, clientNow)
	if err != nil {
		t.Fatal(err)
	}
	if m.Lines != 0 || len(m.EnvDiscovery.Values) != 2 || m.EnvDiscovery.Values[0] != (ddfeeder.EnvValue{Value: "production", Lines: 998}) {
		t.Errorf("measurement %+v, values %+v; want the environments @env carries, most lines first", m, m.EnvDiscovery.Values)
	}

	// Configured with --env-field (or remembered by the poller), no discovery is paid for.
	c, calls, queries = envTwin(t, true)
	if m, err = (datadogx.Measurer{Client: c}).Measure(context.Background(),
		ddfeeder.LogSource{Env: "production", Service: "billing", EnvField: "@env"}, "", from, clientNow); err != nil {
		t.Fatal(err)
	}
	if *calls != 12 || (*queries)[0] != "service:billing @env:production" || m.EnvDiscovery != nil {
		t.Errorf("%d calls, first query %q: a known field is not discovered again", *calls, (*queries)[0])
	}

	// No field carries it: every candidate is tried, stated, and the source is measured on the tag.
	c, calls, _ = envTwin(t, false)
	if m, err = (datadogx.Measurer{Client: c}).Measure(context.Background(),
		ddfeeder.LogSource{Env: "production", Service: "billing"}, "", from, clientNow); err != nil {
		t.Fatal(err)
	}
	if m.EnvField != "" || len(m.EnvDiscovery.Fields) != len(ddfeeder.EnvFields) || *calls != int64(12+1+len(ddfeeder.EnvFields)) {
		t.Errorf("measurement %+v after %d calls; want every field tried and none chosen", m, *calls)
	}

	// Watched without an environment: the field is discovered and its values listed, and the source is
	// still measured on its service alone.
	c, _, queries = envTwin(t, true)
	if m, err = (datadogx.Measurer{Client: c}).Measure(context.Background(),
		ddfeeder.LogSource{Service: "billing"}, "", from, clientNow); err != nil {
		t.Fatal(err)
	}
	if m.EnvField != "@env" || len(m.EnvDiscovery.Values) != 2 || (*queries)[3] != "service:billing" {
		t.Errorf("measurement %+v, queries %q", m, (*queries)[:4])
	}
}
