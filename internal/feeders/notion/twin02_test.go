// SPDX-License-Identifier: Apache-2.0

package notion_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Pierre-Theophile/aisre/internal/feeders/notion"
	"github.com/Pierre-Theophile/aisre/internal/notionx"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/emit"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/record"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/source"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/testkit"
	"github.com/Pierre-Theophile/aisre/pkg/knowledge"
)

// The Notion knowledge twin 02 (008 T026): a postmortem database whose pages carry long property
// sections and bodies with headings, toggles, a table, an embed and a child page, read by the real
// poller through the scripted double over the afternoon of knowledge-notion-01's incident. The
// payloads carry each page's sections, sanitised. Its postmortems are the three of
// knowledge-notion-01, by the same page ids, and the sections it records are that incident's
// knowledge store: the generator writes fixtures/incidents/knowledge-notion-02/documents.yaml from
// what the feeder sent.
//
// Regenerate with SRE_AGENT_GEN_FIXTURES=1, then `aisre fixture record fixtures/notion-knowledge-twin-02`
// rewrites the goldens.

const (
	fixture02Dir      = "fixtures/notion-knowledge-twin-02"
	incident02Docs    = "fixtures/incidents/knowledge-notion-02/documents.yaml"
	dbPostmortems02   = "4e5f6071-8293-44a4-b5c6-d7e8f90a1b2c"
	dsPostmortems02   = "5f607182-93a4-45b5-c6d7-e8f90a1b2c3d"
	pageMay           = "c3000000-0000-4000-8000-000000000001" // checkout and payments; the one to cite
	pageReporting     = "c3000000-0000-4000-8000-000000000002" // reporting only
	pageAfterTheAlert = "c3000000-0000-4000-8000-000000000003" // created at 14:35, after the alert
)

var t02 = time.Date(2026, 9, 1, 13, 0, 0, 0, time.UTC)

// Canary tokens planted where a person or a secret would be written in a postmortem: in a property,
// a heading, a paragraph and a toggle. None may survive into a payload, an event or the committed
// fixture (008 FR-013); scripts/check-no-secrets.sh fails the commit on any `sreagentcanary` token.
const (
	canaryEmail     = "sreagentcanary1a2b3c4d@canary-domain.co"
	canarySecret    = "sk-sreagentcanary5e6f7a8b9c0d"
	canaryMention02 = "@sreagentcanary7d8e9f0a"
)

// symptomsPastTheSummary are the words knowledge-notion-02's alert is matched by. They appear in the
// May postmortem only in its "Signals / Symptoms" property, after its 600th character: past where
// 007's summary was cut.
const symptomsPastTheSummary = "Envoy reported upstream_reset_before_response_started on the payments cluster and the " +
	"checkout pods logged ECONNRESET from the payments client; HikariCP warned that a connection was not " +
	"available after 30000ms."

