// SPDX-License-Identifier: Apache-2.0

package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres/pgtest"
)

func TestMain(m *testing.M) { pgtest.TestMain(m) }

var (
	day0 = time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	day1 = time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)
	day2 = time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
)

// TestMigrateFromEmpty is the migration up test: a database that has never seen sre-agent
// ends up with both schemas, every table of data-model.md, and the bookkeeping to prove it.
func TestMigrateFromEmpty(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := pgtest.Open(t)

	versions, err := store.AppliedVersions(ctx)
	if err != nil {
		t.Fatalf("applied versions: %v", err)
	}
	// Every embedded migration must be applied, in order, starting at 1. The count is read
	// from the embedded set rather than hard-coded, so adding a migration does not fail this
	// test for the wrong reason.
	embedded, err := postgres.Migrations()
	if err != nil {
		t.Fatalf("embedded migrations: %v", err)
	}
	if len(versions) != len(embedded) {
		t.Fatalf("applied versions = %v, want the %d embedded migrations", versions, len(embedded))
	}
	for i, migration := range embedded {
		if versions[i] != migration.Version {
			t.Fatalf("applied versions = %v, want %d at position %d (%s)",
				versions, migration.Version, i, migration.Name)
		}
	}

	relations := []string{
		"log.schema_migrations",
		"log.sources", "log.events", "log.rejected_events",
		"log.duplicate_deliveries", "log.checkpoints",
		"graph.entities", "graph.entity_versions", "graph.edge_versions",
		"graph.identity_claims", "graph.resolution_decisions",
		"graph.suggestions", "graph.principals",
	}
	for _, rel := range relations {
		var oid *uint32
		if err := store.Pool().QueryRow(ctx, "SELECT to_regclass($1)::oid", rel).Scan(&oid); err != nil {
			t.Fatalf("look up %s: %v", rel, err)
		}
		if oid == nil || *oid == 0 {
			t.Errorf("relation %s does not exist after Migrate", rel)
		}
	}

	var hasBtreeGist bool
	if err := store.Pool().QueryRow(ctx,
		"SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'btree_gist')").Scan(&hasBtreeGist); err != nil {
		t.Fatalf("check btree_gist: %v", err)
	}
	if !hasBtreeGist {
		t.Error("btree_gist extension missing; the exclusion constraints cannot exist without it")
	}

	for _, constraint := range []string{"entity_versions_no_overlap", "edge_versions_no_overlap"} {
		var kind string
		err := store.Pool().QueryRow(ctx,
			"SELECT contype::text FROM pg_constraint WHERE conname = $1", constraint).Scan(&kind)
		if err != nil {
			t.Fatalf("look up constraint %s: %v", constraint, err)
		}
		if kind != "x" {
			t.Errorf("constraint %s has contype %q, want %q (exclusion)", constraint, kind, "x")
		}
	}
}

// TestMigrateIsIdempotent: running Migrate on an already-migrated database changes nothing.
func TestMigrateIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := pgtest.Open(t)

	before := migrationFingerprint(ctx, t, store)

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("third Migrate: %v", err)
	}

	after := migrationFingerprint(ctx, t, store)
	if len(before) != len(after) {
		t.Fatalf("migration count changed: %d -> %d", len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Errorf("migration row %d changed: %+v -> %+v", i, before[i], after[i])
		}
	}
}

type migrationRow struct {
	Version   int64
	Name      string
	Checksum  string
	AppliedAt time.Time
}

