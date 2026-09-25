// SPDX-License-Identifier: Apache-2.0

package feeder_test

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// TestNamespacesMatchTheFixtures is the guard that keeps the SDK's namespace constants and the
// recorded fixtures spelling the same identifiers. A constant that drifts from the fixtures
// would silently stop entity resolution from matching anything (research §10).
func TestNamespacesMatchTheFixtures(t *testing.T) {
	t.Parallel()
	const fixtureEvents = "../../fixtures/baseline-topology-01/events.jsonl"

	raw, err := os.ReadFile(fixtureEvents)
	if err != nil {
		t.Skipf("fixture not readable: %v", err)
	}
	used := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(line), &decoded); err != nil {
			t.Fatalf("fixture line: %v", err)
		}
		collectNamespaces(decoded, used)
	}
	if len(used) == 0 {
		t.Fatal("no namespaces found in the baseline fixture; the test is not checking anything")
	}
	for ns := range used {
		if !feeder.IsWellKnownNamespace(ns) {
			t.Errorf("the baseline fixture uses ref namespace %q, which pkg/feeder does not publish", ns)
		}
	}
}

func TestWellKnownNamespacesAreSortedAndUnique(t *testing.T) {
	t.Parallel()
	if !slices.IsSorted(feeder.WellKnownNamespaces) {
		t.Error("WellKnownNamespaces is not sorted")
	}
	seen := map[string]bool{}
	for _, ns := range feeder.WellKnownNamespaces {
		if seen[ns] {
			t.Errorf("%q appears twice", ns)
		}
		seen[ns] = true
	}
}

func TestRefAndRefString(t *testing.T) {
	t.Parallel()
	r := feeder.Ref(feeder.NSOTelService, "checkout")
	if r.GetNamespace() != "otel.service.name" || r.GetValue() != "checkout" {
		t.Fatalf("Ref = %v", r)
	}
	if got, want := feeder.RefString(r), "otel.service.name=checkout"; got != want {
		t.Errorf("RefString = %q, want %q", got, want)
	}
	if feeder.RefString(nil) != "" {
		t.Error("RefString(nil) should be empty")
	}
}

// collectNamespaces walks a decoded event looking for `{"namespace": ..., "value": ...}`
// objects, which is the JSON shape of every Ref.
func collectNamespaces(node any, out map[string]bool) {
	switch value := node.(type) {
	case map[string]any:
		ns, hasNS := value["namespace"].(string)
		_, hasValue := value["value"]
		if hasNS && hasValue && len(value) == 2 {
			out[ns] = true
		}
		for _, child := range value {
			collectNamespaces(child, out)
		}
	case []any:
		for _, child := range value {
			collectNamespaces(child, out)
		}
	}
}
