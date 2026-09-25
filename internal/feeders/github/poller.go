// SPDX-License-Identifier: Apache-2.0

package github

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The live poll cycle (004 T157; FR-006, FR-044, FR-056, FR-073).
//
// ---------------------------------------------------------------------------------------------
// What this is
//
// The feeder reads payloads and never calls GitHub (feeder.go). Poller is the other half: a
// [feeder.Source] that reads GitHub through the [Reader] and yields the SAME payload kinds, in the same
// shapes, that a recording holds. So a live run and a replay are one code path from the first payload
// on, and a recording campaign is `feed github --record` over this source rather than a second reader
// that could drift from it.
//
// # One cycle
//
// The grant, then per repository (in name order): the workflow runs of the window, the deployments of
// each production environment the operator named, each deployment's statuses, and the releases of the
// window; then a poll marker. The marker is what releases the held deployments and writes the
// checkpoint, and it states whether the cycle read everything it meant to.
//
// # What is re-read, and why
//
// GitHub's deployment list takes no time filter and a deployment is not a fact until a status says it
// finished. So a cycle reads the deployments created or updated since the last complete cycle, less
// one reordering window of overlap, and ALSO every deployment an earlier cycle saw without a terminal
// status: a rollout that was `in_progress` at 14:05 and succeeded at 14:07 is found by the next cycle
// whether or not GitHub bumped its `updated_at`. A deployment is followed for PendingHorizon and then
// let go; the feeder has already counted it deferred, and a deployment that never finishes is not one
// this connector should poll forever.
//
// Re-reading is safe because every event id is derived from what it describes: a rollout emitted twice
// is one fact the graph has seen twice (DUPLICATE_NOOP), never two rollouts.
//
// # A partial cycle does not advance the window
//
// Any read that failed or was truncated makes the cycle `partial`, with the reasons joined in the
// marker, and the next cycle starts from the SAME instant this one did. The checkpoint declares the gap
// (FR-056), and the re-read closes it: a transient 502 costs one cycle's latency rather than a hole in
// the history nobody goes back for.
//
// # What a payload holds
//
// Only the fields the transport decodes: the payloads are re-encoded from the domain types, never
// passed through from GitHub. So a deployment's `description` and `payload`, a status's `description`
// and a release's `body` — free text, and exactly where people paste tokens (doc.go) — never enter a
// payload, and therefore never enter a recording, even before the sanitiser runs.

// PendingHorizon is how long a deployment without a terminal status is re-read before the poller lets
// it go.
const PendingHorizon = 24 * time.Hour

// DefaultLookback is how far back the first cycle of a run reads.
//
// It is the reordering window's worth of overlap plus one poll interval: enough that a restart does not
// miss the cycle that was in flight, and small enough that a first run on a large estate does not spend
// its quota re-reading history the graph already holds. A backfill is an explicit, wider --lookback.
const DefaultLookback = DefaultPollInterval + DefaultReorderingWindow

// PollerOptions configures the live source.
type PollerOptions struct {
	// Reader is the transport. Required.
	Reader Reader
	// Environments are the deployment environments listed per repository — the operator's production
	// allowlist (FR-023). Required: listing every environment would read previews the mapper then
	// drops, and there is no default to fall back on.
	Environments []string
	// Interval is the time between cycle starts. Zero uses DefaultPollInterval.
	Interval time.Duration
	// Lookback is how far back the first cycle reads. Zero uses DefaultLookback.
	Lookback time.Duration
	// Overlap is how far each cycle re-reads behind the previous one's start. Zero uses
	// DefaultReorderingWindow.
	Overlap time.Duration
	// Window bounds each list call (page size and page cap). Zero values use the transport's defaults.
	Window ListWindow
	// Once stops after one cycle, which is what a scheduled job or a recording of one cycle wants.
	Once bool
	// Now and Wait are the clock and the sleep between cycles. Injectable for tests.
	Now  func() time.Time
	Wait func(context.Context, time.Duration) error
	// Logger is optional.
	Logger *slog.Logger
}

// Poller is the live [feeder.Source].
type Poller struct {
	opts PollerOptions
	log  *slog.Logger

	queue  []feeder.Payload
	cycles int
	// since is where the next cycle's window starts: the previous complete cycle's start less the
	// overlap. Zero before the first cycle.
	since time.Time
	// pending are deployments seen without a terminal status, re-read until they finish or age out.
	pending map[heldKey]pendingDeployment
}

type pendingDeployment struct {
	repo      Repository
	createdAt time.Time
}

