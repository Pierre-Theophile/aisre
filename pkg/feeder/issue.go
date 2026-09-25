// SPDX-License-Identifier: Apache-2.0

package feeder

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"
)

// The one door a REST connector spends a call through (004 FR-004, FR-071, FR-073; T003, T035).
//
// ---------------------------------------------------------------------------------------------
// Why there is exactly one, and why `take` is not exported
//
// Three things have to be true of every call a connector makes, and each of them is already built:
// the operation is on the published read-only surface (readonly.go), the budget permits it
// (quota.go), and the attempt is recorded whether or not it went out (areastats.go). What was missing
// is the place where all three happen together — and *together* is the whole point. A connector that
// could check one and skip another would have a request log with a hole in it exactly the shape of
// whoever took the shortcut, and FR-004's claim is about the set of operations the feeder **can**
// issue rather than about the ones it remembered to check.
//
// So `Issue` is the only exported way to spend a call, and the metering step below it is unexported.
// An exported `take` would be a way to reach a platform without naming the operation, which is the
// same hole by a different route. This is the shape internal/gcpx/budget.go established for GCP, with
// one difference that matters: there the operation carries its own quota class, and here the platform
// reports which bucket a call came out of (`x-ratelimit-resource`), so the family is **observed** from
// the response rather than derived at the call site.
//
// # An unmetered run is not an ungated one
//
// A replay from disk and a unit test have no budget, and that is a legitimate state rather than an
// error: "we are not counting calls" is never a reason to stop checking what may be called. A nil
// budget therefore passes the surface gate and skips only the quota step — and because nothing leaves
// the process, nothing is recorded as having been issued either.

// Issuer is the one door. It holds the published surface, the budget and the cycle's accounting, so
// that the three checks cannot come apart.
type Issuer struct {
	surface *ReadOnlySurface
	budget  *QuotaBudget
	stats   *AreaStats

	mu sync.Mutex
	// log is the per-operation request record SC-007 is verified from.
	log map[ReadOperation]*RequestRecord
}

// RequestRecord is one operation's line of the request log.
//
// Per operation rather than per call, because the artifact a reader checks is "every operation this
// connector issued" and a row per call would be a payload log — which this deliberately is not.
// Nothing here is derived from a response body.
type RequestRecord struct {
	Operation ReadOperation
	Area      string
	// Issued is how many calls went out; Refused how many the budget stopped before they did.
	Issued  int
	Refused int
	// Blocked is how many times the operation was not issued because the published surface does not
	// carry it, and Refusal is why.
	//
	// It exists so the log records an ATTEMPT rather than only what succeeded. FR-004's claim is that
	// every request the connector is capable of issuing is on the published surface, so an attempt the
	// surface stopped falsifies the claim even though nothing reached the platform — and a log that
	// recorded only permitted calls would report a clean run in exactly the case somebody most needs
	// telling.
	Blocked int
	Refusal string
	First   time.Time
	Last    time.Time
}

// NewIssuer builds the door. The surface is required; a nil budget is an unmetered run and a nil
// AreaStats means the cycle keeps no per-area accounting, which a unit test legitimately wants.
func NewIssuer(surface *ReadOnlySurface, budget *QuotaBudget, stats *AreaStats) (*Issuer, error) {
	if surface == nil {
		return nil, fmt.Errorf("feeder: an issuer with no published surface would admit every " +
			"operation, which is the opposite of what it is for (FR-004)")
	}
	return &Issuer{
		surface: surface, budget: budget, stats: stats,
		log: map[ReadOperation]*RequestRecord{},
	}, nil
}

// Issue decides whether one call may be made, and records the attempt either way.
//
// `family` is the quota bucket to meter against — the platform's own name for it, from the last
// response's `x-ratelimit-resource`. An empty family means the connector has not been told yet, and the
// budget's own rules decide what that permits.
//
// The order is deliberate. The surface gate comes first because it costs nothing and its refusal is
// the stronger statement: an operation nobody published must not be issued whatever the budget says,
// and checking quota first would spend an allowance deciding about a call that was never permitted.
func (i *Issuer) Issue(ctx context.Context, op ReadOperation, family string, now time.Time) error {
	spec, err := i.surface.Issuable(op)
	if err != nil {
		i.record(op, "", now, func(r *RequestRecord) {
			r.Blocked++
			r.Refusal = err.Error()
		})
		return err
	}
	if err := ctx.Err(); err != nil {
		// The context died before anything was decided. Not a request, not a refusal, and not this
		// connector's behaviour to record.
		return err
	}
	if i.budget == nil {
		// An unmetered run: nothing leaves the process, so nothing is recorded as issued. The surface
		// gate above still ran, which is the half that is about capability.
		return nil
	}
	if err := i.budget.Allow(family); err != nil {
		i.record(op, spec.Area, now, func(r *RequestRecord) { r.Refused++ })
		if i.stats != nil {
			i.stats.Call(spec.Area, false)
		}
		return err
	}
	i.record(op, spec.Area, now, func(r *RequestRecord) { r.Issued++ })
	if i.stats != nil {
		i.stats.Call(spec.Area, true)
	}
	return nil
}

