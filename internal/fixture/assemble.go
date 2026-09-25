// SPDX-License-Identifier: Apache-2.0

package fixture

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/Pierre-Theophile/aisre/pkg/feeder/record"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/source"
)

// Assembling one fixture from several recorders (T082, FR-050 (b), constitution VIII).
//
// A live run has two feeders running side by side, each with its own `--record DIR`, and the
// Kubernetes one is restarted in the middle of the feeder-gap scenario, which needs a third
// directory. A fixture is one directory. This is the step between, and it is a command rather
// than a page of instructions because the live run of 2026-09-16 did it by hand, with a Python
// script that lived in /tmp, and the nightly job cannot.
//
// What it merges, and why each rule is what it is:
//
//   - **payloads**, renumbered per kind and copied, with `payloads/index.jsonl` rewritten to
//     point at the new names and sorted by arrival. Renumbering is not cosmetic: the recorder
//     numbers each kind from 000001 on every run and writes with truncation, so two runs into
//     one directory silently overwrite each other's payloads while the index still points at
//     them (live run 2026-09-16, finding 3). Two *different* feeders producing the same payload
//     kind would make the merge ambiguous and is refused rather than guessed at.
//   - **events**, ordered by observed time and renumbered. `appendedSeq` is the log's own
//     counter and has to be dense and ascending across the assembled file, so the line's
//     leading `{"appendedSeq":N,` is rewritten in place. In place, byte for byte, and not by
//     re-serializing: the events are canonical JSON the graph produced and a fixture that
//     re-encodes them is a fixture testing this program's encoder.
//   - **rejected events**, concatenated, because their order carries nothing.
//   - **the manifest**, with the union of every run's declared sources. What a source promises
//     is the feeder's statement about itself, so it is copied from the recording rather than
//     invented here; two runs of the same feeder must agree about it, and disagreeing is an
//     error.
//
// What it does not do is anything a human has to decide: the description, the queries whose
// answers become goldens, and the ground truth are left for the pull request that ships the
// fixture, exactly as `record.WriteManifest` leaves them.

// AssembleOptions configures an assemble run.
type AssembleOptions struct {
	// ID is the fixture id. Empty means the base name of the output directory, which is what
	// the manifest contract requires anyway.
	ID string
	// Family groups fixtures exercising the same behaviour, e.g. "rollout-regression".
	Family string
	// Description is the human summary. Empty leaves record.WriteManifest's placeholder.
	Description string
}

// AssembleReport is what an assemble run produced.
type AssembleReport struct {
	// FixtureID is the id written into the manifest.
	FixtureID string `json:"fixture_id"`
	// Out is the fixture directory that was written.
	Out string `json:"out"`
	// Runs are the recording directories that were merged, in the order they were given.
	Runs []string `json:"runs"`
	// Sources are the source ids the merged manifest declares.
	Sources []string `json:"sources"`
	// Payloads is how many payload files were copied, and PayloadKinds their directories.
	Payloads     int      `json:"payloads"`
	PayloadKinds []string `json:"payload_kinds"`
	// Events is how many accepted events the assembled stream holds, and EventsBySource how
	// many each feeder contributed.
	Events         int            `json:"events"`
	EventsBySource map[string]int `json:"events_by_source"`
	// Rejected is how many must-be-rejected events were carried over.
	Rejected int `json:"rejected"`
	// ClockStart and ClockEnd are the arrival range of the merged payloads, which is what the
	// manifest's clock is derived from.
	ClockStart time.Time `json:"clock_start"`
	ClockEnd   time.Time `json:"clock_end"`
}

// appendedSeqPrefix matches the leading key of a recorded event line. Canonical JSON sorts keys,
// so `appendedSeq` is always first; a line that does not start with it did not come from the
// recorder and is refused rather than renumbered by guesswork.
var appendedSeqPrefix = regexp.MustCompile(`^\{"appendedSeq":\d+,`)

