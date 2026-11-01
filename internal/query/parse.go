// SPDX-License-Identifier: Apache-2.0

package query

import (
	"fmt"
	"strings"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
)

// ParseDirection accepts the spellings a manifest and the CLI use. An empty string means the
// default, BOTH.
func ParseDirection(s string) (graphv1.Direction, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "both":
		return graphv1.Direction_BOTH, nil
	case "up", "upstream":
		return graphv1.Direction_UPSTREAM, nil
	case "down", "downstream":
		return graphv1.Direction_DOWNSTREAM, nil
	default:
		return graphv1.Direction_DIRECTION_UNSPECIFIED,
			fmt.Errorf("query: direction %q: want up, down or both", s)
	}
}

// ParseEdgeTypes converts edge type names to the published enum. Both the canonical spelling
// (`depends_on`) and the hyphenated one used in prose and on the command line (`depends-on`)
// are accepted.
func ParseEdgeTypes(names []string) ([]graphv1.EdgeType, error) {
	if len(names) == 0 {
		return nil, nil
	}
	out := make([]graphv1.EdgeType, 0, len(names))
	for _, name := range names {
		normalized := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(name)), "-", "_")
		if normalized == "" {
			continue
		}
		edgeType, err := graph.ParseEdgeType(normalized)
		if err != nil {
			return nil, fmt.Errorf("query: edge type %q: want one of calls, depends_on, runs_on, deployed_by, owned_by, exposed_via, changed_by", name)
		}
		out = append(out, edgeType.Proto())
	}
	return out, nil
}
