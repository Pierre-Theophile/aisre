// SPDX-License-Identifier: Apache-2.0

package vendornotice_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	vn "github.com/Pierre-Theophile/aisre/internal/feeders/vendornotice"
)

// The allowlist file (T090, FR-059, FR-132a, FR-132b).

const wholeConfig = `
version: "1.0.0"
org: twin
cadence: 15m
refuse_empty_notice_stream: true
platform_senders: [platform-noreply@acme-gpu.test]
platform_vendor: acme-gpu
platform_rules:
  - match: 'platform-wide maintenance on (?P<start>\d{4}-\d{2}-\d{2})'
    kind: maintenance
    product: inference-api
mailboxes:
  - name: platform-notices
    address: notices@example.test
    kind: workspace_mailbox
    path: imap_examine_peek
    authorised_by: the mailbox owner
    server: imap.example.test:993
    folder: Vendor Notices
    user: notices@example.test
    password_env: SRE_AGENT_TEST_MAILBOX_PASSWORD
    lookback: 168h
  - name: essential-contact-1
    address: someone@example.test
    kind: workspace_mailbox
    path: imap_examine_peek
    server: imap.example.test:993
    folder: INBOX
    user: someone@example.test
    password_env: SRE_AGENT_TEST_MAILBOX_PASSWORD
vendors:
  - slug: acme-gpu
    name: Acme GPU
    products: [inference-api, training]
    hosts: [api.acme-gpu.test]
    senders: [status@acme-gpu.test]
    host_products:
      api.acme-gpu.test: inference-api
    status_page: https://status.acme-gpu.test
    statuspage_api: true
    status_feed: https://status.acme-gpu.test/incidents.json
    status_feed_format: gcp-incidents
    changelog_feed: https://acme-gpu.test/changelog.atom
    changelog_feed_format: atom
    rules:
      - match: 'inference-api unavailable from (?P<start>\S+) to (?P<end>\S+)'
        kind: maintenance
        product: inference-api
extraction:
  store_body: false
  on_failure: report_with_pointer
  unknown_valid_start: mark
`

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "vendors.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing the configuration: %v", err)
	}
	return path
}

// The file the repository publishes must load. A loader that needed a different shape would leave
// config/vendors.yaml describing something nobody runs — and that file is what the connector's
// documentation tells a mailbox owner they are approving.
func TestTheRepositorysOwnAllowlistFileLoads(t *testing.T) {
	t.Parallel()

	path := filepath.Join("..", "..", "..", "config", "vendors.yaml")
	cfg, err := vn.LoadConfig(path)
	if err != nil {
		t.Fatalf("the published allowlist does not load: %v", err)
	}
	list, err := cfg.Allowlist()
	if err != nil {
		t.Fatalf("the published allowlist does not build: %v", err)
	}
	if list.Len() == 0 {
		t.Fatal("the published allowlist holds no vendors")
	}
	if _, known := list.Vendor("google-cloud"); !known {
		t.Error("the published allowlist does not carry google-cloud, which is the platform this " +
			"feature reads and the one entry whose endpoints are publicly documented")
	}
	// It refuses an empty notice stream, which is what turns a misconfigured mailbox into a loud
	// failure at campaign start rather than silence recorded as evidence (FR-132b).
	if !cfg.RefuseEmptyNoticeStream {
		t.Error("the published allowlist does not refuse an empty notice stream")
	}
}

