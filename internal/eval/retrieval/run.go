// SPDX-License-Identifier: Apache-2.0

package retrieval

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/internal/investigation/workers/knowledge"
	kstore "github.com/Pierre-Theophile/aisre/pkg/knowledge"
	"github.com/Pierre-Theophile/aisre/pkg/worker"
)

// The retrieval modes the evaluation measures.
const (
	// ModeSummaryOnly is 007's retrieval: no store, every document matched on its registered title
	// and summary alone. It is the baseline the other modes must beat.
	ModeSummaryOnly = "summary_only"
	// ModeLexicalOnly is the knowledge store's sections, ranked lexically.
	ModeLexicalOnly = knowledge.ModeLexicalOnly
)

// Modes is the measured set, in report order.
func Modes() []string { return []string{ModeSummaryOnly, ModeLexicalOnly} }

// RecallDepth is the k of recall@k: the expected document must be among the first three.
const RecallDepth = 3

// Result is one fixture in one mode.
type Result struct {
	Fixture string
	Mode    string
	// Rank is the expected document's 1-based rank, cited with the expected in_scope flag on a
	// matched section; 0 when it was not cited, cited with the other flag, or cited only as an
	// in-scope document nothing in which matched (that is a listing, not a retrieval).
	Rank int
	// Cited is every cited document, in order.
	Cited []string
	// Violations are documents in Expect.Never that were cited.
	Violations []string
}

// Hit reports whether the expected document is within RecallDepth.
func (r Result) Hit() bool { return r.Rank > 0 && r.Rank <= RecallDepth }

// ReciprocalRank is 1/Rank, 0 when not cited.
func (r Result) ReciprocalRank() float64 {
	if r.Rank == 0 {
		return 0
	}
	return kstore.Round6(1 / float64(r.Rank))
}

// Aggregate is one mode over every fixture.
type Aggregate struct {
	Mode       string
	Fixtures   int
	RecallAt3  float64
	MRR        float64
	Violations int
}

// Run evaluates every fixture in every mode.
func Run(ctx context.Context, fixtures []*Fixture) ([]Result, error) {
	var out []Result
	for _, f := range fixtures {
		for _, mode := range Modes() {
			r, err := runOne(ctx, f, mode)
			if err != nil {
				return nil, fmt.Errorf("retrieval: %s (%s): %w", f.ID, mode, err)
			}
			out = append(out, r)
		}
	}
	return out, nil
}

func runOne(ctx context.Context, f *Fixture, mode string) (Result, error) {
	var opts []knowledge.Option
	if mode != ModeSummaryOnly {
		store, err := f.Documents.Store(ctx)
		if err != nil {
			return Result{}, err
		}
		opts = append(opts, knowledge.WithStore(store))
	}
	w := knowledge.New(corpus{documents: f.Documents.Documents}, opts...)

	request := knowledge.SearchTerm(f.Query.EntityIDs, f.Query.Terms, engine.MaxKnowledgeItems, "hyp-retrieval", "has this happened before?")
	request.ValidAt = timestamppb.New(f.Query.ValidAt.UTC())
	request.ObservedAt = timestamppb.New(f.Query.ObservedAt.UTC())
	resp, err := w.Call(ctx, worker.Request{Capability: engine.TermKnowledgeSearch, Mode: worker.ModeRecorded, Algebra: request})
	if err != nil {
		return Result{}, err
	}

	r := Result{Fixture: f.ID, Mode: mode}
	for i, item := range resp.Algebra.GetDigest().GetKnowledge().GetItems() {
		r.Cited = append(r.Cited, item.GetDocumentId())
		if item.GetDocumentId() == f.Expect.Document && item.GetInScope() == f.Expect.InScope &&
			item.GetSectionLabel() != "" && r.Rank == 0 {
			r.Rank = i + 1
		}
		if slices.Contains(f.Expect.Never, item.GetDocumentId()) {
			r.Violations = append(r.Violations, item.GetDocumentId())
		}
	}
	return r, nil
}

