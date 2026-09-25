// SPDX-License-Identifier: Apache-2.0

package vercel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The live poll cycle (004 T158; FR-044, FR-056, FR-073).
//
// ---------------------------------------------------------------------------------------------
// What this is
//
// The same shape as the GitHub connector's poller: a [feeder.Source] over the transport that yields the
// payload kinds a recording holds, so a live run and `--replay` are one code path from the first payload.
//
// # One cycle
//
// Per followed project, in id order: the project's own read (`GET /v9/projects/{id}`), its
// environment-variable metadata, and its production deployments created in the window; then a poll
// marker. The followed projects are the operator's mapped ones, or every project the team lists when
// there is no mapping.
//
// The single-project read rather than the listing, because it states everything the listing does AND
// `lastAliasRequest` — the one place Vercel says a rollback or a promotion happened (research §5.4). A
// listing-only cycle would describe the projects and never see a rollback.
//
// # Why a deployment is followed after the window has passed it
//
// A rollout on Vercel is a STATE, not an event: `readySubstate=PROMOTED` (research §3.1). A deployment
// built at 13:54 and staged is not a rollout; the same deployment promoted at 14:18 is — and the
// listing's `since` is on `createdAt`, so the cycle at 14:20 does not list it. So every production
// deployment a cycle sees unsettled (not PROMOTED, and not ERROR, CANCELED or DELETED) is re-read by uid
// each cycle until it settles, or until PendingHorizon. And a new `lastAliasRequest` names the
// deployment production moved to: that deployment is read in the same cycle, which is how an operator
// promoting — or rolling back to — a deployment built last week is seen at all.
//
// # A partial cycle does not advance the window
//
// As for GitHub: a failed read makes the cycle partial, names the read, and the next cycle re-reads the
// same window.
//
// # What a payload holds
//
// The transport's own types, re-encoded, so a field it does not decode never reaches a payload — an
// environment variable's `value`, `legacyValue` and `encryptedValue` above all (FR-038). A deployment's
// `meta` and `attribution.commitMeta` are maps, so they are narrowed here to the keys the mapper reads:
// the commit and `promotedAt`. The rest of what a Git provider puts there — the commit message, the
// author's name and login — is free text and people identifiers, and it stops at this seam.

// PendingHorizon is how long an unsettled production deployment is re-read before the poller lets it go.
const PendingHorizon = 24 * time.Hour

// DefaultPollInterval is the time between cycle starts.
const DefaultPollInterval = 5 * time.Minute

// DefaultLookback is how far back the first cycle reads: one interval plus one reordering window.
const DefaultLookback = DefaultPollInterval + DefaultReorderingWindow

// metaKeys are the `meta` and `attribution.commitMeta` keys the mapper reads (map.go commitOf and the
// promotion instant). Every other key is dropped before a payload is built.
var metaKeys = []string{
	"githubCommitSha", "gitlabCommitSha", "bitbucketCommitSha", "commitSha", "promotedAt",
}

// Reader is what the poller reads through.
type Reader interface {
	DeploymentReader
	ProjectReader
	ConfigReader
}

var _ Reader = (*Client)(nil)

// PollerOptions configures the live source.
type PollerOptions struct {
	// Reader is the transport. Required.
	Reader Reader
	// Projects are the project ids to follow. Empty follows every project the team lists.
	Projects []string
	// Interval, Lookback and Overlap: zero uses DefaultPollInterval, DefaultLookback and
	// DefaultReorderingWindow.
	Interval time.Duration
	Lookback time.Duration
	Overlap  time.Duration
	// Limit bounds each listing. Zero leaves it to the platform.
	Limit int
	// Once stops after one cycle.
	Once bool
	// Now and Wait are the clock and the sleep between cycles. Injectable for tests.
	Now  func() time.Time
	Wait func(context.Context, time.Duration) error
	// Logger is optional.
	Logger *slog.Logger
}

