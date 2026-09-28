// SPDX-License-Identifier: Apache-2.0

package projector_test

import (
	"context"
	"encoding/json"
	"testing"

	"google.golang.org/protobuf/types/known/structpb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/projector"
)

// The alert lane (005): a transition is folded beside its source's definition of the monitor, not
// in place of it. From the transition onwards the ALERT node carries both what the monitor is (its
// service, environment and type) and what state it is in; a later transition replaces the earlier
// state; and the result does not depend on the order the events arrived in (FR-021).

func monitorDefinitionEvent(id string) *graphv1.EventEnvelope {
	props, _ := structpb.NewStruct(map[string]any{"service.name": "checkout", "deployment.environment.name": "production"})
	return &graphv1.EventEnvelope{
		EventId: id, IdempotencyKey: id, SourceId: propSourceID(0), SchemaVersion: "1.0.0",
		// The poll that listed the monitor, before it fired: the definition holds from there.
		SourceObservedAt: at("2026-09-01T14:00:00Z"),
		Body: &graphv1.EventEnvelope_UpsertNode{UpsertNode: &graphv1.UpsertNode{
			Ref: &graphv1.Ref{Namespace: "gcp.alert", Value: "policy-7"}, Type: graphv1.NodeType_ALERT,
			DisplayName: "checkout error rate", ValidFromUnknown: true, Props: props,
		}},
	}
}

func alertStateEvent(id, from, to, when string) *graphv1.EventEnvelope {
	env := watchAlertEvent(id, "checkout", when)
	env.GetAlertTransition().FromState, env.GetAlertTransition().ToState = from, to
	return env
}

// alertPropsAt reads the current ALERT version's props valid at when, as key → value.
func alertPropsAt(t *testing.T, p *projector.Projector, when string) map[string]any {
	t.Helper()
	var raw []byte
	err := p.Store().Pool().QueryRow(context.Background(), `
		SELECT props FROM graph.entity_versions
		WHERE entity_id = $1 AND upper_inf(observed) AND valid @> $2::timestamptz`,
		graph.EntityID("gcp.alert", "policy-7"), mustTime(when)).Scan(&raw)
	if err != nil {
		t.Fatalf("read the alert at %s: %v", when, err)
	}
	type record struct {
		Value    any    `json:"value"`
		SourceID string `json:"source_id"`
	}
	var set map[string]json.RawMessage
	if err := json.Unmarshal(raw, &set); err != nil {
		t.Fatal(err)
	}
	out := map[string]any{}
	for key, encoded := range set {
		// One record is stored as an object, several as an array (segments.go, propSet).
		var records []record
		if err := json.Unmarshal(encoded, &records); err != nil {
			var one record
			if err := json.Unmarshal(encoded, &one); err != nil {
				t.Fatal(err)
			}
			records = []record{one}
		}
		if len(records) != 1 {
			t.Errorf("%s at %s has %d records; one source says it once", key, when, len(records))
		}
		if records[0].SourceID != propSourceID(0) {
			t.Errorf("%s names source %q; the lane must not leak into provenance", key, records[0].SourceID)
		}
		out[key] = records[0].Value
	}
	return out
}

func TestATransitionKeepsTheMonitorsDefinition(t *testing.T) {
	for name, order := range map[string][]string{
		"definition first":  {"def", "fire", "recover"},
		"transitions first": {"recover", "fire", "def"},
	} {
		t.Run(name, func(t *testing.T) {
			p := projector.New(openStore(t))
			events := map[string]*graphv1.EventEnvelope{
				"def":     monitorDefinitionEvent("d:1"),
				"fire":    alertStateEvent("a:1", "ok", "alert", "2026-09-01T14:21:00Z"),
				"recover": alertStateEvent("a:2", "alert", "ok", "2026-09-01T14:51:00Z"),
			}
			for i, key := range order {
				apply(t, p, events[key], []string{"2026-09-01T15:00:01Z", "2026-09-01T15:00:02Z", "2026-09-01T15:00:03Z"}[i])
			}

			firing := alertPropsAt(t, p, "2026-09-01T14:30:00Z")
			if firing["service.name"] != "checkout" || firing["deployment.environment.name"] != "production" {
				t.Errorf("while firing, the definition is gone: %v", firing)
			}
			if firing[projector.AlertStateProp] != "alert" {
				t.Errorf("while firing, state = %v", firing[projector.AlertStateProp])
			}
			recovered := alertPropsAt(t, p, "2026-09-01T15:00:00Z")
			if recovered[projector.AlertStateProp] != "ok" || recovered["service.name"] != "checkout" {
				t.Errorf("after recovery: %v; the later transition replaces the state and keeps the definition", recovered)
			}
		})
	}
}
