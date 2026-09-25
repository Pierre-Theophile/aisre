// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/fixture"
	"github.com/Pierre-Theophile/aisre/internal/investigation/render"
	investigationstore "github.com/Pierre-Theophile/aisre/internal/investigation/store"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
)

// The human channel and the corpus hand-off at the CLI (T083, T084, T087, contracts/cli.md).
//
// `fact`, `review`, `label`, `report` and `to-incident`. Four of them record a human decision and
// one returns a report; all five are additive, and none of them edits an investigation (FR-007).

func newInvestigateFactCommand(global *globalOptions) *cobra.Command {
	var (
		kind      string
		statement string
		entities  []string
		from      string
		to        string
	)
	cmd := &cobra.Command{
		Use:   "fact <id>",
		Short: "Push a typed human fact at an investigation",
		Long: "fact records something a person knows that the telemetry does not.\n\n" +
			"It never blocks: the fact is written and the current state comes straight back\n" +
			"(FR-057). It is weighted as strong evidence and never as truth — where telemetry\n" +
			"contradicts it both are kept and the contradiction is stated in the affected\n" +
			"hypotheses (FR-057a).\n\n" +
			"A fact against a concluded investigation moves it to `reopened` and produces a new\n" +
			"linked record; the concluded record stays readable exactly as produced (FR-057b).",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := investigationIDArg(cmd, args)
			if err != nil {
				return err
			}
			if err := checkFactKind(kind); err != nil {
				return err
			}
			if strings.TrimSpace(statement) == "" {
				return exitErrorf(ExitUsage,
					"investigate fact: --statement is required; a fact nobody wrote down is not one")
			}
			fact := &investigationv1.HumanFact{
				Kind:        kind,
				Statement:   statement,
				WeightClass: investigationstore.WeightClassStrong,
			}
			for _, raw := range entities {
				ref, err := parseFocusRef(raw)
				if err != nil {
					return err
				}
				fact.EntityIds = append(fact.EntityIds, ref.String())
			}
			concerns, err := parseConcerns(from, to)
			if err != nil {
				return err
			}
			fact.Concerns = concerns

			client, err := newInvestigationClient(global)
			if err != nil {
				return err
			}
			// Which of the two RPCs this is depends on where the investigation is in its
			// lifecycle, and the caller should not have to know (contracts/cli.md: `fact` "pushes
			// a typed human fact; never blocks; **reopens a concluded investigation as a linked
			// record**"). A running investigation takes the fact and carries on; a concluded one
			// is reopened, which records the same fact against the parent and runs the linked
			// child — FR-057b's "moves it to reopened and produces a new linked record".
			//
			// The read comes first because Reopen is the write that records the fact: submitting
			// it and then reopening would write it twice.
			current, err := client.Get(cmd.Context(),
				connect.NewRequest(&investigationv1.GetRequest{InvestigationId: id}))
			if err != nil {
				return remoteError("investigate fact", err)
			}
			p := newPrinter(cmd.OutOrStdout(), global.Output)
			if current.Msg.GetLifecycle() == investigationv1.Lifecycle_CONCLUDED {
				reopened, err := client.Reopen(cmd.Context(),
					connect.NewRequest(&investigationv1.ReopenRequest{
						InvestigationId: id, Fact: fact,
					}))
				if err != nil {
					return remoteError("investigate fact", err)
				}
				child := reopened.Msg
				if p.json() {
					return p.writeJSON(child)
				}
				return p.writeLine(
					"fact recorded against %s, which had concluded; it is reopened as %s "+
						"(lifecycle %s). The concluded record is unchanged: "+
						"`aisre investigate get %s` (FR-057b)",
					id, child.GetInvestigationId(),
					strings.ToLower(child.GetLifecycle().String()), id)
			}
			resp, err := client.SubmitHumanFact(cmd.Context(),
				connect.NewRequest(&investigationv1.SubmitHumanFactRequest{
					InvestigationId: id, Fact: fact,
				}))
			if err != nil {
				return remoteError("investigate fact", err)
			}
			inv := resp.Msg
			if p.json() {
				return p.writeJSON(inv)
			}
			return p.writeLine("fact recorded against %s (lifecycle %s)",
				id, strings.ToLower(inv.GetLifecycle().String()))
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&kind, "kind", "", "fact kind: "+strings.Join(investigationstore.FactKinds(), ", "))
	flags.StringVar(&statement, "statement", "", "what is known, in words (required)")
	flags.StringArrayVar(&entities, "entity", nil, "entity the fact concerns, as <ns>=<value>; repeatable")
	flags.StringVar(&from, "from", "", "start of the interval the fact concerns")
	flags.StringVar(&to, "to", "", "end of the interval the fact concerns")
	return cmd
}

