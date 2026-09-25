// SPDX-License-Identifier: Apache-2.0

package github_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/feeders/github"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The transport seam (004 T047, FR-004, FR-021, FR-056, FR-073, FR-074).
//
// These tests are about the seam rather than the mapping: that every call passes the metered door
// before it reaches the network, that the quota family is learned from the response rather than
// guessed, that a truncated window says so, and that the decoded payload carries the fields a rollout
// is built from and none of the free text around them. The mapping itself — states, actor kinds,
// valid time — is US1's and lands with its fixtures.

var cycleStart = time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)

// server is a GitHub stand-in that records what it was asked for.
type server struct {
	t        *testing.T
	handler  func(w http.ResponseWriter, r *http.Request)
	requests []*http.Request
	waits    []time.Duration
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.requests = append(s.requests, r.Clone(r.Context()))
	s.handler(w, r)
}

// waits are the durations the client asked to sleep for, in order. Recorded rather than slept: a test
// that really waited the hour GitHub's primary limit asks for is a test nobody runs, and the duration
// asked for is the thing FR-074 is about.
func (s *server) waited() []time.Duration { return s.waits }

// queries are the raw query strings of every request, which is what tells a resumed read from a
// restarted one: both fetch the same paths, and only a restart fetches `page=1` twice.
func (s *server) queries() []string {
	var out []string
	for _, r := range s.requests {
		out = append(out, r.URL.RawQuery)
	}
	return out
}

func (s *server) paths() []string {
	var out []string
	for _, r := range s.requests {
		out = append(out, r.URL.Path)
	}
	return out
}

// client wires a live client to a stand-in, with a budget generous enough that the quota is not the
// thing under test unless a case makes it so.
func client(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) (*github.Client, *server, *feeder.Issuer) {
	t.Helper()
	stand := &server{t: t, handler: handler}
	httpServer := httptest.NewServer(stand)
	t.Cleanup(httpServer.Close)

	// A static allowance, because the cycle's first call happens before GitHub has said what is left:
	// see FamilyUnobserved. Without one the connector cannot open its cycle, which is the intended
	// (and loud) failure rather than an unbounded first call.
	budget, err := feeder.NewQuotaBudget(github.Platform, feeder.QuotaPolicy{
		Share: 1, Reserve: 0, StaticAllowance: 50,
	})
	if err != nil {
		t.Fatalf("NewQuotaBudget: %v", err)
	}
	issuer, err := feeder.NewIssuer(github.Surface, budget, &feeder.AreaStats{})
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	c, err := github.NewClient(httpServer.URL, httpServer.Client(), issuer,
		func(context.Context) (string, error) { return "test-token", nil },
		func() time.Time { return cycleStart },
		func(_ context.Context, d time.Duration) error {
			stand.waits = append(stand.waits, d)
			return nil
		})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c, stand, issuer
}

func issuedFor(log []feeder.RequestRecord, op feeder.ReadOperation) int {
	for _, row := range log {
		if row.Operation == op {
			return row.Issued
		}
	}
	return 0
}

// A client that could make a call without an issuer would be a second door, and the whole point of the
// first is that there is only one.
func TestAClientWithoutAnIssuerIsRefused(t *testing.T) {
	t.Parallel()
	_, err := github.NewClient("", nil, nil,
		func(context.Context) (string, error) { return "", nil }, nil, nil)
	if err == nil {
		t.Error("a transport was built with no issuer; it could spend a call the published surface " +
			"never admitted")
	}
	issuer, err := feeder.NewIssuer(github.Surface, nil, nil)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	if _, err := github.NewClient("", nil, issuer, nil, nil, nil); err == nil {
		t.Error("a transport was built with no token source")
	}
}

