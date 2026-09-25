// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"fmt"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/internal/sanitise"
	sdk "github.com/Pierre-Theophile/aisre/pkg/backend"
)

// The backend's declaration and its one entry point (T091, T105; contract §1, §2, §11).
//
// Two properties of this file are contract rather than structure.
//
// **All eight telemetry terms and nothing else.** The graph family is the graph worker's, answered
// by replaying the event log; the knowledge family is the knowledge worker's. A declaration naming
// a term from either is rejected at registration, and so is one that prices no term — an
// unbudgetable term is an unbounded one.
//
// **Executing a term creates no GCP object of any kind**: no saved query, no log sink, no
// dashboard, no alerting policy, no analytics dataset or table (FR-112). That is why §4 of the
// contract refuses log-based metrics and Observability Analytics despite their being the
// technically superior way to aggregate logs — both require creating something, and there is no
// configuration in this feature that enables one. The property is visible here as an absence: the
// transport seam exposes reads and nothing else, so there is no call to make.

// Vendor is the system this backend speaks for.
const Vendor = "gcp"

// Version is the backend's own version, recorded in every digest it returns.
const Version = "0.1.0"

// The vocabularies an executed query is written in, recorded on the digest so a reader knows what
// grammar the `executed_query` field is in.
const (
	// VocabMonitoring is the Cloud Monitoring filter language, versioned by its API surface.
	VocabMonitoring = "gcp-monitoring-filter/v3"
	// VocabLogging is the Cloud Logging query language, versioned by its API surface.
	VocabLogging = "gcp-logging-query/v2"
)

// The window caps, per cost class (budget.md §5, FR-150). A request wider than its cap is
// **narrowed to the cap and says so** rather than issued as asked.
//
// `new_log_patterns` has the tightest, and not out of politeness: with `entries.list` at 60 calls
// per minute per project and no documented page-size maximum, a wide window over a busy service
// exhausts the project's whole per-minute log-read quota in pagination alone — and takes the
// on-call's log console away mid-incident while it does.
const (
	// WindowCapCheap bounds monitor_state and drill_down: an indexed read, so wide.
	WindowCapCheap = 90 * 24 * time.Hour
	// WindowCapStandard bounds compare, errors_by_version and error_spans: one aggregation over
	// one selector and one window pair.
	WindowCapStandard = 7 * 24 * time.Hour
	// WindowCapOnset bounds the onset search window. It is expensive because the series has to
	// come back to this process for the estimator to run on it.
	WindowCapOnset = 24 * time.Hour
	// WindowCapLogs bounds new_log_patterns and exemplars, which spend logging.read.
	WindowCapLogs = 6 * time.Hour
)

// The published truncation criteria this backend adds to the engine's. They are a closed
// vocabulary rather than prose because a fixture asserts on them, and because a caveat written
// three different ways in three digests is a caveat nobody can check.
const (
	// CriterionWindowCap is a window narrowed to its published cap.
	CriterionWindowCap = "window_cap"
	// CriterionLogSampleBound is the bounded sample of §4: entries, bytes or wall clock.
	CriterionLogSampleBound = "log_sample_bound"
	// CriterionDerivedErrorRate states that the error rate is derived from `request_count`
	// grouped by `response_code_class`, because Cloud Run publishes no dedicated error metric.
	CriterionDerivedErrorRate = "derived_error_rate"
	// CriterionRequestCountExcludesIngress states that `request_count` excludes requests that
	// never reached the container — the 401/403 at the ingress and the 429/503 at
	// max-instances. The digest therefore undercounts exactly the ingress failures an incident
	// is often about, and it says so every time (contract §3.1).
	CriterionRequestCountExcludesIngress = "request_count_excludes_ingress_failures"
	// CriterionErrorRateDenominatorAbsent states that an error-rate answer could not be a
	// RATIO because the selector it was given pins a single `response_code_class`, so the
	// total it would divide by is not in the answer. The figure reported is the failing
	// class's own rate. The selector is executed as minted (FR-111), so the alternative is not
	// to widen it — it is to say what the number is.
	CriterionErrorRateDenominatorAbsent = "error_rate_denominator_absent"
	// CriterionAbsentSource marks a NO_DATA whose emptiness is a missing source rather than an
	// empty window. NoData.AbsentSource carries the sentence; this criterion is what a fixture
	// greps for.
	CriterionAbsentSource = "absent_source"
)

