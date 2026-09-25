// SPDX-License-Identifier: Apache-2.0

package vercel_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	vercelfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/vercel"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/emit"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/record"
)

// The live poll cycle (004 T158).

func recorded(t *testing.T, fixture, kind string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "fixtures", fixture, "payloads", kind, "000001.json"))
	if err != nil {
		t.Fatalf("read the recorded %s: %v", kind, err)
	}
	return raw
}

// FR-044, made literal: Vercel answering with vercel-config-change-01's own bodies, read by the live
// transport and the poller, produces that fixture's events byte for byte. The cycle reads the project
// by id rather than by listing, and the events do not change, because the single read describes the
// project exactly as the listing does.
func TestALiveVercelPollReproducesTheRecording(t *testing.T) {
	var listing struct {
		Projects []json.RawMessage `json:"projects"`
	}
	if err := json.Unmarshal(recorded(t, "vercel-config-change-01", "projects"), &listing); err != nil || len(listing.Projects) != 1 {
		t.Fatalf("decode the recorded projects: %v (%d)", err, len(listing.Projects))
	}
	env := recorded(t, "vercel-config-change-01", "project-env")
	deployments := recorded(t, "vercel-config-change-01", "deployments")

	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/v9/projects/" + fixtureProject:
			_, _ = w.Write(listing.Projects[0])
		case "/v9/projects/" + fixtureProject + "/env":
			_, _ = w.Write(env)
		case "/v7/deployments":
			if r.URL.Query().Get("target") != "production" || r.URL.Query().Get("projectId") != fixtureProject {
				t.Errorf("deployments listed with %s; the platform should filter to the project's production", r.URL.RawQuery)
			}
			_, _ = w.Write(deployments)
		default:
			t.Errorf("unexpected read %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	client, err := vercelfeeder.NewClient(vercelfeeder.ClientOptions{BaseURL: srv.URL, Issuer: issuer(t), HTTP: srv.Client()})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	poller, err := vercelfeeder.NewPoller(vercelfeeder.PollerOptions{
		Reader: client, Projects: []string{fixtureProject}, Once: true,
		Now: func() time.Time { return fixtureStart },
	})
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	f, err := vercelfeeder.New(vercelfeeder.Options{
		OrgSlug: fixtureOrg,
		Targets: map[string]*graphv1.Ref{fixtureProject: feeder.Ref(feeder.NSK8sDeployment, "shop/storefront")},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	dir := t.TempDir()
	clock := &vercelClock{base: fixtureStart}
	memory := emit.NewMemoryEmitter(f.Describe(), emit.WithMemoryClock(clock.Now))
	events := record.Emitter(memory, dir)
	if err := f.Run(t.Context(), clock.wrap(poller), events); err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := events.Err(); err != nil {
		t.Fatalf("record events: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	want, err := os.ReadFile(filepath.Join(repoRoot(t), "fixtures", "vercel-config-change-01", "events.jsonl"))
	if err != nil {
		t.Fatalf("read the recorded events: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("the live poll's events differ from the recording's\n--- live\n%s\n--- recorded\n%s", got, want)
	}
	if !slices.Equal(paths, []string{
		"/v9/projects/" + fixtureProject, "/v9/projects/" + fixtureProject + "/env", "/v7/deployments",
	}) {
		t.Errorf("the cycle read %v; a mapped project is read by id, then its env, then its deployments", paths)
	}
}

// fakeVercel is an in-memory Vercel for the cycle's own rules.
type fakeVercel struct {
	project     vercelfeeder.Project
	deployments map[string]vercelfeeder.Deployment
	listed      []string
	failEnv     error
	since       []time.Time
	read        []string
}

func (v *fakeVercel) Deployments(_ context.Context, _, _ string, w vercelfeeder.ListWindow) ([]vercelfeeder.Deployment, error) {
	v.since = append(v.since, w.Since)
	var out []vercelfeeder.Deployment
	for _, uid := range v.listed {
		out = append(out, v.deployments[uid])
	}
	return out, nil
}

func (v *fakeVercel) Deployment(_ context.Context, uid string) (vercelfeeder.Deployment, error) {
	v.read = append(v.read, uid)
	return v.deployments[uid], nil
}

func (v *fakeVercel) Projects(context.Context, vercelfeeder.ListWindow) ([]vercelfeeder.Project, error) {
	return []vercelfeeder.Project{v.project}, nil
}

func (v *fakeVercel) Project(context.Context, string) (vercelfeeder.Project, error) {
	return v.project, nil
}

func (v *fakeVercel) ProjectEnv(context.Context, string) ([]vercelfeeder.ProjectEnv, error) {
	return nil, v.failEnv
}

var cycleAt = time.Date(2026, 9, 21, 14, 0, 0, 0, time.UTC)

func stagedVercel() *fakeVercel {
	return &fakeVercel{
		project: vercelfeeder.Project{ID: fixtureProject, Name: "storefront"},
		deployments: map[string]vercelfeeder.Deployment{"dpl_42": {
			UID: "dpl_42", ProjectID: fixtureProject, Target: "production", ReadyState: "READY",
			ReadySubstate: "STAGED", CreatedAt: cycleAt.Add(-5 * time.Minute).UnixMilli(),
		}},
		listed: []string{"dpl_42"},
	}
}

func vercelCycle(t *testing.T, p *vercelfeeder.Poller) []feeder.Payload {
	t.Helper()
	var out []feeder.Payload
	for {
		payload, err := p.Next(t.Context())
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		out = append(out, payload)
		if payload.Kind == vercelfeeder.PayloadPollMarker {
			return out
		}
	}
}

func newVercelPoller(t *testing.T, r vercelfeeder.Reader, now *time.Time) *vercelfeeder.Poller {
	t.Helper()
	p, err := vercelfeeder.NewPoller(vercelfeeder.PollerOptions{
		Reader: r, Projects: []string{fixtureProject}, Now: func() time.Time { return *now },
		Wait: func(context.Context, time.Duration) error { return nil },
	})
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	return p
}

func deploymentsIn(t *testing.T, payloads []feeder.Payload) []vercelfeeder.Deployment {
	t.Helper()
	var out []vercelfeeder.Deployment
	for _, p := range payloads {
		if p.Kind != vercelfeeder.PayloadDeployments {
			continue
		}
		var body struct {
			Deployments []vercelfeeder.Deployment `json:"deployments"`
		}
		if err := json.Unmarshal(p.Bytes, &body); err != nil {
			t.Fatalf("decode deployments: %v", err)
		}
		out = append(out, body.Deployments...)
	}
	return out
}

// A rollout on Vercel is a state: a deployment staged when one cycle read it and promoted before the next
// is found by the next, although the listing — windowed on creation — no longer returns it.
func TestAStagedDeploymentIsFollowedUntilItIsPromoted(t *testing.T) {
	v := stagedVercel()
	now := cycleAt
	p := newVercelPoller(t, v, &now)
	vercelCycle(t, p)

	now = cycleAt.Add(vercelfeeder.DefaultPollInterval)
	v.listed = nil
	promoted := v.deployments["dpl_42"]
	promoted.ReadySubstate = "PROMOTED"
	v.deployments["dpl_42"] = promoted
	second := deploymentsIn(t, vercelCycle(t, p))
	if len(second) != 1 || second[0].ReadySubstate != "PROMOTED" {
		t.Fatalf("the second cycle carried %+v; want the staged deployment, now promoted", second)
	}

	now = now.Add(vercelfeeder.DefaultPollInterval)
	vercelCycle(t, p)
	if !slices.Equal(v.read, []string{"dpl_42"}) {
		t.Errorf("read by uid %v; a promoted deployment is settled and is not read again", v.read)
	}
}

// An operator promoting — or rolling back to — a deployment built long before the window: the project's
// new alias request names it, and the cycle reads it.
func TestANewAliasRequestReadsTheDeploymentItNames(t *testing.T) {
	v := stagedVercel()
	v.listed = nil
	v.deployments["dpl_old"] = vercelfeeder.Deployment{
		UID: "dpl_old", ProjectID: fixtureProject, Target: "production", ReadyState: "READY",
		ReadySubstate: "PROMOTED", CreatedAt: cycleAt.Add(-7 * 24 * time.Hour).UnixMilli(),
	}
	v.project.LastAliasRequest = &vercelfeeder.AliasRequest{
		Type: "rollback", JobStatus: "succeeded", ToDeploymentID: "dpl_old", FromDeploymentID: "dpl_42",
		RequestedAt: cycleAt.Add(-time.Minute).UnixMilli(),
	}
	now := cycleAt
	p := newVercelPoller(t, v, &now)
	first := vercelCycle(t, p)
	if got := deploymentsIn(t, first); len(got) != 1 || got[0].UID != "dpl_old" {
		t.Fatalf("the cycle carried %+v; want the deployment the rollback moved production to", got)
	}
	if !slices.Contains(kindsOf(first), vercelfeeder.PayloadProject) {
		t.Errorf("no single-project payload: the rollback itself is mapped from it (%v)", kindsOf(first))
	}

	now = now.Add(vercelfeeder.DefaultPollInterval)
	vercelCycle(t, p)
	if len(v.read) != 1 {
		t.Errorf("read %v; an alias request already acted on is not acted on again", v.read)
	}
}

func kindsOf(payloads []feeder.Payload) []string {
	out := make([]string, 0, len(payloads))
	for _, p := range payloads {
		out = append(out, p.Kind)
	}
	return out
}

// A deployment's `meta` is narrowed to the keys the mapper reads: the commit message and the author's
// name and login, which a Git provider puts beside the commit, stop at the poller.
func TestADeploymentsMetaIsNarrowedToWhatTheMapperReads(t *testing.T) {
	v := stagedVercel()
	d := v.deployments["dpl_42"]
	d.Meta = map[string]string{
		"githubCommitSha":        "9f8e7d6c5b4a39281706f5e4d3c2b1a098765432",
		"githubCommitMessage":    "fix: rotate the key sk_live_SHOULD_NEVER_BE_RECORDED",
		"githubCommitAuthorName": "Ada Lovelace",
	}
	d.Attribution.CommitMeta = map[string]string{"githubCommitAuthorLogin": "ada"}
	v.deployments["dpl_42"] = d
	now := cycleAt
	for _, payload := range vercelCycle(t, newVercelPoller(t, v, &now)) {
		for _, leak := range []string{"SHOULD_NEVER_BE_RECORDED", "Ada Lovelace", "githubCommitAuthorLogin"} {
			if strings.Contains(string(payload.Bytes), leak) {
				t.Errorf("the %s payload carries %q: %s", payload.Kind, leak, payload.Bytes)
			}
		}
		if payload.Kind == vercelfeeder.PayloadDeployments && !strings.Contains(string(payload.Bytes), "9f8e7d6c5b4a") {
			t.Errorf("the commit the mapper reads was dropped too: %s", payload.Bytes)
		}
	}
}

// A failed read makes the cycle partial, and the next cycle reads the same window.
func TestAFailedVercelReadHoldsTheWindow(t *testing.T) {
	v := stagedVercel()
	v.failEnv = errors.New("503 from Vercel")
	now := cycleAt
	p := newVercelPoller(t, v, &now)
	first := vercelCycle(t, p)
	var marker struct{ Outcome, Reason string }
	_ = json.Unmarshal(first[len(first)-1].Bytes, &marker)
	if marker.Outcome != "partial" || !strings.Contains(marker.Reason, "environment metadata of "+fixtureProject) {
		t.Errorf("marker = %+v; want partial, naming the read that failed", marker)
	}
	now = cycleAt.Add(vercelfeeder.DefaultPollInterval)
	v.failEnv = nil
	vercelCycle(t, p)
	if len(v.since) != 2 || !v.since[0].Equal(v.since[1]) {
		t.Errorf("windows started at %v; a partial cycle must not advance the next one's start", v.since)
	}
}
