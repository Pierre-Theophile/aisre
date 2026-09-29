// SPDX-License-Identifier: Apache-2.0

package datadog_test

import (
	"os"
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
// `changes` is built (T092) and off by default; `apm_topology` is not.
func TestAnUnbuiltCapabilityCannotBeEnabled(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"apm_topology", "incidents"} {
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
	built, err := ddfeeder.ParseCapabilities("logs,monitors,tags,changes")
	if err != nil {
		t.Fatalf("the changes capability is built and must be enableable: %v", err)
	}
	if got := built.String(); got != "apm_topology=off,changes=on,logs=on,monitors=on,tags=on" {
		t.Errorf("the checkpoint spelling is %q", got)
	}
}

// FR-008b for the events read: `GET /api/v2/events` and its scope `events_read` are declared only under
// the changes capability, the operation is a plain GET, and posting an event stays refused whatever is on.
func TestTheEventsReadIsDeclaredOnlyUnderTheChangesCapability(t *testing.T) {
	t.Parallel()
	def := ddfeeder.DefaultCapabilities()
	if _, err := ddfeeder.SurfaceFor(def).Issuable(ddfeeder.OpListEvents); err == nil {
		t.Error("the events read is issuable with the changes capability off")
	}
	on := ddfeeder.Capabilities{ddfeeder.CapLogs: true, ddfeeder.CapMonitors: true, ddfeeder.CapTags: true, ddfeeder.CapChanges: true}
	surface := ddfeeder.SurfaceFor(on)
	spec, err := surface.Issuable(ddfeeder.OpListEvents)
	if err != nil || spec.Area != "changes" {
		t.Fatalf("the events read is not declared under changes: %v %+v", err, spec)
	}
	if n := surface.StateChanges(); n != 0 {
		t.Errorf("%d published operations change state with changes on", n)
	}
	if !strings.HasPrefix(string(ddfeeder.OpListEvents), "GET ") {
		t.Errorf("%q is not a GET", ddfeeder.OpListEvents)
	}
	if _, err := surface.Issuable("POST /api/v1/events"); err == nil {
		t.Error("posting an event is issuable with changes on")
	}
	scopes := strings.Join(ddfeeder.AllowedScopes(on), ",")
	if !strings.Contains(scopes, "events_read") || strings.Contains(strings.Join(ddfeeder.AllowedScopes(def), ","), "events_read") {
		t.Errorf("events_read is allowed with changes on: %q; and must not be with it off", scopes)
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

// The page an operator approves also publishes what the changes capability adds, verbatim: the operation
// and the reason the code states, and no more than that one operation.
func TestThePageAlsoPublishesWhatTheChangesCapabilityAdds(t *testing.T) {
	t.Parallel()
	on := ddfeeder.DefaultCapabilities()
	on[ddfeeder.CapChanges] = true
	added := map[feeder.ReadOperation]bool{}
	for _, op := range ddfeeder.SurfaceFor(on).Operations() {
		if _, err := ddfeeder.DefaultSurface.Issuable(op); err != nil {
			added[op] = true
		}
	}
	if len(added) != 1 || !added[ddfeeder.OpListEvents] {
		t.Fatalf("the changes capability adds %v, want exactly %s", added, ddfeeder.OpListEvents)
	}
	page, err := os.ReadFile("../../../docs/connectors/datadog.md")
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := ddfeeder.SurfaceFor(on).SpecOf(ddfeeder.OpListEvents)
	row := "| " + spec.Area + " | `" + string(ddfeeder.OpListEvents) + "` | " + spec.Why + " |"
	if !strings.Contains(string(page), row) {
		t.Errorf("docs/connectors/datadog.md does not publish the row:\n%s", row)
	}
	if !strings.Contains(string(page), "not yet verified against a live organisation") {
		t.Error("the page does not say the changes capability is unverified against a live organisation")
	}
}
