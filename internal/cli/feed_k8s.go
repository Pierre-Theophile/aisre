// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	k8sfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/k8s"
	"github.com/Pierre-Theophile/aisre/internal/telemetry"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/emit"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/record"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/source"
)

// `feed k8s` (contracts/cli.md §Feeders, FR-043, FR-044, FR-046).
//
// One command, three modes, one code path through the feeder:
//
//	live     informers → ChanSource → Feeder → ConnectEmitter
//	record   the same, with the payloads and the events teed into a fixture directory
//	replay   FileSource → Feeder → ConnectEmitter, or → stdout with --dry-run
//
// Before any of them emits anything, a live run asks the cluster what its credential may do and
// refuses to start if it can write (exit 3). That check is the whole of constitution VII as far
// as this connector is concerned, and deploy/kind/rbac/ is the credential that passes it.

func init() { addFeedSubcommand(newFeedK8sCommand) }

// feedK8sQueueSize is how many watch events may wait for the feeder before one is dropped. A
// drop is logged and shows up as a gap in the next checkpoint; it is never silent.
const feedK8sQueueSize = 1024

type feedK8sOptions struct {
	sourceID         string
	kubeconfig       string
	kubeContext      string
	clusterName      string
	namespaces       []string
	labelSelector    string
	ownerLabels      []string
	environment      string
	environmentLabel string
	nodePoolLabels   []string
	defaultNodePool  string
	logBackend       string
	checkpointEvery  time.Duration
	gapThreshold     time.Duration
	commitLabels     []string
	stateDir         string
	allowWrite       bool
	recordDir        string
	replayDir        string
	dryRun           bool
	batchSize        int
}

