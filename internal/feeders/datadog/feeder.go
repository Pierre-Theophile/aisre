// SPDX-License-Identifier: Apache-2.0

package datadog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	eventlog "github.com/Pierre-Theophile/aisre/internal/log"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/versionstamp"
)

// The feeder half (contracts/datadog-feeder.md).
//
// It reads payloads — a page of the monitor list, a poll marker — from a feeder.Source and emits
// events, so a live run and the replay of a recorded fixture are one code path (FR-044). The live
// poller that fetches the payloads, and the doorbell that asks it to poll now, sit outside: they push
// into a source, and nothing they do reaches the graph except through here.
//
// It emits events only and never reads the graph to decide what to emit (constitution III). What it
// remembers between payloads — the last state of each monitor group, the monitors a complete poll
// listed — only saves calls or detects a change; every event id is built from Datadog-stated facts, so
// a restarted feeder re-sends ids already sent, which are no-ops.

// SchemaVersion is the event schema version this feeder emits.
const SchemaVersion = "1.0.0"

// The payload kinds, which are also the fixture directory names a recording stores them under.
const (
	// PayloadMonitors is one page of `GET /api/v1/monitor?group_states=all`: a JSON array of
	// monitors, each with its group states.
	PayloadMonitors = "monitors"
	// PayloadDiscovery is one discovery tick: the log sources watched at that instant, whose SERVICE
	// nodes and log-service correlations are asserted every tick whatever the graph holds
	// (contract §2). The list is in the payload rather than only in Options so that a recording states
	// the configuration its live run had, and a source added mid-run replays at the instant it was.
	PayloadDiscovery = "discovery"
	// PayloadPoll declares a poll's outcome. It is its own payload because a partial poll's defining
	// property is that pages are missing, which only a separate marker can state.
	PayloadPoll = "poll"
)

// The monitor poll cadence (contract §3): 15–30 s, default 20 s.
const (
	DefaultPollInterval = 20 * time.Second
	MinPollInterval     = 15 * time.Second
	MaxPollInterval     = 30 * time.Second
)

// DefaultHistory is how far back a group's stated instants are emitted the first time this run sees
// the group. It bounds a first start: a monitor that last resolved three months ago is not news, and
// emitting its whole stated history would flood the intake with incidents long closed.
const DefaultHistory = 24 * time.Hour

// Options configures one feeder run.
type Options struct {
	// OrgSlug suffixes the source id. Required.
	OrgSlug string
	// Site is the Datadog site (e.g. datadoghq.eu). It builds the link to a monitor and is stated in
	// the checkpoint; empty means no link.
	Site string
	// Capabilities in force. Nil uses DefaultCapabilities.
	Capabilities Capabilities
	// MonitorTags is the tag filter (FR-025c): a monitor is in scope only if it carries every one.
	// Empty means every monitor the key can read.
	MonitorTags []string
	// PollInterval is the monitor poll cadence, recorded on every transition as the sampling
	// interval. Zero uses DefaultPollInterval.
	PollInterval time.Duration
	// History overrides DefaultHistory.
	History time.Duration
	// OperatorAsserted is the gate's assertion, "operator_asserted by <name> at <instant>", when the
	// start rested on one; every checkpoint records it (read-only-operations.md §3).
	OperatorAsserted string
	// LogSources are the watched `<env>/<service>` log sources, used by a discovery tick that names
	// none of its own.
	LogSources []LogSource
	// VersionOverrides are per-source version attributes, `<env>/<service>` → `name` (a tag) or
	// `@name` (an attribute). An override is recorded as `source: operator` and is subject to the same
	// share test, so a typo reads as "not found" rather than as a silent empty split.
	VersionOverrides map[string]string
	// Thresholds are the share test's thresholds. Zero uses versionstamp's published defaults.
	Thresholds versionstamp.Thresholds
	// Log is where operational telemetry goes. Nil discards.
	Log *slog.Logger
}

