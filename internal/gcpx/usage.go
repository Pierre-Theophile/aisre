// SPDX-License-Identifier: Apache-2.0

package gcpx

import (
	"errors"
	"sort"
	"sync"
	"time"
)

// The usage report: what Dana sees per run (FR-152, FR-113, SC-022; contracts/budget.md §8).
//
// It is the artifact that makes the budget auditable rather than aspirational. A budget nobody can
// read after the fact is a budget nobody can tell was honoured — and the one figure that matters
// most, "how much of the human reserve did we leave alone", is unobservable from the outside
// entirely. It must be 100% of the reserve in 100% of cycles, and this is where that is checked.
//
// Two properties are easy to get wrong and are load-bearing here:
//
//   - **Every figure is labelled vendor-reported or self-tracked** (§3). GCP reports no usable
//     real-time remaining quota, so almost everything here is self-tracked; saying so is what stops
//     a reader treating this integration's own arithmetic as the vendor's word.
//   - **The feeders' and the backend's usage are counted SEPARATELY** (FR-113). "What do
//     investigations cost" and "what does ingestion cost" are different questions with different
//     owners, and a single total answers neither.

// Consumer is which half of the integration spent a call. FR-113 requires them apart.
type Consumer string

const (
	// ConsumerFeeder is ingestion: the GCP feeder and the vendor-notice feeder.
	ConsumerFeeder Consumer = "feeder"
	// ConsumerBackend is the telemetry backend answering an investigation's algebra terms.
	ConsumerBackend Consumer = "backend"
	// ConsumerGate is the startup read-only check and the quota reconciliation — the calls the
	// integration makes about ITSELF. Counted apart so they are not mistaken for either of the
	// two above, and so their cost is visible rather than folded into ingestion.
	ConsumerGate Consumer = "gate"
)

// FigureSource labels where a number came from. The distinction is FR-148's, and the reason it is a
// type rather than a comment is that a stale vendor figure and a live self-tracked one look
// identical once they are both integers on a page.
type FigureSource string

const (
	// SourceSelfTracked is this integration's own accounting, in real time.
	SourceSelfTracked FigureSource = "self-tracked"
	// SourceVendorReported came from serviceruntime metrics: minutes-stale, and it costs
	// monitoring.query quota to read.
	SourceVendorReported FigureSource = "vendor-reported"
	// SourceFallback is a configured default used because discovery failed. Saying "fallback"
	// rather than "discovered" is the difference between a number and a guess.
	SourceFallback FigureSource = "fallback"
)

// ClassUsage is one endpoint class's line in the report.
type ClassUsage struct {
	Class     EndpointClass
	Key       string
	Calls     int
	Allowed   int
	Refused   int
	FirstCall time.Time
	LastCall  time.Time
	// ReserveUntouched is the claim SC-022 checks. It goes false the moment a Take is served from
	// inside the reserve, which the budget never does — so a false here is a defect report, not a
	// statistic.
	ReserveUntouched bool
	RetryAt          time.Time
	LimitSource      FigureSource
}

// Usage accumulates the report. Safe for concurrent use: the feeder and the backend share one
// budget, so they share one usage.
type Usage struct {
	mu       sync.Mutex
	byKey    map[string]*ClassUsage
	requests map[Operation]*RequestRow
	consumer Consumer
	children map[Consumer]*Usage
}

// NewUsage builds a report for one consumer.
func NewUsage(consumer Consumer) *Usage {
	return &Usage{
		byKey:    map[string]*ClassUsage{},
		requests: map[Operation]*RequestRow{},
		consumer: consumer,
		children: map[Consumer]*Usage{},
	}
}

