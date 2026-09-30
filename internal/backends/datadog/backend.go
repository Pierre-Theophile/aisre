// SPDX-License-Identifier: Apache-2.0

package datadog

import (
	"context"
	"fmt"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/datadogx"
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	sdk "github.com/Pierre-Theophile/aisre/pkg/backend"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// Vendor is the system this backend speaks for.
const Vendor = "datadog"

// Version is the backend's own version, recorded in every digest it returns.
const Version = "0.1.0"

// The window caps (contract §2). A request wider than its cap is narrowed to the cap and says so,
// never issued as asked (FR-082a). new_log_patterns has the tightest because it pages raw events, and
// the log-search quota is the one the humans working an incident are spending too (FR-081b).
const (
	// WindowCapCheap bounds monitor_state and drill_down: an indexed read.
	WindowCapCheap = 90 * 24 * time.Hour
	// WindowCapStandard bounds compare and errors_by_version: aggregations.
	WindowCapStandard = 24 * time.Hour
	// WindowCapOnset bounds the onset search window: the series comes back to this process.
	WindowCapOnset = 6 * time.Hour
	// WindowCapLogs bounds new_log_patterns and exemplars, which page raw events.
	WindowCapLogs = 2 * time.Hour
)

// DefaultLogRetention is Datadog's standard index retention. It is configuration, because an
// organisation's indexes may hold longer; a window older than it is OUTSIDE_RETENTION, never NO_DATA.
const DefaultLogRetention = 15 * 24 * time.Hour

// SpanSourceAbsent is the absent-source statement for error_spans while apm_topology is off
// (FR-049b). It is a noun phrase: the outcome renders "this organisation has no <it>".
const SpanSourceAbsent = "span data source configured for this organisation: the apm_topology " +
	"capability is off, so no Datadog span or APM metric is read"

// Options is what a backend is built from. There is no package-level state and no ambient credential.
type Options struct {
	// OrgSlug names the organisation; the declared name is "datadog:<org-slug>" (FR-006).
	OrgSlug string
	// Client is the live read seam. Required in live mode.
	Client *datadogx.Client
	// Sanitiser and Redactor are both required in live mode: a live investigation must not surface
	// what a recording would not be allowed to keep (FR-052).
	Sanitiser Sanitiser
	Redactor  *engine.Redactor
	// World, when set, puts the backend in recorded mode: it answers from the recording and never
	// makes a network call to satisfy a miss (FR-049a, FR-050).
	World *sdk.World
	// Now is the clock; executed_at and the indexing lag mean nothing without it.
	Now func() time.Time
	// LogRetention is what the organisation's indexes hold. Zero uses DefaultLogRetention.
	LogRetention time.Duration
	// Indexes are the log indexes searched; empty searches the default.
	Indexes []string
	// Site is the Datadog site, used only to build a human deep link to the app (FR-048c). It never
	// enters a pointer or a digest field other than the link.
	Site string
	// APMTopology turns on the span and APM metric terms (the `apm_topology` capability). Off, which is
	// the default, error_spans answers the typed NO_DATA naming the absent span source and compare
	// refuses what a log count cannot state, exactly as before (FR-049b, SC-023, SC-024). The client
	// must declare the capability's operations; one that does not fails the first APM term rather than
	// answering without them.
	APMTopology bool
	// APMEnv is the environment an error_spans entity id that names none is read in.
	APMEnv string
	// EntityService, when set, reads a graph entity id as a Datadog service; without it an id is read as
	// `<env>/<service>` (the way the feeder spells a service node) or a bare service in APMEnv.
	EntityService func(entityID string) (ServiceRef, bool)
}

// Backend is the Datadog telemetry backend.
type Backend struct {
	name      string
	client    *datadogx.Client
	sanitiser Sanitiser
	redactor  *engine.Redactor
	recorded  *engine.Recorded
	now       func() time.Time
	retention time.Duration
	indexes   []string
	site      string

	apm           bool
	apmEnv        string
	entityService func(string) (ServiceRef, bool)
}

// New builds a backend, refusing Options that could not answer honestly.
func New(opts Options) (*Backend, error) {
	if strings.TrimSpace(opts.OrgSlug) == "" {
		return nil, fmt.Errorf("datadog: a backend with no organisation slug; the registry keys on " +
			"\"datadog:<org-slug>\" and two organisations' backends are not interchangeable")
	}
	if opts.Now == nil {
		return nil, fmt.Errorf("datadog: a backend with no clock; executed_at and the indexing lag " +
			"mean nothing without one")
	}
	if opts.World == nil {
		if opts.Client == nil {
			return nil, fmt.Errorf("datadog: a live backend with no client; recorded mode is " +
				"Options.World, not a nil client that would answer nothing and say nothing")
		}
		if opts.Sanitiser == nil || opts.Redactor == nil {
			return nil, fmt.Errorf("datadog: a live backend needs both the sanitiser and the " +
				"redactor; FR-052 says a live investigation must not surface what a recording " +
				"would not be allowed to keep")
		}
	}
	b := &Backend{
		name:      Vendor + ":" + opts.OrgSlug,
		client:    opts.Client,
		sanitiser: opts.Sanitiser,
		redactor:  opts.Redactor,
		now:       opts.Now,
		retention: opts.LogRetention,
		indexes:   append([]string(nil), opts.Indexes...),
		site:      opts.Site,

		apm: opts.APMTopology, apmEnv: opts.APMEnv, entityService: opts.EntityService,
	}
	if b.retention <= 0 {
		b.retention = DefaultLogRetention
	}
	if opts.World != nil {
		b.recorded = engine.NewRecordedFromWorld(opts.World)
	}
	return b, nil
}

// Describe returns the declaration: all eight telemetry terms, one published cost class each, every
// capability read-only (contract §1).
func (b *Backend) Describe() sdk.Description {
	return Declaration(strings.TrimPrefix(b.name, Vendor+":"))
}

// Declaration is the contract as a pure function of the organisation slug, so `backend list` works on
// a machine with no Datadog credential.
func Declaration(orgSlug string) sdk.Description {
	terms := sdk.Terms(sdk.FamilyTelemetry)
	costs := make(map[string]sdk.CostClass, len(terms))
	capabilities := make([]sdk.Capability, 0, len(terms))
	for _, term := range terms {
		class := engine.PublishedCostClass(term)
		costs[term] = class
		capabilities = append(capabilities, sdk.Capability{
			Name: term, ReadOnly: true, CostClass: class, MaxWindow: WindowCap(term),
		})
	}
	return sdk.Description{
		Name:           Vendor + ":" + orgSlug,
		Vendor:         Vendor,
		Terms:          terms,
		CostClasses:    costs,
		Capabilities:   capabilities,
		Redaction:      DeclaredRedactionPolicy(),
		Version:        Version,
		AlgebraVersion: engine.AlgebraVersion,
	}
}

// WindowCap is the widest window a term answers over (contract §2).
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

// Execute answers one term. Every refusal is typed: no path returns an empty digest without saying
// why it is empty.
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
	// An unstamped errors_by_version is the engine's to answer, identically for every backend
	// (ADR-0010 item 2). Asked of this backend directly, it gives the engine's answer rather than a
	// second one of its own.
	if resp, ok, err := engine.AnswerUnstamped(req, b.mode()); err != nil || ok {
		return resp, err
	}
	if b.recorded != nil {
		return b.recorded.Execute(ctx, req)
	}

	clamped, horizon, horizonState := engine.ClampRequest(req)
	if horizonState == engine.PastHorizon {
		return b.pastHorizon(req, horizon)
	}
	req = clamped
	if resp, refused, err := b.checkPointer(req, name); refused || err != nil {
		return resp, err
	}
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
	engine.AnnotateHorizon(resp, horizon, horizonState)
	if err := SanitiseThenRedact(b.sanitiser, b.redactor, resp); err != nil {
		return nil, err
	}
	// Sanitisation changes the bytes, so the digest is recomputed over what leaves the process.
	digest, err := sdk.ResponseDigest(resp)
	if err != nil {
		return nil, err
	}
	resp.ResponseDigest = digest
	return resp, nil
}