// The door comes before the network. A budget that refuses means no request is made at all — not one
// made and discarded, which would have spent somebody else's allowance to learn nothing.
func TestARefusedCallNeverReachesThePlatform(t *testing.T) {
	t.Parallel()
	stand := &server{t: t, handler: func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[]`))
	}}
	httpServer := httptest.NewServer(stand)
	t.Cleanup(httpServer.Close)

	// No static allowance and no reading: a budget with no numbers is not an unlimited budget.
	budget, err := feeder.NewQuotaBudget(github.Platform, feeder.QuotaPolicy{Share: 0.5})
	if err != nil {
		t.Fatalf("NewQuotaBudget: %v", err)
	}
	issuer, err := feeder.NewIssuer(github.Surface, budget, &feeder.AreaStats{})
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	c, err := github.NewClient(httpServer.URL, httpServer.Client(), issuer,
		func(context.Context) (string, error) { return "t", nil },
		func() time.Time { return cycleStart }, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	_, err = c.Deployments(context.Background(), github.Repo{Owner: "acme", Name: "storefront"}, "", github.ListWindow{})
	if _, yielded := feeder.YieldedForQuota(err); !yielded {
		t.Fatalf("Deployments against a budget with nothing in it returned %v, want a quota yield", err)
	}
	if len(stand.requests) != 0 {
		t.Errorf("the platform was asked %v despite the budget refusing; the door has to come before "+
			"the network or a refusal costs a call", stand.paths())
	}
}

// The credential travels in the Authorization header and nowhere else. FR-014 requires the origin link
// carry no credential, and a token that ever reached a query string would be in every access log
// between here and GitHub.
func TestTheCredentialIsAHeaderAndNeverAQueryParameter(t *testing.T) {
	t.Parallel()
	c, stand, _ := client(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	})
	if _, err := c.Releases(context.Background(), github.Repo{Owner: "acme", Name: "storefront"}, github.ListWindow{}); err != nil {
		t.Fatalf("Releases: %v", err)
	}
	if len(stand.requests) != 1 {
		t.Fatalf("the platform saw %d request(s), want 1", len(stand.requests))
	}
	got := stand.requests[0]
	if want := "Bearer test-token"; got.Header.Get("Authorization") != want {
		t.Errorf("Authorization = %q, want %q", got.Header.Get("Authorization"), want)
	}
	if raw := got.URL.RawQuery; strings.Contains(raw, "test-token") {
		t.Errorf("the credential is in the query string: %q", raw)
	}
	if got.Header.Get("X-GitHub-Api-Version") == "" {
		t.Error("the request pins no API version, so GitHub is free to change the payload shape under a " +
			"recorded fixture")
	}
}

// The path comes from the published operation, so a call site names an operation and the concrete URL
// is derived from it rather than the two being written out separately and drifting.
func TestThePathIsDerivedFromThePublishedOperation(t *testing.T) {
	t.Parallel()
	c, stand, _ := client(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/statuses") || strings.HasSuffix(r.URL.Path, "/deployments") ||
			strings.HasSuffix(r.URL.Path, "/releases") || strings.HasSuffix(r.URL.Path, "/runs") {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	})
	repo := github.Repo{Owner: "acme", Name: "storefront"}
	ctx := context.Background()
	if _, err := c.Deployment(ctx, repo, 4321); err != nil {
		t.Fatalf("Deployment: %v", err)
	}
	if _, err := c.DeploymentStatuses(ctx, repo, 4321, github.ListWindow{}); err != nil {
		t.Fatalf("DeploymentStatuses: %v", err)
	}
	if _, err := c.WorkflowRun(ctx, repo, 99); err != nil {
		t.Fatalf("WorkflowRun: %v", err)
	}
	if _, err := c.Release(ctx, repo, 7); err != nil {
		t.Fatalf("Release: %v", err)
	}
	want := []string{
		"/repos/acme/storefront/deployments/4321",
		"/repos/acme/storefront/deployments/4321/statuses",
		"/repos/acme/storefront/actions/runs/99",
		"/repos/acme/storefront/releases/7",
	}
	got := stand.paths()
	if len(got) != len(want) {
		t.Fatalf("the platform saw %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("request %d was %q, want %q", i, got[i], want[i])
		}
	}
}

// The quota family is learned from the response. The first call is metered against `unobserved`
// because nothing has said otherwise yet; the second is metered against what GitHub reported.
func TestTheQuotaFamilyIsLearnedFromTheResponse(t *testing.T) {
	t.Parallel()
	c, _, issuer := client(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("x-ratelimit-limit", "5000")
		w.Header().Set("x-ratelimit-remaining", "4998")
		w.Header().Set("x-ratelimit-used", "2")
		w.Header().Set("x-ratelimit-reset", "1772326800")
		w.Header().Set("x-ratelimit-resource", "core")
		_, _ = w.Write([]byte(`[]`))
	})
	repo := github.Repo{Owner: "acme", Name: "storefront"}
	for range 2 {
		if _, err := c.Releases(context.Background(), repo, github.ListWindow{}); err != nil {
			t.Fatalf("Releases: %v", err)
		}
	}

	families := map[string]int{}
	for _, family := range issuer.Report().Families {
		families[family.Family] = family.Spent
	}
	if families[github.FamilyUnobserved] != 1 {
		t.Errorf("%d call(s) were metered against %q, want exactly the cycle's first; the family has to "+
			"be learned from the response rather than stay provisional",
			families[github.FamilyUnobserved], github.FamilyUnobserved)
	}
	if families["core"] != 1 {
		t.Errorf("%d call(s) were metered against `core`, want 1; GitHub named the family in "+
			"x-ratelimit-resource and the second call should have been metered against it", families["core"])
	}
}

// Every page is a separate trip through the door. A cycle that paged freely once admitted would meter
// one call and make twenty.
func TestEveryPageIsMeteredSeparately(t *testing.T) {
	t.Parallel()
	var c *github.Client
	stand := 0
	c, _, issuer := client(t, func(w http.ResponseWriter, r *http.Request) {
		stand++
		if stand < 3 {
			w.Header().Set("Link", fmt.Sprintf(`<%s://%s%s?page=%d>; rel="next"`,
				"http", r.Host, r.URL.Path, stand+1))
		}
		_, _ = fmt.Fprintf(w, `[{"id": %d, "tag_name": "v%d"}]`, stand, stand)
	})
	releases, err := c.Releases(context.Background(), github.Repo{Owner: "acme", Name: "storefront"},
		github.ListWindow{})
	if err != nil {
		t.Fatalf("Releases across three pages: %v", err)
	}
	if len(releases) != 3 {
		t.Fatalf("three pages of one release each yielded %d releases", len(releases))
	}
	if got := issuedFor(issuer.RequestLog(), "GET /repos/{owner}/{repo}/releases"); got != 3 {
		t.Errorf("the request log records %d issued call(s) for three pages, want 3", got)
	}
}

