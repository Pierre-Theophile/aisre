// SPDX-License-Identifier: Apache-2.0

package k8s_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	k8sfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/k8s"
)

// Refusing a writable credential (T056, FR-046, constitution VII).
//
// The decision is a pure function of the rules the API server returns, which is why it is a
// separate function from the call that fetches them: a fake reviewer can hand it any rule set,
// including the ones a real cluster is unlikely to produce but an operator will eventually
// create by accident.

// fakeReviewer answers a SelfSubjectRulesReview with a crafted rule list.
type fakeReviewer struct {
	rules      map[string][]authorizationv1.ResourceRule
	incomplete bool
	err        error
	reviewed   []string
}

func (f *fakeReviewer) Create(_ context.Context, review *authorizationv1.SelfSubjectRulesReview, _ metav1.CreateOptions) (*authorizationv1.SelfSubjectRulesReview, error) {
	if f.err != nil {
		return nil, f.err
	}
	namespace := review.Spec.Namespace
	f.reviewed = append(f.reviewed, namespace)
	out := review.DeepCopy()
	out.Status = authorizationv1.SubjectRulesReviewStatus{
		ResourceRules: f.rules[namespace],
		Incomplete:    f.incomplete,
	}
	return out, nil
}

func readOnlyRules() []authorizationv1.ResourceRule {
	return []authorizationv1.ResourceRule{
		{Verbs: []string{"get", "list", "watch"}, APIGroups: []string{""}, Resources: []string{"namespaces", "nodes", "services", "configmaps", "secrets"}},
		{Verbs: []string{"get", "list", "watch"}, APIGroups: []string{"apps"}, Resources: []string{"deployments", "statefulsets", "daemonsets"}},
		{Verbs: []string{"get", "list", "watch"}, APIGroups: []string{"batch"}, Resources: []string{"jobs", "cronjobs"}},
		{Verbs: []string{"get", "list", "watch"}, APIGroups: []string{"networking.k8s.io"}, Resources: []string{"ingresses"}},
		{Verbs: []string{"create"}, APIGroups: []string{"authorization.k8s.io"}, Resources: []string{"selfsubjectrulesreviews"}},
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

func TestPermissionGuardAcceptsAReadOnlyCredential(t *testing.T) {
	reviewer := &fakeReviewer{rules: map[string][]authorizationv1.ResourceRule{"shop": readOnlyRules()}}
	guard := &k8sfeeder.PermissionGuard{Reviewer: reviewer, Namespaces: []string{"shop"}, Log: discardLogger()}
	if err := guard.CheckReadOnly(t.Context()); err != nil {
		t.Fatalf("a get,list,watch credential was refused: %v", err)
	}
	if len(reviewer.reviewed) != 1 || reviewer.reviewed[0] != "shop" {
		t.Errorf("reviewed %v, want exactly the watched namespace", reviewer.reviewed)
	}
}

// TestPermissionGuardReviewsADefaultNamespaceWhenWatchingEverything: a namespaced review still
// reports what a ClusterRoleBinding grants, so one is enough to see a cluster-wide write.
func TestPermissionGuardReviewsADefaultNamespaceWhenWatchingEverything(t *testing.T) {
	reviewer := &fakeReviewer{rules: map[string][]authorizationv1.ResourceRule{
		k8sfeeder.ReviewNamespace: readOnlyRules(),
	}}
	guard := &k8sfeeder.PermissionGuard{Reviewer: reviewer, Log: discardLogger()}
	if err := guard.CheckReadOnly(t.Context()); err != nil {
		t.Fatalf("cluster-wide read-only credential was refused: %v", err)
	}
	if len(reviewer.reviewed) != 1 || reviewer.reviewed[0] != k8sfeeder.ReviewNamespace {
		t.Errorf("reviewed %v, want %q", reviewer.reviewed, k8sfeeder.ReviewNamespace)
	}
}

func TestPermissionGuardRefusesAWritableCredential(t *testing.T) {
	rules := append(readOnlyRules(), authorizationv1.ResourceRule{
		Verbs: []string{"patch"}, APIGroups: []string{"apps"}, Resources: []string{"deployments"},
	})
	guard := &k8sfeeder.PermissionGuard{
		Reviewer:   &fakeReviewer{rules: map[string][]authorizationv1.ResourceRule{"shop": rules}},
		Namespaces: []string{"shop"},
		Log:        discardLogger(),
	}
	err := guard.CheckReadOnly(t.Context())
	if err == nil {
		t.Fatal("a credential that can patch Deployments was accepted")
	}
	for _, want := range []string{"deployments.apps", "patch", "FR-046", "deploy/kind/rbac"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, err)
		}
	}
}

// TestPermissionGuardEscapeHatch: the flag exists for a disposable development cluster's admin
// kubeconfig and for nothing else, so it downgrades the refusal and says so.
func TestPermissionGuardEscapeHatch(t *testing.T) {
	rules := []authorizationv1.ResourceRule{
		{Verbs: []string{"*"}, APIGroups: []string{"*"}, Resources: []string{"*"}},
	}
	guard := &k8sfeeder.PermissionGuard{
		Reviewer:   &fakeReviewer{rules: map[string][]authorizationv1.ResourceRule{"shop": rules}},
		Namespaces: []string{"shop"},
		AllowWrite: true,
		Log:        discardLogger(),
	}
	if err := guard.CheckReadOnly(t.Context()); err != nil {
		t.Fatalf("--allow-write-credentials did not allow a cluster-admin credential: %v", err)
	}

	guard.AllowWrite = false
	if err := guard.CheckReadOnly(t.Context()); err == nil {
		t.Fatal("cluster-admin was accepted without --allow-write-credentials")
	}
}

