// SPDX-License-Identifier: Apache-2.0

package datadog

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/versionstamp"
)

// Topology from Datadog APM (T090; spec section B, FR-009–FR-017), the `apm_topology` capability.
//
// Built from the published API shapes and not yet verified against a live organisation. A `topology`
// payload is one environment's window: the services and the services each calls, the traffic between
// them, the versions and operations each service reports, and the hosts it runs on. The poller normalises
// Datadog's answers into it, so a recording of it replays with no Datadog shape in sight.
//
//   - a service is a SERVICE node on the SAME ref a watched log source uses (`datadog.service=<env>/<svc>`),
//     so the log pointer, the APM pointers and the version are one entity's. Because a source's latest
//     assertion of a node replaces its earlier one, the log path and this one both build the node from
//     the same parts (decoration below) rather than each asserting its own half;
//   - a dependency is a `calls` edge carrying only a weight CLASS from the published ordinal scale,
//     derived from a count of retained spans and discarded; an edge whose pair has no count carries no
//     class and the checkpoint says so (a class 0 would claim traffic was watched and found negligible);
//   - the edge's valid start is its first observed window. An edge not observed in RetractAfter
//     consecutive COMPLETE reads is retracted with its valid end at the end of the last window it was
//     seen in. A part of the read that did not finish neither confirms nor denies anything (FR-012);
//   - a dependency target Datadog does not list as a service is a THIRD_PARTY node on the same ref, marked
//     `sre.datadog.unlisted_dependency`; a later listing asserts a SERVICE on that ref, which replaces it;
//   - a version that was not in the previous complete read is a ROLLOUT valid at the window it first
//     appeared in, a bound and marked one, with UNKNOWN actor kind and the evidence (APM states a version,
//     never who deployed it), and the version's `deploy.*` correlation key where it has one;
//   - a host is an INFRA_RESOURCE with a `runs-on` edge from the service. Only the `host` is read: never a
//     container id or a pod name, which are ephemeral replicas (FR-014).
//
// Where Datadog does not state since when a node or a host relation has held, the valid start is unknown
// and never the poll's instant (FR-017).

// PayloadTopology is one environment's topology window.
const PayloadTopology = "topology"

// The topology defaults and bounds.
const (
	// DefaultTopologyInterval is how often each environment is read, and the length of its window.
	DefaultTopologyInterval = 15 * time.Minute
	// DefaultTopologyRetractAfter is how many consecutive complete reads may not observe an edge before it
	// is retracted.
	DefaultTopologyRetractAfter = 4
	// MaxTopologyServices bounds the services one environment's window asserts; a window that reports more
	// says so.
	MaxTopologyServices = 500
	// MaxHostsPerService bounds the hosts asserted per service.
	MaxHostsPerService = 20
)

// The parts of a topology read, and their statuses.
const (
	PartComplete = "complete"
	PartPartial  = "partial"
	PartUnread   = "unread"
)

// NSHost addresses a host Datadog reports a service running on, valued by the host name.
const NSHost = "datadog.host"

// Properties on what this section emits.
const (
	// PropUnlistedDependency marks a THIRD_PARTY node for a dependency Datadog reports but does not list
	// as a service: a later listing promotes it (FR-015).
	PropUnlistedDependency = "sre.datadog.unlisted_dependency"
	// PropWeightBasis says what a weight class was derived from.
	PropWeightBasis = "sre.datadog.weight_basis"
	// PropPreviousVersion is the version a rollout replaced.
	PropPreviousVersion = "sre.datadog.previous_version"
	// BoundFirstSeenInAPM is what an APM-observed rollout's valid start is a bound from.
	BoundFirstSeenInAPM = "first_seen_in_apm"
	// WeightBasisRetainedSpans is the basis of every class: a count of the spans Datadog retained, which is
	// a lower bound of the traffic.
	WeightBasisRetainedSpans = "retained_spans_lower_bound"
)

// TopologyScope is which environments are read and how patient the retraction is. It is configuration,
// and recorded in every topology checkpoint.
type TopologyScope struct {
	// Envs are the environments read; a service in another is out of scope.
	Envs []string
	// RetractAfter is the consecutive complete reads an edge may go unobserved; zero uses the default.
	RetractAfter int
}

// Validate refuses a scope that could not read anything honestly.
func (s TopologyScope) Validate() error {
	if len(s.Envs) == 0 {
		return fmt.Errorf("datadog: the apm_topology capability is enabled and names no environment " +
			"(--apm-envs); a service name is unique only within one environment, and which ones are in " +
			"scope is the operator's to say (FR-009)")
	}
	for _, env := range s.Envs {
		if strings.TrimSpace(env) == "" || strings.ContainsAny(env, " ,:/*") {
			return fmt.Errorf("datadog: APM environment %q is not a single environment name", env)
		}
	}
	if s.RetractAfter < 0 {
		return fmt.Errorf("datadog: a negative retraction threshold (%d)", s.RetractAfter)
	}
	return nil
}

