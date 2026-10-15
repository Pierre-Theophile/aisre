// SPDX-License-Identifier: Apache-2.0

// Package embedding holds the embedding adapters the agent ships and the indexer that keeps the
// knowledge store's vectors current (feature 008, research R5, R10).
//
// Two adapters implement pkg/knowledge.Embedder: openai_compatible, which speaks
// `POST {base_url}/embeddings` and so reaches the hosted APIs and the self-hosted servers alike,
// and recorded, which replays a fixture's vectors.jsonl and is for tests and the evaluation only.
// Anything else is a Go package implementing the interface; nothing in the agent names a vendor.
package embedding
