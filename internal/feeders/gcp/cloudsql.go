// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	sqladmin "google.golang.org/api/sqladmin/v1"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/feeders/vendornotice"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// Cloud SQL: the instances behind the services (T129–T133; FR-031–FR-035; contract §4).
//
// # The client is `google.golang.org/api/sqladmin/v1`, and that is a finding rather than a taste
//
// The preview GAPIC `cloud.google.com/go/sql/apiv1` (v0.1.0) returns `*ApiWarningIterator` from
// `SqlInstancesClient.List` — an iterator over **warnings** rather than over instances. That is a
// code-generation defect, not a different API shape, and it makes the GAPIC unusable for the one
// call this feature needs (research §4). It is recorded here as well as in the contract because it
// is exactly the kind of choice a later contributor would otherwise "modernise" back into a bug.
//
// # Three things that are NOT changes
//
//   - `settings.maintenanceWindow` and `settings.denyMaintenancePeriods` are a recurring **policy**:
//     properties of the instance, never change nodes. A window that says "Sunday 03:00" is not an
//     event that happened on Sunday, and emitting one every poll would fill the change stream with
//     a calendar;
//   - the instance's **state** (`RUNNABLE`, `SUSPENDED`) is a property. A state that changed is a
//     change the audit stream reports, and deriving one from two polls would date it at the poll;
//   - the **flag catalogue** (`flags.list`) is neither a node nor a change. It is what makes a flag
//     change interpretable — "this flag requires a restart", "this one is integer-valued" — and it
//     is held to annotate changes rather than recorded as facts of its own.

// The Cloud SQL payload kinds.
const (
	// PayloadSQLInstances is an `instances.list` response.
	PayloadSQLInstances = "sql_instances"
	// PayloadSQLOperations is an `operations.list` response, filtered on operationType.
	PayloadSQLOperations = "sql_operations"
	// PayloadSQLFlags is a `flags.list` response: the catalogue, not a fact.
	PayloadSQLFlags = "sql_flags"
)

// SQLInstance is a Cloud SQL instance's coordinates.
type SQLInstance struct {
	Project string
	Region  string
	Name    string
}

// Validate refuses coordinates with an empty part.
func (s SQLInstance) Validate() error {
	for name, part := range map[string]string{"project": s.Project, "region": s.Region, "instance": s.Name} {
		if strings.TrimSpace(part) == "" {
			return fmt.Errorf("%w: the %s of a Cloud SQL instance", ErrEmptyIdentifierPart, name)
		}
	}
	return nil
}

// Value renders the instance identifier: `<project>/<region>/<instance>`.
func (s SQLInstance) Value() string { return s.Project + "/" + s.Region + "/" + s.Name }

// Ref returns the instance's addressing ref.
func (s SQLInstance) Ref() *graphv1.Ref { return feeder.Ref(NSSQLInstance, s.Value()) }

// ConnectionName renders the instance connection name GCP publishes:
// `<project>:<region>:<instance>`.
//
// It is the identifier **FR-120's certain rule resolves on**, and the reason is that it is the
// identifier an application is configured with: a Cloud Run service's `--add-cloudsql-instances`,
// a connection string, a sidecar's argument. Two sources naming the same connection name are
// naming the same instance, because the string is globally unique and is what the proxy dials.
func (s SQLInstance) ConnectionName() string { return s.Project + ":" + s.Region + ":" + s.Name }

// The instance properties. `database.version` and `settings.tier` are on the sanitisation table's
// verbatim allowlist as public product identifiers, which is why they are recorded as themselves.
const (
	// PropSQLDatabaseVersion is the engine and version, e.g. `POSTGRES_16`.
	PropSQLDatabaseVersion = "database.version"
	// PropSQLInstalledVersion is the version actually running, which can lag the requested one
	// across a maintenance. Recorded separately because "what we asked for" and "what is serving"
	// are different facts and an incident is usually about the second.
	PropSQLInstalledVersion = "sre.gcp.sql_installed_version"
	// PropSQLTier is the machine type, a public identifier.
	PropSQLTier = "settings.tier"
	// PropSQLState is the instance state as GCP reports it.
	PropSQLState = "sre.gcp.sql_state"
	// PropSQLAvailabilityType is ZONAL or REGIONAL, which is what says whether a zone failure is
	// survivable.
	PropSQLAvailabilityType = "sre.gcp.sql_availability_type"
	// PropSQLConnectionName is the instance connection name, also claimed.
	PropSQLConnectionName = "instance_connection_name"
	// PropSQLBackendType is FIRST_GEN, SECOND_GEN or EXTERNAL.
	PropSQLBackendType = "sre.gcp.sql_backend_type"
	// PropSQLDiskSizeGB and PropSQLDiskType are the storage shape.
	PropSQLDiskSizeGB = "sre.gcp.sql_disk_size_gb"
	PropSQLDiskType   = "sre.gcp.sql_disk_type"
	// PropSQLFlags is the database flags in force, as `name=value` entries. Flag values ARE
	// recorded, and the distinction from an environment variable's value is deliberate: a database
	// flag is a published product setting from a closed catalogue (`flags.list` says which values
	// are even legal), where an environment variable's value is whatever an operator typed and may
	// be a connection string. config.go stores a fingerprint for the second and never the value.
	PropSQLFlags = "sre.gcp.sql_database_flags"
	// PropSQLMaintenanceWindow is the recurring policy — a property, never a change (contract §4).
	PropSQLMaintenanceWindow = "sre.gcp.sql_maintenance_window"
	// PropSQLDenyMaintenancePeriods is the other half of the same policy.
	PropSQLDenyMaintenancePeriods = "sre.gcp.sql_deny_maintenance_periods"
	// PropSQLMaintenanceVersion is the maintenance version in force.
	PropSQLMaintenanceVersion = "sre.gcp.sql_maintenance_version"
	// PropSQLScheduledMaintenanceAt is the announced start of the next maintenance, where GCP
	// announces one. It is a property AND a change: the property says "there is one coming", and
	// the announced change is what a ranker can exclude as not having happened yet.
	PropSQLScheduledMaintenanceAt = "sre.gcp.sql_scheduled_maintenance_at"
	// PropSQLScheduledMaintenanceCanReschedule says whether the window can still be moved, which
	// is what an operator asks first.
	PropSQLScheduledMaintenanceCanReschedule = "sre.gcp.sql_scheduled_maintenance_can_reschedule"
	// PropSQLScheduledMaintenanceDeadline is the instant past which it can no longer be deferred.
	PropSQLScheduledMaintenanceDeadline = "sre.gcp.sql_scheduled_maintenance_deadline"
	// PropSQLValidFromIsABound marks an instance whose creation instant GCP did not report, on the
	// same convention the owner and vendor nodes use: the interval starts at a stated bound rather
	// than at a placeholder nobody can tell from a fact.
	PropSQLValidFromIsABound = "sre.gcp.sql_valid_from_is_a_bound"
)

