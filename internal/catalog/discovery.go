// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/structpb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
)

// Discovery (research R9, FR-010, FR-014, FR-015): a fresh install has a catalog before anyone
// writes one.
//
//   - Every SERVICE entity none of whose names is aliased becomes a proposed entry, with its
//     identity claims as the entry's aliases (rule D1): 001's certain rules already made those claims
//     one entity's names, so carrying them over is not a new inference. Correlation keys are not
//     carried: they are names without an environment, and an alias without one matches every
//     environment (research R2).
//   - Every Notion value no alias names becomes a proposed entry too, aliased by its own name.
//   - When two entries' names are the same service by name (P1, P2), the names of one are proposed
//     as aliases of the other. A person accepts with an alias declaration or rejects durably.
//
// A hidden name is never proposed, and neither is anything already proposed: an entry proposal for
// a slug that exists changes nothing, and an alias proposal is raised once, then left to a person.
// Slack slug tokens are not proposed as entries: they are the words of a channel name ("checkout",
// "latency"), and most of them name no service.

// RuleEntityClaim is the rule id an alias carried by a proposed entry records (the projector writes
// it on the alias row).
const RuleEntityClaim = "D1"

// discoveryCandidate is one entry discovery may propose.
type discoveryCandidate struct {
	slug    string
	display string
	aliases []graph.Ref
	// names are every name of the candidate, for "is any of it already in the catalog?".
	names []Name
}

// catalogEntry is an entry as discovery reads it.
type catalogEntry struct {
	slug, display, status string
}

// Discover appends the proposals discovery finds and returns how many it appended.
func (a *Attributor) Discover(ctx context.Context) (int, error) {
	facts, err := a.facts(ctx, Signal{})
	if err != nil {
		return 0, err
	}
	entries, err := a.entries(ctx)
	if err != nil {
		return 0, err
	}
	candidates, err := a.serviceCandidates(ctx)
	if err != nil {
		return 0, err
	}
	notion, err := a.notionCandidates(ctx)
	if err != nil {
		return 0, err
	}
	candidates = append(candidates, notion...)

	appended := 0
	for _, c := range candidates {
		if c.slug == "" || aliased(c.names, facts.Aliases) {
			continue
		}
		if taken, exists := entries[c.slug]; exists {
			// The slug is taken: whatever took it is the same service by name, which P1 says, as a
			// proposal. A hidden entry was a person's no.
			if taken.status == graph.CatalogStatusHidden {
				continue
			}
			rule, score := NameScore(c.display, taken.display)
			if score == 0 {
				rule, score = RuleNormalised, ScoreNormalised
			}
			for _, ref := range c.aliases {
				ok, err := a.proposeAlias(ctx, Name{Ref: ref}, c.slug, rule, score, map[string]any{
					"to_entry": c.slug, "names": anyList([]string{c.display, taken.display}),
				})
				if err != nil {
					return appended, err
				}
				if ok {
					appended++
				}
			}
			continue
		}
		aliases := make([]*graphv1.Ref, 0, len(c.aliases))
		for _, ref := range c.aliases {
			aliases = append(aliases, ref.Proto())
		}
		ok, err := a.append(ctx, "propose_entry", []string{c.slug},
			&graphv1.EventEnvelope{Body: &graphv1.EventEnvelope_ProposeCatalogEntry{ProposeCatalogEntry: &graphv1.ProposeCatalogEntry{
				Slug: c.slug, DisplayName: c.display, Aliases: aliases,
			}}})
		if err != nil {
			return appended, err
		}
		if ok {
			appended++
		}
		entries[c.slug] = catalogEntry{slug: c.slug, display: c.display, status: graph.CatalogStatusProposed}
		for _, ref := range c.aliases {
			facts.Aliases = append(facts.Aliases, Alias{Name: Name{Ref: ref}, Slug: c.slug, Discovered: true})
		}
	}

	n, err := a.relateEntries(ctx, entries, facts.Aliases)
	return appended + n, err
}

