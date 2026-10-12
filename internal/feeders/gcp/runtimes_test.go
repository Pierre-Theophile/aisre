// SPDX-License-Identifier: Apache-2.0

package gcp_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/protobuf/encoding/protojson"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	gcpfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/gcp"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/emit"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/source"
)

// A runtime Google is retiring becomes a future-dated change on every service that runs it, and on no
// other (T192). The catalogue's own stage decides whether a milestone is announced or confirmed; the
// date passing does not (FR-066).
func TestARetiringRuntimeIsAnAnnouncedChangeOnTheServicesThatRunIt(t *testing.T) {
	t.Parallel()
	onRuntime := func(name, runtime string) *runpb.Service {
		svc := service(name)
		svc.Labels = map[string]string{gcpfeeder.FunctionRuntimeLabel: runtime}
		return svc
	}
	services, _ := json.Marshal(map[string]any{"services": []json.RawMessage{
		mustProtoJSON(t, onRuntime("mailer", "nodejs20")),
		mustProtoJSON(t, onRuntime("pdf-maker", "nodejs20")),
		mustProtoJSON(t, onRuntime("scorer", "nodejs22")),
		mustProtoJSON(t, service("storefront")),
	}})
	runtimes := []byte(`{"runtimes":[
		{"name":"nodejs20","stage":"DEPRECATED","environment":"GEN_2","deprecationDate":{"year":2026,"month":4,"day":30},"decommissionDate":{"year":2026,"month":10,"day":30}},
		{"name":"nodejs20","stage":"DEPRECATED","environment":"GEN_1","deprecationDate":{"year":2026,"month":4,"day":30},"decommissionDate":{"year":2026,"month":10,"day":30}},
		{"name":"nodejs22","stage":"GA","deprecationDate":{"year":2027,"month":4,"day":30},"decommissionDate":{"year":2027,"month":10,"day":30}},
		{"name":"go124","stage":"DEPRECATED","deprecationDate":{"year":2026,"month":9,"day":2},"decommissionDate":{"year":2027,"month":3,"day":2}}]}`)

	f, err := gcpfeeder.New(gcpfeeder.Options{
		OrgSlug: "acme",
		Scope:   gcpfeeder.Scope{Projects: []string{pollProject}, Regions: []string{pollRegion}},
		Horizon: gcpfeeder.Horizon{Earliest: pollStart.Add(-30 * 24 * time.Hour), Reason: gcpfeeder.HorizonConfigured},
	})
	if err != nil {
		t.Fatal(err)
	}
	memory := emit.NewMemoryEmitter(f.Describe())
	payloads := []feeder.Payload{
		{Kind: gcpfeeder.PayloadServices, At: pollStart, Bytes: services},
		{Kind: gcpfeeder.PayloadFunctionRuntimes, At: pollStart, Bytes: runtimes},
		{Kind: gcpfeeder.PayloadPollMarker, At: pollStart, Bytes: []byte(`{"outcome":"complete","projects":["` + pollProject + `"],"regions":["` + pollRegion + `"]}`)},
	}
	if err := f.Run(t.Context(), source.NewSliceSource(payloads), memory); err != nil {
		t.Fatal(err)
	}

	got := map[string]*graphv1.ObserveChange{}
	for _, ev := range memory.Events() {
		if c := ev.GetObserveChange(); c != nil && strings.HasPrefix(c.GetRef().GetValue(), "function-runtime/") {
			got[c.GetRef().GetValue()] = c
		}
	}
	want := map[string]struct {
		state   graphv1.AnnouncementState
		at      string
		targets int
	}{
		"function-runtime/nodejs20/deprecation":  {graphv1.AnnouncementState_CONFIRMED, "2026-04-30", 2},
		"function-runtime/nodejs20/decommission": {graphv1.AnnouncementState_ANNOUNCED, "2026-10-30", 2},
		"function-runtime/nodejs22/deprecation":  {graphv1.AnnouncementState_ANNOUNCED, "2027-04-30", 1},
		"function-runtime/nodejs22/decommission": {graphv1.AnnouncementState_ANNOUNCED, "2027-10-30", 1},
	}
	if len(got) != len(want) {
		names := make([]string, 0, len(got))
		for name := range got {
			names = append(names, name)
		}
		t.Fatalf("changes = %v; want exactly the four milestones of the two runtimes in use, and nothing for go124", names)
	}
	for ref, w := range want {
		c := got[ref]
		if c == nil {
			t.Errorf("no change %s", ref)
			continue
		}
		if c.GetChange().GetKind() != graphv1.ChangeKind_DEPRECATION || c.GetChange().GetAnnouncementState() != w.state {
			t.Errorf("%s: kind %s state %s, want DEPRECATION %s", ref, c.GetChange().GetKind(), c.GetChange().GetAnnouncementState(), w.state)
		}
		if at := c.GetValidAt().AsTime().Format("2006-01-02"); at != w.at {
			t.Errorf("%s: valid at %s, want the announced %s", ref, at, w.at)
		}
		if len(c.GetTargets()) != w.targets {
			t.Errorf("%s: %d targets, want the %d services that run it", ref, len(c.GetTargets()), w.targets)
		}
	}
}

func mustProtoJSON(t *testing.T, svc *runpb.Service) json.RawMessage {
	t.Helper()
	raw, err := protojson.Marshal(svc)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
