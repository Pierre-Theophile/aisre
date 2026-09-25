// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The Cloud Audit Logs admin-activity stream (T054, contracts/gcp-feeder.md §3.2, §5).
//
// This file exists to answer one question: **when did the traffic split take effect?** The Cloud Run
// v2 API has no field that stamps it (research §2), so the instant comes from the audit log, and the
// contract states exactly which instant:
//
//	`UpdateService` is a long-running operation, so it writes two entries correlated by
//	`LogEntry.operation.id`: a request entry with `operation.first` and a completion entry with
//	`operation.last`. The `timestamp` of the `operation.last` entry is the published definition of
//	"the instant the split took effect".
//
// That definition is what makes SC-002's "the instant GCP states" a defined quantity rather than an
// argument during review, and this file is where it is applied.
//
// # Why the entry is decoded from JSON rather than from the audit protobuf
//
// `LogEntry.protoPayload` is an `Any` holding a `google.cloud.audit.AuditLog`. A recording stores
// what the API returned, which is JSON, and this feeder needs three fields from it: the method name,
// the resource name and the authentication info. Decoding the JSON shape directly keeps the recorded
// format and the live format identical, and avoids taking a dependency on the audit proto for three
// strings — a dependency whose only effect would be to make the fixture format a protobuf nobody can
// read in a diff.
//
// # timestamp is valid time; receiveTimestamp never is
//
// GCP's reference defines both: `timestamp` is the event instant, `receiveTimestamp` is the delivery
// instant. Conflating them would make every delayed entry a lie about when production changed, so
// `receiveTimestamp` is used only for lag measurement and watermarking (§5.2).

// auditPayload is a page of `logging.entries.list`, as a recording stores it.
type auditPayload struct {
	Entries []auditEntry `json:"entries"`
}

// auditEntry is the part of a `LogEntry` this feeder reads.
type auditEntry struct {
	// InsertID is GCP's duplicate key. Every poll re-queries a trailing overlap window, so
	// deduplicating on it is what stops one change being observed twice (§5.2).
	InsertID string `json:"insertId"`
	// LogName says which stream the entry came from. The data-access stream is never read and no
	// permission for it is requested (FR-042); an entry from it in a recording is a mistake worth
	// refusing rather than silently processing.
	LogName string `json:"logName"`
	// Timestamp is the event instant and the **only** field used as valid time.
	Timestamp time.Time `json:"timestamp"`
	// ReceiveTimestamp is the delivery instant, used only for lag measurement.
	ReceiveTimestamp time.Time `json:"receiveTimestamp"`
	// Operation correlates the two entries of a long-running operation.
	Operation *auditOperation `json:"operation"`
	// ProtoPayload is the AuditLog, in the JSON shape the API returns.
	ProtoPayload *auditLog `json:"protoPayload"`
}

type auditOperation struct {
	ID       string `json:"id"`
	Producer string `json:"producer"`
	First    bool   `json:"first"`
	Last     bool   `json:"last"`
}

type auditLog struct {
	ServiceName        string                `json:"serviceName"`
	MethodName         string                `json:"methodName"`
	ResourceName       string                `json:"resourceName"`
	AuthenticationInfo *auditAuthentication  `json:"authenticationInfo"`
	RequestMetadata    *auditRequestMetadata `json:"requestMetadata"`
	Status             *auditStatus          `json:"status"`
}

type auditAuthentication struct {
	PrincipalEmail               string `json:"principalEmail"`
	PrincipalSubject             string `json:"principalSubject"`
	ServiceAccountKeyName        string `json:"serviceAccountKeyName"`
	ServiceAccountDelegationInfo []struct {
		FirstPartyPrincipal *struct {
			PrincipalEmail string `json:"principalEmail"`
		} `json:"firstPartyPrincipal"`
		ThirdPartyPrincipal map[string]any `json:"thirdPartyPrincipal"`
	} `json:"serviceAccountDelegationInfo"`
}

