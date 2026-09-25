// SPDX-License-Identifier: Apache-2.0

package replay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	sdk "github.com/Pierre-Theophile/aisre/pkg/backend"
)

// The self-contained artifact (tasks.md T095; FR-042, SC-011, data-model §Derived structures).
//
// An export is a directory that answers the whole investigation with no network, no database and
// no credential:
//
//	investigation.json        the decision record and the hypothesis ledger
//	events.jsonl              the graph events the investigation's graph queries need
//	trajectories/<run>.jsonl  layer 1, one file per recorded run
//	world/…                   layer 2, the recorded telemetry
//	export.json               the manifest: every file with its digest, and the export digest
//
// The reason the graph events are in there, rather than a dump of the investigation tables, is
// the whole justification for the second schema existing at all (plan §Complexity Tracking,
// analyze C3). `investigation.*` is a **derived** structure: every row in it is reconstructible
// from the event log plus the regenerable world. The rebuild test drops the schema and rebuilds
// it from exactly those two things, and it is written in that form on purpose — a rebuild that
// needed something the event log and the world do not hold would mean the investigation store
// had quietly become a substrate of its own, and principle I would no longer be true of this
// system.
//
// The world is *test data*, not a source of truth. It is regenerable: `fixture record-world`
// re-records it from the manifest's own declared shape. That is why "the event log plus a
// regenerable world" is a legitimate rebuild input and "a pg_dump of investigation.*" is not.

// The published file names of an export.
const (
	// InvestigationFile holds the decision record and the hypothesis ledger.
	InvestigationFile = "investigation.json"
	// EventsFile holds the graph events the investigation's graph queries need.
	EventsFile = "events.jsonl"
	// ManifestFile holds the per-file digests and the export digest.
	ManifestFile = "export.json"
	// WorldDirName holds the layer-2 recording.
	WorldDirName = "world"
)

// ExportSource is what Export reads. It is an interface, and a deliberately small one, so that
// the replay package stays free of the store: whoever holds the DAOs implements these four
// methods and this package owns the layout and the digest.
type ExportSource interface {
	// Investigation returns the decision record and the hypothesis ledger.
	Investigation(ctx context.Context, investigationID string) (*investigationv1.Investigation, error)
	// Trajectories returns the layer-1 recordings of the run, in a stable order.
	Trajectories(ctx context.Context, investigationID string) ([]*Trajectory, error)
	// WorldDir is the directory holding the layer-2 recording, or "" when there is none.
	WorldDir(ctx context.Context, investigationID string) (string, error)
	// GraphEvents returns the graph events the investigation's graph queries need, each one a
	// canonical JSON document.
	GraphEvents(ctx context.Context, investigationID string) ([][]byte, error)
}

// Manifest is `export.json`: what is in the artifact and what it hashes to.
type Manifest struct {
	// InvestigationID is what was exported.
	InvestigationID string `json:"investigation_id"`
	// Files maps each relative path to its sha256, hex-encoded.
	Files map[string]string `json:"files"`
	// TrajectoryRecordCount and WorldTermCount are the two recording layers' sizes.
	TrajectoryRecordCount int `json:"trajectory_record_count"`
	WorldTermCount        int `json:"world_term_count"`
	// EventCount is how many graph events travel with the artifact.
	EventCount int `json:"event_count"`
	// ExportDigest is sha256 over the sorted `path:digest` lines of Files. It is what a
	// decision record's `recording_key` points at and what a reviewer compares.
	ExportDigest string `json:"export_digest"`
}

// Export writes the self-contained artifact and returns the published response.
func Export(
	ctx context.Context,
	src ExportSource,
	investigationID, outDir string,
) (*investigationv1.ExportResponse, error) {
	if src == nil {
		return nil, errors.New("replay: export: no source to read from")
	}
	if strings.TrimSpace(investigationID) == "" {
		return nil, errors.New("replay: export: no investigation id")
	}
	if strings.TrimSpace(outDir) == "" {
		return nil, errors.New("replay: export: no output directory")
	}
	if err := os.MkdirAll(outDir, 0o750); err != nil {
		return nil, fmt.Errorf("replay: create %s: %w", outDir, err)
	}

	manifest := &Manifest{InvestigationID: investigationID, Files: map[string]string{}}

	inv, err := src.Investigation(ctx, investigationID)
	if err != nil {
		return nil, err
	}
	if inv == nil {
		return nil, fmt.Errorf("replay: export: no investigation %s", investigationID)
	}
	raw, err := protojson.MarshalOptions{Multiline: true, Indent: "  "}.Marshal(inv)
	if err != nil {
		return nil, fmt.Errorf("replay: render the decision record: %w", err)
	}
	canonical, err := graph.CanonicalJSON(json.RawMessage(raw))
	if err != nil {
		return nil, err
	}
	if err := writeExportFile(outDir, InvestigationFile, append(canonical, '\n'), manifest); err != nil {
		return nil, err
	}

	events, err := src.GraphEvents(ctx, investigationID)
	if err != nil {
		return nil, err
	}
	var eventBody []byte
	for _, event := range events {
		line, err := graph.CanonicalJSON(json.RawMessage(event))
		if err != nil {
			return nil, fmt.Errorf("replay: export: an event is not canonical JSON: %w", err)
		}
		eventBody = append(eventBody, line...)
		eventBody = append(eventBody, '\n')
	}
	manifest.EventCount = len(events)
	if err := writeExportFile(outDir, EventsFile, eventBody, manifest); err != nil {
		return nil, err
	}

	trajectories, err := src.Trajectories(ctx, investigationID)
	if err != nil {
		return nil, err
	}
	for _, traj := range trajectories {
		if traj == nil {
			continue
		}
		written, err := WriteRecords(filepath.Join(outDir, TrajectoriesDirName), traj.Records)
		if err != nil {
			return nil, err
		}
		manifest.Files[filepath.ToSlash(filepath.Join(TrajectoriesDirName,
			filepath.Base(written.Path)))] = written.Digest
		manifest.TrajectoryRecordCount += written.Records
	}

	worldDir, err := src.WorldDir(ctx, investigationID)
	if err != nil {
		return nil, err
	}
	if worldDir != "" {
		terms, err := copyWorld(worldDir, filepath.Join(outDir, WorldDirName), manifest)
		if err != nil {
			return nil, err
		}
		manifest.WorldTermCount = terms
	}

	manifest.ExportDigest = manifestDigest(manifest.Files)
	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("replay: render the export manifest: %w", err)
	}
	// The manifest is not in its own Files map: a digest cannot cover itself.
	if err := os.WriteFile(filepath.Join(outDir, ManifestFile), append(body, '\n'), 0o600); err != nil {
		return nil, fmt.Errorf("replay: write %s: %w", ManifestFile, err)
	}

	return &investigationv1.ExportResponse{
		OutDir:                outDir,
		ExportDigest:          manifest.ExportDigest,
		TrajectoryRecordCount: int64(manifest.TrajectoryRecordCount),
		WorldTermCount:        int64(manifest.WorldTermCount),
	}, nil
}

