// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/types/known/timestamppb"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/pkg/backend/onset"
)

// `onset`, served backend-side (T099; FR-087a, FR-109, constitution IV).
//
// Cloud Monitoring returns the aligned series **to this process**, which calls `pkg/backend/onset`
// — the shipped estimator, not a reimplementation — and returns only the `OnsetDigest`: the
// estimated instant, its uncertainty, the method and its parameters.
//
// **The raw samples do not cross the digest boundary in either direction.** They arrive here and
// they stop here: they appear in no response field, no exemplar and no coverage field, and the
// only thing this function can return is the estimate. That is the shape constitution IV asks for
// where a method genuinely needs the series — the computation moves to the data's side of the
// boundary, and what crosses is the evidence.
//
// A crossing that does not clear the published effect-size and persistence criteria is
// `unavailable` with `no_onset_detected`, and **never** the earliest crossing. An instant that
// describes nothing would key every downstream causal-ordering decision to noise, and a
// causal ordering built on noise is confidently wrong rather than visibly absent.

// onsetMethodName is the published method identifier, carried in the digest so that a reader knows
// which estimator produced the instant and a recorded world stops replaying when it changes.
func onsetMethodName() string { return onset.MethodName }

// onset answers the term.
func (b *Backend) onset(ctx context.Context, term *investigationv1.OnsetTerm) (answer, error) {
	selector := term.GetPointer().GetSelector()
	facts := parseSelector(selector)
	window, narrowed := narrow(term.GetSearchWindow(), WindowCapOnset)

	if b.transport == nil || b.transport.Metrics == nil {
		outcome, err := b.absent("cloud_monitoring:"+facts.MetricType,
			b.transport.absentSourceOf("Cloud Monitoring"), window)
		return answer{outcome: outcome, query: selector, vocabulary: VocabMonitoring}, err
	}

	// One reduced series: the estimator answers "when did THIS depart from normal", and a
	// grouped query would hand it several series and a choice it has no basis to make.
	agg := aggregationFor(investigationv1.Statistic_MEAN, window, nil)
	series, err := b.queryMetrics(ctx, facts.scope(b.project), selector, window, agg)
	if err != nil {
		return b.queryFailed(err, selector, VocabMonitoring)
	}
	if len(series) == 0 {
		return b.emptyMetricAnswer(ctx, facts, selector, window, agg)
	}

	points := make([]onset.Point, 0, 256)
	for _, s := range series {
		values, instants := seriesValues(s)
		for i := range values {
			points = append(points, onset.Point{At: instants[i], Value: values[i]})
		}
	}

	estimate := onset.EstimateOnset(points, onset.Window{
		Start: window.GetStart().AsTime().UTC(),
		End:   window.GetEnd().AsTime().UTC(),
	}, onset.Params{})

	digest := &investigationv1.OnsetDigest{
		Method:           investigationv1.OnsetMethod_SEASONAL_CUSUM,
		MethodParameters: estimate.Params.Map(),
		Examined: &engine.Window{
			Start: timestamppb.New(estimate.Examined.Start),
			End:   timestamppb.New(estimate.Examined.End),
		},
	}
	if estimate.Unavailable {
		digest.Unavailable = true
		digest.UnavailableReason = estimate.Reason
	} else {
		digest.EstimatedOnset = timestamppb.New(estimate.At)
		digest.UncertaintySeconds = estimate.UncertaintySeconds
	}

	criteria := b.metricCriteria(facts, investigationv1.Statistic_STATISTIC_UNSPECIFIED)
	if narrowed {
		criteria = append(criteria, criterion(CriterionWindowCap, fmt.Sprintf(
			"the onset search window is capped at %s and the request was wider; the most recent "+
				"%s was searched, so an onset earlier than that would not be found here",
			WindowCapOnset, WindowCapOnset)))
	}
	lag, lagSource := b.metricLag(ctx, facts.MetricType)
	coverage, err := b.coverage(coverageInput{
		Entities:   entitiesOf(facts),
		DataSource: "cloud_monitoring:" + facts.MetricType,
		Window:     window,
		// The point count is how much was examined, which is a fact about the search. The
		// points themselves are not in the digest and are not in this number's neighbourhood:
		// a count is not a sample.
		Volume:    int64(len(points)),
		Sampling:  agg.Describe + ";estimator=" + onsetMethodName() + "/" + onset.Version,
		Criteria:  criteria,
		Lag:       lag,
		LagSource: lagSource,
	})
	if err != nil {
		return answer{}, err
	}
	return answer{
		outcome: engine.DigestOutcome{Body: &investigationv1.Digest{
			Body:     &investigationv1.Digest_Onset{Onset: digest},
			Coverage: coverage,
		}},
		query:      selector,
		vocabulary: VocabMonitoring,
		deepLink:   b.deepLink(handleMetric, selector, window, facts),
	}, nil
}
