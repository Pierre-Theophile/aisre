// SPDX-License-Identifier: Apache-2.0

package log_test

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	eventlog "github.com/Pierre-Theophile/aisre/internal/log"
)

// One rejected sample per reason the five investigation bodies can be refused for (ADR-0005
// D2/D3, 002 FR-034, 002 SC-010), and one accepted sample per body so that the rules are shown
// to admit the records 002 actually writes rather than merely to refuse things.
//
// The two rules under test are a pair. The **allow-list** says where a decision record's
// properties may live — `sre.investigation.*`, nowhere else — so that a future author has no
// plausible key to hang "just the series that proves it" on. The **denylist** then applies
// inside the namespace exactly as it applies everywhere else, so that a series is still a
// series, a 4 KiB property is still a payload, and `sre.investigation.value` is refused for the
// same reason `value` always was. SC-010 is the second half; the first is what makes the second
// enforceable.

func ts(s string) *timestamppb.Timestamp { return timestamppb.New(mustTime(s)) }

func investigationEnvelope(body any) *graphv1.EventEnvelope {
	env := &graphv1.EventEnvelope{
		EventId:       "inv:demo:e1",
		SourceId:      "investigation:demo",
		SchemaVersion: "1.0.0",
	}
	switch b := body.(type) {
	case *graphv1.RecordInvestigation:
		env.Body = &graphv1.EventEnvelope_RecordInvestigation{RecordInvestigation: b}
	case *graphv1.SubmitHumanFact:
		env.Body = &graphv1.EventEnvelope_SubmitHumanFact{SubmitHumanFact: b}
	case *graphv1.ReopenInvestigation:
		env.Body = &graphv1.EventEnvelope_ReopenInvestigation{ReopenInvestigation: b}
	case *graphv1.LabelInvestigation:
		env.Body = &graphv1.EventEnvelope_LabelInvestigation{LabelInvestigation: b}
	case *graphv1.AlertTransition:
		env.Body = &graphv1.EventEnvelope_AlertTransition{AlertTransition: b}
	}
	return env
}

// decisionRecord is a well-formed conclusion: identifiers, statuses, confidences and a digest,
// every one of them under the allow-listed namespace, none of them telemetry.
func decisionRecord() *graphv1.RecordInvestigation {
	return &graphv1.RecordInvestigation{
		InvestigationId: "inv-0007",
		Subjects:        []*graphv1.Ref{{Namespace: "otel.service.name", Value: "checkout"}},
		TargetEntities:  []*graphv1.Ref{{Namespace: "otel.service.name", Value: "payments"}},
		StartedAt:       ts("2026-09-01T14:32:00Z"),
		EndedAt:         ts("2026-09-01T14:36:30Z"),
		Outcome:         "ranked",
		StopReason:      "diminishing_returns",
		VerdictLine:     "the payments rollout to rev7 is the most likely cause",
		Hypotheses: mustStruct(map[string]any{
			"sre.investigation.hypotheses": map[string]any{
				"h1": map[string]any{
					"statement":  "payments@rev7 caused the checkout error rate",
					"status":     "supported",
					"confidence": 0.72,
					"bucket":     "likely",
					"rank":       1,
				},
			},
		}),
		Spend: mustStruct(map[string]any{
			"sre.investigation.spend": map[string]any{
				"model_calls":  9,
				"worker_calls": 14,
			},
		}),
		RecordingKey:    "inv-0007",
		RecordingDigest: "9f2c7a1b4e5d6c8a0b1c2d3e4f5061728394a5b6c7d8e9f0a1b2c3d4e5f60718",
		Requester:       "sre@example.com",
	}
}

