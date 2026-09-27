// SPDX-License-Identifier: Apache-2.0

package datadog

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/datadogx"
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/internal/investigation/workers/logs"
	sdk "github.com/Pierre-Theophile/aisre/pkg/backend"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// `new_log_patterns`, `drill_down` and `exemplars` over the bounded sample (contract §3.2, §6).
//
// The digest carries masked templates with counts and first-seen instants, never raw lines, and the
// miner version, because a template set is a function of the miner. Exemplars are the one raw-ish
// thing that crosses the boundary: only when the request asked for them, capped, and masked by the
// engine's redaction before they leave.

func (b *Backend) newLogPatterns(ctx context.Context, term *investigationv1.NewLogPatternsTerm) (answer, error) {
	raw := term.GetPointer().GetSelector()
	sel, err := parseSelector(raw)
	if err != nil {
		return unsupported(raw, err), nil
	}
	facet := term.GetPointer().GetJoinKeys()["version"]
	window, narrowed := narrow(term.GetWindow(), WindowCapLogs)
	baselineWindow, baselineNarrowed := narrow(term.GetBaselineWindow(), WindowCapLogs)

	s, err := b.sampleLogs(ctx, sel, window, facet)
	if err != nil {
		return failed(err, sel.query())
	}
	// The baseline is what makes a template new rather than merely present, read under the same cap.
	var base sample
	if baselineWindow.GetStart() != nil && baselineWindow.GetEnd() != nil {
		if base, err = b.sampleLogs(ctx, sel, baselineWindow, facet); err != nil {
			return failed(err, sel.query())
		}
	}

	key, _ := engine.TermKey(engine.NewLogPatterns(term.GetPointer(), term.GetWindow(), term.GetBaselineWindow()))
	templates := logs.Diff(logs.Mine(s.lines), logs.Mine(base.lines))
	patterns, counts := b.patternsOf(templates, sel, func(t logs.Template) *investigationv1.DrillDown {
		return b.mint(key, handlePattern, raw, facet, t.Text, window)
	})

	truncation := s.truncation("window")
	if base.pages > 0 && (base.capped || base.partial) {
		truncation = joinTruncation(truncation, base.truncation("baseline")+
			"; a template marked new may be one the baseline sample did not reach")
	}
	if narrowed || baselineNarrowed {
		truncation = joinTruncation(truncation, narrowedText(sdk.TermNewLogPatterns, WindowCapLogs))
	}
	coverage, err := b.sampleCoverage(sel, s, int64(len(s.lines)+len(base.lines)), truncation)
	if err != nil {
		return answer{}, err
	}
	if s.capped {
		// State the sample against the window's total, so a reader knows what fraction was mined.
		if total, ok := b.countLines(ctx, sel, window); ok {
			coverage.Sampling += fmt.Sprintf(" of %d lines in the window", total)
		}
	}
	digest := &investigationv1.Digest{
		Body: &investigationv1.Digest_Log{Log: &investigationv1.LogDigest{
			Patterns: patterns, CountsByStatus: counts, MinerVersion: logs.MinerVersion,
		}},
		Coverage: coverage,
	}
	return answer{outcome: b.sampleOutcome(s, len(s.lines) > 0, digest), query: sel.query(),
		vocabulary: feeder.VocabDatadogLogs, deepLink: b.deepLink(sel.query(), window)}, nil
}

// patternsOf turns mined templates into digest rows. mint may return nil: a row at the recorded depth
// carries no further handle.
func (b *Backend) patternsOf(templates []logs.Template, sel selector,
	mint func(logs.Template) *investigationv1.DrillDown,
) ([]*investigationv1.LogPattern, map[string]int64) {
	counts := map[string]int64{}
	patterns := make([]*investigationv1.LogPattern, 0, len(templates))
	for _, t := range templates {
		counts[t.Status] += int64(t.Count)
		patterns = append(patterns, &investigationv1.LogPattern{
			Template: t.Text, Count: int64(t.Count), BaselineCount: int64(t.BaselineCount),
			NewInWindow: t.NewInWindow, Status: t.Status,
			JoinKeys:  joinKeys(sel, t.Version, t.PodOrHost, t.FirstSeen),
			DrillDown: mint(t),
		})
	}
	return patterns, counts
}

// drillDown answers a handle one level narrower (contract §6): a version group's lines mined into
// templates, or a template's lines split by host. The rows mint no further handle — worlds record
// depth 1.
func (b *Backend) drillDown(ctx context.Context, term *investigationv1.DrillDownTerm) (answer, error) {
	p, sel, err := b.handleOf(term.GetHandle())
	if err != nil {
		return unsupported("drill_down(unparseable handle)", err), nil
	}
	window := p.window()
	s, err := b.sampleLogs(ctx, sel, window, p.Facet, p.clause()...)
	if err != nil {
		return failed(err, sel.query(p.clause()...))
	}

	var templates []logs.Template
	switch p.Kind {
	case handleVersion:
		templates = logs.Mine(s.lines)
	default: // handlePattern: the template's lines, one row per host
		byHost := map[string][]logs.Line{}
		for _, line := range s.lines {
			if matches(p.Group, line.Text) {
				byHost[line.PodOrHost] = append(byHost[line.PodOrHost], line)
			}
		}
		hosts := make([]string, 0, len(byHost))
		for h := range byHost {
			hosts = append(hosts, h)
		}
		sort.Strings(hosts)
		for _, h := range hosts {
			mined := logs.Mine(byHost[h])
			if len(mined) == 0 {
				continue
			}
			t := mined[0]
			t.Text = p.Group // one row per host, spelled as the handle named the template
			t.Count = len(byHost[h])
			templates = append(templates, t)
		}
	}
	patterns, counts := b.patternsOf(templates, sel, func(logs.Template) *investigationv1.DrillDown { return nil })

	coverage, err := b.sampleCoverage(sel, s, int64(len(s.lines)), s.truncation("drill-down"))
	if err != nil {
		return answer{}, err
	}
	digest := &investigationv1.Digest{
		Body: &investigationv1.Digest_Log{Log: &investigationv1.LogDigest{
			Patterns: patterns, CountsByStatus: counts, MinerVersion: logs.MinerVersion,
		}},
		Coverage: coverage,
	}
	query := sel.query(p.clause()...)
	return answer{outcome: b.sampleOutcome(s, len(patterns) > 0, digest), query: query,
		vocabulary: feeder.VocabDatadogLogs, deepLink: b.deepLink(query, window)}, nil
}

