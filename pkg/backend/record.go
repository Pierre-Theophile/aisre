// SPDX-License-Identifier: Apache-2.0

package backend

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
)

// The world on disk (tasks.md T038; FR-036, FR-037; data-model §"The recording on disk";
// contracts/incident-format.md).
//
//	world/
//	├── index.json       # algebra version, hop radius, drill-down depth, window grid,
//	│                    #   term_key → file, term count, not_recorded count, miss rate,
//	│                    #   redaction policy version, index digest
//	└── <term_key>.json  # one recorded answer per term
//
// A world is a **map**, not a sequence. Layer 1 (the trajectory) is a run and is inherently
// ordered; layer 2 is a function from a question to an answer and is inherently keyed. Keeping
// them apart is what lets a replayed investigation take a different reasoning path and still be
// served — the whole claim of US3.
//
// Two disciplines make a world trustworthy.
//
// **The index digest is checked on load.** A world whose files were edited by hand, or whose
// index and files disagree, is refused rather than replayed: a recording that can drift is a
// recording that proves nothing. The digest covers the sorted (term key → file, response
// digest) triples, so adding, removing or altering any answer moves it.
//
// **Recording the same term twice with different answers is an error.** A key with two values is
// a world that cannot be replayed deterministically, and the second write is far more likely to
// be a bug in the recorder's enumeration than a real disagreement.

// IndexFile is the world's index, relative to the world directory.
const IndexFile = "index.json"

// WorldIndex is the on-disk index. It is the published WorldIndex message plus the digest that
// binds it to the files it names; JSON field names are the canonical protobuf spellings so that
// a reviewer reads the same names in the proto, the index and a `--output json` rendering.
type WorldIndex struct {
	// AlgebraVersion is the algebra the world was recorded against. A world of another version
	// is refused rather than reinterpreted.
	AlgebraVersion string `json:"algebraVersion"`
	// HopRadius is how far from the focus the neighbourhood was taken.
	HopRadius uint32 `json:"hopRadius"`
	// DrillDownDepth is how deep the minted handles were followed. Worlds record depth 1;
	// deeper is NOT_RECORDED by design, and the depth is written down so a fixture states its
	// own limit rather than hiding it.
	DrillDownDepth uint32 `json:"drillDownDepth"`
	// WindowGrid is the before/after pairs the cross product was taken over.
	WindowGrid []json.RawMessage `json:"windowGrid,omitempty"`
	// TermKeyToFile maps each recorded term key to its file, relative to the world directory.
	TermKeyToFile map[string]string `json:"termKeyToFile"`
	// TermCount is how many answers the world holds.
	TermCount uint32 `json:"termCount"`
	// NotRecordedCount is how many in-algebra requests this world answered NOT_RECORDED at
	// the time the index was written. A recorder writes zero; the verifier updates it.
	NotRecordedCount uint32 `json:"notRecordedCount"`
	// MissRate is NotRecordedCount / (TermCount + NotRecordedCount), rounded to six decimals.
	MissRate float64 `json:"missRate"`
	// RedactionPolicyVersion is what was applied to every answer in this world.
	RedactionPolicyVersion string `json:"redactionPolicyVersion"`
	// Focus is the node the neighbourhood was taken around, for a reader of the directory.
	Focus string `json:"focus,omitempty"`
	// LiveCoverage is the provenance of the terms a *live* investigator contributed to this
	// world, and is absent from a world recorded without one. A world recorded only from the
	// deterministic plan and the manifest grid holds the questions those two derive; a live
	// model asks others, and a fixture whose world does not hold them is excluded on its miss
	// rate rather than scored. Recording who widened the world, how many times and under which
	// model configuration is what lets a reviewer tell a world that covers a live run from one
	// that merely got lucky.
	LiveCoverage *LiveCoverage `json:"liveCoverage,omitempty"`
	// IndexDigest binds the index to the files it names. It is computed over every other field
	// and over each file's own response digest, and checked on load.
	IndexDigest string `json:"indexDigest"`
}

