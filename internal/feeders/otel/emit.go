// SPDX-License-Identifier: Apache-2.0

package otel

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/resolution"
	"github.com/Pierre-Theophile/aisre/internal/telemetry"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// Turning window aggregates into events (T052, FR-042, FR-008).
//
// Everything a window produces is a fact about the window, so every event's valid time is the
// window's start and every event id ends in `@w<window key>`. Nothing is emitted twice: a
// service node, a dependency node or an identity claim is emitted on first sight and then only
// when what it says changes, and a `calls` edge only when its weight class changes and the
// change has survived a full window (research §8). A graph that re-states unchanged facts
// every five minutes is a graph nobody can diff.
//
// Two ids are worth reading twice. The window key has two spellings, `20260901T1300Z` and
// `1300` — see IDFormat. And a third party's id and display name are its *short* name
// (`payments-db`, `stripe`), while its identity is always the full address; see shortHostName.

// Pointer backends and the latency instrument, as the baseline fixture records them. They are
// fields on Feeder so that an operator running Jaeger and Mimir is not stuck with them.
const (
	// DefaultTraceBackend is the tracing backend TRACE pointers name.
	DefaultTraceBackend = "tempo"
	// DefaultMetricBackend is the metrics backend METRIC pointers name.
	DefaultMetricBackend = "prometheus"
	// DefaultLatencyMetric is the instrument a service's METRIC pointer selects. It is the
	// OpenTelemetry semantic-convention name for server request duration, which is the metric
	// an SRE looking at a service asks for first.
	DefaultLatencyMetric = "http.server.request.duration"
)

// IDFormat is how the aggregation window is spelled inside an event id.
type IDFormat string

const (
	// IDFormatFull spells the window `@w20260901T1300Z`: the UTC date and the minute the
	// window started. It is the default, and the only one that cannot collide across days.
	IDFormatFull IDFormat = "full"
	// IDFormatCompat spells the window `@w1300`: the UTC hour and minute alone, which is what
	// fixtures/baseline-topology-01 was hand-authored in. Two windows a day apart then share
	// an id and the second is a DUPLICATE_NOOP, so it is for replaying fixtures written in
	// that spelling, never for a feeder that runs for more than a day.
	IDFormatCompat IDFormat = "compat"
)

// IDFormats is every accepted value of --id-format.
var IDFormats = []IDFormat{IDFormatFull, IDFormatCompat}

// Valid reports whether f is one of IDFormats. The empty string is valid and means
// IDFormatFull.
func (f IDFormat) Valid() bool { return f == "" || slices.Contains(IDFormats, f) }

// key renders the window start as it appears in an event id.
func (f IDFormat) key(start time.Time) string {
	if f == IDFormatCompat {
		return start.UTC().Format("1504")
	}
	return start.UTC().Format("20060102T1504Z")
}

// edgeState is what the emitter remembers about one call path between windows: the weight
// class the graph has been told, the class that is waiting for a second window to confirm it,
// and when the edge was last observed, which is what a retraction's valid end is taken from.
type edgeState struct {
	src         string
	srcShort    string
	dst         calleeKey
	dstShort    string
	emitted     uint32
	hasEmitted  bool
	pending     uint32
	hasPending  bool
	pendingFrom time.Time
	lastSeen    time.Time
}

// emitter turns closed windows into events and remembers just enough between them to say only
// what changed. It is not safe for concurrent use; one feeder owns one.
type emitter struct {
	desc          feeder.Description
	window        time.Duration
	retractAfter  int
	idFormat      IDFormat
	traceBackend  string
	metricBackend string
	latencyMetric string
	joinKeys      map[string]string

	nodes    map[calleeKey]string
	claims   map[calleeKey]string
	versions map[string]string
	edges    map[edgeKey]*edgeState
}

