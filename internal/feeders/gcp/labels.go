// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"sort"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// Labels (T049, FR-124, FR-125, FR-126, config/gcp.yaml).
//
// A GCP label namespace is open: anyone with deploy access invents a key, and this organisation's
// labels carry release names, ticket numbers and people's names beside the two or three keys that
// are actually structural. So the allowlist is the whole design, and the rule is stronger than
// "drop the value":
//
// **A key not on the allowlist becomes nothing — not a property, not a node, not a claim.**
//
// The three are named separately because dropping one and keeping another is the plausible
// half-measure. A dropped *property* with a kept *claim* leaves the resolution layer merging on an
// identifier nobody reviewed; a dropped claim with a kept property leaves the value in the graph
// for a reader to copy into a ticket. The unlisted key is simply not observed.
//
// # A label value that measures something never becomes a property
//
// FR-126, and it is the rule that is easiest to lose. `replicas=8`, `cpu=2`, `p99-target=250ms`
// are *measurements*: they change without a deploy, they are what a telemetry backend answers
// about, and a graph property holding one is a stale number that looks authoritative
// (constitution IV — telemetry stays in its backend). The graph records structure. So a label
// whose value parses as a bare number, a number with a unit, or a percentage is dropped with its
// reason recorded, however allowlisted its key.
//
// The asymmetry is deliberate: a structural label misread as a measurement costs one property, and
// a measurement misread as structure puts a number in the graph that nobody will ever refresh.

// LabelPolicy is the label configuration in force, from `config/gcp.yaml`. It is a value rather
// than a package-level default so that the mapping in force can be recorded in the checkpoint —
// "which published mapping produced this node's environment" is a question a later reader asks.
type LabelPolicy struct {
	// Allowlist is the keys that may be observed at all. A key not here becomes nothing.
	Allowlist []string
	// EnvironmentFromLabel is the label key consulted first for a resource's environment.
	EnvironmentFromLabel string
	// EnvironmentFromProject maps a project id to an environment, consulted when the resource
	// carries no environment label.
	EnvironmentFromProject map[string]string
	// EnvironmentDefault is what an unmapped project gets. It is explicit and it is `unknown`:
	// never a guess, and never "production" by default, because a wrong environment is how two
	// entities in different environments get merged into one.
	EnvironmentDefault string
	// OwnerLabel is the key whose value names the owning team, which becomes an OWNER node with
	// an `owned-by` edge (FR-124).
	OwnerLabel string
	// CommitFromLabels are the label keys a commit identifier is read from, in order of
	// preference. The first one holding a value that is a full hex commit object id wins.
	//
	// It is **configuration and not a constant**, because the Cloud Run v2 API states no commit
	// anywhere. Checked against the vendored surface (cloud.google.com/go/run v1.22.0): `Revision`
	// has no commit field and no resolved-digest field, and `Service.BuildConfig` is the
	// functions source-deploy path whose `SourceLocation` is a Cloud Storage bucket URI. The only
	// place a commit can appear is a label or annotation whatever deployed the revision chose to
	// set — a convention of the operator's tooling, not a fact the platform publishes. So the keys
	// are declared, the defaults below are named as conventions rather than as the answer, and a
	// value that is not a full commit id is ignored rather than claimed (004 T135, FR-041).
	CommitFromLabels []string
}

// The published defaults, matching `config/gcp.yaml`. A deployment overrides them in config; these
// are what every fixture uses.
const (
	// EnvironmentUnknown is what an unmapped project gets. It is a first-class answer.
	EnvironmentUnknown = "unknown"
	// DefaultEnvironmentLabel is the label key consulted first.
	DefaultEnvironmentLabel = "environment"
	// DefaultOwnerLabel is the label whose value names the owning team.
	DefaultOwnerLabel = "team"
)

