// SPDX-License-Identifier: Apache-2.0

package fixture

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Placeholders a reviewer still owes (002 FR-055, contracts/incident-format.md).
//
// `investigate to-incident` writes a manifest for a reviewed investigation. Some of it is
// derivable — the question, the culprit, the prior rank, the knowability time — and some of it is
// a reviewer's judgement: which hops carry the mechanism, which predicates make the evidence
// decisive, which candidates are decoys and what rules each of them out. A generated manifest
// writes those structurally valid, so the fixture loads and can be iterated on, and marks each
// one `# TODO(reviewer)`.
//
// The marker is a comment, deliberately. A sentinel *value* would have to be legal in a ref, in
// an instant and in a float, and something legal everywhere is something that can be scored: a
// fixture would grade against its own placeholder and pass. A comment cannot be graded by
// construction — which also means the parsed manifest cannot see it, so this reads the file.
//
// `fixture verify` reports what it finds rather than failing on it. A half-written fixture is a
// fixture in progress and saying so is more useful than refusing to look at it; what would be a
// defect is a half-written fixture that looked finished.

// PlaceholderMarker is the comment a generated manifest marks an underivable field with.
const PlaceholderMarker = "TODO(reviewer)"

// PlaceholderFields lists the manifest fields of a fixture directory that are still placeholders,
// in file order, as `<key>` or `<key> (line N)` for a marker that sits on its own line.
func PlaceholderFields(dir string) ([]string, error) {
	path := filepath.Join(dir, ManifestFile)
	file, err := os.Open(path) //nolint:gosec // a fixture directory the caller named
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("fixture: read %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	var out []string
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for line := 1; scanner.Scan(); line++ {
		text := scanner.Text()
		if !strings.Contains(text, PlaceholderMarker) {
			continue
		}
		if key := placeholderKey(text); key != "" {
			out = append(out, key)
			continue
		}
		out = append(out, fmt.Sprintf("line %d", line))
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("fixture: read %s: %w", path, err)
	}
	return out, nil
}

// placeholderKey is the YAML key a marked line carries, or "" for a marker on a comment line of
// its own — which is the case where the following block, not a single key, is the placeholder.
func placeholderKey(line string) string {
	before, _, ok := strings.Cut(line, "#")
	if !ok {
		return ""
	}
	before = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(before), "- "))
	key, _, ok := strings.Cut(before, ":")
	if !ok {
		return ""
	}
	key = strings.TrimSpace(key)
	if key == "" || strings.ContainsAny(key, " \t") {
		return ""
	}
	return key
}
