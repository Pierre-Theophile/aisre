// SPDX-License-Identifier: Apache-2.0

package gcp_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	gcpfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/gcp"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/resolution"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// What this feeder emits and what the published rules read must be the same strings (T048, T050,
// FR-117, FR-118, FR-121).
//
// This is the failure mode the two packages are most exposed to and the one that reports nothing. A
// rule reading `sre.gcp.revision_name` and a feeder writing `sre.gcp.revision` do not error, do not
// warn and do not merge: the rule is published, it is registered, it evaluates on every claim, and it
// never fires. The symptom is an absence, and an absence is what this project spends its time trying
// to distinguish from a fact.
//
// `internal/resolution` may not import a feeder — a rule has to be testable without a connector — so
// the agreement is asserted from this side, where the import is legal.

func TestTheFeederAndTheRulesSpellTheSameThings(t *testing.T) {
	t.Parallel()

	for _, pair := range []struct {
		what          string
		feeder, rules string
	}{
		{"the Cloud Run service namespace", gcpfeeder.NSService, resolution.NamespaceGCPCloudRunService},
		{"the Cloud Run revision namespace", gcpfeeder.NSRevision, resolution.NamespaceGCPCloudRunRevision},
		{"the Cloud SQL instance namespace", gcpfeeder.NSSQLInstance, resolution.NamespaceGCPSQLInstance},
		{"the revision name attribute", gcpfeeder.PropRevisionLabel, resolution.AttrGCPRevisionName},
		{"the project attribute", gcpfeeder.PropProject, resolution.AttrGCPProject},
		{"the region attribute", gcpfeeder.PropRegion, resolution.AttrGCPRegion},
		{"the declared-service-name marker", gcpfeeder.AttrDeclaredServiceName, resolution.AttrGCPDeclaredServiceName},
		{"the environment-variable names attribute", gcpfeeder.AttrEnvVarNames, resolution.AttrGCPEnvVarNames},
		{"the instance connection name attribute (C7)", gcpfeeder.AttrSQLConnectionName, resolution.AttrGCPSQLConnectionName},
		{"the bare instance name attribute (P4)", gcpfeeder.AttrSQLInstanceName, resolution.AttrGCPInstanceName},
	} {
		if pair.feeder != pair.rules {
			t.Errorf("%s: the feeder writes %q and the rules read %q; a rule whose attribute the "+
				"feeder never emits never fires, and it fails silently",
				pair.what, pair.feeder, pair.rules)
		}
	}
}

// And the claims the feeder actually emits carry them, all the way through the emitter.
//
// Asserted from the emitted events rather than from Claims() because the attributes travel through a
// second hop — emitClaims builds the event's attribute struct — and a hop that dropped them would
// leave every unit test on Claims() green.
func TestTheClaimsThisFeederEmitsCarryWhatThePublishedRulesRead(t *testing.T) {
	t.Parallel()

	claims := emittedClaims(t)

	declared, ok := claims[feeder.NSOTelService+"=checkout"]
	if !ok {
		t.Fatalf("the declared OpenTelemetry service name was not claimed; claims = %v", keysOf(claims))
	}
	if got := declared[resolution.AttrGCPDeclaredServiceName]; got != gcpfeeder.EnvVarOTelServiceName {
		t.Errorf("the declared-name claim carries %s=%q, want %q — without it C5 cannot tell the "+
			"declaring side from an observed name, so it would fire on two observed names (FR-118)",
			resolution.AttrGCPDeclaredServiceName, got, gcpfeeder.EnvVarOTelServiceName)
	}
	if declared[resolution.AttrEnvironment] != "production" {
		t.Errorf("the declared-name claim states environment %q; C5 requires it on both sides and a "+
			"missing one does not satisfy the rule (FR-118)", declared[resolution.AttrEnvironment])
	}

	service, ok := claims[gcpfeeder.NSService+"=nova-production/europe-west1/checkout"]
	if !ok {
		t.Fatal("the service did not claim the ref it is addressed by (FR-115)")
	}
	if service[resolution.AttrGCPEnvVarNames] == "" {
		t.Errorf("the service claim names no environment variables, so P4 has nothing to read; "+
			"attributes = %v", service)
	}

	revision, ok := claims[gcpfeeder.NSRevision+"=nova-production/europe-west1/checkout/checkout-00042-abc"]
	if !ok {
		t.Fatalf("the revision did not claim the ref it is addressed by; claims = %v", keysOf(claims))
	}
	for attr, want := range map[string]string{
		resolution.AttrGCPRevisionName: "checkout-00042-abc",
		resolution.AttrGCPProject:      "nova-production",
		resolution.AttrGCPRegion:       "europe-west1",
	} {
		if revision[attr] != want {
			t.Errorf("the revision claim carries %s=%q, want %q; C4 requires all three, because a "+
				"revision name is unique within a service and not globally (FR-117)",
				attr, revision[attr], want)
		}
	}
}