// The page cap reports a partial window rather than returning a short list as if it were the whole
// thing. FR-056 wants the gap declared; a truncation nobody declared is the silent version.
func TestThePageCapReportsAPartialWindowAndKeepsWhatItRead(t *testing.T) {
	t.Parallel()
	c, _, _ := client(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?page=next>; rel="next"`, r.Host, r.URL.Path))
		_, _ = w.Write([]byte(`[{"id": 1, "tag_name": "v1"}]`))
	})
	releases, err := c.Releases(context.Background(), github.Repo{Owner: "acme", Name: "storefront"},
		github.ListWindow{MaxPages: 2})
	var partial *github.PartialListError
	if !errors.As(err, &partial) {
		t.Fatalf("a list that never ran out of pages returned %v, want PartialListError", err)
	}
	if !errors.Is(err, github.ErrPageLimitReached) {
		t.Errorf("the cause is %v, want ErrPageLimitReached; the cap is the operator's choice and "+
			"reaching it is not a statement about GitHub", err)
	}
	if partial.Pages != 2 {
		t.Errorf("the refusal reports %d page(s) read, want 2", partial.Pages)
	}
	if len(releases) != 2 {
		t.Errorf("the two pages already read yielded %d releases, want 2; the items are true and a "+
			"caller that dropped them would turn a declared gap into a bigger undeclared one", len(releases))
	}
}

// A budget that runs out mid-pagination is the same shape, and its cause is a quota yield rather than
// the cap — which is FR-073's distinction at the transport level.
func TestAQuotaYieldMidPaginationIsDistinguishableFromTheCap(t *testing.T) {
	t.Parallel()
	stand := &server{t: nil, handler: func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?page=next>; rel="next"`, r.Host, r.URL.Path))
		_, _ = w.Write([]byte(`[{"id": 1, "tag_name": "v1"}]`))
	}}
	httpServer := httptest.NewServer(stand)
	t.Cleanup(httpServer.Close)

	// Two calls and then nothing: the static allowance is the whole budget here.
	budget, err := feeder.NewQuotaBudget(github.Platform, feeder.QuotaPolicy{Share: 1, StaticAllowance: 2})
	if err != nil {
		t.Fatalf("NewQuotaBudget: %v", err)
	}
	issuer, err := feeder.NewIssuer(github.Surface, budget, &feeder.AreaStats{})
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	c, err := github.NewClient(httpServer.URL, httpServer.Client(), issuer,
		func(context.Context) (string, error) { return "t", nil },
		func() time.Time { return cycleStart }, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	releases, err := c.Releases(context.Background(), github.Repo{Owner: "acme", Name: "storefront"},
		github.ListWindow{MaxPages: 50})
	var partial *github.PartialListError
	if !errors.As(err, &partial) {
		t.Fatalf("a list stopped by the budget returned %v, want PartialListError", err)
	}
	if _, yielded := feeder.YieldedForQuota(err); !yielded {
		t.Errorf("the cause is %v, want a quota yield; stopping for quota established nothing about "+
			"the window that was not read, and reporting it as the cap would say the operator chose it", err)
	}
	if errors.Is(err, github.ErrPageLimitReached) {
		t.Error("a budget yield was reported as the page cap")
	}
	if len(releases) != 2 {
		t.Errorf("the pages read before the yield yielded %d releases, want 2", len(releases))
	}
}

