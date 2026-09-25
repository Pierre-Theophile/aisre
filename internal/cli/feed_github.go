// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Pierre-Theophile/aisre/internal/feeders/deployrecord"
	githubfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/github"
	"github.com/Pierre-Theophile/aisre/internal/sanitise"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/emit"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/source"
)

// `feed github` (004 T072, FR-006, FR-008; docs/connectors/github.md).
//
// ---------------------------------------------------------------------------------------------
// --dry-run performs the startup gate and nothing else
//
// The same contract the GCP feeder's dry run has, and for the same reason: an operator who has just
// installed a GitHub App wants one question answered — *is this credential actually read-only, and
// what is it scoped to?* — without emitting a single event and without spending a single
// quota-bearing call.
//
// For GitHub it is cheaper than for GCP. Everything the gate reports is on the installation token's
// own response, which the connector has to obtain anyway, so a dry run issues **no operation from the
// published surface at all**. A dry run that read "just a little" would turn the one command an
// operator trusts to be harmless into one they have to reason about.
//
// # Why the App's private key is not a flag
//
// The one non-read call this connector makes is minting an installation token, and signing the
// assertion for it needs the App's private key. That key is not read here and there is no flag for a
// path to it: a connector that read a private key from a file would hold one in its process for far
// longer than the one request that needs it, and would put its path in a command line and a shell
// history. The assertion is supplied by whatever the operator's secret management provides, which is
// why `AppMinter.JWT` is a function.
//
// A live run lasts longer than one assertion (GitHub caps an App JWT at ten minutes) and longer than one
// installation token (an hour), so `--app-assertion-command` names a credential helper: a command run
// each time a token is renewed, printing a fresh assertion on stdout — the shape of git's credential
// helpers and kubectl's exec plugins. The key stays wherever the helper keeps it. `--app-assertion` is
// the one-shot form, for `--dry-run` and `--once`.
//
// # A live run (004 T157)
//
// The poller reads GitHub every --interval and hands the feeder the same payloads a recording holds, so
// a live run and `--replay` are one code path from the first payload on. Every token renewal is proved
// read-only again, and a renewal the gate refuses ends the run.

func init() { addFeedSubcommand(newFeedGitHubCommand) }

type feedGitHubOptions struct {
	orgSlug          string
	installationID   int64
	baseURL          string
	assertion        string
	assertionCommand string
	dryRun           bool
	recordDir        string
	replayDir        string
	environments     []string
	workflows        []string
	mapFile          string
	interval         time.Duration
	lookback         time.Duration
	once             bool
	quotaShare       float64
	quotaReserve     int
	batchSize        int
}

