// SPDX-License-Identifier: Apache-2.0

package backend

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	sdk "github.com/Pierre-Theophile/aisre/pkg/backend"
)

// The digest contract (tasks.md T034; FR-014, FR-014a, FR-014b, FR-027, FR-037;
// contracts/telemetry-backend.md §2–§4; docs/schema/digests.md).
//
// A digest is everything a worker or a backend is allowed to return. This file is where the
// four mandatory parts are built and checked, where the six outcomes are kept apart, and where
// the caps are applied — and it is deliberately the only file that can produce an
// AlgebraResponse, so that "every digest carries a coverage block" is a property of the type
// rather than a promise made by each implementation.
//
// Three things here are contract rather than convenience.
//
// The six outcomes are distinct Go types, not six values of one enum with a comment. The defect
// FR-027 names — reporting NOT_YET_INGESTED, QUERY_FAILED or NOT_RECORDED as if it were
// NO_DATA — is a defect precisely because the six read alike once they are collapsed into "the
// answer was empty". Making them six types means a caller that wants "nothing happened" has to
// write down NoData, and a renderer has to say something different about each.
//
// Truncation is written into the response it applies to, never reported alongside it. A replay
// must reproduce the truncation the investigator actually saw, and a note kept outside the
// response is a note the recording does not hold (FR-037).
//
// Coverage is checked at construction. A digest that reaches a consumer without one has already
// cost the reader the one thing they needed: "this window was searched and held nothing" and
// "we do not know what was searched" are different sentences.

// The published per-response caps. They bound what one answer may cost the investigator's
// context and, through it, what a recorded world may cost a reviewer reading a diff. Exceeding
// one is not an error: the answer is truncated and the truncation is written into it, because a
// bounded answer that says it was bounded is evidence and an unbounded one is a liability.
const (
	// MaxSeriesPerDigest caps SeriesSummary entries in a metric digest.
	MaxSeriesPerDigest = 50
	// MaxComparisons caps Comparison entries in a metric or trace digest.
	MaxComparisons = 50
	// MaxLogPatterns caps mined templates in a log digest.
	MaxLogPatterns = 50
	// MaxSpanGroups caps operation × error-kind groups in a trace digest.
	MaxSpanGroups = 50
	// MaxVersionBreakdowns caps version splits in an errors_by_version digest.
	MaxVersionBreakdowns = 25
	// MaxMonitorTransitions caps transitions in a monitor-state digest.
	MaxMonitorTransitions = 100
	// MaxKnowledgeItems caps cited documents in a knowledge digest.
	MaxKnowledgeItems = 20
	// MaxExemplars caps exemplars, which are the one raw-ish thing that may cross the boundary
	// and are therefore capped hardest.
	MaxExemplars = 10
	// MaxFreeTextBytes caps the one bounded free-text field per digest.
	MaxFreeTextBytes = 512
	// MaxExemplarBytes caps one exemplar's text.
	MaxExemplarBytes = 512
	// MaxResponseBytes caps the canonical encoding of a whole response. A response over it is
	// truncated family by family until it fits, and says so.
	MaxResponseBytes = 64 * 1024
)

// The published truncation criteria, so that "what was dropped and by which criterion" is a
// closed vocabulary a fixture can assert on rather than prose.
const (
	// CriterionCardinalityCap is the per-family cardinality cap above.
	CriterionCardinalityCap = "cardinality_cap"
	// CriterionResponseSizeCap is the whole-response byte cap.
	CriterionResponseSizeCap = "response_size_cap"
	// CriterionFreeTextCap is the free-text byte cap.
	CriterionFreeTextCap = "free_text_cap"
)

// Outcome is one of the six published outcomes, kept distinct at the type level (FR-027).
//
// Only NoData is evidence that nothing happened. A consumer that wants to conclude "nothing
// happened" must match on NoData; the other five do not satisfy the interface's Empty contract,
// so the collapse cannot be written by accident.
type Outcome interface {
	// Kind is the published enum value this outcome carries into the response.
	Kind() investigationv1.TermOutcome
	// MeansNothingHappened reports whether this outcome is evidence that nothing happened.
	// It is true for NoData and false for every other outcome, which is the whole of FR-027.
	MeansNothingHappened() bool
	// Render is the one-line human rendering. Each outcome renders differently, because a
	// reader who cannot tell them apart has been told nothing.
	Render() string
	// apply writes the outcome onto a response under construction.
	apply(resp *investigationv1.AlgebraResponse)
}

