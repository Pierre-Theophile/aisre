// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// Cloud Run topology (T051, T052, FR-015, contracts/gcp-feeder.md §3.1).
//
// A SERVICE node per Cloud Run service, a WORKLOAD node per revision, and one property that is the
// whole point of this file.
//
// # trafficStatuses, never traffic
//
// Cloud Run reports the split twice, and the distinction is the difference between what production
// serves and what somebody asked for:
//
//   - `traffic[]` is the **desired** split — an intention, settable, and possibly not converged;
//   - `trafficStatuses[]` is the **observed** split — output-only, what is actually being served.
//
// The graph records what production is actually serving, so `trafficStatuses` is the source and
// `traffic` is not read at all. Reading `traffic` would make the graph say the new revision is
// serving 100% from the instant somebody pressed deploy, which is the exact window an investigation
// is asking about: during a slow or stuck rollout the two disagree, and the disagreement *is* the
// incident.
//
// # The split is a property versioned in valid time, and it is not a measurement
//
// FR-015. The split is structure: a set of revision names with percentages, versioned in valid
// time, so a rollback gives the property three versions rather than overwriting the second. It is
// **never** derived from observed request volume — that is a measurement, it belongs in a telemetry
// backend, and a graph property holding one is a number nobody will refresh (constitution IV).
//
// The distinction is easy to blur because both are "traffic". `trafficStatuses[].percent` is the
// routing *configuration* Cloud Run reports as in effect; requests per second is a measurement. The
// first is here; the second is what a `compare` digest answers.

// Property names this feeder mints. OpenTelemetry conventions where one exists, `sre.` otherwise.
const (
	// PropProject is the GCP project. It is in the ref value as well (FR-010); this is for a
	// reader, and for a query that filters by project without parsing identifiers.
	PropProject = "sre.gcp.project"
	// PropRegion is the Cloud Run region. `cloud.region` is the OTel spelling and is emitted too.
	PropRegion = "sre.gcp.region"
	// PropTrafficSplit is the observed traffic split: `<revision>=<percent>` entries, sorted by
	// revision so two reads of one split are one value. Versioned in valid time (FR-015).
	PropTrafficSplit = "sre.gcp.traffic_split"
	// PropTrafficSplitSource records which Cloud Run field the split came from. It is recorded
	// because the answer must always be `trafficStatuses`, and a golden that ever says otherwise
	// is the regression this file exists to prevent.
	PropTrafficSplitSource = "sre.gcp.traffic_split_source"
	// PropServing lists the revisions holding a non-zero share, sorted. It is derived from the
	// split rather than stored beside it, so the two cannot disagree.
	PropServing = "sre.gcp.serving_revisions"
	// PropLatestReadyRevision and PropLatestCreatedRevision are how the "created but not yet
	// serving" window is recognised cheaply between polls (contract §3.3).
	PropLatestReadyRevision   = "sre.gcp.latest_ready_revision"
	PropLatestCreatedRevision = "sre.gcp.latest_created_revision"
	// PropGeneration is the service's desired-state generation, and PropObservedGeneration what
	// Cloud Run has converged to. They differ exactly while a change is in flight.
	PropGeneration         = "sre.gcp.generation"
	PropObservedGeneration = "sre.gcp.observed_generation"
	// PropReconciling is Cloud Run's own convergence flag.
	PropReconciling = "sre.gcp.reconciling"
	// PropUpdateTime is `Service.updateTime`, recorded as **metadata only**. It moves for any
	// change to the resource — an image bump, an environment variable, a label — so it is never a
	// change instant (contract §3.2 consequence 2). It is recorded so that a reader can see the
	// resource moved, and named so that nobody mistakes it for a rollout time.
	PropUpdateTime = "sre.gcp.update_time_metadata_only"
	// PropIngress is the service's ingress setting: structure, and a plausible cause.
	PropIngress = "sre.gcp.ingress"
	// PropImage is the container image reference a revision runs.
	PropImage = "sre.gcp.image"
	// PropImageDigest is the image digest, which is the identifier a build pipeline also knows.
	PropImageDigest = "sre.gcp.image_digest"
	// PropServiceAccount is the identity a revision runs as — structure, and it determines what
	// the revision can reach, which makes it a cause of a permission failure.
	PropServiceAccount = "sre.gcp.service_account"
	// PropExecutionEnvironment is gen1 or gen2.
	PropExecutionEnvironment = "sre.gcp.execution_environment"
	// PropEnvironment is the derived environment (labels.go).
	PropEnvironment = "sre.gcp.environment"
	// PropEnvironmentSource names which of the three rules derived it.
	PropEnvironmentSource = "sre.gcp.environment_source"
	// PropRevisionOf names the service a revision belongs to, as this feeder addresses it.
	PropRevisionOf = "sre.gcp.revision_of"
)

