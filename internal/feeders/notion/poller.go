// SPDX-License-Identifier: Apache-2.0

package notion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/notionx"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The live source (007 T020–T023; research R5–R7): one payload per read of one data source.
//
// An incremental poll queries each data source for pages edited on or after the watermark less two
// minutes, ascending; a reconciliation lists every page and reads, once, each registered page the
// list no longer returns, to tell trashed or moved (links end at its last edit) from gone (links end
// when it was last seen) from a read that failed (a coverage gap, nothing ends).
//
// The poller keeps what it registered in a state file — per page the binding, the entity refs it was
// linked to and when it was last seen; per data source the watermark — so that a restart still
// notices a page that disappeared while it was stopped. A payload's state is committed when the next
// one is asked for: Run asks for the next payload only after the last one's events were accepted, so
// a crash between the two re-reads rather than forgets.

// Reader is what the poller reads Notion with. *notionx.Client satisfies it.
type Reader interface {
	Database(ctx context.Context, id string) (notionx.Database, error)
	QueryDataSource(ctx context.Context, id string, filter notionx.QueryFilter, cursor string) (notionx.QueryPage, error)
	Page(ctx context.Context, id string) (notionx.Page, error)
	PropertyItems(ctx context.Context, pageID, propertyID string, stop func(notionx.Property) bool) (notionx.Property, error)
}

// Overlap is how far before the watermark an incremental poll reads, because Notion rounds
// last_edited_time to the minute (research R6). Event ids are content digests, so the overlap
// re-emits nothing.
const Overlap = 2 * time.Minute

// MaxPages bounds one read of one data source, in pages of 100.
const MaxPages = 200

// PollerOptions configures the poller.
type PollerOptions struct {
	Client Reader
	Config *Config
	// StatePath is where the poller keeps what it registered. Empty keeps it in memory only.
	StatePath string
	// ReadOnlyState loads the state file but never writes it: `--check` compares against what the
	// connector registered without moving anything.
	ReadOnlyState bool
	// Once makes one reconciliation and stops.
	Once bool
	// Cycles stops after that many cycles; 0 runs until the context ends.
	Cycles int
	Now    func() time.Time
	Sleep  func(ctx context.Context, d time.Duration) error
	Logger *slog.Logger
}

// Poller is a feeder.Source over the configured databases.
type Poller struct {
	opts   PollerOptions
	logger *slog.Logger

	state   *State
	pending *State // the state as it will be once the last payload returned is accepted

	queue         []feeder.Payload
	resolved      bool
	sources       [][]string // per binding, its data source ids
	nextPoll      time.Time
	nextReconcile time.Time
	cycles        int
}

var _ feeder.Source = (*Poller)(nil)

