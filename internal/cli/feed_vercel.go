// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/spf13/cobra"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/feeders/deployrecord"
	vercelfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/vercel"
	"github.com/Pierre-Theophile/aisre/internal/sanitise"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/emit"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/source"
)

// `feed vercel` (004 T090).
//
// ---------------------------------------------------------------------------------------------
// What this command can and cannot do today, stated rather than discovered
//
// A REPLAY and a LIVE run are one code path (004 T158). `vercel.Poller` reads each followed project by
// id — the read that states `lastAliasRequest`, where Vercel says a rollback happened — then its
// environment metadata and its production deployments, and hands the feeder the payloads a recording
// holds. `--once` runs one cycle.
//
// # A live run needs the operator's assertion
//
// Vercel reports nothing about what a token may write, so FR-003's platform-reported proof is not
// available, and a live run refuses unless `--assert-read-only` is given. The checkpoint then records
// `operator_asserted` rather than `platform_reported`: the operator said so, and the record says who
// did (contracts/read-only-operations.md §3.1). What is enforced regardless is the surface: every
// operation this connector can issue is a GET, and the query that would decrypt a variable is not on it.
//
// # --projects is the operator's mapping, and it has no default
//
// A Vercel project id says nothing about which service runs in production; that mapping is the
// operator's, for the reason internal/feeders/github/targets.go sets out at length. There is deliberately
// no default and no inference from the project's NAME: a name that happens to match a Kubernetes
// deployment would attach one estate's rollouts to another's graph, and it would do it silently.

func init() { addFeedSubcommand(newFeedVercelCommand) }

type feedVercelOptions struct {
	orgSlug        string
	baseURL        string
	token          string
	teamID         string
	dryRun         bool
	recordDir      string
	replayDir      string
	projects       []string
	batchSize      int
	assertReadOnly bool
	interval       time.Duration
	lookback       time.Duration
	once           bool
	quotaShare     float64
	quotaReserve   int
}

func newFeedVercelCommand(global *globalOptions) *cobra.Command {
	opts := &feedVercelOptions{}
	cmd := &cobra.Command{
		Use:   "vercel",
		Short: "Feed Vercel production promotions as rollout changes",
		Long: "Reads the team's deployments and its projects, and emits a ROLLOUT for each deployment\n" +
			"that is `target=production` AND `readySubstate=PROMOTED`. `READY` alone means built and\n" +
			"available, not serving, so a staged deployment emits nothing (research §3.1).\n\n" +
			"It writes nothing anywhere: the published read-only operation surface carries no method\n" +
			"but GET, an operation it does not carry is refused before any quota is spent, and the\n" +
			"query that would decrypt an environment variable is not on it (FR-038).\n\n" +
			"A live run polls every --interval: each followed project by id (the read that states a\n" +
			"rollback), its environment metadata and its production deployments. It needs\n" +
			"--assert-read-only, because Vercel reports nothing about a token's write capability.\n" +
			"--replay runs the same cycle from a recorded directory.\n\n" +
			"--projects has no default. Which entity a project's rollouts change is operator\n" +
			"configuration, and inferring it from the project's name would attach one estate's\n" +
			"rollouts to another's graph.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runFeedVercel(cmd.Context(), global, opts, cmd)
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&opts.orgSlug, "org", "", "organisation slug; the source id is vercel:<org>")
	flags.StringVar(&opts.baseURL, "api", "https://api.vercel.com",
		"API root; overridden in tests and for a self-hosted proxy")
	flags.StringVar(&opts.token, "vercel-token", "",
		"a Vercel access token, from wherever the operator's secret management provides it. There is "+
			"deliberately no flag for a file path: a path in a shell history is a pointer to a "+
			"credential, and this process holds the token only for the requests that need it")
	flags.StringVar(&opts.teamID, "team", "",
		"scope every call to one team, where the credential is a personal one")
	flags.BoolVar(&opts.dryRun, "dry-run", false,
		"report what regime the token puts this connector in, and nothing else: no operation from the "+
			"published surface, no event, no server connection")
	flags.StringVar(&opts.recordDir, "record", "", "also record the run to this fixture directory, "+
		"sanitised in the connector before anything touches disk under the contract table and the corpus "+
		"key in $SRE_AGENT_CORPUS_KEY (FR-137); the recording's events are derived from its sanitised payloads")
	flags.StringVar(&opts.replayDir, "replay", "",
		"read payloads from this fixture directory instead of Vercel")
	flags.StringSliceVar(&opts.projects, "projects", nil,
		"the operator's mapping, as `projectId=namespace:value` — for example "+
			"`prj_abc=k8s.deployment:shop/storefront`. Required for a live run; there is deliberately "+
			"no default and no inference from the project's name")
	flags.IntVar(&opts.batchSize, "batch-size", 0, "events per ingest batch; 0 uses the emitter's default")
	flags.BoolVar(&opts.assertReadOnly, "assert-read-only", false,
		"the operator's statement that this token cannot write. Required for a live run: Vercel reports "+
			"nothing about a token's write capability, so FR-003 needs the assertion rather than an "+
			"assumption, and the checkpoint records it as operator_asserted")
	flags.DurationVar(&opts.interval, "interval", vercelfeeder.DefaultPollInterval, "time between poll cycles")
	flags.DurationVar(&opts.lookback, "lookback", vercelfeeder.DefaultLookback,
		"how far back the first cycle reads; widen it for a backfill")
	flags.BoolVar(&opts.once, "once", false, "run one poll cycle and exit")
	flags.Float64Var(&opts.quotaShare, "quota-share", 0.5,
		"the fraction of each remaining rate-limit window this connector may spend")
	flags.IntVar(&opts.quotaReserve, "quota-reserve", 20,
		"calls always left unspent in each measured window, so the team's own tooling is never locked out")
	return cmd
}

