// SPDX-License-Identifier: Apache-2.0

package vendornotice_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	vn "github.com/Pierre-Theophile/aisre/internal/feeders/vendornotice"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/emit"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/record"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/source"
)

// Generating the US2 fixtures (T086–T088).
//
// These are **synthetic structural twins**, on the same terms as US1's: the same event shapes, the same
// sequences and the same instants as a real recording, with no identifier derived from the organisation
// and no vendor it actually depends on. The sanitisation contract's §7 split is why they exist in this
// repository at all — a claim made here is a claim about the twins, and constitution VIII is explicit
// that synthetic-only data is not enough for a connector to be marked stable.
//
// A vendor-notice twin has one property a GCP twin does not: **its payloads are already the sanitised
// form.** The feeder's payload shape is the six typed fields and a pointer, so a recording carries no
// body and no sender to begin with (FR-071, FR-072). That is not a concession made for the fixture; it
// is the shape the source emits, and the fixture being publishable is the visible consequence.
//
// Regenerate with:
//
//	SRE_AGENT_GEN_FIXTURES=1 go test ./internal/feeders/vendornotice -run TestGenerateFixtures
//
// The output is byte-for-byte reproducible: every instant and identifier is a literal below, and
// nothing reads a clock.

// The twin's instants. A notice read on 17 September for a window on 2 October: valid time leads
// observed time by a fortnight, which is the whole of what US2 has to get right.
var (
	noticeRead      = time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	noticeCorrected = time.Date(2026, 9, 19, 8, 30, 0, 0, time.UTC)
	// A day after the correction, which is more than the 6h reordering window: see the comment in
	// rescheduleFixture for why the spacing is load-bearing rather than incidental.
	noticeRescheduled = time.Date(2026, 9, 20, 8, 30, 0, 0, time.UTC)
	windowStart       = time.Date(2026, 10, 2, 2, 0, 0, 0, time.UTC)
	windowEnd         = time.Date(2026, 10, 2, 4, 0, 0, 0, time.UTC)
	rescheduledStart  = time.Date(2026, 10, 9, 2, 0, 0, 0, time.UTC)
	rescheduledEnd    = time.Date(2026, 10, 9, 4, 0, 0, 0, time.UTC)
)

// The twin's identifiers. None is derived from the organisation (§7): a vendor called `twin-gpu` with a
// product called `inference-api`, on a reserved-for-testing domain.
const (
	twinVendor     = "twin-gpu"
	twinVendorName = "Twin GPU"
	twinProduct    = "inference-api"
	twinOtherProd  = "training"
	twinNoticeID   = "TWIN-2026-1002"
	twinHost       = "api.twin-gpu.test"
)

// genFixturesEnv arms the generator. Regenerating a corpus is a deliberate act: the corpus is the test.
const genFixturesEnv = "SRE_AGENT_GEN_FIXTURES"

func TestGenerateFixtures(t *testing.T) {
	if os.Getenv(genFixturesEnv) == "" {
		t.Skipf("set %s=1 to regenerate the US2 fixtures", genFixturesEnv)
	}
	for _, fx := range vendorFixtureSet(t) {
		writeVendorFixture(t, fx)
	}
}

// vendorFixtureSpec is one fixture to generate.
type vendorFixtureSpec struct {
	dir         string
	description string
	payloads    []feeder.Payload
	// queries are authored rather than derived: they state what the fixture proves. A fixture with no
	// queries verifies nothing, which is why the description of one says so out loud.
	queries string
}

func vendorFixtureSet(t *testing.T) []vendorFixtureSpec {
	t.Helper()
	return []vendorFixtureSpec{
		maintenanceFutureFixture(t),
		cancellationFixture(t),
		rescheduleFixture(t),
		duplicateTwoSourcesFixture(t),
		notAllowlistedFixture(t),
		unextractableFixture(t),
	}
}

// twinAllowlist is the fixture's allowlist. It is stated here rather than read from
// `config/vendors.yaml`, because a fixture that depended on the shipped configuration would change
// meaning the next time somebody added a vendor to it.
func twinAllowlist(t *testing.T) *vn.Allowlist {
	t.Helper()
	list, err := vn.NewAllowlist([]vn.Vendor{{
		Slug: twinVendor, Name: twinVendorName,
		Products: []string{twinProduct, twinOtherProd},
		Hosts:    []string{twinHost},
	}})
	if err != nil {
		t.Fatalf("NewAllowlist: %v", err)
	}
	return list
}

