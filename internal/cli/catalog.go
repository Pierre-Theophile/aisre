// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"os"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"
	"gopkg.in/yaml.v3"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/catalog"
	notionfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/notion"
	"github.com/Pierre-Theophile/aisre/internal/graph"
)

// `catalog …` (feature 009, contracts/catalog.md §4, FR-005, FR-010–FR-013).
//
// The service catalog is this agent's own list of what the organisation runs. `list` and `show`
// are reads. Every other command is a human decision: it needs the `decider` role, it is signed by
// the token's principal, and it prints the event it appended and that principal (FR-012). A
// decision outranks every automated rule from then on.
//
// A decision with no `--reason` is recorded with one naming the command and the instant, so that
// undoing a decision and making it again later are two decisions, not one replayed.

func newCatalogCommand(global *globalOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "catalog",
		Short: "Read and curate the service catalog",
		Long: "catalog is the agent's own list of the services and platforms the organisation runs, and\n" +
			"the names each goes by in every tool (aliases). Signals are attributed to its entries.\n\n" +
			"`list` and `show` are reads; every other command is a decision that needs the `decider`\n" +
			"role and records who made it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return exitWith(ExitUsage, cmd.Help())
		},
	}
	cmd.AddCommand(
		newCatalogListCommand(global),
		newCatalogShowCommand(global),
		newCatalogProposalsCommand(global),
		newCatalogDeclareCommand(global),
		newCatalogConfirmCommand(global),
		newCatalogRenameCommand(global),
		newCatalogHideCommand(global, true),
		newCatalogHideCommand(global, false),
		newCatalogMergeCommand(global),
		newCatalogSplitCommand(global),
		newCatalogAliasCommand(global),
		newCatalogImportCommand(global),
	)
	return cmd
}

// decisionReason is --reason, or one naming the command and the instant.
func decisionReason(reason, command string) string {
	if s := strings.TrimSpace(reason); s != "" {
		return s
	}
	return fmt.Sprintf("aisre %s at %s", command, time.Now().UTC().Format(time.RFC3339Nano))
}

// catalogAsOf is the as-of of a catalog read: --as-of, or now.
func catalogAsOf(raw string) (*graphv1.AsOf, error) {
	at, err := parseInstant("--as-of", raw)
	if err != nil {
		return nil, err
	}
	if at.IsZero() {
		return &graphv1.AsOf{ValidAt: timestamppb.Now()}, nil
	}
	return &graphv1.AsOf{ValidAt: timestamppb.New(at), ObservedAt: timestamppb.New(at)}, nil
}

func checkSlug(slug string) error {
	if !graph.ValidSlug(slug) {
		return exitErrorf(ExitUsage, "%q is not a slug: lower-case letters, digits and dashes (candidate-view)", slug)
	}
	return nil
}

func parseName(raw string) (graph.Ref, error) {
	ref, err := graph.ParseRef(strings.TrimSpace(raw))
	if err != nil {
		return graph.Ref{}, exitErrorf(ExitUsage, "%q: write a name as <namespace>=<value> (datadog.log_service=candidate-api)", raw)
	}
	return ref, nil
}

func aliasLabel(a *graphv1.CatalogAlias) string {
	return catalog.AliasKeyOf(graph.RefFromProto(a.GetName()), a.GetEnvironment())
}

func aliasDecider(a *graphv1.CatalogAlias) string {
	if a.GetPrincipal() != "" {
		return a.GetPrincipal()
	}
	return a.GetRuleId()
}

func newCatalogListCommand(global *globalOptions) *cobra.Command {
	var status, asOf string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the catalog's entries and their aliases",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			at, err := catalogAsOf(asOf)
			if err != nil {
				return err
			}
			clients, err := newClients(global)
			if err != nil {
				return err
			}
			resp, err := clients.catalog.ListEntries(cmd.Context(), connect.NewRequest(&graphv1.ListEntriesRequest{
				AsOf: at, Status: status,
			}))
			if err != nil {
				return remoteError("catalog list", err)
			}
			p := newPrinter(cmd.OutOrStdout(), global.Output)
			if p.json() {
				return p.writeJSON(resp.Msg)
			}
			rows := make([][]string, 0, len(resp.Msg.GetEntries()))
			for _, e := range resp.Msg.GetEntries() {
				names := make([]string, 0, len(e.GetAliases()))
				for _, a := range e.GetAliases() {
					names = append(names, aliasLabel(a))
				}
				rows = append(rows, []string{e.GetSlug(), e.GetNode().GetDisplayName(), e.GetKind(),
					e.GetStatus(), e.GetOwner(), strings.Join(names, ", ")})
			}
			for _, a := range resp.Msg.GetHidden() {
				rows = append(rows, []string{"-", "(not a service)", "", "hidden", "", aliasLabel(a)})
			}
			return p.writeTable([]string{"SLUG", "NAME", "KIND", "STATUS", "OWNER", "ALIASES"}, rows)
		},
	}
	cmd.Flags().StringVar(&status, "status", "", "only entries with this status: proposed, confirmed or hidden")
	cmd.Flags().StringVar(&asOf, "as-of", "", "the catalog as it was then (RFC 3339, -30m or now)")
	return cmd
}

