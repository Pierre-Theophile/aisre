// SPDX-License-Identifier: Apache-2.0

package projector

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/resolution"
)

// From a source's name for a thing to the graph's name for it (FR-036 to FR-040).
//
// Feeders never know canonical ids: they say `{namespace: "otel.service.name", value:
// "checkout"}` and the graph works out which entity that is. How it works it out is the
// decision this file makes, and it is deliberately blunt:
//
//	an upsert's Ref resolves to the entity whose canonical id is EntityID(namespace, value),
//	creating it if it does not exist yet; a foreign source's claim on that identifier does
//	*not* capture the ref.
//
// That looks like it throws away information — the Kubernetes feeder told us at 13:01 that its
// `shop/checkout` workload is also `otel.service.name=checkout`, so why does the telemetry
// feeder's `otel.service.name=checkout` at 13:03 not resolve straight to that workload? —
// but it is the whole point of constitution VI. Claims are stored *before* any merge; making
// them silently capture other sources' refs would merge two entities as a side effect of
// resolution, with no decision row, no rule id, no rationale and nothing to reverse. Instead
// the two entities exist side by side for as long as it takes an identity_claim event to run
// the certain rules, and the merge that follows is recorded, explained and auditable.
//
// Lookup (retraction targets, change targets) is the mirror image: it never creates, and it
// *does* consult foreign claims, because there the question is "does the graph know this name
// at all?" rather than "which entity does this name mint?".

// defaultNamespaceTypes is the type given to an entity created without one: an edge endpoint
// nothing has upserted yet, or the subject of an identity claim that arrived before its node.
//
// graph.entities.type is NOT NULL and NODE_TYPE_UNSPECIFIED is not a type, so something has to
// be written. The namespace is the only evidence available, and it is good evidence: a ref in
// `otel.service.name` is a service, one in `k8s.deployment` is a workload. Anything unknown is
// a third-party dependency, which is what an unrecognised callee is.
//
// A placeholder carries NO facet: facets are the types *sources asserted*, and nobody asserted
// this one. The first upsert_node for the entity supplies the first facet and, with it, the
// resolved type. That is what keeps an entity's type independent of whether its edge or its
// node event arrived first (FR-021).
var defaultNamespaceTypes = map[string]graph.NodeType{
	"otel.service.name": graph.NodeTypeService,
	"service.name":      graph.NodeTypeService,
	"k8s.deployment":    graph.NodeTypeWorkload,
	"k8s.statefulset":   graph.NodeTypeWorkload,
	"k8s.daemonset":     graph.NodeTypeWorkload,
	"k8s.job":           graph.NodeTypeWorkload,
	"k8s.cronjob":       graph.NodeTypeWorkload,
}

// PlaceholderProp marks an entity the projector minted to satisfy an edge endpoint or a claim
// subject before any source described it. Consumers can tell "we have never seen this thing
// described" from "this thing has no properties".
const PlaceholderProp = "sre.placeholder"

func namespaceDefaultType(namespace string) graph.NodeType {
	if typ, ok := defaultNamespaceTypes[namespace]; ok {
		return typ
	}
	return graph.NodeTypeThirdParty
}

// entityRow is graph.entities as the projector reads it.
type entityRow struct {
	entityID   string
	typ        graph.NodeType
	facets     []graph.NodeType
	mergedInto string
}

// resolveRef returns the entity id an upsert's Ref denotes, creating the entity on first
// sight. assertedType is the type the event asserts, or NodeTypeUnspecified when the event
// carries none (an identity claim, an edge endpoint), in which case the namespace supplies a
// provisional type and no facet is recorded.
func (p *Projector) resolveRef(ctx context.Context, tx pgx.Tx, ref graph.Ref, assertedType graph.NodeType, eventID, sourceID string, observedAt time.Time) (string, error) {
	canonical := graph.EntityID(ref.Namespace, ref.Value)
	row, found, err := p.entity(ctx, tx, canonical)
	if err != nil {
		return "", err
	}
	if found {
		return p.follow(ctx, tx, row.entityID)
	}
	if err := p.createEntity(ctx, tx, canonical, ref, assertedType, eventID, sourceID, observedAt); err != nil {
		return "", err
	}
	return canonical, nil
}

// lookupRef returns the entity a Ref denotes without creating anything. It answers the
// question retractions and change targets ask — "is this a thing the graph already knows?" —
// and follows both the canonical id and any claim another source made on the identifier.
func (p *Projector) lookupRef(ctx context.Context, tx pgx.Tx, ref graph.Ref) (string, bool, error) {
	canonical := graph.EntityID(ref.Namespace, ref.Value)
	if _, found, err := p.entity(ctx, tx, canonical); err != nil {
		return "", false, err
	} else if found {
		id, err := p.follow(ctx, tx, canonical)
		return id, err == nil, err
	}

	var entityID string
	err := tx.QueryRow(ctx, `
		SELECT c.entity_id
		FROM graph.identity_claims c
		JOIN log.events e ON e.event_id = c.event_id
		WHERE c.namespace = $1 AND c.value = $2
		ORDER BY e.appended_seq, c.claim_id
		LIMIT 1`, ref.Namespace, ref.Value).Scan(&entityID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("projector: look up %s: %w", ref, err)
	}
	id, err := p.follow(ctx, tx, entityID)
	return id, err == nil, err
}

// follow walks the merged_into chain to the surviving entity. Merges are recorded as redirects
// and never rewrite history, so every read has to follow them (FR-039).
func (p *Projector) follow(ctx context.Context, tx pgx.Tx, entityID string) (string, error) {
	current := entityID
	for hop := 0; hop < maxMergeChain; hop++ {
		row, found, err := p.entity(ctx, tx, current)
		if err != nil {
			return "", err
		}
		if !found || row.mergedInto == "" {
			return current, nil
		}
		current = row.mergedInto
	}
	return "", fmt.Errorf("projector: merge chain from %s is longer than %d hops", entityID, maxMergeChain)
}

// maxMergeChain bounds the redirect walk. The chain is acyclic by construction — a survivor is
// never merged into something that already points at it — so a longer chain means a corrupted
// projection, and failing loudly beats looping.
const maxMergeChain = 64

func (p *Projector) entity(ctx context.Context, tx pgx.Tx, entityID string) (*entityRow, bool, error) {
	var (
		row        entityRow
		typ        string
		facets     []string
		mergedInto *string
	)
	err := tx.QueryRow(ctx, `
		SELECT entity_id, type, facets, merged_into FROM graph.entities WHERE entity_id = $1`,
		entityID).Scan(&row.entityID, &typ, &facets, &mergedInto)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("projector: read entity %s: %w", entityID, err)
	}
	row.typ = graph.NodeType(typ)
	row.facets = facetsFromStrings(facets)
	if mergedInto != nil {
		row.mergedInto = *mergedInto
	}
	return &row, true, nil
}

