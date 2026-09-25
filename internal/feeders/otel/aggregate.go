// SPDX-License-Identifier: Apache-2.0

package otel

import (
	"log/slog"
	"math"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// Window aggregation (T051, FR-042, research §8, §11).
//
// One window holds three maps and no spans: the services that produced telemetry, the callees
// they were seen talking to, and a weight per (service, callee) pair. A span is read into
// those counters and dropped; at window close the counters become events and the window itself
// is freed. Nothing here ever retains a trace id, a span id, a span name or a duration — the
// only attributes that survive the read are the ones named in allowedResourceAttrs and
// allowedSpanAttrs, and only those may reach a property (constitution IV, FR-009).
//
// Two things make the result independent of delivery order, which testkit.Shuffle checks:
//
//   - A span's window comes from the span's own timestamp, never from when the payload
//     carrying it arrived.
//   - A window closes when the watermark — the newest payload arrival time seen — has passed
//     the window's end by a full lateness allowance, which the feeder sets to the aggregation
//     window. A permutation of the payloads inside that allowance therefore cannot make a span
//     arrive after its own window has closed.
//
// Where two spans of one window disagree about an attribute (two versions during a rollout,
// two ports for one address), the greatest value in string order wins. That rule is arbitrary
// but it is not order-dependent, which is the property that matters; a rollout spanning a
// window boundary is reported from the window where the new version stands alone.

// Defaults published in contracts/cli.md.
const (
	// DefaultWindow is the aggregation window, aligned to the wall clock.
	DefaultWindow = 5 * time.Minute
	// DefaultRetractAfter is how many windows an edge may go unobserved before it is
	// retracted (research §11: 6 windows of 5 minutes = 30 minutes).
	DefaultRetractAfter = 6
)

// Span attribute keys that are read but never stored. They are not in pkg/feeder's constants
// because nothing else in the system writes them.
const (
	// attrPeerService is `peer.service`, the callee's telemetry service name.
	attrPeerService = "peer.service"
	// attrSamplingProbability is the head-sampling probability a span was kept with. A span
	// carrying it stands for 1/p spans, which is what makes a sampled pipeline land in the
	// same weight class as an unsampled one (research §11).
	attrSamplingProbability = "sampling.probability"
)

// allowedResourceAttrs is every resource attribute this feeder reads. A resource attribute
// outside this list cannot reach a property, a selector or a claim.
var allowedResourceAttrs = []string{
	feeder.AttrServiceName,
	feeder.AttrServiceNamespace,
	feeder.AttrServiceVersion,
	feeder.AttrDeploymentEnvironment,
	feeder.AttrK8sDeploymentName,
	feeder.AttrK8sNamespaceName,
	// The GCP resource detector's four, which carry the Cloud Run revision an observed service is
	// running in (003 FR-117, T182). They are read for the same reason the Kubernetes pair is: so the
	// claim can carry them and a published *certain* rule can pair the two sides. Reading them is not
	// a GCP dependency — they are ordinary OpenTelemetry resource attributes, absent on a process that
	// is not running in Cloud Run, and absent is the ordinary case.
	feeder.AttrCloudPlatform,
	feeder.AttrFaaSVersion,
	feeder.AttrCloudRegion,
	feeder.AttrCloudAccountID,
}

// CloudRunPlatform is the `cloud.platform` value the GCP detector sets on Cloud Run.
//
// Everything below is gated on it, and the gate is not defensiveness: `faas.version` is a revision
// name on Cloud Run and an alias on AWS Lambda, so a claim that carried the version without the
// platform would let a certain rule pair a Lambda alias with a Cloud Run revision of the same name —
// and merge two unrelated entities with no human in the loop.
const CloudRunPlatform = "gcp_cloud_run"

// CloudSQLSocketPrefix is where the Cloud SQL connectors — the Auth Proxy, the Cloud Run and Cloud
// Functions built-in connector, the GKE sidecar — mount the instance's Unix socket.
//
// The path is `/cloudsql/<project>:<region>:<instance>`, optionally with the driver's socket file
// under it (`/.s.PGSQL.5432`). A client reaching the database that way has the **instance connection
// name inside its own server address**, which is what makes the observed side of 003's C7 rule
// available at all: the caller is not resembling the instance, it is quoting the exact string its
// configuration was given.
const CloudSQLSocketPrefix = "/cloudsql/"

// cloudSQLConnectionName reads the instance connection name out of a Cloud SQL socket address,
// reporting false for an address that is not one.
//
// **It is deliberately not gated on `cloud.platform`,** and that is a different decision from the
// Cloud Run revision attributes above rather than an inconsistency. That gate exists because
// `faas.version` is a revision on Cloud Run and an alias on Lambda — the same attribute means
// different things in different places, so the platform is what disambiguates it. This path has no
// such ambiguity: `/cloudsql/` is a Cloud SQL mount wherever it appears, and the string under it is
// globally unique by construction. Gating on Cloud Run would instead make the rule miss GKE and
// Compute Engine, which is where the Auth Proxy is most used — a gate that only loses true matches.
//
// The shape is the evidence, so the shape is checked strictly: exactly three colon-separated,
// non-empty, whitespace-free parts. A lenient parse here would let a malformed address merge
// unrelated instances under a *certain* rule with no human in the loop.
func cloudSQLConnectionName(address string) (string, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(address), CloudSQLSocketPrefix)
	if !ok {
		return "", false
	}
	// Drop the driver's socket file, which sits under the instance directory.
	if slash := strings.IndexByte(rest, '/'); slash >= 0 {
		rest = rest[:slash]
	}
	parts := strings.Split(rest, ":")
	if len(parts) != 3 {
		return "", false
	}
	for _, part := range parts {
		if part == "" || strings.ContainsFunc(part, unicode.IsSpace) {
			return "", false
		}
	}
	return rest, true
}

