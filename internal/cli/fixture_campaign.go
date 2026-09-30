// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Pierre-Theophile/aisre/internal/campaign"
	"github.com/Pierre-Theophile/aisre/internal/fixture"
	"github.com/Pierre-Theophile/aisre/internal/sanitise"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/record"
)

// `fixture campaign` (T153; FR-129–FR-132, FR-134, FR-138–FR-140; contracts/sanitisation.md §6).
//
// Five subcommands over one campaign directory, and the order they run in is the order the contract's
// commit gates are listed in: `record`, then the recording itself with `feed … --record`, then
// `sanitise`, `scan`, `sign` and `parity`.
//
// ---------------------------------------------------------------------------------------------
// What `sanitise` is not
//
// It is not the first cleaning, and a reader who assumed it was would have the security model
// backwards. FR-137 puts sanitisation **in the connector, before anything touches disk** — by the
// time a campaign directory exists, the cleaning has either happened or the recording is already
// unsafe and cannot be made safe, because the bytes were written. So `sanitise` *asserts* that what
// is on disk is clean and stamps the policy version it was cleaned under. That is a real gate; it is
// just a different one from the name's first reading, which is why the command says so.
//
// # Why `scan` does not claim to satisfy FR-138
//
// FR-138 requires the independent scan to be **implemented separately from the sanitiser, so that
// one defect cannot both leak and pass**. This binary contains the sanitiser. A scan run from inside
// it shares every defect the sanitiser has, by construction — so `scan` runs the in-process check and
// then tells the operator to run `scripts/check-no-secrets.sh`, naming it. A command that reported
// "independent scan passed" from in-process would be the exact failure FR-138 is written against.
//
// # Why the campaign directory is also the recording directory
//
// `feed gcp --record <campaign-dir>` and `feed vendor-notice --record <campaign-dir>` both record
// into one directory (quickstart §7), because two connectors recording one campaign are one arrival
// stream — the same reason a fixture's `events.jsonl` is shared. So a campaign directory normally
// holds `campaign.yaml` beside a recording. A campaign split into several windows holds those as
// subdirectories instead, and `recordingsIn` accepts either.

func newFixtureCampaignCommand(global *globalOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "campaign",
		Short: "Write and read a recording campaign record, and run its commit gates",
		Long: "A campaign record states the scope in force throughout the campaign, the mailbox\n" +
			"access path and who authorised it, and the campaign's project and region scope.\n\n" +
			"Recording begins before the scope is finally agreed (FR-130), so the record carries\n" +
			"the scope in force rather than one scope for the whole campaign: that is what lets a\n" +
			"later reader tell \"not present\" from \"not in scope at that time\".\n\n" +
			"The gates run in the order contracts/sanitisation.md §6 lists them:\n" +
			"  record    write or extend the campaign record\n" +
			"  sanitise  assert the recording is clean and stamp the policy version\n" +
			"  scan      the in-process personal-data and canary check, plus what to run next\n" +
			"  sign      write or countersign the signed manifest (FR-140)\n" +
			"  verify    the commit gate: every recording carries a signed, current manifest\n" +
			"  parity    compare a live run against the recording of the same run (FR-143)",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error { return c.Help() },
	}
	cmd.AddCommand(
		newCampaignRecordCommand(global),
		newCampaignSanitiseCommand(global),
		newCampaignScanCommand(global),
		newCampaignSignCommand(global),
		newCampaignVerifyCommand(global),
		newCampaignParityCommand(global),
	)
	return cmd
}

// ---- record ----------------------------------------------------------------------------------

type campaignRecordOptions struct {
	id           string
	org          string
	startedAt    string
	projects     []string
	regions      []string
	mailboxPath  string
	authorisedBy string
	authorisedOn string
	mailboxNote  string
	senders      []string
	signatories  []string
	policy       string
	note         string
	// addScope closes the open window at this instant and opens a new one with the projects and
	// regions given. It is the whole reason `record` is not write-once: FR-130's scope changes
	// mid-campaign, and closing one window without opening the next leaves a record that answers
	// "not in scope" for an instant nobody decided about.
	addScope string
	scopeWhy string
	endedAt  string
	// The Datadog scope of the window, for a campaign recording that connector (005 T082). A window
	// names projects, a Datadog scope, or both.
	ddSite    string
	ddEnvs    []string
	ddSources []string
	ddIndexes []string
}

