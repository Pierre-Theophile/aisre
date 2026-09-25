// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1/investigationv1connect"
	"github.com/Pierre-Theophile/aisre/internal/investigation/intake"
	"github.com/Pierre-Theophile/aisre/internal/investigation/render"
)

// `investigate …` (T088, contracts/cli.md §Investigate, FR-046, FR-053, FR-064).
//
// Thirteen commands, and one rule they all obey: **both renderings come from the same run**
// (FR-064). `--output table` prints the published order — verdict, ranked list, timeline,
// narrative — and `--output json` prints the canonical serialisation of the same
// `Investigation` message. Neither is derived from the other and neither contains a claim the
// other does not, because both are projections of one structure (internal/investigation/render).
//
// Exit codes are 001's, unchanged (contracts/cli.md §Exit codes):
//
//	0 ok · 1 usage · 2 transport · 3 auth · 4 verification
//
// Two of them carry this feature's specific meanings. Exit 3 is what a write-scoped or anonymous
// credential gets (FR-008, FR-066) — the server refuses and remoteError maps
// Unauthenticated/PermissionDenied onto it. Exit 4 is a replay divergence (FR-039–FR-041): the
// recording and the run disagree, which is a verification failure in exactly the sense `fixture
// verify` already uses.
//
// The commands are grouped the way an on-call uses them: run one (`investigate`, `declare`), read
// one (`get`, `list`, `watch`, `report`), move the record on (`fact`, `review`, `label`), and
// take it out of the tool (`export`, `replay`, `to-incident`).

func newInvestigateCommand(global *globalOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "investigate [<ns>=<value>]",
		Short: "Run and read investigations",
		Long: "investigate runs an investigation on a node reference, an alert or a human\n" +
			"declaration, and reads what previous runs concluded.\n\n" +
			"Every command produces both renderings from the same run: the human form ordered\n" +
			"verdict → ranked list with exonerations → timeline → narrative, and the machine form\n" +
			"under `--output json` with the same claims and no others (FR-064).\n\n" +
			"The engine is report-only. It never proposes, prepares or executes a remediation:\n" +
			"what it says about what to do next is limited to what to investigate or observe\n" +
			"(FR-028).",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return exitWith(ExitUsage, cmd.Help())
			}
			return exitErrorf(ExitUsage,
				"investigate %s: --at is required; an investigation is about an instant (FR-001)", args[0])
		},
	}
	// The run flags live on the root `investigate` command, because `investigate
	// otel.service.name=checkout --at T` is the shape contracts/cli.md publishes.
	newInvestigateRunCommand(global, cmd)

	cmd.AddCommand(
		newInvestigateDeclareCommand(global),
		newInvestigateGetCommand(global),
		newInvestigateListCommand(global),
		newInvestigateWatchCommand(global),
		newInvestigateExportCommand(global),
		newInvestigateReplayCommand(global),
		newInvestigateFactCommand(global),
		newInvestigateReviewCommand(global),
		newInvestigateLabelCommand(global),
		newInvestigateReportCommand(global),
		newInvestigateToIncidentCommand(global),
	)
	return cmd
}

// runOptions are the flags of `investigate <ref> --at T`.
type runOptions struct {
	at         string
	observedAt string
	lookback   time.Duration
	profile    string
	reviewMode bool
	watch      bool
	alert      string
}

func newInvestigateRunCommand(global *globalOptions, cmd *cobra.Command) {
	o := &runOptions{}
	flags := cmd.Flags()
	flags.StringVar(&o.at, "at", "", "the instant to investigate (RFC 3339, `-30m` or `now`)")
	flags.StringVar(&o.observedAt, "observed-at", "",
		"what the investigation is entitled to know; defaults to --at (FR-004)")
	flags.DurationVar(&o.lookback, "lookback", intake.DefaultLookback, "look-back window")
	flags.StringVar(&o.profile, "profile", "page", "budget profile: page or review")
	flags.BoolVar(&o.reviewMode, "review-mode", false,
		"observe as of now rather than the symptom instant; recorded on the investigation (FR-005)")
	flags.BoolVar(&o.watch, "watch", false, "stream the anytime shape as it is produced")
	flags.StringVar(&o.alert, "alert", "",
		"run from a normalised alert intake in a file, or `-` for stdin")

	inner := cmd.RunE
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if o.alert == "" && len(args) == 0 {
			return inner(cmd, args)
		}
		return runInvestigate(cmd, global, o, args)
	}
}

