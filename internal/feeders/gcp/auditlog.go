// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The audit stream as the general change stream (T141–T152; FR-037–FR-043; contract §5).
//
// # The one idea this file rests on
//
// **Every admin-activity entry is a change.** Google writes the admin-activity stream for operations
// that modify configuration or metadata and for nothing else — it is always written, it cannot be
// disabled, and a read does not produce an entry. So the catch-all does not need a list of methods it
// recognises, and that is the whole point of US6: a connector that emitted a change only for the
// methods somebody wrote a case for would make the graph's answer to "what changed" mean "only the
// things we wrote a special case for", which is the failure this story exists to prevent.
//
// What *is* configurable is the **scope** — which services and which methods are in scope at all —
// and the configuration in force is recorded in the checkpoint (FR-040). That is what lets a later
// reader tell "no change happened" from "we were not looking for that kind of change". The two are
// indistinguishable without it, and the second is the one that quietly loses an investigation.
//
// # The catch-all catches what nothing else does
//
// A `google.cloud.run.v2.Services.UpdateService` entry is already the evidence that dates a traffic
// shift, and a `cloudsql.instances.update` entry already dates a configuration change. Emitting a
// general change for them as well would put two changes in the graph for one event.
//
// So an entry is filed, and a specialised path that uses it **consumes** it. At the end of the cycle
// whatever was not consumed becomes a general change. That covers the case an allowlist could not: an
// `UpdateService` that changed something this connector does not model, or a Cloud SQL update whose
// diff was empty because it touched a user rather than a flag. The entry is not lost, and the change
// it describes is not invented either — it says what GCP's own operation name says and no more.
//
// # `timestamp` is valid time; `receiveTimestamp` never is
//
// Stated in audit.go and restated here because this is the file that emits: `timestamp` is the event
// instant and the **only** field used as valid time. `receiveTimestamp` is the delivery instant and
// is used only for lag measurement and watermarking. Conflating them would make every delayed entry a
// lie about when production changed.
//
// # A late entry is still emitted
//
// An entry arriving after the extent it belongs to has been checkpointed is emitted with **its own**
// instant as valid time, and the gap stands in the checkpoint (§5.2). It is not clamped forward to the
// current extent and it is not dropped: changes learned after an investigation has run are exactly
// what feature 002's reopening path exists for, and the reopening cannot happen for a change that was
// never emitted.

// AuditScope is the log-type, service and operation scope in force (FR-040).
//
// An empty Services or Methods list means **no restriction on that axis**, which is the right default
// for a catch-all: the operator narrows it, and the narrowing is recorded. A default that listed
// services would make the connector silently blind to every service nobody thought of.
type AuditScope struct {
	// Services restricts `protoPayload.serviceName`. Empty means every service.
	Services []string
	// Methods restricts `protoPayload.methodName` exactly. Empty means every method.
	Methods []string
	// ExcludeMethods drops methods that are in scope by the two lists above. It is how an operator
	// silences a noisy platform method without narrowing the scope to an allowlist.
	ExcludeMethods []string
}

// InScope reports whether an entry's service and method are in scope.
func (s AuditScope) InScope(serviceName, methodName string) bool {
	if slices.Contains(s.ExcludeMethods, methodName) {
		return false
	}
	if len(s.Services) > 0 && !slices.Contains(s.Services, serviceName) {
		return false
	}
	if len(s.Methods) > 0 && !slices.Contains(s.Methods, methodName) {
		return false
	}
	return true
}

// String renders the scope for the checkpoint. "every service" and "every method" are spelled out
// rather than left as an empty list, because an empty list in a note reads as "nothing was in scope".
func (s AuditScope) String() string {
	parts := []string{"services=" + scopeList(s.Services, "every"), "methods=" + scopeList(s.Methods, "every")}
	if len(s.ExcludeMethods) > 0 {
		parts = append(parts, "excluded_methods="+scopeList(s.ExcludeMethods, "none"))
	}
	return strings.Join(parts, " ")
}