// datadogScope is the window's Datadog scope, or nil when no Datadog flag was given. A partial one is
// kept rather than dropped, so the record's own validation names what is missing.
func (o campaignRecordOptions) datadogScope() *campaign.DatadogScope {
	if o.ddSite == "" && len(o.ddEnvs) == 0 && len(o.ddSources) == 0 && len(o.ddIndexes) == 0 {
		return nil
	}
	return &campaign.DatadogScope{Site: o.ddSite, Environments: o.ddEnvs, LogSources: o.ddSources, Indexes: o.ddIndexes}
}

func newCampaignRecordCommand(global *globalOptions) *cobra.Command {
	var opts campaignRecordOptions
	cmd := &cobra.Command{
		Use:   "record <dir>",
		Short: "Write or extend the campaign record",
		Long: "record writes `<dir>/campaign.yaml` the first time and extends it after.\n\n" +
			"--add-scope closes the open scope window at that instant and opens a new one, which is\n" +
			"the two halves of a scope change made in one step: doing only the first leaves a record\n" +
			"whose windows are no longer continuous, and a gap answers \"not in scope\" for an instant\n" +
			"nobody ever decided about (FR-130).\n\n" +
			"No mailbox address is recorded. The access path, the authoriser and the sender allowlist\n" +
			"are what a reviewer needs; the address is a people identifier under FR-135.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCampaignRecord(cmd, global, args[0], opts)
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.id, "id", "", "campaign id, conventionally campaign-<date> (default: the directory name)")
	f.StringVar(&opts.org, "org", "", "organisation slug the source ids carry (required on a new record)")
	f.StringVar(&opts.startedAt, "started-at", "", "when recording began, RFC 3339 (required on a new record; FR-130 wants it before the scope was agreed)")
	f.StringSliceVar(&opts.projects, "projects", nil, "projects in the scope window (required unless the window has a Datadog scope; no code may assume a project name — FR-131)")
	f.StringVar(&opts.ddSite, "datadog-site", "", "Datadog site of the scope window, e.g. datadoghq.eu (005 FR-007)")
	f.StringSliceVar(&opts.ddEnvs, "datadog-env", nil, "environments in the window's Datadog scope; required with --datadog-site, since an empty list would read as \"all\"")
	f.StringSliceVar(&opts.ddSources, "datadog-watch", nil, "watched log sources in the window's Datadog scope, as the feeder's --watch takes them")
	f.StringSliceVar(&opts.ddIndexes, "datadog-index", nil, "log indexes searched; empty means the default")
	f.StringSliceVar(&opts.regions, "regions", nil, "regions in the scope window; empty means ALL regions, which is the first campaign's scope (FR-131)")
	f.StringVar(&opts.mailboxPath, "mailbox-path", "", "how the notice mailbox is read: "+accessPathList())
	f.StringVar(&opts.authorisedBy, "authorised-by", "", "named individual who authorised the mailbox read (FR-132)")
	f.StringVar(&opts.authorisedOn, "authorised-on", "", "date they authorised it, RFC 3339")
	f.StringVar(&opts.mailboxNote, "mailbox-note", "", "what a reviewer needs that the path does not say; required for subscribed_member, which reaches a person's whole mailbox")
	f.StringSliceVar(&opts.senders, "sender", nil, "configured sender whose notices are read; repeatable (FR-132b forbids hard-coding one)")
	f.StringSliceVar(&opts.signatories, "signatory", nil, "named individual who may sign a recording (FR-140); repeatable, and the count decides whether a second signature is required")
	f.StringVar(&opts.policy, "policy-version", sanitise.PolicyVersion, "sanitisation contract version the campaign runs under (FR-134)")
	f.StringVar(&opts.note, "note", "", "free text for the reviewer")
	f.StringVar(&opts.addScope, "add-scope", "", "close the open scope window at this instant and open a new one with --projects and --regions, RFC 3339")
	f.StringVar(&opts.scopeWhy, "scope-why", "", "what changed and why, recorded on the new window")
	f.StringVar(&opts.endedAt, "ended-at", "", "close the campaign at this instant, RFC 3339")
	return cmd
}

