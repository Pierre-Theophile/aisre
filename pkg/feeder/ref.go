// SPDX-License-Identifier: Apache-2.0

package feeder

import (
	"slices"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
)

// Identifier namespaces (FR-036, research §10, fixtures/README.md §"Ref namespaces").
//
// A feeder never knows a canonical entity id. It says "the thing my source calls
// `shop/checkout` in the namespace `k8s.deployment`" and the graph decides whether that is the
// same entity as "the thing called `checkout` in `otel.service.name`". That is the whole of
// entity resolution's input, which is why the namespace strings below are part of the
// published schema rather than a detail of any one connector: the certain match rules in
// internal/resolution key on them, the fixtures are written in them, and two connectors that
// spell the same namespace differently will never be merged.
//
// A namespace is an *identifier* namespace, not an attribute name. `k8s.deployment` is a
// namespace whose values are `<namespace>/<name>`; `k8s.deployment.name` is an OTel attribute
// key and belongs in props (see props.go). The two are deliberately different strings.

const (
	// NSOTelService is the OpenTelemetry `service.name` of a service. Values are the service
	// name as the telemetry reports it, e.g. "checkout".
	NSOTelService = "otel.service.name"

	// NSK8sCluster is a Kubernetes cluster, valued by its name, e.g. "shop-prod".
	NSK8sCluster = "k8s.cluster"
	// NSK8sNamespace is a Kubernetes namespace, valued `<cluster>/<name>` or `<name>` when the
	// feeder watches a single cluster.
	NSK8sNamespace = "k8s.namespace"
	// NSK8sNodePool is a pool of Kubernetes nodes, valued `<cluster>/<pool>`.
	NSK8sNodePool = "k8s.nodepool"
	// NSK8sNode is a single Kubernetes node, valued `<cluster>/<name>`.
	NSK8sNode = "k8s.node"
	// NSK8sDeployment is a Kubernetes Deployment, valued `<namespace>/<name>`.
	NSK8sDeployment = "k8s.deployment"
	// NSK8sStatefulSet is a Kubernetes StatefulSet, valued `<namespace>/<name>`.
	NSK8sStatefulSet = "k8s.statefulset"
	// NSK8sDaemonSet is a Kubernetes DaemonSet, valued `<namespace>/<name>`.
	NSK8sDaemonSet = "k8s.daemonset"
	// NSK8sJob is a Kubernetes Job, valued `<namespace>/<name>`.
	NSK8sJob = "k8s.job"
	// NSK8sCronJob is a Kubernetes CronJob, valued `<namespace>/<name>`.
	NSK8sCronJob = "k8s.cronjob"
	// NSK8sService is a Kubernetes Service, valued `<namespace>/<name>`.
	NSK8sService = "k8s.service"
	// NSK8sIngress is a Kubernetes Ingress, valued `<namespace>/<name>`.
	NSK8sIngress = "k8s.ingress"
	// NSK8sConfigMap is a Kubernetes ConfigMap, valued `<namespace>/<name>`.
	NSK8sConfigMap = "k8s.configmap"
	// NSK8sSecret is a Kubernetes Secret, valued `<namespace>/<name>`. A secret node carries
	// its version identifier and never its value (log.ReasonSecretValue).
	NSK8sSecret = "k8s.secret"
	// NSK8sChange identifies a change observed by the Kubernetes feeder, valued
	// `<namespace>/<workload>@rev<N>` for a rollout or `@rv<N>` for a configuration change.
	NSK8sChange = "k8s.change"

	// NSOTelChange identifies a change observed from telemetry, valued
	// `<service>@<version>` for a `service.version` transition.
	NSOTelChange = "otel.change"

	// NSAppName is the Kubernetes label `app.kubernetes.io/name`, the identifier a workload
	// uses to declare its OpenTelemetry service name. Certain rule C2 keys on it
	// (research §10).
	NSAppName = "app.kubernetes.io/name"

	// NSServerAddress is a network endpoint a service talks to, valued by its host — the
	// namespace third-party dependencies discovered from spans are named in, e.g.
	// "analytics-db.shop.svc.cluster.local".
	NSServerAddress = "server.address"
	// NSThirdParty is an external dependency named by a vendor identifier rather than an
	// address, e.g. "stripe". Prefer NSServerAddress when the dependency has one.
	NSThirdParty = "third_party"

	// NSOwnerTeam is the team that owns something, valued by the team's identifier in the
	// system that declared it, e.g. "payments".
	NSOwnerTeam = "owner.team"

	// The deploy vocabulary (feature 004, FR-041; contracts/deploy-claims.md §1).
	//
	// The three `deploy.*` namespaces are the whole cross-source vocabulary of a rollout, and they
	// are here rather than in a connector's own package for the reason this list exists: **more than
	// one connector mints them**. GitHub states a commit, Vercel states the same commit, Cloud Run
	// states an image digest, and certain rule C8 merges two observations of one rollout by keying on
	// them. A namespace two connectors spell differently is a merge that silently never happens —
	// which feature 003's C4, C5 and C7 each demonstrated before a cross-source fixture existed.
	//
	// The four platform namespaces follow `NSK8sChange` and `NSOTelChange`'s precedent of living in
	// the SDK rather than in the connector: C8 is a published rule keyed on them, and a rule cannot
	// key on a string only one package can see. Feature 003 kept its whole `gcp.*` set local instead,
	// which is the right call for a set that large and nothing else reads.
	//
	// Each one's value form is fixed by a normaliser in deployref.go, not left to the caller. A
	// namespace whose value form is unfixed does not deliver the join it exists for.

	// NSDeployCommitSHA is the commit a rollout shipped, valued by the full lower-cased hex object
	// id. An abbreviated sha is omitted rather than normalised into a guess (FR-041).
	NSDeployCommitSHA = "deploy.commit_sha"
	// NSDeployImage is the container image a rollout deployed, valued by its **immutable digest**
	// form. A reference stating only a mutable tag is omitted: two rollouts sharing `:latest` would
	// merge into one.
	NSDeployImage = "deploy.image"
	// NSDeployRelease is the release identifier a rollout shipped, as the platform states it.
	NSDeployRelease = "deploy.release"

	// NSGitHubRepo is the repository shipping a service, valued `<owner>/<repository>` lower-cased.
	// GitHub treats both halves case-insensitively, so a spelling difference between two sources
	// would otherwise be two repositories.
	NSGitHubRepo = "github.repo"
	// NSGitHubChange is a change observed on GitHub — a workflow run, a deployment or a release —
	// valued by the platform's stable identifiers and never by a display name, which is neither
	// unique nor stable across a rename.
	NSGitHubChange = "github.change"

	// NSVercelProject is a Vercel project, valued by its **id** rather than its name, which is
	// stable across a rename.
	NSVercelProject = "vercel.project"
	// NSVercelChange is a Vercel deployment, valued by its `uid`.
	NSVercelChange = "vercel.change"
)