// LiveCoverage is the live-investigator provenance of a world.
//
// TermKeys is the load-bearing field: it names every answer a live pass filed, so a later
// re-record with no live pass at all can re-ask exactly those terms against the same
// deterministic generator and carry the coverage forward. Without it a `--live-passes 0`
// re-record would drop everything the live passes bought, because a recorder deletes the answers
// its enumeration did not visit.
type LiveCoverage struct {
	// Passes is how many live engine runs have contributed to this world, cumulatively across
	// recordings. A re-record with no live pass carries the count forward unchanged.
	Passes uint32 `json:"passes"`
	// ModelConfigDigests are the model configurations those passes ran under, sorted and
	// distinct. More than one means the coverage was accumulated across a configuration change,
	// which is a thing a reviewer should be told rather than have averaged away.
	ModelConfigDigests []string `json:"modelConfigDigests,omitempty"`
	// TermKeys are the recorded term keys the live passes filed, sorted and distinct.
	TermKeys []string `json:"termKeys,omitempty"`
}

// normalised returns the coverage in the one order a world is written in. A world's bytes must
// not depend on the order two passes happened to ask their terms in.
func (c *LiveCoverage) normalised() *LiveCoverage {
	if c == nil {
		return nil
	}
	return &LiveCoverage{
		Passes:             c.Passes,
		ModelConfigDigests: sortedDistinct(c.ModelConfigDigests),
		TermKeys:           sortedDistinct(c.TermKeys),
	}
}

// keys is the coverage's term keys, nil-safe.
func (c *LiveCoverage) keys() []string {
	if c == nil {
		return nil
	}
	return c.TermKeys
}

func sortedDistinct(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, each := range in {
		if each == "" {
			continue
		}
		if _, dup := seen[each]; dup {
			continue
		}
		seen[each] = struct{}{}
		out = append(out, each)
	}
	sort.Strings(out)
	return out
}

// Proto renders the index as the published WorldIndex message, for an RPC or a decision record.
func (w *WorldIndex) Proto() (*investigationv1.WorldIndex, error) {
	grid := make([]*investigationv1.WindowPair, 0, len(w.WindowGrid))
	for i, raw := range w.WindowGrid {
		pair := &investigationv1.WindowPair{}
		if err := protojson.Unmarshal(raw, pair); err != nil {
			return nil, fmt.Errorf("backend: world index: window grid entry %d: %w", i, err)
		}
		grid = append(grid, pair)
	}
	return &investigationv1.WorldIndex{
		AlgebraVersion:         w.AlgebraVersion,
		HopRadius:              w.HopRadius,
		DrillDownDepth:         w.DrillDownDepth,
		WindowGrid:             grid,
		TermKeyToFile:          w.TermKeyToFile,
		TermCount:              w.TermCount,
		NotRecordedCount:       w.NotRecordedCount,
		MissRate:               w.MissRate,
		RedactionPolicyVersion: w.RedactionPolicyVersion,
	}, nil
}

// RedactionPolicy is the policy the world was recorded under, as a declaration a backend can
// return from Describe. Only the version is recorded on disk: a world states what was applied to
// it, not how the applying was configured, because the configuration is the backend's and the
// statement is the recording's.
func (w *WorldIndex) RedactionPolicy() *investigationv1.RedactionPolicy {
	return &investigationv1.RedactionPolicy{
		LogBodiesAsTemplates: true,
		PolicyVersion:        w.RedactionPolicyVersion,
	}
}

// RecordOptions is the shape of the world a recorder is about to write. Every field lands in
// the index, because a fixture that does not state its own shape cannot be reviewed.
type RecordOptions struct {
	// HopRadius is the neighbourhood radius the cross product was taken over.
	HopRadius uint32
	// DrillDownDepth is how deep minted handles were followed; worlds record 1.
	DrillDownDepth uint32
	// WindowGrid is the before/after pairs of the cross product.
	WindowGrid []*investigationv1.WindowPair
	// RedactionPolicyVersion is what was applied to every answer.
	RedactionPolicyVersion string
	// Focus is the node reference the neighbourhood was taken around.
	Focus string
	// LiveCoverage is the live-investigator provenance to write into the index. Nil for a world
	// no live pass contributed to, which is what keeps such a world's bytes unchanged.
	LiveCoverage *LiveCoverage
}

type recorder struct {
	dir  string
	opts RecordOptions

	mu      sync.Mutex
	entries map[string]recordedEntry
	closed  bool
}

var _ LiveCoverageRecorder = (*recorder)(nil)

type recordedEntry struct {
	file           string
	responseDigest string
	canonical      []byte
}

// NewRecorder returns a Recorder writing into dir in the published layout. The directory is
// created if it does not exist; an existing world in it is replaced wholesale when Close runs,
// so a re-record cannot leave a stale answer behind under a key the new run did not visit.
func NewRecorder(dir string) (Recorder, error) {
	return NewRecorderWithOptions(dir, RecordOptions{DrillDownDepth: 1})
}

