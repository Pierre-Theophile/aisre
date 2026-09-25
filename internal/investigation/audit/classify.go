// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"fmt"
	"slices"
)

// Classification of one incident under one feeder set (T011, FR-069, FR-070).
//
// What this file does NOT do is worth saying first. It does not ask the graph anything. The
// question "would the cause have been a node or a change in the graph at the alert instant,
// with observed time pinned to it?" is answered by a human, against a graph running a named
// feeder set, and transcribed into the list's per-feeder-set verdict. That is the only honest
// arrangement available: the audit's premise is a cause the engine did not derive (FR-069), and
// a cause the engine did not derive cannot be matched against the graph by the engine either
// without reintroducing exactly the circularity FR-069 exists to prevent.
//
// What is mechanical — and therefore lives here — is the mapping from a verdict to the three
// published classifications, the separation of `symptom_only` from both of them, and the
// arithmetic that turns a column of verdicts into a ceiling. The auditor supplies judgement;
// this file supplies consistency.
//
// `cause.ref`, when an auditor chooses to supply it, is carried through to
// `coverage_audit_items.matched_entity_id` so a reader can check a classification by hand. It
// is never required and never inferred.

// Item is one incident as the audit classified it under one feeder set.
type Item struct {
	// ItemID is deterministic — `<audit_id>:<feeder_set>:<incident_ref>` — so that re-running
	// the audit over the same list rewrites the same rows instead of accumulating them.
	ItemID string `json:"item_id"`
	// IncidentRef is the opaque reference from the list.
	IncidentRef string `json:"incident_ref"`
	// AlertAt is when the alert fired.
	AlertAt Instant `json:"alert_at"`
	// StatedCause is the human-supplied cause in the form the database records:
	// `<category>/<class>`. It is composed rather than transcribed precisely because the
	// input format carries no prose to transcribe.
	StatedCause string `json:"stated_cause"`
	// Category is the stated cause's category.
	Category Category `json:"category"`
	// Class is the stated cause's class.
	Class CauseClass `json:"class"`
	// Verdict is the auditor's observability verdict under this feeder set.
	Verdict Verdict `json:"verdict"`
	// Classification is the published three-value classification derived from it.
	Classification Classification `json:"classification"`
	// Reason is the coded reason, present only on an undecidable classification.
	Reason Reason `json:"reason,omitempty"`
	// MatchedEntityID is the optional graph identifier the auditor supplied.
	MatchedEntityID string `json:"matched_entity_id,omitempty"`
	// CountsTowardCeiling is false for an undecidable item, which leaves the denominator
	// rather than being scored as a miss.
	CountsTowardCeiling bool `json:"counts_toward_ceiling"`
	// OwesFixture is true when this item belongs to the unobservable remainder and its
	// category therefore owes the evaluation corpus a fixture (FR-071b).
	OwesFixture bool `json:"owes_fixture"`
}

// classificationFor maps an observability verdict onto the three published classifications.
//
// `symptom_only` classifies as `cause_absent`: the effect being visible is not the cause being
// present, and counting it toward the ceiling would publish a recall bound the engine could not
// reach. It is reported separately (FeederSetResult.SymptomOnly) because the distinction is
// what tells a reader whether a feeder would add localisation or add causes.
func classificationFor(verdict Verdict) (Classification, error) {
	switch verdict {
	case VerdictObserved:
		return ClassificationCausePresent, nil
	case VerdictSymptomOnly, VerdictNotObservable:
		return ClassificationCauseAbsent, nil
	case VerdictUndecidable:
		return ClassificationUndecidable, nil
	default:
		return "", fmt.Errorf("audit: verdict %q: want one of %s", verdict, oneOf(verdictOrder))
	}
}

// statedCause composes the database's `stated_cause` from the two closed-set fields. The
// column is NOT NULL because the audit refuses a list that supplies no cause (FR-069); it is
// composed rather than free text because the format carries none.
func statedCause(cause Cause) string {
	return string(cause.Category) + "/" + string(cause.Class)
}

// Classify classifies every incident in the list under the named feeder set.
//
// The list has already been validated, so the only error this can raise is an unknown feeder
// set name — which is a caller's typo rather than a broken input.
func Classify(list *List, feederSet string) ([]Item, error) {
	if list == nil {
		return nil, fmt.Errorf("audit: classify: no incident list")
	}
	if !slices.ContainsFunc(list.FeederSets, func(s FeederSet) bool { return s.Name == feederSet }) {
		return nil, fmt.Errorf("audit: feeder set %q is not declared by this list (declared: %s)",
			feederSet, oneOf(list.feederSetNames()))
	}

	items := make([]Item, 0, len(list.Incidents))
	for _, incident := range list.Incidents {
		observation := incident.Observability[feederSet]
		classification, err := classificationFor(observation.Verdict)
		if err != nil {
			return nil, fmt.Errorf("audit: incident %s: %w", incident.Ref, err)
		}
		items = append(items, Item{
			ItemID:              list.AuditID + ":" + feederSet + ":" + incident.Ref,
			IncidentRef:         incident.Ref,
			AlertAt:             incident.AlertAt,
			StatedCause:         statedCause(incident.Cause),
			Category:            incident.Cause.Category,
			Class:               incident.Cause.Class,
			Verdict:             observation.Verdict,
			Classification:      classification,
			Reason:              observation.Reason,
			MatchedEntityID:     incident.Cause.Ref,
			CountsTowardCeiling: classification != ClassificationUndecidable,
			OwesFixture: observation.Verdict == VerdictNotObservable &&
				incident.Cause.Class.NeedsFixture(),
		})
	}
	slices.SortStableFunc(items, func(a, b Item) int {
		if c := a.AlertAt.Compare(b.AlertAt.Time); c != 0 {
			return c
		}
		return compareStrings(a.IncidentRef, b.IncidentRef)
	})
	return items, nil
}

// feederSetNames returns the declared feeder set names in cumulative order.
func (l *List) feederSetNames() []string {
	sets := l.orderedFeederSets()
	names := make([]string, len(sets))
	for i, set := range sets {
		names[i] = set.Name
	}
	return names
}

func compareStrings(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}
