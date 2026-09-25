// SPDX-License-Identifier: Apache-2.0

package gcp_test

import (
	"strings"
	"testing"
	"time"

	sqladmin "google.golang.org/api/sqladmin/v1"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	gcpfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/gcp"
)

// Cloud SQL (T129–T133; FR-031–FR-035).

const (
	sqlProject  = "nova-production"
	sqlRegion   = "europe-west1"
	sqlInstance = "orders-primary"
)

var sqlObserved = time.Date(2026, 9, 21, 14, 0, 0, 0, time.UTC)

func instanceJSON(flags map[string]string, tier string, scheduled *sqladmin.SqlScheduledMaintenance) *sqladmin.DatabaseInstance {
	var databaseFlags []*sqladmin.DatabaseFlags
	for name, value := range flags {
		databaseFlags = append(databaseFlags, &sqladmin.DatabaseFlags{Name: name, Value: value})
	}
	return &sqladmin.DatabaseInstance{
		Name:            sqlInstance,
		Project:         sqlProject,
		Region:          sqlRegion,
		ConnectionName:  sqlProject + ":" + sqlRegion + ":" + sqlInstance,
		DatabaseVersion: "POSTGRES_16",
		State:           "RUNNABLE",
		CreateTime:      "2026-03-01T09:00:00Z",
		Settings: &sqladmin.Settings{
			Tier:             tier,
			AvailabilityType: "REGIONAL",
			DataDiskSizeGb:   100,
			DataDiskType:     "PD_SSD",
			DatabaseFlags:    databaseFlags,
			MaintenanceWindow: &sqladmin.MaintenanceWindow{
				Day: 7, Hour: 3, UpdateTrack: "stable",
			},
			UserLabels: map[string]string{"team": "platform"},
		},
		IpAddresses:          []*sqladmin.IpMapping{{IpAddress: "10.24.0.7", Type: "PRIVATE"}},
		ScheduledMaintenance: scheduled,
	}
}

func observe(t *testing.T, instance *sqladmin.DatabaseInstance) gcpfeeder.InstanceObservation {
	t.Helper()
	obs, err := gcpfeeder.ObserveInstance(instance, gcpfeeder.DefaultLabelPolicy())
	if err != nil {
		t.Fatalf("ObserveInstance: %v", err)
	}
	return obs
}

// The connection name is the identifier FR-120's certain rule resolves on, so it is claimed AND
// carried as a supporting attribute on every claim about the instance — including the claims whose
// value is something else entirely.
func TestEveryInstanceClaimCarriesTheConnectionName(t *testing.T) {
	obs := observe(t, instanceJSON(map[string]string{"max_connections": "200"}, "db-custom-4-16384", nil))
	claims := obs.Claims()
	if len(claims) < 3 {
		t.Fatalf("claims = %d, want the addressing ref, the connection name and the resource name", len(claims))
	}
	want := sqlProject + ":" + sqlRegion + ":" + sqlInstance
	for _, claim := range claims {
		if got := claim.Attrs[gcpfeeder.AttrSQLConnectionName]; got != want {
			t.Errorf("claim %s=%s carries connection name %q, want %q; C7 reads the attribute, so a "+
				"claim without it is a claim the rule cannot fire on",
				claim.Namespace, claim.Value, got, want)
		}
		if got := claim.Attrs[gcpfeeder.AttrSQLInstanceName]; got != sqlInstance {
			t.Errorf("claim %s=%s carries instance name %q, want %q", claim.Namespace, claim.Value, got, sqlInstance)
		}
	}
	// And the addressing ref is itself a claim (FR-115).
	var addressing bool
	for _, claim := range claims {
		if claim.Namespace == gcpfeeder.NSSQLInstance && claim.Value == obs.Instance.Value() {
			addressing = true
		}
	}
	if !addressing {
		t.Error("the ref this feeder addresses the instance by is not among its claims (FR-115)")
	}
}