// NewRecorderWithOptions returns a Recorder that also writes the world's shape into the index.
func NewRecorderWithOptions(dir string, opts RecordOptions) (Recorder, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("backend: NewRecorder needs a directory to write the world into")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("backend: create %s: %w", dir, err)
	}
	if opts.RedactionPolicyVersion == "" {
		return nil, Reject(ReasonUndeclaredRedaction,
			"a world recorded without a redaction policy version; what was applied to a fixture must stay knowable after the policy changes")
	}
	return &recorder{dir: dir, opts: opts, entries: make(map[string]recordedEntry)}, nil
}

// Record writes one answer into the world, keyed by the request's canonicalised term.
func (r *recorder) Record(_ context.Context, req *AlgebraRequest, resp *AlgebraResponse) error {
	if req == nil || resp == nil {
		return fmt.Errorf("backend: Record needs both a request and a response")
	}
	key, err := TermKey(req.GetTerm())
	if err != nil {
		return err
	}
	if FamilyOf(TermNameOf(req.GetTerm())) == FamilyGraph {
		return fmt.Errorf(
			"backend: refusing to record graph term %s: the graph family is answered by replaying events.jsonl, and a second source of truth for a deterministic answer is a source of drift",
			TermNameOf(req.GetTerm()))
	}
	if resp.GetOutcome() == investigationv1.TermOutcome_NOT_RECORDED {
		return fmt.Errorf(
			"backend: refusing to record a NOT_RECORDED answer for %s; NOT_RECORDED is what a world says about a term it does not hold, never a term it holds",
			TermNameOf(req.GetTerm()))
	}

	stored := proto.Clone(resp).(*AlgebraResponse) //nolint:errcheck // proto.Clone returns the dynamic type it was given
	stored.TermKey = key
	// The three fields the digest is blind to are cleared on the way to disk as well as on the
	// way to the hash, so a world file holds no duration a re-record would change and no mode
	// that would contradict the reader's own. NormaliseForDigest is the one definition of which
	// three they are.
	NormaliseForDigest(stored)
	digest, err := ResponseDigest(stored)
	if err != nil {
		return err
	}
	stored.ResponseDigest = digest

	record := &worldRecord{Term: req.GetTerm(), Response: stored}
	canonical, err := record.canonical()
	if err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return fmt.Errorf("backend: Record after Close")
	}
	if existing, ok := r.entries[key]; ok {
		if existing.responseDigest == digest {
			return nil // the same answer under the same key is the enumeration visiting twice
		}
		return fmt.Errorf(
			"backend: term %s (%s) was recorded twice with different answers (%s, then %s); a world is a map, and a key with two values cannot be replayed deterministically",
			TermNameOf(req.GetTerm()), key, existing.responseDigest, digest)
	}
	r.entries[key] = recordedEntry{file: key + ".json", responseDigest: digest, canonical: canonical}
	return nil
}

// DeclareLiveCoverage records which of this world's answers a live investigator asked for.
func (r *recorder) DeclareLiveCoverage(coverage *LiveCoverage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.opts.LiveCoverage = coverage
}

// Close writes world/index.json and every answer, and returns the index digest.
func (r *recorder) Close(_ context.Context) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return "", fmt.Errorf("backend: Close called twice")
	}
	r.closed = true

	if err := removeStaleWorld(r.dir, r.entries); err != nil {
		return "", err
	}
	keys := make([]string, 0, len(r.entries))
	for key := range r.entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	index := &WorldIndex{
		AlgebraVersion:         AlgebraVersion,
		HopRadius:              r.opts.HopRadius,
		DrillDownDepth:         r.opts.DrillDownDepth,
		TermKeyToFile:          make(map[string]string, len(keys)),
		TermCount:              uint32(len(keys)),
		RedactionPolicyVersion: r.opts.RedactionPolicyVersion,
		Focus:                  r.opts.Focus,
		LiveCoverage:           r.opts.LiveCoverage.normalised(),
	}
	// A world that claims live coverage it does not hold is worse than one that claims none: the
	// claim is what a later re-record carries forward, and carrying forward a key with no answer
	// behind it would fail on the next load instead of here.
	for _, key := range index.LiveCoverage.keys() {
		if _, held := r.entries[key]; !held {
			return "", fmt.Errorf(
				"backend: the world's live coverage names term %s, which this recording did not file; a world cannot declare coverage it does not hold", key)
		}
	}
	for _, pair := range r.opts.WindowGrid {
		encoded, err := graph.CanonicalJSON(pair)
		if err != nil {
			return "", err
		}
		index.WindowGrid = append(index.WindowGrid, encoded)
	}
	digests := make([]string, 0, len(keys))
	for _, key := range keys {
		entry := r.entries[key]
		index.TermKeyToFile[key] = entry.file
		digests = append(digests, key+":"+entry.responseDigest)
		path := filepath.Join(r.dir, entry.file)
		if err := os.WriteFile(path, append(entry.canonical, '\n'), 0o644); err != nil {
			return "", fmt.Errorf("backend: write %s: %w", path, err)
		}
	}

	digest, err := index.digestOver(digests)
	if err != nil {
		return "", err
	}
	index.IndexDigest = digest
	if err := writeIndex(r.dir, index); err != nil {
		return "", err
	}
	return digest, nil
}

