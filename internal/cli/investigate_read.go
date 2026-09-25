// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/investigation/render"
	"github.com/Pierre-Theophile/aisre/internal/investigation/replay"
	investigationstore "github.com/Pierre-Theophile/aisre/internal/investigation/store"
)

// The reading half of `investigate` (T088, contracts/cli.md §Investigate, FR-046, FR-053).
//
// `get`, `list`, `watch`, `export`, `replay` and `report`. None of them runs the engine except
// `replay`, which runs it against a recording with no network — so all of them work on a server
// whose model configuration is absent, which is what FR-067 asks for.

func newInvestigateGetCommand(global *globalOptions) *cobra.Command {
	var (
		withEvidence bool
		withLedger   bool
		withChain    bool
	)
	cmd := &cobra.Command{
		Use:   "get <id>",
		Short: "Read an investigation",
		Long: "get prints an investigation in the published order (FR-057c), or its canonical\n" +
			"serialisation under `--output json`.\n\n" +
			"--chain prints the full evidence chain in the order it was produced, which is how a\n" +
			"reviewer checks that a claim rests on what it says it rests on (FR-053).",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := investigationIDArg(cmd, args)
			if err != nil {
				return err
			}
			client, err := newInvestigationClient(global)
			if err != nil {
				return err
			}
			resp, err := client.Get(cmd.Context(),
				connect.NewRequest(&investigationv1.GetRequest{InvestigationId: id}))
			if err != nil {
				return remoteError("investigate get", err)
			}
			inv := resp.Msg
			p := newPrinter(cmd.OutOrStdout(), global.Output)
			if p.json() {
				return p.writeJSON(inv)
			}
			if err := renderInvestigation(cmd, global, inv); err != nil {
				return err
			}
			if withEvidence || withChain {
				if err := p.writeRaw(renderEvidenceChain(inv, withChain)); err != nil {
					return err
				}
			}
			if withLedger {
				if err := p.writeRaw(renderLedgerTable(inv)); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&withEvidence, "evidence", false, "print the evidence items")
	cmd.Flags().BoolVar(&withLedger, "ledger", false, "print the hypothesis ledger as a table")
	cmd.Flags().BoolVar(&withChain, "chain", false,
		"print the full evidence chain in the order it was produced")
	return cmd
}

// renderEvidenceChain prints the evidence items. Every line carries a deep link or the reason
// there is none (FR-057d) — there is no path here that emits a bare id.
func renderEvidenceChain(inv *investigationv1.Investigation, full bool) string {
	items := inv.GetLedger().GetEvidence()
	if len(items) == 0 {
		return "\nevidence: none recorded\n"
	}
	var b strings.Builder
	b.WriteString("\n## evidence\n\n")
	for _, e := range items {
		link := e.GetDeepLink()
		if link == "" {
			link = "no deep link: " + e.GetDeepLinkAbsentReason()
		}
		fmt.Fprintf(&b, "%s  %-18s %s.%s → %s (%s)\n",
			formatTime(e.GetCalledAt()), e.GetKind(), e.GetWorker(), e.GetCapability(),
			strings.ToLower(strings.TrimPrefix(e.GetOutcome().String(), "TERM_OUTCOME_")), link)
		if !full {
			continue
		}
		fmt.Fprintf(&b, "    id %s  digest %s  valid %s  observed %s  mode %s\n",
			e.GetEvidenceId(), short(e.GetResponseDigest()),
			formatTime(e.GetValidAt()), formatTime(e.GetObservedAt()), e.GetMode())
		if cov := e.GetCoverage(); cov != nil {
			fmt.Fprintf(&b, "    coverage: source %s, %d considered, executed %s\n",
				cov.GetDataSource(), cov.GetVolumeConsidered(), formatTime(cov.GetExecutedAt()))
		}
		if len(e.GetGraphEventIds()) > 0 {
			fmt.Fprintf(&b, "    graph events: %s\n", strings.Join(e.GetGraphEventIds(), ", "))
		}
	}
	return b.String()
}

func renderLedgerTable(inv *investigationv1.Investigation) string {
	var b strings.Builder
	b.WriteString("\n## ledger\n\n")
	for _, h := range inv.GetLedger().GetHypotheses() {
		bucket := ""
		if hb := h.GetBucket(); hb != nil {
			bucket = fmt.Sprintf("%s [%.2f–%.2f]", hb.GetName(), hb.GetRangeLow(), hb.GetRangeHigh())
		}
		fmt.Fprintf(&b, "%d  %-14s %-12s prior %.6f  confidence %.6f  %s  %s\n",
			h.GetRank(), strings.ToLower(h.GetStatus().String()),
			strings.ToLower(h.GetKind().String()), h.GetPrior(), h.GetConfidence(),
			bucket, h.GetStatement())
	}
	return b.String()
}

func short(digest string) string {
	if len(digest) > 12 {
		return digest[:12]
	}
	if digest == "" {
		return unknownCell
	}
	return digest
}

func newInvestigateListCommand(global *globalOptions) *cobra.Command {
	var (
		incident  string
		lifecycle string
		since     string
		limit     int
	)
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List investigations, newest first",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			req := &investigationv1.ListInvestigationsRequest{
				IncidentId: incident,
				Lifecycle:  lifecycle,
				PageSize:   int32(limit), //nolint:gosec // bounded by the flag below
			}
			if err := checkLifecycle(lifecycle); err != nil {
				return err
			}
			at, err := parseInstant("--since", since)
			if err != nil {
				return err
			}
			if !at.IsZero() {
				req.Since = timestamppb.New(at)
			}
			client, err := newInvestigationClient(global)
			if err != nil {
				return err
			}
			resp, err := client.List(cmd.Context(), connect.NewRequest(req))
			if err != nil {
				return remoteError("investigate list", err)
			}
			p := newPrinter(cmd.OutOrStdout(), global.Output)
			if p.json() {
				return p.writeJSON(resp.Msg)
			}
			rows := make([][]string, 0, len(resp.Msg.GetInvestigations()))
			for _, inv := range resp.Msg.GetInvestigations() {
				rows = append(rows, []string{
					inv.GetInvestigationId(),
					inv.GetIncidentId(),
					strings.ToLower(inv.GetLifecycle().String()),
					outcomeCell(inv),
					formatTime(inv.GetStartedAt()),
					inv.GetVerdictLine(),
				})
			}
			return p.writeTable(
				[]string{"INVESTIGATION", "INCIDENT", "LIFECYCLE", "OUTCOME", "STARTED", "VERDICT"},
				rows)
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&incident, "incident", "", "list the runs covering one incident")
	flags.StringVar(&lifecycle, "status", "",
		"list the runs in one lifecycle: running, concluded, reopened or failed")
	flags.StringVar(&since, "since", "", "list the runs started at or after an instant")
	flags.IntVar(&limit, "limit", investigationstore.DefaultListLimit, "page size")
	return cmd
}

