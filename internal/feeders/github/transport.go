// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The transport seam: the one place in this package that speaks HTTP to GitHub (004 T047, FR-021).
//
// ---------------------------------------------------------------------------------------------
// Why the seam is a set of interfaces
//
// Everything above this file works against the reader interfaces below, which is what makes live mode
// and recorded mode **one code path** rather than two implementations that agree until they do not.
// The recorder tees the decoded payloads these methods return; the replayer hands the same values back
// from disk. A caller holding one of these interfaces cannot tell which side it is on and has no HTTP
// client to reach around them with — the shape internal/feeders/gcp/transport.go established, and for
// the same reason.
//
// # Why the payload types are narrow rather than GitHub's own
//
// The GCP feeder uses Google's decoded messages directly, because a private mirror would be a second
// schema to keep in step with the vendor's. Here the trade runs the other way and the narrow struct
// wins, for a reason that is about the graph rather than about convenience: a GitHub deployment
// payload carries a free-text `description`, an arbitrary JSON `payload` the deployer chose, and a
// `creator` object with a person's profile in it. Decoding the whole thing would put all of it one
// struct field away from something that writes to the graph, and doc.go's list of what this feeder
// does not read would be a promise kept by remembering rather than by construction.
//
// So each type below carries only the fields a rollout is built from, and the fields deliberately
// left out are named where they are left out. The recorded corpus is still GitHub's own bytes — the
// fixtures record what the API returned, and the narrowing happens here, in code a test can probe.
//
// # Every call goes through the door
//
// There is exactly one method that performs a request, and it cannot run without [feeder.Issuer]
// admitting the operation first. So FR-004's claim — write-incapability is verifiable from the set of
// operations the feeder can issue — is structural here: a new call site has to name an operation, the
// operation has to be on [Surface], and the surface refuses anything whose method is not a read.
//
// # Which quota family a call is metered against
//
// GitHub reports it: `x-ratelimit-resource` names the bucket the request counted against, and
// docs/connectors/github.md §5 is explicit that the connector learns the family from the response
// rather than keeping its own map from endpoint to family. A map would be this code's opinion about
// GitHub's billing, wrong silently and only under load.
//
// Which leaves the first call of a cycle, before any response has said anything. It is metered
// against [FamilyUnobserved] — not against a guessed `core`, because a guess that turns out wrong
// spends one family's allowance out of another's, and not against `unnamed`, which already means
// something else (the platform answered with numbers but no bucket name). A usage report showing more
// than the cycle's opening call under `unobserved` is a signal in its own right: the resource header
// stopped arriving.
//
// That first call therefore lands on [feeder.QuotaBudget]'s static fallback, and a connector
// configured with no fallback allowance cannot open its cycle at all. That is the intended failure and
// it is loud: the alternative is a budget whose first call is unbounded.

// FamilyUnobserved is the quota family a call is metered against before GitHub has said which bucket
// its requests come out of.
//
// Distinct from pkg/feeder's `unnamed`, which means the platform reported numbers without naming a
// bucket. Here there is no reading at all yet.
const FamilyUnobserved = "unobserved"

// Repo identifies one repository the way every path template does.
type Repo struct {
	Owner string
	Name  string
}

func (r Repo) String() string { return r.Owner + "/" + r.Name }

// Account is a GitHub actor as the platform types it.
//
// Type is GitHub's own field — `User`, `Bot`, `Organization` — and it is here because FR-027 requires
// actor kind to be derived from the platform's typing. Login is carried for the origin link and for
// the graph's actor identity, and **never** consulted to decide a kind: SC-005 has a fixture in which
// a person's login resembles a bot's and vice versa, and the login is exactly the field that gets it
// wrong (T058, T059).
type Account struct {
	Login string
	ID    int64
	Type  string
}

// Repository is one repository in the installation's grant. Nothing here becomes a node: FR-030 is
// explicit that the repository is a **claim** on a change, not an entity of its own (T057).
type Repository struct {
	ID       int64
	FullName string
	Owner    string
	Name     string
	Private  bool
}

// Repo returns the owner and name as a path pair.
func (r Repository) Repo() Repo { return Repo{Owner: r.Owner, Name: r.Name} }

// Scope is what the installation was granted (FR-008).
//
// Selection is GitHub's `repository_selection` — `all` or `selected` — and it is reported rather than
// interpreted, because it is the sentence the startup checkpoint has to be able to say: a repository
// outside the grant is not skipped by policy, it is unreachable.
type Scope struct {
	Selection    string
	Repositories []Repository
	// Total is GitHub's own `total_count`, kept so a short read is detectable rather than silent.
	Total int
}

