// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/internal/investigation/workers/logs"
	"github.com/Pierre-Theophile/aisre/internal/investigation/workers/metrics"
	"github.com/Pierre-Theophile/aisre/internal/investigation/workers/traces"
	sdk "github.com/Pierre-Theophile/aisre/pkg/backend"
	"github.com/Pierre-Theophile/aisre/pkg/worker"
)

// `worker list`, `worker call` and `worker record` (tasks.md T039, T041; contracts/cli.md
// §Workers and backends; FR-010, FR-057d, FR-042b).
//
// `worker call` is the command a human uses to re-run an evidence item by hand (FR-057d). That is
// its whole reason for existing: an investigation's output cites a digest, and the person reading
// it has to be able to ask the same question again — in the same algebra, against the same
// recording — and get the same answer, without running the engine. If that were not possible the
// citation would be a claim rather than a reference.
//
// Three things it does deliberately:
//
//   - It refuses anything outside the algebra with exit 1, naming what was asked and what is
//     available. An SRE who mistypes a term should read the algebra, not a stack trace.
//   - It requires `--question`, defaulting to `exploratory:manual` rather than to nothing, so
//     that a hand-issued call is recorded the same way an engine-issued one is (FR-018a).
//   - It runs against a **recording** by default. `--mode live` exists, but there is no live
//     telemetry backend in this build: the ones this feature ships are `recorded` and
//     `synthetic`, and 003's GCP backend is the first live one.

func newWorkerCommand(global *globalOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "worker",
		Short: "Inspect and call the investigation's workers",
		Long: "A worker is a read-only capability provider bound to exactly one source of truth. It is\n" +
			"what the investigator calls; it is never what the investigator reads around.\n\n" +
			"Every worker answers in the published query algebra and in nothing else, returns digests\n" +
			"rather than telemetry, and runs in both live and recorded mode with identical outputs for\n" +
			"identical inputs (contracts/worker-sdk.md).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return exitWith(ExitUsage, cmd.Help())
		},
	}
	cmd.AddCommand(
		newWorkerListCommand(global),
		newWorkerCallCommand(global),
		newWorkerRecordCommand(global),
	)
	return cmd
}

// workerSet builds the workers that do not need a database: the three telemetry workers over one
// backend. The graph worker needs a query engine and is built where one exists (`worker record`,
// and Phase 6's engine loop).
func workerSet(b sdk.TelemetryBackend) (*worker.Registry, error) {
	registry := worker.NewRegistry()
	for _, w := range []worker.Worker{
		metrics.New(b),
		traces.New(b),
		logs.New(b),
	} {
		if err := registry.Register(w); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

func newWorkerListCommand(global *globalOptions) *cobra.Command {
	var recording string

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the registered workers, their capabilities, modes and redaction",
		Long: "list prints every registered worker with its single source of truth, the algebra terms it\n" +
			"answers and what each costs, whether a model touched its answers, the redaction policy it\n" +
			"applies and the modes it supports.\n\n" +
			"`contains_model` is declared, not discovered (FR-009a): a worker whose answer a model\n" +
			"touched says so here, and its digest states what the model added over the algorithm.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			b, err := backendFor(recording)
			if err != nil {
				return err
			}
			registry, err := workerSet(b)
			if err != nil {
				return exitWith(ExitUsage, err)
			}
			return renderWorkerList(cmd, global, registry.Descriptions())
		},
	}
	cmd.Flags().StringVar(&recording, "recording", "",
		"directory holding a recorded world/ the workers answer from (default: the synthetic backend)")
	return cmd
}

