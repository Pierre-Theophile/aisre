// SPDX-License-Identifier: Apache-2.0

package github_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/feeders/github"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The startup read-only gate (004 T032, T033; FR-003, FR-006, FR-008).

type mintedToken struct {
	token github.InstallationToken
	err   error
}

func (m mintedToken) MintInstallationToken(context.Context) (github.InstallationToken, error) {
	return m.token, m.err
}

var tokenExpiry = time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)

func readOnlyToken() github.InstallationToken {
	return github.InstallationToken{
		Token:     "ghs_installation",
		ExpiresAt: tokenExpiry,
		Permissions: map[string]string{
			"deployments": "read", "actions": "read", "contents": "read", "metadata": "read",
		},
		RepositorySelection: "selected",
	}
}

// The operation that mints a credential is a POST, and it is NOT on the published surface. That is the
// distinction the gate rests on: the surface governs what this connector reads from the estate, and
// authenticating is not reading the estate — so the two must not converge.
func TestTheTokenMintIsNotOnTheReadOnlySurface(t *testing.T) {
	t.Parallel()
	if _, err := github.Issuable(github.OpInstallationToken); err == nil {
		t.Errorf("%q is on the published read-only surface; a surface containing a POST is not a "+
			"read-only surface, and the test asserting so is what makes FR-004's claim checkable",
			github.OpInstallationToken)
	}
	if got := github.Surface.StateChanges(); got != 0 {
		t.Errorf("StateChanges() = %d; the mint must not have leaked onto the surface", got)
	}
}

// A credential the platform reports as read-only passes, and the evidence records that it was the
// PLATFORM that said so — which is the strong form of FR-003, and the thing Vercel's gate cannot do.
func TestAPlatformReportedReadOnlyCredentialPasses(t *testing.T) {
	t.Parallel()
	got, err := github.Gate(context.Background(), mintedToken{token: readOnlyToken()})
	if err != nil {
		t.Fatalf("Gate: %v", err)
	}
	switch {
	case got.Evidence != feeder.EvidencePlatformReported:
		t.Errorf("evidence = %q, want the platform's own report; an operator reading a checkpoint must "+
			"not take an assertion for a platform statement", got.Evidence)
	case !got.Scope.PlatformEnforced:
		t.Errorf("the scope is not reported as platform-enforced; an installation's repository " +
			"selection is GitHub's boundary, and a repository outside it is unreachable rather than skipped")
	case got.Scope.Selection != "selected":
		t.Errorf("selection = %q, want GitHub's own word so the checkpoint can say which regime this is "+
			"(FR-008)", got.Scope.Selection)
	case !got.ExpiresAt.Equal(tokenExpiry):
		t.Errorf("ExpiresAt = %v, want %v so a long campaign renews rather than failing mid-cycle",
			got.ExpiresAt, tokenExpiry)
	}
	// The permissions are reported sorted, because a checkpoint two runs apart should be diffable.
	want := "actions=read,contents=read,deployments=read,metadata=read"
	if strings.Join(got.Permissions, ",") != want {
		t.Errorf("permissions = %v, want %q", got.Permissions, want)
	}
}

// A single write permission refuses the whole run, and the refusal names it. FR-003 is not "prefer a
// read-only credential".
func TestOneWritePermissionRefusesTheRun(t *testing.T) {
	t.Parallel()
	for name, value := range map[string]string{
		"write":                       "write",
		"admin":                       "admin",
		"a value nobody has ruled on": "manage",
	} {
		token := readOnlyToken()
		token.Permissions["deployments"] = value

		_, err := github.Gate(context.Background(), mintedToken{token: token})
		var writable *feeder.WriteCapableError
		if !errors.As(err, &writable) {
			t.Errorf("a credential with deployments=%s (%s) was admitted, returning %v", value, name, err)
			continue
		}
		if !strings.Contains(err.Error(), "deployments="+value) {
			t.Errorf("the refusal does not name the grant that caused it: %v", err)
		}
	}
}

// A credential that can do nothing is a misconfiguration worth failing on, not a read-only credential
// worth celebrating.
func TestACredentialWithNoPermissionsIsRefused(t *testing.T) {
	t.Parallel()
	token := readOnlyToken()
	token.Permissions = map[string]string{}
	if _, err := github.Gate(context.Background(), mintedToken{token: token}); err == nil {
		t.Error("a token reporting no permissions at all was admitted")
	}
}