// TrafficSplitSourceObserved is the only value PropTrafficSplitSource ever takes.
const TrafficSplitSourceObserved = "trafficStatuses (observed)"

// TrafficShare is one revision's share of the observed split.
type TrafficShare struct {
	Revision string
	Percent  int32
	// Tag is the traffic tag, where the split carries one. A tagged target addresses a revision
	// by a stable URL, so it is structure a reader wants.
	Tag string
}

// TrafficSplit is the observed split: the shares, canonically ordered.
type TrafficSplit struct {
	Shares []TrafficShare
}

// ObservedTrafficSplit reads the split from `trafficStatuses` (T051).
//
// It takes the statuses rather than the whole Service so that no call site can accidentally pass
// `traffic` — the type system will not stop `[]*TrafficTarget` from being converted by hand, but it
// will stop it being passed here, and the one narrow signature is easier to review than a comment
// asking the reader to check which field was used.
//
// Revision names are normalised to the bare name: Cloud Run spells the same revision qualified in
// `Revision.name` and bare in `trafficStatuses[].revision`, and treating those as two revisions
// would double the graph.
func ObservedTrafficSplit(statuses []*runpb.TrafficTargetStatus) TrafficSplit {
	split := TrafficSplit{}
	for _, status := range statuses {
		name := RevisionName(status.GetRevision())
		if name == "" {
			// A target with no revision is a `LATEST` allocation Cloud Run has not resolved yet.
			// It is skipped rather than recorded as a revision called "", because "" would become
			// a node.
			continue
		}
		split.Shares = append(split.Shares, TrafficShare{
			Revision: name,
			Percent:  status.GetPercent(),
			Tag:      status.GetTag(),
		})
	}
	split.canonicalise()
	return split
}

// canonicalise sorts and merges the shares, so that two reads of one split produce one value and
// the property does not gain a version because Cloud Run reordered a list.
//
// Merging matters: a revision can appear twice in `trafficStatuses`, once as the untagged target and
// once per tag. Summing the percentages of one revision is what makes the total 100.
func (s *TrafficSplit) canonicalise() {
	if len(s.Shares) == 0 {
		return
	}
	merged := make(map[string]TrafficShare, len(s.Shares))
	for _, share := range s.Shares {
		existing, seen := merged[share.Revision]
		if !seen {
			merged[share.Revision] = share
			continue
		}
		existing.Percent += share.Percent
		if existing.Tag == "" {
			existing.Tag = share.Tag
		} else if share.Tag != "" && share.Tag != existing.Tag {
			existing.Tag = existing.Tag + "," + share.Tag
		}
		merged[share.Revision] = existing
	}
	out := make([]TrafficShare, 0, len(merged))
	for _, share := range merged {
		out = append(out, share)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Revision < out[j].Revision })
	s.Shares = out
}

// Entries renders the split as the property's sorted `<revision>=<percent>` strings.
func (s TrafficSplit) Entries() []string {
	out := make([]string, 0, len(s.Shares))
	for _, share := range s.Shares {
		entry := share.Revision + "=" + strconv.FormatInt(int64(share.Percent), 10)
		if share.Tag != "" {
			entry += "#" + share.Tag
		}
		out = append(out, entry)
	}
	return out
}

// Serving returns the revisions holding a non-zero share, sorted. A revision at 0% is deliberately
// absent: it exists, it has a node, and it is not serving (FR-018).
func (s TrafficSplit) Serving() []string {
	var out []string
	for _, share := range s.Shares {
		if share.Percent > 0 {
			out = append(out, share.Revision)
		}
	}
	return out
}

// Percent returns one revision's share, and whether the split mentions it at all. The second return
// separates "mentioned at 0%" from "not in the split", which are different facts: the first is a
// revision Cloud Run is still routing to, and the second is a revision it has stopped tracking.
func (s TrafficSplit) Percent(revision string) (int32, bool) {
	for _, share := range s.Shares {
		if share.Revision == revision {
			return share.Percent, true
		}
	}
	return 0, false
}

// Total is the sum of the shares. It is not asserted to be 100: Cloud Run reports what it reports,
// and a split that does not total 100 during a rollout is a real observation rather than a parse
// error. The checkpoint records it when it is not 100 so that a reader can see it was observed.
func (s TrafficSplit) Total() int32 {
	var total int32
	for _, share := range s.Shares {
		total += share.Percent
	}
	return total
}

