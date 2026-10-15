// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"connectrpc.com/connect"

	knowledgev1 "github.com/Pierre-Theophile/aisre/api/sreagent/knowledge/v1"
	"github.com/Pierre-Theophile/aisre/api/sreagent/knowledge/v1/knowledgev1connect"
	"github.com/Pierre-Theophile/aisre/pkg/knowledge"
)

// The section endpoint (feature 008 FR-005, FR-006, research R7).
//
// A feeder sends a document's whole current set of sections after the document's graph events.
// The store keeps them outside the graph (constitution 1.2.0): the graph says which documents
// exist, what they are linked to and when; the store holds what they say.
//
// Authorisation is IngestBatch's, plus one rule: a source may write only documents in its own
// namespace. A source id is `<kind>:<name>` and a document ref is `<kind>.<type>=<id>`, so a
// token for `notion:LDMA` may write `notion.page=…` and nothing else. Page ids are globally
// unique, so the namespace check is what stops one feeder overwriting another's content.

// KnowledgeService implements sreagent.knowledge.v1.KnowledgeService.
type KnowledgeService struct {
	store  knowledge.Store
	logger *slog.Logger
	now    func() time.Time
}

var _ knowledgev1connect.KnowledgeServiceHandler = (*KnowledgeService)(nil)

// NewKnowledgeService returns the section handler over store. logger defaults to slog.Default().
func NewKnowledgeService(store knowledge.Store, logger *slog.Logger) *KnowledgeService {
	if logger == nil {
		logger = slog.Default()
	}
	return &KnowledgeService{store: store, logger: logger, now: time.Now}
}

// PutSections replaces a document's sections. Unchanged sections are no-ops, so a feeder may
// resend a document as often as it likes.
func (s *KnowledgeService) PutSections(ctx context.Context, req *connect.Request[knowledgev1.PutSectionsRequest]) (*connect.Response[knowledgev1.PutSectionsResponse], error) {
	msg := req.Msg
	if msg.GetSourceId() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("put_sections: source_id is required"))
	}
	if err := RequireSource(ctx, msg.GetSourceId()); err != nil {
		return nil, err
	}
	if msg.GetDocumentRef() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("put_sections: document_ref is required"))
	}
	if !knowledge.OwnsDocument(msg.GetSourceId(), msg.GetDocumentRef()) {
		return nil, connect.NewError(connect.CodePermissionDenied,
			fmt.Errorf("put_sections: source %q does not own document %q", msg.GetSourceId(), msg.GetDocumentRef()))
	}

	sections := make([]knowledge.Section, 0, len(msg.GetSections()))
	for _, section := range msg.GetSections() {
		sections = append(sections, knowledge.Section{
			Label:  section.GetLabel(),
			Part:   int(section.GetPart()),
			Text:   section.GetText(),
			Origin: knowledge.Origin(section.GetOrigin()),
		})
	}
	if err := knowledge.ValidateFeederSections(sections); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("put_sections: %w", err))
	}

	result, err := s.store.Put(ctx, msg.GetDocumentRef(), sections, s.now())
	if err != nil {
		if errors.Is(err, knowledge.ErrInvalidSections) {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("put_sections: %w", err))
		}
		return nil, connect.NewError(connect.CodeInternal,
			fmt.Errorf("put_sections %s: %w", msg.GetDocumentRef(), err))
	}
	s.logger.Debug("sections stored",
		"source", msg.GetSourceId(), "document", msg.GetDocumentRef(),
		"opened", result.Opened, "closed", result.Closed, "unchanged", result.Unchanged)
	return connect.NewResponse(&knowledgev1.PutSectionsResponse{
		Opened:    uint32(result.Opened),    //nolint:gosec // bounded by MaxSections
		Closed:    uint32(result.Closed),    //nolint:gosec // bounded by MaxSections
		Unchanged: uint32(result.Unchanged), //nolint:gosec // bounded by MaxSections
	}), nil
}