// DefaultCommitLabels is the key a commit is read from unless an operator names their own.
//
// It is a **convention of deploy tooling, not an API field** — see LabelPolicy.CommitFromLabels — so
// it is a default rather than the answer, and the cost of it being wrong for an organisation is
// bounded by the value check: a label under this key whose value is not a full hex commit object id
// mints no claim at all.
//
// One key rather than the several spellings deploy tools use, because a key here has to be on the
// label allowlist to be read at all (see Apply), and the allowlist is deliberately short — every
// entry is a value that reaches a golden and a grader. An organisation spelling it differently sets
// both lists, which the ordered slice exists for.
var DefaultCommitLabels = []string{"commit-sha"}

// DefaultLabelAllowlist is `config/gcp.yaml`'s allowlist. Six keys, and the list is short on
// purpose: every addition is a value that reaches a golden and a grader.
var DefaultLabelAllowlist = []string{
	"environment", "service", "component", "team", "managed-by", "version",
	// `commit-sha` is the seventh, and it is here rather than read around the allowlist because the
	// allowlist is a disposal boundary as well as an observation one: a key not on it is dropped
	// before anything touches disk (FR-137). Reading a commit outside it would make a claim out of a
	// label the operator chose not to observe. It is on the list because the commit is the only
	// identifier a GitHub rollout and a Cloud Run rollout can share (004 T135).
	"commit-sha",
}

// DefaultLabelPolicy returns the published policy. `EnvironmentFromProject` is empty, which is not
// an oversight: `config/gcp.yaml` ships it empty because no code and no checked-in default may
// assume a project name (FR-131).
func DefaultLabelPolicy() LabelPolicy {
	return LabelPolicy{
		Allowlist:              append([]string(nil), DefaultLabelAllowlist...),
		EnvironmentFromLabel:   DefaultEnvironmentLabel,
		EnvironmentFromProject: map[string]string{},
		EnvironmentDefault:     EnvironmentUnknown,
		OwnerLabel:             DefaultOwnerLabel,
		CommitFromLabels:       append([]string(nil), DefaultCommitLabels...),
	}
}

// Allows reports whether key is on the allowlist. Comparison is on the key as GCP stores it:
// lower case, and GCP itself rejects anything else, so no normalisation is invented here.
func (p LabelPolicy) Allows(key string) bool {
	for _, allowed := range p.Allowlist {
		if allowed == key {
			return true
		}
	}
	return false
}

// PropOwnerValidFromIsABound marks an OWNER node's valid start as a bound rather than the instant the
// ownership began (FR-125). It is a property rather than a flag because `valid_from_unknown` cannot
// carry it: an `owned-by` edge and both of its endpoints must agree on that flag, and the owned Cloud
// Run service has a documented creation instant. See emitOwner in map.go.
const PropOwnerValidFromIsABound = "sre.gcp.owner_valid_from_is_a_bound"

// OwnerValidFromBoundReason is what that property says.
const OwnerValidFromBoundReason = "the earliest instant the labelled state of the owned entity is " +
	"known to have held; a label carries no history, so the instant the team came to own it is not a " +
	"fact this feeder has (FR-125)"

// LabelOutcome is what happened to one label, for the checkpoint and for a reviewer. A dropped
// label is recorded as dropped-with-a-reason rather than silently absent, because "this key is not
// on the allowlist" and "nobody set this label" are different facts about the estate.
type LabelOutcome struct {
	Key string
	// Kept is the value that became a property, empty when the label was dropped.
	Kept string
	// Dropped says why, empty when the label was kept.
	Dropped string
}

// Labels is the result of applying the policy to one resource's labels.
type Labels struct {
	// Props are the allowlisted, non-measurement labels, as properties.
	Props map[string]string
	// Environment is the derived environment: the label where the resource carries one, else the
	// project mapping, else the default.
	Environment string
	// EnvironmentSource names which of the three produced it, so a reader can tell a declared
	// environment from a mapped one from the default.
	EnvironmentSource string
	// Owner is the owning team from the owner label, empty when there is none. An empty owner
	// means no OWNER node and no `owned-by` edge — not an owner called "unknown", because an
	// OWNER node nobody owns is a node somebody will try to page.
	Owner string
	// Commit is the commit identifier a label declared, empty when none did or when the value was
	// not a full hex commit object id. It is what feature 004's `deploy.commit_sha` claim is minted
	// from, and it is the only join a GitHub rollout and a Cloud Run rollout can share.
	Commit string
	// CommitSource is the label key Commit came from, so a reader can tell which convention was in
	// force rather than having to guess from the policy.
	CommitSource string
	// Outcomes is every label seen and what happened to it, sorted by key.
	Outcomes []LabelOutcome
}