// allowedSpanAttrs is every span attribute this feeder reads. Nothing outside it — not a URL,
// not a status message, not an exception stack — is looked at, let alone kept.
var allowedSpanAttrs = []string{
	attrPeerService,
	attrSamplingProbability,
	feeder.AttrDBSystem,
	feeder.AttrDBSystemName,
	feeder.AttrServerAddress,
	feeder.AttrServerPort,
	feeder.AttrURLScheme,
}

// serviceAgg is one telemetry service as a window saw it.
//
// It is keyed by `service.name` alone, because that is what the `otel.service.name` namespace
// is valued by: two services of that name in different namespaces are one identity to the
// graph, and inventing a compound value here would mint identifiers no other connector could
// meet (ref.go). The namespace and the environment are carried as attributes, which is what a
// resolution rule reads.
type serviceAgg struct {
	name          string
	namespace     string
	env           string
	version       string
	k8sDeployment string
	k8sNamespace  string
	// The Cloud Run coordinates, from the GCP resource detector, where the process is running in
	// Cloud Run and states them. All three are needed together or none is useful: a revision name is
	// unique within a service and not globally (003 FR-117).
	cloudRunRevision string
	cloudRunProject  string
	cloudRunRegion   string
}

// calleeKey is the identity of the far end of a call: the namespace and value of its ref.
type calleeKey struct {
	ns    string
	value string
}

// calleeAgg is one callee as a window saw it. `short` is the name it is displayed and
// identified by; see shortHostName for how an address becomes one.
type calleeAgg struct {
	key      calleeKey
	short    string
	dbSystem string
	address  string
	scheme   string
	env      string
	port     int64
	// external marks a callee that is a dependency rather than a service of ours: it was
	// identified by an address or a database system, so it gets a THIRD_PARTY node unless the
	// same window also saw it produce spans of its own.
	external bool
}

// edgeKey is one directed call path inside a window.
type edgeKey struct {
	src string
	dst calleeKey
}

// edgeAgg counts one call path. The weight is a float because a sampled span stands for 1/p
// spans; it is rounded once, at the end, into the count a weight class is derived from.
type edgeAgg struct {
	key    edgeKey
	weight float64
}

// windowAgg is everything one aggregation window knows. The insertion orders are kept so that
// a replay emits in the order the payloads introduced things, which is what a recorded
// events.jsonl compares against; the *set* of events does not depend on them.
type windowAgg struct {
	start  time.Time
	length time.Duration

	services     map[string]*serviceAgg
	serviceOrder []string

	callees     map[calleeKey]*calleeAgg
	calleeOrder []calleeKey

	edges     map[edgeKey]*edgeAgg
	edgeOrder []edgeKey
}