// Options is what a backend is built from. Everything it needs to answer is here; there is no
// package-level state and no ambient credential.
type Options struct {
	// OrgSlug distinguishes one organisation's backend from another's in the registry: the
	// declared name is "gcp:<org-slug>".
	OrgSlug string
	// Project and Region are the GCP scope this backend answers within. A selector naming
	// another project is executed as written — the selector is what GCP receives — but these
	// are what a descriptor read and a console link are built from.
	Project string
	Region  string
	// Transport is the read seam. A nil member is an absent source, answered NO_DATA naming it.
	Transport *Transport
	// Sanitiser applies this feature's sanitisation contract to a live answer, and Redactor is
	// feature 002's. Both are required in live mode: FR-110 says a live investigation must not
	// be able to surface what a recording would not be allowed to keep.
	Sanitiser Sanitiser
	Redactor  *engine.Redactor
	// World, when set, puts this backend in recorded mode: it answers from the recording and
	// makes no network call, never falling through to a live one to satisfy a miss.
	World *sdk.World
	// Now is the clock. A backend with no clock cannot state `executed_at`, and `executed_at`
	// is what makes the ingestion lag it reports mean anything.
	Now func() time.Time
	// MetricRetention and LogRetention are what the vendor states it holds. A window older than
	// one is OUTSIDE_RETENTION carrying the horizon — never NO_DATA.
	MetricRetention time.Duration
	LogRetention    time.Duration
}

// The vendor's stated retention (contract §6, research §3). They are defaults rather than
// constants on the type so that an organisation with a longer log bucket can say so.
const (
	// DefaultMetricRetention is six weeks: `run.googleapis.com/*` metrics.
	DefaultMetricRetention = 42 * 24 * time.Hour
	// DefaultLogRetention is the `_Default` bucket's 30 days. The `_Required` bucket holds 400
	// days, but it holds only the admin-activity stream, which is the feeder's half.
	DefaultLogRetention = 30 * 24 * time.Hour
)

// Backend is the GCP telemetry backend.
type Backend struct {
	name      string
	transport *Transport
	sanitiser Sanitiser
	redactor  *engine.Redactor
	world     *sdk.World
	recorded  *engine.Recorded
	project   string
	region    string
	now       func() time.Time
	retention retention
}

type retention struct {
	metrics time.Duration
	logs    time.Duration
}

// New builds a backend, refusing an Options that could not answer honestly.
//
// The refusals are all of the same kind: each names a thing that would otherwise be discovered
// as a wrong answer rather than as an error. A live backend with no sanitiser would surface what
// a recording could not keep; one with no clock would report an ingestion lag relative to
// nothing.
func New(opts Options) (*Backend, error) {
	if strings.TrimSpace(opts.OrgSlug) == "" {
		return nil, fmt.Errorf("gcp: a backend with no organisation slug; the registry keys on " +
			"\"gcp:<org-slug>\" and two organisations' backends are not interchangeable")
	}
	if opts.Now == nil {
		return nil, fmt.Errorf("gcp: a backend with no clock; coverage.executed_at is what makes " +
			"the ingestion lag a digest reports mean anything")
	}
	live := opts.World == nil
	if live {
		if opts.Transport == nil {
			return nil, fmt.Errorf("gcp: a live backend with no transport; recorded mode is " +
				"Options.World, not a nil transport that would answer nothing and say nothing")
		}
		if opts.Sanitiser == nil || opts.Redactor == nil {
			return nil, fmt.Errorf("gcp: a live backend needs both the sanitiser and the " +
				"redactor; FR-110 says a live investigation must not surface what a recording " +
				"would not be allowed to keep")
		}
	}

	b := &Backend{
		name:      Vendor + ":" + opts.OrgSlug,
		transport: opts.Transport,
		sanitiser: opts.Sanitiser,
		redactor:  opts.Redactor,
		world:     opts.World,
		project:   opts.Project,
		region:    opts.Region,
		now:       opts.Now,
		retention: retention{
			metrics: orDuration(opts.MetricRetention, DefaultMetricRetention),
			logs:    orDuration(opts.LogRetention, DefaultLogRetention),
		},
	}
	if opts.World != nil {
		b.recorded = engine.NewRecordedFromWorld(opts.World)
	}
	return b, nil
}

