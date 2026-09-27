// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	eventlog "github.com/Pierre-Theophile/aisre/internal/log"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// Cloud Monitoring alerts on the shared intake (T113, T114, T119, T120; FR-046–FR-053;
// contracts/gcp-feeder.md §6).
//
// The half that is GA and the half that is not are deliberately separate, because they fail
// separately:
//
//	projects.alertPolicies.list   GA, in the first-party GAPIC. It gives the ALERT nodes: the
//	                              policy identifier as identity, the display name as a CLAIM, the
//	                              severity where stated, and each condition's filter as a pointer.
//	projects.alerts.list          Public Preview, bound only in a maintenance-mode client. It
//	                              gives the TRANSITIONS, and it sits behind a declared capability
//	                              flag (transport.go, T115).
//
// With the flag off, ALERT nodes still exist and `monitor_state` answers NO_DATA naming the absent
// source. That is not a degraded mode bolted on afterwards: it is the contract's own answer for a
// source the organisation does not have, and `gcp-alert-transitions-unavailable-01` ships it
// tested rather than assumed.
//
// # Why the display name is a claim and not an identity
//
// A policy's display name is what an operator renames on a Tuesday. Keyed on it, every rename
// would be a new alert with no history and the old one would silently stop firing. The policy
// identifier GCP assigns is stable across renames, so it is the identity — and the display name is
// emitted as a claim, which is how the resolution layer gets to propose a merge if somebody
// genuinely does recreate a policy under the old name (FR-115).
//
// # Why a grouped policy produces one ALERT per group
//
// A policy whose condition aggregates `groupByFields` opens one incident per group, and SC-005 says
// the policy-level entity must never be reported as alerting because one group is. The projector
// writes an alert's state onto the node its `monitor` ref names, so N groups sharing one ref would
// mean N states on one node and the last write wins — an investigation asking "is this firing"
// would get whichever group was polled last. So the alerting entity is the **group**, addressed
// `<policy>#<group>`, and the policy node carries the definition and no state.

// The monitoring payload kinds.
const (
	// PayloadAlertPolicies is a `projects.alertPolicies.list` response.
	PayloadAlertPolicies = "alert_policies"
	// PayloadAlerts is a `projects.alerts.list` response — the Preview surface, recorded only
	// when the capability flag is on.
	PayloadAlerts = "alerts"
)

// AlertPolicySeparator joins a policy to the group whose incidents it opens. `#` is used because
// it cannot occur in a policy resource name and is not produced by any monitored-resource label,
// so no group key can be made to collide with another policy.
const AlertPolicySeparator = "#"

// AlertPolicy is a Cloud Monitoring alert policy's coordinates.
type AlertPolicy struct {
	// Project is the project the policy lives in.
	Project string
	// ID is the identifier GCP assigns, the last segment of the resource name.
	ID string
}

// ParseAlertPolicyName reads `projects/<project>/alertPolicies/<id>` as the API returns it.
func ParseAlertPolicyName(name string) (AlertPolicy, error) {
	parts := strings.Split(strings.TrimPrefix(name, "//monitoring.googleapis.com/"), "/")
	if len(parts) != 4 || parts[0] != "projects" || parts[2] != "alertPolicies" {
		return AlertPolicy{}, fmt.Errorf(
			"gcp: %q is not an alert policy resource name; the form is projects/<project>/alertPolicies/<id>", name)
	}
	policy := AlertPolicy{Project: parts[1], ID: parts[3]}
	return policy, policy.Validate()
}

// Validate refuses coordinates with an empty part.
func (a AlertPolicy) Validate() error {
	for name, part := range map[string]string{"project": a.Project, "policy id": a.ID} {
		if strings.TrimSpace(part) == "" {
			return fmt.Errorf("%w: the %s of an alert policy", ErrEmptyIdentifierPart, name)
		}
	}
	return nil
}

// Value renders the policy identifier: the fully qualified resource name.
//
// The resource name rather than the bare id, because the bare id is unique only within a project
// and this graph holds several. A reader who sees the value knows which project it is in without a
// second lookup.
func (a AlertPolicy) Value() string { return "projects/" + a.Project + "/alertPolicies/" + a.ID }

// Ref returns the policy's addressing ref.
func (a AlertPolicy) Ref() *graphv1.Ref { return feeder.Ref(NSAlertPolicy, a.Value()) }

// Group returns the alerting entity for one group of this policy. An empty key is the policy
// itself, which is the ungrouped case: one policy, one alert, one history.
func (a AlertPolicy) Group(key string) AlertGroup { return AlertGroup{Policy: a, Key: key} }

// AlertGroup is the entity that alerts: a policy, or one group within it.
type AlertGroup struct {
	Policy AlertPolicy
	// Key is the group key, empty for an ungrouped policy.
	Key string
}

// Value renders the alerting entity's identifier.
func (g AlertGroup) Value() string {
	if g.Key == "" {
		return g.Policy.Value()
	}
	return g.Policy.Value() + AlertPolicySeparator + g.Key
}

// Ref returns the alerting entity's addressing ref.
func (g AlertGroup) Ref() *graphv1.Ref { return feeder.Ref(NSAlertPolicy, g.Value()) }