type auditRequestMetadata struct {
	CallerIP                string `json:"callerIp"`
	CallerSuppliedUserAgent string `json:"callerSuppliedUserAgent"`
}

type auditStatus struct {
	Code int32 `json:"code"`
}

// DataAccessLogSuffix identifies the stream this feeder never reads (FR-042). Admin activity is
// always written and cannot be disabled, which is why it is a dependable change stream; data access
// is off by default, enormous, and disproportionate to its value for change detection.
const DataAccessLogSuffix = "%2Fdata_access"

// AdminActivityLogSuffix identifies the stream it does read.
const AdminActivityLogSuffix = "%2Factivity"

// CloudRunServiceMutations are the methods that can change what a service serves. They are named
// rather than matched by prefix because a prefix match on `google.cloud.run.v2.Services.` would also
// catch `GetService`, and a read is not a change.
var CloudRunServiceMutations = map[string]bool{
	"google.cloud.run.v2.Services.CreateService": true,
	"google.cloud.run.v2.Services.UpdateService": true,
	"google.cloud.run.v2.Services.DeleteService": true,
}

// CloudSQLInstanceMutations are the Cloud SQL Admin methods that can change an instance.
//
// Cloud SQL writes audit entries under its own older method naming — `cloudsql.instances.update`,
// not `google.cloud.sql.v1.SqlInstancesService.Update` — so these are not the Cloud Run spellings
// with the product swapped, and a prefix match would be wrong for the same reason it is wrong there:
// `cloudsql.instances.get` and `cloudsql.instances.list` are reads.
//
// The set stops at the instance. `cloudsql.databases.*` and `cloudsql.users.*` are changes to what is
// *inside* the database, which this feature does not model — FR-030's rule about not reaching below a
// cluster is the same rule — and `cloudsql.instances.export` is the data-egress capability whose
// presence in roles/cloudsql.viewer disqualifies that role under FR-004.
var CloudSQLInstanceMutations = map[string]bool{
	"cloudsql.instances.create": true,
	"cloudsql.instances.update": true,
	"cloudsql.instances.patch":  true,
	"cloudsql.instances.delete": true,
}

// DNSMutations are the Cloud DNS methods that change a record set. A change to a record is what dates a
// DNS_SWITCH; the poll that sees the new targets says only what they now are.
var DNSMutations = map[string]bool{
	"dns.changes.create": true,
	"dns.changes.get":    false,
}

// LoadBalancerMutations are the compute methods that move what a load balancer fronts. `patch` and
// `update` on a backend service or a URL map are the two that swap a backend; `setUrlMap` on a proxy is
// the third, and it is the one that moves a whole load balancer at once.
var LoadBalancerMutations = map[string]bool{
	"v1.compute.backendServices.patch":        true,
	"v1.compute.backendServices.update":       true,
	"v1.compute.regionBackendServices.patch":  true,
	"v1.compute.urlMaps.patch":                true,
	"v1.compute.urlMaps.update":               true,
	"v1.compute.targetHttpsProxies.setUrlMap": true,
	"v1.compute.targetHttpProxies.setUrlMap":  true,
}

// authInfo converts the recorded shape into the actor ladder's input.
func (a *auditLog) authInfo() AuthenticationInfo {
	if a == nil || a.AuthenticationInfo == nil {
		return AuthenticationInfo{}
	}
	out := AuthenticationInfo{
		PrincipalEmail:        a.AuthenticationInfo.PrincipalEmail,
		PrincipalSubject:      a.AuthenticationInfo.PrincipalSubject,
		ServiceAccountKeyName: a.AuthenticationInfo.ServiceAccountKeyName,
	}
	// The delegation chain, which is where the human behind an impersonated service account is.
	for _, delegation := range a.AuthenticationInfo.ServiceAccountDelegationInfo {
		if delegation.FirstPartyPrincipal != nil {
			out.FirstPartyPrincipals = append(out.FirstPartyPrincipals, delegation.FirstPartyPrincipal.PrincipalEmail)
		}
		if len(delegation.ThirdPartyPrincipal) > 0 {
			out.ThirdPartyPrincipal = true
		}
	}
	if a.RequestMetadata != nil {
		out.CallerSuppliedUserAgent = a.RequestMetadata.CallerSuppliedUserAgent
	}
	return out
}

