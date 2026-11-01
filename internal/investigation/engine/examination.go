// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"fmt"
	"sort"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/investigation/expectations"
	"github.com/Pierre-Theophile/aisre/internal/investigation/ledger"
)

// The examination report (FR-022b, T125, plan V6).
//
// A conclusion says what it examined and what it did not. For each hypothesis: the signal families
// it queried on its target entities; the decisive families (the expectation table's row for its
// cause category) that were reachable on those targets and never queried, which are omissions;
// the decisive families that were not reachable, which are coverage gaps (FR-027) and never
// omissions; and, once for the run, the entities in scope that no call touched.
//
// It is computed at the stop from three things the engine already holds: the search-space profile
// (what was reachable), the ledger's evidence items (what was asked, of which entity, in which
// family) and the hypotheses. Nothing is asked to build it.
//
// In `observe` mode, the only mode version 1 of the table is in, it changes no status. The hold of
// `enforce` mode is T132's, after ADR-0019 D4's dual gate.

// Examination is the report.
type Examination struct {
	// Table is the expectation table that was applied, as `v1 (observe)`.
	Table string
	// Hypotheses are the per-hypothesis findings, in ledger rank order.
	Hypotheses []HypothesisExamination
	// Untouched are the entities in scope that no call touched, by reference.
	Untouched []string
	// Omissions is the total over hypotheses, for the omission rate (FR-059a).
	Omissions int
}

// HypothesisExamination is one hypothesis's part of the report.
type HypothesisExamination struct {
	HypothesisID string
	// Category is the cause category the table was read by; empty for a hypothesis that has
	// none (the open `no observed change` hypothesis).
	Category string
	// Targets are the hypothesis's target entities, by reference.
	Targets []string
	// Queried are the families asked of its targets, as `metrics on <ref>`.
	Queried []string
	// Omissions are decisive families reachable on a target and never asked of it.
	Omissions []string
	// Gaps are decisive families no target could be asked for.
	Gaps []string
}

// CategoryOf is a hypothesis's cause category (FR-022b): the change kind for a change, the
// `not_change_induced` category for a named condition, and none for the open hypothesis.
func (e *Engine) CategoryOf(h ledger.Hypothesis) string {
	switch h.Kind {
	case ledger.KindChange:
		e.mu.Lock()
		kind, ok := e.changeKinds[h.CandidateChangeEntityID]
		e.mu.Unlock()
		if !ok || kind == graphv1.ChangeKind_CHANGE_KIND_UNSPECIFIED {
			return "other"
		}
		return strings.ToLower(strings.TrimPrefix(kind.String(), "CHANGE_KIND_"))
	case ledger.KindNoObservedChange:
		return ""
	default:
		return "not_change_induced"
	}
}

// Examination returns the report as the run stands. It is what Stop records.
func (e *Engine) Examination() *Examination {
	table := e.expectations
	if table == nil {
		table = expectations.Published()
	}
	profile := e.Profile()
	out := &Examination{Table: table.Label()}

	// What was asked of which entity, in which family.
	queried := map[string]map[string]bool{} // entity id -> family -> asked
	touched := map[string]bool{}
	for _, item := range e.ledger.EvidenceItems() {
		family := familyOfWorker(item.Worker)
		if family == "" || item.Term == nil {
			continue
		}
		for _, id := range e.entitiesOfTerm(item.Term, profile) {
			touched[id] = true
			if queried[id] == nil {
				queried[id] = map[string]bool{}
			}
			queried[id][family] = true
		}
	}

	scope := map[string]ProfileEntity{}
	for _, ent := range profile.InScope() {
		scope[ent.ID] = ent
	}
	refOf := func(id string) string {
		if ent, ok := scope[id]; ok {
			return ent.Ref
		}
		if ref := e.catalogue.RefFor(id); ref != "" {
			return ref
		}
		return id
	}

	for _, h := range e.ledger.Hypotheses() {
		category := e.CategoryOf(h)
		hx := HypothesisExamination{HypothesisID: h.ID, Category: category}
		targets := append([]string(nil), h.TargetEntityIDs...)
		sort.Strings(targets)
		for _, id := range targets {
			hx.Targets = append(hx.Targets, refOf(id))
			for _, family := range sortedKeys(queried[id]) {
				hx.Queried = append(hx.Queried, family+" on "+refOf(id))
			}
		}
		if decisive, ok := table.Decisive(category); ok && category != "" {
			for _, family := range decisive {
				reachable := false
				for _, id := range targets {
					ent, inScope := scope[id]
					if !inScope || !ent.Has(family) {
						continue
					}
					reachable = true
					if !queried[id][family] {
						hx.Omissions = append(hx.Omissions, family+" on "+ent.Ref)
					}
				}
				if !reachable {
					hx.Gaps = append(hx.Gaps, family+" (no target has a "+family+" pointer)")
				}
			}
		}
		out.Omissions += len(hx.Omissions)
		out.Hypotheses = append(out.Hypotheses, hx)
	}

	for _, ent := range profile.InScope() {
		if !touched[ent.ID] {
			out.Untouched = append(out.Untouched, ent.Ref)
		}
	}
	return out
}

