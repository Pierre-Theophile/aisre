// SPDX-License-Identifier: Apache-2.0

package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/investigation/intake"
)

// The investigation tools (010 FR-023, FR-024, section F). Reading needs `reader`, as the API's
// Get and List do; starting needs `investigator`, as Investigate and Declare do.

// OriginSystem is the symptom origin an explicit investigation started over MCP carries, so that
// it can be told apart from the CLI's (`investigate`) and muted (010 FR-054).
const OriginSystem = "mcp"

// drainTimeout bounds how long the server keeps an investigation's stream open after the tool has
// returned. The engine persists the run whether or not anyone listens; draining only avoids
// cancelling it by hanging up (research R5).
const drainTimeout = 30 * time.Minute

// InvestigationListInput is the input of `investigation_list`.
type InvestigationListInput struct {
	IncidentID string `json:"incident_id,omitempty" jsonschema:"only the runs covering this incident"`
	Status     string `json:"status,omitempty" jsonschema:"only runs in this lifecycle: running, concluded, reopened or failed"`
	Since      string `json:"since,omitempty" jsonschema:"only runs started at or after this instant (RFC 3339, -24h or now)"`
	PageSize   *int   `json:"page_size,omitempty" jsonschema:"page size (default 50)"`
	PageToken  string `json:"page_token,omitempty" jsonschema:"the continuation of a previous page"`
}

// InvestigationGetInput is the input of `investigation_get`.
type InvestigationGetInput struct {
	InvestigationID string `json:"investigation_id" jsonschema:"the investigation's id, from investigation_list or investigation_start"`
}

// Declaration is a declared incident (002 FR-001a). None of its fields is inferred (FR-002a).
type Declaration struct {
	Title    string   `json:"title" jsonschema:"the incident's title, as declared"`
	Severity string   `json:"severity" jsonschema:"severity, as declared (sev1, high...)"`
	Origin   string   `json:"origin" jsonschema:"where the incident lives, as <system>:<stable-id> (slack:C0123ABCD, pagerduty:P123). Re-declaring the same origin at the same instant returns the same investigation"`
	Services []string `json:"services,omitempty" jsonschema:"affected services as <namespace>=<value>; recorded as supplied by a person"`
}

// InvestigationStartInput is the input of `investigation_start`: exactly one of reference and
// declaration.
type InvestigationStartInput struct {
	Reference   string       `json:"reference,omitempty" jsonschema:"investigate this node (a service, or a monitor): <namespace>=<value> or id:<entity_id>"`
	Declaration *Declaration `json:"declaration,omitempty" jsonschema:"declare an incident instead of naming a node"`
	At          string       `json:"at" jsonschema:"the instant the problem is about (the alert instant, or the declaration instant): RFC 3339, -10m or now"`
	Lookback    string       `json:"lookback,omitempty" jsonschema:"how far before at to look, as a duration (default 90m)"`
}

