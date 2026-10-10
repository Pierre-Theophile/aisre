// SPDX-License-Identifier: Apache-2.0

package gcp_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	gcpfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/gcp"
	"github.com/Pierre-Theophile/aisre/internal/gcpx"
	"github.com/Pierre-Theophile/aisre/internal/sanitise"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/emit"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/source"
)

// Live graph parity against a real project (003 T189, FR-143's graph half, SC-016).
//
// One live cycle is read once and fed to the feeder twice: as GCP returned it, and as a recording
// keeps it — through the pre-pass and the sanitisation table. The two graphs cannot be equal byte for
// byte, because every identifier in the second is a pseudonym. What must be equal is their SHAPE:
// the same events, of the same kinds, about the same node, edge and change types, at the same
// instants. A difference is something sanitising lost — a name a parse no longer reads, a field a
// rule dropped that the feeder needed — and the report says which kind of event moved.
//
// It runs only when asked, with the credential in the environment and the scope given as variables,
// so nothing of the organisation is written into this file:
//
//	GCP_LIVE_PARITY=1 GCP_PARITY_PROJECT=<project> GCP_PARITY_REGION=<region> \
//	GCP_ASSERT_READ_ONLY_BY='<name>' GOOGLE_APPLICATION_CREDENTIALS=<key file> \
//	SRE_AGENT_CORPUS_KEY=<hex> go test ./internal/feeders/gcp/ -run LiveGraphParity -v
//
// The report names event shapes and counts, never an identifier.
func TestLiveGraphParity(t *testing.T) {
	if os.Getenv("GCP_LIVE_PARITY") != "1" {
		t.Skip("live only: set GCP_LIVE_PARITY=1 and the variables in the comment above")
	}
	project, region := os.Getenv("GCP_PARITY_PROJECT"), os.Getenv("GCP_PARITY_REGION")
	assertBy := os.Getenv("GCP_ASSERT_READ_ONLY_BY")
	if project == "" || region == "" || assertBy == "" {
		t.Fatal("GCP_PARITY_PROJECT, GCP_PARITY_REGION and GCP_ASSERT_READ_ONLY_BY are required")
	}
	ctx := t.Context()

	creds, err := gcpx.Gate(ctx, gcpx.GateOptions{
		Projects:  []string{project},
		Assertion: &gcpx.Assertion{By: assertBy, At: time.Now().UTC().Format(time.RFC3339), Acknowledged: gcpx.UnverifiableAreas()},
	})
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	usage := gcpx.NewUsage(gcpx.ConsumerFeeder)
	budget := gcpx.NewBudget(gcpx.PublishedLimits(), usage)
	poller, err := gcpfeeder.NewPoller(gcpfeeder.PollerOptions{
		Connect: func(ctx context.Context) (*gcpfeeder.Transport, error) {
			return gcpfeeder.NewLiveTransport(ctx, creds, budget)
		},
		Scope:  gcpfeeder.Scope{Projects: []string{project}, Regions: []string{region}},
		Cycles: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	var raw []feeder.Payload
	for {
		p, err := poller.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("live read: %v", err)
		}
		raw = append(raw, p)
	}
	if !usage.ReadOnlyHonoured() || len(usage.StateChanges()) != 0 {
		t.Fatalf("the live cycle changed state: %v", usage.StateChanges())
	}

	key, err := sanitise.KeyFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	san, err := sanitise.New(sanitise.ContractPolicy(), key)
	if err != nil {
		t.Fatal(err)
	}
	var clean []feeder.Payload
	for _, p := range raw {
		prepared, err := gcpfeeder.PreparePayload(p.Kind, p.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		out, err := san.JSON("gcp."+p.Kind, prepared)
		if err != nil {
			t.Fatalf("a %s payload could not be recorded: %v", p.Kind, err)
		}
		clean = append(clean, feeder.Payload{Kind: p.Kind, At: p.At, Seq: p.Seq, Bytes: out})
	}
	pseudoProject, err := san.LabelIdentifier(sanitise.KindProject, project)
	if err != nil {
		t.Fatal(err)
	}

	earliest := time.Now().UTC().Add(-30 * 24 * time.Hour)
	live := graphShape(t, raw, gcpfeeder.Scope{Projects: []string{project}, Regions: []string{region}}, earliest)
	recorded := graphShape(t, clean, gcpfeeder.Scope{Projects: []string{pseudoProject}, Regions: []string{region}}, earliest)

	t.Logf("live cycle: %d payload(s), %d event(s); recorded: %d event(s)", len(raw), total(live), total(recorded))
	var diffs []string
	for _, shape := range union(live, recorded) {
		if live[shape] == recorded[shape] {
			continue
		}
		line := fmt.Sprintf("%-70s live %6d  recorded %6d", shape, live[shape], recorded[shape])
		if reason, ok := expectedLoss(shape); ok && recorded[shape] < live[shape] {
			t.Logf("expected loss: %s\n    because %s", line, reason)
			continue
		}
		diffs = append(diffs, line)
	}
	if len(diffs) > 0 {
		t.Errorf("sanitising changed the graph's shape in %d place(s):\n%s", len(diffs), strings.Join(diffs, "\n"))
	}
}

// expectedLoss names what sanitising is DESIGNED to cost the recorded graph, and why. Anything else
// that differs is a defect: a rule dropping a field the feeder needs, or a pseudonym a parse no longer
// reads. A loss is only ever accepted in one direction — the recording having less.
func expectedLoss(shape string) (string, bool) {
	switch {
	case strings.HasPrefix(shape, "claim "+gcpfeeder.NSAlertPolicy+" on "+gcpfeeder.NSAlertPolicy):
		return "a policy's display name is one of its claims, and the contract drops every display " +
			"name as possibly naming a person (FR-135)", true
	}
	return "", false
}

// graphShape runs payloads through a feeder and counts its events by shape: what kind of event, about
// what type, and when — everything an event says except which thing it is about.
func graphShape(t *testing.T, payloads []feeder.Payload, scope gcpfeeder.Scope, earliest time.Time) map[string]int {
	t.Helper()
	f, err := gcpfeeder.New(gcpfeeder.Options{
		OrgSlug: "parity",
		Scope:   scope,
		Labels:  gcpfeeder.DefaultLabelPolicy(),
		Horizon: gcpfeeder.Horizon{Earliest: earliest, Reason: gcpfeeder.HorizonConfigured},
	})
	if err != nil {
		t.Fatal(err)
	}
	memory := emit.NewMemoryEmitter(f.Describe())
	if err := f.Run(t.Context(), source.NewSliceSource(payloads), memory); err != nil {
		t.Fatalf("feeding: %v", err)
	}
	shapes := map[string]int{}
	for _, ev := range memory.Events() {
		shapes[eventShape(ev)]++
	}
	return shapes
}

func eventShape(ev *graphv1.EventEnvelope) string {
	at := func(ts interface{ AsTime() time.Time }, unknown bool) string {
		if unknown {
			return "unknown"
		}
		return ts.AsTime().UTC().Format(time.RFC3339)
	}
	switch body := ev.GetBody().(type) {
	case *graphv1.EventEnvelope_UpsertNode:
		n := body.UpsertNode
		return fmt.Sprintf("node %s %s", n.GetType(), at(n.GetValidAt(), n.GetValidFromUnknown()))
	case *graphv1.EventEnvelope_UpsertEdge:
		e := body.UpsertEdge
		return fmt.Sprintf("edge %s %s", e.GetType(), at(e.GetValidAt(), e.GetValidFromUnknown()))
	case *graphv1.EventEnvelope_ObserveChange:
		c := body.ObserveChange
		return fmt.Sprintf("change %s targets=%d %s", c.GetChange().GetKind(), len(c.GetTargets()), at(c.GetValidAt(), c.GetValidFromUnknown()))
	case *graphv1.EventEnvelope_IdentityClaim:
		c := body.IdentityClaim
		return fmt.Sprintf("claim %s on %s attrs=%s", c.GetClaim().GetNamespace(), c.GetSubject().GetNamespace(), attrKeys(c.GetAttributes().GetFields()))
	case *graphv1.EventEnvelope_CorrelateEntity:
		c := body.CorrelateEntity
		return fmt.Sprintf("correlate %s on %s", c.GetKey().GetNamespace(), c.GetSubject().GetNamespace())
	case *graphv1.EventEnvelope_AlertTransition:
		a := body.AlertTransition
		return fmt.Sprintf("alert-transition %s->%s watches=%d", a.GetFromState(), a.GetToState(), len(a.GetWatches()))
	case *graphv1.EventEnvelope_SourceCheckpoint:
		return "checkpoint"
	default:
		return fmt.Sprintf("other %T", body)
	}
}

func attrKeys[V any](m map[string]V) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

func union(a, b map[string]int) []string {
	seen := map[string]bool{}
	for k := range a {
		seen[k] = true
	}
	for k := range b {
		seen[k] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func total(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}
