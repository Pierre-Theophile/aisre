// SPDX-License-Identifier: Apache-2.0

package datadog

import (
	"fmt"
	"strings"
	"time"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// Kind is the connector family, as fixtures and manifests group it.
const Kind = "datadog"

// ReorderingWindow is how far out of order this feeder may deliver its own events. A discovery cycle
// and a monitor poll can land in either order inside one poll interval, and the shuffle step permutes
// within this window.
const ReorderingWindow = 10 * time.Minute

// Describe returns the feeder's description for one Datadog organisation. Each organisation is a
// distinct source (FR-006): `datadog:<org>`.
func Describe(org string) (feeder.Description, error) {
	if strings.TrimSpace(org) == "" || strings.ContainsAny(org, ": /") {
		return feeder.Description{}, fmt.Errorf("datadog: organisation slug %q must be non-empty and "+
			"contain no `:`, `/` or space; it becomes the source id", org)
	}
	return feeder.Description{
		SourceID:         Kind + ":" + org,
		Kind:             Kind,
		Ordering:         feeder.OrderingNone,
		ReorderingWindow: ReorderingWindow,
	}, nil
}