func (t *toolset) addInvestigationTools() {
	inv := t.opts.Clients.Investigation

	addTool(t, roleReader, &mcp.Tool{
		Name:  "investigation_list",
		Title: "Investigations",
		Description: "List aisre's investigations, newest first, with their verdict line and lifecycle. " +
			"Filter by incident, status or start time.",
		Annotations: readOnly("Investigations"),
	}, func(ctx context.Context, in InvestigationListInput) (*mcp.CallToolResult, error) {
		const tool = "investigation_list"
		since, err := parseInstant("since", in.Since)
		if err != nil {
			return usageError(tool, err), nil
		}
		size := defaultListLimit
		if in.PageSize != nil {
			if *in.PageSize < 1 || *in.PageSize > 500 {
				return usageError(tool, fmt.Errorf("page_size %d: want 1 to 500", *in.PageSize)), nil
			}
			size = *in.PageSize
		}
		req := &investigationv1.ListInvestigationsRequest{
			IncidentId: strings.TrimSpace(in.IncidentID), Lifecycle: strings.TrimSpace(in.Status),
			PageSize: int32(size), PageToken: in.PageToken, //nolint:gosec // bounded above
		}
		if !since.IsZero() {
			req.Since = tsOf(since)
		}
		resp, err := inv.List(ctx, connect.NewRequest(req))
		if err != nil {
			return apiError(tool, err), nil
		}
		m := resp.Msg
		return answer{
			msg:          m,
			summary:      fmt.Sprintf("%d investigation(s) on this page.%s", len(m.GetInvestigations()), pageNote(m.GetNextPageToken())),
			partial:      m.GetNextPageToken() != "",
			continuation: m.GetNextPageToken(),
		}.result()
	})

	addTool(t, roleReader, &mcp.Tool{
		Name:  "investigation_get",
		Title: "One investigation: verdict and evidence",
		Description: "Return one investigation whole: its verdict, ranked hypotheses, the evidence ledger " +
			"with citations, what it did not examine, and whether it is still provisional. Use it to " +
			"carry the investigator's evidence into the fix you write.",
		Annotations: readOnly("One investigation: verdict and evidence"),
	}, func(ctx context.Context, in InvestigationGetInput) (*mcp.CallToolResult, error) {
		const tool = "investigation_get"
		id := strings.TrimSpace(in.InvestigationID)
		if id == "" {
			return usageError(tool, errors.New("investigation_id is required")), nil
		}
		resp, err := inv.Get(ctx, connect.NewRequest(&investigationv1.GetRequest{InvestigationId: id}))
		if err != nil {
			return apiError(tool, err), nil
		}
		return investigationAnswer(resp.Msg).result()
	})

	if t.opts.Running == nil {
		return
	}
	addTool(t, roleInvestigator, &mcp.Tool{
		Name:  "investigation_start",
		Title: "Start an investigation",
		Description: "Start an aisre investigation of a node at an instant, or of a declared incident. It " +
			"returns at once with the investigation's id and its first, provisional ranking; follow it " +
			"with investigation_get until it is no longer running. It writes only to aisre's own store, " +
			"never to the systems it observes.",
		Annotations: func() *mcp.ToolAnnotations {
			f := false
			return &mcp.ToolAnnotations{Title: "Start an investigation", ReadOnlyHint: false,
				DestructiveHint: &f, IdempotentHint: false, OpenWorldHint: &f}
		}(),
	}, func(ctx context.Context, in InvestigationStartInput) (*mcp.CallToolResult, error) {
		return t.startInvestigation(ctx, in)
	})
}

func (t *toolset) startInvestigation(ctx context.Context, in InvestigationStartInput) (*mcp.CallToolResult, error) {
	const tool = "investigation_start"
	inv := t.opts.Clients.Investigation
	hasRef := strings.TrimSpace(in.Reference) != ""
	if hasRef == (in.Declaration != nil) {
		return usageError(tool, errors.New("give exactly one of reference and declaration")), nil
	}
	at, err := parseInstant("at", in.At)
	if err != nil {
		return usageError(tool, err), nil
	}
	if at.IsZero() {
		return usageError(tool, errors.New("at is required: an investigation pins the instant it is about")), nil
	}
	lookback := intake.DefaultLookback
	if strings.TrimSpace(in.Lookback) != "" {
		if lookback, err = time.ParseDuration(in.Lookback); err != nil || lookback <= 0 {
			return usageError(tool, fmt.Errorf("lookback %q: want a positive duration such as 90m", in.Lookback)), nil
		}
	}

	open := func(ctx context.Context) (*connect.ServerStreamForClient[investigationv1.Investigation], error) {
		if hasRef {
			ref, err := parseRef("reference", in.Reference)
			if err != nil {
				return nil, err
			}
			name := ref.GetNamespace() + "=" + ref.GetValue()
			return inv.Investigate(ctx, connect.NewRequest(&investigationv1.InvestigateRequest{
				Symptom: &investigationv1.Symptom{
					Transport:        intake.TransportExplicitReference,
					Statement:        "explicit investigation of " + name + ", started over MCP",
					OriginSystem:     OriginSystem,
					OriginRef:        name,
					NamedIdentifiers: []*graphv1.Ref{ref},
					FiredAt:          tsOf(at),
				},
				ValidAt:         tsOf(at),
				LookbackSeconds: int64(lookback / time.Second),
				Profile:         "page",
				Stream:          true,
			}))
		}
		req, err := declareRequest(in.Declaration, at, lookback)
		if err != nil {
			return nil, err
		}
		return inv.Declare(ctx, connect.NewRequest(req))
	}

	subject := t.opts.Subject
	release, err := t.opts.Running.acquire(subject)
	if err != nil {
		return usageError(tool, err), nil
	}
	// The run outlives this call: its context keeps the caller's values (the credential) but not
	// the call's cancellation, and is bounded by drainTimeout.
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), drainTimeout)
	stream, err := open(runCtx)
	if err != nil {
		cancel()
		release()
		var cerr *connect.Error
		if errors.As(err, &cerr) {
			return apiError(tool, err), nil
		}
		return usageError(tool, err), nil
	}
	if !stream.Receive() {
		err := stream.Err()
		_ = stream.Close()
		cancel()
		release()
		if err == nil {
			err = connect.NewError(connect.CodeInternal, io.ErrUnexpectedEOF)
		}
		return apiError(tool, err), nil
	}
	first := stream.Msg()
	go func() {
		defer release()
		defer cancel()
		defer func() { _ = stream.Close() }()
		for stream.Receive() {
		}
	}()
	a := investigationAnswer(first)
	a.summary = fmt.Sprintf("Started investigation %s. %s Follow it with investigation_get.",
		first.GetInvestigationId(), a.summary)
	return a.result()
}

