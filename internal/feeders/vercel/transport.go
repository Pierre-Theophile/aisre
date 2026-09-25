// SPDX-License-Identifier: Apache-2.0

package vercel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The transport seam: the one place in this package that speaks HTTP to Vercel (004 T081, FR-031).
//
// ---------------------------------------------------------------------------------------------
// Why the seam is a set of interfaces
//
// Everything above this file works against the reader interfaces below, which is what makes live mode
// and recorded mode ONE code path rather than two implementations that agree until they do not. The
// recorder tees the decoded payloads these methods return; the replayer hands the same values back from
// disk. A caller holding one of these interfaces cannot tell which side it is on and has no HTTP client
// to reach around them with — the shape internal/feeders/gcp/transport.go established and
// internal/feeders/github/transport.go followed.
//
// # Why the payload types are narrow rather than Vercel's own
//
// Each type below carries only the fields a rollout is built from, and the omissions are the point.
// A Vercel deployment payload carries the creator's `username` and `githubLogin`, an `inspectorUrl`,
// build metadata and an arbitrary `meta` map the deployer populated. Decoding all of it would put a
// person's login one struct field away from something that writes to the graph, and doc.go's list of
// what this feeder does not read would be a promise kept by remembering rather than by construction.
//
// Two omissions are worth naming because they look like oversights:
//
//   - `creator.username` and `creator.githubLogin` are NOT decoded. FR-037 and research §3.2 say the
//     actor kind must come from `creator.type` and `source`, never from a login string, and the surest
//     way to honour that is for the login not to exist in this process. A future need for a display
//     name is a change to this struct and to the sanitisation policy, which is the review the rule
//     deserves.
//   - the environment variable's `value` is NOT decoded, nor `legacyValue`, nor
//     `internalContentHint.encryptedValue`; `decrypt=true` is never sent; and the operation that would
//     return a decrypted value is not on [Surface] (FR-038). Four independent refusals for the same
//     secret, which is not excessive for the one field in this connector that is a credential.
//
// # Every call goes through the metered door
//
// `get` is the only method here that performs a request, and its first act is `Issuer.Issue`: the
// surface gate, then the budget. An operation this connector does not publish is refused before a
// token is read, let alone spent.

// Deployment is one Vercel deployment, narrowed to what a rollout is built from.
type Deployment struct {
	// UID is the deployment's stable id, and the subject every claim hangs off.
	UID string `json:"uid"`
	// Name is the project's name as Vercel renders it in the deployment; the mapping to a service is
	// made from ProjectID, not from this.
	Name string `json:"name"`
	// ProjectID is what FR-034 maps to a service.
	ProjectID string `json:"projectId"`
	// Target is `production`, `staging`, or absent for a preview. A platform-stated value rather than
	// a heuristic, which is what makes FR-032's filter honest.
	Target string `json:"target"`
	// ReadyState is BLOCKED, BUILDING, CANCELED, DELETED, ERROR, INITIALIZING, QUEUED or READY.
	ReadyState string `json:"readyState"`
	// ReadySubstate is STAGED, ROLLING or PROMOTED once READY — and it, not ReadyState, is what says
	// the deployment is serving production traffic (research §3.1).
	ReadySubstate string `json:"readySubstate"`
	// Source is how the deployment was triggered: git, cli, redeploy, api-trigger-git-deploy,
	// git-deploy-hook, import, drop, clone/repo, import/repo, v0-web. It corroborates the actor kind.
	Source string `json:"source"`
	// InspectorURL is the origin reference (FR-014). It carries no credential.
	InspectorURL string `json:"inspectorUrl"`
	// IsRollbackCandidate says a deployment CAN be rolled back to, not that one happened. Decoded so
	// the mapper can record it as a property and so nobody re-derives it from something else; it is
	// deliberately not read as a rollback (research §5.2).
	IsRollbackCandidate bool `json:"isRollbackCandidate"`

	// Creator carries the actor evidence, and only the evidence.
	Creator DeploymentCreator `json:"creator"`

	// Meta is the Git provider's metadata, where the commit usually lives.
	Meta map[string]string `json:"meta"`
	// Attribution is the newer home for the same thing.
	Attribution DeploymentAttribution `json:"attribution"`

	// CreatedAt and Ready are Unix milliseconds, which is how Vercel states them.
	CreatedAt   int64 `json:"createdAt"`
	BuildingAt  int64 `json:"buildingAt"`
	ReadyMillis int64 `json:"ready"`
}

// DeploymentCreator is the actor evidence and nothing else: no username, no githubLogin.
// See the file comment for why those are absent rather than merely unused.
type DeploymentCreator struct {
	// UID identifies the account without naming a person.
	UID string `json:"uid"`
	// Type is the account type the platform states — the evidence FR-037 requires.
	Type string `json:"type"`
}