// The connection name is the authority where the response's own fields disagree with it: it is the
// string the proxy dials, and a response with an empty `region` must not produce a ref with a hole.
func TestTheConnectionNameOverridesEmptyCoordinateFields(t *testing.T) {
	instance := instanceJSON(nil, "db-custom-4-16384", nil)
	instance.Region = ""
	instance.Project = ""
	obs := observe(t, instance)
	if got := obs.Instance.Value(); got != sqlProject+"/"+sqlRegion+"/"+sqlInstance {
		t.Fatalf("instance = %q, want the coordinates read from the connection name", got)
	}
}

// A maintenance window is a recurring policy: a property, and never a change node (T133).
func TestTheMaintenanceWindowIsAPropertyAndNeverInADiff(t *testing.T) {
	before := observe(t, instanceJSON(nil, "db-custom-4-16384", nil))
	after := instanceJSON(nil, "db-custom-4-16384", nil)
	after.Settings.MaintenanceWindow = &sqladmin.MaintenanceWindow{Day: 1, Hour: 5, UpdateTrack: "canary"}
	after.Settings.DenyMaintenancePeriods = []*sqladmin.DenyMaintenancePeriod{
		{StartDate: "2026-12-20", EndDate: "2027-01-05", Time: "00:00:00"},
	}
	afterObs := observe(t, after)

	props := propsMap(t, afterObs.Props())
	if props[gcpfeeder.PropSQLMaintenanceWindow] == "" {
		t.Error("the maintenance window is not recorded as a property at all")
	}
	if props[gcpfeeder.PropSQLDenyMaintenancePeriods] == "" {
		t.Error("the deny-maintenance periods are not recorded as a property at all")
	}
	diff := gcpfeeder.DiffInstance(before, afterObs)
	if !diff.Empty() {
		t.Fatalf("editing the maintenance policy produced a change diff %+v; a recurring window is a "+
			"calendar entry and never an event (T133)", diff)
	}
}

// The instance state is a property. A state change observed between two polls must not become a
// change node: deriving one would date it at the poll rather than when it happened.
func TestAStateChangeIsNotADiff(t *testing.T) {
	before := observe(t, instanceJSON(nil, "db-custom-4-16384", nil))
	after := instanceJSON(nil, "db-custom-4-16384", nil)
	after.State = "SUSPENDED"
	if diff := gcpfeeder.DiffInstance(before, observe(t, after)); !diff.Empty() {
		t.Fatalf("a state change produced a diff %+v; a state that changed is what the audit stream "+
			"reports, and deriving one from two polls would date it at the poll", diff)
	}
}

// A flag change names the flag with both values, because GCP states both.
func TestAFlagChangeNamesTheOldAndNewValues(t *testing.T) {
	before := observe(t, instanceJSON(map[string]string{"max_connections": "200"}, "db-custom-4-16384", nil))
	after := observe(t, instanceJSON(map[string]string{"max_connections": "500"}, "db-custom-4-16384", nil))
	diff := gcpfeeder.DiffInstance(before, after)
	if len(diff.FlagsChanged) != 1 || diff.FlagsChanged[0] != "max_connections: 200 -> 500" {
		t.Fatalf("FlagsChanged = %v, want the flag with both values", diff.FlagsChanged)
	}
	if len(diff.FlagsAdded) != 0 || len(diff.FlagsRemoved) != 0 {
		t.Errorf("a changed flag was also reported as added or removed: %+v", diff)
	}
}

// A flag appearing is recorded as SET and never as changed from a default: GCP does not state the
// engine's default here, and inventing it would put a value in the graph nobody asserted.
func TestAFlagAppearingIsSetRatherThanChangedFromADefault(t *testing.T) {
	before := observe(t, instanceJSON(nil, "db-custom-4-16384", nil))
	after := observe(t, instanceJSON(map[string]string{"log_min_duration_statement": "500"}, "db-custom-4-16384", nil))
	diff := gcpfeeder.DiffInstance(before, after)
	if len(diff.FlagsAdded) != 1 || diff.FlagsAdded[0] != "log_min_duration_statement=500" {
		t.Fatalf("FlagsAdded = %v, want the flag recorded as set", diff.FlagsAdded)
	}
	for _, entry := range diff.FlagsChanged {
		if strings.Contains(entry, "->") {
			t.Errorf("a flag that appeared was reported as a transition: %q", entry)
		}
	}
}

