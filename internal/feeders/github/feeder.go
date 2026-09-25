// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The poll cycle (004 US1; FR-006, FR-021, FR-052, FR-056, FR-057).
//
// ---------------------------------------------------------------------------------------------
// Why a payload stream rather than a loop of API calls
//
// The feeder reads [feeder.Payload]s and never calls GitHub itself. Live mode supplies them from the
// transport; a replay supplies the same bytes from disk. So FR-044's "live and recorded are one code
// path" is structural rather than a rule somebody remembers — this file cannot tell which side it is
// on, and there is no client here to reach around the seam with.
//
// # Why a deployment is held until its statuses arrive
//
// A deployment payload says a rollout was *requested*; the statuses say whether anything reached
// production and when. One without the other is not a change, and emitting on the deployment alone
// would date every rollout at its request and mark it successful before it was.
//
// So a deployment is held, and the poll marker is what releases the cycle: at the end of a poll, every
// deployment whose statuses arrived becomes changes and every deployment whose statuses did not is
// **counted as deferred, not dropped**. FR-073's distinction at the level of one object — work not
// done yet is not work decided against.
//
// # Why the checkpoint carries the extent and the scope
//
// FR-057: an operator reading a checkpoint has to know what window was covered, under which grant, and
// whether the feeder was watching immediately before it. A checkpoint that said only "up to 14:30"
// cannot distinguish a quiet window from a window nobody read.

// Kind is the connector family. Fixture payload directories are grouped by it.
const Kind = "github"

// SourceIDPrefix makes the source id `github:<org>`. A feeder token is scoped to exactly one source
// id, so an empty org would make every organisation's events one source.
const SourceIDPrefix = "github:"

// SchemaVersion is the event schema version this feeder emits.
const SchemaVersion = "1.0.0"

// DefaultReorderingWindow is how far out of order this feeder may deliver its own events.
//
// Deployments, statuses, runs and releases are four independent paginated reads plus an inbound
// doorbell, none of which GitHub orders against the others, so the window is the poll interval rather
// than zero. Declaring one wider than the truth makes the conformance shuffle stricter, never weaker.
const DefaultReorderingWindow = 5 * time.Minute

// DefaultPollInterval is how often this connector reads GitHub, and therefore the interval every
// change it emits declares its history was sampled at (FR-052).
//
// It is the same five minutes as DefaultReorderingWindow, and that is a consequence rather than a
// definition: the window is derived FROM the cadence, because four independent paginated reads plus a
// doorbell can deliver a cycle's events across the length of one poll. Writing it as a separate
// constant is the point. The two answer different questions — how far out of order I may deliver, and
// how often I look — and an operator who widened the window for a slow paginated read, without
// touching their cadence, would otherwise silently change the sampling interval that every change in
// the graph claims.
const DefaultPollInterval = 5 * time.Minute

// The payload kinds, named here rather than in a fixture author's head so a recording and a replay
// agree.
const (
	PayloadInstallationRepositories = "installation-repositories"
	PayloadDeployments              = "deployments"
	PayloadDeploymentStatuses       = "deployment-statuses"
	PayloadWorkflowRuns             = "workflow-runs"
	PayloadReleases                 = "releases"
	// PayloadPollMarker ends a poll. It is what releases held deployments and writes the checkpoint,
	// and it carries the poll's outcome so a partial read is declared rather than inferred.
	PayloadPollMarker = "poll-marker"
)

// RequiredScopes documents the permissions an operator grants. Documentation and a startup checklist;
// the enforcement point is the gate and the published surface.
var RequiredScopes = []string{
	"deployments: read",
	"actions: read",
	"contents: read (metadata only; no file is ever fetched)",
	"metadata: read",
}