func newWindowAgg(start time.Time, length time.Duration) *windowAgg {
	return &windowAgg{
		start:    start,
		length:   length,
		services: map[string]*serviceAgg{},
		callees:  map[calleeKey]*calleeAgg{},
		edges:    map[edgeKey]*edgeAgg{},
	}
}

// end is the exclusive upper bound of the window.
func (w *windowAgg) end() time.Time { return w.start.Add(w.length) }

// service returns the aggregate for name, creating it in first-seen order.
func (w *windowAgg) service(name string) *serviceAgg {
	if svc, ok := w.services[name]; ok {
		return svc
	}
	svc := &serviceAgg{name: name}
	w.services[name] = svc
	w.serviceOrder = append(w.serviceOrder, name)
	return svc
}

// callee returns the aggregate for key, creating it in first-seen order.
func (w *windowAgg) callee(key calleeKey) *calleeAgg {
	if c, ok := w.callees[key]; ok {
		return c
	}
	c := &calleeAgg{key: key}
	w.callees[key] = c
	w.calleeOrder = append(w.calleeOrder, key)
	return c
}

// edge returns the aggregate for key, creating it in first-seen order.
func (w *windowAgg) edge(key edgeKey) *edgeAgg {
	if e, ok := w.edges[key]; ok {
		return e
	}
	e := &edgeAgg{key: key}
	w.edges[key] = e
	w.edgeOrder = append(w.edgeOrder, key)
	return e
}

// count is the span count an edge's weight stands for, which is what a weight class is derived
// from. It is rounded once so that a sampled count of 3599.9999 is 3600.
func (e *edgeAgg) count() int {
	if e.weight <= 0 {
		return 0
	}
	return int(math.Round(e.weight))
}

// AggregatorStats is what the aggregator has seen. It is logged rather than stored: a counter
// about the feeder is not a fact about the production system.
type AggregatorStats struct {
	// Spans is how many spans were read.
	Spans int64
	// LateSpans is how many arrived for a window that had already closed and were dropped.
	LateSpans int64
	// SkippedResources is how many resource groups carried no `service.name` and could not be
	// attributed to anything.
	SkippedResources int64
	// Windows is how many windows have been closed.
	Windows int64
}

// Aggregator turns OTLP trace exports into per-window counters.
//
// It is not safe for concurrent use: one feeder owns one aggregator and feeds it from its own
// loop, which is also what makes the result reproducible.
type Aggregator struct {
	window   time.Duration
	lateness time.Duration
	log      *slog.Logger

	open map[int64]*windowAgg
	// watermark is the newest payload arrival time seen. It is the feeder's clock in both
	// modes: live it is now, replaying it is what the recording says (FR-044).
	watermark time.Time
	// closedThrough is the end of the newest window that has been closed. A span older than
	// it has missed its window.
	closedThrough time.Time

	stats AggregatorStats
}

// NewAggregator returns an aggregator over windows of the given length.
//
// lateness is how long after a window's end the window is held open for stragglers. It is an
// input property — how late an exporter's payloads may be — and the feeder passes its own
// aggregation window: held for less, a late export could lose its spans; held for more,
// nothing is lost and events are merely emitted later. It is deliberately not the window the
// feeder *declares* in Describe, which is a promise about this feeder's own event order and is
// much smaller (Feeder.DeclaredReorderingWindow).
func NewAggregator(window, lateness time.Duration, log *slog.Logger) *Aggregator {
	if window <= 0 {
		window = DefaultWindow
	}
	if lateness < 0 {
		lateness = 0
	}
	if log == nil {
		log = slog.Default()
	}
	return &Aggregator{window: window, lateness: lateness, log: log, open: map[int64]*windowAgg{}}
}

// Window is the aggregation window length.
func (a *Aggregator) Window() time.Duration { return a.window }

// Stats is what the aggregator has seen so far.
func (a *Aggregator) Stats() AggregatorStats { return a.stats }