// A refusal carries the platform's own wait instruction, and a 403 is only a rate limit where the
// response says so. A missing permission is a scope the operator has to grant, and waiting never
// fixes it.
func TestARefusalTellsAWaitApartFromAMissingPermission(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		status      int
		headers     map[string]string
		wantLimited bool
		wantWait    time.Duration
	}{
		"429 is unambiguous": {
			status: http.StatusTooManyRequests, wantLimited: true,
		},
		"429 with a wait": {
			status: http.StatusTooManyRequests, headers: map[string]string{"Retry-After": "60"},
			wantLimited: true, wantWait: time.Minute,
		},
		"403 asking to be retried is the secondary limit": {
			status: http.StatusForbidden, headers: map[string]string{"Retry-After": "30"},
			wantLimited: true, wantWait: 30 * time.Second,
		},
		"403 reporting nothing left is the primary limit": {
			status: http.StatusForbidden,
			headers: map[string]string{
				"x-ratelimit-limit": "5000", "x-ratelimit-remaining": "0", "x-ratelimit-resource": "core",
			},
			wantLimited: true,
		},
		"403 with quota to spare is a permission this credential does not have": {
			status: http.StatusForbidden,
			headers: map[string]string{
				"x-ratelimit-limit": "5000", "x-ratelimit-remaining": "4999", "x-ratelimit-resource": "core",
			},
			wantLimited: false,
		},
		"404 is neither": {status: http.StatusNotFound, wantLimited: false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c, _, _ := client(t, func(w http.ResponseWriter, _ *http.Request) {
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"message": "nope"}`))
			})
			_, err := c.Deployment(context.Background(), github.Repo{Owner: "acme", Name: "storefront"}, 1)
			var status *github.StatusError
			if !errors.As(err, &status) {
				t.Fatalf("a %d returned %v, want StatusError", tc.status, err)
			}
			if status.Status != tc.status {
				t.Errorf("Status = %d, want %d", status.Status, tc.status)
			}
			if status.RateLimited() != tc.wantLimited {
				t.Errorf("RateLimited() = %v, want %v", status.RateLimited(), tc.wantLimited)
			}
			if status.RetryAfter != tc.wantWait {
				t.Errorf("RetryAfter = %v, want %v", status.RetryAfter, tc.wantWait)
			}
		})
	}
}

// The rate-limit headers on a refusal are still observed. A 403 for a spent allowance is the response
// whose numbers matter most, and dropping them would leave the budget believing the last good reading.
func TestARefusalsQuotaHeadersAreStillObserved(t *testing.T) {
	t.Parallel()
	c, _, issuer := client(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("x-ratelimit-limit", "5000")
		w.Header().Set("x-ratelimit-remaining", "0")
		w.Header().Set("x-ratelimit-reset", "1772326800")
		w.Header().Set("x-ratelimit-resource", "core")
		w.WriteHeader(http.StatusForbidden)
	})
	_, _ = c.Deployment(context.Background(), github.Repo{Owner: "acme", Name: "storefront"}, 1)

	var found bool
	for _, family := range issuer.Report().Families {
		if family.Family != "core" {
			continue
		}
		found = true
		if family.Remaining != 0 {
			t.Errorf("the budget reports %d remaining on `core` after a refusal that said 0",
				family.Remaining)
		}
	}
	if !found {
		t.Error("the refusal's rate-limit headers were dropped; the budget would keep spending against " +
			"the last reading that happened to succeed")
	}
}

// The cycle's opening reading covers every family GitHub reports, not only the one the request itself
// counted against — a cycle that learned about `core` alone would run blind against every other bucket.
func TestTheOpeningReadingCoversEveryFamilySorted(t *testing.T) {
	t.Parallel()
	c, _, issuer := client(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"resources": {
			"search": {"limit": 30, "remaining": 30, "used": 0, "reset": 1772326800},
			"core":   {"limit": 5000, "remaining": 4987, "used": 13, "reset": 1772326800},
			"graphql":{"limit": 5000, "remaining": 5000, "used": 0, "reset": 1772326800}
		}}`))
	})
	readings, err := c.RateLimit(context.Background())
	if err != nil {
		t.Fatalf("RateLimit: %v", err)
	}
	var names []string
	for _, r := range readings {
		names = append(names, r.Family)
		if r.Source != feeder.QuotaReported {
			t.Errorf("%s was reported with source %q, want the platform's", r.Family, r.Source)
		}
		if r.Reset.IsZero() {
			t.Errorf("%s carries no reset instant, so the budget cannot tell one window from the next",
				r.Family)
		}
	}
	want := []string{"core", "graphql", "search"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("RateLimit returned %v, want %v — sorted, because a JSON object's key order is not a "+
			"thing and a usage report that reorders between runs cannot be diffed", names, want)
	}
	// And the budget has them, so the next call against `search` is paced by `search`'s numbers.
	var families []string
	for _, family := range issuer.Report().Families {
		families = append(families, family.Family)
	}
	for _, family := range want {
		if !strings.Contains(strings.Join(families, ","), family) {
			t.Errorf("the budget has no reading for %q after the opening call; it reports %v",
				family, families)
		}
	}
}