// The properties an ALERT node carries from the policy read. The state properties are the
// projector's (`sre.alert.*`) and are deliberately not written here: a policy read says what the
// policy IS, and a transition says what it is DOING. A feeder that wrote a state from a policy
// read would be asserting a state nobody observed.
const (
	// PropAlertPolicyDisplayName is the display name, as a property as well as a claim. The
	// property is what an operator reads; the claim is what the resolution layer can merge on.
	PropAlertPolicyDisplayName = "sre.gcp.alert_policy_display_name"
	// PropAlertPolicySeverity is the severity the policy declares, written only where the policy
	// declares one. An unspecified severity is not "low": it is a policy that said nothing, and
	// inventing a level would put a number in an operator's triage order that nobody chose.
	PropAlertPolicySeverity = "sre.gcp.alert_policy_severity"
	// PropAlertPolicyEnabled is whether the policy is enabled. A disabled policy still exists
	// and still explains why nothing fired, which is why it is recorded rather than skipped.
	PropAlertPolicyEnabled = "sre.gcp.alert_policy_enabled"
	// PropAlertPolicyConditions lists the condition display names, so a reader can see what the
	// policy watches for without parsing a filter.
	PropAlertPolicyConditions = "sre.gcp.alert_policy_conditions"
	// PropAlertPolicyCombiner is how the conditions combine (AND, OR, AND_WITH_MATCHING_RESOURCE).
	PropAlertPolicyCombiner = "sre.gcp.alert_policy_combiner"
	// PropAlertPolicyGroupByFields are the fields the policy's conditions aggregate by, which is
	// what makes it a grouped policy and therefore what makes one incident per group.
	PropAlertPolicyGroupByFields = "sre.gcp.alert_policy_group_by_fields"
	// PropAlertWatches lists the entities this policy's condition filters name (T114, FR-048).
	//
	// It is a property and not a set of WATCHES edges, and the reason is the projector's own: an
	// edge to a ref the graph has not observed mints a placeholder endpoint — an entity with no
	// type and no description, indistinguishable to an investigation from an alert watching
	// something real. A feeder cannot tell which of its refs the graph already holds, so the
	// EDGES are asserted by transitions, where `alert.transition.watches` goes through the
	// unattached convention (internal/projector/alert_transition.go, T017).
	//
	// That leaves one case this property exists for: a policy that has never fired has no
	// transition and therefore no edge, and "what does this alert watch" must still be answerable
	// — otherwise a quiet policy is indistinguishable from one watching nothing.
	PropAlertWatches = "sre.gcp.alert_watches"
	// PropAlertPolicyPreview marks a node whose transitions come from the Public Preview
	// incident API. It is on the node rather than in a release note because a reader looking at
	// a state history is entitled to know the surface it came from is Pre-GA and its labels may
	// change under them.
	PropAlertPolicyPreview = "sre.gcp.alert_transitions_preview_api"
)

// AlertCondition is one condition of a policy, reduced to what the graph records.
type AlertCondition struct {
	// Name is the condition's own resource name.
	Name string
	// DisplayName is what the console shows.
	DisplayName string
	// Kind names which of the condition oneof's members was set, so a reader can tell a
	// threshold from an absence without a filter to read.
	Kind string
	// Filter is the Monitoring filter the condition evaluates, which becomes a pointer. It is
	// empty for a condition kind that carries a query rather than a filter — PromQL, MQL, SQL —
	// and that emptiness is stated rather than papered over: see UnexecutableKinds.
	Filter string
	// GroupByFields are the condition's aggregation grouping, which is what makes the policy
	// open one incident per group.
	GroupByFields []string
}

// The condition kinds. They are named rather than inferred because the absence of a filter is a
// fact about the kind: a PromQL or MQL or SQL condition has a query this feeder does not mint
// pointers in, and saying which kind it was is what lets a reader tell "no pointer because there
// is no filter" from "no pointer because nobody wrote one" (FR-085's reasoning, applied here).
const (
	ConditionThreshold = "threshold"
	ConditionAbsence   = "absence"
	ConditionLogMatch  = "log_match"
	ConditionMQL       = "monitoring_query_language"
	ConditionPromQL    = "prometheus_query_language"
	ConditionSQL       = "sql"
	ConditionUnknown   = "unknown"
)

// UnexecutableKinds are the condition kinds whose query is not in a vocabulary this feeder mints
// pointers in. A condition of one of these kinds contributes a claim and a property and no
// pointer, and the reason is recorded on the node.
var UnexecutableKinds = []string{ConditionMQL, ConditionPromQL, ConditionSQL, ConditionUnknown}

// PropAlertPolicyUnexecutableConditions lists the conditions whose query this feeder cannot mint a
// pointer for, with the kind. Without it, a policy with one threshold condition and one PromQL
// condition would look like a policy with one condition.
const PropAlertPolicyUnexecutableConditions = "sre.gcp.alert_policy_unexecutable_conditions"

// AlertPolicyObservation is one policy as a poll saw it.
type AlertPolicyObservation struct {
	// Policy is the coordinates.
	Policy AlertPolicy
	// DisplayName is the policy's display name.
	DisplayName string
	// Severity is the declared severity, empty when the policy declares none.
	Severity string
	// Enabled is whether the policy is enabled.
	Enabled bool
	// Combiner is how the conditions combine.
	Combiner string
	// Conditions are the policy's conditions.
	Conditions []AlertCondition
	// Watches are the entities the conditions' filters name, which become WATCHES edges.
	Watches []*graphv1.Ref
	// UserLabels are the policy's own labels, passed through the label policy.
	Labels Labels
	// CreateTime is the policy's creation instant where the API reports one. A policy with no
	// creation record has an unknown valid start rather than an invented one.
	CreateTime time.Time
	// MutateTime is the instant of the policy's latest edit, from its mutation record, where the API
	// reports one. It dates a later state of the policy (003 T183); it is never a creation instant.
	MutateTime time.Time
}

// ObserveAlertPolicy reads one policy.
func ObserveAlertPolicy(policy *monitoringpb.AlertPolicy, labels LabelPolicy) (AlertPolicyObservation, error) {
	var obs AlertPolicyObservation
	parsed, err := ParseAlertPolicyName(policy.GetName())
	if err != nil {
		return obs, err
	}
	obs.Policy = parsed
	obs.DisplayName = policy.GetDisplayName()
	obs.Enabled = policy.GetEnabled().GetValue()
	if combiner := policy.GetCombiner(); combiner != monitoringpb.AlertPolicy_COMBINE_UNSPECIFIED {
		obs.Combiner = combiner.String()
	}
	// Severity only where stated. `SEVERITY_UNSPECIFIED` is a policy that said nothing, and
	// mapping it to a level would put a number in a triage order nobody chose.
	if severity := policy.GetSeverity(); severity != monitoringpb.AlertPolicy_SEVERITY_UNSPECIFIED {
		obs.Severity = severity.String()
	}
	obs.Labels = labels.Apply(parsed.Project, policy.GetUserLabels())
	if record := policy.GetCreationRecord(); record.GetMutateTime() != nil {
		obs.CreateTime = record.GetMutateTime().AsTime().UTC()
	}
	if record := policy.GetMutationRecord(); record.GetMutateTime() != nil {
		obs.MutateTime = record.GetMutateTime().AsTime().UTC()
	}

	seen := map[string]struct{}{}
	for _, condition := range policy.GetConditions() {
		observed := observeCondition(condition)
		obs.Conditions = append(obs.Conditions, observed)
		for _, ref := range watchesFromFilter(observed.Filter) {
			key := ref.GetNamespace() + "=" + ref.GetValue()
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			obs.Watches = append(obs.Watches, ref)
		}
	}
	// Sorted, because two policies whose conditions are listed in a different order watch the
	// same entities and must produce the same event bytes.
	sort.Slice(obs.Watches, func(i, j int) bool {
		if obs.Watches[i].GetNamespace() != obs.Watches[j].GetNamespace() {
			return obs.Watches[i].GetNamespace() < obs.Watches[j].GetNamespace()
		}
		return obs.Watches[i].GetValue() < obs.Watches[j].GetValue()
	})
	return obs, nil
}