// AuditIndex correlates the two entries of each long-running operation and holds the completions a
// traffic shift needs to be dated.
//
// It is a type rather than a map because three rules travel with it and each has been got wrong
// somewhere: entries are deduplicated on `insertId`; only a completion entry dates anything; and a
// completion is consumed once, so two polls observing one shift do not date it twice.
type AuditIndex struct {
	// seen deduplicates on insertId across the trailing overlap window every poll re-queries.
	seen map[string]bool
	// completions holds the datable completions by service identifier, oldest first.
	completions map[string][]Completion
	// requests holds request entries whose completion has not arrived, by operation id. They are
	// kept because the *principal* is on the request entry for some operations and the completion
	// entry carries only the outcome — so the two halves are merged rather than one being preferred.
	requests map[string]Completion
	// lag is the measured `receiveTimestamp − timestamp` distribution.
	lag []time.Duration
	// general holds every in-scope entry as a candidate general change, by its change key
	// (auditlog.go). A specialised path that uses an entry CONSUMES it; whatever is left is emitted
	// as a general change, which is what makes the catch-all catch the cases nobody wrote a case for.
	general map[string]AuditChange
	// owned marks the keys whose method belongs to a specialised path. Those wait out the staleness
	// bound before becoming general changes; see Unconsumed for why the wait is not optional.
	owned map[string]bool
	// staleness is how long an owned entry waits. Zero uses DefaultAuditStaleness.
	staleness time.Duration
	// order is the insertion order of `general`, so the events a cycle emits are deterministic
	// rather than dependent on Go's map iteration.
	order []string
	// scope is the log-type, service and operation scope in force (FR-040). The zero value is no
	// restriction, which is the right default for a catch-all.
	scope AuditScope
	// watermark is the latest extent this index has been told was checkpointed. An entry older than
	// it is marked LATE and still emitted (§5.2).
	watermark time.Time
	// Skew observes GCP's clock against this process's, on the one GCP surface where the comparison
	// means something (004 T142, 003 FR-153). Nil is a valid value and observes nothing.
	//
	// It is here rather than beside the node mappers for a reason worth stating, because the obvious
	// place is wrong. A Cloud Run service's `createTime` or a Cloud SQL instance's is a HISTORICAL
	// fact — it can be months old — so comparing it to this process's clock would report a
	// three-month "skew" that is not a clock disagreement at all, and FR-058's one-minute threshold
	// would be exceeded by every node a poll ever read. Skew is only measurable against an instant
	// the platform states about something that just happened.
	//
	// The audit stream is that, and GCP is the one platform here that makes the measurement clean:
	// it states BOTH its event instant (`timestamp`) and its own delivery instant
	// (`receiveTimestamp`), so `receiveTimestamp − timestamp` is GCP-internal delivery lag — already
	// measured above for §5.2 — and `receiveTimestamp` against our arrival is what is left. Pairing
	// `timestamp` with our arrival instead would fold the two together and report the sum as skew.
	Skew *feeder.Skew
}

// NewAuditIndex returns an empty index.
func NewAuditIndex() *AuditIndex {
	return &AuditIndex{
		seen:        map[string]bool{},
		completions: map[string][]Completion{},
		requests:    map[string]Completion{},
		general:     map[string]AuditChange{},
		owned:       map[string]bool{},
	}
}

