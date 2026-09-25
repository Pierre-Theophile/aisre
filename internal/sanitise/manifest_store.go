// SPDX-License-Identifier: Apache-2.0

package sanitise

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Reading and writing the signed manifest.

// WriteManifest writes `<dir>/sanitisation.yaml`.
//
// It validates the manifest's SHAPE and deliberately not the second-signature count. The distinction
// is not pedantry: a two-person campaign's first signature has to be writable, or the second person
// has nothing to countersign and a two-signature sign-off is unreachable. Checking the count here
// made exactly that mistake and the CLI test caught it.
//
// So the count is a COMMIT gate, not a write gate. `Check` is what enforces it, `fixture campaign
// sign` reports when a second signature is still owed, and a manifest on disk with one of two
// signatures is an honest intermediate state: the file exists, the gate is not passed.
//
// It refuses to overwrite. A signed manifest records a person's sign-off, and a command that replaced
// one would let a second run drop a signature a human gave — the same failure as an unsigned commit,
// arrived at by accident instead of on purpose.
func WriteManifest(dir string, m Manifest, _ int) error {
	if err := m.CheckShape(); err != nil {
		return err
	}
	path := filepath.Join(dir, ManifestFile)
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("sanitise: %s already exists; it records a person's sign-off and is not "+
			"replaced by a later run. Countersign it instead, or sign a recording that has not been "+
			"signed", path)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("sanitise: stat %s: %w", path, err)
	}
	body, err := yaml.Marshal(m)
	if err != nil {
		return fmt.Errorf("sanitise: encode %s: %w", path, err)
	}
	const header = "# SPDX-License-Identifier: Apache-2.0\n" +
		"#\n" +
		"# The signed manifest for this recording (FR-140, contracts/sanitisation.md §6).\n" +
		"#\n" +
		"# `content_hash` is over every committed file in this directory EXCEPT this one, so a file\n" +
		"# edited, added or removed after signing no longer matches. See `attestation` for what a\n" +
		"# signature here does and does not mean.\n"
	if err := os.WriteFile(path, append([]byte(header), body...), 0o644); err != nil { //nolint:gosec // a fixture file is world-readable by design
		return fmt.Errorf("sanitise: write %s: %w", path, err)
	}
	return nil
}

// Countersign adds a signature to an existing manifest and rewrites it.
//
// It re-verifies the content hash first, which is the whole reason this is a function rather than an
// edit. A second signature on a recording that changed since the first is worse than no second
// signature: it reads as two people having reviewed the same thing when they reviewed two different
// things.
func Countersign(dir string, s Signature, declaredSignatories int) error {
	m, err := ReadManifest(dir)
	if err != nil {
		return err
	}
	if err := m.Verify(dir); err != nil {
		return fmt.Errorf("sanitise: refusing to countersign: %w. The first signature covers "+
			"different content, so two signatures would read as two reviews of one recording", err)
	}
	m.Signatures = append(m.Signatures, s)
	// The shape, again for the same reason: a third signature on a three-person campaign is written
	// while the count is still short. What must not pass is a duplicate signer, which CheckShape
	// catches — two signatures from one person are one signature however many are owed.
	if err := m.CheckShape(); err != nil {
		return err
	}
	_ = declaredSignatories
	body, err := yaml.Marshal(m)
	if err != nil {
		return fmt.Errorf("sanitise: encode manifest: %w", err)
	}
	path := filepath.Join(dir, ManifestFile)
	existing, err := os.ReadFile(path) //nolint:gosec // the path we just read
	if err != nil {
		return fmt.Errorf("sanitise: read %s: %w", path, err)
	}
	header := headerOf(existing)
	if err := os.WriteFile(path, append(header, body...), 0o644); err != nil { //nolint:gosec // as above
		return fmt.Errorf("sanitise: write %s: %w", path, err)
	}
	return nil
}

// ReadManifest reads `<dir>/sanitisation.yaml` without checking the signature count — the caller
// supplies that from the campaign record, and a reader that needed it could not report an unsigned
// manifest as unsigned.
func ReadManifest(dir string) (Manifest, error) {
	path := filepath.Join(dir, ManifestFile)
	body, err := os.ReadFile(path) //nolint:gosec // a recording directory the caller named
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Manifest{}, fmt.Errorf("sanitise: %s has no %s; every committed recording "+
				"carries a signed manifest (FR-140)", dir, ManifestFile)
		}
		return Manifest{}, fmt.Errorf("sanitise: read %s: %w", path, err)
	}
	var m Manifest
	if err := yaml.Unmarshal(body, &m); err != nil {
		return Manifest{}, fmt.Errorf("sanitise: parse %s: %w", path, err)
	}
	return m, nil
}

// HasManifest reports whether a recording carries a manifest at all.
func HasManifest(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ManifestFile))
	return err == nil
}

// headerOf returns the leading comment block of a YAML file, so rewriting preserves it.
func headerOf(body []byte) []byte {
	var out []byte
	for len(body) > 0 {
		end := len(body)
		for i, b := range body {
			if b == '\n' {
				end = i + 1
				break
			}
		}
		line := body[:end]
		if len(line) == 0 || line[0] != '#' {
			break
		}
		out = append(out, line...)
		body = body[end:]
	}
	return out
}
