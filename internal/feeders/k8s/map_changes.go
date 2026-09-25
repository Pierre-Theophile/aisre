// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"fmt"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// Change mapping (T059, FR-043, FR-003, research §12).
//
// Change is a first-class node type, not an annotation on a workload (constitution,
// Definitions): it is what a diff ranks and what an investigation is looking for. A watch
// stream does not announce changes, it announces object versions, so a change is the
// *difference* between two versions of one object — the one place in this feeder where a fact
// is derived from more than the payload in hand.
//
// That has two consequences the rest of the design pays for.
//
//   - A difference needs a previous version. On ADDED there is none — the informer's initial
//     list is not a change, and neither is the first object seen after a restart — so ADDED
//     records the state and emits nothing. The checkpoint (FR-032) is what tells the graph that
//     the silence before it was ignorance rather than absence, which is the honest answer.
//   - A difference depends on the order the two versions arrive in. The feeder therefore
//     declares a reordering window (60 s) and the shuffle check permutes only inside it: two
//     versions of one object that are minutes apart cannot be swapped, and two that are
//     milliseconds apart are one edit an operator would not distinguish either.
//
// Every change identity is a pure function of the object and the change: a rollout is named by
// its revision, a scaling by the minute it happened in, a configuration change by the
// resourceVersion it produced. Re-reading the same transition mints the same id and is a
// DUPLICATE_NOOP.

// changeManagerHPA is the fieldManager the HorizontalPodAutoscaler controller writes under.
// It is how a scaling by an autoscaler is told from one an operator performed.
const changeManagerHPA = "horizontal-pod-autoscaler"

// Typing the actor (ADR-0005 D1, 002 FR-029d).
//
// `Change.actor` has always been a string: `alice`, `argocd`, `hpa/shop/storefront`. A string
// is enough to render and useless to reason with, and 002 has to reason with it — "a controller
// reacting to the outage" is evidence of a different kind from "a person deployed something",
// and the causal ordering exonerates the first class after the estimated onset. So the feeder
// types what it observed, from the two things Kubernetes actually tells it:
//
//   - `kubernetes.io/change-cause`, the annotation a human or a delivery tool writes. An
//     operator annotating a rollout is the clearest evidence of a PERSON there is, and where
//     the annotation names a pipeline the field manager below still decides.
//   - `managedFields`, the field manager that last wrote the object. Kubernetes' own
//     controllers are named there — `deployment-controller`, `kube-controller-manager`,
//     `horizontal-pod-autoscaler` — and so are the delivery tools: `argocd`, `flux`,
//     `helm`, `kubectl-client-side-apply`.
//
// Three rules, in this order, and **nothing is guessed**: an object carrying neither an
// annotation nor a manager leaves the kind ACTOR_KIND_UNSPECIFIED, which canonical
// serialisation omits entirely, so the graph records no claim rather than a weak one (plan §I1).
// ACTOR_KIND_UNKNOWN is reserved for the case where an actor *was* observed and could not be
// classified — a field manager none of the patterns below recognise.

// controllerManagers are the field managers that mean "a controller reacting to state".
var controllerManagers = []string{
	changeManagerHPA,
	"kube-controller-manager",
	"deployment-controller",
	"replicaset-controller",
	"statefulset-controller",
	"daemonset-controller",
	"cluster-autoscaler",
	"vertical-pod-autoscaler",
	"operator",
}

// automationManagers are the field managers that mean "a CI job, bot or deploy principal
// acting on a person's behalf" — the class 003 FR-013 and 005 FR-033a need told apart from a
// controller, because a deploy pipeline is a person's intent on a delay and an autoscaler is
// not anybody's intent at all.
var automationManagers = []string{
	"argocd", "argo-cd", "flux", "fluxcd", "helm", "kustomize-controller", "helm-controller",
	"terraform", "pulumi", "spinnaker", "jenkins", "github-actions", "gitlab-runner",
	"kubectl-client-side-apply", "kubectl-apply", "kubectl-edit", "kubectl-patch", "kubectl",
	"skaffold", "tilt",
}