// DefaultAuditStaleness is how long an entry a specialised path might still want is held before it
// becomes a general change.
//
// It is twice the default reordering window, and the reasoning is the ordering a real poller produces:
// the audit entry that dates a traffic shift routinely arrives **one poll before** the poll that
// notices the split, because the log is queried on its own cadence. Draining at every poll marker
// would emit a general change for that entry and then a ROLLOUT for the same event one poll later —
// two changes in the graph for one thing that happened, which is worse than either failure it was
// meant to avoid.
//
// So an entry whose method a specialised path owns waits. If that path uses it, it is consumed and
// never becomes a general change; if nothing uses it within the bound, it becomes one — which is the
// case the catch-all exists for, an `UpdateService` that changed something this connector does not
// model. The bound is what stops the ledger growing without limit, and it is a stated bound rather
// than a heuristic that decides a change's *kind*.
const DefaultAuditStaleness = 2 * DefaultReorderingWindow

// NewScopedAuditIndex returns an index that files only the entries in scope (FR-040).
func NewScopedAuditIndex(scope AuditScope, staleness time.Duration) *AuditIndex {
	idx := NewAuditIndex()
	idx.scope = scope
	idx.staleness = staleness
	if idx.staleness <= 0 {
		idx.staleness = DefaultAuditStaleness
	}
	return idx
}

// Watermark tells the index how far the extent has been checkpointed, so a later entry older than it
// is marked late rather than silently indistinguishable from a timely one (§5.2).
func (idx *AuditIndex) Watermark(at time.Time) { idx.watermark = at }

// Unconsumed returns the general changes ready to be emitted, oldest first, and removes them.
//
// An entry whose method nothing specialised owns is ready immediately. An entry that a specialised
// path might still want waits out the staleness bound, because that path routinely runs a poll later
// — see DefaultAuditStaleness. `now` is the poll instant, passed in so the wait is a function of its
// inputs and a fixture can assert it at a fixed clock.
//
// Removing is what makes it once per entry: an entry emitted as a general change is not re-emitted on
// the next poll, and the trailing overlap window's duplicates were already dropped on `insertId`.
func (idx *AuditIndex) Unconsumed(now time.Time) []AuditChange {
	out := make([]AuditChange, 0, len(idx.general))
	kept := make([]string, 0, len(idx.order))
	for _, key := range idx.order {
		change, still := idx.general[key]
		if !still {
			continue
		}
		if idx.owned[key] && !idx.stale(change, now) {
			kept = append(kept, key)
			continue
		}
		out = append(out, change)
		delete(idx.general, key)
		delete(idx.owned, key)
	}
	idx.order = kept
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.Before(out[j].At)
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// stale reports whether an owned entry has waited long enough to become a general change.
func (idx *AuditIndex) stale(change AuditChange, now time.Time) bool {
	bound := idx.staleness
	if bound <= 0 {
		bound = DefaultAuditStaleness
	}
	if change.FiledAt.IsZero() || now.IsZero() {
		// Nothing to measure from. Holding it for ever would hide it, so it is released and whatever
		// is wrong with it is the caller's to report.
		return true
	}
	return now.Sub(change.FiledAt) > bound
}

// ConsumeGeneral removes an entry from the general ledger, because a specialised path used it.
//
// It is exported because the consumption is the *caller's* statement, not the index's: `Take` handing
// out a completion is not the same thing as a change being emitted from it, and a path that took a
// completion and then decided the poll showed nothing must leave the entry for the catch-all.
func (idx *AuditIndex) ConsumeGeneral(key string) {
	if key == "" {
		return
	}
	delete(idx.general, key)
	delete(idx.owned, key)
}

// ErrDataAccessStream is returned for an entry from the stream this feeder never reads.
var ErrDataAccessStream = fmt.Errorf("gcp: an entry from the data-access log stream")

// Ingest reads one page of audit entries.
//
// It returns how many entries were new, so a caller can tell an empty page from a page of
// duplicates — which matters because §5.1 makes one pagination rule a correctness rule: if
// `nextPageToken` appears but `entries` is empty, the search is **not** finished.
func (idx *AuditIndex) Ingest(raw []byte, arrivedAt time.Time) (int, error) {
	var payload auditPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return 0, fmt.Errorf("gcp: decoding a %s payload: %w", PayloadAuditEntries, err)
	}
	fresh := 0
	for _, entry := range payload.Entries {
		if strings.Contains(entry.LogName, DataAccessLogSuffix) {
			return fresh, fmt.Errorf("%w (%s). The data-access stream is not read and no permission "+
				"for it is requested (FR-042); an entry from it in a recording is a mistake rather "+
				"than something to process quietly", ErrDataAccessStream, entry.LogName)
		}
		if entry.InsertID != "" {
			if idx.seen[entry.InsertID] {
				continue
			}
			idx.seen[entry.InsertID] = true
		}
		fresh++
		if !entry.ReceiveTimestamp.IsZero() && !entry.Timestamp.IsZero() {
			if lag := entry.ReceiveTimestamp.Sub(entry.Timestamp); lag >= 0 {
				idx.lag = append(idx.lag, lag)
			}
		}
		// GCP's delivery clock against ours. Reported, never used to correct either — the entry is
		// recorded below at its own `timestamp` whatever this says (FR-153, FR-058).
		idx.Skew.Observe(entry.ReceiveTimestamp, arrivedAt)
		idx.record(entry, arrivedAt)
	}
	return fresh, nil
}