// MintedNamespaces are the namespaces this feeder mints refs in, whether as an identity or as a
// correlation key. The testkit fails a run that emits a ref outside a Description's declared set, which
// is what stops a connector inventing a namespace nothing joins on.
//
// The set is flat and does not say which kind each namespace is used as. That is not an omission: what
// a namespace MEANS is published once, in internal/graph's correlation registry, and the event log
// enforces it on every event. A second, per-connector opinion about it would be a second place for the
// answer to drift.
//
// The namespaces it TARGETS are not here, and cannot be: a deploy feeder attaches its changes to
// entities other connectors own, and which of those an installation touches is the operator's mapping.
// Describe() adds them from the configuration — see TargetMap.Namespaces.
var MintedNamespaces = []string{
	feeder.NSGitHubChange,
	feeder.NSDeployCommitSHA,
	feeder.NSDeployRelease,
}

// Options is the feeder's configuration.
type Options struct {
	// OrgSlug is the suffix of the source id.
	OrgSlug string
	// Map is the operator's allowlists, target mapping, actor policy and release policy.
	Map MapOptions
	// ReorderingWindow overrides DefaultReorderingWindow.
	ReorderingWindow time.Duration
	// PollInterval is the cadence this connector reads GitHub at. Zero uses DefaultPollInterval. It
	// reaches every change as the interval its history was sampled at (FR-052), which is why a
	// negative one is refused at startup rather than clamped: a run whose changes claimed a negative
	// sampling interval would be recording nonsense about its own coverage.
	PollInterval time.Duration
	// Logger is optional.
	Logger *slog.Logger
}

// Gate proves the credential read-only before anything is emitted. A replay has none, and the
// checkpoint says so rather than implying a check that did not run.
type Gater interface {
	Prove(ctx context.Context) (GateResult, error)
}

// Feeder turns one GitHub installation into graph events.
type Feeder struct {
	opts Options
	log  *slog.Logger

	// Gate is asked to prove the credential read-only before anything is emitted (FR-003). Nil means
	// there is nothing to prove — a replay has no credential.
	Gate Gater

	// skew observes the gap between GitHub's timestamps and this process's clock (FR-058, T142).
	//
	// Reported, NEVER used to correct either clock: a vendor timestamp is evidence about when a fact
	// was true and the arrival instant is when this system learned it, so shifting either to make them
	// agree would destroy the distinction and do it invisibly.
	skew *feeder.Skew

	mu sync.Mutex
	// held is the deployments observed and not yet resolvable, by repository and id. A deployment
	// whose statuses have not arrived is not a change yet.
	held map[heldKey]Observation
	// statuses are the transitions observed so far, by the deployment they belong to.
	statuses map[heldKey][]DeploymentStatus
	// runs are the workflow runs observed this cycle, by id, so a deployment can find the run that
	// produced it.
	runs map[int64]WorkflowRun
	// repositories is the grant, by numeric id, which is what every change identity is built from.
	repositories map[string]Repository
	// scope is the regime the grant puts the feeder in, for the checkpoint (FR-008).
	scope feeder.CredentialScope
	// gateEvidence is HOW that regime was established, and it is on the checkpoint beside the regime
	// itself because the two answer different questions: the regime says what the credential may
	// reach, the evidence says who said so. It is empty on a replay, and the note says so in words
	// rather than leaving a silence a reader would take for a check that passed (FR-003).
	gateEvidence string
	// excluded accumulates the cycle's exclusions by reason (FR-019).
	excluded Exclusions
	// deferred counts deployments held across a poll boundary (FR-073).
	deferred int
	// gapBefore marks the next checkpoint as following a partial read.
	gapBefore bool
	// extentFrom is the start of the window the current poll covers.
	extentFrom time.Time
}

type heldKey struct {
	repo Repo
	id   int64
}

