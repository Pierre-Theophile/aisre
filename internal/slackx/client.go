// SPDX-License-Identifier: Apache-2.0

// Package slackx is the read-only Slack Web API client the Slack connector's intake and chat reader
// share (006 FR-001–FR-003, FR-014).
//
// It is hand-written over net/http rather than a generated SDK, for the reason internal/datadogx
// gives (research R1): the connector reads with five operations, and the published read-only list is
// only a proof while it is the literal set of requests this binary can make. Every request goes
// through Do, which refuses an operation the surface does not carry before anything leaves the
// process.
//
// What this package cannot do is the point of it. It holds the **reader** app's bot token, and it has
// no method that posts, edits, joins, invites, archives, renames, reacts or pins. The one write the
// connector makes — the report — is in internal/investigation/render/slacksink, on the **reporter**
// app's token, which is a different type with its own two-operation list (ADR-0007 confinement 3,
// ADR-0011).
package slackx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// Platform is the connector's name in refusals and in the published operation page.
const Platform = "slack"

// DefaultBaseURL is the Web API's root.
const DefaultBaseURL = "https://slack.com"

// The published read operations (contracts/slack-connector.md §2). Slack's read methods all accept
// GET, so the method rule of pkg/feeder.ReadOnlySurface proves them without a named-query exception.
const (
	OpAuthTest             feeder.ReadOperation = "GET /api/auth.test"
	OpConversationsList    feeder.ReadOperation = "GET /api/conversations.list"
	OpConversationsInfo    feeder.ReadOperation = "GET /api/conversations.info"
	OpConversationsHistory feeder.ReadOperation = "GET /api/conversations.history"
)

// Surface is everything the reader app can issue. Built at init, so a write in it is a binary that
// does not start rather than a test someone forgets to run.
var Surface = feeder.MustReadOnlySurface(Platform, map[feeder.ReadOperation]feeder.ReadOperationSpec{
	OpAuthTest: {Area: "auth",
		Why: "which workspace the token belongs to, and the scopes Slack reports it holds, for the read-only gate (FR-002)"},
	OpConversationsList: {Area: "intake",
		Why: "the public channel list, which is where a severity channel is created (FR-005, FR-010)"},
	OpConversationsInfo: {Area: "intake",
		Why: "one channel's creation instant, creator and archive state, for a channel the doorbell or a reader names (FR-006)"},
	OpConversationsHistory: {Area: "chat",
		Why: "the incident channel's own messages, read in memory for the chat worker's digests; only when it is enabled (FR-030)"},
})

// Options configures the client.
type Options struct {
	// Token is the reader app's bot token (`xoxb-`). Never logged, never recorded.
	Token string
	// BaseURL overrides DefaultBaseURL, for tests against a recorded twin.
	BaseURL string
	// HTTP is the transport. Nil uses a client with a 30 s timeout.
	HTTP *http.Client
	// Now is the clock. Nil uses time.Now.
	Now func() time.Time
	// History enables conversations.history. Off, the operation is refused before it is issued, so
	// a deployment that has not turned the chat reader on cannot read a message even by mistake.
	History bool
}

// Client issues the published operations.
type Client struct {
	base    string
	token   string
	http    *http.Client
	now     func() time.Time
	history bool
}

// New returns a client, refusing an incomplete configuration.
func New(opts Options) (*Client, error) {
	token := strings.TrimSpace(opts.Token)
	switch {
	case token == "":
		return nil, errors.New("slackx: no token; the reader app's bot token is required")
	case strings.HasPrefix(token, "xapp-"):
		return nil, errors.New("slackx: an app-level token (xapp-) is the doorbell's credential and opens " +
			"a socket and nothing else; the reader needs its bot token (xoxb-)")
	case strings.HasPrefix(token, "xoxp-"):
		return nil, errors.New("slackx: a user token (xoxp-) acts as a person and reaches whatever they " +
			"can; the connector reads with a bot token (xoxb-) only (research R2)")
	}
	base := strings.TrimRight(opts.BaseURL, "/")
	if base == "" {
		base = DefaultBaseURL
	}
	client := opts.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Client{base: base, token: token, http: client, now: now, history: opts.History}, nil
}

// ErrHistoryDisabled is a history read on a client built without History.
var ErrHistoryDisabled = errors.New("slackx: reading channel history is off; it is enabled only with " +
	"the chat worker (FR-030)")

// APIError is Slack's `ok: false`, or a non-2xx status. Slack answers most errors with 200 and an
// error code, so the code is the field a caller maps to a coverage reason.
type APIError struct {
	Op         feeder.ReadOperation
	Status     int
	Code       string
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("slackx: %s answered %d %s (retry after %s)", e.Op, e.Status, e.Code, e.RetryAfter)
	}
	return fmt.Sprintf("slackx: %s answered %d %s", e.Op, e.Status, e.Code)
}