// declareRequest builds the request `aisre investigate declare` builds, so the idempotency key is
// the same and a declaration made twice, from either surface, is one investigation (002 FR-008b).
func declareRequest(d *Declaration, at time.Time, lookback time.Duration) (*investigationv1.DeclareRequest, error) {
	var missing []string
	for _, f := range []struct{ name, value string }{
		{"title", d.Title}, {"severity", d.Severity}, {"origin", d.Origin},
	} {
		if strings.TrimSpace(f.value) == "" {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("declaration needs %s; none of them is ever inferred", strings.Join(missing, ", "))
	}
	system, stableID, found := strings.Cut(strings.TrimSpace(d.Origin), ":")
	if !found || system == "" || stableID == "" {
		return nil, fmt.Errorf("origin %q: write it as <system>:<stable-id>, for example slack:C0123ABCD", d.Origin)
	}
	targets := make([]*investigationv1.TargetRef, 0, len(d.Services))
	named := make([]*graphv1.Ref, 0, len(d.Services))
	for _, raw := range d.Services {
		ref, err := parseRef("services", raw)
		if err != nil {
			return nil, err
		}
		targets = append(targets, &investigationv1.TargetRef{
			Ref: ref, Provenance: investigationv1.TargetRefProvenance_SUPPLIED_BY_HUMAN,
		})
		named = append(named, ref)
	}
	return &investigationv1.DeclareRequest{
		Declaration: &investigationv1.Symptom{
			OriginSystem:     system,
			OriginRef:        d.Origin,
			Transport:        intake.TransportHumanDeclared,
			Statement:        d.Title,
			FiredAt:          tsOf(at),
			Origin:           investigationv1.IntakeOrigin_INTAKE_ORIGIN_DECLARED,
			Severity:         d.Severity,
			Title:            d.Title,
			TargetRefs:       targets,
			NamedIdentifiers: named,
			IdempotencyKey:   intake.DeclarationKey(system, stableID, at),
		},
		LookbackSeconds: int64(lookback / time.Second),
		Profile:         "page",
	}, nil
}

func investigationAnswer(m *investigationv1.Investigation) answer {
	state := strings.ToLower(strings.TrimPrefix(m.GetLifecycle().String(), "LIFECYCLE_"))
	if m.GetProvisional() {
		state += ", provisional (only the prior-only ranking exists so far)"
	}
	verdict := m.GetVerdictLine()
	if verdict == "" {
		verdict = "no verdict yet"
	}
	var validAt, observedAt time.Time
	if m.GetValidAt() != nil {
		validAt = m.GetValidAt().AsTime()
	}
	if m.GetObservedAt() != nil {
		observedAt = m.GetObservedAt().AsTime()
	}
	return answer{
		msg:     m,
		summary: fmt.Sprintf("Investigation %s is %s. Verdict: %s", m.GetInvestigationId(), state, verdict),
		validAt: validAt, observedAt: observedAt,
	}
}

func pageNote(token string) string {
	if token == "" {
		return ""
	}
	return " More on the next page: pass page_token."
}
