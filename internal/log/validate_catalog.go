// SPDX-License-Identifier: Apache-2.0

package log

import (
	"strings"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
)

// The service catalog's events (ADR-0015, feature 009).
//
// Two kinds of refusal are specific to them, and both exist so that no connector can decide what a
// service is.
//
//   - The namespace `sre.catalog` is reserved: an upsert, a claim or a correlation from any source
//     that names it is refused, as is an `ATTRIBUTED_TO` edge asserted by upsert. Only the catalog
//     events below create entries and attributions.
//   - Proposals and attributions are accepted only from the source `sre.attributor`. A feeder that
//     sent one would be the attributor, which is exactly what ADR-0015 rejects.
//
// That a person's decision carries a principal is decided by the projector, which is where the
// principal arrives (missing_principal).

// ReasonCatalogNamespaceReserved is an event that writes the catalog from a source that may not
// (ADR-0015 item 1).
const ReasonCatalogNamespaceReserved = "catalog_namespace_reserved"

// checkCatalogReserved refuses the ordinary events that would write the catalog.
func checkCatalogReserved(env *graphv1.EventEnvelope) *Rejection {
	reserved := func(field string, ref *graphv1.Ref) *Rejection {
		if ref.GetNamespace() != graph.CatalogNamespace {
			return nil
		}
		return reject(ReasonCatalogNamespaceReserved,
			"%s names the namespace %s, which only catalog decisions may write (ADR-0015 item 1)",
			field, graph.CatalogNamespace)
	}
	switch body := env.GetBody().(type) {
	case *graphv1.EventEnvelope_UpsertNode:
		return reserved("upsert_node.ref", body.UpsertNode.GetRef())
	case *graphv1.EventEnvelope_UpsertEdge:
		if body.UpsertEdge.GetType() == graphv1.EdgeType_ATTRIBUTED_TO {
			return reject(ReasonCatalogNamespaceReserved,
				"upsert_edge.type ATTRIBUTED_TO is written only by attribute_signal and correct_attribution")
		}
		if r := reserved("upsert_edge.src", body.UpsertEdge.GetSrc()); r != nil {
			return r
		}
		return reserved("upsert_edge.dst", body.UpsertEdge.GetDst())
	case *graphv1.EventEnvelope_IdentityClaim:
		if r := reserved("identity_claim.subject", body.IdentityClaim.GetSubject()); r != nil {
			return r
		}
		return reserved("identity_claim.claim", body.IdentityClaim.GetClaim())
	case *graphv1.EventEnvelope_CorrelateEntity:
		return reserved("correlate_entity.subject", body.CorrelateEntity.GetSubject())
	case *graphv1.EventEnvelope_ProposeCatalogEntry, *graphv1.EventEnvelope_ProposeAlias,
		*graphv1.EventEnvelope_AttributeSignal:
		if env.GetSourceId() != graph.AttributorSourceID {
			return reject(ReasonCatalogNamespaceReserved,
				"%s is accepted only from the source %s, not %q", EventType(env),
				graph.AttributorSourceID, env.GetSourceId())
		}
	}
	return nil
}

func validateProposeCatalogEntry(p *graphv1.ProposeCatalogEntry) *Rejection {
	if r := checkSlug("propose_catalog_entry.slug", p.GetSlug()); r != nil {
		return r
	}
	if strings.TrimSpace(p.GetDisplayName()) == "" {
		return reject(ReasonMissingRef, "propose_catalog_entry.display_name")
	}
	return requireRefs("propose_catalog_entry.aliases", p.GetAliases())
}

func validateDeclareCatalogEntry(d *graphv1.DeclareCatalogEntry) *Rejection {
	if r := checkSlug("declare_catalog_entry.slug", d.GetSlug()); r != nil {
		return r
	}
	if strings.TrimSpace(d.GetDisplayName()) == "" {
		return reject(ReasonMissingRef, "declare_catalog_entry.display_name")
	}
	if err := graph.CheckCatalogKind(d.GetKind()); err != nil {
		return reject(ReasonUnknownType, "declare_catalog_entry.kind: %v", err)
	}
	switch d.GetStatus() {
	case graph.CatalogStatusConfirmed, graph.CatalogStatusHidden:
	default:
		return reject(ReasonUnknownType, "declare_catalog_entry.status %q; a person declares an entry "+
			"%s or %s (only the attributor proposes)", d.GetStatus(),
			graph.CatalogStatusConfirmed, graph.CatalogStatusHidden)
	}
	if len(d.GetDisplayName()) > 120 || len(d.GetOwner()) > 120 || len(d.GetDescription()) > 1000 {
		return reject(ReasonTelemetryPayload, "declare_catalog_entry: display_name and owner are at most "+
			"120 characters, description at most 1,000")
	}
	return nil
}

