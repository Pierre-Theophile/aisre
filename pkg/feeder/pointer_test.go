// SPDX-License-Identifier: Apache-2.0

package feeder_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The three GCP pointer vocabularies (003 FR-079–FR-085,
// specs/003-gcp-integration/contracts/pointer-vocabularies.md).
//
// Registering a vocabulary is a schema change, so what is asserted here is the part of the change
// that code can check: that the names are what the contract publishes, that a pointer minted in one
// survives construction unchanged, that no pointer carries a credential, and that the trace
// vocabulary is registered but minted by nobody.

func TestGCPVocabularyNamesAreTheOnesPublished(t *testing.T) {
	t.Parallel()

	// Spelled out rather than compared against the constants, because a test that reads
	// `VocabGCPLoggingQuery == VocabGCPLoggingQuery` would pass through a typo in the very
	// string a stored pointer is read back by. The API version in each name is the vocabulary
	// version: these languages are not independently versioned.
	want := map[string]string{
		"monitoring": "gcp-monitoring-filter/v3",
		"logging":    "gcp-logging-query/v2",
		"trace":      "gcp-trace-filter/v1",
	}
	got := map[string]string{
		"monitoring": feeder.VocabGCPMonitoringFilter,
		"logging":    feeder.VocabGCPLoggingQuery,
		"trace":      feeder.VocabGCPTraceFilter,
	}
	for area, expected := range want {
		if got[area] != expected {
			t.Errorf("%s vocabulary = %q, want %q", area, got[area], expected)
		}
	}

	if len(feeder.GCPVocabularies) != 3 {
		t.Fatalf("GCPVocabularies has %d entries, want 3", len(feeder.GCPVocabularies))
	}
	for i, v := range []string{
		feeder.VocabGCPMonitoringFilter, feeder.VocabGCPLoggingQuery, feeder.VocabGCPTraceFilter,
	} {
		if feeder.GCPVocabularies[i] != v {
			t.Errorf("GCPVocabularies[%d] = %q, want %q", i, feeder.GCPVocabularies[i], v)
		}
	}
}

func TestAGCPSelectorRoundTripsUntranslated(t *testing.T) {
	t.Parallel()

	// FR-111: the backend accepts the pointers the feeders emit WITHOUT translation. So the
	// selector that comes out of NewPointer has to be byte-identical to the one that went in —
	// a constructor that normalised whitespace, reordered clauses or re-quoted values would be
	// changing a query somebody will execute years from now.
	//
	// This selector pins resource.type deliberately: run.googleapis.com/request_count is written
	// against both cloud_run_revision and cloud_run_instance, and cloud_run_instance carries no
	// revision_name label, so a selector that omits the type silently picks up series with no
	// revision.
	// Written multi-line, the way the contract writes it and the way an operator reads it back out
	// of a golden. The layout is part of what must survive: an earlier version of this test used a
	// selector with single spaces throughout, and a constructor that collapsed whitespace was a
	// no-op on it — so the test passed while failing to check the thing it is named for.
	const selector = "metric.type = \"run.googleapis.com/request_count\"\n" +
		"  AND resource.type = \"cloud_run_revision\"\n" +
		"  AND resource.labels.project_id = \"example-project\"\n" +
		"  AND resource.labels.location = \"europe-west1\"\n" +
		"  AND resource.labels.service_name = \"checkout\""

	attrs := map[string]string{
		"service.name":    "checkout",
		"cloud.region":    "europe-west1",
		"cloud.platform":  "gcp_cloud_run",
		"cloud.provider":  "gcp",
		"service.version": "checkout-00042-abc",
	}

	p := feeder.NewPointer(
		graphv1.PointerKind_METRIC, "gcp-monitoring", feeder.VocabGCPMonitoringFilter, selector, attrs)

	if p.GetSelector() != selector {
		t.Errorf("selector was rewritten:\n got %q\nwant %q", p.GetSelector(), selector)
	}
	if p.GetVocabulary() != feeder.VocabGCPMonitoringFilter {
		t.Errorf("vocabulary = %q, want %q", p.GetVocabulary(), feeder.VocabGCPMonitoringFilter)
	}
	if p.GetBackendKind() != "gcp-monitoring" {
		t.Errorf("backend kind = %q, want gcp-monitoring", p.GetBackendKind())
	}

	// The other half of the split: the selector is GCP's language, the ATTRIBUTES stay OTel, so
	// a reader can tell which entity the pointer is about without understanding the grammar.
	for _, key := range []string{"service.name", "cloud.region", "cloud.platform"} {
		if p.GetAttributes()[key] != attrs[key] {
			t.Errorf("attribute %q = %q, want %q", key, p.GetAttributes()[key], attrs[key])
		}
	}

	// The caller's map must not be aliased: a feeder reusing one attribute map across many
	// pointers would otherwise mutate an event it has already emitted.
	attrs["service.name"] = "mutated-after-the-fact"
	if p.GetAttributes()["service.name"] != "checkout" {
		t.Error("the pointer aliases the caller's attribute map")
	}
}

func TestALoggingPointerKeepsWhatIsNotAnAttributeEquality(t *testing.T) {
	t.Parallel()

	// The reason this vocabulary exists rather than being flattened into otel-semconv: severity
	// comparison, RE2 and boolean structure have no equality form. If any of it were rewritten,
	// the pointer would quietly stop matching what it was written to match.
	const selector = `resource.type = "cloud_run_revision" ` +
		`AND resource.labels.service_name = "checkout" ` +
		`AND severity >= WARNING ` +
		`AND NOT (textPayload =~ "health-?check" OR labels.synthetic:*)`

	p := feeder.NewPointer(
		graphv1.PointerKind_LOG, "gcp-logging", feeder.VocabGCPLoggingQuery, selector, nil)

	if p.GetSelector() != selector {
		t.Fatalf("selector was rewritten:\n got %q\nwant %q", p.GetSelector(), selector)
	}
	for _, fragment := range []string{"severity >= WARNING", "=~", "NOT (", "labels.synthetic:*"} {
		if !strings.Contains(p.GetSelector(), fragment) {
			t.Errorf("selector lost %q, which has no attribute-equality form", fragment)
		}
	}
}

// credentialish matches the shapes a credential takes if one ever reaches a pointer: an OAuth or
// service-account token, a private key block, a bearer header, or a query parameter carrying one.
var credentialish = regexp.MustCompile(
	`(?i)(ya29\.|-----BEGIN [A-Z ]*PRIVATE KEY|bearer\s+[a-z0-9._-]{8,}|` +
		`(access_token|refresh_token|client_secret|private_key|api[_-]?key)\s*[=:])`)

func TestNoPointerCarriesACredential(t *testing.T) {
	t.Parallel()

	// FR-082. A pointer says where to look; a credential in one would be committed to the graph,
	// to every golden, and to every fixture in the public repository — including a console deep
	// link, which is the shape most likely to acquire one by accident.
	cases := []struct {
		name    string
		pointer *graphv1.Pointer
	}{
		{"a monitoring filter", feeder.NewPointer(graphv1.PointerKind_METRIC, "gcp-monitoring",
			feeder.VocabGCPMonitoringFilter,
			`metric.type = "run.googleapis.com/request_count" AND resource.type = "cloud_run_revision"`,
			map[string]string{"service.name": "checkout", "cloud.region": "europe-west1"})},
		{"a logging query", feeder.NewPointer(graphv1.PointerKind_LOG, "gcp-logging",
			feeder.VocabGCPLoggingQuery,
			`resource.type = "cloud_run_revision" AND severity >= WARNING`, nil)},
		{"a console deep link", feeder.SourceLinkPointer("gcp", "url",
			"https://console.cloud.google.com/run/detail/europe-west1/checkout/metrics", nil)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			parts := []string{tc.pointer.GetSelector(), tc.pointer.GetBackendKind(), tc.pointer.GetVocabulary()}
			for k, v := range tc.pointer.GetAttributes() {
				parts = append(parts, k, v)
			}
			for role, attr := range tc.pointer.GetJoinKeys() {
				parts = append(parts, role, attr)
			}
			if match := credentialish.FindString(strings.Join(parts, "\x00")); match != "" {
				t.Errorf("pointer carries something credential-shaped: %q", match)
			}
		})
	}
}