func newFeedGitHubCommand(global *globalOptions) *cobra.Command {
	opts := &feedGitHubOptions{}
	cmd := &cobra.Command{
		Use:   "github",
		Short: "Feed GitHub deployments, workflow runs and releases",
		Long: "Reads the installation's repository selection and then, per repository, the deployments\n" +
			"and their statuses, the workflow runs and the releases. It writes nothing anywhere: the\n" +
			"published read-only operation surface carries no method but GET, and an operation it does\n" +
			"not carry is refused before any quota is spent (docs/connectors/github.md §2).\n\n" +
			"--dry-run performs the startup read-only gate and nothing else. For GitHub that costs no\n" +
			"call from the surface at all, because the permissions and the repository selection are on\n" +
			"the installation token's own response.\n\n" +
			"--environments has no default: which environments count as production is operator\n" +
			"configuration, and an empty list emits nothing rather than treating every preview\n" +
			"deployment as a production change (FR-023).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runFeedGitHub(cmd.Context(), global, opts, cmd)
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&opts.orgSlug, "org", "", "organisation slug; the source id is github:<org>")
	flags.Int64Var(&opts.installationID, "installation", 0,
		"the GitHub App installation to read as (required)")
	flags.StringVar(&opts.baseURL, "api", githubfeeder.DefaultBaseURL,
		"API root; an Enterprise Server installation serves the same paths under a different host")
	flags.StringVar(&opts.assertion, "app-assertion", "",
		"a short-lived GitHub App JWT, from wherever the operator's secret management provides it. "+
			"There is deliberately no flag for the App's private key: reading one here would hold it "+
			"in this process far longer than the single request that needs it, and put its path in a "+
			"shell history")
	flags.BoolVar(&opts.dryRun, "dry-run", false,
		"perform the startup read-only gate and nothing else: no operation from the published surface, "+
			"no event, no server connection")
	flags.StringVar(&opts.recordDir, "record", "", "also record the run to this fixture directory, "+
		"sanitised in the connector before anything touches disk under the contract table and the corpus "+
		"key in $SRE_AGENT_CORPUS_KEY (FR-137); the recording's events are derived from its sanitised payloads")
	flags.StringVar(&opts.replayDir, "replay", "",
		"read payloads from this fixture directory instead of GitHub")
	flags.StringSliceVar(&opts.environments, "environments", nil,
		"deployment environments that count as production (required for a live run; there is "+
			"deliberately no default — FR-023)")
	flags.StringSliceVar(&opts.workflows, "deploy-workflows", nil,
		"workflow names whose runs are rollouts even where no deployment object records them (FR-024)")
	flags.StringVar(&opts.assertionCommand, "app-assertion-command", "",
		"a credential helper: a command run each time the installation token is renewed, printing a "+
			"fresh GitHub App JWT on stdout. What a live run uses, since an assertion lasts ten minutes")
	flags.StringVar(&opts.mapFile, "map", "",
		"the operator's mapping file: environments, deploy workflows, automation accounts, repositories "+
			"that ship by release, and each repository's targets (see internal/cli/feed_github_map.go)")
	flags.DurationVar(&opts.interval, "interval", githubfeeder.DefaultPollInterval,
		"time between poll cycles; every change records it as the interval its history was sampled at")
	flags.DurationVar(&opts.lookback, "lookback", githubfeeder.DefaultLookback,
		"how far back the first cycle reads; widen it for a backfill")
	flags.BoolVar(&opts.once, "once", false, "run one poll cycle and exit")
	flags.Float64Var(&opts.quotaShare, "quota-share", 0.5,
		"the fraction of the installation's remaining hourly quota this connector may spend")
	flags.IntVar(&opts.quotaReserve, "quota-reserve", 500,
		"calls always left unspent, so the organisation's own tooling is never locked out (FR-072)")
	flags.IntVar(&opts.batchSize, "batch-size", 0, "events per ingest batch; 0 uses the emitter's default")
	return cmd
}

