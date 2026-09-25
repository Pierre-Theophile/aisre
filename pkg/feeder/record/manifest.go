// SPDX-License-Identifier: Apache-2.0

package record

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/source"
)

// The manifest a recording produces (contracts/fixture-format.md §manifest.yaml).
//
// WriteManifest writes the part a machine knows: which feeders produced the recording and
// under which guarantees, which files hold the events, what the clock window was, and which
// events must always be refused. What it cannot write is the part that makes a fixture worth
// having — the description of what the recording is *of*, and the queries whose answers are
// the golden outputs. Those are left for a human to add, and the file says so in a comment
// rather than inventing them.
//
// `hand_authored` is always false: a recording's events.jsonl was produced from payloads/ and
// may be regenerated from them, which is precisely what the flag is asked about.

// ManifestFile is the file WriteManifest writes.
const ManifestFile = "manifest.yaml"

// ManifestSource is one feeder's declared contract, as the manifest states it.
type ManifestSource struct {
	// SourceID is the feeder's identity, e.g. "k8s:demo".
	SourceID string
	// Kind is the connector family.
	Kind string
	// Ordering is "per_source_sequence" or "none".
	Ordering string
	// ReorderingWindow is how far out of order the feeder may deliver. It is written as a Go
	// duration, e.g. "60s".
	ReorderingWindow time.Duration
}

// SourceOf renders a feeder's Description as a manifest source, so that the fixture and the
// feeder can never disagree about the guarantees under which it was recorded.
func SourceOf(d feeder.Description) ManifestSource {
	return ManifestSource{
		SourceID:         d.SourceID,
		Kind:             d.Kind,
		Ordering:         d.Ordering.String(),
		ReorderingWindow: d.ReorderingWindow,
	}
}

// Manifest is what WriteManifest writes.
type Manifest struct {
	// ID is the fixture id and must equal the directory name.
	ID string
	// Family groups fixtures exercising the same behaviour, e.g. "baseline-topology".
	Family string
	// Description is the human summary shown in verification reports.
	Description string
	// SchemaVersion is the event schema version the recorded events declare. Empty means
	// feeder.SchemaVersion.
	SchemaVersion string
	// SDKVersion is the SDK the recording was made with. Empty means feeder.SDKVersion.
	SDKVersion string
	// Sources are the feeders that produced the recording. At least one is required.
	Sources []ManifestSource
	// Start and End bound the fixture's clock. Both zero means "derive them from the recorded
	// payloads", which is what a recording normally wants.
	Start, End time.Time
	// Events and RejectedEvents name the two event files. Empty means EventsFile and, when
	// `<dir>/rejected.jsonl` exists, RejectedFile.
	Events, RejectedEvents string
	// ExpectRejected states the events that must always be refused, and with which reason
	// code (SC-009). Build it from EventRecorder.Rejections.
	ExpectRejected []Rejection
}

// WriteManifest writes `<dir>/manifest.yaml`.
//
// The clock defaults to the range the recording actually covers, so a recording that ran for
// ninety minutes says so without anyone having to look. The end is what the observed-time-pinned
// golden pass pins to (US7), so an absent clock is not a cosmetic omission and WriteManifest
// refuses to leave it silently empty when the recording can answer it.
//
// The range is the union of the payloads' arrival instants AND the events' observed instants, and
// the second half is a fix (004 T153). It used to be the payloads alone, and a feeder emits its
// events after the payload that carried them has arrived — the last checkpoint is written after the
// stream has closed — so every live recording declared a window ending just before its own final
// events: by 50 milliseconds in `config-change-01`, 300 in `baseline-topology-01` and 1.1 seconds in
// `rollout-regression-02`. The pinned pass answers as known at clock.end, so those events silently
// fell out of every pinned answer. `fixture verify` now refuses a window that excludes its events,
// which is what found this, and this is what stops a live recording from producing one.
func WriteManifest(dir string, m Manifest) error {
	if m.ID == "" {
		m.ID = filepath.Base(filepath.Clean(dir))
	}
	if len(m.Sources) == 0 {
		return errors.New("record: a manifest needs at least one source, so every event has a declared origin")
	}
	if m.Start.IsZero() && m.End.IsZero() {
		start, end, err := payloadExtent(dir)
		if err != nil {
			return err
		}
		first, last, err := eventExtent(filepath.Join(dir, EventsFile))
		if err != nil {
			return err
		}
		if !first.IsZero() && (start.IsZero() || first.Before(start)) {
			start = first
		}
		if last.After(end) {
			end = last
		}
		m.Start, m.End = start, end
	}
	if m.Events == "" {
		m.Events = EventsFile
	}
	if m.RejectedEvents == "" {
		if _, err := os.Stat(filepath.Join(dir, RejectedFile)); err == nil {
			m.RejectedEvents = RejectedFile
		}
	}

	doc := manifestDoc{
		ID:             m.ID,
		Family:         m.Family,
		Description:    m.Description,
		SchemaVersion:  orDefault(m.SchemaVersion, feeder.SchemaVersion),
		SDKVersion:     orDefault(m.SDKVersion, feeder.SDKVersion),
		HandAuthored:   false,
		Events:         m.Events,
		RejectedEvents: m.RejectedEvents,
	}
	for _, src := range m.Sources {
		doc.Sources = append(doc.Sources, sourceDoc{
			SourceID:         src.SourceID,
			Kind:             src.Kind,
			Ordering:         orDefault(src.Ordering, string(feeder.OrderingNone)),
			ReorderingWindow: src.ReorderingWindow.String(),
		})
	}
	doc.Clock.Start = m.Start.UTC()
	doc.Clock.End = m.End.UTC()
	for _, r := range m.ExpectRejected {
		doc.ExpectRejected = append(doc.ExpectRejected, expectDoc{EventID: r.EventID, ReasonCode: r.ReasonCode})
	}

	body, err := yaml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("record: encode manifest: %w", err)
	}
	var out strings.Builder
	out.WriteString("# Recorded fixture. See fixtures/README.md and contracts/fixture-format.md.\n")
	out.WriteString("#\n")
	out.WriteString("# Add a `queries:` list naming the reads whose answers golden/ must hold, then run\n")
	out.WriteString("# `aisre fixture record` to write them and `aisre fixture verify` to check them.\n")
	out.Write(body)

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("record: create %s: %w", dir, err)
	}
	path := filepath.Join(dir, ManifestFile)
	if err := os.WriteFile(path, []byte(out.String()), 0o644); err != nil { //nolint:gosec // fixture files are world-readable by design
		return fmt.Errorf("record: write %s: %w", path, err)
	}
	return nil
}

