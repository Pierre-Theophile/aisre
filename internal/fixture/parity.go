// SPDX-License-Identifier: Apache-2.0

package fixture

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The live parity check (T159; FR-143, SC-014, SC-016).
//
// FR-143: *"the graph built live against the organisation MUST equal the graph built from the
// recording of the same run, and a digest computed live over a settled window MUST equal the digest
// computed from the recorded response for the same request."*
//
// ---------------------------------------------------------------------------------------------
// Two assertions, not one, and they fail for different reasons
//
// The **graph** half catches a feeder that behaves differently when it is recording — a code path
// taken only with `--record`, an instant read from the clock rather than from the payload, a map
// iteration that reached the emitter in a different order. The **digest** half catches something
// else entirely: a backend that derives an answer from the *connection* rather than from the response
// (FR-008). A backend reading a project number off the credential would produce identical graphs from
// live and recorded runs and different digests, because only the digest is computed from what the API
// returned.
//
// So a parity run that checked only the graph would pass the case FR-008 exists to prevent, which is
// why this returns both and why neither is optional.
//
// # Why the comparison is of two RECORDINGS rather than of a live run against a recording
//
// A live run's output *is* a recording: `feed … --record` writes one. So "live against recorded"
// means two directories, and that is the shape this takes — which has the side effect of making the
// check runnable without a credential, against two recordings of anything. That is not the check
// FR-143 asks for, and the CLI says so; it is the check's own test.
//
// # What "byte for byte" means here
//
// For the graph, the structural snapshot the shuffle step already compares: entities by their
// identifiers, their versions' valid intervals, properties and edges. Not the raw event bytes — two
// runs of the same feeder legitimately mint different event ids, and a comparison that failed on
// those would fail on every parity run and be switched off. For the world, the bytes, because a
// recorded response is exactly what the digest was computed from and a canonicalised comparison
// would hide the field a `NaN` or a reordered map turned into.

// ParityResult is what a parity run found.
type ParityResult struct {
	// GraphEqual and WorldEqual are the two halves of FR-143.
	GraphEqual bool `json:"graph_equal"`
	WorldEqual bool `json:"world_equal"`
	// GraphDetail and WorldDetail say what differed, or what was compared when nothing did.
	GraphDetail string `json:"graph_detail"`
	WorldDetail string `json:"world_detail"`
	// Events and WorldEntries are what the comparison covered, so a reader can tell parity over a
	// real corpus from parity over an empty pair of directories.
	Events       int `json:"events"`
	WorldEntries int `json:"world_entries"`
}

// Passed reports whether both halves held.
func (r ParityResult) Passed() bool { return r.GraphEqual && r.WorldEqual }

// ErrNothingCompared is returned when neither directory holds anything to compare. Two empty
// directories are byte-identical, and reporting that as parity would be the emptiest possible pass.
var ErrNothingCompared = errors.New("fixture: neither recording holds events or a recorded world, " +
	"so parity held over nothing; two empty directories are identical and that is not the claim")

// Parity compares the graph and the recorded world of two recordings.
//
// `live` and `recorded` are named for FR-143's framing rather than because the function treats them
// differently: the comparison is symmetric, and the names decide only which side a difference is
// reported as.
func Parity(ctx context.Context, newStore StoreFactory, live, recorded string) (ParityResult, error) {
	var out ParityResult

	liveEvents, liveManifest, err := parityLoad(live)
	if err != nil {
		return out, err
	}
	recordedEvents, recordedManifest, err := parityLoad(recorded)
	if err != nil {
		return out, err
	}
	out.Events = len(recordedEvents)

	liveWorld, err := parityWorld(live)
	if err != nil {
		return out, err
	}
	recordedWorld, err := parityWorld(recorded)
	if err != nil {
		return out, err
	}
	out.WorldEntries = len(recordedWorld)

	if len(liveEvents) == 0 && len(recordedEvents) == 0 && len(liveWorld) == 0 && len(recordedWorld) == 0 {
		return out, ErrNothingCompared
	}

	// The graph half. Each side is applied into a database of its own, in its own recorded order,
	// because the point is whether the two produce the same graph — not whether one ordering of a
	// merged stream does.
	liveSnapshot, err := applyShuffled(ctx, newStore, liveManifest, liveEvents)
	if err != nil {
		return out, err
	}
	recordedSnapshot, err := applyShuffled(ctx, newStore, recordedManifest, recordedEvents)
	if err != nil {
		return out, err
	}
	if liveSnapshot == recordedSnapshot {
		out.GraphEqual = true
		out.GraphDetail = fmt.Sprintf("%d live and %d recorded event(s) produced the same graph",
			len(liveEvents), len(recordedEvents))
	} else {
		out.GraphDetail = "the live and recorded graphs differ:\n" +
			firstDifference(recordedSnapshot, liveSnapshot)
	}

	out.WorldEqual, out.WorldDetail = compareWorlds(liveWorld, recordedWorld)
	return out, nil
}