// InstanceObservation is one Cloud SQL instance as a poll saw it.
type InstanceObservation struct {
	// Instance is the coordinates.
	Instance SQLInstance
	// DatabaseVersion is the requested engine version, InstalledVersion the one running.
	DatabaseVersion  string
	InstalledVersion string
	// Tier, AvailabilityType, BackendType, State and the disk shape are the instance's shape.
	Tier             string
	AvailabilityType string
	BackendType      string
	State            string
	DiskSizeGB       int64
	DiskType         string
	// Flags are the database flags in force, sorted, as `name=value`.
	Flags []string
	// FlagValues is the same set keyed by name, which is what a change diff compares.
	FlagValues map[string]string
	// MaintenanceWindow and DenyMaintenancePeriods are the recurring policy.
	MaintenanceWindow      string
	DenyMaintenancePeriods []string
	MaintenanceVersion     string
	// Scheduled is the announced maintenance, where GCP announces one.
	Scheduled *ScheduledMaintenance
	// CreateTime is the instance's creation instant. Zero means GCP reported none, and the node
	// then starts at a stated bound rather than at a guess.
	CreateTime time.Time
	// Labels is the label policy's verdict on the instance's user labels.
	Labels Labels
	// Addresses are the instance's IP addresses, which the sanitisation table pseudonymises as
	// infrastructure rather than dropping: an address that names a machine serving traffic is a
	// join key, and one that names where a person was sitting is not (contracts/sanitisation.md).
	Addresses []string
}

// ScheduledMaintenance is an announced Cloud SQL maintenance window.
type ScheduledMaintenance struct {
	// StartTime is the announced start — a genuinely future-dated instant, which is what makes
	// this the one place the platform feeder emits an announced fact.
	StartTime time.Time
	// CanReschedule and CanDefer say whether the operator can still move it.
	CanReschedule bool
	CanDefer      bool
	// DeadlineTime is when rescheduling stops being possible.
	DeadlineTime time.Time
}

// ObserveInstance reads one instance.
func ObserveInstance(instance *sqladmin.DatabaseInstance, labels LabelPolicy) (InstanceObservation, error) {
	var obs InstanceObservation
	coords := SQLInstance{
		Project: instance.Project,
		Region:  instance.Region,
		Name:    instance.Name,
	}
	// The connection name is the authority where the three fields disagree with it, because it is
	// the string the proxy dials and the one an application is configured with. A response that
	// carries one is trusted over fields that may be empty.
	if parsed, ok := parseConnectionName(instance.ConnectionName); ok {
		coords = parsed
	}
	if err := coords.Validate(); err != nil {
		return obs, err
	}
	obs.Instance = coords
	obs.DatabaseVersion = instance.DatabaseVersion
	obs.InstalledVersion = instance.DatabaseInstalledVersion
	obs.BackendType = instance.BackendType
	obs.State = instance.State
	obs.MaintenanceVersion = instance.MaintenanceVersion

	if settings := instance.Settings; settings != nil {
		obs.Tier = settings.Tier
		obs.AvailabilityType = settings.AvailabilityType
		obs.DiskSizeGB = settings.DataDiskSizeGb
		obs.DiskType = settings.DataDiskType
		obs.FlagValues = make(map[string]string, len(settings.DatabaseFlags))
		for _, flag := range settings.DatabaseFlags {
			if flag.Name == "" {
				continue
			}
			obs.FlagValues[flag.Name] = flag.Value
		}
		obs.Flags = flagEntries(obs.FlagValues)
		obs.MaintenanceWindow = maintenanceWindowOf(settings.MaintenanceWindow)
		obs.DenyMaintenancePeriods = denyPeriodsOf(settings.DenyMaintenancePeriods)
		obs.Labels = labels.Apply(coords.Project, settings.UserLabels)
	} else {
		obs.FlagValues = map[string]string{}
		obs.Labels = labels.Apply(coords.Project, nil)
	}

	for _, mapping := range instance.IpAddresses {
		if mapping.IpAddress != "" {
			obs.Addresses = append(obs.Addresses, mapping.IpAddress)
		}
	}
	sort.Strings(obs.Addresses)

	if instance.CreateTime != "" {
		at, err := time.Parse(time.RFC3339Nano, instance.CreateTime)
		if err != nil {
			return obs, fmt.Errorf("gcp: instance %s createTime %q: %w", coords.Value(), instance.CreateTime, err)
		}
		obs.CreateTime = at.UTC()
	}

	if scheduled := instance.ScheduledMaintenance; scheduled != nil && scheduled.StartTime != "" {
		start, err := time.Parse(time.RFC3339Nano, scheduled.StartTime)
		if err != nil {
			return obs, fmt.Errorf("gcp: instance %s scheduledMaintenance.startTime %q: %w",
				coords.Value(), scheduled.StartTime, err)
		}
		announced := &ScheduledMaintenance{
			StartTime:     start.UTC(),
			CanReschedule: scheduled.CanReschedule,
			CanDefer:      scheduled.CanDefer,
		}
		if scheduled.ScheduleDeadlineTime != "" {
			deadline, err := time.Parse(time.RFC3339Nano, scheduled.ScheduleDeadlineTime)
			if err != nil {
				return obs, fmt.Errorf("gcp: instance %s scheduleDeadlineTime %q: %w",
					coords.Value(), scheduled.ScheduleDeadlineTime, err)
			}
			announced.DeadlineTime = deadline.UTC()
		}
		obs.Scheduled = announced
	}
	return obs, nil
}

// parseConnectionName reads `<project>:<region>:<instance>`. A string with any other shape yields
// false rather than a partially-filled instance: a ref with an empty part resolves against
// something and names nothing anybody meant.
func parseConnectionName(name string) (SQLInstance, bool) {
	parts := strings.Split(name, ":")
	if len(parts) != 3 {
		return SQLInstance{}, false
	}
	instance := SQLInstance{Project: parts[0], Region: parts[1], Name: parts[2]}
	if instance.Validate() != nil {
		return SQLInstance{}, false
	}
	return instance, true
}

// flagEntries renders the flags as sorted `name=value` entries, so two polls of one instance
// produce the same bytes whatever order the API listed them in.
func flagEntries(values map[string]string) []string {
	out := make([]string, 0, len(values))
	for name, value := range values {
		out = append(out, name+"="+value)
	}
	sort.Strings(out)
	return out
}

// maintenanceWindowOf renders the recurring policy as one readable string. It is a property and
// never a change: a window that says "Sunday 03:00" is a calendar entry, not an event.
func maintenanceWindowOf(window *sqladmin.MaintenanceWindow) string {
	if window == nil || (window.Day == 0 && window.Hour == 0 && window.UpdateTrack == "") {
		return ""
	}
	out := fmt.Sprintf("day=%d hour=%d", window.Day, window.Hour)
	if window.UpdateTrack != "" {
		out += " track=" + window.UpdateTrack
	}
	return out
}