// record files one entry against its operation, and as a candidate general change.
func (idx *AuditIndex) record(entry auditEntry, arrivedAt time.Time) {
	payload := entry.ProtoPayload
	if payload == nil {
		return
	}
	if !idx.scope.InScope(payload.ServiceName, payload.MethodName) {
		// Out of scope, and that is recorded in the checkpoint as the configuration in force rather
		// than left as a silence a reader would read as "nothing happened" (FR-040).
		return
	}
	idx.recordGeneral(entry, arrivedAt)

	key, ok := completionKey(payload)
	if !ok {
		return
	}
	completion := Completion{
		At:         entry.Timestamp,
		MethodName: payload.MethodName,
		Auth:       payload.authInfo(),
	}
	if entry.Operation != nil {
		completion.OperationID = entry.Operation.ID
	}

	switch {
	case entry.Operation == nil:
		// Not a long-running operation: a single entry that both requests and completes. Its
		// timestamp is the instant, and there is nothing to correlate.
		idx.completions[key] = append(idx.completions[key], completion)
	case entry.Operation.Last:
		// The completion entry. Its timestamp is the published definition of the instant — and the
		// principal is merged in from the request entry, because the completion entry of an
		// UpdateService operation carries the service agent rather than the caller.
		if request, paired := idx.requests[completion.OperationID]; paired {
			if request.Auth.PrincipalEmail != "" || request.Auth.PrincipalSubject != "" {
				completion.Auth = request.Auth
			}
			delete(idx.requests, completion.OperationID)
		}
		idx.completions[key] = append(idx.completions[key], completion)
	case entry.Operation.First:
		// The request entry. It carries the caller and **not** the instant: the operation had not
		// happened yet, and dating the shift from here would put the change minutes early, at the
		// moment somebody pressed deploy rather than the moment production served differently.
		idx.requests[completion.OperationID] = completion
	}
}

