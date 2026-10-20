// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTokenAndTokenFileAreExclusive(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("t"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, stderr, code := run(t, context.Background(),
		"--token", "t", "--token-file", tokenFile, "query", "subgraph", "k8s.service=x")
	if code != ExitUsage || !strings.Contains(stderr, "pass one") {
		t.Fatalf("exit %d, stderr %q; want a usage error", code, stderr)
	}
}

// `--server host:port` reads as http. With a token, that would send it in clear.
func TestATokenIsNotSentInClearToARemoteServer(t *testing.T) {
	_, stderr, code := run(t, context.Background(),
		"--server", "aisre.example.com:8080", "--token", "t",
		"query", "subgraph", "k8s.service=x", "--as-of", "2026-10-20T00:00:00Z")
	if code != ExitUsage || !strings.Contains(stderr, "plaintext http") {
		t.Fatalf("exit %d, stderr %q; want the plaintext refusal", code, stderr)
	}
}

func TestServeRefusesTrustedIssuersWithDevAuth(t *testing.T) {
	_, stderr, code := run(t, context.Background(), "serve", "--db", "postgres://invalid/invalid",
		"--auth", "dev", "--dev", "--listen", "127.0.0.1:0", "--trusted-issuers", "nowhere.yaml")
	if code != ExitUsage || !strings.Contains(stderr, "--trusted-issuers needs --auth oidc") {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
}

func TestServeRefusesATrustedIssuerWithoutAudience(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "trusted-issuers.yaml")
	if err := os.WriteFile(cfg, []byte(`issuers:
  - name: verda-k3s
    issuer: https://kubernetes.default.svc.cluster.local
    jwks_url: https://example.com/jwks
    subjects:
      "system:serviceaccount:sre-agent:sre-agent-feeder": {roles: [feeder], source_id: k8s:verda}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, stderr, code := run(t, context.Background(), "serve", "--db", "postgres://invalid/invalid",
		"--auth", "oidc", "--trusted-issuers", cfg)
	if code != ExitUsage || !strings.Contains(stderr, "audience is required") {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
}

// A valid trust file is enough for --auth oidc without an organisation IdP: serve gets past
// authentication and fails only on the database it was given.
func TestServeStartsWithTrustedIssuersAlone(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "trusted-issuers.yaml")
	if err := os.WriteFile(cfg, []byte(`issuers:
  - name: google
    issuer: https://accounts.google.com
    audience: aisre-staging
    jwks_url: https://www.googleapis.com/oauth2/v3/certs
    require_email_verified: true
    subjects:
      "108000000000000000001": {name: Pierre, roles: [decider]}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, stderr, code := run(t, context.Background(), "serve", "--db", "postgres://invalid:1/invalid",
		"--auth", "oidc", "--trusted-issuers", cfg)
	if code == ExitUsage {
		t.Fatalf("a valid trust file was refused: %q", stderr)
	}
}