// manifestDoc is the YAML shape. It exists separately from Manifest so that durations are
// written the way the loader reads them ("60s", not a nanosecond count) and so that the key
// order in the file is fixed rather than alphabetical.
type manifestDoc struct {
	ID             string      `yaml:"id"`
	Family         string      `yaml:"family,omitempty"`
	Description    string      `yaml:"description,omitempty"`
	SchemaVersion  string      `yaml:"schema_version"`
	SDKVersion     string      `yaml:"sdk_version"`
	HandAuthored   bool        `yaml:"hand_authored"`
	Events         string      `yaml:"events"`
	RejectedEvents string      `yaml:"rejected_events,omitempty"`
	Sources        []sourceDoc `yaml:"sources"`
	Clock          struct {
		Start time.Time `yaml:"start"`
		End   time.Time `yaml:"end"`
	} `yaml:"clock"`
	ExpectRejected []expectDoc `yaml:"expect_rejected,omitempty"`
}

type sourceDoc struct {
	SourceID         string `yaml:"source_id"`
	Kind             string `yaml:"kind"`
	Ordering         string `yaml:"ordering"`
	ReorderingWindow string `yaml:"reordering_window"`
}

type expectDoc struct {
	EventID    string `yaml:"event_id"`
	ReasonCode string `yaml:"reason_code"`
}

// payloadExtent reads the recorded payload index and returns the first and last arrival time.
// A recording with no index, or one whose payloads carried no times, yields two zero times and
// no error: the caller may still have a clock of its own.
func payloadExtent(dir string) (time.Time, time.Time, error) {
	path := filepath.Join(source.PayloadsRoot(dir), source.IndexFile)
	raw, err := os.ReadFile(path) //nolint:gosec // a fixture path supplied by the caller
	if err != nil {
		if os.IsNotExist(err) {
			return time.Time{}, time.Time{}, nil
		}
		return time.Time{}, time.Time{}, fmt.Errorf("record: read %s: %w", path, err)
	}
	var first, last time.Time
	for i, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var entry source.IndexEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("record: %s line %d: %w", path, i+1, err)
		}
		if entry.At.IsZero() {
			continue
		}
		at := entry.At.UTC()
		if first.IsZero() || at.Before(first) {
			first = at
		}
		if at.After(last) {
			last = at
		}
	}
	return first, last, nil
}

// eventExtent is the range of observed instants in a recorded events file, or zero when there is
// none. Only the log-added `observedAt` is read: the envelope itself is none of this function's
// business, and decoding it here would make a manifest writer fail on a schema change.
func eventExtent(path string) (time.Time, time.Time, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // a fixture path supplied by the caller
	if err != nil {
		if os.IsNotExist(err) {
			return time.Time{}, time.Time{}, nil
		}
		return time.Time{}, time.Time{}, fmt.Errorf("record: read %s: %w", path, err)
	}
	var first, last time.Time
	for i, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var entry struct {
			ObservedAt time.Time `json:"observedAt"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("record: %s line %d: %w", path, i+1, err)
		}
		if entry.ObservedAt.IsZero() {
			continue
		}
		at := entry.ObservedAt.UTC()
		if first.IsZero() || at.Before(first) {
			first = at
		}
		if at.After(last) {
			last = at
		}
	}
	return first, last, nil
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
