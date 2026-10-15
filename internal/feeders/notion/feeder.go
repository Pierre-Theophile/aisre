// SPDX-License-Identifier: Apache-2.0

// Package notion is the Notion knowledge connector (feature 007): it registers the pages of the
// databases an operator names as KNOWLEDGE_DOC nodes linked by CONCERNS edges to the entities they
// concern, so that the knowledge worker can cite them.
//
// The poller (poller.go) reads Notion through internal/notionx and writes one payload per read of a
// data source; the feeder (this file) turns payloads into events and keeps no memory of its own. What
// a change is computed against — the entities a page was linked to before — travels in the payload,
// so a replay of a recording produces exactly the events the live run did.
package notion

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/types/known/structpb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/investigation/workers/knowledge"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	knowledgestore "github.com/Pierre-Theophile/aisre/pkg/knowledge"
)

// Published names (ADR-0013 item 1).
const (
	Kind           = "notion"
	SourceIDPrefix = "notion:"
	NSPage         = "notion.page"
	Rule           = "notion/notion-db@1"
)

// The skip reasons of the registration report (contracts §6). A closed set.
const (
	SkipNoMappedEntity = "no mapped entity"
	SkipUnmappedValue  = "unmapped value"
	SkipOtherDatabase  = "in another bound database"
	SkipUntitled       = "untitled"
)

// Options configures the feeder.
type Options struct {
	Config *Config
	Logger *slog.Logger
}

// Feeder turns reads of Notion data sources into registrations.
type Feeder struct {
	cfg  *Config
	desc feeder.Description
	log  *slog.Logger

	mu     sync.Mutex
	report Report
}