func checkFactKind(kind string) error {
	for _, k := range investigationstore.FactKinds() {
		if k == kind {
			return nil
		}
	}
	return exitErrorf(ExitUsage, "--kind %q: want one of %s",
		kind, strings.Join(investigationstore.FactKinds(), ", "))
}

func parseConcerns(from, to string) (*graphv1.Interval, error) {
	start, err := parseInstant("--from", from)
	if err != nil {
		return nil, err
	}
	end, err := parseInstant("--to", to)
	if err != nil {
		return nil, err
	}
	if start.IsZero() && end.IsZero() {
		return nil, nil
	}
	interval := &graphv1.Interval{}
	if !start.IsZero() {
		interval.Start = timestamppb.New(start)
	}
	if !end.IsZero() {
		interval.End = timestamppb.New(end)
	}
	return interval, nil
}

func newInvestigateReviewCommand(global *globalOptions) *cobra.Command {
	var (
		rootCause string
		amend     []string
		reason    string
	)
	cmd := &cobra.Command{
		Use:   "review <id>",
		Short: "Record a human review of a completed investigation",
		Long: "review records the validated root cause, amendments to any hypothesis's status or\n" +
			"rank, and a rationale (FR-054).\n\n" +
			"It is additive and attributable, it survives a full replay, and it takes precedence\n" +
			"over the engine's own scores wherever the two are compared. The investigation itself\n" +
			"is never edited.\n\n" +
			"--root-cause takes a change reference, or `unobserved`, or\n" +
			"`not_change_induced:<category>`. The last two are answers, not failures to answer:\n" +
			"the coverage audit's remainder is a real class of incident.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := investigationIDArg(cmd, args)
			if err != nil {
				return err
			}
			if strings.TrimSpace(rootCause) == "" {
				return exitErrorf(ExitUsage,
					"investigate review: --root-cause is required (a change reference, `unobserved`, "+
						"or `not_change_induced:<category>`)")
			}
			if strings.TrimSpace(reason) == "" {
				return exitErrorf(ExitUsage,
					"investigate review: --reason is required; a correction nobody explained teaches "+
						"the corpus nothing (FR-054, FR-056)")
			}
			amendments, err := parseAmendments(amend)
			if err != nil {
				return err
			}
			client, err := newInvestigationClient(global)
			if err != nil {
				return err
			}
			resp, err := client.Review(cmd.Context(), connect.NewRequest(&investigationv1.ReviewRequest{
				InvestigationId: id,
				Review: &investigationv1.HumanReview{
					ValidatedRootCause: rootCause,
					Amendments:         amendments,
					Rationale:          reason,
				},
			}))
			if err != nil {
				return remoteError("investigate review", err)
			}
			p := newPrinter(cmd.OutOrStdout(), global.Output)
			if p.json() {
				return p.writeJSON(resp.Msg)
			}
			return p.writeLine("review recorded against %s: root cause %s, %s",
				id, rootCause, fmtCount(len(amendments), "amendment", "amendments"))
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&rootCause, "root-cause", "",
		"the validated root cause: a change reference, `unobserved`, or `not_change_induced:<category>`")
	flags.StringArrayVar(&amend, "amend", nil,
		"amend a hypothesis, as <hypothesis-id>=<status>; repeatable")
	flags.StringVar(&reason, "reason", "", "why (required)")
	return cmd
}