func runInvestigate(cmd *cobra.Command, global *globalOptions, o *runOptions, args []string) error {
	req := &investigationv1.InvestigateRequest{
		Profile:    o.profile,
		ReviewMode: o.reviewMode,
		Stream:     o.watch,
	}
	if o.lookback > 0 {
		req.LookbackSeconds = int64(o.lookback / time.Second)
	}

	switch {
	case o.alert != "":
		symptom, err := readAlertIntake(cmd, o.alert)
		if err != nil {
			return err
		}
		req.Symptom = symptom
		if symptom.GetFiredAt() != nil {
			req.ValidAt = symptom.GetFiredAt()
		}
	default:
		ref, err := parseFocusRef(args[0])
		if err != nil {
			return err
		}
		req.Symptom = &investigationv1.Symptom{
			Transport:        intake.TransportExplicitReference,
			Statement:        "explicit investigation of " + ref.String(),
			OriginSystem:     "investigate",
			OriginRef:        ref.String(),
			NamedIdentifiers: []*graphv1.Ref{ref.Proto()},
		}
	}

	validAt, err := parseInstant("--at", o.at)
	if err != nil {
		return err
	}
	if !validAt.IsZero() {
		req.ValidAt = timestamppb.New(validAt)
		if req.GetSymptom().GetFiredAt() == nil {
			req.Symptom.FiredAt = timestamppb.New(validAt)
		}
	}
	if req.GetValidAt() == nil {
		return exitErrorf(ExitUsage,
			"--at is required: an investigation pins the instant it is about (FR-001, FR-004)")
	}
	observedAt, err := parseInstant("--observed-at", o.observedAt)
	if err != nil {
		return err
	}
	if !observedAt.IsZero() {
		req.ObservedAt = timestamppb.New(observedAt)
	}

	clients, err := newInvestigationClient(global)
	if err != nil {
		return err
	}
	stream, err := clients.Investigate(cmd.Context(), connect.NewRequest(req))
	if err != nil {
		return remoteError("investigate", err)
	}
	return renderStream(cmd, global, stream, o.watch)
}

// readAlertIntake reads a normalised alert from a file or stdin. It is deliberately the
// `Symptom` message rather than a bespoke shape: intake normalises both front doors into it, so
// a connector that can write one can drive the CLI.
func readAlertIntake(cmd *cobra.Command, path string) (*investigationv1.Symptom, error) {
	var raw []byte
	var err error
	if path == "-" {
		raw, err = io.ReadAll(cmd.InOrStdin())
	} else {
		raw, err = readFile(path)
	}
	if err != nil {
		return nil, exitErrorf(ExitUsage, "--alert %s: %v", path, err)
	}
	var symptom investigationv1.Symptom
	if err := unmarshalJSON(raw, &symptom); err != nil {
		return nil, exitErrorf(ExitUsage,
			"--alert %s: not a sreagent.investigation.v1.Symptom: %v", path, err)
	}
	if symptom.GetFiredAt() == nil {
		return nil, exitErrorf(ExitUsage,
			"--alert %s: the symptom carries no instant; the engine will not substitute the "+
				"instant it observed the alert (FR-004a)", path)
	}
	return &symptom, nil
}

// declareOptions are the flags of `investigate declare`.
type declareOptions struct {
	severity string
	at       string
	title    string
	origin   string
	services []string
	lookback time.Duration
	profile  string
	watch    bool
}