func scopeList(values []string, empty string) string {
	if len(values) == 0 {
		return empty
	}
	sorted := append([]string(nil), values...)
	sort.Strings(sorted)
	return "[" + strings.Join(sorted, ",") + "]"
}

// The audit-change properties.
const (
	// PropAuditService is `protoPayload.serviceName`, which says which API was called.
	PropAuditService = "sre.gcp.audit_service"
	// PropAuditResource is `protoPayload.resourceName` as GCP wrote it. It is recorded on **every**
	// audit-derived change, including the ones that also carry a resolved target: the resource name
	// is what a reader checks the change against in the console, and it is the only thing recorded
	// for a resource in a namespace this connector does not own.
	PropAuditResource = "sre.gcp.audit_resource"
	// PropAuditStatusCode is `protoPayload.status.code`. Zero is success, and a non-zero code is a
	// change that was **attempted and refused** — which is a different fact from one that happened,
	// and a reader ranking causes needs to be able to tell them apart.
	PropAuditStatusCode = "sre.gcp.audit_status_code"
	// PropAuditTargetUnresolved states that the resource the entry names is in no namespace this
	// connector mints in, so the change carries no target ref (FR-036, and see targetsFor).
	PropAuditTargetUnresolved = "sre.gcp.audit_target_unresolved"
	// PropAuditLate marks a change whose entry arrived after the extent it belongs to had been
	// checkpointed. The change is emitted at its own instant; the marker is how a reader knows the
	// graph learned it late (§5.2).
	PropAuditLate = "sre.gcp.audit_arrived_late"
	// PropAuditPipelineRef is the pipeline or build reference the entry carries, where it carries
	// one. It is claimed as well as recorded: it is how feature 004's build-system changes resolve
	// against this change rather than duplicating it (FR-043).
	PropAuditPipelineRef = "sre.gcp.audit_pipeline_ref"
)

// AuditTargetUnresolved is what a change says when the resource it names is not one this connector
// addresses.
const AuditTargetUnresolved = "the resource this entry names is in no namespace this connector mints " +
	"in, so the change carries the resource name and no target ref. A ref minted in another " +
	"connector's namespace would be this connector naming entities it does not own (FR-116)"

// AuditLateArrival is what a late change says about itself.
const AuditLateArrival = "this entry arrived after the extent it belongs to had been checkpointed. It " +
	"is emitted at its own instant rather than clamped forward or dropped, and the gap stands in the " +
	"checkpoint (contracts/gcp-feeder.md §5.2)"

// AuditChange is one in-scope admin-activity entry, ready to be emitted as a change.
type AuditChange struct {
	// Project is the project whose log the entry came from.
	Project string
	// Key is the change's deterministic identity: the operation id where the entry belongs to a
	// long-running operation, and the `insertId` otherwise. Both are GCP's own identifiers, so
	// re-reading the trailing overlap window is a no-op rather than a second change.
	Key string
	// At is the entry's `timestamp`, which is the only field used as valid time.
	At time.Time
	// ReceivedAt is `receiveTimestamp`, kept for lag measurement and never used as valid time.
	ReceivedAt time.Time
	// FiledAt is the instant the poll that carried this entry arrived. It is what the staleness bound
	// is measured from, and **not** the entry's own timestamp: a late entry's timestamp can be hours
	// old, and measuring from it would release the entry for the catch-all on the very poll it
	// arrived on — before the poll that would have used it had run (DefaultAuditStaleness).
	FiledAt time.Time
	// ServiceName and MethodName are GCP's own names for what was called.
	ServiceName string
	MethodName  string
	// ResourceName is `protoPayload.resourceName`.
	ResourceName string
	// StatusCode is `protoPayload.status.code`: zero for success.
	StatusCode int32
	// Auth is the caller, for the actor ladder. It is never stored — the ladder classifies it and
	// drops the principal (FR-135).
	Auth AuthenticationInfo
	// Late marks an entry that arrived after its extent was checkpointed.
	Late bool
}

