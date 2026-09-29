// SPDX-License-Identifier: Apache-2.0

package datadog

import (
	"context"
	"errors"
	"math"
	"net/http"
	"sort"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/datadogx"
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	sdk "github.com/Pierre-Theophile/aisre/pkg/backend"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/versionstamp"
)

// `errors_by_version`: error lines grouped by the version the service stamps on its own logs (005
// FR-040b–FR-040d; contract datadog-telemetry-backend.md §3.1).
//
// Two aggregates over the same window and the same facet: one with the error-level clause for the
// errors, one without for the total. Lines that carry no stamp form their own named group — the
// `missing` sentinel — rather than vanishing, because a stamp missing from the lines a crash handler
// writes is exactly the case where errors hide. Every group then names its deploy reference through
// pkg/feeder/versionstamp, the rule every backend applies, so the group for a new commit resolves to the
// change that shipped it, whichever feeder recorded that.
//
// A pointer with no version stamp never reaches here: the engine answers it (ADR-0010 item 2).

// missingVersion is the group Datadog puts lines without the facet in. It is not a version, and it is
// never normalised into a deploy reference.
const missingVersion = "__no_version_stamp__"

// maxVersions bounds the groups asked for. Datadog allows up to 10 000 across group-bys; a service
// with more live versions than this in one window is a fact the truncation states.
const maxVersions = 100

// versionCount is one group's lines and how many of them are errors.
type versionCount struct{ errors, total int64 }

// tally is what either path counted: per version, before any group is named.
type tally struct {
	counts     map[string]*versionCount
	considered int64
	truncation string
	resp       *datadogx.Response
	// sampled is set when the counts come from a paged search, not from Datadog's aggregation.
	sampled *sample
	// groupCap is true when the aggregate hit the group cap.
	groupCap bool
}

func (t *tally) row(version string) *versionCount {
	if r, ok := t.counts[version]; ok {
		return r
	}
	r := &versionCount{}
	t.counts[version] = r
	return r
}

func (b *Backend) errorsByVersion(ctx context.Context, term *investigationv1.ErrorsByVersionTerm) (answer, error) {
	sel, err := parseSelector(term.GetPointer().GetSelector())
	if err != nil {
		return answer{outcome: engine.QueryFailed{Reason: investigationv1.FailureReason_UNSUPPORTED_POINTER,
			Detail: err.Error()}, query: term.GetPointer().GetSelector(), vocabulary: feeder.VocabDatadogLogs}, nil
	}
	window, narrowed := narrow(term.GetWindow(), WindowCapStandard)
	facet := term.GetVersionAttribute()
	group := []datadogx.GroupBy{{Facet: facet, Limit: maxVersions, Missing: missingVersion}}
	count := []datadogx.Compute{{Aggregation: "count", Type: "total"}}
	totalReq := datadogx.AggregateRequest{Compute: count, Filter: filterOf(sel, window, b.indexes), GroupBy: group}
	errorReq := datadogx.AggregateRequest{Compute: count, Filter: filterOf(sel, window, b.indexes, "status:error"), GroupBy: group}
	query := canonical([]datadogx.AggregateRequest{errorReq, totalReq})

	t, err := b.tallyByAggregate(ctx, facet, errorReq, totalReq)
	if notGroupable(err) {
		// Datadog will not group by this attribute: count it from the newest lines instead, and say so.
		query = sel.query()
		t, err = b.tallyBySample(ctx, sel, window, facet)
	}
	if err != nil {
		return failed(err, query)
	}

	key := termKeyOf(term)
	handleWindow := window
	if t.sampled != nil {
		handleWindow = t.sampled.window
	}
	rows := make([]*investigationv1.VersionBreakdown, 0, len(t.counts))
	for version, c := range t.counts {
		r := &investigationv1.VersionBreakdown{Version: version, Errors: c.errors, Total: c.total}
		if r.Total > 0 {
			r.ErrorRate = math.Round(float64(r.Errors)/float64(r.Total)*1e6) / 1e6
		}
		if version == missingVersion {
			// Lines without the stamp: a named group with no version and no deploy reference.
			r.Version = ""
			r.DeployRefAbsentReason = investigationv1.DeployRefAbsentReason_NOT_A_STABLE_IDENTIFIER
		} else {
			r.DeployRef, r.DeployRefAbsentReason = versionstamp.Normalise(version)
			r.JoinKeys = &investigationv1.JoinKeys{Version: version, Workload: sel.Service}
			r.DrillDown = b.mint(key, handleVersion, term.GetPointer().GetSelector(), facet, version, handleWindow)
		}
		rows = append(rows, r)
	}
	// Worst error rate first, so the reader's eye lands on the failing version; ties by version so
	// two recordings of one world are byte-identical.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].ErrorRate != rows[j].ErrorRate {
			return rows[i].ErrorRate > rows[j].ErrorRate
		}
		return rows[i].Version < rows[j].Version
	})

	truncation := t.truncation
	if narrowed {
		truncation = joinTruncation(narrowedText(sdk.TermErrorsByVersion, WindowCapStandard), truncation)
	}
	if t.groupCap {
		truncation = joinTruncation(truncation, "group_cap: at most 100 versions are grouped; the rest are not in the answer")
	}
	var coverage *investigationv1.Coverage
	if t.sampled != nil {
		coverage, err = b.sampleCoverage(sel, *t.sampled, t.considered, truncation)
	} else {
		coverage, err = b.coverageOf(sel, window, t.considered, "none", truncation, t.resp)
	}
	if err != nil {
		return answer{}, err
	}
	digest := &investigationv1.Digest{
		Body: &investigationv1.Digest_ErrorsByVersion{ErrorsByVersion: &investigationv1.ErrorsByVersionDigest{
			Versions: rows, VersionAttribute: facet,
		}},
		Coverage: coverage,
	}
	var outcome engine.Outcome = engine.DigestOutcome{Body: digest}
	switch {
	case t.sampled != nil:
		// A sample the cap stopped, or Datadog reported incomplete, is PARTIAL whatever it found.
		outcome = b.sampleOutcome(*t.sampled, t.considered > 0, digest)
	case t.considered == 0:
		outcome = engine.NoData{Coverage: coverage}
	}
	return answer{outcome: outcome, query: query, vocabulary: feeder.VocabDatadogLogs,
		deepLink: b.deepLink(sel.query("status:error"), window)}, nil
}

