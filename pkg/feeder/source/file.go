// SPDX-License-Identifier: Apache-2.0

package source

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// Replaying a recording (FR-044, contracts/fixture-format.md).
//
// A fixture's `payloads/` directory holds what the source system actually sent, grouped by
// kind, plus an `index.jsonl` written by the recorder that says which file was which payload,
// when it arrived and what its sequence number was. Replaying in index order reproduces the
// arrival order exactly, which is what makes "the recorded mode is the test" a meaningful
// claim rather than an approximation.
//
// The index is not required. A payload set assembled by hand — curl output saved into
// `payloads/deployments/` — replays in filename order, which is why the recorder writes
// zero-padded names. Without an index there is no arrival time to replay, so `At` is zero and
// every payload falls inside every reordering window; a shuffle check over such a fixture is
// therefore stricter than the feeder promised, never weaker.

// PayloadsDir is the directory inside a fixture holding recorded payloads.
const PayloadsDir = "payloads"

// IndexFile is the recorder's manifest of payloads, one JSON object per line, in arrival
// order.
const IndexFile = "index.jsonl"

// IndexEntry is one line of a payloads index.
type IndexEntry struct {
	// Kind is the payload kind, which is also the directory File sits in.
	Kind string `json:"kind"`
	// At is when the source produced the payload.
	At time.Time `json:"at"`
	// Seq is the source-native sequence number, or 0.
	Seq int64 `json:"seq"`
	// File is the payload's path relative to the payloads directory, e.g.
	// "deployments/000001.json".
	File string `json:"file"`
}

// FileOption configures a FileSource.
type FileOption func(*fileOptions)

type fileOptions struct {
	kinds []string
}

// WithKinds restricts the replay to the named payload kinds. It is how a feeder that watches
// several resource types is tested against one of them at a time.
func WithKinds(kinds ...string) FileOption {
	return func(o *fileOptions) { o.kinds = append(o.kinds, kinds...) }
}

// FileSource replays recorded payloads in arrival order.
type FileSource struct {
	dir      string
	payloads []feeder.Payload

	mu   sync.Mutex
	next int
}

var _ feeder.Source = (*FileSource)(nil)

// NewFileSource opens the recorded payloads under dir.
//
// dir is the fixture directory; `<dir>/payloads` is read when it exists, and dir itself
// otherwise, so that both a fixture root and a bare payloads directory work.
func NewFileSource(dir string, opts ...FileOption) (*FileSource, error) {
	root := PayloadsRoot(dir)
	payloads, err := ReadPayloads(dir, opts...)
	if err != nil {
		return nil, err
	}
	return &FileSource{dir: root, payloads: payloads}, nil
}

// PayloadsRoot resolves the directory payloads are read from: `<dir>/payloads` when it exists,
// dir otherwise.
func PayloadsRoot(dir string) string {
	nested := filepath.Join(dir, PayloadsDir)
	if info, err := os.Stat(nested); err == nil && info.IsDir() {
		return nested
	}
	return dir
}

// Next returns the next payload, or io.EOF when the recording is exhausted.
func (s *FileSource) Next(ctx context.Context) (feeder.Payload, error) {
	if err := ctx.Err(); err != nil {
		return feeder.Payload{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.next >= len(s.payloads) {
		return feeder.Payload{}, io.EOF
	}
	p := s.payloads[s.next]
	s.next++
	return p, nil
}

// Payloads returns every payload the source will replay, in order. It is what a harness
// permutes to check order independence.
func (s *FileSource) Payloads() []feeder.Payload {
	return append([]feeder.Payload(nil), s.payloads...)
}

// Len is how many payloads were read.
func (s *FileSource) Len() int { return len(s.payloads) }

// Dir is the directory the payloads were read from.
func (s *FileSource) Dir() string { return s.dir }

// Reset rewinds to the first payload, so one FileSource can drive two runs.
func (s *FileSource) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next = 0
}

// ReadPayloads reads every recorded payload under dir into memory, in replay order.
//
// Order comes from `index.jsonl` when the recorder wrote one. Otherwise kinds are walked in
// directory-name order and files within a kind in filename order, which is why the recorder
// zero-pads them. A `.jsonl` file without an index entry of its own is expanded one payload
// per line, the shape a hand-assembled watch-event capture usually takes.
func ReadPayloads(dir string, opts ...FileOption) ([]feeder.Payload, error) {
	var o fileOptions
	for _, opt := range opts {
		opt(&o)
	}
	root := PayloadsRoot(dir)
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("source: %s is not a payload directory: %w", root, err)
	}

	entries, err := readIndex(filepath.Join(root, IndexFile))
	if err != nil {
		return nil, err
	}
	if entries != nil {
		return payloadsFromIndex(root, entries, o)
	}
	return payloadsFromWalk(root, o)
}

