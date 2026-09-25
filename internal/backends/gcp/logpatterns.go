// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"fmt"
	"time"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/internal/investigation/workers/logs"
)

// `new_log_patterns`: mined client-side over a bounded sample (T100, T101; contract §4).
//
// The digest carries masked templates with counts and each template's first-seen instant (FR-090,
// FR-095) — **never raw entries**. The miner version is recorded, because a template set is a
// function of the miner: the same entries through a different miner are a different answer, and a
// recorded world that did not say which miner produced its templates could not be replayed.
//
// The three coverage obligations of contract §4 are discharged here and are not optional:
//
//	volume_considered       how many entries were actually examined
//	truncation              that a sample bound was reached, and WHICH criterion stopped it
//	window_actually_covered the sub-window the sample spans, where pagination stopped early
//
// And the outcome follows the pagination rule rather than the entry count. An unfinished search is
// PARTIAL with what is missing named — never NO_DATA, which would report that nothing happened
// because the vendor ran out of time.

// newLogPatterns answers the term.
func (b *Backend) newLogPatterns(ctx context.Context, term *investigationv1.NewLogPatternsTerm) (answer, error) {
	selector := term.GetPointer().GetSelector()
	facts := parseSelector(selector)
	window, narrowed := narrow(term.GetWindow(), WindowCapLogs)
	baselineWindow, baselineNarrowed := narrow(term.GetBaselineWindow(), WindowCapLogs)

	if b.transport == nil || b.transport.Logs == nil {
		outcome, err := b.absent("cloud_logging:projects/"+facts.scope(b.project),
			b.transport.absentSourceOf("Cloud Logging"), window)
		return answer{outcome: outcome, query: selector, vocabulary: VocabLogging}, err
	}

	sample, err := b.sampleLogs(ctx, facts.scope(b.project), selector, window)
	if err != nil {
		return b.queryFailed(err, selector, VocabLogging)
	}
	// The baseline half is what makes a template "new" rather than merely present. It is read
	// under the same budget: a baseline that could spend an unbounded number of calls would make
	// the bound on the window half decorative.
	var baseline logSample
	if baselineWindow.GetStart() != nil && baselineWindow.GetEnd() != nil {
		baseline, err = b.sampleLogs(ctx, facts.scope(b.project), selector, baselineWindow)
		if err != nil {
			return b.queryFailed(err, selector, VocabLogging)
		}
	}

	templates := logs.Diff(logs.Mine(linesOf(sample)), logs.Mine(linesOf(baseline)))
	key := termKeyOf(engine.NewLogPatterns(term.GetPointer(), term.GetWindow(), term.GetBaselineWindow()))

	counts := make(map[string]int64, 4)
	patterns := make([]*investigationv1.LogPattern, 0, len(templates))
	for _, t := range templates {
		counts[t.Status] += int64(t.Count)
		patterns = append(patterns, &investigationv1.LogPattern{
			Template:      t.Text,
			Count:         int64(t.Count),
			BaselineCount: int64(t.BaselineCount),
			NewInWindow:   t.NewInWindow,
			Status:        t.Status,
			JoinKeys:      joinKeysWithInstance(facts, t.Version, t.PodOrHost, t.FirstSeen),
			DrillDown:     b.mint(key, handleLog, selector, t.Text, window, facts),
		})
	}

	criteria := logCriteria(sample, baseline)
	if narrowed || baselineNarrowed {
		criteria = append(criteria, criterion(CriterionWindowCap, fmt.Sprintf(
			"new_log_patterns is capped at %s per window — it is the term that spends the "+
				"project's logging.read quota — and the request was wider; the most recent %s was searched",
			WindowCapLogs, WindowCapLogs)))
	}
	lag, lagSource := logLag(b.now(), sample.NewestReceive)
	coverage, err := b.coverage(coverageInput{
		Entities:   entitiesOf(facts),
		DataSource: "cloud_logging:projects/" + facts.scope(b.project),
		// The sub-window the sample actually spans, not the one asked for.
		Window:    sample.Covered,
		Volume:    int64(len(sample.Entries) + len(baseline.Entries)),
		Sampling:  samplingOf(sample),
		Criteria:  criteria,
		Lag:       lag,
		LagSource: lagSource,
	})
	if err != nil {
		return answer{}, err
	}

	digest := &investigationv1.Digest{
		Body: &investigationv1.Digest_Log{Log: &investigationv1.LogDigest{
			Patterns:       patterns,
			CountsByStatus: counts,
			MinerVersion:   logs.MinerVersion,
			ModelUsed:      false,
		}},
		Coverage: coverage,
	}
	out := answer{
		query:      selector,
		vocabulary: VocabLogging,
		deepLink:   b.deepLink(handleLog, selector, window, facts),
	}
	out.outcome = logOutcome(b.now(), sample, digest, window, lag, lagSource)
	return out, nil
}