// Equal reports whether two splits are the same routing configuration. It is what decides whether a
// poll saw a *change*, so it compares the canonical form and nothing else — not the read time, not
// the generation.
func (s TrafficSplit) Equal(other TrafficSplit) bool {
	if len(s.Shares) != len(other.Shares) {
		return false
	}
	for i := range s.Shares {
		if s.Shares[i] != other.Shares[i] {
			return false
		}
	}
	return true
}

// String renders the split for a summary line.
func (s TrafficSplit) String() string { return strings.Join(s.Entries(), " ") }

// ServiceObservation is everything one Cloud Run service poll yields, decoded and typed. It is a
// value so that the mapping is a pure function of the payload, which is what lets the recorded and
// the live path be the same code (FR-044).
type ServiceObservation struct {
	Service Service
	// Split is the observed split, from trafficStatuses.
	Split TrafficSplit
	// OTelServiceName is the OpenTelemetry service name the service declares, where it declares
	// one. FR-118's certain resolution rule depends on it.
	OTelServiceName string
	// EnvVarNames is the names of the environment variables the service's template defines,
	// sorted and de-duplicated. The **names only**: a value is configuration and is dropped
	// (FR-034). P4 reads them to suggest a Cloud SQL dependency nothing else can see.
	EnvVarNames []string
	// Labels is the result of the label policy.
	Labels Labels
	// Generation, ObservedGeneration and Reconciling are the convergence predicate.
	Generation, ObservedGeneration int64
	Reconciling                    bool
	// LatestReady and LatestCreated are the two revisions whose difference is the
	// "created but not yet serving" window (contract §3.3).
	LatestReady, LatestCreated string
	// CreateTime is when the service was created, which is its valid-from when the feeder can
	// see it.
	CreateTime time.Time
	// UpdateTime is metadata only and is never a change instant (contract §3.2).
	UpdateTime time.Time
	// DeleteTime is set only on a delete response.
	DeleteTime time.Time
	// Ingress is the ingress setting.
	Ingress string
}

// instantOf converts a protobuf timestamp to a time.Time, mapping an **absent** one to the zero
// value rather than to the Unix epoch.
//
// This is not defensive tidying. `(*timestamppb.Timestamp)(nil).AsTime()` returns 1970-01-01, which
// is a perfectly valid instant — so a service Cloud Run reported without a `createTime` would assert
// a node valid from 1970 instead of asserting an unknown start, and every such service would claim
// to have existed since before the company did. Guessing a start is a defect (FR-011), and the epoch
// is the guess a nil timestamp makes on your behalf.
func instantOf(ts *timestamppb.Timestamp) time.Time {
	if ts == nil {
		return time.Time{}
	}
	return ts.AsTime()
}

// EnvVarOTelServiceName is the environment variable a Cloud Run service sets to declare its
// OpenTelemetry service name. It is read as a *declaration* — an operator wrote it — which is what
// makes FR-118's rule certain rather than probable.
const EnvVarOTelServiceName = "OTEL_SERVICE_NAME"

// ObserveService decodes one Cloud Run service.
func ObserveService(svc *runpb.Service, policy LabelPolicy) (ServiceObservation, error) {
	coords, ok := ParseServiceResourceName(svc.GetName())
	if !ok {
		return ServiceObservation{}, fmt.Errorf("gcp: %q is not a Cloud Run service resource name; "+
			"a service whose coordinates cannot be read is not emitted under a guessed ref, because a "+
			"guessed ref merges with something (FR-010)", svc.GetName())
	}
	obs := ServiceObservation{
		Service:            coords,
		Split:              ObservedTrafficSplit(svc.GetTrafficStatuses()),
		Labels:             policy.Apply(coords.Project, svc.GetLabels()),
		Generation:         svc.GetGeneration(),
		ObservedGeneration: svc.GetObservedGeneration(),
		Reconciling:        svc.GetReconciling(),
		LatestReady:        RevisionName(svc.GetLatestReadyRevision()),
		LatestCreated:      RevisionName(svc.GetLatestCreatedRevision()),
		CreateTime:         instantOf(svc.GetCreateTime()),
		UpdateTime:         instantOf(svc.GetUpdateTime()),
		DeleteTime:         instantOf(svc.GetDeleteTime()),
		Ingress:            svc.GetIngress().String(),
	}
	obs.OTelServiceName = declaredOTelServiceName(svc.GetTemplate())
	obs.EnvVarNames = declaredEnvVarNames(svc.GetTemplate())
	return obs, nil
}

