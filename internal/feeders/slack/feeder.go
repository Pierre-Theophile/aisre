// SPDX-License-Identifier: Apache-2.0

package slack

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	eventlog "github.com/Pierre-Theophile/aisre/internal/log"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// Kind is the connector family, and SourceIDPrefix the prefix of every source id: one source per
// workspace, `slack:<team id>` (ADR-0011).
const (
	Kind           = "slack"
	SourceIDPrefix = "slack:"
)

// PayloadChannels is the one payload kind this feeder reads: one poll of the channel list, already
// reduced to the fields a declaration needs.
const PayloadChannels = "channels"

// PlaceNamespace is the namespace of the place a declared incident lives. It is the engine's
// (intake.PlaceNamespace), spelled here so that a connector does not import the engine; a test asserts
// the two agree, because the idempotency key rests on it.
const PlaceNamespace = "incident"

// UnrecordedCreator is the declaring identity of a channel whose creator a recording does not carry.
// People identifiers are dropped from a recording, never hashed (sanitise.ErrPeopleNeverHashed), so a
// replayed declaration names nobody and says so rather than inventing someone.
const UnrecordedCreator = "creator-not-recorded"

// Defaults.
const (
	// DefaultHorizon is how old a channel may be when first seen and still be declared (FR-012).
	DefaultHorizon = 24 * time.Hour
	// DefaultLateAfter is how long after its creation a channel may be first seen and still be
	// declared live rather than in review mode: twice the default poll interval, plus slack.
	DefaultLateAfter = 5 * time.Minute
	// DefaultPollInterval is FR-010's default.
	DefaultPollInterval = 30 * time.Second
)

// Options configures the feeder.
type Options struct {
	// TeamID is the workspace. Required: it is the suffix of the source id, so two workspaces are
	// two sources.
	TeamID string
	// Rule is the naming rule. The zero value is DefaultRule.
	Rule Rule
	// Services maps a slug token to the entity it names, as an operator stated it (FR-007). A token
	// not in the map is never a target.
	Services map[string]*graphv1.Ref
	// People maps a Slack user id to the deployment's OIDC subject, where an operator has stated
	// one (FR-052). Unmapped, the declaring identity is the Slack user's workspace-scoped
	// pseudonym under IdentityKey.
	People map[string]string
	// IdentityKey is the deployment's key for declaring-identity pseudonyms (FR-052): an unmapped
	// creator is `slack:<team>/u_<HMAC(team, user id)>`, stable across restarts for as long as the
	// key is. It is the deployment's own secret, never the corpus key: a recording drops the
	// creator instead (FR-050). Without it, an unmapped creator is recorded as
	// UnrecordedCreator — never as the raw user id.
	IdentityKey []byte
	// Horizon and LateAfter: see the defaults.
	Horizon   time.Duration
	LateAfter time.Duration
	// PollInterval is the live cadence; it bounds the reordering window.
	PollInterval time.Duration
	// IncludeShared admits channels shared with another organisation (Slack Connect). Off by
	// default: whose incident it is is not this organisation's to declare.
	IncludeShared bool
	// OnDeclared is called once per channel, after its declaration was accepted by the graph. It is
	// how the live run opens the investigation (cli: `feed slack`); a replay leaves it nil.
	OnDeclared func(ctx context.Context, d Declared)
	// Logger, nil for slog.Default.
	Logger *slog.Logger
}

// Declared is one declaration the feeder made: the event it emitted, in the shape the engine's
// Declare call takes.
type Declared struct {
	SourceID       string
	ChannelID      string
	ChannelName    string
	OriginRef      string
	DeclaredAt     time.Time
	FirstSeenAt    time.Time
	ReviewMode     bool
	Severity       string
	Title          string
	Declarer       string
	Targets        []*graphv1.Ref
	RuleRef        string
	IdempotencyKey string
}

// Feeder turns polls of a workspace's channel list into declarations.
type Feeder struct {
	opts Options
	desc feeder.Description
	log  *slog.Logger

	mu       sync.Mutex
	declared map[string]string // channel id → name at declaration
	skipped  map[string]string // channel id → why it is not declared, as of the last poll
	report   Report
}

