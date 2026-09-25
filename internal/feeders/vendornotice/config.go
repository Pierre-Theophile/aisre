// SPDX-License-Identifier: Apache-2.0

package vendornotice

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Pierre-Theophile/aisre/internal/deprecation"
)

// The allowlist file (T090, FR-059, FR-132a, FR-132b, `config/vendors.yaml`).
//
// This reads the file the repository already publishes, rather than a shape of its own. That matters
// more than it sounds: `config/vendors.yaml` is what `docs/connectors/vendor-notice.md` tells a mailbox
// owner they are approving, and a loader that needed a different file would leave the published one
// describing something nobody runs.
//
// Unknown keys are refused, so a typo is a failure rather than a setting that silently does nothing —
// the failure mode of every configuration file that ignores what it does not recognise.
//
// # Everything is configuration, and nothing is built in
//
// FR-132b: no address, domain, sender, project or product appears anywhere in this package. Adding a
// vendor is editing this file. The five provider families ship as stubs with the shape filled in and
// the identity blank, because guessing which LLM provider or which ATS an organisation uses would put a
// wrong entry in an allowlist — and a wrong entry does not fail, it silently matches nothing.
//
// # Credentials are named, never written
//
// A password or a token appears here only as the **name of an environment variable** holding it. This
// file is the sort of thing that ends up in a repository, and the first time somebody commits one with a
// password in it, the password is in the history for ever. The credential is also separate from the GCP
// feeder's (FR-002): different variables, different file, different run.
//
// # A Google Group is refused at load
//
// `kind: google_group` is a configuration that **reads nothing, for ever, without erroring**: a Group is
// a distribution list and its archive is reachable by no API (research §9). The docs say so; this
// refuses it, because a documented silent failure is still a silent failure. The remedy — a subscribed
// member's mailbox — is named in the refusal.
//
// # Extraction rules belong to the vendor
//
// What an announcement looks like is a fact about the vendor who writes them, so the rules live on the
// vendor entry. A message is attributed to a vendor by its sender first, and only that vendor's rules
// are then tried; `platform_rules` covers senders that carry notices for a platform rather than one
// product. Nothing anywhere reads prose and decides what it means (FR-073).

// Config is `config/vendors.yaml`.
type Config struct {
	// Version is the file's own version, for a future migration. It is read and not interpreted.
	Version string `yaml:"version"`
	// Org suffixes the source id: `vendor-notice:<org>`. The CLI's --org overrides it.
	Org string `yaml:"org"`
	// Cadence is how long to wait between cycles. Zero means one cycle per invocation.
	Cadence time.Duration `yaml:"cadence"`
	// ReorderingWindow overrides DefaultReorderingWindow.
	ReorderingWindow time.Duration `yaml:"reordering_window"`
	// Mailboxes are the mailboxes to read. Plural, and that is FR-132a rather than generality:
	// platform notices arrive in individuals' mailboxes and reach a shared address only where
	// somebody configured it, so per-user delivery is the shape of the file rather than a workaround.
	Mailboxes []MailboxConfig `yaml:"mailboxes"`
	// RefuseEmptyNoticeStream fails a campaign whose sources yield nothing, rather than recording
	// silence as evidence (FR-132b).
	RefuseEmptyNoticeStream bool `yaml:"refuse_empty_notice_stream"`
	// PlatformSenders carry notices for a platform rather than for one product. They are matched
	// after a vendor's own senders and are attributed to PlatformVendor.
	PlatformSenders []string `yaml:"platform_senders"`
	// PlatformVendor is the slug platform senders are attributed to. Required where PlatformSenders
	// is non-empty: a notice attributed to no vendor is a notice nobody can act on.
	PlatformVendor string `yaml:"platform_vendor"`
	// PlatformRules are the rules for those senders' messages.
	PlatformRules []RuleConfig `yaml:"platform_rules"`
	// Vendors is the allowlist, and carries each vendor's sources and rules.
	Vendors []VendorConfig `yaml:"vendors"`
	// NoticeKinds maps this file's kind names onto the published ChangeKind names. It is read for
	// documentation: the mapping in code is ChangeKindFor, and a file that disagreed with it is a
	// file that would silently mean something else.
	NoticeKinds map[string]string `yaml:"notice_kinds"`
	// Extraction carries the properties that are not settings.
	Extraction ExtractionConfig `yaml:"extraction"`
	// CaptureDeprecationHeaders turns the `Deprecation`/`Sunset` observation on the shared HTTP client
	// into a source of its own. It costs nothing — the headers arrive on responses already being read
	// — so it defaults to on.
	CaptureDeprecationHeaders *bool `yaml:"capture_deprecation_headers"`
}

