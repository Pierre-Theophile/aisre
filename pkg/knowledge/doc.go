// SPDX-License-Identifier: Apache-2.0

// Package knowledge is the public contract of the knowledge store and the embedding model
// (feature 008, ADR-0014): the Store a document's sections live in, the Embedder that turns text
// into vectors, the Section that is their unit, and the limits both sides enforce.
//
// The graph scopes and dates the RAG index (constitution 1.2.0): a `KNOWLEDGE_DOC` node and its
// `CONCERNS` edges say that a document exists, what it concerns and since when, and the graph
// never holds its content. The content lives here, outside the graph, versioned by the instant
// each version became known, so that a replay matches exactly the text the original run could
// have read.
//
// Both interfaces are published because the agent is open source and every external dependency
// is the operator's choice, self-hosted included. A program that embeds the agent supplies its own
// Store or Embedder; storetest.Run is the conformance suite a Store passes unmodified. The shipped
// implementations are an in-memory store (package memory: tests and the retrieval evaluation) and
// the agent's own PostgreSQL (the default).
//
// The published contract is specs/008-postmortem-rag/contracts/knowledge-store.md.
package knowledge
