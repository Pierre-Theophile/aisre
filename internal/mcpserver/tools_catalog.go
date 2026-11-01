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

// The catalog tools (010 FR-022): the agent-owned service catalog of feature 009, read back.

// CatalogListInput is the input of `catalog_list`.
type CatalogListInput struct {
	Status string `json:"status,omitempty" jsonschema:"only entries with this status: proposed, confirmed or hidden"`
	AsOf   string `json:"as_of,omitempty" jsonschema:"the catalog as it was then: RFC 3339, -30m or now (default now)"`
}

// CatalogShowInput is the input of `catalog_show`.
type CatalogShowInput struct {
	Slug string `json:"slug" jsonschema:"the catalog entry's slug, for example candidate-view"`
	AsOf string `json:"as_of,omitempty" jsonschema:"the entry as it was then (default now)"`
}

// AttributionWhyInput is the input of `attribution_why`.
type AttributionWhyInput struct {
	Signal string `json:"signal" jsonschema:"the signal (alert, declaration, document or change): <namespace>=<value> or id:<entity_id>"`
	AsOf   string `json:"as_of,omitempty" jsonschema:"the answer as it was then (default now)"`
}

// catalogAsOf reads one instant as both valid and observed time, as `aisre catalog` does; empty
// is "now, as known now".
func catalogAsOf(raw string) (*graphv1.AsOf, time.Time, error) {
	at, err := parseInstant("as_of", raw)
	if err != nil {
		return nil, at, err
	}
	if at.IsZero() {
		at = now()
		return &graphv1.AsOf{ValidAt: tsOf(at)}, at, nil
	}
	return &graphv1.AsOf{ValidAt: tsOf(at), ObservedAt: tsOf(at)}, at, nil
}

func (t *toolset) addCatalogTools() {
	c := t.opts.Clients.Catalog

	addTool(t, roleReader, &mcp.Tool{
		Name:  "catalog_list",
		Title: "The service catalog",
		Description: "List the services aisre knows, with each one's kind, owner, status and every name it " +
			"goes by in each source (Datadog service, Cloud Run service, Notion value, Slack name). Use it " +
			"to map a name you see in code or an alert onto the service it belongs to.",
		Annotations: readOnly("The service catalog"),
	}, func(ctx context.Context, in CatalogListInput) (*mcp.CallToolResult, error) {
		const tool = "catalog_list"
		at, when, err := catalogAsOf(in.AsOf)
		if err != nil {
			return usageError(tool, err), nil
		}
		resp, err := c.ListEntries(ctx, connect.NewRequest(&graphv1.ListEntriesRequest{AsOf: at,
			Status: strings.TrimSpace(in.Status)}))
		if err != nil {
			return apiError(tool, err), nil
		}
		m := resp.Msg
		return answer{
			msg:     m,
			summary: fmt.Sprintf("%d catalog entr(ies), %d hidden name(s).", len(m.GetEntries()), len(m.GetHidden())),
			validAt: when, observedAt: observedOf(at),
		}.result()
	})

	addTool(t, roleReader, &mcp.Tool{
		Name:        "catalog_show",
		Title:       "One catalog entry",
		Description: "Return one service of the catalog with its aliases and its history.",
		Annotations: readOnly("One catalog entry"),
	}, func(ctx context.Context, in CatalogShowInput) (*mcp.CallToolResult, error) {
		const tool = "catalog_show"
		slug := strings.TrimSpace(in.Slug)
		if slug == "" {
			return usageError(tool, fmt.Errorf("slug is required")), nil
		}
		at, when, err := catalogAsOf(in.AsOf)
		if err != nil {
			return usageError(tool, err), nil
		}
		resp, err := c.GetEntry(ctx, connect.NewRequest(&graphv1.GetEntryRequest{Slug: slug, AsOf: at}))
		if err != nil {
			return apiError(tool, err), nil
		}
		m := resp.Msg
		e := m.GetEntry()
		return answer{
			msg: m,
			summary: fmt.Sprintf("%s: %s %s, %d alias(es), %d version(s).", e.GetSlug(), e.GetStatus(),
				firstNonEmpty(e.GetKind(), "service"), len(e.GetAliases()), len(m.GetHistory())),
			validAt: when, observedAt: observedOf(at),
		}.result()
	})

	addTool(t, roleReader, &mcp.Tool{
		Name:  "attribution_why",
		Title: "Which service a signal belongs to, and why",
		Description: "Explain which catalog service an alert, declaration, document or change was " +
			"attributed to, by which rule and with what confidence, including corrections by people.",
		Annotations: readOnly("Which service a signal belongs to, and why"),
	}, func(ctx context.Context, in AttributionWhyInput) (*mcp.CallToolResult, error) {
		const tool = "attribution_why"
		signal, err := parseRef("signal", in.Signal)
		if err != nil {
			return usageError(tool, err), nil
		}
		at, when, err := catalogAsOf(in.AsOf)
		if err != nil {
			return usageError(tool, err), nil
		}
		resp, err := c.WhyAttributed(ctx, connect.NewRequest(&graphv1.WhyAttributedRequest{Signal: signal, AsOf: at}))
		if err != nil {
			return apiError(tool, err), nil
		}
		m := resp.Msg
		cur := m.GetCurrent()
		entries := make([]string, 0, len(cur.GetEntries()))
		for _, e := range cur.GetEntries() {
			entries = append(entries, e.GetEntrySlug())
		}
		target := "unknown"
		if len(entries) > 0 {
			target = strings.Join(entries, ", ")
		}
		return answer{
			msg: m,
			summary: fmt.Sprintf("%s=%s is attributed to %s (%s); %d decision(s) in its history.",
				signal.GetNamespace(), signal.GetValue(), target, firstNonEmpty(cur.GetKind(), "no decision"),
				len(m.GetHistory())),
			validAt: when, observedAt: observedOf(at),
		}.result()
	})
}

func observedOf(at *graphv1.AsOf) time.Time {
	if at.GetObservedAt() == nil {
		return time.Time{}
	}
	return at.GetObservedAt().AsTime()
}