// ExtractionConfig is the extraction block. Two of its three fields are **not settings**: they are
// stated so that a reader of the file sees the guarantee, and a value that contradicted the code is
// refused rather than honoured.
type ExtractionConfig struct {
	// StoreBody must be false. It is in the file so the guarantee is visible where an operator looks,
	// and `true` is refused: FR-071 is a property of the code path, and a configuration that appeared
	// to turn it off would be worse than no configuration at all.
	StoreBody bool `yaml:"store_body"`
	// OnFailure must be `report_with_pointer`. A notice whose fields cannot be extracted is reported
	// with its pointer, never dropped and never guessed at (FR-074).
	OnFailure string `yaml:"on_failure"`
	// UnknownValidStart must be `mark`. An unknown valid start is marked, never guessed (FR-069).
	UnknownValidStart string `yaml:"unknown_valid_start"`
}

// VendorConfig is one allowlist entry, with its sources and its rules.
type VendorConfig struct {
	Slug string `yaml:"slug"`
	// Family groups the stub entries by dependency category. Read for reporting only.
	Family   string   `yaml:"family"`
	Name     string   `yaml:"name"`
	Products []string `yaml:"products"`
	Hosts    []string `yaml:"hosts"`
	// Senders are the addresses that carry this vendor's notices. Matching configuration, never
	// stored: a sender address is a people identifier (FR-072, FR-134).
	Senders []string `yaml:"senders"`
	// HostProducts maps a host to the product it serves, for a `Deprecation` header that says when and
	// cannot say which product.
	HostProducts map[string]string `yaml:"host_products"`

	// StatusPage is the vendor's status page. It is polled only where StatuspageAPI says it is an
	// Atlassian Statuspage: coverage is not universal, and a page that is not one answers 404 on the
	// API paths (FR-075).
	StatusPage string `yaml:"status_page"`
	// StatuspageAPI states that StatusPage serves `/api/v2`. It is a statement about the vendor, so
	// it is configuration rather than something probed and remembered.
	StatuspageAPI bool `yaml:"statuspage_api"`
	// SkipStatusIncidents reads scheduled maintenances only.
	SkipStatusIncidents bool `yaml:"skip_status_incidents"`

	// StatusFeed is a vendor-native status document, with its format.
	StatusFeed       string `yaml:"status_feed"`
	StatusFeedFormat string `yaml:"status_feed_format"`
	// Changelog and ChangelogFeed are a page and a feed, each with its format.
	Changelog            string `yaml:"changelog"`
	ChangelogFormat      string `yaml:"changelog_format"`
	ChangelogEntryRegexp string `yaml:"changelog_entry_pattern"`
	ChangelogFeed        string `yaml:"changelog_feed"`
	ChangelogFeedFormat  string `yaml:"changelog_feed_format"`

	// Rules are how an operator states what this vendor's announcements look like. They apply to this
	// vendor's mail, feeds and pages alike, because what an announcement looks like is a fact about
	// the vendor rather than about the medium.
	Rules []RuleConfig `yaml:"rules"`
}