// Assemble merges the recording directories in runs into one fixture directory at out.
//
// runs are given in the order their recordings began; it only decides the tie-break between two
// events with the same observed time, which is exactly the ambiguity the caller is the one who
// can resolve.
func Assemble(runs []string, out string, opts AssembleOptions) (*AssembleReport, error) {
	if len(runs) == 0 {
		return nil, errors.New("fixture: assemble needs at least one recording directory")
	}
	if out == "" {
		return nil, errors.New("fixture: assemble needs an output directory")
	}
	id := opts.ID
	if id == "" {
		id = filepath.Base(filepath.Clean(out))
	}

	report := &AssembleReport{
		FixtureID:      id,
		Out:            out,
		Runs:           slices.Clone(runs),
		EventsBySource: map[string]int{},
	}
	sources, err := mergeSources(runs)
	if err != nil {
		return nil, err
	}
	for _, src := range sources {
		report.Sources = append(report.Sources, src.SourceID)
	}

	if err := os.MkdirAll(out, 0o755); err != nil {
		return nil, fmt.Errorf("fixture: create %s: %w", out, err)
	}
	if err := assemblePayloads(runs, out, report); err != nil {
		return nil, err
	}
	if err := assembleEvents(runs, out, report); err != nil {
		return nil, err
	}
	if err := assembleRejected(runs, out, report); err != nil {
		return nil, err
	}

	if err := record.WriteManifest(out, record.Manifest{
		ID:          id,
		Family:      opts.Family,
		Description: opts.Description,
		Sources:     sources,
	}); err != nil {
		return nil, err
	}
	m, err := LoadManifest(out)
	if err != nil {
		return nil, fmt.Errorf("fixture: the assembled manifest does not load: %w", err)
	}
	report.ClockStart, report.ClockEnd = m.Clock.Start, m.Clock.End
	return report, nil
}

// mergeSources reads each run's recorded manifest and returns the union of the sources they
// declare. Two runs of the same feeder must agree about what it promises; disagreeing means one
// of the two recordings was made with a different build or different flags, and merging them
// would produce a fixture whose declared contract is true of neither half.
func mergeSources(runs []string) ([]record.ManifestSource, error) {
	var (
		merged []record.ManifestSource
		byID   = map[string]record.ManifestSource{}
	)
	for _, dir := range runs {
		m, err := LoadManifest(dir)
		if err != nil {
			return nil, fmt.Errorf("fixture: assemble: %s has no usable recorded manifest: %w", dir, err)
		}
		for _, src := range m.Sources {
			candidate := record.ManifestSource{
				SourceID:         src.SourceID,
				Kind:             src.Kind,
				Ordering:         src.Ordering,
				ReorderingWindow: src.ReorderingWindow,
			}
			seen, ok := byID[src.SourceID]
			if !ok {
				byID[src.SourceID] = candidate
				merged = append(merged, candidate)
				continue
			}
			if seen != candidate {
				return nil, fmt.Errorf("fixture: assemble: %s declares source %s as %+v, "+
					"but an earlier run declared %+v; two recordings of one feeder must agree "+
					"about what it promises", dir, src.SourceID, candidate, seen)
			}
		}
	}
	return merged, nil
}

// payloadEntry is one merged index line with the run it came from, kept so the sort is stable
// and reproducible.
type payloadEntry struct {
	entry source.IndexEntry
	run   int
}