// createEntity mints an entity and its primary claim. The primary claim is what makes the
// entity findable under the name it was created from, from the very first event (FR-036).
func (p *Projector) createEntity(ctx context.Context, tx pgx.Tx, entityID string, ref graph.Ref, assertedType graph.NodeType, eventID, sourceID string, observedAt time.Time) error {
	facets := []graph.NodeType{}
	typ := assertedType
	if typ == graph.NodeTypeUnspecified {
		typ = namespaceDefaultType(ref.Namespace)
	} else {
		facets = orderFacets([]graph.NodeType{assertedType})
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO graph.entities (entity_id, type, facets, created_by_event_id)
		VALUES ($1, $2, $3, $4)`,
		entityID, string(typ), facetStrings(facets), eventID); err != nil {
		return fmt.Errorf("projector: create entity %s (%s): %w", entityID, ref, err)
	}
	if err := p.storeClaim(ctx, tx, entityID, ref, nil, sourceID, eventID, observedAt); err != nil {
		return err
	}
	// A change that named this ref before anything described it has been waiting for exactly
	// this moment (attach.go, edge case "dangling change").
	return p.attachWaiting(ctx, tx, entityID, ref, eventID, observedAt)
}

// storeClaim records one identity assertion. Claims are unique per (namespace, value, source):
// the same source re-asserting the same identifier is the same claim whichever event carried
// it, so a re-delivery changes nothing here either (data-model.md graph.identity_claims).
func (p *Projector) storeClaim(ctx context.Context, tx pgx.Tx, entityID string, claim graph.Ref, attributes json.RawMessage, sourceID, eventID string, observedAt time.Time) error {
	if attributes == nil {
		attributes = json.RawMessage("{}")
	}
	claimID := graph.ClaimID(claim.Namespace, claim.Value, sourceID)
	if _, err := tx.Exec(ctx, `
		INSERT INTO graph.identity_claims
			(claim_id, entity_id, namespace, value, attributes, source_id, observed_at, event_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (claim_id) DO UPDATE SET attributes = EXCLUDED.attributes
			WHERE EXCLUDED.attributes <> '{}'::jsonb`,
		claimID, entityID, claim.Namespace, claim.Value, []byte(attributes), sourceID, observedAt.UTC(), eventID); err != nil {
		return fmt.Errorf("projector: store claim %s: %w", claim, err)
	}
	return nil
}

// storeCorrelation records one correlation key on one entity (004 T148).
//
// The insert differs from storeClaim's in exactly one way and it is the point: the id carries the
// ENTITY, so many entities may hold one key. A redelivered event still lands on the same row, which is
// what keeps double delivery a no-op.
//
// # Why the conflict target is the natural key and not the id
//
// A merge re-points these rows onto the survivor, exactly as it re-points identity claims — and unlike
// a claim id, a correlation id is a function of the entity, so a re-pointed row's id no longer matches
// its own inputs. It stays as it was minted, because it is the opaque id a recorded decision cites.
//
// That makes the id the weaker of the two available conflict targets. `ON CONFLICT (correlation_id)`
// would not see a re-pointed row, so an insert of the same (entity, namespace, value, source) would
// reach the unique constraint and be refused — turning the event into a failure rather than a no-op.
// The natural key is the constraint the table actually has, and conflicting on it cannot be defeated
// that way.
//
// No current path reaches the difference: a redelivered event short-circuits at the log's idempotency
// check and never gets here, and a replay from empty re-derives the same entity ids in the same order.
// So this is a guard rather than a fix for a live defect — chosen because it costs nothing and the
// alternative's failure mode is a refused event, which is the expensive kind to diagnose.
func (p *Projector) storeCorrelation(ctx context.Context, tx pgx.Tx, entityID string, key graph.Ref, attributes json.RawMessage, sourceID, eventID string, observedAt time.Time) error {
	if attributes == nil {
		attributes = json.RawMessage("{}")
	}
	correlationID := graph.CorrelationID(entityID, key.Namespace, key.Value, sourceID)
	if _, err := tx.Exec(ctx, `
		INSERT INTO graph.correlation_keys
			(correlation_id, entity_id, namespace, value, attributes, source_id, observed_at, event_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (entity_id, namespace, value, source_id)
			DO UPDATE SET attributes = EXCLUDED.attributes
			WHERE EXCLUDED.attributes <> '{}'::jsonb`,
		correlationID, entityID, key.Namespace, key.Value, []byte(attributes), sourceID,
		observedAt.UTC(), eventID); err != nil {
		return fmt.Errorf("projector: store correlation %s on %s: %w", key, entityID, err)
	}
	return nil
}

// repointCorrelations moves the merged entity's correlation keys onto the survivor (004 T148).
//
// The counterpart of the identity-claim re-point a line above, and it needs two statements where that
// needs one, because a correlation's uniqueness includes the entity. If both sides already carry the
// same (namespace, value, source) — two changes one source described identically, merged by some other
// rule — a plain UPDATE would collide with the survivor's own row.
//
// So the rows that would collide are deleted, and no fact is lost by deleting them: what such a row
// records is "this source said this entity carries this value", the two entities are now one entity,
// and the survivor's row records exactly that. The rows that have no twin are moved.
//
// Deleting a row does drop an id a decision may cite. That is why the delete is narrowed to a twin
// existing: the audit's question is what evidence stood behind a merge, and the evidence is still
// there under the twin's id. A row with no twin is never deleted.
func (p *Projector) repointCorrelations(ctx context.Context, tx pgx.Tx, merged, survivor string) error {
	if _, err := tx.Exec(ctx, `
		DELETE FROM graph.correlation_keys k
		WHERE k.entity_id = $1
		  AND EXISTS (
			SELECT 1 FROM graph.correlation_keys s
			WHERE s.entity_id = $2 AND s.namespace = k.namespace
			  AND s.value = k.value AND s.source_id = k.source_id)`, merged, survivor); err != nil {
		return fmt.Errorf("projector: drop duplicate correlations of %s: %w", merged, err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE graph.correlation_keys SET entity_id = $2 WHERE entity_id = $1`, merged, survivor); err != nil {
		return fmt.Errorf("projector: re-point correlations of %s: %w", merged, err)
	}
	return nil
}

// correlationColumns and correlationJoins mirror the claim ones: the log row for the append position
// that decides which of two merged entities survives, and the entity for its resolved type.
const correlationColumns = `
	k.correlation_id, k.entity_id, k.namespace, k.value, k.attributes, k.source_id, k.event_id,
	k.observed_at, e.appended_seq, coalesce(ent.type, '')`

const correlationJoins = `
	JOIN log.events e ON e.event_id = k.event_id
	LEFT JOIN graph.entities ent ON ent.entity_id = k.entity_id`

// correlation reads one stored correlation back, for the rule runner.
func (p *Projector) correlation(ctx context.Context, tx pgx.Tx, correlationID string) (resolution.Correlation, bool, error) {
	rows, err := tx.Query(ctx, `
		SELECT `+correlationColumns+`
		FROM graph.correlation_keys k`+correlationJoins+`
		WHERE k.correlation_id = $1`, correlationID)
	if err != nil {
		return resolution.Correlation{}, false, fmt.Errorf("projector: read correlation %s: %w", correlationID, err)
	}
	keys, err := scanCorrelations(rows)
	if err != nil || len(keys) == 0 {
		return resolution.Correlation{}, false, err
	}
	return keys[0], true, nil
}

func scanCorrelations(rows pgx.Rows) ([]resolution.Correlation, error) {
	defer rows.Close()
	var out []resolution.Correlation
	for rows.Next() {
		var (
			key        resolution.Correlation
			attributes []byte
			entityType string
		)
		if err := rows.Scan(&key.CorrelationID, &key.EntityID, &key.Namespace, &key.Value,
			&attributes, &key.SourceID, &key.EventID, &key.ObservedAt, &key.AppendedSeq,
			&entityType); err != nil {
			return nil, fmt.Errorf("projector: scan correlation: %w", err)
		}
		key.EntityType = graph.NodeType(entityType)
		key.Attributes = decodeAttributes(attributes)
		out = append(out, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("projector: read correlations: %w", err)
	}
	return out, nil
}

// CorrelatedWith returns every entity carrying one correlation key (004 T148).
//
// Ordered by entity id so a rule's candidate list — and therefore the decisions it records — does not
// depend on the order rows happen to come back in.
func (s claimStore) CorrelatedWith(ctx context.Context, namespace, value string) ([]resolution.Correlation, error) {
	rows, err := s.tx.Query(ctx, `
		SELECT `+correlationColumns+`
		FROM graph.correlation_keys k`+correlationJoins+`
		WHERE k.namespace = $1 AND k.value = $2
		ORDER BY k.entity_id, k.source_id`, namespace, value)
	if err != nil {
		return nil, fmt.Errorf("projector: correlations for %s=%s: %w", namespace, value, err)
	}
	return scanCorrelations(rows)
}

// addFacet records a type a source asserted for an entity and returns whether the entity's
// facet set changed. A change of facets is a change of the resolved type, which is a new
// version of every current version of the entity (ADR-0001 D7, constitution II).
func (p *Projector) addFacet(ctx context.Context, tx pgx.Tx, entityID string, typ graph.NodeType) (bool, error) {
	if typ == graph.NodeTypeUnspecified {
		return false, nil
	}
	row, found, err := p.entity(ctx, tx, entityID)
	if err != nil || !found {
		return false, err
	}
	if slices.Contains(row.facets, typ) {
		return false, nil
	}
	facets := orderFacets(append(slices.Clone(row.facets), typ))
	return true, p.setFacets(ctx, tx, entityID, facets)
}

func (p *Projector) setFacets(ctx context.Context, tx pgx.Tx, entityID string, facets []graph.NodeType) error {
	if _, err := tx.Exec(ctx, `
		UPDATE graph.entities SET type = $2, facets = $3 WHERE entity_id = $1`,
		entityID, string(resolveType(facets)), facetStrings(facets)); err != nil {
		return fmt.Errorf("projector: set facets of %s: %w", entityID, err)
	}
	return nil
}

// ---------- the certain-rule pass ----------

// claimStore reads graph.identity_claims for the rule registry, inside the transaction the
// event is being applied in, so a rule sees the claim that triggered it.
type claimStore struct {
	tx pgx.Tx
}

const claimColumns = `
	c.claim_id, c.entity_id, c.namespace, c.value, c.attributes, c.source_id, c.event_id,
	c.observed_at, e.appended_seq, coalesce(ent.type, '')`

// claimJoins are the joins claimColumns needs: the log row, for the append position that
// decides which of two merged entities survives, and the entity, for the resolved type the
// probable rules' guard reads (research §10).
const claimJoins = `
	JOIN log.events e ON e.event_id = c.event_id
	LEFT JOIN graph.entities ent ON ent.entity_id = c.entity_id`

func (s claimStore) ClaimsFor(ctx context.Context, namespace, value string) ([]resolution.Claim, error) {
	rows, err := s.tx.Query(ctx, `
		SELECT `+claimColumns+`
		FROM graph.identity_claims c`+claimJoins+`
		WHERE c.namespace = $1 AND c.value = $2
		ORDER BY e.appended_seq, c.claim_id`, namespace, value)
	if err != nil {
		return nil, fmt.Errorf("projector: read claims for %s=%s: %w", namespace, value, err)
	}
	return scanClaims(rows)
}

func (s claimStore) ClaimsMatchingAttributes(ctx context.Context, namespace string, attrs map[string]string) ([]resolution.Claim, error) {
	filter, err := json.Marshal(attrs)
	if err != nil {
		return nil, fmt.Errorf("projector: encode claim attribute filter: %w", err)
	}
	rows, err := s.tx.Query(ctx, `
		SELECT `+claimColumns+`
		FROM graph.identity_claims c`+claimJoins+`
		WHERE c.namespace = $1 AND c.attributes @> $2::jsonb
		ORDER BY e.appended_seq, c.claim_id`, namespace, filter)
	if err != nil {
		return nil, fmt.Errorf("projector: read claims in %s by attributes: %w", namespace, err)
	}
	return scanClaims(rows)
}

// ClaimsInNamespaces returns every claim in the given identifier namespaces, in log order.
//
// It is a scan, and deliberately so: the probable rules compare *normalized* names, which no
// index on `value` can answer. The candidate set is bounded by the number of distinct service
// and workload names a deployment has, not by the size of the graph, and the rules run only on
// identity_claim events. Should it ever become the bottleneck, the fix is to store the
// normalized name in a column of its own, not to weaken the rule.
func (s claimStore) ClaimsInNamespaces(ctx context.Context, namespaces []string) ([]resolution.Claim, error) {
	rows, err := s.tx.Query(ctx, `
		SELECT `+claimColumns+`
		FROM graph.identity_claims c`+claimJoins+`
		WHERE c.namespace = ANY($1)
		ORDER BY e.appended_seq, c.claim_id`, namespaces)
	if err != nil {
		return nil, fmt.Errorf("projector: read claims in %v: %w", namespaces, err)
	}
	return scanClaims(rows)
}

// SharedOwner reports whether a and b are owned by one and the same owner entity, which is
// rule P3's corroboration (research §10).
//
// "Owned by" is read as it is stored: an `owned_by` edge whose observed interval is still open.
// Endpoints are resolved through the merge redirects, because an owner the graph has since
// merged is still the same owner. Valid time is not filtered: ownership that ended yesterday is
// still evidence that two things belong together, and P3 only ever produces a suggestion.
func (s claimStore) SharedOwner(ctx context.Context, a, b string) (bool, error) {
	if a == "" || b == "" || a == b {
		return false, nil
	}
	var shared bool
	err := s.tx.QueryRow(ctx, `
		WITH owners AS (
			SELECT e.src_id AS owned, coalesce(t.merged_into, e.dst_id) AS owner
			FROM graph.edge_versions e
			LEFT JOIN graph.entities t ON t.entity_id = e.dst_id
			WHERE e.type = $3 AND upper_inf(e.observed) AND e.src_id = ANY(ARRAY[$1, $2])
		)
		SELECT EXISTS (
			SELECT 1 FROM owners x JOIN owners y ON x.owner = y.owner
			WHERE x.owned = $1 AND y.owned = $2)`,
		a, b, string(graph.EdgeTypeOwnedBy)).Scan(&shared)
	if err != nil {
		return false, fmt.Errorf("projector: read shared owner of %s/%s: %w", a, b, err)
	}
	return shared, nil
}

// ChangeTargets returns the entities a change was applied to, after merge redirects.
//
// The `changed_by` edge points FROM the target TO the change (observe_change.go's linkChange), so
// the targets are the edge's sources. Redirects are followed on the source side for the same reason
// scanClaims follows them on a claim: a rule comparing raw ids would miss two targets that
// resolution has already made one, which is exactly the case C8 exists to catch.
func (s claimStore) ChangeTargets(ctx context.Context, changeEntityID string) ([]string, error) {
	if changeEntityID == "" {
		return nil, nil
	}
	rows, err := s.tx.Query(ctx, `
		SELECT DISTINCT coalesce(t.merged_into, e.src_id)
		FROM graph.edge_versions e
		LEFT JOIN graph.entities t ON t.entity_id = e.src_id
		WHERE e.type = $2 AND upper_inf(e.observed) AND e.dst_id = $1
		ORDER BY 1`, changeEntityID, string(graph.EdgeTypeChangedBy))
	if err != nil {
		return nil, fmt.Errorf("projector: read targets of change %s: %w", changeEntityID, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var target string
		if err := rows.Scan(&target); err != nil {
			return nil, fmt.Errorf("projector: scan target of change %s: %w", changeEntityID, err)
		}
		out = append(out, target)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("projector: read targets of change %s: %w", changeEntityID, err)
	}
	return out, nil
}

func scanClaims(rows pgx.Rows) ([]resolution.Claim, error) {
	defer rows.Close()
	var claims []resolution.Claim
	for rows.Next() {
		var (
			claim      resolution.Claim
			attributes []byte
		)
		var entityType string
		if err := rows.Scan(&claim.ClaimID, &claim.EntityID, &claim.Namespace, &claim.Value,
			&attributes, &claim.SourceID, &claim.EventID, &claim.ObservedAt, &claim.AppendedSeq,
			&entityType); err != nil {
			return nil, fmt.Errorf("projector: scan claim: %w", err)
		}
		claim.EntityType = graph.NodeType(entityType)
		claim.Attributes = decodeAttributes(attributes)
		claims = append(claims, claim)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("projector: read claims: %w", err)
	}
	return claims, nil
}

// decodeAttributes flattens a claim's attributes to strings. Rules compare identifiers, and a
// port number written as 5432 by one source and "5432" by another is the same identifier.
func decodeAttributes(raw []byte) map[string]string {
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return map[string]string{}
	}
	out := make(map[string]string, len(decoded))
	for key, value := range decoded {
		switch v := value.(type) {
		case string:
			out[key] = v
		case bool:
			out[key] = strconv.FormatBool(v)
		case float64:
			out[key] = strconv.FormatFloat(v, 'f', -1, 64)
		case nil:
			out[key] = ""
		default:
			encoded, err := json.Marshal(v)
			if err == nil {
				out[key] = string(encoded)
			}
		}
	}
	return out
}

// runResolutionRules evaluates the published rules against a freshly stored claim, merges what
// the certain ones find and files what the probable ones find as suggestions.
//
// The two halves are kept apart here rather than inside a rule, so that "a probable rule never
// merges" is a property of this function and can be read in one place (FR-037, ADR-0001 D6,
// SC-007). resolution.Evaluate already refuses to report a pair as probable when a certain rule
// matched it, so a pair is either merged or suggested, never both.
func (p *Projector) runResolutionRules(ctx context.Context, tx pgx.Tx, claimID string, eventID string, observedAt time.Time, principal string) error {
	claim, found, err := p.claim(ctx, tx, claimID)
	if err != nil || !found {
		return err
	}
	matches, err := resolution.Evaluate(ctx, claimStore{tx: tx}, claim)
	if err != nil {
		return err
	}
	return p.applyMatches(ctx, tx, matches, eventID, observedAt, principal)
}

// runCorrelationRules evaluates the rules a newly stored correlation key triggers (004 T148).
//
// Separate from runResolutionRules because the trigger is a different kind, and sharing applyMatches
// because what happens to a match afterwards is not: a merge is a merge whichever rule proposed it, and
// two copies of the redirect-then-absorb ordering would be two places to get it wrong.
func (p *Projector) runCorrelationRules(ctx context.Context, tx pgx.Tx, entityID string, key graph.Ref, sourceID, eventID string, observedAt time.Time, principal string) error {
	return p.runCorrelationRulesByID(ctx, tx,
		graph.CorrelationID(entityID, key.Namespace, key.Value, sourceID), eventID, observedAt, principal)
}

// runCorrelationRulesByID is the same from an id, which is what the re-trigger drain holds: it queued
// the key's id when the graph moved under it and has no event to recompute one from (retrigger.go).
//
// A key that is gone is not an error. A merge deletes a correlation row whose twin the survivor already
// carries (repointCorrelations), so a queued id can name a row that no longer exists by the time the
// drain reaches it — and the fact it recorded is on the survivor, where the merge that deleted it
// queued its own re-evaluation.
func (p *Projector) runCorrelationRulesByID(ctx context.Context, tx pgx.Tx, correlationID, eventID string, observedAt time.Time, principal string) error {
	stored, found, err := p.correlation(ctx, tx, correlationID)
	if err != nil || !found {
		return err
	}
	matches, err := resolution.EvaluateCorrelation(ctx, claimStore{tx: tx}, stored)
	if err != nil {
		return err
	}
	return p.applyMatches(ctx, tx, matches, eventID, observedAt, principal)
}

// applyMatches records the suggestions, applies the merges, and re-triggers what the merges moved.
//
// The ordering here is load-bearing and documented where it happens: redirects first, then one absorb
// per survivor, then edges once for the whole pass.
func (p *Projector) applyMatches(ctx context.Context, tx pgx.Tx, matches []resolution.Match, eventID string, observedAt time.Time, principal string) error {
	certain := make([]resolution.Match, 0, len(matches))
	for _, match := range matches {
		if match.Certain {
			certain = append(certain, match)
			continue
		}
		if err := p.recordSuggestion(ctx, tx, match, eventID, observedAt); err != nil {
			return err
		}
	}
	matches = certain

	// Redirects first, then one absorb per survivor. Doing them in two passes matters: a claim
	// can trigger two merges into the same survivor, and absorbing twice in one transaction
	// would try to close an observed interval that was opened an instant earlier in the same
	// transaction — which close_observed rightly refuses.
	var mergedIDs []string
	for _, match := range matches {
		merged, err := p.applyMatch(ctx, tx, match, eventID, observedAt, principal)
		if err != nil {
			return err
		}
		if merged != "" {
			mergedIDs = append(mergedIDs, merged)
		}
	}

	bySurvivor := map[string][]string{}
	for _, merged := range mergedIDs {
		survivor, err := p.follow(ctx, tx, merged)
		if err != nil {
			return err
		}
		bySurvivor[survivor] = append(bySurvivor[survivor], merged)
	}
	for _, survivor := range sortedKeys(bySurvivor) {
		if err := p.absorb(ctx, tx, survivor, bySurvivor[survivor], eventID, observedAt); err != nil {
			return err
		}
	}

	// Edges are absorbed once for the whole pass rather than once per survivor: one claim can
	// merge two entities at once, and an edge between the two losers has to be re-pointed once,
	// straight to its final endpoints (absorbEdges).
	slices.Sort(mergedIDs)
	if err := p.absorbEdges(ctx, tx, slices.Compact(mergedIDs), eventID, observedAt); err != nil {
		return err
	}

	// The merges this pass made can have moved another change's target set: two targets that were
	// distinct are now one entity, so an intersection C8 found empty is not. Nothing else would notice
	// — rules run when their evidence is stored — so the deploy keys on the changes attached to either
	// side are re-evaluated here (T137, contracts/deploy-claims.md §2.1).
	if len(mergedIDs) == 0 {
		return nil
	}
	affected := slices.Clone(mergedIDs)
	affected = append(affected, sortedKeys(bySurvivor)...)
	slices.Sort(affected)
	affected = slices.Compact(affected)
	changes, err := p.changesTargeting(ctx, tx, affected)
	if err != nil {
		return err
	}
	// The merged entities themselves as well as the changes attached to them, because a merged entity
	// may BE a change: when two change observations become one, the survivor holds the union of both
	// target sets, so its remaining deploy keys face a larger intersection than the ones just
	// evaluated. `changesTargeting` cannot find those — a change does not target itself — and
	// queueDeployKeys is a no-op for an entity carrying no deploy key, so naming them costs one
	// indexed query on every other kind of merge.
	requeue := append(changes, affected...)
	slices.Sort(requeue)
	return p.queueDeployKeys(ctx, tx, slices.Compact(requeue),
		"two target identities merged, so an intersection that was empty may not be", eventID, observedAt)
}

func (p *Projector) claim(ctx context.Context, tx pgx.Tx, claimID string) (resolution.Claim, bool, error) {
	rows, err := tx.Query(ctx, `
		SELECT `+claimColumns+`
		FROM graph.identity_claims c`+claimJoins+`
		WHERE c.claim_id = $1`, claimID)
	if err != nil {
		return resolution.Claim{}, false, fmt.Errorf("projector: read claim %s: %w", claimID, err)
	}
	claims, err := scanClaims(rows)
	if err != nil || len(claims) == 0 {
		return resolution.Claim{}, false, err
	}
	return claims[0], true, nil
}

// applyMatch turns a certain match into a merge, unless a human has said no.
//
// A human decision outranks any automated rule regardless of score (FR-040). A recorded
// `reject` on the pair therefore blocks the merge permanently and the disagreement is surfaced
// as a conflict suggestion for review rather than applied. US6 owns the human side; this is
// the hook it plugs into.
func (p *Projector) applyMatch(ctx context.Context, tx pgx.Tx, match resolution.Match, eventID string, observedAt time.Time, principal string) (string, error) {
	a, err := p.follow(ctx, tx, match.EntityA)
	if err != nil {
		return "", err
	}
	b, err := p.follow(ctx, tx, match.EntityB)
	if err != nil {
		return "", err
	}
	if a == b {
		return "", nil
	}

	rejected, err := p.pairRejected(ctx, tx, a, b)
	if err != nil {
		return "", err
	}
	if rejected {
		return "", p.recordConflict(ctx, tx, match, a, b, observedAt,
			"blocked by a human rejection of this pair")
	}

	// A person who confirmed that B is A also said that B is nothing else. A rule that now
	// wants to merge that entity into a third one disagrees with them, and FR-040 says a
	// disagreement is surfaced, never applied.
	pinned, err := p.eitherPinned(ctx, tx, a, b)
	if err != nil {
		return "", err
	}
	if pinned {
		return "", p.recordConflict(ctx, tx, match, a, b, observedAt,
			"blocked by a human merge decision pinning this entity")
	}

	return p.merge(ctx, tx, match, a, b, eventID, observedAt, mergeOptions{
		kind:      decisionAutoMerge,
		principal: principal,
	})
}

// pairRejected reports whether a human has rejected this pair. Rejections are permanent until
// superseded by another decision (research §10, "human decision > certain rule").
func (p *Projector) pairRejected(ctx context.Context, tx pgx.Tx, a, b string) (bool, error) {
	var rejected bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM graph.resolution_decisions
			WHERE kind = 'reject' AND superseded_by IS NULL
			  AND ((surviving_id = $1 AND merged_id = $2) OR (surviving_id = $2 AND merged_id = $1)))`,
		a, b).Scan(&rejected)
	if err != nil {
		return false, fmt.Errorf("projector: check rejected pair %s/%s: %w", a, b, err)
	}
	return rejected, nil
}

// recordConflict surfaces an automated rule that disagrees with a human decision, without
// applying it (FR-040, US6 acceptance scenario 3).
func (p *Projector) recordConflict(ctx context.Context, tx pgx.Tx, match resolution.Match, a, b string, observedAt time.Time, reason string) error {
	lo, hi := orderedPair(a, b)
	_, err := tx.Exec(ctx, `
		INSERT INTO graph.suggestions (pair_key, entity_a, entity_b, rule_id, score, rationale, created_at, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'conflict')
		ON CONFLICT (pair_key) DO UPDATE SET
			status = 'conflict', rule_id = EXCLUDED.rule_id, score = EXCLUDED.score,
			rationale = EXCLUDED.rationale`,
		graph.PairKey(lo, hi), lo, hi, match.RuleID, match.Score,
		reason+": "+match.Rationale, observedAt.UTC())
	if err != nil {
		return fmt.Errorf("projector: record conflict for %s/%s: %w", lo, hi, err)
	}
	return nil
}

// merge makes two entities one (FR-038, FR-039).
//
// The survivor is the entity that appeared in the log first — the lower minimum appended_seq
// over its claims, lexicographically smaller id on a tie. Log position is the only tie-break
// that is a pure function of the log, which is what makes a replay reproduce the same survivor
// and therefore the same ids (research §4).
//
// Nothing is deleted: the merged-away entity keeps its id, its versions and its history, and
// gains a redirect. Its claims are re-pointed so that every name it was known by resolves to
// the survivor.
func (p *Projector) merge(ctx context.Context, tx pgx.Tx, match resolution.Match, a, b, eventID string, observedAt time.Time, opts mergeOptions) (string, error) {
	survivor, merged, err := p.chooseSurvivor(ctx, tx, a, b)
	if err != nil {
		return "", err
	}

	if _, err := tx.Exec(ctx,
		`UPDATE graph.entities SET merged_into = $2 WHERE entity_id = $1`, merged, survivor); err != nil {
		return "", fmt.Errorf("projector: redirect %s to %s: %w", merged, survivor, err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE graph.identity_claims SET entity_id = $2 WHERE entity_id = $1`, merged, survivor); err != nil {
		return "", fmt.Errorf("projector: re-point claims of %s: %w", merged, err)
	}
	if err := p.repointCorrelations(ctx, tx, merged, survivor); err != nil {
		return "", err
	}

	pairKey := graph.PairKey(survivor, merged)
	if err := p.recordDecision(ctx, tx, decisionRecord{
		kind:        opts.kind,
		survivingID: survivor,
		mergedID:    merged,
		ruleID:      match.RuleID,
		score:       match.Score,
		rationale:   match.Rationale,
		claimIDs:    match.SupportingClaimIDs,
		principal:   opts.principal,
		observedAt:  observedAt,
		eventID:     eventID,
	}); err != nil {
		return "", err
	}

	// The suggestion, if a probable rule had filed one, is now decided. A rule closing it says
	// `confirmed` too: the pair the graph proposed did turn out to be one entity, and that is
	// what the calibration report measures a probable score against (constitution V).
	status := statusConfirmed
	if _, err := tx.Exec(ctx, `
		UPDATE graph.suggestions SET status = $2, decision_id = $3
		WHERE pair_key = $1 AND status IN ('pending', 'conflict')`,
		pairKey, status, decisionID(opts.kind, pairKey, eventID)); err != nil {
		return "", fmt.Errorf("projector: close suggestion %s: %w", pairKey, err)
	}
	return merged, nil
}

// mergeOptions is what a merge needs beyond the match: which kind of decision produced it and,
// for a human one, who made it (FR-041).
type mergeOptions struct {
	// kind is the decision kind recorded: auto_merge, confirm or manual_merge.
	kind string
	// principal is the authenticated individual behind a human decision, empty for a rule.
	principal string
}

// chooseSurvivor implements the published rule: first seen in the log wins.
func (p *Projector) chooseSurvivor(ctx context.Context, tx pgx.Tx, a, b string) (survivor, merged string, err error) {
	firstSeen := func(entityID string) (int64, error) {
		var seq *int64
		err := tx.QueryRow(ctx, `
			SELECT min(e.appended_seq)
			FROM graph.identity_claims c
			JOIN log.events e ON e.event_id = c.event_id
			WHERE c.entity_id = $1`, entityID).Scan(&seq)
		if err != nil {
			return 0, fmt.Errorf("projector: read first sighting of %s: %w", entityID, err)
		}
		if seq == nil {
			// No claim carries it (it cannot normally happen: every entity is created with a
			// primary claim). Sort it last so a claimed entity always wins.
			return int64(1)<<62 - 1, nil
		}
		return *seq, nil
	}
	seqA, err := firstSeen(a)
	if err != nil {
		return "", "", err
	}
	seqB, err := firstSeen(b)
	if err != nil {
		return "", "", err
	}
	switch {
	case seqA < seqB:
		return a, b, nil
	case seqB < seqA:
		return b, a, nil
	case a <= b:
		return a, b, nil
	default:
		return b, a, nil
	}
}

// absorb folds merged entities into their survivor (FR-012, FR-038, ADR-0001 D7).
//
// Two things happen, and they have to happen together:
//
//  1. the survivor takes the union of the facets, so a Kubernetes workload merged with the
//     OpenTelemetry service running on it resolves to SERVICE while keeping WORKLOAD;
//  2. the assertions that were producing the merged entity's current versions are re-applied
//     to the survivor, and the merged entity's rows have their observed intervals closed.
//
// Point 2 is what makes a merge order-independent. A source's facts about an entity have to end
// up in the same place whether that source's upsert arrived before the merge (in which case it
// landed on its own entity, which is now being absorbed) or after it (in which case it landed
// on the survivor directly). Without the fold, shuffling a fixture within its declared
// reordering window would produce one version series or two depending on arrival order, which
// FR-021 forbids.
//
// Nothing is deleted. The merged entity keeps its id, its closed version rows stay queryable as
// of any observed instant inside their interval (FR-014), and its redirect makes every name it
// was known by resolve to the survivor (FR-039).
func (p *Projector) absorb(ctx context.Context, tx pgx.Tx, survivor string, mergedIDs []string, eventID string, observedAt time.Time) error {
	survivorRow, found, err := p.entity(ctx, tx, survivor)
	if err != nil || !found {
		return err
	}

	facets := slices.Clone(survivorRow.facets)
	survivorRows, err := p.currentNodeRows(ctx, tx, survivor)
	if err != nil {
		return err
	}
	eventIDs := eventIDsOf(survivorRows)

	mergedRows := map[string][]*nodeRow{}
	for _, merged := range mergedIDs {
		row, mergedFound, err := p.entity(ctx, tx, merged)
		if err != nil {
			return err
		}
		if mergedFound {
			facets = append(facets, row.facets...)
		}
		rows, err := p.currentNodeRows(ctx, tx, merged)
		if err != nil {
			return err
		}
		mergedRows[merged] = rows
		eventIDs = append(eventIDs, eventIDsOf(rows)...)
	}
	facets = orderFacets(facets)

	// A change is a record, not a segmented node: its rows come from observe_change, which the node
	// fold below cannot read, so re-folding them here used to write the survivor with no change at
	// all. It is re-folded from its observations instead (change_fold.go).
	mergedAll := make([][]*nodeRow, 0, len(mergedIDs)+1)
	mergedAll = append(mergedAll, survivorRows)
	for _, merged := range mergedIDs {
		mergedAll = append(mergedAll, mergedRows[merged])
	}
	if hasChangeRecord(mergedAll...) {
		return p.absorbChange(ctx, tx, survivor, survivorRows, mergedIDs, mergedRows, eventIDs, facets,
			survivorRow.facets, eventID, observedAt)
	}

	assertions, err := p.nodeAssertions(ctx, tx, eventIDs)
	if err != nil {
		return err
	}

	// Close the absorbed rows before re-applying what produced them, so the survivor and the
	// merged entity never both claim to be current.
	var absorbed []segment
	for _, merged := range mergedIDs {
		for _, seg := range segmentsOf(mergedRows[merged], assertions) {
			absorbed = append(absorbed, seg)
			if err := closeObserved(ctx, tx, "entity_versions", seg.versionID, observedAt, eventID); err != nil {
				return err
			}
		}
	}

	if !slices.Equal(facets, survivorRow.facets) {
		if err := p.setFacets(ctx, tx, survivor, facets); err != nil {
			return err
		}
	}

	planned := segmentsOf(survivorRows, assertions)
	for _, item := range absorbedAssertions(absorbed, assertions) {
		planned = planUpsert(planned, item.sourceID, item.eventID, item.assertedAt,
			item.fromUnknown, assertedAtFunc(assertions), nodeContentEqual(assertions, facets))
	}
	return p.writeNodeSegments(ctx, tx, survivor, survivorRows, planned, assertions, facets, eventID, observedAt)
}

// absorbChange is absorb for change entities: the merged rows are closed and the survivor's record
// becomes the fold of every observation behind any of them, so a C8 merge keeps what each source
// said about the rollout instead of whichever was applied last — or, as it did before, nothing.
func (p *Projector) absorbChange(ctx context.Context, tx pgx.Tx, survivor string, survivorRows []*nodeRow, mergedIDs []string, mergedRows map[string][]*nodeRow, eventIDs []string, facets, survivorFacets []graph.NodeType, eventID string, observedAt time.Time) error {
	for _, merged := range mergedIDs {
		for _, row := range mergedRows[merged] {
			if err := closeObserved(ctx, tx, "entity_versions", row.versionID, observedAt, eventID); err != nil {
				return err
			}
		}
	}
	if !slices.Equal(facets, survivorFacets) {
		if err := p.setFacets(ctx, tx, survivor, facets); err != nil {
			return err
		}
	}
	observations, err := p.changeObservations(ctx, tx, eventIDs)
	if err != nil {
		return err
	}
	return p.writeChangeRecord(ctx, tx, survivor, survivorRows, observations, facets, eventID, observedAt)
}

// absorbedAssertions lists the assertions behind the absorbed versions, once each, ordered by
// valid instant and then by source and event id so the fold is deterministic on replay.
func absorbedAssertions(segments []segment, assertions map[string]nodeAssertion) []nodeAssertion {
	seen := map[string]bool{}
	var out []nodeAssertion
	for _, seg := range segments {
		// Restatements too: coalescing folds a later same-content assertion into the segment it
		// agrees with, and remembers it here so that its INSTANT survives (segments.go). A merge that
		// re-applied only the primary assertions dropped that instant for the absorbed side alone, so
		// whether it survived depended on which side of the merge it happened to have landed on —
		// arrival order, which gcp-cross-source-merge-01's shuffle found (003 T184).
		for _, eventIDs := range []map[string]string{seg.assertions, seg.restatements} {
			for _, eventID := range eventIDs {
				if seen[eventID] {
					continue
				}
				if _, known := assertions[eventID]; !known {
					continue
				}
				seen[eventID] = true
				out = append(out, assertions[eventID])
			}
		}
	}
	slices.SortFunc(out, func(a, b nodeAssertion) int {
		return cmpAssertion(a.assertedAt, a.sourceID, a.eventID, b.assertedAt, b.sourceID, b.eventID)
	})
	return out
}

// ---------- absorbing edges ----------

// absorbEdges re-points the relationships a merge left naming an entity that no longer exists
// in its own right (FR-038, FR-039, constitution II).
//
// This is point 2 of absorb applied to edges, and leaving it out is a defect no constraint
// catches. `payments → payments-db`, asserted while `payments` was still two entities, is stored
// under the OpenTelemetry id; the same relationship re-asserted after the merge resolves to the
// survivor and is stored under *that* id. The exclusion constraint keys on the raw endpoint
// columns, so it sees two unrelated series and fires on neither — and every reader, which
// resolves endpoints through `merged_into` as it reads, then sees one relationship twice, at one
// instant, in two weight classes. An impact walk takes whichever it meets first.
//
// So the stale rows have their observed intervals closed with the merge event, and what produced
// them is re-applied to the canonical endpoints. Assertions go back through the ordinary edge
// segmentation, and "ordinary" is the whole point: an assertion already sitting on the canonical
// endpoints and one being absorbed meet under the rules any two assertions meet under — identical
// content coalesces into one row, a different weight class becomes a valid-time split decided by
// the primary-assertion rule, never a second concurrent row. Rows no assertion stands behind —
// the `changed_by` edges attach.go writes directly — are carried over as they are, because there
// is nothing to re-derive them from and dropping them would lose the link between a change and
// what it changed.
//
// Nothing is deleted. The closed rows keep their ids and stay queryable as of any observed
// instant inside their interval, which is what makes "as known at 13:04" go on returning the
// graph as it was before the merge (FR-014).
//
// Caveat, shared with the node fold: only the assertions are re-applied, so a valid-time bound a
// retraction put on an absorbed row is not carried over with it. Feeders emit a retraction after
// the facts it ends and outside their declared reordering window (research §11), so a merge of an
// entity whose edges have already been retracted is not a case the fixtures reach; when it has
// to be, the retraction belongs here too.
func (p *Projector) absorbEdges(ctx context.Context, tx pgx.Tx, mergedIDs []string, eventID string, observedAt time.Time) error {
	if len(mergedIDs) == 0 {
		return nil
	}
	stale, err := p.staleEdgeKeys(ctx, tx, mergedIDs)
	if err != nil {
		return err
	}

	folded := map[edgeKey]*foldedEdge{}
	var order []edgeKey
	for _, key := range stale {
		rows, err := p.currentEdgeRows(ctx, tx, key)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			continue
		}
		fresh, err := p.openedAt(ctx, tx, rows, observedAt)
		if err != nil {
			return err
		}
		if fresh {
			// A row this very transaction opened cannot have its observed interval closed at
			// the instant it opened — close_observed refuses an empty interval, and rightly.
			// It happens only when an identity claim mints its subject, attach.go links a
			// change to it, and the rules then merge that subject away in the same event: the
			// edge is a `changed_by` whose canonical form a reader resolves anyway. Leaving it
			// is the conservative answer; re-pointing it would mean two current rows.
			continue
		}
		assertions, err := p.edgeAssertions(ctx, tx, key.typ, edgeEventIDs(rows))
		if err != nil {
			return err
		}

		// Close before re-applying, so the stale series and the canonical one are never both
		// current — the order absorb uses for nodes, for the same reason.
		for _, row := range rows {
			if err := closeObserved(ctx, tx, "edge_versions", row.versionID, observedAt, eventID); err != nil {
				return err
			}
		}

		canonical, err := p.canonicalEdgeKey(ctx, tx, key)
		if err != nil {
			return err
		}
		if canonical.srcID == canonical.dstID {
			// The merge made the two ends one entity. A self-edge carries no information and
			// applyUpsertEdge drops it, so the closed rows have no successor.
			continue
		}
		fold, ok := folded[canonical]
		if !ok {
			fold = newFoldedEdge()
			folded[canonical] = fold
			order = append(order, canonical)
		}
		fold.take(rows, assertions)
	}

	slices.SortFunc(order, compareEdgeKey)
	for _, canonical := range order {
		if err := p.reassertEdge(ctx, tx, canonical, folded[canonical], eventID, observedAt); err != nil {
			return err
		}
	}
	return nil
}

