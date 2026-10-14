// SPDX-License-Identifier: Apache-2.0

// Package slacksink is the Slack report-delivery transport (006 US2, ADR-0011 item 5): it posts an
// investigation's report into the incident channel it was declared from, and edits that one message
// on every later delivery.
//
// It is the second render.Sink and the first that leaves the process, so everything the constitution
// confines about the write path (VII, ADR-0007) is made true here rather than assumed:
//
//   - **Three operations, listed.** Operations is the literal set of requests this package can make:
//     a preflight `auth.test`, `chat.postMessage` and `chat.update`. There is no join, no reaction, no
//     file upload, no delete. A test asserts the list.
//   - **A token that can do nothing else.** Before its first write the sink asks Slack which scopes
//     the token holds and refuses to post with anything beyond `chat:write` and `chat:write.public`.
//     A reporter token that could also read history, join channels or manage the workspace is a
//     misconfigured deployment, and it fails closed before a byte of the report leaves.
//   - **One place.** The target must be `slack:<team>/<channel>` on the workspace the credential was
//     minted for (render.Deliver checks the system; this checks the ref belongs to it), and the
//     workspace the token authenticates as must be that same workspace.
//   - **A report, not a page.** `@here`, `@channel`, `@everyone` and user-group mentions are
//     neutralised, Slack's control characters are escaped, links are not unfurled, and the body is
//     cut to Slack's limits with a line saying so.
//   - **Never an error.** A Slack failure is a DeliveryResult with Outcome "failed" and Slack's error
//     code, so a chat outage cannot touch an investigation.
package slacksink

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Pierre-Theophile/aisre/internal/investigation/render"
)

// SinkID is this transport's published id (render.SinkSlack).
const SinkID = render.SinkSlack

// DefaultBaseURL is the Web API's root.
const DefaultBaseURL = "https://slack.com"

// The operations this package can issue (contracts/slack-connector.md §2). Nothing else is
// compiled in.
const (
	OpAuthTest    = "GET /api/auth.test"
	OpPostMessage = "POST /api/chat.postMessage"
	OpUpdate      = "POST /api/chat.update"
)

// Operations is the published list, in the order a delivery uses them.
func Operations() []string { return []string{OpAuthTest, OpPostMessage, OpUpdate} }

// AllowedScopes is everything a reporter token may hold. `chat:write.public` lets it post in a
// public channel it has not joined, so the connector never joins one (research R2).
var AllowedScopes = []string{"chat:write", "chat:write.public"}

// Slack's limits, with margin: a section block's text is at most 3000 characters, a message at most
// 50 blocks, and the fallback `text` is what notifications and screen readers show.
const (
	sectionLimit  = 2900
	maxSections   = 45
	fallbackLimit = 2900
)

// Options configures the sink.
type Options struct {
	// BaseURL overrides DefaultBaseURL, for tests.
	BaseURL string
	// HTTP is the transport. Nil uses a client with a 30 s timeout.
	HTTP *http.Client
	// Now is the clock. Nil uses time.Now.
	Now func() time.Time
}

// Sink delivers reports to Slack.
type Sink struct {
	base string
	http *http.Client
	now  func() time.Time

	mu       sync.Mutex
	verified map[string]string // token fingerprint → team id it authenticated as
}

var _ render.Sink = (*Sink)(nil)

// New returns a sink.
func New(opts Options) *Sink {
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
	return &Sink{base: base, http: client, now: now, verified: map[string]string{}}
}

// MessageRef is the external reference of a delivered message: `<channel>/<ts>`, which is what
// chat.update needs to edit it.
func MessageRef(channel, ts string) string { return channel + "/" + ts }

// SplitTarget parses a delivery target: system `slack:<team>` and ref `slack:<team>/<channel>`.
func SplitTarget(system, ref string) (team, channel string, err error) {
	team, ok := strings.CutPrefix(system, "slack:")
	if !ok || team == "" || strings.Contains(team, "/") {
		return "", "", fmt.Errorf("slacksink: target system %q is not slack:<team>", system)
	}
	rest, ok := strings.CutPrefix(ref, system+"/")
	if !ok || rest == "" || strings.ContainsAny(rest, "/ ") {
		return "", "", fmt.Errorf("slacksink: target %q is not a channel of %s", ref, system)
	}
	return team, rest, nil
}