// WindowStart is the start of the window t belongs to, aligned to the wall clock.
func (a *Aggregator) WindowStart(t time.Time) time.Time {
	return t.UTC().Truncate(a.window)
}

// Add reads one OTLP export request into the open windows and advances the watermark to at.
//
// Every span is attributed to the window its own timestamp falls in, so a request carrying
// spans either side of a boundary is split rather than being assigned to whichever window the
// request arrived in.
func (a *Aggregator) Add(req *coltracepb.ExportTraceServiceRequest, at time.Time) {
	a.Advance(at)
	for _, rs := range req.GetResourceSpans() {
		a.addResourceSpans(rs, at)
	}
}

// Advance moves the watermark forward. A heartbeat in live mode calls it with the wall clock
// so that a quiet window still closes; a replay never needs it, because every payload carries
// its own arrival time.
func (a *Aggregator) Advance(at time.Time) {
	if at.IsZero() {
		return
	}
	at = at.UTC()
	if at.After(a.watermark) {
		a.watermark = at
	}
}

func (a *Aggregator) addResourceSpans(rs *tracepb.ResourceSpans, at time.Time) {
	res := readAttrs(rs.GetResource().GetAttributes(), allowedResourceAttrs)
	name := res[feeder.AttrServiceName]
	if name == "" {
		a.stats.SkippedResources++
		a.log.Debug("otel: resource spans without service.name dropped",
			"scope_spans", len(rs.GetScopeSpans()))
		return
	}
	for _, ss := range rs.GetScopeSpans() {
		for _, span := range ss.GetSpans() {
			a.addSpan(res, name, span, at)
		}
	}
}

func (a *Aggregator) addSpan(res map[string]string, name string, span *tracepb.Span, at time.Time) {
	a.stats.Spans++

	w := a.windowFor(spanTime(span, at))
	if w == nil {
		a.stats.LateSpans++
		return
	}

	svc := w.service(name)
	svc.namespace = keepMax(svc.namespace, res[feeder.AttrServiceNamespace])
	svc.env = keepMax(svc.env, res[feeder.AttrDeploymentEnvironment])
	svc.version = keepMax(svc.version, res[feeder.AttrServiceVersion])
	svc.k8sDeployment = keepMax(svc.k8sDeployment, res[feeder.AttrK8sDeploymentName])
	svc.k8sNamespace = keepMax(svc.k8sNamespace, res[feeder.AttrK8sNamespaceName])
	// The Cloud Run coordinates, gated on the platform. See CloudRunPlatform: `faas.version` is a
	// revision only on Cloud Run, and taking it unconditionally would let a certain rule merge a
	// Lambda alias with a Cloud Run revision.
	if res[feeder.AttrCloudPlatform] == CloudRunPlatform {
		svc.cloudRunRevision = keepMax(svc.cloudRunRevision, res[feeder.AttrFaaSVersion])
		svc.cloudRunProject = keepMax(svc.cloudRunProject, res[feeder.AttrCloudAccountID])
		svc.cloudRunRegion = keepMax(svc.cloudRunRegion, res[feeder.AttrCloudRegion])
	}

	attrs := readAttrs(span.GetAttributes(), allowedSpanAttrs)
	target, ok := resolveCallee(attrs)
	if !ok {
		return
	}
	target.env = res[feeder.AttrDeploymentEnvironment]

	c := w.callee(target.key)
	c.short = keepMax(c.short, target.short)
	c.dbSystem = keepMax(c.dbSystem, target.dbSystem)
	c.address = keepMax(c.address, target.address)
	c.scheme = keepMax(c.scheme, target.scheme)
	c.env = keepMax(c.env, target.env)
	c.port = max(c.port, target.port)
	c.external = c.external || target.external

	w.edge(edgeKey{src: name, dst: target.key}).weight += spanWeight(attrs)
}

// windowFor returns the open window t belongs to, opening it if needed, or nil when that
// window has already been closed and t is a straggler nothing can be done about.
func (a *Aggregator) windowFor(t time.Time) *windowAgg {
	start := a.WindowStart(t)
	if !a.closedThrough.IsZero() && !start.Add(a.window).After(a.closedThrough) {
		a.log.Warn("otel: span dropped, its window is already closed",
			"window_start", start.Format(time.RFC3339), "closed_through", a.closedThrough.Format(time.RFC3339))
		return nil
	}
	key := start.Unix()
	if w, ok := a.open[key]; ok {
		return w
	}
	w := newWindowAgg(start, a.window)
	a.open[key] = w
	return w
}