// Observe hands a response's rate-limit reading to the budget. It is on the issuer rather than on the
// budget directly so that a connector has one object to hold, and so that a transport cannot spend
// calls through the door while reporting quota somewhere else.
func (i *Issuer) Observe(reading Reading) {
	if i.budget != nil {
		i.budget.Observe(reading)
	}
}

// Report is the cycle's quota usage, per family (FR-071).
//
// It is on the issuer because the issuer is the object a connector holds: a transport that had to be
// handed the budget separately in order to publish its spending would be a transport holding a way to
// spend calls outside the door. An unmetered run reports its platform and no families, which is a true
// statement rather than an empty one — nothing was metered.
func (i *Issuer) Report() UsageReport {
	if i.budget == nil {
		return UsageReport{Platform: i.surface.Platform()}
	}
	return i.budget.Report()
}

// Areas is the cycle's operational report, per platform area (FR-009). Nil where the cycle keeps no
// per-area accounting.
func (i *Issuer) Areas() []AreaReport {
	if i.stats == nil {
		return nil
	}
	return i.stats.Report()
}

// Deferred records that work in an operation's area was left for a later cycle — a page not read, a
// repository not reached.
//
// It is here rather than on AreaStats directly so that the area comes from the published surface
// rather than from a string at the call site. A deferral filed under an area nobody polled is a
// deferral nobody can act on, and an operation off the surface has no area to file it under at all —
// so that case is reported rather than guessed.
func (i *Issuer) Deferred(op ReadOperation) error {
	spec, err := i.surface.Issuable(op)
	if err != nil {
		return err
	}
	if i.stats != nil {
		i.stats.Deferred(spec.Area)
	}
	return nil
}

func (i *Issuer) record(op ReadOperation, area string, now time.Time, update func(*RequestRecord)) {
	i.mu.Lock()
	defer i.mu.Unlock()
	row, ok := i.log[op]
	if !ok {
		row = &RequestRecord{Operation: op, Area: area, First: now}
		i.log[op] = row
	}
	if row.Area == "" {
		row.Area = area
	}
	if row.First.IsZero() || (!now.IsZero() && now.Before(row.First)) {
		row.First = now
	}
	if now.After(row.Last) {
		row.Last = now
	}
	update(row)
}

// RequestLog returns the cycle's record, in operation order so two runs of the same shape read the
// same.
func (i *Issuer) RequestLog() []RequestRecord {
	i.mu.Lock()
	defer i.mu.Unlock()
	out := make([]RequestRecord, 0, len(i.log))
	for _, op := range i.surface.Operations() {
		if row, ok := i.log[op]; ok {
			out = append(out, *row)
		}
	}
	// An operation the surface does not carry cannot appear in Operations(), and a blocked attempt on
	// one is exactly what the log exists to show — so those rows are appended after, in the order the
	// surface would have sorted them.
	var unpublished []ReadOperation
	for op := range i.log {
		if _, published := i.surface.SpecOf(op); !published {
			unpublished = append(unpublished, op)
		}
	}
	slices.Sort(unpublished)
	for _, op := range unpublished {
		out = append(out, *i.log[op])
	}
	return out
}

// StateChanges reports how many operations in this cycle's log change state. For a connector that only
// reads it is zero on every run, which is the cheapest form SC-007 takes over a real cycle rather than
// over the table alone.
func (i *Issuer) StateChanges() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	var n int
	for op, row := range i.log {
		if row.Issued == 0 {
			continue
		}
		if method, _, ok := splitOperation(op); !ok || !isReadMethod(method) {
			n++
		}
	}
	return n
}