// MailboxConfig is one mailbox.
type MailboxConfig struct {
	// Name identifies the mailbox in a gap and in a log line. It is a label — "platform-notices",
	// "essential-contact-1" — and not an address: an address here would put a people identifier into
	// a checkpoint (FR-072, FR-134).
	Name string `yaml:"name"`
	// Address is the mailbox this reads. It is matching configuration and reaches no event.
	Address string `yaml:"address"`
	// Kind records what the address is. `google_group` is refused: a Group's archive is reachable by
	// no API, so the configuration would read nothing for ever without erroring.
	Kind string `yaml:"kind"`
	// Path is the access path: `gmail_readonly` or `imap_examine_peek`.
	Path string `yaml:"path"`
	// AuthorisedBy is who authorised the read, recorded in the campaign record (FR-140).
	AuthorisedBy string `yaml:"authorised_by"`
	// Server, Folder and User configure the IMAP path. Folder has no default: a default of INBOX
	// would point the connector at whoever's credential it was given (FR-132b).
	Server string `yaml:"server"`
	Folder string `yaml:"folder"`
	User   string `yaml:"user"`
	// PasswordEnv and TokenEnv name the environment variable holding the credential. The credential
	// itself is never in this file.
	PasswordEnv string `yaml:"password_env"`
	TokenEnv    string `yaml:"token_env"`
	// Query narrows the Gmail search, in Gmail's own language.
	Query string `yaml:"query"`
	// MaxMessages bounds one cycle on the Gmail path.
	MaxMessages int64 `yaml:"max_messages"`
	// Lookback is how far back each cycle reads. The first mailbox's value applies to all of them: the
	// lookback is a property of the cycle, and two windows would make the cycle's extent meaningless.
	Lookback time.Duration `yaml:"lookback"`
}

// The published access paths and mailbox kinds.
const (
	// PathGmailReadonly is the Gmail API with the `gmail.readonly` scope alone.
	PathGmailReadonly = "gmail_readonly"
	// PathIMAPExaminePeek is IMAP with EXAMINE and BODY.PEEK, both of which are required.
	PathIMAPExaminePeek = "imap_examine_peek"
	// KindWorkspaceMailbox is a real mailbox.
	KindWorkspaceMailbox = "workspace_mailbox"
	// KindGoogleGroup is a distribution list, whose archive no API can read.
	KindGoogleGroup = "google_group"
)

// RuleConfig is one stated extraction rule.
type RuleConfig struct {
	// Match is the expression, with the named captures `product`, `notice_id`, `start` and `end`.
	Match string `yaml:"match"`
	// Kind is what a matching entry announces. Required: there is no default, because the kind decides
	// the change kind the graph records (ADR-0006).
	Kind string `yaml:"kind"`
	// Product is the product where the expression captures none.
	Product string `yaml:"product"`
	// State makes a match a correction: cancelled, superseded or confirmed.
	State string `yaml:"state"`
	// Layouts are the time layouts to try on the captured instants.
	Layouts []string `yaml:"layouts"`
	// VagueStart states that matching entries announce no start instant, so the valid start is marked
	// unknown and the vendor's wording is not stored in its place (FR-069).
	VagueStart bool `yaml:"vague_start"`
}

// LoadConfig reads and validates the allowlist file.
func LoadConfig(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("vendornotice: reading %s: %w", path, err)
	}
	var cfg Config
	decoder := yaml.NewDecoder(strings.NewReader(string(raw)))
	// A typo must be a failure rather than a setting that silently does nothing.
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("vendornotice: decoding %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("vendornotice: %s: %w", path, err)
	}
	return cfg, nil
}

