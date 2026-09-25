// SPDX-License-Identifier: Apache-2.0

package mcpfeeder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// Talking to an MCP server (ADR-0001 D4: a connector pulls from an external source over its
// API *or its MCP surface*).
//
// One MCP client replaces one hand-written vendor SDK per source. What it does not replace is
// the judgement about what may be called: an MCP server offers whatever tools it likes, a
// `tools/list` answer can change under us between two polls, and a tool named `send_message`
// is one `CallTool` away from writing to production. So the connector keeps a fixed allowlist
// of tool names in its own source code and refuses to call anything else (FR-046,
// constitution VII).
//
// The server's own ReadOnlyHint annotation is checked and reported, never trusted: it is a
// hint from the thing being policed, and the Go SDK's own documentation says clients should
// not make tool-use decisions on annotations from untrusted servers.

// The MCP surface this connector reads, and the whole of it. Adding a name here is the only
// way to make the connector call something new, which is what makes the read-only claim
// reviewable in a diff.
const (
	toolListChannels = "list_channels"
	toolListDocs     = "list_docs"
	resourceChanges  = "directory://changes"
)

// allowedTools is the allowlist. It is a sorted, fixed slice rather than a set built at
// startup so that it reads as a list of permissions in review.
var allowedTools = []string{toolListChannels, toolListDocs}

// allowedResources is the same for resources/read.
var allowedResources = []string{resourceChanges}

// Payload kinds, one per MCP call. They are the directory names the recorded payloads live
// under, and the switch Run dispatches on.
const (
	KindChannels = "channels"
	KindDocs     = "docs"
	KindChanges  = "changes"
)

// Client is a read-only MCP session.
type Client struct {
	session *mcp.ClientSession
}

// Dial opens an MCP session over the stdio of a command it spawns. This is the transport a
// locally-run server uses — including the mock directory in ../mockserver.
func Dial(ctx context.Context, name string, args ...string) (*Client, error) {
	//nolint:gosec // the command is operator configuration, exactly like a database DSN
	return DialCommand(ctx, exec.CommandContext(ctx, name, args...))
}

// DialCommand is Dial over a command the caller has prepared, for when the environment or the
// working directory matters.
func DialCommand(ctx context.Context, cmd *exec.Cmd) (*Client, error) {
	return connect(ctx, &mcp.CommandTransport{Command: cmd})
}

// DialHTTP opens an MCP session over the streamable HTTP transport, which is how a hosted
// server — a vendor's own MCP endpoint — is reached. Authentication is the caller's problem;
// see the limitations section of docs/connectors/mcp.md.
func DialHTTP(ctx context.Context, endpoint string, httpClient *http.Client) (*Client, error) {
	return connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: httpClient})
}

func connect(ctx context.Context, transport mcp.Transport) (*Client, error) {
	impl := &mcp.Implementation{Name: "sre-agent-mcp-feeder", Version: feeder.SDKVersion}
	session, err := mcp.NewClient(impl, nil).Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp-feeder: connect: %w", err)
	}
	return &Client{session: session}, nil
}

// Close ends the session, and with it the child process of a stdio transport.
func (c *Client) Close() error { return c.session.Close() }

// ServerName is the connected server's implementation name, for logging. It is deliberately
// *not* what the connector builds pointers from: a pointer's backend_kind has to be identical
// in a replay, where there is no session to ask, so it is configuration (Feeder.ServerName).
func (c *Client) ServerName() string {
	if init := c.session.InitializeResult(); init != nil && init.ServerInfo != nil {
		return init.ServerInfo.Name
	}
	return ""
}