func newEmitter(f *Feeder) *emitter {
	return &emitter{
		desc:          f.Describe(),
		window:        f.windowLength(),
		retractAfter:  f.retractWindows(),
		idFormat:      f.idFormat(),
		traceBackend:  orDefault(f.TraceBackend, DefaultTraceBackend),
		metricBackend: orDefault(f.MetricBackend, DefaultMetricBackend),
		latencyMetric: orDefault(f.LatencyMetric, DefaultLatencyMetric),
		joinKeys:      f.PointerCompat.JoinKeys(serviceJoinKeys),
		nodes:         map[calleeKey]string{},
		claims:        map[calleeKey]string{},
		versions:      map[string]string{},
		edges:         map[edgeKey]*edgeState{},
	}
}

// emitWindow sends everything one closed window has to say, in a fixed order: the services
// that produced spans, the dependencies they were seen calling, the call paths whose weight
// class changed, the call paths that have gone quiet for long enough to be retracted, the
// identity claims, the version transitions, and the checkpoint that says the window was
// watched at all.
func (e *emitter) emitWindow(ctx context.Context, em feeder.Emitter, w *windowAgg) error {
	// One span per window and one lag sample per window (plan.md §Observability, FR-051). Lag
	// for this feeder is the age of the window that just closed: a receiver that is keeping up
	// closes a window one aggregation period after the traffic in it, so the gauge sits at
	// roughly one window and climbs the moment the aggregator stalls.
	ctx, span := telemetry.StartFeederWindow(ctx, e.desc.SourceID,
		w.start.UTC().Format(time.RFC3339), w.end().UTC().Format(time.RFC3339))
	defer span.End()
	telemetry.Default().SetFeederLag(e.desc.SourceID, time.Since(w.end()))

	var events []*graphv1.EventEnvelope

	nodes, err := e.serviceNodes(w)
	if err != nil {
		return err
	}
	events = append(events, nodes...)

	deps, err := e.dependencyNodes(w)
	if err != nil {
		return err
	}
	events = append(events, deps...)

	edges, err := e.edgeEvents(w)
	if err != nil {
		return err
	}
	events = append(events, edges...)
	events = append(events, e.retractions(w)...)

	claims, err := e.identityClaims(w)
	if err != nil {
		return err
	}
	events = append(events, claims...)
	events = append(events, e.changes(w)...)
	events = append(events, e.checkpoint(w))

	span.SetAttributes(attribute.Int(telemetry.SpanAttrEventCount, len(events)))
	if err := emitAll(ctx, em, events); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "emit window")
		return err
	}
	return nil
}

// serviceNodes asserts every service that produced spans in the window.
func (e *emitter) serviceNodes(w *windowAgg) ([]*graphv1.EventEnvelope, error) {
	var out []*graphv1.EventEnvelope
	for _, name := range w.serviceOrder {
		svc := w.services[name]
		ref := calleeKey{ns: feeder.NSOTelService, value: name}
		fingerprint := strings.Join([]string{"svc", svc.namespace, svc.version, svc.env}, "\x1f")
		if e.nodes[ref] == fingerprint {
			continue
		}
		e.nodes[ref] = fingerprint

		props, err := setStr(setStr(setStr(feeder.NewProps().
			Str(feeder.AttrServiceName, name),
			feeder.AttrServiceNamespace, svc.namespace),
			feeder.AttrServiceVersion, svc.version),
			feeder.AttrDeploymentEnvironment, svc.env).
			Build()
		if err != nil {
			return nil, fmt.Errorf("otel: service %s props: %w", name, err)
		}

		attrs := map[string]string{feeder.AttrServiceName: name}
		putAttr(attrs, feeder.AttrServiceNamespace, svc.namespace)
		putAttr(attrs, feeder.AttrDeploymentEnvironment, svc.env)

		out = append(out, feeder.UpsertNode(e.desc, e.id("svc", name, w.start), feeder.NodeFact{
			Ref:         feeder.Ref(ref.ns, ref.value),
			Type:        graphv1.NodeType_SERVICE,
			DisplayName: name,
			Props:       props,
			Pointers: []*graphv1.Pointer{
				// Join keys name which attribute of this vocabulary plays which role, so that
				// two answers about one service can be related to each other and to the graph
				// (ADR-0005 D6, 002 FR-014b). In OpenTelemetry semantic conventions the
				// version is `service.version` and the workload is `k8s.deployment.name`;
				// both are spelled differently in every other vocabulary, which is exactly
				// why the pointer has to say.
				feeder.WithJoinKeys(
					feeder.TracePointer(e.traceBackend,
						feeder.OTelSelector(attrs, feeder.AttrServiceName, feeder.AttrServiceNamespace), attrs),
					e.joinKeys),
				feeder.WithJoinKeys(
					feeder.MetricPointer(e.metricBackend,
						feeder.MetricSelector(e.latencyMetric, attrs,
							feeder.AttrServiceName, feeder.AttrServiceNamespace), attrs),
					e.joinKeys),
			},
			ValidAt: w.start,
		}))
	}
	return out, nil
}

