// SPDX-License-Identifier: Apache-2.0

package log_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	eventlog "github.com/Pierre-Theophile/aisre/internal/log"
)

// One rejected sample per published reason code (FR-024, data-model.md "Validation rules"), plus
// the sample the baseline fixture ships as its telemetry-rejection case (SC-009).

func validAt() *timestamppb.Timestamp {
	return timestamppb.New(mustTime("2026-09-01T13:00:00Z"))
}

func envelope(body any) *graphv1.EventEnvelope {
	env := &graphv1.EventEnvelope{
		EventId:        "otel:demo:e1",
		IdempotencyKey: "otel:demo:e1",
		SourceId:       "otel:demo",
		SchemaVersion:  "1.0.0",
	}
	switch b := body.(type) {
	case *graphv1.UpsertNode:
		env.Body = &graphv1.EventEnvelope_UpsertNode{UpsertNode: b}
	case *graphv1.UpsertEdge:
		env.Body = &graphv1.EventEnvelope_UpsertEdge{UpsertEdge: b}
	case *graphv1.RetractNode:
		env.Body = &graphv1.EventEnvelope_RetractNode{RetractNode: b}
	case *graphv1.IdentityClaim:
		env.Body = &graphv1.EventEnvelope_IdentityClaim{IdentityClaim: b}
	case nil:
	}
	return env
}

func serviceNode() *graphv1.UpsertNode {
	return &graphv1.UpsertNode{
		Ref:         &graphv1.Ref{Namespace: "otel.service.name", Value: "checkout"},
		Type:        graphv1.NodeType_SERVICE,
		DisplayName: "checkout",
		ValidAt:     validAt(),
		Props: mustStruct(map[string]any{
			"service.name":      "checkout",
			"service.namespace": "shop",
		}),
	}
}