func runCampaignRecord(cmd *cobra.Command, global *globalOptions, dir string, opts campaignRecordOptions) error {
	if campaign.Exists(dir) {
		return extendCampaignRecord(cmd, global, dir, opts)
	}
	if opts.addScope != "" {
		return exitErrorf(ExitUsage, "fixture campaign record: --add-scope needs an existing record; "+
			"%s has none, so there is no open window to close", dir)
	}

	startedAt, err := parseInstant("--started-at", opts.startedAt)
	if err != nil {
		return err
	}
	if startedAt.IsZero() {
		return exitErrorf(ExitUsage, "fixture campaign record: --started-at is required on a new "+
			"record; FR-130's point is that recording began before the scope was agreed, and without "+
			"the instant nobody can tell whether it did")
	}
	authorisedOn, err := parseInstant("--authorised-on", opts.authorisedOn)
	if err != nil {
		return err
	}
	id := opts.id
	if id == "" {
		id = filepath.Base(filepath.Clean(dir))
	}

	r := campaign.Record{
		ID:           id,
		Organisation: opts.org,
		StartedAt:    startedAt,
		Scopes: []campaign.Scope{{
			From:     startedAt,
			Projects: opts.projects,
			Regions:  opts.regions,
			Datadog:  opts.datadogScope(),
			Why:      firstNonEmpty(opts.scopeWhy, "the scope in force when recording began (FR-130)"),
		}},
		Mailbox: campaign.Mailbox{
			Path:            campaign.AccessPath(opts.mailboxPath),
			AuthorisedBy:    opts.authorisedBy,
			AuthorisedOn:    authorisedOn,
			Note:            opts.mailboxNote,
			SenderAllowlist: opts.senders,
		},
		MailboxAddress: campaign.NoAddressRecorded,
		Signatories:    opts.signatories,
		PolicyVersion:  opts.policy,
		Note:           opts.note,
	}
	if err := campaign.Write(dir, r); err != nil {
		return exitErrorf(ExitUsage, "fixture campaign record: %v", err)
	}
	return renderCampaignRecord(cmd, global, dir, r, "written")
}

func extendCampaignRecord(cmd *cobra.Command, global *globalOptions, dir string, opts campaignRecordOptions) error {
	addAt, err := parseInstant("--add-scope", opts.addScope)
	if err != nil {
		return err
	}
	endedAt, err := parseInstant("--ended-at", opts.endedAt)
	if err != nil {
		return err
	}
	if addAt.IsZero() && endedAt.IsZero() && len(opts.signatories) == 0 {
		return exitErrorf(ExitUsage, "fixture campaign record: %s already has a record and this "+
			"invocation changes nothing. Pass --add-scope to change the scope, --ended-at to close "+
			"the campaign, or --signatory to add one. The record is not replaced, because the "+
			"windows it already holds were true when they were written (FR-130)", dir)
	}

	err = campaign.Update(dir, func(r *campaign.Record) error {
		if !addAt.IsZero() {
			if len(opts.projects) == 0 && opts.datadogScope() == nil {
				return fmt.Errorf("--add-scope needs --projects or a Datadog scope; a window naming " +
					"neither is a record that says nothing rather than one meaning \"all\" (FR-131)")
			}
			last := len(r.Scopes) - 1
			if !r.Scopes[last].To.IsZero() {
				return fmt.Errorf("the last scope window already closed at %s, so there is no open "+
					"window to change; --add-scope closes the open one and opens the next",
					r.Scopes[last].To.Format(time.RFC3339))
			}
			if !addAt.After(r.Scopes[last].From) {
				return fmt.Errorf("--add-scope %s is at or before the open window's start (%s)",
					addAt.Format(time.RFC3339), r.Scopes[last].From.Format(time.RFC3339))
			}
			r.Scopes[last].To = addAt
			r.Scopes = append(r.Scopes, campaign.Scope{
				From:     addAt,
				Projects: opts.projects,
				Regions:  opts.regions,
				Datadog:  opts.datadogScope(),
				Why:      opts.scopeWhy,
			})
		}
		if !endedAt.IsZero() {
			r.EndedAt = endedAt
			last := len(r.Scopes) - 1
			if r.Scopes[last].To.IsZero() {
				r.Scopes[last].To = endedAt
			}
		}
		for _, who := range opts.signatories {
			if !containsFold(r.Signatories, who) {
				r.Signatories = append(r.Signatories, who)
			}
		}
		return nil
	})
	if err != nil {
		return exitErrorf(ExitUsage, "fixture campaign record: %v", err)
	}
	r, err := campaign.Read(dir)
	if err != nil {
		return exitErrorf(ExitUsage, "fixture campaign record: %v", err)
	}
	return renderCampaignRecord(cmd, global, dir, r, "extended")
}