func denyPeriodsOf(periods []*sqladmin.DenyMaintenancePeriod) []string {
	out := make([]string, 0, len(periods))
	for _, period := range periods {
		out = append(out, fmt.Sprintf("%s..%s (%s)", period.StartDate, period.EndDate, period.Time))
	}
	sort.Strings(out)
	return out
}

// Claims returns every identifier this feeder knows the instance by, **including the one it
// addresses it by** (FR-115).
//
// The **instance connection name** is the load-bearing one: FR-120's certain rule resolves two
// sources onto one entity when they claim the same connection name, and it is the identifier an
// application is actually configured with. A feeder that minted the addressing ref and forgot the
// connection name would leave that rule with nothing to fire on — a rule whose supporting claim
// nobody emits is a rule that never fires, and it fails silently.
func (o InstanceObservation) Claims() []Claim {
	claims := []Claim{
		{Namespace: NSSQLInstance, Value: o.Instance.Value(),
			Why: "the ref this feeder addresses the Cloud SQL instance by"},
		{Namespace: NSSQLInstance, Value: o.Instance.ConnectionName(),
			Why: "the instance connection name, which is what an application is configured with " +
				"and what FR-120's certain rule resolves on"},
		{Namespace: NSSQLInstance,
			Value: "projects/" + o.Instance.Project + "/instances/" + o.Instance.Name,
			Why:   "the fully qualified Cloud SQL resource name"},
	}
	// The supporting attributes the published rules read, on **every** claim rather than only on the
	// claim whose value is the connection name. That is the case C7 exists for: another source
	// addresses the instance by its resource name or by a name an operator typed, and carries the
	// connection name alongside — so the rule compares the attribute, and the attribute has to be
	// there whichever identifier the pair is matched on.
	supporting := map[string]string{
		AttrSQLConnectionName: o.Instance.ConnectionName(),
		// P4's substring test reads the bare name. It is emitted rather than left to be derived
		// from the value, because the value's shape is this feeder's choice and the rule must not
		// depend on it.
		AttrSQLInstanceName: o.Instance.Name,
	}
	for i := range claims {
		// Cloned per claim rather than shared: three claims pointing at one map is a trap for
		// whoever next wants to add an attribute to only one of them.
		claims[i].Attrs = maps.Clone(supporting)
	}
	return claims
}

// AttrSQLConnectionName and AttrSQLInstanceName are the supporting attributes the published Cloud
// SQL rules read: C7 resolves two sources onto one instance by the connection name (FR-120), and P4
// looks for the bare instance name inside a service's environment-variable names (FR-121).
//
// They are emitted on every claim about an instance so a rule can decide from the claim rather than
// by parsing the ref's value — the value's shape is this feeder's choice, and a rule that parsed it
// would break the next time the shape changed, silently, by never firing again.
const (
	AttrSQLConnectionName = "sre.gcp.instance_connection_name"
	AttrSQLInstanceName   = "sre.gcp.instance_name"
)

// Props renders the instance's properties.
func (o InstanceObservation) Props() *feeder.Props {
	props := feeder.NewProps().
		Str(PropProject, o.Instance.Project).
		Str(PropRegion, o.Instance.Region).
		Str(feeder.AttrCloudRegion, o.Instance.Region).
		Str(PropSQLConnectionName, o.Instance.ConnectionName()).
		Str(PropSQLDatabaseVersion, o.DatabaseVersion).
		Str(PropSQLState, o.State).
		Str(PropEnvironment, o.Labels.Environment).
		Str(PropEnvironmentSource, o.Labels.EnvironmentSource)
	if o.InstalledVersion != "" && o.InstalledVersion != o.DatabaseVersion {
		// Only where it differs. A property that repeats another one is a property a reader has
		// to compare to learn nothing.
		props = props.Str(PropSQLInstalledVersion, o.InstalledVersion)
	}
	if o.Tier != "" {
		props = props.Str(PropSQLTier, o.Tier)
	}
	if o.AvailabilityType != "" {
		props = props.Str(PropSQLAvailabilityType, o.AvailabilityType)
	}
	if o.BackendType != "" {
		props = props.Str(PropSQLBackendType, o.BackendType)
	}
	if o.DiskSizeGB > 0 {
		props = props.Int(PropSQLDiskSizeGB, o.DiskSizeGB)
	}
	if o.DiskType != "" {
		props = props.Str(PropSQLDiskType, o.DiskType)
	}
	if len(o.Flags) > 0 {
		props = props.Strs(PropSQLFlags, o.Flags...)
	}
	if o.MaintenanceWindow != "" {
		props = props.Str(PropSQLMaintenanceWindow, o.MaintenanceWindow)
	}
	if len(o.DenyMaintenancePeriods) > 0 {
		props = props.Strs(PropSQLDenyMaintenancePeriods, o.DenyMaintenancePeriods...)
	}
	if o.MaintenanceVersion != "" {
		props = props.Str(PropSQLMaintenanceVersion, o.MaintenanceVersion)
	}
	if o.Scheduled != nil {
		props = props.
			Time(PropSQLScheduledMaintenanceAt, o.Scheduled.StartTime).
			Bool(PropSQLScheduledMaintenanceCanReschedule, o.Scheduled.CanReschedule)
		if !o.Scheduled.DeadlineTime.IsZero() {
			props = props.Time(PropSQLScheduledMaintenanceDeadline, o.Scheduled.DeadlineTime)
		}
	}
	if len(o.Addresses) > 0 {
		props = props.Strs("ipAddresses", o.Addresses...)
	}
	for key, value := range o.Labels.Props {
		props = props.Str("sre.gcp.label."+key, value)
	}
	if o.CreateTime.IsZero() {
		props = props.Str(PropSQLValidFromIsABound, ValidFromBoundReason)
	}
	return props
}

// ValidFromBoundReason is what a node whose creation instant GCP did not report says about its own
// interval start. It is the convention the owner and vendor nodes already follow, and the reason it
// is a stated bound rather than `valid_from_unknown` is the conformance shuffle: an unknown start is
// stored as a first-observation placeholder, so its value is whichever event reached the graph
// first, and permuting the arrival order inside the reordering window moves the interval.
const ValidFromBoundReason = "the earliest instant this connector observed the instance; GCP " +
	"reported no createTime, so this is a bound and not the instant it was created"

// NodeFact renders the instance as the INFRA_RESOURCE node it is.
func (o InstanceObservation) NodeFact(observedAt time.Time) feeder.NodeFact {
	fact := feeder.NodeFact{
		Ref:         o.Instance.Ref(),
		Type:        graphv1.NodeType_INFRA_RESOURCE,
		DisplayName: o.Instance.Name,
	}
	if o.CreateTime.IsZero() {
		fact.ValidAt = observedAt
	} else {
		fact.ValidAt = o.CreateTime
	}
	return fact
}