// The decoded payload carries the fields a rollout is built from and none of the free text around
// them. This is the narrowing doc.go promises, asserted rather than remembered: the deployment's
// description and payload are where people paste tokens, and neither has a field to land in.
func TestTheFreeTextFieldsHaveNowhereToLand(t *testing.T) {
	t.Parallel()
	const planted = "ghp_thisWouldBeACredentialSomebodyPasted"
	c, _, _ := client(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
			"id": 4321,
			"sha": "8f5bd3c0b0e4a1d2c3f4a5b6c7d8e9f0a1b2c3d4",
			"ref": "main",
			"task": "deploy",
			"environment": "production",
			"production_environment": true,
			"transient_environment": false,
			"created_at": "2026-03-01T09:05:00Z",
			"updated_at": "2026-03-01T09:07:00Z",
			"description": "` + planted + `",
			"payload": {"note": "` + planted + `"},
			"creator": {"login": "release-bot", "id": 77, "type": "Bot"},
			"statuses_url": "https://api.github.com/repos/acme/storefront/deployments/4321/statuses",
			"url": "https://api.github.com/repos/acme/storefront/deployments/4321"
		}`))
	})
	got, err := c.Deployment(context.Background(), github.Repo{Owner: "acme", Name: "storefront"}, 4321)
	if err != nil {
		t.Fatalf("Deployment: %v", err)
	}
	if rendered := fmt.Sprintf("%+v", got); strings.Contains(rendered, planted) {
		t.Errorf("the decoded deployment carries the payload's free text: %s\n\nA field for it is one "+
			"struct assignment away from the graph, and doc.go's promise not to read it would be kept "+
			"by remembering rather than by construction", rendered)
	}
	// And the fields a rollout IS built from are all there, so the assertion above is not "nothing was
	// decoded".
	switch {
	case got.ID != 4321:
		t.Errorf("ID = %d, want 4321", got.ID)
	case got.SHA != "8f5bd3c0b0e4a1d2c3f4a5b6c7d8e9f0a1b2c3d4":
		t.Errorf("SHA = %q", got.SHA)
	case got.Environment != "production":
		t.Errorf("Environment = %q, want production", got.Environment)
	case got.Creator.Type != "Bot":
		t.Errorf("Creator.Type = %q, want GitHub's own typing; FR-027 derives actor kind from it and "+
			"never from the login (SC-005)", got.Creator.Type)
	case got.ProductionEnvironment == nil || !*got.ProductionEnvironment:
		t.Errorf("ProductionEnvironment = %v, want true stated", got.ProductionEnvironment)
	case got.Transient == nil || *got.Transient:
		t.Errorf("Transient = %v, want false stated", got.Transient)
	case !got.CreatedAt.Equal(time.Date(2026, 3, 1, 9, 5, 0, 0, time.UTC)):
		t.Errorf("CreatedAt = %v", got.CreatedAt)
	}
}

// A flag GitHub did not state is absent, not false. `production_environment` missing and
// `production_environment: false` are different facts, and FR-023's allowlist is the operator's
// either way.
func TestAnUnstatedFlagIsAbsentRatherThanFalse(t *testing.T) {
	t.Parallel()
	c, _, _ := client(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id": 1, "environment": "staging"}`))
	})
	got, err := c.Deployment(context.Background(), github.Repo{Owner: "acme", Name: "storefront"}, 1)
	if err != nil {
		t.Fatalf("Deployment: %v", err)
	}
	if got.ProductionEnvironment != nil {
		t.Errorf("ProductionEnvironment = %v for a payload that stated none; absent and false are "+
			"different facts", *got.ProductionEnvironment)
	}
}