// Closed returns every open window the watermark has moved past, oldest first, and forgets
// them. A window is closed only once its end plus the lateness allowance is strictly behind
// the watermark, which is what makes a permutation inside the declared reordering window
// unable to lose a span.
func (a *Aggregator) Closed() []*windowAgg {
	var out []*windowAgg
	for key, w := range a.open {
		if a.watermark.After(w.end().Add(a.lateness)) {
			out = append(out, w)
			delete(a.open, key)
		}
	}
	return a.take(out)
}

// Drain returns every remaining window, oldest first, and empties the aggregator. A replay
// calls it at end of input so the last window is not lost with the process.
func (a *Aggregator) Drain() []*windowAgg {
	out := make([]*windowAgg, 0, len(a.open))
	for key, w := range a.open {
		out = append(out, w)
		delete(a.open, key)
	}
	return a.take(out)
}

// take sorts closed windows into chronological order and records how far the aggregator has
// closed. Emission order across windows is therefore always time order, whatever order the
// payloads arrived in.
func (a *Aggregator) take(windows []*windowAgg) []*windowAgg {
	slices.SortFunc(windows, func(x, y *windowAgg) int { return x.start.Compare(y.start) })
	for _, w := range windows {
		if w.end().After(a.closedThrough) {
			a.closedThrough = w.end()
		}
	}
	a.stats.Windows += int64(len(windows))
	return windows
}

// resolveCallee reads the far end of a call out of a span's attributes (research §11).
//
// The three shapes, in the order they are preferred:
//
//   - `peer.service` names a telemetry service, so the callee is addressed in
//     `otel.service.name`. If the span also carries a database system or an address, the peer
//     is a dependency rather than one of our services — redis is both `peer.service=redis` and
//     `server.address=redis.…`, and the address becomes an identity claim on the peer.
//   - `db.system` (or its 1.30 spelling `db.system.name`) with a `server.address` is a
//     database, addressed by its host.
//   - a `server.address` reached over http or https, with no peer service, is an external
//     host.
//
// Anything else — an internal span, a producer span with no addressing at all — yields no
// callee, and the span only contributes the existence of its own service.
func resolveCallee(attrs map[string]string) (calleeAgg, bool) {
	peer := strings.TrimSpace(attrs[attrPeerService])
	db := strings.TrimSpace(attrs[feeder.AttrDBSystem])
	if db == "" {
		db = strings.TrimSpace(attrs[feeder.AttrDBSystemName])
	}
	address := strings.TrimSpace(attrs[feeder.AttrServerAddress])
	scheme := strings.ToLower(strings.TrimSpace(attrs[feeder.AttrURLScheme]))
	port := parsePort(attrs[feeder.AttrServerPort])

	out := calleeAgg{dbSystem: db, address: address, port: port}
	switch {
	case peer != "":
		out.key = calleeKey{ns: feeder.NSOTelService, value: peer}
		out.short = peer
		out.external = db != "" || address != ""
		if scheme == "http" || scheme == "https" {
			out.scheme = scheme
		}
	case db != "" && address != "":
		out.key = calleeKey{ns: feeder.NSServerAddress, value: address}
		out.short = shortHostName(address)
		out.external = true
	case address != "" && (scheme == "http" || scheme == "https"):
		out.key = calleeKey{ns: feeder.NSServerAddress, value: address}
		out.short = shortHostName(address)
		out.scheme = scheme
		out.external = true
	default:
		return calleeAgg{}, false
	}
	if out.short == "" {
		out.short = out.key.value
	}
	return out, true
}

// spanWeight is how many spans one span stands for. A head-sampled span carrying
// `sampling.probability` stands for 1/p of them, which is what keeps a sampled pipeline in the
// same weight class as an unsampled one (research §11). A probability outside (0, 1] is
// meaningless and the span counts once.
func spanWeight(attrs map[string]string) float64 {
	raw, ok := attrs[attrSamplingProbability]
	if !ok {
		return 1
	}
	p, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(p) || p <= 0 || p > 1 {
		return 1
	}
	return 1 / p
}