// DeploymentAttribution is `attribution.commitMeta`, the newer place a commit is stated.
type DeploymentAttribution struct {
	CommitMeta map[string]string `json:"commitMeta"`
}

// Project is a Vercel project, narrowed to the mapping FR-034 needs.
type Project struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// CreatedAt is when the project was created, in epoch milliseconds: the valid start of the
	// project node, where the platform states it (004 T093).
	CreatedAt int64 `json:"createdAt"`
	// Link is the connected Git repository, where Vercel reports one.
	Link ProjectLink `json:"link"`
	// LastAliasRequest is the last promotion or rollback of the project's production alias, as the
	// platform states it (004 T117). Only the single-project read carries it: the list endpoint's
	// published schema does not, which is why a rollback needs PayloadProject rather than the listing.
	LastAliasRequest *AliasRequest `json:"lastAliasRequest"`
}

// AliasRequest is Vercel's own record of moving a project's production alias to a deployment, published
// on `GET /v9/projects/{idOrName}` as `lastAliasRequest`.
//
// It is the ONE place either deploy platform states a rollback (research §5.4): `type` is `rollback` or
// `promote`, and `toDeploymentId` names the deployment production was moved to. That is the platform
// saying so, which is what FR-016 requires before a change may be flagged — as opposed to
// `isRollbackCandidate`, which says a deployment COULD be rolled back to.
type AliasRequest struct {
	// Type is `rollback` or `promote`.
	Type string `json:"type"`
	// JobStatus is `succeeded`, `failed`, `pending`, `in-progress` or `skipped`. Only a succeeded job
	// moved production.
	JobStatus string `json:"jobStatus"`
	// FromDeploymentID is the deployment production was moved away from, where stated.
	FromDeploymentID string `json:"fromDeploymentId"`
	// ToDeploymentID is the deployment production was moved to.
	ToDeploymentID string `json:"toDeploymentId"`
	// RequestedAt is when the move was requested, in epoch milliseconds.
	RequestedAt int64 `json:"requestedAt"`
}

// ProjectLink is the repository a project is connected to.
//
// It is carried as a PROPERTY of the change rather than as a claim or a correlation key. T148 settled
// why: a repository is neither. It does not NAME the deployment — several deployments ship from one
// repository — so it is not an identity claim; and it is not in the correlation registry either,
// because `acme/storefront` does name exactly one repository, and a feeder addressing a rollout's
// target by it mints an entity with a primary claim in that namespace, which a registry entry would
// then refuse. T087's task line asks for `github.repo` to be minted here; following it verbatim would
// have reintroduced the defect T148 removed.
type ProjectLink struct {
	Type   string `json:"type"`
	Org    string `json:"org"`
	Repo   string `json:"repo"`
	RepoID int64  `json:"repoId"`
}

// ProjectEnv is one environment-variable's METADATA. There is no value field, by construction.
//
// "By construction" is load-bearing here, because the platform sends the secret whether anybody wants
// it or not. The published response schema for `GET /v10/projects/{idOrName}/env` lists `value` among
// its REQUIRED fields, and two more carriers beside it: `legacyValue` ("Legacy now-encryption
// ciphertext") and `internalContentHint.encryptedValue` ("Contains the `value` of the env variable,
// encrypted with a special key"). So the bytes arrive on every read, and the only thing that keeps them
// out of this process is that this struct has nowhere to put any of the three.
//
// That is the same shape as the GitHub doorbell's body, and the same answer: a guarantee written into a
// type rather than remembered. Adding a field for any of them is a change a reviewer sees, and FR-038
// forbids the value "not plaintext, not ciphertext, not truncated, not hashed" — `legacyValue` and
// `encryptedValue` are ciphertext, so they are refused by the same clause that refuses the plaintext.
// `decrypt=true` is never sent, which is the third refusal (see ProjectEnv on [Client]).
type ProjectEnv struct {
	ID     string   `json:"id"`
	Key    string   `json:"key"`
	Target []string `json:"target"`
	Type   string   `json:"type"`
	// CreatedBy and UpdatedBy are the platform's user ids for whoever created and last edited the
	// variable, nullable in the schema. They are ids rather than logins, and nothing in the response
	// types them, so they name an actor and do NOT type one — see ConfigActorKind.
	CreatedBy string `json:"createdBy"`
	UpdatedBy string `json:"updatedBy"`
	// System marks a variable the PLATFORM assigns (`VERCEL_URL` and the rest) rather than one an
	// operator set. Read so that those can be excluded and counted rather than reported as somebody's
	// configuration change.
	System bool `json:"system"`
	// CreatedAt and UpdatedAt are Unix milliseconds.
	CreatedAt int64 `json:"createdAt"`
	UpdatedAt int64 `json:"updatedAt"`
}