// exemplars returns up to the cap of lines behind a handle, only when the request asked for them
// (FR-014): a backend that served them anyway would make that flag advisory.
func (b *Backend) exemplars(ctx context.Context, term *investigationv1.ExemplarsTerm, wanted bool) (answer, error) {
	p, sel, err := b.handleOf(term.GetHandle())
	if err != nil {
		return unsupported("exemplars(unparseable handle)", err), nil
	}
	if !wanted {
		return answer{outcome: engine.QueryFailed{Reason: investigationv1.FailureReason_REJECTED_BY_BACKEND,
			Detail: "datadog: exemplars are never returned by default (FR-014); the request did not set " +
				"want_exemplars"}, query: "exemplars(not requested)", vocabulary: feeder.VocabDatadogLogs}, nil
	}
	limit := int(term.GetLimit())
	if limit <= 0 || limit > engine.MaxExemplars {
		limit = engine.MaxExemplars
	}
	window := p.window()
	s, err := b.sampleLogs(ctx, sel, window, p.Facet, p.clause()...)
	if err != nil {
		return failed(err, sel.query(p.clause()...))
	}

	// Lines that mask alike on the same host and version are one exemplar: repeating a sanitised line
	// adds nothing and spends the response budget.
	out := make([]*investigationv1.Exemplar, 0, limit)
	seen := map[string]bool{}
	for _, line := range s.lines {
		if p.Kind == handlePattern && !matches(p.Group, line.Text) {
			continue
		}
		fingerprint := engine.MaskLine(line.Text) + "\x00" + line.PodOrHost + "\x00" + line.Version
		if seen[fingerprint] {
			continue
		}
		seen[fingerprint] = true
		out = append(out, &investigationv1.Exemplar{Text: line.Text,
			JoinKeys: joinKeys(sel, line.Version, line.PodOrHost, line.At)})
		if len(out) >= limit {
			break
		}
	}
	coverage, err := b.sampleCoverage(sel, s, int64(len(s.lines)), s.truncation("exemplar"))
	if err != nil {
		return answer{}, err
	}
	digest := &investigationv1.Digest{
		Body: &investigationv1.Digest_Exemplars{Exemplars: &investigationv1.ExemplarDigest{
			Exemplars: out, Cap: uint32(limit),
		}},
		Coverage: coverage,
	}
	query := sel.query(p.clause()...)
	return answer{outcome: b.sampleOutcome(s, len(out) > 0, digest), query: query,
		vocabulary: feeder.VocabDatadogLogs, deepLink: b.deepLink(query, window)}, nil
}

// handleOf reads a handle this backend minted, and its selector.
func (b *Backend) handleOf(h *investigationv1.Handle) (handlePayload, selector, error) {
	p, err := parseHandle(h)
	if err != nil {
		return handlePayload{}, selector{}, err
	}
	sel, err := parseSelector(p.Selector)
	if err != nil {
		return handlePayload{}, selector{}, err
	}
	return p, sel, nil
}

// clause is the extra query term a version handle narrows by; a pattern handle narrows after reading.
func (p handlePayload) clause() []string {
	if p.Kind != handleVersion || p.Facet == "" {
		return nil
	}
	return []string{facetClause(p.Facet, p.Group)}
}

// facetClause spells `facet:value`, quoting a value Datadog's query syntax would otherwise split.
func facetClause(facet, value string) string {
	if strings.ContainsAny(value, ` :"()\*?`) {
		value = `"` + strings.ReplaceAll(value, `"`, `\"`) + `"`
	}
	return facet + ":" + value
}

func unsupported(query string, err error) answer {
	return answer{outcome: engine.QueryFailed{Reason: investigationv1.FailureReason_UNSUPPORTED_POINTER,
		Detail: err.Error()}, query: query, vocabulary: feeder.VocabDatadogLogs}
}

// joinKeys are the keys every group and pattern preserves (contract §6).
func joinKeys(sel selector, version, host string, firstSeen time.Time) *investigationv1.JoinKeys {
	keys := &investigationv1.JoinKeys{Version: version, Workload: sel.Service, PodOrHost: host}
	if !firstSeen.IsZero() {
		keys.FirstSeen = timestamppb.New(firstSeen.UTC())
	}
	return keys
}

// countLines is the window's total line count, from one aggregate. A failure leaves the total unstated
// rather than failing an answer that is already in hand.
func (b *Backend) countLines(ctx context.Context, sel selector, window *engine.Window) (int64, bool) {
	out, _, err := b.client.AggregateLogs(ctx, datadogx.AggregateRequest{
		Compute: []datadogx.Compute{{Aggregation: "count", Type: "total"}},
		Filter:  filterOf(sel, window, b.indexes),
	})
	if err != nil || len(out.Data.Buckets) == 0 {
		return 0, false
	}
	return out.Data.Buckets[0].Count(0)
}