func runFeedVercel(ctx context.Context, global *globalOptions, opts *feedVercelOptions, cmd *cobra.Command) error {
	if opts.recordDir != "" && opts.replayDir != "" {
		return exitErrorf(ExitUsage, "feed vercel: --record and --replay are mutually exclusive; "+
			"re-recording a replay would produce a fixture of a fixture")
	}
	// A live recording is sanitised in the connector before anything touches disk (004 T104, FR-137);
	// without the corpus key it is refused here, before a directory exists or anything is read.
	var san *sanitise.Sanitiser
	if opts.recordDir != "" && opts.replayDir == "" && !opts.dryRun {
		built, err := recordingSanitiser()
		if err != nil {
			return exitErrorf(ExitUsage, "feed vercel: --record on a live run is sanitised in the connector "+
				"before anything touches disk (FR-137), and it cannot start: %v", err)
		}
		san = built
	}

	if opts.dryRun {
		return runFeedVercelDryRun(opts, cmd)
	}

	if opts.orgSlug == "" {
		return exitErrorf(ExitUsage, "feed vercel: --org is required; it is the suffix of the source "+
			"id, and an empty one makes every team's events one source")
	}

	targets, err := parseVercelTargets(opts.projects)
	if err != nil {
		return exitErrorf(ExitUsage, "feed vercel: %v", err)
	}

	live := opts.replayDir == ""
	if live {
		switch {
		case strings.TrimSpace(opts.token) == "":
			return exitErrorf(ExitUsage, "feed vercel: --vercel-token is required for a live run")
		case !opts.assertReadOnly:
			return exitErrorf(ExitAuth, "feed vercel: a live run is refused without --assert-read-only. %v",
				&feeder.UnverifiedCredentialError{Platform: vercelfeeder.Platform})
		}
	}
	if len(targets) == 0 {
		// A replay with no mapping still emits the changes, unattached. That is Edge case 1's shape —
		// a deploy that happened is a fact whether or not this connector can say what it touched — so
		// it is a warning rather than a refusal.
		fmt.Fprintf(cmd.ErrOrStderr(), "feed vercel: no --projects mapping, so every rollout will be "+
			"emitted unattached; a target that appears later attaches through the projector's pending "+
			"queue rather than requiring a re-read\n")
	}

	logger := slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), nil))
	feederOpts := vercelfeeder.Options{OrgSlug: opts.orgSlug, Targets: targets, Log: logger}
	var src feeder.Source
	var recording *deployrecord.Tee
	if live {
		followed := make([]string, 0, len(targets))
		for project := range targets {
			followed = append(followed, project)
		}
		feederOpts.Gate = assertedVercelGate{projects: followed}
		budget, err := feeder.NewQuotaBudget(vercelfeeder.Platform, feeder.QuotaPolicy{
			Share: opts.quotaShare, Reserve: opts.quotaReserve, StaticAllowance: 30,
		})
		if err != nil {
			return exitErrorf(ExitUsage, "feed vercel: %v", err)
		}
		issuer, err := feeder.NewIssuer(vercelfeeder.Surface, budget, &feeder.AreaStats{})
		if err != nil {
			return exitErrorf(ExitUsage, "feed vercel: %v", err)
		}
		token := opts.token
		client, err := vercelfeeder.NewClient(vercelfeeder.ClientOptions{
			BaseURL: opts.baseURL, Issuer: issuer, TeamID: opts.teamID,
			Token: func(context.Context) (string, error) { return token, nil },
		})
		if err != nil {
			return exitErrorf(ExitUsage, "feed vercel: %v", err)
		}
		poller, err := vercelfeeder.NewPoller(vercelfeeder.PollerOptions{
			Reader: client, Projects: followed, Interval: opts.interval, Lookback: opts.lookback,
			Once: opts.once, Logger: logger,
		})
		if err != nil {
			return exitErrorf(ExitUsage, "feed vercel: %v", err)
		}
		src = poller
		if san != nil {
			tee, err := deployrecord.NewTee(poller, vercelfeeder.Kind, san, opts.recordDir)
			if err != nil {
				return exitErrorf(ExitUsage, "feed vercel: %v", err)
			}
			recording = tee
			src = tee
		}
	} else {
		fileSource, err := source.NewFileSource(opts.replayDir)
		if err != nil {
			return exitErrorf(ExitUsage, "feed vercel: %v", err)
		}
		src = fileSource
	}
	f, err := vercelfeeder.New(feederOpts)
	if err != nil {
		return exitErrorf(ExitUsage, "feed vercel: %v", err)
	}

	connect, err := emit.NewConnectEmitter(global.Server, global.Token, f.Describe(),
		emit.WithBatchSize(opts.batchSize))
	if err != nil {
		return exitErrorf(ExitTransport, "feed vercel: %v", err)
	}
	var em feeder.Emitter = connect

	runErr := f.Run(ctx, src, em)
	if recording != nil {
		shadowTargets, err := sanitisedVercelTargets(san, targets)
		if err != nil {
			return exitErrorf(ExitUsage, "feed vercel: %v", err)
		}
		shadow, err := vercelfeeder.New(vercelfeeder.Options{OrgSlug: opts.orgSlug, Targets: shadowTargets})
		if err != nil {
			return exitErrorf(ExitUsage, "feed vercel: %v", err)
		}
		events, err := recording.Finish(context.WithoutCancel(ctx), opts.recordDir, "vercel-deployment", shadow)
		if err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "feed vercel: the recording was not completed: %v\n", err)
		} else {
			fmt.Fprintf(cmd.ErrOrStderr(), "recorded %d sanitised payload(s) and %d derived event(s) to %s; refused: %v\n",
				recording.Written(), events, opts.recordDir, recording.Dropped())
		}
	}
	if err := runErr; err != nil {
		if live && errors.Is(err, context.Canceled) {
			return nil
		}
		return exitErrorf(ExitTransport, "feed vercel: %v", err)
	}
	return nil
}

