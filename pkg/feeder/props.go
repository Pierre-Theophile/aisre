// SPDX-License-Identifier: Apache-2.0

package feeder

import (
	"maps"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	eventlog "github.com/Pierre-Theophile/aisre/internal/log"
)

// Property names and the builder that refuses telemetry (FR-007, FR-009, constitution IV).
//
// Two decisions are encoded here.
//
// First, property *names* follow OpenTelemetry semantic conventions wherever one exists, and
// the project namespace `sre.` everywhere else. That is not tidiness: the resolution rules in
// internal/resolution match on `k8s.deployment.name` and `service.namespace` by name, and a
// connector that invents `kubernetes_deployment` instead has written a fact no rule can read.
//
// Second, the builder has typed setters and no `Set(key string, value any)`. There is no way
// to attach a slice of numbers, an arbitrary nested document or a struct that happens to hold
// a metric — the shapes telemetry arrives in — without going out of your way. What still gets
// through is caught by Build, which runs the graph's own validator (internal/log/validate.go)
// over the finished struct and returns the same reason code and the same wording the server
// would have returned. There is exactly one denylist in this repository and this is not a copy
// of it.

// OpenTelemetry semantic convention attribute keys. These are property and pointer-attribute
// names, never Ref namespaces (see ref.go for those).
const (
	// AttrServiceName is the OpenTelemetry `service.name`.
	AttrServiceName = "service.name"
	// AttrServiceNamespace is the OpenTelemetry `service.namespace`, the grouping two facts
	// must share before a certain rule may merge them.
	AttrServiceNamespace = "service.namespace"
	// AttrServiceVersion is the OpenTelemetry `service.version`; a transition in it is what
	// the telemetry feeder reports as a rollout.
	AttrServiceVersion = "service.version"
	// AttrDeploymentEnvironment is the OpenTelemetry `deployment.environment.name`.
	// `checkout` in staging is not `checkout` in production, so almost every rule reads it.
	AttrDeploymentEnvironment = "deployment.environment.name"

	// AttrK8sClusterName is the OpenTelemetry `k8s.cluster.name`.
	AttrK8sClusterName = "k8s.cluster.name"
	// AttrK8sNamespaceName is the OpenTelemetry `k8s.namespace.name`.
	AttrK8sNamespaceName = "k8s.namespace.name"
	// AttrK8sDeploymentName is the OpenTelemetry `k8s.deployment.name`.
	AttrK8sDeploymentName = "k8s.deployment.name"
	// AttrK8sNodeName is the OpenTelemetry `k8s.node.name`.
	AttrK8sNodeName = "k8s.node.name"
	// AttrK8sPodName is the OpenTelemetry `k8s.pod.name`. Pods are not watched as entities
	// (research §12), but a log or metric answer is grouped by them, so the attribute is
	// named here for pointers' join keys (ADR-0005 D6).
	AttrK8sPodName = "k8s.pod.name"
	// AttrK8sServiceName is the Kubernetes Service name, `k8s.service.name`.
	AttrK8sServiceName = "k8s.service.name"
	// AttrK8sIngressName is the Kubernetes Ingress name, `k8s.ingress.name`.
	AttrK8sIngressName = "k8s.ingress.name"
	// AttrK8sConfigMapName is the Kubernetes ConfigMap name, `k8s.configmap.name`.
	AttrK8sConfigMapName = "k8s.configmap.name"
	// AttrK8sSecretName is the Kubernetes Secret name, `k8s.secret.name`.
	AttrK8sSecretName = "k8s.secret.name"

	// AttrDBSystem is the OpenTelemetry `db.system`, the pre-1.30 spelling still emitted by
	// many SDKs. Record whichever one the telemetry actually carries.
	AttrDBSystem = "db.system"
	// AttrDBSystemName is the OpenTelemetry `db.system.name`, the 1.30 spelling.
	AttrDBSystemName = "db.system.name"
	// AttrServerAddress is the OpenTelemetry `server.address`.
	AttrServerAddress = "server.address"
	// AttrServerPort is the OpenTelemetry `server.port`.
	AttrServerPort = "server.port"
	// AttrURLScheme is the OpenTelemetry `url.scheme`.
	AttrURLScheme = "url.scheme"
	// AttrCloudRegion is the OpenTelemetry `cloud.region`.
	AttrCloudRegion = "cloud.region"
	// The four attributes the OpenTelemetry **GCP resource detector** sets on a process running in
	// Cloud Run. They are named here rather than in a connector because two connectors read them: the
	// OTel aggregator carries them onto the claim it emits, and `internal/resolution`'s C4 reads that
	// claim to pair an observed service with the revision it runs in (003 FR-117, T182).
	//
	// Without them C4 is published and dead — it evaluates on every claim, finds nothing to pair with,
	// and fails silently. That is the shape of failure this whole cluster of constants exists to
	// prevent: a rule whose supporting attribute nobody emits never fires and nothing reports it.
	//
	// AttrCloudPlatform is `cloud.platform`, valued `gcp_cloud_run` on Cloud Run. It is the **gate**:
	// `faas.version` means a revision only on Cloud Run, and on AWS Lambda it means something else
	// entirely, so a claim that carried the version without the platform would let C4 pair a Lambda
	// alias with a Cloud Run revision of the same name.
	AttrCloudPlatform = "cloud.platform"
	// AttrFaaSVersion is `faas.version`, which on Cloud Run is the **revision name**. That is the
	// mapping research §2 established and it is not obvious from the attribute's name, which is why
	// the translation is one documented place rather than an inline lookup.
	AttrFaaSVersion = "faas.version"
	// AttrFaaSName is `faas.name`, which on Cloud Run is the service name.
	AttrFaaSName = "faas.name"
	// AttrCloudAccountID is `cloud.account.id`, which on GCP is the **project id**. A revision name is
	// unique within a service and not globally, so without the project C4 would merge across projects
	// — the failure FR-010 exists to prevent.
	AttrCloudAccountID = "cloud.account.id"
)