// declaredEnvVarNames reads the NAMES of the environment variables the revision template defines,
// from every container, including the ones sourced from a secret.
//
// The names and never the values. A value is configuration and is dropped (FR-034); a value from a
// secret the feeder has no permission to read is not even reachable. The name alone is what P4's
// suggestion stands on — a service that mentions a database in `ORDERS_PRIMARY_DSN` usually talks to
// it — and it is the most a read-only connector can honestly record.
//
// A secret-sourced variable is included **because** its name is the interesting part: a service whose
// DSN comes from Secret Manager is exactly the service whose dependency nothing else can see.
func declaredEnvVarNames(template *runpb.RevisionTemplate) []string {
	var names []string
	for _, container := range template.GetContainers() {
		for _, env := range container.GetEnv() {
			if name := strings.TrimSpace(env.GetName()); name != "" {
				names = append(names, name)
			}
		}
	}
	return sortedUniqueNames(names)
}

// declaredOTelServiceName reads OTEL_SERVICE_NAME from the revision template's containers.
//
// Only a literal value counts. A value sourced from a secret is not read — the feeder has no
// permission to and would not record it if it had — and a name the feeder cannot see is a name it
// does not claim, which leaves FR-118's certain rule unsatisfied and the probable rules to it. That
// is the correct outcome: a certain rule fires on evidence, not on a gap.
func declaredOTelServiceName(template *runpb.RevisionTemplate) string {
	for _, container := range template.GetContainers() {
		for _, env := range container.GetEnv() {
			if env.GetName() != EnvVarOTelServiceName {
				continue
			}
			if value := strings.TrimSpace(env.GetValue()); value != "" {
				return value
			}
		}
	}
	return ""
}

// Props renders the service's properties.
//
// `PropUpdateTime` carries `_metadata_only` in its name rather than a comment beside it. A property
// name is what a reader sees in a golden and what a query author autocompletes; the one place a
// warning cannot be missed is the name itself, and `Service.updateTime` being mistaken for a
// rollout instant is the specific error contract §3.2 exists to prevent.
func (o ServiceObservation) Props() *feeder.Props {
	props := feeder.NewProps().
		Str(PropProject, o.Service.Project).
		Str(PropRegion, o.Service.Region).
		Str(feeder.AttrCloudRegion, o.Service.Region).
		Str(PropEnvironment, o.Labels.Environment).
		Str(PropEnvironmentSource, o.Labels.EnvironmentSource).
		Str(PropTrafficSplitSource, TrafficSplitSourceObserved).
		Int(PropGeneration, o.Generation).
		Int(PropObservedGeneration, o.ObservedGeneration).
		Bool(PropReconciling, o.Reconciling)

	if entries := o.Split.Entries(); len(entries) > 0 {
		props = props.Strs(PropTrafficSplit, entries...)
	}
	if serving := o.Split.Serving(); len(serving) > 0 {
		props = props.Strs(PropServing, serving...)
	}
	if o.LatestReady != "" {
		props = props.Str(PropLatestReadyRevision, o.LatestReady)
	}
	if o.LatestCreated != "" {
		props = props.Str(PropLatestCreatedRevision, o.LatestCreated)
	}
	if !o.UpdateTime.IsZero() {
		props = props.Time(PropUpdateTime, o.UpdateTime)
	}
	if o.Ingress != "" {
		props = props.Str(PropIngress, o.Ingress)
	}
	if o.OTelServiceName != "" {
		props = props.Str(feeder.AttrServiceName, o.OTelServiceName)
	}
	for key, value := range o.Labels.Props {
		props = props.Str("sre.gcp.label."+key, value)
	}
	return props
}

// RevisionObservation is one Cloud Run revision poll, decoded.
type RevisionObservation struct {
	Revision Revision
	// CreateTime is `Revision.createTime`: output-only and documented, so the creation change's
	// valid time comes from the API and `unknown` is not accepted here (contract §3.3).
	CreateTime time.Time
	// Labels is the label policy's result for the revision's own labels.
	Labels Labels
	// Image and ImageDigest are what the revision runs. The digest is the claim a pipeline shares.
	Image, ImageDigest string
	// ServiceAccount is the identity the revision runs as.
	ServiceAccount string
	// ExecutionEnvironment is gen1 or gen2.
	ExecutionEnvironment string
	// Generation is the revision's generation.
	Generation int64
	// Ready is whether the revision's `Ready` condition is true. Only `Ready` is read: the other
	// `Condition.type` values — `RoutesReady`, `ConfigurationsReady` — are not in the public
	// contract (research §2) and are not relied on, however often they appear in practice.
	Ready bool
	// ReadyObserved says whether a `Ready` condition was present at all, so that "not ready" and
	// "Cloud Run did not say" stay distinguishable.
	ReadyObserved bool
	// DeleteTime is set only on a delete response.
	DeleteTime time.Time
}

