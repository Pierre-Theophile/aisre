// SPDX-License-Identifier: Apache-2.0

package worker_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/pkg/worker"
	"github.com/Pierre-Theophile/aisre/pkg/worker/testkit"
)

// stub is the smallest declaration that passes the registration gate. Every rejection test
// below starts from it and breaks exactly one thing, so a test that fails names the rule it
// broke rather than "the description is invalid".
type stub struct{ d worker.Description }

func (s stub) Describe() worker.Description { return s.d }

func (s stub) Call(context.Context, worker.Request) (worker.Response, error) {
	return worker.Response{}, errors.New("stub: Call lands with Phase 4")
}

func validDescription() worker.Description {
	return worker.Description{
		Name:          "metrics",
		SourceOfTruth: "telemetry-backend:recorded",
		Capabilities: []worker.Capability{
			{Name: "compare", ReadOnly: true, CostClass: worker.CostClassStandard, MaxWindow: time.Hour},
			{Name: "onset", ReadOnly: true, CostClass: worker.CostClassExpensive},
		},
		Redaction: worker.RedactionPolicy{PolicyVersion: "1.0.0"},
		Modes:     []worker.Mode{worker.ModeLive, worker.ModeRecorded},
		Version:   "0.1.0",
	}
}

func TestValidDescriptionRegisters(t *testing.T) {
	t.Parallel()
	testkit.Declaration(t, stub{d: validDescription()})
}

func TestDescriptionValidateRejections(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*worker.Description)
		reason string
	}{
		{
			name:   "a capability that changes state",
			mutate: func(d *worker.Description) { d.Capabilities[0].ReadOnly = false },
			reason: worker.ReasonWriteCapability,
		},
		{
			name:   "a capability with no cost class",
			mutate: func(d *worker.Description) { d.Capabilities[0].CostClass = worker.CostClassUnspecified },
			reason: worker.ReasonUndeclaredCapability,
		},
		{
			name:   "two sources of truth is no source of truth",
			mutate: func(d *worker.Description) { d.SourceOfTruth = "" },
			reason: worker.ReasonUndeclaredCapability,
		},
		{
			name:   "no capabilities at all",
			mutate: func(d *worker.Description) { d.Capabilities = nil },
			reason: worker.ReasonUndeclaredCapability,
		},
		{
			name:   "the same term declared twice",
			mutate: func(d *worker.Description) { d.Capabilities[1].Name = d.Capabilities[0].Name },
			reason: worker.ReasonUndeclaredCapability,
		},
		{
			name:   "a model that ran without being declared",
			mutate: func(d *worker.Description) { d.ModelID = "claude-haiku-4-5" },
			reason: worker.ReasonUndeclaredCapability,
		},
		{
			name:   "a declared model with no id",
			mutate: func(d *worker.Description) { d.ContainsModel = true },
			reason: worker.ReasonUndeclaredCapability,
		},
		{
			name:   "redaction with no policy version",
			mutate: func(d *worker.Description) { d.Redaction.PolicyVersion = "" },
			reason: worker.ReasonUndeclaredRedaction,
		},
		{
			name:   "a worker that cannot be replayed",
			mutate: func(d *worker.Description) { d.Modes = []worker.Mode{worker.ModeLive} },
			reason: worker.ReasonUndeclaredCapability,
		},
		{
			name:   "an unpublished mode",
			mutate: func(d *worker.Description) { d.Modes = []worker.Mode{worker.ModeLive, worker.ModeRecorded, "dry-run"} },
			reason: worker.ReasonUndeclaredCapability,
		},
		{
			name:   "no version",
			mutate: func(d *worker.Description) { d.Version = "" },
			reason: worker.ReasonUndeclaredCapability,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := validDescription()
			tc.mutate(&d)
			err := d.Validate()
			if err == nil {
				t.Fatalf("Validate accepted %s", tc.name)
			}
			if got := worker.ReasonOf(err); got != tc.reason {
				t.Fatalf("reason = %q, want %q (error: %v)", got, tc.reason, err)
			}
		})
	}
}

func TestRegistryRefusesDuplicateNames(t *testing.T) {
	t.Parallel()
	registry := worker.NewRegistry()
	if err := registry.Register(stub{d: validDescription()}); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	if err := registry.Register(stub{d: validDescription()}); err == nil {
		t.Fatal("registered two workers under one name; provenance would be ambiguous")
	}
}

func TestRegistryListsInStableOrder(t *testing.T) {
	t.Parallel()
	registry := worker.NewRegistry()
	for _, name := range []string{"traces", "graph", "metrics"} {
		d := validDescription()
		d.Name = name
		if err := registry.Register(stub{d: d}); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}
	want := []string{"graph", "metrics", "traces"}
	got := registry.Names()
	if len(got) != len(want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Names() = %v, want %v", got, want)
		}
	}
	if descriptions := registry.Descriptions(); len(descriptions) != 3 || descriptions[0].Name != "graph" {
		t.Fatalf("Descriptions() = %v, want three, graph first", descriptions)
	}
}

func TestResolveRefusesAnUnknownWorker(t *testing.T) {
	t.Parallel()
	registry := worker.NewRegistry()
	if _, _, err := registry.Resolve("nobody", "compare"); err == nil {
		t.Fatal("resolved a capability on a worker that is not registered")
	} else if worker.ReasonOf(err) != worker.ReasonUndeclaredCapability {
		t.Fatalf("reason = %q, want %q", worker.ReasonOf(err), worker.ReasonUndeclaredCapability)
	}
}

func TestModeValidity(t *testing.T) {
	t.Parallel()
	for _, m := range []worker.Mode{worker.ModeLive, worker.ModeRecorded} {
		if !m.Valid() {
			t.Errorf("%s is not valid", m)
		}
		if m.String() != string(m) {
			t.Errorf("String() = %q, want %q", m.String(), string(m))
		}
	}
	if worker.Mode("cached").Valid() {
		t.Error(`"cached" is not a published mode`)
	}
}
