// SPDX-License-Identifier: Apache-2.0

package feeder

import (
	"fmt"
	"sort"
	"strings"
)

// The read-only startup gate, and the asymmetry it has to be honest about (004 FR-003, T032;
// specs/004-deploy-feeders/contracts/read-only-operations.md §3.1).
//
// ---------------------------------------------------------------------------------------------
// FR-003 in three clauses, and the third is the one that needs a type
//
// Ask the platform what the credential permits; refuse on any write permission naming it; and
// **where the platform cannot report one, require the operator to assert read-only rather than
// assume**. The first two are a check. The third is a fact about the platform that the checkpoint has
// to carry, because a guarantee resting on an operator's word is worth less than one resting on a
// platform's and a reader must be able to tell which they have.
//
// The two platforms this feature reads sit on opposite sides of it, established from their API
// references rather than assumed:
//
//   - **GitHub reports.** The installation access-token response carries a `permissions` object with
//     the granular permission keys, so the connector sees what it was granted every time it mints a
//     token. The gate is a check against a platform statement.
//   - **Vercel does not.** Nothing reports write capability. `GET /v2/user` has a `limited` form
//     meaning the token lacks privileges to read full user data, which is a signal about privileges
//     and not a permission list. So the gate falls back to the operator's assertion — which FR-003
//     permits for exactly this case.
//
// A single boolean "read-only: yes" would flatten those two into one claim. `CredentialReport` keeps
// them apart, and `Evidence` is what a checkpoint records so that an operator reading it afterwards
// knows whether they were told or whether they said so themselves.

// CredentialEvidence is how the read-only conclusion was reached.
type CredentialEvidence string

// The two forms, and there is no third. A conclusion with neither is the zero value, which fails.
const (
	// EvidenceUnassigned is a report nobody filled in. It fails the gate, rather than defaulting to
	// anything: a default is how the next platform nobody thought about gets a read-only guarantee
	// without a human looking at it.
	EvidenceUnassigned CredentialEvidence = ""
	// EvidencePlatformReported means the platform stated what the credential permits and the gate
	// checked it.
	EvidencePlatformReported CredentialEvidence = "platform_reported"
	// EvidenceOperatorAsserted means the platform reports nothing and the operator declared the
	// credential read-only. It is a weaker guarantee and the word "asserted" is in the value so that
	// it cannot be read as the stronger one by a reader skimming a checkpoint.
	EvidenceOperatorAsserted CredentialEvidence = "operator_asserted"
)

// readPermissionValues are the permission values that grant no write.
//
// `none` is included because a platform may report a permission it did not grant, and a permission
// granted at `none` is not a write. Everything else — `write`, `admin`, and anything this list has
// never heard of — refuses, which is the direction that matters: a value nobody anticipated is a
// value nobody has ruled on, and on a read-only gate the fail-closed answer is the only safe one.
// That is also why the list does not try to enumerate a platform's whole vocabulary: it does not need
// to be complete to be correct.
var readPermissionValues = []string{"read", "none"}

// CredentialReport is what is known about one credential's permissions at startup.
type CredentialReport struct {
	// Platform names the connector, for the refusal.
	Platform string
	// Evidence is how read-only was concluded. Required.
	Evidence CredentialEvidence
	// Permissions is what the platform reported, keyed by its own permission name. Required and
	// non-empty when Evidence is EvidencePlatformReported; ignored otherwise.
	//
	// A platform that reports an EMPTY permission set is not the same as one that reports nothing,
	// and neither is treated as read-only by default: an empty set from a platform that does report
	// is a credential that can do nothing, which is a misconfiguration worth failing on rather than a
	// read-only credential worth celebrating.
	Permissions map[string]string
	// Scope describes what the credential is bounded to, and which regime that is (FR-008).
	Scope CredentialScope
}

// CredentialScope is the boundary on what a credential can reach, and who enforces it.
type CredentialScope struct {
	// PlatformEnforced says whether the boundary is the platform's or the operator's. GitHub's
	// installation repository selection is the platform's; a Vercel token created without
	// `projectId` is bounded only by the operator's configured list, which is a narrowing of what the
	// token could read rather than a boundary anyone holds.
	PlatformEnforced bool
	// Selection is the platform's own word for the regime where it has one — GitHub reports
	// `all` or `selected` — and is empty where the platform says nothing.
	Selection string
	// Targets are the repositories or projects in scope, for the checkpoint. It may be empty on a
	// platform-enforced `all`, where the scope is "whatever the installation covers" rather than a
	// list.
	Targets []string
}

// WriteCapableError is the refusal FR-003 turns on: the platform said the credential may write.
type WriteCapableError struct {
	Platform string
	// Grants are the offending permissions, `name=value`, sorted so the refusal reads the same twice.
	Grants []string
}

func (e *WriteCapableError) Error() string {
	return fmt.Sprintf("feeder: %s's credential holds %s, which is not read-only; constitution VII "+
		"permits no write to any production system, so the connector refuses to start rather than "+
		"trusting itself not to use it (FR-003)", e.Platform, strings.Join(e.Grants, ", "))
}

// UnverifiedCredentialError is the refusal for the third clause: the platform reports nothing and
// nobody asserted anything.
type UnverifiedCredentialError struct {
	Platform string
}

func (e *UnverifiedCredentialError) Error() string {
	return fmt.Sprintf("feeder: %s reports nothing about what its credential permits, and no operator "+
		"has asserted that it is read-only; FR-003 requires the assertion rather than the assumption, "+
		"so there is nothing to start from", e.Platform)
}

// CheckReadOnly is the gate. It returns the evidence to record, or the reason it refused.
//
// It never concludes read-only from silence. The three ways to fail are all of them: a platform that
// reported a write, a platform that reports nothing with nobody asserting, and a report nobody filled
// in at all.
func CheckReadOnly(report CredentialReport) (CredentialEvidence, error) {
	platform := strings.TrimSpace(report.Platform)
	if platform == "" {
		return EvidenceUnassigned, fmt.Errorf("feeder: a credential report with no platform name; its " +
			"refusal would not say whose credential was refused")
	}
	switch report.Evidence {
	case EvidencePlatformReported:
		if len(report.Permissions) == 0 {
			return EvidenceUnassigned, fmt.Errorf("feeder: %s claims the platform reported its "+
				"permissions and names none; an empty set from a platform that does report is a "+
				"credential that can do nothing, which is a misconfiguration rather than a read-only "+
				"credential", platform)
		}
		var grants []string
		for name, value := range report.Permissions {
			if !isReadPermission(value) {
				grants = append(grants, name+"="+value)
			}
		}
		if len(grants) > 0 {
			sort.Strings(grants)
			return EvidenceUnassigned, &WriteCapableError{Platform: platform, Grants: grants}
		}
		return EvidencePlatformReported, nil
	case EvidenceOperatorAsserted:
		return EvidenceOperatorAsserted, nil
	default:
		return EvidenceUnassigned, &UnverifiedCredentialError{Platform: platform}
	}
}

// isReadPermission is deliberately a whitelist with a fail-closed default. Comparison folds case
// because a platform's spelling is a platform's business, and no read value is distinguished from
// another by case.
func isReadPermission(value string) bool {
	folded := strings.ToLower(strings.TrimSpace(value))
	for _, allowed := range readPermissionValues {
		if folded == allowed {
			return true
		}
	}
	return false
}
