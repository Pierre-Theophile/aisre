// SPDX-License-Identifier: Apache-2.0

package resolution

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/graph"
)

// The published match-rule registry (FR-037, research §10).
//
// A pod, a telemetry service name, a repository and a chat channel may be the same thing under
// four names. Deciding that they are is the hard problem of this project, and constitution VI
// says it must be done in the open: every claim stored before any merge, every rule published,
// every merge explained.
//
// This package holds the rules and nothing else. It does not touch the database and does not
// merge anything: it reads claims through a small interface and returns matches. The projector
// decides what to do with them, which keeps "what counts as the same entity" reviewable on its
// own, and keeps the rules testable without Postgres.
//
// Rules are either **certain** or **probable** (FR-037, ADR-0001 D6). Only certain rules may
// cause an automated merge; probable rules produce suggestions a human decides on, and land
// with US6. The registry is open — Register adds a rule — so that US6 adds P1..P3 beside C1..C3
// without editing this file.

// Namespaces the certain rules compare. These are identifier namespaces as used in Ref, not
// property names (fixtures/README.md "Ref namespaces").
const (
	// NamespaceOTelService is the OpenTelemetry `service.name` of a service.
	NamespaceOTelService = "otel.service.name"
	// NamespaceDatadogLogService is a Datadog log source's service name, with the environment as a
	// supporting attribute (005 FR-058). Not identifying on its own; C9 reads it.
	NamespaceDatadogLogService = "datadog.log_service"
	// NamespaceK8sDeployment is a Kubernetes Deployment as `<namespace>/<name>`.
	NamespaceK8sDeployment = "k8s.deployment"
)

// Claim attribute keys the certain rules read. OTel semantic conventions where one exists,
// the project's `sre.` namespace otherwise (FR-007).
const (
	// AttrK8sNamespace is the Kubernetes namespace a workload lives in.
	AttrK8sNamespace = "k8s.namespace.name"
	// AttrK8sCluster is the Kubernetes cluster, where a claim states one (C9).
	AttrK8sCluster = "k8s.cluster.name"
	// AttrK8sDeployment is the Kubernetes Deployment name carried on OTel resource attributes.
	AttrK8sDeployment = "k8s.deployment.name"
	// AttrServiceNamespace is the OpenTelemetry `service.namespace`.
	AttrServiceNamespace = "service.namespace"
	// AttrEnvironment is the deployment environment, which two facts must share before they
	// can be the same fact: `checkout` in staging is not `checkout` in production.
	AttrEnvironment = "deployment.environment.name"
	// AttrClaimKey names the Kubernetes label or annotation a claim was read from, so a rule
	// can tell an operator-declared service name from a coincidence.
	AttrClaimKey = "sre.k8s.claim_key"
)

// DefaultServiceNameClaimKeys are the Kubernetes label and annotation keys a workload may
// declare its OpenTelemetry service name in (research §10, C2). A deployment may configure
// others; these are the defaults every fixture uses.
var DefaultServiceNameClaimKeys = []string{
	"app.kubernetes.io/name",
	"resource.opentelemetry.io/service.name",
}

// Claim is one stored identity assertion, as graph.identity_claims holds it (FR-036).
type Claim struct {
	// ClaimID is the deterministic id of the claim: graph.ClaimID(namespace, value, source_id).
	ClaimID string
	// EntityID is the entity the claim currently points at, after merge redirects.
	EntityID string
	// EntityType is that entity's resolved node type, as graph.entities holds it. It is
	// denormalized onto the claim because the type guard the probable rules apply (research
	// §10) is a statement about entities, and a rule that had to fetch it would need a second
	// round trip per candidate.
	EntityType graph.NodeType
	// Namespace and Value are the identifier itself, e.g. ("otel.service.name", "checkout").
	Namespace string
	Value     string
	// Attributes are the supporting facts the source shipped with the claim.
	Attributes map[string]string
	// SourceID is the feeder that asserted it, and EventID the event that carried it.
	SourceID string
	EventID  string
	// ObservedAt is when the graph learned the claim.
	ObservedAt time.Time
	// AppendedSeq is the log position of the event that carried it, which is what decides
	// which of two merged entities survives (research §4).
	AppendedSeq int64
}

// Attr reads one attribute, returning "" when it is absent.
func (c Claim) Attr(key string) string { return c.Attributes[key] }

