// SPDX-License-Identifier: Apache-2.0

package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres/pgtest"
)

// TestTimeRangeRoundTrip pushes every shape of interval the project writes through a real
// tstzrange column and back, and checks that an open upper bound is stored as an absent bound
// (upper_inf true), never as the `infinity` timestamp.
func TestTimeRangeRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := pgtest.Open(t)

	cases := []struct {
		name          string
		in            postgres.TimeRange
		wantUpperInf  bool
		wantRendering string
	}{
		{
			name:          "bounded",
			in:            postgres.NewTimeRange(day0, day2),
			wantRendering: `["2026-09-15 10:00:00+00","2026-09-17 10:00:00+00")`,
		},
		{
			name:          "unbounded upper",
			in:            postgres.OpenTimeRange(day0),
			wantUpperInf:  true,
			wantRendering: `["2026-09-15 10:00:00+00",)`,
		},
		{
			name:          "sub-second precision",
			in:            postgres.NewTimeRange(day0.Add(1234*time.Microsecond), day1.Add(999*time.Microsecond)),
			wantRendering: `["2026-09-15 10:00:00.001234+00","2026-09-16 10:00:00.000999+00")`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out postgres.TimeRange
			var upperInf, lowerInc, upperInc bool
			var rendered string
			err := store.Pool().QueryRow(ctx, `
				SELECT $1::tstzrange,
				       upper_inf($1::tstzrange),
				       lower_inc($1::tstzrange),
				       upper_inc($1::tstzrange),
				       ($1::tstzrange)::text`, tc.in).
				Scan(&out, &upperInf, &lowerInc, &upperInc, &rendered)
			if err != nil {
				t.Fatalf("round trip %s: %v", tc.in, err)
			}

			if !out.Start.Equal(tc.in.Start) {
				t.Errorf("start = %s, want %s", out.Start, tc.in.Start)
			}
			if out.EndUnbounded != tc.in.EndUnbounded {
				t.Errorf("EndUnbounded = %v, want %v", out.EndUnbounded, tc.in.EndUnbounded)
			}
			if !tc.in.EndUnbounded && !out.End.Equal(tc.in.End) {
				t.Errorf("end = %s, want %s", out.End, tc.in.End)
			}
			if upperInf != tc.wantUpperInf {
				t.Errorf("upper_inf = %v, want %v", upperInf, tc.wantUpperInf)
			}
			// Half-open [) in both directions, always.
			if !lowerInc {
				t.Error("lower_inc is false; intervals must include their lower bound")
			}
			if upperInc {
				t.Error("upper_inc is true; intervals must exclude their upper bound")
			}
			if rendered != tc.wantRendering {
				t.Errorf("PostgreSQL rendered %s, want %s", rendered, tc.wantRendering)
			}
		})
	}
}

// TestOpenUpperBoundIsNotInfinity pins the convention: an open interval has NO upper bound.
// Storing the `infinity` timestamp instead would look similar but make upper_inf false, and
// "current knowledge" is defined as upper_inf(observed).
func TestOpenUpperBoundIsNotInfinity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := pgtest.Open(t)

	var ourUpperInf, infinityUpperInf bool
	err := store.Pool().QueryRow(ctx, `
		SELECT upper_inf($1::tstzrange),
		       upper_inf(tstzrange($2, 'infinity'::timestamptz, '[)'))`,
		postgres.OpenTimeRange(day0), day0).Scan(&ourUpperInf, &infinityUpperInf)
	if err != nil {
		t.Fatalf("compare bounds: %v", err)
	}
	if !ourUpperInf {
		t.Error("an open TimeRange must satisfy upper_inf()")
	}
	if infinityUpperInf {
		t.Error("assumption broken: 'infinity' now satisfies upper_inf(); revisit the convention")
	}
}