// NewPoller validates the options.
func NewPoller(opts PollerOptions) (*Poller, error) {
	if opts.Reader == nil {
		return nil, fmt.Errorf("github: a poller with no reader")
	}
	envs := make([]string, 0, len(opts.Environments))
	for _, env := range opts.Environments {
		if env = strings.TrimSpace(env); env != "" {
			envs = append(envs, env)
		}
	}
	if len(envs) == 0 {
		return nil, fmt.Errorf("github: a poller with no production environment would read nothing " +
			"and report a quiet window; which environments count is operator configuration (FR-023)")
	}
	slices.Sort(envs)
	opts.Environments = slices.Compact(envs)
	for name, d := range map[string]time.Duration{
		"interval": opts.Interval, "lookback": opts.Lookback, "overlap": opts.Overlap,
	} {
		if d < 0 {
			return nil, fmt.Errorf("github: a negative poll %s (%s)", name, d)
		}
	}
	if opts.Interval == 0 {
		opts.Interval = DefaultPollInterval
	}
	if opts.Lookback == 0 {
		opts.Lookback = DefaultLookback
	}
	if opts.Overlap == 0 {
		opts.Overlap = DefaultReorderingWindow
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	if opts.Wait == nil {
		opts.Wait = sleep
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Poller{opts: opts, log: log, pending: map[heldKey]pendingDeployment{}}, nil
}

var _ feeder.Source = (*Poller)(nil)

// ErrGateRefused marks a credential that stopped being read-only mid-run. It ends the run rather than
// making a cycle partial: FR-003 is not a property that may lapse for one cycle.
var ErrGateRefused = errors.New("github: the read-only gate refused a renewed credential")

// Next returns the next payload, running a cycle when the last one has been consumed.
func (p *Poller) Next(ctx context.Context) (feeder.Payload, error) {
	for len(p.queue) == 0 {
		if p.opts.Once && p.cycles > 0 {
			return feeder.Payload{}, io.EOF
		}
		if p.cycles > 0 {
			if err := p.opts.Wait(ctx, p.opts.Interval); err != nil {
				return feeder.Payload{}, err
			}
		}
		if err := ctx.Err(); err != nil {
			return feeder.Payload{}, err
		}
		if err := p.cycle(ctx); err != nil {
			return feeder.Payload{}, err
		}
	}
	next := p.queue[0]
	p.queue = p.queue[1:]
	return next, nil
}

// cycle reads one window into the queue.
func (p *Poller) cycle(ctx context.Context) error {
	p.cycles++
	start := p.opts.Now().UTC()
	if p.since.IsZero() {
		// Pinned now, not after the cycle: a first cycle that turns out partial must hold THIS window
		// for the next one, and a start derived afresh from the next cycle's clock would slide past it.
		p.since = start.Add(-p.opts.Lookback)
	}
	since := p.since
	c := &cycleState{poller: p, ctx: ctx, at: start, since: since}

	// The cycle's opening reading: what is left in each quota family, which the budget's share is a
	// share of. Free against the primary limit, and published as once a cycle (requestlog.go). A
	// failed reading is not a failed cycle — the budget falls back to its static allowance — but it is
	// said, because a cycle paced blind is worth knowing about.
	if _, err := p.opts.Reader.RateLimit(ctx); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if errors.Is(err, ErrGateRefused) {
			return err
		}
		p.log.WarnContext(ctx, "github rate-limit reading failed; the cycle is paced on the static allowance",
			"error", err)
	}

	scope, err := p.opts.Reader.InstallationRepositories(ctx, p.opts.Window)
	if err := c.failed("the installation's repositories", err); err != nil {
		return err
	}
	if err := c.push(PayloadInstallationRepositories, scopePayload(scope)); err != nil {
		return err
	}
	repos := slices.Clone(scope.Repositories)
	slices.SortFunc(repos, func(a, b Repository) int { return cmp.Compare(a.FullName, b.FullName) })
	for _, repo := range repos {
		if err := c.repository(repo); err != nil {
			return err
		}
	}

	marker := pollMarkerPayload{Outcome: "complete"}
	if len(c.reasons) > 0 {
		marker = pollMarkerPayload{Outcome: "partial", Reason: strings.Join(c.reasons, "; ")}
	} else {
		p.since = start.Add(-p.opts.Overlap)
	}
	raw, err := json.Marshal(marker)
	if err != nil {
		return err
	}
	// The marker is stamped when the cycle ENDS: it is what releases the rollouts, and a rollout is
	// known to this connector from the moment its statuses were read, not from when the cycle began.
	p.queue = append(p.queue, feeder.Payload{Kind: PayloadPollMarker, At: p.opts.Now().UTC(), Bytes: raw})
	p.log.InfoContext(ctx, "github poll cycle read", "cycle", p.cycles, "since", since,
		"repositories", len(repos), "outcome", marker.Outcome, "pending", len(p.pending))
	return nil
}

// cycleState is one cycle's reads.
type cycleState struct {
	poller  *Poller
	ctx     context.Context
	at      time.Time
	since   time.Time
	reasons []string
}

// failed records a failed or truncated read as a reason the cycle is partial. It returns an error only
// for what must end the run: a cancelled context, or a credential that is no longer read-only.
func (c *cycleState) failed(what string, err error) error {
	if err == nil {
		return nil
	}
	if ctxErr := c.ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(err, ErrGateRefused) {
		return err
	}
	c.reasons = append(c.reasons, fmt.Sprintf("%s: %v", what, err))
	c.poller.log.WarnContext(c.ctx, "github read failed; the cycle is partial", "read", what, "error", err)
	return nil
}

func (c *cycleState) push(kind string, body any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("github: encoding a %s payload: %w", kind, err)
	}
	c.poller.queue = append(c.poller.queue, feeder.Payload{Kind: kind, At: c.at, Bytes: raw})
	return nil
}

