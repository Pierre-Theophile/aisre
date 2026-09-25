// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
)

// Handles and drill-downs (T104, FR-103).
//
// A drill-down is **a reference, never an embedded payload**. The handle carries the narrower
// question — which group, over which window, against which selector — encoded so that presenting
// it back is a complete question and the caller never composes a selector. The human link opens
// the same query over the same window in the GCP console, and carries no credential (FR-082): a
// console URL that embedded one would put it in the graph, in every golden, and in every fixture
// in the public repository.
//
// Encoding the question rather than an id is what keeps the argument space of `drill_down` and
// `exemplars` **finite**, and therefore keeps a recorded world finite: a handle is minted by an
// answer, so the set of handles a world must hold is the set of answers it recorded.

// The handle kinds. A handle says what family the narrower answer is in, so `exemplars` can refuse
// one minted by a metric answer — an exemplar of a metric series is a raw sample, and those never
// cross the digest boundary in any quantity.
const (
	handleMetric = "metric"
	handleLog    = "log"
	handleAlert  = "alert"
)

// handlePayload is the narrower question. The field names are one letter because the encoded form
// travels in every digest that mints one, and a handle is not a place to spend the response's
// size budget.
type handlePayload struct {
	Kind     string `json:"k"`
	Selector string `json:"s"`
	Start    int64  `json:"a"`
	End      int64  `json:"b"`
	Group    string `json:"g"`
	Project  string `json:"p"`
}

func (p handlePayload) window() *engine.Window {
	return &engine.Window{
		Start: timestamppb.New(time.Unix(p.Start, 0).UTC()),
		End:   timestamppb.New(time.Unix(p.End, 0).UTC()),
	}
}

// mint builds a drill-down for one group of one answer.
func (b *Backend) mint(mintedBy, kind, selector, group string, window *engine.Window, facts selectorFacts) *investigationv1.DrillDown {
	payload := handlePayload{
		Kind:     kind,
		Selector: selector,
		Start:    window.GetStart().AsTime().Unix(),
		End:      window.GetEnd().AsTime().Unix(),
		Group:    group,
		Project:  facts.scope(b.project),
	}
	encoded, err := json.Marshal(payload)
	if err != nil { // unreachable: four strings and two integers
		return nil
	}
	return &investigationv1.DrillDown{
		Handle: &investigationv1.Handle{
			Value:           base64.RawURLEncoding.EncodeToString(encoded),
			MintedByTermKey: mintedBy,
			Depth:           1,
		},
		HumanLink: b.deepLink(kind, selector, window, facts),
	}
}

func parseHandle(h *investigationv1.Handle) (handlePayload, error) {
	var payload handlePayload
	raw, err := base64.RawURLEncoding.DecodeString(h.GetValue())
	if err != nil {
		return payload, fmt.Errorf("gcp: handle %q was not minted by this backend; a handle is "+
			"presented back, never composed", h.GetValue())
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return payload, fmt.Errorf("gcp: handle %q does not decode to a question", h.GetValue())
	}
	if payload.Selector == "" || payload.End <= payload.Start {
		return payload, fmt.Errorf("gcp: handle %q decodes to an incomplete question", h.GetValue())
	}
	return payload, nil
}

// deepLink opens the same query over the same window for a person.
//
// The logs link carries the query and the time range in the console's own path syntax, so it opens
// on the entries this digest was mined from rather than on an empty Logs Explorer. The metrics link
// opens the Cloud Run service's metrics page: Metrics Explorer encodes its state in an opaque blob
// that is neither documented nor stable, and a link built from an undocumented encoding is a link
// that breaks silently.
func (b *Backend) deepLink(kind, selector string, window *engine.Window, facts selectorFacts) string {
	project := facts.scope(b.project)
	switch kind {
	case handleLog:
		start := window.GetStart().AsTime().UTC().Format(time.RFC3339)
		end := window.GetEnd().AsTime().UTC().Format(time.RFC3339)
		return fmt.Sprintf("https://console.cloud.google.com/logs/query;query=%s;timeRange=%s?project=%s",
			url.PathEscape(selector), url.PathEscape(start+"/"+end), url.QueryEscape(project))
	case handleAlert:
		return fmt.Sprintf("https://console.cloud.google.com/monitoring/alerting/incidents?project=%s",
			url.QueryEscape(project))
	default:
		region, service := facts.Region, facts.Service
		if region == "" {
			region = b.region
		}
		if region == "" || service == "" {
			return fmt.Sprintf("https://console.cloud.google.com/monitoring?project=%s", url.QueryEscape(project))
		}
		return fmt.Sprintf("https://console.cloud.google.com/run/detail/%s/%s/metrics?project=%s",
			url.PathEscape(region), url.PathEscape(service), url.QueryEscape(project))
	}
}

