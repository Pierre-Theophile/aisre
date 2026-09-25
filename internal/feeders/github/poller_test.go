// SPDX-License-Identifier: Apache-2.0

package github_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/feeders/github"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/emit"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/record"
)

// The live poll cycle (004 T157).

// steppingClock returns its instants in order, then repeats the last one.
type steppingClock struct {
	instants []time.Time
	calls    int
}

func (c *steppingClock) Now() time.Time {
	at := c.instants[min(c.calls, len(c.instants)-1)]
	c.calls++
	return at
}

// fixtureBody reads one recorded payload of github-deployment-01.
func fixtureBody(t *testing.T, kind string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "fixtures", "github-deployment-01", "payloads", kind, "000001.json"))
	if err != nil {
		t.Fatalf("read the recorded %s: %v", kind, err)
	}
	return raw
}

// FR-044, made literal: GitHub answering with the recording's own bodies, read by the live transport
// and the poller, produces the recording's events byte for byte. So a campaign that records through
// this source records what the replay corpus replays, and the corpus is not a second reader's opinion
// of GitHub.
func TestALivePollReproducesTheRecording(t *testing.T) {
	scope := fixtureBody(t, github.PayloadInstallationRepositories)
	runs := fixtureBody(t, github.PayloadWorkflowRuns)
	deployments := fixtureBody(t, github.PayloadDeployments)
	var wrapped struct {
		Statuses json.RawMessage `json:"statuses"`
	}
	if err := json.Unmarshal(fixtureBody(t, github.PayloadDeploymentStatuses), &wrapped); err != nil {
		t.Fatalf("decode the recorded statuses: %v", err)
	}
	c, stand, _ := client(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/installation/repositories":
			_, _ = w.Write(scope)
		case "/repos/acme/storefront/actions/runs":
			_, _ = w.Write(runs)
		case "/repos/acme/storefront/deployments":
			if r.URL.Query().Get("environment") != "production" {
				t.Errorf("deployments listed for %q; only the production allowlist was configured",
					r.URL.Query().Get("environment"))
			}
			_, _ = w.Write(deployments)
		case "/repos/acme/storefront/deployments/4321/statuses":
			_, _ = w.Write(wrapped.Statuses)
		case "/repos/acme/storefront/releases":
			_, _ = w.Write([]byte(`[]`))
		case "/rate_limit":
			_, _ = w.Write([]byte(`{"resources":{"core":{"limit":5000,"remaining":4990,"used":10}}}`))
		default:
			t.Errorf("unexpected read %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})

	// The recording's instants: the reads at 14:05, the poll marker at 14:30.
	poller, err := github.NewPoller(github.PollerOptions{
		Reader: c, Environments: []string{"production"}, Once: true,
		Now: (&steppingClock{instants: []time.Time{fixtureFirst, fixturePoll}}).Now,
	})
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	f, err := github.New(github.Options{OrgSlug: twinOrg, Map: storefrontOptions()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	dir := t.TempDir()
	clock := &arrivalClock{base: fixtureStart}
	memory := emit.NewMemoryEmitter(f.Describe(), emit.WithMemoryClock(clock.Now))
	events := record.Emitter(memory, dir)
	if err := f.Run(t.Context(), clock.wrap(poller), events); err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := events.Err(); err != nil {
		t.Fatalf("record events: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatalf("read the live events: %v", err)
	}
	want, err := os.ReadFile(filepath.Join(repoRoot(t), "fixtures", "github-deployment-01", "events.jsonl"))
	if err != nil {
		t.Fatalf("read the recorded events: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("the live poll's events differ from the recording's\n--- live\n%s\n--- recorded\n%s", got, want)
	}
	if got, want := stand.paths(), []string{
		"/rate_limit", "/installation/repositories", "/repos/acme/storefront/actions/runs",
		"/repos/acme/storefront/deployments", "/repos/acme/storefront/deployments/4321/statuses",
		"/repos/acme/storefront/releases",
	}; !slices.Equal(got, want) {
		t.Errorf("the cycle read %v, want the quota reading and then one repository's window: %v", got, want)
	}
}

// fakeReader is an in-memory GitHub, for the cycle's own rules.
type fakeReader struct {
	repos       []github.Repository
	deployments map[int64]github.Deployment
	listed      []int64 // what the deployments list returns
	statuses    map[int64][]github.DeploymentStatus
	failRuns    error
	sinceAsked  []time.Time
	reread      []int64
}

func (r *fakeReader) InstallationRepositories(context.Context, github.ListWindow) (github.Scope, error) {
	return github.Scope{Selection: "selected", Repositories: r.repos, Total: len(r.repos)}, nil
}

func (r *fakeReader) Deployments(context.Context, github.Repo, string, github.ListWindow) ([]github.Deployment, error) {
	var out []github.Deployment
	for _, id := range r.listed {
		out = append(out, r.deployments[id])
	}
	return out, nil
}

func (r *fakeReader) Deployment(_ context.Context, _ github.Repo, id int64) (github.Deployment, error) {
	r.reread = append(r.reread, id)
	return r.deployments[id], nil
}

func (r *fakeReader) DeploymentStatuses(_ context.Context, _ github.Repo, id int64, _ github.ListWindow) ([]github.DeploymentStatus, error) {
	return r.statuses[id], nil
}

func (r *fakeReader) WorkflowRuns(_ context.Context, _ github.Repo, w github.ListWindow) ([]github.WorkflowRun, error) {
	r.sinceAsked = append(r.sinceAsked, w.Since)
	return nil, r.failRuns
}

func (r *fakeReader) WorkflowRun(context.Context, github.Repo, int64) (github.WorkflowRun, error) {
	return github.WorkflowRun{}, errors.New("not used")
}

func (r *fakeReader) Releases(context.Context, github.Repo, github.ListWindow) ([]github.Release, error) {
	return nil, nil
}

func (r *fakeReader) Release(context.Context, github.Repo, int64) (github.Release, error) {
	return github.Release{}, errors.New("not used")
}

func (r *fakeReader) RateLimit(context.Context) ([]feeder.Reading, error) { return nil, nil }

var liveAt = time.Date(2026, 3, 1, 14, 5, 0, 0, time.UTC)

func storefrontReader() *fakeReader {
	return &fakeReader{
		repos: []github.Repository{{ID: 555, FullName: "acme/storefront", Owner: "acme", Name: "storefront"}},
		deployments: map[int64]github.Deployment{4321: {
			ID: 4321, SHA: twinSHA, Environment: "production", CreatedAt: liveAt.Add(-2 * time.Minute),
			UpdatedAt: liveAt.Add(-time.Minute),
			URL:       "https://api.github.com/repos/acme/storefront/deployments/4321",
		}},
		listed:   []int64{4321},
		statuses: map[int64][]github.DeploymentStatus{},
	}
}

// cyclePayloads drains one cycle.
func cyclePayloads(t *testing.T, p *github.Poller) []feeder.Payload {
	t.Helper()
	var out []feeder.Payload
	for {
		payload, err := p.Next(t.Context())
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		out = append(out, payload)
		if payload.Kind == github.PayloadPollMarker {
			return out
		}
	}
}

func kinds(payloads []feeder.Payload) []string {
	out := make([]string, 0, len(payloads))
	for _, p := range payloads {
		out = append(out, p.Kind)
	}
	return out
}

func newPoller(t *testing.T, r github.Reader, now func() time.Time) *github.Poller {
	t.Helper()
	p, err := github.NewPoller(github.PollerOptions{
		Reader: r, Environments: []string{"production"}, Now: now,
		Wait: func(context.Context, time.Duration) error { return nil },
	})
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	return p
}

// A rollout that was in progress when one cycle read it and succeeded before the next is found by the
// next, even once the deployment list no longer returns it: the poller follows what it saw unfinished.
func TestAnUnfinishedDeploymentIsReadAgainUntilItSettles(t *testing.T) {
	r := storefrontReader()
	r.statuses[4321] = []github.DeploymentStatus{{ID: 1, State: "in_progress", CreatedAt: liveAt.Add(-time.Minute)}}
	now := liveAt
	p := newPoller(t, r, func() time.Time { return now })

	first := cyclePayloads(t, p)
	if !slices.Contains(kinds(first), github.PayloadDeploymentStatuses) {
		t.Fatalf("the first cycle read no statuses: %v", kinds(first))
	}

	// The next cycle: the list has moved on, and the deployment has succeeded.
	now = liveAt.Add(github.DefaultPollInterval)
	r.listed = nil
	r.statuses[4321] = append([]github.DeploymentStatus{{ID: 2, State: "success", CreatedAt: now.Add(-time.Minute)}},
		r.statuses[4321]...)
	second := cyclePayloads(t, p)
	if !slices.Equal(r.reread, []int64{4321}) {
		t.Fatalf("re-read %v, want the unfinished deployment 4321", r.reread)
	}
	if !slices.Contains(kinds(second), github.PayloadDeployments) ||
		!slices.Contains(kinds(second), github.PayloadDeploymentStatuses) {
		t.Fatalf("the second cycle did not carry the deployment and its statuses: %v", kinds(second))
	}

	// Settled now, so a third cycle leaves it alone.
	now = now.Add(github.DefaultPollInterval)
	cyclePayloads(t, p)
	if len(r.reread) != 1 {
		t.Errorf("a settled deployment was re-read again: %v", r.reread)
	}
}

// A deployment that never finishes is followed for PendingHorizon, not forever.
func TestAnUnfinishedDeploymentIsLetGoAfterTheHorizon(t *testing.T) {
	r := storefrontReader()
	r.statuses[4321] = []github.DeploymentStatus{{ID: 1, State: "in_progress"}}
	now := liveAt
	p := newPoller(t, r, func() time.Time { return now })
	cyclePayloads(t, p)

	r.listed = nil
	now = liveAt.Add(github.PendingHorizon + time.Hour)
	cyclePayloads(t, p) // read once more, and found to be past the horizon
	now = now.Add(github.DefaultPollInterval)
	cyclePayloads(t, p)
	if len(r.reread) != 1 {
		t.Errorf("re-read %d times; a deployment past the horizon is read once more and then let go", len(r.reread))
	}
}

// A failed read makes the cycle partial, says why, and the next cycle reads the same window again.
func TestAFailedReadMakesTheCyclePartialAndHoldsTheWindow(t *testing.T) {
	r := storefrontReader()
	r.failRuns = errors.New("502 from GitHub")
	now := liveAt
	p := newPoller(t, r, func() time.Time { return now })

	first := cyclePayloads(t, p)
	var marker struct{ Outcome, Reason string }
	if err := json.Unmarshal(first[len(first)-1].Bytes, &marker); err != nil {
		t.Fatalf("decode the marker: %v", err)
	}
	if marker.Outcome != "partial" || !strings.Contains(marker.Reason, "workflow runs of acme/storefront") ||
		!strings.Contains(marker.Reason, "502") {
		t.Errorf("marker = %+v; want partial, naming the read that failed and why", marker)
	}
	// The rest of the repository was still read: one failed list is not a lost cycle.
	if !slices.Contains(kinds(first), github.PayloadDeployments) {
		t.Errorf("a failed runs read stopped the deployments read: %v", kinds(first))
	}

	now = liveAt.Add(github.DefaultPollInterval)
	r.failRuns = nil
	cyclePayloads(t, p)
	if len(r.sinceAsked) != 2 || !r.sinceAsked[0].Equal(r.sinceAsked[1]) {
		t.Errorf("windows started at %v; a partial cycle must not advance the next one's start", r.sinceAsked)
	}

	now = now.Add(github.DefaultPollInterval)
	cyclePayloads(t, p)
	if want := liveAt.Add(github.DefaultPollInterval - github.DefaultReorderingWindow); !r.sinceAsked[2].Equal(want) {
		t.Errorf("after a complete cycle the window starts at %s, want its start less the overlap (%s)",
			r.sinceAsked[2], want)
	}
}

// A credential that stops being read-only ends the run; it does not make a cycle partial.
func TestARefusedRenewalEndsTheRun(t *testing.T) {
	r := storefrontReader()
	r.failRuns = fmt.Errorf("token source: %w", github.ErrGateRefused)
	p := newPoller(t, r, func() time.Time { return liveAt })
	for {
		payload, err := p.Next(t.Context())
		if err != nil {
			if !errors.Is(err, github.ErrGateRefused) {
				t.Fatalf("Next: %v, want the gate's refusal", err)
			}
			return
		}
		if payload.Kind == github.PayloadPollMarker {
			t.Fatal("the cycle completed as if a write-capable credential were a transient failure")
		}
	}
}

// Free text never enters a payload: the payloads are re-encoded from what the transport decodes, so the
// deployer's description, its arbitrary payload and a status's description are not there to record.
func TestFreeTextNeverEntersAPayload(t *testing.T) {
	const secret = "ghp_SHOULD_NEVER_BE_RECORDED"
	c, _, _ := client(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/installation/repositories":
			_, _ = fmt.Fprint(w, `{"total_count":1,"repository_selection":"selected","repositories":[`+
				`{"id":555,"name":"storefront","full_name":"acme/storefront","owner":{"login":"acme"}}]}`)
		case "/repos/acme/storefront/deployments":
			_, _ = fmt.Fprintf(w, `[{"id":1,"sha":"%s","environment":"production","created_at":"%s",`+
				`"description":"deploying with %s","payload":{"token":"%s"},`+
				`"url":"https://api.github.com/repos/acme/storefront/deployments/1"}]`,
				twinSHA, liveAt.Format(time.RFC3339), secret, secret)
		case "/repos/acme/storefront/deployments/1/statuses":
			_, _ = fmt.Fprintf(w, `[{"id":1,"state":"success","description":"%s"}]`, secret)
		default:
			_, _ = fmt.Fprint(w, `[]`)
		}
	})
	p, err := github.NewPoller(github.PollerOptions{
		Reader: c, Environments: []string{"production"}, Once: true, Now: func() time.Time { return liveAt },
	})
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	var n int
	for {
		payload, err := p.Next(t.Context())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		n++
		if bytes.Contains(payload.Bytes, []byte(secret)) {
			t.Errorf("the %s payload carries free text the transport never decoded: %s", payload.Kind, payload.Bytes)
		}
	}
	if n < 4 {
		t.Fatalf("%d payloads; the cycle did not read the deployment at all", n)
	}
}

func TestAPollerWithNoProductionEnvironmentIsRefused(t *testing.T) {
	if _, err := github.NewPoller(github.PollerOptions{Reader: storefrontReader(), Environments: []string{" "}}); err == nil {
		t.Error("a poller that lists no environment reads nothing and reports a quiet window")
	}
}

// fakeMinter hands out tokens in order.
type fakeMinter struct {
	tokens []github.InstallationToken
	minted int
}

func (m *fakeMinter) MintInstallationToken(context.Context) (github.InstallationToken, error) {
	token := m.tokens[min(m.minted, len(m.tokens)-1)]
	m.minted++
	return token, nil
}

func renewableToken(value string, expires time.Time) github.InstallationToken {
	return github.InstallationToken{
		Token: value, ExpiresAt: expires, RepositorySelection: "selected",
		Permissions: map[string]string{"deployments": "read", "actions": "read", "metadata": "read"},
	}
}

// A long run renews its credential before it lapses, and proves every renewal read-only: a grant the
// operator widened mid-run ends the run at the next renewal rather than being used.
func TestACredentialIsRenewedAndEveryRenewalIsProved(t *testing.T) {
	now := liveAt
	widened := renewableToken("t3", liveAt.Add(3*time.Hour))
	widened.Permissions["deployments"] = "write"
	minter := &fakeMinter{tokens: []github.InstallationToken{
		renewableToken("t1", liveAt.Add(time.Hour)),
		renewableToken("t2", liveAt.Add(2*time.Hour)),
		widened,
	}}
	source := &github.RenewingToken{Minter: minter, Now: func() time.Time { return now }}

	if _, err := source.Prove(t.Context()); err != nil {
		t.Fatalf("Prove: %v", err)
	}
	if token, err := source.Token(t.Context()); err != nil || token != "t1" || minter.minted != 1 {
		t.Fatalf("Token = %q, %v after %d mint(s); the proved startup token should be reused", token, err, minter.minted)
	}

	now = liveAt.Add(time.Hour - github.RenewalMargin + time.Second)
	if token, err := source.Token(t.Context()); err != nil || token != "t2" {
		t.Fatalf("Token = %q, %v; a token inside its renewal margin should have been renewed", token, err)
	}

	now = liveAt.Add(2*time.Hour - time.Minute)
	token, err := source.Token(t.Context())
	if !errors.Is(err, github.ErrGateRefused) || token != "" {
		t.Fatalf("Token = %q, %v; a renewal granting write must be refused as the gate refuses", token, err)
	}
	if token, err := source.Token(t.Context()); err == nil || token == "t2" {
		t.Errorf("after a refused renewal the previous token was still served (%q)", token)
	}
}