func newFeedK8sCommand(global *globalOptions) *cobra.Command {
	opts := &feedK8sOptions{}
	cmd := &cobra.Command{
		Use:   "k8s",
		Short: "Watch Kubernetes and emit workload, configuration and change events",
		Long: "Watches Deployments, StatefulSets, DaemonSets, Jobs, CronJobs, Services, Ingresses,\n" +
			"ConfigMaps, Secrets, Nodes and Namespaces through shared informers, and emits the\n" +
			"topology they describe: workloads, where they run, who owns them, what configuration\n" +
			"they consume, the names they are known by, and the rollouts, scalings and\n" +
			"configuration changes that happen to them (FR-043).\n\n" +
			"A Secret's values never leave the cluster: the node carries its version, the payload\n" +
			"carries a digest of each value, and neither carries the material (FR-008).\n\n" +
			"The credential must be read-only. The feeder asks the API server with a\n" +
			"SelfSubjectRulesReview and refuses to start if it may create, update, patch or delete\n" +
			"anything it watches (FR-046, constitution VII). deploy/kind/rbac/ grants exactly what\n" +
			"it needs.\n\n" +
			"Exit codes: 0 ok, 1 usage, 2 transport, 3 the credential, 5 an event the graph refused.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runFeedK8s(cmd, global, opts)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&opts.sourceID, "source-id", "",
		"this feeder's identity, e.g. k8s:prod-eu1 (required; the token must be scoped to it)")
	flags.StringVar(&opts.kubeconfig, "kubeconfig", "",
		"path to a kubeconfig (default: $KUBECONFIG, then ~/.kube/config, then the in-cluster credential)")
	flags.StringVar(&opts.kubeContext, "context", "",
		"kubeconfig context to use (default: the current context)")
	flags.StringVar(&opts.clusterName, "cluster-name", "",
		"name to record the cluster under (required live; Kubernetes does not know its own name)")
	flags.StringSliceVar(&opts.namespaces, "namespaces", nil,
		"namespaces to watch (default: all)")
	flags.StringVar(&opts.labelSelector, "label-selector", "",
		"only watch namespaced objects matching this label selector")
	flags.StringSliceVar(&opts.ownerLabels, "owner-labels", []string{k8sfeeder.DefaultOwnerLabel},
		"label keys an owning team is read from, in order")
	flags.StringSliceVar(&opts.commitLabels, "commit-labels", nil,
		"pod-template label or annotation keys a rollout's commit is read from, in order; the "+
			"commit becomes a merge key for C8, so name only keys your deploy tooling writes "+
			"(default: none, and no commit is read)")
	flags.StringVar(&opts.environment, "environment", k8sfeeder.DefaultEnvironment,
		"environment asserted when nothing in the cluster declares one")
	flags.StringVar(&opts.environmentLabel, "environment-label", k8sfeeder.DefaultEnvironmentLabel,
		"label or annotation an object or its namespace declares its environment in")
	flags.StringSliceVar(&opts.nodePoolLabels, "node-pool-labels", k8sfeeder.DefaultNodePoolLabels,
		"node labels and nodeSelector keys a node pool is read from, in order")
	flags.StringVar(&opts.defaultNodePool, "default-node-pool", k8sfeeder.DefaultNodePool,
		"pool asserted for a workload that selects none (pods are not watched, so it cannot be observed)")
	flags.StringVar(&opts.logBackend, "log-backend", k8sfeeder.DefaultLogBackend,
		"backend named by the LOG pointer on every workload")
	flags.DurationVar(&opts.checkpointEvery, "checkpoint-interval", k8sfeeder.DefaultCheckpointInterval,
		"how often a quiet watch records the extent it has observed")
	flags.StringVar(&opts.stateDir, "state-dir", "",
		"where this feeder remembers its last checkpoint and the objects it asserted, so that a "+
			"restart can retract what disappeared while it was down "+
			"(default: $XDG_STATE_HOME/sre-agent/feeders/<source-id>; \"-\" keeps nothing)")
	flags.DurationVar(&opts.gapThreshold, "gap-threshold", 0,
		"how long the hole between the last checkpoint and a restart must be before it is "+
			"reported as a gap (default: twice --checkpoint-interval)")
	flags.BoolVar(&opts.allowWrite, "allow-write-credentials", false,
		"start even though the credential can write to the cluster (constitution VII says refuse; "+
			"this exists only for a disposable development cluster's admin kubeconfig)")
	flags.StringVar(&opts.recordDir, "record", "",
		"record raw watch payloads and emitted events into this fixture directory")
	flags.StringVar(&opts.replayDir, "replay", "",
		"replay a recorded fixture directory instead of watching a cluster")
	flags.BoolVar(&opts.dryRun, "dry-run", false,
		"print the events instead of sending them; exit 5 if any would be refused")
	flags.IntVar(&opts.batchSize, "batch-size", emit.DefaultBatchSize,
		"how many events are sent to the graph per call")
	return cmd
}