func TestAWholeConfigurationBuildsEverySource(t *testing.T) {
	t.Setenv("SRE_AGENT_TEST_MAILBOX_PASSWORD", "not-a-real-password")

	cfg, err := vn.LoadConfig(writeConfig(t, wholeConfig))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	list, err := cfg.Allowlist()
	if err != nil {
		t.Fatalf("Allowlist: %v", err)
	}
	// The platform senders are matched as that vendor's senders: the separate list in the file is
	// documentation — they carry notices for the platform rather than one product — and matching has
	// one index either way.
	if _, known := list.VendorBySender("platform-noreply@acme-gpu.test"); !known {
		t.Error("a platform sender is not matched to the platform vendor")
	}

	sources, err := cfg.Sources(list, vn.NewHTTPSources(vn.ClientOptions{}))
	if err != nil {
		t.Fatalf("Sources: %v", err)
	}
	kinds := map[vn.SourceKind]int{}
	for _, source := range sources {
		kinds[source.Kind()]++
	}
	// One mailbox source reading both mailboxes; two changelog documents; one status page; and the
	// deprecation-header source, which is on by default because it costs nothing.
	if kinds[vn.SourceMailbox] != 1 {
		t.Errorf("the mailbox source appears %d times, want once for both mailboxes", kinds[vn.SourceMailbox])
	}
	if kinds[vn.SourceStatusPage] != 1 {
		t.Errorf("the status page appears %d times", kinds[vn.SourceStatusPage])
	}
	if kinds[vn.SourceChangelog] != 2 {
		t.Errorf("the changelog sources appear %d times, want the status feed and the changelog feed",
			kinds[vn.SourceChangelog])
	}
	if kinds[vn.SourceDeprecationHeader] != 1 {
		t.Errorf("the deprecation-header source appears %d times", kinds[vn.SourceDeprecationHeader])
	}
	if cfg.Cadence.Minutes() != 15 {
		t.Errorf("cadence = %s", cfg.Cadence)
	}
}

// A status page nobody said was an Atlassian Statuspage is not polled on the API paths. Coverage is not
// universal, and probing a page that is not one and reporting the 404 as a gap every cycle would be
// noise about a vendor who simply publishes elsewhere (FR-075).
func TestAStatusPageIsPolledOnlyWhereTheFileSaysItIsAStatuspage(t *testing.T) {
	t.Parallel()

	// A second vendor with a document this feeder can read, so the run has a source at all and this
	// test is about the status page rather than about the empty-configuration refusal.
	body := "vendors:\n  - slug: acme-gpu\n    products: [inference-api]\n" +
		"    status_page: https://status.acme-gpu.test\n" +
		"  - slug: other-gpu\n    products: [other-api]\n" +
		"    status_feed: https://status.other-gpu.test/incidents.json\n" +
		"    status_feed_format: gcp-incidents\n"
	cfg, err := vn.LoadConfig(writeConfig(t, body))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	list, err := cfg.Allowlist()
	if err != nil {
		t.Fatalf("Allowlist: %v", err)
	}
	sources, err := cfg.Sources(list, vn.NewHTTPSources(vn.ClientOptions{}))
	if err != nil {
		t.Fatalf("Sources: %v", err)
	}
	for _, source := range sources {
		if source.Kind() == vn.SourceStatusPage {
			t.Error("a status page nobody called an Atlassian Statuspage is polled on its API paths")
		}
	}
}

// The deprecation-header source watches headers on requests the *other* sources make. On its own it has
// nothing to watch, so a configuration whose only source is that observer is refused: it would report an
// unbroken series of empty cycles under a source's name, which is worse than reporting none.
func TestAConfigurationWhoseOnlySourceWatchesNothingIsRefused(t *testing.T) {
	t.Parallel()

	cfg, err := vn.LoadConfig(writeConfig(t, "vendors:\n  - slug: acme-gpu\n    products: [p]\n"))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	list, err := cfg.Allowlist()
	if err != nil {
		t.Fatalf("Allowlist: %v", err)
	}
	if _, err := cfg.Sources(list, vn.NewHTTPSources(vn.ClientOptions{})); err == nil {
		t.Fatal("a configuration with nothing to poll was accepted")
	}
}