// Misses is the recorded world's miss report; empty in live mode.
func (b *Backend) Misses() engine.MissReport {
	if b.recorded == nil {
		return engine.MissReport{}
	}
	return b.recorded.Misses()
}

// answer is what one term implementation returns: the outcome, and the evidence of how it was
// obtained that every digest carries.
type answer struct {
	outcome    engine.Outcome
	query      string
	vocabulary string
	deepLink   string
}

// dispatch routes one validated term to its implementation.
func (b *Backend) dispatch(ctx context.Context, req *engine.Request) (answer, error) {
	switch t := req.GetTerm().GetTerm().(type) {
	case *investigationv1.AlgebraTerm_ErrorSpans:
		if b.apm {
			return b.errorSpansAPM(ctx, t.ErrorSpans)
		}
		return b.errorSpans(t.ErrorSpans)
	case *investigationv1.AlgebraTerm_ErrorsByVersion:
		return b.errorsByVersion(ctx, t.ErrorsByVersion)
	case *investigationv1.AlgebraTerm_Compare:
		return b.compare(ctx, t.Compare)
	case *investigationv1.AlgebraTerm_Onset:
		return b.onset(ctx, t.Onset)
	case *investigationv1.AlgebraTerm_MonitorState:
		return b.monitorState(ctx, t.MonitorState)
	case *investigationv1.AlgebraTerm_NewLogPatterns:
		return b.newLogPatterns(ctx, t.NewLogPatterns)
	case *investigationv1.AlgebraTerm_DrillDown:
		return b.drillDown(ctx, t.DrillDown)
	case *investigationv1.AlgebraTerm_Exemplars:
		return b.exemplars(ctx, t.Exemplars, req.GetWantExemplars())
	default:
		// Every telemetry term is handled above and refuse() turned away the other families, so this
		// is unreachable; it stays a typed failure rather than an empty digest all the same.
		return answer{outcome: engine.QueryFailed{
			Reason: investigationv1.FailureReason_REJECTED_BY_BACKEND,
			Detail: fmt.Sprintf("datadog: %s is not served by this backend", engine.TermName(req.GetTerm())),
		}}, nil
	}
}

