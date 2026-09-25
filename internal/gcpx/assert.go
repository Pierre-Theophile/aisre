// SPDX-License-Identifier: Apache-2.0

package gcpx

import (
	"fmt"
	"strings"
)

// Layer 3 of the gate: the operator's named assertion covering what layers 1 and 2 cannot reach
// (FR-004; research §8.1).
//
// This layer looks like a hedge and is the opposite of one. Layer 1 cannot see child-resource
// bindings, because IAM inherits downward only; it cannot see Cloud SQL, Cloud Monitoring or
// log-entry permissions per resource, because no per-resource test exists for them; and whether it
// subtracts deny policies is undocumented. Those are not edge cases — they are, between them, most
// of what "this credential cannot write" would have to mean.
//
// The choice is therefore between a gate that SILENTLY ABSORBS its own blind spots and one that
// names them and makes a person sign for them. FR-004 requires the second, and this is it: the
// refusal names what it could not verify, and an operator asserts read-only by name in configuration
// rather than by the absence of an error.
//
// The assertion is recorded in the checkpoint and the usage report, so a later reader knows WHO
// asserted it, not merely that something did.

// Assertion is the operator's declaration, from configuration.
type Assertion struct {
	// By is the person who asserted it. A name, not a service account: this is a human taking
	// responsibility for what a machine could not check, so an automated value defeats it.
	By string
	// At is when, as configured.
	At string
	// Acknowledged lists the unverifiable areas the operator has read and accepted. It must cover
	// UnverifiableAreas(); a partial acknowledgement is refused, because an assertion that skipped
	// the item that mattered is worse than none — it looks like coverage.
	Acknowledged []string
}

// Validate checks the assertion actually covers what could not be verified.
//
// The count check is deliberate rather than fussy. An operator who acknowledges four of seven areas
// has probably copied an older config, and the three they dropped are exactly the ones nobody
// re-read. Requiring the full set makes adding an area to UnverifiableAreas() a change operators
// must notice.
func (a *Assertion) Validate(unverifiable []string) error {
	if a == nil || strings.TrimSpace(a.By) == "" {
		return &GateRefusal{
			Layer: LayerAssertion,
			Detail: fmt.Sprintf("layers 1 and 2 cannot verify %d area(s), and configuration carries no "+
				"named operator assertion. FR-004 requires a person to assert read-only for what the "+
				"credential check cannot reach:\n  - %s",
				len(unverifiable), strings.Join(unverifiable, "\n  - ")),
		}
	}
	missing := make([]string, 0, len(unverifiable))
	for _, area := range unverifiable {
		if !acknowledges(a.Acknowledged, area) {
			missing = append(missing, area)
		}
	}
	if len(missing) > 0 {
		return &GateRefusal{
			Layer: LayerAssertion,
			Detail: fmt.Sprintf("%s asserted read-only but did not acknowledge %d of the %d areas the "+
				"credential check cannot verify. A partial acknowledgement is refused because it looks "+
				"like coverage:\n  - %s",
				a.By, len(missing), len(unverifiable), strings.Join(missing, "\n  - ")),
		}
	}
	return nil
}

// acknowledges matches on the area's leading phrase — the text before the first colon — so that
// rewording the explanation in UnverifiableAreas() does not invalidate every operator's config,
// while adding a NEW area still does.
func acknowledges(acked []string, area string) bool {
	key := area
	if i := strings.Index(area, ":"); i > 0 {
		key = area[:i]
	}
	for _, a := range acked {
		if strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(key)) {
			return true
		}
	}
	return false
}