// The grant is enumerated, and its selection is reported rather than interpreted (FR-008).
func TestTheGrantIsEnumeratedAndItsSelectionReported(t *testing.T) {
	t.Parallel()
	page := 0
	c, _, _ := client(t, func(w http.ResponseWriter, r *http.Request) {
		page++
		if page == 1 {
			w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?page=2>; rel="next"`, r.Host, r.URL.Path))
			_, _ = w.Write([]byte(`{"total_count": 2, "repository_selection": "selected",
				"repositories": [{"id": 1, "name": "storefront", "full_name": "acme/storefront",
				"private": true, "owner": {"login": "acme"}}]}`))
			return
		}
		// The second page says something different on purpose: if the grant changed while the cycle
		// was reading it, the answer is the one the read started from, not whichever page came last.
		_, _ = w.Write([]byte(`{"total_count": 9, "repository_selection": "all",
			"repositories": [{"id": 2, "name": "checkout", "full_name": "acme/checkout",
			"private": false, "owner": {"login": "acme"}}]}`))
	})
	scope, err := c.InstallationRepositories(context.Background(), github.ListWindow{})
	if err != nil {
		t.Fatalf("InstallationRepositories: %v", err)
	}
	switch {
	case scope.Selection != "selected":
		t.Errorf("Selection = %q, want the selection the read STARTED from; the checkpoint has to be "+
			"able to say which regime the feeder ran under, and a grant that changed mid-read would "+
			"otherwise be reported as if it had been that way all along", scope.Selection)
	case scope.Total != 2:
		t.Errorf("Total = %d, want GitHub's own total_count so a short read is detectable", scope.Total)
	case len(scope.Repositories) != 2:
		t.Fatalf("the grant enumerated %d repositories across two pages, want 2", len(scope.Repositories))
	case scope.Repositories[0].Repo().String() != "acme/storefront":
		t.Errorf("the first repository is %q", scope.Repositories[0].Repo())
	case scope.Repositories[1].Repo().String() != "acme/checkout":
		t.Errorf("the second repository is %q", scope.Repositories[1].Repo())
	}
}

// A workflow run's list is GitHub's envelope rather than a bare array, and the window's lower bound is
// sent while no upper bound is — an upper bound would cut off a run that completed while the cycle was
// reading.
func TestWorkflowRunsSendOnlyALowerBound(t *testing.T) {
	t.Parallel()
	c, stand, _ := client(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"total_count": 1, "workflow_runs": [
			{"id": 55, "name": "deploy", "run_number": 12, "run_attempt": 2,
			 "head_sha": "abc", "event": "schedule", "status": "completed", "conclusion": "success",
			 "run_started_at": "2026-03-01T09:00:00Z",
			 "actor": {"login": "a", "id": 1, "type": "User"},
			 "triggering_actor": {"login": "b", "id": 2, "type": "User"}}]}`))
	})
	since := cycleStart.Add(-2 * time.Hour)
	runs, err := c.WorkflowRuns(context.Background(), github.Repo{Owner: "acme", Name: "storefront"},
		github.ListWindow{Since: since})
	if err != nil {
		t.Fatalf("WorkflowRuns: %v", err)
	}
	if len(runs) != 1 || runs[0].ID != 55 {
		t.Fatalf("WorkflowRuns returned %+v, want the one run in the envelope", runs)
	}
	if runs[0].RunAttempt != 2 || runs[0].TriggeringActor.Login != "b" {
		t.Errorf("the attempt and triggering actor are %d/%q; a re-run's new change belongs to whoever "+
			"asked for THIS attempt (FR-028)", runs[0].RunAttempt, runs[0].TriggeringActor.Login)
	}
	query := stand.requests[0].URL.Query()
	if got, want := query.Get("created"), ">="+since.Format(time.RFC3339); got != want {
		t.Errorf("created = %q, want %q", got, want)
	}
	if query.Has("created_before") || strings.Contains(query.Get("created"), "..") {
		t.Errorf("the window sent an upper bound (%q); a run completing while the cycle reads would be "+
			"cut off", query.Get("created"))
	}
}

