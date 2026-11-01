// SPDX-License-Identifier: Apache-2.0

package mcpserver

import (
	"context"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
)

// The graph tools (010 FR-020, FR-021, contracts/mcp-tools.md). Each maps its input onto the
// request the CLI command of the same name builds, with the CLI's defaults (research R4).

// neighbourhood is the part of a subgraph request diff shares.
type neighbourhood struct {
	Hops      *int     `json:"hops,omitempty" jsonschema:"neighbourhood radius in hops (default 2, at most 8)"`
	Direction string   `json:"direction,omitempty" jsonschema:"edge direction to follow: up, down or both (default both)"`
	EdgeTypes []string `json:"edge_types,omitempty" jsonschema:"follow only these edge types: calls, depends_on, runs_on, deployed_by, owned_by, exposed_via, changed_by"`
	MinWeight *int     `json:"min_weight,omitempty" jsonschema:"drop edges below this traffic weight class (0-5)"`
	PerHopCap *int     `json:"per_hop_cap,omitempty" jsonschema:"maximum fan-out expanded per node (default 50)"`
	TotalCap  *int     `json:"total_cap,omitempty" jsonschema:"maximum nodes in the answer (default 500)"`
}

func (n neighbourhood) apply(req *graphv1.SubgraphRequest) error {
	hops := defaultHops
	if n.Hops != nil {
		hops = *n.Hops
	}
	if hops < 0 || hops > maxHops {
		return fmt.Errorf("hops %d: want 0 to %d", hops, maxHops)
	}
	direction, err := parseDirection(n.Direction)
	if err != nil {
		return err
	}
	edgeTypes, err := parseEdgeTypes(n.EdgeTypes)
	if err != nil {
		return err
	}
	req.Hops = uint32(hops)
	req.Direction = direction
	req.EdgeTypes = edgeTypes
	if n.MinWeight != nil {
		if *n.MinWeight < 0 || *n.MinWeight > 5 {
			return fmt.Errorf("min_weight %d: weight classes run from 0 to 5", *n.MinWeight)
		}
		req.MinWeightClass = u32(*n.MinWeight)
	}
	perHop, total := defaultPerHopCap, defaultTotalCap
	if n.PerHopCap != nil {
		perHop = *n.PerHopCap
	}
	if n.TotalCap != nil {
		total = *n.TotalCap
	}
	if perHop > 0 {
		req.PerHopCap = u32(perHop)
	}
	if total > 0 {
		req.TotalCap = u32(total)
	}
	return nil
}

// SubgraphInput is the input of `subgraph`.
type SubgraphInput struct {
	Focus      string `json:"focus" jsonschema:"the node to start from: <namespace>=<value> (otel.service.name=checkout) or id:<entity_id>"`
	ValidAt    string `json:"valid_at" jsonschema:"the instant the graph is read as of: RFC 3339, -30m or now"`
	ObservedAt string `json:"observed_at,omitempty" jsonschema:"answer as the graph was known at this instant (default: as known now)"`
	neighbourhood
}

// DiffInput is the input of `diff`.
type DiffInput struct {
	Focus        string `json:"focus" jsonschema:"the node whose neighbourhood is compared: <namespace>=<value> or id:<entity_id>"`
	At           string `json:"at,omitempty" jsonschema:"the alert instant: compare the window before it and rank changes against it (instead of from/to)"`
	Window       string `json:"window,omitempty" jsonschema:"how far before at the window reaches, as a duration (default 2h)"`
	From         string `json:"from,omitempty" jsonschema:"the window's start (required unless at)"`
	To           string `json:"to,omitempty" jsonschema:"the window's end (required unless at)"`
	ObservedAt   string `json:"observed_at,omitempty" jsonschema:"answer as known at this instant (default: as known now)"`
	Reference    string `json:"reference,omitempty" jsonschema:"instant the ranking's recency is measured to (default: at, else to)"`
	Tau          string `json:"tau,omitempty" jsonschema:"temporal decay constant of the ranking, as a duration (default 30m)"`
	ChangeMargin *int   `json:"change_margin,omitempty" jsonschema:"how many hops past the neighbourhood a change's target may lie (default 1)"`
	neighbourhood
}

