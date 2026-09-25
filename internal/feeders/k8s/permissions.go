// SPDX-License-Identifier: Apache-2.0

package k8s

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"

	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Proving the credential is read-only (T056, FR-046, constitution VII).
//
// "No component MUST write to any production system" is not a promise a connector can make by
// being careful: it is a property of the credential it holds, and the only honest way to state
// it is to ask the system. Kubernetes answers with SelfSubjectRulesReview — the same question
// `kubectl auth can-i --list` asks — which returns every rule the API server would apply to
// this identity, including the ones inherited from ClusterRoleBindings.
//
// The feeder refuses to start if that answer grants create, update, patch, delete or
// deletecollection on anything it watches. Not "avoids calling those endpoints": holds the
// power at all. A cluster-admin kubeconfig pointed at production is a loaded gun, and an
// investigation tool that cannot be trusted not to mutate is not one an SRE will run during an
// incident.
//
// deploy/kind/rbac/ is the documented way to satisfy this: a read-only ClusterRole, a
// ServiceAccount, and a script that mints a kubeconfig for it. Options.AllowWriteCredentials
// exists only so that the demo cluster's default admin kubeconfig can be used while developing
// the feeder itself, and it says so, loudly, every time it is used.

// writeVerbs are the verbs whose presence on a watched resource makes a credential unusable.
// `*` is included because a rule granting every verb grants these.
var writeVerbs = []string{"create", "update", "patch", "delete", "deletecollection", "*"}

// apiResource is one watched resource as RBAC names it.
type apiResource struct {
	// group is the API group, empty for the core group.
	group string
	// resource is the plural resource name, which is also the payload kind.
	resource string
	// version is the group version, used only to render RequiredScopes.
	version string
}

// watchedAPIResources is every resource the informers read, in the order RequiredScopes lists
// them. It is the single place the resource set is written down: informers.go, the permission
// check and deploy/kind/rbac/clusterrole.yaml all derive from it.
var watchedAPIResources = []apiResource{
	{group: "", resource: KindNamespaces, version: "v1"},
	{group: "", resource: KindNodes, version: "v1"},
	{group: "", resource: KindServices, version: "v1"},
	{group: "", resource: KindConfigMaps, version: "v1"},
	{group: "", resource: KindSecrets, version: "v1"},
	{group: "apps", resource: KindDeployments, version: "v1"},
	{group: "apps", resource: KindStatefulSets, version: "v1"},
	{group: "apps", resource: KindDaemonSets, version: "v1"},
	{group: "batch", resource: KindJobs, version: "v1"},
	{group: "batch", resource: KindCronJobs, version: "v1"},
	{group: "networking.k8s.io", resource: KindIngresses, version: "v1"},
}

// RequiredScopes documents the permissions the feeder asks Kubernetes for, in the spelling
// Description.RequiredScopes publishes and deploy/kind/rbac/clusterrole.yaml grants.
func RequiredScopes() []string {
	scopes := make([]string, 0, len(watchedAPIResources)+1)
	for _, r := range watchedAPIResources {
		scopes = append(scopes, "get,list,watch on "+r.path()+" "+r.resource)
	}
	// The only thing the feeder creates is the question "what may I do?".
	scopes = append(scopes, "create on authorization.k8s.io/v1 selfsubjectrulesreviews")
	return scopes
}

// path renders the group-version an operator would write in a role, e.g. "apps/v1".
func (r apiResource) path() string {
	if r.group == "" {
		return r.version
	}
	return r.group + "/" + r.version
}

// Violation is one write permission the credential holds on a watched resource.
type Violation struct {
	// Namespace is the namespace the rules were reviewed in.
	Namespace string
	// Group and Resource name the watched resource, as RBAC spells them.
	Group, Resource string
	// Verbs are the write verbs granted on it, sorted.
	Verbs []string
}

// String renders a violation the way the startup error reports it.
func (v Violation) String() string {
	name := v.Resource
	if v.Group != "" {
		name = v.Resource + "." + v.Group
	}
	scope := "cluster-wide"
	if v.Namespace != "" {
		scope = "in namespace " + v.Namespace
	}
	return fmt.Sprintf("%s %s: %s", name, scope, strings.Join(v.Verbs, ","))
}

// RulesReviewer is the one Kubernetes call the permission check makes. client-go's
// `AuthorizationV1().SelfSubjectRulesReviews()` satisfies it, and so does a fake in a unit
// test, which is how the decision logic is tested without a cluster.
type RulesReviewer interface {
	Create(ctx context.Context, review *authorizationv1.SelfSubjectRulesReview, opts metav1.CreateOptions) (*authorizationv1.SelfSubjectRulesReview, error)
}

// PermissionGuard asks the cluster what the feeder's credential may do and refuses a writable
// one.
type PermissionGuard struct {
	// Reviewer performs the SelfSubjectRulesReview.
	Reviewer RulesReviewer
	// Namespaces are the namespaces to review. Empty means the watch is cluster-wide, which
	// is reviewed in ReviewNamespace: a namespaced review returns the rules granted by
	// ClusterRoleBindings too, so one is enough to see a cluster-wide grant.
	Namespaces []string
	// AllowWrite downgrades the refusal to a warning. See Options.AllowWriteCredentials.
	AllowWrite bool
	// Log receives the warning an allowed write credential produces.
	Log *slog.Logger
}

