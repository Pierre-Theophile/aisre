// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
)

// The published join-key mapping (T095, FR-102; data-model.md §9, contract §9).
//
// | FR-102 key            | JoinKeys field | GCP value                                          |
// |-----------------------|----------------|----------------------------------------------------|
// | the Cloud Run revision| `version`      | the revision name, matching the WORKLOAD node       |
// | the instance          | `pod_or_host`  | the Cloud SQL instance connection name              |
// | the trace identifier  | `trace_ids`    | empty here; this organisation has no trace source   |
// | the first-seen instant| `first_seen`   | per template, error kind or series                  |
//
// with the Cloud Run **service** in `workload`.
//
// These are **pseudonymised, never dropped** (FR-102, FR-135), and that is the one place where the
// keyed HMAC's corpus-consistency is load-bearing rather than convenient: a digest whose keys do
// not join to the graph is evidence about nothing. The pseudonymisation itself happens at the
// digest boundary — SanitiseThenRedact — rather than here, so that one table decides every
// disposition; what this file guarantees is that the keys are *present* to be pseudonymised.
//
// `trace_ids` is empty and that emptiness is a fact about the estate rather than an omission: the
// trace vocabulary is registered and unminted (pointer-vocabularies.md §4), and `error_spans`
// answers NO_DATA naming the absent source. Populating it would take a contract change in neither
// direction — the field is here, waiting.

// joinKeys builds the keys for an answer about one revision of one service.
//
// firstSeen is per row rather than per digest: the first-seen instant of a log template is when
// that template first appeared, and the first-seen instant of a series is where its interval
// starts. A zero instant is left unset rather than stamped with an epoch, because a join key that
// says 1970 is a join key that will match something.
func joinKeys(facts selectorFacts, revision string, firstSeen time.Time) *investigationv1.JoinKeys {
	version := revision
	if version == "" {
		version = facts.Revision
	}
	keys := &investigationv1.JoinKeys{
		Version:  version,
		Workload: facts.Service,
	}
	if !firstSeen.IsZero() {
		keys.FirstSeen = timestamppb.New(firstSeen.UTC())
	}
	return keys
}

// joinKeysWithInstance is joinKeys plus the instance identifier, which is what a Cloud SQL series
// carries in place of a pod: the instance connection name is the series' "host".
func joinKeysWithInstance(facts selectorFacts, revision, instance string, firstSeen time.Time) *investigationv1.JoinKeys {
	keys := joinKeys(facts, revision, firstSeen)
	keys.PodOrHost = instance
	return keys
}

// The Cloud Monitoring resource and metric labels this backend reads a join key out of. They are
// named rather than inlined because the mapping above is published: a label renamed upstream
// should be a change in one place with the contract beside it.
const (
	// LabelRevisionName is the `cloud_run_revision` label that carries the revision, and the
	// reason contract §3.1 insists every selector pins `resource.type`: the same metric is
	// written against `cloud_run_instance`, which has no such label.
	LabelRevisionName = "revision_name"
	// LabelServiceName is the Cloud Run service.
	LabelServiceName = "service_name"
	// LabelInstanceID is the Cloud Run instance, where a series carries one.
	LabelInstanceID = "instance_id"
	// LabelDatabaseID is the Cloud SQL instance connection name, `project:region:instance`.
	LabelDatabaseID = "database_id"
	// LabelResponseCodeClass is the `2xx` / `4xx` / `5xx` split the error rate is derived from.
	LabelResponseCodeClass = "response_code_class"
)

// GroupByRevision is the Monitoring **filter field** the Cloud Run grouping is written against —
// the `resource.labels.` spelling, as against LabelRevisionName which is how a *returned* series'
// label map is keyed. Both are needed and they are not interchangeable: one goes into a query, the
// other reads a result.
//
// It is the one field `errors_by_version` groups by, the value it reports as the digest's
// `version_attribute`, and the value the feeder mints as `Pointer.join_keys["version"]`. That last
// identity is asserted by a test rather than shared through a variable, so that each package keeps
// its own grammar and a rename on either side fails loudly instead of quietly splitting a metric by
// a field that does not exist.
const GroupByRevision = "resource.labels." + LabelRevisionName

// keysFromLabels builds join keys from a returned series' own labels, falling back to what the
// selector pinned. A grouped query returns the group's value in the resource labels, so this is
// where "the new revision is erroring and the old one is not" becomes a key a reader can join
// rather than a string in a tag map.
func keysFromLabels(facts selectorFacts, resource, metric map[string]string, firstSeen time.Time) *investigationv1.JoinKeys {
	keys := joinKeys(facts, resource[LabelRevisionName], firstSeen)
	if keys.GetWorkload() == "" {
		keys.Workload = resource[LabelServiceName]
	}
	switch {
	case resource[LabelInstanceID] != "":
		keys.PodOrHost = resource[LabelInstanceID]
	case resource[LabelDatabaseID] != "":
		keys.PodOrHost = resource[LabelDatabaseID]
	}
	// metric labels carry no join role of their own — response_code_class is a grouping, not an
	// identity — so they are read for the tag map and deliberately not for a key.
	_ = metric
	return keys
}
