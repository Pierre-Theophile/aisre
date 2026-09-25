// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// Knowing it was away (T067, FR-052, spec edge case "feeder gap").
//
// A feeder that is stopped and started again is a new process. It lists the namespace and
// reports what is there, and everything it reports is a DUPLICATE_NOOP — correct, and silent
// about the one thing that matters: what is *not* there any more. Until this file existed, a
// Service deleted while the feeder was down stayed valid in the graph for ever, and the
// checkpoint that closed the reconnection's list said `gap_before: false`, because the only
// memory the feeder had was a resync counter that a restart sets back to zero (live run
// 2026-09-16, finding 8).
//
// The missing piece is memory across processes, and it is deliberately *small*: the instant of
// the last checkpoint this source wrote, and the set of objects it had asserted. Both go into
// one `state.json` under `--state-dir`, rewritten atomically after every checkpoint.
//
// **The state never reaches the feeder directly.** The informers read it and push it as a
// `start` marker payload, before the initial list, and the feeder reads it from there. That is
// the whole trick: a recording of a live run then contains the marker, so `--replay` of that
// recording sees exactly what the live process saw and reconciles identically (FR-044). A
// feeder that read the file itself would behave one way live and another on replay, which is
// the thing this project's fixtures exist to prevent.
//
// A feeder never reads the graph to find this out (constitution I). It could: the graph knows
// every source's last checkpoint. It must not — that would make a connector's behaviour depend
// on a query, and a connector is a writer.

// KindStart is the marker payload an informer pushes before its initial list, carrying what
// the previous process of this source left behind. It is a payload rather than a constructor
// argument so that the information survives into a recording (see the file comment).
const KindStart = "start"

// StateVersion is the schema version of `state.json`. A file written by a newer feeder is
// refused rather than misread.
const StateVersion = 1

// StateFile is the name of the file inside the state directory.
const StateFile = "state.json"

// KnownObject is one object a feeder asserted, as its successor needs to know it: enough to
// retract it without having seen it.
//
// `Kind` is the payload kind (`services`, `deployments`, …), which is what maps back to the
// ref namespace and the id part of a retraction. `ResourceVersion` is carried because a
// retraction's event id names it, so a reconnecting feeder mints the same id its predecessor
// would have (FR-018).
type KnownObject struct {
	Kind            string `json:"kind"`
	Namespace       string `json:"namespace,omitempty"`
	Name            string `json:"name"`
	UID             string `json:"uid,omitempty"`
	ResourceVersion string `json:"resource_version,omitempty"`
}

// objectKey identifies an object across processes: `<payload kind>|<namespace>/<name>`, the
// same shape the feeder's in-memory indexes use.
func (o KnownObject) objectKey() string { return o.Kind + "|" + key(o.Namespace, o.Name) }

// sortObjects puts a set of objects in the one order every writer must produce, so that two
// runs of the same feeder write the same bytes (FR-023).
func sortObjects(objects []KnownObject) []KnownObject {
	sort.Slice(objects, func(i, j int) bool { return objects[i].objectKey() < objects[j].objectKey() })
	return objects
}

// StartState is the body of a `start` marker payload, and the part of `state.json` a successor
// reads.
type StartState struct {
	// PreviousCheckpoint is the extent the last checkpoint of the previous process reached.
	// Nil means this source has never checkpointed: a first run is not a gap.
	PreviousCheckpoint *time.Time `json:"previous_checkpoint"`
	// KnownObjects is what that process had asserted and not retracted, sorted.
	KnownObjects []KnownObject `json:"known_objects"`
}

// StartPayload renders a `start` marker. `at` is when this process began watching, which is
// the instant its coverage resumes and the instant a retraction for something that vanished
// during the gap is dated (Feeder.reconcile).
func StartPayload(at time.Time, s StartState) (feeder.Payload, error) {
	s.KnownObjects = sortObjects(append([]KnownObject(nil), s.KnownObjects...))
	if s.KnownObjects == nil {
		s.KnownObjects = []KnownObject{}
	}
	body, err := json.Marshal(s)
	if err != nil {
		return feeder.Payload{}, fmt.Errorf("k8s: encode the start marker: %w", err)
	}
	return feeder.Payload{Kind: KindStart, At: at.UTC(), Seq: SyncPayloadSeq, Bytes: body}, nil
}

// State is `state.json`: everything one process of a feeder leaves for the next one.
type State struct {
	// Version is StateVersion.
	Version int `json:"version"`
	// SourceID is whose state this is, so that a mis-pointed --state-dir is an error rather
	// than a silent reconciliation against another cluster's objects.
	SourceID string `json:"source_id"`
	// WrittenAt is when the file was last rewritten. Informational; nothing reads it back.
	WrittenAt time.Time `json:"written_at"`
	// LastCheckpoint is the extent the last checkpoint reached.
	LastCheckpoint *time.Time `json:"last_checkpoint"`
	// Objects is what the feeder had asserted at that checkpoint, sorted.
	Objects []KnownObject `json:"objects"`
}