// assemblePayloads copies every run's payloads under out, renumbering per kind, and writes the
// merged index in arrival order.
func assemblePayloads(runs []string, out string, report *AssembleReport) error {
	root := filepath.Join(out, source.PayloadsDir)
	if err := os.RemoveAll(root); err != nil {
		return fmt.Errorf("fixture: clear %s: %w", root, err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("fixture: create %s: %w", root, err)
	}

	var (
		counts  = map[string]int{}
		ownedBy = map[string]string{}
		entries []payloadEntry
	)
	for i, dir := range runs {
		src := filepath.Join(dir, source.PayloadsDir)
		info, err := os.Stat(src)
		if errors.Is(err, os.ErrNotExist) || (err == nil && !info.IsDir()) {
			continue
		}
		if err != nil {
			return fmt.Errorf("fixture: read %s: %w", src, err)
		}
		owner, err := runSourceID(dir)
		if err != nil {
			return err
		}

		remap := map[string]string{}
		kinds, err := os.ReadDir(src)
		if err != nil {
			return fmt.Errorf("fixture: read %s: %w", src, err)
		}
		for _, kind := range kinds {
			if !kind.IsDir() {
				continue
			}
			name := kind.Name()
			if held, ok := ownedBy[name]; ok && held != owner {
				return fmt.Errorf("fixture: assemble: payload kind %q is produced by both %s "+
					"and %s; the merged index could not say which feeder a payload came from",
					name, held, owner)
			}
			ownedBy[name] = owner
			if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
				return fmt.Errorf("fixture: create %s: %w", filepath.Join(root, name), err)
			}
			files, err := os.ReadDir(filepath.Join(src, name))
			if err != nil {
				return fmt.Errorf("fixture: read %s: %w", filepath.Join(src, name), err)
			}
			for _, file := range files {
				if file.IsDir() {
					continue
				}
				counts[name]++
				renamed := fmt.Sprintf("%06d%s", counts[name], filepath.Ext(file.Name()))
				if err := copyFile(
					filepath.Join(src, name, file.Name()),
					filepath.Join(root, name, renamed)); err != nil {
					return err
				}
				remap[name+"/"+file.Name()] = name + "/" + renamed
			}
		}

		lines, err := readLines(filepath.Join(src, source.IndexFile))
		if err != nil {
			return err
		}
		for n, line := range lines {
			var entry source.IndexEntry
			if err := json.Unmarshal(line, &entry); err != nil {
				return fmt.Errorf("fixture: %s/%s line %d: %w", src, source.IndexFile, n+1, err)
			}
			renamed, ok := remap[entry.File]
			if !ok {
				return fmt.Errorf("fixture: %s/%s names %s, which is not on disk",
					src, source.IndexFile, entry.File)
			}
			entry.File = renamed
			entries = append(entries, payloadEntry{entry: entry, run: i})
		}
	}

	slices.SortStableFunc(entries, func(a, b payloadEntry) int {
		if c := a.entry.At.Compare(b.entry.At); c != 0 {
			return c
		}
		if c := a.run - b.run; c != 0 {
			return c
		}
		return strings.Compare(a.entry.File, b.entry.File)
	})

	var merged bytes.Buffer
	for _, item := range entries {
		line, err := json.Marshal(canonicalIndexEntry{
			At: item.entry.At, File: item.entry.File, Kind: item.entry.Kind, Seq: item.entry.Seq,
		})
		if err != nil {
			return fmt.Errorf("fixture: encode index entry: %w", err)
		}
		merged.Write(line)
		merged.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(root, source.IndexFile), merged.Bytes(), 0o644); err != nil {
		return fmt.Errorf("fixture: write %s: %w", filepath.Join(root, source.IndexFile), err)
	}

	report.Payloads = len(entries)
	report.PayloadKinds = slices.Sorted(maps.Keys(counts))
	return nil
}

// canonicalIndexEntry is source.IndexEntry with its fields in alphabetical order, so the merged
// index is written in the project's canonical spelling — sorted keys, no insignificant
// whitespace — like every other JSON file a fixture ships. The recorder writes the same data in
// struct order, which is equally valid to read back; an assembled fixture is reviewed as a diff,
// so it is the canonical one that is worth having.
type canonicalIndexEntry struct {
	At   time.Time `json:"at"`
	File string    `json:"file"`
	Kind string    `json:"kind"`
	Seq  int64     `json:"seq"`
}

// eventLine is one recorded event line, kept as bytes so the merge never re-encodes it.
type eventLine struct {
	observedAt  time.Time
	sourceID    string
	run         int
	appendedSeq int64
	raw         []byte
}

// assembleEvents merges every run's events.jsonl in observed-time order and renumbers
// appendedSeq across the result.
func assembleEvents(runs []string, out string, report *AssembleReport) error {
	var events []eventLine
	for i, dir := range runs {
		lines, err := readLines(filepath.Join(dir, record.EventsFile))
		if err != nil {
			return err
		}
		for n, line := range lines {
			var head struct {
				AppendedSeq int64     `json:"appendedSeq"`
				ObservedAt  time.Time `json:"observedAt"`
				SourceID    string    `json:"sourceId"`
			}
			if err := json.Unmarshal(line, &head); err != nil {
				return fmt.Errorf("fixture: %s/%s line %d: %w", dir, record.EventsFile, n+1, err)
			}
			if !appendedSeqPrefix.Match(line) {
				return fmt.Errorf("fixture: %s/%s line %d does not begin with appendedSeq; "+
					"canonical JSON sorts it first, so this is not a recording this command can "+
					"renumber without re-encoding it", dir, record.EventsFile, n+1)
			}
			events = append(events, eventLine{
				observedAt:  head.ObservedAt,
				sourceID:    head.SourceID,
				run:         i,
				appendedSeq: head.AppendedSeq,
				raw:         line,
			})
		}
	}

	slices.SortStableFunc(events, func(a, b eventLine) int {
		if c := a.observedAt.Compare(b.observedAt); c != 0 {
			return c
		}
		if c := a.run - b.run; c != 0 {
			return c
		}
		return int(a.appendedSeq - b.appendedSeq)
	})

	var merged bytes.Buffer
	for i, event := range events {
		merged.Write(appendedSeqPrefix.ReplaceAll(event.raw,
			[]byte(fmt.Sprintf(`{"appendedSeq":%d,`, i+1))))
		merged.WriteByte('\n')
		report.EventsBySource[event.sourceID]++
	}
	path := filepath.Join(out, record.EventsFile)
	if err := os.WriteFile(path, merged.Bytes(), 0o644); err != nil {
		return fmt.Errorf("fixture: write %s: %w", path, err)
	}
	report.Events = len(events)
	return nil
}