// Project property names. Everything the OpenTelemetry conventions do not name lives under
// `sre.` (FR-007) so that a reader can tell at a glance which half of a node's properties are
// standard vocabulary and which half this project invented.
const (
	// PropK8sRevision is a workload's deployment revision, from
	// `deployment.kubernetes.io/revision`. A change in it is a rollout.
	PropK8sRevision = "sre.k8s.revision"
	// PropK8sResourceVersion is the Kubernetes resourceVersion the fact was read at. It is
	// the per-source sequence number of the Kubernetes feeder.
	PropK8sResourceVersion = "sre.k8s.resource_version"
	// PropK8sReplicas is a workload's desired replica count. A change in it is a scaling
	// change.
	PropK8sReplicas = "sre.k8s.replicas"
	// PropK8sNodePool is the node pool a workload's nodes belong to.
	PropK8sNodePool = "sre.k8s.node_pool"
	// PropK8sNodeCount is how many nodes a pool holds.
	PropK8sNodeCount = "sre.k8s.node_count"
	// PropK8sReference is how a workload references a config object: "env", "envFrom",
	// "volume".
	PropK8sReference = "sre.k8s.reference"
	// PropK8sClaimKey is the label or annotation key an identity claim was read from, so a
	// rule can tell an operator's declaration from a coincidence (research §10, C2).
	PropK8sClaimKey = "sre.k8s.claim_key"
	// PropK8sClaimKind is whether PropK8sClaimKey was a "label" or an "annotation".
	PropK8sClaimKind = "sre.k8s.claim_kind"

	// PropConfigKind is what a config node holds: "configmap", "secret", "flag". The value
	// "secret" is what makes the graph refuse any property that would carry the material.
	PropConfigKind = "sre.config.kind"
	// PropConfigValueHash is a hash of a ConfigMap's contents — enough to see that it
	// changed, never enough to read it.
	PropConfigValueHash = "sre.config.value_hash"
	// PropConfigVersion is a config object's version identifier. For a Secret it is the only
	// thing the graph is allowed to know about its contents.
	PropConfigVersion = "sre.config.version"

	// PropOwnerKind is what an owner node represents: "team", "individual", "rota".
	PropOwnerKind = "sre.owner.kind"
	// PropOwnerTeam is the team identifier an owner node names.
	PropOwnerTeam = "sre.owner.team"
	// PropOwnerSlackChannel is the chat channel to reach the owner on.
	PropOwnerSlackChannel = "sre.owner.slack_channel"
	// PropOwnerSourceLabel is the label or annotation key the ownership was read from.
	PropOwnerSourceLabel = "sre.owner.source_label"

	// PropWindowSeconds is the aggregation window an observation was derived over, in
	// seconds. It is what makes a weight class interpretable (research §8).
	PropWindowSeconds = "sre.window.seconds"
	// PropThirdParty marks a node as an external dependency rather than something the
	// organisation runs.
	PropThirdParty = "sre.third_party"
	// PropPlaceholder marks a node created only because something else referenced it, before
	// any source has described it. It is how a feeder says "I know this exists and nothing
	// more", which a later, fuller assertion overwrites.
	PropPlaceholder = "sre.placeholder"
)

// Props builds a property struct that the graph will accept.
//
// The zero value is not usable; call NewProps. Setters are chainable and record the first
// error rather than panicking, so a long chain reports its problem once, at Build:
//
//	props, err := feeder.NewProps().
//	    Str(feeder.AttrK8sNamespaceName, "shop").
//	    Str(feeder.AttrK8sDeploymentName, "checkout").
//	    Int(feeder.PropK8sReplicas, 3).
//	    Build()
type Props struct {
	fields map[string]*structpb.Value
	err    error
}

