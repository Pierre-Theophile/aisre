// SPDX-License-Identifier: Apache-2.0

package gcpx

import (
	"context"
	"errors"
	"fmt"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// The credential: one read-only service account, shared by the GCP feeder and the GCP telemetry
// backend and by nothing else (FR-003; contracts/gcp-feeder.md §2).
//
// Discovery is Application Default Credentials, which is what every deployment shape this
// integration runs in already provides — workload identity on GKE, the metadata server on Cloud Run,
// an impersonated service account on a developer's machine. There is deliberately no flag for a key
// file: a path to a key on disk is a key on disk, and the one credential shape this integration
// should not make easy is the one that gets committed.
//
// ---------------------------------------------------------------------------------------------
// Why the scopes are requested narrowly even though IAM is what actually decides.
//
// A token's scopes and its principal's IAM policy are two different limits, and BOTH apply: the call
// succeeds only if the policy grants the permission AND the token carries a scope that covers it. So
// a `cloud-platform.read-only` token held by a principal that IAM would let write cannot write —
// the scope refuses it — and that is a materially better position than trusting the policy alone.
//
// This matters because `testIamPermissions` (permissions.go, layer 1) answers about the POLICY and
// is blind to the token. It cannot distinguish a credential minted with `cloud-platform.read-only`
// from one minted with `cloud-platform`, and those two are not equally safe. tokenscope.go is
// layer 2 for exactly that gap (research §8, item 6).
//
// It also covers a case layer 1 cannot reach at all: `cloudsql.flags.list` does not exist as an IAM
// permission — the flags endpoint is project-less and gated purely by OAuth scope — so for that call
// the scope is the ONLY control there is (research §8.1, item 2).

// The scopes this integration asks for, narrowest first.
const (
	// ScopeCloudPlatformReadOnly covers reads across every GCP service this integration touches
	// and authorises no write anywhere. It is the scope to mint the credential with.
	ScopeCloudPlatformReadOnly = "https://www.googleapis.com/auth/cloud-platform.read-only"
	// ScopeMonitoringRead and ScopeLoggingRead are narrower still, and are requested alongside so
	// that a deployment which prefers per-service scopes over the umbrella one also works.
	ScopeMonitoringRead = "https://www.googleapis.com/auth/monitoring.read"
	ScopeLoggingRead    = "https://www.googleapis.com/auth/logging.read"

	// ScopeCloudPlatform is NOT requested. It is named here so that tokenscope.go can recognise it
	// in a token the environment supplied, and say plainly that the credential is wider than this
	// integration asked for.
	ScopeCloudPlatform = "https://www.googleapis.com/auth/cloud-platform"
)

// ReadOnlyScopes is the set passed to Application Default Credentials.
var ReadOnlyScopes = []string{
	ScopeCloudPlatformReadOnly,
	ScopeMonitoringRead,
	ScopeLoggingRead,
}

// ErrNoCredential is returned when a transport is built without a credential. It is a distinct error
// because the ordering it protects is the whole of FR-004: a Credential exists only where the gate
// passed, so a nil one reaching a client constructor is a bug in the wiring, not bad input.
var ErrNoCredential = errors.New("gcpx: no credential; a transport may not be built before the read-only gate has passed")

// Credential is a proved-read-only Google credential.
//
// It is returned ONLY by a gate that passed. That is the point of the type: a caller holding one has
// evidence rather than an intention, and there is no ordering in which a per-area client is
// constructed before the check that makes it safe. `gate` is unexported and unset-able from outside
// this package for the same reason.
type Credential struct {
	creds     *google.Credentials
	projectID string
	scopes    []string
	gate      *GateResult
}

// TokenSource returns the credential's token source, for option.WithTokenSource.
func (c *Credential) TokenSource() oauth2.TokenSource {
	if c == nil || c.creds == nil {
		return nil
	}
	return c.creds.TokenSource
}

// ProjectID is the project Application Default Credentials resolved, which may be empty: a service
// account key names one, the metadata server names one, and an impersonated user often names none.
// Nothing here treats it as the scope to read — that is config (FR-131), and an empty value is
// simply reported rather than defaulted.
func (c *Credential) ProjectID() string {
	if c == nil {
		return ""
	}
	return c.projectID
}

// Scopes are the scopes this credential was requested with.
func (c *Credential) Scopes() []string {
	if c == nil {
		return nil
	}
	return append([]string(nil), c.scopes...)
}

// Gate is the evidence the credential passed the read-only gate, including what each layer could
// and could not verify. It is recorded in the checkpoint and in the usage report, so a later reader
// knows which layers spoke rather than only that the run started.
func (c *Credential) Gate() *GateResult {
	if c == nil {
		return nil
	}
	return c.gate
}

// FindDefaultCredential discovers the credential through Application Default Credentials, with the
// read-only scopes.
//
// It does NOT prove anything about the credential: that is Gate's job, and this function is
// deliberately not exported in a form that yields a *Credential. Discovery and proof are separate
// so that no caller can accidentally obtain the second by doing the first.
func FindDefaultCredential(ctx context.Context) (*google.Credentials, error) {
	creds, err := google.FindDefaultCredentials(ctx, ReadOnlyScopes...)
	if err != nil {
		return nil, fmt.Errorf("gcpx: application default credentials: %w", err)
	}
	return creds, nil
}
