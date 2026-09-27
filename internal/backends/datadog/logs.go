// SPDX-License-Identifier: Apache-2.0

package datadog

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/datadogx"
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// What every log term shares: turning a Datadog failure into a published outcome, building the
// coverage block, the human deep link and the drill-down handle (contract §4–§6).

// failed maps a client error to a typed outcome. A failure is never turned into an empty digest.
func failed(err error, query string) (answer, error) {
	var status *datadogx.StatusError
	if !errors.As(err, &status) {
		var unpublished *feeder.UnpublishedOperationError
		if errors.As(err, &unpublished) {
			return answer{}, err // a programming error: the connector asked for an operation it did not declare
		}
		return answer{outcome: engine.QueryFailed{Reason: investigationv1.FailureReason_TIMED_OUT,
			Detail: "datadog: " + err.Error()}, query: query, vocabulary: feeder.VocabDatadogLogs}, nil
	}
	outcome := engine.QueryFailed{Detail: fmt.Sprintf("datadog answered %d: %s", status.Status, status.Message)}
	switch status.Status {
	case 429:
		outcome.Reason = investigationv1.FailureReason_RATE_LIMITED
		if status.RetryAfter > 0 {
			outcome.Detail += fmt.Sprintf("; retry after %s", status.RetryAfter)
		}
	case 401, 403:
		outcome.Reason = investigationv1.FailureReason_NOT_PERMITTED
	case 408, 504:
		outcome.Reason = investigationv1.FailureReason_TIMED_OUT
	default:
		outcome.Reason = investigationv1.FailureReason_REJECTED_BY_BACKEND
	}
	return answer{outcome: outcome, query: query, vocabulary: feeder.VocabDatadogLogs}, nil
}

// coverageOf builds the coverage block for a log answer: what was searched, over which window, how
// much was considered, the sampling, the quota Datadog reported, and when it ran. The indexing lag is
// stated as undetermined unless a term measured it, never guessed (FR-048a).
func (b *Backend) coverageOf(sel selector, window *engine.Window, volume int64, sampling, truncation string, resp *datadogx.Response) (*investigationv1.Coverage, error) {
	in := engine.CoverageInput{
		SearchedEntities:         []string{sel.Service + "@" + sel.Env},
		DataSource:               "datadog_logs:" + strings.Join(orDefault(sel.indexes(b.indexes)), ","),
		WindowCovered:            window,
		VolumeConsidered:         volume,
		Sampling:                 sampling,
		Truncation:               truncation,
		IngestionLagUndetermined: true,
		ExecutedAt:               b.now().UTC(),
		QuotaUndetermined:        true,
	}
	if resp != nil && resp.HasQuota {
		in.QuotaUndetermined = false
		in.RemainingQuota = int64(resp.Reading.Remaining)
		in.QuotaWindow = resp.Reading.Period
	}
	return in.Coverage()
}

func orDefault(indexes []string) []string {
	if len(indexes) == 0 {
		return []string{"default"}
	}
	return indexes
}

// deepLink opens the same query over the same window in Datadog for a person. It carries no credential
// (FR-037, FR-048c); empty when the site was not configured.
func (b *Backend) deepLink(query string, window *engine.Window) string {
	if b.site == "" {
		return ""
	}
	v := url.Values{}
	v.Set("query", query)
	v.Set("from_ts", strconv.FormatInt(window.GetStart().AsTime().UnixMilli(), 10))
	v.Set("to_ts", strconv.FormatInt(window.GetEnd().AsTime().UnixMilli(), 10))
	return "https://app." + b.site + "/logs?" + v.Encode()
}

// handlePayload is what a drill-down handle carries: enough to ask the narrower question, and nothing
// the caller could use to compose one — a handle is presented back, never composed.
type handlePayload struct {
	Kind     string `json:"k"`
	Selector string `json:"s"`
	Start    int64  `json:"a"`
	End      int64  `json:"b"`
	Group    string `json:"g,omitempty"`
	Facet    string `json:"f,omitempty"`
}

// The handle kinds.
const (
	handleVersion = "version"
	handlePattern = "pattern"
)

func (b *Backend) mint(mintedBy, kind, selectorText, facet, group string, window *engine.Window) *investigationv1.DrillDown {
	raw, err := json.Marshal(handlePayload{
		Kind: kind, Selector: selectorText, Facet: facet, Group: group,
		Start: window.GetStart().AsTime().Unix(), End: window.GetEnd().AsTime().Unix(),
	})
	if err != nil {
		return nil
	}
	query := selectorText
	if kind == handleVersion && facet != "" {
		query += " " + facetClause(facet, group)
	}
	return &investigationv1.DrillDown{
		Handle: &investigationv1.Handle{
			Value: base64.RawURLEncoding.EncodeToString(raw), MintedByTermKey: mintedBy, Depth: 1,
		},
		HumanLink: b.deepLink(query, window),
	}
}

func parseHandle(h *investigationv1.Handle) (handlePayload, error) {
	var p handlePayload
	raw, err := base64.RawURLEncoding.DecodeString(h.GetValue())
	if err == nil {
		err = json.Unmarshal(raw, &p)
	}
	if err != nil || p.Kind == "" || p.Selector == "" {
		return handlePayload{}, fmt.Errorf("datadog: handle %q was not minted by this backend; a handle "+
			"is presented back, never composed", h.GetValue())
	}
	return p, nil
}

func (p handlePayload) window() *engine.Window {
	return &engine.Window{Start: timestamppb.New(time.Unix(p.Start, 0).UTC()), End: timestamppb.New(time.Unix(p.End, 0).UTC())}
}

// narrow cuts a window to its cap from the END — the most recent data is what an investigation is
// about — and reports whether it did.
func narrow(window *engine.Window, limit time.Duration) (*engine.Window, bool) {
	if window.GetStart() == nil || window.GetEnd() == nil || limit <= 0 {
		return window, false
	}
	start, end := window.GetStart().AsTime().UTC(), window.GetEnd().AsTime().UTC()
	if end.Sub(start) <= limit {
		return window, false
	}
	return &engine.Window{Start: timestamppb.New(end.Add(-limit)), End: timestamppb.New(end)}, true
}

func narrowedText(term string, limit time.Duration) string {
	return fmt.Sprintf("window_cap: %s is capped at %s and the request was wider; the most recent %s was queried",
		term, limit, limit)
}

func filterOf(sel selector, window *engine.Window, configured []string, extra ...string) datadogx.LogFilter {
	return datadogx.NewLogFilter(sel.query(extra...), window.GetStart().AsTime(), window.GetEnd().AsTime(),
		sel.indexes(configured))
}

func canonical(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(raw)
}
