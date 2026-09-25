// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// A fixture group is a directory that holds fixtures rather than being one. `fixtures/incidents/`
// is the case in hand: feature 002's incident fixtures extend the 001 format rather than
// replacing it, so `fixtures/*/` has to reach them without anybody maintaining a second glob.
func TestExpandFixtureArgs(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	mkFixture := func(path string) string {
		t.Helper()
		full := filepath.Join(root, path)
		if err := os.MkdirAll(full, 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", full, err)
		}
		if err := os.WriteFile(filepath.Join(full, "manifest.yaml"), []byte("id: x\n"), 0o600); err != nil {
			t.Fatalf("write manifest: %v", err)
		}
		return full
	}
	mkDir := func(path string) string {
		t.Helper()
		full := filepath.Join(root, path)
		if err := os.MkdirAll(full, 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", full, err)
		}
		return full
	}

	plain := mkFixture("baseline-topology-01")
	group := mkDir("incidents")
	memberB := mkFixture("incidents/b-incident")
	memberA := mkFixture("incidents/a-incident")
	empty := mkDir("adversarial")
	readme := filepath.Join(root, "README.md")
	if err := os.WriteFile(readme, []byte("# fixtures\n"), 0o600); err != nil {
		t.Fatalf("write readme: %v", err)
	}

	t.Run("a fixture runs as itself", func(t *testing.T) {
		t.Parallel()
		dirs, groups, err := expandFixtureArgs([]string{plain})
		if err != nil {
			t.Fatalf("expand: %v", err)
		}
		if !slices.Equal(dirs, []string{plain}) {
			t.Errorf("dirs = %v, want [%s]", dirs, plain)
		}
		if len(groups) != 0 {
			t.Errorf("empty groups = %v, want none", groups)
		}
	})

	t.Run("a group expands to its members, sorted", func(t *testing.T) {
		t.Parallel()
		dirs, groups, err := expandFixtureArgs([]string{group})
		if err != nil {
			t.Fatalf("expand: %v", err)
		}
		if !slices.Equal(dirs, []string{memberA, memberB}) {
			t.Errorf("dirs = %v, want [%s %s] in sorted order", dirs, memberA, memberB)
		}
		if len(groups) != 0 {
			t.Errorf("empty groups = %v, want none", groups)
		}
	})

	t.Run("an empty group is reported, not fatal", func(t *testing.T) {
		t.Parallel()
		dirs, groups, err := expandFixtureArgs([]string{plain, empty})
		if err != nil {
			t.Fatalf("expand: %v", err)
		}
		if !slices.Equal(dirs, []string{plain}) {
			t.Errorf("dirs = %v, want [%s]", dirs, plain)
		}
		if !slices.Equal(groups, []string{empty}) {
			t.Errorf("empty groups = %v, want [%s]", groups, empty)
		}
	})

	// A run that verifies nothing is not a run that passed.
	t.Run("nothing to verify is refused", func(t *testing.T) {
		t.Parallel()
		if _, _, err := expandFixtureArgs([]string{empty}); err == nil {
			t.Fatal("expand accepted a set of arguments that verifies no fixture at all")
		}
	})

	t.Run("a file is a usage error", func(t *testing.T) {
		t.Parallel()
		if _, _, err := expandFixtureArgs([]string{readme}); err == nil {
			t.Fatal("expand accepted a file; the glob somebody meant is fixtures/*/")
		}
	})

	t.Run("a path that does not exist is a usage error", func(t *testing.T) {
		t.Parallel()
		if _, _, err := expandFixtureArgs([]string{filepath.Join(root, "no-such-thing")}); err == nil {
			t.Fatal("expand accepted a path that does not exist")
		}
	})
}

// The repository's own layout, checked rather than assumed: `fixtures/*/` must resolve, and
// `fixtures/incidents/` must be seen as a group rather than as a broken fixture.
func TestRepositoryFixturesExpand(t *testing.T) {
	t.Parallel()

	entries, err := filepath.Glob(filepath.Join("..", "..", "fixtures", "*") + string(filepath.Separator))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(entries) == 0 {
		t.Skip("no fixtures in this checkout")
	}
	dirs, groups, err := expandFixtureArgs(entries)
	if err != nil {
		t.Fatalf("expand the repository's own fixtures: %v", err)
	}
	if len(dirs) == 0 {
		t.Fatal("no fixtures resolved from fixtures/*/")
	}
	for _, group := range groups {
		t.Logf("group with no fixtures yet: %s", group)
	}
}