// ConditionReady is the only Cloud Run condition type in the public contract.
const ConditionReady = "Ready"

// ObserveRevision decodes one Cloud Run revision.
func ObserveRevision(rev *runpb.Revision, policy LabelPolicy) (RevisionObservation, error) {
	coords, ok := ParseRevisionResourceName(rev.GetName())
	if !ok {
		return RevisionObservation{}, fmt.Errorf("gcp: %q is not a Cloud Run revision resource name", rev.GetName())
	}
	obs := RevisionObservation{
		Revision:             coords,
		CreateTime:           instantOf(rev.GetCreateTime()),
		DeleteTime:           instantOf(rev.GetDeleteTime()),
		Labels:               policy.Apply(coords.Project, rev.GetLabels()),
		ServiceAccount:       rev.GetServiceAccount(),
		ExecutionEnvironment: rev.GetExecutionEnvironment().String(),
		Generation:           rev.GetGeneration(),
	}
	if containers := rev.GetContainers(); len(containers) > 0 {
		obs.Image = containers[0].GetImage()
		obs.ImageDigest = imageDigest(obs.Image)
	}
	for _, cond := range rev.GetConditions() {
		if cond.GetType() != ConditionReady {
			continue
		}
		obs.ReadyObserved = true
		obs.Ready = cond.GetState() == runpb.Condition_CONDITION_SUCCEEDED
	}
	return obs, nil
}

// imageDigest extracts the `sha256:…` digest from an image reference that pins one, or "".
//
// A tag is not a digest and is not returned as one: `checkout:v2` is mutable, two deploys can carry
// it, and claiming it as an identity would merge two revisions that ran different code.
func imageDigest(image string) string {
	if idx := strings.LastIndex(image, "@"); idx >= 0 {
		return image[idx+1:]
	}
	return ""
}

// Props renders the revision's properties.
func (o RevisionObservation) Props() *feeder.Props {
	props := feeder.NewProps().
		Str(PropProject, o.Revision.Project).
		Str(PropRegion, o.Revision.Region).
		Str(feeder.AttrCloudRegion, o.Revision.Region).
		Str(PropRevisionOf, o.Revision.Service.Value()).
		Str(PropEnvironment, o.Labels.Environment).
		Str(PropEnvironmentSource, o.Labels.EnvironmentSource).
		Int(PropGeneration, o.Generation)

	if o.Image != "" {
		props = props.Str(PropImage, o.Image)
	}
	if o.ImageDigest != "" {
		props = props.Str(PropImageDigest, o.ImageDigest)
	}
	if o.ServiceAccount != "" {
		props = props.Str(PropServiceAccount, o.ServiceAccount)
	}
	if o.ExecutionEnvironment != "" {
		props = props.Str(PropExecutionEnvironment, o.ExecutionEnvironment)
	}
	if o.ReadyObserved {
		props = props.Bool("sre.gcp.ready", o.Ready)
	}
	for key, value := range o.Labels.Props {
		props = props.Str("sre.gcp.label."+key, value)
	}
	return props
}

// NodeFact renders the service as a graph node assertion.
//
// `ValidAt` is the service's creation instant where Cloud Run reports one, and `ValidFromUnknown`
// otherwise. There is no third option: guessing a start is a defect (FR-011), and the poll time is
// the specific guess that would make every first poll claim the estate was created at boot.
func (o ServiceObservation) NodeFact() feeder.NodeFact {
	fact := feeder.NodeFact{
		Ref:         o.Service.Ref(),
		Type:        graphv1.NodeType_SERVICE,
		DisplayName: o.Service.Name,
	}
	if o.CreateTime.IsZero() {
		fact.ValidFromUnknown = true
	} else {
		fact.ValidAt = o.CreateTime
	}
	return fact
}

// NodeFact renders the revision as a graph node assertion. A revision always has a documented
// `createTime`, so `ValidFromUnknown` is not used here — and if one ever arrives without it, that is
// an error rather than an unknown start (contract §3.3).
func (o RevisionObservation) NodeFact() feeder.NodeFact {
	return feeder.NodeFact{
		Ref:         o.Revision.Ref(),
		Type:        graphv1.NodeType_WORKLOAD,
		DisplayName: o.Revision.Revision,
		ValidAt:     o.CreateTime,
	}
}
