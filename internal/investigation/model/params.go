// SPDX-License-Identifier: Apache-2.0

package model

// The request build (T060, T061, research §2/§3, contracts/prompting.md §"Request layout").
//
// The layout is `tools → system → messages`, and that order is not a style choice: the vendor's
// cache is a prefix match, so everything stable has to come before everything volatile and the
// breakpoint has to sit exactly on the boundary. One breakpoint, on the last system block, which
// covers the tool set and the system prefix together — the ~10k tokens that are identical for
// the life of a schema version. Everything after it is the brief, the ledger render and the
// turns, all of which move every turn by design.
//
// Two parameters this feature never sends, recorded here so their absence is visible:
// `temperature` (and the other sampling parameters), which return 400 on this model family; and
// `tool_choice: {type: "any"|"tool"}`, which returns 400 too — forced tool use does not exist
// here, which is why the engine and not the model is the ledger's writer of record (research §3).

// DefaultMaxTokens is the response cap for an ordinary turn. It is generous enough that a turn
// proposing a batch of judgments is never truncated mid-argument and small enough to stay well
// inside the SDK's non-streaming timeout.
const DefaultMaxTokens int64 = 16000

// Tool is one tool the investigator may call, in this feature's terms.
//
// Every tool is `strict: true` and every schema closes itself with `additionalProperties:
// false`. That is what keeps a proposed judgment schema-valid without forcing the call, which is
// the only lever left once forced tool use is off the table.
type Tool struct {
	// Name is the tool name the model calls. For an algebra term it is the term name, so the
	// tool set and the published algebra are the same vocabulary (FR-042b).
	Name string
	// Description is what the tool does, in one or two sentences.
	Description string
	// Properties is the JSON-schema property map.
	Properties map[string]any
	// Required is the required property list, in a fixed order.
	Required []string
}

// Schema renders the tool's input schema as the closed object the API is sent.
func (t Tool) Schema() map[string]any {
	properties := t.Properties
	if properties == nil {
		properties = map[string]any{}
	}
	required := t.Required
	if required == nil {
		required = []string{}
	}
	return map[string]any{
		"type":                 "object",
		"properties":           properties,
		"required":             required,
		"additionalProperties": false,
	}
}
