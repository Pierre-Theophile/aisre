// SPDX-License-Identifier: Apache-2.0

package migrate_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/fixture"
	eventlog "github.com/Pierre-Theophile/aisre/internal/log"
	"github.com/Pierre-Theophile/aisre/internal/log/migrate"
	"github.com/Pierre-Theophile/aisre/internal/projector"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres/pgtest"
)

func TestMain(m *testing.M) { pgtest.TestMain(m) }

// Schema version drift, end to end (T088, FR-024, FR-025, edge case "schema version drift").
//
// The unit tests above prove the registry composes. These three prove the thing the edge case
// actually promises, against a real database and a real fixture:
//
//   - a recording that declares a version the build has never heard of is refused, event by
//     event, with the published reason code — not silently ignored and not half-applied;
//   - the same recording at the accepted version loads;
//   - and registering a transformation from the old version to the accepted one is all it takes
//     to make the first recording load again, which is exactly what constitution IX means by
//     "a migration path expressed as an event-log transformation".
//
// The fixture is copied into a temporary directory and rewritten, so the shipped one is never
// touched: a test that mutated `fixtures/` would be rewriting the evidence.

const (
	sourceFixture = "../../../fixtures/baseline-topology-01"
	driftVersion  = "0.9.0"
)

func TestFixtureDeclaringAnUnknownSchemaVersionIsRejected(t *testing.T) {
	dir := copyFixtureAtVersion(t, driftVersion)
	store := pgtest.Open(t)
	proj := projector.New(store)

	report, err := fixture.Load(context.Background(), proj, dir, fixture.LoadOptions{})
	if err != nil {
		t.Fatalf("load drifted fixture: %v", err)
	}
	if report.Applied != 0 {
		t.Errorf("applied %d events from a fixture at schema version %s; none may reach the graph",
			report.Applied, driftVersion)
	}
	if report.Rejected == 0 {
		t.Fatal("the drifted fixture was not rejected at all")
	}
	for _, result := range report.Results {
		if result.GetStatus() != graphv1.IngestResult_REJECTED {
			t.Fatalf("event %s: status = %s, want REJECTED", result.GetEventId(), result.GetStatus())
		}
		if result.GetReasonCode() != eventlog.ReasonUnknownSchemaVersion {
			t.Fatalf("event %s: reason = %q, want %q",
				result.GetEventId(), result.GetReasonCode(), eventlog.ReasonUnknownSchemaVersion)
		}
		// FR-024: the feeder has to be told which versions it could have sent.
		if !strings.Contains(result.GetReasonDetail(), "1.0.0") {
			t.Fatalf("event %s: detail %q does not name the accepted versions",
				result.GetEventId(), result.GetReasonDetail())
		}
	}
}

func TestFixtureAtTheAcceptedSchemaVersionLoads(t *testing.T) {
	dir := copyFixtureAtVersion(t, migrate.CurrentVersion)
	store := pgtest.Open(t)
	proj := projector.New(store)

	report, err := fixture.Load(context.Background(), proj, dir, fixture.LoadOptions{})
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	if report.Applied == 0 {
		t.Fatal("nothing was applied from a fixture at the accepted schema version")
	}
	for _, result := range report.Results {
		if result.GetReasonCode() == eventlog.ReasonUnknownSchemaVersion {
			t.Fatalf("event %s was refused for its schema version at %s",
				result.GetEventId(), migrate.CurrentVersion)
		}
	}
}

func TestRegisteredTransformerMakesTheDriftedFixtureLoad(t *testing.T) {
	dir := copyFixtureAtVersion(t, driftVersion)
	store := pgtest.Open(t)

	// The migration a real 0.9.0 → 1.0.0 bump would ship: here it only re-labels the envelope,
	// which is all this fixture needs, but it is registered and resolved exactly as a payload
	// rewrite would be.
	registry := migrate.NewRegistry()
	mustRegister(t, registry, migrate.Func(driftVersion, migrate.CurrentVersion,
		func(env *graphv1.EventEnvelope) (*graphv1.EventEnvelope, error) { return env, nil }))

	log := eventlog.New(store, eventlog.WithMigrations(registry))
	proj := projector.New(store, projector.WithLog(log))

	report, err := fixture.Load(context.Background(), proj, dir, fixture.LoadOptions{})
	if err != nil {
		t.Fatalf("load transformed fixture: %v", err)
	}
	if report.Applied == 0 {
		t.Fatal("the transformation was registered but nothing was applied")
	}
	for _, result := range report.Results {
		if result.GetReasonCode() == eventlog.ReasonUnknownSchemaVersion {
			t.Fatalf("event %s still refused for its schema version despite a registered %s → %s transformation",
				result.GetEventId(), driftVersion, migrate.CurrentVersion)
		}
	}

	// FR-025 and FR-023 together: what is in the log is the *transformed* event, so a replay
	// reads the current shape and reproduces the same graph without re-running the migration.
	var versions []string
	rows, err := store.Pool().Query(context.Background(),
		`SELECT DISTINCT schema_version FROM log.events ORDER BY schema_version`)
	if err != nil {
		t.Fatalf("read logged schema versions: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan schema version: %v", err)
		}
		versions = append(versions, v)
	}
	if len(versions) != 1 || versions[0] != migrate.CurrentVersion {
		t.Errorf("log.events holds schema versions %v, want only %s", versions, migrate.CurrentVersion)
	}
}

// copyFixtureAtVersion copies the shipped fixture into a temporary directory with every event
// and the manifest re-stamped at version.
func copyFixtureAtVersion(t *testing.T, version string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), filepath.Base(sourceFixture))
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatalf("create fixture copy: %v", err)
	}
	// The whole fixture is copied, not just the two files that are rewritten: the manifest may
	// name a rejected-events file, and a partial copy would fail to load for the wrong reason.
	entries, err := os.ReadDir(sourceFixture)
	if err != nil {
		t.Fatalf("read %s: %v", sourceFixture, err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		raw, err := os.ReadFile(filepath.Join(sourceFixture, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		body := string(raw)
		body = strings.ReplaceAll(body, `"schemaVersion":"1.0.0"`, `"schemaVersion":"`+version+`"`)
		body = strings.ReplaceAll(body, "schema_version: 1.0.0", "schema_version: "+version)
		if err := os.WriteFile(filepath.Join(dst, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dst
}