// ClaimStore is the read side a rule needs. The projector implements it over the transaction
// the event is being applied in, so a rule sees the claim it was triggered by.
// Correlation is one correlation key on one entity: a value SEVERAL entities may share (004 T148).
//
// A distinct type from Claim rather than an alias of it, and the duplication is the point. The two are
// stored under different rules — identity is unique per (namespace, value, source) and correlation is
// not — and a rule that took either would be a rule whose author has to remember which it was handed.
// The compiler remembers instead.
//
// The fields mirror Claim's because a rule needs the same things of both: who said it, when, on what
// entity, with what supporting attributes.
type Correlation struct {
	// CorrelationID is the deterministic id: graph.CorrelationID(entity, namespace, value, source).
	// It carries the entity, which is exactly what an identity claim's id may not.
	CorrelationID string
	// EntityID is the entity the key is on, after merge redirects.
	EntityID string
	// EntityType is that entity's resolved node type.
	EntityType graph.NodeType
	// Namespace and Value are the shared value, e.g. ("deploy.commit_sha", "8f5b…").
	Namespace string
	Value     string
	// Attributes are what the source shipped with it, and they are what a rule CORROBORATES with: a
	// correlation alone never merges anything.
	Attributes map[string]string
	// SourceID is the feeder that asserted it, and EventID the event that carried it.
	SourceID string
	EventID  string
	// ObservedAt is when the graph learned it.
	ObservedAt time.Time
	// AppendedSeq is the log position, which decides which of two merged entities survives.
	AppendedSeq int64
}

// Attr returns one supporting attribute, or "".
func (c Correlation) Attr(key string) string { return c.Attributes[key] }

type ClaimStore interface {
	// ClaimsFor returns every claim on one identifier, from every source.
	ClaimsFor(ctx context.Context, namespace, value string) ([]Claim, error)
	// ClaimsMatchingAttributes returns every claim in a namespace whose attributes contain
	// all of attrs. It is how a rule looks in the other direction: from a Kubernetes workload
	// to the telemetry service names that name it.
	ClaimsMatchingAttributes(ctx context.Context, namespace string, attrs map[string]string) ([]Claim, error)
	// ClaimsInNamespaces returns every claim recorded in any of the given identifier
	// namespaces. It is what the probable rules compare against: normalized-name equality is
	// not an indexable predicate, so the candidate set is the namespace rather than a lookup.
	ClaimsInNamespaces(ctx context.Context, namespaces []string) ([]Claim, error)
	// SharedOwner reports whether entities a and b both have an `owned_by` edge to one and the
	// same owner entity, which is P3's corroboration. It is a question about the graph rather
	// than about claims, which is why a rule cannot answer it alone.
	SharedOwner(ctx context.Context, a, b string) (bool, error)
	// ChangeTargets returns the entities a change was applied to — the sources of its
	// `changed_by` edges — after merge redirects, sorted.
	//
	// It is the second graph question a rule needs, and C8 needs it for the same reason P3 needs
	// SharedOwner: "the same target" is not a property of an identifier. GitHub calls a target
	// `acme/storefront` and Cloud Run calls it `proj/region/storefront`, so comparing the strings
	// two sources state would make C8 a rule that can never fire — the failure C7 shipped with.
	// What makes the two the same target is that resolution already merged them, and that is a
	// fact about the graph.
	//
	// A change usually has several targets: 003's revision-created change names the Cloud Run
	// service *and* the revision. So the comparison C8 makes is set intersection — the two changes
	// have a target in common — rather than a guess at which one is "the service".
	ChangeTargets(ctx context.Context, changeEntityID string) ([]string, error)
	// CorrelatedWith returns every entity carrying one correlation key, from every source.
	//
	// The correlation counterpart of ClaimsFor, and the difference between them is the whole of
	// 004 T148: ClaimsFor returns at most one entity per source because an identifier NAMES one thing,
	// and this returns as many as carry the value because a correlation key is a thing they share.
	CorrelatedWith(ctx context.Context, namespace, value string) ([]Correlation, error)
}

