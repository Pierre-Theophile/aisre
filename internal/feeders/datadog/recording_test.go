// SPDX-License-Identifier: Apache-2.0

package datadog_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ddfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/datadog"
	"github.com/Pierre-Theophile/aisre/internal/feeders/deployrecord"
	"github.com/Pierre-Theophile/aisre/internal/sanitise"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/emit"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/source"
)

// A live run recorded through the tee (005 T080): the live feeder gets Datadog's bytes, the recording
// gets only sanitised ones, and the recording still joins — its monitors watch the services its
// discovery names, under the same pseudonyms.
func TestARecordingIsSanitisedAndStillJoins(t *testing.T) {
	t.Parallel()
	material := make([]byte, sanitise.KeyBytes)
	for i := range material {
		material[i] = byte(i*13 + 1)
	}
	key, err := sanitise.NewKey(material)
	if err != nil {
		t.Fatal(err)
	}
	san, err := sanitise.New(sanitise.ContractPolicy(), key)
	if err != nil {
		t.Fatal(err)
	}

	// The live run: the transitions fixture's polls, plus a tick measuring checkout's tags.
	payloads := append(transitionsFixture().payloadsOf(t), tagsPayloads(t)...)
	opts := ddfeeder.Options{OrgSlug: "twin", Site: "datadoghq.eu", MonitorTags: []string{"env:production"}}
	live, err := ddfeeder.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	tee, err := deployrecord.NewTee(source.NewSliceSource(payloads), ddfeeder.Kind, san, dir)
	if err != nil {
		t.Fatal(err)
	}
	tee.WithPrepare(ddfeeder.PreparePayload(san))
	liveEvents := emit.NewMemoryEmitter(live.Describe())
	if err := live.Run(context.Background(), tee, liveEvents); err != nil {
		t.Fatal(err)
	}
	if dropped := tee.Dropped(); len(dropped) > 0 || tee.Written() != len(payloads) {
		t.Fatalf("%d of %d payloads written; refused: %v", tee.Written(), len(payloads), dropped)
	}

	// No identifier the twin names survives anywhere in the recording.
	err = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := strings.ToLower(string(raw))
		for _, name := range []string{"checkout", "billing", "search", "payments", "production", "shop",
			"oncall", "runbook", "latency", "heartbeat"} {
			if strings.Contains(text, name) {
				t.Errorf("%s holds %q", path, name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// The recording's events, derived by a shadow feeder over the sanitised payloads, still join.
	shadowOpts, err := ddfeeder.PseudonymousOptions(san, opts)
	if err != nil {
		t.Fatal(err)
	}
	shadow, err := ddfeeder.New(shadowOpts)
	if err != nil {
		t.Fatal(err)
	}
	events, err := tee.Finish(context.Background(), dir, "datadog-monitors", shadow)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := source.NewFileSource(dir)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := ddfeeder.New(shadowOpts) // Finish's shadow already remembers every instant it saw
	if err != nil {
		t.Fatal(err)
	}
	em := emit.NewMemoryEmitter(fresh.Describe())
	if err := fresh.Run(context.Background(), replay, em); err != nil {
		t.Fatal(err)
	}
	sources := map[string]bool{}
	watched := map[string]bool{}
	transitionsSeen := 0
	for _, ev := range em.Events() {
		if n := ev.GetUpsertNode(); n != nil && n.GetRef().GetNamespace() == ddfeeder.NSService {
			sources[n.GetRef().GetValue()] = true
		}
		if a := ev.GetAlertTransition(); a != nil {
			transitionsSeen++
			for _, w := range a.GetWatches() {
				watched[w.GetValue()] = true
			}
		}
	}
	if events == 0 || transitionsSeen != len(transitions(liveEvents)) {
		t.Fatalf("the recording derives %d events and %d transitions; the live run had %d transitions",
			events, transitionsSeen, len(transitions(liveEvents)))
	}
	for w := range watched {
		if !sources[w] {
			t.Errorf("a recorded monitor watches %s, which the recorded discovery never names", w)
		}
	}
	if len(watched) == 0 {
		t.Error("no watched entity survived the recording")
	}
}