// Validate refuses a configuration that could not produce an honest run.
//
// It refuses rather than defaults everywhere a default would be a guess about somebody's estate: no
// default mailbox folder, no default vendor, no default product, no default notion of what an
// announcement looks like.
func (c Config) Validate() error {
	if len(c.Vendors) == 0 {
		return fmt.Errorf("`vendors` is empty. A run with no allowlisted vendor reads its sources and " +
			"drops everything, which is indistinguishable from a quiet week (FR-059)")
	}
	if err := c.Extraction.validate(); err != nil {
		return err
	}
	known := map[string]bool{}
	for i, vendor := range c.Vendors {
		slug := strings.ToLower(strings.TrimSpace(vendor.Slug))
		if slug == "" {
			return fmt.Errorf("`vendors[%d]` has no slug; configuration is the authority on vendor "+
				"identity, so the identity cannot be blank", i)
		}
		if known[slug] {
			return fmt.Errorf("vendor %q appears twice", slug)
		}
		known[slug] = true
		if err := vendor.validate(i); err != nil {
			return err
		}
	}
	if len(c.PlatformSenders) > 0 {
		slug := strings.ToLower(strings.TrimSpace(c.PlatformVendor))
		if slug == "" {
			return fmt.Errorf("`platform_senders` is set and `platform_vendor` is not. A notice " +
				"attributed to no vendor is a notice nobody can act on, so the file has to say which " +
				"vendor those senders write for")
		}
		if !known[slug] {
			return fmt.Errorf("`platform_vendor` is %q, which is not in `vendors`", c.PlatformVendor)
		}
	}
	names := map[string]bool{}
	for i, mailbox := range c.Mailboxes {
		if err := mailbox.validate(i); err != nil {
			return err
		}
		if names[mailbox.Name] {
			return fmt.Errorf("two mailboxes are both called %q", mailbox.Name)
		}
		names[mailbox.Name] = true
	}
	return nil
}

func (e ExtractionConfig) validate() error {
	if e.StoreBody {
		return fmt.Errorf("`extraction.store_body` is true. The body is never written — not redacted, " +
			"not truncated, not hashed (FR-071) — and that is a property of the code path rather than " +
			"a setting. A configuration that appeared to turn it off would be worse than none, so it " +
			"is refused here")
	}
	if on := strings.TrimSpace(e.OnFailure); on != "" && on != "report_with_pointer" {
		return fmt.Errorf("`extraction.on_failure` is %q; the only behaviour is report_with_pointer, "+
			"because a notice whose fields cannot be extracted is reported with its pointer and never "+
			"dropped or guessed at (FR-074)", on)
	}
	if start := strings.TrimSpace(e.UnknownValidStart); start != "" && start != "mark" {
		return fmt.Errorf("`extraction.unknown_valid_start` is %q; an unknown valid start is marked "+
			"and never guessed (FR-069)", start)
	}
	return nil
}

func (v VendorConfig) validate(index int) error {
	where := fmt.Sprintf("`vendors[%d]` (%s)", index, v.Slug)
	if v.StatuspageAPI && strings.TrimSpace(v.StatusPage) == "" {
		return fmt.Errorf("%s says statuspage_api and names no status_page", where)
	}
	for _, pair := range []struct{ what, url, format string }{
		{"status_feed", v.StatusFeed, v.StatusFeedFormat},
		{"changelog_feed", v.ChangelogFeed, v.ChangelogFeedFormat},
		{"changelog", v.Changelog, v.ChangelogFormat},
	} {
		if strings.TrimSpace(pair.url) == "" {
			continue
		}
		if strings.TrimSpace(pair.format) == "" {
			// Not polled rather than guessed. A document whose format nobody stated is one this
			// feeder does not know how to read, and reading it as a guess would place entries from a
			// structure it never verified.
			continue
		}
		if !knownChangelogFormat(pair.format) {
			return fmt.Errorf("%s names %s_format %q, which is not one of %s, %s, %s, %s or %s",
				where, pair.what, pair.format,
				FormatAtom, FormatRSS, FormatJSONFeed, FormatGCPIncidents, FormatHTML)
		}
		if ChangelogFormat(pair.format) == FormatHTML && strings.TrimSpace(v.ChangelogEntryRegexp) == "" {
			return fmt.Errorf("%s reads %s as html and states no changelog_entry_pattern. A page has "+
				"no entry boundary a parser can know, so the boundary is stated rather than guessed",
				where, pair.what)
		}
	}
	return nil
}