// Deployment is a rollout request recorded by GitHub.
//
// Left out on purpose: `description` and `payload`, both free text or arbitrary JSON chosen by
// whoever created the deployment. Neither is evidence about a rollout and both are places people
// paste tokens (doc.go).
type Deployment struct {
	ID          int64
	SHA         string
	Ref         string
	Task        string
	Environment string
	// ProductionEnvironment and Transient are pointers because GitHub states them as tri-state in
	// practice: absent is not false. FR-023's allowlist is the operator's, and these are evidence
	// alongside it rather than a substitute for it (T061).
	ProductionEnvironment *bool
	Transient             *bool
	CreatedAt             time.Time
	UpdatedAt             time.Time
	Creator               Account
	// StatusesURL is GitHub's own link to the statuses collection; the statuses are read by id
	// through [Surface] rather than by following it, so that no call escapes the published set.
	StatusesURL string
	URL         string
}

// DeploymentStatus is one transition of a deployment.
//
// `description` is left out for the same reason as the deployment's: it is the deployer's free text.
// The URLs are kept because FR-014 and FR-029 need an origin link and a log pointer — as links, never
// as content this feeder fetches.
type DeploymentStatus struct {
	ID             int64
	State          string
	Environment    string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	Creator        Account
	LogURL         string
	EnvironmentURL string
	TargetURL      string
}

// WorkflowRun is one execution of a workflow.
//
// Event is the trigger — `push`, `workflow_dispatch`, `schedule`, `repository_dispatch` — and it is
// what FR-027's actor kind is derived from (T058). Actor and TriggeringActor are both carried because
// they differ on a re-run: the actor is who owns the run, the triggering actor is who asked for this
// attempt.
type WorkflowRun struct {
	ID           int64
	Name         string
	RunNumber    int
	RunAttempt   int
	HeadSHA      string
	Event        string
	Status       string
	Conclusion   string
	CreatedAt    time.Time
	RunStartedAt time.Time
	UpdatedAt    time.Time
	Actor        Account
	// TriggeringActor is who caused THIS attempt. On a re-run it is the person who pressed the
	// button, which is the actor FR-028's new change belongs to (T066).
	TriggeringActor Account
	HTMLURL         string
	// LogsURL is a link only. Job logs are never read (doc.go): a run's conclusion is a fact about a
	// rollout, its logs are the application's own output.
	LogsURL string
}

// Release is a tagged release.
//
// `body` — the release notes — is left out. It is the largest free-text field GitHub offers and a
// release note is not evidence about a rollout.
type Release struct {
	ID              int64
	TagName         string
	Name            string
	TargetCommitish string
	Draft           bool
	Prerelease      bool
	CreatedAt       time.Time
	PublishedAt     time.Time
	Author          Account
	HTMLURL         string
}

// ListWindow is the extent a list call covers.
//
// PerPage and MaxPages are both here rather than hidden in the client because a truncated window is a
// fact about the answer: [PartialListError] reports it, and the caller records a declared gap rather
// than a complete history (FR-056).
type ListWindow struct {
	// Since bounds the window where GitHub supports it. Deployments do not take a time filter, so it
	// is applied by the caller after decoding for those; the field is here so one shape describes
	// both.
	Since    time.Time
	PerPage  int
	MaxPages int
}

// ScopeReader enumerates the grant.
type ScopeReader interface {
	InstallationRepositories(ctx context.Context, window ListWindow) (Scope, error)
}

// DeploymentReader reads rollouts and how they ended.
type DeploymentReader interface {
	Deployments(ctx context.Context, repo Repo, environment string, window ListWindow) ([]Deployment, error)
	Deployment(ctx context.Context, repo Repo, id int64) (Deployment, error)
	DeploymentStatuses(ctx context.Context, repo Repo, deploymentID int64, window ListWindow) ([]DeploymentStatus, error)
}

// WorkflowReader reads the deploy pipeline.
type WorkflowReader interface {
	WorkflowRuns(ctx context.Context, repo Repo, window ListWindow) ([]WorkflowRun, error)
	WorkflowRun(ctx context.Context, repo Repo, id int64) (WorkflowRun, error)
}

// ReleaseReader reads tagged releases.
type ReleaseReader interface {
	Releases(ctx context.Context, repo Repo, window ListWindow) ([]Release, error)
	Release(ctx context.Context, repo Repo, id int64) (Release, error)
}

// QuotaReader takes the cycle's opening reading.
//
// It returns one reading per resource family rather than one for the family the request itself
// counted against, because that is what the endpoint answers and because a cycle that only learned
// about `core` would run blind against every other bucket.
type QuotaReader interface {
	RateLimit(ctx context.Context) ([]feeder.Reading, error)
}

// Reader is the whole seam. A mapper takes this; a replay satisfies it from disk.
type Reader interface {
	ScopeReader
	DeploymentReader
	WorkflowReader
	ReleaseReader
	QuotaReader
}

