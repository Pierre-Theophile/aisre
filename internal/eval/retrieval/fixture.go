// SPDX-License-Identifier: Apache-2.0

// Package retrieval is the knowledge retrieval evaluation (008 US4, FR-042, FR-043, research R10):
// a corpus of small fixtures, each a query, a handful of documents with their links and section
// versions, and the one document the query should find. `aisre eval retrieval` runs every fixture
// through the knowledge worker in each retrieval mode and reports recall@3 and MRR; the gate in
// thresholds.go fails CI when either falls.
//
// A fixture needs no database and no model: its documents are an in-memory corpus and an
// in-memory store, so the evaluation runs on every pull request.
package retrieval

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Pierre-Theophile/aisre/internal/investigation/workers/knowledge"
	kstore "github.com/Pierre-Theophile/aisre/pkg/knowledge"
	"github.com/Pierre-Theophile/aisre/pkg/knowledge/memory"
)

// The files of a retrieval fixture (008 data-model §Retrieval fixture). The query file is not
// called manifest.yaml so that `fixture verify fixtures/*/` does not take a retrieval fixture for
// a graph fixture.
const (
	QueryFile     = "retrieval.yaml"
	DocumentsFile = "documents.yaml"
)

// Fixture is one retrieval fixture.
type Fixture struct {
	Dir         string    `yaml:"-"`
	ID          string    `yaml:"id"`
	Shape       string    `yaml:"shape"`
	Description string    `yaml:"description"`
	Query       Query     `yaml:"query"`
	Expect      Expect    `yaml:"expect"`
	Documents   Documents `yaml:"-"`
}

// Query is what the investigation asks: the alert's words, its scope and its two instants.
type Query struct {
	Terms      []string  `yaml:"terms"`
	EntityIDs  []string  `yaml:"entity_ids"`
	ValidAt    time.Time `yaml:"valid_at"`
	ObservedAt time.Time `yaml:"observed_at"`
}

// Expect is the ground truth.
type Expect struct {
	// Document is the reference of the document the query must find.
	Document string `yaml:"document"`
	// InScope is the in_scope flag it must be cited with.
	InScope bool `yaml:"in_scope"`
	// Never are documents that must not be cited at all, e.g. one written after the pin.
	Never []string `yaml:"never"`
}

// Documents is a fixture's documents.yaml.
type Documents struct {
	Documents []Document `yaml:"documents"`
}

// Document is one document: what the graph knows about it, and its section versions.
type Document struct {
	Ref        string    `yaml:"ref"`
	Kind       string    `yaml:"kind"`
	Title      string    `yaml:"title"`
	Summary    string    `yaml:"summary"`
	Location   string    `yaml:"location"`
	AuthoredAt time.Time `yaml:"authored_at"`
	Links      []Link    `yaml:"links"`
	Puts       []Put     `yaml:"puts"`
}

// Link is one CONCERNS edge: valid over [ValidFrom, ValidTo), known from KnownFrom (ValidFrom when
// unset).
type Link struct {
	Entity    string    `yaml:"entity"`
	ValidFrom time.Time `yaml:"valid_from"`
	ValidTo   time.Time `yaml:"valid_to"`
	KnownFrom time.Time `yaml:"known_from"`
}

// Put is one write of a document's whole section set, as a source sends it.
type Put struct {
	At       time.Time `yaml:"at"`
	Sections []Section `yaml:"sections"`
}

// Section is one section of a Put.
type Section struct {
	Label  string `yaml:"label"`
	Part   int    `yaml:"part"`
	Origin string `yaml:"origin"`
	Text   string `yaml:"text"`
}

// Load reads one fixture directory.
func Load(dir string) (*Fixture, error) {
	f := &Fixture{Dir: dir}
	if err := readYAML(filepath.Join(dir, QueryFile), f); err != nil {
		return nil, err
	}
	if err := readYAML(filepath.Join(dir, DocumentsFile), &f.Documents); err != nil {
		return nil, err
	}
	if err := f.Validate(); err != nil {
		return nil, err
	}
	return f, nil
}