func orDuration(value, fallback time.Duration) time.Duration {
	if value <= 0 {
		return fallback
	}
	return value
}

// Describe returns the declaration of contract §1: all eight telemetry terms, one published cost
// class each, every capability read-only, and the redaction policy derived from the sanitisation
// table rather than written out beside it.
//
// The cost classes come from `engine.PublishedCostClass` rather than from a table in this file,
// and that is deliberate: a cost class is a property of the **term**, not of the vendor, so a
// budget written against one backend means the same thing against another. Contract §2's table is
// the same assignment, and TestDescribeMatchesTheContractTable holds the two together.
func (b *Backend) Describe() sdk.Description {
	return declarationOf(b.name, b.declaredRedaction())
}

// Declaration is this backend's contract as a pure function of the organisation slug.
//
// It exists because `backend list` must work on a machine with no GCP credential. Instantiating a
// live backend needs a credential that has passed the read-only gate, and a reader asking "what
// would this backend do, and what would it cost" is asking a question about the declaration rather
// than about the connection. So the declaration is stated separately and `Describe` returns it, by
// which the two cannot drift.
//
// The redaction policy is the sanitisation contract's, which is what a live backend would apply:
// the declaration is what would be registered, not a lighter version of it.
func Declaration(orgSlug string) sdk.Description {
	return declarationOf(Vendor+":"+orgSlug, DeclaredRedactionPolicy(sanitise.ContractPolicy()))
}

func declarationOf(name string, redaction *investigationv1.RedactionPolicy) sdk.Description {
	terms := sdk.Terms(sdk.FamilyTelemetry)
	costs := make(map[string]sdk.CostClass, len(terms))
	capabilities := make([]sdk.Capability, 0, len(terms))
	for _, term := range terms {
		class := engine.PublishedCostClass(term)
		costs[term] = class
		capabilities = append(capabilities, sdk.Capability{
			Name:      term,
			ReadOnly:  true,
			CostClass: class,
			MaxWindow: WindowCap(term),
		})
	}
	return sdk.Description{
		Name:           name,
		Vendor:         Vendor,
		Terms:          terms,
		CostClasses:    costs,
		Capabilities:   capabilities,
		Redaction:      redaction,
		Version:        Version,
		AlgebraVersion: engine.AlgebraVersion,
	}
}

// declaredRedaction is the policy this backend publishes. In live mode it is derived from the
// sanitiser actually in force; in recorded mode there is no sanitiser, and the contract policy is
// what the recording was made under.
func (b *Backend) declaredRedaction() *investigationv1.RedactionPolicy {
	if b.sanitiser != nil {
		return DeclaredRedactionPolicy(b.sanitiser.Policy())
	}
	return DeclaredRedactionPolicy(sanitise.ContractPolicy())
}

// WindowCap is the widest window a term answers over (budget.md §5). It is exported because the
// `backend list` rendering prints it and a fixture asserts on it.
func WindowCap(term string) time.Duration {
	switch term {
	case sdk.TermNewLogPatterns, sdk.TermExemplars:
		return WindowCapLogs
	case sdk.TermOnset:
		return WindowCapOnset
	case sdk.TermMonitorState, sdk.TermDrillDown:
		return WindowCapCheap
	default:
		return WindowCapStandard
	}
}