func knownChangelogFormat(format string) bool {
	switch ChangelogFormat(strings.ToLower(strings.TrimSpace(format))) {
	case FormatAtom, FormatRSS, FormatJSONFeed, FormatGCPIncidents, FormatHTML:
		return true
	default:
		return false
	}
}

func (m MailboxConfig) validate(index int) error {
	where := fmt.Sprintf("`mailboxes[%d]`", index)
	if strings.TrimSpace(m.Name) == "" {
		return fmt.Errorf("%s has no name; a gap has to say which mailbox could not be read, and "+
			"\"the mailbox\" is not an answer when notices arrive in several (FR-132a)", where)
	}
	switch strings.ToLower(strings.TrimSpace(m.Kind)) {
	case KindWorkspaceMailbox:
	case KindGoogleGroup:
		return fmt.Errorf("%s is a %s. A Group is a distribution list and its archive is reachable "+
			"by no API, so this configuration would read nothing for ever without erroring. Configure "+
			"a subscribed member's mailbox instead (research §9)", where, KindGoogleGroup)
	case "":
		return fmt.Errorf("%s does not say what kind of address it is. It matters and it is silent "+
			"when wrong: %s is readable, %s is not readable by anything", where,
			KindWorkspaceMailbox, KindGoogleGroup)
	default:
		return fmt.Errorf("%s has kind %q, which is neither %s nor %s", where, m.Kind,
			KindWorkspaceMailbox, KindGoogleGroup)
	}
	switch strings.ToLower(strings.TrimSpace(m.Path)) {
	case PathGmailReadonly:
		if strings.TrimSpace(m.Address) == "" {
			return fmt.Errorf("%s reads over %s and names no address. It must be a real user "+
				"account: no API can read a Group's archive, and a Group answers with nothing, which "+
				"looks exactly like a quiet week (research §9)", where, PathGmailReadonly)
		}
	case PathIMAPExaminePeek:
		if strings.TrimSpace(m.Server) == "" || strings.TrimSpace(m.User) == "" {
			return fmt.Errorf("%s reads over %s and needs a server and a user", where, PathIMAPExaminePeek)
		}
		if strings.TrimSpace(m.Folder) == "" {
			return fmt.Errorf("%s names no folder. There is no default: a default of INBOX would "+
				"point the connector at whoever's credential it was given rather than at the folder "+
				"somebody configured (FR-132b)", where)
		}
		if strings.TrimSpace(m.PasswordEnv) == "" && strings.TrimSpace(m.TokenEnv) == "" {
			return fmt.Errorf("%s names no credential variable. The credential is named here and read "+
				"from the environment, never written in this file: a config with a password in it "+
				"puts the password in the repository's history for ever", where)
		}
	case "":
		return fmt.Errorf("%s names no access path; it is %s or %s, and the read-only guarantee "+
			"differs between them", where, PathGmailReadonly, PathIMAPExaminePeek)
	default:
		return fmt.Errorf("%s has path %q, which is neither %s nor %s", where, m.Path,
			PathGmailReadonly, PathIMAPExaminePeek)
	}
	return nil
}

// Allowlist builds the filter from the configured vendors.
func (c Config) Allowlist() (*Allowlist, error) {
	vendors := make([]Vendor, 0, len(c.Vendors))
	platform := strings.ToLower(strings.TrimSpace(c.PlatformVendor))
	for _, vendor := range c.Vendors {
		senders := vendor.Senders
		if platform != "" && strings.EqualFold(strings.TrimSpace(vendor.Slug), platform) {
			// The platform senders are that vendor's senders as far as matching is concerned. Keeping
			// them in a separate list in the file is about documentation — they carry notices for the
			// platform rather than one product — and the allowlist has one index either way.
			senders = append(append([]string(nil), senders...), c.PlatformSenders...)
		}
		vendors = append(vendors, Vendor{
			Slug: vendor.Slug, Name: vendor.Name, Products: vendor.Products,
			Hosts: vendor.Hosts, Senders: senders, HostProducts: vendor.HostProducts,
		})
	}
	return NewAllowlist(vendors)
}