// Validate refuses options that could not produce an honest run.
func (o Options) Validate() error {
	if _, err := Describe(o.OrgSlug); err != nil {
		return err
	}
	if o.PollInterval != 0 && (o.PollInterval < MinPollInterval || o.PollInterval > MaxPollInterval) {
		return fmt.Errorf("datadog: a monitor poll interval of %s is outside the published %s–%s; a "+
			"longer one is a sampling the transitions' marker would understate, and a shorter one spends "+
			"the quota the human reserve is kept for", o.PollInterval, MinPollInterval, MaxPollInterval)
	}
	if o.History < 0 {
		return fmt.Errorf("datadog: a negative history (%s)", o.History)
	}
	for source, spec := range o.VersionOverrides {
		if _, err := ParseLogSource(source); err != nil {
			return fmt.Errorf("datadog: version override for %q: %w", source, err)
		}
		if _, err := OverrideCandidate(spec); err != nil {
			return err
		}
	}
	for _, tag := range o.MonitorTags {
		if !strings.Contains(tag, ":") || strings.ContainsAny(tag, " ,") {
			return fmt.Errorf("datadog: monitor tag %q is not a single `key:value`", tag)
		}
	}
	return nil
}

// Feeder reads one Datadog organisation.
type Feeder struct {
	opts Options
	desc feeder.Description
	log  *slog.Logger

	mu sync.Mutex
	// groups is the last observed state of each alerting group, keyed by its entity value. It is
	// what makes a change detectable, and what bounds the stated instants already emitted.
	groups map[string]groupState
	// series is the per-entity series the published suppression filter reads.
	series map[string]*eventlog.AlertSeries
	// cycle is the monitors this poll has listed so far; listed is what the last COMPLETE poll
	// listed. A monitor in listed and not in a complete cycle has been deleted or left scope.
	cycle  map[string]monitorObservation
	listed map[string]monitorObservation
	// emittedGroups are the group entities a transition was emitted for, per monitor, so a retracted
	// monitor's groups are retracted with it.
	emittedGroups map[string]map[string]bool
	// For the next checkpoint: what was stated rather than emitted.
	suppressed []string
	undated    []string
	retracted  []string
	unresolved []string
	outOfScope int
	// verdicts is the digest of the last assertion of each measured log source.
	verdicts       map[string]string
	discoveryNotes []string
	// warnedEnv are the environment warnings already logged: each is logged once per run, and stated in
	// every checkpoint it applies to.
	warnedEnv map[string]bool
	// envNotes are the last environment statements per source.
	envNotes map[string]envStatement
}