// DigestOutcome is a bounded, structured answer.
type DigestOutcome struct {
	// Body is the per-family digest.
	Body *investigationv1.Digest
}

// Kind implements Outcome.
func (DigestOutcome) Kind() investigationv1.TermOutcome { return investigationv1.TermOutcome_DIGEST }

// MeansNothingHappened implements Outcome.
func (DigestOutcome) MeansNothingHappened() bool { return false }

// Render implements Outcome.
func (DigestOutcome) Render() string { return "digest" }

func (o DigestOutcome) apply(resp *investigationv1.AlgebraResponse) {
	resp.Outcome = o.Kind()
	resp.Digest = o.Body
}

// NoData is a valid query over a covered window that holds nothing. It is the only outcome that
// is evidence that nothing happened, and the only one a hypothesis may be refuted on for
// absence.
//
// It is also the answer a backend gives when the organisation has no such data source at all:
// the coverage block names the absent source as the reason and names what was in fact searched,
// identically in live and recorded mode, stable for the whole window, so a consumer concludes
// "this cannot be checked here" instead of retrying (contracts/telemetry-backend.md §1).
type NoData struct {
	// Coverage is mandatory, as on every other outcome: an empty answer whose coverage is
	// unknown says nothing at all.
	Coverage *investigationv1.Coverage
	// AbsentSource names the data source the organisation does not have, when that is why the
	// answer is empty. Empty means the source exists and held nothing.
	AbsentSource string
}

// Kind implements Outcome.
func (NoData) Kind() investigationv1.TermOutcome { return investigationv1.TermOutcome_NO_DATA }

// MeansNothingHappened implements Outcome.
func (NoData) MeansNothingHappened() bool { return true }

// Render implements Outcome.
func (o NoData) Render() string {
	if o.AbsentSource != "" {
		return "no data: this organisation has no " + o.AbsentSource + "; the question cannot be checked here"
	}
	return "no data: the window was covered and held nothing"
}

func (o NoData) apply(resp *investigationv1.AlgebraResponse) {
	resp.Outcome = o.Kind()
	coverage := o.Coverage
	if o.AbsentSource != "" && coverage != nil && coverage.GetTruncation() == "" {
		coverage.Truncation = "absent_source:" + o.AbsentSource
	}
	resp.Digest = &investigationv1.Digest{Coverage: coverage}
}

// NotYetIngested is a window inside the backend's indexing lag, so an empty answer means
// nothing and retrying later means something.
type NotYetIngested struct {
	// Coverage carries the lag that produced this outcome.
	Coverage *investigationv1.Coverage
	// RetryAfter is when the window is expected to be covered.
	RetryAfter time.Time
}

// Kind implements Outcome.
func (NotYetIngested) Kind() investigationv1.TermOutcome {
	return investigationv1.TermOutcome_NOT_YET_INGESTED
}

// MeansNothingHappened implements Outcome.
func (NotYetIngested) MeansNothingHappened() bool { return false }

// Render implements Outcome.
func (o NotYetIngested) Render() string {
	if o.RetryAfter.IsZero() {
		return "not yet ingested: the window is inside the backend's indexing lag, so an empty answer means nothing"
	}
	return "not yet ingested: the window is inside the backend's indexing lag; covered after " +
		o.RetryAfter.UTC().Format(time.RFC3339)
}

func (o NotYetIngested) apply(resp *investigationv1.AlgebraResponse) {
	resp.Outcome = o.Kind()
	resp.Digest = &investigationv1.Digest{Coverage: o.Coverage}
}