var _ PermissionChecker = (*PermissionGuard)(nil)

// ReviewNamespace is the namespace a cluster-wide watch reviews its rules in. Any namespace
// would do; `default` exists in every cluster.
const ReviewNamespace = "default"

// CheckReadOnly refuses to proceed when the credential can write anything the feeder watches.
func (g *PermissionGuard) CheckReadOnly(ctx context.Context) error {
	if g == nil || g.Reviewer == nil {
		return nil
	}
	log := g.Log
	if log == nil {
		log = slog.Default()
	}

	namespaces := g.Namespaces
	clusterWide := len(namespaces) == 0
	if clusterWide {
		namespaces = []string{ReviewNamespace}
	}

	var violations []Violation
	for _, namespace := range namespaces {
		review, err := g.Reviewer.Create(ctx, &authorizationv1.SelfSubjectRulesReview{
			Spec: authorizationv1.SelfSubjectRulesReviewSpec{Namespace: namespace},
		}, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("k8s: SelfSubjectRulesReview in namespace %s: %w (FR-046: the feeder cannot prove its credential is read-only, so it will not start)", namespace, err)
		}
		if review.Status.Incomplete {
			log.Warn("k8s: the API server could not enumerate every rule for this credential; "+
				"the read-only check saw only part of the answer",
				"namespace", namespace, "evaluation_error", review.Status.EvaluationError)
		}
		reported := namespace
		if clusterWide {
			reported = ""
		}
		violations = append(violations, EvaluateRules(reported, review.Status.ResourceRules)...)
	}
	if len(violations) == 0 {
		return nil
	}

	lines := make([]string, 0, len(violations))
	for _, v := range violations {
		lines = append(lines, "  "+v.String())
	}
	if g.AllowWrite {
		log.Warn("k8s: --allow-write-credentials: this credential CAN WRITE to the cluster the feeder is watching. "+
			"Constitution VII requires a read-only credential; this flag exists only for a disposable development "+
			"cluster's admin kubeconfig. Use deploy/kind/rbac/ instead.",
			"grants", strings.Join(lines, ";"))
		return nil
	}
	return fmt.Errorf("k8s: the credential can write to resources this feeder watches, so it will not start (FR-046, constitution VII):\n%s\n"+
		"Grant get,list,watch only — deploy/kind/rbac/ has a ready ClusterRole and a script that mints a kubeconfig for it.\n"+
		"--allow-write-credentials overrides this for a disposable development cluster",
		strings.Join(lines, "\n"))
}

// EvaluateRules is the decision: which watched resources does this rule set let the caller
// write? It takes the rules rather than a client so that it can be tested exhaustively without
// a cluster, which is the whole reason the check is split this way.
func EvaluateRules(namespace string, rules []authorizationv1.ResourceRule) []Violation {
	granted := map[apiResource]map[string]bool{}
	for _, rule := range rules {
		verbs := writeVerbsOf(rule.Verbs)
		if len(verbs) == 0 {
			continue
		}
		for _, watched := range watchedAPIResources {
			if !matchesAny(rule.APIGroups, watched.group) || !matchesResource(rule.Resources, watched.resource) {
				continue
			}
			// A rule narrowed to named objects still grants the verb on them, and the feeder
			// is not in a position to argue that those particular objects do not matter.
			if granted[watched] == nil {
				granted[watched] = map[string]bool{}
			}
			for _, verb := range verbs {
				granted[watched][verb] = true
			}
		}
	}
	if len(granted) == 0 {
		return nil
	}

	out := make([]Violation, 0, len(granted))
	for _, watched := range watchedAPIResources {
		verbs, ok := granted[watched]
		if !ok {
			continue
		}
		names := make([]string, 0, len(verbs))
		for verb := range verbs {
			names = append(names, verb)
		}
		sort.Strings(names)
		out = append(out, Violation{Namespace: namespace, Group: watched.group, Resource: watched.resource, Verbs: names})
	}
	return out
}

// writeVerbsOf returns the write verbs a rule grants, expanding `*` to all of them.
func writeVerbsOf(verbs []string) []string {
	var out []string
	for _, verb := range verbs {
		verb = strings.ToLower(strings.TrimSpace(verb))
		if verb == "*" {
			return []string{"create", "delete", "deletecollection", "patch", "update"}
		}
		if slices.Contains(writeVerbs, verb) {
			out = append(out, verb)
		}
	}
	sort.Strings(out)
	return slices.Compact(out)
}

// matchesAny reports whether a rule's API groups cover one group. The empty string is the core
// group and is spelled "" in RBAC, so an empty entry is a match rather than a wildcard.
func matchesAny(groups []string, want string) bool {
	for _, group := range groups {
		if group == "*" || group == want {
			return true
		}
	}
	return false
}

// matchesResource reports whether a rule's resources cover one resource, ignoring subresources
// (`deployments/scale` is a different resource, and granting it is granting a scale).
func matchesResource(resources []string, want string) bool {
	for _, resource := range resources {
		if resource == "*" || resource == want {
			return true
		}
		// `deployments/scale` is the resource a scale grant names, and it is a write on the
		// Deployment by any honest reading.
		if base, _, found := strings.Cut(resource, "/"); found && base == want {
			return true
		}
	}
	return false
}
