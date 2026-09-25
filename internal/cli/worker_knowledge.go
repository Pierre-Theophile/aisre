// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/investigation/workers/knowledge"
	"github.com/Pierre-Theophile/aisre/internal/projector"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
)

// `knowledge link` (tasks.md T049, contracts/cli.md §Workers and backends, FR-049a, FR-052).
//
// It registers a durable document a human wrote as a `KNOWLEDGE_DOC` node with one `CONCERNS`
// edge per entity and a pointer to where the document lives — **never its content**.
//
// That restriction is the whole design. The graph is the retrieval index (constitution), so what
// it needs is the *link*: which entities this document is about, who says so, and where to go and
// read it. Copying the content in would make the graph a document store, would put prose the
// injection barrier cannot bound inside the thing the model reads, and would create a second copy
// that goes stale the moment somebody edits the original.
//
// What is ranked at retrieval time is the title plus the one-line summary the registering person
// wrote. That is deliberate too: it is what a colleague would say about the document, which is a
// better relevance signal for "is this worth opening" than its full text, and it is theirs rather
// than the document's.

type knowledgeLinkOptions struct {
	kind      string
	title     string
	summary   string
	location  string
	entities  []string
	asOf      string
	authored  string
	principal string
	dsn       string
	dryRun    bool
}

func newKnowledgeCommand(global *globalOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "knowledge",
		Short: "Register the documents graph-scoped retrieval searches",
		Long: "The v1 knowledge corpus is this engine's own past investigations plus the documents a human\n" +
			"registers here. Retrieval is scoped by the graph: only documents linked by a CONCERNS edge\n" +
			"to an entity in the investigation's subgraph are candidates, and a document linked to\n" +
			"nothing can never be retrieved (FR-049).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return exitWith(ExitUsage, cmd.Help())
		},
	}
	cmd.AddCommand(newKnowledgeLinkCommand(global))
	return cmd
}

func newKnowledgeLinkCommand(global *globalOptions) *cobra.Command {
	opts := &knowledgeLinkOptions{}

	cmd := &cobra.Command{
		Use:   "link <doc-ref>",
		Short: "Register a human-written document as a KNOWLEDGE_DOC with its CONCERNS edges",
		Long: "link registers <doc-ref> — a reference in <namespace>=<value> form, e.g.\n" +
			"`notion.page=9f2c` or `github.file=ops/runbooks/checkout.md` — as a KNOWLEDGE_DOC node\n" +
			"with one CONCERNS edge per --entity, plus a pointer to where it lives.\n\n" +
			"The document's content is never read and never stored. What retrieval ranks is the title\n" +
			"and the one-line --summary the registering person writes.\n\n" +
			"The registering identity, the instant and each link's provenance are recorded (FR-049a).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runKnowledgeLink(cmd, global, opts, args[0])
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&opts.kind, "kind", "",
		"postmortem | runbook | decision | investigation (required)")
	flags.StringVar(&opts.title, "title", "", "the document's title (required)")
	flags.StringVar(&opts.summary, "summary", "",
		"one line about what the document says — what retrieval ranks, and never the document's content")
	flags.StringVar(&opts.location, "location", "", "where the document lives, for a person to open")
	flags.StringArrayVar(&opts.entities, "entity", nil,
		"an entity the document concerns, as <namespace>=<value>; repeatable, at least one (required)")
	flags.StringVar(&opts.asOf, "as-of", "", "RFC 3339 instant the link becomes valid (default: now)")
	flags.StringVar(&opts.authored, "authored-at", "", "RFC 3339 instant the document was written")
	flags.StringVar(&opts.principal, "principal", "",
		"the authenticated identity registering the link (required; a citation whose provenance is anonymous is an appeal to authority)")
	flags.StringVar(&opts.dsn, "db", "", "PostgreSQL DSN (default $"+EnvDSN+")")
	flags.BoolVar(&opts.dryRun, "dry-run", false, "print the events that would be appended and append nothing")

	return cmd
}