// actorKindOfManager types a field manager, or returns ACTOR_KIND_UNSPECIFIED when there was
// no manager to type at all.
func actorKindOfManager(manager string) graphv1.ActorKind {
	if manager == "" {
		return graphv1.ActorKind_ACTOR_KIND_UNSPECIFIED
	}
	lowered := strings.ToLower(manager)
	for _, pattern := range controllerManagers {
		if strings.Contains(lowered, pattern) {
			return graphv1.ActorKind_CONTROLLER
		}
	}
	for _, pattern := range automationManagers {
		if strings.Contains(lowered, pattern) {
			return graphv1.ActorKind_AUTOMATION
		}
	}
	// An actor was observed — something wrote this object — and the feeder cannot say what it
	// is. That is precisely ACTOR_KIND_UNKNOWN, and the evidence is the manager name, which
	// travels on the change as `actor`.
	return graphv1.ActorKind_ACTOR_KIND_UNKNOWN
}

// actorKindOf types the actor of a rollout or a configuration change: the annotation first,
// then the field manager.
func actorKindOf(annotations map[string]string, managers []metav1.ManagedFieldsEntry) graphv1.ActorKind {
	if cause := firstValue(annotations, AnnotationChangeCause); cause != "" {
		// A change cause written by a delivery tool is still that tool's; one written by hand
		// is a person's. The manager is the evidence for which, and where there is none the
		// annotation itself is a human act.
		if kind := actorKindOfManager(latestManager(managers)); kind == graphv1.ActorKind_AUTOMATION ||
			kind == graphv1.ActorKind_CONTROLLER {
			return kind
		}
		return graphv1.ActorKind_PERSON
	}
	return actorKindOfManager(latestManager(managers))
}

// RolloutEvent returns the ROLLOUT observed between two versions of a workload, or nil when
// the versions describe no rollout.
//
// A rollout is a new pod template. The Deployment controller records that as an incremented
// `deployment.kubernetes.io/revision`; workload types that keep no revision (a DaemonSet, a
// StatefulSet under some controllers) show it as a changed container image instead, which is
// the same fact observed one level down.
func (m *Mapper) RolloutEvent(prev, cur workloadView, at time.Time) *graphv1.EventEnvelope {
	revision := cur.revision
	switch {
	case revision != "" && revision != prev.revision:
	case revision == "" && cur.image != "" && cur.image != prev.image:
		// No revision to name the change by; the image is what changed and what names it.
		revision = imageTag(cur.image)
		if revision == "" {
			return nil
		}
	default:
		return nil
	}

	validAt := at
	if !cur.progressingAt.IsZero() {
		validAt = cur.progressingAt
	}
	summary := fmt.Sprintf("rollout %s to revision %s", cur.name, revision)
	if tag := imageTag(cur.image); tag != "" {
		summary += " (" + tag + ")"
	}
	value := cur.key() + "@rev" + revision
	return feeder.ObserveChange(m.desc, m.id("change", value), feeder.ChangeFact{
		Meta:      feeder.Meta{Seq: seqOf(cur.resourceVersion)},
		Ref:       feeder.Ref(feeder.NSK8sChange, value),
		Kind:      graphv1.ChangeKind_ROLLOUT,
		Summary:   summary,
		Actor:     m.actorOf(cur),
		ActorKind: actorKindOf(cur.annotations, cur.managers),
		OriginRef: cur.resourcePath(),
		Targets:   []*graphv1.Ref{feeder.Ref(cur.kind.refNamespace, cur.key())},
		ValidAt:   validAt,
	})
}

// ScalingEvent returns the SCALING observed between two versions of a workload, or nil when
// the replica count did not change.
//
// The change is named by the minute it happened in — `<namespace>/<name>@scale-HHMM`, the
// spelling fixtures/rollout-regression-01 publishes — because a scaling has no revision and no
// identifier of its own in Kubernetes. Two scalings of one workload inside one minute
// therefore collapse onto one change node; that is a deliberate loss of resolution, and the
// replica counts in the summary come from the first of them.
func (m *Mapper) ScalingEvent(prev, cur workloadView, at time.Time) *graphv1.EventEnvelope {
	if prev.replicas == nil || cur.replicas == nil || *prev.replicas == *cur.replicas {
		return nil
	}
	value := fmt.Sprintf("%s@scale-%s", cur.key(), at.UTC().Format("1504"))
	return feeder.ObserveChange(m.desc, m.id("change", value), feeder.ChangeFact{
		Meta:    feeder.Meta{Seq: seqOf(cur.resourceVersion)},
		Ref:     feeder.Ref(feeder.NSK8sChange, value),
		Kind:    graphv1.ChangeKind_SCALING,
		Summary: fmt.Sprintf("scale %s from %d to %d replicas", cur.name, *prev.replicas, *cur.replicas),
		Actor:   m.scalingActorOf(cur),
		// A replica count that moved under an autoscaler or a ReplicaSet controller is a
		// CONTROLLER reacting to state, whatever the object's annotations say about the
		// rollout that last touched it (ADR-0005 D1).
		ActorKind: scalingActorKindOf(cur),
		OriginRef: cur.resourcePath(),
		Targets:   []*graphv1.Ref{feeder.Ref(cur.kind.refNamespace, cur.key())},
		ValidAt:   at,
	})
}

