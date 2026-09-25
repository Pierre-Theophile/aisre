// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/gcpx"
)

// Checkpoints, the silence rule and the history horizon (T060–T062, FR-012, FR-023, FR-024, FR-026,
// contracts/gcp-feeder.md §8).
//
// # A partial poll retracts nothing
//
// §8.1, and it is the single rule that keeps a network blip from looking like the organisation
// deleting half its estate. A poll that fails part-way:
//
//   - produces a checkpoint stating the extent **actually** covered;
//   - declares the gap;
//   - emits **no retraction** for the part it did not read;
//   - and does **not count** toward the silence rule's consecutive-poll total, because only a
//     *complete* poll is evidence of absence.
//
// The last clause is the one an implementation loses. Counting a partial poll toward N means three
// flaky polls retract a live service, and the retraction is indistinguishable from a real deletion
// once it is in the graph. So `Poll.Complete` is a required field with no default, and
// `SilenceTracker.Observe` takes the whole Poll rather than a list of names — a caller cannot report
// what it saw without also saying whether it saw everything.

// PollOutcome is whether a poll covered its scope. There is no zero value that means "complete":
// `PollUnknown` is the zero value and is refused, because a partial poll silently typed as complete
// is the retraction bug above.
type PollOutcome uint8

// The published poll outcomes.
const (
	// PollUnknown is the zero value and is an error. See the type comment.
	PollUnknown PollOutcome = iota
	// PollComplete means the poll read its whole scope. Only a complete poll is evidence of
	// absence.
	PollComplete
	// PollPartial means the poll failed part-way. It retracts nothing and counts toward nothing.
	PollPartial
)

// String renders the outcome as a checkpoint records it.
func (o PollOutcome) String() string {
	switch o {
	case PollComplete:
		return "complete"
	case PollPartial:
		return "partial"
	default:
		return "unknown"
	}
}

// Scope is the project and region scope in force. It is recorded in every checkpoint so a later
// reader can tell "not present" from "not in scope at that time" — and so the scope is changeable
// without losing history, which matters because recording starts before the campaign scope is agreed
// (FR-130, FR-009).
type Scope struct {
	Projects []string
	Regions  []string
}

// String renders the scope canonically, sorted, for a checkpoint.
func (s Scope) String() string {
	projects := append([]string(nil), s.Projects...)
	regions := append([]string(nil), s.Regions...)
	sort.Strings(projects)
	sort.Strings(regions)
	return "projects=[" + strings.Join(projects, ",") + "] regions=[" + strings.Join(regions, ",") + "]"
}

// Contains reports whether a project and region were in scope.
func (s Scope) Contains(project, region string) bool {
	return containsString(s.Projects, project) && containsString(s.Regions, region)
}

func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// Poll is one poll of one area, and what it covered.
type Poll struct {
	// Outcome says whether the scope was fully read. Required.
	Outcome PollOutcome
	// Scope is what was meant to be read.
	Scope Scope
	// Covered is the (project, region) pairs actually read, as `project/region`. On a complete
	// poll it is the whole scope; on a partial one it is the prefix that succeeded, which is what
	// the checkpoint's extent statement is built from.
	Covered []string
	// Observed is the entity identifiers seen, by namespace. Only used from a complete poll.
	Observed map[string][]string
	// At is when the poll ran.
	At time.Time
	// Err is why a partial poll stopped, for the checkpoint's gap declaration. A partial poll with
	// no reason is refused: "we did not finish and we will not say why" is not a checkpoint.
	Err error
}

// Validate refuses a poll that cannot be recorded honestly.
func (p Poll) Validate() error {
	switch p.Outcome {
	case PollUnknown:
		return fmt.Errorf("gcp: a poll with no outcome; a partial poll typed as complete retracts " +
			"live entities, so the outcome has no default (FR-012)")
	case PollPartial:
		if p.Err == nil {
			return fmt.Errorf("gcp: a partial poll with no reason; the checkpoint has to declare the gap (FR-012)")
		}
	}
	if p.At.IsZero() {
		return fmt.Errorf("gcp: a poll with no instant")
	}
	return nil
}