func observeCondition(condition *monitoringpb.AlertPolicy_Condition) AlertCondition {
	out := AlertCondition{
		Name:        condition.GetName(),
		DisplayName: condition.GetDisplayName(),
		Kind:        ConditionUnknown,
	}
	switch {
	case condition.GetConditionThreshold() != nil:
		out.Kind = ConditionThreshold
		out.Filter = condition.GetConditionThreshold().GetFilter()
		out.GroupByFields = groupByOf(condition.GetConditionThreshold().GetAggregations())
	case condition.GetConditionAbsent() != nil:
		out.Kind = ConditionAbsence
		out.Filter = condition.GetConditionAbsent().GetFilter()
		out.GroupByFields = groupByOf(condition.GetConditionAbsent().GetAggregations())
	case condition.GetConditionMatchedLog() != nil:
		out.Kind = ConditionLogMatch
		out.Filter = condition.GetConditionMatchedLog().GetFilter()
	case condition.GetConditionMonitoringQueryLanguage() != nil:
		out.Kind = ConditionMQL
	case condition.GetConditionPrometheusQueryLanguage() != nil:
		out.Kind = ConditionPromQL
	case condition.GetConditionSql() != nil:
		out.Kind = ConditionSQL
	}
	return out
}

// groupByOf collects the aggregation grouping of a condition, de-duplicated and sorted.
func groupByOf(aggregations []*monitoringpb.Aggregation) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, aggregation := range aggregations {
		for _, field := range aggregation.GetGroupByFields() {
			if _, dup := seen[field]; dup {
				continue
			}
			seen[field] = struct{}{}
			out = append(out, field)
		}
	}
	sort.Strings(out)
	return out
}

// GroupByFields is every field the policy's conditions aggregate by. A non-empty result is what
// makes this a grouped policy, and therefore what makes one alert per group.
func (o AlertPolicyObservation) GroupByFields() []string {
	seen := map[string]struct{}{}
	var out []string
	for _, condition := range o.Conditions {
		for _, field := range condition.GroupByFields {
			if _, dup := seen[field]; dup {
				continue
			}
			seen[field] = struct{}{}
			out = append(out, field)
		}
	}
	sort.Strings(out)
	return out
}

// WatchRefs renders the watched entities as `<namespace>=<value>`, sorted — the spelling a ref
// takes everywhere else in this project, so a reader can paste one into a query.
func (o AlertPolicyObservation) WatchRefs() []string {
	out := make([]string, 0, len(o.Watches))
	for _, ref := range o.Watches {
		out = append(out, ref.GetNamespace()+"="+ref.GetValue())
	}
	sort.Strings(out)
	return out
}

// Grouped reports whether this policy opens one incident per group.
func (o AlertPolicyObservation) Grouped() bool { return len(o.GroupByFields()) > 0 }

// Claims returns every identifier this feeder knows the policy by, **including the one it
// addresses it by** (FR-115), plus the display name.
//
// The display name is a claim and never an identity: an operator renames a policy on a Tuesday,
// and a graph keyed on the name would give the renamed policy no history and let the old one stop
// firing silently. As a claim it is still available to the resolution layer, which is what makes a
// genuine recreate-under-the-old-name mergeable with a rule and a rationale rather than by
// accident.
func (o AlertPolicyObservation) Claims() []Claim {
	claims := []Claim{
		{Namespace: NSAlertPolicy, Value: o.Policy.Value(),
			Why: "the ref this feeder addresses the alert policy by"},
		{Namespace: NSAlertPolicy, Value: o.Policy.ID,
			Why: "the bare policy identifier, which is what a console URL and an operator use"},
	}
	if o.DisplayName != "" {
		claims = append(claims, Claim{
			Namespace: NSAlertPolicy, Value: o.DisplayName,
			Why: "the policy's display name, which is a claim and never the identity: it is renamed",
		})
	}
	return claims
}

// Props renders the policy's properties.
func (o AlertPolicyObservation) Props(transitionsAvailable bool) *feeder.Props {
	props := feeder.NewProps().
		Str(PropProject, o.Policy.Project).
		Str(PropAlertPolicyDisplayName, o.DisplayName).
		Bool(PropAlertPolicyEnabled, o.Enabled)
	if o.Severity != "" {
		props = props.Str(PropAlertPolicySeverity, o.Severity)
	}
	if o.Combiner != "" {
		props = props.Str(PropAlertPolicyCombiner, o.Combiner)
	}
	if names := o.conditionNames(); len(names) > 0 {
		props = props.Strs(PropAlertPolicyConditions, names...)
	}
	if fields := o.GroupByFields(); len(fields) > 0 {
		props = props.Strs(PropAlertPolicyGroupByFields, fields...)
	}
	if unexecutable := o.unexecutableConditions(); len(unexecutable) > 0 {
		props = props.Strs(PropAlertPolicyUnexecutableConditions, unexecutable...)
	}
	if watches := o.WatchRefs(); len(watches) > 0 {
		props = props.Strs(PropAlertWatches, watches...)
	}
	if o.Labels.Environment != "" {
		props = props.Str(PropEnvironment, o.Labels.Environment).
			Str(PropEnvironmentSource, o.Labels.EnvironmentSource)
	}
	for key, value := range o.Labels.Props {
		props = props.Str("sre.gcp.label."+key, value)
	}
	if transitionsAvailable {
		props = props.Bool(PropAlertPolicyPreview, true)
	}
	return props
}