// compileRules compiles the stated rules of one source.
func compileRules(where string, configs []RuleConfig) ([]EntryRule, error) {
	rules := make([]EntryRule, 0, len(configs))
	for i, rule := range configs {
		expression, err := regexp.Compile(rule.Match)
		if err != nil {
			return nil, fmt.Errorf("vendornotice: %s rule %d: %w", where, i, err)
		}
		kind := AnnouncementKind(strings.ToLower(strings.TrimSpace(rule.Kind)))
		if !kind.Valid() {
			return nil, fmt.Errorf("vendornotice: %s rule %d announces kind %q, which is not one this "+
				"feeder publishes; there is no default, because the kind decides the change kind the "+
				"graph records (ADR-0006)", where, i, rule.Kind)
		}
		rules = append(rules, EntryRule{
			Match: expression, Kind: kind, Product: rule.Product,
			State: rule.State, Layouts: rule.Layouts, VagueStart: rule.VagueStart,
		})
	}
	return rules, nil
}

// HTTPSources carries the shared HTTP client and the log its transport fills, so that every HTTP source
// polls through one client and one header observation rather than each building its own.
type HTTPSources struct {
	Client *Client
	Log    *deprecation.Log
}

// NewHTTPSources builds the pair.
func NewHTTPSources(opts ClientOptions) *HTTPSources {
	log := deprecation.NewLog()
	opts.Deprecations = log
	return &HTTPSources{Client: NewClient(opts), Log: log}
}

// Sources builds every configured source.
//
// The environment is read here and nowhere else in this package, and what it holds is passed straight
// into the session rather than kept: no type in this package has a field holding a credential once the
// session is open.
func (c Config) Sources(list *Allowlist, http *HTTPSources) ([]Announcer, error) {
	var sources []Announcer
	mailbox, err := c.mailboxSource(list)
	if err != nil {
		return nil, err
	}
	if mailbox != nil {
		sources = append(sources, mailbox)
	}
	for _, vendor := range c.Vendors {
		rules, err := compileRules("vendor "+vendor.Slug, vendor.Rules)
		if err != nil {
			return nil, err
		}
		if vendor.StatuspageAPI {
			source, err := NewStatuspage(StatuspageOptions{
				Client: http.Client, Allowlist: list, Vendor: vendor.Slug,
				BaseURL: vendor.StatusPage, SkipIncidents: vendor.SkipStatusIncidents,
			})
			if err != nil {
				return nil, err
			}
			sources = append(sources, source)
		}
		for _, document := range []struct{ url, format string }{
			{vendor.StatusFeed, vendor.StatusFeedFormat},
			{vendor.ChangelogFeed, vendor.ChangelogFeedFormat},
			{vendor.Changelog, vendor.ChangelogFormat},
		} {
			if strings.TrimSpace(document.url) == "" || strings.TrimSpace(document.format) == "" {
				continue
			}
			opts := ChangelogOptions{
				Client: http.Client, Vendor: vendor.Slug, URL: document.url,
				Format: ChangelogFormat(strings.ToLower(strings.TrimSpace(document.format))),
				Rules:  rules,
			}
			if opts.Format == FormatHTML {
				pattern, err := regexp.Compile(vendor.ChangelogEntryRegexp)
				if err != nil {
					return nil, fmt.Errorf("vendornotice: vendor %s changelog_entry_pattern: %w",
						vendor.Slug, err)
				}
				opts.EntryPattern = pattern
			}
			source, err := NewChangelog(opts)
			if err != nil {
				return nil, err
			}
			sources = append(sources, source)
		}
	}
	// The deprecation-header source observes headers on requests the *other* sources make. On its own
	// it has no traffic to watch, so a configuration whose only source is this one is a configuration
	// that can never report anything — which is the empty-cycle failure wearing a source's name. It is
	// added only where something else polls.
	polling := len(sources) > 0
	if polling && (c.CaptureDeprecationHeaders == nil || *c.CaptureDeprecationHeaders) {
		sources = append(sources, NewDeprecationHeaders(http.Log, list))
	}
	if !polling {
		return nil, fmt.Errorf("vendornotice: the configuration names no source this feeder can read: " +
			"no mailbox, no Atlassian Statuspage, and no document whose format it states. A run with " +
			"no sources reports an unbroken series of empty cycles, which is the most convincing " +
			"impression of a working feeder there is (FR-058)")
	}
	return sources, nil
}

