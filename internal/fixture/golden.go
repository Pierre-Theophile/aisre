// SPDX-License-Identifier: Apache-2.0

package fixture

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/Pierre-Theophile/aisre/internal/graph"
)

// Goldens (contracts/fixture-format.md §"Canonical serialization", SC-003).
//
// A golden is the canonical JSON of one query result, byte for byte. "Byte for byte" is only
// meaningful because graph.CanonicalJSON pins what the bytes are — sorted keys, no insignificant
// whitespace, protojson's timestamp spelling — so this file never invents a comparison of its
// own: it canonicalizes both sides and compares with bytes.Equal.
//
// The diff, by contrast, is for humans. Canonical JSON is one very long line, which is useless
// in a pull request, so the diff is computed over an indented rendering of both sides. The
// indented form is never what is compared.

// GoldenDir is the directory inside a fixture holding recorded query outputs.
const GoldenDir = "golden"

// PinnedDir is the subdirectory of GoldenDir holding the same queries with observed time pinned
// to the manifest's clock.end (US7).
const PinnedDir = "pinned"

// GoldenPath returns the file a query's recorded output lives in:
// `<dir>/golden/<kind>.<name>.json`, or `<dir>/golden/pinned/<kind>.<name>.json` for the pinned
// pass.
func GoldenPath(dir string, q Query) string {
	name := q.Kind + "." + q.Name + ".json"
	if q.Pinned {
		return filepath.Join(dir, GoldenDir, PinnedDir, name)
	}
	return filepath.Join(dir, GoldenDir, name)
}

// CompareGolden reads the golden at path and compares it with the canonical serialization of
// got.
//
// It reports equality, a unified text diff when they differ (empty otherwise), and an error only
// when the golden cannot be read or either side cannot be serialized. A missing golden is an
// error: "there is no recorded answer" and "the recorded answer differs" are different
// situations and `fixture verify` reports them differently.
func CompareGolden(path string, got proto.Message) (bool, string, error) {
	want, err := os.ReadFile(path)
	if err != nil {
		return false, "", fmt.Errorf("fixture: read golden: %w", err)
	}
	have, err := graph.CanonicalJSON(got)
	if err != nil {
		return false, "", err
	}

	// A recorded golden is written with a trailing newline; the canonical form of a document
	// has none. Normalizing one insignificant byte here is not a loosening of the byte-for-byte
	// rule: the file's content is otherwise compared exactly.
	trimmed := bytes.TrimRight(want, "\n")
	if bytes.Equal(trimmed, have) {
		return true, "", nil
	}
	return false, unifiedDiff(path, "(got)", indentJSON(trimmed), indentJSON(have)), nil
}

// WriteGolden writes got to path in canonical form, creating the directory. It is what
// `fixture record` uses; the recorded file is reviewed as a diff in the pull request that
// produces it (fixtures/README.md).
func WriteGolden(path string, got proto.Message) error {
	canonical, err := graph.CanonicalJSON(got)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("fixture: create golden directory: %w", err)
	}
	if err := os.WriteFile(path, append(canonical, '\n'), 0o644); err != nil {
		return fmt.Errorf("fixture: write golden: %w", err)
	}
	return nil
}

// indentJSON renders canonical JSON over several lines so a diff is readable. Input that is not
// JSON is returned unchanged, because a broken golden should still show up in the diff rather
// than disappear behind a formatting error.
func indentJSON(raw []byte) []string {
	var out bytes.Buffer
	if err := json.Indent(&out, raw, "", "  "); err != nil {
		return strings.Split(string(raw), "\n")
	}
	return strings.Split(out.String(), "\n")
}

// maxDiffLines bounds the quadratic part of unifiedDiff. Beyond it the diff degrades to a
// summary: a verifier must never become the slow part of CI, and a golden that differs by
// thousands of lines is read in the file, not in the terminal.
const maxDiffLines = 2000

// unifiedDiff renders a standard unified diff of two line slices with three lines of context.
func unifiedDiff(wantName, gotName string, want, got []string) string {
	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ %s\n", wantName, gotName)

	// Trimming the common head and tail first keeps the expensive part small for the usual
	// case, a golden that differs in one place.
	head := 0
	for head < len(want) && head < len(got) && want[head] == got[head] {
		head++
	}
	tail := 0
	for tail < len(want)-head && tail < len(got)-head &&
		want[len(want)-1-tail] == got[len(got)-1-tail] {
		tail++
	}
	midWant, midGot := want[head:len(want)-tail], got[head:len(got)-tail]

	if len(midWant) > maxDiffLines || len(midGot) > maxDiffLines {
		fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", head+1, len(midWant), head+1, len(midGot))
		out.WriteString("(differences are too large to render; compare the files directly)\n")
		return out.String()
	}

	ops := diffOps(midWant, midGot)
	const context = 3
	// Emit one hunk covering everything that changed, padded with context. Golden diffs are
	// small and local; a multi-hunk renderer would be more code for no more information.
	fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n",
		max(head-context+1, 1), len(midWant)+min(head, context),
		max(head-context+1, 1), len(midGot)+min(head, context))
	for _, line := range want[max(head-context, 0):head] {
		out.WriteString(" " + line + "\n")
	}
	for _, op := range ops {
		out.WriteString(op + "\n")
	}
	for _, line := range want[len(want)-tail : min(len(want)-tail+context, len(want))] {
		out.WriteString(" " + line + "\n")
	}
	return out.String()
}

// diffOps returns the "-"/"+"/" " prefixed lines of a shortest edit script between want and got.
func diffOps(want, got []string) []string {
	lcs := make([][]int, len(want)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(got)+1)
	}
	for i := len(want) - 1; i >= 0; i-- {
		for j := len(got) - 1; j >= 0; j-- {
			if want[i] == got[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}

	var ops []string
	i, j := 0, 0
	for i < len(want) && j < len(got) {
		switch {
		case want[i] == got[j]:
			ops = append(ops, " "+want[i])
			i, j = i+1, j+1
		case lcs[i+1][j] >= lcs[i][j+1]:
			ops = append(ops, "-"+want[i])
			i++
		default:
			ops = append(ops, "+"+got[j])
			j++
		}
	}
	for ; i < len(want); i++ {
		ops = append(ops, "-"+want[i])
	}
	for ; j < len(got); j++ {
		ops = append(ops, "+"+got[j])
	}
	return ops
}
