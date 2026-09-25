// SPDX-License-Identifier: Apache-2.0

package sanitise

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Pierre-Theophile/aisre/internal/investigation/backend"
)

// The applier: one policy, one key, one place every field goes through.
//
// Masking is delegated to feature 002's rules rather than reimplemented, which contract §3
// requires by name: "the miner is feature 002's Drain-style miner, so a recorded template and a
// live one are produced by the same code." Two masking implementations would drift, and the
// direction they would drift in is a recorded template that a live answer would not have shown.

// Sanitiser applies one Policy under one Key. It is built once per campaign, because the key is
// what makes a pseudonym consistent across the corpus, and it accumulates what it dropped, because
// FR-140's manifest has to state it.
type Sanitiser struct {
	policy   *Policy
	key      Key
	drops    Drops
	canaries *CanarySet
}

// New returns a Sanitiser applying policy under key. Both are validated here rather than at first
// use: a campaign that is going to refuse should refuse at minute zero, not after a day of
// recording.
func New(policy *Policy, key Key) (*Sanitiser, error) {
	if policy == nil {
		return nil, fmt.Errorf("sanitise: no policy")
	}
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	if !key.Configured() {
		return nil, fmt.Errorf("%w: a Sanitiser with no key would produce unkeyed hashes that look "+
			"identical to pseudonyms in a golden", ErrNoKey)
	}
	return &Sanitiser{policy: policy, key: key}, nil
}

// Policy returns the table in force.
func (s *Sanitiser) Policy() *Policy { return s.policy }

// Key returns the corpus key, for the fingerprint a manifest records. It does not expose the key
// material beyond what Key itself already allows.
func (s *Sanitiser) Key() Key { return s.key }

// WithCanaries returns s watching for the tokens in set. A canary that reaches an output is a
// failure of everything above it, so the check is here, at the last boundary, rather than only at
// commit — the commit gate is the independent second opinion, not the only one (FR-138).
func (s *Sanitiser) WithCanaries(set *CanarySet) *Sanitiser {
	s.canaries = set
	return s
}

// Drops returns what was dropped, for FR-140's manifest.
func (s *Sanitiser) Drops() []Drop { return s.drops.List() }

// Field applies the policy to one field. It returns the value to record and whether to record it
// at all; a dropped field returns ("", false, nil), which is a different outcome from an empty
// value that was kept.
//
// The people check runs *before* the table is consulted, and that ordering is deliberate. It means
// a field naming a person is dropped even under a policy whose table says otherwise — so the
// guarantee does not depend on the table being right, and a mis-edited table is caught by
// Policy.Validate at construction as well as by this check at every call. Two independent
// mechanisms for the one rule that cannot be walked back after a commit.
func (s *Sanitiser) Field(where, path, value string) (string, bool, error) {
	if IsPeopleField(path) {
		s.drops.Record(path, "names a person: "+ErrPeopleNeverHashed.Error())
		return "", false, nil
	}
	rule, err := s.policy.Field(path, where)
	if err != nil {
		return "", false, err
	}
	switch rule.Disposition {
	case Dropped:
		s.drops.Record(path, rule.Why)
		return "", false, nil
	case Verbatim:
		// A value on the verbatim allowlist that carries a person is dropped anyway. This is the
		// case the `version` label row was accepted on: a deploy tag is kept because ADR-0005 D4's
		// join needs it, and `v2-jane-testing` still does not reach disk.
		if LooksLikePerson(value) {
			s.drops.Record(path, "on the verbatim allowlist, but this value carried a person")
			return "", false, nil
		}
		if err := s.checkCanary(where, path, value); err != nil {
			return "", false, err
		}
		return value, true, nil
	case Pseudonym:
		token, err := s.key.Pseudonym(rule.Kind, value)
		if err != nil {
			return "", false, fmt.Errorf("sanitise: %s in %s: %w", path, where, err)
		}
		if err := s.checkCanary(where, path, value); err != nil {
			return "", false, err
		}
		return token, true, nil
	default:
		return "", false, &UnassignedError{Field: path, Where: where}
	}
}