// QueryFailed carries a reason from the published set. It is never a silence: the engine
// records it as an evidence item and the hypotheses it served become untested with that reason.
type QueryFailed struct {
	// Reason is from the published FailureReason set.
	Reason investigationv1.FailureReason
	// Detail names what was asked and, for OUTSIDE_ALGEBRA, what is available.
	Detail string
	// Coverage is what the backend managed to establish before it failed. It may be nil, and
	// is the one outcome for which that is allowed: a query that never ran covered nothing.
	Coverage *investigationv1.Coverage
}

// Kind implements Outcome.
func (QueryFailed) Kind() investigationv1.TermOutcome {
	return investigationv1.TermOutcome_QUERY_FAILED
}

// MeansNothingHappened implements Outcome.
func (QueryFailed) MeansNothingHappened() bool { return false }

// Render implements Outcome.
func (o QueryFailed) Render() string {
	return "query failed (" + strings.ToLower(o.Reason.String()) + "): " + o.Detail
}

func (o QueryFailed) apply(resp *investigationv1.AlgebraResponse) {
	resp.Outcome = o.Kind()
	resp.FailureReason = o.Reason
	resp.FailureDetail = o.Detail
	if o.Coverage != nil {
		resp.Digest = &investigationv1.Digest{Coverage: o.Coverage}
	}
}

// Partial is some of the answer, with what is missing named.
type Partial struct {
	// Body is what was retrieved.
	Body *investigationv1.Digest
	// Missing names what is not in it, in the caller's terms.
	Missing string
}

// Kind implements Outcome.
func (Partial) Kind() investigationv1.TermOutcome { return investigationv1.TermOutcome_PARTIAL }

// MeansNothingHappened implements Outcome.
func (Partial) MeansNothingHappened() bool { return false }

// Render implements Outcome.
func (o Partial) Render() string { return "partial: missing " + o.Missing }

func (o Partial) apply(resp *investigationv1.AlgebraResponse) {
	resp.Outcome = o.Kind()
	resp.Digest = o.Body
	resp.FailureDetail = o.Missing
}

// NotRecorded is recorded mode only: the term is in the algebra and this world does not hold
// it. The term is echoed back so the caller can see exactly what it asked for, and the miss is
// counted against the fixture's miss rate — never against the engine's score (FR-042c).
type NotRecorded struct {
	// Term is the request as asked, echoed.
	Term *Term
	// TermKey is the key that missed, so a reviewer can look for it in world/index.json.
	TermKey string
	// Coverage says what the world does hold, so that "not recorded" is a statement about the
	// recording rather than about production.
	Coverage *investigationv1.Coverage
}

// Kind implements Outcome.
func (NotRecorded) Kind() investigationv1.TermOutcome {
	return investigationv1.TermOutcome_NOT_RECORDED
}

// MeansNothingHappened implements Outcome.
func (NotRecorded) MeansNothingHappened() bool { return false }

// Render implements Outcome.
func (o NotRecorded) Render() string {
	return "not recorded: " + TermName(o.Term) + " (" + o.TermKey +
		") is in the algebra but this world does not hold it; it is a gap in the recording, not a fact about production"
}

func (o NotRecorded) apply(resp *investigationv1.AlgebraResponse) {
	resp.Outcome = o.Kind()
	resp.Digest = &investigationv1.Digest{Coverage: o.Coverage}
}

// OutcomeOf reconstructs the typed outcome of a response, so that a consumer reading a recorded
// world gets the same six types a live call produces.
func OutcomeOf(resp *investigationv1.AlgebraResponse) (Outcome, error) {
	coverage := resp.GetDigest().GetCoverage()
	switch resp.GetOutcome() {
	case investigationv1.TermOutcome_DIGEST:
		return DigestOutcome{Body: resp.GetDigest()}, nil
	case investigationv1.TermOutcome_NO_DATA:
		return NoData{Coverage: coverage, AbsentSource: strings.TrimPrefix(coverage.GetTruncation(), "absent_source:")}, nil
	case investigationv1.TermOutcome_NOT_YET_INGESTED:
		return NotYetIngested{Coverage: coverage}, nil
	case investigationv1.TermOutcome_QUERY_FAILED:
		return QueryFailed{Reason: resp.GetFailureReason(), Detail: resp.GetFailureDetail(), Coverage: coverage}, nil
	case investigationv1.TermOutcome_PARTIAL:
		return Partial{Body: resp.GetDigest(), Missing: resp.GetFailureDetail()}, nil
	case investigationv1.TermOutcome_NOT_RECORDED:
		return NotRecorded{TermKey: resp.GetTermKey(), Coverage: coverage}, nil
	default:
		return nil, fmt.Errorf("backend: response carries outcome %s, which is not one of the six published outcomes",
			resp.GetOutcome())
	}
}