func renderWorkerList(cmd *cobra.Command, global *globalOptions, descriptions []worker.Description) error {
	p := newPrinter(cmd.OutOrStdout(), global.Output)
	if p.json() {
		rows := make([]map[string]any, 0, len(descriptions))
		for _, d := range descriptions {
			capabilities := make([]map[string]any, 0, len(d.Capabilities))
			for _, c := range d.Capabilities {
				capabilities = append(capabilities, map[string]any{
					"name":       c.Name,
					"read_only":  c.ReadOnly,
					"cost_class": sdk.CostClassName(c.CostClass),
				})
			}
			modes := make([]string, 0, len(d.Modes))
			for _, m := range d.Modes {
				modes = append(modes, m.String())
			}
			rows = append(rows, map[string]any{
				"name":             d.Name,
				"source_of_truth":  d.SourceOfTruth,
				"capabilities":     capabilities,
				"contains_model":   d.ContainsModel,
				"model_id":         d.ModelID,
				"redaction_policy": d.Redaction.PolicyVersion,
				"modes":            modes,
				"version":          d.Version,
				"sdk_version":      worker.SDKVersion,
				"algebra_version":  engine.AlgebraVersion,
			})
		}
		return p.writeJSON(map[string]any{"workers": rows})
	}

	var b strings.Builder
	for _, d := range descriptions {
		fmt.Fprintf(&b, "%s (v%s)\n", d.Name, d.Version)
		fmt.Fprintf(&b, "  source of truth  %s\n", d.SourceOfTruth)
		terms := make([]string, 0, len(d.Capabilities))
		for _, c := range d.Capabilities {
			terms = append(terms, fmt.Sprintf("%s [%s]", c.Name, sdk.CostClassName(c.CostClass)))
		}
		sort.Strings(terms)
		fmt.Fprintf(&b, "  capabilities     %s\n", strings.Join(terms, ", "))
		model := "no"
		if d.ContainsModel {
			model = d.ModelID
		}
		fmt.Fprintf(&b, "  contains model   %s\n", model)
		fmt.Fprintf(&b, "  redaction        policy %s\n", d.Redaction.PolicyVersion)
		modes := make([]string, 0, len(d.Modes))
		for _, m := range d.Modes {
			modes = append(modes, m.String())
		}
		fmt.Fprintf(&b, "  modes            %s\n\n", strings.Join(modes, ", "))
	}
	return p.writeRaw(b.String())
}

type workerCallOptions struct {
	args       string
	mode       string
	recording  string
	hypothesis string
	question   string
	exemplars  bool
}

func newWorkerCallCommand(global *globalOptions) *cobra.Command {
	opts := &workerCallOptions{}

	cmd := &cobra.Command{
		Use:   "call <worker> <term>",
		Short: "Issue one algebra term by hand and print the digest",
		Long: "call issues exactly one published algebra term and prints the digest it returns — the way a\n" +
			"human re-runs an evidence item an investigation cited (FR-057d).\n\n" +
			"--args takes the term's arguments as JSON in the published protobuf JSON spelling, e.g.\n" +
			"  worker call metrics compare --args '{\"pointer\":{\"selector\":\"...\"},\n" +
			"    \"windows\":{\"baseline\":{\"start\":\"...\",\"end\":\"...\"},\n" +
			"               \"symptom\":{\"start\":\"...\",\"end\":\"...\"}},\"statistic\":\"ERROR_RATE\"}'\n\n" +
			"A term outside the algebra exits 1, naming what was asked and what is available: the\n" +
			"algebra is the whole surface, and free-form query text is never executed.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorkerCall(cmd, global, opts, args[0], args[1])
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&opts.args, "args", "{}", "the term's arguments as protobuf JSON")
	flags.StringVar(&opts.mode, "mode", "recorded", "live|recorded (recorded makes no network call)")
	flags.StringVar(&opts.recording, "recording", "",
		"directory holding a recorded world/ to answer from")
	flags.StringVar(&opts.hypothesis, "hypothesis", "",
		"the hypothesis this call serves, recorded with it (FR-018a)")
	flags.StringVar(&opts.question, "question", "exploratory:manual",
		"the discriminating question this call is meant to settle (FR-018a)")
	flags.BoolVar(&opts.exemplars, "exemplars", false,
		"ask for bounded, sanitised exemplars; they are never returned by default (FR-014)")
	return cmd
}