// staleEdgeKeys lists the current edge series naming a merged-away entity at either end, in a
// deterministic order so a replay absorbs them identically (FR-023).
func (p *Projector) staleEdgeKeys(ctx context.Context, tx pgx.Tx, mergedIDs []string) ([]edgeKey, error) {
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT src_id, dst_id, type
		FROM graph.edge_versions
		WHERE upper_inf(observed) AND (src_id = ANY($1) OR dst_id = ANY($1))
		ORDER BY src_id, dst_id, type`, mergedIDs)
	if err != nil {
		return nil, fmt.Errorf("projector: read edges of merged entities: %w", err)
	}
	defer rows.Close()

	var keys []edgeKey
	for rows.Next() {
		var (
			key     edgeKey
			typeStr string
		)
		if err := rows.Scan(&key.srcID, &key.dstID, &typeStr); err != nil {
			return nil, fmt.Errorf("projector: scan edge of merged entity: %w", err)
		}
		key.typ = graph.EdgeType(typeStr)
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("projector: read edges of merged entities: %w", err)
	}
	return keys, nil
}

// openedAt reports whether any of the rows had its observed interval opened at at, which means
// this transaction wrote it.
func (p *Projector) openedAt(ctx context.Context, tx pgx.Tx, rows []*edgeRow, at time.Time) (bool, error) {
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.versionID)
	}
	var fresh bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM graph.edge_versions
			WHERE version_id = ANY($1) AND lower(observed) >= $2::timestamptz)`,
		ids, at.UTC()).Scan(&fresh); err != nil {
		return false, fmt.Errorf("projector: read observed bounds of %v: %w", ids, err)
	}
	return fresh, nil
}