// ImpactInput is the input of `impact`.
type ImpactInput struct {
	Focus      string `json:"focus" jsonschema:"the node whose blast radius is wanted: <namespace>=<value> or id:<entity_id>"`
	ValidAt    string `json:"valid_at" jsonschema:"the instant the graph is read as of: RFC 3339, -30m or now"`
	ObservedAt string `json:"observed_at,omitempty" jsonschema:"answer as known at this instant (default: as known now)"`
	MaxHops    *int   `json:"max_hops,omitempty" jsonschema:"how far the blast radius reaches (default 3, at most 8)"`
	TotalCap   *int   `json:"total_cap,omitempty" jsonschema:"maximum nodes expanded per direction (default 500)"`
}

// FocusInput is the input of `history`.
type FocusInput struct {
	Focus string `json:"focus" jsonschema:"the node: <namespace>=<value> or id:<entity_id>"`
}

// PointersInput is the input of `pointers`.
type PointersInput struct {
	Focus      string `json:"focus" jsonschema:"the node whose telemetry pointers are wanted: <namespace>=<value> or id:<entity_id>"`
	ValidAt    string `json:"valid_at" jsonschema:"the instant the graph is read as of: RFC 3339, -30m or now"`
	ObservedAt string `json:"observed_at,omitempty" jsonschema:"answer as known at this instant (default: as known now)"`
}

// ExtentInput is the (empty) input of `extent`.
type ExtentInput struct{}

// ResolveWhyInput is the input of `resolve_why`.
type ResolveWhyInput struct {
	A          string `json:"a" jsonschema:"first identifier: <namespace>=<value> or id:<entity_id>"`
	B          string `json:"b" jsonschema:"second identifier: <namespace>=<value> or id:<entity_id>"`
	ObservedAt string `json:"observed_at,omitempty" jsonschema:"answer as known at this instant (default: as known now)"`
}

