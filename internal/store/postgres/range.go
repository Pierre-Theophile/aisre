// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// TimeRange is the Go side of a PostgreSQL tstzrange column: graph.entity_versions.valid,
// graph.entity_versions.observed and their edge counterparts.
//
// # Convention
//
// Every interval in this project is HALF-OPEN, `[Start, End)`: Start is included, End is
// excluded. An interval that has not ended yet has EndUnbounded set, which is stored as a
// NULL (absent) upper bound -- `[2026-09-15 12:00:00+00,)` -- and NOT as the timestamp
// `infinity`. The two are different to PostgreSQL and only the NULL bound makes
// `upper_inf(observed)` true, which is the predicate the whole model uses to mean "current
// knowledge" (constitution II, data-model.md). `infinity` would be an ordinary finite-looking
// bound for which upper_inf() returns false, so it is never written.
//
// The lower bound is always present. A fact whose real start instant is unknown is stored
// with the first observation time as its physical lower bound plus the row's
// valid_from_unknown flag; a range cannot express "unknown" and must not pretend to
// (FR-011, research section 3).
//
// TimeRange implements pgtype.RangeValuer and pgtype.RangeScanner, so pgx encodes and decodes
// it against tstzrange with no per-query conversion; registerTypes also makes it the default
// PostgreSQL type for the Go type, so it can be passed as a query argument directly.
//
// It is deliberately a local struct rather than the graph package's Interval: the projector
// owns the domain type and converts at the storage boundary with FromBounds/EndPtr.
type TimeRange struct {
	// Start is the inclusive lower bound. It is always set.
	Start time.Time
	// End is the exclusive upper bound. It is meaningless when EndUnbounded is true.
	End time.Time
	// EndUnbounded reports that the interval is still open: no upper bound is stored and
	// upper_inf() is true for it.
	EndUnbounded bool
}

// NewTimeRange returns the half-open interval [start, end).
func NewTimeRange(start, end time.Time) TimeRange {
	return TimeRange{Start: start, End: end}
}

// OpenTimeRange returns the interval [start, ) -- open on the right, so upper_inf() holds.
func OpenTimeRange(start time.Time) TimeRange {
	return TimeRange{Start: start, EndUnbounded: true}
}

// FromBounds builds a TimeRange from a start instant and an optional end, the shape a
// projector naturally has: a nil end means the interval is still open.
func FromBounds(start time.Time, end *time.Time) TimeRange {
	if end == nil {
		return OpenTimeRange(start)
	}
	return NewTimeRange(start, *end)
}

// EndPtr is the inverse of FromBounds: nil when the interval is open, otherwise a copy of the
// upper bound.
func (r TimeRange) EndPtr() *time.Time {
	if r.EndUnbounded {
		return nil
	}
	end := r.End
	return &end
}

// IsOpen reports whether the interval has no upper bound. For an `observed` range this is the
// "current knowledge" test that PostgreSQL spells upper_inf(observed).
func (r TimeRange) IsOpen() bool { return r.EndUnbounded }

// Contains reports whether t falls in [Start, End), matching the PostgreSQL `@>` operator.
func (r TimeRange) Contains(t time.Time) bool {
	if t.Before(r.Start) {
		return false
	}
	return r.EndUnbounded || t.Before(r.End)
}

// IsEmpty reports whether the interval covers no instant at all. PostgreSQL normalises such a
// range to `empty`; the version tables reject it with a CHECK constraint.
func (r TimeRange) IsEmpty() bool {
	return !r.EndUnbounded && !r.Start.Before(r.End)
}

// String renders the interval the way PostgreSQL prints a tstzrange literal.
func (r TimeRange) String() string {
	if r.EndUnbounded {
		return fmt.Sprintf("[%s,)", r.Start.UTC().Format(time.RFC3339Nano))
	}
	return fmt.Sprintf("[%s,%s)", r.Start.UTC().Format(time.RFC3339Nano), r.End.UTC().Format(time.RFC3339Nano))
}

// IsNull implements pgtype.RangeValuer. A TimeRange is never SQL NULL; use *TimeRange and a
// nil pointer for a nullable column.
func (r TimeRange) IsNull() bool { return false }

// BoundTypes implements pgtype.RangeValuer: inclusive lower, exclusive upper, and an absent
// upper bound when the interval is open.
func (r TimeRange) BoundTypes() (lower, upper pgtype.BoundType) {
	if r.EndUnbounded {
		return pgtype.Inclusive, pgtype.Unbounded
	}
	return pgtype.Inclusive, pgtype.Exclusive
}

// Bounds implements pgtype.RangeValuer.
func (r TimeRange) Bounds() (lower, upper any) { return r.Start, r.End }

// ScanNull implements pgtype.RangeScanner.
func (r *TimeRange) ScanNull() error {
	return errors.New("postgres: cannot scan NULL into a TimeRange; scan into *TimeRange instead")
}

// ScanBounds implements pgtype.RangeScanner.
func (r *TimeRange) ScanBounds() (lowerTarget, upperTarget any) { return &r.Start, &r.End }

// SetBoundTypes implements pgtype.RangeScanner. It rejects the shapes this project never
// writes -- an empty range, or one without a lower bound -- rather than decoding them into a
// TimeRange that would silently misrepresent them.
func (r *TimeRange) SetBoundTypes(lower, upper pgtype.BoundType) error {
	if lower == pgtype.Empty || upper == pgtype.Empty {
		*r = TimeRange{}
		return errors.New("postgres: empty tstzrange cannot be represented as a TimeRange")
	}
	if lower == pgtype.Unbounded {
		*r = TimeRange{}
		return errors.New("postgres: tstzrange with an unbounded lower bound cannot be represented as a TimeRange")
	}
	if lower == pgtype.Exclusive {
		return errors.New("postgres: tstzrange with an exclusive lower bound is not half-open [); see TimeRange")
	}
	if upper == pgtype.Unbounded {
		r.End = time.Time{}
		r.EndUnbounded = true
		return nil
	}
	r.EndUnbounded = false
	if upper == pgtype.Inclusive {
		return fmt.Errorf("postgres: tstzrange %s has an inclusive upper bound; intervals must be half-open [)", r.String())
	}
	return nil
}

// registerTypes teaches a pgx connection which PostgreSQL type each project-local Go type
// maps to, so that values can be used as query arguments without an explicit cast.
func registerTypes(m *pgtype.Map) {
	m.RegisterDefaultPgType(TimeRange{}, "tstzrange")
	m.RegisterDefaultPgType(&TimeRange{}, "tstzrange")
}