// recordGeneral files an entry as a candidate general change (auditlog.go).
//
// A long-running operation's two entries collapse onto one key — its `operation.id` — and the
// **completion** entry wins the instant while the **request** entry wins the caller, exactly as the
// specialised paths do it: the completion entry of an operation carries the service agent rather than
// the caller, and attributing a change to it would type a human's action as a controller's.
func (idx *AuditIndex) recordGeneral(entry auditEntry, arrivedAt time.Time) {
	payload := entry.ProtoPayload
	key := entry.InsertID
	if entry.Operation != nil && entry.Operation.ID != "" {
		key = entry.Operation.ID
	}
	if key == "" {
		return
	}
	candidate := AuditChange{
		Project:      projectFromLogName(entry.LogName),
		Key:          key,
		At:           entry.Timestamp,
		ReceivedAt:   entry.ReceiveTimestamp,
		FiledAt:      arrivedAt,
		ServiceName:  payload.ServiceName,
		MethodName:   payload.MethodName,
		ResourceName: payload.ResourceName,
		Auth:         payload.authInfo(),
		Late:         !idx.watermark.IsZero() && entry.Timestamp.Before(idx.watermark),
	}
	if payload.Status != nil {
		candidate.StatusCode = payload.Status.Code
	}

	// Ownership is by method: these are the two paths that emit a change of their own from an entry.
	if CloudRunServiceMutations[payload.MethodName] || CloudSQLInstanceMutations[payload.MethodName] ||
		DNSMutations[payload.MethodName] {
		idx.owned[key] = true
	}

	existing, had := idx.general[key]
	if !had {
		idx.general[key] = candidate
		idx.order = append(idx.order, key)
		return
	}
	// Merge the two halves of one operation. A request entry never supplies the instant, and a
	// completion entry never overwrites a caller the request entry already named.
	merged := existing
	if entry.Operation != nil && entry.Operation.Last {
		merged.At = candidate.At
		merged.ReceivedAt = candidate.ReceivedAt
		merged.StatusCode = candidate.StatusCode
		merged.Late = candidate.Late
	}
	if merged.Auth.Empty() && !candidate.Auth.Empty() {
		merged.Auth = candidate.Auth
	}
	if merged.ResourceName == "" {
		merged.ResourceName = candidate.ResourceName
	}
	idx.general[key] = merged
}

// projectFromLogName reads the project out of `projects/<project>/logs/...`, which is the only place
// an entry states which project's log it came from.
func projectFromLogName(logName string) string {
	parts := strings.Split(logName, "/")
	for i, part := range parts {
		if part == "projects" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

// completionKey decides which surface an entry is about and under what key it is filed, or reports
// that it is about nothing this feeder models.
//
// The key is **prefixed by surface**. A Cloud Run service and a Cloud SQL instance can share a
// project and a name — `nova-production/orders` is a plausible spelling of both — and one map keyed
// on the bare identifier would let a database's audit entry date a service's traffic shift.
func completionKey(payload *auditLog) (string, bool) {
	switch {
	case CloudRunServiceMutations[payload.MethodName]:
		svc, ok := ParseServiceResourceName(payload.ResourceName)
		if !ok {
			return "", false
		}
		return runCompletionKey(svc), true
	case CloudSQLInstanceMutations[payload.MethodName]:
		project, instance, ok := ParseSQLInstanceResourceName(payload.ResourceName)
		if !ok {
			return "", false
		}
		return sqlCompletionKey(project, instance), true
	case DNSMutations[payload.MethodName]:
		project, ok := projectFromDNSResourceName(payload.ResourceName)
		if !ok {
			return "", false
		}
		return dnsCompletionKey(project), true
	default:
		return "", false
	}
}

func runCompletionKey(svc Service) string { return "run:" + svc.Value() }

// projectFromDNSResourceName reads the project out of a Cloud DNS change's resourceName, which GCP
// writes as `projects/<project>/managedZones/<zone>/changes/<id>` or a prefix of it.
func projectFromDNSResourceName(name string) (string, bool) {
	parts := strings.Split(strings.Trim(name, "/"), "/")
	for i, part := range parts {
		if part == "projects" && i+1 < len(parts) && parts[i+1] != "" {
			return parts[i+1], true
		}
	}
	return "", false
}

// sqlCompletionKey keys on `<project>/<instance>` and not on the addressing ref, because the audit
// entry states no region (see ParseSQLInstanceResourceName).
func sqlCompletionKey(project, instance string) string { return "sql:" + project + "/" + instance }

// TakeDNSChange returns the oldest datable Cloud DNS change completion for a project.
//
// It keys on the project and not on the record, because `dns.changes.create` names the **change** in its
// resourceName and not the record sets inside it: one change can move several records, and the entry
// cannot say which. So the instant is per project per change, and a cycle in which two records moved
// takes two completions if GCP wrote two changes and holds the second if it wrote one.
func (idx *AuditIndex) TakeDNSChange(project string) *Completion {
	return idx.take(dnsCompletionKey(project))
}

// dnsCompletionKey keys on the project. See TakeDNSChange.
func dnsCompletionKey(project string) string { return "dns:" + project }

// TakeSQLInstance returns the oldest datable completion for a Cloud SQL instance and removes it.
//
// It takes the coordinates rather than an SQLInstance so that the region — which the audit entry
// never states — is visibly not part of the lookup.
func (idx *AuditIndex) TakeSQLInstance(project, instance string) *Completion {
	return idx.take(sqlCompletionKey(project, instance))
}

// AuthForSQLInstance returns the authentication evidence recorded for an instance's most recent
// mutation, without consuming it.
func (idx *AuditIndex) AuthForSQLInstance(project, instance string) AuthenticationInfo {
	return idx.authFor(sqlCompletionKey(project, instance))
}

// Take returns the oldest datable completion for a service and removes it, or nil.
//
// Consuming it is what stops two polls that both observe one shift from dating it twice: the second
// poll finds nothing and holds, which is correct — the split has not changed again.
func (idx *AuditIndex) Take(svc Service) *Completion { return idx.take(runCompletionKey(svc)) }

// take is the shared body of Take and TakeSQLInstance.
//
// It does **not** consume the general candidate. Handing out a completion is not the same thing as
// emitting a change from it: a caller that takes one and then finds the poll showed nothing must leave
// the entry for the catch-all, so the consumption is the caller's explicit statement
// (ConsumeGeneral).
func (idx *AuditIndex) take(key string) *Completion {
	pending := idx.completions[key]
	if len(pending) == 0 {
		return nil
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].At.Before(pending[j].At) })
	next := pending[0]
	if len(pending) == 1 {
		delete(idx.completions, key)
	} else {
		idx.completions[key] = pending[1:]
	}
	return &next
}