func newCatalogProposalsCommand(global *globalOptions) *cobra.Command {
	var kind, status string
	cmd := &cobra.Command{
		Use:   "proposals",
		Short: "List the entries and aliases discovery proposed, waiting on a person",
		Long: "proposals lists the entries discovery proposed and the aliases it suggests, with the rule,\n" +
			"the score and why. Accept an alias with `catalog alias accept`, reject it with\n" +
			"`catalog alias reject`; confirm or hide an entry with `catalog confirm` or `catalog hide`.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			clients, err := newClients(global)
			if err != nil {
				return err
			}
			resp, err := clients.catalog.ListProposals(cmd.Context(), connect.NewRequest(&graphv1.ListProposalsRequest{
				Kind: kind, Status: status,
			}))
			if err != nil {
				return remoteError("catalog proposals", err)
			}
			p := newPrinter(cmd.OutOrStdout(), global.Output)
			if p.json() {
				return p.writeJSON(resp.Msg)
			}
			rows := make([][]string, 0, len(resp.Msg.GetEntries())+len(resp.Msg.GetAliases()))
			for _, e := range resp.Msg.GetEntries() {
				names := make([]string, 0, len(e.GetAliases()))
				for _, a := range e.GetAliases() {
					names = append(names, aliasLabel(a))
				}
				rows = append(rows, []string{"entry", e.GetSlug(), strings.Join(names, ", "), "D1", "", "proposed",
					e.GetNode().GetDisplayName()})
			}
			for _, a := range resp.Msg.GetAliases() {
				name := graph.RefFromProto(a.GetName())
				rows = append(rows, []string{"alias", a.GetEntrySlug(), catalog.AliasKeyOf(name, a.GetEnvironment()),
					a.GetRuleId(), fmt.Sprintf("%.2f", a.GetScore()), a.GetStatus(), a.GetRationale()})
			}
			return p.writeTable([]string{"KIND", "ENTRY", "NAMES", "RULE", "SCORE", "STATUS", "WHY"}, rows)
		},
	}
	cmd.Flags().StringVar(&kind, "kind", "", "only entry or alias proposals")
	cmd.Flags().StringVar(&status, "status", "", "alias proposals with this status: pending (default), accepted, rejected, conflict or all")
	return cmd
}

func newCatalogShowCommand(global *globalOptions) *cobra.Command {
	var asOf string
	cmd := &cobra.Command{
		Use:   "show <slug>",
		Short: "Show one entry, its aliases and who decided each",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			at, err := catalogAsOf(asOf)
			if err != nil {
				return err
			}
			clients, err := newClients(global)
			if err != nil {
				return err
			}
			resp, err := clients.catalog.GetEntry(cmd.Context(), connect.NewRequest(&graphv1.GetEntryRequest{
				Slug: args[0], AsOf: at,
			}))
			if err != nil {
				return remoteError("catalog show", err)
			}
			p := newPrinter(cmd.OutOrStdout(), global.Output)
			if p.json() {
				return p.writeJSON(resp.Msg)
			}
			e := resp.Msg.GetEntry()
			var b strings.Builder
			fmt.Fprintf(&b, "%s — %s (%s, %s)\n", e.GetSlug(), e.GetNode().GetDisplayName(), e.GetKind(), e.GetStatus())
			if e.GetOwner() != "" {
				fmt.Fprintf(&b, "owner: %s\n", e.GetOwner())
			}
			if e.GetDescription() != "" {
				fmt.Fprintf(&b, "%s\n", e.GetDescription())
			}
			if len(e.GetAliases()) == 0 {
				b.WriteString("no aliases\n")
			}
			for _, a := range e.GetAliases() {
				fmt.Fprintf(&b, "  %s  decided by %s (%s, %s)\n", aliasLabel(a), aliasDecider(a),
					a.GetDecidedByEventId(), formatTime(a.GetObservedFrom()))
			}
			return p.writeRaw(b.String())
		},
	}
	cmd.Flags().StringVar(&asOf, "as-of", "", "the entry as it was then (RFC 3339, -30m or now)")
	return cmd
}