// ChangeRefAuditEntry is the deterministic ref of a general audit change.
//
// Keyed on the project and GCP's own identifier for the operation, so the same entry read on twenty
// polls of the trailing overlap window is one change — and two entries of one long-running operation
// are one change, because they share `operation.id`.
func ChangeRefAuditEntry(project, key string) *graphv1.Ref {
	return feeder.Ref(NSChange, "audit/"+project+"/"+key)
}

// The audit taxonomy mapping (FR-039, contract §5.3).
//
// A kind from the published taxonomy where one fits, and `CHANGE_KIND_OTHER` **with GCP's own
// operation name** otherwise — never dropped. `IAM_CHANGE` and `QUOTA_CHANGE` exist in the taxonomy
// (ADR-0006 D3), so the "other" fallback no longer covers them and a fixture asserting otherwise
// would be asserting a regression.
//
// The mapping is on **method-name shape** rather than an exact list, and that is the one place this
// file is deliberately loose. `SetIamPolicy` is `SetIamPolicy` on every Google API there is, so
// matching the suffix covers services nobody has thought of — which is what a catch-all has to do.
// An exact list would silently classify every unlisted IAM change as "other", and "other" is where a
// reader stops looking.
func ChangeKindForMethod(methodName string) (graphv1.ChangeKind, string) {
	last := methodName
	if idx := strings.LastIndex(methodName, "."); idx >= 0 {
		last = methodName[idx+1:]
	}
	switch {
	case strings.EqualFold(last, "SetIamPolicy"), strings.Contains(strings.ToLower(methodName), ".iam."),
		strings.Contains(strings.ToLower(last), "iampolicy"):
		return graphv1.ChangeKind_IAM_CHANGE, ""
	case strings.Contains(strings.ToLower(methodName), "quota"):
		return graphv1.ChangeKind_QUOTA_CHANGE, ""
	case strings.Contains(strings.ToLower(methodName), "autoscal"),
		strings.Contains(strings.ToLower(last), "resize"),
		strings.Contains(strings.ToLower(last), "setinstancetemplate"):
		return graphv1.ChangeKind_SCALING, ""
	case strings.HasPrefix(strings.ToLower(methodName), "serviceusage."),
		strings.Contains(strings.ToLower(last), "enableservice"),
		strings.Contains(strings.ToLower(last), "disableservice"):
		// Enabling or disabling an API on a project is that project's configuration changing.
		return graphv1.ChangeKind_CONFIG_CHANGE, ""
	default:
		// Every other admin-activity method, **including deletions**. A resource deletion is a real
		// change and the taxonomy names no kind for it, so it is `OTHER` carrying GCP's own operation
		// name rather than squeezed into a kind that would mislead a reader filtering on it. That is
		// FR-039 read as written: a kind where one fits, and the operation name where none does.
		return graphv1.ChangeKind_CHANGE_KIND_OTHER, methodName
	}
}

// AuditSummary renders the one-line summary a change node carries. It is built from the typed fields
// — the method, the resource — and never from anything a caller could have written into a log.
func AuditSummary(change AuditChange) string {
	resource := change.ResourceName
	if idx := strings.LastIndex(resource, "/"); idx >= 0 && idx+1 < len(resource) {
		resource = resource[idx+1:]
	}
	summary := change.MethodName
	if resource != "" {
		summary += " on " + resource
	}
	if change.StatusCode != 0 {
		// A refused change is a different fact from one that happened, and the summary says so
		// rather than leaving it to a property a reader may not open.
		summary += fmt.Sprintf(" (refused, status %d)", change.StatusCode)
	}
	return summary
}

