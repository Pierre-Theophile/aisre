// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"fmt"
)

// RecordPrincipal upserts the individual behind a human decision (data-model.md
// §graph.principals). `first_seen` is kept, `last_seen` moves, an empty email or display name
// never erases a known one, and the roles recorded are the ones granted for this call.
func (s *Store) RecordPrincipal(ctx context.Context, principal, email, displayName string, roles []string) error {
	if roles == nil {
		roles = []string{}
	}
	_, err := s.Pool().Exec(ctx, `
		INSERT INTO graph.principals (principal, email, display_name, roles)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (principal) DO UPDATE SET
			email = coalesce(nullif(EXCLUDED.email, ''), graph.principals.email),
			display_name = coalesce(nullif(EXCLUDED.display_name, ''), graph.principals.display_name),
			roles = EXCLUDED.roles,
			last_seen = now()`,
		principal, email, displayName, roles)
	if err != nil {
		return fmt.Errorf("postgres: record principal %s: %w", principal, err)
	}
	return nil
}
