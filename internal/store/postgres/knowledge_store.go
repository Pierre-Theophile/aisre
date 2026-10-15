// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Pierre-Theophile/aisre/pkg/knowledge"
)

// KnowledgeStore is the default knowledge.Store: the agent's own PostgreSQL, migration 0014
// (feature 008, research R2). It needs nothing the agent does not already require.
//
// Lexical search is a `simple` tsvector, matched by any query term and ordered by ts_rank_cd.
// Semantic search is an exact cosine scan in process, over a cache of the vectors of each model it
// has been asked about, refreshed incrementally on every search (the embeddings table only grows).
// At the stated scale, tens of thousands of sections, that is exact and fast enough; a deployment
// that outgrows it plugs in another store.
type KnowledgeStore struct {
	store *Store

	mu    sync.Mutex
	cache map[string]*vectorCache // by ModelID.String()
}

type vectorCache struct {
	seq     int64
	vectors map[string][]float32 // by digest
}

var _ knowledge.Store = (*KnowledgeStore)(nil)

// NewKnowledgeStore returns the knowledge store over a migrated Store.
func NewKnowledgeStore(store *Store) *KnowledgeStore {
	return &KnowledgeStore{store: store, cache: map[string]*vectorCache{}}
}

type openVersion struct {
	id        int64
	digest    string
	knownFrom time.Time
}

