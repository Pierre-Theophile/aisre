// SPDX-License-Identifier: Apache-2.0

package graph_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"pgregory.net/rapid"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
)

// The worked example from docs/schema/temporal-model.md.
var (
	t1240 = time.Date(2026, 9, 1, 12, 40, 0, 0, time.UTC)
	t1300 = time.Date(2026, 9, 1, 13, 0, 0, 0, time.UTC)
	t1301 = time.Date(2026, 9, 1, 13, 1, 0, 0, time.UTC)
	t1420 = time.Date(2026, 9, 1, 14, 20, 0, 0, time.UTC)
	t1432 = time.Date(2026, 9, 1, 14, 32, 0, 0, time.UTC)
	t1500 = time.Date(2026, 9, 1, 15, 0, 0, 0, time.UTC)
)

func TestIntervalContains(t *testing.T) {
	t.Parallel()

	bounded := graph.NewInterval(t1300, t1420)
	unbounded := graph.NewInterval(t1300, time.Time{})

	tests := []struct {
		name string
		iv   graph.Interval
		at   time.Time
		want bool
	}{
		{"start is inclusive", bounded, t1300, true},
		{"inside", bounded, t1432.Add(-time.Hour), true},
		{"one nanosecond before the end", bounded, t1420.Add(-time.Nanosecond), true},
		{"end is exclusive", bounded, t1420, false},
		{"after the end", bounded, t1432, false},
		{"before the start", bounded, t1240, false},
		{"unbounded contains the far future", unbounded, t1300.AddDate(100, 0, 0), true},
		{"unbounded start is still inclusive", unbounded, t1300, true},
		{"unbounded excludes before the start", unbounded, t1240, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.iv.Contains(tc.at); got != tc.want {
				t.Errorf("%s.Contains(%s) = %v, want %v", tc.iv, tc.at.Format(time.RFC3339), got, tc.want)
			}
		})
	}
}

func TestIntervalOverlaps(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		a, b graph.Interval
		want bool
	}{
		{"disjoint", graph.NewInterval(t1240, t1300), graph.NewInterval(t1420, t1500), false},
		{"touching is not overlapping", graph.NewInterval(t1240, t1300), graph.NewInterval(t1300, t1420), false},
		{"partial", graph.NewInterval(t1240, t1420), graph.NewInterval(t1300, t1500), true},
		{"contained", graph.NewInterval(t1240, t1500), graph.NewInterval(t1300, t1420), true},
		{"identical", graph.NewInterval(t1300, t1420), graph.NewInterval(t1300, t1420), true},
		{"both unbounded", graph.NewInterval(t1240, time.Time{}), graph.NewInterval(t1500, time.Time{}), true},
		{"unbounded meets bounded before it", graph.NewInterval(t1500, time.Time{}), graph.NewInterval(t1240, t1300), false},
		{"unbounded meets bounded across it", graph.NewInterval(t1300, time.Time{}), graph.NewInterval(t1240, t1420), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.a.Overlaps(tc.b); got != tc.want {
				t.Errorf("%s.Overlaps(%s) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
			if got := tc.b.Overlaps(tc.a); got != tc.want {
				t.Errorf("%s.Overlaps(%s) = %v, want %v (not symmetric)", tc.b, tc.a, got, tc.want)
			}
		})
	}
}