// For returns the sub-report for a consumer, creating it on first use. This is how FR-113's
// separation is kept without every call site having to remember which half it is in: the budget is
// handed the right sub-report when it is constructed.
func (u *Usage) For(consumer Consumer) *Usage {
	if u == nil {
		return nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	child, ok := u.children[consumer]
	if !ok {
		child = &Usage{
			byKey:    map[string]*ClassUsage{},
			requests: map[Operation]*RequestRow{},
			consumer: consumer,
			children: map[Consumer]*Usage{},
		}
		u.children[consumer] = child
	}
	return child
}

// record notes one call. remainingAfter and reserve are what the bucket held once the call was
// served; they are passed in rather than inferred because this is where SC-022's claim is decided,
// and a claim computed from a number nobody supplied is a claim nothing can falsify.
//
// An earlier version of this file had ReserveUntouched set true at construction and written false
// by nothing at all — so ReserveHonoured() could not return false, and the test asserting it passed
// against a budget deliberately rewired to drain the reserve. A guard that cannot fail is not a
// guard.
func (u *Usage) record(class EndpointClass, key string, calls int, allowed bool, remainingAfter, reserve float64) {
	if u == nil {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	row := u.row(class, key)
	row.Calls += calls
	if allowed {
		row.Allowed += calls
		// The moment a call is served leaving less than the reserve behind, the reserve has been
		// spent. reserve <= 0 means the class has no reserve to honour, which is not a breach.
		if reserve > 0 && remainingAfter < reserve {
			row.ReserveUntouched = false
		}
	}
	now := time.Now().UTC()
	if row.FirstCall.IsZero() {
		row.FirstCall = now
	}
	row.LastCall = now
}

func (u *Usage) recordRefusal(class EndpointClass, key string, retryAt time.Time) {
	if u == nil {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	row := u.row(class, key)
	row.Refused++
	row.RetryAt = retryAt
	// A refusal is the reserve DOING ITS JOB, not being touched. ReserveUntouched stays true.
}

// row must be called with the lock held.
func (u *Usage) row(class EndpointClass, key string) *ClassUsage {
	row, ok := u.byKey[key]
	if !ok {
		row = &ClassUsage{
			Class:            class,
			Key:              key,
			ReserveUntouched: true,
			LimitSource:      SourceSelfTracked,
		}
		u.byKey[key] = row
	}
	return row
}

// recordRequest notes one operation against this consumer's request log (SC-020).
//
// It is called whether or not the call was served: a refusal is counted apart, because a call the
// budget stopped never reached GCP and reading it as a request would overstate what was issued.
func (u *Usage) recordRequest(op Operation, spec OperationSpec, issued bool) {
	u.recordRequestOutcome(op, spec, issued, "")
}

// recordBlocked notes an operation the published list refused. It is logged rather than dropped so
// ReadOnlyHonoured can fail — see RequestRow.Blocked.
func (u *Usage) recordBlocked(op Operation, reason string) {
	u.recordRequestOutcome(op, OperationSpec{}, false, reason)
}

func (u *Usage) recordRequestOutcome(op Operation, spec OperationSpec, issued bool, blocked string) {
	if u == nil {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.requests == nil {
		u.requests = map[Operation]*RequestRow{}
	}
	row, ok := u.requests[op]
	if !ok {
		row = &RequestRow{
			Consumer:    u.consumer,
			Operation:   op,
			Area:        spec.Area,
			Class:       spec.Class,
			StateChange: spec.StateChange,
		}
		u.requests[op] = row
	}
	if blocked != "" {
		row.Blocked++
		row.Refusal = blocked
		return
	}
	if !issued {
		row.Refused++
		return
	}
	row.Calls++
	now := time.Now().UTC()
	if row.First.IsZero() {
		row.First = now
	}
	row.Last = now
}

// Requests returns this consumer's request log, sorted by operation.
func (u *Usage) Requests() []RequestRow {
	if u == nil {
		return nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]RequestRow, 0, len(u.requests))
	for _, row := range u.requests {
		out = append(out, *row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Operation < out[j].Operation })
	return out
}

// AllRequests returns the whole run's request log — this consumer's and every sub-consumer's — which
// is the artifact SC-020 is verified from. The backend half is in here by construction: it spends
// the same budget through the same readers, and `For(ConsumerBackend)` is what keeps its rows
// legible as the backend's (FR-112, FR-113).
func (u *Usage) AllRequests() []RequestRow {
	if u == nil {
		return nil
	}
	out := u.Requests()
	u.mu.Lock()
	children := make([]*Usage, 0, len(u.children))
	for _, child := range u.children {
		children = append(children, child)
	}
	u.mu.Unlock()
	for _, child := range children {
		out = append(out, child.AllRequests()...)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Consumer != out[j].Consumer {
			return out[i].Consumer < out[j].Consumer
		}
		return out[i].Operation < out[j].Operation
	})
	return out
}

// StateChanges returns the request-log rows that changed anything in the organisation's projects.
//
// SC-020's second half is a claim about this slice: it holds the doorbell acknowledgement or it is
// empty, and nothing else may ever appear in it. A caller that wants the claim rather than the rows
// asks ReadOnlyHonoured.
func (u *Usage) StateChanges() []RequestRow {
	var out []RequestRow
	for _, row := range u.AllRequests() {
		if !row.ReadOnly() && row.Calls > 0 {
			out = append(out, row)
		}
	}
	return out
}

// ReadOnlyHonoured is SC-020 asked of one run's own request log: every operation the run tried to
// issue was on the published read-only list, and the only state change was the declared doorbell
// acknowledgement.
//
// It fails on a BLOCKED row as well as on an issued one, and that is the whole reason blocked rows
// are logged. Were it to look only at what was served it could not return false — Issue is the only
// way to spend a call and it consults the list first — and a guard that cannot fail is not a guard.
// As written, an attempt the list stopped is reported: no write reached GCP, and the claim that
// every issuable operation is published is nonetheless false, which is what an operator must be
// told.
func (u *Usage) ReadOnlyHonoured() bool {
	for _, row := range u.AllRequests() {
		if row.Blocked > 0 {
			return false
		}
		if row.Calls == 0 {
			continue
		}
		if _, err := Issuable(row.Operation); err != nil {
			return false
		}
		if !row.ReadOnly() && row.StateChange != doorbellStateChange {
			return false
		}
	}
	return true
}

// BlockedRequests returns the rows the published list refused. An empty slice is the expected state
// and a non-empty one is a defect report naming the operation and the reason.
func (u *Usage) BlockedRequests() []RequestRow {
	var out []RequestRow
	for _, row := range u.AllRequests() {
		if row.Blocked > 0 {
			out = append(out, row)
		}
	}
	return out
}

// SetLimitSource records how a class's limit was arrived at, so the report can distinguish a
// discovered limit from a fallback (contracts/budget.md §4).
func (u *Usage) SetLimitSource(class EndpointClass, key string, source FigureSource) {
	if u == nil {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.row(class, key).LimitSource = source
}

// Rows returns the report's lines, sorted, for the consumer this Usage belongs to.
func (u *Usage) Rows() []ClassUsage {
	if u == nil {
		return nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]ClassUsage, 0, len(u.byKey))
	for _, row := range u.byKey {
		out = append(out, *row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// Consumers returns the per-consumer reports, which is FR-113's separation made readable.
func (u *Usage) Consumers() map[Consumer][]ClassUsage {
	if u == nil {
		return nil
	}
	u.mu.Lock()
	children := make(map[Consumer]*Usage, len(u.children))
	for k, v := range u.children {
		children[k] = v
	}
	u.mu.Unlock()

	out := make(map[Consumer][]ClassUsage, len(children))
	for consumer, child := range children {
		out[consumer] = child.Rows()
	}
	return out
}

// ReserveHonoured reports whether every line left the human reserve alone. It must be true in 100%
// of cycles (SC-022); a false is a defect, not a measurement.
func (u *Usage) ReserveHonoured() bool {
	for _, row := range u.Rows() {
		if !row.ReserveUntouched {
			return false
		}
	}
	for _, rows := range u.Consumers() {
		for _, row := range rows {
			if !row.ReserveUntouched {
				return false
			}
		}
	}
	return true
}

// asYield is errors.As for *YieldError, kept here so budget.go states its intent without importing
// errors for one line.
func asYield(err error, target **YieldError) bool { return errors.As(err, target) }