// SilenceTracker implements the silence rule (FR-023): an entity is retracted after N consecutive
// **complete** polls that did not observe it, with a valid end inside the last poll it *was* observed
// in.
//
// # Why the valid end is inside the last poll that saw it
//
// The obvious choice — the instant of the poll that noticed the absence — is wrong by up to N poll
// intervals. The entity was gone before that; the feeder only just noticed. Placing the end inside
// the last poll that saw it is the tightest interval the evidence supports, and it is the interval a
// query "what was serving at 14:30" needs to get right.
//
// The audit log wins where it states a deletion instant: a `DeleteService` entry is GCP stating when,
// and a stated instant beats an inferred bound (FR-023).
type SilenceTracker struct {
	// Threshold is N. It comes from config (`silence_rule_consecutive_polls: 3`).
	Threshold int
	// missed counts consecutive complete polls that did not observe each entity.
	missed map[string]int
	// lastSeen is when each entity was last observed, and in which poll window.
	lastSeen map[string]seen
}

type seen struct {
	// At is the instant of the poll that observed it.
	At time.Time
	// WindowStart is the start of that poll's window, so the retraction's valid end can be placed
	// inside it rather than at its edge.
	WindowStart time.Time
}

// NewSilenceTracker returns a tracker with threshold N. A threshold below 1 is refused: a threshold
// of zero retracts everything on the first poll that misses it, which is the flake-is-a-deletion bug
// with the safety rail removed.
func NewSilenceTracker(threshold int) (*SilenceTracker, error) {
	if threshold < 1 {
		return nil, fmt.Errorf("gcp: silence threshold %d; below 1 a single missed poll retracts a "+
			"live entity (FR-023)", threshold)
	}
	return &SilenceTracker{
		Threshold: threshold,
		missed:    map[string]int{},
		lastSeen:  map[string]seen{},
	}, nil
}

// Retraction is one entity the silence rule says to retract.
type Retraction struct {
	// Namespace and Value identify the entity.
	Namespace, Value string
	// ValidEnd is when it stopped being true: inside the last poll that observed it, or the
	// audit-log deletion instant where one was stated.
	ValidEnd time.Time
	// Why records which of those two supplied the instant, so a reader can tell an observed
	// deletion from an inferred one.
	Why string
	// MissedPolls is how many consecutive complete polls missed it, which is the evidence.
	MissedPolls int
}

// The published retraction reasons.
const (
	// RetractedBySilence means N consecutive complete polls did not observe it.
	RetractedBySilence = "N consecutive complete polls did not observe it; valid end placed inside the last poll that did"
	// RetractedByAuditDeletion means the audit log stated a deletion instant, which wins.
	RetractedByAuditDeletion = "the audit log states the deletion instant, which wins over an inferred bound (FR-023)"
)

// Observe records one poll and returns the retractions it licenses (T061).
//
// A **partial** poll returns no retractions and advances no counter. That is the whole of §8.1's
// second clause, and it is here rather than at the call site so that there is no caller that can
// forget it.
func (t *SilenceTracker) Observe(poll Poll, windowStart time.Time) ([]Retraction, error) {
	if err := poll.Validate(); err != nil {
		return nil, err
	}
	if poll.Outcome != PollComplete {
		// No retraction, and no counter movement. A partial poll is not evidence of absence.
		return nil, nil
	}

	present := map[string]bool{}
	for namespace, values := range poll.Observed {
		for _, value := range values {
			key := namespace + "=" + value
			present[key] = true
			t.missed[key] = 0
			t.lastSeen[key] = seen{At: poll.At, WindowStart: windowStart}
		}
	}

	var out []Retraction
	keys := make([]string, 0, len(t.missed))
	for key := range t.missed {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		if present[key] {
			continue
		}
		// Only entities inside the polled scope may be missed. An entity in a project that is no
		// longer in scope is "not in scope", not "absent" — and retracting it would turn a scope
		// change into a mass deletion (FR-009).
		if !t.inScope(key, poll.Scope) {
			continue
		}
		t.missed[key]++
		if t.missed[key] < t.Threshold {
			continue
		}
		last := t.lastSeen[key]
		namespace, value := splitKey(key)
		out = append(out, Retraction{
			Namespace:   namespace,
			Value:       value,
			ValidEnd:    endInside(last),
			Why:         RetractedBySilence,
			MissedPolls: t.missed[key],
		})
		delete(t.missed, key)
		delete(t.lastSeen, key)
	}
	return out, nil
}