func runFeedGitHub(ctx context.Context, global *globalOptions, opts *feedGitHubOptions, cmd *cobra.Command) error {
	if opts.recordDir != "" && opts.replayDir != "" {
		return exitErrorf(ExitUsage, "feed github: --record and --replay are mutually exclusive; "+
			"re-recording a replay would produce a fixture of a fixture")
	}
	// A live recording is sanitised in the connector before anything touches disk (004 T104, FR-137):
	// the sanitiser is built first, from the contract table and the corpus key, and without the key the
	// run is refused here — before a directory exists and before anything is read.
	var san *sanitise.Sanitiser
	if opts.recordDir != "" && opts.replayDir == "" {
		built, err := recordingSanitiser()
		if err != nil {
			return exitErrorf(ExitUsage, "feed github: --record on a live run is sanitised in the connector "+
				"before anything touches disk (FR-137), and it cannot start: %v", err)
		}
		san = built
	}
	if opts.assertion != "" && opts.assertionCommand != "" {
		return exitErrorf(ExitUsage, "feed github: --app-assertion and --app-assertion-command are "+
			"mutually exclusive; one of them is where the credential comes from")
	}

	// --dry-run is the gate and nothing else. It returns before anything constructs a transport or an
	// emitter, which is what makes the claim in --help true rather than aspirational.
	if opts.dryRun {
		return runFeedGitHubDryRun(ctx, opts, cmd)
	}

	mapping, err := loadGitHubMap(opts.mapFile, opts.environments, opts.workflows)
	if err != nil {
		return exitErrorf(ExitUsage, "feed github: %v", err)
	}
	mapping.PollInterval = opts.interval

	live := opts.replayDir == ""
	switch {
	case opts.orgSlug == "":
		return exitErrorf(ExitUsage, "feed github: --org is required; it is the suffix of the source "+
			"id, and an empty one makes every organisation's events one source")
	case live && opts.installationID == 0:
		return exitErrorf(ExitUsage, "feed github: --installation is required; the repositories in "+
			"scope are what the installation was granted (FR-008), so there is nothing to read without "+
			"one")
	case live && len(mapping.Allowlist.Environments) == 0:
		return exitErrorf(ExitUsage, "feed github: --environments is required for a live run (or "+
			"`environments:` in --map). Which environments count as production is operator "+
			"configuration and GitHub's own `production_environment` flag is whoever created the "+
			"deployment stating an opinion, so there is no default to fall back on (FR-023)")
	case live && opts.assertion == "" && opts.assertionCommand == "":
		return exitErrorf(ExitUsage, "feed github: a live run needs a credential: "+
			"--app-assertion-command (a helper printing a fresh App JWT at each renewal) or, for "+
			"--once, --app-assertion")
	}

	logger := slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), nil))
	f, err := githubfeeder.New(githubfeeder.Options{
		OrgSlug: opts.orgSlug, Map: mapping, PollInterval: opts.interval, Logger: logger,
	})
	if err != nil {
		return exitErrorf(ExitUsage, "feed github: %v", err)
	}

	var src feeder.Source
	var recording *deployrecord.Tee
	if live {
		credential := &githubfeeder.RenewingToken{Minter: &githubfeeder.AppMinter{
			BaseURL: opts.baseURL, InstallationID: opts.installationID, JWT: assertionSource(opts),
		}}
		// The gate runs at startup through the feeder, and every renewal after it through the same
		// check: one credential, proved each time it changes.
		f.Gate = credential
		budget, err := feeder.NewQuotaBudget(githubfeeder.Platform, feeder.QuotaPolicy{
			Share: opts.quotaShare, Reserve: opts.quotaReserve,
			// Enough for each operation's first call before GitHub has named its quota family.
			StaticAllowance: 30,
		})
		if err != nil {
			return exitErrorf(ExitUsage, "feed github: %v", err)
		}
		issuer, err := feeder.NewIssuer(githubfeeder.Surface, budget, &feeder.AreaStats{})
		if err != nil {
			return exitErrorf(ExitUsage, "feed github: %v", err)
		}
		client, err := githubfeeder.NewClient(opts.baseURL, nil, issuer, credential.Token, nil, nil)
		if err != nil {
			return exitErrorf(ExitUsage, "feed github: %v", err)
		}
		poller, err := githubfeeder.NewPoller(githubfeeder.PollerOptions{
			Reader: client, Environments: mapping.Allowlist.Environments,
			Interval: opts.interval, Lookback: opts.lookback, Once: opts.once, Logger: logger,
		})
		if err != nil {
			return exitErrorf(ExitUsage, "feed github: %v", err)
		}
		src = poller
		if san != nil {
			tee, err := deployrecord.NewTee(poller, githubfeeder.Kind, san, opts.recordDir)
			if err != nil {
				return exitErrorf(ExitUsage, "feed github: %v", err)
			}
			recording = tee
			src = tee
		}
	} else {
		fileSource, err := source.NewFileSource(opts.replayDir)
		if err != nil {
			return exitErrorf(ExitUsage, "feed github: %v", err)
		}
		src = fileSource
	}

	connect, err := emit.NewConnectEmitter(global.Server, global.Token, f.Describe(),
		emit.WithBatchSize(opts.batchSize))
	if err != nil {
		return exitErrorf(ExitTransport, "feed github: %v", err)
	}
	runErr := f.Run(ctx, src, connect)
	if recording != nil {
		// Finished whatever ended the run: the payloads already on disk are sanitised, and the events
		// derived from them are the recording's own (deployrecord).
		shadowMap, err := sanitisedGitHubMap(san, mapping)
		if err != nil {
			return exitErrorf(ExitUsage, "feed github: %v", err)
		}
		shadow, err := githubfeeder.New(githubfeeder.Options{OrgSlug: opts.orgSlug, Map: shadowMap, PollInterval: opts.interval})
		if err != nil {
			return exitErrorf(ExitUsage, "feed github: %v", err)
		}
		events, err := recording.Finish(context.WithoutCancel(ctx), opts.recordDir, "github-deployment", shadow)
		if err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "feed github: the recording was not completed: %v\n", err)
		} else {
			fmt.Fprintf(cmd.ErrOrStderr(), "recorded %d sanitised payload(s) and %d derived event(s) to %s; refused: %v\n",
				recording.Written(), events, opts.recordDir, recording.Dropped())
		}
	}
	if err := runErr; err != nil {
		// An operator stopping a live run is how a live run ends, not a failure.
		if live && errors.Is(err, context.Canceled) {
			return nil
		}
		if errors.Is(err, githubfeeder.ErrGateRefused) {
			return exitErrorf(ExitAuth, "feed github: %v", err)
		}
		return exitErrorf(ExitTransport, "feed github: %v", err)
	}
	return nil
}