// ConfigChangeEvent returns the CONFIG_CHANGE observed between two versions of a ConfigMap or
// a Secret, or nil when nothing changed or nothing references it.
//
// A configuration object nobody consumes produces no change node: the graph would carry a
// change with no blast radius, which ranks above nothing and explains nothing. `dependents`
// are the workloads the feeder has seen referencing it, and they become targets alongside the
// CONFIG node itself, so the change sits at hop 0 of every workload it could have broken.
func (m *Mapper) ConfigChangeEvent(prev, cur configView, dependents []*graphv1.Ref, at time.Time) *graphv1.EventEnvelope {
	if cur.resourceVersion == "" || cur.resourceVersion == prev.resourceVersion || len(dependents) == 0 {
		return nil
	}
	if cur.kind.payload == KindConfigMaps && prev.valueHash != "" && prev.valueHash == cur.valueHash {
		// The object was touched but its content is the same: an annotation edit, a
		// re-apply. The workload consuming it saw nothing change, so neither did the graph.
		return nil
	}
	singular := strings.TrimSuffix(cur.kind.plural, "s")
	value := fmt.Sprintf("%s/%s/%s@rv%s", cur.namespace, singular, cur.name, cur.resourceVersion)
	targets := append([]*graphv1.Ref{feeder.Ref(cur.kind.refNamespace, cur.key())}, dependents...)
	return feeder.ObserveChange(m.desc, m.id("change", value), feeder.ChangeFact{
		Meta:      feeder.Meta{Seq: seqOf(cur.resourceVersion)},
		Ref:       feeder.Ref(feeder.NSK8sChange, value),
		Kind:      graphv1.ChangeKind_CONFIG_CHANGE,
		Summary:   fmt.Sprintf("%s %s changed to resourceVersion %s", singular, cur.name, cur.resourceVersion),
		Actor:     managerOf(cur.annotations, nil),
		ActorKind: actorKindOf(cur.annotations, nil),
		// Where the change was made, not what it was made of: a ConfigMap's content is
		// summarised by the value hash on its CONFIG node and never carried here.
		OriginRef: resourcePath(cur.kind, cur.namespace, cur.name),
		Targets:   targets,
		ValidAt:   at,
	})
}

// actorOf is who performed a change: what the operator or the delivery tool wrote in
// `kubernetes.io/change-cause`, and failing that the field manager that last touched the
// object.
func (m *Mapper) actorOf(w workloadView) string {
	if cause := annotationOf(w, AnnotationChangeCause); cause != "" {
		return cause
	}
	return managerOf(w.annotations, w.managers)
}

// scalingActorOf names an autoscaler as `hpa/<namespace>/<name>` and anything else by its
// field manager, which is the spelling fixtures/rollout-regression-01 records.
func (m *Mapper) scalingActorOf(w workloadView) string {
	manager := latestManager(w.managers)
	if manager != "" && strings.Contains(strings.ToLower(manager), changeManagerHPA) {
		return "hpa/" + w.namespace + "/" + w.name
	}
	return m.actorOf(w)
}

// scalingActorKindOf types the actor of a scaling. The autoscaler and the ReplicaSet
// controller are the reason this class exists: a replica count that moved because load moved
// is not evidence of anybody's intent, and 002 needs to say so rather than rank it as a
// deployment.
func scalingActorKindOf(w workloadView) graphv1.ActorKind {
	if kind := actorKindOfManager(latestManager(w.managers)); kind != graphv1.ActorKind_ACTOR_KIND_UNSPECIFIED {
		return kind
	}
	return actorKindOf(w.annotations, w.managers)
}

// managerOf falls back from an explicit change cause to the field manager that last wrote.
func managerOf(annotations map[string]string, managers []metav1.ManagedFieldsEntry) string {
	if cause := firstValue(annotations, AnnotationChangeCause); cause != "" {
		return cause
	}
	return latestManager(managers)
}