// dependencyNodes asserts every callee that is a dependency rather than one of our services: a
// database, a queue, an external host, or a peer service that nothing in this window reported
// spans for but that carries an address of its own.
func (e *emitter) dependencyNodes(w *windowAgg) ([]*graphv1.EventEnvelope, error) {
	var out []*graphv1.EventEnvelope
	for _, key := range w.calleeOrder {
		c := w.callees[key]
		if !e.isDependency(w, c) {
			continue
		}
		fingerprint := strings.Join([]string{"dep", c.short, c.dbSystem, c.address, c.scheme, c.env,
			strconv.FormatInt(c.port, 10)}, "\x1f")
		if e.nodes[key] == fingerprint {
			continue
		}
		e.nodes[key] = fingerprint

		builder := setStr(setStr(setStr(setStr(feeder.NewProps(),
			feeder.AttrDBSystem, c.dbSystem),
			feeder.AttrDeploymentEnvironment, c.env),
			feeder.AttrServerAddress, c.address),
			feeder.AttrURLScheme, c.scheme)
		if c.port > 0 {
			builder = builder.Int(feeder.AttrServerPort, c.port)
		}
		props, err := builder.Build()
		if err != nil {
			return nil, fmt.Errorf("otel: dependency %s props: %w", key.value, err)
		}

		attrs := map[string]string{}
		putAttr(attrs, feeder.AttrDBSystem, c.dbSystem)
		putAttr(attrs, feeder.AttrDeploymentEnvironment, c.env)
		putAttr(attrs, feeder.AttrServerAddress, c.address)

		out = append(out, feeder.UpsertNode(e.desc, e.id("dep", c.short, w.start), feeder.NodeFact{
			Ref:         feeder.Ref(key.ns, key.value),
			Type:        graphv1.NodeType_THIRD_PARTY,
			DisplayName: c.short,
			Props:       props,
			Pointers: []*graphv1.Pointer{
				feeder.TracePointer(e.traceBackend,
					feeder.OTelSelector(attrs, feeder.AttrDBSystem, feeder.AttrServerAddress), attrs),
			},
			ValidAt: w.start,
		}))
	}
	return out, nil
}

// isDependency reports whether a callee gets a node of its own. A peer that produced spans in
// the same window is a service and has already been asserted as one; a peer that produced none
// and carries no address is a name and nothing more, which is not enough to describe an entity
// with.
func (e *emitter) isDependency(w *windowAgg, c *calleeAgg) bool {
	if c.key.ns == feeder.NSOTelService {
		if _, ours := w.services[c.key.value]; ours {
			return false
		}
	}
	return c.external
}

