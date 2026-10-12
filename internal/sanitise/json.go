// SPDX-License-Identifier: Apache-2.0

package sanitise

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// A whole JSON payload through the table, with every value keeping its shape (004 T104, FR-062,
// FR-063).
//
// ---------------------------------------------------------------------------------------------
// Why a second walker
//
// `Fields` takes a flattened map of strings, which suits an audit entry whose identifiers are all
// resource names. A deploy payload is not like that, and three of its identifiers break a string
// token outright:
//
//   - a GitHub repository or deployment id is a JSON NUMBER, decoded into an int64. A `px_` token in
//     its place is a payload the connector can no longer read;
//   - a commit is forty hex characters, and the claim normaliser refuses anything else — so a token
//     in its place is a commit C8 can no longer join on, which is the join FR-063 says must survive;
//   - a deployment's own URL is where GitHub states its repository. Pseudonymise the repository's
//     name in one field and leave the URL alone, and the connector reads two different repositories.
//
// So a Pseudonym rule carries a SHAPE: the `px_` token (the default), decimal digits, hex of the
// same length, or a template that rewrites a URL segment by segment with each segment's own kind.
// Every shape is the same keyed HMAC underneath, so one value pseudonymises to one output wherever it
// appears — the property that makes the recorded graph the same graph (§2.3 property 1).
//
// # What stays the same
//
// The people check runs first and wins over any row. A field the table does not name fails the
// whole payload (UnassignedError), and a recorder turns that into a documented drop, never a partial
// write. A verbatim string that carries a person is dropped anyway. Canaries are checked on every
// value that is kept or pseudonymised. The walk is sorted, so a refusal names the same field twice.

// Shape is the form a pseudonym takes.
type Shape uint8

// The shapes.
const (
	// ShapeToken is `px_<kind>_<base32>`, the form every pseudonym had before T104.
	ShapeToken Shape = iota
	// ShapeDigits is fifteen decimal digits. A JSON number stays a number; a string stays a string.
	ShapeDigits
	// ShapeHex is lower-case hex of the input's own length. An input that is not hex is dropped
	// rather than hashed into something that looks like a commit: `target_commitish` is a branch
	// name as often as it is a sha, and a branch name is free text.
	ShapeHex
	// ShapeTemplate rewrites a URL or a path against Rule.Template. A value that does not match the
	// template is dropped: a URL of an unexpected shape is one whose identifiers nobody has located.
	ShapeTemplate
	// ShapeLabel is `px-<tag>-<base32>`, lower-case letters, digits and hyphens only: the shape of a
	// GCP project id, a Cloud SQL instance name and a Cloud Run service. A token's underscores fail the
	// patterns a GCP connection name is parsed by (`<project>:<region>:<instance>`), so a recording
	// that pseudonymised them as tokens would silently lose the database edges built from them.
	ShapeLabel
)

// LabelPrefix opens every ShapeLabel pseudonym.
const LabelPrefix = "px-"

// Label returns the ShapeLabel pseudonym of value: the same keyed digest as Pseudonym, written in a
// DNS label's alphabet. Applying it twice is a no-op.
func (k Key) Label(kind Kind, value string) (string, error) {
	if value == "" || strings.HasPrefix(value, LabelPrefix) {
		return value, nil
	}
	if !kind.Valid() {
		return "", fmt.Errorf("%w: %q", ErrUnknownKind, kind)
	}
	sum, err := k.mac(kind, value)
	if err != nil {
		return "", err
	}
	token := strings.ToLower(tokenEncoding.EncodeToString(sum))
	return LabelPrefix + kindTag[kind] + "-" + token[:PseudonymDigits], nil
}

// mac is the HMAC every shape is cut from, domain-separated by kind exactly as Pseudonym is.
func (k Key) mac(kind Kind, value string) ([]byte, error) {
	if !kind.Valid() {
		return nil, fmt.Errorf("%w: %q", ErrUnknownKind, kind)
	}
	if !k.Configured() {
		return nil, fmt.Errorf("%w: cannot pseudonymise a %s", ErrNoKey, kind)
	}
	m := hmac.New(sha256.New, k.material)
	m.Write([]byte(kind))
	m.Write([]byte{0x00})
	m.Write([]byte(value))
	return m.Sum(nil), nil
}

