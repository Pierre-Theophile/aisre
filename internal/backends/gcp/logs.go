// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"fmt"
	"strings"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/internal/investigation/workers/logs"
)

// Reading Cloud Logging, bounded by necessity and honest about it (T100, T101; contract §4).
//
// **Cloud Logging has no server-side aggregation.** `entries.list` returns raw entries; the query
// language has no `GROUP BY` and no `COUNT`. The two server-side alternatives are both unavailable
// to a read-only connector, and not marginally:
//
//   - a **log-based metric** must be *created*, which FR-005 forbids outright, and it is **not
//     retroactive** — it is calculated from logs received after creation — so it could not answer a
//     question about a past incident even if writing were permitted;
//   - **Observability Analytics** requires *upgrading the log bucket*, also a mutation, with a
//     backfill delay and BigQuery analysis charges on the programmatic path.
//
// So templates are mined client-side over a **bounded sample**, and the bound is enforced here
// rather than discovered by running out. What makes that acceptable under principle V is that the
// digest says so: how many entries were examined, which criterion stopped the sample, and the
// sub-window the sample actually spans.
//
// # The pagination rule, which is a correctness rule
//
// Google's own reference: *"If a value for `nextPageToken` appears and the `entries` field is
// empty, it means that the search found no log entries so far but it did not have time to search
// all the possible log entries."* So an empty page is **not** `NO_DATA`. A backend that reported it
// as one would tell an investigation that nothing happened because the search ran out of time —
// and `NO_DATA` is the one outcome an investigation is entitled to read as evidence.
//
// `pageSize`'s maximum is **not documented**, and the oft-cited 1,000-entry and 10 MB caps appear
// nowhere in Google's reference. This code requests a large page, accepts whatever comes back, and
// enforces its own budget. It designs against neither number, because designing against folklore
// produces code that is wrong in a way nobody can check.

// The published sample budget (budget.md §5). It is a budget, not a guess at the vendor's limits:
// what it protects is the project's 60-calls-per-minute log-read quota, which an algebra term is
// not entitled to spend on the on-call's behalf.
const (
	// MaxLogEntries is the entry bound.
	MaxLogEntries = 5_000
	// MaxLogPages is the call bound, which is the one the quota actually feels.
	MaxLogPages = 20
	// MaxLogBytes is the byte bound, so one pathological entry set cannot cost the budget.
	MaxLogBytes = 8 << 20
	// MaxLogWall is the wall-clock bound: an incident does not wait for a complete search.
	MaxLogWall = 20 * time.Second
	// LogPageSize is what is asked for per page. The maximum is undocumented, so this asks for
	// a large page and reads what actually came.
	LogPageSize = 1_000
)

// The published stop criteria. A sample that stopped says which of these stopped it, because
// "truncated" without a criterion tells a reader that the number is wrong without telling them
// which way.
const (
	stopExhausted = "exhausted"
	stopEntries   = "entries"
	stopBytes     = "bytes"
	stopPages     = "pages"
	stopWallClock = "wall_clock"
)

// errAbsentLogs is the sentinel for a backend given no log reader.
var errAbsentLogs = fmt.Errorf("gcp: no Cloud Logging reader configured")

// logSample is one bounded read of `entries.list`.
type logSample struct {
	// Entries are what was actually examined.
	Entries []*loggingpb.LogEntry
	// Pages and Bytes are what it cost.
	Pages int
	Bytes int
	// Stopped is the criterion that ended the sample, from the published set above.
	Stopped string
	// Unfinished is the pagination rule: the vendor still had a token when this stopped, so
	// the search did not finish. It is never collapsed into "no results".
	Unfinished bool
	// NewestReceive is the greatest `receiveTimestamp` seen, which is what the logging
	// ingestion-lag estimate is computed from.
	NewestReceive time.Time
	// Covered is the sub-window the sample actually spans. Where pagination stopped early it
	// is narrower than the window asked for, and reporting the asked-for window instead would
	// claim coverage the search did not have.
	Covered *engine.Window
}

