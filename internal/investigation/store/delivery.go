// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
)

// The report-delivery ledger (T087, FR-057f, ADR-0005 D8, constitution VII v1.1.0).
//
// This table is the record of the project's only write outside its own stores. What it stores is
// deliberately small: where the report went, the digest of what was delivered, the external
// reference to edit next time, when it was first delivered and last updated, how many times, and
// the outcome. Not the report body — that is a rendering of an immutable investigation and is
// re-derivable — and not the credential.
//
// Two invariants live here rather than in the delivery code, because they must hold however the
// delivery was attempted:
//
//   - **One row per (investigation, target).** A delivery is *edited in place*, never re-posted
//     as a stream, so a second delivery to the same place is an UPDATE with `update_count + 1`
//     and the same `external_message_ref`. An INSERT per attempt would make the row the stream
//     the FR forbids.
//   - **A failure is recorded and the investigation still concludes.** RecordDelivery with
//     outcome `failed` is an ordinary write; nothing here touches the investigation's lifecycle.
//     The only way delivery could block a conclusion would be for the conclusion path to call it,
//     and it does not (ConcludeInTx is in lifecycle.go and mentions no delivery).

// Delivery outcomes, exactly as `investigation.report_deliveries.outcome` accepts them.
const (
	// DeliveryDelivered is a report that reached its place.
	DeliveryDelivered = "delivered"
	// DeliveryFailed is one that did not. It is recorded; the investigation still concludes.
	DeliveryFailed = "failed"
	// DeliveryNotConfigured is the ordinary case with no connector: there is nowhere to post,
	// which is not a failure of anything.
	DeliveryNotConfigured = "not_configured"
)

// ErrDeliveryFailureNeedsDetail is a failed delivery with no detail. A failure nobody can read is
// not a record of anything.
var ErrDeliveryFailureNeedsDetail = errors.New("a failed delivery must record why (FR-057f)")

// DeliveryRecord is one attempt to return a report to the place the incident lives.
type DeliveryRecord struct {
	// DeliveryID is the row's identifier; DeliveryIDFor derives the canonical one.
	DeliveryID string
	// InvestigationID is the investigation whose rendering was delivered.
	InvestigationID string
	// TargetSystem and TargetRef are the stable identifier of the place.
	TargetSystem string
	TargetRef    string
	// ExternalMessageRef is what is edited in place on the next update. Empty until the first
	// successful delivery mints one.
	ExternalMessageRef string
	// RenderingDigest is sha256 of the rendered report that was delivered.
	RenderingDigest string
	// Outcome is one of the three above.
	Outcome string
	// FailureDetail is required when Outcome is DeliveryFailed.
	FailureDetail string
	// At is when the attempt happened. Zero means now.
	At time.Time
}

// DeliveryIDFor is the canonical delivery id: one row per (investigation, target system, target
// ref), which is what makes "edited in place" representable.
func DeliveryIDFor(investigationID, targetSystem, targetRef string) string {
	return "delivery-" + investigationID + "-" + targetSystem + "-" + targetRef
}

