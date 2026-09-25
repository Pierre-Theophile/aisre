// SPDX-License-Identifier: Apache-2.0

package vercel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The run loop: one cycle of payloads becomes events (004 T090's prerequisite; FR-031, FR-032, FR-057).
//
// ---------------------------------------------------------------------------------------------
// What a cycle is
//
// A poll lists the production deployments in a time window, and each promoted one becomes a ROLLOUT
// change followed by the correlation keys C8 joins on. The change is emitted FIRST and its keys after:
// the projector tolerates either order now (T139 made the queued re-evaluation order-independent), but a
// recording that states the change before what describes it is the one a reader can follow.
//
// # The poll marker ends the cycle, and that is not a formality
//
// Nothing is emitted for a deployment the platform did not send, so a cycle's meaning depends on knowing
// it COMPLETED. The marker carries that, and with it the exclusion counts: FR-032 and SC-002 ask for
// preview deployments to be excluded and COUNTED, and a count that is never published is not a
// measurement. A partial poll declares a gap rather than retracting what it did not see (FR-056), which
// is why the marker's outcome is read rather than assumed.
//
// # Why an unknown payload kind is an error
//
// A fixture directory naming a payload this feeder does not read is a fixture that silently verifies
// nothing — it would replay, report a pass, and assert less than its author believed. The same reasoning
// as internal/feeders/github/feeder.go's `apply`.

// Kind is this connector's kind.
const Kind = "vercel"

// SourceIDPrefix makes the source id `vercel:<org>`.
const SourceIDPrefix = "vercel:"

// SchemaVersion is the event schema this feeder emits against.
const SchemaVersion = "1.0.0"

// DefaultReorderingWindow is how far out of order this feeder may deliver its own events.
//
// A listing is paginated by timestamp and the projects read is independent of it, so two payloads that
// describe the same instant can arrive either way round. The window is what tells the fixture shuffle
// how far to permute — a window of zero would permute nothing and the order-independence it is meant to
// check would go untested.
const DefaultReorderingWindow = 5 * time.Minute

// Payload kinds this feeder reads.
const (
	PayloadDeployments = "deployments"
	PayloadProjects    = "projects"
	// PayloadProject is one project's full read (`GET /v9/projects/{idOrName}`). It describes the project
	// as the listing does, and it is the only read that states the last alias request, which is where
	// Vercel says a rollback happened (004 T117).
	PayloadProject    = "project"
	PayloadProjectEnv = "project-env"
	// PayloadPollMarker ends a cycle: it is what publishes the exclusion counts and the outcome.
	PayloadPollMarker = "poll-marker"
)

// Gater proves the credential read-only before anything is emitted.
//
// A narrow local interface rather than a shared one, for the reason the GitHub feeder gives: what
// counts as proof differs per platform — GitHub reads the installation token's own permissions, and
// Vercel's answer depends on what a project-scoped token reports, which T034 establishes rather than
// assumes. The implementation arrives with that task; this is the seam it plugs into.
type Gater interface {
	Prove(ctx context.Context) (GateResult, error)
}

// GateResult is what a proof established.
type GateResult struct {
	// Evidence is the platform's own words, recorded so the proof is checkable rather than asserted.
	Evidence []byte
	// Scope is what the credential can reach, as the platform reports it.
	Scope Scope
}

// Scope is the reach a Vercel credential has, as the PLATFORM reports it.
//
// Deliberately thin, and it stays thin until T034. Research §5.3 established that
// `POST /v3/user/tokens` takes a `projectId`, so the platform CAN pin a token to exactly one project —
// which makes a project-scoped token a platform boundary rather than configuration. What is NOT
// established is the spelling that scope takes in `token.scopes[].type` when the token is read back,
// and guessing it would mean this connector reporting a regime it had invented.
//
// So `Selection` is a string carrying whatever the platform said, and there is no enum. A connector
// that REPORTS which regime its token puts it in is honest; one that DECLARES a regime from a value it
// guessed is the kind of confident wrong answer this project keeps finding.
type Scope struct {
	// Selection is the platform's own description of the token's reach, recorded verbatim.
	Selection string
	// Projects are the project ids the credential can reach, where the platform enumerates them.
	Projects []string
}

// Options configures the feeder.
type Options struct {
	// OrgSlug makes the source id. A token is scoped to one source, so this is not optional in
	// practice even though a replay does not check it.
	OrgSlug string
	// Targets is the operator's project→entity mapping, in the graph's own vocabulary. See Mapper.
	Targets map[string]*graphv1.Ref
	// ReorderingWindow overrides DefaultReorderingWindow.
	ReorderingWindow time.Duration
	// Gate proves the credential is read-only before anything is emitted (FR-003). Nil on a replay,
	// where there is no credential to prove.
	Gate Gater
	// Log is where the cycle reports. Nil means discard.
	Log *slog.Logger
}

// Feeder turns Vercel payloads into graph events.
type Feeder struct {
	opts   Options
	mapper *Mapper
	log    *slog.Logger

	// skew observes the gap between Vercel's own timestamps and this process's clock (FR-058).
	//
	// Reported, NEVER used to correct either clock. This graph is bitemporal and its two dimensions
	// mean different things: a vendor timestamp is evidence about when a fact was true, and the
	// arrival instant is when this system learned it. Shifting either to make them agree would
	// destroy the one property that makes the pair worth having, and would do it invisibly.
	skew *feeder.Skew

	mu sync.Mutex
	// excluded accumulates this cycle's refusals, published by the poll marker.
	excluded []Rollout
	// extentFrom is the arrival instant of the cycle's first payload, where the next checkpoint's
	// extent starts; zero between cycles.
	extentFrom time.Time
	// gapBefore is set by a partial poll, so the NEXT checkpoint says the silence before it was
	// ignorance rather than absence (FR-056).
	gapBefore bool
	// configExcluded accumulates the cycle's CONFIGURATION refusals, counted separately. Separately
	// because the two are not the same measurement: "eleven previews were skipped" and "eleven
	// preview-only variables were skipped" answer different questions, and summing them would report a
	// number that means nothing (FR-032, SC-002).
	configExcluded []ConfigChange
}

// New builds the feeder.
func New(opts Options) (*Feeder, error) {
	if opts.OrgSlug == "" {
		return nil, fmt.Errorf("vercel: a feeder needs an org slug; the source id is vercel:<org>")
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	f := &Feeder{opts: opts, log: log, skew: &feeder.Skew{}}
	f.mapper = NewMapper(f.Describe())
	for id, ref := range opts.Targets {
		f.mapper.ProjectTargets[id] = ref
	}
	return f, nil
}

// Describe publishes what this source is. Pure: it reads configuration and never cycle state, so the
// recorder and the replayer see the same description.
func (f *Feeder) Describe() feeder.Description {
	window := f.opts.ReorderingWindow
	if window == 0 {
		window = DefaultReorderingWindow
	}
	return feeder.Description{
		SourceID:      SourceIDPrefix + f.opts.OrgSlug,
		Kind:          Kind,
		SchemaVersion: SchemaVersion,
		// Two independent paginated reads, neither ordered against the other.
		Ordering:         feeder.OrderingNone,
		ReorderingWindow: window,
		Namespaces:       f.namespaces(),
	}
}

// namespaces is what this feeder mints plus what the operator's mapping targets, deduplicated.
//
// The operator's targets are included because a feeder that attaches changes to `k8s.deployment` without
// declaring it would be writing into a namespace its own description says it does not touch.
func (f *Feeder) namespaces() []string {
	out := []string{feeder.NSVercelChange, feeder.NSVercelProject, feeder.NSDeployCommitSHA}
	for _, ref := range f.opts.Targets {
		if ns := ref.GetNamespace(); ns != "" {
			out = append(out, ns)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Skew is the clock-skew observation this cycle accumulated.
//
// Exported so a caller can read the measurement rather than parse a log line — which is what makes it
// assertable, and what T142 found missing: the machinery existed with no way to observe that it had run.
func (f *Feeder) Skew() feeder.SkewReport { return f.skew.Report() }

// Run reads payloads until the source is exhausted.
func (f *Feeder) Run(ctx context.Context, src feeder.Source, em feeder.Emitter) error {
	desc := f.Describe()
	if err := desc.Validate(); err != nil {
		return err
	}

	// FR-003: proved before anything is emitted, and there is no path in which a read happens first.
	if f.opts.Gate != nil {
		result, err := f.opts.Gate.Prove(ctx)
		if err != nil {
			return fmt.Errorf("vercel: the read-only gate refused, so nothing was emitted: %w", err)
		}
		f.log.InfoContext(ctx, "read-only gate passed",
			"source", desc.SourceID, "evidence", string(result.Evidence),
			"selection", result.Scope.Selection)
	} else {
		f.log.InfoContext(ctx, "no credential to prove: replaying a recording", "source", desc.SourceID)
	}

	defer func() {
		if err := em.Flush(ctx); err != nil {
			f.log.ErrorContext(ctx, "flush failed", "error", err)
		}
	}()

	for {
		payload, err := src.Next(ctx)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := f.apply(ctx, desc, em, payload); err != nil {
			return err
		}
	}
}

var _ feeder.Feeder = (*Feeder)(nil)

// apply routes one payload to the reader for its kind.
func (f *Feeder) apply(ctx context.Context, desc feeder.Description, em feeder.Emitter, payload feeder.Payload) error {
	f.mu.Lock()
	if f.extentFrom.IsZero() && !payload.At.IsZero() {
		f.extentFrom = payload.At
	}
	f.mu.Unlock()
	switch payload.Kind {
	case PayloadDeployments:
		return f.applyDeployments(ctx, desc, em, payload.Bytes, payload.At)
	case PayloadProjects:
		return f.applyProjects(ctx, em, payload.Bytes, payload.At)
	case PayloadProject:
		return f.applyProject(ctx, em, payload.Bytes, payload.At)
	case PayloadProjectEnv:
		return f.applyProjectEnv(ctx, desc, em, payload.Bytes, payload.At)
	case PayloadPollMarker:
		return f.applyPollMarker(ctx, desc, em, payload.Bytes, payload.At)
	default:
		return fmt.Errorf("vercel: payload kind %q is not one this feeder reads; a fixture naming it "+
			"would verify nothing", payload.Kind)
	}
}

// applyProjects describes each project as a node (004 T093) and records the repository it is connected
// to, for the change's property.
//
// It is the PROJECTS read rather than the deployment that states this, which is why the two payloads are
// separate: a deployment names its project by id and says nothing about the repository behind it.
func (f *Feeder) applyProjects(ctx context.Context, em feeder.Emitter, raw []byte, at time.Time) error {
	var body struct {
		Projects []Project `json:"projects"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return fmt.Errorf("vercel: decoding projects: %w", err)
	}
	return f.describeProjects(ctx, em, body.Projects, at)
}

// applyProject reads one project's full record: it describes the project exactly as the listing does, and
// then maps its last alias request, which only this read carries, into a rollback when the platform states
// one (004 T117).
func (f *Feeder) applyProject(ctx context.Context, em feeder.Emitter, raw []byte, at time.Time) error {
	var p Project
	if err := json.Unmarshal(raw, &p); err != nil {
		return fmt.Errorf("vercel: decoding project: %w", err)
	}
	if err := f.describeProjects(ctx, em, []Project{p}, at); err != nil {
		return err
	}
	f.mu.Lock()
	rollback, err := f.mapper.MapAliasRequest(p, at)
	if err == nil && rollback.Excluded != "" {
		f.excluded = append(f.excluded, rollback)
	}
	f.mu.Unlock()
	if err != nil {
		return err
	}
	return emit(ctx, em, rollback.Change)
}

// describeProjects emits each project as a node and records its repository for the change's property.
func (f *Feeder) describeProjects(ctx context.Context, em feeder.Emitter, projects []Project, at time.Time) error {
	var nodes []*graphv1.EventEnvelope
	f.mu.Lock()
	for _, p := range projects {
		if p.ID == "" {
			continue
		}
		if p.Link.Org != "" && p.Link.Repo != "" {
			f.mapper.Repositories[p.ID] = p.Link.Org + "/" + p.Link.Repo
		}
		node, ok, err := f.mapper.MapProject(p, at)
		if err != nil {
			f.mu.Unlock()
			return err
		}
		if ok {
			nodes = append(nodes, node)
		}
	}
	f.mu.Unlock()
	for _, node := range nodes {
		if err := emit(ctx, em, node); err != nil {
			return err
		}
	}
	return nil
}

// applyDeployments maps each deployment, emitting the change and then its correlation keys.
func (f *Feeder) applyDeployments(ctx context.Context, desc feeder.Description, em feeder.Emitter, raw []byte, at time.Time) error {
	var body struct {
		Deployments []Deployment `json:"deployments"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return fmt.Errorf("vercel: decoding deployments: %w", err)
	}
	for _, d := range body.Deployments {
		// The skew observation, taken on EVERY deployment rather than only the promoted ones. A
		// preview's timestamps come off the same platform clock, so excluding them would measure the
		// skew of a subset chosen by a rule that has nothing to do with clocks — and on an estate that
		// deploys mostly previews, that subset is small enough for the measurement to say little.
		//
		// `ready` is the platform instant used because it is the one Vercel actually states for the
		// event this cycle is reading. `createdAt` would measure the age of the build rather than the
		// gap between the clocks.
		if vendorAt, ok := millis(d.ReadyMillis); ok {
			f.skew.Observe(vendorAt, at)
		}
		f.mu.Lock()
		rollout, err := f.mapper.MapDeployment(d, at)
		f.mu.Unlock()
		if err != nil {
			return err
		}
		if rollout.Excluded != "" {
			f.mu.Lock()
			f.excluded = append(f.excluded, rollout)
			f.mu.Unlock()
			continue
		}
		if err := emit(ctx, em, rollout.Change); err != nil {
			return err
		}
		for _, c := range rollout.Correlations {
			if err := emit(ctx, em, c); err != nil {
				return err
			}
		}
	}
	_ = desc
	return nil
}

// applyProjectEnv maps each environment variable into a configuration change.
//
// The project id travels IN the payload rather than beside it, the same way the GitHub feeder's statuses
// payload names its repository and deployment. The reason is the endpoint: `GET /projects/{id}/env`
// returns `{envs: [...]}` and nothing identifying the project, so a recording of the response alone could
// not say whose configuration it was. A recorded payload has to be self-describing or a replay is
// guessing.
//
// Nothing here looks at a deployment, and that is FR-039 rather than an omission: a configuration change
// is emitted at the instant the platform states the variable changed, whether or not a rollout ever
// applies it. There is no queue holding one until a deployment arrives and no path that folds one into a
// rollout.
func (f *Feeder) applyProjectEnv(ctx context.Context, desc feeder.Description, em feeder.Emitter, raw []byte, at time.Time) error {
	var body struct {
		ProjectID string       `json:"projectId"`
		Envs      []ProjectEnv `json:"envs"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return fmt.Errorf("vercel: decoding environment metadata: %w", err)
	}
	for _, env := range body.Envs {
		// No skew observation here. `createdAt` and `updatedAt` are when a person edited a variable,
		// which can be months ago — measuring this process's clock against them would report a skew of
		// weeks and say nothing about either clock. The deployment cycle's `ready` is the platform
		// instant that measures delivery, and it is where the observation belongs (FR-058).
		f.mu.Lock()
		change, err := f.mapper.MapProjectEnv(body.ProjectID, env, at)
		f.mu.Unlock()
		if err != nil {
			return err
		}
		if change.Excluded != "" {
			f.mu.Lock()
			f.configExcluded = append(f.configExcluded, change)
			f.mu.Unlock()
			continue
		}
		if err := emit(ctx, em, change.Change); err != nil {
			return err
		}
	}
	_ = desc
	return nil
}

// applyPollMarker ends the cycle and publishes what it refused.
//
// The counts are logged rather than emitted as graph events, and that is deliberate: an exclusion is a
// fact about this connector's cycle, not about the estate. A preview deployment that was never a
// production change has no place on the graph, and putting one there would make the graph's own content
// depend on how the connector was configured.
func (f *Feeder) applyPollMarker(ctx context.Context, desc feeder.Description, em feeder.Emitter, raw []byte, at time.Time) error {
	var marker struct {
		Outcome string `json:"outcome"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal(raw, &marker); err != nil {
		return fmt.Errorf("vercel: decoding the poll marker: %w", err)
	}

	f.mu.Lock()
	excluded := f.excluded
	f.excluded = nil
	configExcluded := f.configExcluded
	f.configExcluded = nil
	f.mu.Unlock()

	counts := CountExclusions(excluded)
	attrs := []any{"source", desc.SourceID, "outcome", marker.Outcome, "at", at}
	for _, c := range counts {
		attrs = append(attrs, "excluded_"+c.Reason, c.Count)
	}
	// The configuration refusals, counted under their own reasons rather than added to the rollout
	// ones. T101 asks for the preview-only variables to be counted, and a count is only worth reading
	// if it says what was counted.
	for _, c := range CountConfigExclusions(configExcluded) {
		attrs = append(attrs, "excluded_"+c.Reason, c.Count)
	}
	// The skew, published with the cycle (T142, FR-058). `samples` is reported beside the mean because
	// "the clocks agree" and "we never looked" are different claims, and a mean of zero is both.
	skew := f.skew.Report()
	attrs = append(attrs,
		"skew_samples", skew.Samples, "skew_mean", skew.Mean, "skew_min", skew.Min,
		"skew_max", skew.Max, "skew_threshold", skew.Threshold, "skew_exceeded", skew.Exceeded)
	f.log.InfoContext(ctx, "poll complete", attrs...)
	if skew.Beyond() {
		// Past the threshold is worth its own line rather than a field somebody has to notice: a
		// rollout's valid time and the instant the graph learned of it are then far enough apart to
		// change which change a diff window catches, which is exactly when an operator needs told.
		f.log.WarnContext(ctx, "vercel's clock is beyond the reporting threshold; nothing is corrected",
			"source", desc.SourceID, "samples", skew.Samples, "exceeded", skew.Exceeded,
			"mean", skew.Mean, "max", skew.Max, "threshold", skew.Threshold)
	}

	// A partial poll declares a gap rather than retracting what it did not see (FR-056). The checkpoint
	// carries the outcome so a reader can tell a complete extent from a sampled one.
	partial := marker.Outcome != "complete"
	if partial {
		f.log.WarnContext(ctx, "poll did not complete; the extent is partial and nothing is retracted",
			"source", desc.SourceID, "outcome", marker.Outcome, "reason", marker.Reason)
	}

	// The checkpoint (FR-056, FR-057; 004 T153).
	//
	// This feeder was built without one. Its comment above said "the checkpoint carries the outcome",
	// and the function ended `_ = em`: every Vercel fixture in the corpus held no `sourceCheckpoint` at
	// all, so the graph's extent never said "Vercel was read up to T". For an investigation that is the
	// dangerous gap rather than a cosmetic one — "no Vercel deploy in the window" and "Vercel was never
	// read" become the same answer, and the first is what an agent would conclude. `vercel-preview-
	// excluded-01` was written to prove the distinction and could not, because its extent had nothing
	// to show; T151's gate refused its golden, which is how this was found.
	f.mu.Lock()
	extentFrom, gapBefore := f.extentFrom, f.gapBefore
	f.extentFrom = time.Time{}
	// A partial poll means the NEXT checkpoint follows a gap: the silence before it was ignorance.
	f.gapBefore = partial
	targets := len(f.mapper.ProjectTargets)
	f.mu.Unlock()
	if extentFrom.IsZero() || extentFrom.After(at) {
		extentFrom = at
	}
	if err := em.Checkpoint(ctx, feeder.CheckpointFact{
		ExtentFrom: extentFrom,
		ExtentTo:   at,
		GapBefore:  gapBefore,
		Note:       checkpointNote(marker.Outcome, marker.Reason, targets, counts, CountConfigExclusions(configExcluded), skew),
	}); err != nil {
		return err
	}
	return nil
}

// checkpointNote states what a Vercel cycle read and under which filters, in a fixed order so a golden
// reads the same twice. It says what it can establish and nothing else: the token's scope regime is
// NOT stated, because T034 has not established it and a checkpoint asserting one would be inventing it.
func checkpointNote(outcome, reason string, targets int, rollouts, config []ExclusionCount, skew feeder.SkewReport) string {
	parts := []string{"poll=" + strings.TrimSpace(outcome)}
	if r := strings.TrimSpace(reason); r != "" && outcome != "complete" {
		parts = append(parts, "reason="+strconv.Quote(r))
	}
	parts = append(parts,
		"environment=production (the platform-stated target; previews and staging are excluded, FR-032)",
		fmt.Sprintf("mapped_projects=%d", targets))
	for _, c := range rollouts {
		parts = append(parts, fmt.Sprintf("excluded_%s=%d", c.Reason, c.Count))
	}
	for _, c := range config {
		parts = append(parts, fmt.Sprintf("excluded_%s=%d", c.Reason, c.Count))
	}
	parts = append(parts, fmt.Sprintf("skew_samples=%d", skew.Samples))
	if skew.Samples > 0 {
		parts = append(parts, "skew_mean="+skew.Mean.String())
	}
	return strings.Join(parts, " ")
}

// emit sends one event, treating a nil envelope as nothing to do.
//
// A REJECTED result is an error rather than a skip. The graph refusing an event this feeder built means
// the feeder built something invalid, and continuing would produce a recording that replays to a
// different graph than the run that made it.
func emit(ctx context.Context, em feeder.Emitter, env *graphv1.EventEnvelope) error {
	if env == nil {
		return nil
	}
	result, err := em.Emit(ctx, env)
	if err != nil {
		return err
	}
	if result.GetStatus() == graphv1.IngestResult_REJECTED {
		return fmt.Errorf("vercel: the graph refused event %s: %s %s",
			env.GetEventId(), result.GetReasonCode(), result.GetReasonDetail())
	}
	return nil
}