// Deliver posts or edits the report. It never returns an error (render.Sink).
func (s *Sink) Deliver(ctx context.Context, cred *render.Credential, req render.DeliveryRequest) render.DeliveryResult {
	at := req.At
	if at.IsZero() {
		at = s.now()
	}
	result := render.DeliveryResult{RenderingDigest: req.RenderingDigest(), At: at.UTC()}
	fail := func(outcome, detail string) render.DeliveryResult {
		result.Outcome = outcome
		result.FailureDetail = detail
		return result
	}

	team, channel, err := SplitTarget(req.TargetSystem, req.TargetRef)
	if err != nil {
		return fail(render.OutcomeNotConfigured, err.Error())
	}
	if cred == nil || cred.TargetSystem != req.TargetSystem {
		return fail(render.OutcomeNotConfigured, render.ErrCredentialTarget.Error())
	}
	token, err := cred.Secret(render.DeliveryScope)
	if err != nil {
		return fail(render.OutcomeFailed, err.Error())
	}
	if !strings.HasPrefix(token, "xoxb-") {
		return fail(render.OutcomeNotConfigured,
			"slacksink: the reporter credential must be the reporter app's bot token (xoxb-); a user or "+
				"app-level token is refused (ADR-0011 item 5)")
	}
	if err := s.verify(ctx, token, team); err != nil {
		return fail(render.OutcomeFailed, err.Error())
	}

	body := message(channel, req.Rendering)
	op := OpPostMessage
	if prevChannel, ts, ok := strings.Cut(req.PreviousMessageRef, "/"); ok && prevChannel == channel && ts != "" {
		op = OpUpdate
		body["ts"] = ts
	}
	resp, err := s.call(ctx, token, op, body)
	if op == OpUpdate && isMissingMessage(err) {
		// The message was deleted in Slack. Posting a new one is still one message per
		// (investigation, target): the ledger carries the new reference forward.
		delete(body, "ts")
		op = OpPostMessage
		resp, err = s.call(ctx, token, op, body)
	}
	if err != nil {
		return fail(render.OutcomeFailed, err.Error())
	}
	if resp.TS == "" {
		return fail(render.OutcomeFailed, "slacksink: "+op+" answered ok with no message ts")
	}
	result.Outcome = render.OutcomeDelivered
	result.ExternalMessageRef = MessageRef(channel, resp.TS)
	return result
}

// APIError is Slack's `ok: false`, or a non-2xx status.
type APIError struct {
	Op         string
	Status     int
	Code       string
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("slacksink: %s answered %d %s (retry after %s)", e.Op, e.Status, e.Code, e.RetryAfter)
	}
	return fmt.Sprintf("slacksink: %s answered %d %s", e.Op, e.Status, e.Code)
}

func isMissingMessage(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Code == "message_not_found"
}

type apiResponse struct {
	OK      bool   `json:"ok"`
	Error   string `json:"error"`
	TS      string `json:"ts"`
	Channel string `json:"channel"`
	TeamID  string `json:"team_id"`
	scopes  []string
}

// verify is the preflight: the token must authenticate as the target's workspace and hold nothing
// beyond AllowedScopes. It is asked once per token per process.
func (s *Sink) verify(ctx context.Context, token, team string) error {
	key := fingerprint(token)
	s.mu.Lock()
	known, ok := s.verified[key]
	s.mu.Unlock()
	if !ok {
		resp, err := s.call(ctx, token, OpAuthTest, nil)
		if err != nil {
			return err
		}
		var extra []string
		for _, scope := range resp.scopes {
			if !slices.Contains(AllowedScopes, scope) {
				extra = append(extra, scope)
			}
		}
		if len(resp.scopes) == 0 {
			return errors.New("slacksink: Slack reported no scopes for the reporter token, so it cannot be " +
				"shown to grant only chat:write; nothing was posted")
		}
		if len(extra) > 0 {
			return fmt.Errorf("slacksink: the reporter token also holds %s; it may hold only %s, and "+
				"nothing was posted (constitution VII, ADR-0011 item 5)",
				strings.Join(extra, ", "), strings.Join(AllowedScopes, ", "))
		}
		known = resp.TeamID
		s.mu.Lock()
		s.verified[key] = known
		s.mu.Unlock()
	}
	if known != team {
		return fmt.Errorf("slacksink: the reporter token belongs to workspace %q, not %q; nothing was posted",
			known, team)
	}
	return nil
}