// Digits returns the ShapeDigits pseudonym of value: fifteen decimal digits, no leading zero. Fifteen
// keeps it under 2^53, so a JavaScript reader and a float64 round-trip keep it exact, and is wide
// enough that a collision is as unlikely as a token's.
func (k Key) Digits(kind Kind, value string) (string, error) {
	sum, err := k.mac(kind, value)
	if err != nil {
		return "", err
	}
	const lowest = 100_000_000_000_000 // 10^14, the smallest fifteen-digit number
	n := binary.BigEndian.Uint64(sum[:8]) % (9 * lowest)
	return fmt.Sprintf("%d", n+lowest), nil
}

// HexOf returns the ShapeHex pseudonym of value, or false where value is not hex.
func (k Key) HexOf(kind Kind, value string) (string, bool, error) {
	if value == "" || strings.Trim(strings.ToLower(value), "0123456789abcdef") != "" {
		return "", false, nil
	}
	sum, err := k.mac(kind, strings.ToLower(value))
	if err != nil {
		return "", false, err
	}
	out := hex.EncodeToString(sum)
	for len(out) < len(value) {
		more, _ := k.mac(kind, out)
		out += hex.EncodeToString(more)
	}
	return out[:len(value)], true, nil
}

// template is a compiled ShapeTemplate pattern.
type template struct {
	re     *regexp.Regexp
	fields []templateField
}

type templateField struct {
	kind  Kind
	shape Shape
	// keep, when set, keeps the segment verbatim if it matches, and fails the template if not.
	keep *regexp.Regexp
}

// placeholder is `{kind}` or `{kind:digits}` / `{kind:hex}` / `{kind:label}`, or `{region}`.
var placeholder = regexp.MustCompile(`\{([a-z_]+)(?::(digits|hex|label))?\}`)

// regionPlaceholder keeps a GCP location verbatim: a region (`europe-west1`), a zone
// (`europe-west1-b`), a multi-region (`europe`, `us`) or `global`. A location is the platform's
// geography, not anybody's identifier, and the connector validates it by shape — a pseudonymised
// region would fail every parse that reads one. A segment that is NOT a location fails the template,
// so the field is dropped rather than an identifier being kept under the region's name.
const regionPlaceholder = "region"

var locationShape = regexp.MustCompile(`^(?:[a-z]+(?:-[a-z]+[0-9]+(?:-[a-z])?)?)$`)

// TemplateAlternatives separates alternative patterns in one Rule.Template. The first that matches
// rewrites the value; a field whose values come in several shapes — an audit entry's resource name
// is a service, a revision, an instance or a project — is otherwise dropped in all but one.
const TemplateAlternatives = " | "

// compileTemplate parses a pattern. Literal text is matched exactly; each placeholder matches one
// path segment (no `/`, `?` or `#`), and the whole value must match.
func compileTemplate(pattern string) (*template, error) {
	var re strings.Builder
	var fields []templateField
	re.WriteString("^")
	last := 0
	for _, loc := range placeholder.FindAllStringSubmatchIndex(pattern, -1) {
		re.WriteString(regexp.QuoteMeta(pattern[last:loc[0]]))
		kind := Kind(pattern[loc[2]:loc[3]])
		if kind == regionPlaceholder {
			if loc[4] >= 0 {
				return nil, fmt.Errorf("sanitise: template %q gives {region} a shape; a kept segment has none", pattern)
			}
			fields = append(fields, templateField{keep: locationShape})
			re.WriteString(`([^/?#:]+)`)
			last = loc[1]
			continue
		}
		if !kind.Valid() {
			return nil, fmt.Errorf("sanitise: template %q names kind %q, which is not published", pattern, kind)
		}
		shape := ShapeToken
		if loc[4] >= 0 {
			switch pattern[loc[4]:loc[5]] {
			case "digits":
				shape = ShapeDigits
			case "hex":
				shape = ShapeHex
			case "label":
				shape = ShapeLabel
			}
		}
		fields = append(fields, templateField{kind: kind, shape: shape})
		re.WriteString(`([^/?#:@]+)`)
		last = loc[1]
	}
	re.WriteString(regexp.QuoteMeta(pattern[last:]))
	re.WriteString("$")
	if len(fields) == 0 {
		return nil, fmt.Errorf("sanitise: template %q has no placeholder; a template that pseudonymises "+
			"nothing is a verbatim rule spelled wrong", pattern)
	}
	compiled, err := regexp.Compile(re.String())
	if err != nil {
		return nil, fmt.Errorf("sanitise: template %q: %w", pattern, err)
	}
	return &template{re: compiled, fields: fields}, nil
}