// RecordDelivery writes or updates the delivery row (FR-057f).
//
// It is an upsert on the canonical id: the first call inserts with `update_count = 1`, every
// later call to the same place updates in place and increments. `first_delivered_at` is set once
// and never moved, so "when did the on-call first see this?" survives every later edit.
func (d *InvestigationDAO) RecordDelivery(ctx context.Context, rec DeliveryRecord) (*investigationv1.ReportDelivery, error) {
	if d == nil || d.store == nil {
		return nil, errors.New("investigation store: record delivery: no database")
	}
	switch rec.Outcome {
	case DeliveryDelivered, DeliveryNotConfigured:
	case DeliveryFailed:
		if rec.FailureDetail == "" {
			return nil, fmt.Errorf("investigation store: record delivery: %w", ErrDeliveryFailureNeedsDetail)
		}
	default:
		return nil, fmt.Errorf("investigation store: record delivery: outcome %q: want %s, %s or %s",
			rec.Outcome, DeliveryDelivered, DeliveryFailed, DeliveryNotConfigured)
	}
	if rec.DeliveryID == "" {
		rec.DeliveryID = DeliveryIDFor(rec.InvestigationID, rec.TargetSystem, rec.TargetRef)
	}
	at := rec.At
	if at.IsZero() {
		at = time.Now().UTC()
	}
	// `first_delivered_at` is only meaningful for a delivery that happened.
	var firstAt any
	if rec.Outcome == DeliveryDelivered {
		firstAt = at.UTC()
	}

	var out *investigationv1.ReportDelivery
	err := d.store.WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			INSERT INTO investigation.report_deliveries (
				delivery_id, investigation_id, target_system, target_ref, external_message_ref,
				rendering_digest, first_delivered_at, last_updated_at, update_count, outcome,
				failure_detail)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 1, $9, $10)
			ON CONFLICT (delivery_id) DO UPDATE SET
				external_message_ref = coalesce(nullif(EXCLUDED.external_message_ref, ''),
				                                investigation.report_deliveries.external_message_ref),
				rendering_digest = EXCLUDED.rendering_digest,
				first_delivered_at = coalesce(investigation.report_deliveries.first_delivered_at,
				                              EXCLUDED.first_delivered_at),
				last_updated_at = EXCLUDED.last_updated_at,
				update_count = investigation.report_deliveries.update_count + 1,
				outcome = EXCLUDED.outcome,
				failure_detail = EXCLUDED.failure_detail
			RETURNING investigation_id, target_system, target_ref,
			          coalesce(external_message_ref, ''), rendering_digest,
			          first_delivered_at, last_updated_at, update_count, outcome,
			          coalesce(failure_detail, '')`,
			rec.DeliveryID, rec.InvestigationID, rec.TargetSystem, rec.TargetRef,
			rec.ExternalMessageRef, rec.RenderingDigest, firstAt, at.UTC(), rec.Outcome,
			nullable(rec.FailureDetail))
		var err error
		out, err = scanDelivery(row)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("investigation store: record delivery %s: %w", rec.DeliveryID, err)
	}
	return out, nil
}

// DeliveriesOf reads an investigation's deliveries.
func (d *InvestigationDAO) DeliveriesOf(ctx context.Context, investigationID string) ([]*investigationv1.ReportDelivery, error) {
	rows, err := d.store.Pool().Query(ctx, `
		SELECT investigation_id, target_system, target_ref, coalesce(external_message_ref, ''),
		       rendering_digest, first_delivered_at, last_updated_at, update_count, outcome,
		       coalesce(failure_detail, '')
		FROM investigation.report_deliveries WHERE investigation_id = $1
		ORDER BY target_system, target_ref`, investigationID)
	if err != nil {
		return nil, fmt.Errorf("investigation store: deliveries of %s: %w", investigationID, err)
	}
	defer rows.Close()

	var out []*investigationv1.ReportDelivery
	for rows.Next() {
		delivery, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, delivery)
	}
	return out, rows.Err()
}

type scanner interface{ Scan(dest ...any) error }

func scanDelivery(row scanner) (*investigationv1.ReportDelivery, error) {
	var (
		d           investigationv1.ReportDelivery
		first, last *time.Time
		updateCount int32
	)
	if err := row.Scan(&d.InvestigationId, &d.TargetSystem, &d.TargetRef,
		&d.ExternalMessageRef, &d.RenderingDigest, &first, &last, &updateCount,
		&d.Outcome, &d.FailureDetail); err != nil {
		return nil, fmt.Errorf("investigation store: scan delivery: %w", err)
	}
	if first != nil {
		d.FirstDeliveredAt = timestamppb.New(first.UTC())
	}
	if last != nil {
		d.LastUpdatedAt = timestamppb.New(last.UTC())
	}
	d.UpdateCount = uint32(updateCount) //nolint:gosec // update_count is CHECKed >= 0
	return &d, nil
}
