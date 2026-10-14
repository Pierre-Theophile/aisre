// SPDX-License-Identifier: Apache-2.0

package slackx

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/doorbell"
)

// The Socket Mode doorbell (006 FR-011, T016; contracts/slack-connector.md §2).
//
// Slack pushes an event when something happens in the workspace — a channel created, renamed,
// archived — and the doorbell turns each one into a count and nothing else. The poller then reads
// the channel list, which is the truth; so an event that is forged, replayed, out of order or about
// a channel the rule ignores costs at most one early read of the list, and nothing an event says can
// create, alter or retract a declaration (pkg/feeder/doorbell).
//
// What this file reads from a message is its type and its envelope id: the type to tell a greeting
// and a disconnect from an event, the id to acknowledge it so Slack does not deliver it again. The
// event's payload is never decoded into anything, and there is no field in this package it could be
// stored in.
//
// The connection is opened with the reader app's app-level token, which carries
// `connections:write`. That scope writes nothing to the workspace, but it is named like a write, so
// the token is a feeder.DoorbellCredential rather than a read credential: CheckReadOnly stays strict
// for everything that reads, and the doorbell's token can open this socket and nothing else (G2).

// OpAppsConnectionsOpen is the doorbell's one Web API call: it returns the socket's URL. It is not on
// Surface, because it is a POST with a doorbell credential rather than a read with the reader's.
const OpAppsConnectionsOpen = "POST /api/apps.connections.open"

// DoorbellScopes is everything the doorbell's app-level token may hold.
var DoorbellScopes = []string{"connections:write"}

// NewDoorbellCredential wraps an app-level token (`xapp-`) as the doorbell's credential, refusing any
// other kind of token before anything leaves the process.
func NewDoorbellCredential(token string) (*feeder.DoorbellCredential, error) {
	token = strings.TrimSpace(token)
	switch {
	case token == "":
		return nil, errors.New("slackx: no app-level token; the doorbell opens its socket with the reader " +
			"app's app-level token (xapp-)")
	case !strings.HasPrefix(token, "xapp-"):
		return nil, errors.New("slackx: the doorbell's credential must be the reader app's app-level token " +
			"(xapp-); a bot or user token reads or writes, and the doorbell does neither")
	}
	return feeder.NewDoorbellCredential(Platform, token, nil, DoorbellScopes)
}

// SocketOptions configures the doorbell's socket.
type SocketOptions struct {
	// Credential opens the socket. Required.
	Credential *feeder.DoorbellCredential
	// BaseURL overrides DefaultBaseURL, for tests against a twin.
	BaseURL string
	// HTTP is the transport for apps.connections.open. Nil uses a client with a 30 s timeout.
	HTTP *http.Client
	// Dialer and TLS are for the socket itself; nil uses defaults.
	Dialer *net.Dialer
	TLS    *tls.Config
	// IdleTimeout is how long the socket may be silent — no event, no ping — before it is presumed
	// dead and reopened. Zero uses DefaultIdleTimeout.
	IdleTimeout time.Duration
	// MinBackoff and MaxBackoff bound the wait between two failed opens. Zero uses 1 s and 2 min.
	MinBackoff, MaxBackoff time.Duration
	// Logger, nil for slog.Default.
	Logger *slog.Logger
}

// DefaultIdleTimeout is the silence after which a socket is reopened. Slack pings an open socket
// well inside it.
const DefaultIdleTimeout = 2 * time.Minute

// Socket is the doorbell's connection. It is a doorbell.Source: Drain returns how many envelopes
// arrived since the last drain, and never what they said.
type Socket struct {
	opts SocketOptions
	base string
	http *http.Client
	log  *slog.Logger

	mu       sync.Mutex
	pending  int
	lastErr  error
	opened   int
	received int
}

var _ doorbell.Source = (*Socket)(nil)

// NewSocket builds the socket; Run opens it.
func NewSocket(opts SocketOptions) (*Socket, error) {
	if opts.Credential == nil {
		return nil, errors.New("slackx: a doorbell socket with no credential")
	}
	base := strings.TrimRight(opts.BaseURL, "/")
	if base == "" {
		base = DefaultBaseURL
	}
	client := opts.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = DefaultIdleTimeout
	}
	if opts.MinBackoff <= 0 {
		opts.MinBackoff = time.Second
	}
	if opts.MaxBackoff < opts.MinBackoff {
		opts.MaxBackoff = 2 * time.Minute
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Socket{opts: opts, base: base, http: client, log: logger}, nil
}

// Drain returns the envelopes counted since the last drain. A connection failure is returned once,
// when nothing is pending, so a bell reports "unreachable" separately from "quiet".
func (s *Socket) Drain(context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n := s.pending; n > 0 {
		s.pending = 0
		return n, nil
	}
	if err := s.lastErr; err != nil {
		s.lastErr = nil
		return 0, err
	}
	return 0, nil
}

// Stats is what the socket did: how many times it opened and how many envelopes it counted.
func (s *Socket) Stats() (opened, received int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.opened, s.received
}

// DoorbellRefusedError is an app-level token Slack says holds more than the doorbell needs. It stops
// the doorbell for good; the poller carries on without it.
type DoorbellRefusedError struct{ Err error }