// ListWindow is the time window a listing covers. Vercel paginates by timestamp rather than by cursor
// token, which suits a checkpointed extent (FR-057) — the next poll resumes at the last instant seen
// rather than at an opaque string whose meaning the platform owns.
type ListWindow struct {
	Since time.Time
	Until time.Time
	Limit int
}

// DeploymentReader reads rollouts.
type DeploymentReader interface {
	Deployments(ctx context.Context, projectID, target string, window ListWindow) ([]Deployment, error)
	Deployment(ctx context.Context, idOrURL string) (Deployment, error)
}

// ProjectReader reads the projects a deployment belongs to.
type ProjectReader interface {
	Projects(ctx context.Context, window ListWindow) ([]Project, error)
	Project(ctx context.Context, idOrName string) (Project, error)
}

// ConfigReader reads environment-variable metadata, never a value.
type ConfigReader interface {
	ProjectEnv(ctx context.Context, idOrName string) ([]ProjectEnv, error)
}

// Client is the live transport.
type Client struct {
	http   *http.Client
	base   *url.URL
	issuer *feeder.Issuer
	token  func(context.Context) (string, error)
	now    func() time.Time
	teamID string

	mu       sync.Mutex
	families map[feeder.ReadOperation]string
}

// ClientOptions configures the live transport.
type ClientOptions struct {
	HTTP    *http.Client
	BaseURL string
	Issuer  *feeder.Issuer
	// Token reads the credential at the moment of use rather than holding it: a rotated token is
	// picked up without a restart, and nothing here keeps one in a field a dump would show.
	Token func(context.Context) (string, error)
	Now   func() time.Time
	// TeamID scopes every call to one team where the credential is a personal one. Vercel takes it as
	// a query parameter rather than a path segment.
	TeamID string
}

// NewClient builds the live transport.
func NewClient(opts ClientOptions) (*Client, error) {
	if opts.Issuer == nil {
		return nil, fmt.Errorf("vercel: a transport needs an issuer; every call goes through the metered door")
	}
	base := strings.TrimSpace(opts.BaseURL)
	if base == "" {
		base = "https://api.vercel.com"
	}
	parsed, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("vercel: base url %q: %w", base, err)
	}
	client := opts.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	token := opts.Token
	if token == nil {
		token = func(context.Context) (string, error) { return "", nil }
	}
	return &Client{
		http: client, base: parsed, issuer: opts.Issuer, token: token, now: now,
		teamID: strings.TrimSpace(opts.TeamID), families: map[feeder.ReadOperation]string{},
	}, nil
}

// Deployments lists deployments, narrowed by project and target where the caller states them.
//
// The target filter is passed to the PLATFORM rather than applied after the fact. That matters for
// FR-032 and for the quota: a preview deployment excluded by the server is one this connector never
// paid for, and a count of exclusions taken here would be a count of what the platform chose to send.
// The mapper counts what it refuses from what does arrive (T085).
func (c *Client) Deployments(ctx context.Context, projectID, target string, window ListWindow) ([]Deployment, error) {
	q := url.Values{}
	if projectID != "" {
		q.Set("projectId", projectID)
	}
	if target != "" {
		q.Set("target", target)
	}
	c.applyWindow(q, window)
	var body struct {
		Deployments []Deployment `json:"deployments"`
	}
	if _, err := c.get(ctx, OpDeployments, c.url("/v7/deployments", q), &body); err != nil {
		return nil, err
	}
	return body.Deployments, nil
}

// Deployment reads one deployment, for when the list window no longer covers a referenced id.
func (c *Client) Deployment(ctx context.Context, idOrURL string) (Deployment, error) {
	if strings.TrimSpace(idOrURL) == "" {
		return Deployment{}, fmt.Errorf("vercel: a deployment needs an id")
	}
	var out Deployment
	path := "/v13/deployments/" + url.PathEscape(idOrURL)
	if _, err := c.get(ctx, OpDeployment, c.url(path, url.Values{}), &out); err != nil {
		return Deployment{}, err
	}
	return out, nil
}

// Projects lists the team's projects.
func (c *Client) Projects(ctx context.Context, window ListWindow) ([]Project, error) {
	q := url.Values{}
	c.applyWindow(q, window)
	var body struct {
		Projects []Project `json:"projects"`
	}
	if _, err := c.get(ctx, OpProjects, c.url("/v9/projects", q), &body); err != nil {
		return nil, err
	}
	return body.Projects, nil
}

// Project reads one project by id or name.
func (c *Client) Project(ctx context.Context, idOrName string) (Project, error) {
	if strings.TrimSpace(idOrName) == "" {
		return Project{}, fmt.Errorf("vercel: a project needs an id or a name")
	}
	var out Project
	path := "/v9/projects/" + url.PathEscape(idOrName)
	if _, err := c.get(ctx, OpProject, c.url(path, url.Values{}), &out); err != nil {
		return Project{}, err
	}
	return out, nil
}