// relateEntries proposes the names of a proposed entry as aliases of another entry that is the
// same service by name. Only what discovery carried over moves: an entry a person gave a name to is
// theirs. The direction is fixed so a pair is proposed once: to an entry a person declared; else from
// an entry named by a system (a Datadog or Cloud Run service) to one named by people (a Notion
// value); and between two of a kind from the later slug to the earlier.
func (a *Attributor) relateEntries(ctx context.Context, entries map[string]catalogEntry, aliases []Alias) (int, error) {
	namesOf, discovered, curated := map[string][]Name{}, map[string][]Name{}, map[string]bool{}
	for _, alias := range aliases {
		namesOf[alias.Slug] = append(namesOf[alias.Slug], alias.Name)
		if alias.Discovered {
			discovered[alias.Slug] = append(discovered[alias.Slug], alias.Name)
		} else {
			curated[alias.Slug] = true
		}
	}
	slugs := make([]string, 0, len(entries))
	for slug := range entries {
		slugs = append(slugs, slug)
	}
	slices.Sort(slugs)

	movable := func(slug string) bool {
		return entries[slug].status == graph.CatalogStatusProposed && len(discovered[slug]) > 0 && !curated[slug]
	}
	toward := func(to, from string) bool {
		target := entries[to]
		switch {
		case to == from || target.status == graph.CatalogStatusHidden:
			return false
		case target.status != graph.CatalogStatusProposed:
			return true
		case len(namesOf[to]) == 0:
			return false
		}
		toPeople, fromPeople := namedByPeople(namesOf[to]), namedByPeople(discovered[from])
		if toPeople != fromPeople {
			return toPeople
		}
		return to < from
	}

	appended := 0
	for _, from := range slugs {
		if !movable(from) {
			continue
		}
		source := entries[from]
		bestRule, bestScore, bestTo := "", 0.0, ""
		for _, to := range slugs {
			if !toward(to, from) {
				continue
			}
			rule, score := entryScore(source, discovered[from], entries[to], namesOf[to])
			if score > bestScore {
				bestRule, bestScore, bestTo = rule, score, to
			}
		}
		if bestTo == "" {
			continue
		}
		for _, name := range discovered[from] {
			ok, err := a.proposeAlias(ctx, name, bestTo, bestRule, bestScore, map[string]any{
				"from_entry": from, "to_entry": bestTo,
				"names": anyList([]string{source.display, entries[bestTo].display}),
			})
			if err != nil {
				return appended, err
			}
			if ok {
				appended++
			}
		}
	}
	return appended, nil
}

// namedByPeople says every name of an entry is one people wrote: a Notion value or a Slack slug.
func namedByPeople(names []Name) bool {
	for _, n := range names {
		if n.Ref.Namespace != graph.NotionValueNamespace && n.Ref.Namespace != graph.SlackSlugNamespace {
			return false
		}
	}
	return true
}

// entryScore is the best name score between two entries' display names and names.
func entryScore(a catalogEntry, aNames []Name, b catalogEntry, bNames []Name) (string, float64) {
	texts := func(e catalogEntry, names []Name) []string {
		out := []string{e.display, e.slug}
		for _, n := range names {
			out = append(out, NameText(n.Ref))
		}
		return sortedUnique(out)
	}
	bestRule, best := "", 0.0
	for _, x := range texts(a, aNames) {
		for _, y := range texts(b, bNames) {
			if rule, score := NameScore(x, y); score > best {
				bestRule, best = rule, score
			}
		}
	}
	return bestRule, best
}

// proposeAlias raises one alias proposal unless one is already open or decided.
func (a *Attributor) proposeAlias(ctx context.Context, name Name, slug, rule string, score float64,
	evidence map[string]any,
) (bool, error) {
	var exists bool
	err := a.projector.Store().Pool().QueryRow(ctx, `SELECT true FROM graph.alias_proposals WHERE proposal_key = $1`,
		graph.AliasKey(name.Ref, name.Environment, slug)).Scan(&exists)
	switch {
	case err == nil:
		// Raised before: pending, or a person decided. Raising it again would not be a new
		// proposal, and a rejected pair stays rejected (FR-011).
		return false, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return false, fmt.Errorf("catalog: read alias proposal: %w", err)
	}
	ev, err := structpb.NewStruct(evidence)
	if err != nil {
		return false, fmt.Errorf("catalog: evidence of alias proposal: %w", err)
	}
	score = math.Round(score*1e4) / 1e4
	return a.append(ctx, "propose_alias", []string{graph.AliasKey(name.Ref, name.Environment, slug), rule},
		&graphv1.EventEnvelope{Body: &graphv1.EventEnvelope_ProposeAlias{ProposeAlias: &graphv1.ProposeAlias{
			Name: name.Ref.Proto(), Environment: name.Environment, EntrySlug: slug, RuleId: rule, Score: score,
			Rationale: fmt.Sprintf("%s is named like the entry %s (%s)", name, slug, rule),
			Evidence:  ev,
		}}})
}