// The end of the path: a claim this feeder emitted, paired with an observed service, makes C5 fire.
//
// Nothing else asserts this. Every test above checks one hop — the claim carries the attribute, the
// rule reads the attribute — and a feature made of correct hops that do not join is exactly what a
// published-and-dead rule is.
func TestC5FiresOnTheClaimsThisFeederEmits(t *testing.T) {
	t.Parallel()

	var stored []resolution.Claim
	for identifier, attrs := range emittedClaims(t) {
		namespace, value := splitClaim(t, identifier)
		stored = append(stored, resolution.Claim{
			ClaimID: "gcp:" + identifier, EntityID: "entity-cloud-run-service",
			EntityType: graph.NodeTypeService, SourceID: "gcp:nova",
			Namespace: namespace, Value: value, Attributes: attrs,
		})
	}
	// The other side: telemetry arriving under the name the service declares, in the same
	// environment. It is synthetic here because it belongs to another feeder, and the agreement
	// C5 requires is the environment rather than anything GCP-specific.
	observed := resolution.Claim{
		ClaimID: "otel:checkout", EntityID: "entity-observed-service",
		EntityType: graph.NodeTypeService, SourceID: "otel:nova",
		Namespace: feeder.NSOTelService, Value: "checkout",
		Attributes: map[string]string{resolution.AttrEnvironment: "production"},
	}
	store := claimSlice(append(stored, observed))

	matches, err := resolution.Evaluate(context.Background(), store, observed)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	fired := false
	for _, match := range matches {
		if match.RuleID == "C5" {
			fired = true
		}
	}
	if !fired {
		var ids []string
		for _, match := range matches {
			ids = append(ids, match.RuleID)
		}
		t.Errorf("C5 did not fire on the claims this feeder emits: matched %v. The rule is published "+
			"and registered, so the failure is silent — nothing errors and no merge is proposed", ids)
	}
}

// emittedClaims runs the feeder over one service and one revision and returns every identity claim it
// emitted, keyed `namespace=value`, with the claim's attributes flattened to strings.
func emittedClaims(t *testing.T) map[string]map[string]string {
	t.Helper()

	const revision = `{
      "name": "projects/nova-production/locations/europe-west1/services/checkout/revisions/checkout-00042-abc",
      "uid": "rev-uid-1",
      "createTime": "2026-09-21T14:18:00Z",
      "labels": {"team": "payments", "environment": "production"},
      "containers": [{"image": "europe-docker.pkg.dev/p/r/checkout@sha256:abc"}],
      "conditions": [{"type": "Ready", "state": "CONDITION_SUCCEEDED"}]
    }`
	body, err := json.Marshal(map[string]any{"revisions": []json.RawMessage{json.RawMessage(revision)}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	at := time.Date(2026, 9, 21, 14, 30, 0, 0, time.UTC)
	em := &recordingEmitter{}
	src := &countingSource{payloads: []feeder.Payload{
		{Kind: gcpfeeder.PayloadServices, At: at, Bytes: servicePayload(t, checkoutService)},
		{Kind: gcpfeeder.PayloadRevisions, At: at, Bytes: body},
	}}
	if err := newFeeder(t).Run(context.Background(), src, em); err != nil {
		t.Fatalf("Run: %v", err)
	}

	out := map[string]map[string]string{}
	for _, ev := range em.events {
		claim := ev.GetIdentityClaim()
		if claim == nil {
			continue
		}
		attrs := map[string]string{}
		for key, value := range claim.GetAttributes().GetFields() {
			attrs[key] = value.GetStringValue()
		}
		out[claim.GetClaim().GetNamespace()+"="+claim.GetClaim().GetValue()] = attrs
	}
	if len(out) == 0 {
		t.Fatal("the feeder emitted no identity claims at all")
	}
	return out
}

func keysOf(claims map[string]map[string]string) []string {
	out := make([]string, 0, len(claims))
	for key := range claims {
		out = append(out, key)
	}
	return out
}

func splitClaim(t *testing.T, identifier string) (namespace, value string) {
	t.Helper()
	for i := range identifier {
		if identifier[i] == '=' {
			return identifier[:i], identifier[i+1:]
		}
	}
	t.Fatalf("%q is not a namespace=value identifier", identifier)
	return "", ""
}

// claimSlice is a ClaimStore over a fixed slice, enough for the two rules exercised here.
type claimSlice []resolution.Claim

func (s claimSlice) ClaimsFor(_ context.Context, namespace, value string) ([]resolution.Claim, error) {
	var out []resolution.Claim
	for _, claim := range s {
		if claim.Namespace == namespace && claim.Value == value {
			out = append(out, claim)
		}
	}
	return out, nil
}

func (s claimSlice) ClaimsMatchingAttributes(_ context.Context, namespace string, attrs map[string]string) ([]resolution.Claim, error) {
	var out []resolution.Claim
	for _, claim := range s {
		if claim.Namespace != namespace {
			continue
		}
		match := true
		for key, want := range attrs {
			if claim.Attr(key) != want {
				match = false
				break
			}
		}
		if match {
			out = append(out, claim)
		}
	}
	return out, nil
}

func (s claimSlice) ClaimsInNamespaces(_ context.Context, namespaces []string) ([]resolution.Claim, error) {
	var out []resolution.Claim
	for _, claim := range s {
		for _, namespace := range namespaces {
			if claim.Namespace == namespace {
				out = append(out, claim)
				break
			}
		}
	}
	return out, nil
}

func (claimSlice) SharedOwner(context.Context, string, string) (bool, error) { return false, nil }

// ChangeTargets: no change in these fixtures has a target, so C8 finds no shared one. It is nil
// rather than a panic because Evaluate runs every registered rule on every claim.
func (claimSlice) ChangeTargets(context.Context, string) ([]string, error) { return nil, nil }

// CorrelatedWith completes resolution.ClaimStore. The rules exercised here — C4 and C5 — compare
// identity claims, so a correlation returned here would answer a question neither of them asks. The
// deploy keys this feeder emits are tested against C8 in deployclaims_test.go and in the fixture corpus.
func (claimSlice) CorrelatedWith(context.Context, string, string) ([]resolution.Correlation, error) {
	return nil, nil
}