// endInside returns a valid end inside the last poll that observed the entity: the instant of that
// poll itself.
//
// "Inside" rather than "at the start of the window" because the poll observed it *then* — the entity
// was alive at that instant, and the earliest moment it can be said to have stopped being true is
// immediately after. Using the window start would claim it was already gone when the poll saw it,
// which contradicts the observation the interval is built from.
func endInside(last seen) time.Time {
	if last.At.IsZero() {
		return last.WindowStart
	}
	return last.At
}

// inScope reports whether the entity's identifier places it inside the polled scope. Identifiers are
// `<project>/<region>/…` by construction (identity.go), which is what makes this answerable without
// a lookup.
func (t *SilenceTracker) inScope(key string, scope Scope) bool {
	_, value := splitKey(key)
	parts := strings.Split(value, "/")
	if len(parts) < 2 {
		// An identifier that does not carry coordinates cannot be placed. It is treated as in
		// scope, which is the conservative direction here: the alternative is never retracting it.
		return true
	}
	return scope.Contains(parts[0], parts[1])
}

func splitKey(key string) (namespace, value string) {
	if idx := strings.Index(key, "="); idx >= 0 {
		return key[:idx], key[idx+1:]
	}
	return "", key
}

// RetractAtAuditDeletion overrides the inferred bound with GCP's stated deletion instant, which wins
// (FR-023). It returns the retraction to emit and drops the entity from the tracker, so the silence
// rule does not later retract it a second time at a worse instant.
func (t *SilenceTracker) RetractAtAuditDeletion(namespace, value string, at time.Time) Retraction {
	key := namespace + "=" + value
	delete(t.missed, key)
	delete(t.lastSeen, key)
	return Retraction{
		Namespace: namespace,
		Value:     value,
		ValidEnd:  at,
		Why:       RetractedByAuditDeletion,
	}
}

// Horizon is the revision history horizon (FR-024): how far back the feeder looked.
//
// A query reaching past it is **told so** rather than answered with what happens to be in the graph.
// That is the difference between "there was no revision then" and "we did not look that far back",
// and the second is the answer an investigation needs when it is reasoning about a regression that
// predates the connector.
type Horizon struct {
	// Earliest is the oldest instant the feeder read revisions from.
	Earliest time.Time
	// Reason names why it stops there: a configured limit, the oldest revision Cloud Run returned,
	// or the instant the connector was first run.
	Reason string
}

// The published horizon reasons.
const (
	// HorizonConfigured means a configured limit stopped the walk.
	HorizonConfigured = "the configured revision history horizon"
	// HorizonExhausted means Cloud Run returned no older revision, so the horizon is the estate's
	// own age rather than a limit.
	HorizonExhausted = "Cloud Run returned no older revision; this is the estate's age, not a limit"
	// HorizonFirstRun means the connector had not run before this instant.
	HorizonFirstRun = "the connector was first run at this instant"
)

// ErrPastHorizon is what a query reaching past the horizon gets. It names the horizon and why, so the
// caller can say which it is.
type ErrPastHorizon struct {
	Asked   time.Time
	Horizon Horizon
}

func (e *ErrPastHorizon) Error() string {
	return fmt.Sprintf("gcp: %s is before the revision history horizon %s (%s); "+
		"this is \"we did not look that far back\", not \"there was no revision\" (FR-024)",
		instant(e.Asked), instant(e.Horizon.Earliest), e.Horizon.Reason)
}