// New builds a feeder.
func New(opts Options) (*Feeder, error) {
	team := strings.TrimSpace(opts.TeamID)
	if team == "" {
		return nil, errors.New("slack: no workspace (team id); it is the suffix of the source id, and an " +
			"empty one would make every workspace's declarations one source")
	}
	if opts.Rule.ID == "" {
		opts.Rule = DefaultRule
	}
	if err := opts.Rule.Validate(); err != nil {
		return nil, err
	}
	if opts.Horizon <= 0 {
		opts.Horizon = DefaultHorizon
	}
	if opts.LateAfter <= 0 {
		opts.LateAfter = DefaultLateAfter
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = DefaultPollInterval
	}
	for token, ref := range opts.Services {
		if ref.GetNamespace() == "" || ref.GetValue() == "" {
			return nil, fmt.Errorf("slack: the service map's entry for %q names no entity", token)
		}
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	f := &Feeder{opts: opts, log: logger, declared: map[string]string{}, skipped: map[string]string{}}
	f.desc = feeder.Description{
		SourceID: SourceIDPrefix + team,
		Kind:     Kind,
		Ordering: feeder.OrderingNone,
		// The poller reads the list in order, one poll per interval. A declaration's instant is
		// Slack's own creation instant, so which poll first sees a channel changes when the event
		// arrives and nothing in it — except its title, which is the name at first sight; that is
		// why the window is one poll interval and not the horizon.
		ReorderingWindow: opts.PollInterval,
		RequiredScopes:   []string{"channels:read (reader app)"},
	}
	return f, nil
}

// Describe returns the feeder's contract.
func (f *Feeder) Describe() feeder.Description { return f.desc }

// channelsPayload is one poll, reduced. It is what the poller writes and a recording holds.
type channelsPayload struct {
	Team     string            `json:"team"`
	Channels []observedChannel `json:"channels"`
	// GapBefore says the poller could not read the list for a while before this poll, so the
	// silence before it was ignorance rather than absence (FR-032).
	GapBefore bool `json:"gap_before,omitempty"`
}

// observedChannel is one channel as a poll saw it. Creator is absent from a recording.
type observedChannel struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Created  time.Time `json:"created"`
	Creator  string    `json:"creator,omitempty"`
	Archived bool      `json:"archived,omitempty"`
	Shared   bool      `json:"shared,omitempty"`
}

// Run reads polls from src and writes declarations to em.
func (f *Feeder) Run(ctx context.Context, src feeder.Source, em feeder.Emitter) error {
	if err := f.desc.Validate(); err != nil {
		return err
	}
	// A run is one process's view of the workspace: what an earlier run declared is the graph's to
	// remember (the key makes a re-declaration a no-op), not this value's.
	f.mu.Lock()
	f.declared = map[string]string{}
	f.skipped = map[string]string{}
	f.report = Report{}
	f.mu.Unlock()
	defer func() {
		if err := em.Flush(context.WithoutCancel(ctx)); err != nil {
			f.log.ErrorContext(ctx, "flush failed", "error", err)
		}
	}()
	for {
		payload, err := src.Next(ctx)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := f.apply(ctx, em, payload); err != nil {
			return err
		}
	}
}

func (f *Feeder) apply(ctx context.Context, em feeder.Emitter, payload feeder.Payload) error {
	if payload.Kind != PayloadChannels {
		return fmt.Errorf("slack: payload kind %q is not one this feeder reads (%s)", payload.Kind, PayloadChannels)
	}
	var poll channelsPayload
	if err := json.Unmarshal(payload.Bytes, &poll); err != nil {
		return fmt.Errorf("slack: decoding a %s payload: %w", payload.Kind, err)
	}
	at := payload.At.UTC()
	channels := append([]observedChannel(nil), poll.Channels...)
	sort.Slice(channels, func(i, j int) bool {
		if !channels[i].Created.Equal(channels[j].Created) {
			return channels[i].Created.Before(channels[j].Created)
		}
		return channels[i].ID < channels[j].ID
	})
	for _, ch := range channels {
		if err := f.observe(ctx, em, ch, at); err != nil {
			return err
		}
	}
	f.mu.Lock()
	f.report.Polls++
	f.report.LastPoll = at
	f.mu.Unlock()
	note := f.Report().Note()
	return em.Checkpoint(ctx, feeder.CheckpointFact{
		ExtentFrom: at.Add(-f.opts.PollInterval),
		ExtentTo:   at,
		GapBefore:  poll.GapBefore,
		Note:       note,
	})
}

