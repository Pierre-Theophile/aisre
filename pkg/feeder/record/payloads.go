// SPDX-License-Identifier: Apache-2.0

package record

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/source"
)

// Recording payloads.
//
// Each payload is written to `<dir>/payloads/<kind>/NNNNNN.<ext>` — `.json` when the bytes
// parse as JSON, `.pb` otherwise — and a line is appended to `<dir>/payloads/index.jsonl`
// naming the file, its kind, its arrival time and its sequence number. The index is what makes
// the replay faithful: the file names alone would give the order within a kind, but not the
// interleaving between kinds, and not the arrival times a shuffle check permutes within.
//
// Files are numbered per kind from 1 and zero-padded to six digits so that a directory listing
// is also the replay order, which is what a payload set assembled by hand relies on.

// payloadDigits is the width of a payload's file number. Six digits is a million payloads per
// kind, well past the size at which a fixture stops being reviewable.
const payloadDigits = 6

// PayloadRecorder is a Source that saves everything it passes through.
type PayloadRecorder struct {
	inner feeder.Source
	dir   string

	mu       sync.Mutex
	counts   map[string]int
	total    int
	first    time.Time
	last     time.Time
	err      error
	prepared bool
}

var _ feeder.Source = (*PayloadRecorder)(nil)

// Wrap tees src's payloads into `<dir>/payloads/`, returning a Source a feeder can be run
// against unchanged.
//
// The directory is created on the first payload, not here: a recorder that is never read
// should leave nothing behind, and a feeder that fails at startup should not produce an empty
// fixture. A write failure surfaces from Next — the payload is still returned, because losing
// the run as well as the recording helps nobody, and the error is reported on the following
// call.
func Wrap(src feeder.Source, dir string) *PayloadRecorder {
	return &PayloadRecorder{inner: src, dir: dir, counts: map[string]int{}}
}

// Writer returns a recorder with no source of its own, for a caller that decides what reaches disk
// payload by payload — the sanitising tee, which hands the live feeder one payload and writes another
// (004 T104). Record is how it writes.
func Writer(dir string) *PayloadRecorder {
	return &PayloadRecorder{dir: dir, counts: map[string]int{}}
}

// Record writes one payload to the recording exactly as given.
func (r *PayloadRecorder) Record(p feeder.Payload) error { return r.write(p) }

// Next returns the next payload, having written it to the recording.
func (r *PayloadRecorder) Next(ctx context.Context) (feeder.Payload, error) {
	p, err := r.inner.Next(ctx)
	if err != nil {
		return p, err
	}
	if writeErr := r.write(p); writeErr != nil {
		return p, writeErr
	}
	return p, nil
}

// Count is how many payloads have been recorded.
func (r *PayloadRecorder) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.total
}

// Extent is the arrival time of the first and last recorded payload, which is what a fixture
// manifest records as its clock. Both are zero when nothing carried a time.
func (r *PayloadRecorder) Extent() (first, last time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.first, r.last
}

// Err returns the first write failure, or nil.
func (r *PayloadRecorder) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

func (r *PayloadRecorder) write(p feeder.Payload) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	kind := sanitizeSegment(p.Kind)
	root := filepath.Join(r.dir, source.PayloadsDir)
	if !r.prepared {
		if err := os.MkdirAll(root, 0o755); err != nil {
			return r.fail(fmt.Errorf("record: create %s: %w", root, err))
		}
		r.prepared = true
	}
	kindDir := filepath.Join(root, kind)
	if err := os.MkdirAll(kindDir, 0o755); err != nil {
		return r.fail(fmt.Errorf("record: create %s: %w", kindDir, err))
	}

	// A name is never reused. Several sources recording into one directory — a cross-source fixture
	// is exactly that — each number their own payloads from one, and two feeders that both call a
	// payload `deployments` would otherwise write the same file: the second recording silently
	// replaced the first's bytes while the index kept both entries, so the index named one file twice
	// and one platform's answer was gone. `O_EXCL` makes the collision visible to this loop, which
	// moves on to the next free number instead (004 T113 found it in deploy-cross-source-merge-01).
	var name string
	for {
		r.counts[kind]++
		name = fmt.Sprintf("%0*d%s", payloadDigits, r.counts[kind], payloadExt(p.Bytes))
		err := writeNew(filepath.Join(kindDir, name), p.Bytes)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrExist) {
			return r.fail(fmt.Errorf("record: write payload %s/%s: %w", kind, name, err))
		}
	}

	entry := source.IndexEntry{Kind: kind, At: p.At.UTC(), Seq: p.Seq, File: kind + "/" + name}
	line, err := json.Marshal(entry)
	if err != nil {
		return r.fail(fmt.Errorf("record: encode index entry: %w", err))
	}
	if err := appendLine(filepath.Join(root, source.IndexFile), line); err != nil {
		return r.fail(err)
	}

	r.total++
	if !p.At.IsZero() {
		at := p.At.UTC()
		if r.first.IsZero() || at.Before(r.first) {
			r.first = at
		}
		if at.After(r.last) {
			r.last = at
		}
	}
	return nil
}

func (r *PayloadRecorder) fail(err error) error {
	if r.err == nil {
		r.err = err
	}
	return err
}

// payloadExt picks the extension by looking at the bytes rather than trusting the kind: a
// connector's "traces" payload is protobuf from one transport and JSON from another, and a
// file named `.json` that is not JSON makes the fixture unreadable to everything downstream.
func payloadExt(raw []byte) string {
	if json.Valid(raw) {
		return ".json"
	}
	return ".pb"
}

// sanitizeSegment makes a payload kind safe to use as a directory name. A kind is connector
// data, so it may not be allowed to escape the fixture directory.
func sanitizeSegment(kind string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-', r == '_', r == '.':
			return r
		default:
			return '_'
		}
	}, strings.TrimSpace(kind))
	cleaned = strings.Trim(cleaned, ".")
	if cleaned == "" {
		return "unknown"
	}
	return cleaned
}

// appendLine appends one line to a file, creating it if needed.
func appendLine(path string, line []byte) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644) //nolint:gosec // fixture files are world-readable by design
	if err != nil {
		return fmt.Errorf("record: open %s: %w", path, err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("record: write %s: %w", path, err)
	}
	return nil
}

// writeNew writes a payload file that must not already exist.
func writeNew(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) //nolint:gosec // fixture payloads are world-readable by design
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
