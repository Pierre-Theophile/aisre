// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
)

// `exemplars`: bounded, redacted, and only on explicit request (T103; FR-096, FR-014).
//
// An exemplar is the one raw-ish thing allowed to cross the digest boundary, so it is capped
// hardest: **never returned by default**, capped by the same published limit a recording uses,
// counting toward the digest's size bound like everything else, and reduced to a masked template
// before it leaves the process.
//
// "Only on explicit request" is `AlgebraRequest.want_exemplars`, and it is checked here rather
// than assumed by the planner. A backend that returned exemplars because the handle was a log
// handle would make the flag advisory — and a flag that is advisory is a flag that is off in the
// contract and on in production.
//
// A handle minted by a **metric** answer is refused rather than served smaller. The exemplar of a
// metric series is a raw sample and the exemplar of a span group is a span payload; neither may
// cross the boundary in any quantity (constitution IV), so the honest answer is a refusal with a
// reason and not a reduced violation.

// exemplars answers the term.
func (b *Backend) exemplars(ctx context.Context, term *investigationv1.ExemplarsTerm, wanted bool) (answer, error) {
	payload, err := parseHandle(term.GetHandle())
	if err != nil {
		return answer{
			outcome:    engine.QueryFailed{Reason: investigationv1.FailureReason_UNSUPPORTED_POINTER, Detail: err.Error()},
			query:      "exemplars(unparseable handle)",
			vocabulary: VocabLogging,
		}, nil
	}
	if !wanted {
		return answer{
			outcome: engine.QueryFailed{
				Reason: investigationv1.FailureReason_REJECTED_BY_BACKEND,
				Detail: "gcp: exemplars are never returned by default (FR-014); the request did " +
					"not set want_exemplars, and a backend that served them anyway would make " +
					"that flag advisory",
			},
			query:      "exemplars(not requested)",
			vocabulary: VocabLogging,
		}, nil
	}
	if payload.Kind != handleLog {
		return answer{
			outcome: engine.QueryFailed{
				Reason: investigationv1.FailureReason_UNSUPPORTED_POINTER,
				Detail: "gcp: exemplars are bounded, sanitised log lines; handle " +
					term.GetHandle().GetValue() + " was minted by a " + payload.Kind +
					" answer, whose exemplars would be raw samples — and those never cross the " +
					"digest boundary",
			},
			query:      "exemplars(" + payload.Kind + " handle)",
			vocabulary: VocabLogging,
		}, nil
	}

	facts := parseSelector(payload.Selector)
	window := payload.window()
	if b.transport == nil || b.transport.Logs == nil {
		outcome, err := b.absent("cloud_logging:projects/"+facts.scope(b.project),
			b.transport.absentSourceOf("Cloud Logging"), window)
		return answer{outcome: outcome, query: payload.Selector, vocabulary: VocabLogging}, err
	}

	limit := int(term.GetLimit())
	if limit <= 0 || limit > engine.MaxExemplars {
		limit = engine.MaxExemplars
	}

	sample, err := b.sampleLogs(ctx, facts.scope(b.project), payload.Selector, window)
	if err != nil {
		return b.queryFailed(err, payload.Selector, VocabLogging)
	}

	// Redaction reduces a line to its masked template before it leaves the process, so two lines
	// that mask alike and carry the same join keys are the same exemplar. They are de-duplicated
	// here rather than returned ten times: repeating one sanitised line adds nothing a reader can
	// use and spends the whole response budget doing it.
	exemplars := make([]*investigationv1.Exemplar, 0, limit)
	seen := make(map[string]struct{}, limit)
	for _, line := range linesOf(sample) {
		masked := engine.MaskLine(line.Text)
		if payload.Group != "" && masked != payload.Group {
			continue
		}
		keys := joinKeysWithInstance(facts, line.Version, line.PodOrHost, line.At)
		fingerprint := masked + "\x00" + keys.GetPodOrHost() + "\x00" + keys.GetVersion()
		if _, dup := seen[fingerprint]; dup {
			continue
		}
		seen[fingerprint] = struct{}{}
		exemplars = append(exemplars, &investigationv1.Exemplar{Text: line.Text, JoinKeys: keys})
		if len(exemplars) >= limit {
			break
		}
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
		Body: &investigationv1.Digest_Exemplars{Exemplars: &investigationv1.ExemplarDigest{
			Exemplars: exemplars,
			Cap:       uint32(limit),
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