func runFeedK8s(cmd *cobra.Command, global *globalOptions, opts *feedK8sOptions) error {
	if strings.TrimSpace(opts.sourceID) == "" {
		return exitErrorf(ExitUsage, "--source-id is required: it is the identity every event is stamped with")
	}
	live := opts.replayDir == ""
	if live && strings.TrimSpace(opts.clusterName) == "" {
		return exitErrorf(ExitUsage,
			"--cluster-name is required: the Kubernetes API does not know what its cluster is called, "+
				"and every workload is asserted to belong to one")
	}
	if opts.clusterName == "" {
		// A replay is reproducing a recording, and the cluster name it was recorded with is
		// part of what is being reproduced; naming it is still the operator's job, but an
		// unnamed replay is a usable default rather than a refusal.
		opts.clusterName = feedK8sReplayClusterName(opts.replayDir)
	}

	level, err := telemetry.ParseLogLevel(global.LogLevel)
	if err != nil {
		return exitErrorf(ExitUsage, "--log-level %q", global.LogLevel)
	}
	log := slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), &slog.HandlerOptions{Level: level}))

	stopTelemetry, err := feederTelemetry(cmd.Context(), opts.sourceID, log)
	if err != nil {
		return err
	}
	defer stopTelemetry()

	f, err := k8sfeeder.New(k8sfeeder.Options{
		SourceID:              opts.sourceID,
		ClusterName:           opts.clusterName,
		Namespaces:            opts.namespaces,
		LabelSelector:         opts.labelSelector,
		OwnerLabels:           opts.ownerLabels,
		CommitLabels:          opts.commitLabels,
		Environment:           opts.environment,
		EnvironmentLabel:      opts.environmentLabel,
		NodePoolLabels:        opts.nodePoolLabels,
		DefaultNodePool:       opts.defaultNodePool,
		LogBackend:            opts.logBackend,
		CheckpointInterval:    opts.checkpointEvery,
		GapThreshold:          opts.gapThreshold,
		AllowWriteCredentials: opts.allowWrite,
	}, log)
	if err != nil {
		return exitWith(ExitUsage, err)
	}

	// A recording needs one answer from the graph per event, because the observed time it
	// writes down is the one the graph assigned at acceptance (FR-023). A batching emitter has
	// not asked yet and answers nothing, so `--record` at the default batch size produced
	// payloads, an empty events.jsonl and a directory that looked like a recording — the first
	// thing the live run of 2026-09-16 hit (finding 1). `record.Emitter` now refuses such an
	// emitter loudly; forcing the batch size here means the operator never meets the refusal.
	if opts.recordDir != "" && opts.batchSize != 1 {
		log.Info("k8s: --record needs one graph answer per event; using --batch-size=1",
			"requested", opts.batchSize)
		opts.batchSize = 1
	}

	src, stop, err := feedK8sSource(cmd.Context(), f, opts, log)
	if err != nil {
		return err
	}
	defer stop()

	em, finish, err := feedK8sEmitter(cmd, global, opts, f.Describe(), log)
	if err != nil {
		return err
	}

	if opts.recordDir != "" {
		payloads := record.Wrap(src, opts.recordDir)
		events := record.Emitter(em, opts.recordDir)
		src, em = payloads, events
		defer feedK8sWriteManifest(cmd, opts, f.Describe(), payloads, events, log)
	}

	runErr := f.Run(cmd.Context(), src, em)
	if runErr != nil && !errors.Is(runErr, io.EOF) && !errors.Is(runErr, context.Canceled) {
		if code := finish(runErr); code != nil {
			return code
		}
		return exitWith(ExitTransport, runErr)
	}
	return finish(nil)
}

// feedK8sSource builds the input: a recording, or a live watch over a cluster.
//
// The permission check is attached here rather than run here: Run makes it before it emits
// anything, so that a feeder embedded in another program gets the same refusal a command does.
func feedK8sSource(ctx context.Context, f *k8sfeeder.Feeder, opts *feedK8sOptions, log *slog.Logger) (feeder.Source, func(), error) {
	if opts.replayDir != "" {
		src, err := source.NewFileSource(opts.replayDir)
		if err != nil {
			return nil, nil, exitWith(ExitUsage, err)
		}
		// A replay keeps no state: everything a reconnection needs is in the recording's own
		// `start` marker payload, which is the point of putting it there (state.go).
		log.Info("k8s: replaying a recording", "dir", opts.replayDir, "payloads", src.Len())
		return src, func() {}, nil
	}

	config, err := feedK8sRESTConfig(opts)
	if err != nil {
		return nil, nil, exitWith(ExitUsage, err)
	}
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, nil, exitWith(ExitUsage, err)
	}
	f.Permissions = &k8sfeeder.PermissionGuard{
		Reviewer:   client.AuthorizationV1().SelfSubjectRulesReviews(),
		Namespaces: opts.namespaces,
		AllowWrite: opts.allowWrite,
		Log:        log,
	}

	// What the previous process of this source left behind. It is pushed as a `start` marker
	// payload before the initial list, so a recording of this run carries it and `--replay`
	// reconciles exactly as the live run did (FR-044, FR-052, T067).
	start, store, err := feedK8sState(opts, log)
	if err != nil {
		return nil, nil, err
	}
	f.State = store

	watch, err := k8sfeeder.NewInformers(k8sfeeder.InformerOptions{
		Client:        client,
		Namespaces:    opts.namespaces,
		LabelSelector: opts.labelSelector,
		Start:         start,
		Log:           log,
		QueueSize:     feedK8sQueueSize,
	})
	if err != nil {
		return nil, nil, exitWith(ExitUsage, err)
	}

	sink := source.NewChanSource(feedK8sQueueSize)
	watchCtx, cancel := context.WithCancel(ctx)
	go func() {
		defer sink.Close()
		if err := watch.Run(watchCtx, sink); err != nil && !errors.Is(err, context.Canceled) {
			log.Error("k8s: the watch stopped", "error", err.Error())
		}
	}()
	log.Info("k8s: watching", "cluster", opts.clusterName,
		"namespaces", feedK8sNamespaceLabel(opts.namespaces))
	return sink, cancel, nil
}

