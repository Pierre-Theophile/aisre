// SPDX-License-Identifier: Apache-2.0

package mcpserver

import (
	"fmt"
	"strings"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
)

// The CLI's defaults for the same queries (internal/query). They are repeated rather than
// imported because internal/query is the Postgres adapter, which this package must not link; a
// test pins each one to its source so they cannot drift.
const (
	defaultHops        = 2
	defaultPerHopCap   = 50
	defaultTotalCap    = 500
	defaultImpactHops  = 3
	maxHops            = 8
	defaultAlertWindow = 2 * time.Hour
	idNamespace        = "id"
	defaultListLimit   = 50
)

// now is replaceable in tests.
var now = func() time.Time { return time.Now().UTC() }

// parseInstant accepts what the CLI accepts: RFC 3339, a duration relative to now (`-30m`) or
// `now`. Empty is the zero time, read as unset.
func parseInstant(name, raw string) (time.Time, error) {
	value := strings.TrimSpace(raw)
	switch {
	case value == "":
		return time.Time{}, nil
	case strings.EqualFold(value, "now"):
		return now(), nil
	case value[0] == '-' || value[0] == '+':
		offset, err := time.ParseDuration(value)
		if err != nil {
			return time.Time{}, fmt.Errorf("%s %q: a relative instant is a duration such as -30m or +2h", name, raw)
		}
		return now().Add(offset), nil
	}
	t, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s %q: want an RFC 3339 instant (2026-09-01T14:32:00Z), "+
			"a relative duration (-30m) or `now`", name, raw)
	}
	return t.UTC(), nil
}

// parseRef accepts `<namespace>=<value>` and `id:<entity_id>`, as the CLI does.
func parseRef(name, raw string) (*graphv1.Ref, error) {
	trimmed := strings.TrimSpace(raw)
	if rest, ok := strings.CutPrefix(trimmed, idNamespace+":"); ok && rest != "" {
		return graph.Ref{Namespace: idNamespace, Value: rest}.Proto(), nil
	}
	ref, err := graph.ParseRef(trimmed)
	if err != nil {
		return nil, fmt.Errorf("%s %q: write it as <namespace>=<value> (e.g. otel.service.name=checkout) "+
			"or id:<entity_id>", name, raw)
	}
	return ref.Proto(), nil
}

func parseDirection(s string) (graphv1.Direction, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "both":
		return graphv1.Direction_BOTH, nil
	case "up", "upstream":
		return graphv1.Direction_UPSTREAM, nil
	case "down", "downstream":
		return graphv1.Direction_DOWNSTREAM, nil
	}
	return graphv1.Direction_DIRECTION_UNSPECIFIED, fmt.Errorf("direction %q: want up, down or both", s)
}

func parseEdgeTypes(names []string) ([]graphv1.EdgeType, error) {
	var out []graphv1.EdgeType
	for _, name := range names {
		normalized := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(name)), "-", "_")
		if normalized == "" {
			continue
		}
		t, err := graph.ParseEdgeType(normalized)
		if err != nil {
			return nil, fmt.Errorf("edge type %q: want one of calls, depends_on, runs_on, deployed_by, "+
				"owned_by, exposed_via, changed_by", name)
		}
		out = append(out, t.Proto())
	}
	return out, nil
}

// asOf builds the AsOf every graph read takes. valid_at is required: the graph answers as of an
// instant (constitution II).
func asOf(validRaw, observedRaw string) (*graphv1.AsOf, time.Time, time.Time, error) {
	if strings.TrimSpace(validRaw) == "" {
		return nil, time.Time{}, time.Time{}, fmt.Errorf("valid_at is required: the graph answers as of an instant")
	}
	validAt, err := parseInstant("valid_at", validRaw)
	if err != nil {
		return nil, time.Time{}, time.Time{}, err
	}
	observedAt, err := parseInstant("observed_at", observedRaw)
	if err != nil {
		return nil, time.Time{}, time.Time{}, err
	}
	at := &graphv1.AsOf{ValidAt: tsOf(validAt)}
	if !observedAt.IsZero() {
		at.ObservedAt = tsOf(observedAt)
	}
	return at, validAt, observedAt, nil
}

func u32(v int) *uint32 {
	out := uint32(v)
	return &out
}
