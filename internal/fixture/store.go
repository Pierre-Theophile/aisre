// SPDX-License-Identifier: Apache-2.0

package fixture

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
)

// Disposable databases for verification.
//
// A verification run needs one empty, migrated database per pass and must not be able to touch
// anything else: `aisre fixture verify` is meant to be runnable by an operator against the
// same PostgreSQL server that holds the real graph, and a harness that could write into it
// would be unusable there. NewStoreFactoryFromDSN therefore never opens the database named in
// the DSN for anything but CREATE DATABASE and DROP DATABASE.
//
// The naming and drop mechanics deliberately mirror internal/store/postgres/pgtest, which does
// the same job for tests. They are not shared: pgtest links testcontainers and
// embedded-postgres, and no production binary may depend on those (constitution X).

// verifyDatabasePrefix is the prefix of every database this factory creates, so an abandoned
// one is recognizable and safe to drop by hand.
const verifyDatabasePrefix = "sre_verify_"

// adminTimeout bounds a CREATE/DROP DATABASE round trip.
const adminTimeout = 60 * time.Second

// NewStoreFactoryFromDSN returns a StoreFactory that creates a uniquely named database on the
// server behind dsn, migrates it, and drops it again when the verifier is done with it.
//
// The role in dsn needs the CREATEDB privilege; the database it names is used only as the
// maintenance connection CREATE DATABASE and DROP DATABASE are issued on, and its contents are
// never read or written. The drop uses WITH (FORCE) so that a leaked connection cannot leave a
// stale database behind to collide with the next run.
func NewStoreFactoryFromDSN(dsn string) StoreFactory {
	return func(ctx context.Context) (*postgres.Store, func(), error) {
		name, err := uniqueDatabaseName()
		if err != nil {
			return nil, nil, err
		}
		if err := adminExec(ctx, dsn, "CREATE DATABASE "+quoteIdent(name)); err != nil {
			return nil, nil, fmt.Errorf("fixture: create verification database %s (the dsn's role needs CREATEDB): %w", name, err)
		}

		drop := func() {
			// context.WithoutCancel: the database has to go even when the run was
			// cancelled, or the next run collides with it.
			sql := "DROP DATABASE IF EXISTS " + quoteIdent(name) + " WITH (FORCE)"
			_ = adminExec(context.WithoutCancel(ctx), dsn, sql)
		}

		target, err := withDatabase(dsn, name)
		if err != nil {
			drop()
			return nil, nil, err
		}
		store, err := postgres.Open(ctx, target)
		if err != nil {
			drop()
			return nil, nil, fmt.Errorf("fixture: open verification database %s: %w", name, err)
		}
		if err := store.Migrate(ctx); err != nil {
			store.Close()
			drop()
			return nil, nil, fmt.Errorf("fixture: migrate verification database %s: %w", name, err)
		}

		return store, func() {
			store.Close()
			drop()
		}, nil
	}
}

func uniqueDatabaseName() (string, error) {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("fixture: name verification database: %w", err)
	}
	return verifyDatabasePrefix + hex.EncodeToString(suffix[:]), nil
}

// adminExec runs one statement on the DSN's maintenance database. CREATE and DROP DATABASE
// cannot run inside a transaction, so this uses a bare connection rather than the pool.
func adminExec(ctx context.Context, dsn, sql string) error {
	ctx, cancel := context.WithTimeout(ctx, adminTimeout)
	defer cancel()

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()

	if _, err := conn.Exec(ctx, sql); err != nil {
		return fmt.Errorf("exec %q: %w", sql, err)
	}
	return nil
}

// withDatabase rewrites a DSN to point at another database, supporting both the URL form
// ("postgres://...") and the keyword/value form ("host=... dbname=...").
func withDatabase(dsn, name string) (string, error) {
	if u, err := url.Parse(dsn); err == nil && (u.Scheme == "postgres" || u.Scheme == "postgresql") {
		u.Path = "/" + name
		return u.String(), nil
	}
	fields := strings.Fields(dsn)
	rewritten := make([]string, 0, len(fields)+1)
	for _, field := range fields {
		if strings.HasPrefix(field, "dbname=") {
			continue
		}
		rewritten = append(rewritten, field)
	}
	if len(rewritten) == 0 {
		return "", fmt.Errorf("fixture: cannot rewrite dsn %q to another database", dsn)
	}
	return strings.Join(append(rewritten, "dbname="+name), " "), nil
}

// quoteIdent renders a SQL identifier. The names this file quotes are generated, but quoting is
// not conditional on that: an unquoted identifier built by string concatenation is the kind of
// thing that stops being safe the day someone makes the name configurable.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
