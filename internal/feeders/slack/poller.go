// SPDX-License-Identifier: Apache-2.0

package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/slackx"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The live source: one payload per poll of the public channel list (FR-010).
//
// Polling is the truth. A doorbell (Socket Mode, FR-011) may cut a wait short, and that is all it can
// do: Ring carries no channel, no name and no instant, so an event Slack pushed — or one a forger
// pushed — can only make the next read of the list happen sooner.
//
// The poller keeps only channels created inside the horizon. A channel older than that cannot be
// declared (FR-012), so carrying it would only carry another team's channel name into the process and,
// on a recording run, towards the disk.

// DoorbellMinInterval is the shortest gap between two polls the doorbell may cause: a third of the
// default interval, so a flood of Socket Mode events costs at most three reads of the list a poll
// interval, against tier 2's twenty a minute (FR-011, FR-014).
const DoorbellMinInterval = 10 * time.Second

// Lister is the one read the poller makes. *slackx.Client satisfies it.
type Lister interface {
	ListChannels(ctx context.Context, cursor string, limit int) (slackx.ChannelPage, error)
}

// PollerOptions configures the poller.
type PollerOptions struct {
	Client   Lister
	TeamID   string
	Interval time.Duration
	Horizon  time.Duration
	// Cycles is how many polls to make; 0 polls until the context ends.
	Cycles int
	// Ring, when set, cuts the wait between two polls short. It is the doorbell's whole surface.
	Ring <-chan struct{}
	// MaxPages bounds one poll. A workspace with more public channels than MaxPages × 200 is polled
	// partially, and the payload says so with GapBefore on the next one.
	MaxPages int
	Now      func() time.Time
	Logger   *slog.Logger
}

// Poller is a feeder.Source over the live channel list.
type Poller struct {
	opts   PollerOptions
	polls  int
	gap    bool
	logger *slog.Logger
}

var _ feeder.Source = (*Poller)(nil)

// NewPoller builds a poller.
func NewPoller(opts PollerOptions) (*Poller, error) {
	if opts.Client == nil {
		return nil, errors.New("slack: a poller with no client")
	}
	if strings.TrimSpace(opts.TeamID) == "" {
		return nil, errors.New("slack: a poller with no workspace")
	}
	if opts.Interval <= 0 {
		opts.Interval = DefaultPollInterval
	}
	if opts.Horizon <= 0 {
		opts.Horizon = DefaultHorizon
	}
	if opts.MaxPages <= 0 {
		opts.MaxPages = 50
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Poller{opts: opts, logger: logger}, nil
}

// Next blocks until the next poll is due, reads the list and returns it as one payload.
func (p *Poller) Next(ctx context.Context) (feeder.Payload, error) {
	for {
		if p.opts.Cycles > 0 && p.polls >= p.opts.Cycles {
			return feeder.Payload{}, io.EOF
		}
		if p.polls > 0 || p.gap {
			if err := p.wait(ctx, p.opts.Interval); err != nil {
				return feeder.Payload{}, err
			}
		}
		payload, retryAfter, err := p.poll(ctx)
		if err == nil {
			p.polls++
			return payload, nil
		}
		if ctx.Err() != nil {
			return feeder.Payload{}, ctx.Err()
		}
		// A failed poll is a gap, not silence: the next payload says so, and the poller waits for
		// whatever Slack asked before trying again (FR-014).
		p.gap = true
		p.logger.WarnContext(ctx, "slack: the channel list could not be read; the next poll marks a gap",
			"error", err.Error(), "retry_after", retryAfter.String())
		if retryAfter > p.opts.Interval {
			if err := p.wait(ctx, retryAfter-p.opts.Interval); err != nil {
				return feeder.Payload{}, err
			}
		}
	}
}

func (p *Poller) wait(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	case <-p.opts.Ring:
		return nil
	}
}

func (p *Poller) poll(ctx context.Context) (feeder.Payload, time.Duration, error) {
	at := p.opts.Now().UTC()
	out := channelsPayload{Team: p.opts.TeamID, GapBefore: p.gap, Channels: []observedChannel{}}
	cursor := ""
	for page := 0; page < p.opts.MaxPages; page++ {
		got, err := p.opts.Client.ListChannels(ctx, cursor, 200)
		if err != nil {
			return feeder.Payload{}, retryAfter(err, at), err
		}
		for _, ch := range got.Channels {
			created := ch.CreatedAt()
			if at.Sub(created) > p.opts.Horizon {
				continue
			}
			out.Channels = append(out.Channels, observedChannel{
				ID: ch.ID, Name: ch.Name, Created: created, Creator: ch.Creator,
				Archived: ch.IsArchived, Shared: ch.IsShared, Private: ch.IsPrivate,
			})
		}
		cursor = got.NextCursor
		if cursor == "" {
			break
		}
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return feeder.Payload{}, 0, err
	}
	p.gap = cursor != ""
	return feeder.Payload{Kind: PayloadChannels, At: at, Bytes: raw}, 0, nil
}

// retryAfter is how long Slack, or the budget, asked the poller to wait.
func retryAfter(err error, now time.Time) time.Duration {
	var apiErr *slackx.APIError
	if errors.As(err, &apiErr) {
		return apiErr.RetryAfter
	}
	if yield, ok := feeder.YieldedForQuota(err); ok && !yield.RetryAt.IsZero() {
		return yield.RetryAt.Sub(now)
	}
	var deferred *slackx.DeferredError
	if errors.As(err, &deferred) && !deferred.RetryAt.IsZero() {
		return deferred.RetryAt.Sub(now)
	}
	return 0
}

// PreparePayload is the pre-pass a live recording runs before the sanitisation table (FR-050).
//
// A channel whose name the rule's pattern does not match is not an incident, and its name is another team's
// business: it is blanked, which a replay reads exactly as it read the original — "no rule match".
// The creator is a person and is dropped by the table itself; this pass leaves it to the table, so
// that the guarantee does not rest on two places remembering it.
func PreparePayload(rule Rule) func(kind string, raw []byte) ([]byte, error) {
	if rule.ID == "" {
		rule = DefaultRule
	}
	return func(kind string, raw []byte) ([]byte, error) {
		if kind != PayloadChannels {
			return nil, fmt.Errorf("slack: payload kind %q is not one this connector records", kind)
		}
		var poll channelsPayload
		if err := json.Unmarshal(raw, &poll); err != nil {
			return nil, fmt.Errorf("slack: decoding a %s payload: %w", kind, err)
		}
		for i := range poll.Channels {
			// A private channel's name is blanked whatever it is; the feeder skips it as private
			// before it reads the name, so the replay says the same.
			// The pattern, not the match: an excluded test channel keeps its name, so a replay
			// reports it as excluded rather than as no match.
			if poll.Channels[i].Private ||
				!rule.Pattern.MatchString(strings.ToLower(strings.TrimSpace(poll.Channels[i].Name))) {
				poll.Channels[i].Name = ""
			}
		}
		return json.Marshal(poll)
	}
}