func (t *toolset) addGraphTools() {
	q := t.opts.Clients.Query

	addTool(t, roleReader, &mcp.Tool{
		Name:  "subgraph",
		Title: "Neighbourhood of a node",
		Description: "Return the nodes and edges around a node (services, workloads, databases, " +
			"deployments, owners) as they were at valid_at. Use it to learn what a service talks to and " +
			"runs on before reading code or logs. Truncation is reported, never silent.",
		Annotations: readOnly("Neighbourhood of a node"),
	}, func(ctx context.Context, in SubgraphInput) (*mcp.CallToolResult, error) {
		const tool = "subgraph"
		focus, err := parseRef("focus", in.Focus)
		if err != nil {
			return usageError(tool, err), nil
		}
		at, validAt, observedAt, err := asOf(in.ValidAt, in.ObservedAt)
		if err != nil {
			return usageError(tool, err), nil
		}
		req := &graphv1.SubgraphRequest{Focus: focus, AsOf: at}
		if err := in.apply(req); err != nil {
			return usageError(tool, err), nil
		}
		resp, err := q.Subgraph(ctx, connect.NewRequest(req))
		if err != nil {
			return apiError(tool, err), nil
		}
		m := resp.Msg
		return answer{
			msg: m,
			summary: fmt.Sprintf("%d node(s) and %d edge(s) around %s=%s as of %s.%s",
				len(m.GetNodes()), len(m.GetEdges()), focus.GetNamespace(), focus.GetValue(),
				validAt.Format(time.RFC3339), truncationNote(m.GetTruncation())),
			validAt: validAt, observedAt: observedAt,
			partial:      m.GetTruncation().GetTruncated(),
			continuation: truncationHint(m.GetTruncation()),
		}.result()
	})

	addTool(t, roleReader, &mcp.Tool{
		Name:  "diff",
		Title: "What changed around a node",
		Description: "Compare a node's neighbourhood at two instants and rank the changes of the window " +
			"(deploys, config changes, revisions, scaling) by how likely they are to matter, with the " +
			"formula and each score's components. Give `at` (the alert instant) to look at the 2h before " +
			"it, or `from` and `to`. This is the first question to ask when something broke.",
		Annotations: readOnly("What changed around a node"),
	}, func(ctx context.Context, in DiffInput) (*mcp.CallToolResult, error) {
		const tool = "diff"
		req, t1, t2, observedAt, err := in.request()
		if err != nil {
			return usageError(tool, err), nil
		}
		resp, err := q.Diff(ctx, connect.NewRequest(req))
		if err != nil {
			return apiError(tool, err), nil
		}
		m := resp.Msg
		top := ""
		if changes := m.GetChanges(); len(changes) > 0 {
			c := changes[0].GetChange()
			top = fmt.Sprintf(" Top-ranked: %s (score %.3f).", firstNonEmpty(c.GetDisplayName(), c.GetEntityId()),
				changes[0].GetScore())
		}
		return answer{
			msg: m,
			summary: fmt.Sprintf("%d ranked change(s) between %s and %s; nodes +%d/-%d/~%d, edges +%d/-%d/~%d.%s%s",
				len(m.GetChanges()), t1.Format(time.RFC3339), t2.Format(time.RFC3339),
				len(m.GetNodesAdded()), len(m.GetNodesRemoved()), len(m.GetNodesChanged()),
				len(m.GetEdgesAdded()), len(m.GetEdgesRemoved()), len(m.GetEdgesChanged()),
				top, truncationNote(m.GetTruncation())),
			validAt: t2, observedAt: observedAt,
			partial:      m.GetTruncation().GetTruncated(),
			continuation: truncationHint(m.GetTruncation()),
		}.result()
	})

	addTool(t, roleReader, &mcp.Tool{
		Name:  "impact",
		Title: "Blast radius of a node",
		Description: "Return what depends on a node (downstream) and what it depends on (upstream), with " +
			"hop distances and the heaviest path, as of valid_at. Use it to see who a change can hurt.",
		Annotations: readOnly("Blast radius of a node"),
	}, func(ctx context.Context, in ImpactInput) (*mcp.CallToolResult, error) {
		const tool = "impact"
		focus, err := parseRef("focus", in.Focus)
		if err != nil {
			return usageError(tool, err), nil
		}
		at, validAt, observedAt, err := asOf(in.ValidAt, in.ObservedAt)
		if err != nil {
			return usageError(tool, err), nil
		}
		req := &graphv1.ImpactRequest{Focus: focus, AsOf: at}
		if in.MaxHops != nil {
			if *in.MaxHops < 1 || *in.MaxHops > maxHops {
				return usageError(tool, fmt.Errorf("max_hops %d: want 1 to %d", *in.MaxHops, maxHops)), nil
			}
			req.MaxHops = u32(*in.MaxHops)
		}
		total := defaultTotalCap
		if in.TotalCap != nil {
			total = *in.TotalCap
		}
		if total > 0 {
			req.TotalCap = u32(total)
		}
		resp, err := q.Impact(ctx, connect.NewRequest(req))
		if err != nil {
			return apiError(tool, err), nil
		}
		m := resp.Msg
		return answer{
			msg: m,
			summary: fmt.Sprintf("%d downstream and %d upstream node(s) of %s=%s as of %s.%s",
				len(m.GetDownstream()), len(m.GetUpstream()), focus.GetNamespace(), focus.GetValue(),
				validAt.Format(time.RFC3339), truncationNote(m.GetTruncation())),
			validAt: validAt, observedAt: observedAt,
			partial:      m.GetTruncation().GetTruncated(),
			continuation: truncationHint(m.GetTruncation()),
		}.result()
	})

	addTool(t, roleReader, &mcp.Tool{
		Name:  "history",
		Title: "Every version of a node",
		Description: "Return every version of a node across observed time, superseded ones included, " +
			"and every identity-resolution decision about it. Takes no instant: it shows corrections.",
		Annotations: readOnly("Every version of a node"),
	}, func(ctx context.Context, in FocusInput) (*mcp.CallToolResult, error) {
		const tool = "history"
		focus, err := parseRef("focus", in.Focus)
		if err != nil {
			return usageError(tool, err), nil
		}
		resp, err := q.NodeHistory(ctx, connect.NewRequest(&graphv1.NodeHistoryRequest{Focus: focus}))
		if err != nil {
			return apiError(tool, err), nil
		}
		m := resp.Msg
		return answer{
			msg: m,
			summary: fmt.Sprintf("%d version(s) and %d resolution decision(s) for %s=%s.",
				len(m.GetVersions()), len(m.GetDecisions()), focus.GetNamespace(), focus.GetValue()),
		}.result()
	})

	addTool(t, roleReader, &mcp.Tool{
		Name:  "pointers",
		Title: "Where a node's telemetry lives",
		Description: "Return where a node's metrics, logs, traces, dashboards and source links live in " +
			"the backends the team already runs, as of valid_at. aisre stores no telemetry: these are " +
			"pointers to query with the backend's own tools.",
		Annotations: readOnly("Where a node's telemetry lives"),
	}, func(ctx context.Context, in PointersInput) (*mcp.CallToolResult, error) {
		const tool = "pointers"
		focus, err := parseRef("focus", in.Focus)
		if err != nil {
			return usageError(tool, err), nil
		}
		at, validAt, observedAt, err := asOf(in.ValidAt, in.ObservedAt)
		if err != nil {
			return usageError(tool, err), nil
		}
		resp, err := q.Pointers(ctx, connect.NewRequest(&graphv1.PointersRequest{Focus: focus, AsOf: at}))
		if err != nil {
			return apiError(tool, err), nil
		}
		m := resp.Msg
		count := 0
		for _, list := range m.GetByKind() {
			count += len(list.GetPointers())
		}
		return answer{
			msg: m,
			summary: fmt.Sprintf("%d pointer(s) in %d kind(s) for %s=%s as of %s.", count, len(m.GetByKind()),
				focus.GetNamespace(), focus.GetValue(), validAt.Format(time.RFC3339)),
			validAt: validAt, observedAt: observedAt,
		}.result()
	})

	addTool(t, roleReader, &mcp.Tool{
		Name:  "extent",
		Title: "What the graph has seen, and when",
		Description: "Return the earliest and latest observed instants the graph holds and, per source, " +
			"its last checkpoint and its gaps. Ask it first to choose valid_at, and to know whether a " +
			"silence is real or a feeder gap.",
		Annotations: readOnly("What the graph has seen, and when"),
	}, func(ctx context.Context, _ ExtentInput) (*mcp.CallToolResult, error) {
		resp, err := q.Extent(ctx, connect.NewRequest(&graphv1.ExtentRequest{}))
		if err != nil {
			return apiError("extent", err), nil
		}
		m := resp.Msg
		return answer{
			msg: m,
			summary: fmt.Sprintf("Observed from %s to %s across %d source(s).",
				formatTS(m.GetEarliestObserved().AsTime(), m.GetEarliestObserved() != nil),
				formatTS(m.GetLatestObserved().AsTime(), m.GetLatestObserved() != nil), len(m.GetSources())),
		}.result()
	})

	addTool(t, roleReader, &mcp.Tool{
		Name:  "resolve_why",
		Title: "Are two identifiers the same entity",
		Description: "Explain whether two identifiers (a Datadog service and a Cloud Run service, say) " +
			"resolve to one entity, with the identity claims and the audited decisions behind it.",
		Annotations: readOnly("Are two identifiers the same entity"),
	}, func(ctx context.Context, in ResolveWhyInput) (*mcp.CallToolResult, error) {
		const tool = "resolve_why"
		a, err := parseRef("a", in.A)
		if err != nil {
			return usageError(tool, err), nil
		}
		b, err := parseRef("b", in.B)
		if err != nil {
			return usageError(tool, err), nil
		}
		observedAt, err := parseInstant("observed_at", in.ObservedAt)
		if err != nil {
			return usageError(tool, err), nil
		}
		req := &graphv1.ResolutionAuditRequest{A: a, B: b}
		if !observedAt.IsZero() {
			req.ObservedAt = tsOf(observedAt)
		}
		resp, err := q.ResolutionAudit(ctx, connect.NewRequest(req))
		if err != nil {
			return apiError(tool, err), nil
		}
		m := resp.Msg
		verdict := "are not the same entity"
		if m.GetSameEntity() {
			verdict = "are the same entity (" + m.GetCanonicalId() + ")"
		}
		return answer{
			msg:        m,
			summary:    fmt.Sprintf("%s=%s and %s=%s %s; %d decision(s).", a.GetNamespace(), a.GetValue(), b.GetNamespace(), b.GetValue(), verdict, len(m.GetDecisions())),
			observedAt: observedAt,
		}.result()
	})
}