// New returns a feeder over opts.
func New(opts Options) (*Feeder, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	desc, err := Describe(opts.OrgSlug)
	if err != nil {
		return nil, err
	}
	desc.SchemaVersion = SchemaVersion
	desc.Namespaces = []string{NSService, NSLogService, NSMonitor}
	if opts.Capabilities == nil {
		opts.Capabilities = DefaultCapabilities()
	}
	if opts.PollInterval == 0 {
		opts.PollInterval = DefaultPollInterval
	}
	if opts.History == 0 {
		opts.History = DefaultHistory
	}
	if opts.Thresholds == (versionstamp.Thresholds{}) {
		opts.Thresholds = versionstamp.DefaultThresholds()
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Feeder{
		opts: opts, desc: desc, log: log,
		groups: map[string]groupState{}, series: map[string]*eventlog.AlertSeries{},
		cycle: map[string]monitorObservation{}, listed: map[string]monitorObservation{},
		emittedGroups: map[string]map[string]bool{}, verdicts: map[string]string{},
	}, nil
}

// Describe returns the feeder's contract.
func (f *Feeder) Describe() feeder.Description { return f.desc }

// Run reads from src and writes to em until src is exhausted or ctx ends.
func (f *Feeder) Run(ctx context.Context, src feeder.Source, em feeder.Emitter) error {
	if err := f.desc.Validate(); err != nil {
		return err
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
		if err := f.apply(ctx, em, payload); err != nil {
			return err
		}
	}
}

// PollMarker is the `poll` payload: what a poll covered and whether it finished.
type PollMarker struct {
	// Outcome is "complete" or "partial".
	Outcome string `json:"outcome"`
	// Pages is how many pages the poll read.
	Pages int `json:"pages,omitempty"`
	// Reason says why a partial poll stopped: a page failure, a timeout, the quota.
	Reason string `json:"reason,omitempty"`
	// StopReason is the typed stop, when the poll or an area was stopped by the budget or by Datadog:
	// StopQuota or StopRateLimited (FR-083). A stop is never the same fact as having found nothing.
	StopReason string `json:"stop_reason,omitempty"`
	// Deferred are the areas that yielded since the last poll, in the published order (FR-082).
	Deferred []string `json:"deferred,omitempty"`
	// ResumeAt is when a rate-limited poller reads again: Datadog's own Retry-After, never a guess.
	ResumeAt *time.Time `json:"resume_at,omitempty"`
	// Usage is the budget's usage report at the poll (FR-084).
	Usage string `json:"usage,omitempty"`
}

// apply dispatches one payload. A payload kind nobody handles is an error, never a silently ignored
// file in a fixture directory.
func (f *Feeder) apply(ctx context.Context, em feeder.Emitter, payload feeder.Payload) error {
	switch payload.Kind {
	case PayloadMonitors:
		if !f.opts.Capabilities.Enabled(CapMonitors) {
			return fmt.Errorf("datadog: a %s payload arrived with the monitors capability off; replaying "+
				"it would make the capability decorative", payload.Kind)
		}
		return f.applyMonitors(ctx, em, payload.Bytes, payload.At)
	case PayloadDiscovery:
		if !f.opts.Capabilities.Enabled(CapLogs) {
			return fmt.Errorf("datadog: a %s payload arrived with the logs capability off", payload.Kind)
		}
		var tick DiscoveryTick
		if err := json.Unmarshal(payload.Bytes, &tick); err != nil {
			return fmt.Errorf("datadog: decoding a %s payload: %w", payload.Kind, err)
		}
		return f.applyDiscovery(ctx, em, tick, payload.At)
	case PayloadPoll:
		var marker PollMarker
		if err := json.Unmarshal(payload.Bytes, &marker); err != nil {
			return fmt.Errorf("datadog: decoding a %s payload: %w", payload.Kind, err)
		}
		return f.applyPoll(ctx, em, marker, payload.At)
	default:
		return fmt.Errorf("datadog: payload kind %q is not one this feeder reads (%s, %s); a payload "+
			"nobody handles is a fixture that silently tests less than it claims",
			payload.Kind, PayloadMonitors, PayloadPoll+", "+PayloadDiscovery)
	}
}

// DiscoveryTick is the `discovery` payload.
type DiscoveryTick struct {
	// LogSources are the watched sources as `<env>/<service>`. Empty uses Options.LogSources.
	LogSources []string `json:"log_sources,omitempty"`
	// Window is what the measurements were taken over; nil when the tick carries none.
	Window *DiscoveryWindow `json:"window,omitempty"`
	// Measurements are the presence counts per source (discovery.go).
	Measurements []SourceMeasurement `json:"measurements,omitempty"`
}

// applyDiscovery asserts every watched log source's SERVICE node and log-service correlation. The
// event ids are the source's own, so an unchanged tick re-sends ids already sent (T063 builds the
// version-stamp discovery on this tick).
func (f *Feeder) applyDiscovery(ctx context.Context, em feeder.Emitter, tick DiscoveryTick, at time.Time) error {
	sources := f.opts.LogSources
	if len(tick.LogSources) > 0 {
		sources = nil
		for _, spec := range tick.LogSources {
			src, err := ParseLogSource(spec)
			if err != nil {
				return err
			}
			sources = append(sources, f.configured(src))
		}
	}
	measured := f.sourceMeasurements(tick)
	for _, src := range sources {
		batch, err := LogSourceEvents(f.desc, src, at)
		if err != nil {
			return err
		}
		m, ok := measured[src.Key()]
		f.warnEnv(ctx, src, m, ok)
		for _, ev := range batch {
			if ok && m.Failed == "" && tick.Window != nil && ev.GetUpsertNode() != nil {
				continue // the measured assertion below replaces the plain one
			}
			if err := emit(ctx, em, ev); err != nil {
				return err
			}
		}
		switch {
		case ok && m.Failed == "" && tick.Window != nil:
			if m.EnvField != "" && src.Env != "" {
				// The field the environment was found in is the one the pointer searches.
				src.EnvField = m.EnvField
			}
			if err := f.emitMeasuredSource(ctx, em, src, m, *tick.Window, at); err != nil {
				return err
			}
		case ok:
			f.mu.Lock()
			f.discoveryNotes = append(f.discoveryNotes, fmt.Sprintf("%s: not measured (%s); its pointer and "+
				"verdict stand as last asserted", src.Key(), orUnset(m.Failed)))
			f.mu.Unlock()
		}
	}
	if tick.Window == nil {
		return nil
	}
	// The discovery tick's own checkpoint: what was measured, with the shares, over which window.
	f.mu.Lock()
	lines := append([]string{"datadog log-source discovery", "capabilities: " + f.opts.Capabilities.String(),
		fmt.Sprintf("thresholds: %.4g of lines and %.4g of error lines; conventions %s",
			f.opts.Thresholds.LineShare, f.opts.Thresholds.ErrorLineShare, versionstamp.ConventionsVersion)},
		f.discoveryLines()...)
	f.mu.Unlock()
	return em.Checkpoint(ctx, feeder.CheckpointFact{
		ExtentFrom: tick.Window.From, ExtentTo: at, Note: strings.Join(lines, "\n"),
	})
}

// applyPoll closes a poll: retractions if it was complete, then the checkpoint.
func (f *Feeder) applyPoll(ctx context.Context, em feeder.Emitter, marker PollMarker, at time.Time) error {
	var complete bool
	switch marker.Outcome {
	case "complete":
		complete = true
	case "partial":
	default:
		return fmt.Errorf("datadog: poll outcome %q is neither \"complete\" nor \"partial\"; a poll whose "+
			"outcome is unstated cannot be evidence of absence (FR-012)", marker.Outcome)
	}

	f.mu.Lock()
	var gone []monitorObservation
	if complete {
		for id, obs := range f.listed {
			if _, still := f.cycle[id]; !still {
				gone = append(gone, obs)
			}
		}
		f.listed = f.cycle
	}
	// A partial poll keeps what the last complete one listed: an unread page is not a deletion.
	f.cycle = map[string]monitorObservation{}
	f.mu.Unlock()

	sort.Slice(gone, func(i, j int) bool { return gone[i].ID < gone[j].ID })
	for _, obs := range gone {
		if err := f.retractMonitor(ctx, em, obs, at); err != nil {
			return err
		}
	}
	// A complete poll vouches for the interval it sampled. A partial one vouches for nothing: its extent
	// is the instant it ran, so the gap it declares runs from the last instant anything covered to it,
	// never an interval that ends before it starts.
	from := at.Add(-f.opts.PollInterval)
	if !complete {
		from = at
	}
	return em.Checkpoint(ctx, feeder.CheckpointFact{
		ExtentFrom: from,
		ExtentTo:   at,
		GapBefore:  !complete,
		Note:       f.checkpointNote(marker, complete),
	})
}

// retractMonitor ends a deleted monitor's validity, and its groups'. There is no stated instant for a
// deletion, so the end is this poll: the last poll that listed it and this one bound the instant, and
// the checkpoint says so.
func (f *Feeder) retractMonitor(ctx context.Context, em feeder.Emitter, obs monitorObservation, at time.Time) error {
	f.mu.Lock()
	groups := make([]string, 0, len(f.emittedGroups[obs.ID]))
	for g := range f.emittedGroups[obs.ID] {
		groups = append(groups, g)
	}
	delete(f.emittedGroups, obs.ID)
	f.retracted = append(f.retracted, fmt.Sprintf("monitor %s (%s): absent from a complete poll; ended at "+
		"that poll, the deletion falling between it and the previous one", obs.ID, obs.Name))
	f.mu.Unlock()
	sort.Strings(groups)
	stamp := at.UTC().Format(time.RFC3339)
	for _, g := range groups {
		ref := feeder.Ref(NSMonitor, g)
		if err := emit(ctx, em, feeder.RetractNode(f.desc, feeder.NewID(f.desc.SourceID, "retract", g, stamp),
			feeder.NodeRetraction{Meta: feeder.Meta{SourceObservedAt: at}, Ref: ref, ValidEnd: at})); err != nil {
			return err
		}
	}
	return emit(ctx, em, feeder.RetractNode(f.desc, feeder.NewID(f.desc.SourceID, "retract", obs.ID, stamp),
		feeder.NodeRetraction{Meta: feeder.Meta{SourceObservedAt: at}, Ref: obs.Ref(), ValidEnd: at}))
}

// checkpointNote states the scope in force and everything this poll stated rather than emitted, then
// clears the latter (FR-057, FR-012).
func (f *Feeder) checkpointNote(marker PollMarker, complete bool) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	lines := []string{
		"datadog monitor poll: " + marker.Outcome,
		"capabilities: " + f.opts.Capabilities.String(),
		"site: " + orUnset(f.opts.Site),
		"read-only: " + readOnlyStatement(f.opts.OperatorAsserted),
		"monitor tag filter: " + orUnset(strings.Join(f.opts.MonitorTags, ",")),
		fmt.Sprintf("poll interval: %s; every transition is sampled at it, and one that opened and closed "+
			"between two polls is visible only through the instants Datadog states", f.opts.PollInterval),
		fmt.Sprintf("monitors listed: %d; out of the tag scope and skipped: %d", len(f.listed), f.outOfScope),
	}
	if !complete {
		lines = append(lines, "partial: "+orUnset(marker.Reason)+"; nothing unread was retracted")
	}
	if marker.StopReason != "" {
		stop := "stopped: " + marker.StopReason
		if marker.ResumeAt != nil {
			stop += "; reading again from " + marker.ResumeAt.UTC().Format(time.RFC3339) + ", as Datadog asked"
		}
		lines = append(lines, stop)
	}
	if len(marker.Deferred) > 0 {
		lines = append(lines, "deferred for quota, in the published order: "+strings.Join(marker.Deferred, ", ")+
			"; what they would have read is unread, not absent")
	}
	if marker.Usage != "" {
		lines = append(lines, "usage: "+marker.Usage)
	}
	add := func(title string, items []string) {
		items = uniqueSorted(items)
		if len(items) > 0 {
			lines = append(lines, title+":")
			for _, item := range items {
				lines = append(lines, "  - "+item)
			}
		}
	}
	add("suppressed transitions (stated, not triggering)", f.suppressed)
	add("undated transitions (a state changed and Datadog states no instant for it; not emitted, "+
		"because a transition is keyed on its instant and the poll instant is never used)", f.undated)
	add("unresolved watched entities (no service and environment in the query, tags or group)", f.unresolved)
	add("retracted", f.retracted)
	f.suppressed, f.undated, f.retracted, f.unresolved, f.outOfScope = nil, nil, nil, nil, 0
	return strings.Join(lines, "\n")
}