// TokenSource returns the credential for one request.
//
// A function rather than a string field, so that the token is fetched when it is used and is never a
// value sitting on a struct that something might format. FR-014 requires the origin link carry no
// credential, and the cheapest way to honour that is for there to be no field to accidentally print.
type TokenSource func(ctx context.Context) (string, error)

// Client is the live implementation of [Reader].
type Client struct {
	base   *url.URL
	http   *http.Client
	issuer *feeder.Issuer
	token  TokenSource
	now    func() time.Time
	// wait is how the client honours a rate-limit instruction (FR-074). Injectable so a test can
	// record the duration asked for and return immediately: a test that actually slept for the hour
	// GitHub's primary limit asks for would not be a test anybody runs.
	wait func(context.Context, time.Duration) error

	mu sync.Mutex
	// families is the last family GitHub said each operation counted against.
	families map[feeder.ReadOperation]string
}

// DefaultBaseURL is api.github.com. GitHub Enterprise Server puts the same API under a different
// host, so it is configuration rather than a constant in a path.
const DefaultBaseURL = "https://api.github.com"

// NewClient builds the live reader. The issuer is required: a client that could make a call without
// one would be a second door, and the whole point of the first is that there is only one.
func NewClient(base string, httpClient *http.Client, issuer *feeder.Issuer, token TokenSource,
	now func() time.Time, wait func(context.Context, time.Duration) error) (*Client, error) {
	if issuer == nil {
		return nil, fmt.Errorf("github: a transport with no issuer could spend a call without the " +
			"published surface admitting the operation, which is the one thing FR-004 rules out")
	}
	if token == nil {
		return nil, fmt.Errorf("github: a transport with no token source")
	}
	if strings.TrimSpace(base) == "" {
		base = DefaultBaseURL
	}
	parsed, err := url.Parse(strings.TrimRight(base, "/"))
	if err != nil {
		return nil, fmt.Errorf("github: base url %q: %w", base, err)
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	if wait == nil {
		wait = sleep
	}
	return &Client{
		base: parsed, http: httpClient, issuer: issuer, token: token, now: now, wait: wait,
		families: map[feeder.ReadOperation]string{},
	}, nil
}

var _ Reader = (*Client)(nil)

// sleep is the default wait: it honours the duration and the context, whichever ends first.
//
// A cancelled context returns its error rather than falling through, because a run being shut down
// must not go on to make the request the wait was protecting.
func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// MaxRateLimitWait is the longest a single paginated read will wait for a rate limit to clear before
// it gives up and reports a partial window instead.
//
// Ten minutes, and the number is a judgement about which failure is worse. GitHub's primary limit
// resets on the hour, so a read that exhausted it may be told to wait fifty-nine minutes — and a
// connector that obeyed would hold a cycle open for an hour, miss every poll in between, and report
// one enormous extent instead of a series of honest partial ones. Below the cap, waiting is right:
// the alternative is a partial window every cycle and a graph full of declared gaps for a limit that
// would have cleared in ninety seconds. Above it, stopping and declaring the gap is right, because
// FR-056's partial extent is a true statement and an hour-long silence inside a claimed extent is
// not.
const MaxRateLimitWait = 10 * time.Minute

// PartialListError reports a list that was not read to the end, and why.
//
// It is returned **alongside** the items read so far, which is deliberate: the pages already decoded
// are true, and a caller that threw them away would turn a declared gap into a smaller one that
// nobody declared. FR-056 wants the gap stated, and FR-073 wants the reason distinguishable from
// having looked and found nothing — so the cause is wrapped rather than described.
type PartialListError struct {
	Operation feeder.ReadOperation
	Pages     int
	Cause     error
	// ResumeFrom is the page the next attempt should start from, in GitHub's own cursor form, or
	// empty where there is nothing to resume (the very first page failed, so there is no cursor).
	//
	// It exists because FR-074's second clause is "MUST resume from where it stopped rather than
	// restarting the cycle", and before T037 this type made that impossible: it said HOW MANY pages
	// were read and not WHERE reading stopped, so the only thing a caller could do was start again
	// at page one — re-spending every call it had already spent, on a limit it had just exhausted.
	// The cursor was in scope at both exits and was dropped at both.
	//
	// A cursor rather than a page number, for the reason nextPage gives: on a collection that changes
	// while it is being read, a page number re-slices the collection and a cursor does not.
	ResumeFrom string
}

func (e *PartialListError) Error() string {
	resume := "there is no cursor to resume from, so the next attempt starts the list again"
	if e.ResumeFrom != "" {
		resume = "the next attempt resumes from the stated cursor rather than re-reading what it has"
	}
	return fmt.Sprintf("github: %s was read for %d page(s) and then stopped: %v; the window is "+
		"incomplete and the extent must record that rather than the items standing for the whole of "+
		"it, and %s", e.Operation, e.Pages, e.Cause, resume)
}

func (e *PartialListError) Unwrap() error { return e.Cause }

// ErrPageLimitReached is the cause when the configured page cap stopped a list, as opposed to the budget
// or the platform. A cap is the operator's choice and reaching it is not an error about GitHub.
var ErrPageLimitReached = errors.New("github: the configured page limit was reached")

// StatusError is a response GitHub refused. The body is deliberately not carried: an error body is
// still an arbitrary payload, and this connector does not put arbitrary vendor text where something
// might log it.
type StatusError struct {
	Operation feeder.ReadOperation
	Status    int
	// RetryAfter is the platform's own instruction where it gave one, so a caller waits at least as
	// long as it asked rather than guessing (FR-074).
	RetryAfter time.Duration
	// Exhausted is true where the response's own rate-limit headers said nothing was left. It is what
	// tells a 403 that is a spent allowance from a 403 that is a permission this credential does not
	// have — see RateLimited.
	Exhausted bool
}

func (e *StatusError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("github: %s answered %d and asked to be retried after %s",
			e.Operation, e.Status, e.RetryAfter)
	}
	return fmt.Sprintf("github: %s answered %d", e.Operation, e.Status)
}

