// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	compute "google.golang.org/api/compute/v1"
	container "google.golang.org/api/container/v1"
	dns "google.golang.org/api/dns/v1"
	sqladmin "google.golang.org/api/sqladmin/v1"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// Applying the Cloud SQL, configuration and GKE payloads (T129–T140).
//
// The three `apply*` functions here follow the same shape as the Cloud Run ones in map.go, and one
// property is worth stating because it is the one the conformance shuffle checks: **each of them is
// one assertion per entity per interval**. An instance is upserted once per payload, its claims once,
// its pointers on the node — and never a second node for the same entity over the same interval,
// because two assertions of one node over one valid interval do not commute, so a reordering inside
// the declared window would change the result.

// applySQLInstances maps one `instances.list` response (T129, T131, T132, T133).
func (f *Feeder) applySQLInstances(ctx context.Context, desc feeder.Description, em feeder.Emitter, raw []byte, at time.Time) error {
	var payload sqladmin.InstancesListResponse
	if err := json.Unmarshal(raw, &payload); err != nil {
		return fmt.Errorf("gcp: decoding a %s payload: %w", PayloadSQLInstances, err)
	}
	for _, instance := range payload.Items {
		if err := f.emitInstance(ctx, desc, em, instance, at); err != nil {
			return err
		}
	}
	return nil
}

// emitInstance emits everything one instance read asserts: the node, its claims, its pointers, the
// configuration change since the last poll, and the announced maintenance.
func (f *Feeder) emitInstance(ctx context.Context, desc feeder.Description, em feeder.Emitter,
	instance *sqladmin.DatabaseInstance, at time.Time) error {
	obs, err := ObserveInstance(instance, f.opts.Labels)
	if err != nil {
		return err
	}

	props, err := obs.Props().Build()
	if err != nil {
		return err
	}
	pointers, err := obs.Pointers()
	if err != nil {
		return err
	}
	node := obs.NodeFact(at)
	node.Props = props
	node.Pointers = pointers
	node.SourceObservedAt = at
	id, node, err := f.stateAssertion(desc.SourceID, "sql_instance", obs.Instance.Value(), node, at, time.Time{})
	if err != nil {
		return err
	}
	if err := emit(ctx, em, feeder.UpsertNode(desc, id, node)); err != nil {
		return err
	}
	if err := f.emitClaims(ctx, desc, em, obs.Instance.Ref(), obs.Claims(), obs.Labels.Environment, at); err != nil {
		return err
	}
	if err := f.emitOwner(ctx, desc, em, obs.Instance.Ref(), obs.Labels, obs.CreateTime, at); err != nil {
		return err
	}

	f.mu.Lock()
	previous, had := f.instances[obs.Instance.Value()]
	f.instances[obs.Instance.Value()] = obs
	f.mu.Unlock()

	if err := f.emitAnnouncedMaintenance(ctx, desc, em, previous, obs, had, at); err != nil {
		return err
	}

	// The configuration change. The first observation of an instance asserts the properties and
	// emits no change: there is nothing to have changed from, and emitting one would claim a
	// configuration edit at the instant the connector was first run.
	if !had {
		return nil
	}
	diff := DiffInstance(previous, obs)
	if diff.Empty() {
		return nil
	}
	completion := f.audit.TakeSQLInstance(obs.Instance.Project, obs.Instance.Name)
	if completion == nil {
		// Held, and the checkpoint says so. Dating it at the poll would put a change in the graph
		// at an instant GCP never stated.
		f.holdSQLConfig(HoldSQLConfig(obs.Instance, diff, at))
		return nil
	}
	actor := f.opts.Actors.Classify(completion.Auth, false)
	change, err := SQLConfigChange(obs.Instance, completion.At, diff, actor)
	if err != nil {
		return err
	}
	if _, err := f.sqlConfigProps(obs.Instance, diff, actor, completion).Build(); err != nil {
		// Built here so a bad property fails at the feeder rather than in the projector. The change
		// node's properties are written by the projector from the change body.
		return err
	}
	// The entry that dated this edit is consumed, so it does not also become a general audit change.
	f.consumeAuditEntry(completion.OperationID)
	change.SourceObservedAt = at
	return emit(ctx, em, feeder.ObserveChange(desc,
		feeder.NewID(desc.SourceID, "change", change.Ref.GetValue()), change))
}