// Put implements knowledge.Store.
func (k *KnowledgeStore) Put(ctx context.Context, documentRef string, sections []knowledge.Section, at time.Time) (knowledge.PutResult, error) {
	if documentRef == "" {
		return knowledge.PutResult{}, fmt.Errorf("%w: no document reference", knowledge.ErrInvalidSections)
	}
	if err := knowledge.ValidateSections(sections); err != nil {
		return knowledge.PutResult{}, err
	}
	at = at.UTC().Truncate(time.Microsecond)

	var result knowledge.PutResult
	err := k.store.WithTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		result = knowledge.PutResult{}
		// One put per document at a time: two concurrent puts would each read the same open set.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 8008))`, documentRef); err != nil {
			return fmt.Errorf("postgres: knowledge: lock %s: %w", documentRef, err)
		}
		open, err := openVersions(ctx, tx, documentRef)
		if err != nil {
			return err
		}
		for _, v := range open {
			if at.Before(v.knownFrom) {
				return fmt.Errorf("postgres: knowledge: put of %s at %s is before its current version (known from %s)",
					documentRef, at.Format(time.RFC3339Nano), v.knownFrom.Format(time.RFC3339Nano))
			}
		}

		present := make(map[sectionKey]struct{}, len(sections))
		for _, s := range sections {
			key := sectionKey{label: s.Label, part: s.Part}
			present[key] = struct{}{}
			digest := s.Digest()
			if v, ok := open[key]; ok {
				if v.digest == digest {
					result.Unchanged++
					continue
				}
				if err := closeVersion(ctx, tx, documentRef, v, at); err != nil {
					return err
				}
				result.Closed++
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO knowledge.section_versions (document_ref, label, part, digest, origin, text, known)
				VALUES ($1, $2, $3, $4, $5, $6, tstzrange($7::timestamptz, NULL, '[)'))`,
				documentRef, s.Label, s.Part, digest, string(s.Origin), s.Text, at); err != nil {
				return fmt.Errorf("postgres: knowledge: insert %s/%s#%d: %w", documentRef, s.Label, s.Part, err)
			}
			result.Opened++
		}
		for key, v := range open {
			if _, ok := present[key]; ok {
				continue
			}
			if err := closeVersion(ctx, tx, documentRef, v, at); err != nil {
				return err
			}
			result.Closed++
		}
		return nil
	})
	if err != nil {
		return knowledge.PutResult{}, err
	}
	return result, nil
}

type sectionKey struct {
	label string
	part  int
}

func openVersions(ctx context.Context, tx pgx.Tx, documentRef string) (map[sectionKey]openVersion, error) {
	rows, err := tx.Query(ctx, `
		SELECT version_id, label, part, digest, lower(known)
		FROM knowledge.section_versions
		WHERE document_ref = $1 AND upper_inf(known)`, documentRef)
	if err != nil {
		return nil, fmt.Errorf("postgres: knowledge: read %s: %w", documentRef, err)
	}
	defer rows.Close()
	out := map[sectionKey]openVersion{}
	for rows.Next() {
		var (
			v     openVersion
			label string
			part  int
		)
		if err := rows.Scan(&v.id, &label, &part, &v.digest, &v.knownFrom); err != nil {
			return nil, fmt.Errorf("postgres: knowledge: scan %s: %w", documentRef, err)
		}
		v.knownFrom = v.knownFrom.UTC()
		out[sectionKey{label: label, part: part}] = v
	}
	return out, rows.Err()
}

// closeVersion ends an open version at at. A version opened at the same instant cannot be closed
// there — it would be known over an empty interval — so a second change at one instant is refused.
func closeVersion(ctx context.Context, tx pgx.Tx, documentRef string, v openVersion, at time.Time) error {
	if !at.After(v.knownFrom) {
		return fmt.Errorf("postgres: knowledge: %s changed twice at %s; puts of one document need distinct instants",
			documentRef, at.Format(time.RFC3339Nano))
	}
	if _, err := tx.Exec(ctx, `
		UPDATE knowledge.section_versions
		SET known = tstzrange(lower(known), $2::timestamptz, '[)')
		WHERE version_id = $1 AND upper_inf(known)`, v.id, at); err != nil {
		return fmt.Errorf("postgres: knowledge: close %s: %w", documentRef, err)
	}
	return nil
}

// orQuery renders the query as a tsquery matching any of its terms. websearch_to_tsquery would
// AND them, and an alert's words are rarely all in one section. The terms are knowledge.Tokenise's,
// letters and digits only, so nothing in them is tsquery syntax.
func orQuery(query string) string {
	tokens := knowledge.Tokenise(query)
	seen := make(map[string]struct{}, len(tokens))
	var unique []string
	for _, t := range tokens {
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		unique = append(unique, "'"+t+"'")
	}
	return strings.Join(unique, " | ")
}

// Lexical implements knowledge.Store.
func (k *KnowledgeStore) Lexical(ctx context.Context, query string, documents []string, observed time.Time, limit int) ([]knowledge.Hit, error) {
	tsquery := orQuery(query)
	if tsquery == "" || len(documents) == 0 || limit == 0 {
		return nil, nil
	}
	rows, err := k.store.Pool().Query(ctx, `
		SELECT document_ref, label, part, text, digest, origin
		FROM knowledge.section_versions, to_tsquery('simple', $1) AS q
		WHERE document_ref = ANY($2) AND known @> $3::timestamptz AND search @@ q
		ORDER BY round(ts_rank_cd(search, q)::numeric, 6) DESC, document_ref, label, part
		LIMIT $4`, tsquery, documents, observed.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: knowledge: lexical: %w", err)
	}
	return scanHits(rows)
}

func scanHits(rows pgx.Rows) ([]knowledge.Hit, error) {
	defer rows.Close()
	var out []knowledge.Hit
	for rows.Next() {
		var (
			h      knowledge.Hit
			origin string
		)
		if err := rows.Scan(&h.DocumentRef, &h.Label, &h.Part, &h.Text, &h.Digest, &origin); err != nil {
			return nil, fmt.Errorf("postgres: knowledge: scan: %w", err)
		}
		h.Origin = knowledge.Origin(origin)
		h.Rank = len(out) + 1
		out = append(out, h)
	}
	return out, rows.Err()
}

// Semantic implements knowledge.Store.
func (k *KnowledgeStore) Semantic(ctx context.Context, vector []float32, model knowledge.ModelID, documents []string, observed time.Time, limit int) ([]knowledge.Hit, error) {
	if len(vector) != model.Dimensions {
		return nil, fmt.Errorf("%w: query vector of %d for %s", knowledge.ErrDimensionMismatch, len(vector), model)
	}
	if len(documents) == 0 || limit == 0 {
		return nil, nil
	}
	vectors, err := k.refresh(ctx, model)
	if err != nil {
		return nil, err
	}
	rows, err := k.store.Pool().Query(ctx, `
		SELECT document_ref, label, part, text, digest, origin
		FROM knowledge.section_versions
		WHERE document_ref = ANY($1) AND known @> $2::timestamptz`, documents, observed.UTC())
	if err != nil {
		return nil, fmt.Errorf("postgres: knowledge: semantic: %w", err)
	}
	candidates, err := scanHits(rows)
	if err != nil {
		return nil, err
	}

	type scored struct {
		hit   knowledge.Hit
		score float64
	}
	var hits []scored
	for _, h := range candidates {
		stored, ok := vectors[h.Digest]
		if !ok {
			continue
		}
		hits = append(hits, scored{hit: h, score: knowledge.Cosine(vector, stored)})
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		a, b := hits[i].hit, hits[j].hit
		if a.DocumentRef != b.DocumentRef {
			return a.DocumentRef < b.DocumentRef
		}
		if a.Label != b.Label {
			return a.Label < b.Label
		}
		return a.Part < b.Part
	})
	if limit > 0 && len(hits) > limit {
		hits = hits[:limit]
	}
	out := make([]knowledge.Hit, 0, len(hits))
	for i, s := range hits {
		s.hit.Rank = i + 1
		out = append(out, s.hit)
	}
	return out, nil
}

// refresh loads the model's vectors added since the last call and returns the model's cache. The
// returned map is shared and must not be written.
func (k *KnowledgeStore) refresh(ctx context.Context, model knowledge.ModelID) (map[string][]float32, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	key := model.String()
	c, ok := k.cache[key]
	if !ok {
		c = &vectorCache{vectors: map[string][]float32{}}
		k.cache[key] = c
	}
	rows, err := k.store.Pool().Query(ctx, `
		SELECT digest, vector, seq FROM knowledge.section_embeddings
		WHERE model = $1 AND seq > $2 ORDER BY seq`, key, c.seq)
	if err != nil {
		return nil, fmt.Errorf("postgres: knowledge: read vectors: %w", err)
	}
	defer rows.Close()
	// Copy on write, so a map a concurrent Semantic is scanning is never mutated under it.
	next := c.vectors
	copied := false
	for rows.Next() {
		var (
			digest string
			vector []float32
			seq    int64
		)
		if err := rows.Scan(&digest, &vector, &seq); err != nil {
			return nil, fmt.Errorf("postgres: knowledge: scan vectors: %w", err)
		}
		if !copied {
			next = make(map[string][]float32, len(c.vectors)+1)
			for d, v := range c.vectors {
				next[d] = v
			}
			copied = true
		}
		next[digest] = vector
		c.seq = seq
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: knowledge: read vectors: %w", err)
	}
	c.vectors = next
	return next, nil
}

// Pending implements knowledge.Store.
func (k *KnowledgeStore) Pending(ctx context.Context, model knowledge.ModelID, limit int) ([]knowledge.PendingSection, error) {
	if limit <= 0 {
		limit = 1 << 30
	}
	rows, err := k.store.Pool().Query(ctx, `
		SELECT DISTINCT ON (first_id) digest, label, text
		FROM (
			SELECT v.digest, v.label, v.text,
			       min(v.version_id) OVER (PARTITION BY v.digest) AS first_id, v.version_id
			FROM knowledge.section_versions v
			WHERE upper_inf(v.known)
			  AND NOT EXISTS (
				SELECT 1 FROM knowledge.section_embeddings e
				WHERE e.digest = v.digest AND e.model = $1)
		) pending
		ORDER BY first_id, version_id
		LIMIT $2`, model.String(), limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: knowledge: pending: %w", err)
	}
	defer rows.Close()
	var out []knowledge.PendingSection
	for rows.Next() {
		var p knowledge.PendingSection
		if err := rows.Scan(&p.Digest, &p.Label, &p.Text); err != nil {
			return nil, fmt.Errorf("postgres: knowledge: scan pending: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PutVectors implements knowledge.Store.
func (k *KnowledgeStore) PutVectors(ctx context.Context, model knowledge.ModelID, vectors []knowledge.DigestVector) error {
	for _, dv := range vectors {
		if len(dv.Vector) != model.Dimensions {
			return fmt.Errorf("%w: %d for %s, digest %s", knowledge.ErrDimensionMismatch, len(dv.Vector), model, dv.Digest)
		}
	}
	if len(vectors) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, dv := range vectors {
		batch.Queue(`
			INSERT INTO knowledge.section_embeddings (digest, model, dimensions, vector)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (digest, model) DO NOTHING`, dv.Digest, model.String(), model.Dimensions, dv.Vector)
	}
	results := k.store.Pool().SendBatch(ctx, batch)
	defer results.Close()
	for range vectors {
		if _, err := results.Exec(); err != nil {
			return fmt.Errorf("postgres: knowledge: put vectors: %w", err)
		}
	}
	return nil
}