func renderCampaignRecord(cmd *cobra.Command, global *globalOptions, dir string, r campaign.Record, what string) error {
	p := newPrinter(cmd.OutOrStdout(), global.Output)
	if p.json() {
		return p.writeJSON(map[string]any{
			"campaign": r.ID, "dir": dir, "outcome": what,
			"scopes": len(r.Scopes), "signatories": r.Signatories,
			"policy_version": r.PolicyVersion,
		})
	}
	if err := p.writeLine("## %s — %s\n", r.ID, what); err != nil {
		return err
	}
	rows := make([][]string, 0, len(r.Scopes))
	for _, s := range r.Scopes {
		to := "open"
		if !s.To.IsZero() {
			to = s.To.Format(time.RFC3339)
		}
		regions := strings.Join(s.Regions, ",")
		if s.AllRegions() {
			regions = "all"
		}
		if len(s.Projects) == 0 {
			regions = ""
		}
		datadog := ""
		if s.Datadog != nil {
			datadog = s.Datadog.Site + " " + strings.Join(s.Datadog.Environments, ",")
		}
		rows = append(rows, []string{s.From.Format(time.RFC3339), to, strings.Join(s.Projects, ","), regions, datadog, s.Why})
	}
	if err := p.writeTable([]string{"FROM", "TO", "PROJECTS", "REGIONS", "DATADOG", "WHY"}, rows); err != nil {
		return err
	}
	mailbox := "none: no window names a project, so no notice mailbox is read"
	if r.Mailbox.Path != "" {
		mailbox = fmt.Sprintf("%s, authorised by %s on %s", r.Mailbox.Path, r.Mailbox.AuthorisedBy,
			r.Mailbox.AuthorisedOn.Format("2006-01-02"))
	}
	return p.writeLine("\nmailbox   %s\nsignatories %s\npolicy    %s",
		mailbox, strings.Join(r.Signatories, ", "), r.PolicyVersion)
}

// ---- sanitise --------------------------------------------------------------------------------

func newCampaignSanitiseCommand(global *globalOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "sanitise <dir>",
		Short: "Assert the recording is clean and stamp the policy version",
		Long: "sanitise is NOT the first cleaning, and reading it that way has the security model\n" +
			"backwards. FR-137 puts sanitisation in the connector, before anything touches disk: by\n" +
			"the time a campaign directory exists the cleaning has either happened or the recording\n" +
			"is already unsafe and cannot be made safe, because the bytes were written.\n\n" +
			"So this asserts that every committed byte passes the boundary check and records the\n" +
			"policy version the recording was cleaned under (FR-134). It is a real gate; it is a\n" +
			"different one from the name's first reading.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCampaignAssert(cmd, global, args[0], "sanitise")
		},
	}
}

// ---- scan ------------------------------------------------------------------------------------

func newCampaignScanCommand(global *globalOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "scan <dir>",
		Short: "The in-process personal-data and canary check, plus what to run next",
		Long: "scan runs the in-process check over every committed byte: a people-shaped value that\n" +
			"is not a documentation placeholder, and any surviving canary token (FR-139).\n\n" +
			"It does NOT satisfy FR-138. That requires the scan to be implemented separately from\n" +
			"the sanitiser, so one defect cannot both leak and pass — and this binary contains the\n" +
			"sanitiser, so a scan run from inside it shares every defect the sanitiser has. The\n" +
			"independent scan is scripts/check-no-secrets.sh, and this command names it rather than\n" +
			"reporting a gate it cannot be.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCampaignAssert(cmd, global, args[0], "scan")
		},
	}
}