// AuditChangeFact builds the change for one general audit entry (T141, T144, T147, T148).
func AuditChangeFact(change AuditChange, actor Actor) (feeder.ChangeFact, error) {
	if change.At.IsZero() {
		return feeder.ChangeFact{}, fmt.Errorf("gcp: audit entry %s/%s has no timestamp; it is the "+
			"event instant and the only field used as valid time, so an entry without one is not a "+
			"change that can be dated (contracts/gcp-feeder.md §5.2)", change.Project, change.Key)
	}
	if strings.TrimSpace(change.Key) == "" {
		return feeder.ChangeFact{}, fmt.Errorf("gcp: an audit entry with neither an operation id nor " +
			"an insertId; one of them is GCP's duplicate key, and without either the same entry read " +
			"twice would be two changes")
	}
	kind, other := ChangeKindForMethod(change.MethodName)
	return feeder.ChangeFact{
		Ref:       ChangeRefAuditEntry(change.Project, change.Key),
		Kind:      kind,
		KindOther: other,
		Summary:   AuditSummary(change),
		ActorKind: actor.Kind,
		Targets:   targetsFor(change.ResourceName),
		ValidAt:   change.At,
	}, nil
}

// AuditChangeProps renders the change's properties.
func AuditChangeProps(change AuditChange, actor Actor) *feeder.Props {
	props := feeder.NewProps().
		Str(PropProject, change.Project).
		Str(PropAuditService, change.ServiceName).
		// GCP's own operation name, on **every** audit-derived change and not only the `OTHER` ones.
		// FR-039 requires it where the taxonomy has no equivalent; recording it always costs nothing
		// and is what makes a mapped change checkable against the console.
		Str(PropAuditMethod, change.MethodName).
		Str(PropActorRung, actor.Rung)
	if change.ResourceName != "" {
		props = props.Str(PropAuditResource, change.ResourceName)
	}
	if change.StatusCode != 0 {
		props = props.Int(PropAuditStatusCode, int64(change.StatusCode))
	}
	if len(targetsFor(change.ResourceName)) == 0 && change.ResourceName != "" {
		props = props.Str(PropAuditTargetUnresolved, AuditTargetUnresolved)
	}
	if change.Late {
		props = props.Str(PropAuditLate, AuditLateArrival)
	}
	if len(actor.Evidence) > 0 {
		props = props.Strs(PropActorEvidence, actor.Evidence...)
	}
	return props
}

// targetsFor resolves an audit entry's `resourceName` to the refs this connector addresses.
//
// It returns refs **only** in namespaces this connector mints in. A GCS bucket, a firewall rule or a
// BigQuery dataset named by an entry gets no target: minting a ref outside the declared set fails
// `pkg/feeder/testkit` (FR-116) and would be this connector naming entities another one owns.
//
// The change is still emitted, carrying the resource name. That is FR-036's "kept and marked
// unattached" from the feeder's side — and where the target IS one this connector addresses but the
// graph has not seen it yet, the projector records it as unattached and creates the `changed-by` edge
// the moment it appears. Neither case mints a placeholder.
func targetsFor(resourceName string) []*graphv1.Ref {
	if svc, ok := ParseServiceResourceName(resourceName); ok {
		targets := []*graphv1.Ref{svc.Ref()}
		if rev, isRevision := ParseRevisionResourceName(resourceName); isRevision {
			targets = append(targets, rev.Ref())
		}
		return targets
	}
	if project, instance, ok := ParseSQLInstanceResourceName(resourceName); ok {
		// A Cloud SQL audit entry states no region, and a ref with a guessed one addresses a
		// different entity — so the change carries the resource name and no target. The projector's
		// unattached list is the wrong tool here: there is no ref to wait for.
		_, _ = project, instance
		return nil
	}
	if cluster, ok := parseClusterResourceName(resourceName); ok {
		return []*graphv1.Ref{cluster.Ref()}
	}
	return nil
}

// parseClusterResourceName reads `projects/P/locations/L/clusters/C`.
func parseClusterResourceName(name string) (GKECluster, bool) {
	parts := strings.Split(strings.Trim(name, "/"), "/")
	if len(parts) != 6 || parts[0] != "projects" || parts[2] != "locations" || parts[4] != "clusters" {
		return GKECluster{}, false
	}
	cluster := GKECluster{Project: parts[1], Location: parts[3], Name: parts[5]}
	if cluster.Validate() != nil {
		return GKECluster{}, false
	}
	return cluster, true
}