// feedK8sState resolves the state directory, reads what the previous process left there, and
// returns the marker to push and the store to write back to (FR-052, T067).
//
// `--state-dir -` keeps nothing: the feeder then behaves as it did before this existed, which
// is what a one-off run against a cluster somebody else is watching should do — two feeders
// sharing a state directory would reconcile against each other's objects.
func feedK8sState(opts *feedK8sOptions, log *slog.Logger) (*k8sfeeder.StartState, k8sfeeder.StateStore, error) {
	if strings.TrimSpace(opts.stateDir) == "-" {
		log.Info("k8s: keeping no state; a restart will not detect a gap or retract what vanished")
		return nil, nil, nil
	}
	dir := opts.stateDir
	if dir == "" {
		resolved, err := k8sfeeder.DefaultStateDir(opts.sourceID)
		if err != nil {
			return nil, nil, exitWith(ExitUsage, err)
		}
		dir = resolved
	}
	state, err := k8sfeeder.LoadState(dir, opts.sourceID)
	if err != nil {
		return nil, nil, exitWith(ExitUsage, err)
	}
	start := state.Start()
	if start.PreviousCheckpoint == nil {
		log.Info("k8s: no previous state for this source; this run is a first list, not a reconnection",
			"state_dir", dir)
	} else {
		log.Info("k8s: resuming", "state_dir", dir,
			"previous_checkpoint", start.PreviousCheckpoint.Format(time.RFC3339Nano),
			"known_objects", len(start.KnownObjects))
	}
	return &start, k8sfeeder.FileStateStore{Dir: dir}, nil
}