// NewPoller builds a poller, loading its state file when there is one.
func NewPoller(opts PollerOptions) (*Poller, error) {
	if opts.Client == nil || opts.Config == nil {
		return nil, errors.New("notion: a poller needs a client and a configuration")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Sleep == nil {
		opts.Sleep = func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		}
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	state, err := LoadState(opts.StatePath)
	if err != nil {
		return nil, err
	}
	return &Poller{opts: opts, logger: logger, state: state}, nil
}

// Next returns the next read. It commits the state of the payload before it.
func (p *Poller) Next(ctx context.Context) (feeder.Payload, error) {
	if p.pending != nil {
		p.state = p.pending
		p.pending = nil
		if !p.opts.ReadOnlyState {
			if err := p.state.Save(p.opts.StatePath); err != nil {
				return feeder.Payload{}, err
			}
		}
	}
	for len(p.queue) == 0 {
		if (p.opts.Once && p.cycles > 0) || (p.opts.Cycles > 0 && p.cycles >= p.opts.Cycles) {
			return feeder.Payload{}, io.EOF
		}
		now := p.opts.Now().UTC()
		if p.cycles > 0 {
			due := p.nextPoll
			if p.nextReconcile.Before(due) {
				due = p.nextReconcile
			}
			if wait := due.Sub(now); wait > 0 {
				if err := p.opts.Sleep(ctx, wait); err != nil {
					return feeder.Payload{}, err
				}
				now = p.opts.Now().UTC()
			}
		}
		if err := p.cycle(ctx, now); err != nil {
			return feeder.Payload{}, err
		}
	}
	payload := p.queue[0]
	p.queue = p.queue[1:]
	return payload, nil
}

// cycle reads every binding once and queues the payloads. The state it computes becomes pending
// with the last payload of the cycle.
func (p *Poller) cycle(ctx context.Context, now time.Time) error {
	reconcile := p.cycles == 0 || !now.Before(p.nextReconcile)
	if err := p.resolve(ctx); err != nil {
		return err
	}
	next := p.state.clone()
	claimed := map[string]int{} // page id → binding that holds it this cycle
	type read struct {
		out pagesPayload
		at  time.Time
	}
	var reads []read
	for i := range p.opts.Config.Databases {
		for _, ds := range p.sources[i] {
			out, at := p.read(ctx, i, ds, reconcile, next, claimed)
			reads = append(reads, read{out, at})
		}
	}
	// The last read says the cycle is over, so the feeder can report a reconciliation once, whole.
	reads[len(reads)-1].out.EndOfCycle = true
	for _, r := range reads {
		payload, err := marshal(r.out, r.at)
		if err != nil {
			return err
		}
		p.queue = append(p.queue, payload)
	}
	p.cycles++
	p.nextPoll = now.Add(p.opts.Config.PollInterval)
	if reconcile {
		p.nextReconcile = now.Add(p.opts.Config.ReconcileEvery)
	}
	p.pending = next
	return nil
}

// resolve reads each configured database's data sources, once per process.
func (p *Poller) resolve(ctx context.Context) error {
	if p.resolved {
		return nil
	}
	p.sources = make([][]string, len(p.opts.Config.Databases))
	for i, b := range p.opts.Config.Databases {
		db, err := p.opts.Client.Database(ctx, b.Database)
		if err != nil {
			// A database that cannot be read is a coverage gap for it, not a stop for the others.
			p.logger.WarnContext(ctx, "notion: a configured database could not be read", "database", b.Database, "error", err.Error())
			p.sources[i] = []string{""}
			continue
		}
		for _, ds := range db.DataSources {
			p.sources[i] = append(p.sources[i], NormaliseID(ds.ID))
		}
		if len(p.sources[i]) == 0 {
			p.sources[i] = []string{""}
		}
	}
	p.resolved = true
	return nil
}

// read reads one data source of one binding.
func (p *Poller) read(ctx context.Context, binding int, ds string, reconcile bool, next *State,
	claimed map[string]int) (pagesPayload, time.Time) {
	b := &p.opts.Config.Databases[binding]
	at := p.opts.Now().UTC()
	out := pagesPayload{Workspace: p.opts.Config.Workspace, Binding: binding, Database: b.Database, DataSource: ds,
		Mode: ModeIncremental, Complete: true, Pages: []observedPage{}}
	if reconcile {
		out.Mode = ModeReconcile
	}
	if ds == "" {
		out.Complete, out.Gap = false, "the database could not be read"
		return out, at
	}
	filter := notionx.QueryFilter{}
	if !reconcile {
		if wm := next.Watermarks[ds]; !wm.IsZero() {
			filter.EditedOnOrAfter = wm.Add(-Overlap)
			out.From = filter.EditedOnOrAfter
		}
	}

	listed := map[string]bool{}
	cursor := ""
	for range MaxPages {
		got, err := p.opts.Client.QueryDataSource(ctx, ds, filter, cursor)
		if err != nil {
			out.Complete, out.Gap = false, gapOf(err)
			break
		}
		for _, page := range got.Pages {
			id := NormaliseID(page.ID)
			listed[id] = true
			for _, name := range b.properties() {
				if _, ok := page.Properties[name]; ok && !slices.Contains(out.Properties, name) {
					out.Properties = append(out.Properties, name)
				}
			}
			if !p.bound(page.Parent.DataSourceID) {
				out.Outside++
			}
			obs, err := p.observe(ctx, b, page, next)
			if err != nil {
				out.Complete, out.Gap = false, gapOf(err)
			}
			if holder := p.holder(id, page, binding, claimed); holder != binding {
				// Another binding holds the page: the one whose data source is its parent, else the
				// first to list it this cycle. This read neither registers nor ends anything for it,
				// so a read of the other database cut short cannot make this one end its links.
				obs.Conflict = p.opts.Config.Databases[holder].Database
			} else {
				claimed[id] = binding
				p.commit(next, binding, ds, obs, at)
			}
			out.Pages = append(out.Pages, obs)
			if page.LastEditedTime.After(next.Watermarks[ds]) {
				next.Watermarks[ds] = page.LastEditedTime.UTC()
			}
		}
		if !got.HasMore || got.NextCursor == "" {
			cursor = ""
			break
		}
		cursor = got.NextCursor
	}
	if cursor != "" {
		out.Complete, out.Gap = false, fmt.Sprintf("more than %d pages of results", MaxPages)
	}

	if reconcile && out.Complete {
		gone, err := p.absent(ctx, ds, listed, next)
		if err != nil {
			out.Complete, out.Gap = false, gapOf(err)
		}
		out.Pages = append(out.Pages, gone...)
	}
	return out, at
}

// bound reports a data source of a configured database. A page listed by a bound data source
// whose parent is another (a linked view) is outside the configured databases.
func (p *Poller) bound(dataSource string) bool {
	parent := NormaliseID(dataSource)
	if parent == "" {
		return true
	}
	for _, sources := range p.sources {
		if slices.Contains(sources, parent) {
			return true
		}
	}
	return false
}

// holder is the binding a page belongs to.
func (p *Poller) holder(id string, page notionx.Page, binding int, claimed map[string]int) int {
	parent := NormaliseID(page.Parent.DataSourceID)
	for i, sources := range p.sources {
		for _, ds := range sources {
			if ds != "" && ds == parent {
				return i
			}
		}
	}
	if holder, ok := claimed[id]; ok {
		return holder
	}
	return binding
}

// observe reduces one page to what the payload carries, completing truncated named properties.
func (p *Poller) observe(ctx context.Context, b *Binding, page notionx.Page, st *State) (observedPage, error) {
	id := NormaliseID(page.ID)
	obs := observedPage{ID: id, URL: page.URL, Created: page.CreatedTime.UTC(), Edited: page.LastEditedTime.UTC()}
	if prev, ok := st.Pages[id]; ok {
		obs.Registered = true
		obs.Previous = append([]string(nil), prev.Refs...)
	}
	for _, prop := range page.Properties {
		if prop.Type == "title" {
			obs.Title = prop.Text()
		}
	}
	var firstErr error
	complete := func(name string, stop func(notionx.Property) bool) notionx.Property {
		prop := page.Properties[name]
		if !prop.Truncated() || prop.ID == "" {
			return prop
		}
		full, err := p.opts.Client.PropertyItems(ctx, page.ID, prop.ID, stop)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			return prop
		}
		return full
	}
	for _, name := range b.SummaryProperties {
		prop := complete(name, func(pr notionx.Property) bool { return len(pr.Text()) > SummaryCap })
		if t := prop.Text(); t != "" {
			obs.Parts = append(obs.Parts, part{Property: name, Text: t})
		}
	}
	obs.Values = complete(b.EntityProperty, nil).Values()
	if b.AuthoredProperty != "" {
		if d := page.Properties[b.AuthoredProperty].Date; d != nil {
			obs.Author = parseDate(d.Start)
		}
	}
	return obs, firstErr
}