// TestPermissionGuardRefusesWhenItCannotAsk: not being able to prove the credential is
// read-only is not the same as it being read-only.
func TestPermissionGuardRefusesWhenItCannotAsk(t *testing.T) {
	guard := &k8sfeeder.PermissionGuard{
		Reviewer: &fakeReviewer{err: errors.New("forbidden")},
		Log:      discardLogger(),
	}
	err := guard.CheckReadOnly(t.Context())
	if err == nil || !strings.Contains(err.Error(), "FR-046") {
		t.Fatalf("a failed review did not stop the feeder: %v", err)
	}
}

// TestPermissionGuardWithoutAReviewerIsANoOp is the replay case: a recording has no cluster and
// no credential, and refusing would make the recorded mode untestable.
func TestPermissionGuardWithoutAReviewerIsANoOp(t *testing.T) {
	guard := &k8sfeeder.PermissionGuard{Log: discardLogger()}
	if err := guard.CheckReadOnly(t.Context()); err != nil {
		t.Fatalf("a guard with no reviewer refused: %v", err)
	}
}

func TestEvaluateRules(t *testing.T) {
	cases := []struct {
		name  string
		rules []authorizationv1.ResourceRule
		want  []string
	}{
		{
			name:  "read-only",
			rules: readOnlyRules(),
		},
		{
			name: "wildcard verbs on a watched group",
			rules: []authorizationv1.ResourceRule{
				{Verbs: []string{"*"}, APIGroups: []string{"apps"}, Resources: []string{"deployments"}},
			},
			want: []string{"deployments.apps cluster-wide: create,delete,deletecollection,patch,update"},
		},
		{
			name: "wildcard everything",
			rules: []authorizationv1.ResourceRule{
				{Verbs: []string{"*"}, APIGroups: []string{"*"}, Resources: []string{"*"}},
			},
			want: []string{
				"namespaces cluster-wide: create,delete,deletecollection,patch,update",
				"nodes cluster-wide: create,delete,deletecollection,patch,update",
				"services cluster-wide: create,delete,deletecollection,patch,update",
				"configmaps cluster-wide: create,delete,deletecollection,patch,update",
				"secrets cluster-wide: create,delete,deletecollection,patch,update",
				"deployments.apps cluster-wide: create,delete,deletecollection,patch,update",
				"statefulsets.apps cluster-wide: create,delete,deletecollection,patch,update",
				"daemonsets.apps cluster-wide: create,delete,deletecollection,patch,update",
				"jobs.batch cluster-wide: create,delete,deletecollection,patch,update",
				"cronjobs.batch cluster-wide: create,delete,deletecollection,patch,update",
				"ingresses.networking.k8s.io cluster-wide: create,delete,deletecollection,patch,update",
			},
		},
		{
			name: "write on something the feeder does not watch",
			rules: []authorizationv1.ResourceRule{
				{Verbs: []string{"delete"}, APIGroups: []string{""}, Resources: []string{"pods"}},
			},
		},
		{
			name: "a subresource grant is a grant on the resource",
			rules: []authorizationv1.ResourceRule{
				{Verbs: []string{"update"}, APIGroups: []string{"apps"}, Resources: []string{"deployments/scale"}},
			},
			want: []string{"deployments.apps cluster-wide: update"},
		},
		{
			name: "creating a rules review is not a write on anything watched",
			rules: []authorizationv1.ResourceRule{
				{Verbs: []string{"create"}, APIGroups: []string{"authorization.k8s.io"}, Resources: []string{"selfsubjectrulesreviews"}},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			violations := k8sfeeder.EvaluateRules("", tc.rules)
			got := make([]string, 0, len(violations))
			for _, v := range violations {
				got = append(got, v.String())
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %d violations %v, want %d %v", len(got), got, len(tc.want), tc.want)
			}
			for i := range got {
				if !strings.Contains(got[i], tc.want[i]) {
					t.Errorf("violation %d = %q, want it to contain %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestRequiredScopesCoverEveryWatchedResource keeps the documentation and the watch in step: a
// resource added to the informers without a scope would be a credential the operator was never
// asked for.
func TestRequiredScopesCoverEveryWatchedResource(t *testing.T) {
	scopes := strings.Join(k8sfeeder.RequiredScopes(), "\n")
	for _, resource := range k8sfeeder.WatchedResources {
		if !strings.Contains(scopes, " "+resource) {
			t.Errorf("RequiredScopes does not mention %s, which the informers watch", resource)
		}
	}
}

// TestPermissionGuardWarnsOnAnIncompleteAnswer: an API server that cannot enumerate every rule
// — a webhook authorizer, most often — has not said the credential can write, so the feeder
// starts; it has also not said it cannot, so the operator is told.
func TestPermissionGuardWarnsOnAnIncompleteAnswer(t *testing.T) {
	guard := &k8sfeeder.PermissionGuard{
		Reviewer: &fakeReviewer{
			rules:      map[string][]authorizationv1.ResourceRule{"shop": readOnlyRules()},
			incomplete: true,
		},
		Namespaces: []string{"shop"},
		Log:        discardLogger(),
	}
	if err := guard.CheckReadOnly(t.Context()); err != nil {
		t.Fatalf("an incomplete rules review stopped the feeder: %v", err)
	}
}
