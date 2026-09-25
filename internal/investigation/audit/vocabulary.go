// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"fmt"
	"slices"
)

// The audit's closed vocabularies (FR-069, FR-070).
//
// Every field of the input list is drawn from one of the sets below. There is deliberately no
// free-text field anywhere in the format: the private audit this format exists to carry is a
// transcription of real production incidents, and a "notes" column is how a title, a customer
// name or a stack trace ends up in a public repository. What the audit needs in order to
// compute a ceiling is a category, a class and a verdict per feeder set — nothing narrative.
//
// The category set is the eighteen values `investigation.coverage_audit_items.category` accepts
// (migration 0006 as widened by 0007). The two lists are checked against each other by the
// migration test and by TestCategoriesMatchTheMigration here: a value added to one and not the
// other is a bug, not a tuning decision.
//
// The set was twelve until 2026-09-17. FR-070's original list was the causes the feature-001
// feeders miss, and it had no member at all for the most ordinary change-induced causes: a
// deployment, a configuration change, a provider maintenance window, a vendor API change, a
// capacity limit. Transcribing the first real audit put six of thirteen incidents under `other`,
// which is a category breakdown that ranks no feeder and a remainder that names no fixture. The
// six members below close that, additively: nothing was renamed and nothing was removed, so a
// row written under the twelve is still a row that reads.

// Category is a stated cause's category, from the published eighteen-value set.
type Category string

// The published category set (FR-070; `coverage_audit_items_category_check`, migration 0006 as
// widened by 0007). The order is the migration's order and is the order every report prints
// them in: the six values added in 0007 follow the original eleven, and `other` stays last so
// that a report lists every named cause before the catch-all.
const (
	// CategoryFlagFlip is a feature flag turned on or off.
	CategoryFlagFlip Category = "flag_flip"
	// CategoryIaCApply is an infrastructure-as-code apply.
	CategoryIaCApply Category = "iac_apply"
	// CategoryDBMigration is a database migration.
	CategoryDBMigration Category = "db_migration"
	// CategoryCertificateExpiry is a certificate or key that expired.
	CategoryCertificateExpiry Category = "certificate_expiry"
	// CategoryScheduledJob is a cron or scheduled job.
	CategoryScheduledJob Category = "scheduled_job"
	// CategoryThirdPartyOutage is a vendor or upstream SaaS outage. An unplanned provider
	// network or hardware incident is CategoryInfrastructureIncident, because the two are read
	// from different feeders.
	CategoryThirdPartyOutage Category = "third_party_outage"
	// CategoryTrafficShift is a change in traffic shape or volume.
	CategoryTrafficShift Category = "traffic_shift"
	// CategoryLatentBug is a defect shipped earlier and triggered later.
	CategoryLatentBug Category = "latent_bug"
	// CategoryClientSideConfiguration is configuration held outside the system under audit.
	CategoryClientSideConfiguration Category = "client_side_configuration"
	// CategoryBusinessDataChange is a data change made through the product itself.
	CategoryBusinessDataChange Category = "business_data_change"
	// CategoryCredentialLeak is a leaked or revoked credential.
	CategoryCredentialLeak Category = "credential_leak"

	// --- added by migration 0007 (2026-09-17), all of them change-induced causes the
	// original twelve could only spell `other` ---

	// CategoryDeployment is a release of application code to an environment: a container or
	// serverless revision, a frontend deploy, a rollback. It is what the deploy feeders exist
	// to carry, and one of the causes the original twelve could only spell `other`.
	CategoryDeployment Category = "deployment"
	// CategoryConfigurationChange is a change to configuration the audited system itself owns,
	// applied without a code release: an environment variable, a runtime setting, a routing or
	// autoscaling rule. Distinct from CategoryClientSideConfiguration, which is configuration
	// held outside the system and therefore outside every feeder.
	CategoryConfigurationChange Category = "configuration_change"
	// CategoryCloudMaintenance is planned or announced work by a cloud or infrastructure
	// provider: a node-pool upgrade, a managed-database failover window, a region maintenance.
	// It is observable, but only to a provider maintenance-notice feeder.
	CategoryCloudMaintenance Category = "cloud_maintenance"
	// CategoryVendorAPIChange is a vendor changing or deprecating an API, a schema, a default
	// or a quota policy. Missed maintenance and deprecation notices were the single most
	// repeated cause of the first audit; this one is carried by an API-changelog feeder rather
	// than by a status page.
	CategoryVendorAPIChange Category = "vendor_api_change"
	// CategoryCapacityLimit is a quota, rate limit, pool or capacity ceiling reached or moved:
	// a vendor quota lowered, a connection pool exhausted, a limit raised somewhere that moved
	// load somewhere else.
	CategoryCapacityLimit Category = "capacity_limit"
	// CategoryInfrastructureIncident is an unplanned provider network or hardware incident: a
	// zone network partition, a host failure, a managed-service degradation. It is kept apart
	// from CategoryThirdPartyOutage because the two rank different feeders: this one is read
	// from the cloud provider's health feed, a SaaS outage from that vendor's status page.
	CategoryInfrastructureIncident Category = "infrastructure_incident"

	// CategoryOther is a stated cause that fits none of the above. After 0007 it should be
	// rare: a cause filed here is a prompt to ask whether the vocabulary is missing a member,
	// not a resting place.
	CategoryOther Category = "other"
)

