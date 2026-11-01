// SPDX-License-Identifier: Apache-2.0

package expectations_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/investigation/expectations"
)

const published = "../../../config/expectations/v1.yaml"

// TestThePublishedTableLoadsInObserveMode: version 1 is checked in, valid, and observes. Moving it
// to enforce is T132's, after the dual gate (ADR-0019 D4).
func TestThePublishedTableLoadsInObserveMode(t *testing.T) {
	t.Parallel()
	table, err := expectations.Load(filepath.Clean(published))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if table.Version != 1 || table.Mode != expectations.ModeObserve {
		t.Errorf("table = %s, want v1 (observe)", table.Label())
	}
	if decisive, ok := table.Decisive("rollout"); !ok || len(decisive) == 0 {
		t.Errorf("rollout decisive = %v, %v", decisive, ok)
	}
}

// TestTheCompiledTableIsTheCheckedInOne: operators read config/expectations/v1.yaml; the engine
// runs the compiled-in copy. They are the same bytes.
func TestTheCompiledTableIsTheCheckedInOne(t *testing.T) {
	t.Parallel()
	onDisk, err := os.ReadFile(filepath.Clean(published))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	compiled, err := os.ReadFile("v1.yaml")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(onDisk, compiled) {
		t.Fatal("config/expectations/v1.yaml and the compiled-in table differ")
	}
	if expectations.Published().Label() != "v1 (observe)" {
		t.Errorf("published = %s", expectations.Published().Label())
	}
}

// TestEveryChangeKindIsACategory: a change kind 001 adds without a row would leave its hypotheses
// unexamined; the published category list follows the proto.
func TestEveryChangeKindIsACategory(t *testing.T) {
	t.Parallel()
	known := map[string]bool{}
	for _, c := range expectations.Categories {
		known[c] = true
	}
	for value, name := range graphv1.ChangeKind_name {
		if value == 0 {
			continue
		}
		category := strings.ToLower(strings.TrimPrefix(name, "CHANGE_KIND_"))
		if !known[category] {
			t.Errorf("change kind %s has no cause category %q", name, category)
		}
	}
}

// TestABadTableIsRefused: the schema test.
func TestABadTableIsRefused(t *testing.T) {
	t.Parallel()
	valid := "version: 1\nmode: observe\ncategories:\n"
	for _, c := range expectations.Categories {
		valid += "  " + c + ":\n    decisive: [metrics]\n    why: because\n"
	}
	if _, err := expectations.Parse([]byte(valid)); err != nil {
		t.Fatalf("a valid table was refused: %v", err)
	}
	for name, bad := range map[string]string{
		"mode":            strings.Replace(valid, "mode: observe", "mode: maybe", 1),
		"version":         strings.Replace(valid, "version: 1", "version: 0", 1),
		"unknown family":  strings.Replace(valid, "decisive: [metrics]", "decisive: [vibes]", 1),
		"repeated family": strings.Replace(valid, "decisive: [metrics]", "decisive: [metrics, metrics]", 1),
		"no reason":       strings.Replace(valid, "why: because", "why: ''", 1),
		"missing row":     strings.Replace(valid, "  rollout:\n    decisive: [metrics]\n    why: because\n", "", 1),
		"unknown row":     valid + "  gremlins:\n    decisive: []\n    why: x\n",
		"unknown key":     strings.Replace(valid, "mode: observe", "mode: observe\nstrict: true", 1),
	} {
		if _, err := expectations.Parse([]byte(bad)); err == nil {
			t.Errorf("%s: a bad table was accepted", name)
		}
	}
}
