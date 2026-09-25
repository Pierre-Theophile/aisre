// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The startup read-only gate (004 T032, T033; FR-003, FR-006, FR-008).
//
// ---------------------------------------------------------------------------------------------
// Why minting the token is a POST, and why that is not a hole in the read-only surface
//
// A GitHub App gets its credential by POSTing to `/app/installations/{id}/access_tokens`. The
// published read-only surface refuses every method but GET and HEAD, so that call is not on it and
// **must never be**: a surface containing a POST would not be a read-only surface, and the test that
// asserts so is the one thing making FR-004's claim checkable.
//
// The resolution is not an exception, it is a distinction. [Surface] governs what this connector reads
// **from the estate it observes**. Minting a token is not reading the estate: it is how the connector
// authenticates in the first place, it changes nothing an operator runs, and its result is a
// credential rather than an observation. Constitution VII forbids a write to any production system;
// obtaining a token writes to no system anybody deploys.
//
// So the mint lives here, in the credential layer, behind an interface — and a test asserts the
// operation is absent from the surface, so the two cannot quietly converge.
//
// # Why this gate is stronger than Vercel's, and says so
//
// The token response carries a granular `permissions` object and a `repository_selection`. Both are
// **the platform's own statement**, checked on every cycle, which is what FR-003's strong form asks
// for. Vercel reports nothing about write capability, so its gate rests on an operator's assertion
// (contracts/read-only-operations.md §3.1). A checkpoint that presented the two alike would overstate
// one of them, which is why the evidence is typed rather than a boolean.
//
// # Why the dry run costs no area call
//
// Everything the gate reports — the permissions and the repository selection — is on the token
// response, which the connector has to obtain anyway. So `--dry-run` answers "is this credential
// read-only, and what is it scoped to" without issuing one operation from [Surface], which is what
// makes the GCP feeder's promise about its own dry run true here too: a dry run that read "just a
// little" would turn the one command an operator trusts to be harmless into one they have to reason
// about.

// OpInstallationToken is the operation that mints a credential. It is **deliberately not** on
// [Surface]: see the file comment. It is named here so that a reader looking for it finds the reason
// rather than concluding it was forgotten.
const OpInstallationToken = "POST /app/installations/{installation_id}/access_tokens"

// InstallationToken is what GitHub answers when an App asks for a credential.
type InstallationToken struct {
	// Token is the credential. It is never logged, never recorded and never part of a pointer.
	Token string
	// ExpiresAt is when it stops working, so a long campaign renews rather than failing mid-cycle.
	ExpiresAt time.Time
	// Permissions is the granular set GitHub granted, keyed by its own permission name with values
	// like `read`, `write`, `admin`. This is the platform statement FR-003's strong form rests on.
	Permissions map[string]string
	// RepositorySelection is `all` or `selected` — FR-008's regime, reported rather than interpreted.
	RepositorySelection string
}

// TokenMinter obtains an installation credential.
//
// An interface so that a replay, a dry run against a recorded response and a unit test are the same
// code path as a live run — and so that the one call in this package that is not a read has exactly
// one implementation, in one place, with the reason beside it.
type TokenMinter interface {
	MintInstallationToken(ctx context.Context) (InstallationToken, error)
}

// GateResult is what the gate concluded.
type GateResult struct {
	// Evidence is how read-only was concluded. For GitHub it is always EvidencePlatformReported on
	// success: the permissions come from the platform on every cycle.
	Evidence feeder.CredentialEvidence
	// Scope is the regime the credential puts the connector in (FR-008).
	Scope feeder.CredentialScope
	// Permissions is what GitHub reported, for the checkpoint. Sorted `name=value` pairs, so two runs
	// of the same shape read the same.
	Permissions []string
	// ExpiresAt is the credential's expiry, so a caller can say how long the run has.
	ExpiresAt time.Time
}

// Gate proves the credential read-only before anything is emitted, and reports what it is scoped to.
//
// It refuses rather than warning. FR-003 is not "prefer a read-only credential": a connector that ran
// with a write-capable one would be a connector whose read-only claim rests on it never having made a
// mistake.
func Gate(ctx context.Context, minter TokenMinter) (GateResult, error) {
	if minter == nil {
		return GateResult{}, fmt.Errorf("github: a gate with no way to obtain a credential; it would " +
			"conclude read-only from having asked nobody, which is the one conclusion FR-003 forbids")
	}
	token, err := minter.MintInstallationToken(ctx)
	if err != nil {
		return GateResult{}, fmt.Errorf("github: obtaining the installation credential: %w", err)
	}
	return proveToken(token)
}

// proveToken is the gate's judgement on one minted credential: what the platform said it may do, checked
// against the read-only rule. Startup and every renewal share it, so the two cannot drift.
func proveToken(token InstallationToken) (GateResult, error) {
	report := feeder.CredentialReport{
		Platform:    Platform,
		Evidence:    feeder.EvidencePlatformReported,
		Permissions: token.Permissions,
		Scope: feeder.CredentialScope{
			// The platform's own boundary, not the operator's: an installation's repository selection
			// is enforced by GitHub, and a repository outside it is unreachable rather than skipped.
			PlatformEnforced: true,
			Selection:        token.RepositorySelection,
		},
	}
	evidence, err := feeder.CheckReadOnly(report)
	if err != nil {
		return GateResult{}, err
	}

	permissions := make([]string, 0, len(token.Permissions))
	for name, value := range token.Permissions {
		permissions = append(permissions, name+"="+value)
	}
	slices.Sort(permissions)

	return GateResult{
		Evidence:    evidence,
		Scope:       report.Scope,
		Permissions: permissions,
		ExpiresAt:   token.ExpiresAt,
	}, nil
}