// familyOfWorker is the signal family a worker's answers belong to; "" for one that is not a
// signal family (chat, the onset estimator's own items are the metrics worker's).
func familyOfWorker(worker string) string {
	switch worker {
	case "metrics":
		return FamilyMetrics
	case "logs":
		return FamilyLogs
	case "traces":
		return FamilyTraces
	case "knowledge":
		return FamilyKnowledge
	case "graph":
		return FamilyChanges
	default:
		return ""
	}
}

// entitiesOfTerm finds the entities a term asked about: every pointer it carries, resolved through
// the profile, and every graph reference, resolved through the catalogue. It walks the message
// rather than switching on term kinds, so a term added to the algebra is covered without an edit.
func (e *Engine) entitiesOfTerm(term proto.Message, profile *Profile) []string {
	seen := map[string]bool{}
	var walk func(m protoreflect.Message)
	walk = func(m protoreflect.Message) {
		switch v := m.Interface().(type) {
		case *graphv1.Pointer:
			if id, ok := profile.EntityOfPointer(v); ok {
				seen[id] = true
			}
			return
		case *investigationv1.ErrorSpansTerm:
			// The one term whose fields are entity ids rather than references (catalogue.go).
			for _, id := range []string{v.GetSrcEntityId(), v.GetDstEntityId()} {
				if id != "" {
					seen[id] = true
				}
			}
			return
		case *graphv1.Ref:
			if v.GetNamespace() != "" && v.GetValue() != "" {
				if id := e.catalogue.EntityIDFor(v.GetNamespace() + "=" + v.GetValue()); id != "" {
					seen[id] = true
				}
			}
			return
		}
		m.Range(func(fd protoreflect.FieldDescriptor, val protoreflect.Value) bool {
			if fd.Kind() != protoreflect.MessageKind {
				return true
			}
			switch {
			case fd.IsList():
				list := val.List()
				for i := 0; i < list.Len(); i++ {
					walk(list.Get(i).Message())
				}
			case fd.IsMap():
				if fd.MapValue().Kind() == protoreflect.MessageKind {
					val.Map().Range(func(_ protoreflect.MapKey, mv protoreflect.Value) bool {
						walk(mv.Message())
						return true
					})
				}
			default:
				walk(val.Message())
			}
			return true
		})
	}
	walk(term.ProtoReflect())
	return sortedKeys(seen)
}

// Proto is the report in its published shape.
func (x *Examination) Proto() *investigationv1.ExaminationReport {
	if x == nil {
		return nil
	}
	out := &investigationv1.ExaminationReport{
		ExpectationTable:  x.Table,
		UntouchedEntities: x.Untouched,
		Omissions:         uint32(x.Omissions), //nolint:gosec // a count of strings, never negative
	}
	for _, h := range x.Hypotheses {
		out.Hypotheses = append(out.Hypotheses, &investigationv1.HypothesisExamination{
			HypothesisId: h.HypothesisID, CauseCategory: h.Category, Targets: h.Targets,
			Queried: h.Queried, Omissions: h.Omissions, CoverageGaps: h.Gaps,
		})
	}
	return out
}

// Render is the report as a reader reads it.
func (x *Examination) Render() string {
	if x == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "expectation table: %s\n", x.Table)
	for _, h := range x.Hypotheses {
		fmt.Fprintf(&b, "%s", h.HypothesisID)
		if h.Category != "" {
			fmt.Fprintf(&b, " [%s]", h.Category)
		}
		b.WriteString("\n")
		fmt.Fprintf(&b, "  targets: %s\n", listOrNone(h.Targets))
		fmt.Fprintf(&b, "  queried: %s\n", listOrNone(h.Queried))
		if len(h.Omissions) > 0 {
			fmt.Fprintf(&b, "  reachable and not queried (decisive): %s\n", strings.Join(h.Omissions, "; "))
		}
		if len(h.Gaps) > 0 {
			fmt.Fprintf(&b, "  coverage gap (decisive, unreachable): %s\n", strings.Join(h.Gaps, "; "))
		}
	}
	fmt.Fprintf(&b, "entities in scope no call touched: %s\n", listOrNone(x.Untouched))
	return b.String()
}

func listOrNone(items []string) string {
	if len(items) == 0 {
		return "none"
	}
	return strings.Join(items, ", ")
}
