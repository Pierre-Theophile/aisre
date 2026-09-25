// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"fmt"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	sdk "github.com/Pierre-Theophile/aisre/pkg/backend"
)

// Retention: too old is not empty (T106, FR-104; contract §6).
//
// `NO_DATA` means *the query was valid, the window was covered, and there is nothing in it* — and
// it is the only outcome that is evidence nothing happened. A window older than the vendor's
// retention was **never covered**: the data aged out before anyone asked. Reporting it as NO_DATA
// would tell an investigation that nothing happened during exactly the window it cannot see, which
// is the most confident wrong answer this system is capable of.
//
// `run.googleapis.com/*` retains for six weeks, so this bites immediately rather than
// theoretically: any question about an incident older than that is outside retention, not empty.
//
// The refusal carries the horizon in `AlgebraResponse.retention_horizon`. Without it the caller
// learns only "not this window" and discovers the boundary by bisection, spending quota to find a
// number the vendor already published; with it, one refusal says how far back it *could* have
// asked.

// retentionFor is the horizon a term is bounded by: the metric retention for the terms served by
// Cloud Monitoring, the log retention for those served by Cloud Logging.
//
// `error_spans` has neither, and answers NO_DATA naming the absent trace source long before
// retention could matter; `drill_down` and `exemplars` carry a handle minted by an answer that was
// itself checked. Both are excluded here rather than given an arbitrary horizon.
func (b *Backend) retentionFor(term string) (time.Duration, bool) {
	switch term {
	case sdk.TermCompare, sdk.TermErrorsByVersion, sdk.TermOnset, sdk.TermMonitorState:
		return b.retention.metrics, true
	case sdk.TermNewLogPatterns:
		return b.retention.logs, true
	default:
		return 0, false
	}
}

// checkRetention refuses a term reaching back past what the vendor states it holds.
//
// The test is on the window's **start**: a window that begins outside retention and ends inside it
// is still a window the vendor cannot serve in full, and answering the part it can serve under the
// name of the whole would report a comparison against a baseline that was silently shortened. The
// caller is told the horizon and asks again.
func (b *Backend) checkRetention(req *engine.Request, name string) (*engine.Response, bool, error) {
	window, ok := b.retentionFor(name)
	if !ok {
		return nil, false, nil
	}
	horizon := b.now().UTC().Add(-window)

	var earliest time.Time
	for _, w := range windowsOf(req.GetTerm()) {
		if w.GetStart() == nil {
			continue
		}
		start := w.GetStart().AsTime().UTC()
		if earliest.IsZero() || start.Before(earliest) {
			earliest = start
		}
	}
	if earliest.IsZero() || !earliest.Before(horizon) {
		return nil, false, nil
	}

	resp, err := b.refuse(req, investigationv1.FailureReason_OUTSIDE_RETENTION, fmt.Sprintf(
		"gcp: %s asks about %s, and this vendor states it holds %s of data — back to %s. The "+
			"window was never covered, so this is not NO_DATA: nothing can be concluded about "+
			"what happened in it",
		name, earliest.Format(time.RFC3339), window, horizon.Format(time.RFC3339)))
	if err != nil {
		return nil, true, err
	}
	// The horizon rides on the response rather than only in the prose, so a caller re-asks by
	// reading a field instead of parsing a sentence. The response digest is recomputed over it:
	// a digest taken before the field was set would not cover what actually leaves the process.
	resp.RetentionHorizon = timestamppb.New(horizon)
	digest, err := sdk.ResponseDigest(resp)
	if err != nil {
		return nil, true, err
	}
	resp.ResponseDigest = digest
	return resp, true, nil
}

// RetentionHorizonOf is what a caller reads off a refusal. It exists so that a consumer does not
// reach into the response for a field whose meaning depends on the failure reason: a zero instant
// on any other outcome is not a horizon of zero, it is no statement at all.
func RetentionHorizonOf(resp *engine.Response) (time.Time, bool) {
	if resp.GetFailureReason() != investigationv1.FailureReason_OUTSIDE_RETENTION {
		return time.Time{}, false
	}
	if resp.GetRetentionHorizon() == nil {
		return time.Time{}, false
	}
	return resp.GetRetentionHorizon().AsTime().UTC(), true
}
