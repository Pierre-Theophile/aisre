// SPDX-License-Identifier: Apache-2.0

package sanitise

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
)

// The keyed pseudonym (T039, FR-135, contracts/sanitisation.md §2.3).
//
//	pseudonym(kind, value) = kind_prefix + base32(HMAC-SHA256(corpus_key, kind || 0x00 || value))[:12]
//
// Four properties, each load-bearing:
//
//  1. **Consistent across a whole corpus.** The same service is the same token in a topology
//     payload, an audit entry, a digest join key and a golden. This is not tidiness: a digest
//     whose keys do not join is evidence about nothing, and `errors_by_version` stops being able
//     to say "the new revision is failing and the old one is not" — the single most useful
//     sentence the corpus contains.
//  2. **Typed by kind**, so a service and an instance that happen to share a name do not collide
//     into one token and silently merge two entities in the recorded graph. The kind goes into
//     the MAC input, not just the prefix, so the two differ in the digest as well as the label.
//     The 0x00 separator is what stops `kind="a", value="bc"` and `kind="ab", value="c"` from
//     being the same input.
//  3. **Keyed**, so the mapping is not recoverable by anyone holding the corpus. An unkeyed hash
//     of an identifier drawn from a namespace an attacker can enumerate — every service name at
//     a company whose repository is public — is a lookup table, not a pseudonym.
//  4. **Stable across a campaign, rotated between organisations.** One corpus, one key. Neither
//     the key nor any mapping table is ever committed.
//
// # Why the prefix is `px_`
//
// Feature 002's redactor already pseudonymises digest join keys, into `px_` + 16 hex characters,
// and 330 of those tokens are baked into its committed goldens. Changing that function to this
// one would rewrite a published corpus, so it stays as it is — and the two must still agree,
// because the graph this feature records and the digests that feature answers with are one
// corpus, and §2.3 property 1 is a claim about the join between them.
//
// They agree by ordering rather than by a shared implementation. `backend.Redactor.Pseudonymise`
// returns a value unchanged when it already carries the `px_` prefix, so a value this package has
// pseudonymised passes through it untouched and one token appears on both sides. That works in
// one direction only: **sanitisation runs before the digest leaves the backend, never after.**
// FR-137 requires that ordering anyway (in the connector, before disk); this is a second reason
// for it, and sanitise_test.go asserts it rather than trusting it.

// PseudonymPrefix marks a value as a pseudonym rather than a production identifier, so that a
// reader of a golden is never misled into pasting one into a vendor console. It is deliberately
// the same prefix feature 002 uses — see the note above.
const PseudonymPrefix = "px_"

// PseudonymDigits is the number of base32 characters kept from the MAC. Sixty bits: over a corpus
// of a hundred thousand distinct identifiers the chance of any collision is about four in a
// billion, and a collision is visible as two entities sharing a token rather than as silent
// corruption, because the kinds would have to match too.
const PseudonymDigits = 12

// Kind types a pseudonym. Two identifiers of different kinds that share a name pseudonymise to
// different tokens (§2.3 property 2).
type Kind string

// The published kinds. Each has a short tag that appears in the token, so a golden reads as
// `px_svc_k3m2...` rather than as undifferentiated noise — a reviewer can see that a service was
// joined to a service.
const (
	KindProject      Kind = "project"
	KindService      Kind = "service"
	KindRevision     Kind = "revision"
	KindInstance     Kind = "instance"
	KindCluster      Kind = "cluster"
	KindNamespace    Kind = "namespace"
	KindHost         Kind = "host"
	KindIPAddress    Kind = "ip"
	KindTeam         Kind = "team"
	KindEnvironment  Kind = "environment"
	KindResource     Kind = "resource"
	KindAlertPolicy  Kind = "alert_policy"
	KindLoadBalancer Kind = "load_balancer"
	KindDNSRecord    Kind = "dns_record"
	KindNoticeID     Kind = "notice_id"
	// The deploy feeders' identifiers (004 T104). An organisation and a repository are named apart
	// from a team and a resource because GitHub states both in several places — a login, a slug in a
	// URL, a numeric id — and each has to join to itself across all of them.
	KindOrganisation Kind = "organisation"
	KindRepository   Kind = "repository"
	KindCommit       Kind = "commit"
	KindRun          Kind = "run"
	KindRelease      Kind = "release"
	KindStatus       Kind = "deployment_status"
)

// kindTag is the token infix per kind. They are short because they appear in every identifier in
// every golden, and fixed because changing one rewrites a corpus.
var kindTag = map[Kind]string{
	KindProject:      "prj",
	KindService:      "svc",
	KindRevision:     "rev",
	KindInstance:     "ins",
	KindCluster:      "cls",
	KindNamespace:    "ns",
	KindHost:         "hst",
	KindIPAddress:    "ip",
	KindTeam:         "tm",
	KindEnvironment:  "env",
	KindResource:     "res",
	KindAlertPolicy:  "alp",
	KindLoadBalancer: "lb",
	KindDNSRecord:    "dns",
	KindNoticeID:     "nid",
	KindOrganisation: "org",
	KindRepository:   "rpo",
	KindCommit:       "sha",
	KindRun:          "run",
	KindRelease:      "rel",
	KindStatus:       "sts",
}