func runKnowledgeLink(cmd *cobra.Command, global *globalOptions, opts *knowledgeLinkOptions, docRef string) error {
	ref, err := graph.ParseRef(docRef)
	if err != nil {
		return exitErrorf(ExitUsage, "document reference %q: %v", docRef, err)
	}
	request := knowledge.LinkRequest{
		DocumentRef:  ref.Proto(),
		Kind:         opts.kind,
		Title:        opts.title,
		Summary:      opts.summary,
		Location:     opts.location,
		RegisteredBy: opts.principal,
	}
	for _, entity := range opts.entities {
		entityRef, err := graph.ParseRef(entity)
		if err != nil {
			return exitErrorf(ExitUsage, "--entity %q: %v", entity, err)
		}
		request.EntityRefs = append(request.EntityRefs, entityRef.Proto())
	}
	if opts.asOf != "" {
		at, err := time.Parse(time.RFC3339, opts.asOf)
		if err != nil {
			return exitErrorf(ExitUsage, "--as-of %q: %v", opts.asOf, err)
		}
		request.AsOf = at
	}
	if opts.authored != "" {
		at, err := time.Parse(time.RFC3339, opts.authored)
		if err != nil {
			return exitErrorf(ExitUsage, "--authored-at %q: %v", opts.authored, err)
		}
		request.AuthoredAt = at
	}
	if err := request.Validate(); err != nil {
		return exitWith(ExitUsage, err)
	}

	const sourceID = "knowledge:human"
	events, err := request.Events(sourceID, time.Now().UTC())
	if err != nil {
		return exitWith(ExitUsage, err)
	}

	p := newPrinter(cmd.OutOrStdout(), global.Output)
	if opts.dryRun {
		return renderKnowledgeLink(p, request, len(events), true)
	}

	dsn, err := dsnFrom(opts.dsn, os.Getenv(EnvDSN))
	if err != nil {
		return err
	}
	store, err := postgres.Open(cmd.Context(), dsn)
	if err != nil {
		return storeError("open database", err)
	}
	defer store.Close()

	proj := projector.New(store)
	for _, event := range events {
		if _, err := proj.Apply(cmd.Context(), event, time.Now().UTC()); err != nil {
			return exitWith(ExitRejected, fmt.Errorf("knowledge link: append %s: %w", event.GetEventId(), err))
		}
	}
	return renderKnowledgeLink(p, request, len(events), false)
}

func renderKnowledgeLink(p *printer, request knowledge.LinkRequest, events int, dryRun bool) error {
	entities := make([]string, 0, len(request.EntityRefs))
	for _, ref := range request.EntityRefs {
		entities = append(entities, ref.GetNamespace()+"="+ref.GetValue())
	}
	if p.json() {
		return p.writeJSON(map[string]any{
			"document":       request.DocumentRef.GetNamespace() + "=" + request.DocumentRef.GetValue(),
			"kind":           request.Kind,
			"title":          request.Title,
			"entities":       entities,
			"registered_by":  request.RegisteredBy,
			"events":         events,
			"dry_run":        dryRun,
			"content_stored": false,
		})
	}
	verb := "registered"
	if dryRun {
		verb = "would register"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s %s=%s as a KNOWLEDGE_DOC\n", verb, request.Kind,
		request.DocumentRef.GetNamespace(), request.DocumentRef.GetValue())
	fmt.Fprintf(&b, "  title            %s\n", request.Title)
	fmt.Fprintf(&b, "  concerns         %s\n", strings.Join(entities, ", "))
	fmt.Fprintf(&b, "  registered by    %s\n", request.RegisteredBy)
	fmt.Fprintf(&b, "  events           %d (1 node, %d CONCERNS edges)\n", events, events-1)
	fmt.Fprintf(&b, "  content          not read and not stored — the graph holds a pointer to where it lives (FR-052)\n")
	return p.writeRaw(b.String())
}