// writeVendorFixture replays the payloads through the feeder with both recorders in place, which is
// exactly what `feed vendor-notice --record` does — so the fixture is what the feeder produced rather
// than what somebody thought it would produce.
func writeVendorFixture(t *testing.T, fx vendorFixtureSpec) {
	t.Helper()
	// `go test` runs with the working directory set to the package, so the path is resolved against
	// the repository root. Writing it relative would silently create
	// `internal/feeders/vendornotice/fixtures/` and report success.
	dir := filepath.Join(repoRoot(t), fx.dir)
	// Only what a generator writes is cleared. `os.RemoveAll(dir)` was the first cut in every fixture
	// generator, and it took `golden/` with it — regenerating deleted the frozen answers, and
	// `fixture verify` then had nothing left to disagree with — along with anything recorded beside
	// the fixture by other means, such as `gcp-cross-source-merge-01`'s `world/` (004 T079, T153).
	// A stale golden that survives a regeneration makes verify FAIL and name the query, which is the
	// failure worth having.
	for _, generated := range []string{"payloads", "events.jsonl", "rejected.jsonl", "manifest.yaml"} {
		if err := os.RemoveAll(filepath.Join(dir, generated)); err != nil {
			t.Fatalf("clear %s: %v", filepath.Join(dir, generated), err)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create %s: %v", dir, err)
	}
	f, err := vn.New(vn.Options{
		OrgSlug:   "twin",
		Allowlist: twinAllowlist(t),
		// The clock is fixed at the last instant any payload arrives, so FR-063's check — observed
		// time is never in the future — is evaluated at a known instant rather than at whatever the
		// machine says when the corpus is regenerated.
		Now: func() time.Time { return noticeRescheduled.Add(time.Hour) },
	})
	if err != nil {
		t.Fatalf("new feeder: %v", err)
	}
	desc := f.Describe()

	clock := &arrivalClock{base: noticeRead}
	src := record.Wrap(clock.wrap(source.NewSliceSource(fx.payloads)), dir)
	memory := emit.NewMemoryEmitter(desc, emit.WithMemoryClock(clock.Now))
	events := record.Emitter(memory, dir)

	if err := f.Run(t.Context(), src, events); err != nil {
		t.Fatalf("%s: run: %v", dir, err)
	}
	if err := src.Err(); err != nil {
		t.Fatalf("%s: record payloads: %v", dir, err)
	}
	if err := events.Err(); err != nil {
		t.Fatalf("%s: record events: %v", dir, err)
	}
	if rejected := memory.Rejected(); len(rejected) > 0 {
		t.Fatalf("%s: %d events were refused; the first is %s (%s)",
			dir, len(rejected), rejected[0].GetEventId(), rejected[0].GetReasonDetail())
	}
	if err := record.WriteManifest(dir, record.Manifest{
		Family: "vendor-notice",
		Description: fx.description +
			" Synthetic structural twin: no identifier is derived from the organisation and no vendor " +
			"it depends on appears (contracts/sanitisation.md §7). The payloads are already the " +
			"sanitised form — the six typed fields and a pointer — because that is the shape the " +
			"source emits, so no body and no sender exists to remove (FR-071, FR-072).",
		Sources:        []record.ManifestSource{record.SourceOf(desc)},
		Start:          noticeRead,
		End:            windowEnd.Add(time.Hour),
		ExpectRejected: events.Rejections(),
	}); err != nil {
		t.Fatalf("%s: write manifest: %v", dir, err)
	}
	if fx.queries != "" {
		appendVendorQueries(t, dir, fx.queries)
	}
	t.Logf("%s: %d payloads, %d events", fx.dir, src.Count(), events.Accepted())
}

func appendVendorQueries(t *testing.T, dir, queries string) {
	t.Helper()
	path := filepath.Join(dir, "manifest.yaml")
	existing, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := os.WriteFile(path, append(existing, []byte("\n"+queries)...), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// --- payload builders -----------------------------------------------------------------------------

// twinAnnouncement renders one recorded announcement, in the wire shape a source produces.
func twinAnnouncement(product, noticeID, pointer, source, state string, start, end time.Time, vague bool) map[string]any {
	entry := map[string]any{
		"vendor": twinVendor, "product": product, "kind": "maintenance",
		"pointer": pointer, "source": source,
	}
	if noticeID != "" {
		entry["notice_id"] = noticeID
	}
	if vague {
		entry["window_start_unknown"] = true
	} else {
		entry["window_start"] = start.Format(time.RFC3339)
	}
	if !end.IsZero() {
		entry["window_end"] = end.Format(time.RFC3339)
	}
	if state != "" {
		entry["state"] = state
	}
	return entry
}

func twinPayload(t *testing.T, kind string, at time.Time, considered int, source string, entries ...map[string]any) feeder.Payload {
	t.Helper()
	body := map[string]any{"announcements": entries, "considered": considered, "source": source}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return feeder.Payload{Kind: kind, At: at, Bytes: raw}
}

func twinUnextractedPayload(t *testing.T, at time.Time, considered int, records ...map[string]any) feeder.Payload {
	t.Helper()
	body := map[string]any{
		"announcements": []map[string]any{}, "unextracted": records,
		"considered": considered, "source": string(vn.SourceMailbox),
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return feeder.Payload{Kind: vn.PayloadAnnouncements, At: at, Bytes: raw}
}

func twinCycleMarker(at time.Time, gaps ...map[string]string) feeder.Payload {
	body := map[string]any{}
	if len(gaps) > 0 {
		body["gaps"] = gaps
	}
	raw, _ := json.Marshal(body)
	return feeder.Payload{Kind: vn.PayloadCycleMarker, At: at, Bytes: raw}
}

// vendor-maintenance-future-01 — the announced fact, joining 001's announced-fact-01.
func maintenanceFutureFixture(t *testing.T) vendorFixtureSpec {
	return vendorFixtureSpec{
		dir: "fixtures/vendor-maintenance-future-01",
		description: "One maintenance window read on 17 September for 2 October: valid time leads " +
			"observed time by a fortnight, which is the project's first announced fact. Exercises the " +
			"deterministic notice ref, the announced window emitted unchanged, the VENDOR actor kind, " +
			"the THIRD_PARTY vendor node with its host claims, and the announcement state defaulting " +
			"to announced and staying there while the window is still ahead.",
		payloads: []feeder.Payload{
			twinPayload(t, vn.PayloadAnnouncements, noticeRead, 1, string(vn.SourceMailbox),
				twinAnnouncement(twinProduct, twinNoticeID, "mailbox:twin-1002",
					string(vn.SourceMailbox), "", windowStart, windowEnd, false)),
			twinCycleMarker(noticeRead.Add(time.Minute)),
		},
		queries: `queries:
  # The vendor and its announced change, as known on the day the notice was read. Pinned, so the
  # golden does not encode the day it was recorded (fixture-format.md).
  - name: vendor-2hop-as-read
    kind: subgraph
    focus: vendor=` + twinVendor + `
    valid_at: 2026-10-02T03:00:00Z
    observed_at: 2026-09-17T12:00:00Z
    hops: 2
    direction: both
  # The same graph read during the announced window, a fortnight after the notice. The change is
  # valid here and was observed a fortnight earlier: an ordinary bitemporal read, which is FR-064's
  # whole claim (nothing about an announced fact is a special case).
  - name: vendor-2hop-in-window
    kind: subgraph
    focus: vendor=` + twinVendor + `
    valid_at: 2026-10-02T03:00:00Z
    observed_at: 2026-10-02T03:00:00Z
    hops: 2
    direction: both
  # The extent, which is what says the feeder was watching.
  - name: extent
    kind: extent
`,
	}
}

// vendor-cancellation-01 — a correction that closes no valid interval.
func cancellationFixture(t *testing.T) vendorFixtureSpec {
	return vendorFixtureSpec{
		dir: "fixtures/vendor-cancellation-01",
		description: "The same window, cancelled two days later. The cancellation is a correction of " +
			"the same change: the observed interval closes and another opens on one ref, the valid " +
			"interval is never rewritten, and \"what did we believe on 18 September about 2 October?\" " +
			"stays answerable by an observed-time query. One change node, two recoverable beliefs.",
		payloads: []feeder.Payload{
			twinPayload(t, vn.PayloadAnnouncements, noticeRead, 1, string(vn.SourceMailbox),
				twinAnnouncement(twinProduct, twinNoticeID, "mailbox:twin-1002",
					string(vn.SourceMailbox), "", windowStart, windowEnd, false)),
			twinCycleMarker(noticeRead.Add(time.Minute)),
			twinPayload(t, vn.PayloadCorrections, noticeCorrected, 1, string(vn.SourceMailbox),
				twinAnnouncement(twinProduct, twinNoticeID, "mailbox:twin-1002-cancel",
					string(vn.SourceMailbox), "cancelled", windowStart, windowEnd, false)),
			twinCycleMarker(noticeCorrected.Add(time.Minute)),
		},
		queries: `queries:
  # What we believed on 18 September: the window is announced. The cancellation was read on the
  # 19th, so an observed-time read before it must not see it — that recoverability is FR-067.
  - name: vendor-believed-18-september
    kind: subgraph
    focus: vendor=` + twinVendor + `
    valid_at: 2026-10-02T03:00:00Z
    observed_at: 2026-09-18T12:00:00Z
    hops: 2
    direction: both
  # What we believe now: the same change, cancelled. One node, not two.
  - name: vendor-believed-now
    kind: subgraph
    focus: vendor=` + twinVendor + `
    valid_at: 2026-10-02T03:00:00Z
    observed_at: 2026-09-20T12:00:00Z
    hops: 2
    direction: both
  # The change's own history, which is where both beliefs are visible side by side.
  - name: notice-history
    kind: history
    focus: vendor.notice=` + twinVendor + `/` + twinNoticeID + `
  - name: extent
    kind: extent
`,
	}
}

// vendor-reschedule-01 — a correction that supersedes a window without rewriting it.
func rescheduleFixture(t *testing.T) vendorFixtureSpec {
	return vendorFixtureSpec{
		dir: "fixtures/vendor-reschedule-01",
		description: "The same window, moved a week later. The superseded window and the new one are " +
			"both recoverable by an observed-time query, and the reschedule creates no second change " +
			"node: the vendor reused its identifier, so the correction addresses the same ref. Zero " +
			"valid intervals are rewritten.",
		payloads: []feeder.Payload{
			twinPayload(t, vn.PayloadAnnouncements, noticeRead, 1, string(vn.SourceMailbox),
				twinAnnouncement(twinProduct, twinNoticeID, "mailbox:twin-1002",
					string(vn.SourceMailbox), "", windowStart, windowEnd, false)),
			twinCycleMarker(noticeRead.Add(time.Minute)),
			twinPayload(t, vn.PayloadCorrections, noticeCorrected, 1, string(vn.SourceMailbox),
				twinAnnouncement(twinProduct, twinNoticeID, "mailbox:twin-1002-moved",
					string(vn.SourceMailbox), "superseded", windowStart, windowEnd, false)),
			twinCycleMarker(noticeCorrected.Add(time.Minute)),
			// The new window is a fresh announcement of the same notice: same ref, a later window. It
			// arrives in a **later cycle**, a day after the supersession, and that spacing is not
			// decoration.
			//
			// Three observations of one change ref resolve to one current version by observed order, so
			// two of them inside one reordering window do not commute — the same property
			// contracts/gcp-feeder.md §3.4 records for a recreation. Delivered a day apart they are in
			// different windows, the shuffle cannot swap them, and the fixture asserts the behaviour
			// rather than the delivery order. It is also what the vendor's mail actually looks like:
			// "the 2 October window is cancelled", and then the replacement.
			twinPayload(t, vn.PayloadAnnouncements, noticeRescheduled, 1, string(vn.SourceMailbox),
				twinAnnouncement(twinProduct, twinNoticeID, "mailbox:twin-1002-moved",
					string(vn.SourceMailbox), "", rescheduledStart, rescheduledEnd, false)),
			twinCycleMarker(noticeRescheduled.Add(time.Minute)),
		},
		queries: `queries:
  # The superseded window, as believed before the reschedule was read.
  - name: vendor-believed-18-september
    kind: subgraph
    focus: vendor=` + twinVendor + `
    valid_at: 2026-10-02T03:00:00Z
    observed_at: 2026-09-18T12:00:00Z
    hops: 2
    direction: both
  # The new window, as believed after it.
  - name: vendor-believed-after-reschedule
    kind: subgraph
    focus: vendor=` + twinVendor + `
    valid_at: 2026-10-09T03:00:00Z
    observed_at: 2026-09-21T12:00:00Z
    hops: 2
    direction: both
  # One change with both windows in its history.
  - name: notice-history
    kind: history
    focus: vendor.notice=` + twinVendor + `/` + twinNoticeID + `
  - name: extent
    kind: extent
`,
	}
}

// vendor-duplicate-two-sources-01 — one announcement, two sources, one change.
func duplicateTwoSourcesFixture(t *testing.T) vendorFixtureSpec {
	return vendorFixtureSpec{
		dir: "fixtures/vendor-duplicate-two-sources-01",
		description: "The same maintenance window carried by the mailbox and the vendor's status " +
			"page, four hours apart. Both observations are emitted — the feeder suppresses neither, " +
			"because suppression is a silent merge with no audit trail — and they address one change " +
			"ref because the vendor stated a notice identifier. Exactly one change node results, and " +
			"the cycle report counts the second reading as merged.",
		payloads: []feeder.Payload{
			twinPayload(t, vn.PayloadAnnouncements, noticeRead, 1, string(vn.SourceMailbox),
				twinAnnouncement(twinProduct, twinNoticeID, "mailbox:twin-1002",
					string(vn.SourceMailbox), "", windowStart, windowEnd, false)),
			twinCycleMarker(noticeRead.Add(time.Minute)),
			twinPayload(t, vn.PayloadAnnouncements, noticeRead.Add(4*time.Hour), 1,
				string(vn.SourceStatusPage),
				twinAnnouncement(twinProduct, twinNoticeID, "statuspage:twin-gpu/maintenance/m1",
					string(vn.SourceStatusPage), "", windowStart, windowEnd, false)),
			twinCycleMarker(noticeRead.Add(4*time.Hour + time.Minute)),
		},
		queries: `queries:
  # One change node, not two adjacent rows for one window in front of somebody being paged.
  - name: vendor-2hop
    kind: subgraph
    focus: vendor=` + twinVendor + `
    valid_at: 2026-10-02T03:00:00Z
    observed_at: 2026-09-18T12:00:00Z
    hops: 2
    direction: both
  # Both readings are in the change's history, each with its own pointer.
  - name: notice-history
    kind: history
    focus: vendor.notice=` + twinVendor + `/` + twinNoticeID + `
  - name: extent
    kind: extent
`,
	}
}

// vendor-not-allowlisted-01 — a counted drop.
func notAllowlistedFixture(t *testing.T) vendorFixtureSpec {
	return vendorFixtureSpec{
		dir: "fixtures/vendor-not-allowlisted-01",
		description: "Three announcements read, one acted on. One names a vendor nobody allowlisted, " +
			"one names an allowlisted vendor and a product this organisation does not use, and one is " +
			"in scope. The two drops are counted and carry their reasons, which is what makes \"the " +
			"graph knows about no upcoming vendor change\" distinguishable from \"the feeder dropped " +
			"forty announcements\" — only one of those is good news.",
		payloads: []feeder.Payload{
			twinPayload(t, vn.PayloadAnnouncements, noticeRead, 3, string(vn.SourceMailbox),
				twinAnnouncement(twinProduct, twinNoticeID, "mailbox:twin-1002",
					string(vn.SourceMailbox), "", windowStart, windowEnd, false),
				// An allowlisted vendor, a product that is not.
				map[string]any{
					"vendor": twinVendor, "product": "billing-console", "kind": "maintenance",
					"window_start": windowStart.Format(time.RFC3339),
					"window_end":   windowEnd.Format(time.RFC3339),
					"notice_id":    "TWIN-2026-1003", "pointer": "mailbox:twin-1003",
					"source": string(vn.SourceMailbox),
				},
				// A vendor nobody allowlisted.
				map[string]any{
					"vendor": "somebody-else", "product": "whatever", "kind": "maintenance",
					"window_start": windowStart.Format(time.RFC3339),
					"notice_id":    "SE-1", "pointer": "mailbox:se-1",
					"source": string(vn.SourceMailbox),
				}),
			twinCycleMarker(noticeRead.Add(time.Minute)),
		},
		queries: `queries:
  # Only the in-scope announcement is in the graph. The other two exist as counted drops in the
  # cycle report and nowhere else — never emitted "just in case" (FR-059).
  - name: vendor-2hop
    kind: subgraph
    focus: vendor=` + twinVendor + `
    valid_at: 2026-10-02T03:00:00Z
    observed_at: 2026-09-18T12:00:00Z
    hops: 2
    direction: both
  - name: extent
    kind: extent
`,
	}
}

// vendor-unextractable-01 — a visible extraction failure.
func unextractableFixture(t *testing.T) vendorFixtureSpec {
	return vendorFixtureSpec{
		dir: "fixtures/vendor-unextractable-01",
		description: "Two messages from a configured sender, one of which no rule could read. The " +
			"unreadable one is recorded as unextracted with its reason and its pointer and is not " +
			"emitted as a change: a human can see what the feeder could not read, which is the " +
			"difference between a gap in extraction and a gap in the vendor's announcements. The " +
			"cycle also carries a gap, because a second mailbox could not be reached.",
		payloads: []feeder.Payload{
			twinPayload(t, vn.PayloadAnnouncements, noticeRead, 1, string(vn.SourceMailbox),
				twinAnnouncement(twinProduct, twinNoticeID, "mailbox:twin-1002",
					string(vn.SourceMailbox), "", windowStart, windowEnd, false)),
			twinUnextractedPayload(t, noticeRead.Add(time.Second), 1, map[string]any{
				"reason": string(vn.UnextractedNoWindow), "field": "window",
				"pointer": "mailbox:twin-1004", "source": string(vn.SourceMailbox),
			}),
			twinCycleMarker(noticeRead.Add(time.Minute), map[string]string{
				"source": string(vn.SourceMailbox) + ":essential-contact-1",
				"why":    "dial tcp: i/o timeout",
			}),
		},
		queries: `queries:
  # The one readable notice is in the graph. The unreadable one is in the cycle report with its
  # pointer, not in the graph as a guess (FR-074).
  - name: vendor-2hop
    kind: subgraph
    focus: vendor=` + twinVendor + `
    valid_at: 2026-10-02T03:00:00Z
    observed_at: 2026-09-18T12:00:00Z
    hops: 2
    direction: both
  # The extent, whose gap says a mailbox was not read — the quiet in this window was ignorance
  # rather than absence (FR-075).
  - name: extent
    kind: extent
`,
	}
}

// arrivalClock stamps observed time from payload arrival, so that a recording's observed order is the
// order the payloads arrived in. See the GCP generator's copy for why this rather than a constant or a
// per-event clock: a constant makes `close_observed` unrepresentable, and a per-event clock moves the
// valid-from of a from-unknown fact.
type arrivalClock struct {
	base time.Time
	step int64
	last time.Time
}

// eventStep is the gap between two events of one payload: bookkeeping rather than a claim about
// duration, since the facts' valid times come from the payload content.
const eventStep = time.Microsecond

func (c *arrivalClock) Now() time.Time {
	at := c.base.Add(time.Duration(c.step) * eventStep)
	c.step++
	if !c.last.IsZero() && !at.After(c.last) {
		at = c.last.Add(eventStep)
	}
	c.last = at
	return at
}

func (c *arrivalClock) wrap(src feeder.Source) feeder.Source {
	return clockedSource{src: src, clock: c}
}

type clockedSource struct {
	src   feeder.Source
	clock *arrivalClock
}

func (s clockedSource) Next(ctx context.Context) (feeder.Payload, error) {
	payload, err := s.src.Next(ctx)
	if err != nil {
		return payload, err
	}
	if !payload.At.IsZero() {
		s.clock.base, s.clock.step = payload.At, 0
	}
	return payload, nil
}

// repoRoot walks up from the test's working directory to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test's working directory")
		}
		dir = parent
	}
}