// New builds the feeder.
func New(opts Options) (*Feeder, error) {
	if opts.OrgSlug == "" {
		return nil, fmt.Errorf("github: a feeder with no organisation slug; the source id would be " +
			"`github:` and every organisation's events would be one source")
	}
	if opts.PollInterval < 0 {
		// Refused rather than clamped. The interval reaches every change as the interval its history
		// was sampled at (FR-052), so a negative one would have this connector record nonsense about
		// its own coverage — and it would record it silently, on every change, for the whole run.
		return nil, fmt.Errorf("github: Options.PollInterval is negative (%s); it is recorded on every "+
			"change as the interval that change's history was sampled at, and a negative sampling "+
			"interval is not a claim this connector can make about its own coverage (FR-052)",
			opts.PollInterval)
	}
	if opts.ReorderingWindow < 0 {
		return nil, fmt.Errorf("github: Options.ReorderingWindow is negative (%s)", opts.ReorderingWindow)
	}
	// The cadence reaches the mapper through MapOptions, which is what every change is built from.
	// Assigned here rather than asked of the caller twice: an operator sets one interval, and a
	// connector whose checkpoint and whose changes could disagree about it would be worse than one
	// that never stated it.
	if opts.Map.PollInterval == 0 {
		opts.Map.PollInterval = opts.PollInterval
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Feeder{
		opts: opts, log: log, skew: &feeder.Skew{},
		held:         map[heldKey]Observation{},
		statuses:     map[heldKey][]DeploymentStatus{},
		runs:         map[int64]WorkflowRun{},
		repositories: map[string]Repository{},
	}, nil
}

// Describe returns the feeder's contract. Pure: the harness calls it before Run and compares what Run
// emits against it.
func (f *Feeder) Describe() feeder.Description {
	window := f.opts.ReorderingWindow
	if window == 0 {
		window = DefaultReorderingWindow
	}
	return feeder.Description{
		SourceID:      SourceIDPrefix + f.opts.OrgSlug,
		Kind:          Kind,
		SchemaVersion: SchemaVersion,
		// Four independent paginated reads plus a doorbell, none ordered against the others.
		Ordering:         feeder.OrderingNone,
		ReorderingWindow: window,
		RequiredScopes:   append([]string(nil), RequiredScopes...),
		Namespaces:       f.namespaces(),
	}
}

// namespaces is what this feeder mints plus what the operator's mapping targets, deduplicated and
// sorted. Pure: it reads configuration, never cycle state, so Describe() stays a pure function the
// harness can call before Run.
func (f *Feeder) namespaces() []string {
	seen := map[string]bool{}
	for _, ns := range MintedNamespaces {
		seen[ns] = true
	}
	for _, ns := range f.opts.Map.Targets.Namespaces() {
		seen[ns] = true
	}
	out := make([]string, 0, len(seen))
	for ns := range seen {
		out = append(out, ns)
	}
	slices.Sort(out)
	return out
}

// Run reads from src and writes to em.
//
// The order is Validate → gate → emit, and nothing before the gate emits. That is what makes
// `--dry-run` a separate step rather than something folded into the first read.
// Skew is the clock-skew observation this cycle accumulated (T142, FR-058).
//
// Exported so a caller can read the measurement rather than parse a log line — which is what makes it
// assertable, and what T142 found missing: the machinery existed with no way to observe that it had run.
func (f *Feeder) Skew() feeder.SkewReport { return f.skew.Report() }

func (f *Feeder) Run(ctx context.Context, src feeder.Source, em feeder.Emitter) error {
	desc := f.Describe()
	if err := desc.Validate(); err != nil {
		return err
	}

	// FR-003. Proved before anything is emitted, and there is no path in which a read happens first.
	if f.Gate != nil {
		result, err := f.Gate.Prove(ctx)
		if err != nil {
			return fmt.Errorf("github: the read-only gate refused, so nothing was emitted: %w", err)
		}
		f.mu.Lock()
		f.scope = result.Scope
		f.gateEvidence = string(result.Evidence)
		f.mu.Unlock()
		f.log.InfoContext(ctx, "read-only gate passed",
			"source", desc.SourceID, "evidence", string(result.Evidence),
			"selection", result.Scope.Selection)
	} else {
		f.log.InfoContext(ctx, "no credential to prove: replaying a recording", "source", desc.SourceID)
	}

	defer func() {
		if err := em.Flush(ctx); err != nil {
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
		if err := f.apply(ctx, desc, em, payload); err != nil {
			return err
		}
	}
}

var _ feeder.Feeder = (*Feeder)(nil)

// apply routes one payload to the reader for its kind.
//
// An unknown kind is an error rather than a skip: a fixture directory naming a payload this feeder does
// not read is a fixture that silently verifies nothing.
func (f *Feeder) apply(ctx context.Context, desc feeder.Description, em feeder.Emitter, payload feeder.Payload) error {
	switch payload.Kind {
	case PayloadInstallationRepositories:
		return f.applyScope(payload.Bytes)
	case PayloadDeployments:
		return f.applyDeployments(payload.Bytes, payload.At)
	case PayloadDeploymentStatuses:
		return f.applyStatuses(payload.Bytes)
	case PayloadWorkflowRuns:
		return f.applyRuns(payload.Bytes)
	case PayloadReleases:
		return f.applyReleases(ctx, desc, em, payload.Bytes, payload.At)
	case PayloadPollMarker:
		return f.applyPollMarker(ctx, desc, em, payload.Bytes, payload.At)
	default:
		return fmt.Errorf("github: payload kind %q is not one this feeder reads; a fixture naming it "+
			"would verify nothing", payload.Kind)
	}
}

// applyScope records the grant. Every change identity is built from a repository's numeric id, so the
// grant is what makes a deployment mappable at all.
func (f *Feeder) applyScope(raw []byte) error {
	var body struct {
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
	if err := json.Unmarshal(raw, &body); err != nil {
		return fmt.Errorf("github: decoding the installation repositories: %w", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range body.Repositories {
		repo := Repository{
			ID: r.ID, FullName: r.FullName, Owner: r.Owner.Login, Name: r.Name, Private: r.Private,
		}
		key, ok := feeder.Repository(repo.Owner, repo.Name)
		if !ok {
			continue
		}
		f.repositories[key] = repo
	}
	if f.scope.Selection == "" && body.RepositorySelection != "" {
		// A replay has no gate, so the regime comes from the payload instead — and it is still the
		// platform's word rather than this code's interpretation of it.
		f.scope.Selection = body.RepositorySelection
		f.scope.PlatformEnforced = true
	}
	return nil
}

// applyDeployments holds each deployment until its statuses arrive.
func (f *Feeder) applyDeployments(raw []byte, at time.Time) error {
	var payloads []deploymentPayload
	if err := json.Unmarshal(raw, &payloads); err != nil {
		return fmt.Errorf("github: decoding deployments: %w", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.extentFrom.IsZero() {
		f.extentFrom = at
	}
	for _, p := range payloads {
		deployment := p.deployment()
		// The skew, taken BEFORE the grant check below, and on every deployment GitHub sent. A
		// deployment in a repository outside the grant still carries a timestamp from the same platform
		// clock, and measuring only the ones this connector can map would measure the skew of a subset
		// chosen by something unrelated to clocks.
		f.skew.Observe(deployment.CreatedAt, at)
		repo, ok := f.repoOfLocked(p.repositoryKey())
		if !ok {
			// A deployment in a repository the grant did not name. Counted rather than dropped: it means
			// the scope payload and the deployment payload disagree, which is worth seeing.
			f.excluded.Exclude("a deployment in a repository the installation's grant does not carry")
			continue
		}
		key := heldKey{repo: repo.Repo(), id: deployment.ID}
		f.held[key] = Observation{
			Repo: repo.Repo(), RepositoryID: repo.ID, Deployment: deployment,
		}
	}
	return nil
}

// repositoryKey reads `owner/repo` off a deployment's own URL, which is the only place the payload
// states it — GitHub's deployment object carries no repository field of its own.
func (d deploymentPayload) repositoryKey() string {
	for _, candidate := range []string{d.URL, d.StatusesURL} {
		path, ok := ResourcePath(candidate)
		if !ok {
			continue
		}
		parts := splitPath(path)
		if len(parts) >= 3 && parts[0] == "repos" {
			if key, ok := feeder.Repository(parts[1], parts[2]); ok {
				return key
			}
		}
	}
	return ""
}

func splitPath(path string) []string {
	var out []string
	for _, part := range slices.Collect(splitSeq(path, '/')) {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func splitSeq(s string, sep byte) func(func(string) bool) {
	return func(yield func(string) bool) {
		start := 0
		for i := 0; i < len(s); i++ {
			if s[i] == sep {
				if !yield(s[start:i]) {
					return
				}
				start = i + 1
			}
		}
		yield(s[start:])
	}
}

func (f *Feeder) repoOfLocked(key string) (Repository, bool) {
	repo, ok := f.repositories[key]
	return repo, ok
}

// applyStatuses attaches transitions to the deployment they belong to, in the order GitHub returned
// them. The order is never normalised: Edge case 11 turns on the platform's own.
func (f *Feeder) applyStatuses(raw []byte) error {
	var body struct {
		// The fixture wraps the array so the statuses can say which deployment they belong to; GitHub's
		// own response is a bare array read from a path that already names it.
		Repository  string `json:"repository"`
		Deployment  int64  `json:"deployment_id"`
		RawStatuses []struct {
			ID             int64          `json:"id"`
			State          string         `json:"state"`
			Environment    string         `json:"environment"`
			CreatedAt      time.Time      `json:"created_at"`
			UpdatedAt      time.Time      `json:"updated_at"`
			Creator        accountPayload `json:"creator"`
			LogURL         string         `json:"log_url"`
			EnvironmentURL string         `json:"environment_url"`
			TargetURL      string         `json:"target_url"`
		} `json:"statuses"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return fmt.Errorf("github: decoding deployment statuses: %w", err)
	}
	owner, name, ok := splitSlug(body.Repository)
	if !ok {
		return fmt.Errorf("github: a deployment-statuses payload names the repository %q, which is not "+
			"`owner/repo`; without it the statuses cannot be attached to their deployment", body.Repository)
	}
	key := heldKey{repo: Repo{Owner: owner, Name: name}, id: body.Deployment}

	statuses := make([]DeploymentStatus, 0, len(body.RawStatuses))
	for _, s := range body.RawStatuses {
		statuses = append(statuses, DeploymentStatus{
			ID: s.ID, State: s.State, Environment: s.Environment,
			CreatedAt: s.CreatedAt.UTC(), UpdatedAt: s.UpdatedAt.UTC(),
			Creator: s.Creator.account(),
			LogURL:  s.LogURL, EnvironmentURL: s.EnvironmentURL, TargetURL: s.TargetURL,
		})
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statuses[key] = append(f.statuses[key], statuses...)
	return nil
}

func splitSlug(slug string) (owner, name string, ok bool) {
	parts := splitPath(slug)
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// applyRuns records the runs a deployment may have come from.
func (f *Feeder) applyRuns(raw []byte) error {
	var body struct {
		TotalCount int                  `json:"total_count"`
		Runs       []workflowRunPayload `json:"workflow_runs"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return fmt.Errorf("github: decoding workflow runs: %w", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range body.Runs {
		f.runs[r.ID] = r.run()
	}
	return nil
}

// applyReleases emits releases directly: a release needs no second payload to be a fact, and a
// repository that ships by release produces its rollout from this alone (FR-025).
func (f *Feeder) applyReleases(ctx context.Context, desc feeder.Description, em feeder.Emitter, raw []byte, at time.Time) error {
	var body struct {
		Repository string           `json:"repository"`
		Releases   []releasePayload `json:"releases"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return fmt.Errorf("github: decoding releases: %w", err)
	}
	owner, name, ok := splitSlug(body.Repository)
	if !ok {
		return fmt.Errorf("github: a releases payload names the repository %q, which is not `owner/repo`",
			body.Repository)
	}
	repo := Repo{Owner: owner, Name: name}

	f.mu.Lock()
	key, _ := feeder.Repository(owner, name)
	repository, known := f.repoOfLocked(key)
	f.mu.Unlock()
	if !known {
		f.mu.Lock()
		f.excluded.Exclude("a release in a repository the installation's grant does not carry")
		f.mu.Unlock()
		return nil
	}

	for _, p := range body.Releases {
		release := p.release()
		decided := f.opts.Map.Releases.ReadRelease(repo, release)
		if !decided.Emit {
			f.mu.Lock()
			f.excluded.Exclude(decided.Why)
			f.mu.Unlock()
			continue
		}
		ref, ok := ReleaseChangeRef(repository.ID, release.ID)
		if !ok {
			return fmt.Errorf("github: release %d in %s has no identity", release.ID, repo)
		}
		releaseProps := feeder.NewProps().
			Str(PropObjectKind, decided.ObjectKind).
			Str(PropReleaseTag, release.TagName).
			Bool(PropPrerelease, release.Prerelease)
		if repository, ok := RepositoryProperty(repo); ok {
			releaseProps = releaseProps.Str(AttrRepository, repository)
		}
		props, err := releaseProps.Build()
		if err != nil {
			return err
		}
		origin, _ := OriginLink(release.HTMLURL)
		var pointers []*graphv1.Pointer
		if pointer, ok := ReleaseSourceLink(repo, release.ID); ok {
			pointers = append(pointers, pointer)
		}
		actor := f.opts.Map.Actors.Classify(ActorEvidence{Account: release.Author})

		fact := feeder.ChangeFact{
			Meta:      feeder.Meta{SourceObservedAt: at},
			Ref:       ref,
			Kind:      decided.Kind,
			KindOther: decided.KindOther,
			Summary:   fmt.Sprintf("released %s of %s", release.TagName, repo),
			Actor:     actor.Login,
			ActorKind: actor.Kind,
			OriginRef: origin,
			ValidAt:   decided.ValidAt,
			Props:     props,
			Pointers:  pointers,
		}
		// A release names no target of its own: which service a tag ships is the operator's mapping,
		// and a release in a repository that ships by release targets whatever that repository does.
		fact.Targets = append(fact.Targets, f.opts.Map.Targets.TargetsFor(repo, "", "")...)
		id := feeder.NewID(desc.SourceID, "change", ref.GetValue())
		if err := emitOne(ctx, em, feeder.ObserveChange(desc, id, fact)); err != nil {
			return err
		}
		for _, key := range releaseKeys(repo, release) {
			fact := feeder.CorrelationFact{
				Meta:       feeder.Meta{SourceObservedAt: at},
				Subject:    ref,
				Key:        key.Ref(),
				Attributes: keyAttributes(key, ""),
			}
			id := feeder.NewID(desc.SourceID, "correlation", ref.GetValue(), key.Namespace, key.Value)
			if err := emitOne(ctx, em, feeder.Correlate(desc, id, fact)); err != nil {
				return err
			}
		}
	}
	return nil
}

// PropReleaseTag and PropPrerelease are what a release carries beyond the taxonomy.
const (
	PropReleaseTag = "sre.github.release_tag"
	PropPrerelease = "sre.github.prerelease"
)

// releaseKeys are the release's shared values: the tag, and the commitish it points at where that is a
// full sha. `target_commitish` is often a branch name, which the normaliser refuses — omitted rather
// than recorded as a commit it is not.
//
// A tag is a correlation and not a name for the same reason a commit is: `v2.3.0` is a tag many
// repositories use in the same week, and one release's commit is also the commit of every rollout that
// shipped it.
func releaseKeys(repo Repo, release Release) []feeder.CorrelationKey {
	// No repository key: several releases share one repository, and no published rule correlates on it
	// (claims.go). It rides as a property on the change and as an attribute on these keys.
	return DeployKeys(repo, release.TargetCommitish, SourceReleaseTag, release.TagName)
}

// applyPollMarker ends a poll: it resolves every deployment whose statuses arrived, counts the rest as
// deferred, and writes the checkpoint.
func (f *Feeder) applyPollMarker(ctx context.Context, desc feeder.Description, em feeder.Emitter, raw []byte, at time.Time) error {
	var marker struct {
		// Outcome is `complete` or `partial`. A partial poll declares a gap rather than having one
		// inferred from a gap in the events (FR-056).
		Outcome string `json:"outcome"`
		// Reason is why a poll was partial, where the poller knows. It is optional: a partial poll
		// with no reason is still recorded as partial, because the honest answer to "why?" is
		// sometimes "the feeder does not know", and dropping the flag for want of a reason would
		// turn ignorance into a claim of completeness.
		Reason string `json:"reason"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &marker); err != nil {
			return fmt.Errorf("github: decoding the poll marker: %w", err)
		}
	}

	f.mu.Lock()
	keys := make([]heldKey, 0, len(f.held))
	for key := range f.held {
		keys = append(keys, key)
	}
	// Sorted, so a poll releasing several deployments emits them in one order rather than Go's.
	slices.SortFunc(keys, func(a, b heldKey) int {
		if c := compareRepo(a.repo, b.repo); c != 0 {
			return c
		}
		return compareInt64(a.id, b.id)
	})
	extentFrom := f.extentFrom
	gapBefore := f.gapBefore
	scope := f.scope
	gateEvidence := f.gateEvidence
	repositories := len(f.repositories)
	f.mu.Unlock()
	// `excluded` and `deferred` are deliberately NOT gathered here. Both are mutated by the release
	// loop below — a deployment whose statuses never arrived is counted deferred there, and a released
	// deployment the target map or the allowlist rejects is excluded there — so reading them at this
	// point reports the PREVIOUS cycle's totals, which on a first poll is zero. That was a real defect
	// in the first cut of this checkpoint (004 T043): the note said `deferred=0` on a cycle that had
	// deferred a deployment, while the log line four lines below it said 1, because the log line read
	// the counters after the loop and the note read them before. The next test written against the
	// note is what caught it, which is the argument for the note being asserted rather than only
	// logged. They are read after the loop, beside `released`.

	// The instants two deployments share are what Edge case 11 turns on, so the ties are computed over
	// the whole released set rather than per deployment.
	instants := make([]time.Time, 0, len(keys))
	for _, key := range keys {
		f.mu.Lock()
		statuses := f.statuses[key]
		f.mu.Unlock()
		instants = append(instants, ReadStatuses(statuses).CompletedAt)
	}
	tied := TieAtSameInstant(instants)

	var released int
	for i, key := range keys {
		f.mu.Lock()
		obs, ok := f.held[key]
		statuses := f.statuses[key]
		f.mu.Unlock()
		if !ok {
			continue
		}
		if len(statuses) == 0 {
			// Held across the boundary: the statuses have not arrived. Deferred, not excluded — one is
			// work not done yet and the other is work decided against (FR-073).
			f.mu.Lock()
			f.deferred++
			f.mu.Unlock()
			continue
		}
		obs.Statuses = statuses
		obs.OrderingAmbiguous = tied[i]
		if run, ok := f.runFor(obs); ok {
			obs.Run = &run
		}

		events, excluded, err := MapDeployment(desc, obs, f.opts.Map, at)
		if err != nil {
			return err
		}
		f.mu.Lock()
		for reason, count := range excluded.ByReason() {
			for range count {
				f.excluded.Exclude(reason)
			}
		}
		delete(f.held, key)
		delete(f.statuses, key)
		f.mu.Unlock()

		for _, event := range events {
			if err := emitOne(ctx, em, event); err != nil {
				return err
			}
		}
		released++
	}

	// The checkpoint. FR-057 asks for three things and this now carries all three: the extent
	// covered, **the filters and scope in force**, and whether the feeder was watching before it.
	// The middle one was missing, and checkpoint.go records why at length — the short version is
	// that `Emitter.Checkpoint` used to take three fields and the scope was gathered, documented on
	// the field that holds it, and then not passed.
	if extentFrom.IsZero() {
		extentFrom = at
	}
	partial := marker.Outcome == "partial"
	f.mu.Lock()
	excluded := f.excluded.ByReason()
	deferred := f.deferred
	f.mu.Unlock()

	checkpoint := Checkpoint{
		From: extentFrom, To: at,
		Partial:          partial,
		PartialReason:    strings.TrimSpace(marker.Reason),
		GapBefore:        gapBefore,
		Scope:            scope,
		GateEvidence:     gateEvidence,
		Repositories:     repositories,
		Environments:     f.opts.Map.Allowlist.Environments,
		Workflows:        f.opts.Map.Allowlist.Workflows,
		TargetRules:      len(f.opts.Map.Targets.Repositories),
		ReorderingWindow: desc.ReorderingWindow,
		PollInterval:     f.opts.Map.pollInterval(),
		Excluded:         excluded,
		Deferred:         deferred,
		Released:         released,
		Skew:             f.skew.Report(),
	}
	if err := em.Checkpoint(ctx, feeder.CheckpointFact{
		ExtentFrom: extentFrom,
		ExtentTo:   at,
		GapBefore:  gapBefore,
		Note:       checkpoint.Note(),
	}); err != nil {
		return err
	}

	f.mu.Lock()
	f.extentFrom = time.Time{}
	// A partial poll means the NEXT checkpoint follows a gap: the silence before it was ignorance
	// rather than absence (FR-032, FR-056).
	f.gapBefore = partial
	f.mu.Unlock()

	// The skew, published with the cycle (T142, FR-058). `samples` rides beside the mean because "the
	// clocks agree" and "we never looked" are different claims and a mean of zero is both.
	skew := f.skew.Report()
	f.log.InfoContext(ctx, "poll complete",
		"released", released, "deferred", f.Deferred(), "excluded", f.ExcludedTotal(),
		"partial", partial,
		"skew_samples", skew.Samples, "skew_mean", skew.Mean, "skew_min", skew.Min,
		"skew_max", skew.Max, "skew_threshold", skew.Threshold, "skew_exceeded", skew.Exceeded)
	if skew.Beyond() {
		// Its own line rather than a field somebody has to notice: past the threshold, a rollout's valid
		// time and the instant the graph learned of it are far enough apart to change which change a diff
		// window catches, which is exactly when an operator needs told.
		f.log.WarnContext(ctx, "github's clock is beyond the reporting threshold; nothing is corrected",
			"samples", skew.Samples, "exceeded", skew.Exceeded, "mean", skew.Mean, "max", skew.Max,
			"threshold", skew.Threshold)
	}
	return nil
}

// runFor finds the workflow run a deployment came from.
//
// By head sha, because GitHub's deployment object does not name its run: a deployment created by a
// workflow shares the commit the run was for. A sha matching several runs takes the **latest attempt**,
// since that is the one that produced the deployment being resolved now.
func (f *Feeder) runFor(obs Observation) (WorkflowRun, bool) {
	sha := obs.Deployment.SHA
	if sha == "" {
		return WorkflowRun{}, false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var best WorkflowRun
	var found bool
	for _, run := range f.runs {
		if run.HeadSHA != sha {
			continue
		}
		if !found || run.RunAttempt > best.RunAttempt ||
			(run.RunAttempt == best.RunAttempt && run.ID > best.ID) {
			best, found = run, true
		}
	}
	return best, found
}

// Excluded is the cycle's exclusions, by reason (FR-019).
//
// A map rather than the accumulator, because the caller wants the counts and an accumulator handed out
// by value is one whose methods do not work on it.
func (f *Feeder) Excluded() map[string]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.excluded.ByReason()
}

// ExcludedTotal is a headline number, and never the whole report: a bare count cannot tell a
// configured filter doing its job from a connector that has gone silent (FR-019).
func (f *Feeder) ExcludedTotal() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.excluded.Total()
}

// Deferred is how many deployments were held across a poll boundary (FR-073).
func (f *Feeder) Deferred() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.deferred
}

// Scope is the regime the grant puts this feeder in, for the checkpoint (FR-008).
func (f *Feeder) Scope() feeder.CredentialScope {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.scope
}

func compareRepo(a, b Repo) int {
	if a.Owner != b.Owner {
		if a.Owner < b.Owner {
			return -1
		}
		return 1
	}
	if a.Name == b.Name {
		return 0
	}
	if a.Name < b.Name {
		return -1
	}
	return 1
}

func compareInt64(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// emitOne sends one event and turns a graph refusal into an error the run stops on.
//
// A REJECTED result is an answer rather than a transport failure, and it is still a stop: an event the
// graph refused means this feeder built something the schema forbids, which is a defect to fix rather
// than a condition to continue through.
func emitOne(ctx context.Context, em feeder.Emitter, event *graphv1.EventEnvelope) error {
	result, err := em.Emit(ctx, event)
	if err != nil {
		return err
	}
	if result.GetStatus() == graphv1.IngestResult_REJECTED {
		return fmt.Errorf("github: the graph refused event %s: %s %s",
			event.GetEventId(), result.GetReasonCode(), result.GetReasonDetail())
	}
	return nil
}