// removeStaleWorld deletes the answers a previous recording left behind that this one did not
// visit. Without it a re-record would be a merge of two worlds, and "a re-record of an unchanged
// fixture is byte-identical" would hold only by luck.
func removeStaleWorld(dir string, entries map[string]recordedEntry) error {
	listing, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("backend: read %s: %w", dir, err)
	}
	for _, item := range listing {
		if item.IsDir() || !strings.HasSuffix(item.Name(), ".json") || item.Name() == IndexFile {
			continue
		}
		key := strings.TrimSuffix(item.Name(), ".json")
		if _, kept := entries[key]; kept {
			continue
		}
		if err := os.Remove(filepath.Join(dir, item.Name())); err != nil {
			return fmt.Errorf("backend: remove stale %s: %w", item.Name(), err)
		}
	}
	return nil
}

func writeIndex(dir string, index *WorldIndex) error {
	encoded, err := json.Marshal(index)
	if err != nil {
		return fmt.Errorf("backend: encode world index: %w", err)
	}
	canonical, err := canonicalJSONBytes(encoded)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, IndexFile)
	if err := os.WriteFile(path, append(canonical, '\n'), 0o644); err != nil {
		return fmt.Errorf("backend: write %s: %w", path, err)
	}
	return nil
}

// digestOver computes the index digest: sha256 over the index's own shape and the sorted
// (term key, response digest) pairs it names.
func (w *WorldIndex) digestOver(entryDigests []string) (string, error) {
	shape := *w
	shape.IndexDigest = ""
	shape.TermKeyToFile = nil
	encoded, err := json.Marshal(shape)
	if err != nil {
		return "", fmt.Errorf("backend: world index digest: %w", err)
	}
	canonical, err := canonicalJSONBytes(encoded)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write(canonical)
	h.Write([]byte("\n"))
	for _, entry := range entryDigests {
		h.Write([]byte(entry))
		h.Write([]byte("\n"))
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func canonicalJSONBytes(raw []byte) ([]byte, error) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("backend: canonicalise index: %w", err)
	}
	return graph.CanonicalJSON(value)
}

// worldRecord is what one world/<term_key>.json file holds: the term as asked and the answer as
// given. The term is stored as well as hashed so that a reviewer reading a diff can see what
// question an answer belongs to without recomputing a hash.
type worldRecord struct {
	Term     *AlgebraTerm     `json:"-"`
	Response *AlgebraResponse `json:"-"`
}

type worldRecordJSON struct {
	Term     json.RawMessage `json:"term"`
	Response json.RawMessage `json:"response"`
}

func (r *worldRecord) canonical() ([]byte, error) {
	normalised, err := Normalise(r.Term)
	if err != nil {
		return nil, err
	}
	term, err := graph.CanonicalJSON(normalised)
	if err != nil {
		return nil, err
	}
	resp, err := graph.CanonicalJSON(r.Response)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(worldRecordJSON{Term: term, Response: resp})
	if err != nil {
		return nil, fmt.Errorf("backend: encode world record: %w", err)
	}
	return canonicalJSONBytes(encoded)
}

// World is a loaded recording: the index, checked, plus the answers it names.
type World struct {
	// Dir is where the world was read from.
	Dir string
	// Index is the checked index.
	Index *WorldIndex

	answers map[string]*AlgebraResponse
	terms   map[string]*AlgebraTerm
}