// The published environment sources.
const (
	// EnvironmentFromLabel means the resource declared it.
	EnvironmentFromLabel = "label"
	// EnvironmentFromProject means the published project mapping supplied it.
	EnvironmentFromProject = "project_mapping"
	// EnvironmentFromDefault means neither did, and it is `unknown`.
	EnvironmentFromDefault = "default"
)

// Apply runs the policy over one resource's labels.
func (p LabelPolicy) Apply(project string, labels map[string]string) Labels {
	out := Labels{
		Props:             map[string]string{},
		Environment:       p.EnvironmentDefault,
		EnvironmentSource: EnvironmentFromDefault,
	}
	if out.Environment == "" {
		out.Environment = EnvironmentUnknown
	}

	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		value := labels[key]
		switch {
		case !p.Allows(key):
			out.Outcomes = append(out.Outcomes, LabelOutcome{
				Key:     key,
				Dropped: "not on the label allowlist: an unlisted key becomes nothing — not a property, a node or a claim (FR-124)",
			})
			continue
		case IsMeasurement(value):
			out.Outcomes = append(out.Outcomes, LabelOutcome{
				Key:     key,
				Dropped: "the value measures something; a measurement belongs in a telemetry backend, not in a graph property that nobody will refresh (FR-126)",
			})
			continue
		}
		out.Props[key] = value
		out.Outcomes = append(out.Outcomes, LabelOutcome{Key: key, Kept: value})
	}

	// Environment: the label the resource carries wins, then the published project mapping, then
	// the default. The label is consulted from the *kept* properties, so an environment label
	// holding a measurement does not become an environment.
	if p.EnvironmentFromLabel != "" {
		if value, ok := out.Props[p.EnvironmentFromLabel]; ok && value != "" {
			out.Environment, out.EnvironmentSource = value, EnvironmentFromLabel
		}
	}
	if out.EnvironmentSource == EnvironmentFromDefault {
		if value, ok := p.EnvironmentFromProject[project]; ok && value != "" {
			out.Environment, out.EnvironmentSource = value, EnvironmentFromProject
		}
	}

	if p.OwnerLabel != "" {
		out.Owner = out.Props[p.OwnerLabel]
	}

	// The commit, read from the *kept* properties for the same reason the environment is: a key the
	// allowlist dropped becomes nothing — not a property, a node or a claim (FR-124) — so reading it
	// here would make a claim out of a label the operator chose not to observe.
	//
	// A value that is not a full commit id is passed over rather than claimed, and the loop continues
	// to the next key: an organisation whose `commit-sha` label holds an abbreviation and whose
	// `git-commit` holds the full id gets the full one. An abbreviation is never padded — a 7-hex
	// prefix is shared by many commits, and a certain rule keyed on it would merge rollouts of
	// different code (FR-041).
	for _, key := range p.CommitFromLabels {
		commit, ok := feeder.CommitSHA(out.Props[key])
		if !ok {
			continue
		}
		out.Commit, out.CommitSource = commit, key
		break
	}
	return out
}

// IsMeasurement reports whether a label value measures something rather than naming it (FR-126). The
// rule is pkg/feeder's, shared by every connector that reads tags or labels.
func IsMeasurement(value string) bool { return feeder.IsMeasurement(value) }

// Dropped returns the labels that became nothing, for the checkpoint. The reason travels with the
// key so that "why is my label not in the graph" has an answer a reader can find without reading
// this file.
func (l Labels) Dropped() []LabelOutcome {
	var out []LabelOutcome
	for _, outcome := range l.Outcomes {
		if outcome.Dropped != "" {
			out = append(out, outcome)
		}
	}
	return out
}