// Execute answers one term (contract §2). It is the only entry point, and every refusal it makes
// is typed: there is no path here that returns an empty digest without saying why it is empty.
func (b *Backend) Execute(ctx context.Context, req *engine.Request) (*engine.Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	name := engine.TermName(req.GetTerm())
	if name == "" || engine.FamilyOf(name) != sdk.FamilyTelemetry {
		return b.refuse(req, investigationv1.FailureReason_OUTSIDE_ALGEBRA, outsideAlgebraDetail(name))
	}
	if err := engine.Validate(req.GetTerm()); err != nil {
		return b.refuse(req, investigationv1.FailureReason_OUTSIDE_ALGEBRA, err.Error())
	}

	// Recorded mode short-circuits everything below: it answers from the recording and makes no
	// network call, never falling through to a live call to satisfy a miss (FR-106, SC-014).
	if b.recorded != nil {
		return b.answerFromWorld(ctx, req)
	}

	// Nobody sees the future (constitution II). The clamp happens before anything is fetched, so
	// no sample past the investigation's observed_at is ever retrieved and discarded — it is
	// never requested.
	clamped, horizon, horizonState := engine.ClampRequest(req)
	if horizonState == engine.PastHorizon {
		return b.pastHorizon(req, horizon)
	}
	req = clamped

	// FR-111: the pointers the feeders emit are accepted without translation, and one this
	// backend cannot execute is named rather than executed as something else.
	if resp, refused, err := b.checkPointer(req, name); refused || err != nil {
		return resp, err
	}
	// A window older than the vendor's retention was never covered. Answering NO_DATA would tell
	// an investigation that nothing happened during a window nobody could look at (contract §6).
	if resp, refused, err := b.checkRetention(req, name); refused || err != nil {
		return resp, err
	}

	started := b.now()
	answer, err := b.dispatch(ctx, req)
	if err != nil {
		return nil, err
	}

	resp, err := engine.NewResponse(engine.ResponseInput{
		Request:        req,
		Outcome:        answer.outcome,
		Mode:           engine.ModeLive,
		CostClass:      engine.PublishedCostClass(name),
		Duration:       b.now().Sub(started),
		BackendVersion: Version,
		Vocabulary:     answer.vocabulary,
		ExecutedQuery:  answer.query,
		DeepLink:       answer.deepLink,
	})
	if err != nil {
		return nil, err
	}
	// The truncation is written into the answer it applies to rather than reported beside it
	// (FR-037), before sanitisation and before the digest is taken over what leaves the process.
	engine.AnnotateHorizon(resp, horizon, horizonState)
	if err := SanitiseThenRedact(b.sanitiser, b.redactor, resp); err != nil {
		return nil, err
	}
	// Sanitisation changes the bytes, so the response digest is recomputed over what actually
	// leaves the process rather than over what was built inside it. ResponseDigest is called
	// rather than reimplemented: the three fields it clears are what make live and recorded hash
	// the same bytes (contract §10).
	digest, err := sdk.ResponseDigest(resp)
	if err != nil {
		return nil, err
	}
	resp.ResponseDigest = digest
	return resp, nil
}

// answer is what one term implementation returns: the outcome, and the evidence about how it was
// obtained that every digest carries.
type answer struct {
	outcome    engine.Outcome
	query      string
	vocabulary string
	deepLink   string
}

