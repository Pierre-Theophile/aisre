// SPDX-License-Identifier: Apache-2.0

package graph_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
)

func mustStruct(t *testing.T, fields map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(fields)
	if err != nil {
		t.Fatalf("structpb.NewStruct: %v", err)
	}
	return s
}

func mustCanonical(t *testing.T, v any) string {
	t.Helper()
	b, err := graph.CanonicalJSON(v)
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	return string(b)
}

func TestCanonicalJSONProtoMessage(t *testing.T) {
	t.Parallel()

	node := &graphv1.NodeVersion{
		EntityId:    checkoutEntityID,
		VersionId:   "cmh3lpki4sx7jrlxrne3rjzkgz",
		Type:        graphv1.NodeType_SERVICE,
		DisplayName: "checkout",
		Valid:       graph.Interval{Start: t1301, StartUnknown: true}.Proto(),
		Observed:    graph.NewInterval(t1301, t1500).Proto(),
		Props: mustStruct(t, map[string]any{
			"zone":               "eu-west-1a",
			"service.version":    "4.2",
			"k8s.namespace.name": "demo",
		}),
	}

	got := mustCanonical(t, node)
	want := `{"displayName":"checkout",` +
		`"entityId":"xvi7oufivacscbjrosgfu4k6f5",` +
		`"observed":{"end":"2026-09-01T15:00:00Z","start":"2026-09-01T13:01:00Z"},` +
		`"props":{"k8s.namespace.name":"demo","service.version":"4.2","zone":"eu-west-1a"},` +
		`"type":"SERVICE",` +
		`"valid":{"start":"2026-09-01T13:01:00Z","startUnknown":true},` +
		`"versionId":"cmh3lpki4sx7jrlxrne3rjzkgz"}`
	if got != want {
		t.Errorf("CanonicalJSON:\n got %s\nwant %s", got, want)
	}

	if strings.ContainsAny(got, " \n\t") {
		t.Errorf("output carries insignificant whitespace: %s", got)
	}
	if strings.Contains(got, `"change"`) || strings.Contains(got, `"aliases"`) {
		t.Errorf("unpopulated fields must be omitted: %s", got)
	}
}

// protojson deliberately randomizes the whitespace it emits, which is why every value is
// re-encoded here. Goldens are compared byte for byte, so this must never vary.
func TestCanonicalJSONIsStableAcrossCalls(t *testing.T) {
	t.Parallel()

	msg := &graphv1.EdgeVersion{
		VersionId:   "befb7galcg6dmlkoexu57fqqis",
		SrcId:       checkoutEntityID,
		DstId:       paymentsEntityID,
		Type:        graphv1.EdgeType_CALLS,
		Valid:       graph.NewInterval(t1301, t1420).Proto(),
		WeightClass: proto.Uint32(3),
		Props:       mustStruct(t, map[string]any{"b": 2.0, "a": 1.0, "c": 3.0}),
	}

	first := mustCanonical(t, msg)
	for range 50 {
		if got := mustCanonical(t, msg); got != first {
			t.Fatalf("output is not stable:\n%s\n%s", first, got)
		}
	}
}

func TestCanonicalJSONIsIdempotent(t *testing.T) {
	t.Parallel()

	inputs := []any{
		&graphv1.NodeVersion{
			VersionId: "v1",
			Valid:     graph.NewInterval(t1300, t1420).Proto(),
			Props:     mustStruct(t, map[string]any{"z": 1.0, "a": "x", "nested": map[string]any{"b": true, "a": nil}}),
		},
		graph.NewInterval(t1300, t1420),
		map[string]any{"b": []any{3.0, 1.0, 2.0}, "a": "2026-09-01T13:00:00.000Z"},
	}
	for _, in := range inputs {
		once := mustCanonical(t, in)
		twice := mustCanonical(t, json.RawMessage(once))
		if once != twice {
			t.Errorf("canonicalizing twice changed the bytes:\n%s\n%s", once, twice)
		}
	}
}

func TestCanonicalJSONSortsProtoStructFields(t *testing.T) {
	t.Parallel()

	props := mustStruct(t, map[string]any{
		"zeta": "z", "alpha": "a", "Mid": "m", "9numeric": "n",
		"nested": map[string]any{"y": 1.0, "x": 2.0},
	})
	got := mustCanonical(t, &graphv1.NodeVersion{Props: props})
	want := `{"props":{"9numeric":"n","Mid":"m","alpha":"a","nested":{"x":2,"y":1},"zeta":"z"}}`
	if got != want {
		t.Errorf("struct keys are not in byte order:\n got %s\nwant %s", got, want)
	}
}