// CoverageInput is what a backend states about the search it performed. Every field is either
// given or explicitly undetermined: a field that cannot be determined is stated as undetermined,
// never omitted (contracts/telemetry-backend.md §2).
type CoverageInput struct {
	// SearchedEntities are the graph entities the query was about.
	SearchedEntities []string
	// DataSource is the index, table or metric namespace searched.
	DataSource string
	// WindowCovered is the window actually covered, which may be narrower than the one asked
	// for.
	WindowCovered *Window
	// VolumeConsidered is points, lines or spans scanned.
	VolumeConsidered int64
	// VolumeUndetermined says the backend cannot report a volume.
	VolumeUndetermined bool
	// Sampling is what the backend applied or was asked for.
	Sampling string
	// Truncation is what was dropped and by which criterion, from the backend's own side.
	Truncation string
	// IngestionLag is the backend's indexing lag at the instant of execution.
	IngestionLag time.Duration
	// IngestionLagUndetermined says the backend cannot report a lag.
	IngestionLagUndetermined bool
	// ExecutedAt is the instant of execution.
	ExecutedAt time.Time
	// RemainingQuota and QuotaWindow are the vendor's quota headers where it reports them;
	// this is what the budget manager spends a share of.
	RemainingQuota int64
	// QuotaWindow is the window RemainingQuota applies over.
	QuotaWindow time.Duration
	// QuotaUndetermined says the vendor reports no quota.
	QuotaUndetermined bool
}

// Coverage builds the mandatory coverage block, refusing an input that would produce one a
// reader cannot act on.
func (in CoverageInput) Coverage() (*investigationv1.Coverage, error) {
	if in.DataSource == "" {
		return nil, Reject(ReasonMissingCoverage,
			"coverage names no data source; a digest that does not say what was searched cannot be read against anything")
	}
	if in.WindowCovered.GetStart() == nil || in.WindowCovered.GetEnd() == nil {
		return nil, Reject(ReasonMissingCoverage,
			"coverage from %s names no window actually covered; the difference between the window asked for and the window covered is most of what coverage is for",
			in.DataSource)
	}
	if in.ExecutedAt.IsZero() {
		return nil, Reject(ReasonMissingCoverage,
			"coverage from %s names no execution instant; the indexing lag it reports is only meaningful relative to one",
			in.DataSource)
	}
	if !in.VolumeUndetermined && in.VolumeConsidered < 0 {
		return nil, Reject(ReasonMissingCoverage,
			"coverage from %s reports a negative volume; state it as undetermined instead", in.DataSource)
	}
	searched := append([]string(nil), in.SearchedEntities...)
	sort.Strings(searched)
	return &investigationv1.Coverage{
		SearchedEntities:         searched,
		DataSource:               in.DataSource,
		WindowActuallyCovered:    in.WindowCovered,
		VolumeConsidered:         in.VolumeConsidered,
		VolumeUndetermined:       in.VolumeUndetermined,
		Sampling:                 in.Sampling,
		Truncation:               in.Truncation,
		IngestionLagSeconds:      int64(in.IngestionLag / time.Second),
		IngestionLagUndetermined: in.IngestionLagUndetermined,
		ExecutedAt:               timestamppb.New(in.ExecutedAt.UTC().Truncate(time.Second)),
		RemainingQuota:           in.RemainingQuota,
		QuotaWindowSeconds:       int64(in.QuotaWindow / time.Second),
		QuotaUndetermined:        in.QuotaUndetermined,
	}, nil
}