func (s TopologyScope) retractAfter() int {
	if s.RetractAfter > 0 {
		return s.RetractAfter
	}
	return DefaultTopologyRetractAfter
}

func (s TopologyScope) has(env string) bool {
	for _, e := range s.Envs {
		if e == env {
			return true
		}
	}
	return false
}

func (s TopologyScope) String() string {
	return fmt.Sprintf("environments [%s], retract after %d consecutive complete reads",
		strings.Join(s.Envs, ", "), s.retractAfter())
}

// ---- the payload -------------------------------------------------------------------------------------

// TopologyPayload is the `topology` payload.
type TopologyPayload struct {
	// Env is the environment read.
	Env string `json:"env"`
	// Window is what the read covered.
	Window TopologyWindow `json:"window"`
	// Parts says how much of each part was read.
	Parts TopologyParts `json:"parts"`
	// Services are the services Datadog reports in the environment and the services each calls.
	Services []TopologyService `json:"services,omitempty"`
	// Traffic are counts of retained spans per caller and callee; they become a weight class and are
	// never stored.
	Traffic []TopologyTraffic `json:"traffic,omitempty"`
	// Versions are the versions each service reports, with the spans that reported each.
	Versions []TopologyVersion `json:"versions,omitempty"`
	// Operations are the operations each service emits, with their spans; the busiest names the APM metric
	// pointer.
	Operations []TopologyOperation `json:"operations,omitempty"`
	// Hosts are the hosts each service's spans came from.
	Hosts []TopologyHost `json:"hosts,omitempty"`
	// Reason says why a part is partial; StopReason, Deferred, ResumeAt and Usage are the poll marker's.
	Reason     string     `json:"reason,omitempty"`
	StopReason string     `json:"stop_reason,omitempty"`
	Deferred   []string   `json:"deferred,omitempty"`
	ResumeAt   *time.Time `json:"resume_at,omitempty"`
	Usage      string     `json:"usage,omitempty"`
}

// TopologyWindow is the interval a topology read covered. Its JSON names are `start` and `end`, not
// `from` and `to`: the sanitiser drops a field named `from` or `to` wherever it is, as an email's sender
// and recipient, and a recording that lost its window would date every edge at the poll.
type TopologyWindow struct {
	From time.Time `json:"start"`
	To   time.Time `json:"end"`
}

// TopologyParts is the status of each part of a read.
type TopologyParts struct {
	Dependencies string `json:"dependencies"`
	Traffic      string `json:"traffic,omitempty"`
	Versions     string `json:"versions,omitempty"`
	Operations   string `json:"operations,omitempty"`
	Hosts        string `json:"hosts,omitempty"`
}

func (p TopologyParts) valid() error {
	for name, status := range map[string]string{"dependencies": p.Dependencies, "traffic": p.Traffic,
		"versions": p.Versions, "operations": p.Operations, "hosts": p.Hosts} {
		switch status {
		case PartComplete, PartPartial, PartUnread, "":
		default:
			return fmt.Errorf("datadog: topology part %s has status %q, neither complete, partial nor unread; a "+
				"part whose completeness is unstated cannot be evidence of absence (FR-012)", name, status)
		}
	}
	return nil
}

func (p TopologyParts) allComplete() bool {
	for _, s := range []string{p.Dependencies, p.Traffic, p.Versions, p.Operations, p.Hosts} {
		if s != PartComplete {
			return false
		}
	}
	return true
}

func (p TopologyParts) String() string {
	name := func(s string) string {
		if s == "" {
			return PartUnread
		}
		return s
	}
	return fmt.Sprintf("dependencies %s, traffic %s, versions %s, operations %s, hosts %s", name(p.Dependencies),
		name(p.Traffic), name(p.Versions), name(p.Operations), name(p.Hosts))
}

// TopologyService is a service and the services it calls.
type TopologyService struct {
	Name  string   `json:"name"`
	Calls []string `json:"calls,omitempty"`
}

// TopologyTraffic is a count of retained spans from a caller to a callee.
type TopologyTraffic struct {
	Caller string `json:"caller"`
	Callee string `json:"callee"`
	Hits   int64  `json:"hits"`
}

// TopologyVersion is a version a service reported and the spans that reported it.
type TopologyVersion struct {
	Service string `json:"service"`
	Version string `json:"version"`
	Hits    int64  `json:"hits"`
}

// TopologyOperation is an operation a service emitted and its spans.
type TopologyOperation struct {
	Service   string `json:"service"`
	Operation string `json:"operation"`
	Hits      int64  `json:"hits"`
}

// TopologyHost is a host a service ran on.
type TopologyHost struct {
	Service string `json:"service"`
	Host    string `json:"host"`
}

// ---- what the feeder remembers -------------------------------------------------------------------------