// errorSpans answers NO_DATA naming the absent span source while apm_topology is off (FR-049b). It is
// identical live and recorded and stable for the whole window, so an investigation concludes "this
// cannot be checked here" once rather than retrying.
func (b *Backend) errorSpans(term *investigationv1.ErrorSpansTerm) (answer, error) {
	coverage, err := engine.CoverageInput{
		SearchedEntities:         []string{term.GetSrcEntityId(), term.GetDstEntityId()},
		DataSource:               "datadog_apm:absent",
		WindowCovered:            term.GetWindow(),
		Sampling:                 "none",
		IngestionLagUndetermined: true,
		QuotaUndetermined:        true,
		ExecutedAt:               b.now().UTC(),
	}.Coverage()
	if err != nil {
		return answer{}, err
	}
	return answer{
		outcome: engine.NoData{Coverage: coverage, AbsentSource: SpanSourceAbsent},
		query: fmt.Sprintf("error_spans(%s -> %s): not executed; no span data source",
			term.GetSrcEntityId(), term.GetDstEntityId()),
		vocabulary: "datadog-spans (not registered: apm_topology is off)",
	}, nil
}

func (b *Backend) mode() string {
	if b.recorded != nil {
		return engine.ModeRecorded
	}
	return engine.ModeLive
}

// refuse is a typed failure with no coverage: a query that never ran covered nothing.
func (b *Backend) refuse(req *engine.Request, reason investigationv1.FailureReason, detail string) (*engine.Response, error) {
	return engine.NewResponse(engine.ResponseInput{
		Request:        orEmptyRequest(req),
		Outcome:        engine.QueryFailed{Reason: reason, Detail: detail},
		Mode:           b.mode(),
		BackendVersion: Version,
	})
}

func orEmptyRequest(req *engine.Request) *engine.Request {
	if req.GetTerm() != nil {
		return req
	}
	return &engine.Request{Term: &engine.Term{AlgebraVersion: engine.AlgebraVersion}}
}

// outsideAlgebraDetail names what was asked and what is available (FR-040a, SC-021).
func outsideAlgebraDetail(name string) string {
	asked := name
	if asked == "" {
		asked = "an unnamed term"
	}
	return fmt.Sprintf("datadog: %s is not in the telemetry algebra; this backend serves exactly %s, "+
		"and caller-supplied query text is never executed",
		asked, strings.Join(sdk.Terms(sdk.FamilyTelemetry), ", "))
}

