// SPDX-License-Identifier: Apache-2.0

package datadog_test

import (
	"os"
	"strings"
	"testing"

	ddfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/datadog"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// FR-008b for the APM operations: the three are declared only under apm_topology, the span aggregate is
// the one named-query POST it adds, and the page publishes each row verbatim.
func TestTheAPMOperationsAreDeclaredOnlyUnderAPMTopologyAndPublished(t *testing.T) {
	t.Parallel()
	ops := []feeder.ReadOperation{ddfeeder.OpServiceDependencies, ddfeeder.OpQueryMetrics, ddfeeder.OpAggregateSpans}
	for _, op := range ops {
		if _, err := ddfeeder.DefaultSurface.Issuable(op); err == nil {
			t.Errorf("%q is issuable with apm_topology off", op)
		}
	}
	on := ddfeeder.DefaultCapabilities()
	on[ddfeeder.CapAPMTopology] = true
	surface := ddfeeder.SurfaceFor(on)
	added := map[feeder.ReadOperation]bool{}
	for _, op := range surface.Operations() {
		if _, err := ddfeeder.DefaultSurface.Issuable(op); err != nil {
			added[op] = true
		}
	}
	if len(added) != len(ops) {
		t.Fatalf("apm_topology adds %v, want exactly %v", added, ops)
	}
	if n := surface.StateChanges(); n != 0 {
		t.Errorf("%d published operations change state with apm_topology on", n)
	}
	page, err := os.ReadFile("../../../docs/connectors/datadog.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range ops {
		spec, err := surface.Issuable(op)
		if err != nil {
			t.Fatal(err)
		}
		if row := "| " + spec.Area + " | `" + string(op) + "` | " + spec.Why + " |"; !strings.Contains(string(page), row) {
			t.Errorf("docs/connectors/datadog.md does not publish the row:\n%s", row)
		}
	}
	scopes := strings.Join(ddfeeder.AllowedScopes(on), ",")
	if !strings.Contains(scopes, "apm_read") || !strings.Contains(scopes, "apm_service_catalog_read") ||
		strings.Contains(strings.Join(ddfeeder.AllowedScopes(ddfeeder.DefaultCapabilities()), ","), "apm_") {
		t.Errorf("the APM scopes are %q with it on, and must be absent with it off", scopes)
	}
	if !strings.Contains(string(page), "not yet verified against a live organisation") {
		t.Error("the page does not say the capability is unverified against a live organisation")
	}
}

// With apm_topology on the checkpoint spelling states it, and only it, as on.
func TestTheCapabilitySpellingStatesAPMTopologyOn(t *testing.T) {
	t.Parallel()
	caps, err := ddfeeder.ParseCapabilities("logs,monitors,tags,apm_topology")
	if err != nil {
		t.Fatalf("apm_topology is built and must be enableable: %v", err)
	}
	if got := caps.String(); got != "apm_topology=on,changes=off,logs=on,monitors=on,tags=on" {
		t.Errorf("the checkpoint spelling is %q", got)
	}
}
