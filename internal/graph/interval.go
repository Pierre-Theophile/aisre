// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
)

// Interval is a half-open time interval [Start, End): the start instant belongs to the
// interval, the end instant does not. It is the Go form of sreagent.graph.v1.Interval and is
// used for both time dimensions the graph carries (constitution II, FR-010):
//
//   - valid time — when the fact was true in the production system;
//   - observed time — when the graph knew the fact.
//
// Half-open intervals tile a timeline with no gaps and no overlaps, so every instant belongs
// to exactly one version of a given fact (docs/schema/temporal-model.md).
//
// A zero End means unbounded: still true (valid) or still believed (observed). End is the only
// bound with a sentinel — Start must always be a real instant, even when StartUnknown is set
// (FR-011, research §3), and Validate enforces that. The algebra below treats a zero Start as
// its literal value (year 1), which orders before every real timestamp, so an accidentally
// unset start behaves as "since forever" rather than panicking; use Validate to reject it.
//
// StartUnknown / EndUnknown carry FR-011's explicit unknown markers. An unknown start means
// the stored Start is the first-observation time, not something a source claimed: queries read
// it as "no earlier than the first observation". An unknown end means "still true", so it
// implies an unbounded End. Guessing a timestamp in place of unknown is prohibited.
//
// The zero Interval is the unset interval: IsZero reports it, Validate rejects it, and it
// marshals to `{}`.
type Interval struct {
	Start        time.Time
	End          time.Time
	StartUnknown bool
	EndUnknown   bool
}

// Errors returned by Interval.Validate.
var (
	// ErrIntervalStartUnset is returned when Start is the zero time. Every interval needs a
	// physical lower bound, including one flagged StartUnknown, where the bound is the first
	// observation time (FR-011, research §3).
	ErrIntervalStartUnset = errors.New("graph: interval start must be set")
	// ErrIntervalNotOrdered is returned when a bounded interval does not satisfy Start < End.
	// Half-open intervals are never empty.
	ErrIntervalNotOrdered = errors.New("graph: interval start must be strictly before end")
	// ErrIntervalUnknownEndBounded is returned when EndUnknown is set on a bounded interval.
	// An unknown end reads as "still true", which is exactly an unbounded end.
	ErrIntervalUnknownEndBounded = errors.New("graph: interval end_unknown requires an unbounded end")
)

// NewInterval builds an interval from two instants. A zero end means unbounded. Both bounds are
// normalized to UTC.
func NewInterval(start, end time.Time) Interval {
	return Interval{Start: start, End: end}.Normalize()
}

// Normalize returns a copy with both bounds expressed in UTC. Canonical serialization and
// storage are UTC-only, so projectors should normalize before comparing or writing.
func (iv Interval) Normalize() Interval {
	if !iv.Start.IsZero() {
		iv.Start = iv.Start.UTC()
	}
	if !iv.End.IsZero() {
		iv.End = iv.End.UTC()
	}
	return iv
}

// IsZero reports whether the interval is the zero value, i.e. no interval at all. Events whose
// interval IsZero must be rejected (FR-010).
func (iv Interval) IsZero() bool {
	return iv.Start.IsZero() && iv.End.IsZero() && !iv.StartUnknown && !iv.EndUnknown
}

// IsCurrent reports whether the interval is unbounded at the end: the fact is still true
// (valid time) or still believed (observed time).
func (iv Interval) IsCurrent() bool { return iv.End.IsZero() }

// Contains reports whether t falls in [Start, End): the start instant is included, the end
// instant is excluded.
func (iv Interval) Contains(t time.Time) bool {
	if t.Before(iv.Start) {
		return false
	}
	if iv.End.IsZero() {
		return true
	}
	return t.Before(iv.End)
}