// NewProps returns an empty property builder.
func NewProps() *Props {
	return &Props{fields: map[string]*structpb.Value{}}
}

// PropsFrom returns a builder seeded with a map of string properties, which is the shape most
// source systems hand a connector (labels, annotations, resource attributes).
func PropsFrom(values map[string]string) *Props {
	p := NewProps()
	for key, value := range values {
		p.Str(key, value)
	}
	return p
}

// Str sets a string property.
func (p *Props) Str(key, value string) *Props {
	return p.set(key, structpb.NewStringValue(value))
}

// Strs sets a list-of-strings property, for the genuinely plural facts — the config objects a
// workload references, the ports a service exposes by name. A list of *numbers* is a sample
// series whatever it is called and is refused by Build, which is why there is no Ints.
func (p *Props) Strs(key string, values ...string) *Props {
	items := make([]*structpb.Value, 0, len(values))
	for _, v := range values {
		items = append(items, structpb.NewStringValue(v))
	}
	return p.set(key, structpb.NewListValue(&structpb.ListValue{Values: items}))
}

// Int sets an integer property: a replica count, a port, a node count, a window in seconds.
func (p *Props) Int(key string, value int64) *Props {
	return p.set(key, structpb.NewNumberValue(float64(value)))
}

// Bool sets a boolean property.
func (p *Props) Bool(key string, value bool) *Props {
	return p.set(key, structpb.NewBoolValue(value))
}

// Float sets a floating-point property. Reach for it rarely: a float on a node is usually a
// measurement, and a measurement belongs in the telemetry backend behind a Pointer
// (constitution IV).
func (p *Props) Float(key string, value float64) *Props {
	return p.set(key, structpb.NewNumberValue(value))
}

// Time sets a timestamp property, rendered RFC 3339 in UTC so that it canonicalizes the same
// way everywhere else in this system does.
func (p *Props) Time(key string, value time.Time) *Props {
	return p.set(key, structpb.NewStringValue(value.UTC().Format(time.RFC3339Nano)))
}

// Len is how many properties have been set.
func (p *Props) Len() int {
	if p == nil {
		return 0
	}
	return len(p.fields)
}

// Err returns the first error recorded by a setter, or nil.
func (p *Props) Err() error {
	if p == nil {
		return nil
	}
	return p.err
}

// Build returns the finished struct, or a *Rejection naming the property the graph would have
// refused and why.
//
// An empty builder returns (nil, nil): a node with no properties is ordinary, and an empty
// struct would serialize differently from an absent one and change every golden.
func (p *Props) Build() (*structpb.Struct, error) {
	if p == nil || len(p.fields) == 0 {
		if p != nil && p.err != nil {
			return nil, p.err
		}
		return nil, nil
	}
	if p.err != nil {
		return nil, p.err
	}
	out := &structpb.Struct{Fields: maps.Clone(p.fields)}
	if r := ValidateProps(out); r != nil {
		return nil, r
	}
	return out, nil
}

// set records one field, refusing an empty key outright.
func (p *Props) set(key string, value *structpb.Value) *Props {
	if p.err != nil {
		return p
	}
	if strings.TrimSpace(key) == "" {
		p.err = &Rejection{ReasonCode: ReasonTelemetryPayload, ReasonDetail: "props: a property name may not be empty"}
		return p
	}
	p.fields[key] = value
	return p
}

// ValidateProps runs the graph's property rules over a finished struct, without a database and
// without sending anything.
//
// It works by validating a probe envelope carrying the struct, so that the denylist, the size
// limit, the numeric-series rule and the secret-value rule are the ones internal/log actually
// enforces rather than a second implementation of them that could drift. The probe is a CONFIG
// node because that is the one node type with an extra rule — a secret may not carry its own
// value — and a feeder building properties for a secret should be told so here.
func ValidateProps(props *structpb.Struct) *Rejection {
	probe := &graphv1.EventEnvelope{
		EventId:       "feeder.props.probe",
		SourceId:      "feeder.props.probe",
		SchemaVersion: SchemaVersion,
		Body: &graphv1.EventEnvelope_UpsertNode{UpsertNode: &graphv1.UpsertNode{
			Ref:   Ref("feeder.props", "probe"),
			Type:  graphv1.NodeType_CONFIG,
			Props: props,
			// The valid time is irrelevant — nothing reads it — but it must be set, or the
			// probe would be refused for missing valid time before the properties were ever
			// examined. It is built per call rather than shared, so that concurrent feeders
			// never hand the same message to two validators.
			ValidAt: timestamppb.New(time.Unix(0, 0).UTC()),
		}},
	}
	// The probe declares no accepted-version set, so the schema-version check is skipped and
	// the only refusals that can come back are about the properties themselves.
	r := convertRejection(eventlog.Validate(probe, nil))
	if r == nil {
		return nil
	}
	r.ReasonDetail = strings.Replace(r.ReasonDetail, "upsert_node.props", "props", 1)
	return r
}