func newInvestigateDeclareCommand(global *globalOptions) *cobra.Command {
	o := &declareOptions{}
	cmd := &cobra.Command{
		Use:   "declare",
		Short: "Declare an incident: a first-class intake with no monitor",
		Long: "declare opens an investigation from a human declaration (FR-001a). No monitor need\n" +
			"have fired.\n\n" +
			"The observed instant is pinned to --at, the instant of declaration, never to the\n" +
			"instant the engine saw it (FR-004a). The idempotency key is (source, the stable id\n" +
			"of the place the incident lives, the declared instant), so re-running the same\n" +
			"command is a no-op that returns the same investigation (FR-008b).\n\n" +
			"--service is optional and is recorded as supplied by a human. With none, the\n" +
			"investigation still opens and concludes `unknown`, asking for the affected\n" +
			"services: the engine does not guess one from the title (FR-002b).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDeclare(cmd, global, o)
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&o.severity, "severity", "", "severity as declared (required)")
	flags.StringVar(&o.at, "at", "", "the instant of declaration (required)")
	flags.StringVar(&o.title, "title", "", "human-readable title as declared (required)")
	flags.StringVar(&o.origin, "origin", "",
		"where the incident lives, as <system>:<stable-id> (required); keyed on the stable id, never the name")
	flags.StringArrayVar(&o.services, "service", nil,
		"affected service, as <ns>=<value>; repeatable; recorded as supplied_by_human")
	flags.DurationVar(&o.lookback, "lookback", intake.DefaultLookback, "look-back window")
	flags.StringVar(&o.profile, "profile", "page", "budget profile: page or review")
	flags.BoolVar(&o.watch, "watch", false, "stream the anytime shape as it is produced")
	return cmd
}