type pendingDeployment struct {
	project   string
	createdAt time.Time
}

// Poller is the live [feeder.Source].
type Poller struct {
	opts PollerOptions
	log  *slog.Logger

	queue  []feeder.Payload
	cycles int
	since  time.Time
	// pending are production deployments seen unsettled, by uid.
	pending map[string]pendingDeployment
	// aliases is the last alias request seen per project, so a new one is acted on once.
	aliases map[string]string
}

// NewPoller validates the options.
func NewPoller(opts PollerOptions) (*Poller, error) {
	if opts.Reader == nil {
		return nil, fmt.Errorf("vercel: a poller with no reader")
	}
	for name, d := range map[string]time.Duration{
		"interval": opts.Interval, "lookback": opts.Lookback, "overlap": opts.Overlap,
	} {
		if d < 0 {
			return nil, fmt.Errorf("vercel: a negative poll %s (%s)", name, d)
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
		opts.Wait = func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		}
	}
	projects := make([]string, 0, len(opts.Projects))
	for _, p := range opts.Projects {
		if p = strings.TrimSpace(p); p != "" {
			projects = append(projects, p)
		}
	}
	slices.Sort(projects)
	opts.Projects = slices.Compact(projects)
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Poller{opts: opts, log: log, pending: map[string]pendingDeployment{}, aliases: map[string]string{}}, nil
}

var _ feeder.Source = (*Poller)(nil)

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

type cycleState struct {
	poller  *Poller
	ctx     context.Context
	at      time.Time
	since   time.Time
	reasons []string
}

func (c *cycleState) failed(what string, err error) error {
	if err == nil {
		return nil
	}
	if ctxErr := c.ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	c.reasons = append(c.reasons, fmt.Sprintf("%s: %v", what, err))
	c.poller.log.WarnContext(c.ctx, "vercel read failed; the cycle is partial", "read", what, "error", err)
	return nil
}

func (c *cycleState) push(kind string, body any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("vercel: encoding a %s payload: %w", kind, err)
	}
	c.poller.queue = append(c.poller.queue, feeder.Payload{Kind: kind, At: c.at, Bytes: raw})
	return nil
}

func (p *Poller) cycle(ctx context.Context) error {
	p.cycles++
	start := p.opts.Now().UTC()
	if p.since.IsZero() {
		p.since = start.Add(-p.opts.Lookback)
	}
	c := &cycleState{poller: p, ctx: ctx, at: start, since: p.since}

	projects := p.opts.Projects
	if len(projects) == 0 {
		listed, err := p.opts.Reader.Projects(ctx, ListWindow{Limit: p.opts.Limit})
		if err := c.failed("the team's projects", err); err != nil {
			return err
		}
		for _, project := range listed {
			if project.ID != "" {
				projects = append(projects, project.ID)
			}
		}
		slices.Sort(projects)
		projects = slices.Compact(projects)
	}
	for _, id := range projects {
		if err := c.project(id); err != nil {
			return err
		}
	}

	marker := struct {
		Outcome string `json:"outcome"`
		Reason  string `json:"reason,omitempty"`
	}{Outcome: "complete"}
	if len(c.reasons) > 0 {
		marker.Outcome, marker.Reason = "partial", strings.Join(c.reasons, "; ")
	} else {
		p.since = start.Add(-p.opts.Overlap)
	}
	raw, err := json.Marshal(marker)
	if err != nil {
		return err
	}
	p.queue = append(p.queue, feeder.Payload{Kind: PayloadPollMarker, At: p.opts.Now().UTC(), Bytes: raw})
	p.log.InfoContext(ctx, "vercel poll cycle read", "cycle", p.cycles, "since", c.since,
		"projects", len(projects), "outcome", marker.Outcome, "pending", len(p.pending))
	return nil
}