// repository reads one repository's window.
func (c *cycleState) repository(repo Repository) error {
	p := c.poller
	r := repo.Repo()
	window := p.opts.Window
	window.Since = c.since

	runs, err := p.opts.Reader.WorkflowRuns(c.ctx, r, window)
	if err := c.failed("workflow runs of "+r.String(), err); err != nil {
		return err
	}
	if len(runs) > 0 {
		if err := c.push(PayloadWorkflowRuns, runsPayload(runs)); err != nil {
			return err
		}
	}

	deployments, err := c.deployments(repo)
	if err != nil {
		return err
	}
	if len(deployments) > 0 {
		if err := c.push(PayloadDeployments, deploymentsPayload(deployments)); err != nil {
			return err
		}
	}
	for _, d := range deployments {
		key := heldKey{repo: r, id: d.ID}
		statuses, err := p.opts.Reader.DeploymentStatuses(c.ctx, r, d.ID, p.opts.Window)
		if err := c.failed(fmt.Sprintf("statuses of deployment %d in %s", d.ID, r), err); err != nil {
			return err
		}
		if len(statuses) > 0 {
			if err := c.push(PayloadDeploymentStatuses, statusesPayload(r, d.ID, statuses)); err != nil {
				return err
			}
		}
		switch {
		case settled(statuses):
			delete(p.pending, key)
		case c.at.Sub(d.CreatedAt) > PendingHorizon:
			delete(p.pending, key)
		default:
			p.pending[key] = pendingDeployment{repo: repo, createdAt: d.CreatedAt}
		}
	}

	releases, err := p.opts.Reader.Releases(c.ctx, r, p.opts.Window)
	if err := c.failed("releases of "+r.String(), err); err != nil {
		return err
	}
	var recent []Release
	for _, release := range releases {
		if !release.PublishedAt.Before(c.since) || !release.CreatedAt.Before(c.since) {
			recent = append(recent, release)
		}
	}
	if len(recent) > 0 {
		return c.push(PayloadReleases, releasesPayload(r, recent))
	}
	return nil
}

// deployments are the repository's deployments this cycle reads: the window's, per production
// environment, and every earlier one still without a terminal status.
func (c *cycleState) deployments(repo Repository) ([]Deployment, error) {
	p := c.poller
	r := repo.Repo()
	seen := map[int64]bool{}
	var out []Deployment
	for _, env := range p.opts.Environments {
		listed, err := p.opts.Reader.Deployments(c.ctx, r, env, p.opts.Window)
		if err := c.failed(fmt.Sprintf("deployments of %s to %s", r, env), err); err != nil {
			return nil, err
		}
		for _, d := range listed {
			if seen[d.ID] || (d.CreatedAt.Before(c.since) && d.UpdatedAt.Before(c.since)) {
				continue
			}
			seen[d.ID] = true
			out = append(out, d)
		}
	}
	// The ones an earlier cycle saw unfinished, which the window may no longer reach. Sorted, so the
	// cycle's payloads do not depend on map order.
	var carried []int64
	for key := range p.pending {
		if key.repo == r && !seen[key.id] {
			carried = append(carried, key.id)
		}
	}
	slices.Sort(carried)
	for _, id := range carried {
		d, err := p.opts.Reader.Deployment(c.ctx, r, id)
		if err := c.failed(fmt.Sprintf("deployment %d in %s", id, r), err); err != nil {
			return nil, err
		}
		if d.ID == 0 {
			continue
		}
		seen[d.ID] = true
		out = append(out, d)
	}
	slices.SortStableFunc(out, func(a, b Deployment) int { return cmp.Compare(a.ID, b.ID) })
	return out, nil
}

// settled reports whether a deployment will not move again: a status ended its progress, or GitHub
// marked it `inactive` — superseded, which the status table does not call terminal because it is not an
// outcome (status.go), but which no later status follows.
func settled(statuses []DeploymentStatus) bool {
	return slices.ContainsFunc(statuses, func(s DeploymentStatus) bool {
		return DispositionOf(s.State).Terminal || strings.TrimSpace(s.State) == StateInactive
	})
}