// ValidateCoverage refuses a digest that carries no coverage block (FR-014a). It is called on
// every response this package builds and again by the worker registry on every response a
// worker returns, because the rule is about what reaches a consumer rather than about who
// produced it.
func ValidateCoverage(resp *investigationv1.AlgebraResponse) error {
	if resp.GetOutcome() == investigationv1.TermOutcome_QUERY_FAILED {
		// A query that never ran covered nothing. Every other outcome, including NO_DATA,
		// carries coverage: that is what makes an empty answer readable.
		return nil
	}
	coverage := resp.GetDigest().GetCoverage()
	if coverage == nil {
		return Reject(ReasonMissingCoverage,
			"a %s response carries no coverage block; \"this window was searched and held nothing\" and \"we do not know what was searched\" are different sentences",
			strings.ToLower(resp.GetOutcome().String()))
	}
	if coverage.GetDataSource() == "" || coverage.GetWindowActuallyCovered() == nil {
		return Reject(ReasonMissingCoverage,
			"a %s response carries a coverage block naming no data source or no covered window",
			strings.ToLower(resp.GetOutcome().String()))
	}
	return nil
}

// ResponseInput is one answer under construction: the request it answers, the outcome, the cost
// class it was priced at, and the backend and vocabulary that produced it.
type ResponseInput struct {
	// Request is the call being answered; its term is what the response is keyed by.
	Request *Request
	// Outcome is one of the six.
	Outcome Outcome
	// Mode is "live" or "recorded", recorded per call.
	Mode string
	// CostClass is what the budget manager spends against.
	CostClass investigationv1.CostClass
	// Duration is wall-clock time in the backend; it is not part of the response digest.
	Duration time.Duration
	// BackendVersion, Vocabulary, ExecutedQuery and DeepLink are the digest's own evidence:
	// the exact query as sent, and who sent it.
	BackendVersion string
	// Vocabulary is the selector vocabulary the executed query is written in.
	Vocabulary string
	// ExecutedQuery is the exact query as sent.
	ExecutedQuery string
	// DeepLink opens the same query over the same window for a person.
	DeepLink string
	// FreeText is the one bounded free-text field. It is flagged unverified and is never
	// citable as evidence on its own; anything longer than the cap is truncated and said so.
	FreeText string
}

// NewResponse builds and validates one response: caps applied, truncation written in, coverage
// checked, term key and response digest computed.
//
// It is the only constructor of an AlgebraResponse in this repository. Everything a consumer
// relies on — a coverage block on every digest, a bounded size, a stable key — holds because
// there is no other way to make one.
func NewResponse(in ResponseInput) (*Response, error) {
	if in.Request == nil || in.Request.GetTerm() == nil {
		return nil, OutsideAlgebra("")
	}
	if in.Outcome == nil {
		return nil, fmt.Errorf("backend: a response with no outcome; the six outcomes are never collapsed and one of them is always the answer")
	}
	key, err := TermKey(in.Request.GetTerm())
	if err != nil {
		return nil, err
	}

	resp := &investigationv1.AlgebraResponse{
		TermKey:    key,
		DurationMs: in.Duration.Milliseconds(),
		Mode:       in.Mode,
		CostClass:  in.CostClass,
	}
	in.Outcome.apply(resp)

	if resp.GetDigest() != nil {
		d := resp.GetDigest()
		d.BackendVersion = in.BackendVersion
		d.Vocabulary = in.Vocabulary
		d.ExecutedQuery = in.ExecutedQuery
		d.DeepLink = in.DeepLink
		applyFreeText(d, in.FreeText)
		orderDigest(d)
		applyCaps(d)
		if err := applySizeCap(d); err != nil {
			return nil, err
		}
	}
	if err := ValidateCoverage(resp); err != nil {
		return nil, err
	}
	digest, err := sdk.ResponseDigest(resp)
	if err != nil {
		return nil, err
	}
	resp.ResponseDigest = digest
	return resp, nil
}