// latestManager is the field manager of the most recent Update or Apply, or "" when the object
// carries no managedFields — an older API server, or an object this feeder read from a
// recording that dropped them.
//
// Ties are broken by manager name so that two entries stamped with the same second produce the
// same answer on every replay (FR-023).
func latestManager(managers []metav1.ManagedFieldsEntry) string {
	entries := make([]metav1.ManagedFieldsEntry, 0, len(managers))
	for _, entry := range managers {
		if entry.Manager == "" || entry.Time == nil {
			continue
		}
		entries = append(entries, entry)
	}
	if len(entries) == 0 {
		return ""
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if !entries[i].Time.Time.Equal(entries[j].Time.Time) {
			return entries[i].Time.After(entries[j].Time.Time)
		}
		return entries[i].Manager < entries[j].Manager
	})
	return entries[0].Manager
}

// imageTag is the version half of a container image reference, which is what a rollout summary
// shows an operator. A digest is not a tag: `nginx@sha256:…` reads as no version at all, and
// saying so is better than showing 64 hexadecimal characters.
func imageTag(image string) string {
	if image == "" || strings.Contains(image, "@") {
		return ""
	}
	slash := strings.LastIndex(image, "/")
	colon := strings.LastIndex(image, ":")
	if colon <= slash {
		return ""
	}
	return image[colon+1:]
}

// RolloutDeployCorrelations are the cross-source correlation keys a Kubernetes rollout carries
// (004 T136, T148; FR-041, FR-045, SC-004, SC-017).
//
// ---------------------------------------------------------------------------------------------
// What this carries, and what it deliberately does not
//
// C8 merges a deploy pipeline's observation of a rollout with the platform's. Feature 003 gave Cloud Run
// its side in T135; this is feature 001's cluster. Without it a GitHub deployment whose target is a
// Kubernetes workload has nothing to agree with, so C8 could never fire on the estate most of this
// project's fixtures describe.
//
// Exactly one key, and its absence is the interesting half:
//
//   - `deploy.image`, from the workload's primary container image, and ONLY where a digest was stated.
//     The value comes from `feeder.Image`, the same normaliser Cloud Run's side uses, so the two
//     sources produce byte-identical values for one image (SC-017) — which is the whole point, since a
//     value that differs by a tag or by case is a value that cannot join.
//
//   - NO key for a mutable tag. A workload running `shop/api:latest` states nothing immutable, and
//     `feeder.Image` returns false for it. That is omission over invention (FR-041), and here the cost
//     of inventing is concrete rather than stylistic: `:latest` is shared by every workload that has
//     ever run it, so claiming it would make one correlation key that every cluster's every rollout
//     carries — and C8 merging on it would join two unrelated workloads in two unrelated clusters,
//     certainly, with no human in the loop. A tag is a name for "whatever is newest", which is the
//     opposite of an identifier.
//
//   - `deploy.commit_sha` ONLY where the operator opted in (004 T149) — see rolloutCommit. A
//     Deployment's spec states no commit, and the label a deploy tool might leave is written by whoever
//     can write to the cluster, so it is read only under an explicit `CommitLabels` configuration, as
//     feature 003 did for Cloud Run with `commit_from_labels`.
//
// # This key has no counterpart yet, and saying so is the point
//
// C8 needs two sources to agree on a key AND to share a target. Across the source set as it stands,
// nothing can agree with this one:
//
//	source      keys minted                          target
//	GitHub      deploy.commit_sha, deploy.release    whatever the operator maps (may be a workload)
//	Cloud Run   deploy.image, deploy.commit_sha      gcp.cloudrun.service
//	Kubernetes  deploy.image                         k8s.deployment
//
// Cloud Run states an image but targets a Cloud Run service, which is never the same entity as a
// workload. GitHub can be mapped onto a workload but states a commit and no image. So there is no pair,
// and **C8 cannot fire for a Kubernetes rollout today** — this is the platform half of a join whose other
// half nobody writes yet.
//
// That was recorded here rather than left for someone to discover, because it is exactly the shape this
// project keeps finding: a correct rule or key, published, evaluated, and never once firing. T149 closes
// it where the operator allows: with `CommitLabels` configured, the rollout carries the commit its pod
// template states, and the GitHub deployment an operator has mapped to `k8s.deployment` meets the rollout
// it caused (rolloutCommit). Without it, the table above still holds and C8 still cannot fire here.
//
// The image key is worth minting regardless: the refusal below is what stops a future source from joining
// on a mutable tag, and a fixture recorded against this feeder carries the key a later join will need.
func RolloutDeployCorrelations(image string) []feeder.CorrelationKey {
	value, ok := feeder.Image(image)
	if !ok {
		return nil
	}
	return []feeder.CorrelationKey{{
		Namespace: feeder.NSDeployImage, Value: value,
		Why: "the digest-pinned image this rollout deployed",
	}}
}

