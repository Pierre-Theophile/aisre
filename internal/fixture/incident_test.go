// SPDX-License-Identifier: Apache-2.0

package fixture_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/fixture"
)

// Every shipped incident fixture parses, and its ground truth is well formed (002 FR-061b,
// FR-063, contracts/incident-format.md).
//
// This is the cheapest gate in the corpus and the one that catches the most: LoadManifest
// refuses a malformed `incident:` block, so a fixture whose culprit is misspelled, whose causal
// path does not start at its culprit, whose decoy has no exonerating evidence, or whose
// provenance names no sanitiser policy fails here — at `go test`, with no database, no model
// and no recorded world. The expensive gates (replay, goldens, world replay, scoring) all
// assume this one has passed.
//
// It walks the directory rather than a checked-in list on purpose. A list would have to be
// edited every time a fixture is added, and the failure mode of forgetting is a fixture that
// is never validated — which is exactly the state a corpus drifts into.

const incidentsDir = "../../fixtures/incidents"

func TestEveryIncidentFixtureParsesAndItsGroundTruthValidates(t *testing.T) {
	dirs := incidentFixtureDirs(t)
	if len(dirs) == 0 {
		t.Fatalf("no incident fixtures under %s; the corpus cannot be empty once Phase 6 has landed",
			incidentsDir)
	}

	for _, dir := range dirs {
		t.Run(filepath.Base(dir), func(t *testing.T) {
			m, err := fixture.LoadManifest(dir)
			if err != nil {
				t.Fatalf("LoadManifest: %v", err)
			}
			if !m.IsIncident() {
				t.Fatalf("%s is under fixtures/incidents/ but carries no `incident:` block; "+
					"a fixture in this directory is an incident fixture or it is in the wrong "+
					"directory (contracts/incident-format.md)", m.ID)
			}
			if m.ID != filepath.Base(dir) {
				t.Errorf("manifest id is %q but the directory is %q; the two must agree",
					m.ID, filepath.Base(dir))
			}

			truth := m.Incident.GroundTruth
			switch truth.Class() {
			case fixture.CulpritNotChangeInduced:
				if truth.CauseCategory() == "" {
					t.Errorf("a not_change_induced ground truth carries no category (FR-071b)")
				}
			case "change_induced":
				if truth.CausalPath[0].Entity != truth.Culprit {
					t.Errorf("causal path starts at %q, culprit is %q",
						truth.CausalPath[0].Entity, truth.Culprit)
				}
			}

			// The knowability time is what makes `unknown` correct before it, so a fixture
			// that put it before the question was even asked would be ungradeable: every
			// answer would be scored as though the evidence had always existed.
			if truth.KnowabilityTime.Before(m.Clock.Start) {
				t.Errorf("knowability_time %s is before the fixture's clock starts at %s",
					truth.KnowabilityTime.Format("15:04:05"), m.Clock.Start.Format("15:04:05"))
			}

			// A decoy is a candidate the run must not name. One with no exonerating evidence
			// is a second culprit, and the only legitimate exception is the pair FR-025 says
			// cannot be separated at all.
			for _, decoy := range truth.Decoys {
				predicates, present := truth.ExoneratingEvidence[decoy.Entity]
				if !present {
					t.Errorf("decoy %s has no exonerating_evidence entry", decoy.Entity)
					continue
				}
				if len(predicates) == 0 && decoy.CausalRole != "not_separable" {
					t.Errorf("decoy %s is exonerated by nothing and its role is %q",
						decoy.Entity, decoy.CausalRole)
				}
			}

			// A fixture that declares a world has to have one, and it has to be the shape the
			// manifest says. The verifier checks this too; checking it here means a corpus
			// error is a unit-test failure rather than a failure that needs PostgreSQL.
			index, present, err := fixture.ReadWorldIndex(dir)
			if err != nil {
				t.Fatalf("ReadWorldIndex: %v", err)
			}
			if !present {
				t.Errorf("no world/ recorded; every incident fixture ships one so that an "+
					"in-algebra telemetry question is served or typed not_recorded, never improvised "+
					"(record it with `bin/aisre fixture record-world %s`)", dir)
				return
			}
			if got, want := int(index.HopRadius), m.Incident.World.HopRadius; got != want {
				t.Errorf("world recorded at hop radius %d, manifest declares %d", got, want)
			}
			if index.AlgebraVersion != m.Incident.World.AlgebraVersion {
				t.Errorf("world recorded under algebra version %q, manifest declares %q",
					index.AlgebraVersion, m.Incident.World.AlgebraVersion)
			}
			if index.MissRate > m.Incident.World.MissRateThreshold {
				t.Errorf("world miss rate %.4f is above the fixture's threshold %.4f",
					index.MissRate, m.Incident.World.MissRateThreshold)
			}

			// Trajectories are recorded from the first live run (T093). Absent is the normal
			// state of the corpus and must not fail anything; this asserts that reading them
			// is graceful rather than that they exist.
			files, err := fixture.TrajectoryFiles(dir)
			if err != nil {
				t.Fatalf("TrajectoryFiles: %v", err)
			}
			t.Logf("%s: %s / %s, %d world term(s), %d trajectory file(s)",
				m.ID, truth.Class(), orNone(truth.CauseCategory()), index.TermCount, len(files))
		})
	}
}

// TestIncidentGroundTruthIsRefusedWhenMalformed pins the refusals themselves, because a
// validator nobody has seen reject anything is a validator that might be returning nil.
func TestIncidentGroundTruthIsRefusedWhenMalformed(t *testing.T) {
	base, err := os.ReadFile(filepath.Join(incidentsDir, "unobserved-latent-bug-01", "manifest.yaml"))
	if err != nil {
		t.Fatalf("read a known-good manifest: %v", err)
	}

	for _, tc := range []struct {
		name    string
		old     string
		new     string
		wantErr string
	}{
		{
			name:    "a not_change_induced culprit with no category",
			old:     "culprit: unobserved\n    category: latent_bug",
			new:     "culprit: not_change_induced",
			wantErr: "must carry a category",
		},
		{
			name:    "an unpublished provenance kind",
			old:     "kind: synthetic",
			new:     "kind: borrowed",
			wantErr: "provenance.kind",
		},
		{
			name:    "a predicate over an algebra family that does not exist",
			old:     "          errors_by_version:",
			new:     "          vibes:",
			wantErr: "not an algebra family",
		},
		{
			name:    "an empty causal path",
			old:     "      - entity: otel.service.name=checkout\n        role: symptom",
			new:     "      []",
			wantErr: "causal_path is empty",
		},
		{
			name:    "a window grid with no reference instant",
			old:     "      - {reference_at: 2026-09-01T14:20:00Z, width_seconds: 900}",
			new:     "      - {width_seconds: 900}",
			wantErr: "names no reference_at",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := strings.Replace(string(base), tc.old, tc.new, 1)
			if text == string(base) {
				t.Fatalf("the manifest no longer contains %q; this table needs updating", tc.old)
			}
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(text), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			_, err := fixture.LoadManifest(dir)
			if err == nil {
				t.Fatalf("LoadManifest accepted a manifest with %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error is %q, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

func incidentFixtureDirs(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(incidentsDir)
	if err != nil {
		t.Fatalf("read %s: %v", incidentsDir, err)
	}
	var dirs []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(incidentsDir, entry.Name())
		if _, err := os.Stat(filepath.Join(dir, fixture.ManifestFile)); err != nil {
			continue
		}
		dirs = append(dirs, dir)
	}
	return dirs
}

func orNone(s string) string {
	if s == "" {
		return "no category"
	}
	return s
}
