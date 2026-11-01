// SPDX-License-Identifier: Apache-2.0

package record

import (
	"errors"
	"fmt"
	"time"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/investigation/ledger"
)

// WeightClassStrong is the only weight class a human fact may carry. A person who is certain is
// still a person, and the ledger has to be able to report a conflict with what was measured.
const WeightClassStrong = "strong"

// The published human-fact kinds (data-model §investigation.human_facts).
const (
	FactKindManualAction = "manual_action"
	FactKindVendorNotice = "vendor_notice"
	FactKindKnownState   = "known_state"
	FactKindCorrection   = "correction"
	FactKindOther        = "other"
)

// EvidenceKindHumanFact is the evidence kind a human fact becomes.
const EvidenceKindHumanFact = "human_fact"

// FactKinds lists the published kinds, for a CLI that has to say what it accepts.
func FactKinds() []string {
	return []string{FactKindManualAction, FactKindVendorNotice, FactKindKnownState,
		FactKindCorrection, FactKindOther}
}

// FactKindAliases map the spellings connectors use onto the published set.
//
// The published vocabulary is `investigation.human_facts.kind`'s CHECK and is deliberately
// short. A connector that observes a fact in a chat thread or a ticket will spell it in its own
// words, and `operator_action` — which the `human-fact-reopen-01` fixture uses in its graph
// event — is the same fact as `manual_action`. The alias table is explicit and small: a spelling
// that is not here is an error rather than a silent `other`, because a fact filed under the
// wrong kind is worse than one that was refused.
var FactKindAliases = map[string]string{
	"operator_action": FactKindManualAction,
	"manual":          FactKindManualAction,
	"vendor":          FactKindVendorNotice,
	"state":           FactKindKnownState,
}

// NormaliseFactKind maps a connector's spelling onto the published set, or reports that it does
// not know it.
func NormaliseFactKind(kind string) (string, error) {
	if ValidFactKind(kind) {
		return kind, nil
	}
	if mapped, ok := FactKindAliases[kind]; ok {
		return mapped, nil
	}
	return "", fmt.Errorf("%w (got %q)", ErrUnknownFactKind, kind)
}

// ErrUnknownFactKind is a fact whose kind is outside the published set.
var ErrUnknownFactKind = fmt.Errorf("human fact kind must be one of %v", FactKinds())

// ErrDecisiveHumanFact is an attempt to weight a human fact as decisive. It is refused here as
// well as by the schema, because the rule is the point rather than the column.
var ErrDecisiveHumanFact = errors.New(
	"a human fact is strong evidence, never decisive: where telemetry contradicts it both are kept (FR-057a)")

// FactEvidence is the evidence item a human fact becomes (FR-057a).
//
// It has no algebra term — nobody queried anything — and says so in its deep-link absence reason
// rather than inventing a link. Its coverage block names the entities the fact concerns, which is
// the honest answer to "what did this look at?": one person's knowledge of those entities.
func FactEvidence(fact *investigationv1.HumanFact, validAt, observedAt time.Time) ledger.EvidenceItem {
	searched := fact.GetEntityIds()
	if searched == nil {
		searched = []string{}
	}
	return ledger.EvidenceItem{
		ID:         fact.GetEvidenceId(),
		Kind:       EvidenceKindHumanFact,
		Worker:     "human",
		Capability: fact.GetKind(),
		ValidAt:    validAt.UTC(),
		ObservedAt: observedAt.UTC(),
		CalledAt:   fact.GetSubmittedAt().AsTime().UTC(),
		Mode:       "live",
		Outcome:    "digest",
		Coverage: &investigationv1.Coverage{
			SearchedEntities:         searched,
			DataSource:               "human",
			ExecutedAt:               fact.GetSubmittedAt(),
			VolumeConsidered:         1,
			IngestionLagUndetermined: true,
			QuotaUndetermined:        true,
		},
		DeepLinkAbsentReason: "a human fact is not a query; `aisre investigate get <id> --evidence` " +
			"shows the statement, its author and the instant it was submitted",
		FreeText: fact.GetStatement(),
	}
}

// FactJudgment is the judgment a human fact makes against one hypothesis: strong, never decisive
// (FR-057a, research §8). The direction is the caller's — a fact can support or refute — and the
// ledger computes the number.
func FactJudgment(id, hypothesisID string, fact *investigationv1.HumanFact, direction ledger.Direction) ledger.Judgment {
	return ledger.Judgment{
		ID:           id,
		HypothesisID: hypothesisID,
		EvidenceID:   fact.GetEvidenceId(),
		Direction:    direction,
		Strength:     ledger.Strong,
		Source:       ledger.SourceHumanFact,
		RecordedAt:   fact.GetSubmittedAt().AsTime().UTC(),
	}
}

// AgreementWithHumans is the metric FR-056 asks for on every evaluation run: of the reviewed
// investigations, the fraction whose top-ranked hypothesis matched the validated root cause,
// and, separately, the fraction of labels that said "right".
//
// It is computed from the rows rather than materialised, because a review or a label added
// tomorrow must move it without a backfill.
type AgreementWithHumans struct {
	// Reviewed is how many investigations carry at least one review.
	Reviewed int
	// Agreed is how many of those had the validated root cause ranked first.
	Agreed int
	// Labelled and LabelledRight are the same counts over the one-click label.
	Labelled      int
	LabelledRight int
}

// Rate is Agreed/Reviewed, or 0 when nothing has been reviewed. A rate over an empty corpus is
// not 100%.
func (a AgreementWithHumans) Rate() float64 {
	if a.Reviewed == 0 {
		return 0
	}
	return float64(a.Agreed) / float64(a.Reviewed)
}

func ValidFactKind(kind string) bool {
	for _, k := range FactKinds() {
		if k == kind {
			return true
		}
	}
	return false
}

func CloneFact(fact *investigationv1.HumanFact) *investigationv1.HumanFact {
	out, ok := Proto3Clone(fact).(*investigationv1.HumanFact)
	if !ok {
		return &investigationv1.HumanFact{}
	}
	return out
}
