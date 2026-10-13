// SPDX-License-Identifier: Apache-2.0

package chat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/internal/investigation/engine"
	"github.com/Pierre-Theophile/aisre/internal/slackx"
	sdk "github.com/Pierre-Theophile/aisre/pkg/backend"
)

// The live chat backend: one channel's history, read in memory and turned into a digest.

// BackendName is the backend's published name.
const BackendName = "slack-chat"

// The coverage reasons a chat answer can carry (006 FR-033). Each opens the failure detail or the
// truncation it explains, so a fixture can assert on it and a reader can act on it.
const (
	// ReasonNotEnabled: the reader was built without history; the chat reader is off.
	ReasonNotEnabled = "not_enabled"
	// ReasonNotMember: the reader app is not in the channel. It never joins one; a person
	// invites it.
	ReasonNotMember = "not_member"
	// ReasonPrivateChannel: the channel is private, archived away or gone, and is not observed.
	ReasonPrivateChannel = "private_channel"
	// ReasonRetentionTruncated: the workspace's plan keeps less history than the window asks for.
	ReasonRetentionTruncated = "retention_truncated"
	// ReasonRateLimited: Slack asked the reader to wait.
	ReasonRateLimited = "rate_limited"
	// ReasonOutsideOrigin: the term names a channel outside the workspace this backend reads.
	ReasonOutsideOrigin = "outside_origin"
)

// threadScope is the standing statement of what a history read covers.
const threadScope = "top-level messages and thread broadcasts only; thread replies are not read"

// maxPages bounds one call: 50 pages of 200 is ten thousand messages, far more than an incident
// channel holds in a window, and a call nobody can bound is a call nobody can budget.
const maxPages = 50

// History is the one read the backend needs: a page of a channel's messages in a window. It is
// slackx.Client's, named here so a test can stand in for it.
type History interface {
	History(ctx context.Context, channel string, oldest, latest time.Time, cursor string) (slackx.HistoryPage, error)
}

// Options configure the live backend.
type Options struct {
	// Team is the workspace id the reader belongs to, from auth.test.
	Team string
	// WorkspaceURL is the workspace's address, "https://<domain>.slack.com/", from auth.test.
	// Permalinks are built on it; without it they are origin references.
	WorkspaceURL string
	// Retention is how much history the workspace's plan keeps; zero is unlimited.
	Retention time.Duration
	// Now is the clock; nil is the wall clock.
	Now func() time.Time
}

// Slack is the live chat backend.
type Slack struct {
	history History
	opts    Options
}

var _ sdk.TelemetryBackend = (*Slack)(nil)

