// SPDX-License-Identifier: Apache-2.0

package fixture_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/fixture"
)

// Feature 004's public corpus is synthetic structural twins, and it stands alone (T108, T109,
// FR-060, SC-011).
//
// FR-060 splits the corpus: the organisation's sanitised recordings live in the private repository
// and are verified by its own CI, and this repository carries twins — the same event shapes and
// sequences with no identifier derived from the organisation. The public claim is then "the
// conformance suite passes on the twins with the private corpus absent", and `fixture verify
// fixtures/*/` in ci.yml is what runs it. It runs whatever is in the directory, though, so it cannot
// notice the two ways the claim silently stops being true:
//
//   - a twin goes missing — deleted, renamed, or never generated — and the suite passes over the
//     fixtures that remain;
//   - a fixture stops being a twin — a real recording is copied in, or one is reached through a
//     symlink into a private checkout — and the public repository now carries (or depends on)
//     what FR-060 says it must not.
//
// This test closes both. It names every twin the feature's stories call for, and it checks each
// deploy-feeder fixture is a twin in the ways that can be checked mechanically. What it cannot check
// is the one thing a twin is FOR: that GitHub and Vercel really return these shapes. That needs the
// recording campaign (T114), and the private corpus's own CI.

// deployTwins is every fixture feature 004's user stories call for, by the task that built it. A
// fixture listed here and missing from fixtures/ fails; so does a deploy-feeder fixture present in
// fixtures/ and missing here, so that adding one is a decision somebody records.
var deployTwins = []string{
	"github-deployment-01",          // T073
	"github-status-states-01",       // T074
	"github-actor-kinds-01",         // T075
	"github-monorepo-01",            // T076, committed since T148
	"github-unattached-01",          // T077
	"github-rerun-01",               // T078
	"github-release-01",             // T078
	"github-doorbell-forged-01",     // T079
	"github-partial-poll-01",        // T079
	"vercel-promotion-01",           // T091
	"vercel-preview-excluded-01",    // T092
	"vercel-config-change-01",       // T102
	"deploy-cross-source-merge-01",  // T019, extended by T093
	"deploy-k8s-commit-merge-01",    // T149
	"deploy-rollback-01",            // T123
	"deploy-telemetry-rejection-01", // T110
}

// The twin estate. `acme` owns the GitHub repositories and `twin` is the Vercel team; neither names
// anybody. A payload naming any other owner or team is a payload that did not come from a twin.
var (
	githubOwner = regexp.MustCompile(`(?:https://github\.com|https://api\.github\.com/repos)/([A-Za-z0-9_.-]+)/`)
	vercelTeam  = regexp.MustCompile(`https://vercel\.com/([A-Za-z0-9_.-]+)/`)
)

const twinMarker = "Synthetic structural twin"

func TestDeployFixturesAreTwinsAndStandAlone(t *testing.T) {
	root := filepath.Join("..", "..", "fixtures")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read %s: %v", root, err)
	}

	var deployFeederFixtures []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		manifest, err := fixture.LoadManifest(filepath.Join(root, entry.Name()))
		if err != nil {
			continue // a group directory such as incidents/, which has no manifest of its own
		}
		if slices.ContainsFunc(manifest.Sources, func(s fixture.Source) bool {
			return s.Kind == "github" || s.Kind == "vercel"
		}) {
			deployFeederFixtures = append(deployFeederFixtures, entry.Name())
		}
	}

	for _, id := range deployTwins {
		if !slices.Contains(deployFeederFixtures, id) {
			t.Errorf("%s is one of feature 004's twins and is not in fixtures/ (or no longer has a "+
				"GitHub or Vercel source); the public suite would pass without it", id)
		}
	}
	for _, id := range deployFeederFixtures {
		if !slices.Contains(deployTwins, id) {
			t.Errorf("%s has a deploy-feeder source and is not listed in deployTwins; add it, with the "+
				"task that built it, so its absence would later be noticed", id)
		}
	}

	for _, id := range deployFeederFixtures {
		t.Run(id, func(t *testing.T) {
			dir := filepath.Join(root, id)
			manifest, err := fixture.LoadManifest(dir)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if !strings.Contains(manifest.Description, twinMarker) {
				t.Errorf("the description does not say %q; a reader of a verification report "+
					"cannot tell a twin from a recording", twinMarker)
			}
			for _, src := range manifest.Sources {
				if (src.Kind == "github" || src.Kind == "vercel") && !strings.HasSuffix(src.SourceID, ":twin") {
					t.Errorf("source %s is not a twin source; a real installation's source id "+
						"belongs to the private corpus (FR-060)", src.SourceID)
				}
			}
			assertStandsAlone(t, dir)
		})
	}
}

// assertStandsAlone checks a fixture is wholly inside this repository and carries only the twin
// estate: no symlink out of it, no sanitisation manifest (which only a real, sanitised recording
// has), and no GitHub owner or Vercel team in any payload but the twins'.
func assertStandsAlone(t *testing.T, dir string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			t.Errorf("%s is a symlink; a public fixture that reaches outside itself does not pass "+
				"with the private corpus absent", path)
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if d.Name() == "sanitisation.yaml" {
			t.Errorf("%s exists: a sanitisation manifest marks a real recording, which belongs in "+
				"the private repository (FR-060)", path)
		}
		if !strings.Contains(filepath.ToSlash(path), "/payloads/") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range githubOwner.FindAllSubmatch(body, -1) {
			if string(m[1]) != "acme" {
				t.Errorf("%s names the GitHub owner %q; the twin estate's is acme", path, m[1])
			}
		}
		for _, m := range vercelTeam.FindAllSubmatch(body, -1) {
			if string(m[1]) != "twin" {
				t.Errorf("%s names the Vercel team %q; the twin estate's is twin", path, m[1])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
}