// Check returns an *ErrPastHorizon when asked is before the horizon.
func (h Horizon) Check(asked time.Time) error {
	if asked.Before(h.Earliest) {
		return &ErrPastHorizon{Asked: asked, Horizon: h}
	}
	return nil
}

// Checkpoint is what §8 requires a checkpoint to carry beyond its extent. Every field answers a
// question a later reader asks, and the table in §8 is the list.
type Checkpoint struct {
	// From and To bound the extent actually covered, half-open.
	From, To time.Time
	// GapBefore marks that the feeder was not watching immediately before From.
	GapBefore bool
	// Outcome is whether the poll behind this checkpoint was complete.
	Outcome PollOutcome
	// Scope is the project and region scope in force (FR-009).
	Scope Scope
	// Horizon is the revision history horizon (FR-024).
	Horizon Horizon
	// AuditFilters is the log type, service and operation filters in force (FR-040), so a reader
	// can tell "no change happened" from "we were not looking for that kind of change".
	AuditFilters []string
	// EnvironmentMapping is the mapping that produced each node's environment, since this
	// organisation has no uniform label.
	EnvironmentMapping map[string]string
	// ReorderingWindow is what the feeder declared, and ObservedLag the distribution it measured
	// (§5.2), so a reader can tell whether a gap was expected.
	ReorderingWindow time.Duration
	ObservedLagP50   time.Duration
	ObservedLagP99   time.Duration
	// OmittedSurfaces are the areas deliberately not read (FR-057), as a scope statement rather
	// than a failure.
	OmittedSurfaces []string
	// MonitoringLimit is the discovered Monitoring quota limit, so a reader knows what a "share of
	// remaining" was a share of (budget.md §4). Zero means it was not discovered on this run.
	MonitoringLimit int
	// HeldSplits are traffic shifts observed but not yet dated (§3.2 consequence 1). The
	// checkpoint says so, which is the clause that makes holding honest rather than silent.
	HeldSplits []HeldSplit
	// DroppedLabels are label keys that became nothing, so "why is my label not in the graph" has
	// a findable answer.
	DroppedLabels []string
	// Recreations are services deleted and recreated under one name during this extent. A
	// retraction event has nowhere to say why, so the statement that the two are not one
	// continuous entity lives here (FR-025).
	Recreations []Recreation
	// Suppressed are the alert transitions the published filter dropped during this extent
	// (FR-052). They are in the checkpoint because a suppression must be STATED rather than
	// applied silently: an operator asking "the monitor flapped six times, where are they" needs
	// an answer, and "the feeder dropped them" is only an answer if it says which and why.
	Suppressed []SuppressedTransition
	// HeldSQLConfigs are Cloud SQL configuration diffs observed and not yet dated (T131). The same
	// clause that makes holding a traffic shift honest makes this one honest: "the settings changed
	// and we do not yet know when" is a real state of the world, and it is stated.
	HeldSQLConfigs []HeldSQLConfig
	// HeldMaintenance are maintenance operations with no instance ref to attach to, because the
	// operation states no region and no instance of that name was observed in this run.
	HeldMaintenance []HeldMaintenance
	// PassedInSilence are announced maintenance windows that stopped being announced after their
	// start had passed. Nothing was emitted for them — the passage of time is not an observation
	// (FR-066) — so this is the only place a reader learns the window is no longer announced.
	PassedInSilence []string
	// MissingZones are the zones GCP itself said a cluster list may be missing. A cluster list
	// missing a zone reads exactly like a zone with no clusters, which is the confusion FR-012
	// exists to prevent.
	MissingZones []string
	// AuditScope is the service and operation scope the audit stream was read under (FR-040). It is
	// what lets a later reader tell "no change happened" from "we were not looking for that kind of
	// change" — the two are indistinguishable without it, and the second is the one that quietly
	// loses an investigation.
	AuditScope string
	// LateArrivals are the audit changes emitted for entries that arrived after the extent they
	// belong to had been checkpointed. Each was emitted at its own instant rather than clamped
	// forward or dropped; this is where the gap is stated (§5.2), and it is what feature 002's
	// reopening path reads.
	LateArrivals []string
	// Deferred are the areas the budget dropped, in the published order (FR-149). Reduced coverage is
	// a **scope statement** and not a silence: a reader who cannot see that the load balancers were
	// dropped will read their absence as "there are none".
	Deferred []string
	// StopReason is why the poll stopped. The one distinction that must never be lost: a poll that
	// ran out of quota and a poll that found nothing are opposite conclusions, and only the second is
	// evidence of absence (FR-149, gcpx.StopReason.IsAbsence).
	StopReason gcpx.StopReason
	// HeldDNSSwitches are record sets whose targets moved with no audit entry to date the switch. The
	// same clause that makes holding a traffic shift honest makes this one honest.
	HeldDNSSwitches []string
	// PartialReason is why a partial poll stopped.
	PartialReason string
}