// RateLimited reports whether a refusal was GitHub's rate limit rather than any other status, so a
// caller can honour it as a wait instead of a failure (FR-074, T037).
//
// 429 is unambiguous. 403 is not: GitHub uses it for the secondary rate limit AND for a permission
// this credential does not have, and those want opposite responses — one is waited out, the other is
// a scope the operator has to grant and no amount of waiting fixes. So a 403 counts as rate limiting
// only where the response itself says so, by asking to be retried or by reporting nothing left.
// Treating every 403 as a rate limit would make a missing permission look like a busy platform, and
// the cycle would retry forever against a door that is never going to open.
func (e *StatusError) RateLimited() bool {
	if e.Status == http.StatusTooManyRequests {
		return true
	}
	return e.Status == http.StatusForbidden && (e.Exhausted || e.RetryAfter > 0)
}

// familyOf is the quota family an operation is metered against: what GitHub last said, or
// [FamilyUnobserved] before it has said anything.
func (c *Client) familyOf(op feeder.ReadOperation) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if family, ok := c.families[op]; ok && family != "" {
		return family
	}
	return FamilyUnobserved
}

func (c *Client) learnFamily(op feeder.ReadOperation, family string) {
	if family == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.families[op] = family
}

// get issues one request. It is the only method in this package that performs one.
//
// `into` is decoded from the body; a nil `into` reads and discards, which is what a HEAD-shaped check
// would want. The returned header is the response's, for the `Link` pagination cursor — the body is
// closed before it returns, so nothing downstream holds a connection open.
func (c *Client) get(ctx context.Context, op feeder.ReadOperation, target *url.URL, into any) (http.Header, error) {
	if err := c.issuer.Issue(ctx, op, c.familyOf(op), c.now()); err != nil {
		return nil, err
	}
	token, err := c.token(ctx)
	if err != nil {
		return nil, fmt.Errorf("github: reading the credential for %s: %w", op, err)
	}
	method, _, _ := strings.Cut(string(op), " ")
	req, err := http.NewRequestWithContext(ctx, method, target.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("github: building the request for %s: %w", op, err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github: %s: %w", op, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// The reading is taken before the status is judged, because a 403 for a secondary rate limit is
	// exactly the response whose headers matter most.
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
			RetryAfter: retryAfter(resp.Header, c.now()), Exhausted: exhausted,
		}
	}
	if into != nil {
		if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
			return resp.Header, fmt.Errorf("github: decoding %s: %w", op, err)
		}
	}
	return resp.Header, nil
}