// applyFreeText writes the one bounded free-text field, truncating it into itself. The field is
// flagged unverified in the rendering, never citable alone (FR-014b): a marker in the value is
// what survives serialisation into a world file and a golden diff.
func applyFreeText(d *investigationv1.Digest, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if len(text) > MaxFreeTextBytes {
		text = text[:MaxFreeTextBytes-len(freeTextEllipsis)] + freeTextEllipsis
		noteTruncation(d, "free_text", CriterionFreeTextCap)
	}
	d.FreeText = freeTextPrefix + text
}

const (
	// FreeTextPrefix marks the one bounded free-text field as unverified wherever it is
	// rendered or recorded. A reader who quotes it is quoting the marker too.
	freeTextPrefix   = "unverified: "
	freeTextEllipsis = "…"
)

// FreeTextPrefix is the published marker on the one bounded free-text field.
const FreeTextPrefix = freeTextPrefix

// orderDigest imposes the published total ordering on every repeated field, before the caps are
// applied.
//
// It runs here rather than in each backend or each worker for two reasons. The cap drops from the
// tail, so the ordering decides **which rows survive truncation** — that cannot be left to a
// vendor's pagination. And a worker that re-ordered after the fact would produce a digest whose
// bytes differ from the recorded ones for the same question, which would make a byte-for-byte
// golden comparison impossible for exactly the digests it matters most for.
//
// The rule in every family is the same: what changed first, then what is largest, then the name,
// so the ordering is total and the reader's eye lands on the lead.
func orderDigest(d *investigationv1.Digest) {
	switch body := d.GetBody().(type) {
	case *investigationv1.Digest_Metric:
		sort.SliceStable(body.Metric.GetSeries(), func(i, j int) bool {
			return seriesKey(body.Metric.GetSeries()[i]) < seriesKey(body.Metric.GetSeries()[j])
		})
	case *investigationv1.Digest_Log:
		patterns := body.Log.GetPatterns()
		sort.SliceStable(patterns, func(i, j int) bool {
			if patterns[i].GetNewInWindow() != patterns[j].GetNewInWindow() {
				return patterns[i].GetNewInWindow()
			}
			if patterns[i].GetCount() != patterns[j].GetCount() {
				return patterns[i].GetCount() > patterns[j].GetCount()
			}
			return patterns[i].GetTemplate() < patterns[j].GetTemplate()
		})
	case *investigationv1.Digest_Trace:
		groups := body.Trace.GetGroups()
		sort.SliceStable(groups, func(i, j int) bool {
			iErr, jErr := groups[i].GetErrorKind() != "", groups[j].GetErrorKind() != ""
			if iErr != jErr {
				return iErr
			}
			if groups[i].GetCount() != groups[j].GetCount() {
				return groups[i].GetCount() > groups[j].GetCount()
			}
			if groups[i].GetOperation() != groups[j].GetOperation() {
				return groups[i].GetOperation() < groups[j].GetOperation()
			}
			return groups[i].GetErrorKind() < groups[j].GetErrorKind()
		})
	case *investigationv1.Digest_MonitorState:
		transitions := body.MonitorState.GetTransitions()
		sort.SliceStable(transitions, func(i, j int) bool {
			if !transitions[i].GetAt().AsTime().Equal(transitions[j].GetAt().AsTime()) {
				return transitions[i].GetAt().AsTime().Before(transitions[j].GetAt().AsTime())
			}
			return transitions[i].GetGroupKey() < transitions[j].GetGroupKey()
		})
	case *investigationv1.Digest_ErrorsByVersion:
		versions := body.ErrorsByVersion.GetVersions()
		sort.SliceStable(versions, func(i, j int) bool {
			if versions[i].GetErrorRate() != versions[j].GetErrorRate() {
				return versions[i].GetErrorRate() > versions[j].GetErrorRate()
			}
			return versions[i].GetVersion() < versions[j].GetVersion()
		})
	case *investigationv1.Digest_Knowledge:
		items := body.Knowledge.GetItems()
		sort.SliceStable(items, func(i, j int) bool {
			if items[i].GetScore() != items[j].GetScore() {
				return items[i].GetScore() > items[j].GetScore()
			}
			return items[i].GetDocumentId() < items[j].GetDocumentId()
		})
	case *investigationv1.Digest_Exemplars, *investigationv1.Digest_Onset:
		// Exemplars keep the order they were sampled in, which is chronological and is the
		// order a person reads them in. An onset digest is one estimate.
	}
}

