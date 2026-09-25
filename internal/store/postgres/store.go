// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"cmp"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migrateLockKey is the advisory lock every Migrate call takes, so that two processes
// starting at once cannot apply the same migration twice. The value is the ASCII bytes of
// "SREAGENT" read as a big-endian int64; it only has to be stable and unlikely to collide.
const migrateLockKey int64 = 0x5352454147454E54

// Migration is one embedded SQL file, identified by the numeric prefix of its name.
type Migration struct {
	// Version is the numeric prefix, e.g. 1 for "0001_log.sql". Versions are applied in
	// ascending order and are never reused.
	Version int64
	// Name is the file name as embedded, e.g. "0001_log.sql".
	Name string
	// SQL is the file contents, verbatim.
	SQL string
	// Checksum is the hex-encoded SHA-256 of SQL, recorded when the migration is applied so
	// that editing an already-applied file is caught instead of silently ignored.
	Checksum string
}

// Store is a handle on the sre-agent PostgreSQL database: a pgx connection pool plus the
// embedded schema migrations. It is safe for concurrent use.
type Store struct {
	pool *pgxpool.Pool
}

// Open dials dsn and returns a ready Store. The pool is verified with a ping before it is
// returned, and every connection it hands out knows the project's range types (see range.go),
// runs in UTC, and identifies itself as "sre-agent". Call Close when done.
func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse dsn: %w", err)
	}
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	// Timestamps are UTC everywhere (data-model.md); pinning the session zone keeps what
	// comes back out of a tstzrange independent of the server's configuration.
	cfg.ConnConfig.RuntimeParams["timezone"] = "UTC"
	if _, ok := cfg.ConnConfig.RuntimeParams["application_name"]; !ok {
		cfg.ConnConfig.RuntimeParams["application_name"] = "sre-agent"
	}
	cfg.AfterConnect = func(_ context.Context, conn *pgx.Conn) error {
		registerTypes(conn.TypeMap())
		return nil
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Pool exposes the underlying connection pool for packages that issue their own queries.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Close releases every pooled connection. It is idempotent.
func (s *Store) Close() {
	if s != nil && s.pool != nil {
		s.pool.Close()
	}
}

// WithTx runs fn inside a transaction, committing when fn returns nil and rolling back
// otherwise. fn must not commit or roll back the transaction itself.
func (s *Store) WithTx(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		return fn(ctx, tx)
	})
}

// Migrate applies every embedded migration that has not been applied yet, in ascending
// version order, each in its own transaction together with its bookkeeping row in
// log.schema_migrations. Applying an already-migrated database is a no-op.
//
// A migration whose file changed after it was applied is an error, not a silent skip: the
// recorded checksum no longer matches and Migrate refuses to continue.
func (s *Store) Migrate(ctx context.Context) error {
	migrations, err := Migrations()
	if err != nil {
		return err
	}

	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("postgres: acquire connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrateLockKey); err != nil {
		return fmt.Errorf("postgres: take migration lock: %w", err)
	}
	defer func() {
		// Best effort: the lock is released with the session in any case.
		_, _ = conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", migrateLockKey)
	}()

	if _, err := conn.Exec(ctx, schemaMigrationsDDL); err != nil {
		return fmt.Errorf("postgres: create log.schema_migrations: %w", err)
	}

	applied, err := appliedChecksums(ctx, conn)
	if err != nil {
		return err
	}

	for _, m := range migrations {
		if have, ok := applied[m.Version]; ok {
			if have != m.Checksum {
				return fmt.Errorf(
					"postgres: migration %s was applied with checksum %s but the embedded file now hashes to %s; migrations are immutable once applied",
					m.Name, have, m.Checksum)
			}
			continue
		}
		if err := applyMigration(ctx, conn, m); err != nil {
			return err
		}
	}
	return nil
}

