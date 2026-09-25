// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"
)

// Credentials must not turn up where a credential is not expected (T090, constitution VII).
//
// The canary is a string no legitimate output contains. If it appears anywhere in help text,
// usage text or an error message, something rendered the caller's token.
const tokenCanary = "sre-agent-token-canary-a3f9"

// `--help` renders every flag's default. Wiring $SRE_AGENT_TOKEN in as the default of --token
// therefore printed the caller's live bearer token to anyone who ran `aisre --help` — in a
// terminal, in a CI log, in a screenshot attached to a ticket. The environment variable is read
// in PersistentPreRunE instead, and this test is what keeps it there.
func TestHelpNeverPrintsTheToken(t *testing.T) {
	t.Setenv(EnvToken, tokenCanary)

	for _, args := range [][]string{
		{"--help"},
		{"query", "--help"},
		{"serve", "--help"},
		{"dev-token", "--help"},
		{"fixture", "verify", "--help"},
	} {
		root := NewRootCommand()
		var out, errOut bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&errOut)
		root.SetArgs(args)
		if err := root.Execute(); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		rendered := out.String() + errOut.String()
		if strings.Contains(rendered, tokenCanary) {
			t.Errorf("`sre-agent %s` printed the token from $%s:\n%s",
				strings.Join(args, " "), EnvToken, rendered)
		}
		// The help must still tell the reader where the token comes from.
		if args[0] == "--help" && !strings.Contains(rendered, EnvToken) {
			t.Errorf("`aisre --help` no longer mentions $%s", EnvToken)
		}
	}
}

// The --token flag's declared default must stay empty: that is the property `--help` renders.
func TestTokenFlagHasNoDefaultValue(t *testing.T) {
	t.Setenv(EnvToken, tokenCanary)

	flag := NewRootCommand().PersistentFlags().Lookup("token")
	if flag == nil {
		t.Fatal("no --token flag")
	}
	if flag.DefValue != "" {
		t.Errorf("--token default = %q, want empty: cobra prints it in --help", flag.DefValue)
	}
}

// Resolution still prefers the flag and falls back to the environment; only the timing moved.
func TestTokenFrom(t *testing.T) {
	for _, tc := range []struct{ flag, env, want string }{
		{"", tokenCanary, tokenCanary},
		{"explicit", tokenCanary, "explicit"},
		{"  ", tokenCanary, tokenCanary},
		{"", "", ""},
	} {
		if got := tokenFrom(tc.flag, tc.env); got != tc.want {
			t.Errorf("tokenFrom(%q, %q) = %q, want %q", tc.flag, tc.env, got, tc.want)
		}
	}
}

// An authentication failure tells the caller what to do about it and never quotes the
// credential that failed.
func TestAuthErrorsDoNotQuoteTheToken(t *testing.T) {
	for _, code := range []connect.Code{
		connect.CodeUnauthenticated, connect.CodePermissionDenied, connect.CodeUnavailable,
	} {
		err := remoteError("query subgraph",
			connect.NewError(code, errors.New("bearer token is not valid")))
		if strings.Contains(err.Error(), tokenCanary) {
			t.Errorf("%v: error quoted the token: %v", code, err)
		}
		if strings.Contains(err.Error(), "eyJ") {
			t.Errorf("%v: error looks like it contains a JWT: %v", code, err)
		}
	}
}

// A DSN carries a password, so the "no database" hint must show a placeholder and never the
// caller's own.
func TestDSNErrorShowsAPlaceholderNotTheRealDSN(t *testing.T) {
	_, err := dsnFrom("", "")
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "sreagent:sreagent@localhost") {
		t.Errorf("the hint should show the published local example: %v", err)
	}
}
