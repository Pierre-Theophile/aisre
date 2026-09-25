// SPDX-License-Identifier: Apache-2.0

package gcp

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

	sqladmin "google.golang.org/api/sqladmin/v1"

	compute "google.golang.org/api/compute/v1"

	"github.com/Pierre-Theophile/aisre/internal/gcpx"
	eventlog "github.com/Pierre-Theophile/aisre/internal/log"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The feeder (T046, contracts/gcp-feeder.md §1, FR-004).
//
// # Ordering is `none`, and that is a promise about the source rather than about the code
//
// The feeder's inputs are polls of independent APIs plus a log stream with **no published ordering
// guarantee** (§5.2, research §3: Google publishes no ingestion-delay or out-of-order bound for Cloud
// Audit Logs — the only statements are qualitative). Declaring `per_source_sequence` would make
// `testkit.Shuffle` permute *less* than reality does, which is the one direction a declaration must
// never err in: the conformance suite would then pass on an ordering the source never promised.
//
// # The reordering window is measured, not assumed
//
// There is no number to cite, so the window is configuration seeded from measurement, recorded in the
// checkpoint, and continuously re-measured from `receiveTimestamp − timestamp`. `Describe` returns
// what is configured; the checkpoint reports what was observed; and the gap between them is what
// tells an operator to widen it.
//
// # The gate runs before anything is emitted
//
// FR-004, and `Run` enforces the ordering structurally: `emitAll` is unreachable until `gate` has
// returned a `*gcpx.Credential`, and `gcpx.Credential` has no exported constructor — only the gate
// makes one. So "prove read-only before emitting" is not a sequence a reviewer has to verify, it is
// the only sequence that compiles.

// SourceIDPrefix is the prefix of every GCP source id. The suffix is the org slug, so two
// organisations feeding one graph are two sources.
const SourceIDPrefix = "gcp:"

// Kind is the connector family.
const Kind = "gcp"

// SchemaVersion is the event schema version this feeder emits.
const SchemaVersion = "1.0.0"

// DefaultReorderingWindow is the configured starting point, seeded from measurement rather than from
// a published bound, because there is no published bound (§5.2). It is deliberately generous: a
// window wider than the truth makes the conformance test stricter, and a window narrower than the
// truth makes it pass on reorderings the source actually produces.
const DefaultReorderingWindow = 10 * time.Minute

// RequiredScopes documents the read-only permissions this feeder asks GCP for. It is documentation
// and a startup checklist; the enforcement is `internal/gcpx`'s three-layer gate.
var RequiredScopes = []string{
	"run.services.list, run.services.get, run.revisions.list, run.revisions.get",
	"logging.logEntries.list on the admin-activity stream only (never data access: FR-042)",
	"monitoring.alertPolicies.list, monitoring.timeSeries.list",
	"cloudsql.instances.list, cloudsql.instances.get (a CUSTOM role: roles/cloudsql.viewer grants " +
		"cloudsql.instances.export, a data-egress capability, and disqualifies itself under FR-004)",
	"container.clusters.get for GKE cluster metadata only (FR-030)",
	gcpx.ScopeCloudPlatformReadOnly,
}