func TestTheTraceVocabularyIsRegisteredAndMintedByNobody(t *testing.T) {
	t.Parallel()

	// FR-085. This organisation has no trace data source. The vocabulary is registered so that
	// error_spans can become live with no schema change if Cloud Trace is ever enabled — but no
	// pointer may be minted in it, because the absence has to be a stated fact about the entity
	// rather than a fabricated pointer. A consumer must be able to tell "no trace pointer because
	// there is no tracing" from "nobody wrote one".
	//
	// Asserted by reading the source rather than by running the feeders, because the property is
	// "nothing anywhere mints this", which no amount of calling can demonstrate. A false positive
	// here is a real finding: it means something now mints a trace pointer.
	root := repoRoot(t)
	var offenders []string

	for _, dir := range []string{"internal/feeders", "internal/backends", "pkg/feeder"} {
		walkGoFiles(t, filepath.Join(root, dir), func(path string, file *ast.File, fset *token.FileSet) {
			// This test file names the constant constantly; so does the declaration itself.
			if strings.HasSuffix(path, "_test.go") || strings.HasSuffix(path, "pkg/feeder/pointer.go") {
				return
			}
			ast.Inspect(file, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "VocabGCPTraceFilter" {
					return true
				}
				rel, _ := filepath.Rel(root, path)
				offenders = append(offenders,
					fmt.Sprintf("%s:%d", rel, fset.Position(sel.Pos()).Line))
				return true
			})
		})
	}

	if len(offenders) > 0 {
		t.Errorf("VocabGCPTraceFilter is referenced outside its declaration, which means something "+
			"may now mint a trace pointer — FR-085 requires the absence of tracing to be a stated "+
			"fact, not a fabricated pointer:\n  %s", strings.Join(offenders, "\n  "))
	}

	// And it really is registered, so error_spans can go live without a schema change.
	found := false
	for _, v := range feeder.GCPVocabularies {
		if v == feeder.VocabGCPTraceFilter {
			found = true
		}
	}
	if !found {
		t.Error("the trace vocabulary is not in GCPVocabularies; it must be registered even though " +
			"nothing mints it")
	}
}

// walkGoFiles parses every .go file under dir and calls fn for each.
func walkGoFiles(t *testing.T, dir string, fn func(string, *ast.File, *token.FileSet)) {
	t.Helper()
	fset := token.NewFileSet()
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		parsed, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", path, perr)
		}
		fn(path, parsed, fset)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
}

// repoRoot walks up from the test's working directory to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test's working directory")
		}
		dir = parent
	}
}