func (s *Sink) call(ctx context.Context, token, op string, body map[string]any) (*apiResponse, error) {
	if !slices.Contains(Operations(), op) {
		return nil, fmt.Errorf("slacksink: %s is not a published operation", op)
	}
	method, path, _ := strings.Cut(op, " ")
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.base+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("slacksink: %s: %w", op, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var out apiResponse
	decodeErr := json.Unmarshal(raw, &out)
	out.scopes = parseScopes(resp.Header.Get("X-OAuth-Scopes"))
	if resp.StatusCode < 200 || resp.StatusCode > 299 || decodeErr != nil || !out.OK {
		apiErr := &APIError{Op: op, Status: resp.StatusCode, Code: out.Error}
		if apiErr.Code == "" {
			apiErr.Code = "no_error_code"
		}
		if secs, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && secs > 0 {
			apiErr.RetryAfter = time.Duration(secs) * time.Second
		}
		return nil, apiErr
	}
	return &out, nil
}

func parseScopes(header string) []string {
	var out []string
	for _, s := range strings.Split(header, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// fingerprint keys the verification cache without keeping the token as a map key in a heap dump.
func fingerprint(token string) string {
	if len(token) < 12 {
		return token
	}
	return token[:5] + "…" + token[len(token)-6:] + "/" + strconv.Itoa(len(token))
}

// message builds the chat.postMessage / chat.update body for one rendering.
func message(channel, rendering string) map[string]any {
	text := Neutralise(rendering)
	sections, truncated := chunk(text, sectionLimit, maxSections)
	blocks := make([]map[string]any, 0, len(sections)+1)
	for _, section := range sections {
		blocks = append(blocks, map[string]any{
			"type": "section",
			"text": map[string]any{"type": "mrkdwn", "text": section},
		})
	}
	if truncated {
		blocks = append(blocks, map[string]any{
			"type": "context",
			"elements": []map[string]any{{"type": "mrkdwn",
				"text": "The report is longer than a Slack message allows and was cut here; " +
					"`aisre investigate report` shows all of it."}},
		})
	}
	fallback, _ := chunk(text, fallbackLimit, 1)
	first := ""
	if len(fallback) > 0 {
		first = fallback[0]
	}
	return map[string]any{
		"channel":      channel,
		"text":         first,
		"blocks":       blocks,
		"unfurl_links": false,
		"unfurl_media": false,
		"parse":        "none",
		"link_names":   false,
	}
}

// broadcasts are the words Slack turns into a notification for a whole channel or workspace.
var broadcasts = []string{"@here", "@channel", "@everyone"}

// Neutralise makes a rendering safe to post: Slack's control characters are escaped, so no `<!here>`,
// `<!subteam^…>` or `<@U…>` in the report can become a mention, and the plain broadcast words are
// broken with a zero-width joiner so a client that re-parses them does not page anyone.
func Neutralise(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	for _, word := range broadcasts {
		s = replaceFold(s, word, "@\u2060"+word[1:])
	}
	return s
}

func replaceFold(s, old, repl string) string {
	lower := strings.ToLower(s)
	var b strings.Builder
	for {
		i := strings.Index(lower, old)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		b.WriteString(repl)
		s, lower = s[i+len(old):], lower[i+len(old):]
	}
}

// chunk splits text into at most max pieces of at most limit runes, on line boundaries where it can.
func chunk(text string, limit, max int) ([]string, bool) {
	var out []string
	for text != "" {
		if len(out) == max {
			return out, true
		}
		if utf8.RuneCountInString(text) <= limit {
			out = append(out, text)
			break
		}
		cut := byteIndexOfRune(text, limit)
		if nl := strings.LastIndex(text[:cut], "\n"); nl > 0 {
			cut = nl + 1
		}
		out = append(out, strings.TrimRight(text[:cut], "\n"))
		text = text[cut:]
	}
	return out, false
}

func byteIndexOfRune(s string, n int) int {
	i := 0
	for pos := range s {
		if i == n {
			return pos
		}
		i++
	}
	return len(s)
}