// A removed flag records the value it had, because "was off and is now unset" and "was on and is now
// unset" are different changes.
func TestARemovedFlagRecordsTheValueItHad(t *testing.T) {
	before := observe(t, instanceJSON(map[string]string{"log_statement": "all"}, "db-custom-4-16384", nil))
	after := observe(t, instanceJSON(nil, "db-custom-4-16384", nil))
	diff := gcpfeeder.DiffInstance(before, after)
	if len(diff.FlagsRemoved) != 1 || diff.FlagsRemoved[0] != "log_statement=all" {
		t.Fatalf("FlagsRemoved = %v, want the flag with the value it had", diff.FlagsRemoved)
	}
}

// A database flag is a CONFIG_CHANGE and not a FLAG_FLIP: a reader filtering for feature-flag
// changes must not find database tuning.
func TestAFlagChangeIsAConfigChangeAndNotAFlagFlip(t *testing.T) {
	before := observe(t, instanceJSON(map[string]string{"max_connections": "200"}, "db-custom-4-16384", nil))
	after := observe(t, instanceJSON(map[string]string{"max_connections": "500"}, "db-custom-4-16384", nil))
	at := time.Date(2026, 9, 21, 3, 12, 0, 0, time.UTC)
	change, err := gcpfeeder.SQLConfigChange(after.Instance, at, gcpfeeder.DiffInstance(before, after),
		gcpfeeder.Actor{Kind: graphv1.ActorKind_PERSON, Rung: gcpfeeder.RungHumanDirectory})
	if err != nil {
		t.Fatalf("SQLConfigChange: %v", err)
	}
	if change.Kind != graphv1.ChangeKind_CONFIG_CHANGE {
		t.Errorf("kind = %s, want CONFIG_CHANGE", change.Kind)
	}
	if !change.ValidAt.Equal(at) {
		t.Errorf("valid_at = %s, want the audit entry's instant %s", change.ValidAt, at)
	}
	if len(change.Targets) != 1 || change.Targets[0].GetValue() != after.Instance.Value() {
		t.Errorf("targets = %v, want the instance, which is what gives it a changed-by edge", change.Targets)
	}
	if change.ActorKind != graphv1.ActorKind_PERSON {
		t.Errorf("actor kind = %s, want the classified actor", change.ActorKind)
	}
}

// A configuration change with no instant is refused rather than dated at the poll. The caller's
// alternative is to hold the diff, which is what the checkpoint reports.
func TestAConfigChangeWithNoInstantIsRefused(t *testing.T) {
	before := observe(t, instanceJSON(map[string]string{"max_connections": "200"}, "db-custom-4-16384", nil))
	after := observe(t, instanceJSON(map[string]string{"max_connections": "500"}, "db-custom-4-16384", nil))
	_, err := gcpfeeder.SQLConfigChange(after.Instance, time.Time{},
		gcpfeeder.DiffInstance(before, after), gcpfeeder.Actor{})
	if err == nil {
		t.Fatal("a configuration change with no instant was accepted; it would land in 1970 and rank " +
			"as maximally distant while looking like a fact")
	}
	held := gcpfeeder.HoldSQLConfig(after.Instance, gcpfeeder.DiffInstance(before, after), sqlObserved)
	if !strings.Contains(held.String(), "held since") {
		t.Errorf("a held diff does not say it is held: %q", held.String())
	}
	if strings.Contains(held.String(), "valid") {
		t.Errorf("a held diff implies an instant: %q", held.String())
	}
}

// An empty diff is refused: a change node naming nothing that changed is noise a ranker cannot
// exclude.
func TestAnEmptyDiffIsNotAChange(t *testing.T) {
	obs := observe(t, instanceJSON(nil, "db-custom-4-16384", nil))
	_, err := gcpfeeder.SQLConfigChange(obs.Instance, sqlObserved, gcpfeeder.SQLConfigDiff{}, gcpfeeder.Actor{})
	if err == nil {
		t.Fatal("a change with an empty diff was accepted")
	}
}