// ReadManifest reads an export's `export.json`.
func ReadManifest(dir string) (*Manifest, error) {
	raw, err := os.ReadFile(filepath.Join(dir, ManifestFile)) //nolint:gosec // an operator's own path
	if err != nil {
		return nil, fmt.Errorf("replay: read the export manifest: %w", err)
	}
	manifest := &Manifest{}
	if err := json.Unmarshal(raw, manifest); err != nil {
		return nil, fmt.Errorf("replay: parse the export manifest: %w", err)
	}
	return manifest, nil
}

// Verify re-hashes every file the manifest names and refuses an artifact that has drifted.
func (m *Manifest) Verify(dir string) error {
	paths := make([]string, 0, len(m.Files))
	for path := range m.Files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		body, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(path))) //nolint:gosec // a path the manifest published
		if err != nil {
			return fmt.Errorf("replay: the export names %s, which is not there: %w", path, err)
		}
		if got := digestOf(body); got != m.Files[path] {
			return fmt.Errorf("replay: %s hashes to %s but the export manifest claims %s; "+
				"an artifact that can drift proves nothing", path, ShortDigest(got), ShortDigest(m.Files[path]))
		}
	}
	if got := manifestDigest(m.Files); got != m.ExportDigest {
		return fmt.Errorf("replay: the export digest is %s but its files hash to %s",
			ShortDigest(m.ExportDigest), ShortDigest(got))
	}
	return nil
}

func writeExportFile(outDir, name string, body []byte, manifest *Manifest) error {
	if err := os.WriteFile(filepath.Join(outDir, name), body, 0o600); err != nil {
		return fmt.Errorf("replay: write %s: %w", name, err)
	}
	manifest.Files[name] = digestOf(body)
	return nil
}

// copyWorld copies the layer-2 recording verbatim. It is copied rather than re-recorded: the
// world is the answers the run actually saw, and re-recording it during an export would replace
// the evidence with a fresh measurement.
func copyWorld(from, to string, manifest *Manifest) (int, error) {
	world, err := sdk.LoadWorld(from)
	if err != nil {
		return 0, fmt.Errorf("replay: export the recorded world: %w", err)
	}
	if err := os.MkdirAll(to, 0o750); err != nil {
		return 0, fmt.Errorf("replay: create %s: %w", to, err)
	}
	entries, err := os.ReadDir(from)
	if err != nil {
		return 0, fmt.Errorf("replay: read %s: %w", from, err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		body, err := os.ReadFile(filepath.Join(from, entry.Name())) //nolint:gosec // inside the world this index named
		if err != nil {
			return 0, fmt.Errorf("replay: read %s: %w", entry.Name(), err)
		}
		if err := os.WriteFile(filepath.Join(to, entry.Name()), body, 0o600); err != nil {
			return 0, fmt.Errorf("replay: write %s: %w", entry.Name(), err)
		}
		manifest.Files[filepath.ToSlash(filepath.Join(WorldDirName, entry.Name()))] = digestOf(body)
	}
	return world.Len(), nil
}

// manifestDigest hashes the sorted `path:digest` lines, so the export digest is stable under any
// order the files happened to be written in.
func manifestDigest(files map[string]string) string {
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	var b strings.Builder
	for _, path := range paths {
		fmt.Fprintf(&b, "%s:%s\n", path, files[path])
	}
	return digestOf([]byte(b.String()))
}

// CopyFile is the small helper the fixture harness uses to stage an export input. It is here
// rather than duplicated there because an export and a fixture stage the same shapes.
func CopyFile(from, to string) error {
	src, err := os.Open(from) //nolint:gosec // an operator's own path
	if err != nil {
		return fmt.Errorf("replay: open %s: %w", from, err)
	}
	defer func() { _ = src.Close() }()
	if err := os.MkdirAll(filepath.Dir(to), 0o750); err != nil {
		return fmt.Errorf("replay: create %s: %w", filepath.Dir(to), err)
	}
	dst, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // an operator's own path
	if err != nil {
		return fmt.Errorf("replay: create %s: %w", to, err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		return fmt.Errorf("replay: copy %s: %w", to, err)
	}
	return dst.Close()
}
