// SPDX-License-Identifier: Apache-2.0

package notion_test

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/feeders/deployrecord"
	"github.com/Pierre-Theophile/aisre/internal/feeders/notion"
	"github.com/Pierre-Theophile/aisre/internal/sanitise"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/source"
)

// TestARecordingCarriesNoIdentifierInTheClear is FR-050 on the live path (T008): the poller's payloads
// go through the recording pre-pass and the contract policy before anything is written, and the events
// derived from the recording are derived from those bytes alone.
func TestARecordingCarriesNoIdentifierInTheClear(t *testing.T) {
	payloads, _ := rawPayloads(t, func(w *twinWorld) {
		for id, p := range w.pages {
			p.URL = "https://www.notion.so/twin/Page-" + strings.ReplaceAll(id, "-", "")
		}
	})
	if !strings.Contains(string(payloads[0].Bytes), "https://www.notion.so/twin/") {
		t.Fatal("the twin's pages carry no URL, so the test proves nothing about dropping it")
	}
	key, err := sanitise.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	san, err := sanitise.New(sanitise.ContractPolicy(), key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	tee, err := deployrecord.NewTee(source.NewSliceSource(payloads), notion.Kind, san, dir)
	if err != nil {
		t.Fatal(err)
	}
	tee = tee.WithPrepare(notion.PreparePayload())
	for {
		if _, err := tee.Next(t.Context()); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
	}
	if tee.Written() != len(payloads) {
		t.Fatalf("%d of %d payloads recorded; refused: %v", tee.Written(), len(payloads), tee.Dropped())
	}

	cfg := fixtureConfig(t)
	shadowCfg, err := cfg.ForRecording(func(kind, value string) (string, error) {
		return san.Identifier(sanitise.Kind(kind), value)
	})
	if err != nil {
		t.Fatal(err)
	}
	shadow, err := notion.New(notion.Options{Config: shadowCfg})
	if err != nil {
		t.Fatal(err)
	}
	events, err := tee.Finish(t.Context(), dir, "notion", shadow)
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if events < 8 {
		t.Errorf("%d events derived from the recording", events)
	}

	leaks := []string{pagePool, pageVendor, pageLong, pageRollback, dbPostmortems, dbRunbooks, dsPostmortems, dsRunbooks,
		"notion.so/twin", "notion:twin", "operator-twin"}
	files := 0
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		files++
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, leak := range leaks {
			if strings.Contains(string(raw), leak) {
				t.Errorf("%s holds %q in the clear", strings.TrimPrefix(path, dir), leak)
			}
		}
		return nil
	})
	if files < 3 {
		t.Errorf("%d files recorded", files)
	}
}