// sqlConfigProps adds the flag-catalogue annotation to the configuration change's properties.
//
// This is the only thing the flag catalogue is for (contract §4): it is neither a node nor a change,
// it is what makes a flag change interpretable. "max_connections changed" and "max_connections
// changed and that requires a restart" are the same edit and different incidents.
func (f *Feeder) sqlConfigProps(inst SQLInstance, diff SQLConfigDiff, actor Actor, completion *Completion) *feeder.Props {
	props := SQLConfigChangeProps(inst, diff, actor, completion.OperationID, completion.MethodName)
	if restarting := f.flagsRequiringRestart(diff); len(restarting) > 0 {
		props = props.Strs(PropSQLFlagsRequiringRestart, restarting...)
	}
	return props
}

// PropSQLFlagsRequiringRestart names the changed flags the catalogue says require a restart.
const PropSQLFlagsRequiringRestart = "sre.gcp.sql_flags_requiring_restart"

// flagsRequiringRestart reads the catalogue for the flags a diff touched. A flag absent from the
// catalogue is omitted rather than reported as not requiring a restart: an unread catalogue and a
// flag that needs no restart are different facts.
func (f *Feeder) flagsRequiringRestart(diff SQLConfigDiff) []string {
	f.mu.Lock()
	catalogue := f.flags
	f.mu.Unlock()
	if len(catalogue) == 0 {
		return nil
	}
	var out []string
	for _, entry := range append(append([]string(nil), diff.FlagsAdded...),
		append(diff.FlagsChanged, diff.FlagsRemoved...)...) {
		name := flagNameOf(entry)
		if flag, known := catalogue[name]; known && flag.RequiresRestart {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return sortedUniqueNames(out)
}

// flagNameOf reads the flag name out of a diff entry, which is `name=value` or `name: old -> new`.
func flagNameOf(entry string) string {
	if idx := strings.IndexAny(entry, "=:"); idx > 0 {
		return entry[:idx]
	}
	return entry
}

// emitAnnouncedMaintenance emits the announced maintenance and its corrections (T132).
func (f *Feeder) emitAnnouncedMaintenance(ctx context.Context, desc feeder.Description, em feeder.Emitter,
	previous, current InstanceObservation, hadPrevious bool, at time.Time) error {
	transition := AnnouncementChange(previous, current, hadPrevious, at)

	// The correction first, so that the observed order matches the order of belief: the old window
	// stopped being what we believed before the new one started being it.
	for _, correction := range []struct {
		window *ScheduledMaintenance
		state  graphv1.AnnouncementState
	}{
		{transition.Superseded, graphv1.AnnouncementState_SUPERSEDED},
		{transition.Cancelled, graphv1.AnnouncementState_CANCELLED},
		{transition.Announced, graphv1.AnnouncementState_ANNOUNCED},
	} {
		if correction.window == nil {
			continue
		}
		// `at` is both the observation instant and the clock here, which is what a replay has: the
		// wall clock of a replay is the recording's instant, not the machine's.
		change, err := ScheduledMaintenanceChange(current.Instance, *correction.window, correction.state, at, at)
		if err != nil {
			return err
		}
		if _, err := ScheduledMaintenanceProps(current.Instance, *correction.window).Build(); err != nil {
			return err
		}
		if err := emit(ctx, em, feeder.ObserveChange(desc, feeder.NewID(desc.SourceID, "change",
			change.Ref.GetValue(), correction.state.String()), change)); err != nil {
			return err
		}
	}
	if transition.PassedInSilence != nil {
		// Nothing is emitted, and the checkpoint says why. An announced window that passes in
		// silence stays announced for ever (FR-066); the passage of time is not an observation.
		f.mu.Lock()
		f.passedInSilence = append(f.passedInSilence, fmt.Sprintf("%s: announced %s, no longer announced "+
			"and not promoted (FR-066)", current.Instance.Value(), instant(transition.PassedInSilence.StartTime)))
		f.mu.Unlock()
	}
	return nil
}

// applySQLOperations maps one `operations.list` response into past maintenance (T132).
func (f *Feeder) applySQLOperations(ctx context.Context, desc feeder.Description, em feeder.Emitter, raw []byte, at time.Time) error {
	var payload sqladmin.OperationsListResponse
	if err := json.Unmarshal(raw, &payload); err != nil {
		return fmt.Errorf("gcp: decoding a %s payload: %w", PayloadSQLOperations, err)
	}
	for _, op := range payload.Items {
		if op == nil || !MaintenanceOperationTypes[op.OperationType] {
			// Every other operation type — a backup, an import, a restart — is read past. The
			// filter is the contract's (§4), and applying it here as well as server-side is what
			// makes a recording of an unfiltered response replay to the same events.
			continue
		}
		project, region, found := f.locateInstance(op)
		if !found {
			// The operation names its instance by a bare name and states no region, and this run has
			// not observed an instance by that name. A ref cannot be minted without the region, and a
			// guessed one addresses a different entity — so it is held and the checkpoint says so.
			f.mu.Lock()
			f.heldMaintenance = append(f.heldMaintenance, HeldMaintenance{
				Instance:  op.TargetId,
				Operation: op.Name,
				Type:      op.OperationType,
				Why:       HeldNoInstanceRegion,
			})
			f.mu.Unlock()
			continue
		}
		obs, ok, err := ObserveMaintenance(op, project, region)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		// The provider performed it, so the actor is the provider: rung 4, `vendorEvent` true. The
		// principal GCP names in `Operation.user` is passed to the ladder and dropped by it —
		// an email address is never stored (FR-134).
		actor := f.opts.Actors.Classify(AuthenticationInfo{PrincipalEmail: obs.User}, true)
		change, err := MaintenanceChange(obs, actor)
		if err != nil {
			return err
		}
		if _, err := MaintenanceChangeProps(obs, actor).Build(); err != nil {
			return err
		}
		change.SourceObservedAt = at
		if err := emit(ctx, em, feeder.ObserveChange(desc,
			feeder.NewID(desc.SourceID, "change", change.Ref.GetValue()), change)); err != nil {
			return err
		}
	}
	return nil
}

// locateInstance finds the project and region of the instance an operation is about.
//
// The project comes from the operation where it states one and from the observed instance otherwise;
// the region can only come from the observed instance, because no field of an operation carries one.
func (f *Feeder) locateInstance(op *sqladmin.Operation) (project, region string, found bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, obs := range f.instances {
		if obs.Instance.Name != op.TargetId {
			continue
		}
		if op.TargetProject != "" && obs.Instance.Project != op.TargetProject {
			continue
		}
		return obs.Instance.Project, obs.Instance.Region, true
	}
	return "", "", false
}

// HeldMaintenance is a maintenance operation that cannot be attached to an instance ref.
type HeldMaintenance struct {
	Instance  string
	Operation string
	Type      string
	Why       string
}

// HeldNoInstanceRegion is the reason a maintenance operation is held.
const HeldNoInstanceRegion = "the operation names its instance by a bare name and states no region, " +
	"and no instance of that name was observed in this run, so the change has no ref to attach to"

// String describes a held operation for the checkpoint.
func (h HeldMaintenance) String() string {
	return fmt.Sprintf("%s operation %s on instance %s: %s", h.Type, h.Operation, h.Instance, h.Why)
}

// applySQLFlags reads the flag catalogue (contract §4).
//
// It emits **nothing**. The catalogue is neither a node nor a change: it is what makes a flag change
// interpretable, and it is held to annotate one. A `flags.list` response in a fixture with no flag
// change in it therefore produces no events at all, which is correct and is asserted.
func (f *Feeder) applySQLFlags(raw []byte) error {
	var payload sqladmin.FlagsListResponse
	if err := json.Unmarshal(raw, &payload); err != nil {
		return fmt.Errorf("gcp: decoding a %s payload: %w", PayloadSQLFlags, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, flag := range payload.Items {
		if flag == nil || flag.Name == "" {
			continue
		}
		f.flags[flag.Name] = flag
	}
	return nil
}

// applyGKEClusters maps one `clusters.list` response (T138).
func (f *Feeder) applyGKEClusters(ctx context.Context, desc feeder.Description, em feeder.Emitter, raw []byte, at time.Time) error {
	var payload container.ListClustersResponse
	if err := json.Unmarshal(raw, &payload); err != nil {
		return fmt.Errorf("gcp: decoding a %s payload: %w", PayloadGKEClusters, err)
	}
	if len(payload.MissingZones) > 0 {
		// GCP's own statement that the list may be incomplete. It reaches the checkpoint rather than
		// being dropped: a cluster list missing a zone reads exactly like a zone with no clusters.
		f.mu.Lock()
		f.missingZones = append(f.missingZones, payload.MissingZones...)
		f.mu.Unlock()
	}
	for _, cluster := range payload.Clusters {
		project, ok := projectFromSelfLink(cluster.SelfLink)
		if !ok {
			return fmt.Errorf("gcp: cluster %q carries no selfLink to read its project from; a cluster "+
				"ref built from a guessed project addresses a different cluster (FR-010)", cluster.Name)
		}
		if err := f.emitCluster(ctx, desc, em, cluster, project, at); err != nil {
			return err
		}
	}
	return nil
}

// projectFromSelfLink reads the project out of a GKE selfLink, which is
// `https://container.googleapis.com/v1/projects/<project>/locations/<location>/clusters/<name>`.
func projectFromSelfLink(link string) (string, bool) {
	parts := strings.Split(link, "/")
	for i, part := range parts {
		if part == "projects" && i+1 < len(parts) && parts[i+1] != "" {
			return parts[i+1], true
		}
	}
	return "", false
}

// emitCluster emits the cluster node, its claims and its pointer — and nothing below it (FR-030).
func (f *Feeder) emitCluster(ctx context.Context, desc feeder.Description, em feeder.Emitter,
	cluster *container.Cluster, project string, at time.Time) error {
	obs, err := ObserveCluster(cluster, project, f.opts.Labels)
	if err != nil {
		return err
	}
	props, err := obs.Props().Build()
	if err != nil {
		return err
	}
	node := obs.NodeFact(at)
	node.Props = props
	node.Pointers = obs.Pointers()
	node.SourceObservedAt = at
	id, node, err := f.stateAssertion(desc.SourceID, "gke_cluster", obs.Cluster.Value(), node, at, time.Time{})
	if err != nil {
		return err
	}
	if err := emit(ctx, em, feeder.UpsertNode(desc, id, node)); err != nil {
		return err
	}
	return f.emitClaims(ctx, desc, em, obs.Cluster.Ref(), obs.Claims(), obs.Labels.Environment, at)
}

// emitConfiguration emits a revision's configuration version, the configuration change since the
// previous revision, and the dependencies its configuration derives or proposes (T134–T137).
//
// It is called from the revision emitter rather than from a payload of its own, because a
// configuration is not a thing GCP returns separately: a Cloud Run **revision** is the immutable
// snapshot of what was deployed, so it arrives inside the `revisions` payload and is read out of it.
//
// # The comparison is by createTime and not by arrival
//
// `revisions.list` returns a service's revisions in one page, newest first, so comparing in arrival
// order would produce a spurious "configuration changed" from the new revision back to the old one.
// The cache therefore holds the **latest by createTime**, and a revision older than the one cached
// updates nothing and emits no change. That also makes this step commute, which the conformance
// shuffle requires: two revisions delivered in either order leave the same state and the same events.
func (f *Feeder) emitConfiguration(ctx context.Context, desc feeder.Description, em feeder.Emitter,
	obs ConfigObservation, at time.Time) error {
	if obs.CreateTime.IsZero() {
		return fmt.Errorf("gcp: revision %s has no createTime, so the configuration it snapshots has "+
			"no instant it became true; the field is output-only and documented, so a missing one is an "+
			"error rather than an unknown start (FR-033)", obs.Revision.Value())
	}
	props, err := obs.Props().Build()
	if err != nil {
		return err
	}
	node := obs.NodeFact()
	node.Props = props
	node.SourceObservedAt = at
	if err := emit(ctx, em, feeder.UpsertNode(desc,
		feeder.NewID(desc.SourceID, "config", obs.Ref().GetValue()), node)); err != nil {
		return err
	}
	if err := f.emitClaims(ctx, desc, em, obs.Ref(), obs.Claims(), "", at); err != nil {
		return err
	}
	// The configuration runs on the revision it is the snapshot of, which is what puts it at hop 0 of
	// the revision and hop 1 of the service in a blast radius.
	if err := emit(ctx, em, feeder.UpsertEdge(desc,
		feeder.NewID(desc.SourceID, "edge", "config-runs-on", obs.Ref().GetValue()),
		feeder.EdgeFact{
			Meta:    feeder.Meta{SourceObservedAt: at},
			Src:     obs.Ref(),
			Dst:     obs.Revision.Ref(),
			Type:    graphv1.EdgeType_RUNS_ON,
			ValidAt: obs.CreateTime,
		})); err != nil {
		return err
	}

	service := obs.Revision.Service.Value()
	f.mu.Lock()
	previous, had := f.configs[service]
	newer := !had || obs.CreateTime.After(previous.CreateTime)
	if newer {
		f.configs[service] = obs
	}
	instances := make([]SQLInstance, 0, len(f.instances))
	for _, instance := range f.instances {
		instances = append(instances, instance.Instance)
	}
	f.mu.Unlock()
	sort.Slice(instances, func(i, j int) bool { return instances[i].Value() < instances[j].Value() })

	if err := f.emitDependencies(ctx, desc, em, obs, instances, at); err != nil {
		return err
	}

	// The configuration change. The first configuration this feeder sees asserts and emits no change:
	// a configuration that is new to the connector is not a configuration that just changed. Nor does
	// an *older* revision arriving later produce one — it is history, not a change.
	if !had || !newer {
		return nil
	}
	diff := DiffConfig(previous, obs)
	if diff.Empty() {
		return nil
	}
	actor := f.opts.Actors.Classify(f.authFor(obs.Revision), false)
	change, err := ConfigChange(previous, obs, diff, actor)
	if err != nil {
		return err
	}
	if _, err := ConfigChangeProps(previous, obs, diff, actor).Build(); err != nil {
		return err
	}
	change.SourceObservedAt = at
	return emit(ctx, em, feeder.ObserveChange(desc,
		feeder.NewID(desc.SourceID, "change", change.Ref.GetValue()), change))
}

// emitDependencies emits the derived `depends-on` edges and the proposals (T134, T135).
func (f *Feeder) emitDependencies(ctx context.Context, desc feeder.Description, em feeder.Emitter,
	config ConfigObservation, instances []SQLInstance, at time.Time) error {
	deps := DeriveDependencies(config, instances)
	for _, derived := range deps.Derived {
		edge, err := derived.EdgeFact(config.CreateTime)
		if err != nil {
			return err
		}
		edge.SourceObservedAt = at
		if err := emit(ctx, em, feeder.UpsertEdge(desc,
			feeder.NewID(desc.SourceID, "edge", "depends-on", derived.Key()), edge)); err != nil {
			return err
		}
	}
	for _, proposal := range deps.Proposed {
		f.mu.Lock()
		raised := f.proposals[proposal.Key()]
		f.proposals[proposal.Key()] = true
		if !raised {
			// Gathered now and emitted at the NEXT poll marker. See emitPendingProposals.
			f.pendingProposals = append(f.pendingProposals, proposal)
		}
		f.mu.Unlock()
	}
	return nil
}

// emitPendingProposals emits the dependency proposals gathered during the **previous** cycle, and
// promotes this cycle's to be emitted at the next one (FR-029).
//
// # Why a proposal waits a cycle, when nothing else does
//
// A proposal is refused by the graph if either endpoint is unknown: "a proposed dependency is a
// question about two known things and may not create either". That is the right rule — a proposal
// that minted a placeholder would be inventing the thing it was asking about — and it makes a
// proposal the one event this feeder emits whose delivery order **matters**.
//
// Every other cross-referencing event tolerates reordering because `upsert_edge` mints a placeholder
// endpoint and the real node supersedes it. A proposal cannot, so emitting it in the same cycle as the
// nodes it names is only correct if the graph receives them in order — and the feeder declares
// `ordering: none`, so it does not. The conformance shuffle finds this immediately: permuting the
// events of one poll delivers the proposal before the service, and the graph refuses it.
//
// Holding it one cycle is what makes it safe without weakening the graph's rule. The endpoints were
// asserted in a cycle that has since been checkpointed, so they are in the graph whatever order this
// cycle's events arrive in. The cost is that a suggestion appears one poll interval after the evidence
// for it — which for something a human reviews at their leisure is not a cost at all.
func (f *Feeder) emitPendingProposals(ctx context.Context, desc feeder.Description, em feeder.Emitter, at time.Time) error {
	f.mu.Lock()
	ready := f.readyProposals
	f.readyProposals = f.pendingProposals
	f.pendingProposals = nil
	// The cycle boundary for the once-per-cycle rule. It is cleared here rather than never, which
	// would make the rule "once per process" and leave a restart as the only way to re-raise.
	f.proposals = map[string]bool{}
	f.mu.Unlock()

	for _, proposal := range ready {
		fact, err := proposal.Fact()
		if err != nil {
			return err
		}
		fact.SourceObservedAt = at
		// The event id is deterministic from the proposal's triple, so a proposal the feeder keeps
		// finding is one event the log collapses — and the durable half of "not re-raised while
		// pending" is the projector's: applyProposeDependency declines to refile a proposal that is
		// already open, so a decision survives replay and outranks any later automated match
		// (FR-122).
		if err := emit(ctx, em, feeder.ProposeDependency(desc,
			feeder.NewID(desc.SourceID, "propose", proposal.Key()), fact)); err != nil {
			return err
		}
	}
	return nil
}

// emitAuditChanges emits the general audit changes: every in-scope admin-activity entry that no
// specialised path consumed (T141, T144, T146–T148, T150).
//
// It runs at the **poll marker** because "nothing consumed this" is only knowable once the cycle's
// payloads have all been applied. An entry that dated a traffic shift or a Cloud SQL configuration
// change was taken by that path and is not emitted again here; an entry that nothing used becomes a
// change carrying GCP's own operation name, which is the catch-all US6 exists to provide.
//
// The lag samples go into the same distribution the checkpoint reports (§5.2), so the configured
// reordering window is justified from data rather than asserted.
func (f *Feeder) emitAuditChanges(ctx context.Context, desc feeder.Description, em feeder.Emitter, at time.Time) error {
	f.mu.Lock()
	unconsumed := f.audit.Unconsumed(at)
	// The watermark moves only now: an entry read during this cycle is not late, and an entry that
	// arrives in a later cycle with an instant before this extent is.
	f.audit.Watermark(at)
	f.mu.Unlock()

	for _, change := range unconsumed {
		actor := f.opts.Actors.Classify(change.Auth, false)
		fact, err := AuditChangeFact(change, actor)
		if err != nil {
			return err
		}
		if _, err := AuditChangeProps(change, actor).Build(); err != nil {
			return err
		}
		fact.SourceObservedAt = at
		if err := emit(ctx, em, feeder.ObserveChange(desc,
			feeder.NewID(desc.SourceID, "change", fact.Ref.GetValue()), fact)); err != nil {
			return err
		}
		if err := f.emitClaims(ctx, desc, em, fact.Ref, AuditClaims(change), "", at); err != nil {
			return err
		}
		if change.Late {
			// The change is emitted at its own instant; the checkpoint is where a reader learns the
			// graph found out late, which is what feature 002's reopening path reads (§5.2).
			f.mu.Lock()
			f.lateArrivals = append(f.lateArrivals, fmt.Sprintf("%s at %s (%s)",
				change.MethodName, instant(change.At), change.Key))
			f.mu.Unlock()
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Load balancers and DNS (T167–T170)
// ---------------------------------------------------------------------------
//
// The four compute payloads are gathered and applied together at the poll marker, because the chain
// crosses all of them: a forwarding rule alone names no service, and a NEG alone names no load
// balancer. Applying them one at a time would mean emitting an exposure as soon as the last piece
// happened to arrive, which makes the events depend on payload order — and this feeder declares
// `ordering: none`.

// applyForwardingRules reads one `forwardingRules.list` response and emits the load-balancer nodes.
func (f *Feeder) applyForwardingRules(ctx context.Context, desc feeder.Description, em feeder.Emitter, raw []byte, at time.Time) error {
	var payload struct {
		Items []*compute.ForwardingRule `json:"items"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return fmt.Errorf("gcp: decoding a %s payload: %w", PayloadForwardingRules, err)
	}
	project := f.firstProject()
	for _, rule := range payload.Items {
		if rule == nil {
			continue
		}
		obs, err := ObserveForwardingRule(rule, project)
		if err != nil {
			return err
		}
		props, err := obs.Props().Build()
		if err != nil {
			return err
		}
		node := obs.NodeFact(at)
		node.Props = props
		node.SourceObservedAt = at
		id, node, err := f.stateAssertion(desc.SourceID, "load_balancer", obs.LoadBalancer.Value(), node, at, time.Time{})
		if err != nil {
			return err
		}
		if err := emit(ctx, em, feeder.UpsertNode(desc, id, node)); err != nil {
			return err
		}
		if err := f.emitClaims(ctx, desc, em, obs.LoadBalancer.Ref(), obs.Claims(), "", at); err != nil {
			return err
		}
		f.mu.Lock()
		f.forwardingRules = append(f.forwardingRules, obs)
		f.mu.Unlock()
	}
	return nil
}

// applyURLMaps, applyBackendServices and applyNEGs gather the rest of the chain. They emit nothing:
// none of the three is an entity this connector models, and each exists only to make the exposure
// derivable. A URL map as a node would be a node nobody asks a question about.
func (f *Feeder) applyURLMaps(raw []byte) error {
	var payload struct {
		Items []*compute.UrlMap `json:"items"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return fmt.Errorf("gcp: decoding a %s payload: %w", PayloadURLMaps, err)
	}
	f.mu.Lock()
	f.urlMaps = append(f.urlMaps, payload.Items...)
	f.mu.Unlock()
	return nil
}

func (f *Feeder) applyBackendServices(raw []byte) error {
	var payload struct {
		Items []*compute.BackendService `json:"items"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return fmt.Errorf("gcp: decoding a %s payload: %w", PayloadBackendServices, err)
	}
	f.mu.Lock()
	f.backendServices = append(f.backendServices, payload.Items...)
	f.mu.Unlock()
	return nil
}

func (f *Feeder) applyNEGs(raw []byte) error {
	var payload struct {
		Items []*compute.NetworkEndpointGroup `json:"items"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return fmt.Errorf("gcp: decoding a %s payload: %w", PayloadNetworkEndpointGroups, err)
	}
	f.mu.Lock()
	f.negs = append(f.negs, payload.Items...)
	f.mu.Unlock()
	return nil
}

// applyDNSRecordSets reads one `resourceRecordSets.list` response.
//
// It emits **no change of its own**: a poll sees a record, not a switch. The record is remembered, and
// the switch is emitted when the audit entry that dates it arrives — which is the same division of
// labour as a traffic shift and a Cloud SQL flag, and for the same reason.
func (f *Feeder) applyDNSRecordSets(raw []byte, at time.Time) error {
	var payload struct {
		Zone string                   `json:"zone"`
		Sets []*dns.ResourceRecordSet `json:"rrsets"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return fmt.Errorf("gcp: decoding a %s payload: %w", PayloadDNSRecordSets, err)
	}
	project := f.firstProject()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, set := range payload.Sets {
		if set == nil {
			continue
		}
		record := ObserveDNSRecord(set, project, payload.Zone)
		previous, had := f.dnsRecords[record.Key()]
		f.dnsRecords[record.Key()] = record
		if had && !slices.Equal(previous.Targets, record.Targets) {
			// The targets moved. Held until the audit entry dates it — and if none ever arrives the
			// checkpoint says so, rather than the switch being dated at the poll.
			f.dnsPending[record.Key()] = previous.Targets
		}
		_ = at
	}
	return nil
}

// emitExposures derives the `exposed-via` relations from everything gathered this cycle (T167).
func (f *Feeder) emitExposures(ctx context.Context, desc feeder.Description, em feeder.Emitter, at time.Time) error {
	f.mu.Lock()
	input := ExposureInput{
		Rules:           append([]ForwardingRuleObservation(nil), f.forwardingRules...),
		URLMaps:         append([]*compute.UrlMap(nil), f.urlMaps...),
		BackendServices: append([]*compute.BackendService(nil), f.backendServices...),
		NEGs:            append([]*compute.NetworkEndpointGroup(nil), f.negs...),
		Project:         f.firstProject(),
	}
	f.mu.Unlock()
	if len(input.Rules) == 0 {
		return nil
	}

	for _, exposure := range DeriveExposures(input) {
		edge, err := exposure.EdgeFact(at)
		if err != nil {
			return err
		}
		edge.SourceObservedAt = at
		if err := emit(ctx, em, feeder.UpsertEdge(desc,
			feeder.NewID(desc.SourceID, "edge", "exposed-via", exposure.Key()), edge)); err != nil {
			return err
		}
	}
	return nil
}

// firstProject is the project a compute or DNS response is about.
//
// Those APIs are called per project and their responses do not always name one — `items` is a bare
// list — so the scope supplies it. It is the scope's **first** project rather than a guess, and a
// multi-project run reads them one project at a time, which is what the per-project call structure
// already forces.
func (f *Feeder) firstProject() string {
	if len(f.opts.Scope.Projects) == 0 {
		return ""
	}
	return f.opts.Scope.Projects[0]
}

// emitDNSSwitches emits the DNS changes whose audit entry has arrived (T168, FR-056).
//
// A poll sees a record and not a switch, so the **what** comes from comparing two polls and the **when**
// and **who** come from the audit entry — the same division of labour as a traffic shift, and for the
// same reason: the DNS poll runs every thirty minutes, which is long enough for a change dated at the
// poll to make a reader exonerate the wrong thing.
//
// A switch with no audit entry stays pending and the checkpoint says so. It is not dated at the poll and
// it is not dropped.
func (f *Feeder) emitDNSSwitches(ctx context.Context, desc feeder.Description, em feeder.Emitter, at time.Time) error {
	f.mu.Lock()
	pending := make(map[string][]string, len(f.dnsPending))
	for key, previous := range f.dnsPending {
		pending[key] = previous
	}
	records := make(map[string]DNSRecord, len(f.dnsRecords))
	for key, record := range f.dnsRecords {
		records[key] = record
	}
	f.mu.Unlock()

	for _, key := range slices.Sorted(maps.Keys(pending)) {
		record, known := records[key]
		if !known {
			continue
		}
		f.mu.Lock()
		completion := f.audit.TakeDNSChange(record.Project)
		f.mu.Unlock()
		if completion == nil {
			// Held. The checkpoint states it rather than the switch being dated at the poll.
			f.mu.Lock()
			f.heldDNS = append(f.heldDNS, fmt.Sprintf("%s %s: targets moved and no Cloud DNS audit "+
				"entry has arrived to date it", record.Type, record.Name))
			f.mu.Unlock()
			continue
		}
		actor := f.opts.Actors.Classify(completion.Auth, false)
		f.mu.Lock()
		fronting := LoadBalancersAnswering(f.forwardingRules, record.Targets, pending[key])
		f.mu.Unlock()
		change, err := DNSSwitch(record, pending[key], completion.At, actor, fronting)
		if err != nil {
			return err
		}
		if _, err := DNSSwitchProps(record, pending[key], actor).Build(); err != nil {
			return err
		}
		f.consumeAuditEntry(completion.OperationID)
		change.SourceObservedAt = at
		if err := emit(ctx, em, feeder.ObserveChange(desc,
			feeder.NewID(desc.SourceID, "change", change.Ref.GetValue()), change)); err != nil {
			return err
		}
		f.mu.Lock()
		delete(f.dnsPending, key)
		f.mu.Unlock()
	}
	return nil
}