// retryAfter reads the platform's own wait instruction, in the two forms GitHub has (FR-074, T037).
//
// `Retry-After` is a count of seconds and is what a SECONDARY limit sends — abuse detection, a burst
// of concurrent requests. `X-RateLimit-Reset` is an absolute epoch second and is what the PRIMARY
// hourly limit sends, which is the common case by a wide margin.
//
// Until T037 this function read only the first and returned 0 for the second, while its own comment
// named both. So a connector that exhausted the ordinary hourly quota — the failure an operator
// actually meets — was told to wait zero, and FR-074's "waiting at least as long as the response
// asks" had nothing to ask. The header was parsed all along, one layer down, in
// `feeder.ReadingFromHeaders`; nothing carried it up.
//
// `now` is a parameter rather than `time.Now` because the reset is an instant and the wait is a
// duration, so the subtraction needs the same clock the rest of the client is tested against.
func retryAfter(h http.Header, now time.Time) time.Duration {
	if raw := strings.TrimSpace(h.Get("Retry-After")); raw != "" {
		if seconds, err := strconv.Atoi(raw); err == nil && seconds > 0 {
			return time.Duration(seconds) * time.Second
		}
	}
	// The primary limit. Taken through the same parser the budget reads, so the two cannot disagree
	// about what the platform said.
	if reading, ok := feeder.ReadingFromHeaders(h); ok && !reading.Reset.IsZero() {
		if d := reading.Reset.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

// nextPage reads the `Link` header's `rel="next"` cursor.
//
// GitHub's own cursor rather than an incremented `page` parameter: on a collection that changes while
// it is being read, a page number re-slices the collection and a cursor does not.
func nextPage(h http.Header) (*url.URL, bool) {
	for _, header := range h.Values("Link") {
		for _, part := range strings.Split(header, ",") {
			section := strings.Split(strings.TrimSpace(part), ";")
			if len(section) < 2 {
				continue
			}
			raw := strings.TrimSpace(section[0])
			if !strings.HasPrefix(raw, "<") || !strings.HasSuffix(raw, ">") {
				continue
			}
			for _, param := range section[1:] {
				if !strings.EqualFold(strings.TrimSpace(param), `rel="next"`) {
					continue
				}
				parsed, err := url.Parse(raw[1 : len(raw)-1])
				if err != nil {
					return nil, false
				}
				return parsed, true
			}
		}
	}
	return nil, false
}

// pageCap is the number of pages a list reads before reporting a partial window. Zero from the caller
// means this, rather than unbounded: a list with no cap is a cycle that can spend the whole budget on
// one repository's history.
const pageCap = 20

// perPage is GitHub's maximum, and the fewest requests for a given window.
const perPage = 100

func (w ListWindow) pages() int {
	if w.MaxPages > 0 {
		return w.MaxPages
	}
	return pageCap
}

func (w ListWindow) size() int {
	if w.PerPage > 0 && w.PerPage <= perPage {
		return w.PerPage
	}
	return perPage
}

// list walks a paginated collection, decoding each page into a fresh slice and appending.
//
// The generic is over the element type so that pagination, the page cap and the partial-window report
// exist once. Every page is a separate trip through the door, which is the point: a cycle that paged
// freely once admitted would meter one call and make twenty.
func list[T any](ctx context.Context, c *Client, op feeder.ReadOperation, target *url.URL, window ListWindow) ([]T, error) {
	var out []T
	limit := window.pages()
	// waited is the total spent on rate limits across this one list, so a collection that is limited
	// on page after page cannot wait MaxRateLimitWait each time and hold a cycle open all afternoon.
	var waited time.Duration
	for page := 1; ; {
		var batch []T
		header, err := c.get(ctx, op, target, &batch)
		if err != nil {
			// FR-074, both clauses. A rate-limited response is honoured by waiting at least as long as
			// the platform asked and then re-reading THIS page — the one that was refused — rather
			// than starting the list again. Restarting would re-spend every call already spent, which
			// on an exhausted limit is the one thing that cannot be afforded.
			var refused *StatusError
			if errors.As(err, &refused) && refused.RateLimited() {
				pause := refused.RetryAfter
				if pause <= 0 {
					// The platform refused for a limit and stated no instant. Nothing here invents a
					// number: with no duration to honour there is nothing to honour, so the window is
					// reported partial and the caller decides. Guessing a backoff would be this
					// connector choosing how hard to press somebody else's quota.
					return out, &PartialListError{
						Operation: op, Pages: page - 1, Cause: err, ResumeFrom: target.String(),
					}
				}
				if waited+pause > MaxRateLimitWait {
					return out, &PartialListError{
						Operation: op, Pages: page - 1, Cause: err, ResumeFrom: target.String(),
					}
				}
				if waitErr := c.wait(ctx, pause); waitErr != nil {
					return out, &PartialListError{
						Operation: op, Pages: page - 1, Cause: waitErr, ResumeFrom: target.String(),
					}
				}
				waited += pause
				// `page` is deliberately not advanced: the same page is read again, which is what
				// "resume from where it stopped" means at this granularity.
				continue
			}
			return out, &PartialListError{
				Operation: op, Pages: page - 1, Cause: err, ResumeFrom: target.String(),
			}
		}
		out = append(out, batch...)
		next, ok := nextPage(header)
		if !ok {
			return out, nil
		}
		if page >= limit {
			return out, &PartialListError{
				Operation: op, Pages: page, Cause: ErrPageLimitReached, ResumeFrom: next.String(),
			}
		}
		target = next
		page++
	}
}

func (c *Client) url(path string, query url.Values) *url.URL {
	target := c.base.JoinPath(path)
	if len(query) > 0 {
		target.RawQuery = query.Encode()
	}
	return target
}

func pathOf(op feeder.ReadOperation) string {
	_, path, _ := strings.Cut(string(op), " ")
	return path
}

// fill substitutes a path template's parameters. It exists so a call site names the published
// operation and the concrete path is derived from it, rather than the two being written out
// separately and drifting.
func fill(op feeder.ReadOperation, pairs ...string) string {
	path := pathOf(op)
	for i := 0; i+1 < len(pairs); i += 2 {
		path = strings.ReplaceAll(path, "{"+pairs[i]+"}", url.PathEscape(pairs[i+1]))
	}
	return path
}

// InstallationRepositories enumerates the grant (FR-008).
func (c *Client) InstallationRepositories(ctx context.Context, window ListWindow) (Scope, error) {
	type payload struct {
		TotalCount          int    `json:"total_count"`
		RepositorySelection string `json:"repository_selection"`
		Repositories        []struct {
			ID       int64  `json:"id"`
			Name     string `json:"name"`
			FullName string `json:"full_name"`
			Private  bool   `json:"private"`
			Owner    struct {
				Login string `json:"login"`
			} `json:"owner"`
		} `json:"repositories"`
	}

	scope := Scope{}
	target := c.url(pathOf(OpInstallationRepositories), url.Values{
		"per_page": {strconv.Itoa(window.size())},
	})
	limit := window.pages()
	for page := 1; ; page++ {
		var body payload
		header, err := c.get(ctx, OpInstallationRepositories, target, &body)
		if err != nil {
			return scope, &PartialListError{Operation: OpInstallationRepositories, Pages: page - 1, Cause: err}
		}
		// Selection and total come from every page and say the same thing; taking the first page's is
		// enough and taking the last page's would be a different answer if the grant changed mid-read.
		if page == 1 {
			scope.Selection = body.RepositorySelection
			scope.Total = body.TotalCount
		}
		for _, r := range body.Repositories {
			scope.Repositories = append(scope.Repositories, Repository{
				ID: r.ID, FullName: r.FullName, Owner: r.Owner.Login, Name: r.Name, Private: r.Private,
			})
		}
		next, ok := nextPage(header)
		if !ok {
			return scope, nil
		}
		if page >= limit {
			return scope, &PartialListError{
				Operation: OpInstallationRepositories, Pages: page, Cause: ErrPageLimitReached,
			}
		}
		target = next
	}
}

type deploymentPayload struct {
	ID                    int64          `json:"id"`
	SHA                   string         `json:"sha"`
	Ref                   string         `json:"ref"`
	Task                  string         `json:"task"`
	Environment           string         `json:"environment"`
	ProductionEnvironment *bool          `json:"production_environment"`
	TransientEnvironment  *bool          `json:"transient_environment"`
	CreatedAt             time.Time      `json:"created_at,omitzero"`
	UpdatedAt             time.Time      `json:"updated_at,omitzero"`
	Creator               accountPayload `json:"creator"`
	StatusesURL           string         `json:"statuses_url"`
	URL                   string         `json:"url"`
}

type accountPayload struct {
	Login string `json:"login"`
	ID    int64  `json:"id"`
	Type  string `json:"type"`
}

// account converts the wire shape to the domain one. The conversion is field-for-field on purpose:
// adding a field to the payload without deciding whether the graph should see it stops compiling, which
// is the review this feeder wants on an account object of all things.
func (a accountPayload) account() Account { return Account(a) }

func (d deploymentPayload) deployment() Deployment {
	return Deployment{
		ID: d.ID, SHA: d.SHA, Ref: d.Ref, Task: d.Task, Environment: d.Environment,
		ProductionEnvironment: d.ProductionEnvironment, Transient: d.TransientEnvironment,
		CreatedAt: d.CreatedAt.UTC(), UpdatedAt: d.UpdatedAt.UTC(),
		Creator: d.Creator.account(), StatusesURL: d.StatusesURL, URL: d.URL,
	}
}

// Deployments lists a repository's deployments, optionally for one environment.
//
// GitHub offers no time filter here, so the window's Since is the caller's to apply after decoding —
// stated in [ListWindow] rather than silently ignored.
func (c *Client) Deployments(ctx context.Context, repo Repo, environment string, window ListWindow) ([]Deployment, error) {
	query := url.Values{"per_page": {strconv.Itoa(window.size())}}
	if strings.TrimSpace(environment) != "" {
		query.Set("environment", environment)
	}
	target := c.url(fill(OpDeployments, "owner", repo.Owner, "repo", repo.Name), query)
	payloads, err := list[deploymentPayload](ctx, c, OpDeployments, target, window)
	out := make([]Deployment, 0, len(payloads))
	for _, p := range payloads {
		out = append(out, p.deployment())
	}
	return out, err
}

// Deployment reads one rollout by id, for a status naming a deployment the list window no longer
// covers.
func (c *Client) Deployment(ctx context.Context, repo Repo, id int64) (Deployment, error) {
	var body deploymentPayload
	target := c.url(fill(OpDeployment,
		"owner", repo.Owner, "repo", repo.Name, "deployment_id", strconv.FormatInt(id, 10)), nil)
	if _, err := c.get(ctx, OpDeployment, target, &body); err != nil {
		return Deployment{}, err
	}
	return body.deployment(), nil
}

// DeploymentStatuses lists one deployment's transitions, newest first as GitHub returns them.
//
// The order is preserved rather than sorted: FR-022 and Edge case 11 both turn on the platform's own
// stated ordering, and re-sorting by timestamp would silently break the tie between two transitions
// recorded in the same second.
func (c *Client) DeploymentStatuses(ctx context.Context, repo Repo, deploymentID int64, window ListWindow) ([]DeploymentStatus, error) {
	type payload struct {
		ID             int64          `json:"id"`
		State          string         `json:"state"`
		Environment    string         `json:"environment"`
		CreatedAt      time.Time      `json:"created_at,omitzero"`
		UpdatedAt      time.Time      `json:"updated_at,omitzero"`
		Creator        accountPayload `json:"creator"`
		LogURL         string         `json:"log_url"`
		EnvironmentURL string         `json:"environment_url"`
		TargetURL      string         `json:"target_url"`
	}
	target := c.url(fill(OpDeploymentStatuses,
		"owner", repo.Owner, "repo", repo.Name, "deployment_id", strconv.FormatInt(deploymentID, 10)),
		url.Values{"per_page": {strconv.Itoa(window.size())}})
	payloads, err := list[payload](ctx, c, OpDeploymentStatuses, target, window)
	out := make([]DeploymentStatus, 0, len(payloads))
	for _, p := range payloads {
		out = append(out, DeploymentStatus{
			ID: p.ID, State: p.State, Environment: p.Environment,
			CreatedAt: p.CreatedAt.UTC(), UpdatedAt: p.UpdatedAt.UTC(),
			Creator: p.Creator.account(),
			LogURL:  p.LogURL, EnvironmentURL: p.EnvironmentURL, TargetURL: p.TargetURL,
		})
	}
	return out, err
}

type workflowRunPayload struct {
	ID              int64          `json:"id"`
	Name            string         `json:"name"`
	RunNumber       int            `json:"run_number"`
	RunAttempt      int            `json:"run_attempt"`
	HeadSHA         string         `json:"head_sha"`
	Event           string         `json:"event"`
	Status          string         `json:"status"`
	Conclusion      string         `json:"conclusion"`
	CreatedAt       time.Time      `json:"created_at,omitzero"`
	RunStartedAt    time.Time      `json:"run_started_at,omitzero"`
	UpdatedAt       time.Time      `json:"updated_at,omitzero"`
	Actor           accountPayload `json:"actor"`
	TriggeringActor accountPayload `json:"triggering_actor"`
	HTMLURL         string         `json:"html_url"`
	LogsURL         string         `json:"logs_url"`
}

func (r workflowRunPayload) run() WorkflowRun {
	return WorkflowRun{
		ID: r.ID, Name: r.Name, RunNumber: r.RunNumber, RunAttempt: r.RunAttempt,
		HeadSHA: r.HeadSHA, Event: r.Event, Status: r.Status, Conclusion: r.Conclusion,
		CreatedAt: r.CreatedAt.UTC(), RunStartedAt: r.RunStartedAt.UTC(), UpdatedAt: r.UpdatedAt.UTC(),
		Actor: r.Actor.account(), TriggeringActor: r.TriggeringActor.account(),
		HTMLURL: r.HTMLURL, LogsURL: r.LogsURL,
	}
}

// WorkflowRuns lists a repository's runs, newest first, bounded by the window where GitHub supports
// it.
//
// `created` takes GitHub's own range syntax, and only a lower bound is sent: an upper bound would cut
// off a run that completed while the cycle was reading.
func (c *Client) WorkflowRuns(ctx context.Context, repo Repo, window ListWindow) ([]WorkflowRun, error) {
	type page struct {
		TotalCount int                  `json:"total_count"`
		Runs       []workflowRunPayload `json:"workflow_runs"`
	}
	query := url.Values{"per_page": {strconv.Itoa(window.size())}}
	if !window.Since.IsZero() {
		query.Set("created", ">="+window.Since.UTC().Format(time.RFC3339))
	}
	target := c.url(fill(OpWorkflowRuns, "owner", repo.Owner, "repo", repo.Name), query)

	var out []WorkflowRun
	limit := window.pages()
	for n := 1; ; n++ {
		var body page
		header, err := c.get(ctx, OpWorkflowRuns, target, &body)
		if err != nil {
			return out, &PartialListError{Operation: OpWorkflowRuns, Pages: n - 1, Cause: err}
		}
		for _, r := range body.Runs {
			out = append(out, r.run())
		}
		next, ok := nextPage(header)
		if !ok {
			return out, nil
		}
		if n >= limit {
			return out, &PartialListError{Operation: OpWorkflowRuns, Pages: n, Cause: ErrPageLimitReached}
		}
		target = next
	}
}

// WorkflowRun reads one run by id, to close an extent whose last page has left the list window.
func (c *Client) WorkflowRun(ctx context.Context, repo Repo, id int64) (WorkflowRun, error) {
	var body workflowRunPayload
	target := c.url(fill(OpWorkflowRun,
		"owner", repo.Owner, "repo", repo.Name, "run_id", strconv.FormatInt(id, 10)), nil)
	if _, err := c.get(ctx, OpWorkflowRun, target, &body); err != nil {
		return WorkflowRun{}, err
	}
	return body.run(), nil
}

type releasePayload struct {
	ID              int64          `json:"id"`
	TagName         string         `json:"tag_name"`
	Name            string         `json:"name"`
	TargetCommitish string         `json:"target_commitish"`
	Draft           bool           `json:"draft"`
	Prerelease      bool           `json:"prerelease"`
	CreatedAt       time.Time      `json:"created_at,omitzero"`
	PublishedAt     time.Time      `json:"published_at,omitzero"`
	Author          accountPayload `json:"author"`
	HTMLURL         string         `json:"html_url"`
}

func (r releasePayload) release() Release {
	return Release{
		ID: r.ID, TagName: r.TagName, Name: r.Name, TargetCommitish: r.TargetCommitish,
		Draft: r.Draft, Prerelease: r.Prerelease,
		CreatedAt: r.CreatedAt.UTC(), PublishedAt: r.PublishedAt.UTC(),
		Author: r.Author.account(), HTMLURL: r.HTMLURL,
	}
}

// Releases lists a repository's releases.
func (c *Client) Releases(ctx context.Context, repo Repo, window ListWindow) ([]Release, error) {
	target := c.url(fill(OpReleases, "owner", repo.Owner, "repo", repo.Name),
		url.Values{"per_page": {strconv.Itoa(window.size())}})
	payloads, err := list[releasePayload](ctx, c, OpReleases, target, window)
	out := make([]Release, 0, len(payloads))
	for _, p := range payloads {
		out = append(out, p.release())
	}
	return out, err
}

// Release reads one release by id, when a deployment references it.
func (c *Client) Release(ctx context.Context, repo Repo, id int64) (Release, error) {
	var body releasePayload
	target := c.url(fill(OpRelease,
		"owner", repo.Owner, "repo", repo.Name, "release_id", strconv.FormatInt(id, 10)), nil)
	if _, err := c.get(ctx, OpRelease, target, &body); err != nil {
		return Release{}, err
	}
	return body.release(), nil
}

// RateLimit takes the cycle's opening reading, one per resource family.
//
// The family names are GitHub's own map keys — `core`, `search`, `graphql` and the rest — so the
// budget's families and the `x-ratelimit-resource` header agree without this code translating between
// them.
func (c *Client) RateLimit(ctx context.Context) ([]feeder.Reading, error) {
	type resource struct {
		Limit     int   `json:"limit"`
		Remaining int   `json:"remaining"`
		Used      int   `json:"used"`
		Reset     int64 `json:"reset"`
	}
	var body struct {
		Resources map[string]resource `json:"resources"`
	}
	target := c.url(pathOf(OpRateLimit), nil)
	if _, err := c.get(ctx, OpRateLimit, target, &body); err != nil {
		return nil, err
	}
	// Sorted, because a JSON object's key order is not a thing and a usage report that reorders
	// between two runs of the same shape is a report nobody can diff.
	out := make([]feeder.Reading, 0, len(body.Resources))
	for _, family := range slices.Sorted(maps.Keys(body.Resources)) {
		r := body.Resources[family]
		reading := feeder.Reading{
			Family: family, Limit: r.Limit, Remaining: r.Remaining, Used: r.Used,
			Source: feeder.QuotaReported,
		}
		if r.Reset > 0 {
			reading.Reset = time.Unix(r.Reset, 0).UTC()
		}
		out = append(out, reading)
		c.issuer.Observe(reading)
	}
	return out, nil
}