func TestValidate(t *testing.T) {
	t.Parallel()

	accepted := eventlog.SchemaVersions{"1.0.0"}

	tests := []struct {
		name       string
		env        *graphv1.EventEnvelope
		wantCode   string
		wantDetail string
	}{
		{
			name: "accepted upsert node",
			env:  envelope(serviceNode()),
		},
		{
			name: "no body is untyped",
			env:  envelope(nil),
			// FR-017: sources communicate only in typed events; a bare envelope is not one.
			wantCode:   eventlog.ReasonUntyped,
			wantDetail: "body",
		},
		{
			name: "missing event id",
			env: func() *graphv1.EventEnvelope {
				env := envelope(serviceNode())
				env.EventId = ""
				return env
			}(),
			wantCode:   eventlog.ReasonMissingEventID,
			wantDetail: "event_id",
		},
		{
			name: "missing source id",
			env: func() *graphv1.EventEnvelope {
				env := envelope(serviceNode())
				env.SourceId = ""
				return env
			}(),
			wantCode:   eventlog.ReasonMissingSource,
			wantDetail: "source_id",
		},
		{
			name: "unknown schema version names the accepted set",
			env: func() *graphv1.EventEnvelope {
				env := envelope(serviceNode())
				env.SchemaVersion = "0.9.0"
				return env
			}(),
			wantCode:   eventlog.ReasonUnknownSchemaVersion,
			wantDetail: "1.0.0",
		},
		{
			name: "upsert without valid time",
			env: func() *graphv1.EventEnvelope {
				node := serviceNode()
				node.ValidAt = nil
				return envelope(node)
			}(),
			wantCode:   eventlog.ReasonMissingValidTime,
			wantDetail: "upsert_node.valid_at",
		},
		{
			name: "valid_from_unknown stands in for a missing start",
			env: func() *graphv1.EventEnvelope {
				node := serviceNode()
				node.ValidAt = nil
				node.ValidFromUnknown = true
				return envelope(node)
			}(),
		},
		{
			name: "unspecified node type",
			env: func() *graphv1.EventEnvelope {
				node := serviceNode()
				node.Type = graphv1.NodeType_NODE_TYPE_UNSPECIFIED
				return envelope(node)
			}(),
			wantCode:   eventlog.ReasonUnknownType,
			wantDetail: "upsert_node.type",
		},
		{
			name: "missing ref",
			env: func() *graphv1.EventEnvelope {
				node := serviceNode()
				node.Ref = &graphv1.Ref{Namespace: "otel.service.name"}
				return envelope(node)
			}(),
			wantCode:   eventlog.ReasonMissingRef,
			wantDetail: "upsert_node.ref",
		},
		{
			name: "denylisted property key",
			env: func() *graphv1.EventEnvelope {
				node := serviceNode()
				node.Props = mustStruct(map[string]any{"span_id": "0123456789abcdef"})
				return envelope(node)
			}(),
			wantCode:   eventlog.ReasonTelemetryPayload,
			wantDetail: "span_id",
		},
		{
			name: "nested denylisted key",
			env: func() *graphv1.EventEnvelope {
				node := serviceNode()
				node.Props = mustStruct(map[string]any{
					"sre.sample": map[string]any{"metric_value": 12.5},
				})
				return envelope(node)
			}(),
			wantCode:   eventlog.ReasonTelemetryPayload,
			wantDetail: "sre.sample.metric_value",
		},
		{
			name: "numeric sample series",
			env: func() *graphv1.EventEnvelope {
				node := serviceNode()
				node.Props = mustStruct(map[string]any{"latency_samples": []any{1.0, 2.0, 3.0}})
				return envelope(node)
			}(),
			wantCode:   eventlog.ReasonTelemetryPayload,
			wantDetail: "latency_samples",
		},
		{
			name: "oversized property",
			env: func() *graphv1.EventEnvelope {
				node := serviceNode()
				node.Props = mustStruct(map[string]any{"sre.note": strings.Repeat("x", 5000)})
				return envelope(node)
			}(),
			wantCode:   eventlog.ReasonTelemetryPayload,
			wantDetail: "sre.note",
		},
		{
			name: "a list of two numbers is already a series, a single number is not",
			env: func() *graphv1.EventEnvelope {
				node := serviceNode()
				node.Props = mustStruct(map[string]any{"server.port": 5432.0})
				return envelope(node)
			}(),
		},
		{
			name: "secret carrying its value",
			env: func() *graphv1.EventEnvelope {
				node := &graphv1.UpsertNode{
					Ref:     &graphv1.Ref{Namespace: "k8s.secret", Value: "shop/payments-secret"},
					Type:    graphv1.NodeType_CONFIG,
					ValidAt: validAt(),
					Props: mustStruct(map[string]any{
						"sre.config.kind":    "secret",
						"sre.config.version": "3",
						"data":               "aGVsbG8=",
					}),
				}
				return envelope(node)
			}(),
			wantCode:   eventlog.ReasonSecretValue,
			wantDetail: "data",
		},
		{
			name: "secret carrying only its version is fine",
			env: func() *graphv1.EventEnvelope {
				node := &graphv1.UpsertNode{
					Ref:     &graphv1.Ref{Namespace: "k8s.secret", Value: "shop/payments-secret"},
					Type:    graphv1.NodeType_CONFIG,
					ValidAt: validAt(),
					Props: mustStruct(map[string]any{
						"sre.config.kind":    "secret",
						"sre.config.version": "3",
					}),
				}
				return envelope(node)
			}(),
		},
		{
			name: "retraction without a valid end",
			env: envelope(&graphv1.RetractNode{
				Ref: &graphv1.Ref{Namespace: "otel.service.name", Value: "legacy-cart"},
			}),
			wantCode:   eventlog.ReasonMissingValidTime,
			wantDetail: "retract_node.valid_end",
		},
		{
			name: "identity claim without a claim",
			env: envelope(&graphv1.IdentityClaim{
				Subject: &graphv1.Ref{Namespace: "otel.service.name", Value: "checkout"},
			}),
			wantCode:   eventlog.ReasonMissingRef,
			wantDetail: "identity_claim.claim",
		},
		{
			name: "edge with an unspecified type",
			env: envelope(&graphv1.UpsertEdge{
				Src:     &graphv1.Ref{Namespace: "otel.service.name", Value: "checkout"},
				Dst:     &graphv1.Ref{Namespace: "otel.service.name", Value: "payments"},
				ValidAt: validAt(),
			}),
			wantCode:   eventlog.ReasonUnknownType,
			wantDetail: "upsert_edge.type",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := eventlog.Validate(tc.env, accepted)
			if tc.wantCode == "" {
				if got != nil {
					t.Fatalf("Validate() = %s, want accepted", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("Validate() accepted the event, want %s", tc.wantCode)
			}
			if got.ReasonCode != tc.wantCode {
				t.Errorf("reason_code = %q, want %q (detail %q)", got.ReasonCode, tc.wantCode, got.ReasonDetail)
			}
			if !strings.Contains(got.ReasonDetail, tc.wantDetail) {
				t.Errorf("reason_detail = %q, want it to name %q", got.ReasonDetail, tc.wantDetail)
			}
		})
	}
}

// TestValidateFixtureRejection pins the contract the baseline fixture states: its one
// hand-authored bad event must be refused as a telemetry payload, naming the field
// (fixtures/baseline-topology-01/manifest.yaml expect_rejected, SC-009).
func TestValidateFixtureRejection(t *testing.T) {
	t.Parallel()

	path := filepath.Join("..", "..", "fixtures", "baseline-topology-01", "rejected.jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		env := &graphv1.EventEnvelope{}
		if err := (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal([]byte(line), env); err != nil {
			t.Fatalf("unmarshal rejected fixture line: %v", err)
		}
		rejection := eventlog.Validate(env, eventlog.DefaultSchemaVersions)
		if rejection == nil {
			t.Fatalf("event %s was accepted, the fixture requires a rejection", env.GetEventId())
		}
		if rejection.ReasonCode != eventlog.ReasonTelemetryPayload {
			t.Errorf("reason_code = %q, want %q", rejection.ReasonCode, eventlog.ReasonTelemetryPayload)
		}
		if !strings.Contains(rejection.ReasonDetail, "latency_samples") {
			t.Errorf("reason_detail = %q, want it to name latency_samples", rejection.ReasonDetail)
		}
	}
}

func mustStruct(m map[string]any) *structpb.Struct {
	s, err := structpb.NewStruct(m)
	if err != nil {
		panic(err)
	}
	return s
}