// parseAmendments parses `--amend <hyp>=<status>`. The status must be one the ledger publishes:
// a review may correct the engine, but it corrects it into the same vocabulary, so that a
// corrected ledger is still a ledger.
func parseAmendments(values []string) ([]*investigationv1.Hypothesis, error) {
	out := make([]*investigationv1.Hypothesis, 0, len(values))
	for _, raw := range values {
		id, status, found := strings.Cut(strings.TrimSpace(raw), "=")
		if !found || id == "" || status == "" {
			return nil, exitErrorf(ExitUsage,
				"--amend %q: write it as <hypothesis-id>=<status>", raw)
		}
		enum, ok := hypothesisStatusValue(status)
		if !ok {
			return nil, exitErrorf(ExitUsage,
				"--amend %q: status %q is not one the ledger publishes (proposed, supported, "+
					"refuted, inconclusive, untested, exonerated)", raw, status)
		}
		out = append(out, &investigationv1.Hypothesis{HypothesisId: id, Status: enum})
	}
	return out, nil
}

func hypothesisStatusValue(name string) (investigationv1.HypothesisStatus, bool) {
	upper := strings.ToUpper(strings.TrimSpace(name))
	if v, ok := investigationv1.HypothesisStatus_value[upper]; ok {
		return investigationv1.HypothesisStatus(v), true
	}
	if v, ok := investigationv1.HypothesisStatus_value["HYPOTHESIS_STATUS_"+upper]; ok {
		return investigationv1.HypothesisStatus(v), true
	}
	return investigationv1.HypothesisStatus_HYPOTHESIS_STATUS_UNSPECIFIED, false
}

func newInvestigateLabelCommand(global *globalOptions) *cobra.Command {
	var (
		right bool
		wrong bool
	)
	cmd := &cobra.Command{
		Use:   "label <id> --right|--wrong",
		Short: `The one-click "was this right?"`,
		Long: "label records a single-question verdict on an investigation, with its author and\n" +
			"time. Additive like every other human decision, and part of the evaluation corpus\n" +
			"(FR-057e, FR-056).",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := investigationIDArg(cmd, args)
			if err != nil {
				return err
			}
			// Neither flag and both flags are the same mistake: the label is a single
			// question, and "unspecified" is not one of its answers.
			if right == wrong {
				return exitErrorf(ExitUsage,
					"investigate label: pass exactly one of --right or --wrong")
			}
			client, err := newInvestigationClient(global)
			if err != nil {
				return err
			}
			resp, err := client.Label(cmd.Context(), connect.NewRequest(&investigationv1.LabelRequest{
				InvestigationId: id, WasThisRight: right,
			}))
			if err != nil {
				return remoteError("investigate label", err)
			}
			p := newPrinter(cmd.OutOrStdout(), global.Output)
			if p.json() {
				return p.writeJSON(resp.Msg)
			}
			verdict := "wrong"
			if right {
				verdict = "right"
			}
			return p.writeLine("labelled %s as %s", id, verdict)
		},
	}
	cmd.Flags().BoolVar(&right, "right", false, "the investigation was right")
	cmd.Flags().BoolVar(&wrong, "wrong", false, "the investigation was wrong")
	return cmd
}