// feedK8sRESTConfig resolves the credential: an explicit kubeconfig, the usual loading rules,
// or the in-cluster ServiceAccount when the feeder runs as a pod.
func feedK8sRESTConfig(opts *feedK8sOptions) (*rest.Config, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if opts.kubeconfig != "" {
		rules.ExplicitPath = opts.kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{CurrentContext: opts.kubeContext}
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err == nil {
		return config, nil
	}
	inCluster, inClusterErr := rest.InClusterConfig()
	if inClusterErr != nil {
		return nil, fmt.Errorf("k8s: no usable credential: %w (in-cluster: %w)", err, inClusterErr)
	}
	return inCluster, nil
}

// feedK8sEmitter builds the output and the function that decides what the command exits with.
func feedK8sEmitter(cmd *cobra.Command, global *globalOptions, opts *feedK8sOptions,
	desc feeder.Description, log *slog.Logger,
) (feeder.Emitter, func(error) error, error) {
	if opts.dryRun {
		dry := &dryRunEmitter{desc: desc, printer: newPrinter(cmd.OutOrStdout(), global.Output)}
		return dry, func(runErr error) error {
			if dry.refused > 0 {
				return exitErrorf(ExitRejected, "%d of %d events would be refused; the first is %s",
					dry.refused, dry.emitted+dry.refused, dry.firstRefusal)
			}
			if runErr != nil {
				return feedK8sRunError(runErr)
			}
			log.Info("k8s: dry run complete", "events", dry.emitted)
			return nil
		}, nil
	}

	if _, err := normalizeServerURL(global.Server); err != nil {
		return nil, nil, err
	}
	em, err := emit.NewConnectEmitter(global.Server, global.Token, desc,
		emit.WithBatchSize(opts.batchSize),
		emit.WithLogger(log),
		emit.WithResultFunc(func(result *graphv1.IngestResult) {
			if result.GetStatus() == graphv1.IngestResult_REJECTED {
				log.Error("k8s: the graph refused an event",
					"event_id", result.GetEventId(),
					"reason_code", result.GetReasonCode(),
					"reason_detail", result.GetReasonDetail())
			}
		}))
	if err != nil {
		return nil, nil, exitWith(ExitUsage, err)
	}
	return em, func(runErr error) error {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(cmd.Context()), clientTimeout)
		defer cancel()
		closeErr := em.Close(closeCtx)
		stats := em.Stats()
		log.Info("k8s: run finished",
			"applied", stats.Applied, "duplicate_noop", stats.DuplicateNoop,
			"rejected", stats.Rejected, "batches", stats.Batches, "dropped", stats.Dropped)
		if stats.Rejected > 0 {
			return exitErrorf(ExitRejected, "the graph refused %d events; see the log for the reason codes",
				stats.Rejected)
		}
		if runErr != nil {
			return feedK8sRunError(runErr)
		}
		return exitWith(ExitTransport, closeErr)
	}, nil
}

// feedK8sRunError maps a failure out of Run onto the published exit codes. A credential the
// feeder refuses is exit 3: it is the operator's authorization that is wrong, not the network
// (contracts/cli.md §Exit codes).
func feedK8sRunError(err error) error {
	if strings.Contains(err.Error(), "FR-046") || strings.Contains(err.Error(), "credential can write") {
		return exitWith(ExitAuth, err)
	}
	return exitWith(ExitTransport, err)
}

// feedK8sWriteManifest completes a recording, so that `--record` leaves a directory
// `fixture verify` can take (the SDK guide §9).
func feedK8sWriteManifest(cmd *cobra.Command, opts *feedK8sOptions, desc feeder.Description,
	payloads *record.PayloadRecorder, events *record.EventRecorder, log *slog.Logger,
) {
	if err := payloads.Err(); err != nil {
		log.Error("k8s: recording payloads failed", "error", err.Error())
	}
	if err := events.Err(); err != nil {
		log.Error("k8s: recording events failed", "error", err.Error())
	}
	err := record.WriteManifest(opts.recordDir, record.Manifest{
		Family: "k8s-topology",
		Description: fmt.Sprintf(
			"%d watch events recorded by `feed k8s` from cluster %s, namespaces %s. "+
				"Secret values are already digested; check the ConfigMaps and the host names, "+
				"and add a queries: list before committing.",
			payloads.Count(), opts.clusterName, feedK8sNamespaceLabel(opts.namespaces)),
		Sources:        []record.ManifestSource{record.SourceOf(desc)},
		ExpectRejected: events.Rejections(),
	})
	if err != nil {
		log.Error("k8s: writing the fixture manifest failed", "error", err.Error())
		return
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "recorded %d payloads and %d events to %s\n",
		payloads.Count(), events.Accepted(), opts.recordDir)
}

// feedK8sNamespaceLabel renders the watched namespaces for a log line.
func feedK8sNamespaceLabel(namespaces []string) string {
	if len(namespaces) == 0 {
		return "all"
	}
	return strings.Join(namespaces, ",")
}

// feedK8sReplayClusterName is the cluster a replay assumes when none is given. It is derived
// from nothing in the recording on purpose: a recording's payloads do not name their cluster,
// so the honest default is a visibly placeholder name rather than one guessed from a path.
func feedK8sReplayClusterName(string) string { return "replay" }