// AuditClaims returns the identity claims an audit entry carries (T150, FR-043).
//
// The point of them is feature 004: a Vercel deployment or a GitHub Actions run that knows the same
// image digest or the same pipeline reference must **resolve against** this change rather than
// duplicate it. A claim nobody emits is a merge that never happens, and the symptom is two rows for
// one deploy in front of somebody being paged.
//
// The claims are only ever about identifiers this connector can read from the entry. It does not
// invent a pipeline reference from a user agent: `gcloud/456.0.0` says which tool was used, not which
// pipeline ran, and a claim built from it would merge every deploy from that tool version.
func AuditClaims(change AuditChange) []Claim {
	ref := ChangeRefAuditEntry(change.Project, change.Key)
	claims := []Claim{{
		Namespace: ref.GetNamespace(), Value: ref.GetValue(),
		Why: "the ref this feeder addresses the audit-derived change by",
	}}
	// The target resource name, so a source that knows the resource by its GCP name resolves onto
	// the same entity. It is claimed in the namespace of whatever the resource IS, where this
	// connector owns one.
	if svc, ok := ParseServiceResourceName(change.ResourceName); ok {
		claims = append(claims, Claim{
			Namespace: NSService, Value: svc.ResourceName(),
			Why: "the fully qualified resource name the audit entry names",
		})
		if rev, isRevision := ParseRevisionResourceName(change.ResourceName); isRevision {
			claims = append(claims, Claim{
				Namespace: NSRevision, Value: rev.ResourceName(),
				Why: "the revision the audit entry names",
			})
		}
	}
	return claims
}