// apmService is what APM states about a service, merged into the node whichever path asserts it.
type apmService struct {
	version   string
	operation string
	// lastDigest and lastID are the last node assertion this path made, so an unchanged node re-sends its id.
	lastDigest string
	lastID     string
	validAt    time.Time
}

// topoEdge is one relation the feeder has asserted and may retract.
type topoEdge struct {
	typ      graphv1.EdgeType
	env      string
	src, dst *graphv1.Ref
	// class is the weight class asserted, or noClass; pending is a different class seen once.
	class       int
	pending     int
	hasPending  bool
	pendingFrom time.Time
	// lastTo is the end of the last window the edge was observed in; missed counts the consecutive
	// complete reads that did not observe it.
	lastTo time.Time
	missed int
}

const noClass = -1

// topoStats is what a window stated rather than emitted, for the next topology checkpoint.
type topoStats struct {
	thirdParty []string
	noTraffic  []string
	retracted  []string
	notes      []string
}

type topoState struct {
	services map[string]*apmService
	// logNodes is the last node assertion the log path made for a source, before any APM decoration.
	logNodes map[string]*graphv1.UpsertNode
	edges    map[string]*topoEdge
	// versions is the set of versions the last complete read reported, per service.
	versions map[string]map[string]bool
	lastTo   map[string]time.Time
	stats    topoStats
	// listed is every service a read has named, per `<env>/<service>`: a target once listed is never a
	// third party because a later, narrower read did not repeat it.
	listed map[string]bool
}

func (f *Feeder) topo() *topoState {
	if f.topology == nil {
		f.topology = &topoState{services: map[string]*apmService{}, logNodes: map[string]*graphv1.UpsertNode{},
			edges: map[string]*topoEdge{}, versions: map[string]map[string]bool{}, lastTo: map[string]time.Time{}, listed: map[string]bool{}}
	}
	return f.topology
}

func (f *Feeder) apmOn() bool { return f.opts.Capabilities.Enabled(CapAPMTopology) }

// ---- the node, shared with the log path ---------------------------------------------------------------

// apmPointers are the pointers APM adds to a service node: its trace metrics (when an operation is
// known) and its spans. Minted only under the capability (FR-008b).
func apmPointers(env, service, operation string) []*graphv1.Pointer {
	attrs := map[string]string{feeder.AttrServiceName: service, feeder.AttrDeploymentEnvironment: env}
	var out []*graphv1.Pointer
	if operation != "" {
		out = append(out, feeder.WithJoinKeys(feeder.NewPointer(graphv1.PointerKind_METRIC, Kind,
			feeder.VocabDatadogAPMMetric, "service:"+service+" env:"+env+" span:"+operation, attrs),
			map[string]string{feeder.JoinRoleVersion: "version", feeder.JoinRoleHost: "host"}))
	}
	return append(out, feeder.WithJoinKeys(feeder.NewPointer(graphv1.PointerKind_TRACE, Kind,
		feeder.VocabDatadogSpans, "service:"+service+" env:"+env, attrs),
		map[string]string{feeder.JoinRoleVersion: "version", feeder.JoinRoleHost: "host", feeder.JoinRoleTrace: "trace_id"}))
}

// decorate adds what APM states to a service node's parts: the version as a property and the APM
// pointers. It returns the parts unchanged when APM has stated nothing about the service, which is every
// service while the capability is off.
func (f *Feeder) decorate(key, env, service string, props *structpb.Struct, pointers []*graphv1.Pointer) (*structpb.Struct, []*graphv1.Pointer) {
	if f.topology == nil {
		return props, pointers
	}
	state := f.topology.services[key]
	if state == nil {
		return props, pointers
	}
	if state.version != "" {
		props = proto.Clone(orEmptyStruct(props)).(*structpb.Struct)
		props.Fields[feeder.AttrServiceVersion] = structpb.NewStringValue(state.version)
	}
	return props, append(append([]*graphv1.Pointer(nil), pointers...), apmPointers(env, service, state.operation)...)
}

func orEmptyStruct(s *structpb.Struct) *structpb.Struct {
	if s == nil {
		return &structpb.Struct{Fields: map[string]*structpb.Value{}}
	}
	if s.Fields == nil {
		s.Fields = map[string]*structpb.Value{}
	}
	return s
}

// noteLogNode remembers the log path's assertion of a source's node, so an APM assertion of the same node
// carries the log pointer and the version-stamp verdict instead of replacing them.
func (f *Feeder) noteLogNode(key string, node *graphv1.UpsertNode) {
	if !f.apmOn() || node == nil {
		return
	}
	f.topo().logNodes[key] = proto.Clone(node).(*graphv1.UpsertNode)
}