// AppliedVersions returns the migration versions recorded as applied, in ascending order. It
// returns an empty slice when the database has never been migrated.
func (s *Store) AppliedVersions(ctx context.Context) ([]int64, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT version FROM log.schema_migrations ORDER BY version`)
	if err != nil {
		return nil, fmt.Errorf("postgres: read applied migrations: %w", err)
	}
	versions, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return nil, fmt.Errorf("postgres: read applied migrations: %w", err)
	}
	return versions, nil
}

const schemaMigrationsDDL = `
CREATE SCHEMA IF NOT EXISTS log;
CREATE TABLE IF NOT EXISTS log.schema_migrations (
	version    bigint PRIMARY KEY,
	name       text NOT NULL,
	checksum   text NOT NULL,
	applied_at timestamptz NOT NULL DEFAULT now()
);`

func appliedChecksums(ctx context.Context, conn *pgxpool.Conn) (map[int64]string, error) {
	rows, err := conn.Query(ctx, `SELECT version, checksum FROM log.schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("postgres: read applied migrations: %w", err)
	}
	defer rows.Close()

	applied := map[int64]string{}
	for rows.Next() {
		var version int64
		var checksum string
		if err := rows.Scan(&version, &checksum); err != nil {
			return nil, fmt.Errorf("postgres: read applied migrations: %w", err)
		}
		applied[version] = checksum
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: read applied migrations: %w", err)
	}
	return applied, nil
}

func applyMigration(ctx context.Context, conn *pgxpool.Conn, m Migration) error {
	err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		// The file carries its own BEGIN/COMMIT so that it can also be piped straight into
		// psql; here the body runs inside the transaction that also records the version, so
		// a migration and its bookkeeping row commit together or not at all.
		if _, err := tx.Exec(ctx, stripOuterTransaction(m.SQL)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO log.schema_migrations (version, name, checksum) VALUES ($1, $2, $3)`,
			m.Version, m.Name, m.Checksum)
		return err
	})
	if err != nil {
		return fmt.Errorf("postgres: apply migration %s: %w", m.Name, err)
	}
	return nil
}

// Migrations returns the embedded migrations in ascending version order.
func Migrations() ([]Migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("postgres: read embedded migrations: %w", err)
	}

	migrations := make([]Migration, 0, len(entries))
	seen := map[int64]string{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		version, err := migrationVersion(entry.Name())
		if err != nil {
			return nil, err
		}
		if other, dup := seen[version]; dup {
			return nil, fmt.Errorf("postgres: migrations %s and %s share version %d", other, entry.Name(), version)
		}
		seen[version] = entry.Name()

		body, err := migrationFS.ReadFile(path.Join("migrations", entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("postgres: read embedded migration %s: %w", entry.Name(), err)
		}
		sum := sha256.Sum256(body)
		migrations = append(migrations, Migration{
			Version:  version,
			Name:     entry.Name(),
			SQL:      string(body),
			Checksum: hex.EncodeToString(sum[:]),
		})
	}
	if len(migrations) == 0 {
		return nil, errors.New("postgres: no embedded migrations found")
	}
	slices.SortFunc(migrations, func(a, b Migration) int {
		return cmp.Compare(a.Version, b.Version)
	})
	return migrations, nil
}

// migrationVersion parses the numeric prefix of a migration file name: "0001_log.sql" is 1.
func migrationVersion(name string) (int64, error) {
	prefix, _, ok := strings.Cut(name, "_")
	if !ok {
		return 0, fmt.Errorf("postgres: migration %q must be named <version>_<name>.sql", name)
	}
	version, err := strconv.ParseInt(prefix, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("postgres: migration %q has a non-numeric version prefix: %w", name, err)
	}
	if version <= 0 {
		return 0, fmt.Errorf("postgres: migration %q must have a positive version", name)
	}
	return version, nil
}

// stripOuterTransaction removes the file's own outermost BEGIN;/COMMIT; so the statements can
// be replayed inside a transaction the runner controls. Only whole lines that are exactly
// "BEGIN;" or "COMMIT;" are removed, which leaves the BEGIN/END of a plpgsql body untouched.
func stripOuterTransaction(sql string) string {
	lines := strings.Split(sql, "\n")
	first, last := -1, -1
	for i, line := range lines {
		switch strings.ToUpper(strings.TrimSpace(line)) {
		case "BEGIN;":
			if first < 0 {
				first = i
			}
		case "COMMIT;":
			last = i
		}
	}
	if first < 0 || last <= first {
		return sql
	}
	lines[first] = ""
	lines[last] = ""
	return strings.Join(lines, "\n")
}