// checkLifecycle refuses a status outside the published four. The lifecycle is a closed set
// (FR-006) and a typo that silently matched nothing would look like "no investigations".
func checkLifecycle(value string) error {
	switch value {
	case "", investigationstore.LifecycleRunning, investigationstore.LifecycleConcluded,
		investigationstore.LifecycleReopened, investigationstore.LifecycleFailed:
		return nil
	default:
		return exitErrorf(ExitUsage,
			"--status %q: the lifecycle is exactly %s, %s, %s or %s (FR-006)", value,
			investigationstore.LifecycleRunning, investigationstore.LifecycleConcluded,
			investigationstore.LifecycleReopened, investigationstore.LifecycleFailed)
	}
}

func outcomeCell(inv *investigationv1.Investigation) string {
	if inv.GetOutcome() == investigationv1.InvestigationOutcome_INVESTIGATION_OUTCOME_UNSPECIFIED {
		return unknownCell
	}
	out := strings.ToLower(strings.TrimPrefix(inv.GetOutcome().String(), "INVESTIGATION_"))
	if kind := inv.GetConclusionKind(); kind != investigationv1.ConclusionKind_CONCLUSION_KIND_UNSPECIFIED {
		out += "/" + strings.ToLower(strings.TrimPrefix(kind.String(), "CONCLUSION_"))
	}
	return out
}

