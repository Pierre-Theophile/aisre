// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"testing"
)

// The refusals `feed datadog` makes before it reads anything (T058).
func TestFeedDatadogRefusesBeforeReading(t *testing.T) {
	t.Setenv("DD_API_KEY", "")
	t.Setenv("DD_APP_KEY", "")
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"a live recording", []string{"--org", "twin", "--site", "datadoghq.eu", "--record", t.TempDir()}, "FR-137"},
		{"no site", []string{"--org", "twin"}, "--site is required"},
		{"an unbuilt capability", []string{"--org", "twin", "--capabilities", "logs,changes"}, "not built"},
		{"a malformed log source", []string{"--org", "twin", "--site", "datadoghq.eu", "--watch", "checkout"}, "<env>/<service>"},
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
