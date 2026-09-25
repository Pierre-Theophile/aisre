// SPDX-License-Identifier: Apache-2.0

package feeder_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	eventlog "github.com/Pierre-Theophile/aisre/internal/log"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// mustTime parses an RFC 3339 instant or fails the test.
func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return parsed.UTC()
}

func TestPropsBuildAcceptsTheFixtureVocabulary(t *testing.T) {
	t.Parallel()
	props, err := feeder.NewProps().
		Str(feeder.AttrK8sNamespaceName, "shop").
		Str(feeder.AttrK8sDeploymentName, "checkout").
		Str(feeder.AttrK8sClusterName, "shop-prod").
		Str(feeder.AttrDeploymentEnvironment, "prod").
		Str(feeder.PropK8sRevision, "7").
		Str(feeder.PropK8sResourceVersion, "1001").
		Int(feeder.PropK8sReplicas, 3).
		Int(feeder.AttrServerPort, 6379).
		Bool(feeder.PropThirdParty, true).
		Float(feeder.PropWindowSeconds, 300).
		Strs(feeder.PropK8sReference, "env", "volume").
		Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got, want := len(props.GetFields()), 11; got != want {
		t.Fatalf("built %d properties, want %d", got, want)
	}
	if got := props.GetFields()[feeder.PropK8sReplicas].GetNumberValue(); got != 3 {
		t.Errorf("replicas = %v, want 3", got)
	}
}

func TestPropsBuildEmptyIsNil(t *testing.T) {
	t.Parallel()
	// An absent props struct and an empty one serialize differently, so "no properties" must
	// stay absent or every golden shifts.
	props, err := feeder.NewProps().Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if props != nil {
		t.Fatalf("Build on an empty builder = %v, want nil", props)
	}
}

func TestPropsRejectsEmptyKey(t *testing.T) {
	t.Parallel()
	if _, err := feeder.NewProps().Str("", "x").Build(); err == nil {
		t.Fatal("Build accepted an empty property name")
	}
}

// TestPropsDenylistParity is the test that matters: for every shape internal/log refuses, the
// SDK must refuse it with the same reason code, before the event leaves the process. The
// expectation is not hard-coded — it is whatever the graph's own validator says — so the two
// cannot drift.
func TestPropsDenylistParity(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		props *structpb.Struct
	}{
		{
			name:  "a bare metric value",
			props: mustStruct(t, map[string]any{"metric_value": 12.5}),
		},
		{
			name:  "a namespaced value key",
			props: mustStruct(t, map[string]any{"sre.config.value": "s3cret"}),
		},
		{
			name:  "a log body",
			props: mustStruct(t, map[string]any{"log_body": "connection refused"}),
		},
		{
			name:  "a bare body",
			props: mustStruct(t, map[string]any{"body": "connection refused"}),
		},
		{
			name:  "a span id",
			props: mustStruct(t, map[string]any{"span_id": "00f067aa0ba902b7"}),
		},
		{
			name:  "a trace id",
			props: mustStruct(t, map[string]any{"trace_id": "4bf92f3577b34da6a3ce929d0e0e4736"}),
		},
		{
			name:  "samples",
			props: mustStruct(t, map[string]any{"samples": []any{1.0, 2.0}}),
		},
		{
			name:  "a numeric array under an innocent name",
			props: mustStruct(t, map[string]any{"latency_samples": []any{1.0, 2.0, 3.0}}),
		},
		{
			name:  "a denylisted key nested inside a struct",
			props: mustStruct(t, map[string]any{"sre.debug": map[string]any{"trace_id": "abc"}}),
		},
		{
			name:  "a property larger than the size limit",
			props: mustStruct(t, map[string]any{"sre.note": strings.Repeat("x", 5<<10)}),
		},
		{
			name: "a secret node carrying its value",
			props: mustStruct(t, map[string]any{
				feeder.PropConfigKind: "secret",
				"data":                "hunter2",
			}),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// What the graph itself would say about exactly these properties.
			want := eventlog.Validate(probeEnvelope(tc.props), nil)
			if want == nil {
				t.Fatalf("internal/log accepted %s; this test's premise is wrong", tc.name)
			}

			got := feeder.ValidateProps(tc.props)
			if got == nil {
				t.Fatalf("ValidateProps accepted %s, which the graph refuses with %s", tc.name, want.ReasonCode)
			}
			if got.ReasonCode != want.ReasonCode {
				t.Errorf("reason code %q, want %q (the graph's own answer)", got.ReasonCode, want.ReasonCode)
			}
			// The detail is rewritten from the probe's field path to the caller's, and
			// otherwise says exactly what the graph would have said.
			if strings.Contains(got.ReasonDetail, "upsert_node.") {
				t.Errorf("reason detail still names the probe envelope: %q", got.ReasonDetail)
			}
			if !strings.HasPrefix(got.ReasonDetail, "props.") && !strings.Contains(got.ReasonDetail, "props.") {
				t.Errorf("reason detail %q does not name the property", got.ReasonDetail)
			}
		})
	}
}

func TestPropsBuildRejectsTelemetryFromTypedSetters(t *testing.T) {
	t.Parallel()
	// The typed setters make a numeric series awkward but not impossible: a denylisted *name*
	// is still reachable, and Build is what catches it.
	_, err := feeder.NewProps().Float("metric_value", 12.5).Build()
	if err == nil {
		t.Fatal("Build accepted a metric_value property")
	}
	var rejection *feeder.Rejection
	if !errors.As(err, &rejection) {
		t.Fatalf("Build returned %T, want a *feeder.Rejection a caller can match on", err)
	}
	if rejection.ReasonCode != feeder.ReasonTelemetryPayload {
		t.Errorf("reason code %q, want %q", rejection.ReasonCode, feeder.ReasonTelemetryPayload)
	}
}

func TestPropsFrom(t *testing.T) {
	t.Parallel()
	props, err := feeder.PropsFrom(map[string]string{
		feeder.AttrServiceName:      "checkout",
		feeder.AttrServiceNamespace: "shop",
	}).Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := props.GetFields()[feeder.AttrServiceName].GetStringValue(); got != "checkout" {
		t.Errorf("service.name = %q, want checkout", got)
	}
}

func TestPropsTime(t *testing.T) {
	t.Parallel()
	props, err := feeder.NewProps().Time("sre.k8s.started_at", mustTime(t, "2026-09-01T13:00:00Z")).Build()
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got, want := props.GetFields()["sre.k8s.started_at"].GetStringValue(), "2026-09-01T13:00:00Z"; got != want {
		t.Errorf("timestamp property = %q, want %q", got, want)
	}
}

// probeEnvelope wraps props the way ValidateProps does internally, so the test compares the
// two validators on identical input.
func probeEnvelope(props *structpb.Struct) *graphv1.EventEnvelope {
	return &graphv1.EventEnvelope{
		EventId:       "feeder.props.probe",
		SourceId:      "feeder.props.probe",
		SchemaVersion: feeder.SchemaVersion,
		Body: &graphv1.EventEnvelope_UpsertNode{UpsertNode: &graphv1.UpsertNode{
			Ref:     feeder.Ref("feeder.props", "probe"),
			Type:    graphv1.NodeType_CONFIG,
			Props:   props,
			ValidAt: timestamppb.New(time.Unix(0, 0).UTC()),
		}},
	}
}

func mustStruct(t *testing.T, fields map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(fields)
	if err != nil {
		t.Fatalf("structpb.NewStruct: %v", err)
	}
	return s
}