// runCampaignAssert is `sanitise` and `scan`: the same walk, reported differently.
//
// They share an implementation on purpose. Both ask "is there anything in these bytes that must not
// be committed", and two walks that could disagree would be two answers to one question — the
// operator would then have to decide which to believe, which is worse than one answer with its limits
// stated.
func runCampaignAssert(cmd *cobra.Command, global *globalOptions, dir, mode string) error {
	r, err := campaign.Read(dir)
	if err != nil {
		return exitErrorf(ExitUsage, "fixture campaign %s: %v", mode, err)
	}
	// The people check needs no key, and that matters operationally: this gate has to run in CI,
	// where the corpus key is deliberately absent (FR-135 forbids committing it). With a key in the
	// environment the canary check can also say WHICH campaign planted a survivor; without one a
	// surviving token is still a failure, just an unattributed one. The report says which ran, so
	// nobody reads the weaker check as the stronger.
	key, keyErr := sanitise.KeyFromEnv()
	canaries := sanitise.NewCanarySet(key)
	keyed := keyErr == nil && key.Configured()

	recordings, err := recordingsIn(dir)
	if err != nil {
		return exitErrorf(ExitUsage, "fixture campaign %s: %v", mode, err)
	}

	var checked int
	var findings []string
	for _, rec := range recordings {
		_, files, err := sanitise.HashRecording(rec)
		if err != nil {
			return exitErrorf(ExitUsage, "fixture campaign %s: %v", mode, err)
		}
		for _, f := range files {
			rel := f.Path
			// campaign.yaml carries the authoriser's name, which is a person by design: FR-132
			// requires it. Asserting the people-check over it would refuse the one file that has
			// to name somebody.
			if rel == campaign.RecordFile || rel == sanitise.ManifestFile {
				continue
			}
			body, err := os.ReadFile(filepath.Join(rec, filepath.FromSlash(rel))) //nolint:gosec // a path from the walk
			if err != nil {
				return exitErrorf(ExitUsage, "fixture campaign %s: read %s: %v", mode, rel, err)
			}
			checked++
			where := filepath.Join(filepath.Base(rec), rel)
			if finding := campaignFindingIn(where, body, canaries, keyed); finding != "" {
				findings = append(findings, finding)
			}
		}
	}

	p := newPrinter(cmd.OutOrStdout(), global.Output)
	if p.json() {
		if err := p.writeJSON(map[string]any{
			"campaign": r.ID, "mode": mode, "files_checked": checked,
			"findings": findings, "policy_version": r.PolicyVersion,
			"satisfies_fr_138":   false,
			"independent_scan":   "scripts/check-no-secrets.sh",
			"canary_attribution": keyed,
		}); err != nil {
			return err
		}
	} else {
		if err := p.writeLine("## %s — %s\n\n- files checked: %d\n- policy version: %s\n- findings: %d",
			r.ID, mode, checked, r.PolicyVersion, len(findings)); err != nil {
			return err
		}
		for _, f := range findings {
			if err := p.writeLine("  - %s", f); err != nil {
				return err
			}
		}
		attribution := "unattributed: no corpus key in the environment, so a surviving canary is a " +
			"failure but not traceable to its campaign"
		if keyed {
			attribution = "attributed: a surviving canary names the campaign that planted it"
		}
		if err := p.writeLine("\ncanaries  %s\n\nThis is the in-process check and shares the "+
			"sanitiser's defects by construction.\nFR-138's independent scan is "+
			"`scripts/check-no-secrets.sh`; run it too.", attribution); err != nil {
			return err
		}
	}
	if len(findings) > 0 {
		return exitErrorf(ExitRejected, "fixture campaign %s: %d finding(s); nothing is committed "+
			"on a promise to clean it later (FR-140)", mode, len(findings))
	}
	if checked == 0 {
		return exitErrorf(ExitUsage, "fixture campaign %s: %s holds no recorded file to check, so "+
			"the gate held over nothing", mode, dir)
	}
	return nil
}

// ---- sign ------------------------------------------------------------------------------------

func newCampaignSignCommand(global *globalOptions) *cobra.Command {
	var (
		signer string
		on     string
		note   string
	)
	cmd := &cobra.Command{
		Use:   "sign <dir>",
		Short: "Write or countersign the signed manifest (FR-140)",
		Long: "sign writes `sanitisation.yaml` for each recording in the campaign: the policy version,\n" +
			"the content hash of what was signed, the named signatory, the date and what was dropped.\n\n" +
			"A signature here is an attestation — a name and a date. Nothing is verified against a\n" +
			"key, and the written file says so, because a reader who took it for cryptography would\n" +
			"trust it more than it deserves. What it records is that somebody read this.\n\n" +
			"Running it again on a signed recording COUNTERSIGNS: the content hash is re-checked\n" +
			"first, because a second signature on content that changed since the first reads as two\n" +
			"reviews of one recording when it was two different recordings.\n\n" +
			"Where the campaign declares more than one signatory, one signature is refused.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCampaignSign(cmd, global, args[0], signer, on, note)
		},
	}
	cmd.Flags().StringVar(&signer, "signer", "", "the named individual signing (required; must be one of the campaign's signatories)")
	cmd.Flags().StringVar(&on, "on", "", "the date they signed, RFC 3339 (default: today)")
	cmd.Flags().StringVar(&note, "note", "", "what they want recorded about what they signed — typically the disclosure they accepted (§4's unfuzzed timestamps)")
	return cmd
}

