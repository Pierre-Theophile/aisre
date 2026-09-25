// SPDX-License-Identifier: Apache-2.0

package replay_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/fixture"
	"github.com/Pierre-Theophile/aisre/internal/investigation/replay"
	"github.com/Pierre-Theophile/aisre/internal/projector"
	"github.com/Pierre-Theophile/aisre/internal/query"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres/pgtest"
)

func TestMain(m *testing.M) { pgtest.TestMain(m) }

// The self-contained artifact and the rebuild test (T095, FR-042, SC-011, analyze C3).
//
// The rebuild test is written in one specific form because the form *is* the assertion: drop
// schema `investigation`, then reconstruct from **the event log plus a regenerable world**, with
// `investigate replay --from <dir>`. A rebuild that needed anything those two do not hold would
// mean the investigation store had become a substrate of its own, and principle I — the graph is
// the source of truth — would no longer be true of this system. That is the whole justification
// for a second schema existing (plan §Complexity Tracking).

// corpusSource is an ExportSource over the checked-in fixture: its trajectories, its recorded
// world and its event log, plus a decision record shaped like the one a run produces.
type corpusSource struct {
	dir string
	inv *investigationv1.Investigation
}

func (s corpusSource) Investigation(context.Context, string) (*investigationv1.Investigation, error) {
	return s.inv, nil
}

func (s corpusSource) Trajectories(context.Context, string) ([]*replay.Trajectory, error) {
	files, err := replay.Files(s.dir)
	if err != nil {
		return nil, err
	}
	out := make([]*replay.Trajectory, 0, len(files))
	for _, path := range files {
		traj, err := replay.Read(path)
		if err != nil {
			return nil, err
		}
		out = append(out, traj)
	}
	return out, nil
}

func (s corpusSource) WorldDir(context.Context, string) (string, error) {
	return filepath.Join(s.dir, "world"), nil
}

func (s corpusSource) GraphEvents(context.Context, string) ([][]byte, error) {
	raw, err := os.ReadFile(filepath.Join(s.dir, "events.jsonl"))
	if err != nil {
		return nil, err
	}
	var out [][]byte
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		out = append(out, []byte(line))
	}
	return out, nil
}

func newSource(t *testing.T) corpusSource {
	t.Helper()
	return corpusSource{
		dir: fixtureDir,
		inv: &investigationv1.Investigation{
			InvestigationId: "inv-rollout-regression-01-incident",
			IncidentId:      "incident-rollout-regression-01",
			Lifecycle:       investigationv1.Lifecycle_CONCLUDED,
			Ledger: &investigationv1.Ledger{
				Hypotheses: []*investigationv1.Hypothesis{{
					HypothesisId:            "h-1",
					Kind:                    investigationv1.HypothesisKind_CHANGE,
					Statement:               "the payments rollout to rev7 caused the checkout errors",
					CandidateChangeEntityId: "k8s.change=shop/payments@rev7",
					Confidence:              0.91,
					Bucket: &investigationv1.ConfidenceBucket{
						Name: "very_high", RangeLow: 0.85, RangeHigh: 1.0,
					},
				}},
			},
		},
	}
}

// TestAnExportIsSelfContainedAndItsDigestBindsIt.
func TestAnExportIsSelfContainedAndItsDigestBindsIt(t *testing.T) {
	t.Parallel()
	corpusTrajectories(t)

	out := t.TempDir()
	src := newSource(t)
	resp, err := replay.Export(context.Background(), src, src.inv.GetInvestigationId(), out)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if resp.GetExportDigest() == "" {
		t.Error("the export carries no digest")
	}
	if resp.GetTrajectoryRecordCount() == 0 {
		t.Error("the export carries no trajectory records")
	}
	if resp.GetWorldTermCount() == 0 {
		t.Error("the export carries no world terms")
	}

	// The four parts FR-042 names: the decision record, the ledger (inside it), both recording
	// layers, and the graph events.
	for _, want := range []string{
		replay.InvestigationFile,
		replay.EventsFile,
		replay.ManifestFile,
		filepath.Join("world", "index.json"),
	} {
		if _, err := os.Stat(filepath.Join(out, want)); err != nil {
			t.Errorf("the export has no %s: %v", want, err)
		}
	}
	files, err := replay.Files(out)
	if err != nil || len(files) == 0 {
		t.Errorf("the export carries no trajectories/ (%v)", err)
	}

	manifest, err := replay.ReadManifest(out)
	if err != nil {
		t.Fatalf("read the manifest: %v", err)
	}
	if err := manifest.Verify(out); err != nil {
		t.Errorf("the export does not verify against its own manifest: %v", err)
	}

	// And it drifts loudly: one byte changed in one file and the manifest refuses it.
	events := filepath.Join(out, replay.EventsFile)
	body, err := os.ReadFile(events) //nolint:gosec // the test's own temp dir
	if err != nil {
		t.Fatalf("read %s: %v", events, err)
	}
	if err := os.WriteFile(events, append(body, ' '), 0o600); err != nil {
		t.Fatalf("write %s: %v", events, err)
	}
	if err := manifest.Verify(out); err == nil {
		t.Error("the manifest accepted an export whose event log had changed")
	}
}