// Summarise aggregates results per mode, in Modes order.
func Summarise(results []Result) []Aggregate {
	byMode := map[string]*Aggregate{}
	for _, r := range results {
		a, ok := byMode[r.Mode]
		if !ok {
			a = &Aggregate{Mode: r.Mode}
			byMode[r.Mode] = a
		}
		a.Fixtures++
		if r.Hit() {
			a.RecallAt3++
		}
		a.MRR += r.ReciprocalRank()
		a.Violations += len(r.Violations)
	}
	var out []Aggregate
	for _, mode := range Modes() {
		a, ok := byMode[mode]
		if !ok {
			continue
		}
		if a.Fixtures > 0 {
			a.RecallAt3 = kstore.Round6(a.RecallAt3 / float64(a.Fixtures))
			a.MRR = kstore.Round6(a.MRR / float64(a.Fixtures))
		}
		out = append(out, *a)
	}
	return out
}

// Report writes the per-fixture table, the aggregate and the gate's verdict, as Markdown.
func Report(w io.Writer, results []Result, aggregates []Aggregate, failures []string) {
	modes := Modes()
	fmt.Fprintf(w, "### Knowledge retrieval: recall@%d and MRR\n\n", RecallDepth)
	fmt.Fprintf(w, "| fixture | %s |\n", strings.Join(modes, " | "))
	fmt.Fprintf(w, "|---|%s\n", strings.Repeat("---|", len(modes)))
	byKey := map[[2]string]Result{}
	var order []string
	for _, r := range results {
		if !slices.Contains(order, r.Fixture) {
			order = append(order, r.Fixture)
		}
		byKey[[2]string{r.Fixture, r.Mode}] = r
	}
	for _, id := range order {
		cells := make([]string, 0, len(modes))
		for _, mode := range modes {
			r := byKey[[2]string{id, mode}]
			cell := "miss"
			if r.Rank > 0 {
				cell = fmt.Sprintf("rank %d", r.Rank)
			}
			if len(r.Violations) > 0 {
				cell += fmt.Sprintf(" (cited %s)", strings.Join(r.Violations, ", "))
			}
			cells = append(cells, cell)
		}
		fmt.Fprintf(w, "| %s | %s |\n", id, strings.Join(cells, " | "))
	}
	fmt.Fprintf(w, "\n| mode | fixtures | recall@%d | MRR | never-cited violations |\n|---|---|---|---|---|\n", RecallDepth)
	for _, a := range aggregates {
		fmt.Fprintf(w, "| %s | %d | %.3f | %.3f | %d |\n", a.Mode, a.Fixtures, a.RecallAt3, a.MRR, a.Violations)
	}
	if len(failures) == 0 {
		fmt.Fprintln(w, "\ngate: pass")
		return
	}
	fmt.Fprintln(w, "\ngate: FAIL")
	for _, f := range failures {
		fmt.Fprintf(w, "- %s\n", f)
	}
}

// corpus is a fixture's documents as the graph would return them: every document with a link
// valid at the valid instant and known at the observed one.
type corpus struct{ documents []Document }

func (c corpus) Candidates(_ context.Context, _ []string, at knowledge.Instants) ([]knowledge.Document, error) {
	var out []knowledge.Document
	for _, d := range c.documents {
		var linked []string
		var since time.Time
		for _, l := range d.Links {
			known := l.KnownFrom
			if known.IsZero() {
				known = l.ValidFrom
			}
			if l.ValidFrom.After(at.Valid) || (!l.ValidTo.IsZero() && !at.Valid.Before(l.ValidTo)) || known.After(at.Observed) {
				continue
			}
			if !slices.Contains(linked, l.Entity) {
				linked = append(linked, l.Entity)
			}
			if since.IsZero() || l.ValidFrom.Before(since) {
				since = l.ValidFrom
			}
		}
		if len(linked) == 0 {
			continue
		}
		doc := knowledge.Document{
			ID: d.Ref, Kind: d.Kind, Title: d.Title, Summary: d.Summary, Location: d.Location,
			LinkedEntityIDs: linked, LinkProvenance: "fixture", AuthoredAt: d.AuthoredAt.UTC(),
		}
		if doc.AuthoredAt.IsZero() {
			doc.AuthoredAt = since
		}
		if at.Valid.After(doc.AuthoredAt) {
			doc.AgeDays = int64(at.Valid.Sub(doc.AuthoredAt) / (24 * time.Hour))
		}
		out = append(out, doc)
	}
	return out, nil
}