func (o AlertPolicyObservation) conditionNames() []string {
	out := make([]string, 0, len(o.Conditions))
	for _, condition := range o.Conditions {
		name := condition.DisplayName
		if name == "" {
			name = condition.Name
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func (o AlertPolicyObservation) unexecutableConditions() []string {
	var out []string
	for _, condition := range o.Conditions {
		if condition.Filter != "" || !slices.Contains(UnexecutableKinds, condition.Kind) {
			continue
		}
		name := condition.DisplayName
		if name == "" {
			name = condition.Name
		}
		out = append(out, name+"="+condition.Kind)
	}
	sort.Strings(out)
	return out
}

// NodeFact renders the policy as the ALERT node it is.
//
// A policy whose creation record the API did not return has an **unknown** valid start rather than
// an invented one: the policy exists now and predates this connector, and guessing a start is the
// defect FR-011 names.
func (o AlertPolicyObservation) NodeFact() feeder.NodeFact {
	fact := feeder.NodeFact{
		Ref:         o.Policy.Ref(),
		Type:        graphv1.NodeType_ALERT,
		DisplayName: o.DisplayName,
	}
	if o.CreateTime.IsZero() {
		fact.ValidFromUnknown = true
	} else {
		fact.ValidAt = o.CreateTime
	}
	return fact
}

// Pointers returns one pointer per condition that carries a filter, in the registered Monitoring
// vocabulary. A condition whose query is PromQL, MQL or SQL contributes none, and
// `unexecutableConditions` above is where that is stated.
func (o AlertPolicyObservation) Pointers() ([]*graphv1.Pointer, error) {
	attrs := map[string]string{PropProject: o.Policy.Project}
	var out []*graphv1.Pointer
	for _, condition := range o.Conditions {
		if condition.Filter == "" {
			continue
		}
		// The filter is minted as a pointer exactly as the policy states it. A condition's
		// filter is what Cloud Monitoring itself evaluates, so it is already a selector in the
		// registered vocabulary — rewriting it to pin `resource.type` would change what the
		// alert watches, which is why MetricPointer's pin is not required here and why this
		// path does not go through it.
		//
		// It carries **no join keys**, and that follows from the same fact. A join key names the
		// field that plays a role *in this selector's resource*, and this selector's resource is
		// whatever the policy's author chose — it may pin no `resource.type` at all. Declaring
		// `version -> resource.labels.revision_name` on a filter watching a Pub/Sub subscription
		// would name a label that does not exist, and a consumer splitting by it would get a
		// confident answer over one merged group. The role set is closed and a role the backend
		// cannot express is left out (ADR-0005 D6), so the map stays empty.
		conditionAttrs := map[string]string{PropProject: o.Policy.Project}
		for key, value := range watchAttributes(condition.Filter) {
			conditionAttrs[key] = value
		}
		out = append(out, feeder.NewPointer(graphv1.PointerKind_METRIC, BackendKindGCP,
			feeder.VocabGCPMonitoringFilter, condition.Filter, conditionAttrs))
	}
	out = append(out, ConsoleLink(fmt.Sprintf(
		"https://console.cloud.google.com/monitoring/alerting/policies/%s?project=%s",
		o.Policy.ID, o.Policy.Project), attrs))
	return out, nil
}

// ---- watches -----------------------------------------------------------------------------------

// equalityClause matches `field="value"` and `field = "value"`, which is the only shape this reader
// claims to understand.
//
// It has a twin in internal/backends/gcp/selector.go, and the duplication is deliberate: the
// backend must not import this package, which is the only one in the repository that links the
// Google Cloud client libraries, and the two readers want different things out of the same clauses
// — a graph Ref here, a metric type and a console region there. The grammar they share is
// published in contracts/pointer-vocabularies.md §2.2, so a change to it is a change to a document
// both files cite.
var equalityClause = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_.]*)\s*=\s*"([^"]*)"`)

// watchAttributes reads the equality clauses of a filter into the labels they name.
func watchAttributes(filter string) map[string]string {
	out := map[string]string{}
	for _, match := range equalityClause.FindAllStringSubmatch(filter, -1) {
		out[match[1]] = match[2]
	}
	return out
}

// watchesFromFilter derives the entities a condition's filter names (T114, FR-048).
//
// It returns refs for the entities the filter pins **completely**, and nothing for one it pins
// partially. That is the whole rule: a filter naming a project and a region and no service is
// about every service in that region, and minting a ref for it would invent an entity — a node
// called `project/region/` that resolves against something and names nothing anybody meant.
//
// A ref this returns may name an entity the graph has not observed. It is emitted anyway: the
// projector keeps it and marks the alert's watch unattached, and attaches it the moment the target
// appears (T017's convention). Dropping it would lose the one fact an operator needs when an alert
// fires on a service no feeder is reading.
func watchesFromFilter(filter string) []*graphv1.Ref {
	if filter == "" {
		return nil
	}
	labels := watchAttributes(filter)
	project := labels["resource.labels.project_id"]
	region := labels["resource.labels.location"]
	service := labels["resource.labels.service_name"]
	revision := labels["resource.labels.revision_name"]
	database := labels["resource.labels.database_id"]

	var out []*graphv1.Ref
	switch {
	case project != "" && region != "" && service != "" && revision != "":
		// The revision AND the service: an alert on one revision is an alert on the service it
		// serves, and an investigation walking WATCHES wants both.
		out = append(out,
			Revision{Service: Service{Project: project, Region: region, Name: service}, Revision: revision}.Ref(),
			Service{Project: project, Region: region, Name: service}.Ref())
	case project != "" && region != "" && service != "":
		out = append(out, Service{Project: project, Region: region, Name: service}.Ref())
	}
	// A Cloud SQL `database_id` is `<project>:<instance>` and carries no region, so the region
	// comes from the filter's own `resource.labels.region` where it states one.
	if database != "" {
		if ref, ok := sqlInstanceRef(database, labels["resource.labels.region"]); ok {
			out = append(out, ref)
		}
	}
	return out
}