// spanTime is the instant a span is attributed to: its start, or its end when the start is
// unset, or the arrival time of the payload that carried it when the span has neither. The
// timestamp is read and dropped with the span; it never reaches an event.
func spanTime(span *tracepb.Span, at time.Time) time.Time {
	if ns := span.GetStartTimeUnixNano(); ns > 0 {
		//nolint:gosec // an OTLP timestamp is nanoseconds since the epoch; the cast is the
		// protocol's own range and a value that overflows is not a time at all.
		return time.Unix(0, int64(ns)).UTC()
	}
	if ns := span.GetEndTimeUnixNano(); ns > 0 {
		//nolint:gosec // as above.
		return time.Unix(0, int64(ns)).UTC()
	}
	return at.UTC()
}

// readAttrs copies the attributes named in want, and only those, into a plain map. It is the
// whitelist constitution IV asks for, applied at the one point span data enters this package.
func readAttrs(attrs []*commonpb.KeyValue, want []string) map[string]string {
	out := make(map[string]string, len(want))
	for _, kv := range attrs {
		key := kv.GetKey()
		if !slices.Contains(want, key) {
			continue
		}
		if value, ok := scalarValue(kv.GetValue()); ok {
			out[key] = value
		}
	}
	return out
}

// scalarValue renders an OTLP attribute value, refusing everything that is not a scalar. An
// array or a nested map is exactly the shape telemetry arrives in and has no business in a
// property, so it never leaves this function (FR-009).
func scalarValue(v *commonpb.AnyValue) (string, bool) {
	switch value := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return value.StringValue, true
	case *commonpb.AnyValue_IntValue:
		return strconv.FormatInt(value.IntValue, 10), true
	case *commonpb.AnyValue_DoubleValue:
		return strconv.FormatFloat(value.DoubleValue, 'g', -1, 64), true
	case *commonpb.AnyValue_BoolValue:
		return strconv.FormatBool(value.BoolValue), true
	default:
		return "", false
	}
}

// parsePort reads a port that may have arrived as an integer or as a string.
func parsePort(raw string) int64 {
	port, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 32)
	if err != nil || port <= 0 {
		return 0
	}
	return port
}

// keepMax resolves two observations of one attribute inside a window to the greater in string
// order. The rule is arbitrary; what matters is that it does not depend on which span arrived
// first (testkit.Shuffle).
func keepMax(current, candidate string) string {
	if candidate > current {
		return candidate
	}
	return current
}

// internalSuffixes are the last labels of a name that is not a public domain. A host under one
// of them is named by its first label — `payments-db.shop.svc.cluster.local` is `payments-db`.
var internalSuffixes = []string{"local", "localdomain", "internal", "intranet", "lan", "home", "arpa", "cluster", "svc"}

// shortHostName is the name a third party is displayed and identified by.
//
// An operator calls the dependency `payments-db` or `stripe`, not
// `payments-db.shop.svc.cluster.local` or `api.stripe.com`, and the identity the graph keys it
// on is the full address either way — the short name is a label and an id part, never a ref
// value. A cluster-internal name is named by its first label and a public one by its
// registrable label, which is what makes `api.stripe.com` "stripe" rather than "api". An
// address that is a literal IP is its own name.
func shortHostName(host string) string {
	h := strings.ToLower(strings.TrimSpace(host))
	h = strings.TrimSuffix(h, ".")
	if h == "" {
		return ""
	}
	if trimmed, _, err := net.SplitHostPort(h); err == nil && trimmed != "" {
		h = trimmed
	}
	h = strings.Trim(h, "[]")
	if net.ParseIP(h) != nil {
		return h
	}
	labels := strings.Split(h, ".")
	if len(labels) < 2 {
		return labels[0]
	}
	if slices.Contains(internalSuffixes, labels[len(labels)-1]) || slices.Contains(labels, "svc") {
		return labels[0]
	}
	return labels[len(labels)-2]
}