// decorateEvent rewrites a plain node event the log path is about to emit, when APM has stated something
// about its service. The id follows the content, so the changed node is a new assertion and not a no-op.
func (f *Feeder) decorateEvent(ev *graphv1.EventEnvelope, key, env, service string) {
	node := ev.GetUpsertNode()
	if node == nil || f.topology == nil || f.topology.services[key] == nil {
		return
	}
	node.Props, node.Pointers = f.decorate(key, env, service, node.Props, node.Pointers)
	digest, err := nodeDigest(node)
	if err != nil {
		return
	}
	ev.EventId += "@apm" + digest
	ev.IdempotencyKey = ev.EventId
}

func nodeDigest(node *graphv1.UpsertNode) (string, error) {
	c := proto.Clone(node).(*graphv1.UpsertNode)
	c.ValidAt, c.ValidFromUnknown = nil, false
	return factDigest(feeder.NodeFact{Ref: c.Ref, Type: c.Type, DisplayName: c.DisplayName, Props: c.Props, Pointers: c.Pointers})
}

// emitAPMNode asserts a service's node from the log path's last assertion (or the plain node), decorated.
func (f *Feeder) emitAPMNode(ctx context.Context, em feeder.Emitter, env, service string, w TopologyWindow, at time.Time) error {
	key := env + "/" + service
	state := f.topo().services[key]
	base := f.topo().logNodes[key]
	var props *structpb.Struct
	var pointers []*graphv1.Pointer
	if base != nil {
		props, pointers = base.Props, base.Pointers
	} else {
		built, err := feeder.NewProps().Str(feeder.AttrServiceName, service).Str(feeder.AttrDeploymentEnvironment, env).Build()
		if err != nil {
			return err
		}
		props = built
	}
	props, pointers = f.decorate(key, env, service, props, pointers)
	fact := feeder.NodeFact{Meta: feeder.Meta{SourceObservedAt: at}, Ref: feeder.Ref(NSService, key),
		Type: graphv1.NodeType_SERVICE, DisplayName: service, Props: props, Pointers: pointers}
	digest, err := factDigest(fact)
	if err != nil {
		return err
	}
	id := state.lastID
	switch {
	case state.lastDigest == "":
		// Dated by the window APM first reported the service in: an instant Datadog states, and not the
		// poll's. Datadog does not say since when the service has existed, so this is a bound and the
		// checkpoint says so (FR-017); it is also what lets the edges of that window, valid from its
		// start, find their endpoints.
		state.validAt = w.From
		id = feeder.NewID(f.desc.SourceID, "apm-service", key+"@"+digest)
	case state.lastDigest != digest:
		state.validAt = w.From
		id = feeder.NewID(f.desc.SourceID, "apm-service", key+"@"+digest+"@w"+w.From.UTC().Format(time.RFC3339))
	}
	fact.ValidAt = state.validAt
	state.lastDigest, state.lastID = digest, id
	return emit(ctx, em, feeder.UpsertNode(f.desc, id, fact))
}

// ---- applying a window ----------------------------------------------------------------------------------

func edgeKeyOf(typ graphv1.EdgeType, env, src, dst string) string {
	return typ.String() + "|" + env + "|" + src + "|" + dst
}

