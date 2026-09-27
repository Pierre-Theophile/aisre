// SPDX-License-Identifier: Apache-2.0

package datadog

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/datadogx"
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	sdk "github.com/Pierre-Theophile/aisre/pkg/backend"
	"github.com/Pierre-Theophile/aisre/pkg/backend/onset"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// `onset`: the instant error-level lines began to rise, estimated here with the published method so
// the series never crosses the digest boundary (contract §2; constitution IV).
//
// The series is one aggregate timeseries of error-level line counts at onsetInterval. Datadog omits
// empty intervals, so the series is zero-filled over the window before the estimate: a quiet minute
// is a count of zero, not a missing sample, and leaving it out would move the baseline.

const onsetInterval = time.Minute

func (b *Backend) onset(ctx context.Context, term *investigationv1.OnsetTerm) (answer, error) {
	raw := term.GetPointer().GetSelector()
	sel, err := parseSelector(raw)
	if err != nil {
		return unsupported(raw, err), nil
	}
	window, narrowed := narrow(term.GetSearchWindow(), WindowCapOnset)
	req := datadogx.AggregateRequest{
		Compute: []datadogx.Compute{{Aggregation: "count", Type: "timeseries", Interval: "1m"}},
		Filter:  filterOf(sel, window, b.indexes, "status:"+errorStatus),
	}
	query := canonical(req)
	out, resp, err := b.client.AggregateLogs(ctx, req)
	if err != nil {
		return failed(err, query)
	}

	start, end := window.GetStart().AsTime().UTC().Truncate(onsetInterval), window.GetEnd().AsTime().UTC()
	counts := map[time.Time]float64{}
	var considered int64
	for _, bucket := range out.Data.Buckets {
		series, _ := bucket.Series(0)
		for _, p := range series {
			at := p.Time.UTC().Truncate(onsetInterval)
			counts[at] += p.Value
			considered += int64(p.Value)
		}
	}
	points := make([]onset.Point, 0, int(end.Sub(start)/onsetInterval)+1)
	for at := start; at.Before(end); at = at.Add(onsetInterval) {
		points = append(points, onset.Point{At: at, Value: counts[at]})
	}

	estimate := onset.EstimateOnset(points, onset.Window{Start: window.GetStart().AsTime().UTC(), End: end}, onset.Params{})
	digest := &investigationv1.OnsetDigest{
		Method:           investigationv1.OnsetMethod_SEASONAL_CUSUM,
		MethodParameters: estimate.Params.Map(),
		Examined: &engine.Window{Start: timestamppb.New(estimate.Examined.Start),
			End: timestamppb.New(estimate.Examined.End)},
	}
	if estimate.Unavailable {
		digest.Unavailable, digest.UnavailableReason = true, estimate.Reason
	} else {
		digest.EstimatedOnset = timestamppb.New(estimate.At)
		digest.UncertaintySeconds = estimate.UncertaintySeconds
	}

	truncation := "log_derived: the series is the count of status:" + errorStatus + " lines per minute, zero-filled"
	if narrowed {
		truncation = joinTruncation(truncation, narrowedText(sdk.TermOnset, WindowCapOnset)+
			", so an onset earlier than that would not be found here")
	}
	if out.Meta.Partial() {
		truncation = joinTruncation(truncation, "partial: Datadog reported a timeout or warnings for this aggregation")
	}
	coverage, err := b.coverageOf(sel, window, considered,
		fmt.Sprintf("aggregate: count per %s; estimator=%s/%s", onsetInterval, onset.MethodName, onset.Version),
		truncation, resp)
	if err != nil {
		return answer{}, err
	}
	body := &investigationv1.Digest{Body: &investigationv1.Digest_Onset{Onset: digest}, Coverage: coverage}
	var outcome engine.Outcome = engine.DigestOutcome{Body: body}
	if out.Meta.Partial() {
		outcome = engine.Partial{Body: body, Missing: "Datadog reported a timeout or warnings, so some minutes may be undercounted"}
	}
	return answer{outcome: outcome, query: query, vocabulary: feeder.VocabDatadogLogs,
		deepLink: b.deepLink(sel.query("status:"+errorStatus), window)}, nil
}