// mailboxSource builds the one mailbox source over every configured mailbox.
func (c Config) mailboxSource(list *Allowlist) (*Mailbox, error) {
	if len(c.Mailboxes) == 0 {
		return nil, nil
	}
	byVendor := map[string][]EntryRule{}
	for _, vendor := range c.Vendors {
		rules, err := compileRules("vendor "+vendor.Slug, vendor.Rules)
		if err != nil {
			return nil, err
		}
		if len(rules) > 0 {
			byVendor[strings.ToLower(strings.TrimSpace(vendor.Slug))] = rules
		}
	}
	platform, err := compileRules("platform", c.PlatformRules)
	if err != nil {
		return nil, err
	}
	mailboxes := make([]NamedMailbox, 0, len(c.Mailboxes))
	for _, configured := range c.Mailboxes {
		open, err := openMailbox(configured)
		if err != nil {
			return nil, err
		}
		mailboxes = append(mailboxes, NamedMailbox{Name: configured.Name, Open: open})
	}
	return NewMailbox(MailboxOptions{
		Mailboxes: mailboxes, Allowlist: list,
		RulesByVendor: byVendor, Rules: platform,
		Lookback: c.Mailboxes[0].Lookback,
	})
}

// openMailbox builds the `Open` for whichever path one mailbox configured, reading the credential from
// the environment the file named.
func openMailbox(c MailboxConfig) (func(ctx context.Context) (MessageReader, error), error) {
	switch strings.ToLower(strings.TrimSpace(c.Path)) {
	case PathIMAPExaminePeek:
		auth := IMAPAuth{User: c.User}
		if name := strings.TrimSpace(c.PasswordEnv); name != "" {
			auth.Password = os.Getenv(name)
			if auth.Password == "" {
				return nil, fmt.Errorf("vendornotice: %s is empty, so the credential the "+
					"configuration names for mailbox %q is not set in this environment", name, c.Name)
			}
		}
		if name := strings.TrimSpace(c.TokenEnv); name != "" {
			auth.AccessToken = os.Getenv(name)
			if auth.AccessToken == "" {
				return nil, fmt.Errorf("vendornotice: %s is empty, so the token the configuration "+
					"names for mailbox %q is not set in this environment", name, c.Name)
			}
		}
		return IMAPMailbox(IMAPOptions{Address: c.Server, Mailbox: c.Folder, Auth: auth}), nil
	case PathGmailReadonly:
		// The authorised client is the deployment's to build, from ambient credentials scoped to
		// GmailReadonlyScope and nothing else. This file names the mailbox; it never holds a token.
		return nil, fmt.Errorf("vendornotice: mailbox %q reads over %s, whose authorised client the "+
			"CLI builds from the ambient credentials rather than from this file. Pass one with "+
			"GmailMailbox, or use %s", c.Name, PathGmailReadonly, PathIMAPExaminePeek)
	default:
		return nil, fmt.Errorf("vendornotice: mailbox %q has no access path", c.Name)
	}
}