// Kinds returns the published kinds, sorted. It is what the canary set iterates over when it
// asserts that a people identifier did not survive in a hashed form under *any* kind.
func Kinds() []Kind {
	out := make([]Kind, 0, len(kindTag))
	for k := range kindTag {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ErrUnknownKind is returned for a kind with no published tag. It is an error rather than a
// fallback tag: an unpublished kind in a recording is a kind no reader can interpret.
var ErrUnknownKind = errors.New("sanitise: unknown pseudonym kind")

// Prefix returns the token prefix for a kind, or "" when the kind is not published.
func (k Kind) Prefix() string {
	tag, ok := kindTag[k]
	if !ok {
		return ""
	}
	return PseudonymPrefix + tag + "_"
}

// Valid reports whether k is a published kind.
func (k Kind) Valid() bool { _, ok := kindTag[k]; return ok }

// ErrNoKey is returned when no corpus key is configured. It is deliberately fatal rather than
// falling back to an unkeyed hash: an unkeyed pseudonym is a hash anybody can reverse by
// enumeration, and it would look identical in a golden.
var ErrNoKey = errors.New("sanitise: no corpus key configured")

// KeyEnv is the environment variable the corpus key is read from. It is an environment variable
// and not a file in the tree for one reason: FR-135 says neither the key nor any mapping table is
// ever committed, and a path inside the repository is an invitation.
const KeyEnv = "SRE_AGENT_CORPUS_KEY"

// KeyBytes is the required key length. Thirty-two bytes, matching the HMAC block construction,
// and fixed so that a short key — the shape a hand-typed value takes — is refused rather than
// stretched.
const KeyBytes = 32

// Key is one corpus key. It is a value type holding a copy, so that a caller zeroing its own
// buffer does not silently change every pseudonym produced afterwards.
type Key struct {
	material []byte
}

// NewKey returns a Key over material, which must be exactly KeyBytes long.
func NewKey(material []byte) (Key, error) {
	if len(material) == 0 {
		return Key{}, ErrNoKey
	}
	if len(material) != KeyBytes {
		return Key{}, fmt.Errorf("sanitise: corpus key is %d bytes, want %d; a short key is refused "+
			"rather than stretched, because a stretched key looks the same in a golden",
			len(material), KeyBytes)
	}
	return Key{material: append([]byte(nil), material...)}, nil
}

// GenerateKey returns a fresh random corpus key. One corpus, one key: this is called once per
// campaign, and what it returns is stored outside the repository.
func GenerateKey() (Key, error) {
	material := make([]byte, KeyBytes)
	if _, err := rand.Read(material); err != nil {
		return Key{}, fmt.Errorf("sanitise: generating a corpus key: %w", err)
	}
	return Key{material: material}, nil
}

// KeyFromEnv reads the corpus key from KeyEnv, hex-encoded. A missing variable is ErrNoKey, which
// the recording path turns into a refusal to start — the failure a campaign wants is at minute
// zero, not after a day of unkeyed recording.
func KeyFromEnv() (Key, error) {
	raw := strings.TrimSpace(os.Getenv(KeyEnv))
	if raw == "" {
		return Key{}, fmt.Errorf("%w: set %s to %d hex-encoded bytes (aisre feed gcp --print-corpus-key "+
			"generates one); it is never committed (FR-135)", ErrNoKey, KeyEnv, KeyBytes)
	}
	material, err := hex.DecodeString(raw)
	if err != nil {
		return Key{}, fmt.Errorf("sanitise: %s is not hex: %w", KeyEnv, err)
	}
	return NewKey(material)
}

// Configured reports whether this Key can produce a pseudonym. A zero Key cannot, and says so
// rather than producing a token keyed on nothing.
func (k Key) Configured() bool { return len(k.material) == KeyBytes }

// Hex returns the key for storage outside the repository. It exists so that a campaign can write
// the key down once, deliberately, in a place a human chose — and it is the only accessor, so
// every path that could serialise a key is this one call and is grep-able.
func (k Key) Hex() string { return hex.EncodeToString(k.material) }

// Fingerprint identifies which key a recording was made under, without revealing it: an HMAC of a
// fixed label under the key itself. A manifest records it (FR-140) so that two recordings can be
// known to share a pseudonym space — or known not to — after the key has been rotated away.
func (k Key) Fingerprint() string {
	if !k.Configured() {
		return ""
	}
	mac := hmac.New(sha256.New, k.material)
	mac.Write([]byte("sre-agent/corpus-key-fingerprint/v1"))
	return hex.EncodeToString(mac.Sum(nil))[:16]
}

// tokenEncoding is lower-case base32 without padding: [a-z2-7], which is legal in every GCP
// resource name, so a pseudonymised identifier can stand where the real one stood.
var tokenEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// Pseudonym returns the kind-typed, keyed pseudonym of value.
//
// Applying it twice is a no-op: a value already carrying the `px_` prefix is returned unchanged,
// so a payload that passes two sanitisation boundaries is not double-hashed into a token that
// joins to nothing.
func (k Key) Pseudonym(kind Kind, value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if !kind.Valid() {
		return "", fmt.Errorf("%w: %q", ErrUnknownKind, kind)
	}
	if strings.HasPrefix(value, PseudonymPrefix) {
		return value, nil
	}
	if !k.Configured() {
		return "", fmt.Errorf("%w: cannot pseudonymise a %s", ErrNoKey, kind)
	}
	mac := hmac.New(sha256.New, k.material)
	mac.Write([]byte(kind))
	mac.Write([]byte{0x00})
	mac.Write([]byte(value))
	token := strings.ToLower(tokenEncoding.EncodeToString(mac.Sum(nil)))
	return kind.Prefix() + token[:PseudonymDigits], nil
}

// IsPseudonym reports whether value has already been through this function or feature 002's. It
// is the check the commit gates use to tell a token from an identifier.
func IsPseudonym(value string) bool { return strings.HasPrefix(value, PseudonymPrefix) }