// VerifyReadOnly is the FR-046 check, run before anything is emitted.
//
// It asks the server what it offers and refuses to proceed unless every tool this connector
// intends to call is present. It also reports any allowlisted tool the server does *not*
// annotate as read-only: that is a signal for the operator, not a decision for the connector,
// because the allowlist is what actually bounds the damage.
//
// PollSource implements it, feeder.Source does not, and Run type-asserts for it — so a replay
// from a recording, where there is no credential to check, skips it rather than pretending.
func (c *Client) VerifyReadOnly(ctx context.Context) error {
	tools, err := c.session.ListTools(ctx, nil)
	if err != nil {
		return fmt.Errorf("mcp-feeder: tools/list: %w", err)
	}
	offered := map[string]*mcp.Tool{}
	for _, t := range tools.Tools {
		offered[t.Name] = t
	}
	var missing, unhinted []string
	for _, name := range allowedTools {
		t, ok := offered[name]
		if !ok {
			missing = append(missing, name)
			continue
		}
		if t.Annotations == nil || !t.Annotations.ReadOnlyHint {
			unhinted = append(unhinted, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("mcp-feeder: server does not offer %s; this connector reads nothing else, so there is nothing to do",
			strings.Join(missing, ", "))
	}
	if len(unhinted) > 0 {
		// Not fatal: the hint is the server's own claim about itself. The allowlist above is
		// the enforcement point, and it is in this file rather than on the wire.
		return &UnhintedToolsWarning{Tools: unhinted}
	}
	return nil
}

// UnhintedToolsWarning reports allowlisted tools the server did not annotate read-only. A
// caller that wants to proceed anyway can match on it; the CLI logs it and continues.
type UnhintedToolsWarning struct {
	Tools []string
}

func (w *UnhintedToolsWarning) Error() string {
	return "mcp-feeder: the server does not annotate " + strings.Join(w.Tools, ", ") +
		" as read-only; proceeding on the connector's own allowlist, which never calls anything else"
}

// callTool calls one allowlisted tool and returns the raw MCP result as JSON.
//
// The recorded payload is this JSON — the protocol's own answer, not a projection of it — so
// that a fixture stays meaningful if the connector's decoding changes, and so that a reader
// can see exactly what the server said.
func (c *Client) callTool(ctx context.Context, name string) ([]byte, error) {
	if !slices.Contains(allowedTools, name) {
		return nil, fmt.Errorf("mcp-feeder: refusing to call %q: not in the read-only allowlist %v", name, allowedTools)
	}
	result, err := c.session.CallTool(ctx, &mcp.CallToolParams{Name: name})
	if err != nil {
		return nil, fmt.Errorf("mcp-feeder: tools/call %s: %w", name, err)
	}
	if result.IsError {
		return nil, fmt.Errorf("mcp-feeder: tools/call %s: %w", name, result.GetError())
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("mcp-feeder: encode %s result: %w", name, err)
	}
	return raw, nil
}

// readResource reads one allowlisted resource and returns the raw MCP result as JSON.
func (c *Client) readResource(ctx context.Context, uri string) ([]byte, error) {
	if !slices.Contains(allowedResources, uri) {
		return nil, fmt.Errorf("mcp-feeder: refusing to read %q: not in the read-only allowlist %v", uri, allowedResources)
	}
	result, err := c.session.ReadResource(ctx, &mcp.ReadResourceParams{URI: uri})
	if err != nil {
		return nil, fmt.Errorf("mcp-feeder: resources/read %s: %w", uri, err)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("mcp-feeder: encode %s result: %w", uri, err)
	}
	return raw, nil
}

// PollSource turns the MCP session into a feeder.Source by polling it.
//
// MCP has no subscription for tool results: `tools/list_changed` says the *set of tools*
// changed, never that their answers did, and `resources/subscribe` is optional and rarely
// implemented. So a directory connector polls, and a poll is one round of every call it makes
// — which is also what a checkpoint covers.
type PollSource struct {
	client   *Client
	interval time.Duration
	maxPolls int

	polls int
	queue []feeder.Payload
}

var _ feeder.Source = (*PollSource)(nil)

// NewPollSource polls c every interval. maxPolls bounds the run — it is what a recording sets
// so that `Next` eventually returns io.EOF; zero polls until the context is done.
func NewPollSource(c *Client, interval time.Duration, maxPolls int) *PollSource {
	if interval <= 0 {
		interval = time.Minute
	}
	return &PollSource{client: c, interval: interval, maxPolls: maxPolls}
}

// VerifyReadOnly forwards to the client, so that Run can find it on the Source.
func (s *PollSource) VerifyReadOnly(ctx context.Context) error { return s.client.VerifyReadOnly(ctx) }

// Next returns the next payload of the current poll, polling again when the queue empties.
func (s *PollSource) Next(ctx context.Context) (feeder.Payload, error) {
	for len(s.queue) == 0 {
		if s.maxPolls > 0 && s.polls >= s.maxPolls {
			return feeder.Payload{}, io.EOF
		}
		if s.polls > 0 {
			select {
			case <-ctx.Done():
				return feeder.Payload{}, ctx.Err()
			case <-time.After(s.interval):
			}
		}
		if err := ctx.Err(); err != nil {
			return feeder.Payload{}, err
		}
		if err := s.poll(ctx); err != nil {
			return feeder.Payload{}, err
		}
	}
	p := s.queue[0]
	s.queue = s.queue[1:]
	return p, nil
}

// poll makes one round of calls, in a fixed order, and queues one payload per answer.
//
// Payload.Seq is the poll number. It is the only thing that groups the three answers of one
// round together, and Run uses it to decide when a round is complete and a checkpoint is due
// — never the arrival order, which a shuffle permutes.
func (s *PollSource) poll(ctx context.Context) error {
	s.polls++
	seq := int64(s.polls)

	calls := []struct {
		kind string
		read func(context.Context) ([]byte, error)
	}{
		{KindChannels, func(ctx context.Context) ([]byte, error) { return s.client.callTool(ctx, toolListChannels) }},
		{KindDocs, func(ctx context.Context) ([]byte, error) { return s.client.callTool(ctx, toolListDocs) }},
		{KindChanges, func(ctx context.Context) ([]byte, error) { return s.client.readResource(ctx, resourceChanges) }},
	}
	for _, call := range calls {
		raw, err := call.read(ctx)
		if err != nil {
			return err
		}
		at, err := generatedAt(call.kind, raw)
		if err != nil {
			return err
		}
		s.queue = append(s.queue, feeder.Payload{Kind: call.kind, At: at, Seq: seq, Bytes: raw})
	}
	return nil
}

// generatedAt reads the instant the *server* says it produced the answer.
//
// It is not the moment the client received it: a payload's arrival time is replayed and
// shuffled, and taking it from the local clock would make a recording unreproducible and a
// shuffle meaningless. A server that does not date its answers leaves a connector no honest
// choice but to say so, which is why this is an error rather than a fallback to time.Now.
func generatedAt(kind string, raw []byte) (time.Time, error) {
	body, err := payloadBody(kind, raw)
	if err != nil {
		return time.Time{}, err
	}
	var stamped struct {
		GeneratedAt string `json:"generatedAt"`
	}
	if err := json.Unmarshal(body, &stamped); err != nil {
		return time.Time{}, fmt.Errorf("mcp-feeder: %s: decode generatedAt: %w", kind, err)
	}
	if stamped.GeneratedAt == "" {
		return time.Time{}, errors.New("mcp-feeder: " + kind + ": the server did not date its answer (no generatedAt)")
	}
	at, err := time.Parse(time.RFC3339, stamped.GeneratedAt)
	if err != nil {
		return time.Time{}, fmt.Errorf("mcp-feeder: %s: generatedAt %q: %w", kind, stamped.GeneratedAt, err)
	}
	return at.UTC(), nil
}

// sortedKeys is a small helper for deterministic iteration over a map.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