// project reads one project's window.
func (c *cycleState) project(id string) error {
	p := c.poller
	project, err := p.opts.Reader.Project(c.ctx, id)
	if err := c.failed("project "+id, err); err != nil {
		return err
	}
	if project.ID != "" {
		if err := c.push(PayloadProject, project); err != nil {
			return err
		}
	}

	envs, err := p.opts.Reader.ProjectEnv(c.ctx, id)
	if err := c.failed("environment metadata of "+id, err); err != nil {
		return err
	}
	if len(envs) > 0 {
		if err := c.push(PayloadProjectEnv, struct {
			ProjectID string       `json:"projectId"`
			Envs      []ProjectEnv `json:"envs"`
		}{id, envs}); err != nil {
			return err
		}
	}

	listed, err := p.opts.Reader.Deployments(c.ctx, id, "production", ListWindow{Since: c.since, Limit: p.opts.Limit})
	if err := c.failed("production deployments of "+id, err); err != nil {
		return err
	}
	seen := map[string]bool{}
	var deployments []Deployment
	add := func(d Deployment) {
		if d.UID != "" && !seen[d.UID] {
			seen[d.UID] = true
			deployments = append(deployments, d)
		}
	}
	for _, d := range listed {
		add(d)
	}

	// The deployment a new alias request moved production to, which may be far older than the window.
	if req := project.LastAliasRequest; req != nil && strings.TrimSpace(req.ToDeploymentID) != "" {
		key := req.Type + "/" + req.ToDeploymentID + "@" + fmt.Sprint(req.RequestedAt)
		requested, stated := millis(req.RequestedAt)
		if p.aliases[id] != key && (!stated || !requested.Before(c.since)) && !seen[req.ToDeploymentID] {
			d, err := p.opts.Reader.Deployment(c.ctx, req.ToDeploymentID)
			if err := c.failed("deployment "+req.ToDeploymentID+" named by "+id+"'s alias request", err); err != nil {
				return err
			}
			add(d)
		}
		p.aliases[id] = key
	}

	// Every deployment of this project an earlier cycle saw unsettled. Sorted, so the payload does not
	// depend on map order.
	var carried []string
	for uid, pending := range p.pending {
		if pending.project == id && !seen[uid] {
			carried = append(carried, uid)
		}
	}
	slices.Sort(carried)
	for _, uid := range carried {
		d, err := p.opts.Reader.Deployment(c.ctx, uid)
		if err := c.failed("deployment "+uid, err); err != nil {
			return err
		}
		add(d)
	}

	for _, d := range deployments {
		created, _ := millis(d.CreatedAt)
		switch {
		case settled(d), !created.IsZero() && c.at.Sub(created) > PendingHorizon:
			delete(p.pending, d.UID)
		case strings.EqualFold(d.Target, "production"):
			p.pending[d.UID] = pendingDeployment{project: id, createdAt: created}
		}
	}
	if len(deployments) == 0 {
		return nil
	}
	narrowed := make([]Deployment, 0, len(deployments))
	for _, d := range deployments {
		narrowed = append(narrowed, narrow(d))
	}
	return c.push(PayloadDeployments, struct {
		Deployments []Deployment `json:"deployments"`
	}{narrowed})
}

// settled reports whether a deployment will not change state in a way the mapper reads again.
func settled(d Deployment) bool {
	if strings.EqualFold(strings.TrimSpace(d.ReadySubstate), "PROMOTED") {
		return true
	}
	switch strings.ToUpper(strings.TrimSpace(d.ReadyState)) {
	case "ERROR", "CANCELED", "DELETED":
		return true
	}
	return false
}

// narrow keeps only the metadata keys the mapper reads.
func narrow(d Deployment) Deployment {
	d.Meta = keep(d.Meta)
	d.Attribution.CommitMeta = keep(d.Attribution.CommitMeta)
	return d
}

func keep(m map[string]string) map[string]string {
	var out map[string]string
	for _, k := range metaKeys {
		if v, ok := m[k]; ok {
			if out == nil {
				out = map[string]string{}
			}
			out[k] = v
		}
	}
	return out
}
