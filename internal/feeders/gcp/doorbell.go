// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"errors"
	"strings"
	"time"

	"github.com/Pierre-Theophile/aisre/pkg/feeder/doorbell"
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
type DoorbellSource = doorbell.Source

// The bell itself — the payload-free Ring and its token bucket — is pkg/feeder/doorbell, shared with
// the Datadog feeder (005 T023). What stays here is what is GCP's: the subscription, and a rate limit
// equal to the alert poll floor.

// DefaultDoorbellMinInterval is the shortest gap between two honoured rings. It is the rate limit
// FR-050 asks for, and the number is the alert poll ceiling: a doorbell that could trigger a poll
// more often than the schedule's own floor would let a flood outspend the schedule it is supposed to
// anticipate.
const DefaultDoorbellMinInterval = MinAlertPollInterval

// DefaultDoorbellBurst is how many honoured rings may happen back to back before the interval binds.
const DefaultDoorbellBurst = doorbell.DefaultBurst

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
type Doorbell = doorbell.Bell

// DoorbellOutcome is what one ring earned.
type DoorbellOutcome = doorbell.Outcome

// DoorbellReport is what the checkpoint says about the doorbell.
type DoorbellReport = doorbell.Report

// The published doorbell outcomes.
const (
	DoorbellPoll        = doorbell.Poll
	DoorbellRateLimited = doorbell.RateLimited
	DoorbellEmpty       = doorbell.Empty
)

// NewDoorbell returns a doorbell over opts.
func NewDoorbell(opts DoorbellOptions) (*Doorbell, error) {
	if strings.TrimSpace(opts.Subscription) == "" {
		return nil, ErrNoSubscription
	}
	interval := opts.MinInterval
	if interval <= 0 {
		interval = DefaultDoorbellMinInterval
	}
	return doorbell.New(doorbell.Options{
		Channel:      opts.Subscription,
		ChannelLabel: "subscription",
		MinInterval:  interval,
		Burst:        opts.Burst,
		Now:          opts.Now,
	})
}
