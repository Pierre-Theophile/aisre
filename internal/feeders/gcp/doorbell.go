// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// The doorbell (T122, FR-050, FR-006; contracts/gcp-feeder.md §6.3).
//
// A Pub/Sub notification channel may be configured. It means **"poll now" and nothing else**.
//
// # The guarantee is in the signatures, not in the discipline
//
// The rules are that the notification's body is never parsed, never trusted and never stored; that a
// forged, replayed or malformed notification costs **at most one extra poll**; and that it creates,
// alters or retracts nothing. Each of those could have been a rule this file remembers. Instead:
//
//   - `Ring` takes **no payload argument**. A function that cannot receive the body cannot parse it,
//     cannot store it and cannot be talked into trusting it — and a future edit that wanted to would
//     have to change the signature, which is a diff a reviewer sees;
//   - `DoorbellSource.Drain` returns an **int**. The one operation this feature performs on the
//     subscription hands back a count, so there is no value in the process for a body to be;
//   - the only thing `Ring` can return is whether to poll, and the poll reads the API. So the most a
//     forged flood can achieve is the rate limit's worth of extra reads of the truth.
//
// # Polling is the source of truth
//
// The integration runs correctly with the doorbell absent, and the fixtures run that way. A doorbell
// only makes a scheduled poll earlier, and both paths converge on one alert through the published
// idempotency key — so a doorbell-triggered poll and the scheduled poll that follows it are one
// event, not two.

// DoorbellSource is the subscription this feature drains.
//
// `Drain` returns **how many notifications were waiting** and never their contents. That is the
// interface's whole design: see the file comment.
//
// Implementing it is the one place this integration is not read-only, and the reason is recorded
// rather than hidden (FR-006, research §12): `pubsub.subscriptions.consume` authorises pull, ack,
// `modifyAckDeadline` and seek **together**, `roles/pubsub.viewer` cannot pull at all, and there is
// **no read-only pull at any granularity — not even with a custom role**. So the exception is
// holding that grant, bound resource-level to one subscription the operator created for this, and
// the integration runs correctly when it is absent.
type DoorbellSource interface {
	Drain(ctx context.Context) (int, error)
}

// DefaultDoorbellMinInterval is the shortest gap between two honoured rings. It is the rate limit
// FR-050 asks for, and the number is the alert poll ceiling: a doorbell that could trigger a poll
// more often than the schedule's own floor would let a flood outspend the schedule it is supposed to
// anticipate.
const DefaultDoorbellMinInterval = MinAlertPollInterval

// DefaultDoorbellBurst is how many honoured rings may happen back to back before the interval binds.
// One, because a burst of doorbells is exactly the shape a forged flood takes and a real one carries
// no more information than its first message.
const DefaultDoorbellBurst = 1

// DoorbellOptions configures one doorbell.
type DoorbellOptions struct {
	// Subscription is the subscription the operator created for this. It is recorded in the
	// report so that the one non-read-only grant in this feature names the exact resource it is
	// bound to, and never held project-wide.
	Subscription string
	// MinInterval is the shortest gap between honoured rings. Zero uses the default.
	MinInterval time.Duration
	// Burst is how many honoured rings may happen back to back. Zero uses the default.
	Burst int
	// Now is the clock. Zero uses time.Now.
	Now func() time.Time
}

// ErrNoSubscription is returned for a doorbell with no subscription named. It is an error rather
// than a no-op doorbell because a configured-but-unnamed doorbell is the shape of a deployment that
// thinks it has one: the honest states are "off" and "bound to this subscription".
var ErrNoSubscription = errors.New("gcp: a doorbell with no subscription")

// Doorbell decides whether a notification earns an early poll.
type Doorbell struct {
	subscription string
	minInterval  time.Duration
	burst        int
	now          func() time.Time

	mu       sync.Mutex
	tokens   int
	lastFill time.Time
	// counters, for the report. They are the only thing a notification leaves behind.
	rings       int
	honoured    int
	rateLimited int
	drainErrors []string
}