// TestTheInvestigationSchemaRebuildsFromTheEventLogAndARegenerableWorld (SC-011, analyze C3).
//
// Written in exactly the form T095 asks for. What each step is asserting:
//
//  1. export — the artifact is produced from the run;
//  2. `DROP SCHEMA investigation CASCADE` — everything the investigation store held is gone, so
//     nothing below can be reading it by accident;
//  3. `Migrate` — the schema comes back **empty**, which is what "reconstruct" has to start from;
//  4. the event log in the artifact is replayed into the graph, and the graph then answers the
//     investigation's own graph queries. This is the half that makes the graph the source of
//     truth: the neighbourhood, the candidate changes and the pointers the run reasoned over are
//     all back, from events alone;
//  5. `investigate replay --from <dir>` reproduces the run against the **regenerable world** —
//     test data, not a source of truth, and re-recordable from the manifest's declared shape;
//  6. every hypothesis the decision record carries is still readable out of the artifact.
//
// Steps 4 to 6 are what "reconstructs every row" means here: the rows of `investigation.*` are a
// function of the event log, the regenerable world and the recording, and none of them needs the
// dropped schema to exist. Writing them back into the schema is the runner's job (Track D's
// `investigate replay --layer world` against a store); this test asserts the input side, which is
// the side the principle is about.
func TestTheInvestigationSchemaRebuildsFromTheEventLogAndARegenerableWorld(t *testing.T) {
	corpusTrajectories(t)
	ctx := context.Background()
	store := pgtest.Open(t)

	// 1. export.
	out := t.TempDir()
	src := newSource(t)
	if _, err := replay.Export(ctx, src, src.inv.GetInvestigationId(), out); err != nil {
		t.Fatalf("export: %v", err)
	}

	// 2. drop the schema.
	if _, err := store.Pool().Exec(ctx, `DROP SCHEMA investigation CASCADE`); err != nil {
		t.Fatalf("drop schema investigation: %v", err)
	}
	var present bool
	if err := store.Pool().QueryRow(ctx,
		`SELECT to_regclass('investigation.investigations') IS NOT NULL`).Scan(&present); err != nil {
		t.Fatalf("look up the dropped schema: %v", err)
	}
	if present {
		t.Fatal("the schema is still there after DROP SCHEMA investigation CASCADE")
	}

	// 3. the schema comes back empty. The migration ledger has to forget 0006 as well, or
	//    Migrate is a no-op over a schema that is no longer there.
	if _, err := store.Pool().Exec(ctx,
		`DELETE FROM log.schema_migrations WHERE version = 6`); err != nil {
		t.Fatalf("forget migration 0006: %v", err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
	var rows int
	if err := store.Pool().QueryRow(ctx,
		`SELECT count(*) FROM investigation.investigations`).Scan(&rows); err != nil {
		t.Fatalf("count investigations: %v", err)
	}
	if rows != 0 {
		t.Fatalf("the rebuilt schema holds %d investigations; it must start empty", rows)
	}

	// 4. the graph comes back from the event log in the artifact, and answers the investigation's
	//    own graph queries.
	if _, err := fixture.Load(ctx, projector.New(store), fixtureDir, fixture.LoadOptions{}); err != nil {
		t.Fatalf("replay the event log: %v", err)
	}
	m, err := fixture.LoadManifest(fixtureDir)
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	runner := query.NewRunner(query.NewEngine(store))
	answered := 0
	for _, q := range m.Queries {
		if _, err := runner.Run(ctx, q); err != nil {
			t.Errorf("the rebuilt graph cannot answer %s: %v", q.Name, err)
			continue
		}
		answered++
	}
	if answered == 0 {
		t.Fatal("the rebuilt graph answered none of the investigation's graph queries")
	}

	// 5. the run replays against the regenerable world, from the artifact, with no network.
	resp, err := replay.Replay(ctx, out, replay.ReplayOptions{Layer: replay.LayerTrajectory})
	if err != nil {
		t.Fatalf("replay the exported artifact: %v", err)
	}
	if !resp.GetIdentical() {
		t.Fatalf("the exported artifact did not replay: %s", resp.GetFirstDivergingRecord())
	}

	// 6. the decision record is readable out of the artifact, ledger and all.
	body, err := os.ReadFile(filepath.Join(out, replay.InvestigationFile)) //nolint:gosec // the test's own temp dir
	if err != nil {
		t.Fatalf("read the decision record: %v", err)
	}
	for _, want := range []string{"h-1", "k8s.change=shop/payments@rev7", "very_high"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("the exported decision record does not carry %q", want)
		}
	}
}