// entryFlags are the fields an entry decision states.
type entryFlags struct {
	name, kind, owner, description, reason string
}

func newCatalogDeclareCommand(global *globalOptions) *cobra.Command {
	var f entryFlags
	cmd := &cobra.Command{
		Use:   "declare <slug>",
		Short: "Declare an entry, or restate one",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkSlug(args[0]); err != nil {
				return err
			}
			if strings.TrimSpace(f.name) == "" {
				return exitErrorf(ExitUsage, "--name is required: an entry is called something")
			}
			msg := &graphv1.DeclareCatalogEntry{
				Slug: args[0], DisplayName: f.name, Kind: f.kind, Status: graph.CatalogStatusConfirmed,
				Owner: f.owner, Description: f.description, Rationale: decisionReason(f.reason, "catalog declare "+args[0]),
			}
			return declareEntry(cmd, global, "entry "+args[0], msg)
		},
	}
	cmd.Flags().StringVar(&f.name, "name", "", "the entry's display name, e.g. \"Candidate View\" (required)")
	cmd.Flags().StringVar(&f.kind, "kind", graph.CatalogKindService, "service or platform")
	cmd.Flags().StringVar(&f.owner, "owner", "", "who owns it")
	cmd.Flags().StringVar(&f.description, "description", "", "one line on what it is")
	cmd.Flags().StringVar(&f.reason, "reason", "", "why (default: the command and the instant)")
	return cmd
}

func declareEntry(cmd *cobra.Command, global *globalOptions, what string, msg *graphv1.DeclareCatalogEntry) error {
	clients, err := newClients(global)
	if err != nil {
		return err
	}
	resp, err := clients.resolution.DeclareCatalogEntry(cmd.Context(), connect.NewRequest(msg))
	if err != nil {
		return remoteError("catalog", err)
	}
	return renderDecision(cmd, global, what, resp.Msg)
}

// restate reads an entry as it is now and declares it again with one change.
func restate(cmd *cobra.Command, global *globalOptions, slug, what, reason string, change func(*graphv1.DeclareCatalogEntry)) error {
	clients, err := newClients(global)
	if err != nil {
		return err
	}
	resp, err := clients.catalog.GetEntry(cmd.Context(), connect.NewRequest(&graphv1.GetEntryRequest{
		Slug: slug, AsOf: &graphv1.AsOf{ValidAt: timestamppb.Now()},
	}))
	if err != nil {
		return remoteError("catalog", err)
	}
	e := resp.Msg.GetEntry()
	msg := &graphv1.DeclareCatalogEntry{
		Slug: slug, DisplayName: e.GetNode().GetDisplayName(), Kind: e.GetKind(), Status: e.GetStatus(),
		Owner: e.GetOwner(), Description: e.GetDescription(), Rationale: reason,
	}
	change(msg)
	return declareEntry(cmd, global, what, msg)
}

func newCatalogConfirmCommand(global *globalOptions) *cobra.Command {
	var f entryFlags
	cmd := &cobra.Command{
		Use:   "confirm <slug>",
		Short: "Confirm a proposed entry, optionally renaming it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return restate(cmd, global, args[0], "confirmation of "+args[0],
				decisionReason(f.reason, "catalog confirm "+args[0]), func(m *graphv1.DeclareCatalogEntry) {
					m.Status = graph.CatalogStatusConfirmed
					if f.name != "" {
						m.DisplayName = f.name
					}
					if f.kind != "" {
						m.Kind = f.kind
					}
				})
		},
	}
	cmd.Flags().StringVar(&f.name, "name", "", "the display name to confirm it under")
	cmd.Flags().StringVar(&f.kind, "kind", "", "service or platform")
	cmd.Flags().StringVar(&f.reason, "reason", "", "why (default: the command and the instant)")
	return cmd
}