// canonicalEdgeKey resolves both endpoints of a series through their merge chains.
func (p *Projector) canonicalEdgeKey(ctx context.Context, tx pgx.Tx, key edgeKey) (edgeKey, error) {
	srcID, err := p.follow(ctx, tx, key.srcID)
	if err != nil {
		return edgeKey{}, err
	}
	dstID, err := p.follow(ctx, tx, key.dstID)
	if err != nil {
		return edgeKey{}, err
	}
	return edgeKey{srcID: srcID, dstID: dstID, typ: key.typ}, nil
}

func compareEdgeKey(a, b edgeKey) int {
	if c := cmp.Compare(a.srcID, b.srcID); c != 0 {
		return c
	}
	if c := cmp.Compare(a.dstID, b.dstID); c != 0 {
		return c
	}
	return cmp.Compare(string(a.typ), string(b.typ))
}

// foldedEdge is what one canonical relationship inherits from the series being absorbed into it:
// the assertions to re-apply, once each, and the rows that rest on no assertion at all.
type foldedEdge struct {
	// assertions is every upsert_edge event behind the absorbed rows, by id, so the planner can
	// materialize them.
	assertions map[string]edgeAssertion
	items      []edgeAssertion
	seen       map[string]bool
	// carried are the absorbed segments no assertion stands behind, paired with the content to
	// re-insert under the canonical endpoints.
	carried []carriedEdge
}