// sqlInstanceRef reads a Cloud SQL `database_id` label — `<project>:<instance>` — into an instance
// ref. A label this reader cannot split completely produces nothing rather than a ref with an
// empty part.
func sqlInstanceRef(databaseID, region string) (*graphv1.Ref, bool) {
	project, instance, found := strings.Cut(databaseID, ":")
	if !found || project == "" || instance == "" || region == "" {
		return nil, false
	}
	return feeder.Ref(NSSQLInstance, project+"/"+region+"/"+instance), true
}

// ---- transitions -------------------------------------------------------------------------------

// The alert poll cadence (config/gcp-budget.yaml §cadences). It is the one cadence the budget may
// not stretch: SC-004 binds transition-to-observed delay to the poll interval plus one minute at
// p95, and alert polling does not touch `logging.read`, which is where the scarcity is.
const (
	// DefaultAlertPollInterval is `alert_transitions.interval`.
	DefaultAlertPollInterval = 20 * time.Second
	// MinAlertPollInterval is the floor. Below it the poll spends quota for no gain in exactness:
	// the transition instant comes from Google, not from the poll.
	MinAlertPollInterval = 15 * time.Second
	// MaxAlertPollInterval is the ceiling, and it is not configurable past this. A longer
	// interval widens the window in which a transition can open and close unseen, which is
	// exactly the number SC-004 bounds.
	MaxAlertPollInterval = 30 * time.Second
)

// AlertIncident is one Cloud Monitoring alerting incident, decoded.
//
// It is this package's own shape rather than the vendor's, and that is a deliberate exception to the
// rule the rest of this feeder follows. The only Go binding for `projects.alerts` is
// `google.golang.org/api/monitoring/v3`, whose package header says it is in maintenance mode, and
// the API itself is Public Preview with Google warning that *"the labels in the response are subject
// to change while this feature is in preview"*. Pinning the fields this feature reads means a label
// shuffle upstream is a compile error in one adapter rather than a silent change in every state
// history.
type AlertIncident struct {
	// Name is `projects/<project>/alerts/<id>`, system-assigned. It is the incident's id and is
	// deliberately NOT the alert's identity: it changes per incident, so a graph keyed on it
	// would give every firing its own alert with no history.
	Name string
	// Policy is the policy that generated the incident, from the snapshot it carries.
	Policy AlertPolicy
	// PolicyDisplayName and Severity come from the same snapshot — the policy **as it was** when
	// the incident opened, which is what makes a transition readable after the policy is edited.
	PolicyDisplayName string
	Severity          string
	// State is OPEN or CLOSED.
	State string
	// OpenTime and CloseTime are the transition instants Google reports. CloseTime is zero on an
	// open incident.
	OpenTime  time.Time
	CloseTime time.Time
	// ResourceType, Resource and Metric are the labels preserved from the generating condition.
	ResourceType string
	Resource     map[string]string
	Metric       map[string]string
}

// The published incident states.
const (
	IncidentOpen   = "OPEN"
	IncidentClosed = "CLOSED"
)

// alertsPayload and alertIncidentJSON mirror the API's own JSON, which is what a recording holds.
//
// Decoding the API's JSON rather than the client's Go structs is what makes the recorded corpus a
// recording of what Google returned. The live reader converts the client's structs into the same
// shape (transport.go), so live and replay meet at AlertIncident and neither is the other's opinion.
type alertsPayload struct {
	Alerts        []alertIncidentJSON `json:"alerts"`
	NextPageToken string              `json:"nextPageToken,omitempty"`
}

type alertIncidentJSON struct {
	Name      string `json:"name"`
	State     string `json:"state"`
	OpenTime  string `json:"openTime"`
	CloseTime string `json:"closeTime"`
	Policy    struct {
		Name        string `json:"name"`
		DisplayName string `json:"displayName"`
		Severity    string `json:"severity"`
	} `json:"policy"`
	Resource struct {
		Type   string            `json:"type"`
		Labels map[string]string `json:"labels"`
	} `json:"resource"`
	Metric struct {
		Type   string            `json:"type"`
		Labels map[string]string `json:"labels"`
	} `json:"metric"`
}

func (a alertIncidentJSON) decode() (AlertIncident, error) {
	var out AlertIncident
	policy, err := ParseAlertPolicyName(a.Policy.Name)
	if err != nil {
		return out, fmt.Errorf("gcp: incident %q carries no readable policy: %w", a.Name, err)
	}
	open, err := parseIncidentTime(a.OpenTime)
	if err != nil {
		return out, fmt.Errorf("gcp: incident %q openTime: %w", a.Name, err)
	}
	closed, err := parseIncidentTime(a.CloseTime)
	if err != nil {
		return out, fmt.Errorf("gcp: incident %q closeTime: %w", a.Name, err)
	}
	if a.State != IncidentOpen && a.State != IncidentClosed {
		return out, fmt.Errorf(
			"gcp: incident %q reports state %q, which is neither %s nor %s; a state this reader "+
				"cannot map is refused rather than guessed at, because guessing is how a Preview "+
				"API's label change becomes a wrong alert history",
			a.Name, a.State, IncidentOpen, IncidentClosed)
	}
	if open.IsZero() {
		return out, fmt.Errorf(
			"gcp: incident %q reports no openTime; the transition instant is the one thing this "+
				"event cannot be derived without (FR-046)", a.Name)
	}
	if a.State == IncidentClosed && closed.IsZero() {
		return out, fmt.Errorf(
			"gcp: incident %q is CLOSED and reports no closeTime; dating the recovery at the poll "+
				"would make two transports produce two events for one fact", a.Name)
	}
	return AlertIncident{
		Name:              a.Name,
		Policy:            policy,
		PolicyDisplayName: a.Policy.DisplayName,
		Severity:          normaliseSeverity(a.Policy.Severity),
		State:             a.State,
		OpenTime:          open,
		CloseTime:         closed,
		ResourceType:      a.Resource.Type,
		Resource:          a.Resource.Labels,
		Metric:            a.Metric.Labels,
	}, nil
}

func parseIncidentTime(value string) (time.Time, error) {
	if strings.TrimSpace(value) == "" {
		return time.Time{}, nil
	}
	at, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, err
	}
	return at.UTC(), nil
}