// Options configures one feeder run.
type Options struct {
	// OrgSlug suffixes the source id. Required: an empty slug would make every organisation's
	// events one source.
	OrgSlug string
	// Scope is the project and region scope. Required, and refusing an empty one is FR-131: no
	// code and no checked-in default may assume a project name.
	Scope Scope
	// ReorderingWindow overrides DefaultReorderingWindow.
	ReorderingWindow time.Duration
	// Labels is the label policy in force.
	Labels LabelPolicy
	// Actors is the actor classification in force.
	Actors ActorPolicy
	// SilenceThreshold is N for the silence rule. Zero uses DefaultSilenceThreshold.
	SilenceThreshold int
	// Horizon is the revision history horizon.
	Horizon Horizon
	// AuditFilters is the log type, service and operation filters in force (FR-040). It is the
	// *request* filter — the strings sent to `entries.list` — and is recorded in the checkpoint so a
	// reader can see what was asked for.
	AuditFilters []string
	// AuditStaleness is how long an audit entry a specialised path might still want waits before it
	// becomes a general change (auditlog.go). Zero uses DefaultAuditStaleness.
	AuditStaleness time.Duration
	// AuditScope is what the feeder keeps of what came back (FR-040). The zero value is no
	// restriction, which is the right default for a catch-all: an operator narrows it, and the
	// narrowing is recorded in the checkpoint rather than left as a silence a reader would read as
	// "nothing happened" (auditlog.go).
	AuditScope AuditScope
	// OmittedSurfaces are the areas deliberately not read (FR-057).
	OmittedSurfaces []string
	// IncidentsAPIEnabled turns on `projects.alerts.list`, which is the only public read API for
	// alerting incidents and therefore the only way a transition instant can be read at all.
	//
	// It is a declared capability rather than an operational convenience, and the reason is the
	// dependency: the surface is **Public Preview**, under Pre-GA terms, with Google warning that
	// the labels in its response may change; and it is **not in the first-party GAPIC** — the only
	// Go binding is google.golang.org/api/monitoring/v3, whose package header says it is in
	// maintenance mode. A Preview API through a maintenance-mode client is this feature's single
	// riskiest dependency, so it is opt-in and the default is off.
	//
	// With it off the feature still works: ALERT nodes come from the GA `alertPolicies.list` read,
	// and `monitor_state` answers NO_DATA with coverage naming the absent source — the contract's
	// own answer for a source the organisation does not have (contracts/gcp-feeder.md §6,
	// config/gcp.yaml alerts.incidents_api).
	IncidentsAPIEnabled bool
	// FingerprintKey keys the configuration fingerprints (T137). Empty means no fingerprint is
	// stored at all and the configuration nodes say so: an unkeyed digest of a configuration value
	// is recoverable by enumeration, which is not a redaction (config.go).
	FingerprintKey []byte
	// AlertPollInterval is the cadence alert transitions are read at. Zero uses
	// DefaultAlertPollInterval. It is recorded on every transition as the sampling interval,
	// because a polled history is a sequence of observations and not a complete record (FR-051).
	AlertPollInterval time.Duration
	// Log is where operational telemetry goes. Nil discards.
	Log *slog.Logger
	// Instruments is what the run publishes about its own operation (FR-011, FR-051). Nil uses
	// gcpx.NopInstruments, because self-observability is never a precondition for feeding: a replay
	// and a test run without a meter provider.
	Instruments gcpx.Instruments
}

// DefaultSilenceThreshold is `config/gcp.yaml`'s `silence_rule_consecutive_polls: 3`.
const DefaultSilenceThreshold = 3

// Validate refuses options that could not produce an honest run.
func (o Options) Validate() error {
	if o.OrgSlug == "" {
		return errors.New("gcp: Options.OrgSlug is required; it is the suffix of the source id, and " +
			"an empty one makes every organisation's events one source")
	}
	if len(o.Scope.Projects) == 0 {
		return errors.New("gcp: Options.Scope.Projects is empty. The scope is operator configuration " +
			"and no code or checked-in default may assume a project name (FR-131), so an empty scope " +
			"is a refusal at startup rather than a run that reads nothing and reports success")
	}
	if len(o.Scope.Regions) == 0 {
		return errors.New("gcp: Options.Scope.Regions is empty; a region scope of none reads nothing")
	}
	if o.ReorderingWindow < 0 {
		return fmt.Errorf("gcp: Options.ReorderingWindow is negative (%s)", o.ReorderingWindow)
	}
	if o.AlertPollInterval < 0 {
		return fmt.Errorf("gcp: Options.AlertPollInterval is negative (%s)", o.AlertPollInterval)
	}
	if o.AlertPollInterval > MaxAlertPollInterval {
		// Not clamped silently. SC-004 binds transition-to-observed delay to the poll interval
		// plus one minute at p95, so a longer interval is a target this run cannot meet — and a
		// run that quietly polls slower than its own success criterion is a run whose number
		// means nothing.
		return fmt.Errorf(
			"gcp: Options.AlertPollInterval is %s, above the published ceiling of %s. The ceiling is "+
				"not configurable past this: SC-004 binds transition-to-observed delay to the poll "+
				"interval plus one minute at p95 (config/gcp-budget.yaml §cadences)",
			o.AlertPollInterval, MaxAlertPollInterval)
	}
	return nil
}