// commit records what a payload page will leave registered.
func (p *Poller) commit(next *State, binding int, ds string, obs observedPage, at time.Time) {
	b := &p.opts.Config.Databases[binding]
	refs, _ := b.Map(obs.Values)
	if len(refs) == 0 {
		if prev, ok := next.Pages[obs.ID]; ok {
			prev.Refs, prev.LastSeen = nil, at
			next.Pages[obs.ID] = prev
		}
		return
	}
	var spelled []string
	for _, r := range refs {
		spelled = append(spelled, graph.RefFromProto(r).String())
	}
	sort.Strings(spelled)
	next.Pages[obs.ID] = PageState{Database: b.Database, DataSource: ds, Refs: spelled, LastSeen: at}
}

// absent reads each registered page of the data source the list did not return.
func (p *Poller) absent(ctx context.Context, ds string, listed map[string]bool, next *State) ([]observedPage, error) {
	bound := map[string]bool{}
	for _, sources := range p.sources {
		for _, s := range sources {
			bound[s] = true
		}
	}
	var out []observedPage
	for _, id := range sortedKeys(next.Pages) {
		st := next.Pages[id]
		if st.DataSource != ds || listed[id] || len(st.Refs) == 0 {
			continue
		}
		obs := observedPage{ID: id, Registered: true, Previous: append([]string(nil), st.Refs...)}
		page, err := p.opts.Client.Page(ctx, id)
		var apiErr *notionx.APIError
		switch {
		case errors.As(err, &apiErr) && apiErr.NotFound():
			obs.Gone, obs.GoneAt = GoneNotFound, st.LastSeen
		case err != nil:
			return out, err
		case page.Gone():
			obs.Gone, obs.GoneAt = GoneTrashed, page.LastEditedTime.UTC()
		case !bound[NormaliseID(page.Parent.DataSourceID)]:
			obs.Gone, obs.GoneAt = GoneMoved, page.LastEditedTime.UTC()
		default:
			// Moved to another bound data source: that read registers it. Nothing ends here.
			continue
		}
		obs.Title = ""
		out = append(out, obs)
		st.Refs = nil
		next.Pages[id] = st
	}
	return out, nil
}

