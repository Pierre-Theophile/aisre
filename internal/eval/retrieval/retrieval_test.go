// SPDX-License-Identifier: Apache-2.0

package retrieval_test

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/eval/retrieval"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres/pgtest"
	kstore "github.com/Pierre-Theophile/aisre/pkg/knowledge"
)

func TestMain(m *testing.M) { pgtest.TestMain(m) }

const corpusDir = "../../../fixtures/retrieval"

func run(t *testing.T, fixtures []*retrieval.Fixture) ([]retrieval.Result, []retrieval.Aggregate) {
	t.Helper()
	results, err := retrieval.Run(context.Background(), fixtures)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return results, retrieval.Summarise(results)
}

// TestTheShippedCorpusPassesTheGate is the gate itself, on every pull request (008 T017, FR-042).
func TestTheShippedCorpusPassesTheGate(t *testing.T) {
	t.Parallel()
	fixtures, err := retrieval.LoadAll(corpusDir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(fixtures) < 8 {
		t.Errorf("%d retrieval fixtures, FR-043 asks for at least eight", len(fixtures))
	}
	results, aggregates := run(t, fixtures)
	var report strings.Builder
	failures := retrieval.Gate(aggregates)
	retrieval.Report(&report, results, aggregates, failures)
	t.Log("\n" + report.String())
	if len(failures) > 0 {
		t.Errorf("the gate fails: %v", failures)
	}
}

// TestDroppingTheBodyFailsTheGate is the planted regression: with every stored section removed,
// lexical retrieval is 007's again and must not pass (008 T017).
func TestDroppingTheBodyFailsTheGate(t *testing.T) {
	t.Parallel()
	fixtures, err := retrieval.LoadAll(corpusDir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, f := range fixtures {
		for i := range f.Documents.Documents {
			f.Documents.Documents[i].Puts = nil
		}
	}
	_, aggregates := run(t, fixtures)
	if failures := retrieval.Gate(aggregates); len(failures) == 0 {
		t.Errorf("the gate passed with every body section dropped: %+v", aggregates)
	}
}

func TestAFixtureThatCannotGradeIsRefused(t *testing.T) {
	t.Parallel()
	dir := t.TempDir() + "/broken-01"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(dir+"/"+name, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(retrieval.QueryFile, "id: broken-01\nquery: {terms: [x], entity_ids: [a=b], valid_at: 2026-09-01T00:00:00Z, observed_at: 2026-09-01T00:00:00Z}\nexpect: {document: n=missing, in_scope: true}\n")
	write(retrieval.DocumentsFile, "documents: []\n")
	if _, err := retrieval.Load(dir); err == nil || !strings.Contains(err.Error(), "not among the documents") {
		t.Errorf("err = %v, want the missing expected document named", err)
	}
}

// TestTheCorpusScoresTheSameOnEveryShippedStore is 008 T038 (SC-008): the same fixtures over the
// PostgreSQL store, which `serve` uses by default, give exactly what they give over the memory
// store. Swapping the store is configuration; the ranking is the worker's, so nothing may move.
func TestTheCorpusScoresTheSameOnEveryShippedStore(t *testing.T) {
	fixtures, err := retrieval.LoadAll(corpusDir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	memoryResults, _ := run(t, fixtures)
	postgresResults, err := retrieval.Run(context.Background(), fixtures, retrieval.WithStore(
		func(context.Context) (kstore.Store, error) { return postgres.NewKnowledgeStore(pgtest.Open(t)), nil }))
	if err != nil {
		t.Fatalf("run over postgres: %v", err)
	}
	if !reflect.DeepEqual(memoryResults, postgresResults) {
		t.Errorf("the corpus scores differently over postgres:\n memory:   %+v\n postgres: %+v", memoryResults, postgresResults)
	}
	if failures := retrieval.Gate(retrieval.Summarise(postgresResults)); len(failures) > 0 {
		t.Errorf("the gate fails over postgres: %v", failures)
	}
}

// TestAFixtureWithoutVectorsFailsTheHybridGate: the hybrid gate is only a gate while every fixture
// is replayed through its recorded vectors.
func TestAFixtureWithoutVectorsFailsTheHybridGate(t *testing.T) {
	t.Parallel()
	fixtures, err := retrieval.LoadAll(corpusDir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	fixtures[0].Vectors = nil
	_, aggregates := run(t, fixtures)
	failures := retrieval.Gate(aggregates)
	if len(failures) != 1 || !strings.Contains(failures[0], "1 of 9 fixtures carry no recorded vectors") {
		t.Errorf("failures = %v", failures)
	}
}
