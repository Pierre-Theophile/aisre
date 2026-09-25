// SPDX-License-Identifier: Apache-2.0

// Package knowledge is the knowledge worker: retrieval scoped by the graph to documents linked
// by CONCERNS to the investigation's subgraph, scored with BM25 over that scoped set
// (FR-049, research §7).
//
// Retrieval is scoped first and ranked second. Inverting the two, as an embedding-first design
// would, retrieves documents linked to nothing — which FR-049 forbids and which is how a
// retrieval system starts citing a postmortem about a different service with a similar name.
package knowledge