func readOnlyStatement(asserted string) string {
	if asserted == "" {
		return "verified by the startup gate, or not applicable to a replay"
	}
	return asserted
}

func uniqueSorted(items []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if !seen[item] {
			seen[item] = true
			out = append(out, item)
		}
	}
	sort.Strings(out)
	return out
}

func orUnset(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// emit sends one event and turns a refusal into an error: this feeder never emits something the graph
// should refuse.
func emit(ctx context.Context, em feeder.Emitter, ev *graphv1.EventEnvelope) error {
	result, err := em.Emit(ctx, ev)
	if err != nil {
		return err
	}
	if result.GetStatus() == graphv1.IngestResult_REJECTED {
		return fmt.Errorf("datadog: the graph refused event %s: %s %s",
			ev.GetEventId(), result.GetReasonCode(), result.GetReasonDetail())
	}
	return nil
}

// warnEnv states, in the checkpoint and once in the log, how a source's environment was read and what
// to fix when it could not be (envfield.go). A source discovered without an environment keeps saying so
// on the ticks that did not rediscover it.
func (f *Feeder) warnEnv(ctx context.Context, src LogSource, m SourceMeasurement, measured bool) {
	note, warning := envNote(src, m, measured)
	f.mu.Lock()
	if f.envNotes == nil {
		f.envNotes = map[string]envStatement{}
	}
	last, known := f.envNotes[src.Key()]
	switch {
	case measured && m.EnvDiscovery == nil && known:
		// A tick that did not rediscover the field says what the last discovery said.
		note, warning = last.note, last.warning
	case note != "":
		f.envNotes[src.Key()] = envStatement{note, warning}
	}
	f.mu.Unlock()
	if note == "" {
		return
	}
	f.noteDiscovery(note)
	f.mu.Lock()
	if f.warnedEnv == nil {
		f.warnedEnv = map[string]bool{}
	}
	first := !f.warnedEnv[note]
	f.warnedEnv[note] = true
	f.mu.Unlock()
	switch {
	case !first:
	case warning:
		f.log.WarnContext(ctx, note)
	default:
		f.log.InfoContext(ctx, note)
	}
}

// envStatement is the last thing said about a source's environment.
type envStatement struct {
	note    string
	warning bool
}

// configured returns a tick's source with what only the configuration states — its index, its
// environment field, its version override — so a recorded tick replays against the same selector.
func (f *Feeder) configured(src LogSource) LogSource {
	for _, c := range f.opts.LogSources {
		if c.Key() == src.Key() {
			return c
		}
	}
	return src
}