func (f *Feeder) observe(ctx context.Context, em feeder.Emitter, ch observedChannel, at time.Time) error {
	if strings.TrimSpace(ch.ID) == "" || ch.Created.IsZero() {
		return fmt.Errorf("slack: a channel with no id or no creation instant; the key rests on both (FR-009)")
	}
	f.mu.Lock()
	previous, seen := f.declared[ch.ID]
	if seen {
		if previous != ch.Name && ch.Name != "" {
			// The key does not move (FR-008b): a rename is said, never re-declared.
			f.report.Renamed++
			f.declared[ch.ID] = ch.Name
		}
		f.mu.Unlock()
		return nil
	}
	f.mu.Unlock()

	match := f.opts.Rule.Match(ch.Name)
	switch {
	case !match.Matched:
		f.skip(ch.ID, match.Reason)
		return nil
	case ch.Shared && !f.opts.IncludeShared:
		f.skip(ch.ID, SkipShared)
		return nil
	case at.Sub(ch.Created) > f.opts.Horizon:
		f.skip(ch.ID, SkipOutsideWindow)
		return nil
	}

	d := f.declaration(ch, match, at)
	fact := feeder.AlertFact{
		Meta:              feeder.Meta{SourceObservedAt: at},
		Monitor:           feeder.Ref(PlaceNamespace, ch.ID),
		TransitionAt:      d.DeclaredAt,
		ToState:           eventlog.AlertStateDeclared,
		Watches:           d.Targets,
		Transport:         feeder.TransportHumanDeclared,
		OriginRef:         d.OriginRef,
		ActorKind:         graphv1.ActorKind_PERSON,
		Severity:          d.Severity,
		Title:             d.Title,
		DeclaringIdentity: d.Declarer,
	}
	if err := emitOne(ctx, em, feeder.ObserveAlertTransition(f.desc, d.IdempotencyKey, fact)); err != nil {
		return err
	}
	f.mu.Lock()
	f.declared[ch.ID] = ch.Name
	delete(f.skipped, ch.ID)
	f.report.Declared = append(f.report.Declared, ch.Name)
	if d.ReviewMode {
		f.report.Late++
	}
	f.mu.Unlock()
	if f.opts.OnDeclared != nil {
		f.opts.OnDeclared(ctx, d)
	}
	return nil
}

// declaration builds the declaration a matched channel makes.
func (f *Feeder) declaration(ch observedChannel, match Match, at time.Time) Declared {
	team := strings.TrimPrefix(f.desc.SourceID, SourceIDPrefix)
	d := Declared{
		SourceID:    f.desc.SourceID,
		ChannelID:   ch.ID,
		ChannelName: ch.Name,
		OriginRef:   OriginRef(team, ch.ID),
		DeclaredAt:  ch.Created.UTC(),
		FirstSeenAt: at,
		ReviewMode:  at.Sub(ch.Created) > f.opts.LateAfter,
		Severity:    match.Severity,
		Title:       ch.Name,
		Declarer:    f.declarer(team, ch.Creator),
		RuleRef:     f.opts.Rule.Ref(),
	}
	seen := map[string]bool{}
	for _, token := range match.Tokens {
		ref, ok := f.opts.Services[token]
		if !ok {
			continue
		}
		key := graph.RefFromProto(ref).String()
		if seen[key] {
			continue
		}
		seen[key] = true
		d.Targets = append(d.Targets, ref)
	}
	d.IdempotencyKey = DeclarationKey(d.SourceID, ch.ID, d.DeclaredAt)
	return d
}

