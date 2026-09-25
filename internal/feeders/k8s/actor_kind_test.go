// SPDX-License-Identifier: Apache-2.0

package k8s_test

import (
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	k8sfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/k8s"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// Typing the actor (ADR-0005 D1, 002 FR-029d).
//
// The evidence Kubernetes gives is thin and these tests keep the feeder honest about how thin:
// it types what it observed and leaves the field unspecified when it observed nothing. The
// difference matters downstream, where a change made by a controller reacting to the outage is
// exonerated after the estimated onset while a change made by a person is not.

// managedBy stamps a field manager on an object, which is the evidence `managedFields` gives.
func managedBy(d *appsv1.Deployment, manager string, at time.Time) *appsv1.Deployment {
	stamp := metav1.NewTime(at)
	d.ManagedFields = []metav1.ManagedFieldsEntry{{
		Manager:   manager,
		Operation: metav1.ManagedFieldsOperationApply,
		Time:      &stamp,
	}}
	return d
}

func TestRolloutActorKind(t *testing.T) {
	cases := []struct {
		name    string
		edit    func(*appsv1.Deployment)
		want    graphv1.ActorKind
		wantWhy string
	}{
		{
			name: "an annotated rollout is a person",
			edit: func(d *appsv1.Deployment) {
				d.Annotations[k8sfeeder.AnnotationChangeCause] = "alice: bump payments to 1.4.1"
			},
			want:    graphv1.ActorKind_PERSON,
			wantWhy: "somebody wrote the change cause by hand",
		},
		{
			name: "a delivery tool is automation, even when it writes a change cause",
			edit: func(d *appsv1.Deployment) {
				d.Annotations[k8sfeeder.AnnotationChangeCause] = "deploy-bot"
				managedBy(d, "argocd-controller", fixtureStart)
			},
			want:    graphv1.ActorKind_AUTOMATION,
			wantWhy: "a CI or deploy principal acting on a person's behalf",
		},
		{
			name: "a controller is a controller",
			edit: func(d *appsv1.Deployment) {
				managedBy(d, "kube-controller-manager", fixtureStart)
			},
			want:    graphv1.ActorKind_CONTROLLER,
			wantWhy: "a controller reacting to state",
		},
		{
			name: "an unrecognised manager is UNKNOWN, not a guess",
			edit: func(d *appsv1.Deployment) {
				managedBy(d, "some-internal-tool", fixtureStart)
			},
			want:    graphv1.ActorKind_ACTOR_KIND_UNKNOWN,
			wantWhy: "an actor was observed and could not be classified",
		},
		{
			name: "nothing observed is left unspecified",
			edit: func(*appsv1.Deployment) {},
			want: graphv1.ActorKind_ACTOR_KIND_UNSPECIFIED,
			wantWhy: "the source said nothing, so the graph records nothing: the zero value is " +
				"omitted by canonical serialisation and no golden moves (plan §I1)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := testFeeder(t)
			before := paymentsDeployment()
			mapOne(t, f, k8sfeeder.KindDeployments, k8sfeeder.EventAdded, before, fixtureStart)

			after := paymentsDeployment()
			after.ResourceVersion = "1050"
			after.Annotations[k8sfeeder.AnnotationRevision] = "8"
			tc.edit(after)

			events := mapOne(t, f, k8sfeeder.KindDeployments, k8sfeeder.EventModified, after,
				fixtureStart.Add(5*time.Minute))
			change, ok := changeOf(events, graphv1.ChangeKind_ROLLOUT)
			if !ok {
				t.Fatal("no ROLLOUT change")
			}
			if got := change.GetChange().GetActorKind(); got != tc.want {
				t.Errorf("actor_kind = %v, want %v (%s)", got, tc.want, tc.wantWhy)
			}
		})
	}
}