// LoadWorld reads dir, verifies the index digest against the files it names, and returns the
// world. A world whose index and files disagree is refused: a recording that can drift is a
// recording that proves nothing.
func LoadWorld(dir string) (*World, error) {
	raw, err := os.ReadFile(filepath.Join(dir, IndexFile))
	if err != nil {
		return nil, fmt.Errorf("backend: read world index: %w", err)
	}
	index := &WorldIndex{}
	if err := json.Unmarshal(raw, index); err != nil {
		return nil, fmt.Errorf("backend: parse %s: %w", filepath.Join(dir, IndexFile), err)
	}
	if index.AlgebraVersion != AlgebraVersion {
		return nil, Reject(ReasonUnknownAlgebraVersion,
			"world %s was recorded against algebra version %s; this build publishes %s, and a world of another version is refused rather than reinterpreted",
			dir, index.AlgebraVersion, AlgebraVersion)
	}

	world := &World{
		Dir:     dir,
		Index:   index,
		answers: make(map[string]*AlgebraResponse, len(index.TermKeyToFile)),
		terms:   make(map[string]*AlgebraTerm, len(index.TermKeyToFile)),
	}
	keys := make([]string, 0, len(index.TermKeyToFile))
	for key := range index.TermKeyToFile {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	digests := make([]string, 0, len(keys))
	for _, key := range keys {
		path := filepath.Join(dir, index.TermKeyToFile[key])
		body, err := os.ReadFile(path) //nolint:gosec // the path comes from the index this world published
		if err != nil {
			return nil, fmt.Errorf("backend: world %s names %s, which is not there: %w", dir, index.TermKeyToFile[key], err)
		}
		var record worldRecordJSON
		if err := json.Unmarshal(body, &record); err != nil {
			return nil, fmt.Errorf("backend: parse %s: %w", path, err)
		}
		term := &AlgebraTerm{}
		if err := protojson.Unmarshal(record.Term, term); err != nil {
			return nil, fmt.Errorf("backend: parse term in %s: %w", path, err)
		}
		resp := &AlgebraResponse{}
		if err := protojson.Unmarshal(record.Response, resp); err != nil {
			return nil, fmt.Errorf("backend: parse response in %s: %w", path, err)
		}
		if resp.GetTermKey() != key {
			return nil, fmt.Errorf(
				"backend: %s is filed under %s but its response names term key %s; the world's index and its files disagree",
				path, key, resp.GetTermKey())
		}
		// Recompute the answer's own digest from what is on disk. The index digest binds the
		// *claimed* digests; this binds each claim to its content, so editing an answer by hand
		// without also recomputing its digest — which is what a hand edit always looks like —
		// is caught here rather than replayed as though somebody had recorded it.
		recomputed, err := ResponseDigest(resp)
		if err != nil {
			return nil, err
		}
		if resp.GetResponseDigest() != "" && resp.GetResponseDigest() != recomputed {
			return nil, fmt.Errorf(
				"backend: %s claims response digest %s but its content hashes to %s; a recording that can drift is a recording that proves nothing",
				path, resp.GetResponseDigest(), recomputed)
		}
		world.answers[key] = resp
		world.terms[key] = term
		digests = append(digests, key+":"+resp.GetResponseDigest())
	}

	want, err := index.digestOver(digests)
	if err != nil {
		return nil, err
	}
	if index.IndexDigest != "" && index.IndexDigest != want {
		return nil, fmt.Errorf(
			"backend: world %s has index digest %s but its files hash to %s; a recording that can drift is a recording that proves nothing",
			dir, index.IndexDigest, want)
	}
	return world, nil
}

// Answer returns the recorded answer for a term key and whether the world holds it.
func (w *World) Answer(key string) (*AlgebraResponse, bool) {
	resp, ok := w.answers[key]
	if !ok {
		return nil, false
	}
	return proto.Clone(resp).(*AlgebraResponse), true //nolint:errcheck // proto.Clone returns the dynamic type it was given
}

// Term returns the term recorded under a key, for rendering a miss or a diff.
func (w *World) Term(key string) (*AlgebraTerm, bool) {
	term, ok := w.terms[key]
	if !ok {
		return nil, false
	}
	return proto.Clone(term).(*AlgebraTerm), true //nolint:errcheck // proto.Clone returns the dynamic type it was given
}

// Keys returns the term keys the world holds, sorted.
func (w *World) Keys() []string {
	keys := make([]string, 0, len(w.answers))
	for key := range w.answers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// Len is how many answers the world holds.
func (w *World) Len() int { return len(w.answers) }
