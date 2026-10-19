// SPDX-License-Identifier: Apache-2.0

package graph

import (
	"fmt"
	"regexp"
	"strings"
)

// The service catalog's published names (ADR-0015, feature 009).
//
// A catalog entry is a SERVICE node whose ref is `sre.catalog=<slug>`. The namespace is reserved:
// only the attributor and a person's catalog decisions may name it, so no connector can write the
// catalog by asserting a node or a claim in it (ADR-0015 item 1). The slug is the entry's stable
// identifier, minted once and never changed by a rename, which is why it is a ref value and the
// display name is not.
const (
	// CatalogNamespace is the ref namespace of every catalog entry.
	CatalogNamespace = "sre.catalog"
	// HiddenEntrySlug is the reserved entry a hidden name ("not a service") is aliased to. It has
	// no node: a name aliased to it attributes nothing and is never proposed again.
	HiddenEntrySlug = "-"
	// AttributorSourceID is the source every proposal and attribution is logged under.
	AttributorSourceID = "sre.attributor"
	// AttributionRuleset is the published ruleset id (ADR-0015 item 5).
	AttributionRuleset = "attribution/rules@1"
)

// Props written on a catalog entry (ADR-0015 item 2).
const (
	CatalogKindProp        = "sre.catalog.kind"
	CatalogStatusProp      = "sre.catalog.status"
	CatalogOwnerProp       = "sre.catalog.owner"
	CatalogDescriptionProp = "sre.catalog.description"
)

// Entry kinds and statuses.
const (
	CatalogKindService  = "service"
	CatalogKindPlatform = "platform"

	CatalogStatusProposed  = "proposed"
	CatalogStatusConfirmed = "confirmed"
	CatalogStatusHidden    = "hidden"
)

// Props written on an ATTRIBUTED_TO edge (ADR-0015 item 4).
const (
	AttributionLayerProp     = "sre.attribution.layer"
	AttributionRuleProp      = "sre.attribution.rule_id"
	AttributionRulesetProp   = "sre.attribution.ruleset"
	AttributionScoreProp     = "sre.attribution.score"
	AttributionDecisionProp  = "sre.attribution.decision_event_id"
	AttributionPrincipalProp = "sre.attribution.principal"
)

// Attribution layers, in precedence order (ADR-0015 item 5).
const (
	AttributionLayerCorrection = "correction"
	AttributionLayerAlias      = "alias"
	AttributionLayerProbable   = "probable"
	AttributionLayerClassifier = "classifier"
)

// Correction scopes.
const (
	CorrectionScopeSignal = "signal"
	CorrectionScopeName   = "name"
)

// Name namespaces a connector's signals are named by when the name is not an entity's identity
// claim (ADR-0015 item 7).
const (
	// NotionValueNamespace names a Notion page by one value of its entity property:
	// `notion.value=<database id>/<value>`.
	NotionValueNamespace = "notion.value"
	// NotionValuesProp is where the Notion feeder writes those raw values on the page's node.
	NotionValuesProp = "sre.notion.values"
	// NotionDatabaseProp is the database the page was read from, the first half of its names.
	NotionDatabaseProp = "sre.notion.database"
	// SlackSlugNamespace names a declared incident by one token of its channel slug.
	SlackSlugNamespace = "slack.slug"
)

var slugPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidSlug reports whether s may name a catalog entry.
func ValidSlug(s string) bool { return slugPattern.MatchString(s) }

// CatalogRef is the ref of the entry with this slug.
func CatalogRef(slug string) Ref { return Ref{Namespace: CatalogNamespace, Value: slug} }

// SlugFor mints a slug from a display name: lowercase, every run of other characters one hyphen,
// trimmed to 63 characters. It returns "" when nothing usable is left.
func SlugFor(name string) string {
	var b strings.Builder
	hyphen := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			hyphen = false
		case b.Len() > 0 && !hyphen:
			b.WriteByte('-')
			hyphen = true
		}
	}
	slug := strings.TrimRight(b.String(), "-")
	if len(slug) > 63 {
		slug = strings.TrimRight(slug[:63], "-")
	}
	return slug
}

// CheckCatalogKind refuses an entry kind outside the published two.
func CheckCatalogKind(kind string) error {
	switch kind {
	case CatalogKindService, CatalogKindPlatform:
		return nil
	}
	return fmt.Errorf("kind %q; an entry is a %s or a %s", kind, CatalogKindService, CatalogKindPlatform)
}

// AliasVersionID is the deterministic id of the alias version an event opens for a name, so a
// replay writes the same row.
func AliasVersionID(namespace, value, environment, eventID string) string {
	return hashID("alias", namespace, value, environment, eventID)
}

// AliasKey is the alias_proposals key of a (name, environment, entry) triple.
func AliasKey(name Ref, environment, slug string) string {
	return name.String() + "|" + environment + "|" + slug
}