// pastHorizon answers a window lying entirely after the investigation's observed_at: NO_DATA naming
// the horizon, with no Datadog call — the measurement does not exist for that investigation.
func (b *Backend) pastHorizon(req *engine.Request, horizon time.Time) (*engine.Response, error) {
	window := &engine.Window{Start: timestamppb.New(horizon.UTC()), End: timestamppb.New(horizon.UTC())}
	coverage, err := engine.CoverageInput{
		DataSource:               "datadog:none",
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

// termPointer is the pointer a term carries and the kind it must be.
func termPointer(term *engine.Term) (*graphv1.Pointer, bool) {
	switch t := term.GetTerm().(type) {
	case *investigationv1.AlgebraTerm_Compare:
		return t.Compare.GetPointer(), true
	case *investigationv1.AlgebraTerm_Onset:
		return t.Onset.GetPointer(), true
	case *investigationv1.AlgebraTerm_ErrorsByVersion:
		return t.ErrorsByVersion.GetPointer(), true
	case *investigationv1.AlgebraTerm_NewLogPatterns:
		return t.NewLogPatterns.GetPointer(), true
	case *investigationv1.AlgebraTerm_MonitorState:
		return t.MonitorState.GetPointer(), true
	default:
		return nil, false
	}
}

// checkPointer refuses a pointer this backend cannot execute, naming its kind and vocabulary
// (FR-053). A log term needs a `datadog-logs/v1` LOG pointer; monitor_state a `datadog-monitor/v1`
// pointer; nothing else is executed as something it is not.
func (b *Backend) checkPointer(req *engine.Request, name string) (*engine.Response, bool, error) {
	pointer, ok := termPointer(req.GetTerm())
	if !ok {
		return nil, false, nil
	}
	want := feeder.VocabDatadogLogs
	if name == sdk.TermMonitorState {
		want = feeder.VocabDatadogMonitor
	}
	// With apm_topology on, compare also executes a METRIC pointer in the APM metric vocabulary; with it
	// off the pointer is refused as any pointer minted for another surface is.
	if name == sdk.TermCompare && b.apm && pointer.GetVocabulary() == feeder.VocabDatadogAPMMetric {
		if pointer.GetKind() != graphv1.PointerKind_METRIC {
			resp, err := b.refuse(req, investigationv1.FailureReason_UNSUPPORTED_POINTER, fmt.Sprintf(
				"datadog: %s over %s needs a METRIC pointer and was given a %s pointer", name,
				feeder.VocabDatadogAPMMetric, pointer.GetKind()))
			return resp, true, err
		}
		return nil, false, nil
	}
	switch {
	case pointer == nil:
		resp, err := b.refuse(req, investigationv1.FailureReason_UNSUPPORTED_POINTER, fmt.Sprintf(
			"datadog: %s carries no pointer, so there is nothing to execute", name))
		return resp, true, err
	case pointer.GetVocabulary() != want:
		resp, err := b.refuse(req, investigationv1.FailureReason_UNSUPPORTED_POINTER, fmt.Sprintf(
			"datadog: %s was given a %s pointer in vocabulary %q; this backend executes %s for it, and a "+
				"pointer minted for another backend is refused rather than executed as something else",
			name, pointer.GetKind(), pointer.GetVocabulary(), want))
		return resp, true, err
	case want == feeder.VocabDatadogLogs && pointer.GetKind() != graphv1.PointerKind_LOG:
		resp, err := b.refuse(req, investigationv1.FailureReason_UNSUPPORTED_POINTER, fmt.Sprintf(
			"datadog: %s needs a LOG pointer and was given a %s pointer", name, pointer.GetKind()))
		return resp, true, err
	}
	return nil, false, nil
}

// checkRetention refuses a window older than the indexes hold: OUTSIDE_RETENTION with the horizon,
// never NO_DATA, which would say nothing happened during a window nobody could look at.
func (b *Backend) checkRetention(req *engine.Request, name string) (*engine.Response, bool, error) {
	if name == sdk.TermMonitorState || (name == sdk.TermErrorSpans && !b.apm) {
		return nil, false, nil
	}
	// A trace metric is kept far longer than a log index; its retention is Datadog's to state, so the
	// window is not judged against the log indexes'.
	if pointer, ok := termPointer(req.GetTerm()); ok && pointer.GetVocabulary() == feeder.VocabDatadogAPMMetric {
		return nil, false, nil
	}
	horizon := b.now().UTC().Add(-b.retention)
	var earliest time.Time
	for _, w := range windowsOf(req.GetTerm()) {
		if start := w.GetStart(); start != nil && (earliest.IsZero() || start.AsTime().Before(earliest)) {
			earliest = start.AsTime().UTC()
		}
	}
	if earliest.IsZero() || !earliest.Before(horizon) {
		return nil, false, nil
	}
	resp, err := b.refuse(req, investigationv1.FailureReason_OUTSIDE_RETENTION, fmt.Sprintf(
		"datadog: %s asks about %s, and the log indexes hold %s — back to %s. The window was never "+
			"covered, so this is not NO_DATA", name, earliest.Format(time.RFC3339), b.retention,
		horizon.Format(time.RFC3339)))
	if err != nil {
		return nil, true, err
	}
	resp.RetentionHorizon = timestamppb.New(horizon)
	digest, derr := sdk.ResponseDigest(resp)
	if derr != nil {
		return nil, true, derr
	}
	resp.ResponseDigest = digest
	return resp, true, nil
}

// windowsOf returns the windows a term carries.
func windowsOf(term *engine.Term) []*engine.Window {
	switch t := term.GetTerm().(type) {
	case *investigationv1.AlgebraTerm_Compare:
		return []*engine.Window{t.Compare.GetWindows().GetBaseline(), t.Compare.GetWindows().GetSymptom()}
	case *investigationv1.AlgebraTerm_Onset:
		return []*engine.Window{t.Onset.GetSearchWindow()}
	case *investigationv1.AlgebraTerm_NewLogPatterns:
		return []*engine.Window{t.NewLogPatterns.GetWindow(), t.NewLogPatterns.GetBaselineWindow()}
	case *investigationv1.AlgebraTerm_ErrorsByVersion:
		return []*engine.Window{t.ErrorsByVersion.GetWindow()}
	case *investigationv1.AlgebraTerm_ErrorSpans:
		return []*engine.Window{t.ErrorSpans.GetWindow()}
	default:
		return nil
	}
}