// AuditLagSamples returns the `receiveTimestamp − timestamp` samples for a set of entries, which is
// what makes the configured reordering window justifiable from data rather than asserted (T145).
//
// A negative sample is dropped rather than folded in: it means the two clocks disagree, which is a
// fact about the clocks and not about ingestion.
func AuditLagSamples(changes []AuditChange) []time.Duration {
	var out []time.Duration
	for _, change := range changes {
		if change.ReceivedAt.IsZero() || change.At.IsZero() {
			continue
		}
		if lag := change.ReceivedAt.Sub(change.At); lag >= 0 {
			out = append(out, lag)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Reading a window (T141, T143; contract §5.1)
// ---------------------------------------------------------------------------
//
// # The pagination rule is a correctness rule, not an efficiency one
//
// **If `nextPageToken` appears but `entries` is empty, the search is not finished.** An
// implementation that stops on an empty page silently under-reads its own window — and the checkpoint
// then claims an extent it did not cover, which is worse than a stated gap because nothing anywhere
// says the window was short.
//
// It is the same rule the telemetry backend's log mining applies, and it is spelled out in both
// places because it has been got wrong in both: an empty page looks like the end of a search in every
// paging API a reader has met before.
//
// # The budget is the feeder's own, not folklore
//
// `pageSize` defaults to 50 on `entries.list` and **its maximum is not documented** (research §6): the
// widely-cited 1,000-entry page cap and ~10 MB response cap appear nowhere in Google's reference. So
// this loop designs against neither. It requests a large page, accepts whatever comes back, paginates,
// and enforces its **own** hard entry, page and wall-clock budget — stating in the outcome where it
// stopped and by which criterion. A limit taken from folklore is a limit that changes without a
// changelog.

// AuditPage is one page of `entries.list` as a reader returns it: the raw response and the token.
type AuditPage struct {
	// Raw is the response body, in the shape a recording stores.
	Raw []byte
	// NextPageToken is GCP's token. Empty means the search is finished — and it is the **only** thing
	// that means that. See the rule above.
	NextPageToken string
}

// AuditReadBudget bounds one window read. A zero field uses the published default beside it.
type AuditReadBudget struct {
	MaxPages   int
	MaxEntries int
	MaxWall    time.Duration
}

// The published defaults for a window read (contract §5.1).
const (
	// DefaultMaxAuditPages bounds the pages one window may take.
	DefaultMaxAuditPages = 50
	// DefaultMaxAuditEntries bounds the entries one window may read.
	DefaultMaxAuditEntries = 20_000
	// DefaultMaxAuditWall bounds the wall-clock time one window may take. It is short relative to the
	// poll cadence on purpose: a read that overran its own cadence would queue polls behind it.
	DefaultMaxAuditWall = 60 * time.Second
)

func (b AuditReadBudget) pages() int {
	if b.MaxPages <= 0 {
		return DefaultMaxAuditPages
	}
	return b.MaxPages
}

func (b AuditReadBudget) entries() int {
	if b.MaxEntries <= 0 {
		return DefaultMaxAuditEntries
	}
	return b.MaxEntries
}

func (b AuditReadBudget) wall() time.Duration {
	if b.MaxWall <= 0 {
		return DefaultMaxAuditWall
	}
	return b.MaxWall
}

// The criteria a window read can stop by. They are published strings because they reach the
// checkpoint: "partial" without a criterion is a gap nobody can act on.
const (
	// AuditStopFinished is GCP saying the search is over: no next-page token.
	AuditStopFinished = "the search finished: no nextPageToken"
	// AuditStopPages, AuditStopEntries and AuditStopWall are the feeder's own budget.
	AuditStopPages   = "the feeder's page budget for one window"
	AuditStopEntries = "the feeder's entry budget for one window"
	AuditStopWall    = "the feeder's wall-clock budget for one window"
)

// AuditReadOutcome is what one window read did.
type AuditReadOutcome struct {
	// Pages and Entries are what it actually read. Entries counts **fresh** entries: the trailing
	// overlap window's duplicates are dropped on `insertId` and are not spent against the budget.
	Pages   int
	Entries int
	// Complete is true only when GCP said the search was finished. Anything else is a partial read,
	// and the checkpoint says so (FR-012).
	Complete bool
	// Criterion names why it stopped, from the published set above.
	Criterion string
}

// ReadAuditWindow drains one window through the pagination rule and the feeder's own budget.
//
// `next` is the reader: it is given the page token — empty for the first page — and returns the page.
// Passing it in rather than taking a client is what lets the rule be asserted against a reader that
// returns an empty page with a token, which is the case the rule exists for and which no live client
// produces on demand.
//
// `now` is the clock, passed in so the wall-clock budget is a function of its inputs.
func ReadAuditWindow(idx *AuditIndex, arrivedAt time.Time, budget AuditReadBudget,
	now func() time.Time, next func(token string) (AuditPage, error)) (AuditReadOutcome, error) {
	var outcome AuditReadOutcome
	started := now()
	token := ""
	for {
		if outcome.Pages >= budget.pages() {
			outcome.Criterion = AuditStopPages
			return outcome, nil
		}
		if now().Sub(started) > budget.wall() {
			outcome.Criterion = AuditStopWall
			return outcome, nil
		}
		page, err := next(token)
		if err != nil {
			return outcome, err
		}
		outcome.Pages++
		fresh, err := idx.Ingest(page.Raw, arrivedAt)
		outcome.Entries += fresh
		if err != nil {
			return outcome, err
		}
		if page.NextPageToken == "" {
			// The only thing that means the search is over.
			outcome.Complete = true
			outcome.Criterion = AuditStopFinished
			return outcome, nil
		}
		// An empty page with a token is NOT the end. The loop continues, deliberately, and that is
		// the whole of T143: `fresh == 0` here means either a page of duplicates or a genuinely empty
		// page, and neither says the search is finished.
		if outcome.Entries >= budget.entries() {
			outcome.Criterion = AuditStopEntries
			return outcome, nil
		}
		token = page.NextPageToken
	}
}
