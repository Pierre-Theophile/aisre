// SPDX-License-Identifier: Apache-2.0

package github_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/record"
)

// T110: the refusal path, for a deploy feeder (FR-067, SC-008).
//
// A deploy feeder is the connector most tempted to attach a measurement to what it emits, because
// the platform hands it one next to the change: the canary analysis the pipeline ran, the log of the
// step that failed, GitHub's own free-form deployment `payload`. Each would make the rollout node
// "more useful" and each is a telemetry payload under a new name. The feeder under test never emits
// one — which is exactly why this fixture has to carry them by hand, since a fixture built only from
// the feeder's output could not tell a working refusal from an absent one.
//
// All three are addressed at the SAME change the recorded stream creates. A refusal that landed on
// a node nobody else describes would prove only that an orphan was dropped; this proves an existing
// rollout was not amended. Each one exercises a different branch of the check, so between them the
// three rules the log applies — a numeric series, a denied key at depth, and the size limit — are
// all reached from a deploy-shaped event rather than only from 003's service-shaped ones.
const refusalChangeRef = "repositories/555/deployments/4321/targets/k8s.deployment/shop/storefront"

func telemetryRejectionFixture(t *testing.T) fixtureSpec {
	t.Helper()
	baseline := baselineFixture()

	// A numeric series: the canary's error rate across the analysis steps. Refused on SHAPE, whatever
	// the key is called.
	series := refusedChangeJSON(t, "github:twin:telemetry-canary-series-1", map[string]any{
		"sre.github.canary_error_rate": []any{0.2, 0.4, 3.1, 7.8},
	})
	// A denied key one level down: the failing step's log, pasted onto the rollout.
	logBody := refusedChangeJSON(t, "github:twin:telemetry-log-body-1", map[string]any{
		"sre.github.failed_step": map[string]any{
			"name":     "smoke test",
			"log_body": "GET /checkout 502 upstream connect error",
		},
	})
	// GitHub's deployment `payload` is free-form JSON the deployer chooses, and it is where a pipeline
	// puts whatever it likes. No denied key and no numbers: only the size limit catches it, which is
	// the branch neither of the other two reaches.
	oversized := refusedChangeJSON(t, "github:twin:telemetry-deployment-payload-1", map[string]any{
		"sre.github.deployment_payload": strings.Repeat(`{"step":"verify","status":"ok"},`, 160),
	})

	return fixtureSpec{
		dir:    "fixtures/deploy-telemetry-rejection-01",
		family: "deploy-refusal",
		description: "The refusal path for a deploy feeder: three events addressed at the rollout the " +
			"recorded stream creates, each carrying a measurement the platform hands a deploy pipeline — " +
			"the canary's error-rate series, the failing step's log body under a denied key, and a " +
			"deployment payload above the property size limit — are each REJECTED with the published " +
			"reason code `telemetry_payload`, and the rollout is left carrying its pointers and none of " +
			"the three. A pointer says where to look and never what was found (constitution IV). The " +
			"feeder under test would never emit one of these, which is why the fixture carries them by " +
			"hand: a fixture built only from its output could not tell a working refusal from an absent one.",
		options:  baseline.options,
		payloads: append([]feeder.Payload(nil), baseline.payloads...),
		rejected: []string{series, logBody, oversized},
		expectRejected: []record.Rejection{
			{EventID: "github:twin:telemetry-canary-series-1", ReasonCode: "telemetry_payload"},
			{EventID: "github:twin:telemetry-log-body-1", ReasonCode: "telemetry_payload"},
			{EventID: "github:twin:telemetry-deployment-payload-1", ReasonCode: "telemetry_payload"},
		},
		queries: queriesYAML(
			subgraphQuery("rollout-2hop-after-the-refusals", "github.change="+refusalChangeRef,
				statusSucceededAt,
				"The rollout after the three refusals. The golden is the assertion: the change is here\n"+
					"with its pointers, and no property of it or of anything within two hops holds any\n"+
					"part of what the refused events carried."),
			fixtureQuery{
				name: "rollout-pointers", kind: "pointers",
				focus:   "github.change=" + refusalChangeRef,
				validAt: statusSucceededAt,
				comment: "Where the canary and the log live: pointers, which is the only form the graph\n" +
					"accepts them in.",
			},
		),
	}
}

// refusedChangeJSON renders one hand-authored change event that is valid in every respect but its
// properties, so the reason it is refused with can only be the one the fixture asserts.
func refusedChangeJSON(t *testing.T, eventID string, props map[string]any) string {
	t.Helper()
	all := map[string]any{"deployment.environment.name": "production"}
	for k, v := range props {
		all[k] = v
	}
	event := map[string]any{
		"eventId":          eventID,
		"idempotencyKey":   eventID,
		"schemaVersion":    "1.0.0",
		"sourceId":         "github:twin",
		"sourceObservedAt": fixtureSecond.Format(time.RFC3339),
		"observeChange": map[string]any{
			"change": map[string]any{
				"actor":     "ada",
				"actorKind": "PERSON",
				"kind":      "ROLLOUT",
				"summary":   "deployed acme/storefront to shop/storefront on production",
			},
			"props":   all,
			"ref":     map[string]any{"namespace": "github.change", "value": refusalChangeRef},
			"targets": []any{map[string]any{"namespace": "k8s.deployment", "value": "shop/storefront"}},
			"validAt": statusSucceededAt,
		},
	}
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal %s: %v", eventID, err)
	}
	return string(raw)
}