// dispatch routes one validated term to its implementation. Nothing outside contract §2's table
// is executed, and caller-supplied query text is never executed: a term's selector reaches GCP
// only through the pointer the feeders minted and the aggregation this backend composes.
func (b *Backend) dispatch(ctx context.Context, req *engine.Request) (answer, error) {
	switch t := req.GetTerm().GetTerm().(type) {
	case *investigationv1.AlgebraTerm_Compare:
		return b.compare(ctx, t.Compare)
	case *investigationv1.AlgebraTerm_ErrorsByVersion:
		return b.errorsByVersion(ctx, t.ErrorsByVersion)
	case *investigationv1.AlgebraTerm_Onset:
		return b.onset(ctx, t.Onset)
	case *investigationv1.AlgebraTerm_NewLogPatterns:
		return b.newLogPatterns(ctx, t.NewLogPatterns)
	case *investigationv1.AlgebraTerm_ErrorSpans:
		return b.errorSpans(t.ErrorSpans)
	case *investigationv1.AlgebraTerm_MonitorState:
		return b.monitorState(ctx, t.MonitorState)
	case *investigationv1.AlgebraTerm_Exemplars:
		return b.exemplars(ctx, t.Exemplars, req.GetWantExemplars())
	case *investigationv1.AlgebraTerm_DrillDown:
		return b.drillDown(ctx, t.DrillDown)
	default:
		// Unreachable: TermName and Validate above have already refused anything else. It is
		// here because "unreachable" is a claim about today's oneof, and a new term added to
		// the algebra without an implementation here should refuse rather than panic.
		return answer{}, fmt.Errorf("gcp: %s is in the telemetry family and this backend has no "+
			"implementation for it", engine.TermName(req.GetTerm()))
	}
}

// answerFromWorld is recorded mode (contract §10). It delegates to the engine's replayer rather
// than reimplementing the lookup, so that a recorded GCP answer and a recorded synthetic answer
// are looked up by the same key computed the same way — and the miss rate is counted in one
// place (FR-108).
func (b *Backend) answerFromWorld(ctx context.Context, req *engine.Request) (*engine.Response, error) {
	resp, err := b.recorded.Execute(ctx, req)
	if err != nil {
		return nil, err
	}
	// The world holds what a live call returned, digest and coverage block and join keys
	// included, so nothing is re-derived here. Only the mode differs, and the engine has
	// already stamped it.
	return resp, nil
}

// Misses is the recorded world's miss report, which is what FR-108's miss-rate metric is computed
// from. It is empty in live mode, where there is no world to miss against.
func (b *Backend) Misses() engine.MissReport {
	if b.recorded == nil {
		return engine.MissReport{}
	}
	return b.recorded.Misses()
}

// refuse is a typed failure with no coverage: a query that never ran covered nothing.
func (b *Backend) refuse(req *engine.Request, reason investigationv1.FailureReason, detail string) (*engine.Response, error) {
	mode := engine.ModeLive
	if b.recorded != nil {
		mode = engine.ModeRecorded
	}
	return engine.NewResponse(engine.ResponseInput{
		Request:        orEmptyRequest(req),
		Outcome:        engine.QueryFailed{Reason: reason, Detail: detail},
		Mode:           mode,
		BackendVersion: Version,
	})
}

func orEmptyRequest(req *engine.Request) *engine.Request {
	if req.GetTerm() != nil {
		return req
	}
	return &engine.Request{Term: &engine.Term{AlgebraVersion: engine.AlgebraVersion}}
}

// outsideAlgebraDetail names what was asked and what is available (FR-087, SC-015). Both halves
// matter: a refusal that names only the first leaves the caller to guess, and guessing at an
// algebra is how a caller ends up sending query text.
func outsideAlgebraDetail(name string) string {
	asked := name
	if asked == "" {
		asked = "an unnamed term"
	}
	return fmt.Sprintf("gcp: %s is not in the telemetry algebra; this backend serves exactly %s, "+
		"and caller-supplied query text is never executed",
		asked, strings.Join(sdk.Terms(sdk.FamilyTelemetry), ", "))
}

