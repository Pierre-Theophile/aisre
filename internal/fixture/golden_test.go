// SPDX-License-Identifier: Apache-2.0

package fixture_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/fixture"
)

func TestGoldenPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		query fixture.Query
		want  string
	}{
		{
			"plain", fixture.Query{Kind: "subgraph", Name: "checkout-2hop-1432"},
			filepath.Join("fx", "golden", "subgraph.checkout-2hop-1432.json"),
		},
		{
			"pinned", fixture.Query{Kind: "subgraph", Name: "checkout-2hop-1432", Pinned: true},
			filepath.Join("fx", "golden", "pinned", "subgraph.checkout-2hop-1432.json"),
		},
		{
			"extent", fixture.Query{Kind: "extent", Name: "extent"},
			filepath.Join("fx", "golden", "extent.extent.json"),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := fixture.GoldenPath("fx", tc.query); got != tc.want {
				t.Errorf("GoldenPath = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCompareGolden(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	query := fixture.Query{Kind: "extent", Name: "extent"}
	path := fixture.GoldenPath(dir, query)

	recorded := &graphv1.Extent{
		Sources: []*graphv1.SourceExtent{{SourceId: "k8s:demo"}, {SourceId: "otel:demo"}},
	}
	if err := fixture.WriteGolden(path, recorded); err != nil {
		t.Fatalf("WriteGolden: %v", err)
	}

	t.Run("identical is equal", func(t *testing.T) {
		equal, diff, err := fixture.CompareGolden(path, recorded)
		if err != nil {
			t.Fatalf("CompareGolden: %v", err)
		}
		if !equal {
			t.Errorf("a result compared with its own recording differs:\n%s", diff)
		}
		if diff != "" {
			t.Errorf("diff = %q, want empty when equal", diff)
		}
	})

	t.Run("different is reported with a diff", func(t *testing.T) {
		changed := &graphv1.Extent{
			Sources: []*graphv1.SourceExtent{{SourceId: "k8s:demo"}, {SourceId: "otel:other"}},
		}
		equal, diff, err := fixture.CompareGolden(path, changed)
		if err != nil {
			t.Fatalf("CompareGolden: %v", err)
		}
		if equal {
			t.Fatal("a changed result compared equal to the golden")
		}
		if !strings.Contains(diff, "otel:other") || !strings.Contains(diff, "otel:demo") {
			t.Errorf("diff does not show both sides:\n%s", diff)
		}
		if !strings.HasPrefix(diff, "--- ") {
			t.Errorf("diff is not unified:\n%s", diff)
		}
	})

	t.Run("a missing golden is an error", func(t *testing.T) {
		_, _, err := fixture.CompareGolden(filepath.Join(dir, "golden", "extent.absent.json"), recorded)
		if err == nil {
			t.Fatal("CompareGolden accepted a golden that does not exist")
		}
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("error = %v, want a not-exist error so the CLI can say \"not recorded yet\"", err)
		}
	})

	t.Run("a trailing newline is insignificant", func(t *testing.T) {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read golden: %v", err)
		}
		bare := filepath.Join(t.TempDir(), "extent.extent.json")
		if err := os.WriteFile(bare, []byte(strings.TrimRight(string(raw), "\n")), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		equal, diff, err := fixture.CompareGolden(bare, recorded)
		if err != nil {
			t.Fatalf("CompareGolden: %v", err)
		}
		if !equal {
			t.Errorf("a golden without its trailing newline differed:\n%s", diff)
		}
	})
}