func (f *Feeder) applyTopology(ctx context.Context, em feeder.Emitter, raw []byte, at time.Time) error {
	if !f.apmOn() {
		return fmt.Errorf("datadog: a %s payload arrived with the apm_topology capability off; replaying it "+
			"would make the capability decorative (FR-008b)", PayloadTopology)
	}
	var p TopologyPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return fmt.Errorf("datadog: decoding a %s payload: %w", PayloadTopology, err)
	}
	if err := p.Parts.valid(); err != nil {
		return err
	}
	if !f.opts.Topology.has(p.Env) {
		return fmt.Errorf("datadog: a %s payload for environment %q, which the scope (%s) does not name",
			PayloadTopology, p.Env, f.opts.Topology)
	}
	t := f.topo()
	if p.Window.From.IsZero() || p.Window.To.IsZero() || !p.Window.To.After(p.Window.From) {
		return fmt.Errorf("datadog: a %s payload with no usable window; an edge's valid interval is the window "+
			"it was observed in (FR-011)", PayloadTopology)
	}

	// The services this window names, from every part that lists them.
	calls := map[string][]string{}
	listed := map[string]bool{}
	for _, s := range p.Services {
		listed[s.Name] = true
		calls[s.Name] = append(calls[s.Name], s.Calls...)
	}
	for _, v := range p.Versions {
		listed[v.Service] = true
	}
	for _, o := range p.Operations {
		listed[o.Service] = true
	}
	names := make([]string, 0, len(listed))
	for name := range listed {
		if name != "" && !strings.ContainsAny(name, "/ ") {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	if len(names) > MaxTopologyServices {
		t.stats.notes = append(t.stats.notes, fmt.Sprintf("%s: %d services reported and the first %d asserted; the "+
			"rest are unread, not absent", p.Env, len(names), MaxTopologyServices))
		names = names[:MaxTopologyServices]
	}
	inScope := map[string]bool{}
	for _, n := range names {
		inScope[n] = true
	}

	// What APM states about each service: its dominant version and its busiest operation.
	oldVersion := map[string]string{}
	for _, name := range names {
		if state := t.services[p.Env+"/"+name]; state != nil {
			oldVersion[name] = state.version
		}
	}
	version, versionSet := dominantVersions(p.Versions, inScope)
	operation := busiestOperations(p.Operations, inScope)
	for _, name := range names {
		key := p.Env + "/" + name
		state := t.services[key]
		if state == nil {
			state = &apmService{}
			t.services[key] = state
		}
		state.version, state.operation = version[name], operation[name]
	}
	// Rollouts are decided before the state moves on: a version not in the previous complete read.
	if err := f.emitRollouts2(ctx, em, p, names, oldVersion, versionSet, at); err != nil {
		return err
	}
	for _, name := range names {
		if err := f.emitAPMNode(ctx, em, p.Env, name, p.Window, at); err != nil {
			return err
		}
	}

	// Third parties: a target Datadog reports and does not list, and that no watched source names.
	targets := map[string]bool{}
	for _, name := range names {
		for _, callee := range calls[name] {
			targets[callee] = true
		}
	}
	targetNames := make([]string, 0, len(targets))
	for callee := range targets {
		targetNames = append(targetNames, callee)
	}
	sort.Strings(targetNames)
	for _, name := range names {
		t.listed[p.Env+"/"+name] = true
	}
	for _, callee := range targetNames {
		if callee == "" || strings.ContainsAny(callee, "/ ") {
			continue
		}
		key := p.Env + "/" + callee
		// Only a complete dependencies read can say a target is unlisted: on a partial one the service is
		// as likely to be in the part that was not read.
		if p.Parts.Dependencies != PartComplete || inScope[callee] || t.listed[key] || f.watched(key) {
			continue
		}
		props, err := feeder.NewProps().Str(feeder.AttrServiceName, callee).Str(feeder.AttrDeploymentEnvironment, p.Env).
			Bool(PropUnlistedDependency, true).Build()
		if err != nil {
			return err
		}
		if err := emit(ctx, em, feeder.UpsertNode(f.desc, feeder.NewID(f.desc.SourceID, "apm-dependency", key),
			feeder.NodeFact{Meta: feeder.Meta{SourceObservedAt: at}, Ref: feeder.Ref(NSService, key),
				Type: graphv1.NodeType_THIRD_PARTY, DisplayName: callee, Props: props, ValidAt: p.Window.From})); err != nil {
			return err
		}
		t.stats.thirdParty = append(t.stats.thirdParty, fmt.Sprintf("%s: reported as called and not listed as a "+
			"service; a third party until a service listing promotes it (FR-015)", key))
	}

	if err := f.emitCallEdges(ctx, em, p, names, calls, at); err != nil {
		return err
	}
	if err := f.emitHostEdges(ctx, em, p, names, at); err != nil {
		return err
	}
	if err := f.retractUnobserved(ctx, em, p, at); err != nil {
		return err
	}
	return f.topologyCheckpoint(ctx, em, p, at)
}

// watched reports whether a `<env>/<service>` is a watched log source, whose node is the log path's.
func (f *Feeder) watched(key string) bool {
	if _, ok := f.topo().logNodes[key]; ok {
		return true
	}
	for _, src := range f.opts.LogSources {
		if src.Key() == key {
			return true
		}
	}
	return false
}

// dominantVersions picks each service's busiest reported version (ties to the greater string) and
// returns every version each reported.
func dominantVersions(versions []TopologyVersion, in map[string]bool) (dominant map[string]string, all map[string]map[string]bool) {
	dominant, all = map[string]string{}, map[string]map[string]bool{}
	best := map[string]int64{}
	for _, v := range versions {
		if !in[v.Service] || v.Version == "" {
			continue
		}
		if all[v.Service] == nil {
			all[v.Service] = map[string]bool{}
		}
		all[v.Service][v.Version] = true
		if v.Hits > best[v.Service] || (v.Hits == best[v.Service] && v.Version > dominant[v.Service]) {
			best[v.Service], dominant[v.Service] = v.Hits, v.Version
		}
	}
	return dominant, all
}

func busiestOperations(ops []TopologyOperation, in map[string]bool) map[string]string {
	out, best := map[string]string{}, map[string]int64{}
	for _, o := range ops {
		if !in[o.Service] || o.Operation == "" {
			continue
		}
		if o.Hits > best[o.Service] || (o.Hits == best[o.Service] && o.Operation < out[o.Service]) {
			best[o.Service], out[o.Service] = o.Hits, o.Operation
		}
	}
	return out
}

// emitRollouts2 emits a ROLLOUT for each version a service reports that the previous complete read did
// not, when this read's versions are complete. The first read of a service is a baseline, not a change.
func (f *Feeder) emitRollouts2(ctx context.Context, em feeder.Emitter, p TopologyPayload, names []string, oldVersion map[string]string, current map[string]map[string]bool, at time.Time) error {
	if p.Parts.Versions != PartComplete {
		return nil
	}
	t := f.topo()
	for _, name := range names {
		key := p.Env + "/" + name
		previous, seen := t.versions[key]
		now := current[name]
		if seen {
			fresh := make([]string, 0, len(now))
			for v := range now {
				if !previous[v] {
					fresh = append(fresh, v)
				}
			}
			sort.Strings(fresh)
			old := oldVersion[name]
			if old != "" && !previous[old] {
				old = ""
			}
			for _, v := range fresh {
				if err := f.emitAPMRollout(ctx, em, p, name, old, v, at); err != nil {
					return err
				}
			}
			if len(fresh) > 1 {
				t.stats.notes = append(t.stats.notes, fmt.Sprintf("%s: %d versions first reported in one window: a "+
					"canary or a rolling deploy; each is one rollout", key, len(fresh)))
			}
		}
		if now == nil {
			now = map[string]bool{}
		}
		t.versions[key] = now
	}
	return nil
}

func (f *Feeder) emitAPMRollout(ctx context.Context, em feeder.Emitter, p TopologyPayload, service, old, version string, at time.Time) error {
	key := p.Env + "/" + service
	value := "apm/" + key + "@" + version + "@" + p.Window.From.UTC().Format(time.RFC3339Nano)
	ref := feeder.Ref(NSChange, value)
	builder := feeder.NewProps().
		Str(feeder.PropChangeValidFromIsABound, BoundFirstSeenInAPM).
		Str(PropChangeVersionValue, version).
		Str(feeder.AttrDeploymentEnvironment, p.Env).
		Str(PropActorEvidence, "Datadog APM states the version a service reports and no actor; deployment tracking's "+
			"origin is not read, so the actor is unclassified")
	if old != "" {
		builder.Str(PropPreviousVersion, old)
	}
	props, err := builder.Build()
	if err != nil {
		return err
	}
	summary := fmt.Sprintf("version %s first reported by APM for %s (%s)", version, service, p.Env)
	if old != "" {
		summary = fmt.Sprintf("version %s → %s reported by APM for %s (%s)", old, version, service, p.Env)
	}
	if err := emit(ctx, em, feeder.ObserveChange(f.desc, feeder.NewID(f.desc.SourceID, "change", value), feeder.ChangeFact{
		Meta: feeder.Meta{SourceObservedAt: at}, Ref: ref, Kind: graphv1.ChangeKind_ROLLOUT, Summary: summary,
		// UNKNOWN, not unspecified: a service that ran one version now runs another, so something deployed it
		// (FR-033a); APM carries no field that says who (FR-033b).
		ActorKind: graphv1.ActorKind_ACTOR_KIND_UNKNOWN,
		Targets:   []*graphv1.Ref{feeder.Ref(NSService, key)}, ValidAt: p.Window.From, Props: props,
	})); err != nil {
		return err
	}
	deploy, _ := versionstamp.Normalise(version)
	if deploy == nil {
		return nil
	}
	attrs, err := feeder.NewProps().Str(AttrEnvironment, p.Env).Build()
	if err != nil {
		return err
	}
	return emit(ctx, em, feeder.Correlate(f.desc,
		feeder.NewID(f.desc.SourceID, "correlation", value, deploy.GetNamespace(), deploy.GetValue()),
		feeder.CorrelationFact{Meta: feeder.Meta{SourceObservedAt: at}, Subject: ref, Key: deploy, Attributes: attrs}))
}

// emitCallEdges asserts the `calls` edges of the window, and records that each was observed.
func (f *Feeder) emitCallEdges(ctx context.Context, em feeder.Emitter, p TopologyPayload, names []string, calls map[string][]string, at time.Time) error {
	t := f.topo()
	seconds := p.Window.To.Sub(p.Window.From)
	hits := map[[2]string]int64{}
	// A traffic part that did not finish says nothing about a class: an existing edge keeps the one it has,
	// and a new edge carries none, rather than either being judged on counts that were not all read.
	trafficRead := p.Parts.Traffic == PartComplete
	if trafficRead {
		for _, tr := range p.Traffic {
			hits[[2]string{tr.Caller, tr.Callee}] += tr.Hits
		}
	}
	for _, name := range names {
		targets := append([]string(nil), calls[name]...)
		sort.Strings(targets)
		var prev string
		for _, callee := range targets {
			if callee == "" || callee == prev || strings.ContainsAny(callee, "/ ") {
				continue
			}
			prev = callee
			key := edgeKeyOf(graphv1.EdgeType_CALLS, p.Env, name, callee)
			class := noClass
			if n, ok := hits[[2]string{name, callee}]; ok {
				class = int(feeder.WeightClassFromCount(int(n), seconds))
			} else if trafficRead {
				t.stats.noTraffic = append(t.stats.noTraffic, fmt.Sprintf("%s/%s -> %s: no retained span in the "+
					"window, so the edge carries no weight class", p.Env, name, callee))
			}
			e := t.edges[key]
			switch {
			case e == nil:
				e = &topoEdge{typ: graphv1.EdgeType_CALLS, env: p.Env, src: feeder.Ref(NSService, p.Env+"/"+name),
					dst: feeder.Ref(NSService, p.Env+"/"+callee)}
				// A new call path is news: emitted at once, valid from the window it was first observed in.
				t.edges[key] = e
				if err := f.assertCall(ctx, em, e, class, p.Window.From, p, at); err != nil {
					return err
				}
			case !trafficRead:
				// The class asserted stands.
			case class == e.class:
				e.hasPending = false
			case e.hasPending && e.pending == class:
				// A second window at the new class confirms it; it became true when first seen.
				if err := f.assertCall(ctx, em, e, class, e.pendingFrom, p, at); err != nil {
					return err
				}
			default:
				e.hasPending, e.pending, e.pendingFrom = true, class, p.Window.From
			}
			e.lastTo, e.missed = p.Window.To, 0
		}
	}
	return nil
}

func (f *Feeder) assertCall(ctx context.Context, em feeder.Emitter, e *topoEdge, class int, from time.Time, p TopologyPayload, at time.Time) error {
	e.class, e.hasPending = class, false
	builder := feeder.NewProps().Int(feeder.PropWindowSeconds, int64(p.Window.To.Sub(p.Window.From)/time.Second))
	fact := feeder.EdgeFact{Meta: feeder.Meta{SourceObservedAt: at}, Src: e.src, Dst: e.dst, Type: graphv1.EdgeType_CALLS,
		ValidAt: from}
	if class != noClass {
		builder.Str(PropWeightBasis, WeightBasisRetainedSpans)
		fact.WeightClass = feeder.WeightClassPtr(uint32(class)) //nolint:gosec // class is 0..5 by construction
	}
	props, err := builder.Build()
	if err != nil {
		return err
	}
	fact.Props = props
	id := feeder.NewID(f.desc.SourceID, "apm-edge", e.src.GetValue()+"->"+e.dst.GetValue()+"@w"+from.UTC().Format(time.RFC3339))
	return emit(ctx, em, feeder.UpsertEdge(f.desc, id, fact))
}

// emitHostEdges asserts the hosts of the window: INFRA_RESOURCE nodes and `runs-on` edges, never a
// container or a pod (FR-014).
func (f *Feeder) emitHostEdges(ctx context.Context, em feeder.Emitter, p TopologyPayload, names []string, at time.Time) error {
	t := f.topo()
	in := map[string]bool{}
	for _, n := range names {
		in[n] = true
	}
	byService := map[string][]string{}
	for _, h := range p.Hosts {
		if in[h.Service] && h.Host != "" && !strings.ContainsAny(h.Host, "/ ") {
			byService[h.Service] = append(byService[h.Service], h.Host)
		}
	}
	for _, name := range names {
		hosts := uniqueSorted(byService[name])
		if len(hosts) > MaxHostsPerService {
			t.stats.notes = append(t.stats.notes, fmt.Sprintf("%s/%s: %d hosts reported and the first %d asserted", p.Env,
				name, len(hosts), MaxHostsPerService))
			hosts = hosts[:MaxHostsPerService]
		}
		for _, host := range hosts {
			hostRef := feeder.Ref(NSHost, host)
			props, err := feeder.NewProps().Str("host.name", host).Build()
			if err != nil {
				return err
			}
			if err := emit(ctx, em, feeder.UpsertNode(f.desc, feeder.NewID(f.desc.SourceID, "apm-host", host), feeder.NodeFact{
				Meta: feeder.Meta{SourceObservedAt: at}, Ref: hostRef, Type: graphv1.NodeType_INFRA_RESOURCE,
				DisplayName: host, Props: props, ValidAt: p.Window.From})); err != nil {
				return err
			}
			key := edgeKeyOf(graphv1.EdgeType_RUNS_ON, p.Env, name, host)
			e := t.edges[key]
			if e == nil {
				e = &topoEdge{typ: graphv1.EdgeType_RUNS_ON, env: p.Env, src: feeder.Ref(NSService, p.Env+"/"+name), dst: hostRef}
				t.edges[key] = e
				// Datadog states the host a span came from, not since when the service has run there:
				// the start is unknown (FR-017).
				id := feeder.NewID(f.desc.SourceID, "apm-runs-on", p.Env+"/"+name+"->"+host+"@w"+p.Window.From.UTC().Format(time.RFC3339))
				if err := emit(ctx, em, feeder.UpsertEdge(f.desc, id, feeder.EdgeFact{
					Meta: feeder.Meta{SourceObservedAt: at}, Src: e.src, Dst: e.dst, Type: graphv1.EdgeType_RUNS_ON,
					ValidFromUnknown: true})); err != nil {
					return err
				}
			}
			e.lastTo, e.missed = p.Window.To, 0
		}
	}
	return nil
}

// retractUnobserved ends the edges a COMPLETE read did not observe often enough. An edge whose part was
// partial or unread is left as it was: an unread page is not a deletion (FR-012).
func (f *Feeder) retractUnobserved(ctx context.Context, em feeder.Emitter, p TopologyPayload, at time.Time) error {
	t := f.topo()
	keys := make([]string, 0, len(t.edges))
	for k := range t.edges {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		e := t.edges[k]
		if e.env != p.Env || e.lastTo.Equal(p.Window.To) {
			continue // another environment, or observed in this very window
		}
		part := p.Parts.Dependencies
		if e.typ == graphv1.EdgeType_RUNS_ON {
			part = p.Parts.Hosts
		}
		if part != PartComplete {
			continue
		}
		e.missed++
		if e.missed < f.opts.Topology.retractAfter() {
			continue
		}
		delete(t.edges, k)
		t.stats.retracted = append(t.stats.retracted, fmt.Sprintf("%s %s -> %s: unobserved in %d consecutive complete "+
			"reads; ended at the end of the last window it was seen in, %s", e.typ, e.src.GetValue(), e.dst.GetValue(),
			e.missed, e.lastTo.UTC().Format(time.RFC3339)))
		id := feeder.NewID(f.desc.SourceID, "apm-retract", e.typ.String()+"/"+e.src.GetValue()+"->"+e.dst.GetValue()+
			"@"+e.lastTo.UTC().Format(time.RFC3339))
		ev := feeder.RetractEdge(f.desc, id, feeder.EdgeRetraction{Meta: feeder.Meta{SourceObservedAt: at},
			Src: e.src, Dst: e.dst, Type: e.typ, ValidEnd: e.lastTo})
		if err := emit(ctx, em, ev); err != nil {
			return err
		}
	}
	return nil
}

// topologyCheckpoint states the extent a window covered and whether the feeder was watching just before
// it (FR-016). Only a read whose every part completed vouches for its interval; a partial one declares the
// gap, and the next window reaches back from the last complete one.
func (f *Feeder) topologyCheckpoint(ctx context.Context, em feeder.Emitter, p TopologyPayload, at time.Time) error {
	t := f.topo()
	complete := p.Parts.allComplete()
	last := t.lastTo[p.Env]
	from, to := p.Window.From, p.Window.To
	gap := last.IsZero() || p.Window.From.After(last)
	if !complete {
		from, to, gap = at, at, true
	} else {
		t.lastTo[p.Env] = p.Window.To
	}
	lines := []string{
		"datadog topology read: " + map[bool]string{true: "complete", false: "partial"}[complete],
		"capabilities: " + f.opts.Capabilities.String(),
		"site: " + orUnset(f.opts.Site),
		"read-only: " + readOnlyStatement(f.opts.OperatorAsserted),
		"topology scope in force: " + f.opts.Topology.String(),
		fmt.Sprintf("environment: %s; window: %s to %s", p.Env, p.Window.From.UTC().Format(time.RFC3339),
			p.Window.To.UTC().Format(time.RFC3339)),
		"parts: " + p.Parts.String(),
		"weight classes are derived from counts of retained spans, a lower bound of the traffic; a count is never stored",
		"hosts only: no container or pod is read or asserted",
	}
	if last.IsZero() {
		lines = append(lines, "first read of this environment in this run: the feeder was not watching before it")
	} else if p.Window.From.After(last) {
		lines = append(lines, fmt.Sprintf("gap before this window: the previous complete read ended %s",
			last.UTC().Format(time.RFC3339)))
	}
	if !complete {
		lines = append(lines, "partial: "+orUnset(p.Reason)+"; nothing unread was retracted and no rollout was inferred from it")
	}
	if p.StopReason != "" {
		stop := "stopped: " + p.StopReason
		if p.ResumeAt != nil {
			stop += "; reading again from " + p.ResumeAt.UTC().Format(time.RFC3339) + ", as Datadog asked"
		}
		lines = append(lines, stop)
	}
	if len(p.Deferred) > 0 {
		lines = append(lines, "deferred for quota, in the published order: "+strings.Join(p.Deferred, ", ")+
			"; what they would have read is unread, not absent")
	}
	if p.Usage != "" {
		lines = append(lines, "usage: "+p.Usage)
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
	add("third-party dependencies (FR-015)", t.stats.thirdParty)
	add("edges without a weight class", t.stats.noTraffic)
	add("retracted (FR-011)", t.stats.retracted)
	add("notes", t.stats.notes)
	t.stats = topoStats{}
	return em.Checkpoint(ctx, feeder.CheckpointFact{ExtentFrom: from, ExtentTo: to, GapBefore: gap, Note: strings.Join(lines, "\n")})
}