func newInvestigateReportCommand(global *globalOptions) *cobra.Command {
	var (
		deliver bool
		dsn     string
	)
	sinkID := render.SinkLog
	cmd := &cobra.Command{
		Use:   "report <id> [--deliver]",
		Short: "Render the report for the place the incident lives",
		Long: "report renders the investigation for the place the incident was declared, in the\n" +
			"published order and with the deep links.\n\n" +
			"--deliver returns it to that place, updated **in place** rather than re-posted as a\n" +
			"stream of messages. Delivery is report-only, non-blocking, and never a precondition\n" +
			"of the investigation concluding: a failure is recorded and printed, and the command\n" +
			"still exits 0, because the investigation is complete whatever the chat connector did\n" +
			"(FR-057f, constitution VII).\n\n" +
			"The transport in this version is a recorded no-op driver: the report is rendered and\n" +
			"logged and nothing leaves the process. A real transport is a connector's job.",
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
				return remoteError("investigate report", err)
			}
			body, err := renderReportBody(resp.Msg)
			if err != nil {
				return exitWith(ExitVerification, err)
			}
			p := newPrinter(cmd.OutOrStdout(), global.Output)
			if p.json() {
				if err := p.writeJSON(resp.Msg); err != nil {
					return err
				}
			} else if err := p.writeRaw(body); err != nil {
				return err
			}
			if !deliver {
				return nil
			}
			result := deliverReport(cmd, resp.Msg, body, sinkID)
			// FR-057f's other half: "a delivery that fails is recorded and the investigation
			// still concludes". Until now nothing on this path wrote the ledger row, so
			// `report_deliveries` only ever moved in tests and an operator asking "did the
			// on-call ever see this, and how many times has it been edited?" had nowhere to
			// look. Recording it is non-blocking in the same sense the delivery is: a ledger
			// that cannot be written is said so and the command still exits 0.
			ledgerNote := recordDeliveryOutcome(cmd, dsn, resp.Msg, result)
			if p.json() {
				return nil
			}
			if ledgerNote != "" {
				if err := p.writeLine("\n%s", ledgerNote); err != nil {
					return err
				}
			}
			switch result.Outcome {
			case render.OutcomeDelivered:
				return p.writeLine("\ndelivered to %s (edited in place as %s)",
					deliveryTarget(resp.Msg), result.ExternalMessageRef)
			case render.OutcomeNotConfigured:
				return p.writeLine("\nnot delivered: %s", result.FailureDetail)
			default:
				// A failed delivery is reported and the command still exits 0: the investigation
				// concluded, and a chat outage is not a verification failure of the engine.
				return p.writeLine("\ndelivery failed and was recorded: %s", result.FailureDetail)
			}
		},
	}
	cmd.Flags().BoolVar(&deliver, "deliver", false,
		"return the report to the place the incident lives, updated in place")
	cmd.Flags().StringVar(&sinkID, "sink", render.SinkLog,
		"the report-delivery transport ("+strings.Join(render.PublishedSinks(), ", ")+
			"). An unpublished one is refused, never defaulted")
	cmd.Flags().StringVar(&dsn, "db", "",
		"PostgreSQL DSN the delivery ledger is written to (default $"+EnvDSN+"). With neither, "+
			"the report is delivered and the attempt is not recorded")
	return cmd
}

// recordDeliveryOutcome writes the delivery ledger row for an attempt (FR-057f).
//
// One row per (investigation, target): a report is **edited in place**, so a second delivery to
// the same place is an update with `update_count + 1` and the same external reference, never a
// second row. The canonical id is store.DeliveryIDFor, which is what makes that representable.
//
// It returns a note for the operator and never an error. Every failure mode here — no database
// configured, a database that will not open, a row that will not write — is a failure to record
// something that already happened, and failing the command over it would make the ledger a
// precondition of delivering a report, which is the thing FR-057f forbids the delivery itself
// from being.
func recordDeliveryOutcome(
	cmd *cobra.Command, flagDSN string, inv *investigationv1.Investigation,
	result render.DeliveryResult,
) string {
	value := strings.TrimSpace(flagDSN)
	if value == "" {
		value = strings.TrimSpace(os.Getenv(EnvDSN))
	}
	if value == "" {
		return "the attempt was not recorded: no delivery ledger is reachable (pass --db or set $" +
			EnvDSN + ")"
	}
	store, err := postgres.Open(cmd.Context(), value)
	if err != nil {
		return "the attempt was not recorded: " + err.Error()
	}
	defer store.Close()

	system, ref := deliverySplit(inv)
	outcome, detail := deliveryLedgerOutcome(result)
	rec := investigationstore.DeliveryRecord{
		DeliveryID: investigationstore.DeliveryIDFor(
			inv.GetInvestigationId(), system, ref),
		InvestigationID:    inv.GetInvestigationId(),
		TargetSystem:       system,
		TargetRef:          ref,
		ExternalMessageRef: result.ExternalMessageRef,
		RenderingDigest:    result.RenderingDigest,
		Outcome:            outcome,
		FailureDetail:      detail,
		At:                 result.At,
	}
	row, err := investigationstore.NewInvestigationDAO(store).RecordDelivery(cmd.Context(), rec)
	if err != nil {
		return "the attempt was not recorded: " + err.Error()
	}
	return fmt.Sprintf("recorded in the delivery ledger as %s (%s, update %d)",
		rec.DeliveryID, row.GetOutcome(), row.GetUpdateCount())
}