// seriesKey is the total order over series summaries: the sorted tag set, which is the only
// thing that distinguishes one series of the same selector from another.
func seriesKey(series *investigationv1.SeriesSummary) string {
	keys := make([]string, 0, len(series.GetTags()))
	for key, value := range series.GetTags() {
		keys = append(keys, key+"="+value)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

// applyCaps truncates each family to its published cardinality cap, writing the truncation into
// the response. Entries are dropped from the tail of the published ordering above, so the cap
// drops the least interesting rows.
func applyCaps(d *investigationv1.Digest) {
	switch body := d.GetBody().(type) {
	case *investigationv1.Digest_Metric:
		if n := len(body.Metric.GetSeries()); n > MaxSeriesPerDigest {
			body.Metric.Series = body.Metric.GetSeries()[:MaxSeriesPerDigest]
			noteCardinality(d, "series", n, MaxSeriesPerDigest)
		}
		if n := len(body.Metric.GetComparisons()); n > MaxComparisons {
			body.Metric.Comparisons = body.Metric.GetComparisons()[:MaxComparisons]
			noteCardinality(d, "comparisons", n, MaxComparisons)
		}
	case *investigationv1.Digest_Log:
		if n := len(body.Log.GetPatterns()); n > MaxLogPatterns {
			body.Log.Patterns = body.Log.GetPatterns()[:MaxLogPatterns]
			noteCardinality(d, "patterns", n, MaxLogPatterns)
		}
	case *investigationv1.Digest_Trace:
		if n := len(body.Trace.GetGroups()); n > MaxSpanGroups {
			body.Trace.Groups = body.Trace.GetGroups()[:MaxSpanGroups]
			noteCardinality(d, "groups", n, MaxSpanGroups)
		}
		if n := len(body.Trace.GetComparisons()); n > MaxComparisons {
			body.Trace.Comparisons = body.Trace.GetComparisons()[:MaxComparisons]
			noteCardinality(d, "comparisons", n, MaxComparisons)
		}
	case *investigationv1.Digest_MonitorState:
		if n := len(body.MonitorState.GetTransitions()); n > MaxMonitorTransitions {
			body.MonitorState.Transitions = body.MonitorState.GetTransitions()[:MaxMonitorTransitions]
			noteCardinality(d, "transitions", n, MaxMonitorTransitions)
		}
	case *investigationv1.Digest_ErrorsByVersion:
		if n := len(body.ErrorsByVersion.GetVersions()); n > MaxVersionBreakdowns {
			body.ErrorsByVersion.Versions = body.ErrorsByVersion.GetVersions()[:MaxVersionBreakdowns]
			noteCardinality(d, "versions", n, MaxVersionBreakdowns)
		}
	case *investigationv1.Digest_Exemplars:
		limit := int(body.Exemplars.GetCap())
		if limit <= 0 || limit > MaxExemplars {
			limit = MaxExemplars
			body.Exemplars.Cap = MaxExemplars
		}
		if n := len(body.Exemplars.GetExemplars()); n > limit {
			body.Exemplars.Exemplars = body.Exemplars.GetExemplars()[:limit]
			noteCardinality(d, "exemplars", n, limit)
		}
		for _, ex := range body.Exemplars.GetExemplars() {
			if len(ex.GetText()) > MaxExemplarBytes {
				ex.Text = ex.GetText()[:MaxExemplarBytes-len(freeTextEllipsis)] + freeTextEllipsis
				noteTruncation(d, "exemplar text", CriterionCardinalityCap)
			}
		}
	case *investigationv1.Digest_Knowledge:
		if n := len(body.Knowledge.GetItems()); n > MaxKnowledgeItems {
			body.Knowledge.Items = body.Knowledge.GetItems()[:MaxKnowledgeItems]
			noteCardinality(d, "items", n, MaxKnowledgeItems)
		}
	case *investigationv1.Digest_Onset:
		// An onset digest is one estimate. There is nothing to cap, which is why it is the
		// cheapest thing in a world to review.
	}
}

// applySizeCap enforces the whole-response byte cap after the cardinality caps, halving the
// largest family until the canonical encoding fits. It refuses rather than silently returning
// an oversized response: a response nobody can bound is a response nobody can budget.
func applySizeCap(d *investigationv1.Digest) error {
	for range 16 {
		encoded, err := graph.CanonicalJSON(d)
		if err != nil {
			return err
		}
		if len(encoded) <= MaxResponseBytes {
			return nil
		}
		if !halveLargest(d) {
			return Reject(ReasonMissingCoverage,
				"a digest of %d bytes cannot be reduced below the %d-byte response cap; the answer is unbounded and an unbounded answer cannot be budgeted",
				len(encoded), MaxResponseBytes)
		}
		noteTruncation(d, "rows", CriterionResponseSizeCap)
	}
	return Reject(ReasonMissingCoverage,
		"a digest could not be reduced below the %d-byte response cap in sixteen passes", MaxResponseBytes)
}

// halveLargest halves the biggest repeated field of the digest, returning false when there is
// nothing left to halve.
func halveLargest(d *investigationv1.Digest) bool {
	switch body := d.GetBody().(type) {
	case *investigationv1.Digest_Metric:
		return halveSeries(body.Metric) || halveComparisons(&body.Metric.Comparisons)
	case *investigationv1.Digest_Log:
		if n := len(body.Log.GetPatterns()); n > 1 {
			body.Log.Patterns = body.Log.GetPatterns()[:n/2]
			return true
		}
	case *investigationv1.Digest_Trace:
		if n := len(body.Trace.GetGroups()); n > 1 {
			body.Trace.Groups = body.Trace.GetGroups()[:n/2]
			return true
		}
		return halveComparisons(&body.Trace.Comparisons)
	case *investigationv1.Digest_MonitorState:
		if n := len(body.MonitorState.GetTransitions()); n > 1 {
			body.MonitorState.Transitions = body.MonitorState.GetTransitions()[:n/2]
			return true
		}
	case *investigationv1.Digest_ErrorsByVersion:
		if n := len(body.ErrorsByVersion.GetVersions()); n > 1 {
			body.ErrorsByVersion.Versions = body.ErrorsByVersion.GetVersions()[:n/2]
			return true
		}
	case *investigationv1.Digest_Exemplars:
		if n := len(body.Exemplars.GetExemplars()); n > 1 {
			body.Exemplars.Exemplars = body.Exemplars.GetExemplars()[:n/2]
			return true
		}
	case *investigationv1.Digest_Knowledge:
		if n := len(body.Knowledge.GetItems()); n > 1 {
			body.Knowledge.Items = body.Knowledge.GetItems()[:n/2]
			return true
		}
	}
	return false
}

func halveSeries(m *investigationv1.MetricDigest) bool {
	if n := len(m.GetSeries()); n > 1 {
		m.Series = m.GetSeries()[:n/2]
		return true
	}
	return false
}

func halveComparisons(comparisons *[]*investigationv1.Comparison) bool {
	if n := len(*comparisons); n > 1 {
		*comparisons = (*comparisons)[:n/2]
		return true
	}
	return false
}

func noteCardinality(d *investigationv1.Digest, what string, had, kept int) {
	noteTruncation(d, fmt.Sprintf("%d of %d %s", had-kept, had, what), CriterionCardinalityCap)
}

// noteTruncation writes the truncation into the response it applies to, accumulating rather
// than overwriting, so a response truncated twice says so twice (FR-037).
func noteTruncation(d *investigationv1.Digest, dropped, criterion string) {
	if d.GetTruncation() == nil {
		d.Truncation = &investigationv1.Truncated{}
	}
	t := d.GetTruncation()
	t.Truncated = true
	if t.GetWhatWasDropped() == "" {
		t.WhatWasDropped = dropped
	} else {
		t.WhatWasDropped += "; " + dropped
	}
	if t.GetCriterion() == "" {
		t.Criterion = criterion
	} else if !strings.Contains(t.GetCriterion(), criterion) {
		t.Criterion += "; " + criterion
	}
}