// Note renders the checkpoint's human-readable note. It is deterministic — sorted lists, canonical
// instants — because a checkpoint note reaches a golden, and a note that reorders makes a fixture
// fail for no reason.
func (c Checkpoint) Note() string {
	parts := []string{
		"poll=" + c.Outcome.String(),
		"scope=" + c.Scope.String(),
	}
	if !c.Horizon.Earliest.IsZero() {
		parts = append(parts, "horizon="+instant(c.Horizon.Earliest)+" ("+c.Horizon.Reason+")")
	}
	if len(c.AuditFilters) > 0 {
		filters := append([]string(nil), c.AuditFilters...)
		sort.Strings(filters)
		parts = append(parts, "audit_filters=["+strings.Join(filters, ",")+"]")
	}
	if len(c.EnvironmentMapping) > 0 {
		keys := make([]string, 0, len(c.EnvironmentMapping))
		for key := range c.EnvironmentMapping {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		pairs := make([]string, 0, len(keys))
		for _, key := range keys {
			pairs = append(pairs, key+"="+c.EnvironmentMapping[key])
		}
		parts = append(parts, "environment_mapping=["+strings.Join(pairs, ",")+"]")
	}
	parts = append(parts, "reordering_window="+c.ReorderingWindow.String())
	if c.ObservedLagP99 > 0 {
		parts = append(parts, "observed_lag_p50="+c.ObservedLagP50.String(), "observed_lag_p99="+c.ObservedLagP99.String())
	}
	if len(c.OmittedSurfaces) > 0 {
		omitted := append([]string(nil), c.OmittedSurfaces...)
		sort.Strings(omitted)
		parts = append(parts, "omitted_surfaces=["+strings.Join(omitted, ",")+"]")
	}
	if c.MonitoringLimit > 0 {
		parts = append(parts, fmt.Sprintf("monitoring_limit=%d", c.MonitoringLimit))
	}
	if len(c.HeldSplits) > 0 {
		parts = append(parts, "held_traffic_shifts=["+SummariseHeld(c.HeldSplits)+"]")
	}
	if len(c.DroppedLabels) > 0 {
		dropped := append([]string(nil), c.DroppedLabels...)
		sort.Strings(dropped)
		parts = append(parts, "dropped_label_keys=["+strings.Join(dropped, ",")+"]")
	}
	if len(c.Recreations) > 0 {
		notes := make([]string, 0, len(c.Recreations))
		for _, rec := range c.Recreations {
			notes = append(notes, rec.String())
		}
		sort.Strings(notes)
		parts = append(parts, "recreated=["+strings.Join(notes, "; ")+"]")
	}
	if len(c.Suppressed) > 0 {
		notes := make([]string, 0, len(c.Suppressed))
		for _, s := range c.Suppressed {
			notes = append(notes, s.String())
		}
		sort.Strings(notes)
		parts = append(parts, "suppressed_transitions=["+strings.Join(notes, "; ")+"]")
	}
	if len(c.HeldSQLConfigs) > 0 {
		parts = append(parts, "held_sql_config_changes=["+SummariseHeldSQL(c.HeldSQLConfigs)+"]")
	}
	if len(c.HeldMaintenance) > 0 {
		notes := make([]string, 0, len(c.HeldMaintenance))
		for _, h := range c.HeldMaintenance {
			notes = append(notes, h.String())
		}
		sort.Strings(notes)
		parts = append(parts, "held_maintenance=["+strings.Join(notes, "; ")+"]")
	}
	if len(c.PassedInSilence) > 0 {
		notes := append([]string(nil), c.PassedInSilence...)
		sort.Strings(notes)
		parts = append(parts, "announced_maintenance_passed_in_silence=["+strings.Join(notes, "; ")+"]")
	}
	if len(c.MissingZones) > 0 {
		zones := append([]string(nil), c.MissingZones...)
		sort.Strings(zones)
		parts = append(parts, "gke_missing_zones=["+strings.Join(zones, ",")+"]")
	}
	if c.AuditScope != "" {
		parts = append(parts, "audit_scope=("+c.AuditScope+")")
	}
	if len(c.LateArrivals) > 0 {
		late := append([]string(nil), c.LateArrivals...)
		sort.Strings(late)
		parts = append(parts, "late_arrivals=["+strings.Join(late, "; ")+"]")
	}
	if len(c.Deferred) > 0 {
		deferred := append([]string(nil), c.Deferred...)
		sort.Strings(deferred)
		parts = append(parts, "deferred_for_budget=["+strings.Join(deferred, ",")+"]")
	}
	if len(c.HeldDNSSwitches) > 0 {
		held := append([]string(nil), c.HeldDNSSwitches...)
		sort.Strings(held)
		parts = append(parts, "held_dns_switches=["+strings.Join(held, "; ")+"]")
	}
	if c.StopReason != "" {
		parts = append(parts, "stop_reason="+string(c.StopReason))
	}
	if c.PartialReason != "" {
		parts = append(parts, "gap_reason="+c.PartialReason)
	}
	return strings.Join(parts, " ")
}

// Validate refuses a checkpoint that would mislead.
//
// The stop-reason rule is the one that matters most here: a **complete** poll may not claim it stopped
// for quota, and a poll that stopped for quota may not claim it was complete. Either would make "we do
// not know" indistinguishable from "we looked", which is the confusion FR-149 exists to prevent.
func (c Checkpoint) Validate() error {
	if c.Outcome == PollUnknown {
		return fmt.Errorf("gcp: a checkpoint with no poll outcome; a reader cannot tell whether " +
			"absence was observed (FR-012)")
	}
	if c.Outcome == PollPartial && c.PartialReason == "" {
		return fmt.Errorf("gcp: a partial checkpoint with no gap reason (FR-012)")
	}
	if c.Outcome == PollComplete && c.StopReason == gcpx.StopQuota {
		return fmt.Errorf("gcp: a checkpoint that claims a complete poll and a quota stop. A poll that " +
			"ran out of its share did not cover its extent, and reporting it as complete would make " +
			"ignorance indistinguishable from an observation (FR-149)")
	}
	if c.StopReason == gcpx.StopQuota && len(c.Deferred) == 0 && c.PartialReason == "" {
		return fmt.Errorf("gcp: a checkpoint that stopped for quota and names neither a deferred area " +
			"nor a gap reason; a reader cannot tell what was not read (FR-149)")
	}
	if c.To.Before(c.From) {
		return fmt.Errorf("gcp: a checkpoint whose extent ends before it starts")
	}
	if len(c.Scope.Projects) == 0 {
		return fmt.Errorf("gcp: a checkpoint with an empty project scope; a reader could not tell " +
			"\"not present\" from \"not in scope\" (FR-009)")
	}
	return nil
}

// Identity is what makes one Cloud Run service *this* service rather than a name (T062, FR-025).
//
// GCP reuses the name: delete `checkout` and create `checkout`, and every field but one is the same.
// The one that is not is `Service.uid` — a server-assigned UUID4, guaranteed unchanged until the
// resource is deleted. So the uid is how a recreation is recognised.
type Identity struct {
	// UID is `Service.uid`.
	UID string
	// CreateTime is when the service was created.
	CreateTime time.Time
	// LastSeen is when the feeder last observed this uid. It is the bound a recreation retracts the
	// predecessor at, so it is carried here rather than recomputed: see Recreation.RetractOldAt.
	LastSeen time.Time
}

// Recreation is a service deleted and recreated under one name (T062).
//
// It is **not** one continuous entity, and treating it as one is the error FR-025 names. The graph
// would hold a single service whose configuration changed at the recreation instant — losing the
// deletion entirely, and attributing the new service's whole history to the old one. An
// investigation asking "what changed" would see a config change where there was a delete and a
// create.
//
// So: the old is retracted, the new is created, and any evidence linking them is emitted as **claims**
// for the resolution layer to decide on. The feeder does not merge them and does not assert they are
// unrelated — it records what it saw and lets the layer whose job is identity decide, which is the
// same division of labour every other identity question in this system uses.
type Recreation struct {
	Service Service
	// OldUID and NewUID are the two server-assigned identifiers.
	OldUID, NewUID string
	// RetractOldAt is when the old service stopped being true: the **last instant its uid was
	// observed**, which is the tightest bound the evidence supports.
	//
	// Not the new service's `createTime`, which is when the successor began and says nothing about
	// when the predecessor ended — the two are separated by however long nobody was looking. And
	// emphatically not the *old* service's own `createTime`, which would close its interval at its
	// own start and erase the entity's whole history; that was this field's first implementation and
	// a fixture caught it.
	//
	// The gap between RetractOldAt and CreateNewAt is genuine ignorance and is left as a gap: the
	// graph holds one identifier with two valid intervals and a hole between them, which is what
	// "not one continuous entity" means bitemporally.
	RetractOldAt time.Time
	// CreateNewAt is the new service's `createTime`.
	CreateNewAt time.Time
	// LinkingEvidence is what suggests the two are related — the shared name, a shared image
	// digest, a shared owning label. It is emitted as claims, never as a merge.
	LinkingEvidence []string
}

// DetectRecreation compares the identity a poll observed against the one the feeder last recorded.
//
// A changed uid under one name is a recreation. An unchanged uid is the same service, whatever else
// moved. A missing uid on either side is **not** a recreation: it is a gap in the evidence, and
// guessing from `createTime` alone would call a service recreated every time a poll read a stale
// cache.
func DetectRecreation(svc Service, previous, current Identity) (*Recreation, bool) {
	if previous.UID == "" || current.UID == "" {
		return nil, false
	}
	if previous.UID == current.UID {
		return nil, false
	}
	retractAt := previous.LastSeen
	if retractAt.IsZero() {
		// Nothing observed the predecessor after its creation, so that is the only bound there is.
		retractAt = previous.CreateTime
	}
	return &Recreation{
		Service:      svc,
		OldUID:       previous.UID,
		NewUID:       current.UID,
		RetractOldAt: retractAt,
		CreateNewAt:  current.CreateTime,
		LinkingEvidence: []string{
			"the two services share the Cloud Run name " + svc.Name + " in " + svc.Project + "/" + svc.Region,
		},
	}, true
}

// String describes a recreation for a checkpoint note, without asserting the two are one entity.
func (r Recreation) String() string {
	return fmt.Sprintf("%s was recreated: uid %s retracted, uid %s created at %s; "+
		"not one continuous entity, and the link is emitted as claims for the resolution layer (FR-025)",
		r.Service.Value(), shortUID(r.OldUID), shortUID(r.NewUID), instant(r.CreateNewAt))
}

// shortUID abbreviates a UUID4 for a note. The whole uid is in the graph; a note is for a human.
func shortUID(uid string) string {
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}
