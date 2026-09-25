// SPDX-License-Identifier: Apache-2.0

package migrate_test

import (
	"errors"
	"strings"
	"testing"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/log/migrate"
)

// Unit tests of the transformation registry (T088, FR-025, constitution IX).
//
// The chain resolver is the part that will be load-bearing on the day a breaking change ships,
// so it is tested now, while the only registered transformation is the identity and a mistake
// costs nothing.

func envelope(version string) *graphv1.EventEnvelope {
	return &graphv1.EventEnvelope{
		EventId:        "otel:demo:1",
		IdempotencyKey: "otel:demo:1",
		SourceId:       "otel:demo",
		SchemaVersion:  version,
		Body: &graphv1.EventEnvelope_UpsertNode{UpsertNode: &graphv1.UpsertNode{
			DisplayName: "checkout",
		}},
	}
}

func TestIdentityChainLeavesTheEnvelopeAlone(t *testing.T) {
	chain, err := migrate.Default().Chain(migrate.CurrentVersion, migrate.CurrentVersion)
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	if !chain.IsIdentity() {
		t.Fatalf("chain %q is not the identity", chain.Steps())
	}
	in := envelope(migrate.CurrentVersion)
	out, err := chain.Apply(in)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if out != in {
		t.Error("the identity chain copied the envelope; it should return it unchanged")
	}
}

func TestChainRefusesAVersionItCannotReach(t *testing.T) {
	_, err := migrate.Default().Chain("0.9.0", migrate.CurrentVersion)
	if err == nil {
		t.Fatal("Chain(0.9.0 → 1.0.0) succeeded; this build knows no such transformation")
	}
	if !strings.Contains(err.Error(), "0.9.0") || !strings.Contains(err.Error(), migrate.CurrentVersion) {
		t.Errorf("error should name both versions, got %v", err)
	}
}

func TestRegisteredChainComposesInOrder(t *testing.T) {
	r := migrate.NewRegistry()
	mustRegister(t, r, migrate.Func("0.9.0", "0.10.0", tag("a")))
	mustRegister(t, r, migrate.Func("0.10.0", "1.0.0", tag("b")))

	chain, err := r.Chain("0.9.0", "1.0.0")
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	if got, want := chain.Steps(), "0.9.0 → 0.10.0 → 1.0.0"; got != want {
		t.Errorf("Steps() = %q, want %q", got, want)
	}

	in := envelope("0.9.0")
	out, err := chain.Apply(in)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got, want := out.GetUpsertNode().GetDisplayName(), "checkout|a|b"; got != want {
		t.Errorf("display name = %q, want %q: the steps must run in order", got, want)
	}
	if out.GetSchemaVersion() != "1.0.0" {
		t.Errorf("schema_version = %q, want 1.0.0", out.GetSchemaVersion())
	}
	if in.GetUpsertNode().GetDisplayName() != "checkout" {
		t.Error("Apply mutated the envelope it was given; a replay still holds that pointer")
	}
}

func TestChainToAnyPrefersTheNewestAcceptedVersion(t *testing.T) {
	r := migrate.NewRegistry()
	mustRegister(t, r, migrate.Func("0.9.0", "1.0.0", tag("x")))
	mustRegister(t, r, migrate.Func("1.0.0", "1.10.0", tag("y")))

	chain, err := r.ChainToAny("0.9.0", []string{"1.0.0", "1.9.0", "1.10.0"})
	if err != nil {
		t.Fatalf("ChainToAny: %v", err)
	}
	if got := chain.To(); got != "1.10.0" {
		t.Errorf("ChainToAny reached %q, want 1.10.0 (1.10.0 is newer than 1.9.0)", got)
	}
}

func TestChainToAnyIsEmptyForAnAcceptedVersion(t *testing.T) {
	chain, err := migrate.Default().ChainToAny(migrate.CurrentVersion, []string{migrate.CurrentVersion})
	if err != nil {
		t.Fatalf("ChainToAny: %v", err)
	}
	if len(chain) != 0 {
		t.Errorf("an accepted version produced a chain of %d steps; it must not be transformed", len(chain))
	}
}

func TestRegisterRefusesASecondSuccessor(t *testing.T) {
	r := migrate.NewRegistry()
	mustRegister(t, r, migrate.Func("0.9.0", "1.0.0", tag("x")))
	if err := r.Register(migrate.Func("0.9.0", "1.1.0", tag("y"))); err == nil {
		t.Fatal("registering a second successor for 0.9.0 succeeded; the chain would be ambiguous")
	}
}

func TestChainRefusesATransformerThatRewritesIdentity(t *testing.T) {
	r := migrate.NewRegistry()
	mustRegister(t, r, migrate.Func("0.9.0", "1.0.0", func(env *graphv1.EventEnvelope) (*graphv1.EventEnvelope, error) {
		env.EventId = "renumbered"
		return env, nil
	}))
	chain, err := r.Chain("0.9.0", "1.0.0")
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	if _, err := chain.Apply(envelope("0.9.0")); err == nil {
		t.Fatal("a transformer renumbered an event and Apply allowed it; idempotency keys are not a migration's to change")
	}
}

func TestChainDetectsACycle(t *testing.T) {
	r := migrate.NewRegistry()
	mustRegister(t, r, migrate.Func("a", "b", tag("1")))
	mustRegister(t, r, migrate.Func("b", "a", tag("2")))
	if _, err := r.Chain("a", "zz"); err == nil {
		t.Fatal("a cyclic registry resolved a chain")
	}
}

func TestApplyPropagatesATransformerError(t *testing.T) {
	boom := errors.New("boom")
	r := migrate.NewRegistry()
	mustRegister(t, r, migrate.Func("0.9.0", "1.0.0", func(*graphv1.EventEnvelope) (*graphv1.EventEnvelope, error) {
		return nil, boom
	}))
	chain, err := r.Chain("0.9.0", "1.0.0")
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	if _, err := chain.Apply(envelope("0.9.0")); !errors.Is(err, boom) {
		t.Errorf("Apply error = %v, want it to wrap %v", err, boom)
	}
}

// tag is a transformer that appends a marker to the display name, so a test can see the order
// the steps ran in.
func tag(mark string) func(*graphv1.EventEnvelope) (*graphv1.EventEnvelope, error) {
	return func(env *graphv1.EventEnvelope) (*graphv1.EventEnvelope, error) {
		if node := env.GetUpsertNode(); node != nil {
			node.DisplayName += "|" + mark
		}
		return env, nil
	}
}

func mustRegister(t *testing.T, r *migrate.Registry, transformer migrate.Transformer) {
	t.Helper()
	if err := r.Register(transformer); err != nil {
		t.Fatalf("Register %s → %s: %v", transformer.From(), transformer.To(), err)
	}
}