func TestIntervalIntersect(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		a, b   graph.Interval
		want   graph.Interval
		wantOK bool
	}{
		{
			name:   "partial overlap",
			a:      graph.NewInterval(t1240, t1420),
			b:      graph.NewInterval(t1300, t1500),
			want:   graph.NewInterval(t1300, t1420),
			wantOK: true,
		},
		{
			name:   "one unbounded",
			a:      graph.NewInterval(t1240, time.Time{}),
			b:      graph.NewInterval(t1300, t1420),
			want:   graph.NewInterval(t1300, t1420),
			wantOK: true,
		},
		{
			name:   "both unbounded stays unbounded",
			a:      graph.NewInterval(t1240, time.Time{}),
			b:      graph.NewInterval(t1300, time.Time{}),
			want:   graph.NewInterval(t1300, time.Time{}),
			wantOK: true,
		},
		{
			name:   "touching intervals do not intersect",
			a:      graph.NewInterval(t1240, t1300),
			b:      graph.NewInterval(t1300, t1420),
			wantOK: false,
		},
		{
			name:   "disjoint",
			a:      graph.NewInterval(t1240, t1300),
			b:      graph.NewInterval(t1420, t1500),
			wantOK: false,
		},
		{
			name:   "the contributing operand supplies the unknown flag",
			a:      graph.Interval{Start: t1240, StartUnknown: true},
			b:      graph.Interval{Start: t1300},
			want:   graph.Interval{Start: t1300},
			wantOK: true,
		},
		{
			name:   "an unknown bound survives when it wins",
			a:      graph.Interval{Start: t1300, StartUnknown: true},
			b:      graph.Interval{Start: t1240},
			want:   graph.Interval{Start: t1300, StartUnknown: true},
			wantOK: true,
		},
		{
			name:   "on a tie one knowing operand makes the bound known",
			a:      graph.Interval{Start: t1300, StartUnknown: true},
			b:      graph.Interval{Start: t1300},
			want:   graph.Interval{Start: t1300},
			wantOK: true,
		},
		{
			name:   "unknown end survives an unbounded tie",
			a:      graph.Interval{Start: t1300, EndUnknown: true},
			b:      graph.Interval{Start: t1240, EndUnknown: true},
			want:   graph.Interval{Start: t1300, EndUnknown: true},
			wantOK: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := tc.a.Intersect(tc.b)
			if ok != tc.wantOK {
				t.Fatalf("%s.Intersect(%s) ok = %v, want %v", tc.a, tc.b, ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if !got.Equal(tc.want) {
				t.Errorf("%s.Intersect(%s) = %s, want %s", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestIntervalSplit(t *testing.T) {
	t.Parallel()

	t.Run("inside", func(t *testing.T) {
		t.Parallel()
		iv := graph.Interval{Start: t1300, End: t1500, StartUnknown: true}
		left, right, ok := iv.Split(t1420)
		if !ok {
			t.Fatalf("%s.Split(14:20) not ok", iv)
		}
		wantLeft := graph.Interval{Start: t1300, End: t1420, StartUnknown: true}
		wantRight := graph.Interval{Start: t1420, End: t1500}
		if !left.Equal(wantLeft) {
			t.Errorf("left = %s, want %s", left, wantLeft)
		}
		if !right.Equal(wantRight) {
			t.Errorf("right = %s, want %s", right, wantRight)
		}
	})

	t.Run("unbounded keeps its unknown end on the right", func(t *testing.T) {
		t.Parallel()
		iv := graph.Interval{Start: t1300, EndUnknown: true}
		left, right, ok := iv.Split(t1420)
		if !ok {
			t.Fatalf("%s.Split(14:20) not ok", iv)
		}
		if !left.Equal(graph.Interval{Start: t1300, End: t1420}) {
			t.Errorf("left = %s", left)
		}
		if !right.Equal(graph.Interval{Start: t1420, EndUnknown: true}) {
			t.Errorf("right = %s", right)
		}
	})

	rejects := []struct {
		name string
		iv   graph.Interval
		at   time.Time
	}{
		{"at the start", graph.NewInterval(t1300, t1500), t1300},
		{"at the end", graph.NewInterval(t1300, t1500), t1500},
		{"before the start", graph.NewInterval(t1300, t1500), t1240},
		{"after the end", graph.NewInterval(t1300, t1500), t1500.Add(time.Hour)},
		{"at the start of an unbounded interval", graph.NewInterval(t1300, time.Time{}), t1300},
	}
	for _, tc := range rejects {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			t.Parallel()
			if _, _, ok := tc.iv.Split(tc.at); ok {
				t.Errorf("%s.Split(%s) unexpectedly ok", tc.iv, tc.at.Format(time.RFC3339))
			}
		})
	}
}

func TestIntervalValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		iv      graph.Interval
		wantErr error
	}{
		{"bounded", graph.NewInterval(t1300, t1420), nil},
		{"unbounded", graph.NewInterval(t1300, time.Time{}), nil},
		{"unknown start still carries the first observation", graph.Interval{Start: t1301, StartUnknown: true}, nil},
		{"unknown end on an unbounded interval", graph.Interval{Start: t1301, EndUnknown: true}, nil},
		{"zero interval", graph.Interval{}, graph.ErrIntervalStartUnset},
		{"unknown start without a start", graph.Interval{StartUnknown: true}, graph.ErrIntervalStartUnset},
		{"end before start", graph.NewInterval(t1420, t1300), graph.ErrIntervalNotOrdered},
		{"empty interval", graph.NewInterval(t1300, t1300), graph.ErrIntervalNotOrdered},
		{"unknown end on a bounded interval", graph.Interval{Start: t1300, End: t1420, EndUnknown: true}, graph.ErrIntervalUnknownEndBounded},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.iv.Validate()
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("%s.Validate() = %v, want %v", tc.iv, err, tc.wantErr)
			}
		})
	}
}