// validateAlias checks the (name, environment, entry) triple every alias body carries.
func validateAlias(field string, name *graphv1.Ref, slug string) *Rejection {
	if r := requireRef(field+".name", name); r != nil {
		return r
	}
	if name.GetNamespace() == graph.CatalogNamespace {
		return reject(ReasonCatalogNamespaceReserved, "%s.name: an entry is not an alias of another", field)
	}
	if slug == graph.HiddenEntrySlug {
		return nil
	}
	return checkSlug(field+".entry_slug", slug)
}

func validateProposeAlias(p *graphv1.ProposeAlias) *Rejection {
	if r := validateAlias("propose_alias", p.GetName(), p.GetEntrySlug()); r != nil {
		return r
	}
	if p.GetEntrySlug() == graph.HiddenEntrySlug {
		return reject(ReasonUnknownType, "propose_alias.entry_slug: only a person hides a name")
	}
	if p.GetRuleId() == "" {
		return reject(ReasonMissingRef, "propose_alias.rule_id (a proposal names the published rule that made it)")
	}
	if p.GetScore() <= 0 || p.GetScore() > 1 {
		return reject(ReasonUnknownType, "propose_alias.score is %v; it must be in (0,1]", p.GetScore())
	}
	return checkTelemetry("propose_alias.evidence", p.GetEvidence())
}

func validateAttributeSignal(a *graphv1.AttributeSignal) *Rejection {
	if r := requireRef("attribute_signal.signal", a.GetSignal()); r != nil {
		return r
	}
	if a.GetRuleset() == "" {
		return reject(ReasonMissingRef, "attribute_signal.ruleset (every attribution records the ruleset it ran under)")
	}
	for _, list := range []struct {
		field string
		items []*graphv1.Attributed
	}{{"attribute_signal.entries", a.GetEntries()}, {"attribute_signal.suggestions", a.GetSuggestions()}} {
		for _, item := range list.items {
			if r := checkSlug(list.field+".entry_slug", item.GetEntrySlug()); r != nil {
				return r
			}
			if item.GetRuleId() == "" || item.GetLayer() == "" {
				return reject(ReasonMissingRef, "%s: every entry names its layer and rule", list.field)
			}
			if item.GetScore() < 0 || item.GetScore() > 1 {
				return reject(ReasonUnknownType, "%s.score is %v; it must be in [0,1]", list.field, item.GetScore())
			}
		}
	}
	return checkTelemetry("attribute_signal.evidence", a.GetEvidence())
}

func validateCorrectAttribution(c *graphv1.CorrectAttribution) *Rejection {
	if r := requireRef("correct_attribution.signal", c.GetSignal()); r != nil {
		return r
	}
	for _, slug := range c.GetEntrySlugs() {
		if r := checkSlug("correct_attribution.entry_slugs", slug); r != nil {
			return r
		}
	}
	switch c.GetScope() {
	case graph.CorrectionScopeSignal:
		if c.GetName() != nil {
			return reject(ReasonUnknownType, "correct_attribution.name is set on a signal-level correction")
		}
	case graph.CorrectionScopeName:
		if r := requireRef("correct_attribution.name", c.GetName()); r != nil {
			return r
		}
		if len(c.GetEntrySlugs()) != 1 {
			return reject(ReasonUnknownType, "correct_attribution: a name-level correction names exactly "+
				"one entry, since a name is an alias of one entry")
		}
	default:
		return reject(ReasonUnknownType, "correct_attribution.scope %q; it is %s or %s", c.GetScope(),
			graph.CorrectionScopeSignal, graph.CorrectionScopeName)
	}
	return nil
}

func checkSlug(field, slug string) *Rejection {
	if slug == "" {
		return reject(ReasonMissingRef, "%s", field)
	}
	if !graph.ValidSlug(slug) {
		return reject(ReasonUnknownType, "%s %q; a slug is lowercase letters, digits and hyphens, "+
			"at most 63 characters", field, slug)
	}
	return nil
}