func newInvestigateWatchCommand(global *globalOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "watch <id>",
		Short: "Follow an in-flight investigation",
		Long: "watch prints the calls made so far and the spend against each budget.\n\n" +
			"It polls rather than subscribes: an investigation's states are rows, and a poll that\n" +
			"reads rows cannot miss one because a connection dropped.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := investigationIDArg(cmd, args)
			if err != nil {
				return err
			}
			client, err := newInvestigationClient(global)
			if err != nil {
				return err
			}
			p := newPrinter(cmd.OutOrStdout(), global.Output)
			ticker := time.NewTicker(watchInterval)
			defer ticker.Stop()
			for {
				resp, err := client.Get(cmd.Context(),
					connect.NewRequest(&investigationv1.GetRequest{InvestigationId: id}))
				if err != nil {
					return remoteError("investigate watch", err)
				}
				inv := resp.Msg
				if p.json() {
					if err := p.writeJSON(inv); err != nil {
						return err
					}
				} else if err := p.writeRaw(watchLine(inv)); err != nil {
					return err
				}
				if inv.GetLifecycle() != investigationv1.Lifecycle_RUNNING {
					return nil
				}
				select {
				case <-cmd.Context().Done():
					return nil
				case <-ticker.C:
				}
			}
		},
	}
}

// watchInterval is how often `watch` re-reads. Two seconds is short enough to feel live and long
// enough not to be a load generator against a server that is already running an investigation.
const watchInterval = 2 * time.Second

func watchLine(inv *investigationv1.Investigation) string {
	spend := inv.GetSpend()
	var calls int64
	for _, n := range spend.GetCallsByWorker() {
		calls += n
	}
	state := strings.ToLower(inv.GetLifecycle().String())
	if inv.GetProvisional() {
		state += " (provisional: prior-only, untested)"
	}
	return fmt.Sprintf("%s  %s  %s  cost %.4f  wall %ds\n",
		formatTime(timestamppb.New(time.Now().UTC())), state,
		fmtCount(int(calls), "worker call", "worker calls"),
		spend.GetCostUnits(), spend.GetWallTimeSeconds())
}

func newInvestigateExportCommand(global *globalOptions) *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use:   "export <id> --out <dir>",
		Short: "Write the self-contained artifact",
		Long: "export writes the decision record, the ledger, both recording layers and the graph\n" +
			"events the investigation's queries need, into one directory that replays with no\n" +
			"network (FR-042).",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := investigationIDArg(cmd, args)
			if err != nil {
				return err
			}
			if strings.TrimSpace(out) == "" {
				return exitErrorf(ExitUsage, "investigate export: --out <dir> is required")
			}
			abs, err := filepath.Abs(out)
			if err != nil {
				return exitErrorf(ExitUsage, "--out %q: %v", out, err)
			}
			client, err := newInvestigationClient(global)
			if err != nil {
				return err
			}
			resp, err := client.Export(cmd.Context(), connect.NewRequest(&investigationv1.ExportRequest{
				InvestigationId: id, OutDir: abs,
			}))
			if err != nil {
				return remoteError("investigate export", err)
			}
			p := newPrinter(cmd.OutOrStdout(), global.Output)
			if p.json() {
				return p.writeJSON(resp.Msg)
			}
			return p.writeLine("exported %s to %s (digest %s, %d trajectory records, %d world terms)",
				id, resp.Msg.GetOutDir(), short(resp.Msg.GetExportDigest()),
				resp.Msg.GetTrajectoryRecordCount(), resp.Msg.GetWorldTermCount())
		},
	}
	cmd.Flags().StringVar(&out, "out", "", "directory to write the artifact into (required)")
	return cmd
}

