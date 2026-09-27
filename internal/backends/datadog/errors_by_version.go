// SPDX-License-Identifier: Apache-2.0

package datadog

import (
	"context"
	"math"
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

	errs, _, err := b.client.AggregateLogs(ctx, errorReq)
	if err != nil {
		return failed(err, query)
	}
	totals, totalResp, err := b.client.AggregateLogs(ctx, totalReq)
	if err != nil {
		return failed(err, query)
	}

	byVersion := map[string]*investigationv1.VersionBreakdown{}
	row := func(version string) *investigationv1.VersionBreakdown {
		if r, ok := byVersion[version]; ok {
			return r
		}
		r := &investigationv1.VersionBreakdown{Version: version}
		byVersion[version] = r
		return r
	}
	var considered int64
	for _, bucket := range totals.Data.Buckets {
		n, _ := bucket.Count(0)
		row(bucket.Key(facet)).Total = n
		considered += n
	}
	for _, bucket := range errs.Data.Buckets {
		n, _ := bucket.Count(0)
		row(bucket.Key(facet)).Errors = n
	}

	key := termKeyOf(term)
	rows := make([]*investigationv1.VersionBreakdown, 0, len(byVersion))
	for version, r := range byVersion {
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
			r.DrillDown = b.mint(key, handleVersion, term.GetPointer().GetSelector(), facet, version, window)
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

	var truncation string
	if narrowed {
		truncation = narrowedText(sdk.TermErrorsByVersion, WindowCapStandard)
	}
	if len(totals.Data.Buckets) >= maxVersions {
		truncation = joinTruncation(truncation, "group_cap: at most 100 versions are grouped; the rest are not in the answer")
	}
	if errs.Meta.Partial() || totals.Meta.Partial() {
		truncation = joinTruncation(truncation, "partial: Datadog reported a timeout or warnings for this aggregation")
	}
	coverage, err := b.coverageOf(sel, window, considered, "none", truncation, totalResp)
	if err != nil {
		return answer{}, err
	}
	var outcome engine.Outcome = engine.DigestOutcome{Body: &investigationv1.Digest{
		Body: &investigationv1.Digest_ErrorsByVersion{ErrorsByVersion: &investigationv1.ErrorsByVersionDigest{
			Versions: rows, VersionAttribute: facet,
		}},
		Coverage: coverage,
	}}
	if considered == 0 {
		outcome = engine.NoData{Coverage: coverage}
	}
	return answer{outcome: outcome, query: query, vocabulary: feeder.VocabDatadogLogs,
		deepLink: b.deepLink(sel.query("status:error"), window)}, nil
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