// request builds the DiffRequest exactly as `aisre query diff` does.
func (in DiffInput) request() (req *graphv1.DiffRequest, t1, t2, observedAt time.Time, err error) {
	focus, err := parseRef("focus", in.Focus)
	if err != nil {
		return nil, t1, t2, observedAt, err
	}
	at := strings.TrimSpace(in.At)
	explicit := strings.TrimSpace(in.From) != "" || strings.TrimSpace(in.To) != ""
	var alertAt time.Time
	switch {
	case at != "" && explicit:
		return nil, t1, t2, observedAt, fmt.Errorf("give either at, or from and to, not both")
	case at != "":
		window := defaultAlertWindow
		if strings.TrimSpace(in.Window) != "" {
			if window, err = time.ParseDuration(in.Window); err != nil || window <= 0 {
				return nil, t1, t2, observedAt, fmt.Errorf("window %q: want a positive duration such as 2h", in.Window)
			}
		}
		if alertAt, err = parseInstant("at", at); err != nil {
			return nil, t1, t2, observedAt, err
		}
		t1, t2 = alertAt.Add(-window), alertAt
	case !explicit || strings.TrimSpace(in.From) == "" || strings.TrimSpace(in.To) == "":
		return nil, t1, t2, observedAt, fmt.Errorf("from and to are both required, or at: a diff is about a window")
	default:
		if t1, err = parseInstant("from", in.From); err != nil {
			return nil, t1, t2, observedAt, err
		}
		if t2, err = parseInstant("to", in.To); err != nil {
			return nil, t1, t2, observedAt, err
		}
	}
	if !t1.Before(t2) {
		return nil, t1, t2, observedAt, fmt.Errorf("from %s is not before to %s", t1.Format(time.RFC3339), t2.Format(time.RFC3339))
	}
	if observedAt, err = parseInstant("observed_at", in.ObservedAt); err != nil {
		return nil, t1, t2, observedAt, err
	}
	reference, err := parseInstant("reference", in.Reference)
	if err != nil {
		return nil, t1, t2, observedAt, err
	}
	if reference.IsZero() && !alertAt.IsZero() {
		reference = alertAt
	}
	sub := &graphv1.SubgraphRequest{Focus: focus}
	if err := in.apply(sub); err != nil {
		return nil, t1, t2, observedAt, err
	}
	req = &graphv1.DiffRequest{Subgraph: sub, T1: tsOf(t1), T2: tsOf(t2)}
	if !observedAt.IsZero() {
		req.ObservedAt = tsOf(observedAt)
	}
	if !reference.IsZero() {
		req.ReferenceAt = tsOf(reference)
	}
	if strings.TrimSpace(in.Tau) != "" {
		tau, perr := time.ParseDuration(in.Tau)
		if perr != nil || tau <= 0 {
			return nil, t1, t2, observedAt, fmt.Errorf("tau %q: want a positive duration such as 30m", in.Tau)
		}
		seconds := tau.Seconds()
		req.TauSeconds = &seconds
	}
	if in.ChangeMargin != nil {
		if *in.ChangeMargin < 0 {
			return nil, t1, t2, observedAt, fmt.Errorf("change_margin %d: want zero or more hops", *in.ChangeMargin)
		}
		req.ChangeHopMargin = u32(*in.ChangeMargin)
	}
	return req, t1, t2, observedAt, nil
}

func truncationNote(t *graphv1.Truncation) string {
	if !t.GetTruncated() {
		return ""
	}
	return fmt.Sprintf(" Truncated (%s): the answer is partial.", firstNonEmpty(t.GetReason(), "cap reached"))
}

// truncationHint is the continuation for a capped answer: the caps are inputs, so the way to get
// the rest is to raise them or narrow the question.
func truncationHint(t *graphv1.Truncation) string {
	if !t.GetTruncated() {
		return ""
	}
	return fmt.Sprintf("raise per_hop_cap (now %d) or total_cap (now %d), or narrow hops, direction or edge_types",
		t.GetPerHopCap(), t.GetTotalCap())
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func formatTS(t time.Time, set bool) string {
	if !set {
		return "unknown"
	}
	return t.UTC().Format(time.RFC3339)
}