func (f *Feeder) declarer(team, creator string) string {
	creator = strings.TrimSpace(creator)
	if creator == "" {
		return SourceIDPrefix + team + "/" + UnrecordedCreator
	}
	if subject, ok := f.opts.People[creator]; ok && strings.TrimSpace(subject) != "" {
		return subject
	}
	if len(f.opts.IdentityKey) == 0 {
		return SourceIDPrefix + team + "/" + UnrecordedCreator
	}
	return SourceIDPrefix + team + "/" + DeclarerPseudonym(f.opts.IdentityKey, team, creator)
}

// DeclarerPseudonym is the workspace-scoped pseudonym of a Slack user (FR-052): `u_` and the first
// 16 hex digits of HMAC-SHA256(key, team "/" user). The team is inside the MAC so that one person in
// two workspaces is two identities, as Slack itself has them.
func DeclarerPseudonym(key []byte, team, user string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(team + "/" + user))
	return "u_" + hex.EncodeToString(mac.Sum(nil))[:16]
}

// skip records why a channel is not declared. It is per channel, not per poll: a test channel listed
// by forty polls is one skipped channel.
func (f *Feeder) skip(channel, reason string) {
	f.mu.Lock()
	f.skipped[channel] = reason
	f.mu.Unlock()
}

// OriginRef is the human-facing reference of an incident channel: `slack:<team>/<channel>`. It is
// also the delivery target a report goes back to.
func OriginRef(team, channel string) string { return SourceIDPrefix + team + "/" + channel }

// ChannelOf reads the channel id back out of an origin reference.
func ChannelOf(originRef string) (team, channel string, ok bool) {
	rest, found := strings.CutPrefix(originRef, SourceIDPrefix)
	if !found {
		return "", "", false
	}
	team, channel, found = strings.Cut(rest, "/")
	if !found || team == "" || channel == "" || strings.Contains(channel, "/") {
		return "", "", false
	}
	return team, channel, true
}

// DeclarationKey is the published 4-tuple key of a channel's declaration: the source, the channel on
// its stable id, an empty group, and the creation instant (FR-009). It is intake.DeclarationKey
// spelled without importing the engine; a test asserts they agree byte for byte.
func DeclarationKey(sourceID, channelID string, declaredAt time.Time) string {
	return eventlog.AlertTransitionKeyParts(sourceID,
		graph.Ref{Namespace: PlaceNamespace, Value: channelID}.String(), "",
		declaredAt.UTC().Format(time.RFC3339Nano))
}

// Report is the run's account of itself (US5).
type Report struct {
	Polls    int
	LastPoll time.Time
	Declared []string
	Late     int
	Renamed  int
	// Skipped counts channels, by the reason each is not declared.
	Skipped map[string]int
}

// Note renders the report for a checkpoint and a log line.
func (r Report) Note() string {
	reasons := make([]string, 0, len(r.Skipped))
	for reason, n := range r.Skipped {
		reasons = append(reasons, fmt.Sprintf("%s: %d", reason, n))
	}
	sort.Strings(reasons)
	skipped := "none"
	if len(reasons) > 0 {
		skipped = strings.Join(reasons, ", ")
	}
	return fmt.Sprintf("slack intake: %d poll(s); %d channel(s) declared (%d in review mode); %d rename(s) "+
		"of declared channels, which never move the key; skipped by reason: %s",
		r.Polls, len(r.Declared), r.Late, r.Renamed, skipped)
}

// Report returns a copy of the report so far.
func (f *Feeder) Report() Report {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.report
	out.Declared = append([]string(nil), f.report.Declared...)
	out.Skipped = make(map[string]int, len(f.skipped))
	for _, reason := range f.skipped {
		out.Skipped[reason]++
	}
	return out
}

func emitOne(ctx context.Context, em feeder.Emitter, ev *graphv1.EventEnvelope) error {
	result, err := em.Emit(ctx, ev)
	if err != nil {
		return err
	}
	if result.GetStatus() == graphv1.IngestResult_REJECTED {
		return fmt.Errorf("slack: the graph refused event %s: %s %s",
			ev.GetEventId(), result.GetReasonCode(), result.GetReasonDetail())
	}
	return nil
}