// RolloutCorrelationEvents are the CorrelateEntity events for one rollout, or nothing when the workload
// states no immutable image.
//
// The environment rides on every key, because C8 requires it to agree and reads it from the key's
// attributes. A rollout whose environment the cluster does not declare still carries the feeder's
// configured default (options.go), so the attribute is never absent — and C8 comparing two absent
// environments as equal is a case that therefore cannot arise here.
func (m *Mapper) RolloutCorrelationEvents(change *graphv1.EventEnvelope, prev, cur workloadView, env string, at time.Time) []*graphv1.EventEnvelope {
	if change == nil {
		return nil
	}
	subject := change.GetObserveChange().GetRef()
	keys := RolloutDeployCorrelations(cur.image)
	if key, ok := m.rolloutCommit(prev, cur); ok {
		keys = append(keys, key)
	}
	var out []*graphv1.EventEnvelope
	for _, key := range keys {
		attrs, err := feeder.NewProps().
			Str(feeder.AttrDeploymentEnvironment, env).
			Str("sre.k8s.correlation_source", key.Why).
			Build()
		if err != nil {
			// NewProps only fails on a malformed value, and both of these are plain strings the caller
			// already put in a node's props. Dropping the key is still better than emitting one with no
			// environment, which C8 would compare against a stated one and refuse.
			continue
		}
		out = append(out, feeder.Correlate(m.desc,
			feeder.NewID(m.desc.SourceID, "correlation", subject.GetValue(), key.Namespace, key.Value),
			feeder.CorrelationFact{
				Meta:       feeder.Meta{SourceObservedAt: at},
				Subject:    subject,
				Key:        key.Ref(),
				Attributes: attrs,
			}))
	}
	return out
}

// rolloutCommit is the commit a rollout shipped, read from the operator's configured pod-template keys
// (004 T149), or false.
//
// # Why opt-in
//
// C8 is a CERTAIN rule: it merges without a human in the loop. A commit read from a label is a value
// anyone with edit rights on the namespace can write, so turning it on makes that value a merge key —
// a decision about whom to trust, which belongs to the operator who knows who deploys to the cluster.
// Feature 003 made the same call for Cloud Run with `commit_from_labels`; the reasoning is identical,
// and a default that silently trusted every cluster's labels would be the one place it was not.
//
// # Why only the pod template
//
// A rollout IS a pod-template change: the controller rolls out a new ReplicaSet because the template
// differs. A label or annotation on the template is therefore part of the revision being rolled out,
// written in the same apply that changed it. A label on the Deployment's own metadata is not: it can be
// updated without a rollout and left behind by one, so it can name a commit this revision does not run.
// Reading it would let C8 merge a pipeline's deployment with a rollout of different code.
//
// # Why only when it changed
//
// The commit is minted only when this rollout CHANGED it. A rollout whose template states the same
// commit as the previous revision — a `kubectl rollout restart`, a resource tweak, an image bumped by
// hand while the label was forgotten — did not ship that commit; the previous rollout did. Minting the
// unchanged value would give two rollouts of one workload the same commit key, and C8 would merge the
// pipeline's deployment with whichever it met, including the one that shipped nothing.
//
// A value that is not a full commit id is passed over, and the next key is tried: an abbreviation is
// never padded (FR-041).
func (m *Mapper) rolloutCommit(prev, cur workloadView) (feeder.CorrelationKey, bool) {
	commit, key, ok := templateCommit(cur, m.opts.CommitLabels)
	if !ok {
		return feeder.CorrelationKey{}, false
	}
	if before, _, had := templateCommit(prev, m.opts.CommitLabels); had && before == commit {
		return feeder.CorrelationKey{}, false
	}
	return feeder.CorrelationKey{
		Namespace: feeder.NSDeployCommitSHA, Value: commit,
		Why: "the commit the pod template states under " + key + ", configured as trusted",
	}, true
}

// templateCommit reads the first configured key holding a full commit id, from the pod template's
// labels and then its annotations.
func templateCommit(w workloadView, keys []string) (commit, key string, ok bool) {
	for _, k := range keys {
		for _, source := range []map[string]string{w.podLabels, w.podAnnotations} {
			if value, valid := feeder.CommitSHA(source[k]); valid {
				return value, k, true
			}
		}
	}
	return "", "", false
}