// deliveryLedgerOutcome maps the transport's outcome onto the three the ledger accepts, and
// supplies the detail a failure must carry: the store refuses a failure that does not say why,
// and "the connector returned nothing" is still why.
func deliveryLedgerOutcome(result render.DeliveryResult) (outcome, detail string) {
	switch result.Outcome {
	case render.OutcomeDelivered:
		return investigationstore.DeliveryDelivered, ""
	case render.OutcomeNotConfigured:
		return investigationstore.DeliveryNotConfigured, result.FailureDetail
	default:
		detail = result.FailureDetail
		if strings.TrimSpace(detail) == "" {
			detail = "the transport reported a failure and no detail"
		}
		return investigationstore.DeliveryFailed, detail
	}
}

// deliverReport runs the delivery through the v1 sink. It returns a result and never an error,
// which is the type-level half of "delivery never blocks" (FR-057f).
func deliverReport(cmd *cobra.Command, inv *investigationv1.Investigation, body, sinkID string) render.DeliveryResult {
	system, ref := deliverySplit(inv)
	// The transport is resolved before the credential is read, so that a typo in --sink is
	// refused rather than quietly served by whichever sink is compiled in (T116).
	sink, err := render.NewSink(sinkID, nil)
	if err != nil {
		return render.DeliveryResult{
			Outcome:       render.OutcomeNotConfigured,
			FailureDetail: err.Error(),
		}
	}
	secret := os.Getenv(EnvDeliveryToken)
	if secret == "" {
		// No delivery credential is the ordinary state of a deployment with no chat connector.
		// It is `not_configured`, which is not a failure of anything, and it is deliberately not
		// satisfied by falling back to $SRE_AGENT_TOKEN: the two credentials are separate
		// (constitution VII, FR-057f).
		return render.DeliveryResult{
			Outcome: render.OutcomeNotConfigured,
			FailureDetail: "no report-delivery credential: set $" + EnvDeliveryToken +
				". It is scoped to writing that one report and is separate from $" + EnvToken,
		}
	}
	cred, credErr := render.NewCredential(system, render.DeliveryScope, secret)
	if credErr != nil {
		return render.DeliveryResult{
			Outcome:       render.OutcomeNotConfigured,
			FailureDetail: credErr.Error(),
		}
	}
	var previous string
	for _, d := range inv.GetDeliveries() {
		if d.GetTargetSystem() == system && d.GetTargetRef() == ref {
			previous = d.GetExternalMessageRef()
		}
	}
	return render.Deliver(cmd.Context(), sink, cred, render.DeliveryRequest{
		InvestigationID:    inv.GetInvestigationId(),
		TargetSystem:       system,
		TargetRef:          ref,
		Rendering:          body,
		PreviousMessageRef: previous,
	})
}

// EnvDeliveryToken is the environment variable the report-delivery credential is read from. It is
// deliberately a different variable from $SRE_AGENT_TOKEN: the delivery credential grants writing
// one report and nothing else, and reusing the read token would merge the two the constitution
// requires to be separate (VII, FR-057f).
const EnvDeliveryToken = "SRE_AGENT_REPORT_TOKEN" //nolint:gosec // the name of a variable, not a credential

// deliverySplit finds the place the incident lives: the origin of the declared symptom.
func deliverySplit(inv *investigationv1.Investigation) (system, ref string) {
	for _, s := range inv.GetSymptoms() {
		if s.GetOrigin() == investigationv1.IntakeOrigin_INTAKE_ORIGIN_DECLARED {
			return s.GetOriginSystem(), s.GetOriginRef()
		}
	}
	return "", ""
}

func deliveryTarget(inv *investigationv1.Investigation) string {
	system, ref := deliverySplit(inv)
	if system == "" {
		return unknownCell
	}
	return system + ":" + ref
}