// append stamps and applies one attributor event whose id is a digest of its kind and key, so a
// proposal raised twice is one event.
func (a *Attributor) append(ctx context.Context, kind string, key []string, env *graphv1.EventEnvelope) (bool, error) {
	h := sha256.New()
	h.Write([]byte(kind))
	for _, part := range key {
		h.Write([]byte{0})
		h.Write([]byte(part))
	}
	env.EventId = graph.AttributorSourceID + ":" + kind + ":" + hex.EncodeToString(h.Sum(nil))[:32]
	env.IdempotencyKey = env.EventId
	env.SourceId = graph.AttributorSourceID
	env.SchemaVersion = SchemaVersion
	result, err := a.projector.Apply(ctx, env, a.now())
	if err != nil {
		return false, fmt.Errorf("catalog: append %s: %w", env.GetEventId(), err)
	}
	if result.GetStatus() == graphv1.IngestResult_REJECTED {
		return false, fmt.Errorf("catalog: %s refused: %s %s", env.GetEventId(), result.GetReasonCode(), result.GetReasonDetail())
	}
	return result.GetStatus() == graphv1.IngestResult_APPLIED, nil
}

func aliased(names []Name, aliases []Alias) bool {
	for _, n := range names {
		for _, alias := range aliases {
			if alias.matches(n) || (alias.Name.Ref == n.Ref && n.Environment == "") {
				return true
			}
		}
	}
	return false
}

// entries reads every catalog entry as it is now.
func (a *Attributor) entries(ctx context.Context) (map[string]catalogEntry, error) {
	rows, err := a.projector.Store().Pool().Query(ctx, `
		SELECT c.value, coalesce(v.display_name, ''), v.props
		FROM graph.identity_claims c
		JOIN graph.entities e ON e.entity_id = c.entity_id AND e.merged_into IS NULL
		JOIN LATERAL (
			SELECT display_name, props FROM graph.entity_versions
			WHERE entity_id = c.entity_id AND upper_inf(observed) AND upper_inf(valid)
			ORDER BY version_id DESC LIMIT 1
		) v ON true
		WHERE c.namespace = $1`, graph.CatalogNamespace)
	if err != nil {
		return nil, fmt.Errorf("catalog: read entries: %w", err)
	}
	defer rows.Close()
	out := map[string]catalogEntry{}
	for rows.Next() {
		var (
			e     catalogEntry
			props []byte
		)
		if err := rows.Scan(&e.slug, &e.display, &props); err != nil {
			return nil, fmt.Errorf("catalog: scan entry: %w", err)
		}
		e.status, _ = propValues(props)[graph.CatalogStatusProp].(string)
		out[e.slug] = e
	}
	return out, rows.Err()
}