// NewDoorbell returns a doorbell over opts.
func NewDoorbell(opts DoorbellOptions) (*Doorbell, error) {
	if strings.TrimSpace(opts.Subscription) == "" {
		return nil, ErrNoSubscription
	}
	interval := opts.MinInterval
	if interval <= 0 {
		interval = DefaultDoorbellMinInterval
	}
	burst := opts.Burst
	if burst <= 0 {
		burst = DefaultDoorbellBurst
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Doorbell{
		subscription: opts.Subscription,
		minInterval:  interval,
		burst:        burst,
		now:          now,
		tokens:       burst,
		lastFill:     now(),
	}, nil
}

// DoorbellOutcome is what one ring earned.
type DoorbellOutcome struct {
	// PollNow says the caller should poll earlier than its schedule would have.
	PollNow bool
	// Reason names why, or why not, in the published vocabulary below.
	Reason string
}

// The published doorbell outcomes.
const (
	// DoorbellPoll is a ring that earned an early poll.
	DoorbellPoll = "poll_now"
	// DoorbellRateLimited is a ring inside the minimum interval. It is dropped, and dropping it
	// costs nothing: the scheduled poll is still coming, and the transition it would have found
	// is read from the API either way.
	DoorbellRateLimited = "rate_limited"
	// DoorbellEmpty is a drain that found nothing waiting.
	DoorbellEmpty = "no_notifications"
)

// Ring records that notifications arrived and returns whether to poll now.
//
// It takes **no payload**, which is the guarantee rather than a convention: see the file comment.
// `count` is how many were waiting, and it is used only to count them — one notification and fifty
// earn exactly the same single poll, because the poll reads the whole window regardless.
func (d *Doorbell) Ring(count int) DoorbellOutcome {
	if count <= 0 {
		return DoorbellOutcome{Reason: DoorbellEmpty}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.rings += count

	now := d.now()
	// A plain token bucket: one token per interval, capped at the burst. It is the rate limit
	// FR-050 names, and it is what bounds a flood to "at most one extra poll" per interval
	// however many messages arrive.
	if elapsed := now.Sub(d.lastFill); elapsed >= d.minInterval {
		gained := int(elapsed / d.minInterval)
		d.tokens = min(d.burst, d.tokens+gained)
		d.lastFill = now
	}
	if d.tokens <= 0 {
		d.rateLimited += count
		return DoorbellOutcome{Reason: DoorbellRateLimited}
	}
	d.tokens--
	d.honoured++
	return DoorbellOutcome{PollNow: true, Reason: DoorbellPoll}
}

// Drain pulls the subscription and rings. It is the whole operational path: drain, count, decide.
//
// A drain that fails is recorded and is **not** an error the caller has to handle by not polling: a
// doorbell that cannot be reached is a doorbell that is absent, and the integration runs correctly
// with it absent. Turning a Pub/Sub outage into a missed poll would make the optional path load-bearing.
func (d *Doorbell) Drain(ctx context.Context, src DoorbellSource) DoorbellOutcome {
	if src == nil {
		return DoorbellOutcome{Reason: DoorbellEmpty}
	}
	count, err := src.Drain(ctx)
	if err != nil {
		d.mu.Lock()
		d.drainErrors = append(d.drainErrors, err.Error())
		d.mu.Unlock()
		return DoorbellOutcome{Reason: DoorbellEmpty}
	}
	return d.Ring(count)
}

// DoorbellReport is what the checkpoint says about the doorbell. It is counts and a subscription
// name: there is nothing else a notification left behind.
type DoorbellReport struct {
	// Subscription is the resource the one declared non-read-only grant is bound to.
	Subscription string
	// Rings is how many notifications were seen, Honoured how many earned a poll, and
	// RateLimited how many were dropped by the rate limit.
	Rings       int
	Honoured    int
	RateLimited int
	// DrainErrors are the failures reaching the subscription, so "the doorbell was quiet" and
	// "the doorbell was unreachable" stay different statements.
	DrainErrors []string
}

// Report returns the counts.
func (d *Doorbell) Report() DoorbellReport {
	d.mu.Lock()
	defer d.mu.Unlock()
	errs := append([]string(nil), d.drainErrors...)
	sort.Strings(errs)
	return DoorbellReport{
		Subscription: d.subscription,
		Rings:        d.rings,
		Honoured:     d.honoured,
		RateLimited:  d.rateLimited,
		DrainErrors:  errs,
	}
}

// String renders the report for the checkpoint note, deterministically.
func (r DoorbellReport) String() string {
	out := fmt.Sprintf("subscription=%s rings=%d honoured=%d rate_limited=%d",
		r.Subscription, r.Rings, r.Honoured, r.RateLimited)
	if len(r.DrainErrors) > 0 {
		out += " drain_errors=[" + strings.Join(r.DrainErrors, "; ") + "]"
	}
	return out
}