// WellKnownNamespaces is every namespace this SDK publishes, sorted. A feeder is free to mint
// its own — a new connector names things this list has never heard of — but two connectors
// that mean the same thing must use the same string, and that is what this list is for.
var WellKnownNamespaces = []string{
	NSAppName,
	NSDeployCommitSHA,
	NSDeployImage,
	NSDeployRelease,
	NSGitHubChange,
	NSGitHubRepo,
	NSK8sChange,
	NSK8sCluster,
	NSK8sConfigMap,
	NSK8sCronJob,
	NSK8sDaemonSet,
	NSK8sDeployment,
	NSK8sIngress,
	NSK8sJob,
	NSK8sNamespace,
	NSK8sNode,
	NSK8sNodePool,
	NSK8sSecret,
	NSK8sService,
	NSK8sStatefulSet,
	NSOTelChange,
	NSOTelService,
	NSOwnerTeam,
	NSServerAddress,
	NSThirdParty,
	NSVercelChange,
	NSVercelProject,
}

// IsWellKnownNamespace reports whether ns is one of WellKnownNamespaces.
func IsWellKnownNamespace(ns string) bool {
	return slices.Contains(WellKnownNamespaces, ns)
}

// Ref builds an identifier in a namespace. Both halves are required: the graph refuses a ref
// missing either (log.ReasonMissingRef).
func Ref(ns, value string) *graphv1.Ref {
	return &graphv1.Ref{Namespace: ns, Value: value}
}

// RefString renders a ref as `<namespace>=<value>`, the spelling the CLI and fixture manifests
// use for a query focus. A nil ref renders as the empty string.
func RefString(r *graphv1.Ref) string {
	if r == nil {
		return ""
	}
	return r.GetNamespace() + "=" + r.GetValue()
}
