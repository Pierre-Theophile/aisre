// SPDX-License-Identifier: Apache-2.0

package knowledge

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/query"
)

// GraphCorpus is the corpus a running deployment reads: the graph's CONCERNS and INVESTIGATED
// edges as of the request's instants (007 FR-005–FR-008, ADR-0013 item 3).
//
// Until feature 007 the worker had only test doubles to read, so a live `knowledge_search`
// answered "no registered worker". This is the one read that turns registered documents and
// reviewed investigations into candidates; the ranking above it is unchanged.
type GraphCorpus struct {
	Engine *query.Engine
}

var _ Corpus = GraphCorpus{}

// Props the projector writes on an INVESTIGATION version that a citation reads
// (internal/projector).
const (
	investigationVerdictProp    = "sre.investigation.verdict_line"
	investigationRequesterProp  = "sre.investigation.requester"
	investigationLabelledByProp = "sre.investigation.labelled_by"
	investigationNamespace      = "sre.investigation.id"
)

// Scoped implements Corpus.
func (c GraphCorpus) Scoped(ctx context.Context, entityIDs []string, at Instants) ([]Document, error) {
	candidates, err := c.Engine.KnowledgeCandidates(ctx, entityIDs, query.AsOf{ValidAt: at.Valid, ObservedAt: at.Observed})
	if err != nil {
		return nil, fmt.Errorf("knowledge: read the corpus: %w", err)
	}
	out := make([]Document, 0, len(candidates))
	for _, candidate := range candidates {
		out = append(out, documentOf(candidate, at.Valid))
	}
	return out, nil
}

func documentOf(c query.KnowledgeCandidate, at time.Time) Document {
	doc := Document{
		ID:              c.Ref,
		Title:           c.DisplayName,
		LinkedEntityIDs: c.Linked,
	}
	if c.Type == graph.NodeTypeInvestigation {
		// A reviewed past investigation: the engine's own verdict, cited by its id. Its summary is
		// empty on purpose — the verdict line is the engine's words, and the title already holds
		// them; the ranking should not count them twice.
		doc.Kind = KindInvestigation
		if verdict := stringProp(c.Props, investigationVerdictProp); verdict != "" {
			doc.Title = verdict
		}
		doc.Location = strings.TrimPrefix(c.Ref, investigationNamespace+"=")
		doc.AuthoredAt = c.LinkedSince
		doc.LinkProvenance = "investigated by " + orUnknown(stringProp(c.Props, investigationRequesterProp)) +
			"; reviewed by " + orUnknown(stringProp(c.Props, investigationLabelledByProp))
	} else {
		doc.Kind = stringProp(c.Props, "kind")
		doc.Summary = stringProp(c.Props, "summary")
		doc.Location = stringProp(c.Props, "location")
		doc.LinkProvenance = stringProp(c.Props, "link_provenance")
		if authored, err := time.Parse(time.RFC3339, stringProp(c.Props, "authored_at")); err == nil {
			doc.AuthoredAt = authored.UTC()
		} else {
			doc.AuthoredAt = c.LinkedSince
		}
	}
	if !doc.AuthoredAt.IsZero() && at.After(doc.AuthoredAt) {
		doc.AgeDays = int64(at.Sub(doc.AuthoredAt) / (24 * time.Hour))
	}
	return doc
}

func stringProp(props map[string]any, key string) string {
	if value, ok := props[key].(string); ok {
		return value
	}
	return ""
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