// Gate proves the credential read-only before anything is emitted. It is an interface so that a
// replay can supply nothing — there is no live credential to check when the input is a recording, and
// `Run` records that the layer did not run rather than claiming a check that did not happen.
type Gate interface {
	// Prove returns a credential only if every layer passed.
	Prove(ctx context.Context) (*gcpx.Credential, error)
}

// Feeder reads one GCP organisation.
type Feeder struct {
	opts Options
	log  *slog.Logger

	// Gate is asked to prove the credential read-only before anything is emitted (FR-004). Nil
	// means there is nothing to prove — a replay has no credential — and the checkpoint says so.
	Gate Gate

	mu      sync.Mutex
	silence *SilenceTracker
	// splits is the last observed split per service, which is what makes a *change* detectable:
	// a poll sees a split, not a transition.
	splits map[string]TrafficSplit
	// identities is the last observed uid per service, for the recreation rule (FR-025).
	identities map[string]Identity
	// held is the traffic shifts observed and not yet dated.
	held []HeldSplit
	// recreations are services deleted and recreated under one name, for the checkpoint note: a
	// retraction event has nowhere to say why (FR-025).
	recreations []Recreation
	// audit correlates the request and completion entries of each long-running operation, and is
	// what dates a traffic shift (audit.go).
	audit *AuditIndex
	// lag is the measured `receiveTimestamp − timestamp` distribution.
	lag []time.Duration
	// skew is GCP's clock against this process's, accumulated by the audit index and published at
	// each poll marker (004 T142; 003 FR-153). See AuditIndex.Skew for why the audit stream is the
	// only GCP surface it is taken from.
	skew *feeder.Skew
	// instruments is where the skew and the rest of FR-011's counters are published.
	instruments gcpx.Instruments
	// policies is the last observed alert policy per policy ref, which is what lets an incident be
	// keyed on ITS policy's aggregation grouping: an incident carries a policy snapshot but not
	// the grouping, and a group key derived from the wrong field set splits one alert's history.
	policies map[string]AlertPolicyObservation
	// alerts is the little per-alerting-entity series the published suppression filter reads. It
	// is deliberately not persisted: losing it costs at most one redundant event, and a redundant
	// event is a DUPLICATE_NOOP.
	alerts map[string]*eventlog.AlertSeries
	// incidents is the latest incident per alerting entity, which is what HandoffFor answers from.
	incidents map[string]AlertIncident
	// suppressed are the transitions the filter dropped this run. They reach the checkpoint,
	// because FR-052 says a suppression is STATED rather than applied silently.
	suppressed []SuppressedTransition
	// instances is the last observed Cloud SQL instance per instance ref, which is what makes a
	// configuration *change* detectable: a poll sees settings, not an edit. It is also what gives a
	// maintenance operation its region, which no field of an operation carries.
	instances map[string]InstanceObservation
	// configs is the last observed configuration version per service, for the same reason.
	configs map[string]ConfigObservation
	// flags is the Cloud SQL flag catalogue. It is neither a node nor a change: it is what makes a
	// flag change interpretable, and it is held to annotate one (contract §4).
	flags map[string]*sqladmin.Flag
	// heldSQL are configuration diffs observed and not yet dated, HeldSplit's siblings.
	heldSQL []HeldSQLConfig
	// heldMaintenance are maintenance operations with no instance ref to attach to.
	heldMaintenance []HeldMaintenance
	// passedInSilence are announced maintenance windows that stopped being announced after their
	// start had passed. Nothing is emitted for them (FR-066) and the checkpoint says so.
	passedInSilence []string
	// missingZones are the zones GCP itself said a cluster list may be missing.
	missingZones []string
	// lateArrivals are the audit changes emitted for entries that arrived after the extent they
	// belong to had been checkpointed. The change is emitted at its own instant; the checkpoint is
	// where the gap is stated (§5.2).
	lateArrivals []string
	// The load-balancer chain, gathered across the cycle's four compute payloads and applied together
	// at the poll marker: a forwarding rule alone names no service and a NEG alone names no load
	// balancer, so an exposure emitted as soon as the last piece arrived would depend on payload order.
	forwardingRules []ForwardingRuleObservation
	urlMaps         []*compute.UrlMap
	backendServices []*compute.BackendService
	negs            []*compute.NetworkEndpointGroup
	// dnsRecords is the last observed record set per record, because a poll sees a record and not a
	// switch; dnsPending holds the previous targets of a record whose targets moved and which has no
	// audit entry to date it yet.
	dnsRecords map[string]DNSRecord
	dnsPending map[string][]string
	// heldDNS are record sets whose targets moved with no Cloud DNS audit entry to date the switch.
	heldDNS []string
	// proposals are the dependency proposal keys gathered this cycle, so one is raised once (FR-029).
	proposals map[string]bool
	// pendingProposals are the proposals gathered this cycle and readyProposals the ones gathered in
	// the previous one, which is what the next poll marker emits. See emitPendingProposals: a
	// proposal is refused if either endpoint is unknown, so it waits until the cycle that asserted
	// them has been checkpointed.
	pendingProposals []DependencyProposal
	readyProposals   []DependencyProposal
}