// Match is one rule firing on one ordered pair of distinct entities.
type Match struct {
	// RuleID is the published rule that fired, e.g. "C2".
	RuleID string
	// Certain says whether the rule may merge on its own (FR-037). A probable match is a
	// suggestion; nothing merges without a human (ADR-0001 D6).
	Certain bool
	// Score is the confidence, 1.0 for certain rules and a published constant otherwise.
	Score float64
	// Rationale is the human-readable explanation recorded with the decision (FR-038) and
	// returned by the resolution audit query (FR-031).
	Rationale string
	// EntityA and EntityB are the entities the rule says are the same. The order carries no
	// meaning: which one survives is decided by log order, not by the rule (research §4).
	EntityA string
	EntityB string
	// SupportingClaimIDs are the claims the rule stood on, in claim-id order (FR-038).
	SupportingClaimIDs []string
}

// PairKey is the order-independent key of the matched pair.
func (m Match) PairKey() string {
	if m.EntityA > m.EntityB {
		return m.EntityB + "|" + m.EntityA
	}
	return m.EntityA + "|" + m.EntityB
}

// Rule is one published match rule.
type Rule struct {
	// ID is the published identifier: C1..C3 are certain, P1..P3 probable.
	ID string
	// Certain reports whether a match may merge automatically (FR-037).
	Certain bool
	// Score is the published confidence a match from this rule carries: 1.0 for a certain
	// rule, a documented constant for a probable one (research §10).
	Score float64
	// Description is the published statement of what the rule compares and requires.
	Description string
	// Namespaces are the identifier namespaces the rule compares, for documentation and for
	// the `aisre rules` listing.
	Namespaces []string
	// Specificity orders evaluation: the most specific rule that fires on a pair is the one
	// recorded as the reason. C2 ("this workload declares this service name") explains a merge
	// better than C1 ("two sources used the same identifier"), even though both are true.
	Specificity int
	// Eval returns the matches this rule finds for a newly stored identity claim.
	//
	// Exactly one of Eval and EvalCorrelation is set. Register refuses a rule with both or neither,
	// because a rule's trigger is part of what it is: "runs when somebody names this thing" and "runs
	// when somebody says this thing shares a value" are different rules however similar the body.
	Eval func(ctx context.Context, store ClaimStore, claim Claim) ([]Match, error)
	// EvalCorrelation returns the matches this rule finds for a newly stored correlation key
	// (004 T148).
	EvalCorrelation func(ctx context.Context, store ClaimStore, correlation Correlation) ([]Match, error)
	// CrossKind declares a rule that pairs an identity claim with a correlation key, and so must run
	// from both: when the claim is stored and when the correlation is (005 C9). It is the one case in
	// which both evaluators are set, and it does not reintroduce the double decision the refusal
	// above exists for, because each evaluator matches only the OTHER kind — the claim side looks up
	// correlations and the correlation side looks up claims — so a pair is found once, by whichever of
	// its two events is stored second. Without it such a rule fires in one arrival order only, which
	// is a graph that depends on delivery order.
	CrossKind bool
}

var registry = []Rule{certainRuleC1, certainRuleC2, certainRuleC3}

// Register adds rules to the published registry. It panics on a duplicate id, because two
// rules under one name would make a recorded decision unreadable.
func Register(rules ...Rule) {
	for _, rule := range rules {
		if rule.ID == "" {
			panic("resolution: a rule needs an id")
		}
		if slices.ContainsFunc(registry, func(existing Rule) bool { return existing.ID == rule.ID }) {
			panic(fmt.Sprintf("resolution: rule %s is already registered", rule.ID))
		}
		// A rule's trigger is part of what it is (004 T148). A rule with neither evaluator is a rule
		// that never fires — the shape this project keeps finding published and unreachable — and one
		// with both would run twice on a pair and record two decisions for one reason.
		switch {
		case rule.Eval == nil && rule.EvalCorrelation == nil:
			panic(fmt.Sprintf("resolution: rule %s has no evaluator, so it would be published and "+
				"never fire", rule.ID))
		case rule.Eval != nil && rule.EvalCorrelation != nil && !rule.CrossKind:
			panic(fmt.Sprintf("resolution: rule %s has both evaluators; a rule triggered by a claim "+
				"and by a correlation would record two decisions for one reason", rule.ID))
		case rule.CrossKind && (rule.Eval == nil || rule.EvalCorrelation == nil):
			panic(fmt.Sprintf("resolution: rule %s is declared cross-kind with one evaluator; it would "+
				"fire in one arrival order only", rule.ID))
		}
		registry = append(registry, rule)
	}
}

// Rules returns every published rule in id order. This is the list the documentation and the
// CLI print, so it is sorted by name rather than by evaluation order.
func Rules() []Rule {
	out := slices.Clone(registry)
	slices.SortFunc(out, func(a, b Rule) int { return cmp.Compare(a.ID, b.ID) })
	return out
}