func newInvestigateReplayCommand(global *globalOptions) *cobra.Command {
	var (
		from       string
		layer      string
		reportJSON string
		remote     bool
	)
	cmd := &cobra.Command{
		Use:   "replay --from <dir>",
		Short: "Replay an exported investigation with no network",
		Long: "replay re-runs an exported investigation against its recording. It exits 4 on a\n" +
			"divergence and names the first diverging record (FR-039, FR-040, FR-041).\n\n" +
			"Exit 4 is the same verification code `fixture verify` uses: a replay that does not\n" +
			"reproduce its recording is a failed check, not a transport problem.\n\n" +
			"It runs **in this process** against the directory, because that is what a replay is:\n" +
			"a recording on disk, re-issued through the same seams with no socket opened. A local\n" +
			"replay needs no server, no database and no credential, which is what lets the CI gate\n" +
			"run it in a job with no egress. --remote sends the same request to a server instead,\n" +
			"for an artifact that lives beside one.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(from) == "" {
				return exitErrorf(ExitUsage, "investigate replay: --from <dir> is required")
			}
			parsed, err := replay.ParseLayer(layer)
			if err != nil {
				return exitWith(ExitUsage, err)
			}
			msg, err := runReplay(cmd, global, from, parsed, reportJSON, remote)
			if err != nil {
				return err
			}
			p := newPrinter(cmd.OutOrStdout(), global.Output)
			if p.json() {
				if err := p.writeJSON(msg); err != nil {
					return err
				}
			} else if err := p.writeLine(
				"replay %s: identical=%v, not_recorded=%d, miss rate %.4f",
				parsed, msg.GetIdentical(), msg.GetNotRecordedCount(), msg.GetMissRate()); err != nil {
				return err
			}
			if !msg.GetIdentical() {
				return exitErrorf(ExitVerification,
					"replay diverged at %s", msg.GetFirstDivergingRecord())
			}
			return nil
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&from, "from", "", "the exported directory to replay (required)")
	flags.StringVar(&layer, "layer", string(replay.LayerTrajectory),
		"which layer to replay: trajectory or world")
	flags.StringVar(&reportJSON, "report-json", "", "write the replay report to this file")
	flags.BoolVar(&remote, "remote", false,
		"ask the server to replay it instead of replaying it here")
	return cmd
}

// runReplay replays locally, or asks the server to.
func runReplay(
	cmd *cobra.Command,
	global *globalOptions,
	from string,
	layer replay.Layer,
	reportJSON string,
	remote bool,
) (*investigationv1.ReplayResponse, error) {
	if !remote {
		msg, err := replay.Replay(cmd.Context(), from, replay.ReplayOptions{
			Layer: layer, ReportJSON: reportJSON,
		})
		if err != nil {
			return nil, exitWith(ExitTransport, err)
		}
		return msg, nil
	}
	client, err := newInvestigationClient(global)
	if err != nil {
		return nil, err
	}
	resp, err := client.Replay(cmd.Context(), connect.NewRequest(&investigationv1.ReplayRequest{
		ExportPath: from, Layer: string(layer),
	}))
	if err != nil {
		return nil, remoteError("investigate replay", err)
	}
	if reportJSON != "" {
		if err := writeJSONFile(reportJSON, resp.Msg); err != nil {
			return nil, err
		}
	}
	return resp.Msg, nil
}

// --- small file helpers, shared with investigate.go ----------------------------------------

func readFile(path string) ([]byte, error) { return os.ReadFile(path) } //nolint:gosec // an operator's own path

func unmarshalJSON(raw []byte, m proto.Message) error {
	return protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(raw, m)
}

func writeJSONFile(path string, m proto.Message) error {
	raw, err := protojson.MarshalOptions{Multiline: true, Indent: "  "}.Marshal(m)
	if err != nil {
		return exitWith(ExitTransport, err)
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		return exitErrorf(ExitUsage, "write %s: %v", path, err)
	}
	return nil
}

func cloneInvestigation(inv *investigationv1.Investigation) *investigationv1.Investigation {
	if inv == nil {
		return nil
	}
	out, _ := proto.Clone(inv).(*investigationv1.Investigation)
	return out
}

// renderReportBody is the body `investigate report` prints and delivers: the published order
// with a header naming the investigation.
func renderReportBody(inv *investigationv1.Investigation) (string, error) {
	report, err := render.FromProto(inv)
	if err != nil {
		return "", err
	}
	return render.DeliveryBody(report)
}
