// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
)

// Canonical serialization (contracts/fixture-format.md §"Canonical serialization").
//
// Golden fixtures are compared byte for byte, so "the same graph" has to mean "the same
// bytes". This file is the single definition of what that means:
//
//   - JSON, UTF-8, object keys sorted by byte order, no insignificant whitespace;
//   - timestamps RFC 3339 in UTC with a `Z` suffix and nanosecond precision;
//   - protobuf messages rendered through the canonical protobuf JSON mapping, lowerCamel field
//     names, unpopulated fields omitted;
//   - arrays keep their order, so callers order node and edge lists by version_id first with
//     SortNodeVersions / SortEdgeVersions.
//
// protojson deliberately randomizes whitespace in its output to stop callers depending on its
// byte stability, and it does not promise key order either, so every value — protobuf or not —
// is decoded and re-encoded here by writeCanonical. Encoding is not HTML-escaped: `<` stays
// `<`, which keeps selectors and PromQL readable in golden diffs.

var protoJSON = protojson.MarshalOptions{
	EmitUnpopulated: false,
	UseProtoNames:   false, // lowerCamel, the canonical protobuf JSON spelling
}

// CanonicalJSON renders v in the canonical form described above.
//
// Protobuf messages go through protojson; anything else goes through encoding/json, so a type
// with its own MarshalJSON (Interval, for instance) keeps control of its shape. Both outputs
// are then re-encoded with sorted keys and normalized timestamps, so the two paths agree:
// CanonicalJSON(iv) and CanonicalJSON(iv.Proto()) return the same bytes.
func CanonicalJSON(v any) ([]byte, error) {
	var (
		raw []byte
		err error
	)
	if m, ok := v.(proto.Message); ok {
		raw, err = protoJSON.Marshal(m)
	} else {
		raw, err = json.Marshal(v)
	}
	if err != nil {
		return nil, fmt.Errorf("graph: canonical json: %w", err)
	}
	return canonicalizeBytes(raw)
}

// CanonicalJSONL renders each item as one canonical JSON document per line, every line
// terminated by `\n`. This is the format of a fixture's events.jsonl.
func CanonicalJSONL(items []proto.Message) ([]byte, error) {
	var out bytes.Buffer
	for i, item := range items {
		line, err := CanonicalJSON(item)
		if err != nil {
			return nil, fmt.Errorf("graph: canonical jsonl: item %d: %w", i, err)
		}
		out.Write(line)
		out.WriteByte('\n')
	}
	return out.Bytes(), nil
}

// SortNodeVersions orders node versions by version_id in place, the array order required of
// canonical outputs. The sort is stable, so equal ids keep their relative order.
func SortNodeVersions(versions []*graphv1.NodeVersion) {
	slices.SortStableFunc(versions, func(a, b *graphv1.NodeVersion) int {
		return strings.Compare(a.GetVersionId(), b.GetVersionId())
	})
}

// SortEdgeVersions orders edge versions by version_id in place, as SortNodeVersions does.
func SortEdgeVersions(versions []*graphv1.EdgeVersion) {
	slices.SortStableFunc(versions, func(a, b *graphv1.EdgeVersion) int {
		return strings.Compare(a.GetVersionId(), b.GetVersionId())
	})
}

// canonicalizeBytes re-encodes arbitrary JSON into the canonical form. It is idempotent.
func canonicalizeBytes(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // keep number literals exactly as written; never round-trip through float64
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, fmt.Errorf("graph: canonical json: decode: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("graph: canonical json: trailing data after top-level value")
	}
	var out bytes.Buffer
	if err := writeCanonical(&out, value); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func writeCanonical(w *bytes.Buffer, value any) error {
	switch v := value.(type) {
	case nil:
		w.WriteString("null")
	case bool:
		if v {
			w.WriteString("true")
		} else {
			w.WriteString("false")
		}
	case json.Number:
		w.WriteString(v.String())
	case string:
		return writeCanonicalString(w, v)
	case []any:
		w.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				w.WriteByte(',')
			}
			if err := writeCanonical(w, item); err != nil {
				return err
			}
		}
		w.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		slices.Sort(keys) // byte order, which for the ASCII keys of this schema is also lexical
		w.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				w.WriteByte(',')
			}
			if err := writeCanonicalString(w, key); err != nil {
				return err
			}
			w.WriteByte(':')
			if err := writeCanonical(w, v[key]); err != nil {
				return err
			}
		}
		w.WriteByte('}')
	default:
		return fmt.Errorf("graph: canonical json: unexpected value of type %T", value)
	}
	return nil
}

// writeCanonicalString writes a JSON string without HTML escaping, normalizing any value that
// is itself an RFC 3339 timestamp to canonical UTC. Normalizing at the string level is what
// makes a time.Time buried in an arbitrary Go struct come out in the same spelling as the same
// instant carried by a protobuf Timestamp; it rewrites nothing else, since re-encoding a
// timestamp preserves the instant it denotes.
func writeCanonicalString(w *bytes.Buffer, s string) error {
	if t, ok := asTimestamp(s); ok {
		s = formatTimestamp(t)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return fmt.Errorf("graph: canonical json: encode string: %w", err)
	}
	w.Write(bytes.TrimRight(buf.Bytes(), "\n"))
	return nil
}

// asTimestamp reports whether s is an RFC 3339 timestamp, and parses it if so. The cheap length
// and shape checks keep the parser off the hot path for ordinary strings.
func asTimestamp(s string) (time.Time, bool) {
	if len(s) < len("2006-01-02T15:04:05Z") || len(s) > 40 || s[4] != '-' || s[10] != 'T' {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// formatTimestamp renders t as RFC 3339 in UTC with a `Z` suffix, using zero, three, six or
// nine fractional digits — exactly the rendering the canonical protobuf JSON mapping gives a
// google.protobuf.Timestamp. Matching protojson digit for digit is what lets a domain type and
// its protobuf counterpart serialize to the same bytes; time.RFC3339Nano, which trims trailing
// zeros, would not.
func formatTimestamp(t time.Time) string {
	t = t.UTC()
	base := t.Format("2006-01-02T15:04:05")
	switch nanos := t.Nanosecond(); {
	case nanos == 0:
		return base + "Z"
	case nanos%1e6 == 0:
		return fmt.Sprintf("%s.%03dZ", base, nanos/1e6)
	case nanos%1e3 == 0:
		return fmt.Sprintf("%s.%06dZ", base, nanos/1e3)
	default:
		return fmt.Sprintf("%s.%09dZ", base, nanos)
	}
}