// shaped applies one shape to one string value. ok=false means the value is dropped.
func (s *Sanitiser) shaped(rule Rule, value string) (string, bool, error) {
	switch rule.Shape {
	case ShapeDigits:
		if value == "" || strings.Trim(value, "0123456789") != "" {
			return "", false, nil
		}
		out, err := s.key.Digits(rule.Kind, value)
		return out, err == nil, err
	case ShapeHex:
		return s.key.HexOf(rule.Kind, value)
	case ShapeLabel:
		out, err := s.key.Label(rule.Kind, value)
		return out, err == nil, err
	case ShapeTemplate:
		for _, pattern := range strings.Split(rule.Template, TemplateAlternatives) {
			out, ok, err := s.applyTemplate(pattern, value)
			if err != nil || ok {
				return out, ok, err
			}
		}
		return "", false, nil
	default:
		if caseFolded[rule.Kind] {
			value = strings.ToLower(value)
		}
		out, err := s.key.Pseudonym(rule.Kind, value)
		return out, err == nil, err
	}
}

// applyTemplate rewrites value against one pattern; ok=false when it does not match.
func (s *Sanitiser) applyTemplate(pattern, value string) (string, bool, error) {
	t, err := compileTemplate(pattern)
	if err != nil {
		return "", false, err
	}
	match := t.re.FindStringSubmatchIndex(value)
	if match == nil {
		return "", false, nil
	}
	var out strings.Builder
	last := 0
	for i, field := range t.fields {
		start, end := match[2*(i+1)], match[2*(i+1)+1]
		out.WriteString(value[last:start])
		segment := value[start:end]
		if field.keep != nil {
			if !field.keep.MatchString(segment) {
				return "", false, nil
			}
			out.WriteString(segment)
			last = end
			continue
		}
		shapedSegment, ok, err := s.shaped(Rule{Kind: field.kind, Shape: field.shape}, segment)
		if err != nil || !ok {
			return "", false, err
		}
		out.WriteString(shapedSegment)
		last = end
	}
	out.WriteString(value[last:])
	return out.String(), true, nil
}

// caseFolded are the kinds whose names the platform compares without case: GitHub treats `Acme` and
// `acme` as one organisation, and a pseudonym that did not would give it two tokens.
var caseFolded = map[Kind]bool{KindOrganisation: true, KindRepository: true}