// edgeEvents asserts the call paths whose weight class the graph does not already know.
//
// The first observation of an edge is emitted immediately — a new dependency is news — and
// every later class change has to hold for a second window before it is believed (research
// §8). The event that finally carries the change is stamped with the window the new class was
// *first* seen in: hysteresis delays belief, which is observed time, and must not move the
// valid time of what was observed.
func (e *emitter) edgeEvents(w *windowAgg) ([]*graphv1.EventEnvelope, error) {
	var out []*graphv1.EventEnvelope
	for _, key := range w.edgeOrder {
		agg := w.edges[key]
		callee := w.callees[key.dst]
		state, ok := e.edges[key]
		if !ok {
			state = &edgeState{
				src:      key.src,
				srcShort: key.src,
				dst:      key.dst,
				dstShort: callee.short,
			}
			e.edges[key] = state
		}
		state.lastSeen = w.start

		class := feeder.WeightClassFromCount(agg.count(), e.window)
		at := w.start
		switch {
		case !state.hasEmitted:
			// A new call path is news; nothing is gained by sitting on it for a window.
			state.hasEmitted, state.emitted, state.hasPending = true, class, false
		case class == state.emitted:
			state.hasPending = false
			continue
		case state.hasPending && state.pending == class:
			// A second window at the new class confirms it. The class became true when it
			// was first seen, so that is the valid time; the delay is belief, not validity.
			at = state.pendingFrom
			state.emitted, state.hasPending = class, false
		default:
			state.hasPending, state.pending, state.pendingFrom = true, class, w.start
			continue
		}

		props, err := feeder.NewProps().
			Int(feeder.PropWindowSeconds, int64(e.window/time.Second)).
			Build()
		if err != nil {
			return nil, fmt.Errorf("otel: edge %s->%s props: %w", key.src, key.dst.value, err)
		}
		out = append(out, feeder.UpsertEdge(e.desc,
			e.id("edge", state.srcShort+"->"+state.dstShort, at), feeder.EdgeFact{
				Src:         feeder.Ref(feeder.NSOTelService, key.src),
				Dst:         feeder.Ref(key.dst.ns, key.dst.value),
				Type:        graphv1.EdgeType_CALLS,
				Props:       props,
				WeightClass: feeder.WeightClassPtr(class),
				ValidAt:     at,
			}))
	}
	return out, nil
}

// retractions ends the call paths that have gone unobserved for RetractAfter windows.
//
// The valid end is the end of the last window the edge *was* seen in, never the moment the
// feeder gave up waiting for it (FR-042): the call path stopped half an hour ago, and saying
// so now is a statement about observed time, which the graph stamps itself.
func (e *emitter) retractions(w *windowAgg) []*graphv1.EventEnvelope {
	if e.retractAfter <= 0 {
		return nil
	}
	stale := make([]edgeKey, 0)
	for key, state := range e.edges {
		if state.lastSeen.IsZero() || !state.lastSeen.Before(w.start) {
			continue
		}
		if int(w.start.Sub(state.lastSeen)/e.window) >= e.retractAfter {
			stale = append(stale, key)
		}
	}
	// Iteration over a map is random; a retraction stream must not be.
	slices.SortFunc(stale, func(x, y edgeKey) int {
		if c := strings.Compare(x.src, y.src); c != 0 {
			return c
		}
		if c := strings.Compare(x.dst.ns, y.dst.ns); c != 0 {
			return c
		}
		return strings.Compare(x.dst.value, y.dst.value)
	})

	out := make([]*graphv1.EventEnvelope, 0, len(stale))
	for _, key := range stale {
		state := e.edges[key]
		delete(e.edges, key)
		out = append(out, feeder.RetractEdge(e.desc,
			e.id("retract", state.srcShort+"->"+state.dstShort, state.lastSeen), feeder.EdgeRetraction{
				Src:      feeder.Ref(feeder.NSOTelService, state.src),
				Dst:      feeder.Ref(state.dst.ns, state.dst.value),
				Type:     graphv1.EdgeType_CALLS,
				ValidEnd: state.lastSeen.Add(e.window),
			}))
	}
	return out
}