// ProjectEnv reads environment-variable METADATA.
//
// `decrypt` is never sent, and the operation that would return a value is not on the surface (FR-038).
// The response's `value` field, if the platform sends one, is not decoded — ProjectEnv has no such
// field, so a plaintext secret has nowhere in this process to land.
func (c *Client) ProjectEnv(ctx context.Context, idOrName string) ([]ProjectEnv, error) {
	if strings.TrimSpace(idOrName) == "" {
		return nil, fmt.Errorf("vercel: environment metadata needs a project id or name")
	}
	var body struct {
		Envs []ProjectEnv `json:"envs"`
	}
	path := "/v9/projects/" + url.PathEscape(idOrName) + "/env"
	if _, err := c.get(ctx, OpProjectEnv, c.url(path, url.Values{}), &body); err != nil {
		return nil, err
	}
	return body.Envs, nil
}

// url builds an absolute URL against the base, carrying the team scope where one is configured.
func (c *Client) url(path string, q url.Values) *url.URL {
	out := *c.base
	out.Path = strings.TrimSuffix(out.Path, "/") + path
	if c.teamID != "" {
		q.Set("teamId", c.teamID)
	}
	out.RawQuery = q.Encode()
	return &out
}

// applyWindow renders the listing window as Vercel states it: Unix milliseconds, and a limit.
func (c *Client) applyWindow(q url.Values, window ListWindow) {
	if !window.Since.IsZero() {
		q.Set("since", strconv.FormatInt(window.Since.UnixMilli(), 10))
	}
	if !window.Until.IsZero() {
		q.Set("until", strconv.FormatInt(window.Until.UnixMilli(), 10))
	}
	if window.Limit > 0 {
		q.Set("limit", strconv.Itoa(window.Limit))
	}
}

// get issues one request. It is the only method in this package that performs one.
func (c *Client) get(ctx context.Context, op feeder.ReadOperation, target *url.URL, into any) (http.Header, error) {
	if err := c.issuer.Issue(ctx, op, c.familyOf(op), c.now()); err != nil {
		return nil, err
	}
	token, err := c.token(ctx)
	if err != nil {
		return nil, fmt.Errorf("vercel: reading the credential for %s: %w", op, err)
	}
	method, _, _ := strings.Cut(string(op), " ")
	req, err := http.NewRequestWithContext(ctx, method, target.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("vercel: building the request for %s: %w", op, err)
	}
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("vercel: %s: %w", op, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// The reading is taken before the status is judged: a 429 is exactly the response whose headers
	// matter most.
	var exhausted bool
	if reading, ok := feeder.ReadingFromHeaders(resp.Header); ok {
		c.learnFamily(op, reading.Family)
		c.issuer.Observe(reading)
		exhausted = reading.Remaining <= 0
	}

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return resp.Header, &StatusError{
			Operation: op, Status: resp.StatusCode,
			RetryAfter: retryAfter(resp.Header), Exhausted: exhausted,
		}
	}
	if into != nil {
		if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
			return resp.Header, fmt.Errorf("vercel: decoding %s: %w", op, err)
		}
	}
	return resp.Header, nil
}

// familyOf is the quota bucket last seen for an operation, or empty before the first response.
func (c *Client) familyOf(op feeder.ReadOperation) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.families[op]
}

func (c *Client) learnFamily(op feeder.ReadOperation, family string) {
	if family == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.families[op] = family
}

// StatusError is a non-200 from Vercel, carrying what the platform said about waiting.
type StatusError struct {
	Operation  feeder.ReadOperation
	Status     int
	RetryAfter time.Duration
	Exhausted  bool
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("vercel: %s: unexpected status %d", e.Operation, e.Status)
}

// retryAfter reads the platform's own wait instruction.
func retryAfter(h http.Header) time.Duration {
	if raw := strings.TrimSpace(h.Get("Retry-After")); raw != "" {
		if seconds, err := strconv.Atoi(raw); err == nil && seconds > 0 {
			return time.Duration(seconds) * time.Second
		}
	}
	return 0
}

// millis converts Vercel's Unix-millisecond instants, and reports whether one was stated at all.
//
// Zero is "not stated" rather than the epoch: a deployment that has not finished building states no
// `ready`, and mapping that to 1970 would date a change before the system existed. Every caller has to
// decide what an absent instant means, which is why this returns the second value rather than a zero
// time nobody checks.
func millis(v int64) (time.Time, bool) {
	if v <= 0 {
		return time.Time{}, false
	}
	return time.UnixMilli(v).UTC(), true
}
