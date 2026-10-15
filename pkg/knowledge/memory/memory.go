// SPDX-License-Identifier: Apache-2.0

// Package memory is an in-memory knowledge.Store: every section version and vector in maps, lexical
// search by knowledge.BM25 and semantic search by exact cosine. It is what the tests and the
// retrieval evaluation use, and the reference a third-party store can be read against. It passes
// storetest.Run.
package memory

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/Pierre-Theophile/aisre/pkg/knowledge"
)

// Store is an in-memory knowledge.Store. The zero value is not usable; call New.
type Store struct {
	mu       sync.RWMutex
	versions []version
	// open indexes the open version of each (document, label, part) in versions.
	open    map[key]int
	vectors map[string]map[string][]float32 // model string -> digest -> vector
}

type key struct {
	document string
	label    string
	part     int
}

type version struct {
	key       key
	section   knowledge.Section
	digest    string
	knownFrom time.Time
	knownTo   time.Time // zero while open
}

func (v version) knownAt(at time.Time) bool {
	return !v.knownFrom.After(at) && (v.knownTo.IsZero() || at.Before(v.knownTo))
}

var _ knowledge.Store = (*Store)(nil)

// New returns an empty store.
func New() *Store {
	return &Store{open: map[key]int{}, vectors: map[string]map[string][]float32{}}
}

// Put implements knowledge.Store.
func (s *Store) Put(_ context.Context, documentRef string, sections []knowledge.Section, at time.Time) (knowledge.PutResult, error) {
	if documentRef == "" {
		return knowledge.PutResult{}, fmt.Errorf("%w: no document reference", knowledge.ErrInvalidSections)
	}
	if err := knowledge.ValidateSections(sections); err != nil {
		return knowledge.PutResult{}, err
	}
	at = at.UTC()

	s.mu.Lock()
	defer s.mu.Unlock()

	for k, idx := range s.open {
		if k.document == documentRef && at.Before(s.versions[idx].knownFrom) {
			return knowledge.PutResult{}, fmt.Errorf(
				"knowledge: put of %s at %s is before its current version (known from %s)",
				documentRef, at.Format(time.RFC3339Nano), s.versions[idx].knownFrom.Format(time.RFC3339Nano))
		}
	}

	var result knowledge.PutResult
	present := make(map[key]struct{}, len(sections))
	for _, section := range sections {
		k := key{document: documentRef, label: section.Label, part: section.Part}
		present[k] = struct{}{}
		digest := section.Digest()
		if idx, ok := s.open[k]; ok {
			if s.versions[idx].digest == digest {
				result.Unchanged++
				continue
			}
			s.versions[idx].knownTo = at
			result.Closed++
		}
		s.versions = append(s.versions, version{key: k, section: section, digest: digest, knownFrom: at})
		s.open[k] = len(s.versions) - 1
		result.Opened++
	}
	for k, idx := range s.open {
		if k.document != documentRef {
			continue
		}
		if _, ok := present[k]; ok {
			continue
		}
		s.versions[idx].knownTo = at
		delete(s.open, k)
		result.Closed++
	}
	return result, nil
}

// known returns the versions of documents known at observed, in a stable order.
func (s *Store) known(documents []string, observed time.Time) []version {
	wanted := make(map[string]struct{}, len(documents))
	for _, d := range documents {
		wanted[d] = struct{}{}
	}
	var out []version
	for _, v := range s.versions {
		if _, ok := wanted[v.key.document]; !ok {
			continue
		}
		if v.knownAt(observed) {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return less(out[i].key, out[j].key) })
	return out
}

func less(a, b key) bool {
	if a.document != b.document {
		return a.document < b.document
	}
	if a.label != b.label {
		return a.label < b.label
	}
	return a.part < b.part
}

type scored struct {
	v     version
	score float64
}

func ranked(in []scored, k int) []knowledge.Hit {
	sort.SliceStable(in, func(i, j int) bool {
		if in[i].score != in[j].score {
			return in[i].score > in[j].score
		}
		return less(in[i].v.key, in[j].v.key)
	})
	if k >= 0 && len(in) > k {
		in = in[:k]
	}
	out := make([]knowledge.Hit, 0, len(in))
	for i, entry := range in {
		out = append(out, knowledge.Hit{
			DocumentRef: entry.v.key.document,
			Label:       entry.v.section.Label,
			Part:        entry.v.section.Part,
			Text:        entry.v.section.Text,
			Digest:      entry.v.digest,
			Origin:      entry.v.section.Origin,
			Rank:        i + 1,
		})
	}
	return out
}

// Lexical implements knowledge.Store, by BM25 over the label and text of the sections known at
// observed among documents.
func (s *Store) Lexical(_ context.Context, query string, documents []string, observed time.Time, k int) ([]knowledge.Hit, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	candidates := s.known(documents, observed.UTC())
	texts := make([]string, len(candidates))
	for i, v := range candidates {
		texts[i] = v.section.Label + " " + v.section.Text
	}
	scores := knowledge.BM25(query, texts)
	var hits []scored
	for i, v := range candidates {
		if scores[i] > 0 {
			hits = append(hits, scored{v: v, score: scores[i]})
		}
	}
	return ranked(hits, k), nil
}

// Semantic implements knowledge.Store, by exact cosine.
func (s *Store) Semantic(_ context.Context, vector []float32, model knowledge.ModelID, documents []string, observed time.Time, k int) ([]knowledge.Hit, error) {
	if len(vector) != model.Dimensions {
		return nil, fmt.Errorf("%w: query vector of %d for %s", knowledge.ErrDimensionMismatch, len(vector), model)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	byDigest := s.vectors[model.String()]
	var hits []scored
	for _, v := range s.known(documents, observed.UTC()) {
		stored, ok := byDigest[v.digest]
		if !ok {
			continue
		}
		hits = append(hits, scored{v: v, score: knowledge.Cosine(vector, stored)})
	}
	return ranked(hits, k), nil
}

// Pending implements knowledge.Store.
func (s *Store) Pending(_ context.Context, model knowledge.ModelID, limit int) ([]knowledge.PendingSection, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	byDigest := s.vectors[model.String()]
	seen := map[string]struct{}{}
	var out []knowledge.PendingSection
	// versions is in insertion order, which is oldest first.
	for _, v := range s.versions {
		if !v.knownTo.IsZero() {
			continue
		}
		if _, ok := byDigest[v.digest]; ok {
			continue
		}
		if _, ok := seen[v.digest]; ok {
			continue
		}
		seen[v.digest] = struct{}{}
		out = append(out, knowledge.PendingSection{Digest: v.digest, Label: v.section.Label, Text: v.section.Text})
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out, nil
}

// PutVectors implements knowledge.Store.
func (s *Store) PutVectors(_ context.Context, model knowledge.ModelID, vectors []knowledge.DigestVector) error {
	for _, dv := range vectors {
		if len(dv.Vector) != model.Dimensions {
			return fmt.Errorf("%w: %d for %s, digest %s", knowledge.ErrDimensionMismatch, len(dv.Vector), model, dv.Digest)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	byDigest := s.vectors[model.String()]
	if byDigest == nil {
		byDigest = map[string][]float32{}
		s.vectors[model.String()] = byDigest
	}
	for _, dv := range vectors {
		if _, ok := byDigest[dv.Digest]; ok {
			continue
		}
		byDigest[dv.Digest] = append([]float32(nil), dv.Vector...)
	}
	return nil
}