// TestScanRejectsUnrepresentableRanges: shapes the schema never writes must fail loudly
// rather than decode into a TimeRange that misstates them.
func TestScanRejectsUnrepresentableRanges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := pgtest.Open(t)

	cases := map[string]string{
		"empty":             `'empty'::tstzrange`,
		"unbounded lower":   `tstzrange(NULL, '2026-09-15Z'::timestamptz, '[)')`,
		"inclusive upper":   `tstzrange('2026-09-15Z'::timestamptz, '2026-09-17Z'::timestamptz, '[]')`,
		"exclusive lower":   `tstzrange('2026-09-15Z'::timestamptz, '2026-09-17Z'::timestamptz, '(]')`,
		"fully unbounded":   `tstzrange(NULL, NULL, '[)')`,
		"exclusive on both": `tstzrange('2026-09-15Z'::timestamptz, '2026-09-17Z'::timestamptz, '()')`,
	}
	for name, expr := range cases {
		t.Run(name, func(t *testing.T) {
			var out postgres.TimeRange
			if err := store.Pool().QueryRow(ctx, "SELECT "+expr).Scan(&out); err == nil {
				t.Fatalf("scanning %s succeeded (got %s), want an error", expr, out)
			}
		})
	}
}

func TestTimeRangeHelpers(t *testing.T) {
	t.Parallel()

	bounded := postgres.NewTimeRange(day0, day2)
	open := postgres.OpenTimeRange(day0)

	if bounded.IsOpen() {
		t.Error("a bounded range reports IsOpen")
	}
	if !open.IsOpen() {
		t.Error("an open range does not report IsOpen")
	}

	// Half-open: the lower bound is in, the upper bound is out.
	if !bounded.Contains(day0) {
		t.Error("Contains(start) is false; the lower bound is inclusive")
	}
	if bounded.Contains(day2) {
		t.Error("Contains(end) is true; the upper bound is exclusive")
	}
	if !bounded.Contains(day1) {
		t.Error("Contains(midpoint) is false")
	}
	if bounded.Contains(day0.Add(-time.Nanosecond)) {
		t.Error("Contains(before start) is true")
	}
	if !open.Contains(day2.AddDate(10, 0, 0)) {
		t.Error("an open range must contain every instant after its start")
	}

	if got := postgres.FromBounds(day0, nil); got != open {
		t.Errorf("FromBounds(start, nil) = %+v, want %+v", got, open)
	}
	end := day2
	if got := postgres.FromBounds(day0, &end); got != bounded {
		t.Errorf("FromBounds(start, &end) = %+v, want %+v", got, bounded)
	}
	if open.EndPtr() != nil {
		t.Error("EndPtr of an open range is not nil")
	}
	if got := bounded.EndPtr(); got == nil || !got.Equal(day2) {
		t.Errorf("EndPtr = %v, want %s", got, day2)
	}

	if bounded.IsEmpty() {
		t.Error("a non-degenerate range reports IsEmpty")
	}
	if !postgres.NewTimeRange(day0, day0).IsEmpty() {
		t.Error("a zero-width range does not report IsEmpty")
	}
	if open.IsEmpty() {
		t.Error("an open range reports IsEmpty")
	}

	if got, want := open.String(), "[2026-09-15T10:00:00Z,)"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	if got, want := bounded.String(), "[2026-09-15T10:00:00Z,2026-09-17T10:00:00Z)"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

// TestEmptyRangeRejectedByCheckConstraint: the schema refuses a degenerate interval, so a
// projector bug cannot store a version that is true for no instant at all.
func TestEmptyRangeRejectedByCheckConstraint(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := pgtest.Open(t)
	seedEntity(ctx, t, store, "ent-1", "service")

	_, err := store.Pool().Exec(ctx, `
		INSERT INTO graph.entity_versions (version_id, entity_id, valid, observed)
		VALUES ('v1', 'ent-1', 'empty'::tstzrange, $1)`, postgres.OpenTimeRange(day0))
	if !isSQLState(err, "23514") {
		t.Fatalf("empty valid range: got %v, want check_violation (23514)", err)
	}
}