// --- maintenance ---------------------------------------------------------------------------------

func scheduledAt(start string, deadline string) *sqladmin.SqlScheduledMaintenance {
	return &sqladmin.SqlScheduledMaintenance{
		StartTime: start, CanReschedule: true, CanDefer: true, ScheduleDeadlineTime: deadline,
	}
}

// The announced window is emitted unchanged even though it begins after the read instant (FR-062),
// and observed time is not moved forward to match it (FR-063).
func TestAnAnnouncedMaintenanceKeepsItsFutureWindow(t *testing.T) {
	start := time.Date(2026, 10, 2, 2, 0, 0, 0, time.UTC)
	obs := observe(t, instanceJSON(nil, "db-custom-4-16384",
		scheduledAt("2026-10-02T02:00:00Z", "2026-10-09T02:00:00Z")))
	if obs.Scheduled == nil || !obs.Scheduled.StartTime.Equal(start) {
		t.Fatalf("scheduled = %+v, want the announced start %s", obs.Scheduled, start)
	}
	change, err := gcpfeeder.ScheduledMaintenanceChange(obs.Instance, *obs.Scheduled,
		graphv1.AnnouncementState_ANNOUNCED, sqlObserved, sqlObserved)
	if err != nil {
		t.Fatalf("ScheduledMaintenanceChange: %v", err)
	}
	if !change.ValidAt.Equal(start) {
		t.Errorf("valid_at = %s, want the announced start %s unchanged (FR-062)", change.ValidAt, start)
	}
	if !change.SourceObservedAt.Equal(sqlObserved) {
		t.Errorf("observed at %s, want the read instant %s; observed time is never moved forward to "+
			"match an announced window (FR-063)", change.SourceObservedAt, sqlObserved)
	}
	if change.Kind != graphv1.ChangeKind_CLOUD_MAINTENANCE {
		t.Errorf("kind = %s, want CLOUD_MAINTENANCE", change.Kind)
	}
	if change.ActorKind != graphv1.ActorKind_VENDOR {
		t.Errorf("actor kind = %s, want VENDOR; the provider performs it", change.ActorKind)
	}
	if change.AnnouncementState != graphv1.AnnouncementState_ANNOUNCED {
		t.Errorf("state = %s, want ANNOUNCED, which is the default and is never skipped", change.AnnouncementState)
	}
	if !change.ValidEnd.IsZero() {
		t.Errorf("valid_end = %s; GCP announces a start and no duration, so an end is not invented",
			change.ValidEnd)
	}
}

// An observation stamped ahead of the clock is refused, and the refusal comes from the vendor-notice
// feeder's own check rather than from a second copy of it.
func TestAnAnnouncementObservedInTheFutureIsRefused(t *testing.T) {
	obs := observe(t, instanceJSON(nil, "db-custom-4-16384", scheduledAt("2026-10-02T02:00:00Z", "")))
	_, err := gcpfeeder.ScheduledMaintenanceChange(obs.Instance, *obs.Scheduled,
		graphv1.AnnouncementState_ANNOUNCED, sqlObserved.Add(time.Hour), sqlObserved)
	if err == nil {
		t.Fatal("an observation stamped ahead of the clock was accepted (FR-063)")
	}
}

// An announced window that has passed is never promoted to CONFIRMED: the passage of time is not an
// observation (FR-066).
func TestAnAnnouncementIsNeverPromotedBySilence(t *testing.T) {
	obs := observe(t, instanceJSON(nil, "db-custom-4-16384", scheduledAt("2026-09-01T02:00:00Z", "")))
	_, err := gcpfeeder.ScheduledMaintenanceChange(obs.Instance, *obs.Scheduled,
		graphv1.AnnouncementState_CONFIRMED, sqlObserved, sqlObserved)
	if err == nil {
		t.Fatal("an announcement was promoted to CONFIRMED on the strength of its start having " +
			"passed (FR-066)")
	}
}