func TestValidateInvestigationBodies(t *testing.T) {
	t.Parallel()

	accepted := eventlog.SchemaVersions{"1.0.0"}

	tests := []struct {
		name       string
		env        *graphv1.EventEnvelope
		wantCode   string
		wantDetail string
	}{
		{
			name: "accepted decision record",
			env:  investigationEnvelope(decisionRecord()),
		},
		{
			name: "decision record carrying a series is a telemetry payload",
			env: func() *graphv1.EventEnvelope {
				body := decisionRecord()
				// SC-010: the smuggled sample. It is inside the allow-listed namespace and
				// still refused, because a list of numbers is a series whatever it is called.
				body.Hypotheses = mustStruct(map[string]any{
					"sre.investigation.hypotheses": map[string]any{
						"error_rate": []any{0.01, 0.04, 0.31, 0.62},
					},
				})
				return investigationEnvelope(body)
			}(),
			wantCode:   eventlog.ReasonTelemetryPayload,
			wantDetail: "numeric samples",
		},
		{
			name: "decision record carrying a denylisted key is a telemetry payload",
			env: func() *graphv1.EventEnvelope {
				body := decisionRecord()
				body.Spend = mustStruct(map[string]any{
					"sre.investigation.spend": map[string]any{"value": 12.5},
				})
				return investigationEnvelope(body)
			}(),
			wantCode:   eventlog.ReasonTelemetryPayload,
			wantDetail: "value",
		},
		{
			name: "decision record with an oversize property is a telemetry payload",
			env: func() *graphv1.EventEnvelope {
				body := decisionRecord()
				body.ModelConfig = mustStruct(map[string]any{
					"sre.investigation.model_config": strings.Repeat("x", 5<<10),
				})
				return investigationEnvelope(body)
			}(),
			wantCode:   eventlog.ReasonTelemetryPayload,
			wantDetail: "above the 4096 byte property limit",
		},
		{
			name: "property outside the allow-listed namespace",
			env: func() *graphv1.EventEnvelope {
				body := decisionRecord()
				body.Spend = mustStruct(map[string]any{"tokens": 12000})
				return investigationEnvelope(body)
			}(),
			wantCode:   eventlog.ReasonPropNamespace,
			wantDetail: "sre.investigation.",
		},
		{
			name: "decision record without an interval",
			env: func() *graphv1.EventEnvelope {
				body := decisionRecord()
				body.EndedAt = nil
				return investigationEnvelope(body)
			}(),
			wantCode:   eventlog.ReasonMissingValidTime,
			wantDetail: "record_investigation.ended_at",
		},
		{
			name: "decision record naming a half-written subject",
			env: func() *graphv1.EventEnvelope {
				body := decisionRecord()
				body.Subjects = []*graphv1.Ref{{Namespace: "otel.service.name"}}
				return investigationEnvelope(body)
			}(),
			wantCode:   eventlog.ReasonMissingRef,
			wantDetail: "record_investigation.subjects[0]",
		},
		{
			name: "accepted human fact",
			env: investigationEnvelope(&graphv1.SubmitHumanFact{
				InvestigationId: "inv-0007",
				Kind:            "observation",
				Statement:       "the canary is still on the old build",
				Entities:        []*graphv1.Ref{{Namespace: "otel.service.name", Value: "payments"}},
				Author:          "sre@example.com",
				SubmittedAt:     ts("2026-09-01T14:40:00Z"),
			}),
		},
		{
			name: "anonymous human fact",
			env: investigationEnvelope(&graphv1.SubmitHumanFact{
				InvestigationId: "inv-0007",
				Statement:       "the canary is still on the old build",
				SubmittedAt:     ts("2026-09-01T14:40:00Z"),
			}),
			// FR-041's rule applied to the human channel: a fact nobody signed is not
			// evidence, and it is refused rather than recorded unattributed.
			wantCode:   eventlog.ReasonMissingPrincipal,
			wantDetail: "submit_human_fact.author",
		},
		{
			name: "human fact with no statement",
			env: investigationEnvelope(&graphv1.SubmitHumanFact{
				InvestigationId: "inv-0007",
				Author:          "sre@example.com",
				SubmittedAt:     ts("2026-09-01T14:40:00Z"),
			}),
			wantCode:   eventlog.ReasonMissingRef,
			wantDetail: "submit_human_fact.statement",
		},
		{
			name: "accepted reopen",
			env: investigationEnvelope(&graphv1.ReopenInvestigation{
				ParentInvestigationId: "inv-0007",
				ChildInvestigationId:  "inv-0008",
				Cause:                 "human_fact",
				CauseRef:              "fact-0001",
			}),
		},
		{
			name: "a reopen that names itself is not a reopen",
			env: investigationEnvelope(&graphv1.ReopenInvestigation{
				ParentInvestigationId: "inv-0007",
				ChildInvestigationId:  "inv-0007",
			}),
			wantCode:   eventlog.ReasonMissingRef,
			wantDetail: "a reopen is a NEW investigation",
		},
		{
			name: "accepted label",
			env: investigationEnvelope(&graphv1.LabelInvestigation{
				InvestigationId: "inv-0007",
				WasThisRight:    true,
				Author:          "sre@example.com",
				LabelledAt:      ts("2026-09-01T15:02:00Z"),
			}),
		},
		{
			name: "anonymous label",
			env: investigationEnvelope(&graphv1.LabelInvestigation{
				InvestigationId: "inv-0007",
				LabelledAt:      ts("2026-09-01T15:02:00Z"),
			}),
			wantCode:   eventlog.ReasonMissingPrincipal,
			wantDetail: "label_investigation.author",
		},
		{
			name: "accepted alert transition",
			env:  investigationEnvelope(alertTransition()),
		},
		{
			name: "accepted human declaration, which is the same event",
			env: func() *graphv1.EventEnvelope {
				body := alertTransition()
				body.GroupKey = ""
				body.FromState = eventlog.AlertStateOK
				body.ToState = eventlog.AlertStateDeclared
				body.Transport = "human_declared"
				body.ActorKind = graphv1.ActorKind_PERSON
				body.DeclaringIdentity = "sre@example.com"
				return investigationEnvelope(body)
			}(),
		},
		{
			name: "alert transition without a monitor",
			env: func() *graphv1.EventEnvelope {
				body := alertTransition()
				body.Monitor = nil
				return investigationEnvelope(body)
			}(),
			wantCode:   eventlog.ReasonMissingRef,
			wantDetail: "alert_transition.monitor",
		},
		{
			name: "alert transition without an instant",
			env: func() *graphv1.EventEnvelope {
				body := alertTransition()
				body.TransitionAt = nil
				return investigationEnvelope(body)
			}(),
			wantCode:   eventlog.ReasonMissingValidTime,
			wantDetail: "alert_transition.transition_at",
		},
		{
			name: "alert transition without a destination state",
			env: func() *graphv1.EventEnvelope {
				body := alertTransition()
				body.ToState = ""
				return investigationEnvelope(body)
			}(),
			wantCode:   eventlog.ReasonUnknownType,
			wantDetail: "alert_transition.to_state",
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

// TestInvestigationEventTypesAreNamed pins the vocabulary log.events.type carries, which is
// also the vocabulary migration 0005 widened events_type_check to. A body the switch does not
// know returns "", which the NOT NULL column would refuse — loudly, but at the wrong layer.
func TestInvestigationEventTypesAreNamed(t *testing.T) {
	t.Parallel()

	want := map[string]*graphv1.EventEnvelope{
		"record_investigation": investigationEnvelope(decisionRecord()),
		"submit_human_fact":    investigationEnvelope(&graphv1.SubmitHumanFact{}),
		"reopen_investigation": investigationEnvelope(&graphv1.ReopenInvestigation{}),
		"label_investigation":  investigationEnvelope(&graphv1.LabelInvestigation{}),
		"alert_transition":     investigationEnvelope(alertTransition()),
	}
	for typ, env := range want {
		if got := eventlog.EventType(env); got != typ {
			t.Errorf("EventType = %q, want %q", got, typ)
		}
		if eventlog.BodyMessage(env) == nil {
			t.Errorf("BodyMessage(%s) = nil", typ)
		}
	}
}
