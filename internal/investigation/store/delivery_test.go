// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/investigation/render"
	investigationstore "github.com/Pierre-Theophile/aisre/internal/investigation/store"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres/pgtest"
)

// The delivery ledger and the delivered message have to be the same thing (T116, FR-057f,
// constitution VII v1.1.0).
//
// `lifecycle_test.go` covers what the row does — upsert in place, update_count, a failure
// recorded while the investigation still concludes. What is asserted here is the *seam*: the
// renderer decides which message it edits, the store decides which row it updates, and if the two
// disagree then "one target, one message" is true of one of them and false of the other.

// TestDeliveryIdentityMatchesTheLedgerRowIdentity pins the two derivations to each other. Neither
// package imports the other's identity function — render must not depend on a database and the
// store must not depend on a transport — so the agreement is a test rather than a call.
func TestDeliveryIdentityMatchesTheLedgerRowIdentity(t *testing.T) {
	t.Parallel()

	cases := [][3]string{
		{"inv-01", "slack:acme", "C0123"},
		{"inv-02", "slack:acme", "C0123"}, // same place, another investigation: another message
		{"inv-01", "jira:acme", "OPS-42"},
		{"inv-01", "", ""},
	}
	seen := map[string]string{}
	for _, c := range cases {
		row := investigationstore.DeliveryIDFor(c[0], c[1], c[2])
		message := render.DeliveryIdentity(c[0], c[1], c[2])
		if row != message {
			t.Errorf("investigation %q at %s/%s: ledger row %q, delivered message %q — the row "+
				"and the message it describes must be the same thing", c[0], c[1], c[2], row, message)
		}
		if other, dup := seen[row]; dup {
			t.Errorf("identity %q is shared by %v and %v", row, other, c)
		}
		seen[row] = c[0]
	}
}

// TestEveryOutcomeTheRendererProducesIsStorable is the other half of the seam. `render.Deliver`
// may return exactly three outcomes and the ledger accepts exactly three; a fourth on either side
// — the zero-valued result a panicking connector used to produce before T116, say — is a delivery
// that happened and was never recorded.
func TestEveryOutcomeTheRendererProducesIsStorable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := pgtest.Open(t)

	dao := newLifecycleFixture(ctx, t, store, "inv-delivery-outcomes")

	for _, outcome := range []string{
		render.OutcomeDelivered, render.OutcomeFailed, render.OutcomeNotConfigured,
	} {
		rec := investigationstore.DeliveryRecord{
			InvestigationID: "inv-delivery-outcomes", TargetSystem: "slack:acme",
			TargetRef: "C0OUTCOME", RenderingDigest: "sha256:aaa", Outcome: outcome,
			At: lifecycleAt,
		}
		if outcome == render.OutcomeFailed {
			rec.FailureDetail = "the chat connector returned 503"
		}
		if _, err := dao.RecordDelivery(ctx, rec); err != nil {
			t.Errorf("the renderer can produce outcome %q and the ledger refuses it: %v", outcome, err)
		}
	}

	// The zero value is not one of them, and must not be written as if it were an outcome.
	if _, err := dao.RecordDelivery(ctx, investigationstore.DeliveryRecord{
		InvestigationID: "inv-delivery-outcomes", TargetSystem: "slack:acme",
		TargetRef: "C0OUTCOME", RenderingDigest: "sha256:bbb", At: lifecycleAt.Add(time.Minute),
	}); err == nil {
		t.Error("an empty outcome was written to the ledger; a delivery with no outcome is not a record")
	}
}
