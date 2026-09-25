// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"connectrpc.com/connect"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/investigation/replay"
	eventlog "github.com/Pierre-Theophile/aisre/internal/log"
	sdk "github.com/Pierre-Theophile/aisre/pkg/backend"
)

// Export and replay (FR-039–FR-042).
//
// The on-disk layout, the digests and the divergence report belong to
// `internal/investigation/replay` (Phase 7 Track E, T093–T095); what belongs here is the
// **source** — the four reads that turn a stored investigation into an exportable one. Splitting
// it that way means the fixture harness and the server export the same bytes from different
// inputs: the harness reads an incident fixture's directory, the runner reads the database, and
// both hand the exporter the same interface.
//
// `Export` and `Replay` are therefore one line each. That is the whole of what this file adds to
// the RPC: an export that improvised its own layout would be an export the replay gate cannot
// read, so the layout is owned in one place and called from two.
//
// Neither call opens a socket. A replay that fell back to a live call to satisfy a miss would be
// evidence of nothing (research §9), and an export that reached for a vendor would not be
// self-contained.

// WorldRoot is where a run's layer-2 recording lives when one was recorded beside it. It is the
// exporter's own name for the directory, so a recording written here lands where the export
// expects to find it.
const WorldRoot = replay.WorldDirName

var _ replay.ExportSource = (*Runner)(nil)

// Investigation is the export source's decision record and hypothesis ledger.
func (r *Runner) Investigation(ctx context.Context, investigationID string) (*investigationv1.Investigation, error) {
	inv, err := r.dao.Get(ctx, investigationID)
	if err != nil {
		return nil, err
	}
	rows, err := r.ledgerDAO.Load(ctx, investigationID)
	if err != nil {
		return nil, err
	}
	inv.Ledger = rows.Proto()
	spend, err := r.dao.SpendOf(ctx, investigationID)
	if err != nil {
		return nil, err
	}
	if spend != nil {
		inv.Spend = spend
	}
	return inv, nil
}

// Trajectories is the export source's layer 1: every recorded run of this investigation.
func (r *Runner) Trajectories(_ context.Context, investigationID string) ([]*replay.Trajectory, error) {
	if r.cfg.RecordingRoot == "" {
		return nil, nil
	}
	paths, err := replay.Files(filepath.Join(r.cfg.RecordingRoot, investigationID))
	if err != nil {
		return nil, err
	}
	out := make([]*replay.Trajectory, 0, len(paths))
	for _, path := range paths {
		traj, err := replay.Read(path)
		if err != nil {
			return nil, err
		}
		out = append(out, traj)
	}
	return out, nil
}

// WorldDir is the export source's layer 2: **the world the run actually read from**. An empty
// answer means there is none, which is the ordinary case for a run against a live backend.
//
// It used to look only under `<root>/<investigation-id>/world`, and nothing ever writes there.
// `serve` serves a recorded world from `--recording-root` itself — `<root>/world`, or `<root>`
// when the root *is* the world directory (`investigationTelemetry`, `worldDirOf`) — so every
// export from a fixture-served deployment silently carried layer 1 and no layer 2, and
// `investigate replay --layer world` against it had nothing to replay.
//
// The three candidates are tried in the order that puts the most specific first:
//
//  1. `<root>/<investigation-id>/world` — a world recorded beside this run's own trajectories.
//     Nothing in this build writes one, but the layout is published (FR-042a) and a per-run
//     recorder belongs there, so it wins where it exists.
//  2. `Config.WorldDir`, when the deployment named one explicitly.
//  3. the recording root itself, then `<root>/world` — the world the engine is being served
//     from, which is the one the run actually read.
//
// A candidate counts only when it holds an `index.json`. A bare directory is not a world, and
// exporting one would produce an artifact whose layer 2 cannot be loaded.
func (r *Runner) WorldDir(_ context.Context, investigationID string) (string, error) {
	candidates := []string{r.cfg.WorldDir}
	if r.cfg.RecordingRoot != "" {
		candidates = append([]string{filepath.Join(r.cfg.RecordingRoot, investigationID, WorldRoot)},
			append(candidates, r.cfg.RecordingRoot,
				filepath.Join(r.cfg.RecordingRoot, WorldRoot))...)
	}
	for _, dir := range candidates {
		if isWorldDir(dir) {
			return dir, nil
		}
	}
	return "", nil
}

// isWorldDir reports whether a directory is a recorded world: one holding the index every world
// is addressed through.
func isWorldDir(dir string) bool {
	if strings.TrimSpace(dir) == "" {
		return false
	}
	info, err := os.Stat(filepath.Join(dir, sdk.IndexFile))
	return err == nil && !info.IsDir()
}

// GraphEvents is the export source's event log: what a rebuild replays to answer the
// investigation's graph queries.
//
// It exports the whole log rather than a slice of it, and that is deliberate rather than lazy. A
// graph answer at a pinned instant depends on every event the projector had applied by then —
// including the merges and retractions that decided identity — and any filter narrow enough to be
// worth applying would be a filter that could omit one. The rebuild test (T095) is what this
// serves, and a rebuild missing an event is a rebuild that silently answers differently.
func (r *Runner) GraphEvents(ctx context.Context, _ string) ([][]byte, error) {
	var out [][]byte
	err := eventlog.New(r.cfg.Store).Iterate(ctx, 0, func(record *eventlog.Record) error {
		raw, err := graph.CanonicalJSON(record.Envelope)
		if err != nil {
			return fmt.Errorf("export graph events: %w", err)
		}
		out = append(out, raw)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Export writes the self-contained artifact (FR-042).
func (r *Runner) Export(ctx context.Context, req *investigationv1.ExportRequest) (*investigationv1.ExportResponse, error) {
	if req.GetInvestigationId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("export: no investigation id"))
	}
	if req.GetOutDir() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("export: no output directory; the artifact is written where the caller asked "+
				"and nowhere else"))
	}
	return replay.Export(ctx, r, req.GetInvestigationId(), req.GetOutDir())
}

// Replay replays an exported investigation with no network (FR-039–FR-041).
//
// A divergence is a *result*, not an error: the response comes back with `identical: false` and
// the first diverging record named, the RPC stays a 200, and the CLI turns it into exit 4. An
// error means the export could not be read at all.
func (r *Runner) Replay(ctx context.Context, req *investigationv1.ReplayRequest) (*investigationv1.ReplayResponse, error) {
	if req.GetExportPath() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("replay: no export path; a replay runs against a recording, never against "+
				"a live backend (FR-039)"))
	}
	layer, err := replay.ParseLayer(req.GetLayer())
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	return replay.Replay(ctx, req.GetExportPath(), replay.ReplayOptions{Layer: layer})
}