func runCampaignSign(cmd *cobra.Command, global *globalOptions, dir, signer, on, note string) error {
	r, err := campaign.Read(dir)
	if err != nil {
		return exitErrorf(ExitUsage, "fixture campaign sign: %v", err)
	}
	if strings.TrimSpace(signer) == "" {
		return exitErrorf(ExitUsage, "fixture campaign sign: --signer is required; FR-140 wants a "+
			"NAMED individual, and the campaign declares %s", strings.Join(r.Signatories, ", "))
	}
	// The signer has to be one the campaign declared. Otherwise the second-signature rule is
	// satisfiable by naming anybody, which makes the count meaningless.
	if !containsFold(r.Signatories, signer) {
		return exitErrorf(ExitUsage, "fixture campaign sign: %q is not one of the campaign's "+
			"signatories (%s). The second-signature rule counts the declared signatories, so a "+
			"signer from outside that list would make the count mean nothing — add them with "+
			"`record --signatory` if they should be there", signer, strings.Join(r.Signatories, ", "))
	}
	signedOn, err := parseInstant("--on", on)
	if err != nil {
		return err
	}
	if signedOn.IsZero() {
		signedOn = time.Now().UTC().Truncate(24 * time.Hour)
	}
	sig := sanitise.Signature{By: signer, On: signedOn, Note: note}

	recordings, err := recordingsIn(dir)
	if err != nil {
		return exitErrorf(ExitUsage, "fixture campaign sign: %v", err)
	}

	p := newPrinter(cmd.OutOrStdout(), global.Output)
	type outcome struct {
		Recording string `json:"recording"`
		Action    string `json:"action"`
		Hash      string `json:"content_hash"`
	}
	results := make([]outcome, 0, len(recordings))
	for _, rec := range recordings {
		rel, relErr := filepath.Rel(dir, rec)
		if relErr != nil {
			rel = rec
		}
		if sanitise.HasManifest(rec) {
			if err := sanitise.Countersign(rec, sig, len(r.Signatories)); err != nil {
				return exitErrorf(ExitRejected, "fixture campaign sign: %s: %v", rel, err)
			}
			m, err := sanitise.ReadManifest(rec)
			if err != nil {
				return exitErrorf(ExitUsage, "fixture campaign sign: %s: %v", rel, err)
			}
			results = append(results, outcome{Recording: rel, Action: "countersigned", Hash: m.ContentHash})
			continue
		}
		m, err := sanitise.NewManifest(rec, r.PolicyVersion, []sanitise.Signature{sig}, nil, nil, 0)
		if err != nil {
			return exitErrorf(ExitUsage, "fixture campaign sign: %s: %v", rel, err)
		}
		if err := sanitise.WriteManifest(rec, m, len(r.Signatories)); err != nil {
			return exitErrorf(ExitRejected, "fixture campaign sign: %s: %v", rel, err)
		}
		results = append(results, outcome{Recording: rel, Action: "signed", Hash: m.ContentHash})
	}

	if p.json() {
		return p.writeJSON(map[string]any{
			"campaign": r.ID, "signer": signer, "on": signedOn.Format(time.RFC3339),
			"declared_signatories": len(r.Signatories), "recordings": results,
		})
	}
	if err := p.writeLine("## %s — signed by %s on %s\n", r.ID, signer, signedOn.Format("2006-01-02")); err != nil {
		return err
	}
	rows := make([][]string, 0, len(results))
	for _, res := range results {
		rows = append(rows, []string{res.Recording, res.Action, res.Hash})
	}
	if err := p.writeTable([]string{"RECORDING", "ACTION", "CONTENT HASH"}, rows); err != nil {
		return err
	}
	if len(r.Signatories) > 1 {
		return p.writeLine("\nThe campaign declares %d signatories, so a second signature is "+
			"required: run `sign` again as the other person (FR-140).", len(r.Signatories))
	}
	return nil
}

// ---- helpers ---------------------------------------------------------------------------------