// categoryOrder is the published order, matching the CHECK constraint of migration 0007 --
// the latest migration that defines `coverage_audit_items_category_check` -- exactly.
var categoryOrder = []Category{
	CategoryFlagFlip,
	CategoryIaCApply,
	CategoryDBMigration,
	CategoryCertificateExpiry,
	CategoryScheduledJob,
	CategoryThirdPartyOutage,
	CategoryTrafficShift,
	CategoryLatentBug,
	CategoryClientSideConfiguration,
	CategoryBusinessDataChange,
	CategoryCredentialLeak,
	CategoryDeployment,
	CategoryConfigurationChange,
	CategoryCloudMaintenance,
	CategoryVendorAPIChange,
	CategoryCapacityLimit,
	CategoryInfrastructureIncident,
	CategoryOther,
}

// Categories returns the published category set in its published order.
func Categories() []Category { return slices.Clone(categoryOrder) }

// Valid reports whether c is one of the eighteen published categories.
func (c Category) Valid() bool { return slices.Contains(categoryOrder, c) }

// rank is the category's index in the published order, used to sort report rows.
func (c Category) rank() int { return slices.Index(categoryOrder, c) }

// CauseClass says what kind of thing the cause was, which decides what a corpus fixture for it
// has to assert (FR-071b).
type CauseClass string

const (
	// CauseChangeInduced means a change caused the incident. Whether the graph could see that
	// change is what the per-feeder-set verdict answers.
	CauseChangeInduced CauseClass = "change_induced"
	// CauseNotChangeInduced means no change caused it — the correct engine answer on such an
	// incident is `not_change_induced` with the category.
	CauseNotChangeInduced CauseClass = "not_change_induced"
	// CauseUnobserved means a change or an act caused it but is unobservable to any feeder the
	// audit contemplates — the correct engine answer is `unobserved`.
	CauseUnobserved CauseClass = "unobserved"
)

var causeClassOrder = []CauseClass{CauseChangeInduced, CauseNotChangeInduced, CauseUnobserved}

// CauseClasses returns the published cause classes.
func CauseClasses() []CauseClass { return slices.Clone(causeClassOrder) }

// Valid reports whether c is a published cause class.
func (c CauseClass) Valid() bool { return slices.Contains(causeClassOrder, c) }

// NeedsFixture reports whether a cause of this class owes the evaluation corpus a fixture
// carrying its category as ground truth (FR-071b).
func (c CauseClass) NeedsFixture() bool {
	return c == CauseNotChangeInduced || c == CauseUnobserved
}

// Verdict is the observability verdict for one incident under one feeder set: could the graph,
// running that feeder set, have held the cause as a node or a change at the alert instant?
type Verdict string

const (
	// VerdictObserved means the cause itself would have been in the graph at alert time.
	VerdictObserved Verdict = "observed"
	// VerdictSymptomOnly means the effect was visible but the cause was not — counted apart
	// from the ceiling, because a symptom does not localise a cause.
	VerdictSymptomOnly Verdict = "symptom_only"
	// VerdictNotObservable means no feeder in the set could have carried the cause.
	VerdictNotObservable Verdict = "not_observable"
	// VerdictUndecidable means the auditor could not decide, and says why. It leaves the
	// ceiling's denominator rather than being counted as a miss.
	VerdictUndecidable Verdict = "undecidable"
)

var verdictOrder = []Verdict{
	VerdictObserved, VerdictSymptomOnly, VerdictNotObservable, VerdictUndecidable,
}