// Every refusal the file has, because each one is a default that would otherwise be a guess about
// somebody's estate — or a setting that appeared to turn off a guarantee.
func TestTheConfigurationRefusesRatherThanDefaults(t *testing.T) {
	t.Parallel()

	const vendorsOnly = "vendors:\n  - slug: acme-gpu\n    products: [inference-api]\n"
	cases := map[string]struct {
		config string
		expect string
	}{
		"no vendors": {
			config: "org: twin\n",
			expect: "quiet week",
		},
		"a vendor with no slug": {
			config: "vendors:\n  - name: Acme\n",
			expect: "identity cannot be blank",
		},
		"a mailbox with no name": {
			config: vendorsOnly + "mailboxes:\n  - kind: workspace_mailbox\n    path: imap_examine_peek\n",
			expect: "which mailbox",
		},
		"a Google Group": {
			config: vendorsOnly + "mailboxes:\n  - name: n\n    kind: google_group\n    path: gmail_readonly\n",
			expect: "subscribed member's mailbox",
		},
		"a mailbox that does not say what kind of address it is": {
			config: vendorsOnly + "mailboxes:\n  - name: n\n    path: gmail_readonly\n",
			expect: "silent when wrong",
		},
		"a mailbox with no access path": {
			config: vendorsOnly + "mailboxes:\n  - name: n\n    kind: workspace_mailbox\n",
			expect: "read-only guarantee differs",
		},
		"an IMAP mailbox with no folder": {
			config: vendorsOnly + "mailboxes:\n  - name: n\n    kind: workspace_mailbox\n" +
				"    path: imap_examine_peek\n    server: s:993\n    user: u\n    password_env: E\n",
			expect: "INBOX",
		},
		"an IMAP mailbox naming no credential variable": {
			config: vendorsOnly + "mailboxes:\n  - name: n\n    kind: workspace_mailbox\n" +
				"    path: imap_examine_peek\n    server: s:993\n    user: u\n    folder: f\n",
			expect: "repository's history",
		},
		"two mailboxes with one name": {
			config: vendorsOnly + "mailboxes:\n" +
				"  - name: n\n    kind: workspace_mailbox\n    path: gmail_readonly\n    address: a@example.test\n" +
				"  - name: n\n    kind: workspace_mailbox\n    path: gmail_readonly\n    address: b@example.test\n",
			expect: "both called",
		},
		"store_body true": {
			config: vendorsOnly + "extraction:\n  store_body: true\n",
			expect: "never written",
		},
		"a failure behaviour that drops a notice": {
			config: vendorsOnly + "extraction:\n  on_failure: drop\n",
			expect: "report_with_pointer",
		},
		"a guessed valid start": {
			config: vendorsOnly + "extraction:\n  unknown_valid_start: guess\n",
			expect: "never guessed",
		},
		"platform senders attributed to nobody": {
			config: vendorsOnly + "platform_senders: [noreply@acme-gpu.test]\n",
			expect: "nobody can act on",
		},
		"a changelog format nobody publishes": {
			config: "vendors:\n  - slug: acme-gpu\n    products: [p]\n" +
				"    changelog_feed: u\n    changelog_feed_format: yaml\n",
			expect: "not one of",
		},
		"an html changelog with no entry boundary": {
			config: "vendors:\n  - slug: acme-gpu\n    products: [p]\n" +
				"    changelog: u\n    changelog_format: html\n",
			expect: "entry boundary",
		},
		"an unknown key": {
			config: vendorsOnly + "mailbox: whatever\n",
			expect: "field mailbox not found",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := vn.LoadConfig(writeConfig(t, tc.config))
			if err == nil {
				t.Fatalf("the configuration was accepted")
			}
			if !strings.Contains(err.Error(), tc.expect) {
				t.Errorf("the refusal does not explain itself (%q not in): %v", tc.expect, err)
			}
		})
	}
}

// A credential the file names and the environment does not hold is a refusal, not a run that connects
// with an empty password and reports a gap nobody can diagnose.
func TestAMissingCredentialVariableIsARefusal(t *testing.T) {
	cfg, err := vn.LoadConfig(writeConfig(t, wholeConfig))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	list, err := cfg.Allowlist()
	if err != nil {
		t.Fatalf("Allowlist: %v", err)
	}
	// SRE_AGENT_TEST_MAILBOX_PASSWORD is deliberately not set here.
	if _, err := cfg.Sources(list, vn.NewHTTPSources(vn.ClientOptions{})); err == nil {
		t.Fatal("a run was built with the credential variable unset")
	}
}