// Label applies the policy to one label. A key that is not on the allowlist becomes nothing — not
// a property, not a node, not a claim (FR-124) — which is why the returned key is empty rather
// than the original.
func (s *Sanitiser) Label(where, key, value string) (string, string, bool, error) {
	if IsPeopleField(key) {
		s.drops.Record("labels."+key, "a label key naming a person")
		return "", "", false, nil
	}
	rule, err := s.policy.Label(key)
	if err != nil {
		return "", "", false, err
	}
	switch rule.Disposition {
	case Dropped:
		s.drops.Record("labels."+key, rule.Why)
		return "", "", false, nil
	case Verbatim:
		if LooksLikePerson(value) {
			s.drops.Record("labels."+key, "allowlisted, but this value carried a person")
			return "", "", false, nil
		}
		if err := s.checkCanary(where, "labels."+key, value); err != nil {
			return "", "", false, err
		}
		return strings.ToLower(strings.TrimSpace(key)), value, true, nil
	case Pseudonym:
		token, err := s.key.Pseudonym(rule.Kind, value)
		if err != nil {
			return "", "", false, fmt.Errorf("sanitise: label %s in %s: %w", key, where, err)
		}
		if err := s.checkCanary(where, "labels."+key, value); err != nil {
			return "", "", false, err
		}
		return strings.ToLower(strings.TrimSpace(key)), token, true, nil
	default:
		return "", "", false, &UnassignedError{Field: "labels." + key, Where: where}
	}
}

// Fields applies the policy to a whole flattened payload, in sorted order so that a refusal names
// the same field every run. A single unassigned field fails the whole payload: FR-140's "drop
// rather than promise" means a payload that cannot be made safe is dropped, not partially
// recorded.
func (s *Sanitiser) Fields(where string, in map[string]string) (map[string]string, error) {
	paths := make([]string, 0, len(in))
	for path := range in {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	out := make(map[string]string, len(in))
	for _, path := range paths {
		value, keep, err := s.Field(where, path, in[path])
		if err != nil {
			return nil, err
		}
		if keep {
			out[path] = value
		}
	}
	return out, nil
}

// Labels applies the policy to a label map.
func (s *Sanitiser) Labels(where string, in map[string]string) (map[string]string, error) {
	keys := make([]string, 0, len(in))
	for key := range in {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	out := make(map[string]string, len(in))
	for _, key := range keys {
		outKey, value, keep, err := s.Label(where, key, in[key])
		if err != nil {
			return nil, err
		}
		if keep {
			out[outKey] = value
		}
	}
	return out, nil
}

// Template reduces one log line to its masked template (§3, FR-136, FR-090). It delegates to
// feature 002's masking rules so that a recorded template and a live one are the same string.
//
// Three outcomes, and which one a case gets is the point:
//
//   - a template, kept — the ordinary path;
//   - dropped, recorded in Drops — the line still names a person after masking. The masking rules
//     cover an address (it becomes <email>) but not an @handle, and a line mentioning one is
//     dropped rather than failing the payload around it. A handle in a log line is data variance,
//     not a broken gate, and failing the payload would mean one chatty log line costs an hour of
//     recorded topology;
//   - an error — either the masker left an unmasked literal behind, which is a bug in the masker
//     rather than a property of the line, or a canary reached here, which is a broken gate.
func (s *Sanitiser) Template(where, line string) (string, bool, error) {
	if err := s.checkCanary(where, "logEntry.template", line); err != nil {
		return "", false, err
	}
	masked := backend.MaskLine(line)
	if unmasked := backend.UnmaskedLiterals(masked); unmasked != "" {
		return "", false, fmt.Errorf("sanitise: a log line in %s still carries an unmasked %s after "+
			"masking; a raw line has no route to disk (FR-136). This is a defect in the masking "+
			"rules, not a property of the line", where, unmasked)
	}
	if LooksLikePerson(masked) {
		s.drops.Record("logEntry.template", "a log line naming a person survived masking as a handle")
		return "", false, nil
	}
	return masked, true, nil
}

// checkCanary refuses a value carrying a seeded canary token. A canary that got this far means
// every rule above it was wrong about this payload, so the outcome is a refusal and not a warning
// (FR-139, SC-018).
func (s *Sanitiser) checkCanary(where, path, value string) error {
	if s.canaries == nil {
		return nil
	}
	if hit := s.canaries.FindIn(value); hit != nil {
		return &CanarySurvivedError{Canary: *hit, Where: where, Field: path}
	}
	return nil
}