func newCatalogRenameCommand(global *globalOptions) *cobra.Command {
	var f entryFlags
	cmd := &cobra.Command{
		Use:   "rename <slug>",
		Short: "Change an entry's display name; its slug and aliases stay",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(f.name) == "" {
				return exitErrorf(ExitUsage, "--name is required")
			}
			return restate(cmd, global, args[0], "rename of "+args[0],
				decisionReason(f.reason, "catalog rename "+args[0]),
				func(m *graphv1.DeclareCatalogEntry) { m.DisplayName = f.name })
		},
	}
	cmd.Flags().StringVar(&f.name, "name", "", "the new display name (required)")
	cmd.Flags().StringVar(&f.reason, "reason", "", "why (default: the command and the instant)")
	return cmd
}

// newCatalogHideCommand is `hide` and `restore`: of an entry, or of a name ("not a service").
func newCatalogHideCommand(global *globalOptions, hide bool) *cobra.Command {
	var name, env, reason string
	use, short := "hide", "Hide an entry, or say a name is not a service"
	if !hide {
		use, short = "restore", "Restore a hidden entry or name"
	}
	cmd := &cobra.Command{
		Use:   use + " [<slug>] [--name <ns>=<value>]",
		Short: short,
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if (len(args) == 1) == (name != "") {
				return exitErrorf(ExitUsage, "%s takes a slug or --name, exactly one", use)
			}
			if len(args) == 1 {
				status := graph.CatalogStatusHidden
				if !hide {
					status = graph.CatalogStatusConfirmed
				}
				return restate(cmd, global, args[0], use+" of "+args[0],
					decisionReason(reason, "catalog "+use+" "+args[0]),
					func(m *graphv1.DeclareCatalogEntry) { m.Status = status })
			}
			ref, err := parseName(name)
			if err != nil {
				return err
			}
			clients, err := newClients(global)
			if err != nil {
				return err
			}
			rationale := decisionReason(reason, "catalog "+use+" --name "+ref.String())
			var resp *connect.Response[graphv1.IngestResult]
			if hide {
				resp, err = clients.resolution.DeclareAlias(cmd.Context(), connect.NewRequest(&graphv1.DeclareAlias{
					Name: ref.Proto(), Environment: env, EntrySlug: graph.HiddenEntrySlug, Rationale: rationale,
				}))
			} else {
				resp, err = clients.resolution.RetractAlias(cmd.Context(), connect.NewRequest(&graphv1.RetractAlias{
					Name: ref.Proto(), Environment: env, EntrySlug: graph.HiddenEntrySlug, Rationale: rationale,
				}))
			}
			if err != nil {
				return remoteError("catalog "+use, err)
			}
			return renderDecision(cmd, global, use+" of "+catalog.AliasKeyOf(ref, env), resp.Msg)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "a name instead of an entry: <namespace>=<value>")
	cmd.Flags().StringVar(&env, "env", "", "the environment the name is stated in, if only one")
	cmd.Flags().StringVar(&reason, "reason", "", "why (default: the command and the instant)")
	return cmd
}

func newCatalogMergeCommand(global *globalOptions) *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use:   "merge <slug-a> <slug-b>",
		Short: "Say two entries are one",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			for _, slug := range args {
				if err := checkSlug(slug); err != nil {
					return err
				}
			}
			a, b := graph.CatalogRef(args[0]).String(), graph.CatalogRef(args[1]).String()
			clients, err := newClients(global)
			if err != nil {
				return err
			}
			resp, err := clients.resolution.Merge(cmd.Context(), connect.NewRequest(&graphv1.ManualMerge{
				EntityA: a, EntityB: b, Rationale: decisionReason(reason, "catalog merge "+args[0]+" "+args[1]),
			}))
			if err != nil {
				return remoteError("catalog merge", err)
			}
			return renderDecision(cmd, global, "merge of "+pairLabel(a, b), resp.Msg)
		},
	}
	cmd.Flags().StringVar(&reason, "reason", "", "why (default: the command and the instant)")
	return cmd
}

func newCatalogSplitCommand(global *globalOptions) *cobra.Command {
	var detach []string
	var reason string
	cmd := &cobra.Command{
		Use:   "split <slug> --detach <ns>=<value>...",
		Short: "Undo a merge of entries",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkSlug(args[0]); err != nil {
				return err
			}
			refs, err := parseDetachRefs(detach)
			if err != nil {
				return err
			}
			clients, err := newClients(global)
			if err != nil {
				return err
			}
			entity := graph.CatalogRef(args[0]).String()
			resp, err := clients.resolution.Split(cmd.Context(), connect.NewRequest(&graphv1.SplitEntity{
				EntityId: entity, DetachClaims: refs, Rationale: decisionReason(reason, "catalog split "+args[0]),
			}))
			if err != nil {
				return remoteError("catalog split", err)
			}
			return renderDecision(cmd, global, "split of "+entity, resp.Msg)
		},
	}
	cmd.Flags().StringSliceVar(&detach, "detach", nil, "the slugs to detach, as sre.catalog=<slug> (required)")
	cmd.Flags().StringVar(&reason, "reason", "", "why (default: the command and the instant)")
	return cmd
}

