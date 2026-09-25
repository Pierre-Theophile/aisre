// SPDX-License-Identifier: Apache-2.0

package mockserver

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"sort"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ServerName is the MCP server's implementation name. The connector turns it into the
// `backend_kind` of every pointer it attaches — `mcp:acme-directory` — so it is part of the
// published mapping and not an implementation detail.
const ServerName = "acme-directory"

// ChangesURI is the resource the announcements are read from.
const ChangesURI = "directory://changes"

// Tool names. The connector keeps the same two strings in its allowlist and calls nothing
// else, ever (feeder/client.go).
const (
	ToolListChannels = "list_channels"
	ToolListDocs     = "list_docs"
)

// DefaultStart is the virtual instant the first answer is generated at. It is fixed rather
// than taken from the wall clock so that two runs of the same server produce the same bytes,
// which is what lets the live test compare against a recording made a month earlier.
var DefaultStart = time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC)

// DefaultTick is how far the virtual clock advances between polls. It is longer than the
// connector's declared reordering window, so a conformance shuffle permutes payloads within a
// poll and never across two of them.
const DefaultTick = 30 * time.Second

// Options configures a mock directory.
type Options struct {
	// Seed selects the on-call rotation. Any seed produces a directory; the same seed
	// produces the same one.
	Seed int64
	// Start is the virtual instant of the first answer. Zero means DefaultStart.
	Start time.Time
	// Tick is how far the virtual clock advances per poll. Zero means DefaultTick.
	Tick time.Duration
}

// directory is the mock's state: fixed content, plus a per-endpoint call counter that drives
// the virtual clock.
//
// The clock advances per endpoint rather than per call, so that the three answers of one poll
// share one `generatedAt` however the connector orders them, and poll N+1's answers are one
// tick later than poll N's. That is the smallest amount of state that makes two polls
// distinguishable without making them differ in content — which is the case a directory
// connector spends its life in, and the case checkpoints exist for.
type directory struct {
	start time.Time
	tick  time.Duration

	channels []Channel
	docs     []Doc
	changes  []Change

	mu    sync.Mutex
	calls map[string]int
}

// New returns an MCP server exposing the mock directory. Run it on any transport:
//
//	mockserver.New(mockserver.Options{Seed: 1}).Run(ctx, &mcp.StdioTransport{})
func New(o Options) *mcp.Server {
	d := newDirectory(o)
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, Title: "read-only"}

	server := mcp.NewServer(&mcp.Implementation{
		Name:    ServerName,
		Title:   "Acme ownership directory",
		Version: "1.0.0",
	}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name:        ToolListChannels,
		Description: "List chat channels, the services each one owns, and who is on call.",
		Annotations: readOnly,
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ noArgs) (*mcp.CallToolResult, ChannelsResult, error) {
		return nil, d.listChannels(), nil
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        ToolListDocs,
		Description: "List runbooks and the services each one concerns.",
		Annotations: readOnly,
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ noArgs) (*mcp.CallToolResult, DocsResult, error) {
		return nil, d.listDocs(), nil
	})

	server.AddResource(&mcp.Resource{
		URI:         ChangesURI,
		Name:        "changes",
		Title:       "Announced changes",
		Description: "Changes to the directory's services that somebody announced.",
		MIMEType:    "application/json",
	}, func(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		body, err := json.Marshal(d.listChanges())
		if err != nil {
			return nil, fmt.Errorf("mockserver: encode changes: %w", err)
		}
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{
			URI:      req.Params.URI,
			MIMEType: "application/json",
			Text:     string(body),
		}}}, nil
	})

	return server
}

// noArgs is the argument type of both tools: neither takes a parameter, which is what makes
// them safe to put on an allowlist without also having to police their arguments.
type noArgs struct{}

