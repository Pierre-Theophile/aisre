// SPDX-License-Identifier: Apache-2.0

// Package fixturetest hands a test the disposable databases fixture.Verify needs.
//
// It lives beside the fixture package rather than inside it so that production code never links
// testcontainers or embedded-postgres through pgtest: only test binaries import this package
// (the same reason pgtest itself is separate from the store).
//
// A package whose tests call PgtestFactory needs pgtest's TestMain, so the shared server is
// started once for the whole binary instead of once per verification:
//
//	func TestMain(m *testing.M) { pgtest.TestMain(m) }
package fixturetest

import (
	"context"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/fixture"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres/pgtest"
)

// PgtestFactory returns a fixture.StoreFactory backed by pgtest: every call creates a fresh,
// migrated database on the test binary's shared server.
//
// The returned cleanup closes the store's pool as soon as the verifier is finished with that
// pass; dropping the database is left to pgtest's own t.Cleanup, which runs when the test ends.
// Verification therefore holds one database per pass until then — a handful, not one per event.
func PgtestFactory(t *testing.T) fixture.StoreFactory {
	t.Helper()
	return func(_ context.Context) (*postgres.Store, func(), error) {
		store := pgtest.Open(t)
		return store, store.Close, nil
	}
}