// normaliseSeverity drops the unspecified value. A policy that said nothing about severity has no
// severity, and `SEVERITY_UNSPECIFIED` in a graph property is a level somebody will sort on.
func normaliseSeverity(value string) string {
	if value == "" || value == "SEVERITY_UNSPECIFIED" {
		return ""
	}
	return value
}

// GroupKey derives the alerting group from the policy's aggregation grouping and this incident's
// own labels (FR-049).
//
// The key is `field=value` pairs joined with `,`, in the grouping's own sorted order, so the same
// group read twice produces the same key and therefore the same idempotency key. A grouping field
// the incident carries no label for contributes `field=` rather than being skipped: a group that is
// missing a dimension is a different group from one that has it empty, and collapsing the two would
// merge two incidents' histories.
func (i AlertIncident) GroupKey(groupBy []string) string {
	if len(groupBy) == 0 {
		return ""
	}
	parts := make([]string, 0, len(groupBy))
	for _, field := range groupBy {
		parts = append(parts, field+"="+i.labelOf(field))
	}
	return strings.Join(parts, ",")
}

// labelOf reads a `resource.labels.x` or `metric.labels.x` grouping field off the incident.
func (i AlertIncident) labelOf(field string) string {
	switch {
	case strings.HasPrefix(field, "resource.labels."):
		return i.Resource[strings.TrimPrefix(field, "resource.labels.")]
	case strings.HasPrefix(field, "metric.labels."):
		return i.Metric[strings.TrimPrefix(field, "metric.labels.")]
	case field == "resource.type":
		return i.ResourceType
	default:
		return ""
	}
}

// Watches derives the entities this incident's own labels name, which is narrower than the
// policy's: a grouped policy watches every service its filter matches, and one incident is about
// the one group that fired.
func (i AlertIncident) Watches() []*graphv1.Ref {
	project := i.Resource["project_id"]
	region := i.Resource["location"]
	service := i.Resource["service_name"]
	revision := i.Resource["revision_name"]

	var out []*graphv1.Ref
	switch {
	case project != "" && region != "" && service != "" && revision != "":
		out = append(out,
			Revision{Service: Service{Project: project, Region: region, Name: service}, Revision: revision}.Ref(),
			Service{Project: project, Region: region, Name: service}.Ref())
	case project != "" && region != "" && service != "":
		out = append(out, Service{Project: project, Region: region, Name: service}.Ref())
	}
	if database := i.Resource["database_id"]; database != "" {
		if ref, ok := sqlInstanceRef(database, i.Resource["region"]); ok {
			out = append(out, ref)
		}
	}
	return out
}

// ConsoleLink is the incident's own console URL, carried as the transition's origin_ref. It carries
// no credential (FR-082).
func (i AlertIncident) ConsoleLink() string {
	return fmt.Sprintf("https://console.cloud.google.com/monitoring/alerting/incidents/%s?project=%s",
		lastSegment(i.Name), i.Policy.Project)
}

func lastSegment(name string) string {
	if idx := strings.LastIndex(name, "/"); idx >= 0 {
		return name[idx+1:]
	}
	return name
}

// AlertTransitionObservation is one state change an incident implies.
type AlertTransitionObservation struct {
	// At is the instant Google reports.
	At time.Time
	// From and To are from the published vocabulary.
	From string
	To   string
}

// Transitions renders an incident as the transitions it implies, in chronological order.
//
// An open incident is one transition; a closed one is two. **Both halves of a closed incident are
// emitted**, and the recovery is not an optimisation to skip: `alert → ok` bounds the outage, and an
// investigation that cannot see it cannot tell whether the change it is looking at was the fix.
//
// The `from` state of the opening transition is `ok` rather than empty. That is a claim, and it is
// the right one: Cloud Monitoring opens an incident when a condition starts being met, which means
// it was not being met immediately before. A blank `from` would leave the projector's
// previous-state property empty and cost a reader the one thing a transition is for.
func (i AlertIncident) Transitions() []AlertTransitionObservation {
	out := []AlertTransitionObservation{{
		At:   i.OpenTime,
		From: eventlog.AlertStateOK,
		To:   eventlog.AlertStateAlert,
	}}
	if i.State == IncidentClosed && !i.CloseTime.IsZero() {
		out = append(out, AlertTransitionObservation{
			At:   i.CloseTime,
			From: eventlog.AlertStateAlert,
			To:   eventlog.AlertStateOK,
		})
	}
	return out
}

// SuppressedTransition is a transition the published filter dropped, recorded so that "why did you
// not emit that?" is answerable (FR-052).
type SuppressedTransition struct {
	// Monitor is the alerting entity's ref value.
	Monitor string
	// Group is the alerting group, empty for an ungrouped policy.
	Group string
	// At is the transition instant.
	At time.Time
	// From and To are the states.
	From, To string
	// Reason is from the published set: no_transition | no_data | flapping.
	Reason string
}

// String renders one suppression for the checkpoint note, deterministically.
func (s SuppressedTransition) String() string {
	group := s.Group
	if group == "" {
		group = "-"
	}
	return fmt.Sprintf("%s[%s] %s→%s at %s: %s",
		s.Monitor, group, s.From, s.To, s.At.UTC().Format(time.RFC3339), s.Reason)
}

// ---- the emit path -----------------------------------------------------------------------------