// assertedVercelGate is the live run's read-only gate: the operator's assertion, checked by the same
// function every connector's gate goes through, and recorded as exactly what it is.
type assertedVercelGate struct{ projects []string }

func (g assertedVercelGate) Prove(context.Context) (vercelfeeder.GateResult, error) {
	evidence, err := feeder.CheckReadOnly(feeder.CredentialReport{
		Platform: vercelfeeder.Platform, Evidence: feeder.EvidenceOperatorAsserted,
		Scope: feeder.CredentialScope{Targets: g.projects},
	})
	if err != nil {
		return vercelfeeder.GateResult{}, err
	}
	selection := "every project the token can list (no --projects mapping)"
	if len(g.projects) > 0 {
		selection = fmt.Sprintf("%d project(s) named by the operator's --projects; the token may reach more", len(g.projects))
	}
	return vercelfeeder.GateResult{
		Evidence: []byte(evidence), Scope: vercelfeeder.Scope{Selection: selection, Projects: g.projects},
	}, nil
}

// runFeedVercelDryRun reports the regime the token puts this connector in (T034).
//
// It REPORTS rather than declares, and the difference is the whole point. Research §5.3 established that
// `POST /v3/user/tokens` accepts a `projectId`, so the platform CAN pin a token to exactly one project —
// which would make a project-scoped token a platform boundary rather than operator configuration. What
// is NOT established is the spelling that scope takes in `token.scopes[].type` when the token is read
// back, and a command that printed "boundary enforced by the platform" from a value it had guessed would
// be making the stronger claim on no evidence.
//
// So until T034 settles the spelling, this prints what it knows and names what it does not. That is less
// satisfying than a verdict and it is the honest output: `feed github --dry-run` can say "boundary
// enforced by the platform" because GitHub's response states the repository selection in a documented
// field, and flattening Vercel's weaker position into the same sentence would erase the difference
// (contracts/read-only-operations.md §3.1).
func runFeedVercelDryRun(opts *feedVercelOptions, cmd *cobra.Command) error {
	out := cmd.OutOrStdout()
	if strings.TrimSpace(opts.token) == "" {
		return exitErrorf(ExitUsage, "feed vercel --dry-run: --vercel-token is required; there is nothing to "+
			"report about a credential that was not supplied")
	}

	fmt.Fprintf(out, "read-only surface: %d operation(s), every one a GET\n", len(vercelfeeder.Surface.Operations()))
	for _, op := range vercelfeeder.Surface.Operations() {
		fmt.Fprintf(out, "  %s\n", op)
	}
	fmt.Fprintf(out, "the query that would decrypt an environment variable is not on this surface, and "+
		"the decoded type has no field a value could land in (FR-038)\n")
	fmt.Fprintf(out, "token regime: NOT ESTABLISHED. Vercel can pin a token to one project "+
		"(POST /v3/user/tokens takes projectId), which would be a platform-enforced boundary — but the "+
		"spelling that scope takes in token.scopes[].type when the token is read back is not "+
		"established, and reporting a regime from a guessed field would claim more than is known "+
		"(004 T034)\n")
	fmt.Fprintf(out, "no operation from the published surface was issued, no event was emitted, and no "+
		"connection to the graph was opened\n")
	return nil
}

// parseVercelTargets reads the operator's `projectId=namespace:value` mapping.
//
// Strict about the shape, because a silently-dropped mapping is a rollout attached to nothing, and the
// operator would see a working run with a quietly emptier graph.
func parseVercelTargets(specs []string) (map[string]*graphv1.Ref, error) {
	out := map[string]*graphv1.Ref{}
	for _, spec := range specs {
		project, ref, found := strings.Cut(strings.TrimSpace(spec), "=")
		if !found {
			return nil, fmt.Errorf("--projects %q is not `projectId=namespace:value`", spec)
		}
		namespace, value, found := strings.Cut(ref, ":")
		project, namespace, value = strings.TrimSpace(project), strings.TrimSpace(namespace), strings.TrimSpace(value)
		if !found || project == "" || namespace == "" || value == "" {
			return nil, fmt.Errorf("--projects %q is not `projectId=namespace:value`", spec)
		}
		if _, exists := out[project]; exists {
			return nil, fmt.Errorf("--projects names %s twice; one project changes one entity, and two "+
				"rules for it would silently keep whichever was parsed last", project)
		}
		out[project] = feeder.Ref(namespace, value)
	}
	return out, nil
}
