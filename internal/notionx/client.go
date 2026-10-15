// SPDX-License-Identifier: Apache-2.0

// Package notionx is the read-only Notion API client the Notion knowledge connector uses (007
// FR-001–FR-003, research R4).
//
// It is hand-written over net/http rather than an SDK, for the reason internal/slackx gives: the
// connector reads with five operations, and the published read-only list is only a proof while it is
// the literal set of requests this binary can make. A Notion SDK carries every write the API has.
// Every request goes through Do, which asks the issuer (pkg/feeder.Issuer) before anything leaves the
// process, so an operation the surface does not carry is refused and recorded as an attempt.
//
// Notion reports neither an integration's capabilities nor what is left of its rate limit. The first
// is why the credential evidence is the operator's assertion (ADR-0013 item 4); the second is why the
// budget is a static allowance (budget.go).
package notionx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// Platform is the connector's name in refusals and on the published operation page.
const Platform = "notion"

// DefaultBaseURL is the API's root.
const DefaultBaseURL = "https://api.notion.com"

// APIVersion is the `Notion-Version` every request carries. 2025-09-03 is the version in which a
// database is a container of data sources and queries run per data source (research R5).
const APIVersion = "2025-09-03"

// The published read operations (contracts/notion-connector.md §2).
const (
	OpUsersMe         feeder.ReadOperation = "GET /v1/users/me"
	OpDatabase        feeder.ReadOperation = "GET /v1/databases/{id}"
	OpQueryDataSource feeder.ReadOperation = "POST /v1/data_sources/{id}/query"
	OpPage            feeder.ReadOperation = "GET /v1/pages/{id}"
	OpPageProperty    feeder.ReadOperation = "GET /v1/pages/{id}/properties/{property_id}"
	OpBlockChildren   feeder.ReadOperation = "GET /v1/blocks/{id}/children"
)

// Surface is everything the connector can issue. Built at init, so a write in it is a binary that
// does not start rather than a test someone forgets to run.
var Surface = feeder.MustReadOnlySurface(Platform, map[feeder.ReadOperation]feeder.ReadOperationSpec{
	OpUsersMe: {Area: "auth",
		Why: "the integration's own bot user, whose workspace name is checked against the configured label (FR-003)"},
	OpDatabase: {Area: "binding",
		Why: "a configured database's title and data sources, to resolve the binding at start (FR-010, research R5)"},
	OpQueryDataSource: {Area: "pages", NamedQuery: true,
		Why: "named query: Notion documents it as returning the pages of a data source that match a filter " +
			"and sort; its body carries only filter, sorts, start_cursor and page_size, and no field creates, " +
			"updates or deletes anything (FR-040)"},
	OpPage: {Area: "pages",
		Why: "one registered page no longer returned by its data source, read once to tell trashed or moved from gone (FR-043)"},
	OpPageProperty: {Area: "pages",
		Why: "the rest of a named summary or entity property cut at 25 references in a query result (research R7)"},
	OpBlockChildren: {Area: "bodies",
		Why: "the body of a registered page whose last_edited_time moved, 100 blocks per call, to depth 3 and at most 1,000 blocks (008 FR-010, research R6)"},
})

// Secret is the integration secret. It never prints: a log line, an error or a %v of a struct that
// holds one shows the redaction instead.
type Secret struct{ value string }

// NewSecret validates an internal integration secret: `ntn_`, or the legacy `secret_` prefix.
func NewSecret(raw string) (Secret, error) {
	raw = strings.TrimSpace(raw)
	switch {
	case raw == "":
		return Secret{}, errors.New("notionx: no integration secret")
	case strings.HasPrefix(raw, "ntn_"), strings.HasPrefix(raw, "secret_"):
		return Secret{value: raw}, nil
	default:
		return Secret{}, errors.New("notionx: the secret is not an internal integration secret (ntn_ or secret_); " +
			"a public integration's OAuth token acts for whoever authorised it, and the connector reads with an " +
			"internal integration only")
	}
}

func (s Secret) String() string   { return "notion-secret(redacted)" }
func (s Secret) GoString() string { return s.String() }

// Options configures the client.
type Options struct {
	Secret Secret
	// BaseURL overrides DefaultBaseURL, for tests against a recorded twin.
	BaseURL string
	// HTTP is the transport. Nil uses a client with a 30 s timeout.
	HTTP *http.Client
	// Budget paces the calls (FR-044). Nil is an unmetered client: the surface gate still runs.
	Budget *Budget
	// Now is the clock. Nil uses time.Now.
	Now func() time.Time
}

// Client issues the published operations.
type Client struct {
	base   string
	secret Secret
	http   *http.Client
	budget *Budget
	issuer *feeder.Issuer
	now    func() time.Time
}