// A reschedule is two beliefs about one instance: the old window is superseded carrying its original
// interval, and the new one opens as announced.
func TestARescheduleSupersedesTheOldWindowAndAnnouncesTheNew(t *testing.T) {
	before := observe(t, instanceJSON(nil, "db-custom-4-16384", scheduledAt("2026-10-02T02:00:00Z", "")))
	after := observe(t, instanceJSON(nil, "db-custom-4-16384", scheduledAt("2026-10-09T02:00:00Z", "")))
	transition := gcpfeeder.AnnouncementChange(before, after, true, sqlObserved)
	if transition.Superseded == nil {
		t.Fatal("the old window was not superseded")
	}
	if !transition.Superseded.StartTime.Equal(before.Scheduled.StartTime) {
		t.Errorf("superseded window = %s, want the ORIGINAL %s; both beliefs must stay recoverable",
			transition.Superseded.StartTime, before.Scheduled.StartTime)
	}
	if transition.Announced == nil || !transition.Announced.StartTime.Equal(after.Scheduled.StartTime) {
		t.Errorf("announced window = %+v, want the new one", transition.Announced)
	}
	if transition.Cancelled != nil {
		t.Error("a reschedule was also reported as a cancellation")
	}
	// The two observations carry different refs, because GCP states no notice identifier to reuse.
	old := gcpfeeder.ChangeRefSQLScheduledMaintenance(before.Instance, before.Scheduled.StartTime)
	fresh := gcpfeeder.ChangeRefSQLScheduledMaintenance(after.Instance, after.Scheduled.StartTime)
	if old.GetValue() == fresh.GetValue() {
		t.Error("the two windows share a ref, so the reschedule would overwrite the first belief")
	}
}

// A window withdrawn before it arrives is a cancellation — a correction, not a retraction.
func TestAWithdrawnFutureWindowIsCancelled(t *testing.T) {
	before := observe(t, instanceJSON(nil, "db-custom-4-16384", scheduledAt("2026-10-02T02:00:00Z", "")))
	after := observe(t, instanceJSON(nil, "db-custom-4-16384", nil))
	transition := gcpfeeder.AnnouncementChange(before, after, true, sqlObserved)
	if transition.Cancelled == nil {
		t.Fatal("a window withdrawn before its start was not cancelled")
	}
	if transition.PassedInSilence != nil {
		t.Error("a future window was reported as having passed in silence")
	}
}

// A window that disappears after its start has passed produces NOTHING: it stays announced for ever,
// and the checkpoint is where a reader learns it is no longer announced (FR-066).
func TestAWindowThatPassesInSilenceIsNotPromotedAndNotCancelled(t *testing.T) {
	before := observe(t, instanceJSON(nil, "db-custom-4-16384", scheduledAt("2026-09-20T02:00:00Z", "")))
	after := observe(t, instanceJSON(nil, "db-custom-4-16384", nil))
	transition := gcpfeeder.AnnouncementChange(before, after, true, sqlObserved)
	if transition.PassedInSilence == nil {
		t.Fatal("a window whose start had passed and which stopped being announced was not reported")
	}
	if transition.Cancelled != nil {
		t.Error("a window whose start had passed was reported as cancelled; GCP withdrew nothing, " +
			"it stopped announcing something that had probably happened")
	}
	if transition.Announced != nil {
		t.Error("a window that is no longer announced was announced again")
	}
}

// The first poll of an instance whose announcement was already withdrawn must not cancel something
// this feeder never announced.
func TestTheFirstPollCancelsNothing(t *testing.T) {
	after := observe(t, instanceJSON(nil, "db-custom-4-16384", nil))
	transition := gcpfeeder.AnnouncementChange(gcpfeeder.InstanceObservation{}, after, false, sqlObserved)
	if transition.Cancelled != nil || transition.Superseded != nil || transition.PassedInSilence != nil {
		t.Fatalf("the first poll produced a correction: %+v", transition)
	}
}