// sampleLogs reads one bounded sample.
func (b *Backend) sampleLogs(ctx context.Context, project, filter string, window *engine.Window) (logSample, error) {
	if b.transport == nil || b.transport.Logs == nil {
		return logSample{}, errAbsentLogs
	}
	start := window.GetStart().AsTime().UTC()
	end := window.GetEnd().AsTime().UTC()
	// The window is expressed in the query language rather than in a request field, because
	// `entries.list` has no interval parameter: the time bounds ARE part of the filter. The
	// caller's selector is appended to, never rewritten — a conjunction adds a bound and cannot
	// change what the selector matches.
	bounded := fmt.Sprintf(`%s AND timestamp >= "%s" AND timestamp < "%s"`,
		filter, start.Format(time.RFC3339Nano), end.Format(time.RFC3339Nano))

	sample := logSample{Stopped: stopExhausted}
	deadline := b.now().Add(MaxLogWall)
	token := ""
	for {
		if err := ctx.Err(); err != nil {
			return sample, err
		}
		page, next, err := b.transport.Logs.ListEntries(ctx, &loggingpb.ListLogEntriesRequest{
			ResourceNames: []string{"projects/" + project},
			Filter:        bounded,
			PageSize:      LogPageSize,
			PageToken:     token,
		})
		if err != nil {
			return sample, err
		}
		sample.Pages++
		for _, entry := range page {
			sample.Bytes += proto.Size(entry)
			if receive := entry.GetReceiveTimestamp(); receive != nil {
				at := receive.AsTime().UTC()
				if at.After(sample.NewestReceive) {
					sample.NewestReceive = at
				}
			}
		}
		sample.Entries = append(sample.Entries, page...)
		token = next

		// An empty page WITH a token is an unfinished search, not an empty one: the loop
		// continues. It stops only when the vendor says there is no more, or when one of the
		// published bounds is reached — and then it says which.
		if token == "" {
			sample.Unfinished = false
			break
		}
		sample.Unfinished = true
		switch {
		case len(sample.Entries) >= MaxLogEntries:
			sample.Stopped = stopEntries
		case sample.Bytes >= MaxLogBytes:
			sample.Stopped = stopBytes
		case sample.Pages >= MaxLogPages:
			sample.Stopped = stopPages
		case !b.now().Before(deadline):
			sample.Stopped = stopWallClock
		default:
			continue
		}
		break
	}

	sample.Covered = coveredBy(sample.Entries, window)
	return sample, nil
}

// coveredBy is the sub-window a sample actually spans. With no entries it is the window asked for:
// the search covered it and found nothing there, which is a different statement from having
// searched a narrower window.
func coveredBy(entries []*loggingpb.LogEntry, asked *engine.Window) *engine.Window {
	if len(entries) == 0 {
		return asked
	}
	var first, last time.Time
	for _, entry := range entries {
		at := entryInstant(entry)
		if at.IsZero() {
			continue
		}
		if first.IsZero() || at.Before(first) {
			first = at
		}
		if at.After(last) {
			last = at
		}
	}
	if first.IsZero() {
		return asked
	}
	return &engine.Window{Start: timestamppb.New(first), End: timestamppb.New(last)}
}

func entryInstant(entry *loggingpb.LogEntry) time.Time {
	if ts := entry.GetTimestamp(); ts != nil {
		return ts.AsTime().UTC()
	}
	if ts := entry.GetReceiveTimestamp(); ts != nil {
		return ts.AsTime().UTC()
	}
	return time.Time{}
}

// entryText is the line the miner sees. A structured payload contributes its `message` field where
// it has one, because that is the human line; a payload with none contributes its field names
// only, which is enough for the miner to tell two shapes apart and carries no value a mask would
// have had to catch.
func entryText(entry *loggingpb.LogEntry) string {
	if text := entry.GetTextPayload(); text != "" {
		return text
	}
	if payload := entry.GetJsonPayload(); payload != nil {
		fields := payload.GetFields()
		if message, ok := fields["message"]; ok {
			if s := message.GetStringValue(); s != "" {
				return s
			}
		}
		keys := make([]string, 0, len(fields))
		for key := range fields {
			keys = append(keys, key)
		}
		return "jsonPayload{" + strings.Join(sortedStrings(keys), ",") + "}"
	}
	if payload := entry.GetProtoPayload(); payload != nil {
		return "protoPayload{" + payload.GetTypeUrl() + "}"
	}
	return ""
}

func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// statusOf maps Cloud Logging's severity onto the status vocabulary the log digest counts by.
func statusOf(entry *loggingpb.LogEntry) string {
	severity := entry.GetSeverity().String()
	switch severity {
	case "EMERGENCY", "ALERT", "CRITICAL", "ERROR":
		return "error"
	case "WARNING":
		return "warn"
	case "DEBUG", "DEFAULT":
		return "debug"
	default:
		return "info"
	}
}

// linesOf turns a sample into the miner's input. The lines never leave this process: what leaves
// is the template.
func linesOf(sample logSample) []logs.Line {
	out := make([]logs.Line, 0, len(sample.Entries))
	for _, entry := range sample.Entries {
		text := entryText(entry)
		if text == "" {
			continue
		}
		labels := entry.GetResource().GetLabels()
		out = append(out, logs.Line{
			At:        entryInstant(entry),
			Text:      text,
			Status:    statusOf(entry),
			PodOrHost: labels[LabelInstanceID],
			Version:   labels[LabelRevisionName],
		})
	}
	return out
}

// samplingOf is what the coverage block states about a log read: the page size asked for, the
// pages and bytes spent, and the criterion that stopped it.
func samplingOf(sample logSample) string {
	return fmt.Sprintf("entries.list;page_size=%d;pages=%d;entries=%d;bytes=%d;stopped=%s",
		LogPageSize, sample.Pages, len(sample.Entries), sample.Bytes, sample.Stopped)
}