// readIndex reads index.jsonl, returning nil when the recording has none.
func readIndex(path string) ([]IndexEntry, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // a fixture path supplied by the caller
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("source: read %s: %w", path, err)
	}
	var entries []IndexEntry
	for i, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var entry IndexEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			return nil, fmt.Errorf("source: %s line %d: %w", path, i+1, err)
		}
		if entry.File == "" {
			return nil, fmt.Errorf("source: %s line %d: no file", path, i+1)
		}
		entries = append(entries, entry)
	}
	// An index that exists but is empty is still an index: the recording observed nothing.
	if entries == nil {
		entries = []IndexEntry{}
	}
	return entries, nil
}

func payloadsFromIndex(root string, entries []IndexEntry, o fileOptions) ([]feeder.Payload, error) {
	payloads := make([]feeder.Payload, 0, len(entries))
	for _, entry := range entries {
		if !wanted(entry.Kind, o) {
			continue
		}
		path := filepath.Join(root, filepath.FromSlash(entry.File))
		raw, err := os.ReadFile(path) //nolint:gosec // a fixture path named by the index
		if err != nil {
			return nil, fmt.Errorf("source: read payload %s: %w", entry.File, err)
		}
		payloads = append(payloads, feeder.Payload{
			Kind:  entry.Kind,
			At:    entry.At.UTC(),
			Seq:   entry.Seq,
			Bytes: raw,
		})
	}
	return payloads, nil
}

func payloadsFromWalk(root string, o fileOptions) ([]feeder.Payload, error) {
	dirEntries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("source: read %s: %w", root, err)
	}
	kinds := make([]string, 0, len(dirEntries))
	for _, entry := range dirEntries {
		if entry.IsDir() && wanted(entry.Name(), o) {
			kinds = append(kinds, entry.Name())
		}
	}
	slices.Sort(kinds)

	var payloads []feeder.Payload
	for _, kind := range kinds {
		kindDir := filepath.Join(root, kind)
		files, err := os.ReadDir(kindDir)
		if err != nil {
			return nil, fmt.Errorf("source: read %s: %w", kindDir, err)
		}
		names := make([]string, 0, len(files))
		for _, file := range files {
			if !file.IsDir() {
				names = append(names, file.Name())
			}
		}
		slices.Sort(names)
		for _, name := range names {
			path := filepath.Join(kindDir, name)
			raw, err := os.ReadFile(path) //nolint:gosec // a fixture path under the caller's directory
			if err != nil {
				return nil, fmt.Errorf("source: read payload %s: %w", path, err)
			}
			if strings.EqualFold(filepath.Ext(name), ".jsonl") {
				for _, line := range bytes.Split(raw, []byte("\n")) {
					if len(bytes.TrimSpace(line)) == 0 {
						continue
					}
					payloads = append(payloads, feeder.Payload{Kind: kind, Bytes: slices.Clone(line)})
				}
				continue
			}
			payloads = append(payloads, feeder.Payload{Kind: kind, Bytes: raw})
		}
	}
	return payloads, nil
}

func wanted(kind string, o fileOptions) bool {
	return len(o.kinds) == 0 || slices.Contains(o.kinds, kind)
}

// SliceSource replays payloads a caller already holds. It is what a harness builds after
// permuting a FileSource's payloads, and what a feeder's own unit test uses for a handful of
// hand-written payloads.
type SliceSource struct {
	mu       sync.Mutex
	payloads []feeder.Payload
	next     int
}

var _ feeder.Source = (*SliceSource)(nil)

// NewSliceSource returns a Source over payloads, in the order given.
func NewSliceSource(payloads []feeder.Payload) *SliceSource {
	return &SliceSource{payloads: append([]feeder.Payload(nil), payloads...)}
}

// Next returns the next payload, or io.EOF.
func (s *SliceSource) Next(ctx context.Context) (feeder.Payload, error) {
	if err := ctx.Err(); err != nil {
		return feeder.Payload{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.next >= len(s.payloads) {
		return feeder.Payload{}, io.EOF
	}
	p := s.payloads[s.next]
	s.next++
	return p, nil
}

// Len is how many payloads remain to be replayed in total.
func (s *SliceSource) Len() int { return len(s.payloads) }

// Reset rewinds to the first payload.
func (s *SliceSource) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next = 0
}