// A MAINTENANCE operation is a CLOUD_MAINTENANCE bounded by its own interval; a
// RESCHEDULE_MAINTENANCE operation is not — its interval is when the control-plane call ran.
func TestTheTwoMaintenanceOperationTypesAreTwoDifferentChanges(t *testing.T) {
	for _, tc := range []struct {
		operationType string
		wantKind      graphv1.ChangeKind
	}{
		{"MAINTENANCE", graphv1.ChangeKind_CLOUD_MAINTENANCE},
		{"RESCHEDULE_MAINTENANCE", graphv1.ChangeKind_CONFIG_CHANGE},
	} {
		op := &sqladmin.Operation{
			Name:          "op-" + tc.operationType,
			OperationType: tc.operationType,
			TargetId:      sqlInstance,
			TargetProject: sqlProject,
			InsertTime:    "2026-09-20T02:55:00Z",
			StartTime:     "2026-09-20T03:00:00Z",
			EndTime:       "2026-09-20T03:18:00Z",
			Status:        "DONE",
			User:          "system@google.com",
		}
		obs, ok, err := gcpfeeder.ObserveMaintenance(op, sqlProject, sqlRegion)
		if err != nil || !ok {
			t.Fatalf("ObserveMaintenance(%s): ok=%v err=%v", tc.operationType, ok, err)
		}
		change, err := gcpfeeder.MaintenanceChange(obs, gcpfeeder.Actor{
			Kind: graphv1.ActorKind_VENDOR, Rung: gcpfeeder.RungVendorEvent,
		})
		if err != nil {
			t.Fatalf("MaintenanceChange(%s): %v", tc.operationType, err)
		}
		if change.Kind != tc.wantKind {
			t.Errorf("%s: kind = %s, want %s", tc.operationType, change.Kind, tc.wantKind)
		}
		if change.AnnouncementState != graphv1.AnnouncementState_ANNOUNCEMENT_STATE_UNSPECIFIED {
			t.Errorf("%s: an operation that happened carries announcement state %s; it is an ordinary "+
				"observed change", tc.operationType, change.AnnouncementState)
		}
		// GCP's own operation name is the ref, so re-reading the page is a no-op.
		if !strings.Contains(change.Ref.GetValue(), op.Name) {
			t.Errorf("%s: ref %q does not carry the operation name", tc.operationType, change.Ref.GetValue())
		}
	}
}

// A past maintenance states that it is NOT correlated with any announcement, because GCP publishes
// no identifier linking the two — and a tolerance window matching them would be a guess.
func TestAPastMaintenanceStatesThatItIsNotCorrelatedWithAnAnnouncement(t *testing.T) {
	op := &sqladmin.Operation{
		Name: "op-1", OperationType: "MAINTENANCE", TargetId: sqlInstance, TargetProject: sqlProject,
		StartTime: "2026-09-20T03:00:00Z", EndTime: "2026-09-20T03:18:00Z", Status: "DONE",
	}
	obs, ok, err := gcpfeeder.ObserveMaintenance(op, sqlProject, sqlRegion)
	if err != nil || !ok {
		t.Fatalf("ObserveMaintenance: ok=%v err=%v", ok, err)
	}
	props := propsMap(t, gcpfeeder.MaintenanceChangeProps(obs, gcpfeeder.Actor{Kind: graphv1.ActorKind_VENDOR}))
	if props[gcpfeeder.PropSQLAnnouncedNotCorrelated] == "" {
		t.Error("a past maintenance does not state that it is uncorrelated with any announcement; a " +
			"reader would assume the announced window was confirmed")
	}
	if props[gcpfeeder.PropSQLMaintenanceOperationType] != "MAINTENANCE" {
		t.Errorf("operation type = %q, want it recorded", props[gcpfeeder.PropSQLMaintenanceOperationType])
	}
}