// New builds a feeder.
func New(opts Options) (*Feeder, error) {
	if opts.Config == nil {
		return nil, errors.New("notion: a feeder with no configuration")
	}
	if err := opts.Config.Validate(); err != nil {
		return nil, err
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	f := &Feeder{cfg: opts.Config, log: logger}
	f.desc = feeder.Description{
		SourceID: SourceIDPrefix + opts.Config.Workspace,
		Kind:     Kind,
		Ordering: feeder.OrderingNone,
		// None: the poller reads one data source at a time and Run emits each read before it asks
		// for the next, through one emitter, so events are delivered in the order they were read.
		// Their valid instants are Notion's own edit instants and never the delivery's.
		ReorderingWindow: 0,
		RequiredScopes:   []string{"Read content (internal integration), shared with each configured database"},
	}
	f.report = newReport(opts.Config)
	return f, nil
}

// Describe returns the feeder's contract.
func (f *Feeder) Describe() feeder.Description { return f.desc }

// Run reads payloads from src and writes events to em.
func (f *Feeder) Run(ctx context.Context, src feeder.Source, em feeder.Emitter) error {
	if err := f.desc.Validate(); err != nil {
		return err
	}
	f.mu.Lock()
	f.report = newReport(f.cfg)
	f.mu.Unlock()
	defer func() {
		if err := em.Flush(context.WithoutCancel(ctx)); err != nil {
			f.log.ErrorContext(ctx, "flush failed", "error", err)
		}
	}()
	for {
		payload, err := src.Next(ctx)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := f.apply(ctx, em, payload); err != nil {
			return err
		}
	}
}

func (f *Feeder) apply(ctx context.Context, em feeder.Emitter, payload feeder.Payload) error {
	if payload.Kind != PayloadPages {
		return fmt.Errorf("notion: payload kind %q is not one this feeder reads (%s)", payload.Kind, PayloadPages)
	}
	var read pagesPayload
	if err := json.Unmarshal(payload.Bytes, &read); err != nil {
		return fmt.Errorf("notion: decoding a %s payload: %w", payload.Kind, err)
	}
	if read.Binding < 0 || read.Binding >= len(f.cfg.Databases) {
		return fmt.Errorf("notion: a read of binding %d, but the configuration has %d database(s)", read.Binding, len(f.cfg.Databases))
	}
	binding := &f.cfg.Databases[read.Binding]
	at := payload.At.UTC()

	pages := append([]observedPage(nil), read.Pages...)
	sort.SliceStable(pages, func(i, j int) bool {
		if !pages[i].Edited.Equal(pages[j].Edited) {
			return pages[i].Edited.Before(pages[j].Edited)
		}
		return pages[i].ID < pages[j].ID
	})
	for _, page := range pages {
		if err := f.observe(ctx, em, binding, read, page, at); err != nil {
			return err
		}
	}

	f.mu.Lock()
	f.report.Reads++
	d := &f.report.Databases[read.Binding]
	if read.DataSource != "" && !slices.Contains(d.DataSources, read.DataSource) {
		d.DataSources = append(d.DataSources, read.DataSource)
	}
	for _, name := range read.Properties {
		d.PropertiesFound[name] = true
	}
	if read.Mode == ModeReconcile && read.DataSource != "" {
		// A reconciliation lists every page, so its count replaces the last one.
		d.outside[read.DataSource] = read.Outside
	}
	if !read.Complete {
		f.report.Gaps = append(f.report.Gaps, fmt.Sprintf("%s: %s", binding.Database, read.Gap))
	}
	if read.Mode == ModeReconcile && read.Complete {
		f.report.LastReconciled = at
	}
	note := f.report.noteLocked()
	var skipped []string
	if read.Mode == ModeReconcile && read.EndOfCycle {
		skipped = f.report.skippedLocked()
	}
	f.mu.Unlock()

	if read.Mode == ModeReconcile && read.EndOfCycle {
		// The registration report at the end of each reconciliation (contracts §6, T033): counts, then
		// each page not registered with its title and reason. Never a summary or a body.
		f.log.InfoContext(ctx, "notion: reconciliation report", "report", note)
		for _, line := range skipped {
			f.log.InfoContext(ctx, "notion: not registered", "page", line)
		}
	}

	from := read.From
	if from.IsZero() || !from.Before(at) {
		from = at.Add(-f.cfg.PollInterval)
	}
	return em.Checkpoint(ctx, feeder.CheckpointFact{
		ExtentFrom: from,
		ExtentTo:   at,
		GapBefore:  !read.Complete,
		Note:       note,
	})
}

func (f *Feeder) observe(ctx context.Context, em feeder.Emitter, b *Binding, read pagesPayload, page observedPage, at time.Time) error {
	if strings.TrimSpace(page.ID) == "" {
		return errors.New("notion: a page with no id; the document reference rests on it")
	}
	ref := feeder.Ref(NSPage, page.ID)
	previous := map[string]*graphv1.Ref{}
	for _, spelled := range page.Previous {
		r, err := graph.ParseRef(spelled)
		if err != nil {
			return fmt.Errorf("notion: page %s: a previous link %q: %w", page.ID, spelled, err)
		}
		previous[spelled] = r.Proto()
	}

	// A registered page gone from its data source: its links end, the node stays (research R6).
	if page.Gone != "" {
		for _, key := range sortedKeys(previous) {
			if err := f.retract(ctx, em, ref, previous[key], page.GoneAt, page.Gone); err != nil {
				return err
			}
		}
		// Its content goes with it: the sections close at this read, so an as-of read before it still
		// finds them.
		if err := f.putSections(ctx, em, read.Binding, ref, page, nil); err != nil {
			return err
		}
		f.count(read.Binding, page, func(c *DatabaseReport) { c.Gone++ })
		return nil
	}
	if page.Conflict != "" {
		f.skip(read.Binding, page, SkipOtherDatabase+" "+page.Conflict)
		return nil
	}

	mapped, unmapped := b.Map(page.Values)
	for _, v := range unmapped {
		f.skip(read.Binding, page, SkipUnmappedValue+" "+v)
	}
	current := map[string]*graphv1.Ref{}
	for _, r := range mapped {
		current[graph.RefFromProto(r).String()] = r
	}

	if len(current) == 0 {
		// Nothing links the page any more: the links it had end at the edit that removed them.
		for _, key := range sortedKeys(previous) {
			if err := f.retract(ctx, em, ref, previous[key], page.Edited, "value removed"); err != nil {
				return err
			}
		}
		if len(unmapped) == 0 {
			f.skip(read.Binding, page, SkipNoMappedEntity)
		}
		return nil
	}

	// First registration is valid from the page's creation; every later change from its edit.
	validAt := page.Edited
	if !page.Registered || validAt.IsZero() {
		validAt = page.Created
	}
	if validAt.IsZero() {
		validAt = at
	}
	validAt = validAt.UTC().Truncate(time.Second)

	summary, truncated := summarise(page.Parts)
	request := knowledge.LinkRequest{
		DocumentRef:      ref,
		Kind:             b.Kind,
		Title:            strings.TrimSpace(page.Title),
		Summary:          summary,
		Location:         pageURL(page),
		EntityRefs:       mapped,
		RegisteredBy:     f.desc.SourceID,
		AuthoredAt:       page.Author,
		ConfiguredBy:     f.cfg.Principal,
		SummaryTruncated: truncated,
		Provenance:       Rule + " configured by " + f.cfg.Principal,
	}
	if request.AuthoredAt.IsZero() {
		request.AuthoredAt = page.Created
	}
	if err := request.Validate(); err != nil {
		f.skip(read.Binding, page, SkipUntitled)
		return nil //nolint:nilerr // an untitled page is reported, not fatal
	}
	nodeProps := request.Props(at)
	props, err := structpb.NewStruct(nodeProps)
	if err != nil {
		return fmt.Errorf("notion: page %s: properties: %w", page.ID, err)
	}
	digest := contentDigest(request)
	if err := emitOne(ctx, em, feeder.UpsertNode(f.desc, f.id(ref, "node", digest), feeder.NodeFact{
		Meta:        feeder.Meta{SourceObservedAt: at},
		Ref:         ref,
		Type:        graphv1.NodeType_KNOWLEDGE_DOC,
		DisplayName: request.Title,
		Props:       props,
		ValidAt:     validAt,
	})); err != nil {
		return err
	}

	values := valuesByRef(b, page.Values)
	for _, key := range sortedKeys(current) {
		if _, had := previous[key]; had {
			continue
		}
		edgeProps := map[string]any{}
		for k, v := range nodeProps {
			edgeProps[k] = v
		}
		edgeProps["rule"] = Rule
		edgeProps["database"] = read.Database
		edgeProps["property"] = b.EntityProperty
		edgeProps["property_value"] = values[key]
		eprops, err := structpb.NewStruct(edgeProps)
		if err != nil {
			return fmt.Errorf("notion: page %s: edge properties: %w", page.ID, err)
		}
		if err := emitOne(ctx, em, feeder.UpsertEdge(f.desc, f.id(ref, "edge", key, validAt.Format(time.RFC3339)), feeder.EdgeFact{
			Meta:    feeder.Meta{SourceObservedAt: at},
			Src:     ref,
			Dst:     current[key],
			Type:    graphv1.EdgeType_CONCERNS,
			Props:   eprops,
			ValidAt: validAt,
		})); err != nil {
			return err
		}
	}
	for _, key := range sortedKeys(previous) {
		if _, still := current[key]; still {
			continue
		}
		if err := f.retract(ctx, em, ref, previous[key], page.Edited, "value removed"); err != nil {
			return err
		}
	}
	if page.SectionsRead {
		// After the page's events, so the graph registers the document before its content arrives.
		if err := f.putSections(ctx, em, read.Binding, ref, page, toKnowledge(page.Sections)); err != nil {
			return err
		}
	}
	f.count(read.Binding, page, func(c *DatabaseReport) { c.Registered[page.ID] = request.Title })
	return nil
}

// putSections sends a page's whole current set of sections when the emitter carries content
// (008 research R7), and records them in the report. An emitter that does not is a graph without
// a knowledge store: the page is registered by its summary alone, as in 007.
func (f *Feeder) putSections(ctx context.Context, em feeder.Emitter, binding int, ref *graphv1.Ref, page observedPage,
	sections []knowledgestore.Section) error {
	if page.Gone == "" {
		f.mu.Lock()
		f.report.Databases[binding].Sections[page.ID] = newPageSections(page)
		f.mu.Unlock()
	}
	se, ok := em.(feeder.SectionsEmitter)
	if !ok {
		return nil
	}
	if _, err := se.PutSections(ctx, graph.RefFromProto(ref).String(), sections); err != nil {
		return fmt.Errorf("notion: page %s: sections: %w", page.ID, err)
	}
	return nil
}

func (f *Feeder) retract(ctx context.Context, em feeder.Emitter, src, dst *graphv1.Ref, end time.Time, why string) error {
	if end.IsZero() {
		return fmt.Errorf("notion: ending %s → %s (%s) with no instant; a link never ends at \"now\" (research R6)",
			graph.RefFromProto(src), graph.RefFromProto(dst), why)
	}
	end = end.UTC().Truncate(time.Second)
	key := graph.RefFromProto(dst).String()
	return emitOne(ctx, em, feeder.RetractEdge(f.desc, f.id(src, "end", key, end.Format(time.RFC3339)), feeder.EdgeRetraction{
		Src: src, Dst: dst, Type: graphv1.EdgeType_CONCERNS, ValidEnd: end,
	}))
}

// id is a deterministic event id: the source, then a digest of the reference and the parts (data
// model §Event ids). A re-read of an unchanged page mints ids the log already holds (FR-041).
func (f *Feeder) id(ref *graphv1.Ref, parts ...string) string {
	h := sha256.New()
	h.Write([]byte(f.desc.SourceID))
	h.Write([]byte{0})
	h.Write([]byte(graph.RefFromProto(ref).String()))
	for _, p := range parts {
		h.Write([]byte{0})
		h.Write([]byte(p))
	}
	return feeder.NewID(f.desc.SourceID, parts[0], hex.EncodeToString(h.Sum(nil))[:32])
}

// contentDigest covers what a citation shows: title, summary, kind, location and authored instant.
func contentDigest(r knowledge.LinkRequest) string {
	h := sha256.New()
	for _, s := range []string{r.Title, r.Summary, r.Kind, r.Location, r.AuthoredAt.UTC().Format(time.RFC3339)} {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func valuesByRef(b *Binding, values []string) map[string]string {
	out := map[string]string{}
	for _, v := range values {
		refs, _ := b.Map([]string{v})
		for _, r := range refs {
			key := graph.RefFromProto(r).String()
			if _, ok := out[key]; !ok {
				out[key] = v
			}
		}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func emitOne(ctx context.Context, em feeder.Emitter, ev *graphv1.EventEnvelope) error {
	result, err := em.Emit(ctx, ev)
	if err != nil {
		return err
	}
	if result.GetStatus() == graphv1.IngestResult_REJECTED {
		return fmt.Errorf("notion: the graph refused event %s: %s %s",
			ev.GetEventId(), result.GetReasonCode(), result.GetReasonDetail())
	}
	return nil
}

// ---- the registration report (contracts §6) ------------------------------------------------------

// Report is the run's account of itself.
type Report struct {
	Reads          int
	LastReconciled time.Time
	Gaps           []string
	Databases      []DatabaseReport
}

// Skip is one page not registered.
type Skip struct {
	Title  string
	Reason string
}

// DatabaseReport is one binding's line.
type DatabaseReport struct {
	Database string
	Kind     string
	// Registered maps page id → title, for the pages linked as of the last read of each.
	Registered map[string]string
	// Skipped maps page id → the page's title and why it is not registered, as of its last read.
	Skipped map[string]Skip
	Gone    int
	// DataSources are the data sources read; PropertiesFound the configured property names some
	// listed page carried.
	DataSources     []string
	PropertiesFound map[string]bool
	// Sections maps page id → what was indexed of the page, for the pages whose sections were read
	// this run (contracts §6). Labels, origins and sizes only; never text.
	Sections map[string]PageSections
	// outside is, per data source, the listed pages whose parent is not a configured database, as of
	// its last reconciliation.
	outside map[string]int
}

// PageSections is what was indexed of one page.
type PageSections struct {
	Title    string
	Sections []SectionLine
	Skipped  []SkippedLine
}

// SectionLine is one section: its label, origin, how many parts and characters.
type SectionLine struct {
	Label  string
	Origin string
	Parts  int
	Chars  int
}

// SkippedLine is something of a page not indexed: a section's label, or a count of blocks, and why.
type SkippedLine struct {
	Label  string
	Reason string
	Count  int
}

// String renders the line as contracts §6 prints it.
func (s SectionLine) String() string {
	parts := ""
	if s.Parts > 1 {
		parts = fmt.Sprintf(", %d parts", s.Parts)
	}
	return fmt.Sprintf("%s (%s, %d chars%s)", s.Label, s.Origin, s.Chars, parts)
}

// String renders the line as contracts §6 prints it.
func (s SkippedLine) String() string {
	switch {
	case s.Count > 0:
		return fmt.Sprintf("%s: %d", s.Reason, s.Count)
	case s.Label != "":
		return fmt.Sprintf("%s: %s", s.Label, s.Reason)
	default:
		return s.Reason
	}
}

func newPageSections(page observedPage) PageSections {
	out := PageSections{Title: page.Title}
	if out.Title == "" {
		out.Title = "(untitled)"
	}
	for _, part := range page.Sections {
		n := len(out.Sections)
		if n > 0 && out.Sections[n-1].Label == part.Label {
			out.Sections[n-1].Parts++
			out.Sections[n-1].Chars += utf8.RuneCountInString(part.Text)
			continue
		}
		out.Sections = append(out.Sections, SectionLine{Label: part.Label, Origin: part.Origin, Parts: 1,
			Chars: utf8.RuneCountInString(part.Text)})
	}
	for _, s := range page.Skipped {
		out.Skipped = append(out.Skipped, SkippedLine(s))
	}
	return out
}

// Outside is how many listed pages are outside the configured databases (a count only).
func (d DatabaseReport) Outside() int {
	n := 0
	for _, c := range d.outside {
		n += c
	}
	return n
}

func newReport(cfg *Config) Report {
	r := Report{}
	for _, b := range cfg.Databases {
		r.Databases = append(r.Databases, DatabaseReport{
			Database: b.Database, Kind: b.Kind, Registered: map[string]string{}, Skipped: map[string]Skip{},
			PropertiesFound: map[string]bool{}, Sections: map[string]PageSections{}, outside: map[string]int{},
		})
	}
	return r
}

func (f *Feeder) skip(binding int, page observedPage, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	title := page.Title
	if title == "" {
		title = "(untitled)"
	}
	d := &f.report.Databases[binding]
	d.Skipped[page.ID] = Skip{Title: title, Reason: reason}
	delete(d.Registered, page.ID)
}

func (f *Feeder) count(binding int, page observedPage, update func(*DatabaseReport)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := &f.report.Databases[binding]
	delete(d.Skipped, page.ID)
	if page.Gone != "" {
		delete(d.Registered, page.ID)
	}
	update(d)
}

// Report returns a copy of the report so far.
func (f *Feeder) Report() Report {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.report
	out.Gaps = append([]string(nil), f.report.Gaps...)
	out.Databases = make([]DatabaseReport, len(f.report.Databases))
	for i, d := range f.report.Databases {
		c := DatabaseReport{Database: d.Database, Kind: d.Kind, Gone: d.Gone,
			Registered: maps.Clone(d.Registered), Skipped: maps.Clone(d.Skipped),
			DataSources: slices.Clone(d.DataSources), PropertiesFound: maps.Clone(d.PropertiesFound),
			Sections: maps.Clone(d.Sections), outside: maps.Clone(d.outside)}
		out.Databases[i] = c
	}
	return out
}

// skippedLocked lists each page not registered as `<database>: "<title>": <reason>`, sorted.
func (r Report) skippedLocked() []string {
	var out []string
	for _, d := range r.Databases {
		for _, s := range d.Skipped {
			out = append(out, fmt.Sprintf("%s: %q: %s", d.Database, s.Title, s.Reason))
		}
	}
	sort.Strings(out)
	return out
}

// Note renders the report for a checkpoint and a log line: counts only, never a title.
func (r Report) Note() string { return r.noteLocked() }

func (r Report) noteLocked() string {
	var lines []string
	for _, d := range r.Databases {
		reasons := map[string]int{}
		for _, skip := range d.Skipped {
			reason := skip.Reason
			if i := strings.Index(reason, " "); i > 0 && strings.HasPrefix(reason, SkipUnmappedValue) {
				reason = SkipUnmappedValue
			} else if strings.HasPrefix(reason, SkipOtherDatabase) {
				reason = SkipOtherDatabase
			}
			reasons[reason]++
		}
		var skipped []string
		for _, k := range sortedKeys(reasons) {
			skipped = append(skipped, fmt.Sprintf("%s: %d", k, reasons[k]))
		}
		s := "none"
		if len(skipped) > 0 {
			s = strings.Join(skipped, ", ")
		}
		lines = append(lines, fmt.Sprintf("%s (%s): %d page(s) registered, %d gone; skipped: %s",
			d.Database, d.Kind, len(d.Registered), d.Gone, s))
	}
	gaps := ""
	if len(r.Gaps) > 0 {
		gaps = fmt.Sprintf("; %d coverage gap(s)", len(r.Gaps))
	}
	return fmt.Sprintf("notion knowledge: %d read(s)%s; %s", r.Reads, gaps, strings.Join(lines, "; "))
}