func (e *DoorbellRefusedError) Error() string { return e.Err.Error() }
func (e *DoorbellRefusedError) Unwrap() error { return e.Err }

// Run keeps the socket open until ctx ends, reopening it after a disconnect or a failure with a
// bounded backoff. It returns nil when ctx ends, and an error only for a credential it must not use.
func (s *Socket) Run(ctx context.Context) error {
	backoff := s.opts.MinBackoff
	for ctx.Err() == nil {
		err := s.session(ctx)
		if ctx.Err() != nil {
			return nil
		}
		var refused *DoorbellRefusedError
		if errors.As(err, &refused) {
			s.fail(err)
			return err
		}
		if err == nil {
			// A clean disconnect Slack asked for: reopen at once.
			backoff = s.opts.MinBackoff
			continue
		}
		s.fail(err)
		s.log.WarnContext(ctx, "slack doorbell: the socket closed; polling carries on without it until it reopens",
			"error", err.Error(), "retry_in", backoff.String())
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		backoff = min(2*backoff, s.opts.MaxBackoff)
	}
	return nil
}

func (s *Socket) fail(err error) {
	s.mu.Lock()
	s.lastErr = err
	s.mu.Unlock()
}

// session opens one socket and reads it until it closes. It returns nil for a disconnect Slack
// announced, which is how Slack rotates sockets.
func (s *Socket) session(ctx context.Context) error {
	wsURL, err := s.open(ctx)
	if err != nil {
		return err
	}
	conn, err := dialWebSocket(ctx, wsURL, s.opts.Dialer, s.opts.TLS)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	s.mu.Lock()
	s.opened++
	s.mu.Unlock()
	for {
		message, err := conn.readMessage(time.Now().Add(s.opts.IdleTimeout))
		if err != nil {
			if errors.Is(err, io.EOF) {
				return fmt.Errorf("slackx: the doorbell socket was closed by Slack")
			}
			return fmt.Errorf("slackx: doorbell socket: %w", err)
		}
		// The type and the envelope id, and nothing else: the payload is not a field here.
		var envelope struct {
			Type       string `json:"type"`
			EnvelopeID string `json:"envelope_id"`
		}
		if err := json.Unmarshal(message, &envelope); err != nil {
			// Not an envelope. It earns nothing, and a stream of them is a socket worth reopening.
			return fmt.Errorf("slackx: the doorbell socket sent a message that is not JSON")
		}
		switch {
		case envelope.Type == "disconnect":
			return nil
		case envelope.EnvelopeID != "":
			ack, err := json.Marshal(struct {
				EnvelopeID string `json:"envelope_id"`
			}{envelope.EnvelopeID})
			if err != nil {
				return err
			}
			if err := conn.writeFrame(opText, ack); err != nil {
				return fmt.Errorf("slackx: acknowledging a doorbell envelope: %w", err)
			}
			s.mu.Lock()
			s.pending++
			s.received++
			s.mu.Unlock()
		}
	}
}

// open asks Slack for a socket URL with the doorbell credential.
func (s *Socket) open(ctx context.Context) (string, error) {
	_, path, _ := strings.Cut(OpAppsConnectionsOpen, " ")
	var (
		status int
		scopes []string
		raw    []byte
	)
	err := s.opts.Credential.Open(func(token string) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.base+path, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := s.http.Do(req)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		status, scopes = resp.StatusCode, ParseScopes(resp.Header.Get("X-OAuth-Scopes"))
		raw, err = io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return err
	})
	if err != nil {
		return "", fmt.Errorf("slackx: %s: %w", OpAppsConnectionsOpen, err)
	}
	if extra := extraScopes(scopes); len(extra) > 0 {
		return "", &DoorbellRefusedError{Err: &feeder.DoorbellScopeError{Platform: Platform, Extra: extra}}
	}
	var body struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		URL   string `json:"url"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || status != http.StatusOK || !body.OK {
		code := body.Error
		if code == "" {
			code = "no_error_code"
		}
		if code == "invalid_auth" || code == "not_allowed_token_type" || code == "missing_scope" {
			return "", &DoorbellRefusedError{Err: fmt.Errorf("slackx: %s answered %d %s; the doorbell stays "+
				"off and polling carries on", OpAppsConnectionsOpen, status, code)}
		}
		return "", fmt.Errorf("slackx: %s answered %d %s", OpAppsConnectionsOpen, status, code)
	}
	// A plain ws:// socket is accepted only from a plain-HTTP twin: against Slack itself the socket is
	// TLS, as the call that named it was.
	plainTwin := strings.HasPrefix(s.base, "http://")
	if !strings.HasPrefix(body.URL, "wss://") && (!plainTwin || !strings.HasPrefix(body.URL, "ws://")) {
		return "", fmt.Errorf("slackx: %s returned no wss:// socket url", OpAppsConnectionsOpen)
	}
	return body.URL, nil
}

func extraScopes(reported []string) []string {
	var extra []string
	for _, scope := range reported {
		if !slices.Contains(DoorbellScopes, scope) {
			extra = append(extra, scope)
		}
	}
	return extra
}