// New returns a client.
func New(opts Options) (*Client, error) {
	if opts.Secret.value == "" {
		return nil, errors.New("notionx: no integration secret")
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
	budget := opts.Budget
	if budget == nil {
		budget = unmetered()
	}
	return &Client{base: base, secret: opts.Secret, http: client, budget: budget, issuer: budget.issuer, now: now}, nil
}

// APIError is a non-2xx answer. Notion names the failure in `code`; a 429 carries Retry-After.
type APIError struct {
	Op         feeder.ReadOperation
	Status     int
	Code       string
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("notionx: %s answered %d %s (retry after %s)", e.Op, e.Status, e.Code, e.RetryAfter)
	}
	return fmt.Sprintf("notionx: %s answered %d %s", e.Op, e.Status, e.Code)
}

// NotFound reports a page or database the integration cannot see: deleted, or no longer shared.
func (e *APIError) NotFound() bool {
	return e.Status == http.StatusNotFound || e.Code == "object_not_found"
}

// RateLimited reports Notion's 429.
func (e *APIError) RateLimited() bool {
	return e.Status == http.StatusTooManyRequests || e.Code == "rate_limited"
}

// Do issues one published operation. ids fill the `{…}` segments of the operation's path in order;
// body is the JSON body of a named query and nil for every GET. It is the only path to the network.
func (c *Client) Do(ctx context.Context, op feeder.ReadOperation, ids []string, query url.Values, body any) ([]byte, error) {
	if err := c.budget.allow(ctx, op); err != nil {
		return nil, err
	}
	method, pattern, _ := strings.Cut(string(op), " ")
	path, err := fill(pattern, ids)
	if err != nil {
		return nil, err
	}
	var reader io.Reader
	if body != nil {
		if method != http.MethodPost {
			return nil, fmt.Errorf("notionx: %s takes no body", op)
		}
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}
	target := c.base + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.secret.value)
	req.Header.Set("Notion-Version", APIVersion)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("notionx: %s: %w", op, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		return raw, nil
	}
	apiErr := &APIError{Op: op, Status: resp.StatusCode}
	var envelope struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(raw, &envelope) == nil {
		apiErr.Code = envelope.Code
	}
	if apiErr.Code == "" {
		apiErr.Code = "no_error_code"
	}
	if secs, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && secs > 0 {
		apiErr.RetryAfter = time.Duration(secs) * time.Second
	}
	if apiErr.RateLimited() {
		wait := apiErr.RetryAfter
		if wait <= 0 {
			wait = time.Second
		}
		c.budget.closeUntil(c.now().Add(wait))
	}
	return nil, apiErr
}

// fill substitutes ids into a path pattern, refusing an id that would change the path's shape.
func fill(pattern string, ids []string) (string, error) {
	out := pattern
	for _, id := range ids {
		start := strings.Index(out, "{")
		end := strings.Index(out, "}")
		if start < 0 || end < start {
			return "", fmt.Errorf("notionx: %s takes %d id(s), got %d", pattern, strings.Count(pattern, "{"), len(ids))
		}
		if id == "" || strings.ContainsAny(id, "/?#%") {
			return "", fmt.Errorf("notionx: id %q is not a Notion id", id)
		}
		out = out[:start] + url.PathEscape(id) + out[end+1:]
	}
	if strings.Contains(out, "{") {
		return "", fmt.Errorf("notionx: %s takes %d id(s), got %d", pattern, strings.Count(pattern, "{"), len(ids))
	}
	return out, nil
}

// Issuer is the client's door, for the request log a run reports (SC-007).
func (c *Client) Issuer() *feeder.Issuer { return c.issuer }

// CapabilitiesAcknowledgement is what a signed configuration's `acknowledge_read_only` must carry:
// the operator's statement that the integration was created with Read content only (contracts §3).
const CapabilitiesAcknowledgement = "notion.capabilities"

// ErrNotAcknowledged is a configuration that does not acknowledge the integration's capabilities.
var ErrNotAcknowledged = errors.New("notionx: the configuration does not acknowledge " + CapabilitiesAcknowledgement +
	"; Notion does not report an integration's capabilities, so the operator states that it was created with " +
	"Read content only — no Update content, Insert content, comments or user information — by listing " +
	CapabilitiesAcknowledgement + " under acknowledge_read_only (ADR-0013 item 4)")

// CheckReadOnly is the read-only gate for a platform that reports no capabilities (ADR-0013 item 4):
// the evidence is the operator's assertion, accepted only when the signed configuration acknowledges
// `notion.capabilities`, and recorded as exactly what it is.
func CheckReadOnly(workspace string, acknowledged []string) (feeder.CredentialEvidence, error) {
	ok := false
	for _, a := range acknowledged {
		if strings.TrimSpace(a) == CapabilitiesAcknowledgement {
			ok = true
		}
	}
	if !ok {
		return feeder.EvidenceUnassigned, ErrNotAcknowledged
	}
	return feeder.CheckReadOnly(feeder.CredentialReport{
		Platform: Platform + " integration",
		Evidence: feeder.EvidenceOperatorAsserted,
		Scope: feeder.CredentialScope{
			PlatformEnforced: true, Selection: "pages and databases shared with the integration",
			Targets: []string{workspace},
		},
	})
}