// A gate with nothing to ask concludes read-only from having asked nobody, which is the one conclusion
// FR-003 forbids.
func TestAGateWithNoMinterIsRefused(t *testing.T) {
	t.Parallel()
	if _, err := github.Gate(context.Background(), nil); err == nil {
		t.Error("a gate with no way to obtain a credential returned a pass")
	}
}

// A mint that fails is a refusal and not a pass: a connector that could not authenticate has not shown
// its credential is read-only.
func TestAFailedMintIsARefusal(t *testing.T) {
	t.Parallel()
	_, err := github.Gate(context.Background(), mintedToken{err: errors.New("the app key was revoked")})
	if err == nil {
		t.Fatal("a failed mint returned a pass")
	}
	if !strings.Contains(err.Error(), "the app key was revoked") {
		t.Errorf("the refusal loses the reason: %v", err)
	}
}

// The live minter reads GitHub's answer: the permissions, the selection and the expiry.
func TestTheLiveMinterReadsWhatGitHubReports(t *testing.T) {
	t.Parallel()
	var sawMethod, sawAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawMethod, sawAuth = r.Method, r.Header.Get("Authorization")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{
			"token": "ghs_minted",
			"expires_at": "2026-03-01T10:00:00Z",
			"permissions": {"deployments": "read", "metadata": "read"},
			"repository_selection": "selected"
		}`))
	}))
	t.Cleanup(server.Close)

	minter := &github.AppMinter{
		BaseURL: server.URL, InstallationID: 42, HTTP: server.Client(),
		JWT: func(context.Context) (string, error) { return "app-assertion", nil },
	}
	got, err := minter.MintInstallationToken(context.Background())
	if err != nil {
		t.Fatalf("MintInstallationToken: %v", err)
	}
	switch {
	case sawMethod != http.MethodPost:
		t.Errorf("the mint used %s, want POST", sawMethod)
	case sawAuth != "Bearer app-assertion":
		t.Errorf("the mint sent %q, want the app assertion", sawAuth)
	case got.Token != "ghs_minted":
		t.Errorf("token = %q", got.Token)
	case got.RepositorySelection != "selected":
		t.Errorf("selection = %q", got.RepositorySelection)
	case got.Permissions["deployments"] != "read":
		t.Errorf("permissions = %v", got.Permissions)
	case !got.ExpiresAt.Equal(tokenExpiry):
		t.Errorf("ExpiresAt = %v, want %v", got.ExpiresAt, tokenExpiry)
	}

	// And the gate accepts it, so the live path and the gate agree.
	result, err := github.Gate(context.Background(), minter)
	if err != nil {
		t.Fatalf("Gate over the live minter: %v", err)
	}
	if result.Evidence != feeder.EvidencePlatformReported {
		t.Errorf("evidence = %q", result.Evidence)
	}
}

// A refused mint is reported as GitHub's status rather than as a decoding failure, so an operator sees
// whether the App is uninstalled, the key is wrong, or the platform is busy.
func TestARefusedMintReportsTheStatus(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message": "Bad credentials"}`))
	}))
	t.Cleanup(server.Close)

	minter := &github.AppMinter{
		BaseURL: server.URL, InstallationID: 42, HTTP: server.Client(),
		JWT: func(context.Context) (string, error) { return "stale", nil },
	}
	_, err := minter.MintInstallationToken(context.Background())
	var status *github.StatusError
	if !errors.As(err, &status) {
		t.Fatalf("a 401 returned %v, want StatusError", err)
	}
	if status.Status != http.StatusUnauthorized {
		t.Errorf("Status = %d, want 401", status.Status)
	}
	if status.Operation != github.OpInstallationToken {
		t.Errorf("the refusal names %q, want the mint", status.Operation)
	}
}

// A minter missing what it needs refuses before reaching the network.
func TestAnIncompleteMinterRefusesBeforeTheNetwork(t *testing.T) {
	t.Parallel()
	for name, minter := range map[string]*github.AppMinter{
		"no assertion source": {InstallationID: 42},
		"no installation":     {JWT: func(context.Context) (string, error) { return "a", nil }},
	} {
		if _, err := minter.MintInstallationToken(context.Background()); err == nil {
			t.Errorf("a minter with %s returned a token", name)
		}
	}
}