// claims2 asserts every identifier this feeder knows an entity by (rule 4 of the SDK guide).
//
// A service claims its own telemetry name and carries the Kubernetes deployment it runs as in
// the claim's attributes, which is the input certain rule C3 reads — the feeder never merges
// the two itself. A dependency discovered behind a peer name claims the address it answers on,
// which is how `otel.service.name=redis` and `server.address=redis.shop.svc.cluster.local`
// become candidates for the same entity.
func (e *emitter) identityClaims(w *windowAgg) ([]*graphv1.EventEnvelope, error) {
	var out []*graphv1.EventEnvelope
	for _, name := range w.serviceOrder {
		svc := w.services[name]
		subject := calleeKey{ns: feeder.NSOTelService, value: name}
		fingerprint := strings.Join([]string{"svc", svc.env, svc.k8sDeployment, svc.k8sNamespace,
			svc.namespace, svc.cloudRunRevision, svc.cloudRunProject, svc.cloudRunRegion}, "\x1f")
		if e.claims[subject] == fingerprint {
			continue
		}
		e.claims[subject] = fingerprint

		builder := setStr(setStr(setStr(setStr(feeder.NewProps(),
			feeder.AttrDeploymentEnvironment, svc.env),
			feeder.AttrK8sDeploymentName, svc.k8sDeployment),
			feeder.AttrK8sNamespaceName, svc.k8sNamespace),
			feeder.AttrServiceNamespace, svc.namespace)
		// The Cloud Run revision this service is running in, under the spellings the published C4 rule
		// reads (003 FR-117, T182). All three or none: a revision name is unique within a service and
		// not globally, so a claim carrying the revision without the project would let C4 merge across
		// projects — which is the failure FR-010 exists to prevent, made by a *certain* rule with no
		// human in the loop.
		if svc.cloudRunRevision != "" && svc.cloudRunProject != "" && svc.cloudRunRegion != "" {
			builder = setStr(setStr(setStr(builder,
				resolution.AttrGCPRevisionName, svc.cloudRunRevision),
				resolution.AttrGCPProject, svc.cloudRunProject),
				resolution.AttrGCPRegion, svc.cloudRunRegion)
		}
		attrs, err := builder.Build()
		if err != nil {
			return nil, fmt.Errorf("otel: claim for %s: %w", name, err)
		}
		out = append(out, feeder.IdentityClaim(e.desc, e.id("claim", name, w.start), feeder.IdentityFact{
			Subject:    feeder.Ref(subject.ns, subject.value),
			Claim:      feeder.Ref(subject.ns, subject.value),
			Attributes: attrs,
		}))
	}

	for _, key := range w.calleeOrder {
		c := w.callees[key]
		if !e.isDependency(w, c) {
			continue
		}
		fingerprint := strings.Join([]string{"dep", c.dbSystem, c.address, c.env, strconv.FormatInt(c.port, 10)}, "\x1f")
		if e.claims[key] == fingerprint {
			continue
		}
		e.claims[key] = fingerprint

		builder := setStr(setStr(feeder.NewProps(),
			feeder.AttrDBSystem, c.dbSystem),
			feeder.AttrDeploymentEnvironment, c.env)
		if c.port > 0 {
			builder = builder.Int(feeder.AttrServerPort, c.port)
		}
		// The Cloud SQL instance connection name, when the address this caller reached the database
		// at is the connector's Unix socket (003 FR-120, C7). The address already *is* the evidence —
		// `/cloudsql/<project>:<region>:<instance>` is the string the client was configured with — so
		// this lifts it onto the claim under the spelling the published rule reads, rather than
		// asking the rule to parse an address it should not have to understand.
		//
		// Without it C7 has only the GCP feeder's own side and cannot fire: a rule with one half of
		// its data is published, evaluated on every claim, and silently never matches.
		if connection, ok := cloudSQLConnectionName(c.address); ok {
			builder = setStr(builder, resolution.AttrGCPSQLConnectionName, connection)
		}
		attrs, err := builder.Build()
		if err != nil {
			return nil, fmt.Errorf("otel: claim for %s: %w", key.value, err)
		}
		claim := feeder.Ref(key.ns, key.value)
		if c.address != "" {
			claim = feeder.Ref(feeder.NSServerAddress, c.address)
		}
		out = append(out, feeder.IdentityClaim(e.desc, e.id("claim", c.short, w.start), feeder.IdentityFact{
			Subject:    feeder.Ref(key.ns, key.value),
			Claim:      claim,
			Attributes: attrs,
		}))
	}
	return out, nil
}

// serviceJoinKeys is the role → attribute map every service pointer this feeder mints carries.
// It is a package-level value rather than a literal per pointer because the map is a statement
// about the vocabulary, not about the pointer: every `otel-semconv/1.30` pointer spells these
// two roles the same way, and two spellings that drifted apart would be a silent join failure.
var serviceJoinKeys = map[string]string{
	feeder.JoinRoleVersion:  feeder.AttrServiceVersion,
	feeder.JoinRoleWorkload: feeder.AttrK8sDeploymentName,
}