// carriedEdge is one assertion-less row moving to the canonical endpoints unchanged.
type carriedEdge struct {
	origin  string
	segment segment
	content edgeContent
}

func newFoldedEdge() *foldedEdge {
	return &foldedEdge{assertions: map[string]edgeAssertion{}, seen: map[string]bool{}}
}

// take folds one stale series in.
func (f *foldedEdge) take(rows []*edgeRow, assertions map[string]edgeAssertion) {
	for id, assertion := range assertions {
		f.assertions[id] = assertion
	}
	for i, seg := range edgeSegmentsOf(rows, assertions) {
		if len(seg.assertions) == 0 {
			f.carried = append(f.carried, carriedEdge{
				origin:  rows[i].versionID,
				segment: seg,
				content: edgeContent{props: rows[i].props, weightClass: rows[i].weightClass},
			})
			continue
		}
		for _, id := range seg.assertions {
			if f.seen[id] {
				continue
			}
			f.seen[id] = true
			f.items = append(f.items, assertions[id])
		}
	}
}

// orderedItems lists the absorbed assertions by valid instant, then source and event id, so the
// fold is a function of the set rather than of the order the rows came back in.
func (f *foldedEdge) orderedItems() []edgeAssertion {
	out := slices.Clone(f.items)
	slices.SortFunc(out, func(a, b edgeAssertion) int {
		return cmpAssertion(a.assertedAt, a.sourceID, a.eventID, b.assertedAt, b.sourceID, b.eventID)
	})
	return out
}

