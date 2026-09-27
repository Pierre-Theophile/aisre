// SPDX-License-Identifier: Apache-2.0

package datadog_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ddfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/datadog"
	"github.com/Pierre-Theophile/aisre/internal/feeders/deployrecord"
	"github.com/Pierre-Theophile/aisre/internal/sanitise"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/emit"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/source"
)

// SC-017 for the Datadog recording path (005 T081): canaries seeded where a real monitor carries
// people, infrastructure names and free text are absent, raw and hashed, from everything the tee writes.
func TestNoCanarySurvivesTheRecording(t *testing.T) {
	t.Parallel()
	material := make([]byte, sanitise.KeyBytes)
	for i := range material {
		material[i] = byte(i*29 + 3)
	}
	key, err := sanitise.NewKey(material)
	if err != nil {
		t.Fatal(err)
	}
	canaries := sanitise.NewCanarySet(key)
	seeded, err := canaries.Seed("datadog.monitors", sanitise.CanaryPerson, sanitise.CanaryInfrastructure, sanitise.CanaryFreeText)
	if err != nil {
		t.Fatal(err)
	}
	person, infra, text := seeded[0].Carrier, seeded[1].Carrier, seeded[2].Carrier
	san, err := sanitise.New(sanitise.ContractPolicy(), key)
	if err != nil {
		t.Fatal(err)
	}
	san = san.WithCanaries(canaries)

	// A monitor as Datadog returns it, with the fields the feeder never reads carrying the canaries too.
	page := []map[string]any{{
		"id": 7, "name": "checkout errors", "type": "log alert", "priority": 2,
		"query":   `logs("service:` + infra + ` env:production \"` + text + `\"").index("*").rollup("count").last("5m") > 10`,
		"message": "Paging " + person + ". " + text,
		"tags":    []string{"env:production", "service:" + infra, "note:" + text},
		"creator": map[string]any{"email": person, "handle": person, "name": text},
		"options": map[string]any{"escalation_message": text},
		"org_id":  4242, "modified": "2026-09-01T09:00:00Z", "overall_state": "Alert",
		"state": map[string]any{"groups": map[string]any{
			"env:production,service:" + infra: map[string]any{"status": "Alert", "last_triggered_ts": 1790001120},
		}},
	}}
	raw, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	tee, err := deployrecord.NewTee(source.NewSliceSource([]feeder.Payload{
		{Kind: ddfeeder.PayloadMonitors, At: hm(14, 33), Bytes: raw}, poll(hm(14, 33), "complete"),
	}), ddfeeder.Kind, san, dir)
	if err != nil {
		t.Fatal(err)
	}
	tee.WithPrepare(ddfeeder.PreparePayload(san))
	f, err := ddfeeder.New(ddfeeder.Options{OrgSlug: "twin"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Run(context.Background(), tee, emit.NewMemoryEmitter(f.Describe())); err != nil {
		t.Fatalf("the run ended: %v", err)
	}
	if tee.Written() != 2 {
		t.Fatalf("%d payloads written; refused: %v", tee.Written(), tee.Dropped())
	}
	assertCleanDir(t, dir, canaries)
}

// A canary in a field the table itself pseudonymises means every rule before it was wrong about this
// payload: the run ends rather than write it (FR-139).
func TestACanaryTheTableWouldHashEndsTheRun(t *testing.T) {
	t.Parallel()
	material := make([]byte, sanitise.KeyBytes)
	key, err := sanitise.NewKey(material)
	if err != nil {
		t.Fatal(err)
	}
	canaries := sanitise.NewCanarySet(key)
	seeded, err := canaries.Seed("datadog.monitors", sanitise.CanaryInfrastructure)
	if err != nil {
		t.Fatal(err)
	}
	san, err := sanitise.New(sanitise.ContractPolicy(), key)
	if err != nil {
		t.Fatal(err)
	}
	san = san.WithCanaries(canaries)
	raw, _ := json.Marshal([]map[string]any{{"id": 7, "name": "errors on " + seeded[0].Carrier, "type": "log alert",
		"query": "", "modified": "2026-09-01T09:00:00Z"}})
	dir := t.TempDir()
	tee, err := deployrecord.NewTee(source.NewSliceSource([]feeder.Payload{{Kind: ddfeeder.PayloadMonitors, At: hm(14, 33), Bytes: raw}}),
		ddfeeder.Kind, san, dir)
	if err != nil {
		t.Fatal(err)
	}
	tee.WithPrepare(ddfeeder.PreparePayload(san))
	f, err := ddfeeder.New(ddfeeder.Options{OrgSlug: "twin"})
	if err != nil {
		t.Fatal(err)
	}
	var survived *sanitise.CanarySurvivedError
	if err := f.Run(context.Background(), tee, emit.NewMemoryEmitter(f.Describe())); !errors.As(err, &survived) {
		t.Fatalf("the run did not end on the canary: %v", err)
	}
	assertCleanDir(t, dir, canaries)
}

// Every committed Datadog fixture is clean: no people identifier in any form, no canary token.
func TestEveryDatadogFixtureIsClean(t *testing.T) {
	t.Parallel()
	dirs, err := filepath.Glob(filepath.Join(repoRoot(t), "fixtures", "datadog-*"))
	if err != nil || len(dirs) == 0 {
		t.Fatalf("no Datadog fixture: %v", err)
	}
	for _, dir := range dirs {
		assertCleanDir(t, dir, nil)
	}
}

func assertCleanDir(t *testing.T, dir string, canaries *sanitise.CanarySet) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if class := sanitise.PeopleInArtifact(data); class != "" {
			t.Errorf("%s carries %s", path, class)
		}
		if strings.Contains(string(data), sanitise.CanaryPrefix) {
			t.Errorf("%s carries a canary token", path)
		}
		if canaries != nil {
			survivors, err := canaries.Survivors(data)
			if err != nil {
				return err
			}
			for _, s := range survivors {
				t.Errorf("%s: %s survived (%s)", path, s.Canary.Kind, s.Form)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