// assembleRejected concatenates the runs' must-be-rejected events, or removes a stale file when
// none of them recorded any.
func assembleRejected(runs []string, out string, report *AssembleReport) error {
	var merged bytes.Buffer
	for _, dir := range runs {
		lines, err := readLines(filepath.Join(dir, record.RejectedFile))
		if err != nil {
			return err
		}
		for _, line := range lines {
			merged.Write(line)
			merged.WriteByte('\n')
			report.Rejected++
		}
	}
	path := filepath.Join(out, record.RejectedFile)
	if report.Rejected == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("fixture: remove %s: %w", path, err)
		}
		return nil
	}
	if err := os.WriteFile(path, merged.Bytes(), 0o644); err != nil {
		return fmt.Errorf("fixture: write %s: %w", path, err)
	}
	return nil
}

// runSourceID is the single source a recording directory declares. A recorder writes one
// feeder's stream, so more than one is a directory that has already been merged and merging it
// again would renumber payloads a second time.
func runSourceID(dir string) (string, error) {
	m, err := LoadManifest(dir)
	if err != nil {
		return "", fmt.Errorf("fixture: assemble: %s has no usable recorded manifest: %w", dir, err)
	}
	if len(m.Sources) != 1 {
		return "", fmt.Errorf("fixture: assemble: %s declares %d sources; a recording directory "+
			"holds one feeder's stream, so this is already an assembled fixture", dir, len(m.Sources))
	}
	return m.Sources[0].SourceID, nil
}

// readLines returns the non-empty lines of path, or nothing when it does not exist: a feeder
// that recorded no rejection and a run that produced no events are both ordinary.
func readLines(path string) ([][]byte, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // a recording path supplied by the caller
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("fixture: read %s: %w", path, err)
	}
	var out [][]byte
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		out = append(out, line)
	}
	return out, nil
}

// copyFile copies src to dst verbatim. Payloads are the recording's evidence and are never
// rewritten, only renamed.
func copyFile(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // a recording path supplied by the caller
	if err != nil {
		return fmt.Errorf("fixture: read %s: %w", src, err)
	}
	defer in.Close()
	outFile, err := os.Create(dst) //nolint:gosec // inside the caller's output directory
	if err != nil {
		return fmt.Errorf("fixture: write %s: %w", dst, err)
	}
	if _, err := io.Copy(outFile, in); err != nil {
		outFile.Close()
		return fmt.Errorf("fixture: copy %s to %s: %w", src, dst, err)
	}
	if err := outFile.Close(); err != nil {
		return fmt.Errorf("fixture: write %s: %w", dst, err)
	}
	return nil
}

// Markdown renders the report for a terminal or a job summary.
func (r *AssembleReport) Markdown() string {
	var out strings.Builder
	fmt.Fprintf(&out, "## %s — assembled\n\n", r.FixtureID)
	fmt.Fprintf(&out, "- out: %s\n", r.Out)
	fmt.Fprintf(&out, "- runs merged: %s\n", strings.Join(r.Runs, ", "))
	fmt.Fprintf(&out, "- sources: %s\n", strings.Join(r.Sources, ", "))
	fmt.Fprintf(&out, "- payloads: %d in %s\n", r.Payloads, strings.Join(r.PayloadKinds, ", "))
	fmt.Fprintf(&out, "- events: %d", r.Events)
	if len(r.EventsBySource) > 0 {
		parts := make([]string, 0, len(r.EventsBySource))
		for _, id := range slices.Sorted(maps.Keys(r.EventsBySource)) {
			parts = append(parts, fmt.Sprintf("%s %d", id, r.EventsBySource[id]))
		}
		fmt.Fprintf(&out, " (%s)", strings.Join(parts, ", "))
	}
	out.WriteString("\n")
	fmt.Fprintf(&out, "- rejected: %d\n", r.Rejected)
	fmt.Fprintf(&out, "- clock: %s .. %s\n",
		r.ClockStart.UTC().Format(time.RFC3339), r.ClockEnd.UTC().Format(time.RFC3339))
	out.WriteString("\nAdd a `queries:` list and a description to the manifest, then run " +
		"`aisre fixture record` and `aisre fixture verify`.\n")
	return out.String()
}