// JSON sanitises one JSON payload. Every field is addressed as `root.path`, with array elements
// collapsed to `[]`, so a payload kind's rows cannot collide with another source's: GitHub's
// `deployments[].id` is `github.deployments[].id` and nothing else answers to it.
//
// An error means the payload must not be recorded at all: a field the table does not name, or a
// canary that reached a kept value.
func (s *Sanitiser) JSON(root string, raw []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("sanitise: %s is not JSON: %w", root, err)
	}
	out, keep, err := s.walk(root, root, value)
	if err != nil {
		return nil, err
	}
	if !keep {
		out = nil
	}
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(out); err != nil {
		return nil, fmt.Errorf("sanitise: encoding %s: %w", root, err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func (s *Sanitiser) walk(where, path string, value any) (any, bool, error) {
	switch v := value.(type) {
	case map[string]any:
		// A subtree rule decides the whole object at once, so `x.*` dropped means the object is
		// never descended into — a vendor's unbounded map needs no row per key.
		if rule, ok := s.policy.prefixRule(CanonicalField(path + ".")); ok && rule.Disposition == Dropped &&
			!s.policy.hasRowUnder(CanonicalField(path+".")) {
			s.drops.Record(path, rule.Why)
			return nil, false, nil
		}
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		out := make(map[string]any, len(v))
		for _, key := range keys {
			child, keep, err := s.walk(where, path+"."+key, v[key])
			if err != nil {
				return nil, false, err
			}
			if keep {
				out[key] = child
			}
		}
		return out, true, nil
	case []any:
		out := make([]any, 0, len(v))
		for _, element := range v {
			child, keep, err := s.walk(where, path+"[]", element)
			if err != nil {
				return nil, false, err
			}
			if keep {
				out = append(out, child)
			}
		}
		return out, true, nil
	default:
		return s.leaf(where, path, v)
	}
}

// leaf applies the table to one scalar.
func (s *Sanitiser) leaf(where, path string, value any) (any, bool, error) {
	if IsPeopleField(path) {
		s.drops.Record(path, "names a person: "+ErrPeopleNeverHashed.Error())
		return nil, false, nil
	}
	rule, err := s.policy.Field(path, where)
	if err != nil {
		return nil, false, err
	}
	switch rule.Disposition {
	case Dropped:
		s.drops.Record(path, rule.Why)
		return nil, false, nil
	case Verbatim:
		if text, ok := value.(string); ok {
			if LooksLikePerson(text) {
				s.drops.Record(path, "on the verbatim allowlist, but this value carried a person")
				return nil, false, nil
			}
			if err := s.checkCanary(where, path, text); err != nil {
				return nil, false, err
			}
		}
		return value, true, nil
	case Pseudonym:
		switch v := value.(type) {
		case nil:
			return nil, true, nil
		case string:
			if v == "" {
				return v, true, nil
			}
			if err := s.checkCanary(where, path, v); err != nil {
				return nil, false, err
			}
			out, ok, err := s.shaped(rule, v)
			if err != nil {
				return nil, false, fmt.Errorf("sanitise: %s in %s: %w", path, where, err)
			}
			if !ok {
				s.drops.Record(path, "did not have the shape its pseudonym rule expects, so it was dropped rather than guessed at")
				return nil, false, nil
			}
			return out, true, nil
		case json.Number:
			if rule.Shape != ShapeDigits {
				return nil, false, fmt.Errorf("sanitise: %s in %s is a number and its rule is not ShapeDigits; "+
					"a token in a numeric field is a payload the connector cannot read", path, where)
			}
			out, ok, err := s.shaped(rule, v.String())
			if err != nil {
				return nil, false, fmt.Errorf("sanitise: %s in %s: %w", path, where, err)
			}
			if !ok {
				s.drops.Record(path, "a non-integer where an integer id was expected")
				return nil, false, nil
			}
			return json.Number(out), true, nil
		default:
			return nil, false, fmt.Errorf("sanitise: %s in %s is a %T and its rule pseudonymises it; only a "+
				"string or an integer can be", path, where, value)
		}
	default:
		return nil, false, &UnassignedError{Field: path, Where: where}
	}
}

// hasRowUnder reports whether the table names any field below prefix, other than the subtree rule
// itself — in which case the walk descends, so the more specific rows get their say.
func (p *Policy) hasRowUnder(prefix string) bool {
	for path := range p.fields {
		if strings.HasPrefix(path, prefix) && path != prefix+"*" {
			return true
		}
	}
	return false
}

// Identifier is the token a Pseudonym row of this kind gives value, exactly as JSON gives it —
// case-folded where the kind is. It is how configuration that NAMES recorded identifiers (an
// operator's repository→service mapping) is pseudonymised to match the recording it configures,
// without a second derivation that could drift from this one.
func (s *Sanitiser) Identifier(kind Kind, value string) (string, error) {
	out, _, err := s.shaped(Rule{Kind: kind}, value)
	return out, err
}

// LabelIdentifier is the ShapeLabel pseudonym of value under kind: what a GCP project or a Cloud SQL
// instance becomes in a recording, so a recording's own configuration (its scope) can name them the
// way its payloads do.
func (s *Sanitiser) LabelIdentifier(kind Kind, value string) (string, error) {
	return s.key.Label(kind, value)
}

// Hex is the ShapeHex pseudonym of value under kind, or false where value is not hex. It is how a
// commit written inside a string a table cannot address (a Datadog event's `git.commit.sha:<sha>` tag)
// gets the SAME pseudonym the commit has in a field the table does address, so the two still join on it.
func (s *Sanitiser) Hex(kind Kind, value string) (string, bool, error) {
	return s.shaped(Rule{Kind: kind, Shape: ShapeHex}, value)
}