// tallyByAggregate counts with two aggregates grouped by the attribute: exact, and cheap.
func (b *Backend) tallyByAggregate(ctx context.Context, facet string, errorReq, totalReq datadogx.AggregateRequest) (tally, error) {
	errs, _, err := b.client.AggregateLogs(ctx, errorReq)
	if err != nil {
		return tally{}, err
	}
	totals, totalResp, err := b.client.AggregateLogs(ctx, totalReq)
	if err != nil {
		return tally{}, err
	}
	t := tally{counts: map[string]*versionCount{}, resp: totalResp, groupCap: len(totals.Data.Buckets) >= maxVersions}
	for _, bucket := range totals.Data.Buckets {
		n, _ := bucket.Count(0)
		t.row(bucket.Key(facet)).total = n
		t.considered += n
	}
	for _, bucket := range errs.Data.Buckets {
		n, _ := bucket.Count(0)
		t.row(bucket.Key(facet)).errors = n
	}
	if errs.Meta.Partial() || totals.Meta.Partial() {
		t.truncation = "partial: Datadog reported a timeout or warnings for this aggregation"
	}
	return t, nil
}

// tallyBySample counts from the newest lines up to the line cap, grouping client-side: the fallback for
// an attribute the aggregate API will not group by (contract §3.1).
func (b *Backend) tallyBySample(ctx context.Context, sel selector, window *engine.Window, facet string) (tally, error) {
	s, err := b.sampleLogs(ctx, sel, window, facet)
	if err != nil {
		return tally{}, err
	}
	s.why = "the aggregate API would not group by " + facet + ", so the lines were read and grouped here"
	t := tally{counts: map[string]*versionCount{}, resp: s.last, sampled: &s, considered: int64(len(s.lines)),
		truncation: s.truncation("errors_by_version")}
	for _, line := range s.lines {
		version := line.Version
		if version == "" {
			version = missingVersion
		}
		c := t.row(version)
		c.total++
		if line.Status == "error" {
			c.errors++
		}
	}
	return t, nil
}

// notGroupable reports whether Datadog refused the aggregate itself — a 400 — rather than failing to
// answer it. Anything else (a rate limit, a permission, a timeout) is not a reason to sample.
func notGroupable(err error) bool {
	var status *datadogx.StatusError
	return errors.As(err, &status) && status.Status == http.StatusBadRequest
}

func joinTruncation(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

// termKeyOf is the key a handle records as its minting term.
func termKeyOf(term *investigationv1.ErrorsByVersionTerm) string {
	key, err := engine.TermKey(engine.ErrorsByVersion(term.GetPointer(), term.GetWindow(), term.GetVersionAttribute()))
	if err != nil {
		return ""
	}
	return key
}