// applyAlertPolicies maps one `projects.alertPolicies.list` response (T113).
func (f *Feeder) applyAlertPolicies(ctx context.Context, desc feeder.Description, em feeder.Emitter, raw []byte, at time.Time) error {
	var payload struct {
		AlertPolicies []json.RawMessage `json:"alertPolicies"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return fmt.Errorf("gcp: decoding a %s payload: %w", PayloadAlertPolicies, err)
	}
	for _, item := range payload.AlertPolicies {
		var policy monitoringpb.AlertPolicy
		if err := protojson.Unmarshal(item, &policy); err != nil {
			return fmt.Errorf("gcp: decoding an alert policy: %w", err)
		}
		if err := f.emitAlertPolicy(ctx, desc, em, &policy, at); err != nil {
			return err
		}
	}
	return nil
}

// emitAlertPolicy emits everything one policy read asserts: the ALERT node, its claims, and its
// condition pointers. It emits **no state**, because a policy read observed no transition.
func (f *Feeder) emitAlertPolicy(ctx context.Context, desc feeder.Description, em feeder.Emitter, policy *monitoringpb.AlertPolicy, at time.Time) error {
	obs, err := ObserveAlertPolicy(policy, f.opts.Labels)
	if err != nil {
		return err
	}

	props, err := obs.Props(f.opts.IncidentsAPIEnabled).Build()
	if err != nil {
		return err
	}
	pointers, err := obs.Pointers()
	if err != nil {
		return err
	}
	node := obs.NodeFact()
	node.Props = props
	node.Pointers = pointers
	node.SourceObservedAt = at
	id, node, err := f.stateAssertion(desc.SourceID, "alert_policy", obs.Policy.Value(), node, at, obs.MutateTime)
	if err != nil {
		return err
	}
	if err := emit(ctx, em, feeder.UpsertNode(desc, id, node)); err != nil {
		return err
	}
	if err := f.emitClaims(ctx, desc, em, obs.Policy.Ref(), obs.Claims(), obs.Labels.Environment, at); err != nil {
		return err
	}

	// The policy is remembered so that an incident can be keyed on ITS grouping. An incident
	// carries a policy snapshot but not the aggregation, and a group key derived from the wrong
	// field set would split one alert's history into several (FR-049).
	f.mu.Lock()
	f.policies[obs.Policy.Value()] = obs
	f.mu.Unlock()
	return nil
}

// applyAlerts maps one `projects.alerts.list` response (T115, T118, T119, T120, T121).
func (f *Feeder) applyAlerts(ctx context.Context, desc feeder.Description, em feeder.Emitter, raw []byte, at time.Time) error {
	if !f.opts.IncidentsAPIEnabled {
		// A payload that could only have been produced with the capability on, replayed with it
		// off, is a configuration mismatch and not a gap. Emitting the transitions anyway would
		// make the flag decorative; dropping them silently would make a fixture test less than it
		// claims.
		return fmt.Errorf(
			"gcp: an %s payload arrived with the incident capability disabled. `projects.alerts` is "+
				"Public Preview and bound only in a maintenance-mode client, so reading it is opt-in "+
				"(config/gcp.yaml alerts.incidents_api.enabled); with it off this feeder emits ALERT "+
				"nodes from the GA policy read and no transitions, and the backend answers "+
				"monitor_state NO_DATA naming the absent source", PayloadAlerts)
	}
	var payload alertsPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return fmt.Errorf("gcp: decoding a %s payload: %w", PayloadAlerts, err)
	}
	for _, item := range payload.Alerts {
		incident, err := item.decode()
		if err != nil {
			return err
		}
		if err := f.emitIncident(ctx, desc, em, incident, at); err != nil {
			return err
		}
	}
	return nil
}

// emitIncident emits the transitions one incident implies, per group, with the published filter
// applied and every suppression stated.
func (f *Feeder) emitIncident(ctx context.Context, desc feeder.Description, em feeder.Emitter, incident AlertIncident, at time.Time) error {
	f.mu.Lock()
	policy, known := f.policies[incident.Policy.Value()]
	f.mu.Unlock()

	groupBy := []string(nil)
	if known {
		groupBy = policy.GroupByFields()
	}
	group := incident.GroupKey(groupBy)
	entity := incident.Policy.Group(group)

	// A grouped policy's alerting entity is the group, and its state history has to name its
	// policy (FR-049). It does, **structurally**: the group's ref is the policy's value with
	// `#<group>` appended, and the projector writes `sre.alert.group_key` onto the version the
	// transition asserts. So the policy and the group are both readable off the alert itself.
	//
	// An earlier version of this code also emitted a separate UpsertNode for the group, carrying
	// the policy ref as a property. It was wrong, and the grouped fixture's shuffle step caught it:
	// two assertions of the SAME node over the SAME valid interval do not commute — whichever
	// arrived last decided which property set the version carried, so "what is this alert's state"
	// depended on the order two payloads of one poll reached the graph. The information the extra
	// node carried was already in the ref and in the transition, so it bought nothing and cost the
	// one property a bitemporal graph cannot give up.

	// The incident is remembered per alerting entity so that an intake can ask for its handoff by
	// alert reference (HandoffFor). Only the latest is kept: a handoff is about the alert's current
	// state, and a history of incidents is what the graph is for.
	f.mu.Lock()
	if previous, seen := f.incidents[entity.Value()]; !seen || !previous.OpenTime.After(incident.OpenTime) {
		f.incidents[entity.Value()] = incident
	}
	f.mu.Unlock()

	for _, transition := range incident.Transitions() {
		fact := feeder.AlertFact{
			Monitor:      entity.Ref(),
			GroupKey:     group,
			TransitionAt: transition.At,
			FromState:    transition.From,
			ToState:      transition.To,
			Watches:      incident.Watches(),
			Transport:    feeder.TransportPoll,
			OriginRef:    incident.ConsoleLink(),
			Severity:     incident.Severity,
			Title:        incident.PolicyDisplayName,
			// FR-051: a polled history is a sequence of observations at an interval, not a
			// complete record. Any transition that opened and closed between two polls is
			// invisible, and without this marker a gap in it reads as "the alert did not fire"
			// when it means "we were not looking at the moment it did".
			Sampled:         true,
			SampledInterval: f.alertPollInterval(),
		}
		fact.SourceObservedAt = at

		if reason := f.suppress(entity, &fact); reason != "" {
			f.recordSuppression(SuppressedTransition{
				Monitor: entity.Value(), Group: group, At: transition.At,
				From: transition.From, To: transition.To, Reason: reason,
			})
			continue
		}
		body := &graphv1.AlertTransition{
			Monitor:      fact.Monitor,
			GroupKey:     fact.GroupKey,
			TransitionAt: timestamppb.New(transition.At.UTC()),
			FromState:    fact.FromState,
			ToState:      fact.ToState,
		}
		// The event id is the published 4-tuple, derived rather than invented: it is what makes
		// the poll, the doorbell-triggered poll that follows it and a human declaration about the
		// same alert one event (FR-046, FR-050).
		eventID := eventlog.AlertTransitionKey(desc.SourceID, body)
		if err := emit(ctx, em, feeder.ObserveAlertTransition(desc, eventID, fact)); err != nil {
			return err
		}
	}
	return nil
}

// suppress applies the published filter and advances the series. It is a method because the series
// state is per (source, alerting entity) and lives on the feeder: losing it costs at most one
// redundant event, and a redundant event is a DUPLICATE_NOOP.
func (f *Feeder) suppress(entity AlertGroup, fact *feeder.AlertFact) string {
	body := &graphv1.AlertTransition{
		FromState:    fact.FromState,
		ToState:      fact.ToState,
		TransitionAt: timestamppb.New(fact.TransitionAt.UTC()),
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	prev := f.alerts[entity.Value()]
	if reason := eventlog.SuppressAlertTransition(prev, body, 0); reason != "" {
		return reason
	}
	next := &eventlog.AlertSeries{State: fact.ToState, At: fact.TransitionAt.UTC()}
	if prev != nil {
		next.PreviousState = prev.State
	} else {
		next.PreviousState = fact.FromState
	}
	f.alerts[entity.Value()] = next
	return ""
}

func (f *Feeder) recordSuppression(s SuppressedTransition) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.suppressed = append(f.suppressed, s)
}

// Suppressed returns the transitions this run's filter dropped, sorted, for the checkpoint.
func (f *Feeder) Suppressed() []SuppressedTransition {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]SuppressedTransition(nil), f.suppressed...)
	sort.Slice(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.Before(out[j].At)
		}
		return out[i].Monitor < out[j].Monitor
	})
	return out
}