func migrationFingerprint(ctx context.Context, t *testing.T, store *postgres.Store) []migrationRow {
	t.Helper()
	rows, err := store.Pool().Query(ctx,
		`SELECT version, name, checksum, applied_at FROM log.schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	collected, err := pgx.CollectRows(rows, pgx.RowToStructByPos[migrationRow])
	if err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	return collected
}

// TestExclusionConstraintRejectsOverlap is the structural half of the bitemporal invariant:
// one entity cannot have two versions whose valid AND observed intervals both overlap.
func TestExclusionConstraintRejectsOverlap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := pgtest.Open(t)
	seedEntity(ctx, t, store, "ent-1", "service")
	seedEntity(ctx, t, store, "ent-2", "service")

	insert := func(versionID string, valid, observed postgres.TimeRange) error {
		_, err := store.Pool().Exec(ctx, `
			INSERT INTO graph.entity_versions (version_id, entity_id, display_name, valid, observed)
			VALUES ($1, 'ent-1', 'checkout', $2, $3)`, versionID, valid, observed)
		return err
	}

	if err := insert("v1", postgres.NewTimeRange(day0, day2), postgres.OpenTimeRange(day0)); err != nil {
		t.Fatalf("insert first version: %v", err)
	}

	// Same entity, valid overlaps [day1,day2) and observed overlaps (both open): refused.
	err := insert("v2", postgres.NewTimeRange(day1, day2), postgres.OpenTimeRange(day1))
	if !isSQLState(err, "23P01") {
		t.Fatalf("overlapping version: got %v, want exclusion_violation (23P01)", err)
	}

	// Disjoint valid time is fine even with overlapping observed time: that is an entity
	// whose history has two consecutive states, both currently known.
	if err := insert("v3", postgres.NewTimeRange(day2, day2.Add(24*time.Hour)), postgres.OpenTimeRange(day1)); err != nil {
		t.Fatalf("disjoint valid range should be accepted: %v", err)
	}

	// A different entity with the identical intervals is fine: the constraint is per entity.
	_, err = store.Pool().Exec(ctx, `
		INSERT INTO graph.entity_versions (version_id, entity_id, display_name, valid, observed)
		VALUES ('v4', 'ent-2', 'checkout', $1, $2)`,
		postgres.NewTimeRange(day0, day2), postgres.OpenTimeRange(day0))
	if err != nil {
		t.Fatalf("other entity with the same intervals should be accepted: %v", err)
	}
}

func TestEdgeExclusionConstraintRejectsOverlap(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := pgtest.Open(t)
	seedEntity(ctx, t, store, "src", "service")
	seedEntity(ctx, t, store, "dst", "service")

	insert := func(versionID, edgeType string) error {
		_, err := store.Pool().Exec(ctx, `
			INSERT INTO graph.edge_versions (version_id, src_id, dst_id, type, valid, observed)
			VALUES ($1, 'src', 'dst', $2, $3, $4)`,
			versionID, edgeType, postgres.NewTimeRange(day0, day2), postgres.OpenTimeRange(day0))
		return err
	}

	if err := insert("e1", "calls"); err != nil {
		t.Fatalf("insert first edge version: %v", err)
	}
	if err := insert("e2", "calls"); !isSQLState(err, "23P01") {
		t.Fatalf("overlapping edge version: got %v, want exclusion_violation (23P01)", err)
	}
	// A different edge type between the same endpoints is a different fact.
	if err := insert("e3", "depends_on"); err != nil {
		t.Fatalf("different edge type should be accepted: %v", err)
	}
}

// TestCloseObserved exercises the only permitted mutation of a version row.
func TestCloseObserved(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := pgtest.Open(t)
	seedEntity(ctx, t, store, "ent-1", "service")

	_, err := store.Pool().Exec(ctx, `
		INSERT INTO graph.entity_versions (version_id, entity_id, display_name, valid, observed)
		VALUES ('v1', 'ent-1', 'checkout', $1, $2)`,
		postgres.NewTimeRange(day0, day2), postgres.OpenTimeRange(day0))
	if err != nil {
		t.Fatalf("insert version: %v", err)
	}

	var openBefore bool
	if err := store.Pool().QueryRow(ctx,
		`SELECT upper_inf(observed) FROM graph.entity_versions WHERE version_id = 'v1'`).Scan(&openBefore); err != nil {
		t.Fatalf("read upper_inf: %v", err)
	}
	if !openBefore {
		t.Fatal("a freshly inserted version must have an open observed upper bound")
	}

	var closed postgres.TimeRange
	err = store.Pool().QueryRow(ctx,
		`SELECT graph.close_observed('entity_versions', 'v1', $1, 'evt-correction')`, day1).Scan(&closed)
	if err != nil {
		t.Fatalf("close_observed: %v", err)
	}
	if closed.EndUnbounded || !closed.End.Equal(day1) || !closed.Start.Equal(day0) {
		t.Fatalf("close_observed returned %s, want [%s,%s)", closed, day0, day1)
	}

	var stored postgres.TimeRange
	var closedBy *string
	var stillOpen bool
	err = store.Pool().QueryRow(ctx, `
		SELECT observed, closed_by_event_id, upper_inf(observed)
		FROM graph.entity_versions WHERE version_id = 'v1'`).Scan(&stored, &closedBy, &stillOpen)
	if err != nil {
		t.Fatalf("read back version: %v", err)
	}
	if stillOpen {
		t.Error("upper_inf(observed) is still true after close_observed")
	}
	if !stored.End.Equal(day1) || stored.EndUnbounded {
		t.Errorf("stored observed = %s, want upper bound %s", stored, day1)
	}
	if closedBy == nil || *closedBy != "evt-correction" {
		t.Errorf("closed_by_event_id = %v, want evt-correction", closedBy)
	}

	// Refuses to move or reopen a bound that is already closed.
	_, err = store.Pool().Exec(ctx,
		`SELECT graph.close_observed('entity_versions', 'v1', $1, 'evt-second')`, day2)
	if !isSQLState(err, "23514") {
		t.Fatalf("closing an already closed bound: got %v, want check_violation (23514)", err)
	}

	// And the row is untouched by the refusal.
	var afterRefusal postgres.TimeRange
	if err := store.Pool().QueryRow(ctx,
		`SELECT observed FROM graph.entity_versions WHERE version_id = 'v1'`).Scan(&afterRefusal); err != nil {
		t.Fatalf("read back after refusal: %v", err)
	}
	if !afterRefusal.End.Equal(day1) {
		t.Errorf("observed moved to %s after a refused close", afterRefusal)
	}
}

func TestCloseObservedRejections(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := pgtest.Open(t)
	seedEntity(ctx, t, store, "ent-1", "service")

	_, err := store.Pool().Exec(ctx, `
		INSERT INTO graph.entity_versions (version_id, entity_id, valid, observed)
		VALUES ('v1', 'ent-1', $1, $2)`,
		postgres.NewTimeRange(day0, day2), postgres.OpenTimeRange(day1))
	if err != nil {
		t.Fatalf("insert version: %v", err)
	}

	cases := []struct {
		name     string
		sql      string
		args     []any
		sqlState string
	}{
		{
			name:     "unknown table",
			sql:      `SELECT graph.close_observed('log.events', 'v1', $1, 'e')`,
			args:     []any{day2},
			sqlState: "22023",
		},
		{
			name:     "unknown version",
			sql:      `SELECT graph.close_observed('entity_versions', 'nope', $1, 'e')`,
			args:     []any{day2},
			sqlState: "P0002",
		},
		{
			name:     "closed_at before the observed lower bound",
			sql:      `SELECT graph.close_observed('entity_versions', 'v1', $1, 'e')`,
			args:     []any{day0},
			sqlState: "22023",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := store.Pool().Exec(ctx, tc.sql, tc.args...)
			if !isSQLState(err, tc.sqlState) {
				t.Fatalf("got %v, want SQLSTATE %s", err, tc.sqlState)
			}
		})
	}
}

func TestWithTxRollsBack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := pgtest.Open(t)

	sentinel := errors.New("boom")
	err := store.WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO graph.entities (entity_id, type, created_by_event_id) VALUES ('rolled-back', 'service', 'e1')`)
		if err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("WithTx error = %v, want %v", err, sentinel)
	}

	var count int
	if err := store.Pool().QueryRow(ctx,
		`SELECT count(*) FROM graph.entities WHERE entity_id = 'rolled-back'`).Scan(&count); err != nil {
		t.Fatalf("count entities: %v", err)
	}
	if count != 0 {
		t.Errorf("rolled back row is still present (%d rows)", count)
	}

	err = store.WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO graph.entities (entity_id, type, created_by_event_id) VALUES ('committed', 'service', 'e1')`)
		return err
	})
	if err != nil {
		t.Fatalf("WithTx commit: %v", err)
	}
	if err := store.Pool().QueryRow(ctx,
		`SELECT count(*) FROM graph.entities WHERE entity_id = 'committed'`).Scan(&count); err != nil {
		t.Fatalf("count entities: %v", err)
	}
	if count != 1 {
		t.Errorf("committed row count = %d, want 1", count)
	}
}

// TestLogAppendRoundTrip is a smoke test of the log schema: a registered source, an event,
// and the observed_at the projector will read back on replay.
func TestLogAppendRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := pgtest.Open(t)

	_, err := store.Pool().Exec(ctx, `
		INSERT INTO log.sources (source_id, kind, ordering, reordering_window, schema_version)
		VALUES ('k8s:prod-eu1', 'k8s', 'per_source_sequence', '30 seconds', '1.0.0')`)
	if err != nil {
		t.Fatalf("register source: %v", err)
	}

	_, err = store.Pool().Exec(ctx, `
		INSERT INTO log.events
			(event_id, idempotency_key, source_id, source_seq, schema_version, type,
			 valid_at, observed_at, payload)
		VALUES ('k8s:1', 'k8s:1', 'k8s:prod-eu1', 1, '1.0.0', 'upsert_node', $1, $2, '{"ref":{}}')`,
		day0, day1)
	if err != nil {
		t.Fatalf("append event: %v", err)
	}

	var observedAt time.Time
	var appendedSeq int64
	err = store.Pool().QueryRow(ctx,
		`SELECT observed_at, appended_seq FROM log.events WHERE event_id = 'k8s:1'`).Scan(&observedAt, &appendedSeq)
	if err != nil {
		t.Fatalf("read event: %v", err)
	}
	if !observedAt.Equal(day1) {
		t.Errorf("observed_at = %s, want %s", observedAt, day1)
	}
	if appendedSeq <= 0 {
		t.Errorf("appended_seq = %d, want a positive sequence value", appendedSeq)
	}

	// The idempotency key is globally unique, which is what makes re-delivery a no-op.
	_, err = store.Pool().Exec(ctx, `
		INSERT INTO log.events (event_id, idempotency_key, source_id, schema_version, type, observed_at)
		VALUES ('k8s:2', 'k8s:1', 'k8s:prod-eu1', '1.0.0', 'upsert_node', $1)`, day2)
	if !isSQLState(err, "23505") {
		t.Fatalf("duplicate idempotency key: got %v, want unique_violation (23505)", err)
	}

	// Unknown event types never reach the log.
	_, err = store.Pool().Exec(ctx, `
		INSERT INTO log.events (event_id, idempotency_key, source_id, schema_version, type, observed_at)
		VALUES ('k8s:3', 'k8s:3', 'k8s:prod-eu1', '1.0.0', 'delete_everything', $1)`, day2)
	if !isSQLState(err, "23514") {
		t.Fatalf("unknown event type: got %v, want check_violation (23514)", err)
	}
}

func seedEntity(ctx context.Context, t *testing.T, store *postgres.Store, id, nodeType string) {
	t.Helper()
	_, err := store.Pool().Exec(ctx,
		`INSERT INTO graph.entities (entity_id, type, created_by_event_id) VALUES ($1, $2, 'seed')`,
		id, nodeType)
	if err != nil {
		t.Fatalf("seed entity %s: %v", id, err)
	}
}

func isSQLState(err error, state string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == state
}
