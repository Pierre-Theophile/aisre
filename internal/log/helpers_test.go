// SPDX-License-Identifier: Apache-2.0

package log_test

import (
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
)

// Envelope builders shared with internal/event's validation tests.

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
	case *graphv1.ObserveChange:
		env.Body = &graphv1.EventEnvelope_ObserveChange{ObserveChange: b}
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

func mustStruct(m map[string]any) *structpb.Struct {
	s, err := structpb.NewStruct(m)
	if err != nil {
		panic(err)
	}
	return s
}