// alertPollInterval is the cadence in force, clamped to the published ceiling.
func (f *Feeder) alertPollInterval() time.Duration {
	interval := f.opts.AlertPollInterval
	if interval <= 0 {
		return DefaultAlertPollInterval
	}
	if interval > MaxAlertPollInterval {
		return MaxAlertPollInterval
	}
	return interval
}

// ---- the handoff (T124, FR-053) ----------------------------------------------------------------

// AlertHandoff is what an investigation is started from: the alert, what it watches, the instant to
// reason from, and the pointers to execute.
//
// It carries **no telemetry payload** and there is no field it could live in — which is the point.
// FR-053 asks for a handoff, and the failure mode it exists to prevent is a connector helpfully
// attaching the metric values that made the alert fire. Those are a digest's business, behind the
// algebra, where they are bounded and sanitised; here they would be an unbounded, unsanitised copy
// of production arriving through the intake.
type AlertHandoff struct {
	// Monitor is the alerting entity — the policy, or the group within it that fired.
	Monitor *graphv1.Ref
	// Policy is the policy's own ref, which is where the definition lives.
	Policy *graphv1.Ref
	// GroupKey is the group that fired, empty for an ungrouped policy.
	GroupKey string
	// Watches are the entities the alert watches, which is where an investigation starts walking.
	Watches []*graphv1.Ref
	// ReferenceAt is the transition instant to use as the investigation's reference — Google's
	// instant, never the instant the feeder learned of it.
	ReferenceAt time.Time
	// State is the state the transition entered.
	State string
	// Severity and Title are as the policy stated them.
	Severity string
	Title    string
	// Pointers are the condition filters to execute, in the registered vocabulary.
	Pointers []*graphv1.Pointer
	// OriginRef is the incident's console link.
	OriginRef string
	// Sampled and SampledInterval say whether the history around this instant is complete.
	Sampled         bool
	SampledInterval time.Duration
}

// HandoffFor returns the handoff for an alert by its reference value — a policy resource name, or
// one with `#<group>` appended for a grouped policy. It is how an intake asks "what should an
// investigation of this alert start from" without holding the incident itself.
//
// It reports false for an alert this run saw no incident for, which is a different answer from a
// handoff with nothing in it: "this alert has not fired in what I read" and "this alert fired and I
// can tell you nothing about it" are different sentences, and only the second is a defect.
func (f *Feeder) HandoffFor(monitor string) (AlertHandoff, bool) {
	f.mu.Lock()
	incident, ok := f.incidents[monitor]
	f.mu.Unlock()
	if !ok {
		return AlertHandoff{}, false
	}
	return f.Handoff(incident), true
}

// Handoff builds the handoff for one incident (FR-053).
//
// A policy this run has not read yields a handoff with no pointers rather than an error: the
// transition is still the most useful thing the intake has, and refusing it because the policy list
// arrived in a later payload would lose an incident to an ordering this feeder declares it does not
// guarantee.
func (f *Feeder) Handoff(incident AlertIncident) AlertHandoff {
	f.mu.Lock()
	policy, known := f.policies[incident.Policy.Value()]
	f.mu.Unlock()

	groupBy := []string(nil)
	if known {
		groupBy = policy.GroupByFields()
	}
	group := incident.GroupKey(groupBy)
	out := AlertHandoff{
		Monitor:         incident.Policy.Group(group).Ref(),
		Policy:          incident.Policy.Ref(),
		GroupKey:        group,
		Watches:         incident.Watches(),
		ReferenceAt:     incident.OpenTime,
		State:           eventlog.AlertStateAlert,
		Severity:        incident.Severity,
		Title:           incident.PolicyDisplayName,
		OriginRef:       incident.ConsoleLink(),
		Sampled:         true,
		SampledInterval: f.alertPollInterval(),
	}
	// A closed incident's reference instant is still the OPENING. An investigation asks "what
	// changed before this started", and dating it at the recovery would put the change that fixed
	// the outage in the window and the change that caused it outside.
	if known {
		if pointers, err := policy.Pointers(); err == nil {
			out.Pointers = pointers
		}
	}
	if len(out.Watches) == 0 && known {
		// The incident's own labels named nothing complete, so fall back to what the policy
		// watches. It is wider than the group, and saying so is the caller's business: an
		// investigation with a wider neighbourhood is worse than one with a narrower one, and
		// both are better than one with no entities at all.
		out.Watches = policy.Watches
	}
	return out
}

// WatchesFromFilterForTest exposes the watch derivation to this package's tests. The rule it
// implements — a filter pinned completely names an entity, a filter pinned partially names nothing
// — is asserted directly rather than only through a whole poll, because the partial case emits
// nothing and a test over the emitted events cannot tell "nothing because the rule held" from
// "nothing because the payload was wrong".
func WatchesFromFilterForTest(filter string) []*graphv1.Ref { return watchesFromFilter(filter) }
