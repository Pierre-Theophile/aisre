// SPDX-License-Identifier: Apache-2.0

package sanitise

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// People identifiers are dropped, never hashed (T040, FR-135, SC-019, contract §1).
//
// This is the reverse of the mistake §1 warns about, and it is the one that matters more. A hashed
// principal email address is still personal data: it is stable, it is linkable across every
// artifact in the corpus, and it is reversible for any address an attacker can guess — which is
// every address at a company whose domain and naming convention are public. Hashing it changes
// who can read it, not whether it is there.
//
// So the rule holds in live mode as well as in recordings (FR-110), and the assertion is "zero
// people identifiers in any form, hashed included" (SC-019). Two mechanisms carry it:
//
//   - Policy.Validate refuses a table in which a people field is anything but Dropped, so the
//     violation cannot be introduced by editing the table — which is the one place no downstream
//     scan would look.
//   - Sanitiser.Field refuses to pseudonymise a people field even if a caller asks for it, so the
//     violation cannot be introduced by a call site either.
//
// There is no investigative loss. What an investigation needs from a change is the *actor kind* —
// person, automation, unknown — and that is a closed enumeration recorded verbatim. Nothing in the
// causal question "did this change break that service" is answered by which person pressed deploy.

// PeopleFieldKeys is the published set of field and attribute names that name a person. A value
// under one of these is dropped wherever it appears, in any payload, under any policy.
//
// The set is deliberately generous, and the asymmetry is the reason: a false positive costs one
// field on one recording, and a false negative puts a person's identity in a corpus that is shared
// with graders and replayed by strangers. It is a superset of feature 002's PeopleAttributeKeys —
// sanitise_test.go asserts that, so a key added there cannot go missing here.
var PeopleFieldKeys = map[string]struct{}{
	// Feature 002's published vocabulary.
	"actor": {}, "author": {}, "committer": {}, "creator": {}, "email": {},
	"enduser.id": {}, "git.author": {}, "git.commit.author": {}, "operator": {},
	"owner": {}, "principal": {}, "requested_by": {}, "triggered_by": {},
	"user": {}, "user.email": {}, "user.id": {}, "user.name": {}, "user_id": {},
	"username": {},

	// GCP audit log spellings. The delegation entries are the ones that matter: an audit entry
	// for an impersonated service account names the service account as the principal and the
	// human behind it one level down, and a rule that only covered `principalEmail` at the top
	// would drop the robot and keep the person.
	"principalemail":                                   {},
	"principalsubject":                                 {},
	"serviceaccountkeyname":                            {},
	"authenticationinfo.principalemail":                {},
	"authenticationinfo.principalsubject":              {},
	"firstpartyprincipal.principalemail":               {},
	"thirdpartyprincipal.principalemail":               {},
	"serviceaccountdelegationinfo.principalemail":      {},
	"protopayload.authenticationinfo.principalemail":   {},
	"protopayload.authenticationinfo.principalsubject": {},

	// Message transports. A notice's addresses are people even when one of them is a group.
	"sender": {}, "recipient": {}, "recipients": {}, "from": {}, "to": {},
	"cc": {}, "bcc": {}, "replyto": {}, "deliveredto": {}, "returnpath": {},
	"mailbox": {}, "essential_contact": {},

	// Vercel's environment-variable metadata, which credits the person who created and last edited a
	// variable (004 T095). `creator` above does not cover them: the vocabulary matches on a collapsed
	// leaf, so `createdBy` collapses to `createdby` and misses `creator` entirely. They are ids rather
	// than names or addresses, which changes nothing — an id that identifies one person is a people
	// identifier, and FR-061 drops those rather than pseudonymising them, because a stable pseudonym
	// for a person is still a way to follow that person through a corpus.
	"created_by": {}, "updated_by": {}, "last_edited_by": {},
	// The audit-log spellings, added with them rather than later. Vercel's audit log names the actor
	// as `actor_vercel_id`, `actor_name` and `actor_email`, and if a drain reader is ever built the
	// day it lands is the worst day to discover the vocabulary did not cover it.
	"actor_vercel_id": {}, "actor_name": {}, "actor_email": {},

	// Where a person appears named as such.
	"assignee": {}, "approver": {}, "reviewer": {}, "signatory": {},
	"contact": {}, "contact_email": {}, "on_call": {}, "oncall": {},
	"display_name": {}, "displayname": {}, "full_name": {}, "given_name": {},
	"family_name": {}, "phone": {}, "phone_number": {},
}