func runWorkerCall(cmd *cobra.Command, global *globalOptions, opts *workerCallOptions, workerName, termName string) error {
	mode := worker.Mode(opts.mode)
	if !mode.Valid() {
		return exitErrorf(ExitUsage, "--mode %q is neither %q nor %q", opts.mode, worker.ModeLive, worker.ModeRecorded)
	}
	term, err := termFromArgs(termName, opts.args)
	if err != nil {
		return exitWith(ExitUsage, err)
	}

	b, err := backendFor(opts.recording)
	if err != nil {
		return err
	}
	registry, err := workerSet(b)
	if err != nil {
		return exitWith(ExitUsage, err)
	}

	caller := worker.NewCaller(registry, worker.RetryPolicy{})
	resp, record, err := caller.Call(cmd.Context(), workerName, worker.Request{
		Capability: termName,
		Mode:       mode,
		Algebra: &engine.Request{
			Term:                   term,
			ServesHypothesisId:     opts.hypothesis,
			DiscriminatingQuestion: opts.question,
			WantExemplars:          opts.exemplars,
		},
	})
	if err != nil {
		// Every refusal this path produces is a usage error: an undeclared capability, a term
		// outside the algebra, a malformed window. Exit 1, naming what was wrong.
		return exitWith(ExitUsage, err)
	}
	return renderWorkerCall(cmd, global, resp, record)
}

