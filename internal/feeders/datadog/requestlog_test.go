// SPDX-License-Identifier: Apache-2.0

package datadog_test

import (
	"strings"
	"testing"

	ddfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/datadog"
	"github.com/Pierre-Theophile/aisre/internal/resolution"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/testkit"
)

// The published read-only operation surface (005 T008; FR-004, SC-015).

// The page an operator approves and the surface the process enforces are the same list, in both
// directions, area and reason included.
func TestThePublishedPageIsTheEnforcedSurface(t *testing.T) {
	t.Parallel()
	testkit.AssertSurfaceMatchesPage(t, ddfeeder.DefaultSurface, "../../../docs/connectors/datadog.md")
}

// Nothing on the surface changes state, and the only POSTs are the two named queries.
func TestNothingOnTheSurfaceChangesState(t *testing.T) {
	t.Parallel()
	s := ddfeeder.DefaultSurface
	if n := s.StateChanges(); n != 0 {
		t.Fatalf("%d published operations change state", n)
	}
	var posts []string
	for _, op := range s.Operations() {
		if strings.HasPrefix(string(op), "POST ") {
			posts = append(posts, string(op))
		}
	}
	want := []string{string(ddfeeder.OpAggregateLogs), string(ddfeeder.OpSearchLogs)}
	if strings.Join(posts, "|") != strings.Join(want, "|") {
		t.Errorf("the POSTs on the surface are %v, want exactly the named queries %v", posts, want)
	}
}

// The operations the contract names as never declared are refused, whatever capabilities are on.
func TestTheNeverDeclaredOperationsAreRefused(t *testing.T) {
	t.Parallel()
	all := ddfeeder.SurfaceFor(ddfeeder.Capabilities{
		ddfeeder.CapLogs: true, ddfeeder.CapMonitors: true, ddfeeder.CapTags: true,
	})
	for _, op := range []feeder.ReadOperation{
		"POST /api/v1/monitor/{monitor_id}/mute",
		"POST /api/v1/monitor/{monitor_id}/unmute",
		"POST /api/v1/monitor",
		"PUT /api/v1/monitor/{monitor_id}",
		"DELETE /api/v1/monitor/{monitor_id}",
		"POST /api/v2/downtime",
		"POST /api/v1/events",
		"POST /api/v2/incidents",
		"POST /api/v1/dashboard",
		"POST /api/v1/logs/config/indexes",
		"POST /api/v2/logs",
	} {
		if _, err := all.Issuable(op); err == nil {
			t.Errorf("%q is issuable; the contract names it as never declared", op)
		}
	}
}

// A disabled capability declares nothing, so its operations are refused (FR-008b).
func TestADisabledCapabilityDeclaresNothing(t *testing.T) {
	t.Parallel()
	monitorsOnly := ddfeeder.SurfaceFor(ddfeeder.Capabilities{ddfeeder.CapMonitors: true})
	for _, op := range []feeder.ReadOperation{ddfeeder.OpSearchLogs, ddfeeder.OpAggregateLogs} {
		if _, err := monitorsOnly.Issuable(op); err == nil {
			t.Errorf("%q is issuable with the logs capability off", op)
		}
	}
	if _, err := monitorsOnly.Issuable(ddfeeder.OpValidateKeys); err != nil {
		t.Errorf("the startup gate's operations must be declared in every configuration: %v", err)
	}
}

// A capability that is specified but not built cannot be enabled, so it can never be half-enabled.
func TestAnUnbuiltCapabilityCannotBeEnabled(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"apm_topology", "changes", "incidents"} {
		if _, err := ddfeeder.ParseCapabilities("logs," + name); err == nil {
			t.Errorf("capability %q was accepted", name)
		}
	}
	caps, err := ddfeeder.ParseCapabilities("logs, monitors")
	if err != nil {
		t.Fatal(err)
	}
	if got := caps.String(); got != "apm_topology=off,changes=off,logs=on,monitors=on,tags=off" {
		t.Errorf("the checkpoint spelling is %q; every capability must be stated, off ones included", got)
	}
}

// The namespace C9 reads is spelled the same by the feeder that mints it and the rule that reads it;
// a difference would be a certain rule that silently never fires.
func TestTheLogServiceNamespaceIsTheOneC9Reads(t *testing.T) {
	t.Parallel()
	if ddfeeder.NSLogService != resolution.NamespaceDatadogLogService {
		t.Fatalf("the feeder mints %q and C9 reads %q", ddfeeder.NSLogService, resolution.NamespaceDatadogLogService)
	}
}