func newCatalogAliasCommand(global *globalOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "alias",
		Short: "Say which entry a name denotes",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return exitWith(ExitUsage, cmd.Help())
		},
	}
	for _, use := range []string{"add", "remove", "accept", "reject"} {
		var env, reason string
		short := map[string]string{
			"add":    "Say a name denotes an entry, in every environment or one",
			"remove": "End an alias from now on; its history stays",
			"accept": "Accept a proposed alias (catalog proposals lists them)",
			"reject": "Say a name is not this entry; it is never proposed again",
		}[use]
		sub := &cobra.Command{
			Use:   use + " <ns>=<value> <slug>",
			Short: short,
			Args:  cobra.ExactArgs(2),
			RunE: func(cmd *cobra.Command, args []string) error {
				ref, err := parseName(args[0])
				if err != nil {
					return err
				}
				if err := checkSlug(args[1]); err != nil {
					return err
				}
				clients, err := newClients(global)
				if err != nil {
					return err
				}
				rationale := decisionReason(reason, "catalog alias "+use+" "+ref.String()+" "+args[1])
				var resp *connect.Response[graphv1.IngestResult]
				what := "alias " + catalog.AliasKeyOf(ref, env) + " → " + args[1]
				switch use {
				case "add", "accept":
					// Accepting a proposal is declaring the alias it proposed; the projector marks the
					// proposal accepted.
					resp, err = clients.resolution.DeclareAlias(cmd.Context(), connect.NewRequest(&graphv1.DeclareAlias{
						Name: ref.Proto(), Environment: env, EntrySlug: args[1], Rationale: rationale,
					}))
				case "remove":
					resp, err = clients.resolution.RetractAlias(cmd.Context(), connect.NewRequest(&graphv1.RetractAlias{
						Name: ref.Proto(), Environment: env, EntrySlug: args[1], Rationale: rationale,
					}))
				case "reject":
					what = "rejection of " + what
					resp, err = clients.resolution.RejectAlias(cmd.Context(), connect.NewRequest(&graphv1.RejectAlias{
						Name: ref.Proto(), Environment: env, EntrySlug: args[1], Rationale: rationale,
					}))
				}
				if err != nil {
					return remoteError("catalog alias "+use, err)
				}
				return renderDecision(cmd, global, what, resp.Msg)
			},
		}
		sub.Flags().StringVar(&env, "env", "", "only in this environment (default: every environment)")
		sub.Flags().StringVar(&reason, "reason", "", "why (default: the command and the instant)")
		cmd.AddCommand(sub)
	}
	return cmd
}