// The file has nowhere to put a credential. There is no field for one, which is what makes it a rule
// rather than a convention — asserted by the decoder refusing the key.
func TestTheConfigurationHasNowhereToPutAPassword(t *testing.T) {
	t.Parallel()

	for _, key := range []string{"password", "token", "secret", "app_password"} {
		body := "vendors:\n  - slug: acme-gpu\n    products: [p]\nmailboxes:\n  - name: n\n" +
			"    kind: workspace_mailbox\n    path: imap_examine_peek\n    server: s:993\n" +
			"    user: u\n    folder: f\n    " + key + ": hunter2\n"
		if _, err := vn.LoadConfig(writeConfig(t, body)); err == nil {
			t.Errorf("the configuration accepted an inline %s", key)
		}
	}
}

// A rule announcing no kind, or a kind this feeder does not publish, is refused: there is no default,
// because the kind decides the change kind the graph records (ADR-0006).
func TestARuleWithNoKindIsRefused(t *testing.T) {
	t.Parallel()

	for _, kind := range []string{"", "other", "outage"} {
		body := "vendors:\n  - slug: acme-gpu\n    products: [p]\n    rules:\n" +
			"      - match: x\n        kind: " + kind + "\n"
		cfg, err := vn.LoadConfig(writeConfig(t, body))
		if err != nil {
			continue // A refusal at load time is fine too.
		}
		list, err := cfg.Allowlist()
		if err != nil {
			t.Fatalf("Allowlist: %v", err)
		}
		if _, err := cfg.Sources(list, vn.NewHTTPSources(vn.ClientOptions{})); err == nil {
			t.Errorf("a rule announcing kind %q was accepted", kind)
		}
	}
}

// One vendor's phrasing must not place another's mail. The rules live on the vendor entry, and a message
// is attributed by its sender first.
func TestOneVendorsRulesDoNotPlaceAnothersMail(t *testing.T) {
	t.Parallel()

	list, err := vn.NewAllowlist([]vn.Vendor{
		{Slug: "acme-gpu", Products: []string{"inference-api"}, Senders: []string{"status@acme-gpu.test"}},
		{Slug: "other-gpu", Products: []string{"other-api"}, Senders: []string{"status@other-gpu.test"}},
	})
	if err != nil {
		t.Fatalf("NewAllowlist: %v", err)
	}
	acme := maintenanceRule()
	script := &imapScript{readOnly: true, messages: []string{
		// The message is from the *other* vendor and matches acme's rule word for word.
		strings.ReplaceAll(maintenanceEmail, "status@acme-gpu.test", "status@other-gpu.test"),
	}}
	source, err := vn.NewMailbox(vn.MailboxOptions{
		Mailboxes: []vn.NamedMailbox{{Name: "n", Open: vn.IMAPMailbox(vn.IMAPOptions{
			Address: "imap.example.test:993", Mailbox: "Vendor Notices",
			Auth: vn.IMAPAuth{User: "notices@example.test", Password: "app-password"},
			Dial: script.serve(t),
		})}},
		Allowlist:     list,
		RulesByVendor: map[string][]vn.EntryRule{"acme-gpu": {acme}},
		Now:           func() time.Time { return clockNow },
	})
	if err != nil {
		t.Fatalf("NewMailbox: %v", err)
	}
	batch, err := source.Read(t.Context())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(batch.Announcements) != 0 {
		t.Fatalf("a rule stated for acme-gpu placed other-gpu's mail: %+v", batch.Announcements)
	}
	// It is still visible: the sender is configured, so a message this vendor's rules cannot read is
	// the shape of a notice going missing (FR-074).
	if len(batch.Unextracted) != 1 {
		t.Errorf("unextracted = %+v, want the one message from a configured sender", batch.Unextracted)
	}
}