func runDeclare(cmd *cobra.Command, global *globalOptions, o *declareOptions) error {
	missing := make([]string, 0, 4)
	for _, f := range []struct {
		name  string
		value string
	}{
		{"--severity", o.severity}, {"--at", o.at}, {"--title", o.title}, {"--origin", o.origin},
	} {
		if strings.TrimSpace(f.value) == "" {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return exitErrorf(ExitUsage,
			"investigate declare: %s required; none of severity, instant, title or origin is "+
				"ever inferred (FR-002a)", strings.Join(missing, ", "))
	}
	system, stableID, err := parseOrigin(o.origin)
	if err != nil {
		return err
	}
	declaredAt, err := parseInstant("--at", o.at)
	if err != nil {
		return err
	}

	targets := make([]*investigationv1.TargetRef, 0, len(o.services))
	for _, raw := range o.services {
		ref, err := parseFocusRef(raw)
		if err != nil {
			return err
		}
		targets = append(targets, &investigationv1.TargetRef{
			Ref: ref.Proto(),
			// A service a person typed on the command line is supplied by a human, never parsed
			// (FR-002b). The parse-rule registry belongs to the connector that observes
			// declarations, and this command is not one.
			Provenance: investigationv1.TargetRefProvenance_SUPPLIED_BY_HUMAN,
		})
	}

	req := &investigationv1.DeclareRequest{
		Declaration: &investigationv1.Symptom{
			OriginSystem:   system,
			OriginRef:      o.origin,
			Transport:      intake.TransportHumanDeclared,
			Statement:      o.title,
			FiredAt:        timestamppb.New(declaredAt),
			Origin:         investigationv1.IntakeOrigin_INTAKE_ORIGIN_DECLARED,
			Severity:       o.severity,
			Title:          o.title,
			TargetRefs:     targets,
			IdempotencyKey: intake.DeclarationKey(system, stableID, declaredAt),
		},
		Profile: o.profile,
	}
	for _, t := range targets {
		req.Declaration.NamedIdentifiers = append(req.Declaration.NamedIdentifiers, t.GetRef())
	}
	if o.lookback > 0 {
		req.LookbackSeconds = int64(o.lookback / time.Second)
	}

	clients, err := newInvestigationClient(global)
	if err != nil {
		return err
	}
	stream, err := clients.Declare(cmd.Context(), connect.NewRequest(req))
	if err != nil {
		return remoteError("investigate declare", err)
	}
	return renderStream(cmd, global, stream, o.watch)
}

// parseOrigin splits `<system>:<stable-id>`. The split is on the first colon, so an identifier
// that itself contains colons — which chat and ticket identifiers often do — survives intact.
func parseOrigin(raw string) (system, stableID string, err error) {
	system, stableID, found := strings.Cut(strings.TrimSpace(raw), ":")
	if !found || system == "" || stableID == "" {
		return "", "", exitErrorf(ExitUsage,
			"--origin %q: write it as <system>:<stable-id> (e.g. slack:C0123ABCD). The stable "+
				"identifier is what the idempotency key rests on, never the channel's name (FR-008b)", raw)
	}
	return system, stableID, nil
}

// newInvestigationClient builds the Connect client for InvestigationService.
func newInvestigationClient(global *globalOptions) (investigationv1connect.InvestigationServiceClient, error) {
	base, err := normalizeServerURL(global.Server)
	if err != nil {
		return nil, err
	}
	httpClient := &http.Client{
		Transport: &bearerTransport{base: http.DefaultTransport, token: global.Token},
		// An investigation runs for longer than a query: the `page` profile's wall-time budget
		// is minutes, not seconds, and a client timeout shorter than the server's budget would
		// look like a transport failure to whoever is reading the pager.
		Timeout: investigationTimeout,
	}
	return investigationv1connect.NewInvestigationServiceClient(httpClient, base), nil
}

// investigationTimeout bounds one CLI-driven investigation. It is deliberately longer than
// clientTimeout: the budget manager stops the run, not the HTTP client.
const investigationTimeout = 15 * time.Minute

// renderStream consumes the server stream and prints the answer.
//
// With `--watch` every state is printed as it arrives — the provisional prior-only ranking, then
// the first wave, then each turn (FR-046a). Without it only the last one is, because an on-call
// who did not ask to watch wants the answer and not the working.
func renderStream(
	cmd *cobra.Command,
	global *globalOptions,
	stream *connect.ServerStreamForClient[investigationv1.Investigation],
	watch bool,
) error {
	defer func() { _ = stream.Close() }()

	var last *investigationv1.Investigation
	for stream.Receive() {
		inv := stream.Msg()
		if watch {
			if err := renderInvestigation(cmd, global, inv); err != nil {
				return err
			}
		}
		last = cloneInvestigation(inv)
	}
	if err := stream.Err(); err != nil {
		return remoteError("investigate", err)
	}
	if last == nil {
		return exitErrorf(ExitTransport, "investigate: the server produced no investigation")
	}
	if watch {
		return nil
	}
	return renderInvestigation(cmd, global, last)
}

// renderInvestigation prints one investigation in the format the caller asked for.
//
// The two renderings are produced from the same message. `--output json` is the canonical
// serialisation the goldens are written in; the table form is the published order of FR-057c.
func renderInvestigation(cmd *cobra.Command, global *globalOptions, inv *investigationv1.Investigation) error {
	p := newPrinter(cmd.OutOrStdout(), global.Output)
	if p.json() {
		return p.writeJSON(inv)
	}
	text, err := renderHuman(inv)
	if err != nil {
		// A rendering the guard refused is not printed with a warning. FR-028 is a property of
		// the output, so an output that violates it is not an output.
		return exitWith(ExitVerification, err)
	}
	return p.writeRaw(text)
}

// renderHuman renders the published order from an Investigation message.
//
// It rebuilds the render.Report from the wire message rather than sharing the engine's in-memory
// one, because the CLI may be talking to a server it does not share a process with. The order,
// the deep links and the guard are the same code either way.
func renderHuman(inv *investigationv1.Investigation) (string, error) {
	report, err := render.FromProto(inv)
	if err != nil {
		return "", err
	}
	return report.Human()
}

// errNoInvestigationID is the usage error every id-taking command raises.
var errNoInvestigationID = errors.New("an investigation id is required")

func investigationIDArg(cmd *cobra.Command, args []string) (string, error) {
	if len(args) == 0 || strings.TrimSpace(args[0]) == "" {
		return "", exitErrorf(ExitUsage, "%s: %v", cmd.CommandPath(), errNoInvestigationID)
	}
	return strings.TrimSpace(args[0]), nil
}

func fmtCount(n int, singular, plural string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, singular)
	}
	return fmt.Sprintf("%d %s", n, plural)
}
