// SPDX-License-Identifier: Apache-2.0

package log_test

import (
	"strings"
	"testing"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	eventlog "github.com/Pierre-Theophile/aisre/internal/log"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The event log refuses a correlation asserted as a name (004 T148).
//
// This is the guard that makes the distinction between the two kinds enforceable rather than advisory.
// `graph.identity_claims` is unique per (namespace, value, source_id), so a value several entities
// share, stored as an identity, lands on whichever entity the projector reached first — and the graph's
// state then depends on the order events arrived in. Four of the seven US1 fixtures failed their
// shuffle step on exactly that before this existed.
//
// It is refused at the log rather than corrected in the projector because a refused event is not in
// `log.events` at all: a feeder author sees the reason code on the first run, and no corrupt claim is
// ever appended to history that a replay would have to reproduce.

func correlationEnvelope(body *graphv1.CorrelateEntity) *graphv1.EventEnvelope {
	return &graphv1.EventEnvelope{
		EventId:        "github:acme:e1",
		IdempotencyKey: "github:acme:e1",
		SourceId:       "github:acme",
		SchemaVersion:  "1.0.0",
		Body:           &graphv1.EventEnvelope_CorrelateEntity{CorrelateEntity: body},
	}
}

func claimEnvelope(namespace, value string) *graphv1.EventEnvelope {
	return &graphv1.EventEnvelope{
		EventId:        "github:acme:e1",
		IdempotencyKey: "github:acme:e1",
		SourceId:       "github:acme",
		SchemaVersion:  "1.0.0",
		Body: &graphv1.EventEnvelope_IdentityClaim{IdentityClaim: &graphv1.IdentityClaim{
			Subject: &graphv1.Ref{Namespace: "github.change", Value: "repositories/1/deployments/2"},
			Claim:   &graphv1.Ref{Namespace: namespace, Value: value},
		}},
	}
}

// Every published correlation namespace is refused as an identity claim, and the refusal names the
// namespace so a feeder author can see which of the two kinds they reached for.
func TestEveryCorrelationNamespaceIsRefusedAsAnIdentityClaim(t *testing.T) {
	t.Parallel()

	if len(feeder.CorrelationNamespaces) == 0 {
		t.Fatal("the SDK publishes no correlation namespaces, so this test asserts nothing")
	}
	for _, namespace := range feeder.CorrelationNamespaces {
		got := eventlog.Validate(claimEnvelope(namespace, "some-shared-value"), nil)
		if got == nil {
			t.Errorf("a claim in %q was accepted; a value several entities share, stored as a name, "+
				"lands on whichever entity was processed first", namespace)
			continue
		}
		if got.ReasonCode != eventlog.ReasonCorrelationAsIdentity {
			t.Errorf("a claim in %q was refused as %q, want %q", namespace,
				got.ReasonCode, eventlog.ReasonCorrelationAsIdentity)
		}
		if !strings.Contains(got.ReasonDetail, namespace) {
			t.Errorf("the refusal of %q does not name the namespace: %q", namespace, got.ReasonDetail)
		}
	}
}

// And the guard is a guard and not an off switch: a namespace that does name one entity is still
// accepted as a claim. Without this the assertion above would pass on a validator that refused
// everything.
func TestAnIdentifyingNamespaceIsStillAcceptedAsAClaim(t *testing.T) {
	t.Parallel()

	for _, namespace := range []string{
		"otel.service.name",
		feeder.NSGitHubChange,
		"k8s.deployment",
	} {
		if got := eventlog.Validate(claimEnvelope(namespace, "checkout"), nil); got != nil {
			t.Errorf("a claim in %q was refused as %s: %s; a name is what identity_claims is for",
				namespace, got.ReasonCode, got.ReasonDetail)
		}
	}
}

// The same value IS accepted as a correlation key, which is the other half of the contract: the
// refusal above redirects a feeder rather than closing a door.
func TestADeployValueIsAcceptedAsACorrelation(t *testing.T) {
	t.Parallel()

	env := correlationEnvelope(&graphv1.CorrelateEntity{
		Subject: &graphv1.Ref{Namespace: feeder.NSGitHubChange, Value: "repositories/1/deployments/2"},
		Key:     &graphv1.Ref{Namespace: feeder.NSDeployCommitSHA, Value: strings.Repeat("a", 40)},
	})
	if got := eventlog.Validate(env, nil); got != nil {
		t.Fatalf("the correlation was refused as %s: %s", got.ReasonCode, got.ReasonDetail)
	}
}

// A correlation in a namespace the registry has never heard of is accepted, deliberately.
//
// The asymmetry is the design. A new connector correlates on things this schema does not know — a build
// id, a vendor's incident number — and a registry that had to be edited before a feeder could ship
// would make the list the bottleneck rather than the vocabulary. The direction with a wrong answer is
// the other one: a correlation stored as a name corrupts resolution, while a name stored as a
// correlation merely fails to merge.
func TestACorrelationInAnUnregisteredNamespaceIsAccepted(t *testing.T) {
	t.Parallel()

	env := correlationEnvelope(&graphv1.CorrelateEntity{
		Subject: &graphv1.Ref{Namespace: feeder.NSGitHubChange, Value: "repositories/1/deployments/2"},
		Key:     &graphv1.Ref{Namespace: "buildkite.build", Value: "42"},
	})
	if got := eventlog.Validate(env, nil); got != nil {
		t.Fatalf("a correlation in an unregistered namespace was refused as %s: %s; the registry is a "+
			"published vocabulary, not an allowlist", got.ReasonCode, got.ReasonDetail)
	}
	// And the namespace really is unregistered, so the case above is the one it claims to be.
	if graph.IsCorrelationNamespace("buildkite.build") {
		t.Error("buildkite.build is registered, so this test no longer exercises the unregistered case")
	}
}

// Both refs are required. A correlation with no subject is a value attached to nothing, and one with no
// key is a subject correlated with nothing — neither is a fact.
func TestACorrelationNeedsBothRefs(t *testing.T) {
	t.Parallel()

	key := &graphv1.Ref{Namespace: feeder.NSDeployCommitSHA, Value: strings.Repeat("a", 40)}
	subject := &graphv1.Ref{Namespace: feeder.NSGitHubChange, Value: "repositories/1/deployments/2"}

	for field, body := range map[string]*graphv1.CorrelateEntity{
		"correlate_entity.subject": {Key: key},
		"correlate_entity.key":     {Subject: subject},
	} {
		got := eventlog.Validate(correlationEnvelope(body), nil)
		if got == nil {
			t.Errorf("a correlation missing %s was accepted", field)
			continue
		}
		if got.ReasonCode != eventlog.ReasonMissingRef {
			t.Errorf("a correlation missing %s was refused as %q, want %q",
				field, got.ReasonCode, eventlog.ReasonMissingRef)
		}
		if !strings.Contains(got.ReasonDetail, field) {
			t.Errorf("the refusal does not name %s: %q", field, got.ReasonDetail)
		}
	}
}