// AppMinter is the live implementation: it exchanges an App JWT for an installation token.
//
// The JWT is supplied rather than built here, because signing it needs the App's private key and this
// package has no business holding one — the key lives wherever the operator's secret management put
// it, and a connector that read a private key from a file would be a connector with a private key in
// its process for longer than one request.
type AppMinter struct {
	// BaseURL is the API root, so GitHub Enterprise Server works without a code change.
	BaseURL string
	// InstallationID is the installation to get a credential for.
	InstallationID int64
	// JWT returns a short-lived App assertion. A function rather than a string so the assertion is
	// fetched when it is used and is never a field something might format.
	JWT func(ctx context.Context) (string, error)
	// HTTP is the client. Nil gets one with a timeout.
	HTTP *http.Client
}

// MintInstallationToken performs the one non-read call this connector makes. See the file comment.
func (m *AppMinter) MintInstallationToken(ctx context.Context) (InstallationToken, error) {
	if m.JWT == nil {
		return InstallationToken{}, fmt.Errorf("github: an app minter with no assertion source")
	}
	if m.InstallationID == 0 {
		return InstallationToken{}, fmt.Errorf("github: an app minter with no installation id")
	}
	base := strings.TrimRight(strings.TrimSpace(m.BaseURL), "/")
	if base == "" {
		base = DefaultBaseURL
	}
	assertion, err := m.JWT(ctx)
	if err != nil {
		return InstallationToken{}, fmt.Errorf("github: building the app assertion: %w", err)
	}
	client := m.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}

	url := fmt.Sprintf("%s/app/installations/%d/access_tokens", base, m.InstallationID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return InstallationToken{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+assertion)

	resp, err := client.Do(req)
	if err != nil {
		return InstallationToken{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return InstallationToken{}, &StatusError{
			Operation: OpInstallationToken, Status: resp.StatusCode,
			// `time.Now` rather than an injected clock: the gate mints a token before any client
			// exists, so there is no client clock to borrow. The only cost of a real clock here is
			// that a test cannot pin this particular wait, and no test needs to.
			RetryAfter: retryAfter(resp.Header, time.Now().UTC()),
		}
	}

	var body struct {
		Token               string            `json:"token"`
		ExpiresAt           time.Time         `json:"expires_at"`
		Permissions         map[string]string `json:"permissions"`
		RepositorySelection string            `json:"repository_selection"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return InstallationToken{}, fmt.Errorf("github: decoding the installation token: %w", err)
	}
	return InstallationToken{
		Token:               body.Token,
		ExpiresAt:           body.ExpiresAt.UTC(),
		Permissions:         body.Permissions,
		RepositorySelection: body.RepositorySelection,
	}, nil
}

var _ TokenMinter = (*AppMinter)(nil)

// RenewingToken is the credential of a long-running feeder: it proves the first token at startup (as
// the feeder's [Gater]), serves it to the transport (as its [TokenSource]), and mints a new one before the
// old one expires — proving EVERY renewal read-only with the same check as the first (004 T157).
//
// Re-proving is the point. An installation's permissions are the operator's to change at any time, and
// a connector that checked once at 09:00 and kept renewing would be running at 15:00 on whatever the App
// had been granted since. A renewal the check refuses is returned as [ErrGateRefused], which ends the
// run rather than making one cycle partial.
type RenewingToken struct {
	// Minter obtains credentials. Required.
	Minter TokenMinter
	// Now is the clock. Nil uses the wall clock.
	Now func() time.Time
	// Margin is how long before expiry a token is renewed. Zero uses RenewalMargin.
	Margin time.Duration

	mu      sync.Mutex
	current InstallationToken
}

// RenewalMargin renews a token five minutes before GitHub says it expires, so a request issued in the
// last minute never carries a credential that lapses in flight.
const RenewalMargin = 5 * time.Minute

// Prove mints and proves the first credential. It is the feeder's startup gate.
func (r *RenewingToken) Prove(ctx context.Context) (GateResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.renewLocked(ctx)
}

// Token returns a proved credential, renewing it when it is close to expiry.
func (r *RenewingToken) Token(ctx context.Context) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	margin := r.Margin
	if margin <= 0 {
		margin = RenewalMargin
	}
	if r.current.Token != "" && (r.current.ExpiresAt.IsZero() || r.now().Before(r.current.ExpiresAt.Add(-margin))) {
		return r.current.Token, nil
	}
	if _, err := r.renewLocked(ctx); err != nil {
		return "", err
	}
	return r.current.Token, nil
}

func (r *RenewingToken) renewLocked(ctx context.Context) (GateResult, error) {
	if r.Minter == nil {
		return GateResult{}, fmt.Errorf("github: a renewing credential with no minter")
	}
	token, err := r.Minter.MintInstallationToken(ctx)
	if err != nil {
		return GateResult{}, fmt.Errorf("github: obtaining the installation credential: %w", err)
	}
	result, err := proveToken(token)
	if err != nil {
		// The old token is dropped too: it was proved under a grant the platform no longer reports.
		r.current = InstallationToken{}
		return GateResult{}, fmt.Errorf("%w: %w", ErrGateRefused, err)
	}
	r.current = token
	return result, nil
}

func (r *RenewingToken) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now().UTC()
}