// Overlaps reports whether the two intervals share at least one instant. It is symmetric.
// Intervals are assumed non-empty (Validate); an empty interval has no defined behaviour here.
func (iv Interval) Overlaps(o Interval) bool {
	if !iv.End.IsZero() && !o.Start.Before(iv.End) {
		return false
	}
	if !o.End.IsZero() && !iv.Start.Before(o.End) {
		return false
	}
	return true
}

// Intersect returns the overlap of the two intervals and whether it is non-empty.
//
// The unknown flags of the result come from the operand that contributed each bound. When both
// operands contribute the same instant, the bound is unknown only if neither operand knows it:
// one source knowing where the boundary is makes it known.
func (iv Interval) Intersect(o Interval) (Interval, bool) {
	var out Interval

	switch {
	case iv.Start.After(o.Start):
		out.Start, out.StartUnknown = iv.Start, iv.StartUnknown
	case o.Start.After(iv.Start):
		out.Start, out.StartUnknown = o.Start, o.StartUnknown
	default:
		out.Start, out.StartUnknown = iv.Start, iv.StartUnknown && o.StartUnknown
	}

	switch {
	case iv.End.IsZero():
		out.End, out.EndUnknown = o.End, o.EndUnknown
	case o.End.IsZero():
		out.End, out.EndUnknown = iv.End, iv.EndUnknown
	case iv.End.Before(o.End):
		out.End, out.EndUnknown = iv.End, iv.EndUnknown
	case o.End.Before(iv.End):
		out.End, out.EndUnknown = o.End, o.EndUnknown
	default:
		out.End, out.EndUnknown = iv.End, iv.EndUnknown && o.EndUnknown
	}

	if !out.End.IsZero() && !out.Start.Before(out.End) {
		return Interval{}, false
	}
	return out, true
}

// Split cuts the interval at instant `at`, returning [Start, at) and [at, End). It reports
// false when `at` does not fall strictly inside the interval, since a split there would
// produce an empty half.
//
// The split point is a known instant, so the left half's end and the right half's start are
// never flagged unknown; the outer bounds keep the original flags. This is the operation the
// projector uses when a new version's valid interval lands inside an existing one.
func (iv Interval) Split(at time.Time) (left, right Interval, ok bool) {
	if !iv.Start.Before(at) {
		return Interval{}, Interval{}, false
	}
	if !iv.End.IsZero() && !at.Before(iv.End) {
		return Interval{}, Interval{}, false
	}
	left = Interval{Start: iv.Start, End: at, StartUnknown: iv.StartUnknown}
	right = Interval{Start: at, End: iv.End, EndUnknown: iv.EndUnknown}
	return left, right, true
}

// Equal reports whether the two intervals denote the same interval, comparing instants by
// time.Time.Equal so that location and monotonic readings do not matter.
func (iv Interval) Equal(o Interval) bool {
	return iv.Start.Equal(o.Start) &&
		iv.End.Equal(o.End) &&
		iv.StartUnknown == o.StartUnknown &&
		iv.EndUnknown == o.EndUnknown
}

// Validate enforces the rules of FR-010 and FR-011:
//
//   - Start must be set, including when StartUnknown is true, where it holds the
//     first-observation time that makes the row indexable.
//   - a bounded interval must satisfy Start < End.
//   - EndUnknown ("still true") requires an unbounded end.
func (iv Interval) Validate() error {
	if iv.Start.IsZero() {
		if iv.StartUnknown {
			return fmt.Errorf("%w: an unknown start still stores the first observation time", ErrIntervalStartUnset)
		}
		return ErrIntervalStartUnset
	}
	if iv.EndUnknown && !iv.End.IsZero() {
		return ErrIntervalUnknownEndBounded
	}
	if !iv.End.IsZero() && !iv.Start.Before(iv.End) {
		return fmt.Errorf("%w: [%s, %s)", ErrIntervalNotOrdered, formatTimestamp(iv.Start), formatTimestamp(iv.End))
	}
	return nil
}

