// SPDX-License-Identifier: Apache-2.0

package projector

import (
	"context"
	"fmt"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/telemetry"
)

// The two observable gauges, answered from the graph (FR-051, FR-052, plan.md §Observability).
//
// `events_applied_total` and friends are recorded as the events go past (apply.go). The other
// two instruments are not events at all — they are questions about the current state of the
// graph — so they are observables, called on every metric collection, and this file is where
// they get their answer.
//
// Both read the graph rather than a counter held in memory, which is what makes them survive a
// restart and a replay: a process that came up a second ago reports the same pending-suggestion
// count and the same feeder lag as the one it replaced, because the answer was never in the
// process.

// PendingSuggestions counts the probable matches awaiting a human decision (FR-033). It is the
// callback behind the `suggestions_pending` gauge.
func (p *Projector) PendingSuggestions(ctx context.Context) (int64, error) {
	var pending int64
	err := p.store.Pool().QueryRow(ctx,
		`SELECT count(*) FROM graph.suggestions WHERE status = 'pending'`).Scan(&pending)
	if err != nil {
		return 0, fmt.Errorf("projector: count pending suggestions: %w", err)
	}
	return pending, nil
}

// FeederLag reports, per registered source, how far behind the graph is: the age of the source's
// last checkpoint (FR-052), falling back to the observed time of its last event for a source
// that has not checkpointed yet. It is a callback behind the `feeder_lag_seconds` gauge.
//
// This is the graph's view of lag, and it is the one an operator can alert on without the
// feeder being reachable: a connector that has died stops moving its checkpoint and the gauge
// climbs. A feeder that is running also pushes its own, finer measurement — the OTLP one
// reports the age of its last closed window — through Metrics.SetFeederLag; the two answer the
// same question from the two ends of the pipe.
func (p *Projector) FeederLag(ctx context.Context) ([]telemetry.FeederLag, error) {
	// Two scalar sub-selects rather than two LEFT JOINs: joining log.events on a source with a
	// million rows and then aggregating would build the cross product of events and checkpoints
	// before collapsing it, and this callback runs on every metric collection.
	rows, err := p.store.Pool().Query(ctx, `
		SELECT s.source_id,
		       GREATEST(
		           COALESCE((SELECT max(COALESCE(c.extent_to, c.checkpoint_at))
		                     FROM log.checkpoints c WHERE c.source_id = s.source_id), to_timestamp(0)),
		           COALESCE((SELECT max(e.observed_at)
		                     FROM log.events e WHERE e.source_id = s.source_id), to_timestamp(0))
		       ) AS last_known
		FROM log.sources s
		ORDER BY s.source_id`)
	if err != nil {
		return nil, fmt.Errorf("projector: read feeder lag: %w", err)
	}
	defer rows.Close()

	now := time.Now().UTC()
	var lags []telemetry.FeederLag
	for rows.Next() {
		var (
			sourceID string
			lastKnwn time.Time
		)
		if err := rows.Scan(&sourceID, &lastKnwn); err != nil {
			return nil, fmt.Errorf("projector: read feeder lag: %w", err)
		}
		if lastKnwn.Unix() <= 0 {
			// A source that registered and has said nothing yet has no lag to report; a
			// fabricated one would page somebody about a feeder that was never started.
			continue
		}
		lag := now.Sub(lastKnwn).Seconds()
		if lag < 0 {
			// A source may assert coverage slightly into the future (a window that has been
			// closed ahead of the wall clock). Negative lag is not a thing an operator can act
			// on, so it is reported as caught up.
			lag = 0
		}
		lags = append(lags, telemetry.FeederLag{Source: sourceID, Seconds: lag})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("projector: read feeder lag: %w", err)
	}
	return lags, nil
}

// RegisterObservables wires both gauges to this projector. The server calls it at startup; a
// benchmark or a test calls it after building its own Metrics.
func (p *Projector) RegisterObservables(m *telemetry.Metrics) {
	if m == nil {
		return
	}
	m.RegisterSuggestionsPending(p.PendingSuggestions)
	m.RegisterFeederLag(p.FeederLag)
}
