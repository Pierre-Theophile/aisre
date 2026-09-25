// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// Dependencies, derived and proposed (T134, T135; FR-028, FR-029; contract §4.1).
//
// # The line this file draws
//
// A `depends-on` edge is an assertion. Once it is in the graph, a blast radius traverses it, a diff
// ranks changes on the far side of it, and a person reading a hop-1 list believes it. So the bar for
// asserting one is the same bar the *certain* resolution rules meet: the edge is derived from
// something **somebody configured to be true**, not from something that looks likely.
//
// Two derivations meet it, and both are configuration in the literal sense:
//
//   - the revision **attaches** the instance. Cloud Run v2 carries a `cloud_sql_instance` volume
//     holding connection names, which is the structured form of `--add-cloudsql-instances`. An
//     operator wrote it down and Cloud Run acts on it: the proxy is mounted, the socket exists;
//   - an environment variable's **value is** a connection name. The operator put the instance's
//     dialling string into the service's configuration, which is the same assertion by another route.
//
// Everything else suggests. An instance whose *name* appears inside a variable *name* —
// `orders-primary` inside `ORDERS_PRIMARY_DSN` — is the case FR-029 exists for: strong enough to be
// worth a person's attention, nowhere near strong enough to assert. The variable may be dead
// configuration left behind by a migration, and an edge asserted on a name nobody can check is an
// edge nobody can remove with confidence either.
//
// # Why the value is read and not stored
//
// The second derivation reads an environment variable's value, which FR-034 says is never stored. Both
// hold. The feeder reads the value in memory, derives the edge, records **which variable** it derived
// from, and stores the variable's fingerprint and not its content. What reaches the graph is "the
// dependency was derived from the value of DATABASE_URL" — the fact, the evidence, and no value.
//
// # A proposal is raised once per cycle and is never an edge
//
// The durable half of "not re-raised while pending" is the projector's: applyProposeDependency files a
// proposal or declines to refile one, so a decision survives replay and outranks any later automated
// match. The feeder's half is narrower and still necessary: within one cycle, the same weak evidence
// may be visible on every service poll of every instance, and emitting it each time would produce a
// queue in which one suggestion appears fifty times.

// The published derivations. They are constants because a derivation reaches the graph as evidence,
// and evidence a reader cannot look up is evidence they cannot check.
const (
	// DerivationCloudSQLVolume is the revision attaching the instance through a `cloud_sql_instance`
	// volume — the structured form of `--add-cloudsql-instances`.
	DerivationCloudSQLVolume = "cloudsql_volume"
	// DerivationEnvVarValue is an environment variable whose value is the instance's connection name.
	DerivationEnvVarValue = "env_var_value"
)

// The published dependency-proposal rules. They are this feeder's, not `internal/resolution`'s: the
// resolution rules decide whether two identifiers name **one entity**, and these decide whether two
// **different** entities are related, which is a different question and a different event body.
const (
	// DependencyRuleEnvVarName is D1: an instance's name inside an environment-variable name.
	DependencyRuleEnvVarName = "D1"
	// ScoreD1 is D1's published confidence. It matches the identity rule P4's score deliberately —
	// the evidence is the same evidence, and two numbers for one observation would be two numbers a
	// reviewer has to reconcile.
	ScoreD1 = 0.6
)

// MinInstanceNameMatch is the shortest instance name D1 will match inside a variable name. Below it
// the rule fires on coincidence: `db` is inside `DB_HOST`, `DATABASE_URL` and almost every variable
// name in existence.
//
// It is spelled here as well as in `internal/resolution` — where P4 applies the same bound to the
// same evidence — and the two are asserted equal in this package's tests. A feeder proposing a
// dependency on evidence the identity rule would refuse would be holding the same observation to two
// standards.
const MinInstanceNameMatch = 4

// The dependency properties.
const (
	// PropDependencyDerivation names which published derivation asserted the edge.
	PropDependencyDerivation = "sre.gcp.dependency_derivation"
	// PropDependencyEvidence is the deriving evidence, as `field=what-it-showed` pairs (FR-028).
	PropDependencyEvidence = "sre.gcp.dependency_evidence"
	// PropDependencyConnectionName is the connection name the derivation turned on, which is the
	// one string a reviewer can check against a deployment.
	PropDependencyConnectionName = "sre.gcp.dependency_connection_name"
)

// DerivedDependency is a `depends-on` edge this feeder is willing to assert, with what derived it.
type DerivedDependency struct {
	// Service is the dependent and Instance the dependency: the edge reads service → instance.
	Service  Service
	Instance SQLInstance
	// Derivation is one of the published derivations.
	Derivation string
	// Evidence is what was read, as `field=what-it-showed` pairs, sorted. It carries the variable's
	// **name** and never its value (FR-034).
	Evidence []string
}

