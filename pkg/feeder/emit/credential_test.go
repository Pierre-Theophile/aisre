// SPDX-License-Identifier: Apache-2.0

package emit

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

func TestCheckTransportRefusesPlaintextToARemoteHost(t *testing.T) {
	for _, base := range []string{
		"https://aisre.example.run.app",
		"http://localhost:8080",
		"http://127.0.0.1:8080",
		"http://127.0.0.2:8080",
		"http://[::1]:8080",
	} {
		if err := CheckTransport(base); err != nil {
			t.Errorf("CheckTransport(%q) = %v, want nil", base, err)
		}
	}
	for _, base := range []string{
		"http://aisre.example.com",
		"http://10.0.0.7:8080",
		"http://0.0.0.0:8080",
		"http://localhost.example.com:8080",
	} {
		if err := CheckTransport(base); !errors.Is(err, ErrInsecureTransport) {
			t.Errorf("CheckTransport(%q) = %v, want ErrInsecureTransport", base, err)
		}
	}
}

func testDesc() feeder.Description {
	return feeder.Description{Kind: "k8s", SourceID: "k8s:test"}
}

func TestNewConnectEmitterRefusesATokenInClear(t *testing.T) {
	// `host:port` without a scheme reads as http: the case the check exists for.
	if _, err := NewConnectEmitter("aisre.example.com:8080", "tok", testDesc()); !errors.Is(err, ErrInsecureTransport) {
		t.Fatalf("token over http to a remote host: %v, want ErrInsecureTransport", err)
	}
	if _, err := NewConnectEmitter("http://10.0.0.7:8080", "", testDesc(),
		WithTokenSource(StaticToken("tok"))); !errors.Is(err, ErrInsecureTransport) {
		t.Fatalf("token source over http to a remote host: %v, want ErrInsecureTransport", err)
	}
	if _, err := NewConnectEmitter("http://10.0.0.7:8080", "tok", testDesc(), WithInsecureTransport()); err != nil {
		t.Fatalf("--insecure-transport: %v", err)
	}
	if _, err := NewConnectEmitter("http://10.0.0.7:8080", "", testDesc()); err != nil {
		t.Fatalf("no credential, nothing to protect: %v", err)
	}
}

// The kubelet rewrites a projected token before it expires; every call must read the file
// again rather than keep the first token.
func TestTokenFileIsReadOnEveryCall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
	}))
	defer srv.Close()

	client := &http.Client{Transport: &bearerTransport{base: http.DefaultTransport, source: TokenFile(path)}}
	get := func() {
		t.Helper()
		resp, err := client.Get(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	get()
	if err := os.WriteFile(path, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	get()
	if len(seen) != 2 || seen[0] != "Bearer first" || seen[1] != "Bearer second" {
		t.Fatalf("Authorization headers = %q, want the rotated token on the second call", seen)
	}

	if err := os.WriteFile(path, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if resp, err := client.Get(srv.URL); err == nil {
		_ = resp.Body.Close()
		t.Fatal("an empty token file sent an anonymous call")
	}
	if len(seen) != 2 {
		t.Fatalf("a call reached the server without a token: %q", seen)
	}
}