// Pointers returns the instance's pointers: the Cloud SQL metrics on `cloudsql_database`, and the
// console link (FR-079 asks for at least one metric pointer per instance).
func (o InstanceObservation) Pointers() ([]*graphv1.Pointer, error) {
	attrs := map[string]string{
		PropProject:            o.Instance.Project,
		feeder.AttrCloudRegion: o.Instance.Region,
	}
	base := fmt.Sprintf(`resource.type="%s" AND resource.labels.project_id="%s" AND `+
		`resource.labels.region="%s" AND resource.labels.database_id="%s:%s"`,
		ResourceTypeCloudSQLDatabase, o.Instance.Project, o.Instance.Region,
		o.Instance.Project, o.Instance.Name)

	var out []*graphv1.Pointer
	for _, metric := range []string{MetricSQLCPUUtilisation, MetricSQLMemoryUtilisation, MetricSQLConnections} {
		pointer, err := MetricPointer(fmt.Sprintf(`metric.type="%s" AND %s`, metric, base), attrs)
		if err != nil {
			return nil, err
		}
		out = append(out, pointer)
	}
	out = append(out, ConsoleLink(fmt.Sprintf(
		"https://console.cloud.google.com/sql/instances/%s/overview?project=%s",
		o.Instance.Name, o.Instance.Project), attrs))
	return out, nil
}

// ResourceTypeCloudSQLDatabase is the monitored resource Cloud SQL metrics are written against.
// The `database_id` label is `<project>:<instance>` — note it carries no region, which is why the
// region is pinned separately and why sqlInstanceRef in monitoring.go needs one to build a ref.
const ResourceTypeCloudSQLDatabase = "cloudsql_database"

// cloudSQLJoinKeys is the role -> field map every `cloudsql_database` pointer carries.
//
// Only one of the five published roles is expressible, and the other four are absent for reasons
// that are facts about Cloud SQL rather than gaps:
//
//   - `host` is `database_id`, the instance connection name. An instance is what a Cloud SQL
//     series is *of*, which is the role a replica plays elsewhere — and it is the reading
//     internal/backends/gcp already publishes, mapping `database_id` to `JoinKeys.pod_or_host`
//     (FR-102). The feeder naming a different role would put the two ends of one join on
//     different fields;
//   - `version` — a managed database has no deployed version in its metric labels. The engine
//     version is a property of the instance, read by the configuration path and stored on the
//     node; it is not a label any series is grouped by, so splitting an error rate by it is not
//     a query that exists;
//   - `workload` — `cloudsql_database` carries no `service_name`. Which services talk to this
//     instance is an edge in the graph (US5's DEPENDS_ON), not a tag on the telemetry;
//   - `pod` and `trace` — no per-replica label on a managed instance, and no trace source in
//     this estate (FR-085).
var cloudSQLJoinKeys = map[string]string{
	feeder.JoinRoleHost: FieldDatabaseID,
}

// The published Cloud SQL metrics FR-079 requires a pointer for.
const (
	MetricSQLCPUUtilisation    = "cloudsql.googleapis.com/database/cpu/utilization"
	MetricSQLMemoryUtilisation = "cloudsql.googleapis.com/database/memory/utilization"
	MetricSQLConnections       = "cloudsql.googleapis.com/database/postgresql/num_backends"
)

// ---------------------------------------------------------------------------
// Changes: what a poll saw change, dated by the audit stream (T131, FR-031)
// ---------------------------------------------------------------------------
//
// The division of labour is the one US1 established for a traffic shift, and for the same reason: a
// poll sees a *state*, not a transition, so the **what** comes from comparing two polls and the
// **when** and **who** come from the audit entry. A change dated at the poll would be a change at an
// instant GCP never stated, and it would be wrong by up to one poll interval on every edit.
//
// So a diff with no correlated audit completion is **held**, exactly as a traffic shift is, and the
// checkpoint says so. The alternatives are dating it at the poll or dropping it, and both are worse
// than a stated gap.

// The change properties.
const (
	// PropSQLChangeKind says which kind of Cloud SQL change this is, from the closed set below.
	PropSQLChangeKind = "sre.gcp.sql_change_kind"
	// PropSQLFlagsAdded, PropSQLFlagsRemoved and PropSQLFlagsChanged are the flag diff. A removed
	// flag records the value it had, because "this flag was set to off and is now unset" and "this
	// flag was set to on and is now unset" are different changes.
	PropSQLFlagsAdded   = "sre.gcp.sql_flags_added"
	PropSQLFlagsRemoved = "sre.gcp.sql_flags_removed"
	PropSQLFlagsChanged = "sre.gcp.sql_flags_changed"
	// PropSQLSettingsChanged is the settings diff, as `field: old -> new`.
	PropSQLSettingsChanged = "sre.gcp.sql_settings_changed"
	// PropSQLMaintenanceOperation is GCP's own operation name, which is what makes a past
	// maintenance addressable as one change however many times it is read.
	PropSQLMaintenanceOperation = "sre.gcp.sql_maintenance_operation"
	// PropSQLMaintenanceOperationType is `MAINTENANCE` or `RESCHEDULE_MAINTENANCE`, recorded rather
	// than collapsed into the change kind: the two are different events (see MaintenanceChange).
	PropSQLMaintenanceOperationType = "sre.gcp.sql_maintenance_operation_type"
	// PropSQLMaintenanceStatus is the operation's status as GCP reports it.
	PropSQLMaintenanceStatus = "sre.gcp.sql_maintenance_status"
	// PropSQLAnnouncedNotCorrelated states the gap that cannot be closed from the API: GCP
	// publishes nothing linking a `scheduledMaintenance` to the operation that later performed it.
	PropSQLAnnouncedNotCorrelated = "sre.gcp.sql_announced_not_correlated"
)

// The closed set of Cloud SQL change kinds this feeder emits.
const (
	// SQLChangeConfig is a settings or flag edit, observed by diffing two polls.
	SQLChangeConfig = "config"
	// SQLChangeMaintenance is a maintenance that happened, read from `operations.list`.
	SQLChangeMaintenance = "maintenance"
	// SQLChangeMaintenanceRescheduled is an operator moving a maintenance window.
	SQLChangeMaintenanceRescheduled = "maintenance_rescheduled"
	// SQLChangeMaintenanceAnnounced is a maintenance GCP has announced and not yet performed.
	SQLChangeMaintenanceAnnounced = "maintenance_announced"
)

// SQLConfigDiff is what changed between two observations of one instance.
//
// Three things are deliberately **not** in it:
//
//   - `settings.maintenanceWindow` and `settings.denyMaintenancePeriods`, which are a recurring
//     policy and never a change node (T133). Editing one is still recoverable — it is a property, so
//     the node's own history holds both values — but it is not an event, and a change stream that
//     carried every calendar edit would bury the edits that moved production;
//   - the instance **state**. A state that changed is a change the audit stream reports, and
//     deriving one from two polls would date it at the poll;
//   - the IP addresses. They move when GCP moves them, and an address change nobody asked for is a
//     fact about the platform rather than a configuration change somebody made.
type SQLConfigDiff struct {
	// FlagsAdded and FlagsRemoved are `name=value` entries; FlagsChanged is `name: old -> new`.
	FlagsAdded   []string
	FlagsRemoved []string
	FlagsChanged []string
	// Settings is `field: old -> new` for the settings that are not flags.
	Settings []string
}