func fixture02Config(t *testing.T) *notion.Config {
	t.Helper()
	cfg, err := notion.ParseConfig([]byte(`
workspace: twin
principal: oidc:operator-twin
acknowledge_read_only: [notion.capabilities]
poll_interval: 30m
reconcile_every: 24h
databases:
  - database: ` + dbPostmortems02 + `
    kind: postmortem
    entity_property: Services
    entities:
      Checkout: otel.service.name=checkout
      Payments: otel.service.name=payments
      Reporting: otel.service.name=reporting
    summary_properties: [Summary]
    index_properties: [Summary, Signals / Symptoms, Root cause, Resolution]
    authored_property: Incident date
`))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func para(id, s string) notionx.Block {
	return notionx.Block{ID: id, Type: "paragraph", RichText: text(s)}
}

func typed(id, typ, s string, children bool) notionx.Block {
	b := notionx.Block{ID: id, Type: typ, HasChildren: children}
	if s != "" {
		b.RichText = text(s)
	}
	return b
}

func row(id string, cells ...string) notionx.Block {
	b := notionx.Block{ID: id, Type: "table_row"}
	for _, c := range cells {
		b.Cells = append(b.Cells, text(c))
	}
	return b
}

func page02(id, title string, created time.Time, props map[string]notionx.Property) *notionx.Page {
	props["Name"] = titleProp(title)
	return &notionx.Page{ID: id, CreatedTime: created, LastEditedTime: created,
		Parent: notionx.Parent{Type: "data_source_id", DataSourceID: dsPostmortems02, DatabaseID: dbPostmortems02}, Properties: props}
}

func newTwin02() *twinWorld {
	w := newTwin()
	w.clock.now = t02
	w.pages, w.listedIn, w.mutations = map[string]*notionx.Page{}, map[string][]string{}, nil
	w.databases[dbPostmortems02] = notionx.Database{ID: dbPostmortems02, Title: "Postmortems",
		DataSources: []notionx.DataSourceRef{{ID: dsPostmortems02}}}
	may14 := time.Date(2026, 5, 14, 11, 0, 0, 0, time.UTC)

	// The May postmortem. Its symptoms property opens with a long account of what the dashboards
	// showed and names the distinctive symptoms only after its 600th character.
	opening := strings.Repeat("The checkout dashboard showed the error rate climbing from the usual background "+
		"to several percent within minutes of the deploy window, with latency rising in step. ", 5)
	may := page02(pageMay, "Checkout 5xx after a payments rollout", may14, map[string]notionx.Property{
		"Summary":            richProp("sum", "Checkout returned 5xx for 25 minutes after the payments rollout; rolled back."),
		"Signals / Symptoms": richProp("sig", opening+symptomsPastTheSummary),
		"Root cause":         richProp("rc", "Payments rev7 shipped a client with a one-second pool acquire timeout."),
		"Resolution":         richProp("res", "Rolled payments back to rev6; the leaked key "+canarySecret+" was rotated."),
		"Services":           multi("Checkout", "Payments"),
		"Incident date":      dateProp("2026-05-14"),
		// Never named in the binding, so never read.
		"Owner notes": richProp("own", "Owned by "+canaryEmail),
	})
	may.LastEditedTime = may14.Add(22 * time.Hour)
	w.pages[pageMay] = may
	w.listedIn[pageMay] = []string{dsPostmortems02}

	var timeline []notionx.Block
	for i := range 14 {
		timeline = append(timeline, typed(fmt.Sprintf("tl-%d", i), "bulleted_list_item",
			fmt.Sprintf("14:%02d the on-call engineer checked the payments dashboard again and posted an update in the incident channel.", 20+i), false))
	}
	mention := notionx.Block{ID: "esc-p", Type: "paragraph",
		RichText: append(text("Paged "+canaryEmail+" and "), mention(canaryMention02))}
	w.blocks[pageMay] = append(append([]notionx.Block{
		para("intro", "This postmortem follows the incident template."),
		typed("h-timeline", "heading_1", "Timeline", false),
	}, timeline...),
		typed("h-detection", "heading_1", "Detection", false),
		para("det-p", "The checkout error-rate monitor fired; the payments rollout had finished twelve minutes earlier."),
		typed("det-toggle", "toggle", "Alert details, raw key "+canarySecret, true),
		typed("embed", "embed", "", false),
		typed("image", "image", "", false),
		typed("h-escalated", "heading_2", "Escalated to "+canaryEmail, false),
		mention,
		typed("h-contact", "heading_2", "Contact", false),
		para("contact-p", canaryEmail),
		typed("h-followups", "heading_1", "Follow-ups", false),
		typed("fu-table", "table", "", true),
		typed("fu-tracker", "child_page", "", true),
		typed("fu-code", "code", "payments.client.poolAcquireTimeout=30s", false),
	)
	w.blocks["det-toggle"] = []notionx.Block{para("det-toggle-p", "Monitor 20110 on checkout, error rate above 5 % for 5 minutes.")}
	w.blocks["fu-table"] = []notionx.Block{row("fu-1", "Raise the pool acquire timeout", "done")}
	w.blocks["fu-tracker"] = []notionx.Block{para("tracker-p", "Another page's content, never read.")}

	// The reporting postmortem: the same words in a summary, about a service outside the alert's
	// subgraph.
	w.pages[pageReporting] = page02(pageReporting, "Reporting nightly: connection pool exhaustion and 5xx after a rollout",
		time.Date(2026, 6, 2, 9, 0, 0, 0, time.UTC), map[string]notionx.Property{
			"Summary":            richProp("sum", "The nightly reporting job exhausted its connection pool after a rollout and returned 5xx."),
			"Signals / Symptoms": richProp("sig", "Pool exhaustion warnings and 5xx from the reporting API."),
			"Services":           multi("Reporting"),
			"Incident date":      dateProp("2026-06-02"),
		})
	w.listedIn[pageReporting] = []string{dsPostmortems02}
	w.blocks[pageReporting] = []notionx.Block{
		typed("r-h", "heading_1", "Timeline", false),
		para("r-p", "02:10 the nightly job started failing; 02:40 the pool was resized."),
	}

	w.mutations = []mutation{
		{at: time.Date(2026, 9, 1, 14, 35, 0, 0, time.UTC), fn: func(w *twinWorld) {
			// Written after the alert at 14:32, with the same symptoms: never cited for it.
			p := page02(pageAfterTheAlert, "Checkout 5xx on 2026-09-01", time.Date(2026, 9, 1, 14, 35, 0, 0, time.UTC),
				map[string]notionx.Property{
					"Summary":            richProp("sum", "Draft: checkout 5xx during the payments rollout."),
					"Signals / Symptoms": richProp("sig", symptomsPastTheSummary),
					"Services":           multi("Checkout"),
					"Incident date":      dateProp("2026-09-01"),
				})
			w.pages[pageAfterTheAlert] = p
			w.listedIn[pageAfterTheAlert] = []string{dsPostmortems02}
			w.blocks[pageAfterTheAlert] = []notionx.Block{para("a-p", "Investigation in progress.")}
		}},
		{at: time.Date(2026, 9, 1, 14, 50, 0, 0, time.UTC), fn: func(w *twinWorld) {
			// A follow-up is closed in the May postmortem: its body is read again and one section changes.
			w.pages[pageMay].LastEditedTime = time.Date(2026, 9, 1, 14, 50, 0, 0, time.UTC)
			w.blocks["fu-table"] = append(w.blocks["fu-table"], row("fu-2", "Alert on pool acquire timeouts", "done"))
		}},
	}
	return w
}

// live02Payloads runs the real poller over twin 02: five cycles half an hour apart, from 13:00.
func live02Payloads(t *testing.T) ([]feeder.Payload, *twinWorld) {
	t.Helper()
	w := newTwin02()
	poller, err := notion.NewPoller(notion.PollerOptions{
		Client: w, Config: fixture02Config(t), Cycles: 5, Now: w.Now, Sleep: w.Sleep,
	})
	if err != nil {
		t.Fatal(err)
	}
	var out []feeder.Payload
	for {
		p, err := poller.Next(t.Context())
		if err != nil {
			break
		}
		raw, err := notion.PreparePayload()(p.Kind, p.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		p.Bytes = raw
		out = append(out, p)
	}
	return out, w
}

const description02 = "A synthetic postmortem database read over the afternoon of 2026-09-01, with sections (008). " +
	"`Checkout 5xx after a payments rollout` (May, checkout and payments) carries four indexed properties and a body " +
	"by heading: its `Signals / Symptoms` names ECONNRESET and upstream_reset_before_response_started only after " +
	"its 600th character; its `Timeline` is split into two parts; a toggle's child is read; an embed, an image and a " +
	"child page are reported unsupported; a `Contact` heading holding only an e-mail address is skipped as emptied " +
	"by sanitisation; e-mail addresses, a user mention and a secret planted in a property, a heading, a paragraph and " +
	"a toggle are redacted. At 14:50 a follow-up is added to its table and its sections are read and sent again. " +
	"`Reporting nightly` concerns reporting only. `Checkout 5xx on 2026-09-01` is written at 14:35 with the same " +
	"symptoms. Unedited pages are not read again."

const queries02 = `
queries:
  # 14:32, the alert: the May postmortem concerns checkout and payments.
  - name: checkout-at-the-alert
    kind: subgraph
    focus: otel.service.name=checkout
    valid_at: 2026-09-01T14:32:00Z
    observed_at: 2026-09-01T15:30:00Z
    hops: 1
    direction: both
  # 15:10: the postmortem written at 14:35 concerns checkout too.
  - name: checkout-after-the-alert
    kind: subgraph
    focus: otel.service.name=checkout
    valid_at: 2026-09-01T15:10:00Z
    observed_at: 2026-09-01T15:30:00Z
    hops: 1
    direction: both
`

// incidentDocuments is the knowledge-notion-02 documents file: the sections twin 02 sent for each page,
// put at the instant the feeder sent them.
type incidentDocuments struct {
	Documents []incidentDocument `yaml:"documents"`
}

type incidentDocument struct {
	Ref  string        `yaml:"ref"`
	Puts []incidentPut `yaml:"puts"`
}

type incidentPut struct {
	At       time.Time         `yaml:"at"`
	Sections []incidentSection `yaml:"sections"`
}

type incidentSection struct {
	Label  string `yaml:"label"`
	Part   int    `yaml:"part,omitempty"`
	Origin string `yaml:"origin"`
	Text   string `yaml:"text"`
}

func documentsOf(puts []emit.SectionsPut, at func(i int) time.Time) incidentDocuments {
	var out incidentDocuments
	index := map[string]int{}
	for i, put := range puts {
		j, ok := index[put.DocumentRef]
		if !ok {
			j = len(out.Documents)
			index[put.DocumentRef] = j
			out.Documents = append(out.Documents, incidentDocument{Ref: put.DocumentRef})
		}
		p := incidentPut{At: at(i)}
		for _, s := range put.Sections {
			p.Sections = append(p.Sections, incidentSection{Label: s.Label, Part: s.Part, Origin: string(s.Origin), Text: s.Text})
		}
		out.Documents[j].Puts = append(out.Documents[j].Puts, p)
	}
	return out
}

// run02 replays twin 02's payloads through the feeder into a memory emitter clocked by the payloads'
// own instants, and returns the emitter and each put's instant.
func run02(t *testing.T, payloads []feeder.Payload, wrap func(feeder.Source, feeder.Emitter) (feeder.Source, feeder.Emitter)) (*notion.Feeder, *emit.MemoryEmitter, []time.Time) {
	t.Helper()
	f, err := notion.New(notion.Options{Config: fixture02Config(t)})
	if err != nil {
		t.Fatal(err)
	}
	clock := &arrivalClock{}
	src := clock.wrap(source.NewSliceSource(payloads))
	memory := emit.NewMemoryEmitter(f.Describe(), emit.WithMemoryClock(clock.Now))
	var em feeder.Emitter = memory
	if wrap != nil {
		src, em = wrap(src, em)
	}
	var instants []time.Time
	timed := &putClock{Emitter: em, sections: memory, clock: clock, instants: &instants}
	if err := f.Run(t.Context(), src, timed); err != nil {
		t.Fatal(err)
	}
	if rejected := memory.Rejected(); len(rejected) > 0 {
		t.Fatalf("%d events refused; the first is %s (%s)", len(rejected), rejected[0].GetEventId(), rejected[0].GetReasonDetail())
	}
	return f, memory, instants
}

// putClock passes PutSections to the memory emitter and notes when each arrived on the payload clock.
type putClock struct {
	feeder.Emitter
	sections *emit.MemoryEmitter
	clock    *arrivalClock
	instants *[]time.Time
}

func (p *putClock) PutSections(ctx context.Context, ref string, sections []knowledge.Section) (knowledge.PutResult, error) {
	*p.instants = append(*p.instants, p.clock.base.UTC().Truncate(time.Second))
	return p.sections.PutSections(ctx, ref, sections)
}

func TestGenerateNotionFixture02(t *testing.T) {
	if os.Getenv(genFixturesEnv) == "" {
		t.Skipf("set %s=1 to regenerate the Notion fixture 02", genFixturesEnv)
	}
	root := repoRoot(t)
	dir := filepath.Join(root, fixture02Dir)
	for _, generated := range []string{"payloads", "events.jsonl", "rejected.jsonl", "manifest.yaml", "golden"} {
		if err := os.RemoveAll(filepath.Join(dir, generated)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	payloads, _ := live02Payloads(t)
	var recorded *record.PayloadRecorder
	var events *record.EventRecorder
	f, memory, instants := run02(t, payloads, func(src feeder.Source, em feeder.Emitter) (feeder.Source, feeder.Emitter) {
		recorded = record.Wrap(src, dir)
		events = record.Emitter(em, dir)
		return recorded, events
	})
	if err := recorded.Err(); err != nil {
		t.Fatal(err)
	}
	if err := events.Err(); err != nil {
		t.Fatal(err)
	}
	if err := record.WriteManifest(dir, record.Manifest{
		Family:      "notion-knowledge",
		Description: description02 + " Synthetic structural twin: no identifier is derived from any organisation.",
		Sources:     []record.ManifestSource{record.SourceOf(f.Describe())},
		Start:       t02.Add(-time.Minute),
		End:         t02.Add(3 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "manifest.yaml")
	existing, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(existing, []byte(queries02)...), 0o644); err != nil {
		t.Fatal(err)
	}

	docs := documentsOf(memory.SectionsPuts(), func(i int) time.Time { return instants[i] })
	raw, err := yaml.Marshal(docs)
	if err != nil {
		t.Fatal(err)
	}
	header := "# SPDX-License-Identifier: Apache-2.0\n# Generated by internal/feeders/notion/twin02_test.go from " +
		"fixtures/notion-knowledge-twin-02: the sections the feeder sent, at the instants it sent them.\n"
	if err := os.WriteFile(filepath.Join(root, incident02Docs), append([]byte(header), raw...), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestNotionFixture02Conforms(t *testing.T) {
	f, err := notion.New(notion.Options{Config: fixture02Config(t)})
	if err != nil {
		t.Fatal(err)
	}
	testkit.Conformance(t, f, filepath.Join(repoRoot(t), fixture02Dir))
}

// TestTheRecordingIsTheLiveRun02: the committed payloads are what the poller produces today, and the
// incident's documents are what the feeder sends from them.
func TestTheRecordingIsTheLiveRun02(t *testing.T) {
	live, _ := live02Payloads(t)
	recorded, err := source.NewFileSource(filepath.Join(repoRoot(t), fixture02Dir))
	if err != nil {
		t.Fatal(err)
	}
	var committed []feeder.Payload
	for {
		p, err := recorded.Next(t.Context())
		if err != nil {
			break
		}
		committed = append(committed, p)
	}
	if len(committed) != len(live) {
		t.Fatalf("%d committed payloads, %d live; regenerate with %s=1", len(committed), len(live), genFixturesEnv)
	}
	for i := range live {
		if !jsonEqual(t, live[i].Bytes, committed[i].Bytes) {
			t.Fatalf("payload %d differs from the live run; regenerate with %s=1\nlive: %s\ncommitted: %s",
				i, genFixturesEnv, live[i].Bytes, committed[i].Bytes)
		}
	}

	_, memory, instants := run02(t, committed, nil)
	want, err := yaml.Marshal(documentsOf(memory.SectionsPuts(), func(i int) time.Time { return instants[i] }))
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(repoRoot(t), incident02Docs))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(got), string(want)) {
		t.Errorf("%s is not what the feeder sends from the recording; regenerate with %s=1", incident02Docs, genFixturesEnv)
	}
}

func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatal(err)
	}
	ra, _ := json.Marshal(x)
	rb, _ := json.Marshal(y)
	return string(ra) == string(rb)
}

// TestTwin02Sections is what the fixture is for, on the sections the feeder sent.
func TestTwin02Sections(t *testing.T) {
	payloads, w := live02Payloads(t)
	for _, p := range payloads {
		raw := string(p.Bytes)
		for _, leak := range []string{"sreagentcanary", "canary-domain", "created_by", "Another page's content"} {
			if strings.Contains(raw, leak) {
				t.Errorf("a payload carries %q: %s", leak, raw)
			}
		}
	}
	f, memory, _ := run02(t, payloads, nil)
	puts := memory.SectionsPuts()

	var refs []string
	for _, put := range puts {
		refs = append(refs, put.DocumentRef)
		if put.AfterEvents == 0 {
			t.Errorf("the sections of %s arrived before any event", put.DocumentRef)
		}
	}
	page := func(id string) string { return notion.NSPage + "=" + id }
	// The May postmortem twice (13:00 and after its 14:50 edit), the others once: an unedited page is
	// not read again.
	if want := []string{page(pageMay), page(pageReporting), page(pageAfterTheAlert), page(pageMay)}; !slices.Equal(refs, want) {
		t.Fatalf("puts %v, want %v", refs, want)
	}
	// The May postmortem's body is three requests (the page, the toggle, the table; never the child
	// page) per read, the others one each.
	if got := w.calls["blocks"]; got != 3+1+1+3 {
		t.Errorf("%d block reads, want 8", got)
	}

	labels := func(sections []knowledge.Section) []string {
		var out []string
		for _, s := range sections {
			out = append(out, fmt.Sprintf("%s/%d/%s", s.Label, s.Part, s.Origin))
		}
		return out
	}
	want := []string{
		"Summary/0/property", "Signals / Symptoms/0/property", "Root cause/0/property", "Resolution/0/property",
		"Body/0/body", "Timeline/0/body", "Timeline/1/body", "Detection/0/body", "Escalated to [redacted]/0/body",
		"Follow-ups/0/body",
	}
	if got := labels(puts[0].Sections); !slices.Equal(got, want) {
		t.Errorf("May sections:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	for _, s := range puts[0].Sections {
		switch s.Label {
		case "Signals / Symptoms":
			if i := strings.Index(s.Text, "ECONNRESET"); i < notion.SummaryCap {
				t.Errorf("ECONNRESET at character %d; the fixture needs it past %d", i, notion.SummaryCap)
			}
		case "Resolution":
			if !strings.Contains(s.Text, "the leaked key [redacted] was rotated") {
				t.Errorf("Resolution = %q", s.Text)
			}
		case "Detection":
			for _, want := range []string{"Alert details, raw key [redacted]", "Monitor 20110 on checkout"} {
				if !strings.Contains(s.Text, want) {
					t.Errorf("Detection lacks %q: %q", want, s.Text)
				}
			}
		case "Escalated to [redacted]":
			if s.Text != "Paged [redacted] and" {
				t.Errorf("escalation = %q", s.Text)
			}
		}
	}
	if strings.Contains(fmt.Sprint(puts[0].Sections), "Raise the pool") == false ||
		strings.Contains(fmt.Sprint(puts[0].Sections), "Alert on pool acquire timeouts") {
		t.Error("the first read's follow-ups are wrong")
	}
	if last := puts[3]; last.Result.Opened != 1 || last.Result.Closed != 1 || last.Result.Unchanged != len(want)-1 ||
		!strings.Contains(fmt.Sprint(last.Sections), "Alert on pool acquire timeouts") {
		t.Errorf("the 14:50 edit: %+v", last.Result)
	}

	report := f.Report().Databases[0]
	var skipped []string
	for _, s := range report.Sections[pageMay].Skipped {
		skipped = append(skipped, s.String())
	}
	if want := []string{"Contact: emptied by sanitisation", "unsupported block child_page: 1",
		"unsupported block embed: 1", "unsupported block image: 1"}; !slices.Equal(skipped, want) {
		t.Errorf("skipped %v, want %v", skipped, want)
	}
}