func newDirectory(o Options) *directory {
	d := &directory{start: o.Start, tick: o.Tick, calls: map[string]int{}}
	if d.start.IsZero() {
		d.start = DefaultStart
	}
	if d.tick <= 0 {
		d.tick = DefaultTick
	}

	// The on-call handle is the only thing the seed varies: it is the field a real directory
	// changes hourly, and varying it is enough to show that the fixture is of one particular
	// directory rather than of the connector's imagination.
	rotation := []string{"alice", "bob", "carol", "dave", "erin", "frank"}
	pick := rand.New(rand.NewPCG(uint64(o.Seed), 0x5EED)) //nolint:gosec // reproducible content, not a security decision
	oncall := func() string { return rotation[pick.IntN(len(rotation))] }

	d.channels = []Channel{
		{
			Channel: "#team-checkout", Team: "checkout", Oncall: oncall(),
			URL:  "https://acme.slack.example/archives/C0CHECKOUT",
			Owns: []string{"cart", "checkout"},
		},
		{
			Channel: "#team-payments", Team: "payments", Oncall: oncall(),
			URL:  "https://acme.slack.example/archives/C0PAYMENTS",
			Owns: []string{"ledger", "payments"},
		},
		{
			Channel: "#team-search", Team: "discovery", Oncall: oncall(),
			URL:  "https://acme.slack.example/archives/C0SEARCH",
			Owns: []string{"recommendations", "search"},
		},
	}
	d.docs = []Doc{
		{Title: "Checkout runbook", URL: "https://runbooks.acme.example/checkout", Concerns: []string{"cart", "checkout"}},
		{Title: "Ledger reconciliation", URL: "https://runbooks.acme.example/ledger", Concerns: []string{"ledger"}},
		{Title: "Payments on-call guide", URL: "https://runbooks.acme.example/payments", Concerns: []string{"payments"}},
		{Title: "Search latency playbook", URL: "https://runbooks.acme.example/search", Concerns: []string{"recommendations", "search"}},
	}
	// Three announcements, the last of which happens between the first and the second poll.
	// That is how a polling connector's change detection is exercised: nothing about the
	// directory changes, and the third change still has to arrive.
	d.changes = []Change{
		{
			ID: "chg-0001", Kind: "flag_flip", Flag: "checkout.new_pricing", Enabled: true,
			Actor: "bob", At: rfc3339(d.start.Add(-19 * time.Minute)),
			Services: []string{"checkout"},
			Summary:  "flag checkout.new_pricing enabled by bob",
			URL:      "https://acme.slack.example/archives/C0CHECKOUT/p0001",
		},
		{
			ID: "chg-0002", Kind: "flag_flip", Flag: "cart.async_reserve", Enabled: false,
			Actor: "carol", At: rfc3339(d.start.Add(-5 * time.Minute)),
			Services: []string{"cart", "checkout"},
			Summary:  "flag cart.async_reserve disabled by carol",
			URL:      "https://acme.slack.example/archives/C0CHECKOUT/p0002",
		},
		{
			ID: "chg-0003", Kind: "flag_flip", Flag: "search.vector_rerank", Enabled: true,
			Actor: "dave", At: rfc3339(d.start.Add(20 * time.Second)),
			Services: []string{"search"},
			Summary:  "flag search.vector_rerank enabled by dave",
			URL:      "https://acme.slack.example/archives/C0SEARCH/p0003",
		},
	}
	return d
}

// now returns the virtual instant of this endpoint's next answer.
func (d *directory) now(endpoint string) time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := d.calls[endpoint]
	d.calls[endpoint]++
	return d.start.Add(time.Duration(n) * d.tick)
}

func (d *directory) listChannels() ChannelsResult {
	channels := append([]Channel(nil), d.channels...)
	sort.Slice(channels, func(i, j int) bool { return channels[i].Channel < channels[j].Channel })
	return ChannelsResult{
		Version:     version(channels),
		GeneratedAt: rfc3339(d.now(ToolListChannels)),
		Channels:    channels,
	}
}

func (d *directory) listDocs() DocsResult {
	docs := append([]Doc(nil), d.docs...)
	sort.Slice(docs, func(i, j int) bool { return docs[i].URL < docs[j].URL })
	return DocsResult{
		Version:     version(docs),
		GeneratedAt: rfc3339(d.now(ToolListDocs)),
		Docs:        docs,
	}
}

// listChanges returns the announcements made by the time of this answer. A directory has no
// way to push, so this is the whole of change detection: the connector asks again and sees
// one more line than last time.
func (d *directory) listChanges() ChangesResult {
	at := d.now(ChangesURI)
	var out []Change
	for _, c := range d.changes {
		when, err := time.Parse(time.RFC3339, c.At)
		if err != nil || when.After(at) {
			continue
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return ChangesResult{
		Version:     version(out),
		GeneratedAt: rfc3339(at),
		Changes:     out,
	}
}