// Key identifies the edge, for de-duplication within a cycle.
func (d DerivedDependency) Key() string { return d.Service.Value() + "->" + d.Instance.Value() }

// EdgeFact renders the derived dependency as the edge it is.
//
// `validAt` is when the relationship became true, which is the creation instant of the revision the
// configuration was deployed with. It is required: a dependency edge with no start is an edge every
// as-of query finds, including the ones asking what the topology looked like before the service
// existed.
//
// The edge may name an instance the graph has not polled — one in another project, or one whose poll
// is later in the same cycle. That is correct and is what FR-036 covers: the endpoint is minted as a
// placeholder and fills in when the instance arrives. It is a real attachment either way; Cloud Run
// would have refused the deployment if the instance did not exist.
func (d DerivedDependency) EdgeFact(validAt time.Time) (feeder.EdgeFact, error) {
	if validAt.IsZero() {
		return feeder.EdgeFact{}, fmt.Errorf("gcp: a derived depends-on edge from %s to %s with no "+
			"instant; the instant is the creation time of the revision whose configuration derived it "+
			"(FR-028)", d.Service.Value(), d.Instance.Value())
	}
	if err := d.Service.Validate(); err != nil {
		return feeder.EdgeFact{}, err
	}
	if err := d.Instance.Validate(); err != nil {
		return feeder.EdgeFact{}, err
	}
	props, err := d.Props().Build()
	if err != nil {
		return feeder.EdgeFact{}, err
	}
	return feeder.EdgeFact{
		Src:     d.Service.Ref(),
		Dst:     d.Instance.Ref(),
		Type:    graphv1.EdgeType_DEPENDS_ON,
		Props:   props,
		ValidAt: validAt,
	}, nil
}

// Props renders the deriving evidence onto the edge (FR-028).
func (d DerivedDependency) Props() *feeder.Props {
	props := feeder.NewProps().
		Str(PropDependencyDerivation, d.Derivation).
		Str(PropDependencyConnectionName, d.Instance.ConnectionName())
	if len(d.Evidence) > 0 {
		props = props.Strs(PropDependencyEvidence, d.Evidence...)
	}
	return props
}

// DependencyProposal is suggestive-but-insufficient evidence of a dependency: a suggestion, never an
// edge (FR-029).
type DependencyProposal struct {
	Service  Service
	Instance SQLInstance
	RuleID   string
	Score    float64
	// Rationale is one line a reviewer can act on.
	Rationale string
	// Evidence is what the rule saw, as `field=what-it-showed` pairs, sorted.
	Evidence []string
}

// Key identifies the proposal, for the once-per-cycle rule.
func (p DependencyProposal) Key() string {
	return p.RuleID + ":" + p.Service.Value() + "->" + p.Instance.Value()
}

// Fact renders the proposal as the event body.
func (p DependencyProposal) Fact() (feeder.DependencyProposal, error) {
	if err := p.Service.Validate(); err != nil {
		return feeder.DependencyProposal{}, err
	}
	if err := p.Instance.Validate(); err != nil {
		return feeder.DependencyProposal{}, err
	}
	if p.Score <= 0 || p.Score > 1 {
		return feeder.DependencyProposal{}, fmt.Errorf("gcp: dependency proposal %s carries score %v, "+
			"which is not in (0,1]; a proposal is ranked for review by its score, so one outside the "+
			"range is unrankable rather than weak", p.Key(), p.Score)
	}
	evidence, err := feeder.NewProps().
		Strs(PropDependencyEvidence, p.Evidence...).
		Str(PropDependencyConnectionName, p.Instance.ConnectionName()).
		Str(PropProject, p.Instance.Project).
		Build()
	if err != nil {
		return feeder.DependencyProposal{}, err
	}
	return feeder.DependencyProposal{
		Src:       p.Service.Ref(),
		Dst:       p.Instance.Ref(),
		Type:      graphv1.EdgeType_DEPENDS_ON,
		Score:     p.Score,
		RuleID:    p.RuleID,
		Rationale: p.Rationale,
		Evidence:  evidence,
	}, nil
}

// Dependencies is what one service's configuration yielded.
type Dependencies struct {
	// Derived are the edges, and Proposed the suggestions.
	Derived  []DerivedDependency
	Proposed []DependencyProposal
}

