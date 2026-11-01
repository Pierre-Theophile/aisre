// SPDX-License-Identifier: Apache-2.0

package mcpserver

import (
	"context"
	_ "embed"

	"connectrpc.com/connect"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
)

// The read-only resources (010 FR-028, FR-029): the reference for the tools, and the catalog as a
// document a client can load as context.

// GuideURI and CatalogURI are the resources' addresses.
const (
	GuideURI   = "aisre://guide"
	CatalogURI = "aisre://catalog"
)

//go:embed guide.md
var guide string

func (t *toolset) addResources() {
	t.server.AddResource(&mcp.Resource{
		URI:         GuideURI,
		Name:        "guide",
		Title:       "Which aisre tool answers which question",
		Description: "The graph's time model, how to write a reference, and the tool for each question.",
		MIMEType:    "text/markdown",
	}, func(_ context.Context, _ *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{
			URI: GuideURI, MIMEType: "text/markdown", Text: guide,
		}}}, nil
	})

	c := t.opts.Clients.Catalog
	if c == nil {
		return
	}
	t.server.AddResource(&mcp.Resource{
		URI:         CatalogURI,
		Name:        "catalog",
		Title:       "The service catalog, now",
		Description: "Every catalog entry with its aliases in each source, as canonical JSON.",
		MIMEType:    "application/json",
	}, func(ctx context.Context, _ *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		resp, err := c.ListEntries(ctx, connect.NewRequest(&graphv1.ListEntriesRequest{
			AsOf: &graphv1.AsOf{ValidAt: tsOf(now())},
		}))
		if err != nil {
			return nil, err
		}
		encoded, err := graph.CanonicalJSON(resp.Msg)
		if err != nil {
			return nil, err
		}
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{
			URI: CatalogURI, MIMEType: "application/json", Text: string(encoded),
		}}}, nil
	})
}