// pastHorizon answers a window lying entirely at or after the investigation's observed_at:
// NO_DATA naming the horizon. No GCP call is made — not because the answer is cached, but
// because a measurement after the instant an investigation observes the world does not exist for
// that investigation.
func (b *Backend) pastHorizon(req *engine.Request, horizon time.Time) (*engine.Response, error) {
	window := &engine.Window{
		Start: timestamppb.New(horizon.UTC()),
		End:   timestamppb.New(horizon.UTC()),
	}
	coverage, err := engine.CoverageInput{
		DataSource:               "gcp:none",
		WindowCovered:            window,
		Sampling:                 "none",
		Truncation:               engine.HorizonReason(horizon, engine.PastHorizon),
		IngestionLagUndetermined: true,
		ExecutedAt:               b.now().UTC(),
		QuotaUndetermined:        true,
	}.Coverage()
	if err != nil {
		return nil, err
	}
	engine.AnnotateCoverageHorizon(coverage, horizon, engine.PastHorizon)
	return engine.NewResponse(engine.ResponseInput{
		Request: req,
		Outcome: engine.NoData{
			Coverage: coverage,
			AbsentSource: "telemetry after the investigation's observed_at " +
				horizon.UTC().Format(time.RFC3339) + "; it does not exist at the instant this investigation observes the world",
		},
		Mode:           engine.ModeLive,
		CostClass:      engine.PublishedCostClass(engine.TermName(req.GetTerm())),
		BackendVersion: Version,
		ExecutedQuery:  "horizon:" + horizon.UTC().Format(time.RFC3339),
	})
}

// absent is the answer for a source this organisation does not have or this backend was not
// given: NO_DATA with coverage naming the absent source and stating that nothing was searched.
// It is identical in live and recorded mode and stable for the whole window, so a consumer
// concludes "this cannot be checked here" rather than retrying (FR-091, contract §7).
func (b *Backend) absent(source, absent string, window *engine.Window) (engine.Outcome, error) {
	coverage, err := b.coverage(coverageInput{
		DataSource:    source,
		Window:        window,
		Sampling:      "none",
		LagSource:     LagUndetermined,
		NothingSearch: true,
	})
	if err != nil {
		return nil, err
	}
	return engine.NoData{Coverage: coverage, AbsentSource: absent}, nil
}

// narrow cuts a window back to a term's published cap and reports whether it did. The narrowed
// window is what is queried, and the fact that it was narrowed is written into the digest's
// coverage — a truncated answer that does not say so is worse than a refusal (FR-099).
func narrow(window *engine.Window, cap time.Duration) (*engine.Window, bool) {
	if window.GetStart() == nil || window.GetEnd() == nil || cap <= 0 {
		return window, false
	}
	start := window.GetStart().AsTime().UTC()
	end := window.GetEnd().AsTime().UTC()
	if end.Sub(start) <= cap {
		return window, false
	}
	// The cap is applied from the END of the window: the most recent data is what an
	// investigation is about, and dropping the newest hours to keep the oldest would answer a
	// different question than the one asked.
	return &engine.Window{Start: timestamppb.New(end.Add(-cap)), End: timestamppb.New(end)}, true
}

// windowsOf returns the windows a term carries. The engine has the same switch for the horizon
// clamp and keeps it unexported; this one exists because the retention check needs the EARLIEST
// instant a term reaches back to, which is a different question from where a window sits relative
// to a horizon, and importing a clamp to ask it would be the wrong shape.
func windowsOf(term *engine.Term) []*engine.Window {
	switch t := term.GetTerm().(type) {
	case *investigationv1.AlgebraTerm_Compare:
		return []*engine.Window{t.Compare.GetWindows().GetBaseline(), t.Compare.GetWindows().GetSymptom()}
	case *investigationv1.AlgebraTerm_Onset:
		return []*engine.Window{t.Onset.GetSearchWindow()}
	case *investigationv1.AlgebraTerm_NewLogPatterns:
		return []*engine.Window{t.NewLogPatterns.GetWindow(), t.NewLogPatterns.GetBaselineWindow()}
	case *investigationv1.AlgebraTerm_ErrorSpans:
		return []*engine.Window{t.ErrorSpans.GetWindow()}
	case *investigationv1.AlgebraTerm_ErrorsByVersion:
		return []*engine.Window{t.ErrorsByVersion.GetWindow()}
	case *investigationv1.AlgebraTerm_MonitorState:
		return []*engine.Window{t.MonitorState.GetWindow()}
	default:
		// Handle-bearing terms carry no window of their own: the handle was minted by an
		// earlier answer that was itself checked.
		return nil
	}
}