// Verdicts returns the published verdicts, best first.
func Verdicts() []Verdict { return slices.Clone(verdictOrder) }

// Valid reports whether v is a published verdict.
func (v Verdict) Valid() bool { return slices.Contains(verdictOrder, v) }

// observabilityRank orders the three decidable verdicts from least to most observable. It is
// what makes the cumulative-feeder-set check in Validate possible: adding feeders can only ever
// move an incident up this scale.
func (v Verdict) observabilityRank() (int, bool) {
	switch v {
	case VerdictNotObservable:
		return 0, true
	case VerdictSymptomOnly:
		return 1, true
	case VerdictObserved:
		return 2, true
	case VerdictUndecidable:
		return 0, false
	default:
		return 0, false
	}
}

// Reason is the coded explanation an `undecidable` verdict must carry. It is a closed set for
// the same reason everything else here is: "why could you not tell?" is exactly the field an
// auditor would otherwise answer in prose about a real incident.
type Reason string

const (
	// ReasonCauseNeverEstablished means the incident was resolved without a cause being agreed.
	ReasonCauseNeverEstablished Reason = "cause_never_established"
	// ReasonConflictingAccounts means the record disagrees with itself.
	ReasonConflictingAccounts Reason = "conflicting_accounts"
	// ReasonOutsideRetention means the evidence needed to decide has aged out.
	ReasonOutsideRetention Reason = "outside_retention"
	// ReasonNoRecord means nothing was written down at the time.
	ReasonNoRecord Reason = "no_record"
	// ReasonFeederSetUntested means this feeder set was never assessed for this incident.
	ReasonFeederSetUntested Reason = "feeder_set_untested"
)

var reasonOrder = []Reason{
	ReasonCauseNeverEstablished,
	ReasonConflictingAccounts,
	ReasonOutsideRetention,
	ReasonNoRecord,
	ReasonFeederSetUntested,
}

// Reasons returns the published undecidable reasons.
func Reasons() []Reason { return slices.Clone(reasonOrder) }

// Valid reports whether r is a published reason.
func (r Reason) Valid() bool { return slices.Contains(reasonOrder, r) }

// ExclusionReason is why an incident of the period is not in the audited list at all. The
// published September 2026 audit excluded two security incidents and one unclassifiable
// incident, and the count the ceiling rests on is only readable if the exclusions are too
// (FR-069a).
type ExclusionReason string

const (
	// ExclusionSecurityIncident is an incident handled under the security process.
	ExclusionSecurityIncident ExclusionReason = "security_incident"
	// ExclusionUnclassifiable is an incident whose record does not support classification.
	ExclusionUnclassifiable ExclusionReason = "unclassifiable"
	// ExclusionDuplicate is a second record of an incident already listed.
	ExclusionDuplicate ExclusionReason = "duplicate"
	// ExclusionNotProduction is a non-production incident.
	ExclusionNotProduction ExclusionReason = "not_production"
	// ExclusionOutsideWindow is an incident outside the corpus window.
	ExclusionOutsideWindow ExclusionReason = "outside_window"
)

var exclusionOrder = []ExclusionReason{
	ExclusionSecurityIncident,
	ExclusionUnclassifiable,
	ExclusionDuplicate,
	ExclusionNotProduction,
	ExclusionOutsideWindow,
}

// ExclusionReasons returns the published exclusion reasons.
func ExclusionReasons() []ExclusionReason { return slices.Clone(exclusionOrder) }

// Valid reports whether r is a published exclusion reason.
func (r ExclusionReason) Valid() bool { return slices.Contains(exclusionOrder, r) }

// Classification is the audit's verdict on one incident under one feeder set, in exactly the
// three values `investigation.coverage_audit_items.classification` accepts (FR-069).
type Classification string

const (
	// ClassificationCausePresent means the cause would have been a node or a change in the
	// graph at alert time.
	ClassificationCausePresent Classification = "cause_present"
	// ClassificationCauseAbsent means it would not have been, and the category says what was
	// missing.
	ClassificationCauseAbsent Classification = "cause_absent"
	// ClassificationUndecidable means the auditor could not tell, and the reason says why.
	ClassificationUndecidable Classification = "undecidable"
)

// oneOf renders a closed set for an error message, so a rejection tells the auditor what the
// accepted values are instead of only that theirs was not one.
func oneOf[T ~string](values []T) string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = string(v)
	}
	return fmt.Sprintf("%v", out)
}