// NewSlack returns the live backend over a reader client built with History.
func NewSlack(history History, opts Options) (*Slack, error) {
	if history == nil {
		return nil, errors.New("chat: no history reader")
	}
	if strings.TrimSpace(opts.Team) == "" {
		return nil, errors.New("chat: the backend is bound to no workspace; a worker bound to no source of truth can answer nothing")
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Slack{history: history, opts: opts}, nil
}

// SourceOfTruth is the workspace the backend reads, "slack:<team>".
func (s *Slack) SourceOfTruth() string { return "slack:" + s.opts.Team }

// Describe returns the backend's declaration.
func (s *Slack) Describe() sdk.Description {
	terms := sdk.Terms(sdk.FamilyChat)
	costs := make(map[string]sdk.CostClass, len(terms))
	capabilities := make([]sdk.Capability, 0, len(terms))
	for _, term := range terms {
		class := backend.PublishedCostClass(term)
		costs[term] = class
		capabilities = append(capabilities, sdk.Capability{Name: term, ReadOnly: true, CostClass: class})
	}
	return sdk.Description{
		Name:           BackendName,
		Vendor:         "slack",
		Terms:          terms,
		CostClasses:    costs,
		Capabilities:   capabilities,
		Redaction:      backend.DefaultRedactionPolicy(),
		Version:        Version,
		AlgebraVersion: sdk.TermVersion(sdk.TermChatMentions),
	}
}

// message is what one call holds of a message, for as long as the call lasts.
type message struct {
	at        time.Time
	author    string
	text      string
	permalink string
}

// Execute answers one chat term.
func (s *Slack) Execute(ctx context.Context, req *sdk.AlgebraRequest) (*sdk.AlgebraResponse, error) {
	term := req.GetTerm()
	name := backend.TermName(term)
	if sdk.FamilyOf(name) != sdk.FamilyChat {
		return s.failed(req, investigationv1.FailureReason_OUTSIDE_ALGEBRA, nil,
			"%s is not a chat term; this backend answers %v", name, sdk.Terms(sdk.FamilyChat))
	}
	if err := backend.Validate(term); err != nil {
		return s.failed(req, investigationv1.FailureReason_OUTSIDE_ALGEBRA, nil, "%v", err)
	}

	// Nobody sees the future (constitution II): a window past observed_at is cut to it before
	// anything is read.
	clamped, horizon, state := backend.ClampRequest(req)
	window, channelRef := chatScope(clamped.GetTerm())
	team, channel, ok := splitChannel(channelRef)
	if !ok || team != s.opts.Team {
		return s.failed(req, investigationv1.FailureReason_NOT_PERMITTED, nil,
			"%s: %s is not a channel of workspace %s, which is the only one this reader reads (006 FR-030)",
			ReasonOutsideOrigin, channelRef, s.opts.Team)
	}
	executed := s.opts.Now().UTC()
	if state == backend.PastHorizon {
		coverage, err := s.coverage(channelRef, window, 0, "", executed)
		if err != nil {
			return nil, err
		}
		backend.AnnotateCoverageHorizon(coverage, horizon, state)
		return s.respond(req, backend.NoData{Coverage: coverage}, channelRef, window)
	}

	start, end := window.GetStart().AsTime().UTC(), window.GetEnd().AsTime().UTC()
	var truncation []string
	if s.opts.Retention > 0 {
		if kept := executed.Add(-s.opts.Retention); start.Before(kept) {
			truncation = append(truncation, fmt.Sprintf("%s: the workspace keeps %s of history; before %s is not observed",
				ReasonRetentionTruncated, s.opts.Retention, kept.Format(time.RFC3339)))
			start = kept
			if !start.Before(end) {
				start = end
			}
		}
	}
	covered := backend.NewWindow(start, end)

	messages, pageCapped, err := s.read(ctx, channel, channelRef, start, end)
	if err != nil {
		return s.readFailed(req, channelRef, covered, executed, err)
	}
	if pageCapped {
		truncation = append(truncation, fmt.Sprintf("the read stopped at %d pages; later messages are not counted", maxPages))
	}
	truncation = append(truncation, threadScope)

	coverage, err := s.coverage(channelRef, covered, int64(len(messages)), strings.Join(truncation, "; "), executed)
	if err != nil {
		return nil, err
	}
	backend.AnnotateCoverageHorizon(coverage, horizon, state)

	var outcome backend.Outcome
	switch t := clamped.GetTerm().GetTerm().(type) {
	case *investigationv1.AlgebraTerm_ChatMentions:
		outcome = mentions(t.ChatMentions, messages, coverage)
	case *investigationv1.AlgebraTerm_ChatTimeline:
		outcome = timeline(t.ChatTimeline, start, end, messages, coverage)
	}
	if strings.Contains(coverage.GetTruncation(), ReasonRetentionTruncated) {
		if body, ok := outcome.(backend.DigestOutcome); ok {
			outcome = backend.Partial{Body: body.Body, Missing: ReasonRetentionTruncated + ": history before the plan's retention"}
		}
	}
	return s.respond(req, outcome, channelRef, covered)
}

// read pages through the window, keeping the messages people wrote: no bot, no join or topic
// notice, and never this deployment's own report.
func (s *Slack) read(ctx context.Context, channel, channelRef string, start, end time.Time) ([]message, bool, error) {
	var out []message
	cursor := ""
	for page := 0; page < maxPages; page++ {
		got, err := s.history.History(ctx, channel, start, end, cursor)
		if err != nil {
			return nil, false, err
		}
		for _, m := range got.Messages {
			if m.BotID != "" || (m.Subtype != "" && m.Subtype != "thread_broadcast") || m.User == "" {
				continue
			}
			at := m.At()
			if at.Before(start) || !at.Before(end) {
				continue
			}
			out = append(out, message{at: at, author: m.User, text: m.Text, permalink: s.permalink(channel, channelRef, m.TS)})
		}
		if !got.HasMore || got.NextCursor == "" {
			return out, false, nil
		}
		cursor = got.NextCursor
	}
	return out, true, nil
}

// permalink is the message's link on the workspace's own address, or its origin reference when
// the address is unknown.
func (s *Slack) permalink(channel, channelRef, ts string) string {
	if s.opts.WorkspaceURL == "" {
		return channelRef + "/" + ts
	}
	return strings.TrimSuffix(s.opts.WorkspaceURL, "/") + "/archives/" + channel + "/p" + strings.Replace(ts, ".", "", 1)
}

// mentions is the chat_mentions digest: the messages matching one class, counted.
func mentions(term *investigationv1.ChatMentionsTerm, messages []message, coverage *investigationv1.Coverage) backend.Outcome {
	class := strings.ToLower(strings.TrimSpace(term.GetTermClass()))
	digest := &investigationv1.ChatMentionsDigest{TermClass: class, Vocabulary: Vocabulary}
	authors := map[string]struct{}{}
	for _, m := range messages {
		// Instruction-like messages are counted over everything read, whatever the class: an
		// attempt is recorded where it was seen, and the text goes nowhere (FR-035).
		if len(engine.InstructionPatterns(m.text)) > 0 {
			digest.InstructionLike++
		}
		if !Matches(class, m.text) {
			continue
		}
		digest.Messages++
		authors[m.author] = struct{}{}
		if digest.FirstAt == nil || m.at.Before(digest.GetFirstAt().AsTime()) {
			digest.FirstAt = timestamppb.New(m.at)
		}
		if digest.LastAt == nil || m.at.After(digest.GetLastAt().AsTime()) {
			digest.LastAt = timestamppb.New(m.at)
		}
		digest.Permalinks = append(digest.Permalinks, m.permalink)
	}
	digest.Participants = int64(len(authors))
	if digest.Messages == 0 {
		// The channel was read and nobody said it. A recorded attempt still has to reach the
		// engine, so an answer carrying one stays a digest.
		if digest.InstructionLike == 0 {
			return backend.NoData{Coverage: coverage}
		}
	}
	return backend.DigestOutcome{Body: &investigationv1.Digest{
		Body:     &investigationv1.Digest_ChatMentions{ChatMentions: digest},
		Coverage: coverage,
	}}
}

// timeline is the chat_timeline digest: messages and distinct participants per bucket, every
// bucket present so a quiet stretch reads as quiet.
func timeline(term *investigationv1.ChatTimelineTerm, start, end time.Time, messages []message, coverage *investigationv1.Coverage) backend.Outcome {
	width := time.Duration(term.GetBucketSeconds()) * time.Second
	if len(messages) == 0 || !start.Before(end) {
		return backend.NoData{Coverage: coverage}
	}
	// Buckets are aligned on the window the term asked for, so the same question bucketed the
	// same way lines up whatever retention cut from its start.
	origin := term.GetWindow().GetStart().AsTime().UTC()
	first := origin.Add(start.Sub(origin) / width * width)
	var buckets []*investigationv1.ChatBucket
	authors := []map[string]struct{}{}
	for at := first; at.Before(end); at = at.Add(width) {
		buckets = append(buckets, &investigationv1.ChatBucket{Start: timestamppb.New(at)})
		authors = append(authors, map[string]struct{}{})
	}
	for _, m := range messages {
		i := int(m.at.Sub(first) / width)
		if i < 0 || i >= len(buckets) {
			continue
		}
		buckets[i].Messages++
		authors[i][m.author] = struct{}{}
	}
	for i := range buckets {
		buckets[i].Participants = int64(len(authors[i]))
	}
	return backend.DigestOutcome{Body: &investigationv1.Digest{
		Body: &investigationv1.Digest_ChatTimeline{ChatTimeline: &investigationv1.ChatTimelineDigest{
			Buckets: buckets, BucketSeconds: term.GetBucketSeconds(),
		}},
		Coverage: coverage,
	}}
}

// readFailed turns a history read that did not answer into the published outcome and reason.
func (s *Slack) readFailed(req *sdk.AlgebraRequest, channelRef string, covered *backend.Window, executed time.Time, err error) (*sdk.AlgebraResponse, error) {
	if errors.Is(err, slackx.ErrHistoryDisabled) {
		return s.failed(req, investigationv1.FailureReason_NOT_PERMITTED, nil,
			"%s: the chat reader is off; it is enabled per deployment and reads only the incident's own channel", ReasonNotEnabled)
	}
	var apiErr *slackx.APIError
	if !errors.As(err, &apiErr) {
		return nil, err
	}
	switch apiErr.Code {
	case "not_in_channel":
		return s.failed(req, investigationv1.FailureReason_NOT_PERMITTED, nil,
			"%s: the reader app is not in %s; it never joins a channel, so a person invites it", ReasonNotMember, channelRef)
	case "channel_not_found", "is_archived", "missing_scope":
		return s.failed(req, investigationv1.FailureReason_NOT_PERMITTED, nil,
			"%s: %s is not observed (%s); a private channel is never read", ReasonPrivateChannel, channelRef, apiErr.Code)
	case "ratelimited", "rate_limited":
		return s.failed(req, investigationv1.FailureReason_RATE_LIMITED, nil,
			"%s: Slack asked the reader to wait %s", ReasonRateLimited, apiErr.RetryAfter)
	default:
		return s.failed(req, investigationv1.FailureReason_REJECTED_BY_BACKEND, nil,
			"conversations.history answered %s for %s", apiErr.Code, channelRef)
	}
}

func (s *Slack) coverage(channelRef string, window *backend.Window, volume int64, truncation string, executed time.Time) (*investigationv1.Coverage, error) {
	return backend.CoverageInput{
		SearchedEntities: []string{channelRef},
		DataSource:       channelRef + " conversations.history",
		WindowCovered:    window,
		VolumeConsidered: volume,
		Sampling:         "none",
		Truncation:       truncation,
		// A message is readable the instant it is posted.
		IngestionLag:      0,
		ExecutedAt:        executed,
		QuotaUndetermined: true,
	}.Coverage()
}

func (s *Slack) failed(req *sdk.AlgebraRequest, reason investigationv1.FailureReason, coverage *investigationv1.Coverage, format string, args ...any) (*sdk.AlgebraResponse, error) {
	return backend.NewResponse(backend.ResponseInput{
		Request:        req,
		Outcome:        backend.QueryFailed{Reason: reason, Detail: fmt.Sprintf(format, args...), Coverage: coverage},
		Mode:           string(backend.ModeLive),
		CostClass:      backend.PublishedCostClass(backend.TermName(req.GetTerm())),
		BackendVersion: Version,
	})
}

func (s *Slack) respond(req *sdk.AlgebraRequest, outcome backend.Outcome, channelRef string, window *backend.Window) (*sdk.AlgebraResponse, error) {
	return backend.NewResponse(backend.ResponseInput{
		Request:        req,
		Outcome:        outcome,
		Mode:           string(backend.ModeLive),
		CostClass:      backend.PublishedCostClass(backend.TermName(req.GetTerm())),
		BackendVersion: Version,
		Vocabulary:     Vocabulary,
		ExecutedQuery: fmt.Sprintf("conversations.history channel=%s oldest=%s latest=%s",
			channelRef, window.GetStart().AsTime().UTC().Format(time.RFC3339), window.GetEnd().AsTime().UTC().Format(time.RFC3339)),
	})
}

// chatScope reads the window and the channel a chat term carries.
func chatScope(term *backend.Term) (*backend.Window, string) {
	switch t := term.GetTerm().(type) {
	case *investigationv1.AlgebraTerm_ChatMentions:
		return t.ChatMentions.GetWindow(), t.ChatMentions.GetChannelRef()
	case *investigationv1.AlgebraTerm_ChatTimeline:
		return t.ChatTimeline.GetWindow(), t.ChatTimeline.GetChannelRef()
	}
	return nil, ""
}

// splitChannel reads "slack:<team>/<channel>".
func splitChannel(ref string) (team, channel string, ok bool) {
	rest, found := strings.CutPrefix(ref, "slack:")
	if !found {
		return "", "", false
	}
	team, channel, found = strings.Cut(rest, "/")
	if !found || team == "" || channel == "" || strings.Contains(channel, "/") {
		return "", "", false
	}
	return team, channel, true
}