// Empty reports whether nothing changed.
func (d SQLConfigDiff) Empty() bool {
	return len(d.FlagsAdded) == 0 && len(d.FlagsRemoved) == 0 &&
		len(d.FlagsChanged) == 0 && len(d.Settings) == 0
}

// Summary renders the diff as one line for the change node.
func (d SQLConfigDiff) Summary() string {
	var parts []string
	if n := len(d.FlagsAdded); n > 0 {
		parts = append(parts, fmt.Sprintf("%d flag(s) set", n))
	}
	if n := len(d.FlagsChanged); n > 0 {
		parts = append(parts, fmt.Sprintf("%d flag(s) changed", n))
	}
	if n := len(d.FlagsRemoved); n > 0 {
		parts = append(parts, fmt.Sprintf("%d flag(s) unset", n))
	}
	if n := len(d.Settings); n > 0 {
		parts = append(parts, fmt.Sprintf("%d setting(s) changed", n))
	}
	return strings.Join(parts, ", ")
}

// DiffInstance compares two observations of one instance.
//
// Old and new values are recorded **only where GCP states them** (FR-031). That is not a caveat
// about effort: a flag that is absent from `settings.databaseFlags` is a flag left at the engine's
// default, and GCP does not say what that default is in this response. So a flag appearing is
// recorded as "set to X", never as "changed from <default> to X" — the second would be the feeder
// inventing the old value, and a reader comparing it against the engine's real default would find a
// fact nobody asserted.
func DiffInstance(before, after InstanceObservation) SQLConfigDiff {
	var diff SQLConfigDiff
	for _, name := range slices.Sorted(maps.Keys(after.FlagValues)) {
		newValue := after.FlagValues[name]
		oldValue, had := before.FlagValues[name]
		switch {
		case !had:
			diff.FlagsAdded = append(diff.FlagsAdded, name+"="+newValue)
		case oldValue != newValue:
			diff.FlagsChanged = append(diff.FlagsChanged, name+": "+oldValue+" -> "+newValue)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(before.FlagValues)) {
		if _, still := after.FlagValues[name]; !still {
			diff.FlagsRemoved = append(diff.FlagsRemoved, name+"="+before.FlagValues[name])
		}
	}
	for _, field := range []struct {
		name       string
		old, fresh string
	}{
		{PropSQLDatabaseVersion, before.DatabaseVersion, after.DatabaseVersion},
		{PropSQLInstalledVersion, before.InstalledVersion, after.InstalledVersion},
		{PropSQLTier, before.Tier, after.Tier},
		{PropSQLAvailabilityType, before.AvailabilityType, after.AvailabilityType},
		{PropSQLDiskType, before.DiskType, after.DiskType},
		{PropSQLMaintenanceVersion, before.MaintenanceVersion, after.MaintenanceVersion},
	} {
		if field.old != field.fresh && field.old != "" && field.fresh != "" {
			diff.Settings = append(diff.Settings, field.name+": "+field.old+" -> "+field.fresh)
		}
	}
	if before.DiskSizeGB != after.DiskSizeGB && before.DiskSizeGB > 0 && after.DiskSizeGB > 0 {
		diff.Settings = append(diff.Settings, fmt.Sprintf("%s: %d -> %d",
			PropSQLDiskSizeGB, before.DiskSizeGB, after.DiskSizeGB))
	}
	sort.Strings(diff.Settings)
	return diff
}

// ChangeRefSQLConfig is the deterministic ref of a Cloud SQL configuration change: the instance and
// the instant the audit entry states.
//
// Keyed on the instant and not on what changed, so that two polls observing one edit produce one
// change — and so that two edits at two instants are two changes even when they touched the same
// flag, which is what a history is.
func ChangeRefSQLConfig(inst SQLInstance, at time.Time) *graphv1.Ref {
	return feeder.Ref(NSChange, "sql-config/"+inst.Value()+"@"+instant(at))
}

// SQLConfigChange builds the configuration change (T131, FR-031).
//
// `at` is the audit entry's own instant and nothing else. A zero instant is an error rather than an
// unknown start: the caller's alternative is to hold the diff, which HeldSQLConfig exists for.
func SQLConfigChange(inst SQLInstance, at time.Time, diff SQLConfigDiff, actor Actor) (feeder.ChangeFact, error) {
	if at.IsZero() {
		return feeder.ChangeFact{}, fmt.Errorf("gcp: a Cloud SQL configuration change for %s with no "+
			"instant; the instant is the audit entry's timestamp, and a diff with no correlated entry "+
			"is held rather than dated at the poll (contracts/gcp-feeder.md §4, §5.2)", inst.Value())
	}
	if err := inst.Validate(); err != nil {
		return feeder.ChangeFact{}, err
	}
	if diff.Empty() {
		return feeder.ChangeFact{}, fmt.Errorf("gcp: a Cloud SQL configuration change for %s with an "+
			"empty diff; a change node that names nothing that changed is noise a ranker cannot "+
			"exclude", inst.Value())
	}
	return feeder.ChangeFact{
		Ref: ChangeRefSQLConfig(inst, at),
		// CONFIG_CHANGE and not FLAG_FLIP. `FLAG_FLIP` in the taxonomy is a feature flag — a
		// deliberate behaviour switch inside an application — where a Cloud SQL database flag is a
		// setting of the engine, from a closed published catalogue, changed by an administrative API
		// call. A reader filtering for feature-flag changes must not find database tuning.
		Kind:      graphv1.ChangeKind_CONFIG_CHANGE,
		Summary:   "Cloud SQL instance " + inst.Name + ": " + diff.Summary(),
		ActorKind: actor.Kind,
		// The instance is the target, which is what gives it the `changed-by` edge FR-031 requires.
		Targets: []*graphv1.Ref{inst.Ref()},
		ValidAt: at,
	}, nil
}

// SQLConfigChangeProps renders the configuration change's properties.
func SQLConfigChangeProps(inst SQLInstance, diff SQLConfigDiff, actor Actor, operationID, auditMethod string) *feeder.Props {
	props := feeder.NewProps().
		Str(PropSQLChangeKind, SQLChangeConfig).
		Str(PropProject, inst.Project).
		Str(PropRegion, inst.Region).
		Str(PropActorRung, actor.Rung)
	if len(diff.FlagsAdded) > 0 {
		props = props.Strs(PropSQLFlagsAdded, diff.FlagsAdded...)
	}
	if len(diff.FlagsChanged) > 0 {
		props = props.Strs(PropSQLFlagsChanged, diff.FlagsChanged...)
	}
	if len(diff.FlagsRemoved) > 0 {
		props = props.Strs(PropSQLFlagsRemoved, diff.FlagsRemoved...)
	}
	if len(diff.Settings) > 0 {
		props = props.Strs(PropSQLSettingsChanged, diff.Settings...)
	}
	if operationID != "" {
		props = props.Str(PropOperationID, operationID)
	}
	if auditMethod != "" {
		props = props.Str(PropAuditMethod, auditMethod)
	}
	if len(actor.Evidence) > 0 {
		props = props.Strs(PropActorEvidence, actor.Evidence...)
	}
	return props
}

// HeldSQLConfig is a configuration diff the poll observed with no audit entry to date it.
//
// It is HeldSplit's sibling and exists for the same reason: "the settings changed and we do not yet
// know when" is a real state of the world, and the checkpoint has to say it.
type HeldSQLConfig struct {
	Instance SQLInstance
	Diff     SQLConfigDiff
	// ObservedAt is when the poll saw it. It is **not** a candidate valid time.
	ObservedAt time.Time
	Why        string
}

// HeldNoSQLAuditEntry is the reason a Cloud SQL diff is held.
const HeldNoSQLAuditEntry = "the instance settings changed and no Cloud SQL Admin audit entry has arrived to date it"

// HoldSQLConfig records a diff that cannot yet be dated.
func HoldSQLConfig(inst SQLInstance, diff SQLConfigDiff, observedAt time.Time) HeldSQLConfig {
	return HeldSQLConfig{Instance: inst, Diff: diff, ObservedAt: observedAt, Why: HeldNoSQLAuditEntry}
}

// String describes a held diff for the checkpoint, without implying an instant.
func (h HeldSQLConfig) String() string {
	return fmt.Sprintf("%s: %s, held since %s (%s)",
		h.Instance.Value(), h.Diff.Summary(), instant(h.ObservedAt), h.Why)
}

// SummariseHeldSQL renders held diffs for a checkpoint note, oldest first.
func SummariseHeldSQL(held []HeldSQLConfig) string {
	if len(held) == 0 {
		return ""
	}
	sorted := append([]HeldSQLConfig(nil), held...)
	sort.Slice(sorted, func(i, j int) bool {
		if !sorted[i].ObservedAt.Equal(sorted[j].ObservedAt) {
			return sorted[i].ObservedAt.Before(sorted[j].ObservedAt)
		}
		return sorted[i].Instance.Value() < sorted[j].Instance.Value()
	})
	lines := make([]string, 0, len(sorted))
	for _, h := range sorted {
		lines = append(lines, h.String())
	}
	return strings.Join(lines, "; ")
}

// ---------------------------------------------------------------------------
// Maintenance: what happened, and what is announced (T132, FR-032)
// ---------------------------------------------------------------------------
//
// # The announced fact reuses US2's machinery rather than reimplementing it
//
// `DatabaseInstance.scheduledMaintenance.startTime` is a genuinely future-dated instant, so this is
// the one place the platform feeder emits an **announced fact** — a fact whose valid time begins
// after its observed time. The semantics are the vendor-notice feeder's, unchanged: the valid
// interval is the announced window even though it starts after the read; observed time is never
// moved forward to match it; a cancellation and a reschedule are *corrections of the same change*
// rather than retractions; and an announced window that passes in silence stays announced for ever.
//
// This file therefore **imports the two invariant checks from that feeder** rather than restating
// them. It is an unusual dependency for one connector to take on another, and it is the point: FR-063
// and FR-066 now have exactly one implementation, so a future edit to either cannot leave two
// connectors disagreeing about what an announcement means. The alternative was a second copy, which
// contracts/gcp-feeder.md §4 forbids in as many words.
//
// # The two operation types are two different events
//
// `operations.list` filtered on `operationType` gives both `MAINTENANCE` and
// `RESCHEDULE_MAINTENANCE`, and they are not the same fact wearing two names:
//
//   - a `MAINTENANCE` operation's `[startTime, endTime)` is when the instance was **actually under
//     maintenance**. That is a CLOUD_MAINTENANCE whose actor is the provider;
//   - a `RESCHEDULE_MAINTENANCE` operation's interval is when the **control-plane call that moved
//     the window** ran. Giving it CLOUD_MAINTENANCE would assert a maintenance at an instant no
//     maintenance happened, so it is a CONFIG_CHANGE — somebody changed when maintenance will
//     happen — and the window it moved *to* is read from the next poll's `scheduledMaintenance`,
//     which is where the SUPERSEDED correction below comes from.
//
// # The announcement and the operation that performed it are not correlated, and that is stated
//
// GCP publishes **nothing** linking a `scheduledMaintenance` to the `MAINTENANCE` operation that
// later carried it out: the announcement has a start, a deferability flag and a deadline, and the
// operation has a uuid, a type and its own instants. There is no shared identifier.
//
// So a past maintenance is emitted as its own observed change and the announcement is **not promoted
// to CONFIRMED** on the strength of it. A tolerance window matching the two would be a guess, and a
// guess that promoted an announcement would be asserting that a specific announced window is the one
// that happened — which nobody observed. The gap is recorded on the change
// (PropSQLAnnouncedNotCorrelated) rather than closed by inference.

// MaintenanceOperationTypes are the `operations.list` operation types this feeder reads (contract §4).
var MaintenanceOperationTypes = map[string]bool{
	"MAINTENANCE":            true,
	"RESCHEDULE_MAINTENANCE": true,
}

// OperationTypeMaintenance and OperationTypeRescheduleMaintenance are the two, spelled once.
const (
	OperationTypeMaintenance           = "MAINTENANCE"
	OperationTypeRescheduleMaintenance = "RESCHEDULE_MAINTENANCE"
)

// MaintenanceActor names the provider as the actor of a maintenance it performed. It is a display
// name and never a principal: `Operation.user` is an email address, which is dropped and never
// stored (FR-134), and reaches the graph only as the actor ladder's classification of it.
const MaintenanceActor = "Google Cloud SQL"

// AnnouncedNotCorrelated is the stated gap. See the file comment above.
const AnnouncedNotCorrelated = "GCP publishes no identifier linking a scheduledMaintenance announcement " +
	"to the operation that performed it, so this maintenance is recorded as its own observed change " +
	"and no announcement is promoted to CONFIRMED on the strength of it (FR-066)"

// MaintenanceObservation is one `operations.list` entry this feeder reads.
type MaintenanceObservation struct {
	// Instance is the instance it was about.
	Instance SQLInstance
	// Name is GCP's operation name — a uuid, and the identifier that makes the change addressable.
	Name string
	// Type is `MAINTENANCE` or `RESCHEDULE_MAINTENANCE`.
	Type string
	// Start and End are the operation's own instants. Start is required; End is zero for an
	// operation still running, which is emitted with no bound rather than with a guessed one.
	Start, End time.Time
	// InsertTime is when the operation was requested. It is **not** valid time: the request is not
	// the occurrence, which is the same distinction §5.1 makes for an audit request entry.
	InsertTime time.Time
	// Status is the operation's status as GCP reports it.
	Status string
	// User is the principal GCP names, carried only as far as the actor ladder and never stored.
	User string
}

// ObserveMaintenance reads one operation, or reports that it is not one this feeder models.
//
// `region` is supplied by the caller because an operation names its instance by `targetId` — a bare
// instance name with no region — for the same reason an audit entry does (ParseSQLInstanceResourceName).
func ObserveMaintenance(op *sqladmin.Operation, project, region string) (MaintenanceObservation, bool, error) {
	var obs MaintenanceObservation
	if op == nil || !MaintenanceOperationTypes[op.OperationType] {
		return obs, false, nil
	}
	name := op.TargetId
	if name == "" {
		return obs, false, nil
	}
	inst := SQLInstance{Project: project, Region: region, Name: name}
	if op.TargetProject != "" {
		inst.Project = op.TargetProject
	}
	if err := inst.Validate(); err != nil {
		return obs, false, err
	}
	obs = MaintenanceObservation{
		Instance: inst,
		Name:     op.Name,
		Type:     op.OperationType,
		Status:   op.Status,
		User:     op.User,
	}
	for _, field := range []struct {
		label string
		raw   string
		into  *time.Time
	}{
		{"startTime", op.StartTime, &obs.Start},
		{"endTime", op.EndTime, &obs.End},
		{"insertTime", op.InsertTime, &obs.InsertTime},
	} {
		if field.raw == "" {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, field.raw)
		if err != nil {
			return obs, false, fmt.Errorf("gcp: operation %s %s %q: %w", op.Name, field.label, field.raw, err)
		}
		*field.into = at.UTC()
	}
	if obs.Name == "" {
		return obs, false, fmt.Errorf("gcp: a Cloud SQL %s operation on %s with no name; the operation "+
			"name is what makes the change addressable, so re-reading the page would otherwise be a "+
			"second change", obs.Type, inst.Value())
	}
	if obs.Start.IsZero() {
		// An operation GCP has accepted but not started. It is not a maintenance that happened, and
		// dating it at insertTime would claim the instance went down when somebody asked for it to.
		return obs, false, nil
	}
	return obs, true, nil
}

// ChangeRefSQLMaintenance is the deterministic ref of a maintenance change: the instance and GCP's
// own operation name.
//
// The operation name is a uuid GCP assigns, so the same operation read on twenty polls is one change.
// The instant is deliberately **not** in the ref: an operation's `endTime` fills in once it finishes,
// and a ref keyed on the interval would make the running maintenance and the finished one two
// changes.
func ChangeRefSQLMaintenance(inst SQLInstance, operation string) *graphv1.Ref {
	return feeder.Ref(NSChange, "sql-maintenance/"+inst.Value()+"/"+operation)
}

// MaintenanceChange builds the change for a maintenance operation that happened (T132, FR-032).
func MaintenanceChange(obs MaintenanceObservation, actor Actor) (feeder.ChangeFact, error) {
	if obs.Start.IsZero() {
		return feeder.ChangeFact{}, fmt.Errorf("gcp: a Cloud SQL maintenance change for %s with no "+
			"start; an operation GCP has not started is not a maintenance that happened",
			obs.Instance.Value())
	}
	if err := obs.Instance.Validate(); err != nil {
		return feeder.ChangeFact{}, err
	}
	fact := feeder.ChangeFact{
		Ref:       ChangeRefSQLMaintenance(obs.Instance, obs.Name),
		ActorKind: actor.Kind,
		Targets:   []*graphv1.Ref{obs.Instance.Ref()},
		ValidAt:   obs.Start,
		// Zero for a running operation, which leaves the change unbounded rather than bounded at a
		// guess. An operation with no endTime has not finished.
		ValidEnd: obs.End,
	}
	switch obs.Type {
	case OperationTypeMaintenance:
		fact.Kind = graphv1.ChangeKind_CLOUD_MAINTENANCE
		fact.Actor = MaintenanceActor
		fact.Summary = "Cloud SQL maintenance on " + obs.Instance.Name
	case OperationTypeRescheduleMaintenance:
		// See the file comment: this operation's interval is when the control-plane call ran, not
		// when maintenance happened, so it is not a CLOUD_MAINTENANCE.
		fact.Kind = graphv1.ChangeKind_CONFIG_CHANGE
		fact.Summary = "Cloud SQL maintenance rescheduled for " + obs.Instance.Name
	default:
		return feeder.ChangeFact{}, fmt.Errorf("gcp: operation type %q is not one this feeder reads "+
			"(%s); the filter and the mapping must agree, or an operation reaches the graph as a kind "+
			"nobody chose", obs.Type, strings.Join(slices.Sorted(maps.Keys(MaintenanceOperationTypes)), ", "))
	}
	return fact, nil
}

// MaintenanceChangeProps renders the maintenance change's properties.
func MaintenanceChangeProps(obs MaintenanceObservation, actor Actor) *feeder.Props {
	kind := SQLChangeMaintenance
	if obs.Type == OperationTypeRescheduleMaintenance {
		kind = SQLChangeMaintenanceRescheduled
	}
	props := feeder.NewProps().
		Str(PropSQLChangeKind, kind).
		Str(PropProject, obs.Instance.Project).
		Str(PropRegion, obs.Instance.Region).
		Str(PropSQLMaintenanceOperation, obs.Name).
		Str(PropSQLMaintenanceOperationType, obs.Type).
		Str(PropActorRung, actor.Rung)
	if obs.Status != "" {
		props = props.Str(PropSQLMaintenanceStatus, obs.Status)
	}
	if obs.Type == OperationTypeMaintenance {
		props = props.Str(PropSQLAnnouncedNotCorrelated, AnnouncedNotCorrelated)
	}
	if len(actor.Evidence) > 0 {
		props = props.Strs(PropActorEvidence, actor.Evidence...)
	}
	return props
}

// ChangeRefSQLScheduledMaintenance is the deterministic ref of an announced maintenance: the instance
// and the announced start.
//
// The start is in the ref, and that follows the vendor-notice feeder's derived-identifier rule rather
// than departing from it. There, a notice the vendor gives an identifier keeps one ref across a
// reschedule; a notice with **no** identifier is keyed on its window, so re-announcing a different
// window is a different change — "rescheduling from one date to another is a correction of the same
// notice only when the vendor says so, and the vendor says so by reusing its identifier".
//
// GCP states no identifier for a scheduled maintenance. So the window is the key, and the reschedule
// is expressed the way that rule expresses it: the old ref is observed again as SUPERSEDED carrying
// its original window, and the new window opens as a new ANNOUNCED change. Both beliefs stay
// recoverable by an observed-time query, which is the property a reschedule has to preserve.
func ChangeRefSQLScheduledMaintenance(inst SQLInstance, start time.Time) *graphv1.Ref {
	return feeder.Ref(NSChange, "sql-maintenance-announced/"+inst.Value()+"@"+instant(start))
}

// ScheduledMaintenanceChange builds the announced maintenance in one of the announcement states
// (T132, FR-032, FR-062–FR-068).
//
// `observedAt` is the poll instant and `now` the wall clock, both passed in rather than read, so that
// the two invariants are *checked* at a fixed instant a fixture can assert:
//
//   - observed time is never moved forward to match an announced window (FR-063). The way this goes
//     wrong is a feeder that, seeing a window in the future, helpfully stamps its own observation at
//     the window's start;
//   - an announced window is never promoted to CONFIRMED because its start has passed (FR-066).
//
// Both checks are the vendor-notice feeder's, called rather than copied.
func ScheduledMaintenanceChange(inst SQLInstance, announced ScheduledMaintenance,
	state graphv1.AnnouncementState, observedAt, now time.Time) (feeder.ChangeFact, error) {
	if err := inst.Validate(); err != nil {
		return feeder.ChangeFact{}, err
	}
	if announced.StartTime.IsZero() {
		return feeder.ChangeFact{}, fmt.Errorf("gcp: an announced maintenance for %s with no start; "+
			"scheduledMaintenance.startTime is the whole of what GCP announces, so a missing one is "+
			"nothing to announce rather than an unknown window", inst.Value())
	}
	if observedAt.IsZero() {
		return feeder.ChangeFact{}, fmt.Errorf("gcp: an announced maintenance for %s observed at no "+
			"instant", inst.Value())
	}
	if err := vendornotice.AssertObservedNotInFuture(observedAt, now); err != nil {
		return feeder.ChangeFact{}, err
	}
	// FR-066, and this connector can state it more strongly than the general rule.
	//
	// The vendor-notice check is called first, so there is one implementation of the rule rather than
	// two. It keys on the window's **end** having passed, and a Cloud SQL announcement has no end —
	// GCP announces a start and no duration — so it cannot fire here.
	//
	// The refusal below is what does fire, and it is not a stricter reading of the same rule but a
	// fact about this connector: **nothing it reads is an observation that an announced window
	// happened.** A past `MAINTENANCE` operation is not that observation, because GCP publishes no
	// identifier linking an announcement to the operation that performed it (AnnouncedNotCorrelated),
	// so correlating them would be a guess — and a guess that promoted an announcement would be
	// asserting that a specific announced window is the one that took place.
	if err := vendornotice.AssertNotPromotedBySilence(state,
		vendornotice.Window{Start: announced.StartTime, End: announced.StartTime}, now); err != nil {
		return feeder.ChangeFact{}, err
	}
	if state == graphv1.AnnouncementState_CONFIRMED {
		return feeder.ChangeFact{}, fmt.Errorf("gcp: an announced maintenance for %s was promoted to "+
			"CONFIRMED. Nothing this connector reads is an observation that an announced window "+
			"happened: GCP publishes no identifier linking a scheduledMaintenance to the operation "+
			"that performed it, so a promotion here would be a guess dressed as an observation "+
			"(FR-066, contracts/gcp-feeder.md §4)", inst.Value())
	}
	return feeder.ChangeFact{
		Meta:      feeder.Meta{SourceObservedAt: observedAt},
		Ref:       ChangeRefSQLScheduledMaintenance(inst, announced.StartTime),
		Kind:      graphv1.ChangeKind_CLOUD_MAINTENANCE,
		Summary:   "Cloud SQL maintenance announced for " + inst.Name + " at " + instant(announced.StartTime),
		Actor:     MaintenanceActor,
		ActorKind: graphv1.ActorKind_VENDOR,
		Targets:   []*graphv1.Ref{inst.Ref()},
		// The valid interval IS the announced window, emitted unchanged even though it begins after
		// the read instant (FR-062). Nothing here clamps it to now, and GCP announces no end — so
		// ValidEnd stays zero and the graph gives it the shortest non-empty interval, rather than
		// this feeder inventing a duration.
		ValidAt:           announced.StartTime,
		AnnouncementState: state,
	}, nil
}

// ScheduledMaintenanceProps renders the announced maintenance's properties.
func ScheduledMaintenanceProps(inst SQLInstance, announced ScheduledMaintenance) *feeder.Props {
	props := feeder.NewProps().
		Str(PropSQLChangeKind, SQLChangeMaintenanceAnnounced).
		Str(PropProject, inst.Project).
		Str(PropRegion, inst.Region).
		Time(PropSQLScheduledMaintenanceAt, announced.StartTime).
		Bool(PropSQLScheduledMaintenanceCanReschedule, announced.CanReschedule)
	if !announced.DeadlineTime.IsZero() {
		props = props.Time(PropSQLScheduledMaintenanceDeadline, announced.DeadlineTime)
	}
	return props
}

// AnnouncementTransition is what one poll's comparison of the announced maintenance found.
type AnnouncementTransition struct {
	// Announced is the window now announced, where one is. It is emitted on every poll that sees
	// one, which is idempotent: the ref, the interval and the state are all deterministic, so the
	// second emission is a duplicate the log collapses.
	Announced *ScheduledMaintenance
	// Superseded is the window that was announced before this poll and has been moved. It is
	// re-observed carrying its ORIGINAL window, which is what keeps "we believed it was the 2nd,
	// then we believed it was the 9th" answerable by an observed-time query.
	Superseded *ScheduledMaintenance
	// Cancelled is a window that was announced and has been withdrawn before it arrived.
	Cancelled *ScheduledMaintenance
	// PassedInSilence is a window that disappeared after its start had passed. Nothing is emitted
	// for it: the passage of time is not an observation, so the announcement stays announced
	// (FR-066). It is carried so a caller can report it rather than silently drop the case.
	PassedInSilence *ScheduledMaintenance
}

// AnnouncementChange compares the announced maintenance across two polls (FR-066–FR-068).
//
// `hadPrevious` distinguishes "the previous poll saw no announcement" from "there was no previous
// poll", which is the same distinction the traffic-split code draws: without it, the first poll of an
// instance whose announcement was already withdrawn would emit a cancellation of something this
// feeder never announced.
func AnnouncementChange(before, after InstanceObservation, hadPrevious bool, observedAt time.Time) AnnouncementTransition {
	var out AnnouncementTransition
	out.Announced = after.Scheduled
	if !hadPrevious || before.Scheduled == nil {
		return out
	}
	previous := before.Scheduled
	switch {
	case after.Scheduled == nil && previous.StartTime.After(observedAt):
		// Withdrawn before it arrived: a correction, not a retraction. The window was never true —
		// it was announced, and then withdrawn — and those are different facts.
		out.Cancelled = previous
	case after.Scheduled == nil:
		// Its start has passed and GCP stopped announcing it. Almost certainly it happened, and
		// "almost certainly" is not an observation: the announcement stays announced (FR-066).
		out.PassedInSilence = previous
	case !after.Scheduled.StartTime.Equal(previous.StartTime):
		out.Superseded = previous
	}
	return out
}