// New returns a feeder over opts.
func New(opts Options) (*Feeder, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	threshold := opts.SilenceThreshold
	if threshold == 0 {
		threshold = DefaultSilenceThreshold
	}
	silence, err := NewSilenceTracker(threshold)
	if err != nil {
		return nil, err
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if len(opts.Labels.Allowlist) == 0 {
		opts.Labels = DefaultLabelPolicy()
	}
	instruments := opts.Instruments
	if instruments == nil {
		instruments = gcpx.NopInstruments{}
	}
	skew := &feeder.Skew{}
	audit := NewScopedAuditIndex(opts.AuditScope, opts.AuditStaleness)
	audit.Skew = skew
	return &Feeder{
		opts:        opts,
		log:         log,
		silence:     silence,
		skew:        skew,
		instruments: instruments,
		splits:      map[string]TrafficSplit{},
		identities:  map[string]Identity{},
		audit:       audit,
		policies:    map[string]AlertPolicyObservation{},
		alerts:      map[string]*eventlog.AlertSeries{},
		incidents:   map[string]AlertIncident{},
		instances:   map[string]InstanceObservation{},
		configs:     map[string]ConfigObservation{},
		flags:       map[string]*sqladmin.Flag{},
		proposals:   map[string]bool{},
		dnsRecords:  map[string]DNSRecord{},
		dnsPending:  map[string][]string{},
	}, nil
}

// Skew is the clock-skew observation this run accumulated (004 T142; 003 FR-153).
//
// Exported rather than only logged, and that is what makes FR-153 testable at all: a log line is not a
// measurement a test can read, and "the skew MUST be reported" was satisfied for the whole of feature
// 003 by machinery nobody ever called. A reader of this report cannot use it to correct anything —
// SkewReport is a value, and the instants it was computed from have already been written to the graph
// unaltered.
func (f *Feeder) Skew() feeder.SkewReport { return f.skew.Report() }

// Describe returns the feeder's contract. It is a pure function: the harness calls it before Run and
// compares what Run emits against it.
func (f *Feeder) Describe() feeder.Description {
	window := f.opts.ReorderingWindow
	if window == 0 {
		window = DefaultReorderingWindow
	}
	return feeder.Description{
		SourceID:      SourceIDPrefix + f.opts.OrgSlug,
		Kind:          Kind,
		SchemaVersion: SchemaVersion,
		// §5.2: polls of independent APIs plus a log stream with no published ordering guarantee.
		Ordering:         feeder.OrderingNone,
		ReorderingWindow: window,
		RequiredScopes:   append([]string(nil), RequiredScopes...),
		Namespaces:       append([]string(nil), Namespaces...),
	}
}

// Run reads from src and writes to em (T046).
//
// The order is `Validate` → gate → emit, and nothing before the gate emits. `--dry-run` is the gate
// and nothing else, which is why the gate is a separate step rather than folded into the first read.
func (f *Feeder) Run(ctx context.Context, src feeder.Source, em feeder.Emitter) error {
	desc := f.Describe()
	if err := desc.Validate(); err != nil {
		return err
	}

	// FR-004. The credential is proved read-only before anything is emitted, and the proof is what
	// produces the credential — there is no path in which a read happens first.
	var proved *gcpx.Credential
	if f.Gate != nil {
		creds, err := f.Gate.Prove(ctx)
		if err != nil {
			return fmt.Errorf("gcp: the read-only gate refused, so nothing was emitted: %w", err)
		}
		proved = creds
		f.log.InfoContext(ctx, "read-only gate passed", "source", desc.SourceID)
	} else {
		// A replay. The checkpoint records that the layer did not run, so a reader is never told a
		// check happened that did not.
		f.log.InfoContext(ctx, "no credential to prove: replaying a recording", "source", desc.SourceID)
	}
	_ = proved

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

// PayloadKinds are the fixture directory names this feeder's payloads are stored under. They are
// named here rather than in the fixture author's head so that a recording and a replay agree.
const (
	// PayloadServices is a `projects.locations.services.list` response.
	PayloadServices = "services"
	// PayloadRevisions is a `projects.locations.services.revisions.list` response.
	PayloadRevisions = "revisions"
	// PayloadAuditEntries is a page of `logging.entries.list` admin-activity entries.
	PayloadAuditEntries = "audit"
	// PayloadPollMarker declares a poll's outcome and scope. It is its own payload rather than a
	// field on the others because §8.1's rule is about the *poll*, and a partial poll's defining
	// property is that some payloads are missing — which only a separate marker can state.
	PayloadPollMarker = "poll"
)

// PollMarker is the `poll` payload: what a poll covered and whether it finished.
type PollMarker struct {
	Outcome  string   `json:"outcome"`
	Covered  []string `json:"covered,omitempty"`
	Reason   string   `json:"reason,omitempty"`
	Projects []string `json:"projects,omitempty"`
	Regions  []string `json:"regions,omitempty"`
	// Deferred are the areas the budget dropped, from the published order (FR-149). They are on the
	// marker rather than derived, because a replay has no budget: the recording states what the live
	// run deferred, and the checkpoint carries it either way.
	Deferred []string `json:"deferred,omitempty"`
	// StopReason is why the poll stopped, from gcpx's published set. "We ran out of quota" and "we
	// looked and found nothing" are opposite conclusions, and a poll that did not say which would
	// let the first be read as the second.
	StopReason string `json:"stop_reason,omitempty"`
}

// apply dispatches one payload. It is deliberately a switch on the payload kind rather than a
// registry: the set is closed, and a payload kind nobody handles must be an error rather than a
// silently ignored file in a fixture directory.
func (f *Feeder) apply(ctx context.Context, desc feeder.Description, em feeder.Emitter, payload feeder.Payload) error {
	switch payload.Kind {
	case PayloadPollMarker:
		var marker PollMarker
		if err := json.Unmarshal(payload.Bytes, &marker); err != nil {
			return fmt.Errorf("gcp: decoding a %s payload: %w", payload.Kind, err)
		}
		return f.applyPollMarker(ctx, desc, em, marker, payload.At)
	case PayloadServices:
		return f.applyServices(ctx, desc, em, payload.Bytes, payload.At)
	case PayloadRevisions:
		return f.applyRevisions(ctx, desc, em, payload.Bytes, payload.At)
	case PayloadAuditEntries:
		return f.applyAuditEntries(payload.Bytes, payload.At)
	case PayloadAlertPolicies:
		return f.applyAlertPolicies(ctx, desc, em, payload.Bytes, payload.At)
	case PayloadAlerts:
		return f.applyAlerts(ctx, desc, em, payload.Bytes, payload.At)
	case PayloadSQLInstances:
		return f.applySQLInstances(ctx, desc, em, payload.Bytes, payload.At)
	case PayloadSQLOperations:
		return f.applySQLOperations(ctx, desc, em, payload.Bytes, payload.At)
	case PayloadSQLFlags:
		return f.applySQLFlags(payload.Bytes)
	case PayloadGKEClusters:
		return f.applyGKEClusters(ctx, desc, em, payload.Bytes, payload.At)
	case PayloadForwardingRules:
		return f.applyForwardingRules(ctx, desc, em, payload.Bytes, payload.At)
	case PayloadURLMaps:
		return f.applyURLMaps(payload.Bytes)
	case PayloadBackendServices:
		return f.applyBackendServices(payload.Bytes)
	case PayloadNetworkEndpointGroups:
		return f.applyNEGs(payload.Bytes)
	case PayloadDNSRecordSets:
		return f.applyDNSRecordSets(payload.Bytes, payload.At)
	default:
		return fmt.Errorf("gcp: payload kind %q is not one this feeder reads (%s); a payload nobody "+
			"handles is a fixture that silently tests less than it claims",
			payload.Kind, strings.Join([]string{PayloadServices, PayloadRevisions, PayloadAuditEntries,
				PayloadAlertPolicies, PayloadAlerts, PayloadSQLInstances, PayloadSQLOperations,
				PayloadSQLFlags, PayloadGKEClusters, PayloadForwardingRules, PayloadURLMaps,
				PayloadBackendServices, PayloadNetworkEndpointGroups, PayloadDNSRecordSets,
				PayloadPollMarker}, ", "))
	}
}

// applyPollMarker records a poll and emits its checkpoint.
func (f *Feeder) applyPollMarker(ctx context.Context, desc feeder.Description, em feeder.Emitter, marker PollMarker, at time.Time) error {
	outcome, err := parseOutcome(marker.Outcome)
	if err != nil {
		return err
	}
	f.mu.Lock()
	held := append([]HeldSplit(nil), f.held...)
	recreations := append([]Recreation(nil), f.recreations...)
	heldSQL := append([]HeldSQLConfig(nil), f.heldSQL...)
	lateArrivals := append([]string(nil), f.lateArrivals...)
	f.lateArrivals = nil
	heldDNS := append([]string(nil), f.heldDNS...)
	f.heldDNS = nil
	heldMaintenance := append([]HeldMaintenance(nil), f.heldMaintenance...)
	passedInSilence := append([]string(nil), f.passedInSilence...)
	missingZones := append([]string(nil), f.missingZones...)
	f.mu.Unlock()

	// The exposure relations, derived from everything the cycle's compute payloads gathered, and the
	// DNS switches whose audit entry has arrived.
	if err := f.emitExposures(ctx, desc, em, at); err != nil {
		return err
	}
	if err := f.emitDNSSwitches(ctx, desc, em, at); err != nil {
		return err
	}

	// The general audit changes: every in-scope entry no specialised path consumed (auditlog.go).
	// They are emitted at the poll marker because "nothing consumed it" is only knowable once the
	// cycle's payloads have all been applied.
	if err := f.emitAuditChanges(ctx, desc, em, at); err != nil {
		return err
	}

	// The dependency proposals gathered in the previous cycle, and the cycle boundary for the
	// once-per-cycle rule. They are emitted here rather than where they were found because a
	// proposal is refused if either endpoint is unknown (emitPendingProposals).
	if err := f.emitPendingProposals(ctx, desc, em, at); err != nil {
		return err
	}

	scope := f.opts.Scope
	if len(marker.Projects) > 0 {
		scope = Scope{Projects: marker.Projects, Regions: marker.Regions}
	}
	checkpoint := Checkpoint{
		From:               at.Add(-f.Describe().ReorderingWindow),
		To:                 at,
		Outcome:            outcome,
		Scope:              scope,
		Horizon:            f.opts.Horizon,
		AuditFilters:       f.opts.AuditFilters,
		EnvironmentMapping: f.opts.Labels.EnvironmentFromProject,
		ReorderingWindow:   f.Describe().ReorderingWindow,
		OmittedSurfaces:    f.opts.OmittedSurfaces,
		HeldSplits:         held,
		Recreations:        recreations,
		Suppressed:         f.Suppressed(),
		HeldSQLConfigs:     heldSQL,
		HeldMaintenance:    heldMaintenance,
		PassedInSilence:    passedInSilence,
		MissingZones:       missingZones,
		AuditScope:         f.opts.AuditScope.String(),
		LateArrivals:       lateArrivals,
		Deferred:           marker.Deferred,
		HeldDNSSwitches:    heldDNS,
		StopReason:         gcpx.StopReason(marker.StopReason),
		PartialReason:      marker.Reason,
	}
	checkpoint.ObservedLagP50, checkpoint.ObservedLagP99 = f.observedLag()
	if err := checkpoint.Validate(); err != nil {
		return err
	}
	f.reportSkew(ctx)
	// A partial poll declares its gap: the graph is told the silence before `from` was ignorance
	// rather than absence. The NOTE goes with it, and until now it did not: this feeder built a
	// checkpoint stating the audit scope, the region scope, the held shifts, the suppressions, the
	// environment mapping and the measured lag distribution, validated it, asserted its `Note()` in
	// four test files — and then handed three of its fields to an emitter that could carry no more.
	// So everything FR-057 asks a checkpoint to say about the scope in force died here, one call
	// short of the graph. `Emitter.Checkpoint` now takes the whole fact (004 T043).
	return em.Checkpoint(ctx, feeder.CheckpointFact{
		ExtentFrom: checkpoint.From,
		ExtentTo:   checkpoint.To,
		GapBefore:  outcome == PollPartial,
		Note:       checkpoint.Note(),
	})
}

// reportSkew publishes the run's clock-skew observation (004 T142; 003 FR-153, 004 FR-058).
//
// Three destinations rather than one, because they answer different readers. `Instruments.Skew` is the
// interface FR-011 declared for this and is given its first caller here. The log line carries
// `skew_samples` beside the mean because "the mean skew is zero" and "no sample was taken" are
// different facts and a mean alone cannot tell them apart — which is precisely how this measurement
// went unmade for a whole feature. And the Warn fires only beyond the threshold, so a normal run does
// not train an operator to ignore the line.
//
// Nothing here corrects anything. By the time this runs, every event of the cycle has been emitted at
// the instant GCP stated.
func (f *Feeder) reportSkew(ctx context.Context) {
	report := f.skew.Report()
	f.instruments.Skew(ctx, report)
	f.log.Info("clock skew observed",
		"skew_samples", report.Samples, "skew_mean", report.Mean, "skew_min", report.Min,
		"skew_max", report.Max, "skew_last", report.Last, "skew_threshold", report.Threshold,
		"skew_exceeded", report.Exceeded)
	if report.Beyond() {
		f.log.Warn("GCP's clock and this graph's observed time disagree beyond the stated threshold; "+
			"the skew is REPORTED and neither clock is corrected with the other, so a change's valid "+
			"time is still the instant GCP stated (FR-153, FR-058)",
			"samples", report.Samples, "exceeded", report.Exceeded, "mean", report.Mean,
			"max", report.Max, "threshold", report.Threshold)
	}
}

// applyAuditEntries reads one page of audit entries into the correlation index.
//
// It emits nothing itself. An audit entry is evidence about *when* and *who*, and it is the service
// poll that says *what* changed — so an entry is filed here and consumed when the poll that needs it
// arrives. Emitting a change straight from an entry would mean reading the split from the request
// body, which is the desired split and not the observed one (contracts/gcp-feeder.md §3.1).
func (f *Feeder) applyAuditEntries(raw []byte, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	before := len(f.audit.Lag())
	if _, err := f.audit.Ingest(raw, at); err != nil {
		return err
	}
	// Fold the newly measured lag into the distribution the checkpoint reports (§5.2).
	samples := f.audit.Lag()
	if len(samples) > before {
		f.lag = append(f.lag, samples[before:]...)
	}
	return nil
}

func parseOutcome(value string) (PollOutcome, error) {
	switch value {
	case "complete":
		return PollComplete, nil
	case "partial":
		return PollPartial, nil
	default:
		return PollUnknown, fmt.Errorf("gcp: poll outcome %q is neither \"complete\" nor \"partial\"; "+
			"a poll whose outcome is unstated cannot be evidence of absence (FR-012)", value)
	}
}

// ObserveLag records one `receiveTimestamp − timestamp` sample (§5.2). The distribution is reported
// in the checkpoint so the configured window can be justified from data and widened when the data
// says so — which is the whole of what "measured, not assumed" buys.
func (f *Feeder) ObserveLag(sample time.Duration) {
	if sample < 0 {
		// A negative lag means the two clocks disagree, which is a fact about the clocks and not
		// about ingestion. It is not folded into the distribution.
		return
	}
	f.mu.Lock()
	f.lag = append(f.lag, sample)
	f.mu.Unlock()
}

// observedLag returns the p50 and p99 of the samples so far.
func (f *Feeder) observedLag() (p50, p99 time.Duration) {
	f.mu.Lock()
	samples := append([]time.Duration(nil), f.lag...)
	f.mu.Unlock()
	if len(samples) == 0 {
		return 0, 0
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	return samples[percentileIndex(len(samples), 50)], samples[percentileIndex(len(samples), 99)]
}

// percentileIndex is the nearest-rank index of the pth percentile of n samples.
func percentileIndex(n, p int) int {
	if n == 0 {
		return 0
	}
	idx := (p*n + 99) / 100
	if idx < 1 {
		idx = 1
	}
	if idx > n {
		idx = n
	}
	return idx - 1
}

// HoldShift records a traffic shift that cannot yet be dated, so the next checkpoint states it.
func (f *Feeder) HoldShift(held HeldSplit) {
	f.mu.Lock()
	f.held = append(f.held, held)
	f.mu.Unlock()
}

// Held returns the shifts currently held, for a checkpoint or a test.
func (f *Feeder) Held() []HeldSplit {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]HeldSplit(nil), f.held...)
}

// holdSQLConfig records a configuration diff that cannot yet be dated, so the next checkpoint states
// it (T131).
func (f *Feeder) holdSQLConfig(held HeldSQLConfig) {
	f.mu.Lock()
	f.heldSQL = append(f.heldSQL, held)
	f.mu.Unlock()
}

// HeldSQLConfigs returns the configuration diffs currently held, for a checkpoint or a test.
func (f *Feeder) HeldSQLConfigs() []HeldSQLConfig {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]HeldSQLConfig(nil), f.heldSQL...)
}

// Instances returns the Cloud SQL instances observed so far, sorted by ref. It is what a test and the
// dependency derivation read, and it is a copy: a caller holding the feeder's map would be reading
// state another payload is writing.
func (f *Feeder) Instances() []InstanceObservation {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]InstanceObservation, 0, len(f.instances))
	for _, obs := range f.instances {
		out = append(out, obs)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Instance.Value() < out[j].Instance.Value() })
	return out
}