func TestCanonicalJSONNormalizesTimestamps(t *testing.T) {
	t.Parallel()

	type record struct {
		At    time.Time `json:"at"`
		Label string    `json:"label"`
	}
	berlin := time.FixedZone("CEST", 2*60*60)

	tests := []struct {
		name string
		in   any
		want string
	}{
		{
			name: "a non-UTC time.Time is normalized",
			in:   record{At: t1432.In(berlin), Label: "alert"},
			want: `{"at":"2026-09-01T14:32:00Z","label":"alert"}`,
		},
		{
			name: "nanoseconds survive",
			in:   record{At: t1432.Add(123456789 * time.Nanosecond)},
			want: `{"at":"2026-09-01T14:32:00.123456789Z","label":""}`,
		},
		{
			name: "an offset timestamp inside a plain map is normalized",
			in:   map[string]string{"t": "2026-09-01T16:32:00+02:00"},
			want: `{"t":"2026-09-01T14:32:00Z"}`,
		},
		{
			name: "a protobuf Timestamp uses the same spelling",
			in:   timestamppb.New(t1432.Add(100 * time.Millisecond)),
			want: `"2026-09-01T14:32:00.100Z"`,
		},
		{
			name: "ordinary strings are untouched",
			in:   map[string]string{"selector": `rate(http_requests_total{service="checkout"}[5m])`},
			want: `{"selector":"rate(http_requests_total{service=\"checkout\"}[5m])"}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := mustCanonical(t, tc.in); got != tc.want {
				t.Errorf("CanonicalJSON:\n got %s\nwant %s", got, tc.want)
			}
		})
	}
}

// The domain Interval and its protobuf counterpart must serialize identically: fixtures mix
// both and a golden diff must not depend on which side produced a value.
func TestCanonicalJSONIntervalMatchesProto(t *testing.T) {
	t.Parallel()

	intervals := []graph.Interval{
		graph.NewInterval(t1300, t1420),
		graph.NewInterval(t1300, time.Time{}),
		{Start: t1301, StartUnknown: true},
		{Start: t1301, EndUnknown: true},
		{Start: t1300.Add(100 * time.Millisecond), End: t1420.Add(1234 * time.Nanosecond)},
		{},
	}
	for _, iv := range intervals {
		domain := mustCanonical(t, iv)
		fromProto := mustCanonical(t, iv.Proto())
		if domain != fromProto {
			t.Errorf("%s: domain form %s != protobuf form %s", iv, domain, fromProto)
		}
	}
}

func TestCanonicalJSONPreservesNumbers(t *testing.T) {
	t.Parallel()

	// protojson renders 64-bit integers as strings and doubles as numbers; neither may be
	// rewritten by the re-encoding step.
	got := mustCanonical(t, &graphv1.EventEnvelope{
		EventId:   "otel:demo:evt-1",
		SourceSeq: proto.Int64(9007199254740993), // beyond float64's exact integer range
	})
	want := `{"eventId":"otel:demo:evt-1","sourceSeq":"9007199254740993"}`
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}

	if got := mustCanonical(t, json.RawMessage(`{"a":1e400,"b":0.1000000000000000000001}`)); got != `{"a":1e400,"b":0.1000000000000000000001}` {
		t.Errorf("number literals were rewritten: %s", got)
	}
}

func TestCanonicalJSONRejectsBadInput(t *testing.T) {
	t.Parallel()

	if _, err := graph.CanonicalJSON(json.RawMessage(`{"a":1} trailing`)); err == nil {
		t.Error("trailing data must be rejected")
	}
	if _, err := graph.CanonicalJSON(make(chan int)); err == nil {
		t.Error("an unmarshalable value must be rejected")
	}
}

func TestCanonicalJSONL(t *testing.T) {
	t.Parallel()

	events := []proto.Message{
		&graphv1.EventEnvelope{EventId: "otel:demo:evt-1", SourceId: "otel:demo"},
		&graphv1.EventEnvelope{EventId: "otel:demo:evt-2", SourceId: "otel:demo"},
	}
	got, err := graph.CanonicalJSONL(events)
	if err != nil {
		t.Fatalf("CanonicalJSONL: %v", err)
	}
	want := `{"eventId":"otel:demo:evt-1","sourceId":"otel:demo"}` + "\n" +
		`{"eventId":"otel:demo:evt-2","sourceId":"otel:demo"}` + "\n"
	if string(got) != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if lines := bytes.Count(got, []byte("\n")); lines != len(events) {
		t.Errorf("got %d newlines for %d events", lines, len(events))
	}

	empty, err := graph.CanonicalJSONL(nil)
	if err != nil || len(empty) != 0 {
		t.Errorf("CanonicalJSONL(nil) = %q, %v", empty, err)
	}
}

func TestSortVersions(t *testing.T) {
	t.Parallel()

	nodes := []*graphv1.NodeVersion{
		{VersionId: "c"}, {VersionId: "a"}, {VersionId: "b"}, {VersionId: "a", DisplayName: "second a"},
	}
	graph.SortNodeVersions(nodes)
	var ids []string
	for _, n := range nodes {
		ids = append(ids, n.GetVersionId())
	}
	if strings.Join(ids, "") != "aabc" {
		t.Errorf("SortNodeVersions gave %v", ids)
	}
	if nodes[1].GetDisplayName() != "second a" {
		t.Error("SortNodeVersions must be stable")
	}

	edges := []*graphv1.EdgeVersion{{VersionId: "z"}, {VersionId: "m"}, {VersionId: "a"}}
	graph.SortEdgeVersions(edges)
	if edges[0].GetVersionId() != "a" || edges[2].GetVersionId() != "z" {
		t.Errorf("SortEdgeVersions gave %v", edges)
	}

	graph.SortNodeVersions(nil) // must not panic
	graph.SortEdgeVersions(nil)
}