// assertionSource is where each App assertion comes from: the flag's value, or the credential helper's
// stdout, run afresh at every renewal. The helper's stderr is the operator's; its stdout is never
// logged.
func assertionSource(opts *feedGitHubOptions) func(context.Context) (string, error) {
	if opts.assertionCommand == "" {
		assertion := opts.assertion
		return func(context.Context) (string, error) { return assertion, nil }
	}
	command := opts.assertionCommand
	return func(ctx context.Context) (string, error) {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		helper := exec.CommandContext(ctx, "/bin/sh", "-c", command)
		helper.Stderr = os.Stderr
		out, err := helper.Output()
		if err != nil {
			return "", fmt.Errorf("the --app-assertion-command helper failed: %w", err)
		}
		assertion := strings.TrimSpace(string(out))
		if assertion == "" {
			return "", fmt.Errorf("the --app-assertion-command helper printed nothing")
		}
		return assertion, nil
	}
}

func runFeedGitHubDryRun(ctx context.Context, opts *feedGitHubOptions, cmd *cobra.Command) error {
	out := cmd.OutOrStdout()
	if opts.installationID == 0 {
		return exitErrorf(ExitUsage, "feed github --dry-run: --installation is required; the gate "+
			"reads the permissions GitHub granted THAT installation")
	}
	if strings.TrimSpace(opts.assertion) == "" && opts.assertionCommand == "" {
		return exitErrorf(ExitUsage, "feed github --dry-run: --app-assertion is required (or --app-assertion-command). It is a "+
			"short-lived GitHub App JWT from wherever the operator's secret management provides it; "+
			"this command deliberately does not read the App's private key itself")
	}

	minter := &githubfeeder.AppMinter{
		BaseURL:        opts.baseURL,
		InstallationID: opts.installationID,
		JWT:            assertionSource(opts),
	}
	result, err := githubfeeder.Gate(ctx, minter)
	if err != nil {
		fmt.Fprintf(out, "read-only gate: REFUSED\n  %v\n", err)
		return exitErrorf(ExitAuth, "feed github --dry-run: the gate refused, so nothing would be emitted")
	}

	fmt.Fprintf(out, "read-only gate: passed for installation %d\n", opts.installationID)
	// The evidence is printed, not just the verdict. GitHub's gate rests on a PLATFORM statement and
	// Vercel's rests on an operator's assertion; a checkpoint that printed "read-only: yes" for both
	// would flatten the stronger claim into the weaker one
	// (contracts/read-only-operations.md §3.1).
	fmt.Fprintf(out, "  evidence: %s\n", result.Evidence)
	fmt.Fprintf(out, "  permissions: %s\n", strings.Join(result.Permissions, ", "))
	// FR-008's regime, reported rather than interpreted: a repository outside the selection is
	// unreachable rather than skipped by policy.
	selection := result.Scope.Selection
	if selection == "" {
		selection = "not stated by the platform"
	}
	fmt.Fprintf(out, "  repository selection: %s (boundary enforced by %s)\n",
		selection, boundaryHolder(result.Scope.PlatformEnforced))
	if !result.ExpiresAt.IsZero() {
		fmt.Fprintf(out, "  credential expires: %s\n", result.ExpiresAt.Format(time.RFC3339))
	}
	fmt.Fprintf(out, "no operation from the published surface was issued, no event was emitted, and no "+
		"connection to the graph was opened\n")
	return nil
}

func boundaryHolder(platformEnforced bool) string {
	if platformEnforced {
		return "the platform"
	}
	return "the operator's configuration"
}
