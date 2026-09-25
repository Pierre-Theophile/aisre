// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
)

// `migrate` (contracts/cli.md §Server and operations, constitution III).
//
// Migrations are embedded in the binary, applied in version order, each with its bookkeeping
// row in one transaction, and are immutable once applied: editing an applied file changes its
// checksum and the store refuses to continue rather than pretending the database matches the
// code. That refusal is the "refuses history-dropping migrations" guarantee in practice — the
// event log is append-only, and a migration that would drop it cannot be slipped in under an
// already-applied version.

type migrateOptions struct {
	dsn string
}

func newMigrateCommand(global *globalOptions) *cobra.Command {
	opts := &migrateOptions{}

	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Apply the embedded schema migrations",
		Long: "migrate applies every embedded migration the database has not seen yet, in version\n" +
			"order. Applying an already-migrated database is a no-op. A migration file that changed\n" +
			"after it was applied is an error, not a silent skip.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()

			dsn, err := dsnFrom(opts.dsn, os.Getenv(EnvDSN))
			if err != nil {
				return err
			}
			store, err := postgres.Open(ctx, dsn)
			if err != nil {
				return storeError("open database", err)
			}
			defer store.Close()

			before, err := store.AppliedVersions(ctx)
			if err != nil {
				// An unmigrated database has no bookkeeping table yet; that is the normal
				// first run, not a failure.
				before = nil
			}
			if err := store.Migrate(ctx); err != nil {
				return storeError("apply migrations", err)
			}
			after, err := store.AppliedVersions(ctx)
			if err != nil {
				return storeError("read applied migrations", err)
			}

			return renderMigrations(cmd, global, before, after)
		},
	}

	cmd.Flags().StringVar(&opts.dsn, "db", "", "PostgreSQL DSN (default $"+EnvDSN+")")
	return cmd
}

func renderMigrations(cmd *cobra.Command, global *globalOptions, before, after []int64) error {
	applied := map[int64]bool{}
	for _, v := range before {
		applied[v] = true
	}

	migrations, err := postgres.Migrations()
	if err != nil {
		return storeError("read embedded migrations", err)
	}
	names := map[int64]string{}
	for _, m := range migrations {
		names[m.Version] = m.Name
	}

	p := newPrinter(cmd.OutOrStdout(), global.Output)
	if p.json() {
		type row struct {
			Version int64  `json:"version"`
			Name    string `json:"name"`
			State   string `json:"state"`
		}
		out := struct {
			Applied    int   `json:"applied"`
			Migrations []row `json:"migrations"`
		}{}
		for _, v := range after {
			state := "already_applied"
			if !applied[v] {
				state = "applied"
				out.Applied++
			}
			out.Migrations = append(out.Migrations, row{Version: v, Name: names[v], State: state})
		}
		return p.writeJSON(out)
	}

	rows := make([][]string, 0, len(after))
	newly := 0
	for _, v := range after {
		state := "already applied"
		if !applied[v] {
			state = "applied"
			newly++
		}
		rows = append(rows, []string{strconv.FormatInt(v, 10), names[v], state})
	}
	if err := p.writeTable([]string{"VERSION", "NAME", "STATE"}, rows); err != nil {
		return err
	}
	return p.writeLine("\n%d migration(s) applied, %d already present", newly, len(after)-newly)
}