func renderWorkerCall(cmd *cobra.Command, global *globalOptions, resp worker.Response, record worker.CallRecord) error {
	p := newPrinter(cmd.OutOrStdout(), global.Output)
	if p.json() {
		encoded, err := protojson.Marshal(resp.Algebra)
		if err != nil {
			return exitWith(ExitTransport, err)
		}
		var body any
		if err := json.Unmarshal(encoded, &body); err != nil {
			return exitWith(ExitTransport, err)
		}
		return p.writeJSON(map[string]any{
			"worker":     record.Worker,
			"capability": record.Capability,
			"mode":       record.Mode.String(),
			"cost_class": sdk.CostClassName(record.CostClass),
			"term_key":   record.TermKey,
			"attempts":   len(record.Attempts),
			"response":   body,
		})
	}

	outcome, err := engine.OutcomeOf(resp.Algebra)
	if err != nil {
		return exitWith(ExitTransport, err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", record.Summary())
	fmt.Fprintf(&b, "  %s\n", outcome.Render())
	if key := resp.Algebra.GetTermKey(); key != "" {
		fmt.Fprintf(&b, "  term key         %s\n", key)
		fmt.Fprintf(&b, "  response digest  %s\n", resp.Algebra.GetResponseDigest())
	}
	if coverage := resp.Algebra.GetDigest().GetCoverage(); coverage != nil {
		fmt.Fprintf(&b, "  coverage         %s over %s..%s, %d considered\n",
			coverage.GetDataSource(),
			coverage.GetWindowActuallyCovered().GetStart().AsTime().Format("15:04:05"),
			coverage.GetWindowActuallyCovered().GetEnd().AsTime().Format("15:04:05"),
			coverage.GetVolumeConsidered())
	}
	if truncated := resp.Algebra.GetDigest().GetTruncation(); truncated.GetTruncated() {
		fmt.Fprintf(&b, "  truncated        %s (%s)\n", truncated.GetWhatWasDropped(), truncated.GetCriterion())
	}
	if text := resp.Algebra.GetDigest().GetFreeText(); text != "" {
		fmt.Fprintf(&b, "  note             %s\n", text)
	}
	fmt.Fprintf(&b, "\n%s\n", renderDigestBody(resp.Algebra.GetDigest()))
	return p.writeRaw(b.String())
}

// renderDigestBody prints the rows of whichever digest family came back. Each family renders
// differently on purpose: a reader who cannot tell a log digest from a metric digest at a glance
// has been handed a wall of numbers.
func renderDigestBody(d *investigationv1.Digest) string {
	var b strings.Builder
	switch body := d.GetBody().(type) {
	case *investigationv1.Digest_Metric:
		for _, comparison := range body.Metric.GetComparisons() {
			fmt.Fprintf(&b, "  %s\n", metrics.Describe(comparison))
		}
		for _, series := range body.Metric.GetSeries() {
			fmt.Fprintf(&b, "  series %v: %d points, %v\n", series.GetTags(), series.GetPointCount(), series.GetStatistics())
		}
	case *investigationv1.Digest_Log:
		for _, pattern := range body.Log.GetPatterns() {
			fmt.Fprintf(&b, "  %s\n", logs.Describe(pattern))
		}
		fmt.Fprintf(&b, "  miner %s, model used: %v %s\n", body.Log.GetMinerVersion(), body.Log.GetModelUsed(), body.Log.GetModelAdded())
	case *investigationv1.Digest_Trace:
		for _, group := range body.Trace.GetGroups() {
			fmt.Fprintf(&b, "  %s\n", traces.Describe(group))
		}
	case *investigationv1.Digest_MonitorState:
		fmt.Fprintf(&b, "  %s → %s\n", body.MonitorState.GetStateAtStart(), body.MonitorState.GetStateAtEnd())
		for _, transition := range body.MonitorState.GetTransitions() {
			fmt.Fprintf(&b, "  %s %s → %s (%s)\n", transition.GetAt().AsTime().Format("15:04:05"),
				transition.GetFromState(), transition.GetToState(), transition.GetGroupKey())
		}
	case *investigationv1.Digest_ErrorsByVersion:
		fmt.Fprintf(&b, "  split by %s\n", body.ErrorsByVersion.GetVersionAttribute())
		for _, version := range body.ErrorsByVersion.GetVersions() {
			fmt.Fprintf(&b, "  %-16s %d/%d = %.6f\n", version.GetVersion(),
				version.GetErrors(), version.GetTotal(), version.GetErrorRate())
		}
	case *investigationv1.Digest_Onset:
		if body.Onset.GetUnavailable() {
			fmt.Fprintf(&b, "  onset unavailable: %s (the alert instant stands; nobody is exonerated on timing alone)\n",
				body.Onset.GetUnavailableReason())
			break
		}
		fmt.Fprintf(&b, "  onset %s ±%ds by %s %v\n",
			body.Onset.GetEstimatedOnset().AsTime().Format("15:04:05"),
			body.Onset.GetUncertaintySeconds(), body.Onset.GetMethod(), body.Onset.GetMethodParameters())
	case *investigationv1.Digest_Exemplars:
		for _, exemplar := range body.Exemplars.GetExemplars() {
			fmt.Fprintf(&b, "  %s\n", exemplar.GetText())
		}
	case *investigationv1.Digest_Knowledge:
		for _, item := range body.Knowledge.GetItems() {
			fmt.Fprintf(&b, "  %.6f  %s\n", item.GetScore(), item.GetCitation())
		}
	default:
		fmt.Fprintf(&b, "  (no digest body)\n")
	}
	return b.String()
}

// termFromArgs parses the term's arguments into the typed term named by termName. The term name
// selects the message, which is what keeps free-form query text out of the algebra: there is no
// spelling of `--args` that produces a selector the graph did not publish.
func termFromArgs(termName, args string) (*engine.Term, error) {
	if strings.TrimSpace(args) == "" {
		args = "{}"
	}
	unmarshal := protojson.UnmarshalOptions{DiscardUnknown: false}

	switch termName {
	case engine.TermCompare:
		msg := &investigationv1.CompareTerm{}
		if err := unmarshal.Unmarshal([]byte(args), msg); err != nil {
			return nil, fmt.Errorf("--args for %s: %w", termName, err)
		}
		return engine.Compare(msg.GetPointer(), msg.GetWindows(), msg.GetStatistic()), nil
	case engine.TermOnset:
		msg := &investigationv1.OnsetTerm{}
		if err := unmarshal.Unmarshal([]byte(args), msg); err != nil {
			return nil, fmt.Errorf("--args for %s: %w", termName, err)
		}
		method := msg.GetMethod()
		if method == investigationv1.OnsetMethod_ONSET_METHOD_UNSPECIFIED {
			method = investigationv1.OnsetMethod_SEASONAL_CUSUM
		}
		return engine.Onset(msg.GetPointer(), msg.GetSearchWindow(), method), nil
	case engine.TermNewLogPatterns:
		msg := &investigationv1.NewLogPatternsTerm{}
		if err := unmarshal.Unmarshal([]byte(args), msg); err != nil {
			return nil, fmt.Errorf("--args for %s: %w", termName, err)
		}
		return engine.NewLogPatterns(msg.GetPointer(), msg.GetWindow(), msg.GetBaselineWindow()), nil
	case engine.TermErrorSpans:
		msg := &investigationv1.ErrorSpansTerm{}
		if err := unmarshal.Unmarshal([]byte(args), msg); err != nil {
			return nil, fmt.Errorf("--args for %s: %w", termName, err)
		}
		return engine.ErrorSpans(msg.GetSrcEntityId(), msg.GetDstEntityId(), msg.GetEdgeType(), msg.GetWindow()), nil
	case engine.TermErrorsByVersion:
		msg := &investigationv1.ErrorsByVersionTerm{}
		if err := unmarshal.Unmarshal([]byte(args), msg); err != nil {
			return nil, fmt.Errorf("--args for %s: %w", termName, err)
		}
		return engine.ErrorsByVersion(msg.GetPointer(), msg.GetWindow(), msg.GetVersionAttribute()), nil
	case engine.TermMonitorState:
		msg := &investigationv1.MonitorStateTerm{}
		if err := unmarshal.Unmarshal([]byte(args), msg); err != nil {
			return nil, fmt.Errorf("--args for %s: %w", termName, err)
		}
		return engine.MonitorState(msg.GetPointer(), msg.GetWindow()), nil
	case engine.TermExemplars:
		msg := &investigationv1.ExemplarsTerm{}
		if err := unmarshal.Unmarshal([]byte(args), msg); err != nil {
			return nil, fmt.Errorf("--args for %s: %w", termName, err)
		}
		return engine.Exemplars(msg.GetHandle(), msg.GetLimit()), nil
	case engine.TermDrillDown:
		msg := &investigationv1.DrillDownTerm{}
		if err := unmarshal.Unmarshal([]byte(args), msg); err != nil {
			return nil, fmt.Errorf("--args for %s: %w", termName, err)
		}
		return engine.DrillDown(msg.GetHandle()), nil
	case engine.TermKnowledgeSearch:
		msg := &investigationv1.KnowledgeSearchTerm{}
		if err := unmarshal.Unmarshal([]byte(args), msg); err != nil {
			return nil, fmt.Errorf("--args for %s: %w", termName, err)
		}
		return engine.KnowledgeSearch(msg.GetEntityIds(), msg.GetQueryTerms(), msg.GetLimit()), nil
	default:
		return nil, engine.OutsideAlgebra(termName)
	}
}

// backendFor returns the backend a command answers from: a recorded world when one is named, and
// the synthetic stand-in otherwise. There is no live branch, because this build ships no live
// telemetry backend — 003's GCP backend is the first.
func backendFor(recording string) (sdk.TelemetryBackend, error) {
	if strings.TrimSpace(recording) == "" {
		return nil, exitErrorf(ExitUsage,
			"no --recording directory given: a worker answers from a recorded world unless it is given a live backend. This build ships `recorded` (answers from a world), `synthetic` (generates a fixture's telemetry from its own graph, via `fixture record-world`) and `gcp` (live, over Cloud Monitoring and Cloud Logging); `backend list --gcp <org-slug>` states what the last one serves")
	}
	dir := recording
	if base := worldDirOf(recording); base != "" {
		dir = base
	}
	b, err := engine.NewRecorded(dir)
	if err != nil {
		return nil, exitWith(ExitUsage, err)
	}
	return b, nil
}

// worldDirOf accepts either a world directory or the fixture directory that contains one, so that
// `--recording fixtures/incidents/foo` and `--recording fixtures/incidents/foo/world` both work.
func worldDirOf(dir string) string {
	if _, err := os.Stat(dir + "/" + sdk.IndexFile); err == nil {
		return dir
	}
	nested := dir + "/world"
	if _, err := os.Stat(nested + "/" + sdk.IndexFile); err == nil {
		return nested
	}
	return ""
}