// recordingsIn returns the recording directories of a campaign.
//
// A campaign directory normally IS a recording — both feeders record into one, because two connectors
// recording one campaign are one arrival stream. A campaign split into windows holds them as
// subdirectories instead. Accepting either is what lets quickstart §7's single directory and a
// multi-window campaign use the same commands.
func recordingsIn(dir string) ([]string, error) {
	if _, err := os.Stat(filepath.Join(dir, record.ManifestFile)); err == nil {
		return []string{dir}, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sub := filepath.Join(dir, e.Name())
		if _, err := os.Stat(filepath.Join(sub, record.ManifestFile)); err == nil {
			out = append(out, sub)
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil, fmt.Errorf("%s holds no recording: neither it nor any subdirectory has a %s. "+
			"A campaign record with nothing recorded beside it describes a campaign that produced "+
			"nothing", dir, record.ManifestFile)
	}
	return out, nil
}

func accessPathList() string {
	paths := make([]string, 0, len(campaign.AccessPaths()))
	for _, p := range campaign.AccessPaths() {
		paths = append(paths, string(p))
	}
	return strings.Join(paths, ", ")
}

func containsFold(haystack []string, needle string) bool {
	for _, h := range haystack {
		if strings.EqualFold(strings.TrimSpace(h), strings.TrimSpace(needle)) {
			return true
		}
	}
	return false
}

// campaignFindingIn is the in-process check over one committed file: a people-shaped value that is
// not a documentation placeholder, or a surviving canary token.
//
// It is deliberately not `Sanitiser.AssertArtifact`: that needs a Sanitiser, a Sanitiser needs a key,
// and this gate has to run where the key is absent. The two checks it performs are the same two.
func campaignFindingIn(where string, body []byte, canaries *sanitise.CanarySet, keyed bool) string {
	if class := sanitise.PeopleInArtifact(body); class != "" {
		// The class, never the value. A report quoting what it found would put the address in a CI
		// log, which is the thing the gate exists to prevent.
		return fmt.Sprintf("%s carries %s (FR-135, SC-019)", where, class)
	}
	if keyed {
		survivors, err := canaries.Survivors(body)
		if err != nil {
			return fmt.Sprintf("%s: canary check failed: %v", where, err)
		}
		if len(survivors) > 0 {
			return fmt.Sprintf("%s carries %d surviving canary token(s) (FR-139, SC-018)", where, len(survivors))
		}
		return ""
	}
	if strings.Contains(string(body), sanitise.CanaryPrefix) {
		return fmt.Sprintf("%s carries a canary token; with no corpus key in the environment this "+
			"run cannot say which campaign planted it, and a surviving canary fails the commit "+
			"either way (FR-139, SC-018)", where)
	}
	return ""
}

// ---- verify ----------------------------------------------------------------------------------

// newCampaignVerifyCommand is the gate that makes FR-140 enforceable rather than promised.
//
// `sanitise.Manifest.Check` is documented as *the commit gate* and had no caller outside its own
// unit tests: the function existed, the rule it encodes was written down, and no command ran it — so
// a recording could be committed with no manifest, with a manifest whose hash no longer matched what
// it signed, or with one signature where the campaign declares two, and nothing anywhere would
// object. `scan` and `sanitise` do not close that: they check the recording's *contents* for people
// and canaries and never look at the manifest.
//
// Three failures, and they are distinct on purpose because the remedy differs:
//
//   - **no manifest.** Nobody signed this. Run `sign`;
//   - **the hash no longer matches.** Somebody signed a different recording from the one on disk, so
//     the signature attests to bytes that are not here. The report names what was added, removed or
//     edited, because "the hash differs" is not something a person can act on;
//   - **fewer signatures than the campaign declares.** FR-140's second reader has not read it. The
//     count comes from the campaign record rather than a flag, so it cannot be argued down at the
//     command line.
func newCampaignVerifyCommand(global *globalOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "verify <dir>",
		Short: "The commit gate: every recording carries a signed, current manifest (FR-140)",
		Long: "verify asserts, for every recording in the campaign, that a signed manifest exists, that\n" +
			"its content hash still matches the recording on disk, and that it carries as many\n" +
			"signatures as the campaign declares signatories.\n\n" +
			"It is the check that makes a sanitisation guarantee enforceable at commit rather than a\n" +
			"promise. `scan` and `sanitise` read the recording's contents; this reads what was signed,\n" +
			"and a recording whose bytes have moved since somebody signed them fails here.\n\n" +
			"It needs no corpus key and no credential, which is what lets it run in CI.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCampaignVerify(cmd, global, args[0])
		},
	}
}