// drillDown answers the narrower question behind a handle: the same family, split one level finer.
//
// The rows it returns mint **no further handle**, and that absence is the design rather than an
// omission. A recorded world holds depth 1; a handle minted by a depth-1 answer would invite a
// caller into a depth-2 question the world does not hold, and the caller would meet a NOT_RECORDED
// it had no way to predict.
func (b *Backend) drillDown(ctx context.Context, term *investigationv1.DrillDownTerm) (answer, error) {
	payload, err := parseHandle(term.GetHandle())
	if err != nil {
		return answer{
			outcome:    engine.QueryFailed{Reason: investigationv1.FailureReason_UNSUPPORTED_POINTER, Detail: err.Error()},
			vocabulary: VocabMonitoring,
			query:      "drill_down(unparseable handle)",
		}, nil
	}
	facts := parseSelector(payload.Selector)
	window := payload.window()

	if payload.Kind == handleLog {
		return b.drillDownLogs(ctx, payload, facts, window)
	}
	return b.drillDownMetrics(ctx, payload, facts, window)
}

// drillDownMetrics splits a metric answer by instance: the same selector, grouped one level finer
// than the answer that minted the handle.
func (b *Backend) drillDownMetrics(ctx context.Context, payload handlePayload, facts selectorFacts, window *engine.Window) (answer, error) {
	if b.transport == nil || b.transport.Metrics == nil {
		outcome, err := b.absent("cloud_monitoring:"+payload.Selector,
			b.transport.absentSourceOf("Cloud Monitoring"), window)
		return answer{outcome: outcome, vocabulary: VocabMonitoring, query: payload.Selector}, err
	}
	agg := aggregationFor(investigationv1.Statistic_MEAN, window, []string{"resource.labels." + LabelInstanceID})
	series, err := b.queryMetrics(ctx, facts.scope(b.project), payload.Selector, window, agg)
	if err != nil {
		return b.queryFailed(err, payload.Selector, VocabMonitoring)
	}

	rows := make([]*investigationv1.SeriesSummary, 0, len(series))
	var considered int64
	for _, s := range series {
		values, instants := seriesValues(s)
		considered += int64(len(values))
		var firstSeen time.Time
		if len(instants) > 0 {
			firstSeen = instants[0]
		}
		rows = append(rows, &investigationv1.SeriesSummary{
			Tags:              tagsOf(s),
			PointCount:        int64(len(values)),
			IntervalCovered:   intervalOf(instants, window),
			ResolutionSeconds: int64(agg.Period / time.Second),
			Statistics:        statisticsOf(values),
			JoinKeys:          keysFromLabels(facts, s.GetResource().GetLabels(), s.GetMetric().GetLabels(), firstSeen),
			// No handle: this is the world's recorded depth. See the function comment.
		})
	}
	if len(rows) == 0 {
		return b.emptyMetricAnswer(ctx, facts, payload.Selector, window, agg)
	}

	lag, lagSource := b.metricLag(ctx, facts.MetricType)
	criteria := []string{}
	if facts.derivedFromRequestCount() {
		criteria = append(criteria, requestCountCaveats()...)
	}
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
	return answer{
		outcome: engine.DigestOutcome{Body: &investigationv1.Digest{
			Body:     &investigationv1.Digest_Metric{Metric: &investigationv1.MetricDigest{Series: rows}},
			Coverage: coverage,
		}},
		query:      payload.Selector,
		vocabulary: VocabMonitoring,
		deepLink:   b.deepLink(handleMetric, payload.Selector, window, facts),
	}, nil
}