// orderedCarried lists the assertion-less segments by valid lower bound, then by the id of the
// row they came from.
func (f *foldedEdge) orderedCarried() []carriedEdge {
	out := slices.Clone(f.carried)
	slices.SortFunc(out, func(a, b carriedEdge) int {
		if c := a.segment.start.Compare(b.segment.start); c != 0 {
			return c
		}
		return cmp.Compare(a.origin, b.origin)
	})
	return out
}

// reassertEdge writes what was absorbed onto the canonical relationship.
func (p *Projector) reassertEdge(ctx context.Context, tx pgx.Tx, key edgeKey, fold *foldedEdge, eventID string, observedAt time.Time) error {
	existing, err := p.currentEdgeRows(ctx, tx, key)
	if err != nil {
		return err
	}
	assertions, err := p.edgeAssertions(ctx, tx, key.typ, edgeEventIDs(existing))
	if err != nil {
		return err
	}
	for id, assertion := range fold.assertions {
		assertions[id] = assertion
	}

	planned := edgeSegmentsOf(existing, assertions)
	for _, item := range fold.orderedItems() {
		planned = planUpsert(planned, item.sourceID, item.eventID, item.assertedAt,
			item.fromUnknown, edgeAssertedAtFunc(assertions), edgeContentEqual(assertions))
	}
	// The merge is why these rows exist under these endpoints, so it belongs to their evidence
	// (FR-034). It goes in `boundary`, which is where events that shaped a row without asserting
	// its content live; a row that turns out to be unchanged keeps the provenance it had.
	for i := range planned {
		planned[i].boundary = append(slices.Clone(planned[i].boundary), eventID)
	}
	if err := p.writeEdgeSegments(ctx, tx, key, existing, planned, assertions, eventID, observedAt); err != nil {
		return err
	}
	return p.carryEdges(ctx, tx, key, fold, eventID, observedAt)
}