// A dead context stops before the network, and it is not recorded as this connector's behaviour.
func TestADeadContextStopsBeforeTheNetwork(t *testing.T) {
	t.Parallel()
	c, stand, issuer := client(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Releases(ctx, github.Repo{Owner: "acme", Name: "storefront"}, github.ListWindow{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled Releases returned %v, want context.Canceled", err)
	}
	if len(stand.requests) != 0 {
		t.Errorf("the platform was asked %v after the context died", stand.paths())
	}
	if len(issuer.RequestLog()) != 0 {
		t.Errorf("the request log holds %+v for a call the context killed", issuer.RequestLog())
	}
}

// The `Link` cursor is GitHub's, followed as given. A page number would re-slice a collection that
// changes while it is being read.
func TestThePaginationCursorIsFollowedAsGiven(t *testing.T) {
	t.Parallel()
	c, stand, _ := client(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("after") == "" {
			// `next` is deliberately NOT the first link. GitHub emits prev, next, last and first in
			// whatever order suits the page, and a reader that took the first link it saw would work
			// on the pages where next happens to lead and silently jump to the end on the rest.
			w.Header().Set("Link", fmt.Sprintf(
				`<http://%s%s?after=last>; rel="last", <http://%s%s?after=cursor-42>; rel="next"`,
				r.Host, r.URL.Path, r.Host, r.URL.Path))
			_, _ = w.Write([]byte(`[{"id": 1, "tag_name": "v1"}]`))
			return
		}
		_, _ = w.Write([]byte(`[{"id": 2, "tag_name": "v2"}]`))
	})
	if _, err := c.Releases(context.Background(), github.Repo{Owner: "acme", Name: "storefront"},
		github.ListWindow{}); err != nil {
		t.Fatalf("Releases: %v", err)
	}
	if len(stand.requests) != 2 {
		t.Fatalf("the platform saw %d request(s), want 2", len(stand.requests))
	}
	if got := stand.requests[1].URL.Query().Get("after"); got != "cursor-42" {
		t.Errorf("the second request used after=%q, want the cursor GitHub gave (`cursor-42`) rather "+
			"than a page number this code invented", got)
	}
}