func newCatalogImportCommand(global *globalOptions) *cobra.Command {
	var prune, dryRun bool
	var reason, notionConfig string
	var slackFlags []string
	cmd := &cobra.Command{
		Use:   "import [<catalog.yaml>]",
		Short: "Bring the catalog in line with a catalog file, or with a connector's old service map",
		Long: "import computes the difference between the file and the catalog as it is now and records\n" +
			"the decisions that close it, signed by you. An unchanged file records nothing.\n\n" +
			"It never undoes a person: a name someone has since given to another entry is listed as a\n" +
			"conflict and left. Aliases the catalog has and the file does not are listed, and retracted\n" +
			"only with --prune.\n\n" +
			"Instead of a file, --from-notion-config reads the `entities` maps of a Notion connector\n" +
			"configuration, and --from-slack-flags takes the Slack connector's `--service` flags. Each\n" +
			"line of the old map becomes an alias: the connector's name and the entity it was mapped to\n" +
			"are aliases of one entry, named after the connector's spelling. Run it with --dry-run first.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sources := 0
			for _, given := range []bool{len(args) == 1, notionConfig != "", len(slackFlags) > 0} {
				if given {
					sources++
				}
			}
			if sources != 1 {
				return exitErrorf(ExitUsage, "catalog import: give one of a catalog file, --from-notion-config or --from-slack-flags")
			}
			file, source, err := importSource(args, notionConfig, slackFlags)
			if err != nil {
				return exitErrorf(ExitUsage, "%v", err)
			}
			args = []string{source}
			clients, err := newClients(global)
			if err != nil {
				return err
			}
			current, err := clients.catalog.ListEntries(cmd.Context(), connect.NewRequest(&graphv1.ListEntriesRequest{
				AsOf: &graphv1.AsOf{ValidAt: timestamppb.Now()},
			}))
			if err != nil {
				return remoteError("catalog import", err)
			}
			plan := catalog.PlanImport(file, current.Msg, decisionReason(reason, "catalog import "+args[0]), prune)
			p := newPrinter(cmd.OutOrStdout(), global.Output)
			for _, c := range plan.Conflicts {
				if err := p.writeLine("conflict, left as it is: %s", c); err != nil {
					return err
				}
			}
			if !prune {
				for _, e := range plan.Extra {
					if err := p.writeLine("not in the file, kept (--prune retracts it): %s", e); err != nil {
						return err
					}
				}
			}
			if len(plan.Changes) == 0 {
				return p.writeLine("the catalog already matches %s; nothing recorded", args[0])
			}
			for _, c := range plan.Changes {
				if dryRun {
					if err := p.writeLine("would record: %s", c.Describe); err != nil {
						return err
					}
					continue
				}
				var resp *connect.Response[graphv1.IngestResult]
				switch {
				case c.Entry != nil:
					resp, err = clients.resolution.DeclareCatalogEntry(cmd.Context(), connect.NewRequest(c.Entry))
				case c.Alias != nil:
					resp, err = clients.resolution.DeclareAlias(cmd.Context(), connect.NewRequest(c.Alias))
				default:
					resp, err = clients.resolution.RetractAlias(cmd.Context(), connect.NewRequest(c.Retract))
				}
				if err != nil {
					return remoteError("catalog import", err)
				}
				if err := renderDecision(cmd, global, c.Describe, resp.Msg); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&notionConfig, "from-notion-config", "",
		"import the `entities` maps of this Notion connector configuration (007), one alias per line")
	cmd.Flags().StringArrayVar(&slackFlags, "from-slack-flags", nil,
		"import a Slack `--service <token>=<ns>=<value>` mapping (006), one alias per flag; repeatable")
	cmd.Flags().BoolVar(&prune, "prune", false, "retract the aliases of the file's entries the file does not list")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the decisions without recording them")
	cmd.Flags().StringVar(&reason, "reason", "", "why (default: the command, the file and the instant)")
	return cmd
}

// importSource is the catalog file an import applies, from a file or from a connector's old map,
// and how the decisions name where it came from.
func importSource(args []string, notionConfig string, slackFlags []string) (catalog.File, string, error) {
	var lines []catalog.LegacyLine
	var source string
	switch {
	case len(args) == 1:
		raw, err := os.ReadFile(args[0])
		if err != nil {
			return catalog.File{}, "", fmt.Errorf("catalog import: %w", err)
		}
		file, err := catalog.ParseFile(raw)
		return file, args[0], err
	case notionConfig != "":
		cfg, err := notionfeeder.LoadConfig(notionConfig)
		if err != nil {
			return catalog.File{}, "", err
		}
		for _, b := range cfg.Databases {
			for value, spelled := range b.Entities {
				target, err := graph.ParseRef(spelled)
				if err != nil {
					return catalog.File{}, "", err
				}
				lines = append(lines, catalog.NotionLegacyLine(b.Database, value, target))
			}
		}
		source = "--from-notion-config " + notionConfig
	default:
		services, err := parseSlackServices(slackFlags)
		if err != nil {
			return catalog.File{}, "", err
		}
		for token, ref := range services {
			lines = append(lines, catalog.SlackLegacyLine(token, graph.RefFromProto(ref)))
		}
		source = "--from-slack-flags"
	}
	if len(lines) == 0 {
		return catalog.File{}, "", fmt.Errorf("catalog import: %s maps no name to an entity; there is nothing to import", source)
	}
	file, err := catalog.FileFromLegacy(lines)
	if err != nil {
		return catalog.File{}, "", err
	}
	// Through the file's own validation, so an import from a map is held to what a file is.
	raw, err := yaml.Marshal(file)
	if err != nil {
		return catalog.File{}, "", err
	}
	file, err = catalog.ParseFile(raw)
	return file, source, err
}