// --- the payload shapes, which are the recording's shapes ---------------------------------------

type pollMarkerPayload struct {
	Outcome string `json:"outcome"`
	Reason  string `json:"reason,omitempty"`
}

type scopeRepositoryPayload struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	FullName string `json:"full_name"`
	Private  bool   `json:"private"`
	Owner    struct {
		Login string `json:"login"`
	} `json:"owner"`
}

func scopePayload(scope Scope) any {
	repos := make([]scopeRepositoryPayload, 0, len(scope.Repositories))
	for _, r := range scope.Repositories {
		repo := scopeRepositoryPayload{ID: r.ID, Name: r.Name, FullName: r.FullName, Private: r.Private}
		repo.Owner.Login = r.Owner
		repos = append(repos, repo)
	}
	return struct {
		TotalCount          int                      `json:"total_count"`
		RepositorySelection string                   `json:"repository_selection"`
		Repositories        []scopeRepositoryPayload `json:"repositories"`
	}{scope.Total, scope.Selection, repos}
}

func accountOf(a Account) accountPayload { return accountPayload(a) }

func deploymentsPayload(deployments []Deployment) any {
	out := make([]deploymentPayload, 0, len(deployments))
	for _, d := range deployments {
		out = append(out, deploymentPayload{
			ID: d.ID, SHA: d.SHA, Ref: d.Ref, Task: d.Task, Environment: d.Environment,
			ProductionEnvironment: d.ProductionEnvironment, TransientEnvironment: d.Transient,
			CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt, Creator: accountOf(d.Creator),
			StatusesURL: d.StatusesURL, URL: d.URL,
		})
	}
	return out
}

type statusPayload struct {
	ID             int64          `json:"id"`
	State          string         `json:"state"`
	Environment    string         `json:"environment,omitempty"`
	CreatedAt      time.Time      `json:"created_at,omitzero"`
	UpdatedAt      time.Time      `json:"updated_at,omitzero"`
	Creator        accountPayload `json:"creator"`
	LogURL         string         `json:"log_url,omitempty"`
	EnvironmentURL string         `json:"environment_url,omitempty"`
	TargetURL      string         `json:"target_url,omitempty"`
}

// statusesPayload wraps GitHub's bare array with the deployment it belongs to, which GitHub states in
// the request path rather than the body (feeder.go applyStatuses).
func statusesPayload(repo Repo, deploymentID int64, statuses []DeploymentStatus) any {
	out := make([]statusPayload, 0, len(statuses))
	for _, s := range statuses {
		out = append(out, statusPayload{
			ID: s.ID, State: s.State, Environment: s.Environment,
			CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt, Creator: accountOf(s.Creator),
			LogURL: s.LogURL, EnvironmentURL: s.EnvironmentURL, TargetURL: s.TargetURL,
		})
	}
	return struct {
		Repository string          `json:"repository"`
		Deployment int64           `json:"deployment_id"`
		Statuses   []statusPayload `json:"statuses"`
	}{repo.String(), deploymentID, out}
}

func runsPayload(runs []WorkflowRun) any {
	out := make([]workflowRunPayload, 0, len(runs))
	for _, r := range runs {
		out = append(out, workflowRunPayload{
			ID: r.ID, Name: r.Name, RunNumber: r.RunNumber, RunAttempt: r.RunAttempt,
			HeadSHA: r.HeadSHA, Event: r.Event, Status: r.Status, Conclusion: r.Conclusion,
			CreatedAt: r.CreatedAt, RunStartedAt: r.RunStartedAt, UpdatedAt: r.UpdatedAt,
			Actor: accountOf(r.Actor), TriggeringActor: accountOf(r.TriggeringActor),
			HTMLURL: r.HTMLURL, LogsURL: r.LogsURL,
		})
	}
	return struct {
		TotalCount int                  `json:"total_count"`
		Runs       []workflowRunPayload `json:"workflow_runs"`
	}{len(out), out}
}

func releasesPayload(repo Repo, releases []Release) any {
	out := make([]releasePayload, 0, len(releases))
	for _, r := range releases {
		out = append(out, releasePayload{
			ID: r.ID, TagName: r.TagName, Name: r.Name, TargetCommitish: r.TargetCommitish,
			Draft: r.Draft, Prerelease: r.Prerelease, CreatedAt: r.CreatedAt, PublishedAt: r.PublishedAt,
			Author: accountOf(r.Author), HTMLURL: r.HTMLURL,
		})
	}
	return struct {
		Repository string           `json:"repository"`
		Releases   []releasePayload `json:"releases"`
	}{repo.String(), out}
}