// Start is the marker body this state produces.
func (s State) Start() StartState {
	return StartState{PreviousCheckpoint: s.LastCheckpoint, KnownObjects: s.Objects}
}

// StateStore is where a running feeder writes what it has asserted. Nil means nowhere: a
// replay has no state to keep, and neither does a test.
type StateStore interface {
	// Save replaces the stored state. It is called after every checkpoint, so it must be
	// cheap and it must never leave a half-written file behind.
	Save(State) error
}

// FileStateStore keeps `state.json` in one directory.
type FileStateStore struct {
	// Dir is the directory the file lives in. It is created on the first Save.
	Dir string
}

var _ StateStore = FileStateStore{}

// Save writes the state atomically: a temporary file in the same directory, then a rename. A
// feeder killed mid-write leaves either the old file or the new one, never a truncated one
// that its successor would read as "I knew nothing".
func (f FileStateStore) Save(s State) error {
	if s.Objects == nil {
		s.Objects = []KnownObject{}
	}
	s.Version = StateVersion
	s.Objects = sortObjects(s.Objects)
	body, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("k8s: encode %s: %w", StateFile, err)
	}
	body = append(body, '\n')
	if err := os.MkdirAll(f.Dir, 0o700); err != nil {
		return fmt.Errorf("k8s: create the state directory: %w", err)
	}
	tmp, err := os.CreateTemp(f.Dir, StateFile+".*")
	if err != nil {
		return fmt.Errorf("k8s: create a temporary state file: %w", err)
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("k8s: write %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("k8s: write %s: %w", name, err)
	}
	if err := os.Chmod(name, 0o600); err != nil {
		return fmt.Errorf("k8s: set the mode of %s: %w", name, err)
	}
	if err := os.Rename(name, filepath.Join(f.Dir, StateFile)); err != nil {
		return fmt.Errorf("k8s: replace %s: %w", StateFile, err)
	}
	return nil
}

// LoadState reads `state.json` from dir. A directory with no state file is a first run, not an
// error: the zero State says "this source has never checkpointed and knows of no objects",
// which is exactly what a feeder starting for the first time should assert.
func LoadState(dir, sourceID string) (State, error) {
	raw, err := os.ReadFile(filepath.Join(dir, StateFile)) //nolint:gosec // a path the operator named
	if errors.Is(err, os.ErrNotExist) {
		return State{Version: StateVersion, SourceID: sourceID}, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("k8s: read %s: %w", filepath.Join(dir, StateFile), err)
	}
	var s State
	if err := json.Unmarshal(raw, &s); err != nil {
		return State{}, fmt.Errorf("k8s: decode %s: %w", filepath.Join(dir, StateFile), err)
	}
	if s.Version != StateVersion {
		return State{}, fmt.Errorf(
			"k8s: %s was written by a feeder using state version %d, this one understands %d; "+
				"delete the file to start from an empty state and accept one missed reconciliation",
			filepath.Join(dir, StateFile), s.Version, StateVersion)
	}
	if s.SourceID != "" && sourceID != "" && s.SourceID != sourceID {
		return State{}, fmt.Errorf(
			"k8s: %s holds the state of source %q and this feeder is %q; "+
				"--state-dir must be per source, or one cluster's objects will be retracted from another's graph",
			filepath.Join(dir, StateFile), s.SourceID, sourceID)
	}
	s.SourceID = sourceID
	return s, nil
}

// DefaultStateDir is `$XDG_STATE_HOME/sre-agent/feeders/<source id>`, falling back to
// `~/.local/state` as the XDG base directory specification says.
//
// The source id is part of the path because the state is per source: two feeders watching two
// clusters must not reconcile against each other's objects, and making that a directory rather
// than a key inside one file means the mistake is visible in `ls`.
func DefaultStateDir(sourceID string) (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("k8s: no XDG_STATE_HOME and no home directory: %w", err)
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "sre-agent", "feeders", stateDirName(sourceID)), nil
}

// stateDirName makes a source id safe to use as one path segment. A source id is
// `<kind>:<name>` by convention, and a colon is legal on every filesystem this runs on, but a
// slash is not.
func stateDirName(sourceID string) string {
	if sourceID == "" {
		return "unnamed"
	}
	out := []rune(sourceID)
	for i, r := range out {
		switch r {
		case '/', '\\':
			out[i] = '_'
		}
	}
	return string(out)
}