// CertainRules returns the rules that may merge automatically, in evaluation order: most
// specific first, ties broken by id.
func CertainRules() []Rule {
	out := make([]Rule, 0, len(registry))
	for _, rule := range registry {
		if rule.Certain {
			out = append(out, rule)
		}
	}
	slices.SortFunc(out, compareBySpecificity)
	return out
}

// compareBySpecificity orders rules for evaluation: most specific first, ties broken by id, so
// the rule recorded as the reason for a decision is the one that explains it best and the order
// is a pure function of the registry (FR-023).
func compareBySpecificity(a, b Rule) int {
	if c := cmp.Compare(b.Specificity, a.Specificity); c != 0 {
		return c
	}
	return cmp.Compare(a.ID, b.ID)
}

// Evaluate runs the published rules against a newly stored claim and returns at most one match
// per entity pair.
//
// Certain first, then probable, and a pair a certain rule already matched is never reported
// again as probable: the pair is going to be merged, and a suggestion to merge what is already
// merged is noise. Within each half the most specific rule that fired is the one recorded, so a
// merge is explained by "this workload declares this service name" rather than by "two sources
// used the same string" when both are true.
//
// Nothing here merges or suggests anything. The projector decides what to do with a match, and
// the split between certain and probable — Match.Certain — is the only thing it needs to know
// to honour ADR-0001 D6: a probable match may never cause an automated merge (SC-007).
func Evaluate(ctx context.Context, store ClaimStore, claim Claim) ([]Match, error) {
	return evaluate(ctx, store, func(rule Rule) evaluator {
		if rule.Eval == nil {
			return nil
		}
		return func(ctx context.Context, store ClaimStore) ([]Match, error) {
			return rule.Eval(ctx, store, claim)
		}
	})
}

// EvaluateCorrelation runs the rules triggered by a newly stored correlation key (004 T148).
//
// It is a separate entry point rather than a flag on Evaluate because the two are triggered by
// different events and read different tables. Sharing one function would mean every rule taking both
// kinds and ignoring one, which is how a rule ends up silently reading the wrong thing.
func EvaluateCorrelation(ctx context.Context, store ClaimStore, correlation Correlation) ([]Match, error) {
	return evaluate(ctx, store, func(rule Rule) evaluator {
		if rule.EvalCorrelation == nil {
			return nil
		}
		return func(ctx context.Context, store ClaimStore) ([]Match, error) {
			return rule.EvalCorrelation(ctx, store, correlation)
		}
	})
}

// evaluator is one rule's evaluation with its trigger already bound.
type evaluator func(ctx context.Context, store ClaimStore) ([]Match, error)

// evaluate runs the certain half then the probable half, skipping pairs the certain half already
// matched. Shared by both entry points so the ordering guarantee is stated once.
func evaluate(ctx context.Context, store ClaimStore, bind func(Rule) evaluator) ([]Match, error) {
	certain, seen, err := evaluateRules(ctx, store, CertainRules(), bind, map[string]bool{})
	if err != nil {
		return nil, err
	}
	probable, _, err := evaluateRules(ctx, store, ProbableRules(), bind, seen)
	if err != nil {
		return nil, err
	}
	return append(certain, probable...), nil
}

// evaluateRules runs one half of the registry, skipping pairs already matched, and returns the
// matches in pair-key order. A stable order matters: the decisions are recorded in this order
// and a replay must record them identically (FR-023).
func evaluateRules(ctx context.Context, store ClaimStore, rules []Rule, bind func(Rule) evaluator, seen map[string]bool) ([]Match, map[string]bool, error) {
	var matches []Match
	for _, rule := range rules {
		eval := bind(rule)
		if eval == nil {
			continue
		}
		found, err := eval(ctx, store)
		if err != nil {
			return nil, nil, fmt.Errorf("resolution: rule %s: %w", rule.ID, err)
		}
		for _, match := range found {
			if match.EntityA == match.EntityB || match.EntityA == "" || match.EntityB == "" {
				continue
			}
			key := match.PairKey()
			if seen[key] {
				continue
			}
			seen[key] = true
			matches = append(matches, match)
		}
	}
	slices.SortFunc(matches, func(a, b Match) int { return cmp.Compare(a.PairKey(), b.PairKey()) })
	return matches, seen, nil
}
