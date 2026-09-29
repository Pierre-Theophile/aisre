// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/sanitise"
)

// The refusals `feed datadog` makes before it reads anything (T058).
func TestFeedDatadogRefusesBeforeReading(t *testing.T) {
	t.Setenv("DD_API_KEY", "")
	t.Setenv("DD_APP_KEY", "")
	t.Setenv(sanitise.KeyEnv, "")
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"a live recording with no corpus key", []string{"--org", "twin", "--site", "datadoghq.eu", "--record", t.TempDir()}, "FR-137"},
		{"no site", []string{"--org", "twin"}, "--site is required"},
		{"an unbuilt capability", []string{"--org", "twin", "--capabilities", "logs,changes"}, "not built"},
		{"a malformed log source", []string{"--org", "twin", "--site", "datadoghq.eu", "--watch", "production/check/out"}, "separator"},
		{"a malformed override", []string{"--org", "twin", "--site", "datadoghq.eu", "--version-override", "production/checkout"}, "<env>/<service>=<field>"},
		{"no keys", []string{"--org", "twin", "--site", "datadoghq.eu", "--dry-run"}, "DD_API_KEY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, stderr, code := run(t, t.Context(), append([]string{"feed", "datadog"}, tc.args...)...)
			if code != ExitUsage || !strings.Contains(stderr, tc.want) {
				t.Fatalf("exit %d, stderr %q; want a usage refusal naming %q", code, stderr, tc.want)
			}
		})
	}
}

// A service whose logs carry no environment is watched, and the configuration path says what it will
// not do and how to fix it, before anything is read (005).
func TestAWatchWithoutAnEnvironmentWarnsAtConfiguration(t *testing.T) {
	t.Setenv("DD_API_KEY", "")
	t.Setenv("DD_APP_KEY", "")
	_, stderr, code := run(t, t.Context(), "feed", "datadog", "--org", "twin", "--site", "datadoghq.eu",
		"--watch", "checkout", "--dry-run")
	if code != ExitUsage || !strings.Contains(stderr, "DD_API_KEY") {
		t.Fatalf("exit %d, stderr %q; the source must be accepted and the run stop only on the missing keys", code, stderr)
	}
	for _, want := range []string{"note: --watch checkout", "watched without an environment", "looks for one in its logs", "@env"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr does not say %q:\n%s", want, stderr)
		}
	}
}