func newInvestigateToIncidentCommand(global *globalOptions) *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use:   "to-incident <id> --out fixtures/incidents/<id>",
		Short: "Turn a reviewed investigation into a corpus incident",
		Long: "to-incident writes the incident-format directory the evaluation harness accepts,\n" +
			"with no hand-editing (FR-055, SC-012).\n\n" +
			"It refuses an investigation with no review: the ground truth of a corpus incident is\n" +
			"the human's validated root cause, and a corpus that learned from the engine's own\n" +
			"answer would be measuring the engine against itself.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := investigationIDArg(cmd, args)
			if err != nil {
				return err
			}
			if strings.TrimSpace(out) == "" {
				return exitErrorf(ExitUsage, "investigate to-incident: --out <dir> is required")
			}
			client, err := newInvestigationClient(global)
			if err != nil {
				return err
			}
			resp, err := client.Get(cmd.Context(),
				connect.NewRequest(&investigationv1.GetRequest{InvestigationId: id}))
			if err != nil {
				return remoteError("investigate to-incident", err)
			}
			inv := resp.Msg
			if len(inv.GetReviews()) == 0 {
				return exitErrorf(ExitUsage,
					"investigate to-incident %s: no review recorded. A corpus incident's ground "+
						"truth is a human's validated root cause; record one with "+
						"`aisre investigate review %s --root-cause … --reason …` first (FR-055)", id, id)
			}
			dir, err := filepath.Abs(out)
			if err != nil {
				return exitErrorf(ExitUsage, "--out %q: %v", out, err)
			}
			if err := os.MkdirAll(dir, 0o750); err != nil {
				return exitErrorf(ExitUsage, "--out %q: %v", out, err)
			}
			// The export carries the replayable half (graph events, both recording layers); the
			// incident block carries the question and the validated answer. Writing the second
			// here and asking `investigate export` for the first is what keeps "no hand-editing"
			// true: the two halves are produced by commands, never by a person.
			exportResp, err := client.Export(cmd.Context(), connect.NewRequest(&investigationv1.ExportRequest{
				InvestigationId: id, OutDir: dir,
			}))
			if err != nil {
				return remoteError("investigate to-incident", err)
			}
			if err := writeJSONFile(filepath.Join(dir, "investigation.json"), inv); err != nil {
				return err
			}
			// The manifest is what turns the export into a *fixture*: without it `fixture verify`
			// does not recognise the directory at all, which is what FR-055's "accepted by the
			// evaluation harness with no hand-editing" fails on. Everything derivable is derived;
			// everything a reviewer has to decide is written as a marked placeholder
			// (investigate_incident_manifest.go).
			manifest, err := incidentManifestYAML(dir, inv)
			if err != nil {
				return err
			}
			manifestPath := filepath.Join(dir, fixture.ManifestFile)
			if err := os.WriteFile(manifestPath, []byte(manifest), 0o600); err != nil {
				return exitErrorf(ExitUsage, "write %s: %v", manifestPath, err)
			}
			placeholders, err := fixture.PlaceholderFields(dir)
			if err != nil {
				return err
			}
			p := newPrinter(cmd.OutOrStdout(), global.Output)
			if p.json() {
				return p.writeJSON(exportResp.Msg)
			}
			if err := p.writeLine(
				"wrote corpus incident %s to %s (root cause %s, %s)",
				id, dir, inv.GetReviews()[0].GetValidatedRootCause(),
				fmtCount(len(inv.GetLedger().GetEvidence()), "evidence item", "evidence items")); err != nil {
				return err
			}
			if len(placeholders) == 0 {
				return nil
			}
			return p.writeLine(
				"%s await a reviewer: %s. `aisre fixture verify %s` lists them; the fixture is "+
					"not corpus-ready until none are left (FR-055).",
				fmtCount(len(placeholders), "field", "fields"),
				strings.Join(placeholders, ", "), dir)
		},
	}
	cmd.Flags().StringVar(&out, "out", "", "directory to write the corpus incident into (required)")
	return cmd
}