func TestIntervalIsCurrentAndIsZero(t *testing.T) {
	t.Parallel()

	if !graph.NewInterval(t1300, time.Time{}).IsCurrent() {
		t.Error("an unbounded interval must be current")
	}
	if graph.NewInterval(t1300, t1420).IsCurrent() {
		t.Error("a bounded interval must not be current")
	}
	if !(graph.Interval{}).IsZero() {
		t.Error("the zero value must be IsZero")
	}
	if (graph.Interval{StartUnknown: true}).IsZero() {
		t.Error("a flag alone must not read as the zero interval")
	}
}

func TestIntervalString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		iv   graph.Interval
		want string
	}{
		{graph.NewInterval(t1300, t1420), "[2026-09-01T13:00:00Z, 2026-09-01T14:20:00Z)"},
		{graph.NewInterval(t1300, time.Time{}), "[2026-09-01T13:00:00Z, ∞)"},
		{graph.Interval{Start: t1301, StartUnknown: true}, "[2026-09-01T13:01:00Z?, ∞)"},
		{graph.Interval{Start: t1301, EndUnknown: true}, "[2026-09-01T13:01:00Z, ∞?)"},
		{graph.Interval{}, "[∅, ∞)"},
	}
	for _, tc := range tests {
		if got := tc.iv.String(); got != tc.want {
			t.Errorf("String() = %q, want %q", got, tc.want)
		}
	}
}

func TestIntervalJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		iv   graph.Interval
		want string
	}{
		{
			name: "bounded, keys sorted",
			iv:   graph.NewInterval(t1300, t1420),
			want: `{"end":"2026-09-01T14:20:00Z","start":"2026-09-01T13:00:00Z"}`,
		},
		{
			name: "unbounded end is omitted",
			iv:   graph.NewInterval(t1300, time.Time{}),
			want: `{"start":"2026-09-01T13:00:00Z"}`,
		},
		{
			name: "flags only when true",
			iv:   graph.Interval{Start: t1301, StartUnknown: true, EndUnknown: true},
			want: `{"endUnknown":true,"start":"2026-09-01T13:01:00Z","startUnknown":true}`,
		},
		{
			name: "the zero interval is an empty object",
			iv:   graph.Interval{},
			want: `{}`,
		},
		{
			name: "nanoseconds are preserved",
			iv:   graph.Interval{Start: t1300.Add(123456789 * time.Nanosecond)},
			want: `{"start":"2026-09-01T13:00:00.123456789Z"}`,
		},
		{
			name: "milliseconds keep three digits, as canonical proto JSON does",
			iv:   graph.Interval{Start: t1300.Add(100 * time.Millisecond)},
			want: `{"start":"2026-09-01T13:00:00.100Z"}`,
		},
		{
			name: "non-UTC input is normalized",
			iv:   graph.Interval{Start: t1300.In(time.FixedZone("CEST", 2*60*60))},
			want: `{"start":"2026-09-01T13:00:00Z"}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := json.Marshal(tc.iv)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("Marshal = %s, want %s", got, tc.want)
			}
			var back graph.Interval
			if err := json.Unmarshal(got, &back); err != nil {
				t.Fatalf("Unmarshal(%s): %v", got, err)
			}
			if !back.Equal(tc.iv.Normalize()) {
				t.Errorf("round trip = %s, want %s", back, tc.iv.Normalize())
			}
		})
	}
}

func TestIntervalUnmarshalJSON(t *testing.T) {
	t.Parallel()

	t.Run("accepts protobuf field names", func(t *testing.T) {
		t.Parallel()
		var iv graph.Interval
		input := `{"start":"2026-09-01T13:01:00Z","start_unknown":true,"end_unknown":true}`
		if err := json.Unmarshal([]byte(input), &iv); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		want := graph.Interval{Start: t1301, StartUnknown: true, EndUnknown: true}
		if !iv.Equal(want) {
			t.Errorf("got %s, want %s", iv, want)
		}
	})

	rejects := map[string]string{
		"unknown field":    `{"valid_from":"2026-09-01T13:00:00Z"}`,
		"bad timestamp":    `{"start":"yesterday"}`,
		"wrong flag type":  `{"start":"2026-09-01T13:00:00Z","startUnknown":"yes"}`,
		"not an object":    `[]`,
		"timestamp offset": `{"start":"2026-09-01T13:00:00"}`,
	}
	for name, input := range rejects {
		t.Run("rejects "+name, func(t *testing.T) {
			t.Parallel()
			var iv graph.Interval
			if err := json.Unmarshal([]byte(input), &iv); err == nil {
				t.Errorf("Unmarshal(%s) = nil error, want a failure", input)
			}
		})
	}
}

func TestIntervalProto(t *testing.T) {
	t.Parallel()

	iv := graph.Interval{Start: t1301, End: t1420, StartUnknown: true}
	p := iv.Proto()
	if !p.GetStart().AsTime().Equal(t1301) || !p.GetEnd().AsTime().Equal(t1420) || !p.GetStartUnknown() || p.GetEndUnknown() {
		t.Fatalf("Proto() = %v", p)
	}
	if back := graph.IntervalFromProto(p); !back.Equal(iv) {
		t.Errorf("round trip = %s, want %s", back, iv)
	}

	if got := graph.IntervalFromProto(nil); !got.IsZero() {
		t.Errorf("IntervalFromProto(nil) = %s, want the zero interval", got)
	}
	unbounded := graph.NewInterval(t1300, time.Time{}).Proto()
	if unbounded.GetEnd() != nil {
		t.Error("an unbounded end must stay unset in the protobuf form")
	}
	if got := graph.IntervalFromProto(&graphv1.Interval{}); !got.IsZero() {
		t.Errorf("an empty protobuf interval must convert to the zero interval, got %s", got)
	}
}

// ---------- property tests (research §16: "Property (rapid): ... interval algebra") ----------

// genInterval draws a valid interval inside a decade, half of them unbounded.
func genInterval(t *rapid.T, label string) graph.Interval {
	const (
		minNanos = 1577836800e9 // 2020-01-01T00:00:00Z
		maxNanos = 1893456000e9 // 2030-01-01T00:00:00Z
		maxSpan  = 30 * 24 * 3600 * 1e9
	)
	iv := graph.Interval{
		Start:        time.Unix(0, rapid.Int64Range(minNanos, maxNanos).Draw(t, label+".start")).UTC(),
		StartUnknown: rapid.Bool().Draw(t, label+".startUnknown"),
	}
	if rapid.Bool().Draw(t, label+".bounded") {
		iv.End = iv.Start.Add(time.Duration(rapid.Int64Range(1, maxSpan).Draw(t, label+".span")))
	} else {
		iv.EndUnknown = rapid.Bool().Draw(t, label+".endUnknown")
	}
	return iv
}

// genInstant draws an instant in the same decade, widened so that bounds are hit often.
func genInstant(t *rapid.T, label string) time.Time {
	const (
		minNanos = 1577836800e9 - 24*3600*1e9
		maxNanos = 1893456000e9 + 31*24*3600*1e9
	)
	return time.Unix(0, rapid.Int64Range(minNanos, maxNanos).Draw(t, label)).UTC()
}

func TestIntervalPropertyValidateAcceptsGenerated(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		iv := genInterval(t, "iv")
		if err := iv.Validate(); err != nil {
			t.Fatalf("generated interval %s is invalid: %v", iv, err)
		}
	})
}

func TestIntervalPropertyContainsBoundaries(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		iv := genInterval(t, "iv")
		if !iv.Contains(iv.Start) {
			t.Fatalf("%s must contain its start", iv)
		}
		if !iv.End.IsZero() {
			if iv.Contains(iv.End) {
				t.Fatalf("%s must not contain its end", iv)
			}
			if !iv.Contains(iv.End.Add(-time.Nanosecond)) {
				t.Fatalf("%s must contain the nanosecond before its end", iv)
			}
		}
		if iv.Contains(iv.Start.Add(-time.Nanosecond)) {
			t.Fatalf("%s must not contain the nanosecond before its start", iv)
		}
	})
}

func TestIntervalPropertySplitPartitions(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		iv := genInterval(t, "iv")
		at := genInstant(t, "at")

		left, right, ok := iv.Split(at)
		if !ok {
			// Split may only refuse a point that is not strictly inside.
			if iv.Start.Before(at) && (iv.End.IsZero() || at.Before(iv.End)) {
				t.Fatalf("%s.Split(%s) refused a point strictly inside", iv, at)
			}
			return
		}
		if err := left.Validate(); err != nil {
			t.Fatalf("left half %s invalid: %v", left, err)
		}
		if err := right.Validate(); err != nil {
			t.Fatalf("right half %s invalid: %v", right, err)
		}
		// The two halves tile the original exactly: same outer bounds, meeting at `at`.
		if !left.Start.Equal(iv.Start) || !left.End.Equal(at) {
			t.Fatalf("left half %s does not start the original %s", left, iv)
		}
		if !right.Start.Equal(at) || !right.End.Equal(iv.End) {
			t.Fatalf("right half %s does not end the original %s", right, iv)
		}
		if left.Overlaps(right) {
			t.Fatalf("halves %s and %s overlap", left, right)
		}
		// ... and their union recovers the original, instant by instant.
		for _, probe := range []time.Time{
			iv.Start, at, at.Add(-time.Nanosecond), at.Add(time.Nanosecond),
			genInstant(t, "probe"),
		} {
			inLeft, inRight := left.Contains(probe), right.Contains(probe)
			if inLeft && inRight {
				t.Fatalf("%s is in both halves of %s split at %s", probe, iv, at)
			}
			if got, want := inLeft || inRight, iv.Contains(probe); got != want {
				t.Fatalf("union of %s and %s contains %s = %v, original %s = %v",
					left, right, probe, got, iv, want)
			}
		}
	})
}

func TestIntervalPropertyOverlapsSymmetric(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		a, b := genInterval(t, "a"), genInterval(t, "b")
		if a.Overlaps(b) != b.Overlaps(a) {
			t.Fatalf("Overlaps is not symmetric for %s and %s", a, b)
		}
		if !a.Overlaps(a) {
			t.Fatalf("%s must overlap itself", a)
		}
	})
}

func TestIntervalPropertyIntersectAgreesWithContains(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		a, b := genInterval(t, "a"), genInterval(t, "b")
		inter, ok := a.Intersect(b)
		if ok != a.Overlaps(b) {
			t.Fatalf("Intersect ok = %v but Overlaps = %v for %s and %s", ok, a.Overlaps(b), a, b)
		}
		if !ok {
			return
		}
		if err := inter.Validate(); err != nil {
			t.Fatalf("intersection %s invalid: %v", inter, err)
		}
		for _, probe := range []time.Time{a.Start, b.Start, inter.Start, genInstant(t, "probe")} {
			if got, want := inter.Contains(probe), a.Contains(probe) && b.Contains(probe); got != want {
				t.Fatalf("%s.Contains(%s) = %v, want %v (from %s and %s)", inter, probe, got, want, a, b)
			}
		}
	})
}

func TestIntervalPropertyRoundTrips(t *testing.T) {
	t.Parallel()
	rapid.Check(t, func(t *rapid.T) {
		iv := genInterval(t, "iv")

		encoded, err := json.Marshal(iv)
		if err != nil {
			t.Fatalf("Marshal(%s): %v", iv, err)
		}
		var decoded graph.Interval
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatalf("Unmarshal(%s): %v", encoded, err)
		}
		if !decoded.Equal(iv) {
			t.Fatalf("JSON round trip of %s gave %s (%s)", iv, decoded, encoded)
		}
		if back := graph.IntervalFromProto(iv.Proto()); !back.Equal(iv) {
			t.Fatalf("protobuf round trip of %s gave %s", iv, back)
		}
	})
}