// String renders the interval in mathematical notation, `∞` for an unbounded end and a
// trailing `?` on a bound flagged unknown, e.g. `[2026-09-01T13:00:00Z?, ∞)`.
func (iv Interval) String() string {
	start := "∅"
	if !iv.Start.IsZero() {
		start = formatTimestamp(iv.Start)
	}
	if iv.StartUnknown {
		start += "?"
	}
	end := "∞"
	if !iv.End.IsZero() {
		end = formatTimestamp(iv.End)
	}
	if iv.EndUnknown {
		end += "?"
	}
	return "[" + start + ", " + end + ")"
}

// Proto converts the interval to its published protobuf form. Unset bounds stay unset.
func (iv Interval) Proto() *graphv1.Interval {
	p := &graphv1.Interval{StartUnknown: iv.StartUnknown, EndUnknown: iv.EndUnknown}
	if !iv.Start.IsZero() {
		p.Start = timestamppb.New(iv.Start.UTC())
	}
	if !iv.End.IsZero() {
		p.End = timestamppb.New(iv.End.UTC())
	}
	return p
}

// IntervalFromProto converts a published protobuf interval. A nil message yields the zero
// Interval, which Validate rejects — that is how a missing interval reaches the caller.
func IntervalFromProto(p *graphv1.Interval) Interval {
	if p == nil {
		return Interval{}
	}
	iv := Interval{StartUnknown: p.GetStartUnknown(), EndUnknown: p.GetEndUnknown()}
	if ts := p.GetStart(); ts != nil {
		iv.Start = ts.AsTime()
	}
	if ts := p.GetEnd(); ts != nil {
		iv.End = ts.AsTime()
	}
	return iv
}

// MarshalJSON writes the canonical JSON form: keys sorted, no insignificant whitespace,
// RFC 3339 UTC timestamps, the end omitted when unbounded and each unknown flag emitted only
// when true. The key spelling and timestamp format are those of the canonical protobuf JSON
// mapping, so marshalling an Interval and marshalling its Proto() produce identical bytes.
func (iv Interval) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	first := true
	write := func(key, value string) {
		if !first {
			b.WriteByte(',')
		}
		first = false
		b.WriteByte('"')
		b.WriteString(key)
		b.WriteString(`":`)
		b.WriteString(value)
	}
	// Keys in byte order: end, endUnknown, start, startUnknown.
	if !iv.End.IsZero() {
		write("end", `"`+formatTimestamp(iv.End)+`"`)
	}
	if iv.EndUnknown {
		write("endUnknown", "true")
	}
	if !iv.Start.IsZero() {
		write("start", `"`+formatTimestamp(iv.Start)+`"`)
	}
	if iv.StartUnknown {
		write("startUnknown", "true")
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// UnmarshalJSON reads the form MarshalJSON writes. Both the canonical lowerCamel key spelling
// and the protobuf field names (`start_unknown`) are accepted, as protojson accepts both.
func (iv *Interval) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("graph: interval: %w", err)
	}
	var out Interval
	for key, value := range raw {
		switch key {
		case "start", "end":
			t, err := parseTimestamp(value)
			if err != nil {
				return fmt.Errorf("graph: interval %s: %w", key, err)
			}
			if key == "start" {
				out.Start = t
			} else {
				out.End = t
			}
		case "startUnknown", "start_unknown", "endUnknown", "end_unknown":
			var flag bool
			if err := json.Unmarshal(value, &flag); err != nil {
				return fmt.Errorf("graph: interval %s: %w", key, err)
			}
			if key == "startUnknown" || key == "start_unknown" {
				out.StartUnknown = flag
			} else {
				out.EndUnknown = flag
			}
		default:
			return fmt.Errorf("graph: interval: unknown field %q", key)
		}
	}
	*iv = out
	return nil
}

func parseTimestamp(raw json.RawMessage) (time.Time, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return time.Time{}, err
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("not an RFC 3339 timestamp: %w", err)
	}
	return t.UTC(), nil
}