// DeriveDependencies reads one service's configuration against the instances this feeder has observed
// (T134, T135).
//
// `known` is the instances observed so far. It is needed only for the **proposal** path: a derivation
// turns on a connection name, which parses into coordinates on its own, while a proposal has to
// compare a variable name against an instance's name and therefore has to know the instance exists.
// That asymmetry is why a service polled before any instance still gets its derived edges and gets no
// proposals until the instances arrive — which is right: a suggestion about an instance nobody has
// seen is a suggestion about nothing.
//
// An instance that was **derived** is never also proposed. A proposal for an edge that already exists
// is noise, and worse than noise: a reviewer confirming it would be confirming what the graph already
// asserts, which makes the proposal queue read as though the derivation had failed.
func DeriveDependencies(config ConfigObservation, known []SQLInstance) Dependencies {
	var out Dependencies
	derived := map[string]bool{}

	// Derivation 1: the revision attaches the instance.
	for _, connection := range config.CloudSQLInstances {
		inst, ok := parseConnectionName(connection)
		if !ok {
			// A connection name Cloud Run reported that this feeder cannot parse. It is not turned
			// into an edge under a guessed ref, for the reason every guessed ref is refused: it
			// resolves against something, and that something is not what anybody meant.
			continue
		}
		if derived[inst.Value()] {
			continue
		}
		derived[inst.Value()] = true
		out.Derived = append(out.Derived, DerivedDependency{
			Service:    config.Revision.Service,
			Instance:   inst,
			Derivation: DerivationCloudSQLVolume,
			Evidence: []string{
				"template.volumes[].cloudSqlInstance.instances=" + connection,
			},
		})
	}

	// Derivation 2: an environment variable's value IS a connection name.
	for _, name := range slices.Sorted(maps.Keys(config.EnvValuesNamingInstances)) {
		connection := config.EnvValuesNamingInstances[name]
		inst, ok := parseConnectionName(connection)
		if !ok || derived[inst.Value()] {
			continue
		}
		derived[inst.Value()] = true
		out.Derived = append(out.Derived, DerivedDependency{
			Service:    config.Revision.Service,
			Instance:   inst,
			Derivation: DerivationEnvVarValue,
			Evidence: []string{
				// The variable's NAME and the connection name it contained. Not the value: the value
				// is what was read, and reading is not recording (FR-034).
				"template.containers[].env[].name=" + name,
				"the value of that variable contained the instance connection name",
			},
		})
	}
	sort.Slice(out.Derived, func(i, j int) bool {
		return out.Derived[i].Instance.Value() < out.Derived[j].Instance.Value()
	})

	// D1: an instance's name inside a variable's name. A suggestion, and no edge.
	names := make([]string, 0, len(config.Env))
	for _, entry := range config.Env {
		names = append(names, entry.Name)
	}
	for _, inst := range known {
		if derived[inst.Value()] {
			continue
		}
		if inst.Project != config.Revision.Project {
			// Two projects are two environments until somebody says otherwise (FR-010). A service in
			// staging whose variable happens to name a production instance is not a suggestion, it is
			// a coincidence of naming conventions.
			continue
		}
		if len(inst.Name) < MinInstanceNameMatch {
			continue
		}
		variable, hit := variableNaming(names, inst.Name)
		if !hit {
			continue
		}
		out.Proposed = append(out.Proposed, DependencyProposal{
			Service:  config.Revision.Service,
			Instance: inst,
			RuleID:   DependencyRuleEnvVarName,
			Score:    ScoreD1,
			Rationale: fmt.Sprintf(
				"service %s defines an environment variable named %s, which contains the Cloud SQL "+
					"instance name %s; nothing in the service's configuration names the instance's "+
					"connection name, so the dependency cannot be derived and no edge is asserted "+
					"(FR-029)",
				config.Revision.Name, variable, inst.Name),
			Evidence: []string{
				"template.containers[].env[].name=" + variable,
				"instance=" + inst.Name,
				"connection_name_present_in_configuration=false",
			},
		})
	}
	sort.Slice(out.Proposed, func(i, j int) bool {
		return out.Proposed[i].Instance.Value() < out.Proposed[j].Instance.Value()
	})
	return out
}

// variableNaming reports whether any variable name contains the instance name, folding the separators
// and case that differ between a GCP resource name and an environment-variable name: `orders-primary`
// appears in `ORDERS_PRIMARY_DSN` as `ORDERS_PRIMARY`.
func variableNaming(names []string, instance string) (string, bool) {
	needle := foldSeparators(instance)
	if needle == "" {
		return "", false
	}
	for _, name := range names {
		if strings.Contains(foldSeparators(name), needle) {
			return name, true
		}
	}
	return "", false
}

// foldSeparators lower-cases and removes the separators. It is the same fold `internal/resolution`
// applies to the same comparison, and this package's tests assert the two agree on the cases that
// matter — a rule and a feeder that fold differently would disagree about which observations are
// even candidates.
func foldSeparators(value string) string {
	return strings.NewReplacer("-", "", "_", "", ".", "").Replace(strings.ToLower(value))
}