// An operation GCP has accepted but not started is not a maintenance that happened. Dating it at
// insertTime would claim the instance went down when somebody asked for it to.
func TestAnUnstartedOperationIsNotAMaintenance(t *testing.T) {
	op := &sqladmin.Operation{
		Name: "op-2", OperationType: "MAINTENANCE", TargetId: sqlInstance,
		InsertTime: "2026-09-20T02:55:00Z", Status: "PENDING",
	}
	_, ok, err := gcpfeeder.ObserveMaintenance(op, sqlProject, sqlRegion)
	if err != nil {
		t.Fatalf("ObserveMaintenance: %v", err)
	}
	if ok {
		t.Fatal("an operation with no startTime was read as a maintenance that happened")
	}
}

// Every other operation type is read past: a backup is not maintenance.
func TestOnlyTheTwoDeclaredOperationTypesAreRead(t *testing.T) {
	for _, operationType := range []string{"BACKUP_VOLUME", "RESTART", "UPDATE", "IMPORT"} {
		op := &sqladmin.Operation{
			Name: "op", OperationType: operationType, TargetId: sqlInstance,
			StartTime: "2026-09-20T03:00:00Z", EndTime: "2026-09-20T03:01:00Z",
		}
		if _, ok, err := gcpfeeder.ObserveMaintenance(op, sqlProject, sqlRegion); ok || err != nil {
			t.Errorf("%s was read as maintenance (ok=%v err=%v)", operationType, ok, err)
		}
	}
}

// An audit entry names its instance by `projects/<project>/instances/<instance>` and carries NO
// region, which is why the index keys on the two parts it does state.
func TestASQLAuditEntryResourceNameCarriesNoRegion(t *testing.T) {
	project, instance, ok := gcpfeeder.ParseSQLInstanceResourceName(
		"projects/nova-production/instances/orders-primary")
	if !ok || project != sqlProject || instance != sqlInstance {
		t.Fatalf("parse = (%q, %q, %v), want the project and the instance", project, instance, ok)
	}
	for _, bad := range []string{
		"projects/nova-production/instances/",
		"projects//instances/orders-primary",
		"projects/nova-production/databases/orders-primary",
		"projects/nova-production/locations/europe-west1/instances/orders-primary",
	} {
		if _, _, ok := gcpfeeder.ParseSQLInstanceResourceName(bad); ok {
			t.Errorf("%q was accepted; a partly-filled instance addresses a different entity", bad)
		}
	}
}

// A Cloud SQL audit entry and a Cloud Run audit entry never date each other, even when the project
// and the name coincide.
func TestCloudSQLAndCloudRunCompletionsDoNotCollide(t *testing.T) {
	idx := gcpfeeder.NewAuditIndex()
	page := `{"entries":[
	 {"insertId":"a","logName":"projects/nova-production/logs/cloudaudit.googleapis.com%2Factivity",
	  "timestamp":"2026-09-21T03:12:00Z","receiveTimestamp":"2026-09-21T03:12:30Z",
	  "protoPayload":{"serviceName":"cloudsql.googleapis.com","methodName":"cloudsql.instances.update",
	   "resourceName":"projects/nova-production/instances/orders",
	   "authenticationInfo":{"principalEmail":"operator@example.com"}}}
	]}`
	if _, err := idx.Ingest([]byte(page), auditArrival); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	// The Cloud Run service of the same name in the same project finds nothing.
	if completion := idx.Take(gcpfeeder.Service{Project: sqlProject, Region: sqlRegion, Name: "orders"}); completion != nil {
		t.Fatal("a Cloud SQL audit entry dated a Cloud Run service's change")
	}
	completion := idx.TakeSQLInstance(sqlProject, "orders")
	if completion == nil {
		t.Fatal("the Cloud SQL completion was not filed")
	}
	want := time.Date(2026, 9, 21, 3, 12, 0, 0, time.UTC)
	if !completion.At.Equal(want) {
		t.Errorf("completion at %s, want the entry's timestamp %s", completion.At, want)
	}
	// Consumed once, so two polls that both observe one edit do not date it twice.
	if again := idx.TakeSQLInstance(sqlProject, "orders"); again != nil {
		t.Error("the completion was taken twice")
	}
}
