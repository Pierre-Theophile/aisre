// SPDX-License-Identifier: Apache-2.0

package mcpfeeder

import (
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Decoding an MCP answer.
//
// A recorded payload is the raw `CallToolResult` or `ReadResourceResult` as JSON, so decoding
// it is two steps: unwrap the MCP envelope, then read the directory's own document out of it.
//
// The document is taken from the result's *text content*, not from `structuredContent`.
// Structured output is optional in the protocol and was added late; every MCP server in the
// wild returns text, and the servers that do return structured content return it *as well as*
// the text. Reading the text is therefore the path that works everywhere, and it is the one a
// Slack or Notion server would exercise.
//
// The structs below are the connector's own, deliberately not the mock server's: a connector
// declares the subset of the vendor's JSON it understands and ignores the rest, so that a
// field added upstream is not a decoding failure.

// channelsDoc is what list_channels answers with.
type channelsDoc struct {
	Version     string       `json:"version"`
	GeneratedAt string       `json:"generatedAt"`
	Channels    []channelDoc `json:"channels"`
}

type channelDoc struct {
	Channel string   `json:"channel"`
	Team    string   `json:"team"`
	Oncall  string   `json:"oncall"`
	URL     string   `json:"url"`
	Owns    []string `json:"owns"`
}

// docsDoc is what list_docs answers with.
type docsDoc struct {
	Version     string   `json:"version"`
	GeneratedAt string   `json:"generatedAt"`
	Docs        []docDoc `json:"docs"`
}

type docDoc struct {
	Title    string   `json:"title"`
	URL      string   `json:"url"`
	Concerns []string `json:"concerns"`
}

// changesDoc is what reading directory://changes answers with.
type changesDoc struct {
	Version     string      `json:"version"`
	GeneratedAt string      `json:"generatedAt"`
	Changes     []changeDoc `json:"changes"`
}

type changeDoc struct {
	ID       string   `json:"id"`
	Kind     string   `json:"kind"`
	Flag     string   `json:"flag"`
	Enabled  bool     `json:"enabled"`
	Actor    string   `json:"actor"`
	At       string   `json:"at"`
	Services []string `json:"services"`
	Summary  string   `json:"summary"`
	URL      string   `json:"url"`
}

// payloadBody unwraps the MCP envelope and returns the directory document inside it.
func payloadBody(kind string, raw []byte) ([]byte, error) {
	switch kind {
	case KindChannels, KindDocs:
		var result mcp.CallToolResult
		if err := json.Unmarshal(raw, &result); err != nil {
			return nil, fmt.Errorf("mcp-feeder: %s: decode CallToolResult: %w", kind, err)
		}
		if result.IsError {
			return nil, fmt.Errorf("mcp-feeder: %s: the server answered with an error result", kind)
		}
		for _, content := range result.Content {
			if text, ok := content.(*mcp.TextContent); ok {
				return []byte(text.Text), nil
			}
		}
		return nil, fmt.Errorf("mcp-feeder: %s: the tool returned no text content", kind)
	case KindChanges:
		var result mcp.ReadResourceResult
		if err := json.Unmarshal(raw, &result); err != nil {
			return nil, fmt.Errorf("mcp-feeder: %s: decode ReadResourceResult: %w", kind, err)
		}
		for _, contents := range result.Contents {
			if contents.Text != "" {
				return []byte(contents.Text), nil
			}
		}
		return nil, fmt.Errorf("mcp-feeder: %s: the resource returned no text contents", kind)
	default:
		return nil, fmt.Errorf("mcp-feeder: unknown payload kind %q", kind)
	}
}

// decode unwraps the envelope and unmarshals the document into out.
func decode[T any](kind string, raw []byte, out *T) error {
	body, err := payloadBody(kind, raw)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("mcp-feeder: %s: decode document: %w", kind, err)
	}
	return nil
}
