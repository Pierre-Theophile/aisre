// SPDX-License-Identifier: Apache-2.0

package datadog

import (
	"context"
	"fmt"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/datadogx"
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/internal/investigation/workers/logs"
)

// The bounded, paged log sample behind new_log_patterns, exemplars and drill_down (contract §3.2).
//
// Datadog has no patterns API, so the lines are read and mined here, with the miner every log backend
// uses. The sample is read newest first — the most recent lines are what an investigation is about —
// and stops at the published line cap; where it stops early, the window it actually covers starts at
// the oldest line read, and the answer says so rather than implying it read the whole window.

// LineCap is the most lines one sample reads: five pages of Datadog's maximum page size.
const LineCap = 5000

// pageSize is Datadog's maximum search page.
const pageSize = 1000

// lagHorizon is how close to now a window's end must be for the newest line's age to say anything
// about the indexing lag. An older window's newest line measures how quiet the service was.
const lagHorizon = 5 * time.Minute

type sample struct {
	lines []logs.Line
	// window is the part of the asked window the sample spans.
	window *engine.Window
	// capped is true when the line cap stopped the search before Datadog ran out of lines.
	capped  bool
	partial bool
	pages   int
	newest  time.Time
	last    *datadogx.Response
}

// sampleLogs reads up to LineCap lines matching query over window, newest first.
func (b *Backend) sampleLogs(ctx context.Context, sel selector, window *engine.Window, facet string, extra ...string) (sample, error) {
	req := datadogx.SearchRequest{Filter: filterOf(sel, window, b.indexes, extra...), Sort: "-timestamp",
		Page: datadogx.SearchPage{Limit: pageSize}}
	s := sample{window: window}
	for {
		page, resp, err := b.client.SearchLogs(ctx, req)
		if err != nil {
			return sample{}, err
		}
		s.pages++
		s.last = resp
		s.partial = s.partial || page.Meta.Partial()
		for _, ev := range page.Data {
			a := ev.Attributes
			s.lines = append(s.lines, logs.Line{At: a.Timestamp.UTC(), Text: a.Message, Status: a.Status,
				PodOrHost: a.Host, Version: versionOf(ev, facet)})
			if a.Timestamp.After(s.newest) {
				s.newest = a.Timestamp.UTC()
			}
		}
		if page.Meta.Page.After == "" || len(page.Data) == 0 {
			return s, nil
		}
		if len(s.lines) >= LineCap {
			s.capped = true
			oldest := s.lines[len(s.lines)-1].At
			s.window = &engine.Window{Start: timestamppb.New(oldest), End: window.GetEnd()}
			return s, nil
		}
		req.Page.Cursor = page.Meta.Page.After
	}
}

// versionOf reads the version stamp off one event: a tag when the facet is bare (`version`), an
// attribute when it starts with `@` (`@service.version`, dotted paths nested). Empty when absent.
func versionOf(ev datadogx.LogEvent, facet string) string {
	switch {
	case facet == "":
		return ""
	case strings.HasPrefix(facet, "@"):
		path := strings.TrimPrefix(facet, "@")
		var v any
		if flat, ok := ev.Attributes.Attributes[path]; ok { // a dotted key stored flat
			v = flat
		} else {
			v = ev.Attributes.Attributes
			for _, part := range strings.Split(path, ".") {
				m, ok := v.(map[string]any)
				if !ok {
					v = nil
					break
				}
				v = m[part]
			}
		}
		if v == nil {
			return ""
		}
		if s, ok := v.(string); ok {
			return s
		}
		return fmt.Sprint(v)
	default:
		prefix := facet + ":"
		for _, tag := range ev.Attributes.Tags {
			if strings.HasPrefix(tag, prefix) {
				return strings.TrimPrefix(tag, prefix)
			}
		}
		return ""
	}
}

// matches reports whether a line belongs to a mined template: the same tokens once masked, with the
// template's wildcards matching any one token.
func matches(template, line string) bool {
	want, got := strings.Fields(template), strings.Fields(engine.MaskLine(line))
	if len(want) != len(got) {
		return false
	}
	for i := range want {
		if want[i] != logs.Wildcard && want[i] != got[i] {
			return false
		}
	}
	return true
}

// sampling states the sample in coverage's words.
func (s sample) sampling() string {
	if s.capped {
		return fmt.Sprintf("sampled: the newest %d lines (the line cap) in %d pages", len(s.lines), s.pages)
	}
	return fmt.Sprintf("complete: all %d lines in %d pages", len(s.lines), s.pages)
}

// truncation names what the sample did not read.
func (s sample) truncation(what string) string {
	var out string
	if s.capped {
		out = fmt.Sprintf("line_cap: the %s sample stopped at %d lines; lines before %s were not read", what,
			LineCap, s.window.GetStart().AsTime().UTC().Format(time.RFC3339))
	}
	if s.partial {
		out = joinTruncation(out, "partial: Datadog reported a timeout or warnings for the "+what+" search")
	}
	return out
}

// sampleCoverage builds the coverage block for a sampled answer, with the indexing lag where the newest
// line can measure it (contract §4).
func (b *Backend) sampleCoverage(sel selector, s sample, volume int64, truncation string) (*investigationv1.Coverage, error) {
	cov, err := b.coverageOf(sel, s.window, volume, s.sampling(), truncation, s.last)
	if err != nil {
		return nil, err
	}
	if lag, ok := s.lag(b.now()); ok {
		cov.IngestionLagUndetermined = false
		cov.IngestionLagSeconds = int64(lag / time.Second)
	}
	return cov, nil
}

func (s sample) lag(now time.Time) (time.Duration, bool) {
	end := s.window.GetEnd().AsTime()
	if s.newest.IsZero() || now.Sub(end) > lagHorizon {
		return 0, false
	}
	return max(now.Sub(s.newest), 0), true
}

// sampleOutcome applies the sample rule (contract §4): a read the line cap stopped, or one Datadog
// reported as incomplete, is PARTIAL whatever it found; an empty read of
// a window that ends inside the lag horizon is NOT_YET_INGESTED; only then NO_DATA.
func (b *Backend) sampleOutcome(s sample, found bool, digest *investigationv1.Digest) engine.Outcome {
	if s.partial || s.capped {
		return engine.Partial{Body: digest, Missing: strings.TrimSpace(s.truncation("window") +
			"; nothing here says a line is absent from the window, only from what was read")}
	}
	if found {
		return engine.DigestOutcome{Body: digest}
	}
	end := s.window.GetEnd().AsTime()
	if b.now().Sub(end) < lagHorizon {
		return engine.NotYetIngested{Coverage: digest.GetCoverage(), RetryAfter: end.Add(lagHorizon)}
	}
	return engine.NoData{Coverage: digest.GetCoverage()}
}