// serviceCandidates are the current SERVICE entities, with their names.
func (a *Attributor) serviceCandidates(ctx context.Context) ([]discoveryCandidate, error) {
	rows, err := a.projector.Store().Pool().Query(ctx, `
		SELECT e.entity_id, coalesce(v.display_name, '')
		FROM graph.entities e
		JOIN LATERAL (
			SELECT display_name FROM graph.entity_versions
			WHERE entity_id = e.entity_id AND upper_inf(observed) AND upper_inf(valid)
			ORDER BY version_id DESC LIMIT 1
		) v ON true
		WHERE e.merged_into IS NULL AND e.type = $1
		  AND NOT EXISTS (SELECT 1 FROM graph.identity_claims c WHERE c.entity_id = e.entity_id AND c.namespace = $2)
		ORDER BY e.entity_id`, string(graph.NodeTypeService), graph.CatalogNamespace)
	if err != nil {
		return nil, fmt.Errorf("catalog: list services: %w", err)
	}
	type service struct{ id, display string }
	var services []service
	for rows.Next() {
		var s service
		if err := rows.Scan(&s.id, &s.display); err != nil {
			rows.Close()
			return nil, fmt.Errorf("catalog: scan service: %w", err)
		}
		services = append(services, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: list services: %w", err)
	}

	out := make([]discoveryCandidate, 0, len(services))
	for _, s := range services {
		claims, names, err := a.entityNames(ctx, s.id)
		if err != nil {
			return nil, err
		}
		if len(claims) == 0 {
			continue
		}
		display := s.display
		if strings.TrimSpace(display) == "" {
			display = NameText(claims[0])
		}
		out = append(out, discoveryCandidate{
			slug: Slugify(display), display: display, aliases: claims, names: names,
		})
	}
	return out, nil
}

// entityNames reads an entity's identity claims, and every name it goes by (claims and correlation
// keys, with their environment).
func (a *Attributor) entityNames(ctx context.Context, entityID string) ([]graph.Ref, []Name, error) {
	rows, err := a.projector.Store().Pool().Query(ctx, `
		SELECT 'claim', namespace, value, coalesce(attributes->>$2, '') FROM graph.identity_claims
		WHERE entity_id IN (SELECT entity_id FROM graph.entities WHERE entity_id = $1 OR merged_into = $1)
		UNION
		SELECT 'key', namespace, value, coalesce(attributes->>$2, '') FROM graph.correlation_keys
		WHERE entity_id IN (SELECT entity_id FROM graph.entities WHERE entity_id = $1 OR merged_into = $1)
		ORDER BY 1, 2, 3, 4`, entityID, AttrEnvironment)
	if err != nil {
		return nil, nil, fmt.Errorf("catalog: read names of %s: %w", entityID, err)
	}
	defer rows.Close()
	var (
		claims []graph.Ref
		names  []Name
	)
	for rows.Next() {
		var kind string
		var n Name
		if err := rows.Scan(&kind, &n.Ref.Namespace, &n.Ref.Value, &n.Environment); err != nil {
			return nil, nil, fmt.Errorf("catalog: scan name: %w", err)
		}
		names = append(names, n)
		if kind == "claim" && !slices.Contains(claims, n.Ref) {
			claims = append(claims, n.Ref)
		}
	}
	return claims, sortNames(names), rows.Err()
}

// notionCandidates are the Notion values current pages carry.
func (a *Attributor) notionCandidates(ctx context.Context) ([]discoveryCandidate, error) {
	rows, err := a.projector.Store().Pool().Query(ctx, `
		SELECT v.props FROM graph.entities e
		JOIN graph.entity_versions v ON v.entity_id = e.entity_id
		WHERE e.merged_into IS NULL AND e.type = $1 AND upper_inf(v.observed) AND upper_inf(v.valid)
		ORDER BY e.entity_id, v.version_id`, string(graph.NodeTypeKnowledgeDoc))
	if err != nil {
		return nil, fmt.Errorf("catalog: list documents: %w", err)
	}
	defer rows.Close()
	seen := map[graph.Ref]bool{}
	var out []discoveryCandidate
	for rows.Next() {
		var props []byte
		if err := rows.Scan(&props); err != nil {
			return nil, fmt.Errorf("catalog: scan document: %w", err)
		}
		values := propValues(props)
		database, _ := values[graph.NotionDatabaseProp].(string)
		if database == "" {
			continue
		}
		for _, value := range stringList(values[graph.NotionValuesProp]) {
			ref := NotionValueRef(database, value)
			if value == "" || seen[ref] {
				continue
			}
			seen[ref] = true
			out = append(out, discoveryCandidate{
				slug: Slugify(value), display: value, aliases: []graph.Ref{ref}, names: []Name{{Ref: ref}},
			})
		}
	}
	return out, rows.Err()
}