// TestScalingByAnAutoscalerIsAController is the case 002's causal ordering leans on hardest: a
// replica count that moved because load moved is not evidence of anybody's intent, and it must
// be typed so even though the object's annotations still describe the rollout that last
// touched it.
func TestScalingByAnAutoscalerIsAController(t *testing.T) {
	f := testFeeder(t)
	before := storefrontDeployment()
	before.Annotations[k8sfeeder.AnnotationChangeCause] = "alice: deploy 3.1.0"
	mapOne(t, f, k8sfeeder.KindDeployments, k8sfeeder.EventAdded, before, fixtureStart)

	after := storefrontDeployment()
	after.Annotations[k8sfeeder.AnnotationChangeCause] = "alice: deploy 3.1.0"
	after.ResourceVersion = "1060"
	replicas := int32(6)
	after.Spec.Replicas = &replicas
	managedBy(after, "horizontal-pod-autoscaler", fixtureStart.Add(10*time.Minute))

	events := mapOne(t, f, k8sfeeder.KindDeployments, k8sfeeder.EventModified, after,
		fixtureStart.Add(10*time.Minute))
	change, ok := changeOf(events, graphv1.ChangeKind_SCALING)
	if !ok {
		t.Fatal("no SCALING change")
	}
	if got := change.GetChange().GetActorKind(); got != graphv1.ActorKind_CONTROLLER {
		t.Errorf("actor_kind = %v, want CONTROLLER: the autoscaler reacted, nobody decided", got)
	}
	if got := change.GetChange().GetActor(); got != "hpa/shop/storefront" {
		t.Errorf("actor = %q, want the autoscaler named", got)
	}
}

// TestWorkloadLogPointerCarriesJoinKeys is ADR-0005 D6 on this feeder: the log pointer says
// which attribute of its own vocabulary carries the workload and the pod, and says nothing
// about the version, because a Kubernetes log stream does not carry one.
func TestWorkloadLogPointerCarriesJoinKeys(t *testing.T) {
	events := mapOne(t, testFeeder(t), k8sfeeder.KindDeployments, k8sfeeder.EventAdded,
		checkoutDeployment(), fixtureStart)

	var logPointer *graphv1.Pointer
	for _, ev := range events {
		body, ok := ev.GetBody().(*graphv1.EventEnvelope_UpsertNode)
		if !ok {
			continue
		}
		for _, p := range body.UpsertNode.GetPointers() {
			if p.GetKind() == graphv1.PointerKind_LOG {
				logPointer = p
			}
		}
	}
	if logPointer == nil {
		t.Fatal("the workload has no LOG pointer")
	}
	want := map[string]string{
		feeder.JoinRoleWorkload: feeder.AttrK8sDeploymentName,
		feeder.JoinRolePod:      feeder.AttrK8sPodName,
	}
	for role, attr := range want {
		if got := logPointer.GetJoinKeys()[role]; got != attr {
			t.Errorf("join_keys[%q] = %q, want %q", role, got, attr)
		}
	}
	if _, ok := logPointer.GetJoinKeys()[feeder.JoinRoleVersion]; ok {
		t.Error("a Kubernetes log stream carries no deployed version; the role must be absent, not guessed")
	}
	if got := len(logPointer.GetJoinKeys()); got != len(want) {
		t.Errorf("join_keys has %d roles, want %d: %v", got, len(want), logPointer.GetJoinKeys())
	}
}

// TestPointerCompatOmitsJoinKeys is the other half: replaying a corpus recorded before
// ADR-0005 D6 reproduces the pointer it recorded, with no join keys at all. An empty map is
// omitted by canonical serialisation, so the recording is byte-identical (002 tasks.md T026).
func TestPointerCompatOmitsJoinKeys(t *testing.T) {
	opts := fixtureOptions()
	opts.PointerCompat = feeder.PointerCompatNoJoinKeys
	f, err := k8sfeeder.New(opts, nil)
	if err != nil {
		t.Fatalf("new feeder: %v", err)
	}
	events := mapOne(t, f, k8sfeeder.KindDeployments, k8sfeeder.EventAdded,
		checkoutDeployment(), fixtureStart)
	for _, ev := range events {
		body, ok := ev.GetBody().(*graphv1.EventEnvelope_UpsertNode)
		if !ok {
			continue
		}
		for _, p := range body.UpsertNode.GetPointers() {
			if len(p.GetJoinKeys()) != 0 {
				t.Errorf("pointer %v carries join keys under PointerCompatNoJoinKeys", p.GetKind())
			}
		}
	}
}