func gapOf(err error) string {
	if partial, ok := notionx.IsPartial(err); ok {
		return "cut short by Notion's rate limit until " + partial.RetryAt.UTC().Format(time.RFC3339)
	}
	var apiErr *notionx.APIError
	if errors.As(err, &apiErr) {
		return fmt.Sprintf("Notion answered %d %s", apiErr.Status, apiErr.Code)
	}
	return "the read failed"
}

func parseDate(s string) time.Time {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

func marshal(out pagesPayload, at time.Time) (feeder.Payload, error) {
	raw, err := json.Marshal(out)
	if err != nil {
		return feeder.Payload{}, err
	}
	return feeder.Payload{Kind: PayloadPages, At: at, Bytes: raw}, nil
}

// ---- state ---------------------------------------------------------------------------------------

// State is what the poller registered.
type State struct {
	Pages      map[string]PageState `json:"pages"`
	Watermarks map[string]time.Time `json:"watermarks"`
}

// PageState is one page's line.
type PageState struct {
	Database   string    `json:"database"`
	DataSource string    `json:"data_source"`
	Refs       []string  `json:"refs,omitempty"`
	LastSeen   time.Time `json:"last_seen"`
}

// LoadState reads a state file; a missing one is an empty state.
func LoadState(path string) (*State, error) {
	st := &State{Pages: map[string]PageState{}, Watermarks: map[string]time.Time{}}
	if path == "" {
		return st, nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, st); err != nil {
		return nil, fmt.Errorf("notion: state file %s: %w", path, err)
	}
	if st.Pages == nil {
		st.Pages = map[string]PageState{}
	}
	if st.Watermarks == nil {
		st.Watermarks = map[string]time.Time{}
	}
	return st, nil
}

// Save writes the state atomically. An empty path keeps it in memory.
func (s *State) Save(path string) error {
	if path == "" {
		return nil
	}
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".notion-state-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func (s *State) clone() *State {
	out := &State{Pages: make(map[string]PageState, len(s.Pages)), Watermarks: make(map[string]time.Time, len(s.Watermarks))}
	for k, v := range s.Pages {
		v.Refs = append([]string(nil), v.Refs...)
		out.Pages[k] = v
	}
	for k, v := range s.Watermarks {
		out.Watermarks[k] = v
	}
	return out
}

// PreparePayload is the pre-pass a recording runs before the sanitisation table (FR-050): the page
// URL is dropped, because it carries the page id in the clear and a replay rebuilds one from the
// pseudonymised id.
func PreparePayload() func(kind string, raw []byte) ([]byte, error) {
	return func(kind string, raw []byte) ([]byte, error) {
		if kind != PayloadPages {
			return nil, fmt.Errorf("notion: payload kind %q is not one this connector records", kind)
		}
		var read pagesPayload
		if err := json.Unmarshal(raw, &read); err != nil {
			return nil, fmt.Errorf("notion: decoding a %s payload: %w", kind, err)
		}
		for i := range read.Pages {
			read.Pages[i].URL = ""
		}
		return json.Marshal(read)
	}
}