// PendingRequests is how many request entries are waiting for their completion. A poll that ends
// with pending requests has rollouts in flight, which the checkpoint states.
func (idx *AuditIndex) PendingRequests() int { return len(idx.requests) }

// Lag returns the measured `receiveTimestamp − timestamp` samples, for §5.2's reported distribution.
func (idx *AuditIndex) Lag() []time.Duration { return append([]time.Duration(nil), idx.lag...) }

// AuthFor returns the authentication evidence recorded for a service's most recent mutation, for a
// revision's creation change — there is no `CreateRevision` methodName in v2, so the principal comes
// from the service mutation that created it (contract §3.3).
func (idx *AuditIndex) AuthFor(svc Service) AuthenticationInfo {
	return idx.authFor(runCompletionKey(svc))
}

func (idx *AuditIndex) authFor(key string) AuthenticationInfo {
	pending := idx.completions[key]
	if len(pending) == 0 {
		return AuthenticationInfo{}
	}
	return pending[len(pending)-1].Auth
}

// ParseSQLInstanceResourceName reads a Cloud SQL audit entry's `resourceName`, which GCP writes as
// `projects/<project>/instances/<instance>`.
//
// **It carries no region**, and that is the reason this returns two strings rather than an
// SQLInstance: the feeder addresses an instance by `<project>/<region>/<instance>`, so an entry alone
// cannot name one. The region comes from the instance poll — `AuditIndex` therefore files Cloud SQL
// completions under `<project>/<instance>`, and the caller supplies the region it observed. Minting a
// ref with an empty or guessed region would address a different entity.
func ParseSQLInstanceResourceName(name string) (project, instance string, ok bool) {
	parts := strings.Split(strings.Trim(name, "/"), "/")
	if len(parts) != 4 || parts[0] != "projects" || parts[2] != "instances" {
		return "", "", false
	}
	if strings.TrimSpace(parts[1]) == "" || strings.TrimSpace(parts[3]) == "" {
		return "", "", false
	}
	return parts[1], parts[3], true
}
