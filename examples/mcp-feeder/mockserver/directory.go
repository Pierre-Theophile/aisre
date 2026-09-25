// SPDX-License-Identifier: Apache-2.0

// Package mockserver is a tiny MCP server standing in for a real ownership directory —
// the Slack, Notion or PagerDuty MCP server a production deployment would point at.
//
// It exists so that the connector in ../feeder has something to be written against, and so
// that the recorded fixture in ../testdata holds real MCP responses rather than payloads
// invented by hand. Everything it returns is a pure function of its seed and of how many
// times each endpoint has been called, which is what makes the recording reproducible: the
// live test in ../feeder replays the same two polls against a freshly spawned server and
// compares the events byte for byte against the ones recorded from this server.
//
// What it exposes, mirroring what a real directory server would:
//
//	tool     list_channels          chat channels, the services each one owns, who is on call
//	tool     list_docs              runbooks, and the services each one concerns
//	resource directory://changes    announcements of changes people made to those services
//
// Nothing here mutates anything. Both tools are annotated ReadOnlyHint, which the connector
// checks — and does not trust: see ../feeder/client.go for why the allowlist, not the hint,
// is the enforcement point.
package mockserver

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// Wire types. These are the server's half of the contract; the connector declares its own
// structs over the same JSON (feeder/decode.go), exactly as it would against a vendor whose
// Go types it does not import.

// Channel is one chat channel and the services it owns.
type Channel struct {
	// Channel is the channel's display name, e.g. "#team-checkout".
	Channel string `json:"channel" jsonschema:"the channel name, e.g. #team-checkout"`
	// Team is the team's slug in the directory, e.g. "checkout". It is a second name for the
	// same owner, which is what makes it an identity claim rather than a property.
	Team string `json:"team" jsonschema:"the owning team's slug"`
	// Oncall is the handle currently on call for the channel.
	Oncall string `json:"oncall" jsonschema:"the handle currently on call"`
	// URL is a link back to the channel in the chat tool.
	URL string `json:"url" jsonschema:"a link to the channel"`
	// Owns are the service names the channel declares ownership of.
	Owns []string `json:"owns" jsonschema:"the service names this channel owns"`
}

// ChannelsResult is what list_channels returns.
type ChannelsResult struct {
	// Version is a content hash of the channel list. It changes when, and only when, the
	// directory's answer changes, which is what the connector derives its event ids from.
	Version string `json:"version" jsonschema:"content version of this answer"`
	// GeneratedAt is when the directory produced this answer, RFC 3339.
	GeneratedAt string `json:"generatedAt" jsonschema:"when this answer was produced"`
	// Channels is the directory, sorted by channel name.
	Channels []Channel `json:"channels" jsonschema:"the channels"`
}

// Doc is one runbook and the services it concerns.
type Doc struct {
	// Title is the document's title.
	Title string `json:"title" jsonschema:"the document title"`
	// URL is where the document lives. It becomes a SOURCE_LINK pointer's selector.
	URL string `json:"url" jsonschema:"the document URL"`
	// Concerns are the service names the document is about.
	Concerns []string `json:"concerns" jsonschema:"the service names this document concerns"`
}

// DocsResult is what list_docs returns.
type DocsResult struct {
	Version     string `json:"version" jsonschema:"content version of this answer"`
	GeneratedAt string `json:"generatedAt" jsonschema:"when this answer was produced"`
	Docs        []Doc  `json:"docs" jsonschema:"the documents"`
}

// Change is one announced change to a service.
type Change struct {
	// ID is the announcement's identifier in the directory, stable for ever.
	ID string `json:"id"`
	// Kind is the change kind as the directory names it; only "flag_flip" is produced here.
	Kind string `json:"kind"`
	// Flag is the feature flag that was flipped.
	Flag string `json:"flag"`
	// Enabled is the state it was flipped to.
	Enabled bool `json:"enabled"`
	// Actor is who announced it.
	Actor string `json:"actor"`
	// At is when the change happened, RFC 3339. It is the change's valid time.
	At string `json:"at"`
	// Services are the service names the change was applied to.
	Services []string `json:"services"`
	// Summary is the announcement as a human read it.
	Summary string `json:"summary"`
	// URL links back to the announcement.
	URL string `json:"url"`
}

// ChangesResult is what reading directory://changes returns.
type ChangesResult struct {
	Version     string   `json:"version"`
	GeneratedAt string   `json:"generatedAt"`
	Changes     []Change `json:"changes"`
}

// version renders a short content hash of v, so that an unchanged directory returns an
// unchanged version and the connector's event ids stay put across polls.
func version(v any) string {
	raw, err := json.Marshal(v)
	if err != nil { // unreachable: every value hashed here is a plain struct
		return "0000000000000000"
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8])
}

// rfc3339 renders a timestamp the way every field above spells one.
func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }
