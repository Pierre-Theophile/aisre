// SPDX-License-Identifier: Apache-2.0

package intake

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/internal/investigation/ledger"
)

// Subject resolution, before reasoning (T079, FR-003, constitution VI).
//
// "checkout" in an alert and "checkout-service" in the graph are the same service or they are
// not, and the engine does not get to guess. It resolves through the published resolution path —
// feature 001's `ResolutionAudit`, which returns the canonical entity a reference belongs to
// together with the decisions that made it so — and it reports **both** the identifier it was
// given and the entity it resolved to, every time.
//
// Three properties this file is responsible for.
//
// **The audit is attached as evidence.** Not as a log line: an evidence item, with the algebra
// term that produced it (`graph.resolution_audit`), so that "we investigated the wrong service"
// is a question a reviewer can answer from the record.
//
// **An unresolvable subject is `unknown`, not a guess** (FR-003, FR-026). The intake still opens
// an investigation — dropping it would lose the incident — but it carries a resolution request
// naming the identity to confirm.
//
// **Merges decided after the observed pin are invisible, and the engine says so.** The audit is
// asked at the investigation's observed instant, so a merge recorded an hour after the alert is
// not in force and the two identifiers stay two entities. That is correct — the investigation
// may only know what was known — but it is also surprising, so where the audit reports decisions
// that fall after the pin the resolution carries a note saying which ones, and the note travels
// into the rendering. Saying "there is a merge you cannot see from here" is the difference
// between a pinned answer and a wrong one.

// Resolver is the published resolution path (feature 001's QueryService.ResolutionAudit).
//
// It is an interface so that the intake can be tested without a database and so that the engine
// can pass whichever client it already holds — in-process query engine, Connect client, or a
// replayed recording.
type Resolver interface {
	ResolutionAudit(ctx context.Context, req *graphv1.ResolutionAuditRequest) (*graphv1.ResolutionAuditResponse, error)
}

// EvidenceKindResolutionAudit is the published evidence kind of a resolution audit.
const EvidenceKindResolutionAudit = "resolution_audit"

// Resolution is what one named identifier resolved to.
type Resolution struct {
	// Given is the identifier the alert or declaration named — reported alongside Resolved,
	// always, because FR-003 asks for both.
	Given graph.Ref
	// EntityID is the canonical graph entity it resolved to. Empty when unresolvable.
	EntityID string
	// Resolved reports whether an entity was found at all.
	Resolved bool
	// Evidence is the resolution audit as an evidence item (FR-003).
	Evidence ledger.EvidenceItem
	// LaterMerges names resolution decisions the audit reports that fall *after* the
	// investigation's observed instant, and are therefore not in force. Empty in the ordinary
	// case; non-empty is the "a merge learned after the alert" edge case, stated rather than
	// silently applied.
	LaterMerges []string
	// Note is the human sentence carried into the rendering when LaterMerges is non-empty, or
	// when the subject did not resolve.
	Note string
}

// ResolutionSet is the resolution of every identifier one symptom named.
type ResolutionSet struct {
	// Resolutions are in the order the symptom named the identifiers.
	Resolutions []Resolution
	// EntityIDs are the canonical entities, deduplicated and sorted — what Group and the engine
	// reason over.
	EntityIDs []string
	// Unresolved lists the identifiers that resolved to nothing.
	Unresolved []graph.Ref
	// Evidence is every audit, in order.
	Evidence []ledger.EvidenceItem
}

// AnyUnresolved reports whether the engine must proceed to `unknown` under FR-003/FR-026 rather
// than reasoning on a guessed identity.
func (s ResolutionSet) AnyUnresolved() bool { return len(s.Unresolved) > 0 }

// Notes are the sentences the rendering carries: unresolvable subjects and invisible merges.
func (s ResolutionSet) Notes() []string {
	var out []string
	for _, r := range s.Resolutions {
		if r.Note != "" {
			out = append(out, r.Note)
		}
	}
	return out
}

// Resolve resolves every identifier a symptom named, at the investigation's observed instant
// (FR-003).
//
// It never fails on an unresolvable subject: that is an outcome, not an error. It fails only
// when the resolution path itself is unreachable, because an engine that silently treated "the
// graph is down" as "this service does not exist" would investigate the wrong thing.
func Resolve(ctx context.Context, r Resolver, in *Intake) (ResolutionSet, error) {
	if in == nil || in.Symptom == nil {
		return ResolutionSet{}, fmt.Errorf("resolve: %w", ErrNoSubject)
	}
	var set ResolutionSet
	seen := make(map[string]struct{})
	for _, named := range in.Symptom.GetNamedIdentifiers() {
		ref := graph.RefFromProto(named)
		if ref.IsZero() {
			continue
		}
		res, err := resolveOne(ctx, r, in, ref)
		if err != nil {
			return ResolutionSet{}, err
		}
		set.Resolutions = append(set.Resolutions, res)
		set.Evidence = append(set.Evidence, res.Evidence)
		if !res.Resolved {
			set.Unresolved = append(set.Unresolved, ref)
			continue
		}
		if _, ok := seen[res.EntityID]; !ok {
			seen[res.EntityID] = struct{}{}
			set.EntityIDs = append(set.EntityIDs, res.EntityID)
		}
	}
	sort.Strings(set.EntityIDs)

	// The symptom row records what its identifiers resolved to and the audit that justifies it
	// (data-model §investigation.symptoms).
	in.Symptom.ResolvedEntityIds = append([]string{}, set.EntityIDs...)
	if len(set.Evidence) > 0 {
		in.Symptom.ResolutionEvidenceId = set.Evidence[0].ID
	}
	return set, nil
}

func resolveOne(ctx context.Context, r Resolver, in *Intake, ref graph.Ref) (Resolution, error) {
	out := Resolution{Given: ref}
	observedAt := in.ObservedAt.UTC()

	// The audit is asked about the reference against itself: "what is the canonical entity of
	// this reference, as of the observed instant". Asking it as a pair would be asking a
	// different question — whether two references are the same entity.
	req := &graphv1.ResolutionAuditRequest{
		A:          ref.Proto(),
		B:          ref.Proto(),
		ObservedAt: timestamppb.New(observedAt),
	}
	var resp *graphv1.ResolutionAuditResponse
	var callErr error
	if r != nil {
		resp, callErr = r.ResolutionAudit(ctx, req)
	}
	calledAt := observedAt

	switch {
	case r == nil:
		out.Note = fmt.Sprintf("%s could not be resolved: no resolution path was configured", ref)
		out.Evidence = resolutionEvidence(in, ref, req, calledAt, "query_failed", out.Note, nil)
		return out, nil
	case callErr != nil:
		// The resolution path being unreachable is a transport failure, not an answer about the
		// world. Reporting it as "unresolvable" would send the investigation to `unknown` for
		// the wrong reason.
		return Resolution{}, fmt.Errorf("resolve %s: %w", ref, callErr)
	}

	canonical := strings.TrimSpace(resp.GetCanonicalId())
	if canonical == "" {
		out.Note = fmt.Sprintf("%s resolves to no entity in the graph as of %s; "+
			"the engine will not guess an identity (FR-003)", ref, observedAt.Format(time.RFC3339))
		out.Evidence = resolutionEvidence(in, ref, req, calledAt, "no_data", out.Note, resp)
		return out, nil
	}

	out.EntityID = canonical
	out.Resolved = true
	out.LaterMerges = decisionsAfter(resp.GetDecisions(), observedAt)
	if len(out.LaterMerges) > 0 {
		out.Note = fmt.Sprintf(
			"%s resolved to %s under the observed-time pin of %s; %d later resolution decision(s) "+
				"(%s) are NOT in force for this investigation and were not applied",
			ref, canonical, observedAt.Format(time.RFC3339),
			len(out.LaterMerges), strings.Join(out.LaterMerges, ", "))
	}
	detail := fmt.Sprintf("%s → %s", ref, canonical)
	if out.Note != "" {
		detail = out.Note
	}
	out.Evidence = resolutionEvidence(in, ref, req, calledAt, "digest", detail, resp)
	return out, nil
}

// decisionsAfter names the resolution decisions that were decided after the observed pin, and
// are therefore invisible to this investigation.
//
// The audit is asked at the pin and answers at the pin, so these should not come back at all —
// but an audit implementation that returns the full decision history is not wrong, and an
// investigation that can say "there is a merge you cannot see from here" is more useful than one
// that cannot tell the difference.
func decisionsAfter(decisions []*graphv1.ResolutionDecision, observedAt time.Time) []string {
	var out []string
	for _, d := range decisions {
		at := d.GetDecidedAt()
		if at == nil || !at.AsTime().After(observedAt) {
			continue
		}
		id := d.GetDecisionId()
		if id == "" {
			id = d.GetKind()
		}
		out = append(out, fmt.Sprintf("%s at %s", id, at.AsTime().UTC().Format(time.RFC3339)))
	}
	sort.Strings(out)
	return out
}

func resolutionEvidence(
	in *Intake,
	ref graph.Ref,
	req *graphv1.ResolutionAuditRequest,
	calledAt time.Time,
	outcome, detail string,
	resp *graphv1.ResolutionAuditResponse,
) ledger.EvidenceItem {
	searched := []string{ref.EntityID()}
	var eventIDs []string
	if resp != nil {
		if id := resp.GetCanonicalId(); id != "" && id != ref.EntityID() {
			searched = append(searched, id)
		}
		for _, d := range resp.GetDecisions() {
			if d.GetEventId() != "" {
				eventIDs = append(eventIDs, d.GetEventId())
			}
		}
		for _, c := range resp.GetClaims() {
			if c.GetEventId() != "" {
				eventIDs = append(eventIDs, c.GetEventId())
			}
		}
	}
	sort.Strings(eventIDs)

	return ledger.EvidenceItem{
		ID:            ResolutionEvidenceID(in.Symptom.GetSymptomId(), ref),
		Kind:          EvidenceKindResolutionAudit,
		Worker:        "graph",
		Capability:    "resolution_audit",
		SourceOfTruth: "graph",
		Term:          backend.GraphResolutionAudit(req),
		ValidAt:       in.ValidAt.UTC(),
		ObservedAt:    in.ObservedAt.UTC(),
		CalledAt:      calledAt,
		Mode:          backend.ModeLive,
		Outcome:       outcome,
		Coverage: &investigationv1.Coverage{
			SearchedEntities:  searched,
			DataSource:        "graph.resolution",
			ExecutedAt:        timestampOf(calledAt),
			VolumeConsidered:  int64(len(searched)),
			QuotaUndetermined: true,
		},
		GraphEventIDs: eventIDs,
		DeepLink: fmt.Sprintf("aisre resolve why %s %s --observed-at %s",
			ref, ref, in.ObservedAt.UTC().Format(time.RFC3339)),
		FreeText: detail,
	}
}

// ResolutionEvidenceID is the evidence id a subject's resolution audit is recorded under.
func ResolutionEvidenceID(symptomID string, ref graph.Ref) string {
	return "ev-resolution-" + symptomID + "-" + ref.String()
}

// UnresolvedSubjectRequest is the resolving action an unresolvable subject produces (FR-003,
// FR-026): confirm the identity, naming the identifier that could not be resolved.
func UnresolvedSubjectRequest(ref graph.Ref) *investigationv1.Resolution {
	return &investigationv1.Resolution{
		Statement: fmt.Sprintf("confirm the identity of %s: it resolves to no graph entity at the "+
			"investigation's observed instant, and the engine will not guess one", ref),
		Kind: ResolutionKindConfirmIdentity,
	}
}
