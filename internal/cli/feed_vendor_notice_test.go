// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// `feed vendor-notice` (T090, 003 FR-002, FR-058, FR-132a, FR-132b).

const vendorNoticeConfig = `
org: twin
vendors:
  - slug: acme-gpu
    name: Acme GPU
    products: [inference-api]
    hosts: [api.acme-gpu.test]
    senders: [status@acme-gpu.test]
    status_page: https://status.acme-gpu.test
    statuspage_api: true
    rules:
      - match: 'inference-api unavailable from (?P<start>\S+) to (?P<end>\S+)'
        kind: maintenance
        product: inference-api
mailboxes:
  - name: platform-notices
    address: notices@example.test
    kind: workspace_mailbox
    path: imap_examine_peek
    server: imap.example.test:993
    folder: Vendor Notices
    user: notices@example.test
    password_env: SRE_AGENT_TEST_MAILBOX_PASSWORD
`

func writeVendorConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "vendors.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing the configuration: %v", err)
	}
	return path
}

// --dry-run validates the configuration, says what would be polled, and reads nothing. It is the
// invocation for checking a new allowlist entry before a campaign.
func TestFeedVendorNoticeDryRunReportsWhatWouldBePolled(t *testing.T) {
	t.Setenv("SRE_AGENT_TEST_MAILBOX_PASSWORD", "not-a-real-password")

	stdout, stderr, code := run(t, t.Context(), "feed", "vendor-notice",
		"--allowlist", writeVendorConfig(t, vendorNoticeConfig), "--dry-run")
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %s)", code, ExitOK, stderr)
	}
	for _, want := range []string{
		"configuration: valid",
		"vendor-notice:twin",
		"would poll: mailbox",
		"would poll: status_page",
		"would poll: deprecation_header",
		"no mailbox was opened",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the dry run does not report %q:\n%s", want, stdout)
		}
	}
	// The source id is the vendor-notice feeder's own. Sharing the GCP feeder's would couple one
	// source's silence to the other's, which is exactly what FR-002 separates.
	if strings.Contains(stdout, "gcp:") {
		t.Errorf("the dry run names a GCP source id:\n%s", stdout)
	}
}

// --allowlist has no default. Which vendors, which products and which senders are configuration an
// operator states, and no vendor is built in (FR-132b).
func TestFeedVendorNoticeUsageErrors(t *testing.T) {
	t.Parallel()

	cases := map[string][]string{
		"no allowlist and no replay": {"feed", "vendor-notice"},
		"record and replay together": {
			"feed", "vendor-notice", "--allowlist", "x.yaml", "--record", "a", "--replay", "b",
		},
		"a configuration that does not exist": {
			"feed", "vendor-notice", "--allowlist", filepath.Join(t.TempDir(), "absent.yaml"),
		},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, stderr, code := run(t, t.Context(), args...)
			if code != ExitUsage {
				t.Fatalf("exit code = %d, want %d (stderr: %s)", code, ExitUsage, stderr)
			}
		})
	}
}

// A configuration with no source is refused rather than run: an unbroken series of empty cycles is the
// most convincing impression of a working feeder there is (FR-058).
func TestFeedVendorNoticeRefusesAConfigurationWithNoSource(t *testing.T) {
	t.Parallel()

	path := writeVendorConfig(t, "org: twin\nvendors:\n  - slug: acme-gpu\n    products: [p]\n")
	_, stderr, code := run(t, t.Context(), "feed", "vendor-notice", "--allowlist", path, "--dry-run")
	if code != ExitUsage {
		t.Fatalf("exit code = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(stderr, "working feeder") {
		t.Errorf("the refusal does not explain itself: %s", stderr)
	}
}

// The command is registered once, and it is a command of its own rather than a flag on `feed gcp`: the
// separate credential, cadence, budget and checkpoint of FR-002 are enforced by there being two
// commands rather than promised by a comment.
func TestFeedVendorNoticeIsItsOwnCommandRegisteredOnce(t *testing.T) {
	t.Parallel()

	root := NewRootCommand()
	var feed *cobra.Command
	for _, cmd := range root.Commands() {
		if cmd.Name() == "feed" {
			feed = cmd
		}
	}
	if feed == nil {
		t.Fatal("the root command has no `feed`")
	}
	seen := 0
	var gcp *cobra.Command
	for _, sub := range feed.Commands() {
		switch sub.Name() {
		case "vendor-notice":
			seen++
		case "gcp":
			gcp = sub
		}
	}
	if seen != 1 {
		t.Errorf("`feed vendor-notice` is registered %d times, want once", seen)
	}
	if gcp == nil {
		t.Fatal("`feed gcp` is missing, so this test cannot check the two are separate")
	}
	// And neither command carries the other's flags, which is what would let one run read both.
	if gcp.Flags().Lookup("allowlist") != nil {
		t.Error("`feed gcp` carries --allowlist, so one invocation could read both systems (FR-002)")
	}
	for _, sub := range feed.Commands() {
		if sub.Name() != "vendor-notice" {
			continue
		}
		if sub.Flags().Lookup("projects") != nil {
			t.Error("`feed vendor-notice` carries --projects, so one invocation could read both (FR-002)")
		}
	}
}