// RateLimited reports whether Slack refused the call for its rate limit.
func (e *APIError) RateLimited() bool {
	return e.Status == http.StatusTooManyRequests || e.Code == "ratelimited"
}

// Response is one answer: the body and the scopes Slack reported the token holds.
type Response struct {
	Body   []byte
	Scopes []string
	At     time.Time
}

// Do issues one published operation with the given query. It is the only path to the network.
func (c *Client) Do(ctx context.Context, op feeder.ReadOperation, query map[string]string) (*Response, error) {
	if _, err := Surface.Issuable(op); err != nil {
		return nil, err
	}
	if op == OpConversationsHistory && !c.history {
		return nil, ErrHistoryDisabled
	}
	_, path, _ := strings.Cut(string(op), " ")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, err
	}
	if len(query) > 0 {
		q := req.URL.Query()
		keys := make([]string, 0, len(query))
		for k := range query {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			q.Set(k, query[k])
		}
		req.URL.RawQuery = q.Encode()
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("slackx: %s: %w", op, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	out := &Response{Body: raw, Scopes: ParseScopes(resp.Header.Get("X-OAuth-Scopes")), At: c.now().UTC()}
	var envelope struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	decodeErr := json.Unmarshal(raw, &envelope)
	if resp.StatusCode < 200 || resp.StatusCode > 299 || decodeErr != nil || !envelope.OK {
		apiErr := &APIError{Op: op, Status: resp.StatusCode, Code: envelope.Error}
		if apiErr.Code == "" {
			apiErr.Code = "no_error_code"
		}
		if secs, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && secs > 0 {
			apiErr.RetryAfter = time.Duration(secs) * time.Second
		}
		return out, apiErr
	}
	return out, nil
}

// ParseScopes splits Slack's `X-OAuth-Scopes` header.
func ParseScopes(header string) []string {
	var out []string
	for _, s := range strings.Split(header, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// readScopeSuffixes are the scope families that grant no write. Slack names a scope
// `<resource>:<verb>`, and the reads are `:read` and `:history`; everything else — `:write`,
// `:join`, `:manage`, and any verb this list has never seen — fails closed.
var readScopeSuffixes = []string{":read", ":history"}

// ScopePermissions maps the scopes Slack reported onto the read-only gate's vocabulary, so
// pkg/feeder.CheckReadOnly rules on them: a `:read` or `:history` scope is "read", anything else is
// the scope's own verb and refuses.
func ScopePermissions(scopes []string) map[string]string {
	out := make(map[string]string, len(scopes))
	for _, scope := range scopes {
		value := "write"
		for _, suffix := range readScopeSuffixes {
			if strings.HasSuffix(scope, suffix) {
				value = "read"
			}
		}
		if value != "read" {
			if _, verb, ok := strings.Cut(scope, ":"); ok && verb != "" {
				value = verb
			}
		}
		out[scope] = value
	}
	return out
}

// Identity is what auth.test says about the token.
type Identity struct {
	TeamID string
	Team   string
	BotID  string
	Scopes []string
}

// AuthTest asks which workspace the token belongs to and what it may do.
func (c *Client) AuthTest(ctx context.Context) (Identity, error) {
	resp, err := c.Do(ctx, OpAuthTest, nil)
	if err != nil {
		return Identity{}, err
	}
	var body struct {
		TeamID string `json:"team_id"`
		Team   string `json:"team"`
		BotID  string `json:"bot_id"`
	}
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		return Identity{}, fmt.Errorf("slackx: auth.test: %w", err)
	}
	if body.TeamID == "" {
		return Identity{}, errors.New("slackx: auth.test named no workspace")
	}
	return Identity{TeamID: body.TeamID, Team: body.Team, BotID: body.BotID, Scopes: resp.Scopes}, nil
}

// CheckReadOnly runs the read-only gate on the scopes the token reported (FR-002). Slack reports
// them on every response, so the evidence is the platform's, never an operator's assertion.
func CheckReadOnly(id Identity) (feeder.CredentialEvidence, error) {
	return feeder.CheckReadOnly(feeder.CredentialReport{
		Platform:    Platform + " reader app",
		Evidence:    feeder.EvidencePlatformReported,
		Permissions: ScopePermissions(id.Scopes),
		Scope:       feeder.CredentialScope{PlatformEnforced: true, Selection: "workspace", Targets: []string{id.TeamID}},
	})
}
