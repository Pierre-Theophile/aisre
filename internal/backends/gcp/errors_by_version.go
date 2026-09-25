// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"fmt"
	"sort"
	"time"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	feedergcp "github.com/Pierre-Theophile/aisre/internal/feeders/gcp"
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
)

// `errors_by_version`: the error rate split by Cloud Run revision (T097, T098; FR-089).
//
// The grouping is `resource.labels.revision_name` with `resource.type = "cloud_run_revision"`
// **pinned in the selector**, and the pin is a correctness requirement rather than a style one.
// `run.googleapis.com/request_count` is written against `cloud_run_instance` as well, and that
// resource carries **no `revision_name` label**; an unpinned query silently merges or drops groups.
// It would not error. It would answer — and the answer would be wrong in the direction that
// exonerates a bad deploy.
//
// So an unpinned selector is refused here rather than executed, and the refusal names the pin.
// The feeder's `MetricPointer` refuses to mint one, and this is the other end of the same rule:
// a stored pointer that predates it, or one minted by something else, meets the same wall.
//
// The revision values returned are **join keys matching the WORKLOAD nodes in the graph**
// (`JoinKeys.version`), which is what lets "the new revision is erroring and the old one is not"
// be a fact the digest *states* rather than a comparison the reader has to make.

// errorsByVersion answers the term.
func (b *Backend) errorsByVersion(ctx context.Context, term *investigationv1.ErrorsByVersionTerm) (answer, error) {
	selector := term.GetPointer().GetSelector()
	facts := parseSelector(selector)
	window, narrowed := narrow(term.GetWindow(), WindowCapStandard)

	if facts.ResourceType != feedergcp.ResourceTypeCloudRunRevision {
		return answer{
			outcome: engine.QueryFailed{
				Reason: investigationv1.FailureReason_UNSUPPORTED_POINTER,
				Detail: fmt.Sprintf(
					"gcp: errors_by_version needs a selector pinning resource.type=%q and this one "+
						"pins %q. The same metric is written against %q, which carries no %s label, "+
						"so an unpinned query merges or drops groups without saying so — and the "+
						"wrong answer is the one that exonerates a bad deploy",
					feedergcp.ResourceTypeCloudRunRevision, facts.ResourceType,
					feedergcp.ResourceTypeCloudRunInstance, LabelRevisionName),
			},
			query:      selector,
			vocabulary: VocabMonitoring,
		}, nil
	}
	// The term names the attribute to split by, and this backend groups by exactly one field. A
	// term naming a different one is refused rather than answered: the grouping below would
	// ignore it, the digest would echo the name the term asked for, and the reader would be told
	// the error rate was split by a field it was not. That is the same confident-wrong-answer
	// shape the pin above exists to prevent, arriving by the other door.
	//
	// It sits with the pin and **before** the source check for the same reason the pin does: a
	// term this backend cannot honour is the caller's to fix whether or not Cloud Monitoring is
	// configured, and answering NO_DATA would tell them the source was missing when the request
	// was malformed.
	//
	// The empty string is a mismatch here like any other, and saying so costs nothing: the
	// algebra already refuses a term that names no attribute, pointing at
	// `Pointer.join_keys["version"]` as where one comes from, so an empty attribute does not
	// reach this function by the ordinary path.
	if named := term.GetVersionAttribute(); named != GroupByRevision {
		return answer{
			outcome: engine.QueryFailed{
				Reason: investigationv1.FailureReason_UNSUPPORTED_POINTER,
				Detail: fmt.Sprintf(
					"gcp: errors_by_version was asked to split by %q and this backend groups "+
						"Cloud Run series by %q, the only field that carries a revision. "+
						"Answering would report a breakdown by a field it was not grouped by",
					named, GroupByRevision),
			},
			query:      selector,
			vocabulary: VocabMonitoring,
		}, nil
	}
	if b.transport == nil || b.transport.Metrics == nil {
		outcome, err := b.absent("cloud_monitoring:"+facts.MetricType,
			b.transport.absentSourceOf("Cloud Monitoring"), window)
		return answer{outcome: outcome, query: selector, vocabulary: VocabMonitoring}, err
	}

	agg := aggregationFor(investigationv1.Statistic_COUNT, window, []string{
		GroupByRevision,
		"metric.labels." + LabelResponseCodeClass,
	})
	series, err := b.queryMetrics(ctx, facts.scope(b.project), selector, window, agg)
	if err != nil {
		return b.queryFailed(err, selector, VocabMonitoring)
	}
	if len(series) == 0 {
		return b.emptyMetricAnswer(ctx, facts, selector, window, agg)
	}

	type accumulator struct {
		total, errors float64
		firstSeen     time.Time
	}
	byRevision := map[string]*accumulator{}
	var considered int64
	for _, s := range series {
		revision := s.GetResource().GetLabels()[LabelRevisionName]
		if revision == "" {
			// A group with no revision is the `cloud_run_instance` series the pin exists to
			// keep out. Reaching here means the vendor returned one anyway; dropping it
			// silently is the defect, so it is counted and named in coverage below.
			continue
		}
		values, instants := seriesValues(s)
		considered += int64(len(values))
		acc, ok := byRevision[revision]
		if !ok {
			acc = &accumulator{}
			byRevision[revision] = acc
		}
		var sum float64
		for _, v := range values {
			sum = round6(sum + v)
		}
		acc.total = round6(acc.total + sum)
		if s.GetMetric().GetLabels()[LabelResponseCodeClass] == "5xx" {
			acc.errors = round6(acc.errors + sum)
		}
		if len(instants) > 0 && (acc.firstSeen.IsZero() || instants[0].Before(acc.firstSeen)) {
			acc.firstSeen = instants[0]
		}
	}

	revisions := make([]string, 0, len(byRevision))
	for revision := range byRevision {
		revisions = append(revisions, revision)
	}
	// Sorted by name here, which is the tiebreak rather than the presentation order: the
	// published ordering is applied when the response is built, and it puts the worst error rate
	// first so the reader's eye lands on the revision that is failing. Two revisions with the
	// same rate then order by name, which is what keeps two recordings of one world identical.
	sort.Strings(revisions)

	key := termKeyOf(engine.ErrorsByVersion(term.GetPointer(), term.GetWindow(), term.GetVersionAttribute()))
	rows := make([]*investigationv1.VersionBreakdown, 0, len(revisions))
	for _, revision := range revisions {
		acc := byRevision[revision]
		rate := 0.0
		if acc.total > 0 {
			rate = round6(acc.errors / acc.total)
		}
		rows = append(rows, &investigationv1.VersionBreakdown{
			Version:   revision,
			Errors:    int64(acc.errors),
			Total:     int64(acc.total),
			ErrorRate: rate,
			JoinKeys:  joinKeys(facts, revision, acc.firstSeen),
			DrillDown: b.mint(key, handleMetric, selector, revision, window, facts),
		})
	}

	criteria := b.metricCriteria(facts, investigationv1.Statistic_ERROR_RATE)
	if narrowed {
		criteria = append(criteria, criterion(CriterionWindowCap, fmt.Sprintf(
			"errors_by_version is capped at %s and the request was wider; the most recent %s was queried",
			WindowCapStandard, WindowCapStandard)))
	}
	lag, lagSource := b.metricLag(ctx, facts.MetricType)
	coverage, err := b.coverage(coverageInput{
		Entities:   entitiesOf(facts),
		DataSource: "cloud_monitoring:" + facts.MetricType,
		Window:     window,
		Volume:     considered,
		Sampling:   agg.Describe,
		Criteria:   criteria,
		Lag:        lag,
		LagSource:  lagSource,
	})
	if err != nil {
		return answer{}, err
	}

	// Anything other than GroupByRevision was refused above, so the field reported here is the
	// field the rows were actually grouped by rather than the name the term happened to use.
	versionAttribute := GroupByRevision
	return answer{
		outcome: engine.DigestOutcome{Body: &investigationv1.Digest{
			Body: &investigationv1.Digest_ErrorsByVersion{ErrorsByVersion: &investigationv1.ErrorsByVersionDigest{
				Versions:         rows,
				VersionAttribute: versionAttribute,
			}},
			Coverage: coverage,
		}},
		query:      selector,
		vocabulary: VocabMonitoring,
		deepLink:   b.deepLink(handleMetric, selector, window, facts),
	}, nil
}
