// SPDX-License-Identifier: Apache-2.0

// Package doorbell is the shared "poll now, and nothing else" signal a feeder may be woken by (003
// FR-050, 005 FR-025b; lifted from internal/feeders/gcp/doorbell.go by 005 T023).
//
// A doorbell is never a source of data. Whatever arrives — a Pub/Sub message, a webhook — the most it
// can become is a count, and the count earns at most one early poll per interval; the poll then reads
// the platform's API, which is the truth. So a forged or replayed flood costs one extra read of the
// truth at worst, and can never create, alter or retract anything.
//
// The guarantee is in the signatures rather than in a convention: Bell has no field a payload could be
// stored in, and Ring takes an int.
package doorbell

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Source is a pull channel: Drain returns how many notifications were waiting, and never their
// contents.
type Source interface {
	Drain(ctx context.Context) (int, error)
}

// DefaultBurst is how many honoured rings may happen back to back before the interval binds. One,
// because a burst is exactly the shape a forged flood takes and a real one carries no more information
// than its first message.
const DefaultBurst = 1

// Options configures one bell.
type Options struct {
	// Channel names the resource the bell listens on — a subscription, an endpoint path — so the
	// report says which one. Required: the honest states are "off" and "bound to this channel".
	Channel string
	// ChannelLabel is the word the report prints before the channel, e.g. "subscription". Empty
	// uses "channel".
	ChannelLabel string
	// MinInterval is the shortest gap between honoured rings: the rate limit. Required, because the
	// right floor is the connector's own poll floor, which this package cannot know.
	MinInterval time.Duration
	// Burst is how many honoured rings may happen back to back. Zero uses DefaultBurst.
	Burst int
	// Now is the clock. Nil uses time.Now.
	Now func() time.Time
}

// ErrNoChannel is returned for a bell with no channel named.
var ErrNoChannel = errors.New("doorbell: a bell with no channel")

// Bell decides whether a notification earns an early poll.
type Bell struct {
	label       string
	channel     string
	minInterval time.Duration
	burst       int
	now         func() time.Time

	mu       sync.Mutex
	tokens   int
	lastFill time.Time
	// Counters, for the report. They are the only thing a notification leaves behind.
	rings       int
	honoured    int
	rateLimited int
	refused     int
	drainErrors []string
}

// New returns a bell over opts.
func New(opts Options) (*Bell, error) {
	if strings.TrimSpace(opts.Channel) == "" {
		return nil, ErrNoChannel
	}
	if opts.MinInterval <= 0 {
		return nil, fmt.Errorf("doorbell: %s names no minimum interval; a bell with no rate limit "+
			"lets a flood outspend the schedule it anticipates", opts.Channel)
	}
	burst := opts.Burst
	if burst <= 0 {
		burst = DefaultBurst
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	label := opts.ChannelLabel
	if label == "" {
		label = "channel"
	}
	return &Bell{
		label: label, channel: opts.Channel, minInterval: opts.MinInterval, burst: burst, now: now,
		tokens: burst, lastFill: now(),
	}, nil
}

// Outcome is what one ring earned.
type Outcome struct {
	// PollNow says the caller should poll earlier than its schedule would have.
	PollNow bool
	// Reason names why, or why not, in the published vocabulary below.
	Reason string
}

// The published outcomes.
const (
	// Poll is a ring that earned an early poll.
	Poll = "poll_now"
	// RateLimited is a ring inside the minimum interval. Dropping it costs nothing: the scheduled
	// poll is still coming, and whatever it would have found is read from the API either way.
	RateLimited = "rate_limited"
	// Empty is a drain that found nothing waiting.
	Empty = "no_notifications"
	// Refused is a notification that failed the transport's check (a missing or wrong secret). It
	// never reaches the bucket.
	Refused = "refused"
)

// Ring records that notifications arrived and returns whether to poll now.
//
// It takes no payload. count is how many arrived, used only to count them: one notification and fifty
// earn the same single poll, because the poll reads the whole window regardless.
func (b *Bell) Ring(count int) Outcome {
	if count <= 0 {
		return Outcome{Reason: Empty}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rings += count

	now := b.now()
	// A plain token bucket: one token per interval, capped at the burst. It is what bounds a flood to
	// at most one extra poll per interval however many messages arrive.
	if elapsed := now.Sub(b.lastFill); elapsed >= b.minInterval {
		gained := int(elapsed / b.minInterval)
		b.tokens = min(b.burst, b.tokens+gained)
		b.lastFill = now
	}
	if b.tokens <= 0 {
		b.rateLimited += count
		return Outcome{Reason: RateLimited}
	}
	b.tokens--
	b.honoured++
	return Outcome{PollNow: true, Reason: Poll}
}

// refuse counts a notification the transport turned away before the bucket.
func (b *Bell) refuse() Outcome {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refused++
	return Outcome{Reason: Refused}
}

// Drain pulls a source and rings. A drain that fails is recorded and is not an error the caller must
// handle by not polling: an unreachable doorbell is an absent one, and a feeder runs correctly with it
// absent.
func (b *Bell) Drain(ctx context.Context, src Source) Outcome {
	if src == nil {
		return Outcome{Reason: Empty}
	}
	count, err := src.Drain(ctx)
	if err != nil {
		b.mu.Lock()
		b.drainErrors = append(b.drainErrors, err.Error())
		b.mu.Unlock()
		return Outcome{Reason: Empty}
	}
	return b.Ring(count)
}

// Report is what the checkpoint says about the bell: counts and a channel name, which is everything a
// notification left behind.
type Report struct {
	ChannelLabel string
	Channel      string
	// Rings is how many notifications reached the bucket, Honoured how many earned a poll,
	// RateLimited how many the bucket dropped, and Refused how many the transport turned away first.
	Rings       int
	Honoured    int
	RateLimited int
	Refused     int
	// DrainErrors keep "the doorbell was quiet" and "the doorbell was unreachable" different.
	DrainErrors []string
}

// Report returns the counts.
func (b *Bell) Report() Report {
	b.mu.Lock()
	defer b.mu.Unlock()
	errs := append([]string(nil), b.drainErrors...)
	sort.Strings(errs)
	return Report{
		ChannelLabel: b.label, Channel: b.channel,
		Rings: b.rings, Honoured: b.honoured, RateLimited: b.rateLimited, Refused: b.refused,
		DrainErrors: errs,
	}
}

// String renders the report for a checkpoint note, deterministically. Refused is printed only when
// non-zero, so a pull bell — which refuses nothing — renders as it always did.
func (r Report) String() string {
	out := fmt.Sprintf("%s=%s rings=%d honoured=%d rate_limited=%d",
		r.ChannelLabel, r.Channel, r.Rings, r.Honoured, r.RateLimited)
	if r.Refused > 0 {
		out += fmt.Sprintf(" refused=%d", r.Refused)
	}
	if len(r.DrainErrors) > 0 {
		out += " drain_errors=[" + strings.Join(r.DrainErrors, "; ") + "]"
	}
	return out
}