// changes reports a `service.version` transition as a rollout (FR-042).
//
// The first version ever seen for a service is not a rollout: the service was already running
// that version when the feeder started watching, and calling that a change would put a rollout
// on every service the first time anyone looked.
func (e *emitter) changes(w *windowAgg) []*graphv1.EventEnvelope {
	var out []*graphv1.EventEnvelope
	for _, name := range w.serviceOrder {
		version := w.services[name].version
		if version == "" {
			continue
		}
		previous, seen := e.versions[name]
		e.versions[name] = version
		if !seen || previous == version {
			continue
		}
		out = append(out, feeder.ObserveChange(e.desc,
			e.id("chg", name+"@"+version, w.start), feeder.ChangeFact{
				Ref:     feeder.Ref(feeder.NSOTelChange, name+"@"+version),
				Kind:    graphv1.ChangeKind_ROLLOUT,
				Summary: "service.version " + previous + " → " + version + " observed in traces",
				// ACTOR_KIND_UNKNOWN, deliberately, and not "unspecified" (ADR-0005 D1).
				// The two mean different things and the difference is the whole point of the
				// enum having both: "unspecified" is "this source said nothing about who
				// acted", which is what the graph should record when nothing was observed.
				// Here something *was* observed — a service that was running one version is
				// running another, so somebody or something deployed it — and OTLP carries no
				// field that could say who. The evidence for the claim is in the summary: the
				// two versions and the window they straddle. Naming a kind would be a guess,
				// and the enum exists so that this feeder does not have to make one.
				ActorKind: graphv1.ActorKind_ACTOR_KIND_UNKNOWN,
				Targets:   []*graphv1.Ref{feeder.Ref(feeder.NSOTelService, name)},
				ValidAt:   w.start,
			}))
	}
	return out
}

// checkpoint records that the window was watched, which is what tells "nothing happened" from
// "nobody was watching" (FR-032). It is built rather than delegated to Emitter.Checkpoint
// because the extent of a window is the window, and the id says so.
func (e *emitter) checkpoint(w *windowAgg) *graphv1.EventEnvelope {
	return feeder.SourceCheckpoint(e.desc, feeder.NewID(e.desc.SourceID, "ckpt", "w"+e.idFormat.key(w.start)),
		feeder.CheckpointFact{
			ExtentFrom: w.start,
			ExtentTo:   w.end(),
			Note: fmt.Sprintf("aggregation window [%s, %s) closed",
				w.start.UTC().Format("15:04"), w.end().UTC().Format("15:04")),
		})
}

// id mints `<source>:<kind>:<subject>@w<window>`, the shape every fixture in this repository
// writes (research §4).
func (e *emitter) id(kind, subject string, window time.Time) string {
	return feeder.NewID(e.desc.SourceID, kind, subject+"@w"+e.idFormat.key(window))
}

// emitAll sends events in order, stopping at the first refusal. A REJECTED result is an
// answer, not a transport failure, but for this feeder it means a bug in this file: everything
// it emits is built from a whitelist, so a refusal is worth stopping for.
func emitAll(ctx context.Context, em feeder.Emitter, events []*graphv1.EventEnvelope) error {
	for _, ev := range events {
		result, err := em.Emit(ctx, ev)
		if err != nil {
			return err
		}
		if result.GetStatus() == graphv1.IngestResult_REJECTED {
			return fmt.Errorf("otel: %s refused: %s (%s)",
				ev.GetEventId(), result.GetReasonCode(), result.GetReasonDetail())
		}
	}
	return nil
}

// setStr sets a property only when there is one, so that an absent attribute is absent rather
// than an empty string the graph would have to interpret.
func setStr(p *feeder.Props, key, value string) *feeder.Props {
	if value == "" {
		return p
	}
	return p.Str(key, value)
}

// putAttr is setStr for a pointer's attribute map.
func putAttr(attrs map[string]string, key, value string) {
	if value != "" {
		attrs[key] = value
	}
}

func orDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