// parityLoad reads a recording's manifest and events.
func parityLoad(dir string) ([]Event, *Manifest, error) {
	m, err := LoadManifest(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("fixture: parity %s: %w", dir, err)
	}
	events, err := ReadEvents(m.EventsPath())
	if err != nil {
		return nil, nil, fmt.Errorf("fixture: parity %s: %w", dir, err)
	}
	return events, m, nil
}

// parityWorld reads a recording's `world/` directory as path-to-bytes.
//
// An absent world is not an error: a topology recording has no telemetry world, and FR-143's digest
// half then has nothing to say. Reporting that as a difference would fail every parity run over a
// recording that legitimately holds no world.
func parityWorld(dir string) (map[string][]byte, error) {
	root := filepath.Join(dir, "world")
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("fixture: parity: read %s: %w", root, err)
	}
	out := map[string][]byte{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		body, err := os.ReadFile(filepath.Join(root, e.Name())) //nolint:gosec // a path from the listing
		if err != nil {
			return nil, fmt.Errorf("fixture: parity: read %s: %w", e.Name(), err)
		}
		out[e.Name()] = body
	}
	return out, nil
}

// compareWorlds compares two recorded worlds and says what differs.
//
// A world file's NAME is the digest of the request that produced it, so a name present on one side
// only is a request one run answered and the other did not — a different failure from the same
// request answered differently, and a reviewer needs to be told which. Answering more requests is
// still a failure: a live run that read something the recording did not is a live run the recording
// cannot replay.
func compareWorlds(live, recorded map[string][]byte) (bool, string) {
	if len(live) == 0 && len(recorded) == 0 {
		return true, "neither recording holds a world, so the digest half has nothing to compare " +
			"(a topology recording legitimately has none)"
	}

	var onlyLive, onlyRecorded, differing []string
	for name := range live {
		if _, ok := recorded[name]; !ok {
			onlyLive = append(onlyLive, name)
		}
	}
	for name, recordedBody := range recorded {
		liveBody, ok := live[name]
		if !ok {
			onlyRecorded = append(onlyRecorded, name)
			continue
		}
		if string(liveBody) != string(recordedBody) {
			differing = append(differing, name)
		}
	}
	sort.Strings(onlyLive)
	sort.Strings(onlyRecorded)
	sort.Strings(differing)

	if len(onlyLive) == 0 && len(onlyRecorded) == 0 && len(differing) == 0 {
		return true, fmt.Sprintf("%d recorded response(s) identical on every field, the coverage "+
			"block included", len(recorded))
	}

	var parts []string
	if len(differing) > 0 {
		parts = append(parts, fmt.Sprintf("%d request(s) answered differently (%s)",
			len(differing), strings.Join(truncateList(differing), ", ")))
	}
	if len(onlyLive) > 0 {
		parts = append(parts, fmt.Sprintf("%d request(s) the live run answered and the recording "+
			"does not (%s) — a live run reading something the recording cannot replay",
			len(onlyLive), strings.Join(truncateList(onlyLive), ", ")))
	}
	if len(onlyRecorded) > 0 {
		parts = append(parts, fmt.Sprintf("%d request(s) the recording answers and the live run did "+
			"not (%s)", len(onlyRecorded), strings.Join(truncateList(onlyRecorded), ", ")))
	}
	return false, strings.Join(parts, "; ")
}

// truncateList keeps a difference report readable. A parity failure over a large world can name
// hundreds of files, and a report nobody reads is a report that might as well say "differs".
func truncateList(names []string) []string {
	const most = 5
	if len(names) <= most {
		return names
	}
	out := make([]string, 0, most+1)
	out = append(out, names[:most]...)
	return append(out, fmt.Sprintf("and %d more", len(names)-most))
}