// logOutcome applies the pagination rule and the ingestion cutoff, in that order.
//
// The order is the rule. An unfinished search is PARTIAL whether or not it found anything, because
// what it establishes is "some of the answer" and never "there is none". Only a search the vendor
// said was complete gets to be read against the ingestion cutoff, and only one outside that cutoff
// gets to be NO_DATA.
func logOutcome(now time.Time, sample logSample, digest *investigationv1.Digest,
	window *engine.Window, lag time.Duration, lagSource LagSource,
) engine.Outcome {
	if sample.Unfinished {
		return engine.Partial{
			Body: digest,
			Missing: fmt.Sprintf(
				"the search did not finish: Cloud Logging still had a nextPageToken when this "+
					"backend's sample bound (%s) stopped it after %d pages and %d entries. The "+
					"entries between %s and the end of the window were not examined, so nothing "+
					"here says a template is absent from the window — only that it is absent "+
					"from what was read",
				sample.Stopped, sample.Pages, len(sample.Entries),
				sample.Covered.GetEnd().AsTime().UTC().Format(time.RFC3339)),
		}
	}
	if len(sample.Entries) > 0 {
		return engine.DigestOutcome{Body: digest}
	}
	if !ingested(now, window, lag, lagSource) {
		// Inside the lag, an empty answer means nothing — and saying nothing is exactly what
		// NOT_YET_INGESTED does.
		return engine.NotYetIngested{Coverage: digest.GetCoverage()}
	}
	return engine.NoData{Coverage: digest.GetCoverage()}
}

// logCriteria states the sample bound in the published vocabulary. A sample that ran to the end of
// the vendor's results states nothing, because nothing was dropped.
func logCriteria(sample, baseline logSample) []string {
	var criteria []string
	if sample.Stopped != stopExhausted {
		criteria = append(criteria, criterion(CriterionLogSampleBound, fmt.Sprintf(
			"the window sample stopped on the %s bound after %d pages and %d entries",
			sample.Stopped, sample.Pages, len(sample.Entries))))
	}
	if baseline.Stopped != "" && baseline.Stopped != stopExhausted {
		criteria = append(criteria, criterion(CriterionLogSampleBound, fmt.Sprintf(
			"the baseline sample stopped on the %s bound after %d pages and %d entries, so a "+
				"template marked new may be one the baseline sample did not reach",
			baseline.Stopped, baseline.Pages, len(baseline.Entries))))
	}
	return criteria
}

// drillDownLogs answers a log handle: the entries behind one template, mined again over the
// narrower question the handle carries.
func (b *Backend) drillDownLogs(ctx context.Context, payload handlePayload, facts selectorFacts, window *engine.Window) (answer, error) {
	if b.transport == nil || b.transport.Logs == nil {
		outcome, err := b.absent("cloud_logging:projects/"+facts.scope(b.project),
			b.transport.absentSourceOf("Cloud Logging"), window)
		return answer{outcome: outcome, query: payload.Selector, vocabulary: VocabLogging}, err
	}
	sample, err := b.sampleLogs(ctx, facts.scope(b.project), payload.Selector, window)
	if err != nil {
		return b.queryFailed(err, payload.Selector, VocabLogging)
	}

	// Split by instance: the same template, one row per pod, which is the narrower question a
	// template handle asks. The rows mint no further handle — this is the world's recorded depth.
	counts := make(map[string]int64, 4)
	patterns := make([]*investigationv1.LogPattern, 0, 8)
	for _, t := range logs.Mine(linesOf(sample)) {
		if payload.Group != "" && t.Text != payload.Group {
			continue
		}
		counts[t.Status] += int64(t.Count)
		patterns = append(patterns, &investigationv1.LogPattern{
			Template: t.Text,
			Count:    int64(t.Count),
			Status:   t.Status,
			JoinKeys: joinKeysWithInstance(facts, t.Version, t.PodOrHost, t.FirstSeen),
		})
	}

	lag, lagSource := logLag(b.now(), sample.NewestReceive)
	coverage, err := b.coverage(coverageInput{
		Entities:   entitiesOf(facts),
		DataSource: "cloud_logging:projects/" + facts.scope(b.project),
		Window:     sample.Covered,
		Volume:     int64(len(sample.Entries)),
		Sampling:   samplingOf(sample),
		Criteria:   logCriteria(sample, logSample{}),
		Lag:        lag,
		LagSource:  lagSource,
	})
	if err != nil {
		return answer{}, err
	}
	digest := &investigationv1.Digest{
		Body: &investigationv1.Digest_Log{Log: &investigationv1.LogDigest{
			Patterns:       patterns,
			CountsByStatus: counts,
			MinerVersion:   logs.MinerVersion,
		}},
		Coverage: coverage,
	}
	return answer{
		outcome:    logOutcome(b.now(), sample, digest, window, lag, lagSource),
		query:      payload.Selector,
		vocabulary: VocabLogging,
		deepLink:   b.deepLink(handleLog, payload.Selector, window, facts),
	}, nil
}
