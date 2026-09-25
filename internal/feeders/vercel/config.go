// SPDX-License-Identifier: Apache-2.0

package vercel

import (
	"sort"
	"strconv"
	"strings"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// Environment variables as CONFIG changes (004 T095–T101; FR-038, FR-039).
//
// # What the platform states, and what it does not
//
// Established from the published documentation for `GET /v10/projects/{idOrName}/env`, which is the one
// read on this connector's surface that sees configuration. Per variable it states: `key`, `target` (the
// environments), `type`, `id`, `createdAt`, `updatedAt`, `createdBy`, `updatedBy`, `system`.
//
// FR-038 asks for the key, the environments, the operation, "the version or revision identifier the
// platform assigns", the actor and the actor kind. Three of those need care, and one of them cannot be
// answered at all:
//
//   - **The operation.** The endpoint returns the CURRENT set of variables. So a creation and an update
//     are distinguishable — `updatedAt` equal to `createdAt` means nothing has edited it since — but a
//     **removal is not observable**, because a removed variable is simply absent. See the next section.
//   - **The version identifier.** The platform assigns none. `id` (`icfg_…`) identifies the VARIABLE,
//     not a revision of it, and there is no edit counter. So the property is named `sre.vercel.env_id`
//     and the change's own ref carries the operation instant to tell one edit from the next. Calling
//     `id` a version would be inventing a guarantee the platform does not give: two updates share it.
//   - **The actor kind.** `createdBy` and `updatedBy` are opaque user ids with nothing in the response
//     typing them. FR-013 is explicit that the account type must come from the payload and never from
//     the actor string, so the kind is `UNKNOWN` — an actor was named and could not be typed — rather
//     than `PERSON` guessed from the fact that variables are usually edited by hand. See
//     ConfigActorKind.
//
// # A removal is not observable, and a disappearance is not a removal
//
// Vercel DOES state removals, and states them properly: the audit log records
// `project.env_variable.created`, `.updated` and `.deleted` with a timestamp, `actor_vercel_id`,
// `actor_name` and the object's previous and next state. It is not reachable from here. The audit log
// has **no REST endpoint**: it is a CSV export a team owner downloads from the dashboard, or a push
// stream to a SIEM through Audit Log Drains, and it is available on **enterprise plans** to the
// **owner** role only. A connector that requires an enterprise plan and a dashboard download is not a
// connector, so US3 reads what the API states and says what it cannot.
//
// The tempting substitute is the one thing forbidden here: a variable present in one poll and absent
// from the next is NOT a removal. It is equally a scope change, an environment filter that now excludes
// it, a `gitBranch` query difference, a pagination boundary, or a token that lost a project. Reading
// absence as deletion would emit a retraction-shaped change nobody performed, which is the same mistake
// as retracting for an unread tail (FR-056) with a worse blast radius: "the database URL was deleted at
// 03:10" is an actionable, false fact. So no code path here produces a removal, and if one is wanted the
// answer is an audit-log drain reader, which is a new ingestion surface rather than a line of mapping.
//
// # No fingerprint, which is the difference from the GCP config feeder
//
// internal/feeders/gcp/config.go stores a **keyed HMAC fingerprint** of a configuration value, so that
// "did this change?" is answerable without holding what it is. Nothing of the kind happens here, and the
// difference is not an oversight in either place.
//
// GCP's revision spec returns plain environment values as part of the revision, so FR-033 asks for
// fingerprints and FR-034 bounds them. Vercel's are secrets by default — `type` is `encrypted`,
// `sensitive` or `secret` — and FR-038 refuses the value "not plaintext, not ciphertext, not truncated,
// **not hashed**", with SC-006 counting "hashes of values" among what must be zero over the corpus. A
// keyed fingerprint is a hash of a value, so the stricter clause wins and there is no fingerprint at all,
// keyed or otherwise.
//
// What that costs is worth stating plainly rather than leaving a reader to discover: this connector
// cannot answer "did the value actually change, or was the variable re-saved unchanged?". It answers
// what `updatedAt` states, which is that the platform recorded an edit. For an investigation asking what
// changed before an incident, "PAYMENTS_API_URL was edited at 03:10 by <actor>" is the fact that
// matters, and it is the fact FR-038 asks for.
//
// # Independent of deployments, which is the whole of FR-039
//
// A configuration change is emitted at the instant the platform states the variable changed, and nothing
// here looks at a deployment. There is no "attach this to the next rollout" path and no queue holding a
// config change until one appears — the change's valid instant is `updatedAt` (or `createdAt`), its
// target is the service the project maps to, and the two facts meet in the graph rather than in the
// feeder. A 03:10 config change and a 09:05 rollout are two changes with two valid times, which is what
// makes "what changed before 09:30" answer both.

// ConfigChange is one environment variable's mapped form, or a refusal with the reason.
type ConfigChange struct {
	// Change is the CONFIG_CHANGE event, or nil when this variable is not one.
	Change *graphv1.EventEnvelope
	// Excluded is the reason this variable produced no change, empty when it produced one. A value
	// rather than a log line because FR-032 and SC-002 ask for the exclusions to be COUNTED.
	Excluded string
}

// Exclusion reasons, a closed set so a caller can count them per cycle without parsing prose.
const (
	// ExcludedPreviewOnlyVariable is a variable scoped to no production environment: the same
	// platform-stated `target` filter the deployment mapper applies, applied to configuration
	// (FR-032, T101). `development` and `preview` are real environments and a change to one is a real
	// change — it is simply not a change to what serves production, which is what this corpus is
	// about.
	ExcludedPreviewOnlyVariable = "preview-only-variable"
	// ExcludedSystemVariable is a variable the PLATFORM assigns rather than an operator: `VERCEL_URL`
	// and its siblings. Their instants are the project's rather than anybody's decision, so reporting
	// them as configuration changes would put a row nobody performed into every investigation.
	ExcludedSystemVariable = "system-variable"
	// ExcludedUnusableVariable is a variable naming no key or no id, or stating no instant at all —
	// nothing can be said about when it changed, and FR-012 forbids filling that in with the poll.
	ExcludedUnusableVariable = "unusable-variable"
)

// The operations this connector can observe. There is deliberately no `removed`: see the file comment.
const (
	// ConfigOperationCreated is a variable the platform states has not been edited since it was made.
	ConfigOperationCreated = "created"
	// ConfigOperationUpdated is a variable whose `updatedAt` is later than its `createdAt`.
	ConfigOperationUpdated = "updated"
)

// MapProjectEnv turns one environment variable into a configuration change, or states why it is not one.
//
// `at` is the observation instant — when this reading arrived — and is never used as the change's valid
// time. The valid time comes from the platform's own `createdAt`/`updatedAt`, which is the difference
// between "the variable changed at 03:10" and "we noticed at 14:30".
func (m *Mapper) MapProjectEnv(projectID string, env ProjectEnv, at time.Time) (ConfigChange, error) {
	id, okID := feeder.StableIdentifier(env.ID)
	key := strings.TrimSpace(env.Key)
	project, okProject := feeder.StableIdentifier(projectID)
	if !okID || key == "" || !okProject {
		return ConfigChange{Excluded: ExcludedUnusableVariable}, nil
	}
	if env.System {
		return ConfigChange{Excluded: ExcludedSystemVariable}, nil
	}
	if !targetsProduction(env.Target) {
		return ConfigChange{Excluded: ExcludedPreviewOnlyVariable}, nil
	}

	operation, changedAt, ok := configOperation(env)
	if !ok {
		// No instant at all. The poll instant is available and is exactly what FR-012 forbids
		// substituting, so the variable is excluded and counted instead.
		return ConfigChange{Excluded: ExcludedUnusableVariable}, nil
	}

	props := feeder.NewProps().
		Str("sre.vercel.project_id", project).
		Str("sre.vercel.config_key", key).
		Str("sre.vercel.config_operation", operation).
		Strs("sre.vercel.config_environments", environmentsOf(env.Target)...).
		// The variable's identity, named for what it is. NOT a version: two edits share it, and
		// FR-038's "version or revision identifier the platform assigns" has no answer here.
		Str("sre.vercel.env_id", id)
	// The variable's type, which is how a reader tells a plain configuration value from a secret
	// WITHOUT either being stored. `encrypted`, `sensitive` and `secret` all mean "the platform treats
	// this as a credential"; recording which is not recording any part of the value.
	if variableType := strings.TrimSpace(env.Type); variableType != "" {
		props = props.Str("sre.vercel.config_type", variableType)
	}
	// Both instants, so that an update does not lose when the variable first appeared.
	if created, okCreated := millis(env.CreatedAt); okCreated {
		props = props.Str("sre.vercel.config_created_at", created.Format(time.RFC3339))
	}
	props = props.Strs("sre.vercel.config_actor_evidence", ConfigActorEvidence(env)...)
	built, err := props.Build()
	if err != nil {
		return ConfigChange{}, err
	}

	fact := feeder.ChangeFact{
		Meta: feeder.Meta{SourceObservedAt: at},
		// The ref carries the operation instant, so a second edit of one variable is a second change
		// rather than the same one re-stated. That is the opposite of the doorbell's rule against ids
		// carrying anything about the reading, and not in tension with it: `updatedAt` is the
		// platform's statement about the CHANGE, not about when we happened to look, so two polls of
		// one edit still agree on the ref and the log still answers DUPLICATE_NOOP.
		Ref: feeder.Ref(feeder.NSVercelChange,
			"projects/"+project+"/env/"+id+"/"+operation+"/"+strconv.FormatInt(changedAt.UnixMilli(), 10)),
		Kind:      graphv1.ChangeKind_CONFIG_CHANGE,
		Summary:   configSummary(operation, key, env.Target),
		Actor:     configActor(env, operation),
		ActorKind: ConfigActorKind(env),
		ValidAt:   changedAt,
		Props:     built,
	}
	fact.Targets = m.targets(project)

	change := feeder.ObserveChange(m.desc, m.id("config", project, id, operation,
		strconv.FormatInt(changedAt.UnixMilli(), 10)), fact)
	return ConfigChange{Change: change}, nil
}

// configOperation reads which operation the platform's two timestamps describe, and when.
//
// It compares two values the platform stated rather than inferring from the order things were seen,
// which is the distinction FR-016 draws for rollbacks and which holds here for the same reason: a
// comparison of `updatedAt` against `createdAt` is reading what Vercel said, whereas "this variable
// looks new because we had not seen it before" would be reading our own history.
//
// What it cannot do is count edits. A variable updated three times states one `updatedAt`, so the second
// and third updates are not separate observable changes — the graph holds the latest, and its ref
// carries that instant.
func configOperation(env ProjectEnv) (operation string, changedAt time.Time, ok bool) {
	created, okCreated := millis(env.CreatedAt)
	updated, okUpdated := millis(env.UpdatedAt)
	switch {
	case okUpdated && okCreated && updated.After(created):
		return ConfigOperationUpdated, updated, true
	case okCreated:
		return ConfigOperationCreated, created, true
	case okUpdated:
		// An update with no stated creation. The operation is still an update — the platform says it
		// was edited — and the creation instant is simply not known.
		return ConfigOperationUpdated, updated, true
	default:
		return "", time.Time{}, false
	}
}

// ConfigActorKind is the actor kind for a configuration change, which is always `UNKNOWN`.
//
// Not `UNSPECIFIED`, and the difference is the point. `ACTOR_KIND_UNSPECIFIED` means the source said
// nothing about an actor; `ACTOR_KIND_UNKNOWN` means "an actor was named and the feeder could not type
// it". Vercel names one — `createdBy`/`updatedBy` — and types it nowhere, so `UNKNOWN` is the true
// answer and `UNSPECIFIED` would understate what we know.
//
// It is a function rather than a constant for the same reason `promotedAt` is: the shape of the answer
// is the contract. If the response ever carries an actor type, this is the one place that changes, and
// every caller already handles a kind it did not choose.
func ConfigActorKind(env ProjectEnv) graphv1.ActorKind {
	if configActor(env, "") == "" {
		return graphv1.ActorKind_ACTOR_KIND_UNSPECIFIED
	}
	return graphv1.ActorKind_ACTOR_KIND_UNKNOWN
}

// ConfigActorEvidence is what the payload stated about the actor, so a reader can see the kind was
// derived from fields rather than from the actor string (FR-013).
func ConfigActorEvidence(env ProjectEnv) []string {
	evidence := []string{
		"created_by=" + presence(env.CreatedBy),
		"updated_by=" + presence(env.UpdatedBy),
		// The absence that decides the kind, stated rather than left to be inferred from the other two.
		"actor_type=absent",
	}
	sort.Strings(evidence)
	return evidence
}

// configActor is whoever the platform credits with the operation: the editor for an update, the creator
// for a creation. An empty operation asks "is there an actor at all", which is what ConfigActorKind
// needs.
func configActor(env ProjectEnv, operation string) string {
	updated := strings.TrimSpace(env.UpdatedBy)
	created := strings.TrimSpace(env.CreatedBy)
	if operation == ConfigOperationCreated {
		return created
	}
	if updated != "" {
		return updated
	}
	return created
}

// presence reports whether a field was stated, without repeating what it said. Used for actor evidence:
// the id itself is a people identifier, and the sanitisation policy drops those rather than hashing
// them, so the evidence line records that the field was there.
func presence(value string) string {
	if strings.TrimSpace(value) == "" {
		return "absent"
	}
	return "present"
}

// targetsProduction reports whether the platform scopes this variable to production.
//
// The comparison is against the platform's own `target` values, which is the same filter the deployment
// mapper applies (FR-032) — an environment named by Vercel rather than a guess from the variable's key.
func targetsProduction(targets []string) bool {
	for _, target := range targets {
		if strings.EqualFold(strings.TrimSpace(target), "production") {
			return true
		}
	}
	return false
}

// environmentsOf normalises the environments a variable applies to, sorted so a golden reads the same
// twice whatever order the platform listed them in.
func environmentsOf(targets []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(targets))
	for _, target := range targets {
		value := strings.ToLower(strings.TrimSpace(target))
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

// configSummary is the one line an investigation reads. It names the key and never the value — the key
// is what makes the change actionable, and a summary is the easiest place for a value to leak into.
func configSummary(operation, key string, targets []string) string {
	environments := environmentsOf(targets)
	return operation + " environment variable " + key + " for " + strings.Join(environments, ", ")
}

// CountConfigExclusions tallies a cycle's configuration refusals by reason, the same way
// CountExclusions does for rollouts, because FR-032 and SC-002 ask for exclusions to be counted rather
// than logged. Sorted, so a report reads the same twice.
func CountConfigExclusions(changes []ConfigChange) []ExclusionCount {
	counts := map[string]int{}
	for _, change := range changes {
		if change.Excluded == "" {
			continue
		}
		counts[change.Excluded]++
	}
	out := make([]ExclusionCount, 0, len(counts))
	for reason, count := range counts {
		out = append(out, ExclusionCount{Reason: reason, Count: count})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Reason < out[j].Reason })
	return out
}