// collapse removes the separators three vendors spell the same concept with, so that `user.email`,
// `userEmail` and `user_email` are one key.
var collapse = strings.NewReplacer("-", "", "_", "", ".", "", " ", "")

// peopleCollapsed is PeopleFieldKeys in collapsed form, built once. It matters that this is built
// once rather than per call: the drop check runs on every field of every payload in a campaign.
var peopleCollapsed = func() map[string]struct{} {
	out := make(map[string]struct{}, len(PeopleFieldKeys))
	for key := range PeopleFieldKeys {
		out[collapse.Replace(key)] = struct{}{}
	}
	return out
}()

// IsPeopleField reports whether a field path names a person under the published vocabulary.
//
// Matching is on the path's leaf and on its last two segments, not on the whole path: an audit
// entry addresses the principal as
// `protoPayload.authenticationInfo.serviceAccountDelegationInfo[].firstPartyPrincipal.principalEmail`,
// and a vocabulary that had to spell every enclosing path would be a vocabulary with a hole in it
// the first time GCP nested one level deeper.
func IsPeopleField(path string) bool {
	canonical := CanonicalField(path)
	canonical = strings.ReplaceAll(canonical, "[]", "")
	if _, ok := peopleCollapsed[collapse.Replace(canonical)]; ok {
		return true
	}
	segments := strings.Split(canonical, ".")
	for i := len(segments) - 1; i >= 0 && i >= len(segments)-2; i-- {
		tail := strings.Join(segments[i:], ".")
		if _, ok := peopleCollapsed[collapse.Replace(tail)]; ok {
			return true
		}
	}
	return false
}

// PeopleValuePatterns are the shapes a person is written in, for the case the field name does not
// give them away: an address inside a free-text value, a handle, a `user:` IAM member.
//
// This is a value check and therefore a heuristic, and it is used for exactly one thing: refusing
// to record a value that a field-name rule said was safe. It never decides that something *is*
// safe.
var PeopleValuePatterns = []*regexp.Regexp{
	regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`),
	regexp.MustCompile(`(?:^|\s)@[A-Za-z][A-Za-z0-9._\-]{2,}`),
	regexp.MustCompile(`\buser:[^\s,"]+`),
	regexp.MustCompile(`\bserviceAccount:[^\s,"]*@[^\s,"]+`),
}

// LooksLikePerson reports whether value carries a shape a person is written in. It is the second
// half of the field-name check and never the whole of it, because the field name is the only
// evidence that is actually reliable.
func LooksLikePerson(value string) bool {
	for _, pattern := range PeopleValuePatterns {
		if pattern.MatchString(value) {
			return true
		}
	}
	return false
}

// Drop is the record of one dropped value. It carries no value — that is the point — and it is
// what FR-140's manifest means by "what was dropped": a reader learns that a principal email was
// present and removed, which is a materially different statement from the field never existing.
type Drop struct {
	Field string
	Why   string
}

// Drops is an ordered, de-duplicated set of drops, safe to serialise into a manifest.
type Drops struct {
	seen  map[string]string
	order []string
}

// Record notes that field was dropped for reason why. It is idempotent per field, so a field
// dropped on ten thousand payloads is one manifest line.
func (d *Drops) Record(field, why string) {
	if d.seen == nil {
		d.seen = map[string]string{}
	}
	canonical := CanonicalField(field)
	if _, ok := d.seen[canonical]; ok {
		return
	}
	d.seen[canonical] = why
	d.order = append(d.order, canonical)
}

// List returns the drops, sorted by field, for a manifest.
func (d *Drops) List() []Drop {
	out := make([]Drop, 0, len(d.order))
	for _, field := range d.order {
		out = append(out, Drop{Field: field, Why: d.seen[field]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Field < out[j].Field })
	return out
}

// Len returns how many distinct fields were dropped.
func (d *Drops) Len() int { return len(d.order) }

// ErrPeopleNeverHashed is the refusal a caller gets for asking to pseudonymise a person. It names
// the rule rather than the field, because the caller's next question is always "why not".
var ErrPeopleNeverHashed = fmt.Errorf("people identifiers are dropped, never hashed: a hashed " +
	"principal address is stable, linkable and reversible by enumeration, so it is still personal " +
	"data (FR-135, SC-019)")
