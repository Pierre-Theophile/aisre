// SPDX-License-Identifier: Apache-2.0

package campaign

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Reading and writing the campaign record.
//
// Both ends validate, and that is not belt-and-braces. Writing validates so an invalid record never
// reaches disk, because a campaign directory is copied, moved between repositories and reviewed by
// somebody who was not there. Reading validates because a record can be hand-edited — it is YAML in
// a directory, and it will be — and a reader that accepted a record with a gap in its scope windows
// would answer "not in scope" for an instant nobody ever decided about.

// Write writes the record to `<dir>/campaign.yaml`, creating the directory.
//
// It refuses to overwrite a record that is already there. A campaign record is appended to over
// weeks — a scope window closes, another opens — and a `record` command that silently replaced the
// file would lose the history that FR-130 exists to keep. Updating is Update's job, which reads,
// changes and writes back.
func Write(dir string, r Record) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("campaign: create %s: %w", dir, err)
	}
	path := filepath.Join(dir, RecordFile)
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("campaign: %s already exists; a campaign record is added to rather than "+
			"replaced, because the scope windows it already holds were true when they were written "+
			"(FR-130). Edit it, or write to a new campaign directory", path)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("campaign: stat %s: %w", path, err)
	}
	return writeFile(path, r)
}

// Update reads the record, applies change, validates the result and writes it back.
//
// The validation is on the RESULT rather than on the change, which is the only way to catch the
// mistake this is for: closing a scope window and opening the next one are two edits, and a change
// that did the first without the second leaves a record whose windows are no longer continuous.
// Validating the change alone would pass it.
func Update(dir string, change func(*Record) error) error {
	r, err := Read(dir)
	if err != nil {
		return err
	}
	if err := change(&r); err != nil {
		return err
	}
	if err := r.Validate(); err != nil {
		return fmt.Errorf("campaign: the change would leave an invalid record: %w", err)
	}
	return writeFile(filepath.Join(dir, RecordFile), r)
}

// Read reads and validates `<dir>/campaign.yaml`.
func Read(dir string) (Record, error) {
	path := filepath.Join(dir, RecordFile)
	body, err := os.ReadFile(path) //nolint:gosec // a campaign directory the caller named
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Record{}, fmt.Errorf("campaign: %s has no %s; a recording with no campaign "+
				"record cannot say what scope it was taken under or who authorised the mailbox read "+
				"(FR-130, FR-132)", dir, RecordFile)
		}
		return Record{}, fmt.Errorf("campaign: read %s: %w", path, err)
	}
	var r Record
	if err := yaml.Unmarshal(body, &r); err != nil {
		return Record{}, fmt.Errorf("campaign: parse %s: %w", path, err)
	}
	if err := r.Validate(); err != nil {
		return Record{}, fmt.Errorf("campaign: %s: %w", path, err)
	}
	return r, nil
}

// Exists reports whether a campaign directory carries a record, without validating it. It is for a
// caller deciding between `record` and `record --update`, which should not have to distinguish an
// absent record from an invalid one to make that choice.
func Exists(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, RecordFile))
	return err == nil
}

func writeFile(path string, r Record) error {
	// The header is in the file rather than only in this package's documentation, because the file
	// outlives the checkout it was written from: it is copied into a private repository and read by
	// somebody who has no reason to have this source open.
	const header = "# SPDX-License-Identifier: Apache-2.0\n" +
		"#\n" +
		"# A recording campaign's own account of itself (FR-130, FR-131, FR-132).\n" +
		"#\n" +
		"# `scopes` is a SERIES, oldest first, because recording starts before the scope is agreed and\n" +
		"# the scope then changes. A reader asking what was in scope at an instant walks to the window\n" +
		"# covering it; a gap would answer \"not in scope\" for an instant nobody decided about, which is\n" +
		"# why the windows are required to be continuous.\n" +
		"#\n" +
		"# There is no mailbox address here on purpose. See `mailbox_address`.\n"
	body, err := yaml.Marshal(r)
	if err != nil {
		return fmt.Errorf("campaign: encode %s: %w", path, err)
	}
	if err := os.WriteFile(path, append([]byte(header), body...), 0o644); err != nil { //nolint:gosec // a fixture file is world-readable by design
		return fmt.Errorf("campaign: write %s: %w", path, err)
	}
	return nil
}