func runCampaignVerify(cmd *cobra.Command, global *globalOptions, dir string) error {
	r, err := campaign.Read(dir)
	if err != nil {
		return exitErrorf(ExitUsage, "fixture campaign verify: %v", err)
	}
	recordings, err := recordingsIn(dir)
	if err != nil {
		return exitErrorf(ExitUsage, "fixture campaign verify: %v", err)
	}
	if len(recordings) == 0 {
		return exitErrorf(ExitUsage, "fixture campaign verify: %s holds no recording, so the gate "+
			"held over nothing", dir)
	}

	type row struct {
		Recording  string   `json:"recording"`
		Signed     bool     `json:"signed"`
		Signatures []string `json:"signatures"`
		Problem    string   `json:"problem,omitempty"`
	}
	rows := make([]row, 0, len(recordings))
	var failures int
	for _, rec := range recordings {
		out := row{Recording: filepath.Base(rec)}
		switch m, readErr := sanitise.ReadManifest(rec); {
		case readErr != nil:
			out.Problem = readErr.Error()
		default:
			out.Signed = true
			for _, sig := range m.Signatures {
				out.Signatures = append(out.Signatures, sig.By+" on "+sig.On.UTC().Format(time.DateOnly))
			}
			// The hash first: a manifest that signed different bytes is worse than an unsigned
			// recording, because it reads as reviewed.
			if err := m.Verify(rec); err != nil {
				out.Problem = err.Error()
			} else if err := m.Check(len(r.Signatories)); err != nil {
				out.Problem = err.Error()
			}
		}
		if out.Problem != "" {
			failures++
		}
		rows = append(rows, out)
	}

	p := newPrinter(cmd.OutOrStdout(), global.Output)
	if p.json() {
		if err := p.writeJSON(map[string]any{
			"campaign": r.ID, "declared_signatories": len(r.Signatories),
			"recordings": rows, "failures": failures,
		}); err != nil {
			return err
		}
	} else {
		if err := p.writeLine("## %s — verify\n\n- recordings: %d\n- declared signatories: %d",
			r.ID, len(rows), len(r.Signatories)); err != nil {
			return err
		}
		for _, out := range rows {
			state := "covered"
			if out.Problem != "" {
				state = "NOT COVERED"
			}
			line := "  - " + out.Recording + ": " + state
			if len(out.Signatures) > 0 {
				line += " (" + strings.Join(out.Signatures, "; ") + ")"
			}
			if out.Problem != "" {
				line += "\n      " + out.Problem
			}
			if err := p.writeLine("%s", line); err != nil {
				return err
			}
		}
	}
	if failures > 0 {
		return exitErrorf(ExitRejected, "fixture campaign verify: %d recording(s) are not covered "+
			"by a signed, current manifest; FR-140 is a commit gate and not a promise", failures)
	}
	return nil
}

// ---- parity ----------------------------------------------------------------------------------

func newCampaignParityCommand(global *globalOptions) *cobra.Command {
	var (
		dsn      string
		recorded string
	)
	cmd := &cobra.Command{
		Use:   "parity <live-dir>",
		Short: "Compare a live run against the recording of the same run (FR-143)",
		Long: "parity asserts both halves of FR-143, and they fail for different reasons.\n\n" +
			"The GRAPH half catches a connector that behaves differently when it is recording — a\n" +
			"code path taken only with --record, an instant read from the clock rather than from the\n" +
			"payload, an order that reached the emitter differently. The DIGEST half catches\n" +
			"something else: a backend deriving an answer from the CONNECTION rather than from the\n" +
			"response (FR-008). That one produces identical graphs and different digests, so a check\n" +
			"of the graph alone would pass exactly the case FR-008 exists to prevent.\n\n" +
			"Both sides are recordings, because a live run's output IS one: `feed … --record` writes\n" +
			"it. So this is runnable today against any two recordings — which is how it is tested —\n" +
			"and the run FR-143 actually asks for needs a live credential for the platform recorded.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(recorded) == "" {
				return exitErrorf(ExitUsage, "fixture campaign parity: --recorded is required; "+
					"parity is a comparison and one side is not one")
			}
			resolved, err := dsnFrom(dsn, os.Getenv(EnvDSN))
			if err != nil {
				return err
			}
			got, err := fixture.Parity(cmd.Context(), fixture.NewStoreFactoryFromDSN(resolved),
				args[0], recorded)
			if err != nil {
				return exitErrorf(ExitUsage, "fixture campaign parity: %v", err)
			}

			p := newPrinter(cmd.OutOrStdout(), global.Output)
			if p.json() {
				if err := p.writeJSON(got); err != nil {
					return err
				}
			} else {
				if err := p.writeLine("## parity — %s\n", passedWord(got.Passed())); err != nil {
					return err
				}
				if err := p.writeTable([]string{"HALF", "RESULT", "DETAIL"}, [][]string{
					{"graph (FR-143)", passedWord(got.GraphEqual), got.GraphDetail},
					{"digest (FR-008, SC-014)", passedWord(got.WorldEqual), got.WorldDetail},
				}); err != nil {
					return err
				}
				if err := p.writeLine("\ncompared %d event(s) and %d recorded response(s)",
					got.Events, got.WorldEntries); err != nil {
					return err
				}
			}
			if !got.Passed() {
				return exitErrorf(ExitRejected, "fixture campaign parity: the live run and its "+
					"recording are not equivalent, so the recording cannot stand in for the live "+
					"system (FR-143, SC-016)")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&recorded, "recorded", "", "the recording to compare the live run against (required)")
	cmd.Flags().StringVar(&dsn, "db", "", "PostgreSQL DSN whose role has CREATEDB (default $PG_DSN); each side is loaded into a database of its own")
	return cmd
}

func passedWord(ok bool) string {
	if ok {
		return "equal"
	}
	return "DIFFERS"
}