// LoadAll reads every fixture under root: root itself when it is one, else its immediate
// subdirectories that are, sorted.
func LoadAll(root string) ([]*Fixture, error) {
	if _, err := os.Stat(filepath.Join(root, QueryFile)); err == nil {
		f, err := Load(root)
		if err != nil {
			return nil, err
		}
		return []*Fixture{f}, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("retrieval: %w", err)
	}
	var out []*Fixture
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		if _, err := os.Stat(filepath.Join(dir, QueryFile)); err != nil {
			continue
		}
		f, err := Load(dir)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("retrieval: no fixtures under %s; an evaluation of nothing is not a pass", root)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func readYAML(path string, into any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("retrieval: %w", err)
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(raw)))
	decoder.KnownFields(true)
	if err := decoder.Decode(into); err != nil {
		return fmt.Errorf("retrieval: %s: %w", path, err)
	}
	return nil
}

// Validate refuses a fixture that could not grade anything: no query, no expectation, an expected
// document it does not hold, a section a store would refuse.
func (f *Fixture) Validate() error {
	var problems []string
	if f.ID == "" {
		problems = append(problems, "no id")
	} else if base := filepath.Base(f.Dir); f.Dir != "" && base != f.ID {
		problems = append(problems, fmt.Sprintf("id %q is not the directory name %q", f.ID, base))
	}
	if len(f.Query.Terms) == 0 {
		problems = append(problems, "no query terms")
	}
	if len(f.Query.EntityIDs) == 0 {
		problems = append(problems, "no entity scope")
	}
	if f.Query.ValidAt.IsZero() || f.Query.ObservedAt.IsZero() {
		problems = append(problems, "the query needs both valid_at and observed_at")
	}
	refs := map[string]bool{}
	for i, d := range f.Documents.Documents {
		switch {
		case d.Ref == "" || !strings.Contains(d.Ref, "="):
			problems = append(problems, fmt.Sprintf("document %d: ref %q is not namespace=value", i, d.Ref))
		case refs[d.Ref]:
			problems = append(problems, fmt.Sprintf("document %s appears twice", d.Ref))
		}
		refs[d.Ref] = true
		if !knowledge.ValidKind(d.Kind) {
			problems = append(problems, fmt.Sprintf("document %s: kind %q is not published", d.Ref, d.Kind))
		}
		if d.Title == "" || len(d.Links) == 0 {
			problems = append(problems, fmt.Sprintf("document %s: needs a title and at least one link", d.Ref))
		}
		for j, p := range d.Puts {
			if err := kstore.ValidateSections(p.sections()); err != nil {
				problems = append(problems, fmt.Sprintf("document %s put %d: %v", d.Ref, j, err))
			}
			if j > 0 && p.At.Before(d.Puts[j-1].At) {
				problems = append(problems, fmt.Sprintf("document %s: puts are not in time order", d.Ref))
			}
		}
	}
	if !refs[f.Expect.Document] {
		problems = append(problems, fmt.Sprintf("the expected document %q is not among the documents", f.Expect.Document))
	}
	for _, never := range f.Expect.Never {
		if never == f.Expect.Document {
			problems = append(problems, "the expected document is also in never")
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("retrieval: %s: %w: %s", f.Dir, errInvalid, strings.Join(problems, "; "))
	}
	return nil
}

var errInvalid = errors.New("invalid fixture")

// LoadDocuments reads a documents.yaml on its own. An incident fixture carries one beside its
// events when its documents have sections (008 T011); there the graph side comes from the events,
// so only each document's ref and puts are read.
func LoadDocuments(path string) (Documents, error) {
	var d Documents
	if err := readYAML(path, &d); err != nil {
		return Documents{}, err
	}
	for _, doc := range d.Documents {
		for j, p := range doc.Puts {
			if err := kstore.ValidateSections(p.sections()); err != nil {
				return Documents{}, fmt.Errorf("retrieval: %s: document %s put %d: %w", path, doc.Ref, j, err)
			}
		}
	}
	return d, nil
}

// Store returns an in-memory knowledge store holding every put of every document, in order.
func (d Documents) Store(ctx context.Context) (*memory.Store, error) {
	store := memory.New()
	for _, doc := range d.Documents {
		for _, p := range doc.Puts {
			if _, err := store.Put(ctx, doc.Ref, p.sections(), p.At); err != nil {
				return nil, fmt.Errorf("retrieval: %s: %w", doc.Ref, err)
			}
		}
	}
	return store, nil
}

func (p Put) sections() []kstore.Section {
	out := make([]kstore.Section, 0, len(p.Sections))
	for _, s := range p.Sections {
		out = append(out, kstore.Section{Label: s.Label, Part: s.Part, Text: strings.TrimSpace(s.Text), Origin: kstore.Origin(s.Origin)})
	}
	return out
}