// carryEdges re-inserts the assertion-less rows under the canonical endpoints.
//
// A segment that would overlap something already current there is dropped rather than written:
// the exclusion constraint would refuse it, and an assertion-backed version of the same
// relationship is the better statement of it.
func (p *Projector) carryEdges(ctx context.Context, tx pgx.Tx, key edgeKey, fold *foldedEdge, eventID string, observedAt time.Time) error {
	carried := fold.orderedCarried()
	if len(carried) == 0 {
		return nil
	}
	current, err := p.currentEdgeRows(ctx, tx, key)
	if err != nil {
		return err
	}
	occupied := make([]segment, 0, len(current)+len(carried))
	for _, row := range current {
		occupied = append(occupied, segment{start: row.valid.Start, end: endOf(row.valid)})
	}
	for _, item := range carried {
		if slices.ContainsFunc(occupied, func(s segment) bool { return segmentsOverlap(s, item.segment) }) {
			continue
		}
		seg := item.segment.clone()
		seg.versionID = ""
		seg.boundary = append(seg.boundary, eventID)
		if err := p.insertEdgeVersion(ctx, tx, key, seg, item.content, eventID, observedAt); err != nil {
			return err
		}
		occupied = append(occupied, seg)
	}
	return nil
}

// segmentsOverlap is the half-open `&&` the exclusion constraint applies to two valid ranges.
func segmentsOverlap(a, b segment) bool {
	return (a.end.IsZero() || b.start.Before(a.end)) && (b.end.IsZero() || a.start.Before(b.end))
}

// closeObserved closes an open observed upper bound through the one function allowed to do it
// (0002_graph.sql, research §14). Every correction and every retraction goes through here.
func closeObserved(ctx context.Context, tx pgx.Tx, table, versionID string, at time.Time, eventID string) error {
	if _, err := tx.Exec(ctx,
		`SELECT graph.close_observed($1, $2, $3, $4)`, table, versionID, at.UTC(), eventID); err != nil {
		return fmt.Errorf("projector: close observed interval of %s %s: %w", table, versionID, err)
	}
	return nil
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// refString renders a Ref the way unattached change targets are recorded and matched.
func refString(ref graph.Ref) string { return ref.Namespace + "=" + ref.Value }
