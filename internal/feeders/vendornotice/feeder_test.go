// SPDX-License-Identifier: Apache-2.0

package vendornotice_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	vn "github.com/Pierre-Theophile/aisre/internal/feeders/vendornotice"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The feeder end to end (T069, T085, FR-002, FR-058, FR-075, FR-078).

type sliceSource struct{ payloads []feeder.Payload }

func (s *sliceSource) Next(context.Context) (feeder.Payload, error) {
	if len(s.payloads) == 0 {
		return feeder.Payload{}, io.EOF
	}
	next := s.payloads[0]
	s.payloads = s.payloads[1:]
	return next, nil
}

type recorder struct {
	events      []*graphv1.EventEnvelope
	checkpoints int
	gapBefore   []bool
	flushed     int
	notes       []string
}

func (r *recorder) Emit(_ context.Context, ev *graphv1.EventEnvelope) (*graphv1.IngestResult, error) {
	r.events = append(r.events, ev)
	return &graphv1.IngestResult{}, nil
}

func (r *recorder) Checkpoint(_ context.Context, fact feeder.CheckpointFact) error {
	r.checkpoints++
	r.gapBefore = append(r.gapBefore, fact.GapBefore)
	r.notes = append(r.notes, fact.Note)
	return nil
}

func (r *recorder) Flush(context.Context) error { r.flushed++; return nil }

func newTestFeeder(t *testing.T, refuseEmpty bool) *vn.Feeder {
	t.Helper()
	list, err := vn.NewAllowlist([]vn.Vendor{testVendor()})
	if err != nil {
		t.Fatalf("NewAllowlist: %v", err)
	}
	f, err := vn.New(vn.Options{
		OrgSlug:                 "twin",
		Allowlist:               list,
		RefuseEmptyNoticeStream: refuseEmpty,
		Now:                     func() time.Time { return clockNow },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return f
}

func announcementsPayload(t *testing.T, kind string, at time.Time, entries ...map[string]any) feeder.Payload {
	t.Helper()
	body, err := json.Marshal(map[string]any{"announcements": entries})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return feeder.Payload{Kind: kind, At: at, Bytes: body}
}

func maintenanceEntry() map[string]any {
	return map[string]any{
		"vendor": "acme-gpu", "product": "inference-api", "kind": "maintenance",
		"window_start": windowFrom.Format(time.RFC3339), "window_end": windowTo.Format(time.RFC3339),
		"notice_id": "ACME-2026-1002", "pointer": "mailbox:msg-0001", "source": "mailbox",
	}
}

// FR-002: the source id, kind and ordering are the feeder's own, and separate from the GCP feeder's. The
// two read different systems with different failure modes; sharing a checkpoint would couple one source's
// silence to the other's, and silence is what this feeder exists to distinguish from absence.
func TestTheFeederIsItsOwnSourceSeparateFromTheGCPFeeder(t *testing.T) {
	desc := newTestFeeder(t, false).Describe()
	if desc.SourceID != "vendor-notice:twin" {
		t.Fatalf("source id = %q", desc.SourceID)
	}
	if strings.HasPrefix(desc.SourceID, "gcp:") {
		t.Error("the vendor-notice feeder shares the GCP feeder's source id")
	}
	if desc.Kind != vn.Kind {
		t.Errorf("kind = %q, want %q", desc.Kind, vn.Kind)
	}
	if desc.Ordering != feeder.OrderingNone {
		t.Errorf("ordering = %q; three independently polled third-party surfaces give none", desc.Ordering)
	}
	if desc.ReorderingWindow <= 0 {
		t.Error("the reordering window is zero, so the conformance shuffle would permute nothing")
	}
	if err := desc.Validate(); err != nil {
		t.Errorf("the description does not validate: %v", err)
	}
	// The scopes say the mailbox read cannot change message state, which is FR-007's whole point.
	joined := strings.Join(desc.RequiredScopes, " ")
	if !strings.Contains(joined, "BODY.PEEK") || !strings.Contains(joined, "gmail.readonly") {
		t.Errorf("the declared scopes do not name a read that cannot change message state: %v", desc.RequiredScopes)
	}
}

// An empty allowlist reads the sources and drops everything, which is indistinguishable from a quiet week.
func TestAnEmptyAllowlistIsRefusedAtStartup(t *testing.T) {
	empty, err := vn.NewAllowlist(nil)
	if err != nil {
		t.Fatalf("NewAllowlist: %v", err)
	}
	if _, err := vn.New(vn.Options{OrgSlug: "twin", Allowlist: empty}); err == nil {
		t.Fatal("a feeder with an empty allowlist was built")
	}
	if _, err := vn.New(vn.Options{Allowlist: nil, OrgSlug: "twin"}); err == nil {
		t.Fatal("a feeder with no allowlist was built")
	}
}

// One in-scope announcement produces the vendor node, the change, and the claims — and the change's valid
// interval is the announced window even though it starts a fortnight after the read.
func TestOneAnnouncementProducesTheVendorTheChangeAndTheClaims(t *testing.T) {
	f := newTestFeeder(t, false)
	em := &recorder{}
	src := &sliceSource{payloads: []feeder.Payload{
		announcementsPayload(t, vn.PayloadAnnouncements, readAt, maintenanceEntry()),
		{Kind: vn.PayloadCycleMarker, At: readAt.Add(time.Minute), Bytes: []byte(`{}`)},
	}}
	if err := f.Run(context.Background(), src, em); err != nil {
		t.Fatalf("Run: %v", err)
	}

	vendor := firstNodeOfType(em.events, graphv1.NodeType_THIRD_PARTY)
	if vendor == nil {
		t.Fatal("no THIRD_PARTY node was asserted for the vendor (FR-077)")
	}
	// The vendor's valid start is a **bound**, stated: the earliest announcement this feeder read.
	// Not a first-observation placeholder — the conformance shuffle refused that, because a
	// placeholder's value is whichever of the vendor's events reached the graph first, which makes
	// "since when have we known about this vendor" an answer about delivery order.
	if !vendor.GetValidAt().AsTime().Equal(readAt) {
		t.Errorf("the vendor's valid start is %s, want the bound %s",
			vendor.GetValidAt().AsTime(), readAt)
	}
	if vendor.GetValidFromUnknown() {
		t.Error("the vendor node marks its start unknown as well as stating one")
	}
	if vendor.GetProps().GetFields()[vn.PropVendorValidFromIsABound].GetStringValue() == "" {
		t.Error("the vendor node states a valid start without saying it is a bound; a reader cannot " +
			"then tell it from the instant the relationship began, which no notice carries")
	}
	change := firstChange(em.events)
	if change == nil {
		t.Fatal("no change was emitted")
	}
	if !change.GetValidAt().AsTime().Equal(windowFrom) {
		t.Fatalf("the change is valid at %s, want the announced %s", change.GetValidAt().AsTime(), windowFrom)
	}
	if change.GetChange().GetAnnouncementState() != graphv1.AnnouncementState_ANNOUNCED {
		t.Errorf("state = %s, want ANNOUNCED", change.GetChange().GetAnnouncementState())
	}
	// The host-name claims are minted in `server.address`, so FR-119's certain rule has something to
	// fire on. A vendor-specific namespace would mean the rule never fires.
	if !claimed(em.events, vn.NSServerAddress, "api.acme-gpu.test") {
		t.Error("the vendor's host names were not claimed in server.address (FR-077, FR-119)")
	}

	report := f.Report()
	if report.Read != 1 || report.Extracted != 1 || report.Emitted != 1 {
		t.Errorf("report = %+v, want one read, extracted and emitted", report)
	}
	if em.checkpoints != 1 || em.gapBefore[0] {
		t.Errorf("checkpoints = %d gapBefore = %v; a cycle with no unreachable source declares no gap",
			em.checkpoints, em.gapBefore)
	}
}

// FR-074: an announcement the feeder cannot place is recorded as unextracted with its reason, and is
// **not** emitted as a change. Emitting a guess would put a window in the graph somebody plans around.
func TestAnUnextractableAnnouncementIsRecordedAndNotEmitted(t *testing.T) {
	f := newTestFeeder(t, false)
	em := &recorder{}
	broken := maintenanceEntry()
	delete(broken, "window_start")
	delete(broken, "window_end")
	src := &sliceSource{payloads: []feeder.Payload{
		announcementsPayload(t, vn.PayloadAnnouncements, readAt, broken),
	}}
	if err := f.Run(context.Background(), src, em); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if firstChange(em.events) != nil {
		t.Fatal("an unextractable announcement was emitted as a change (FR-074)")
	}
	report := f.Report()
	if report.Read != 1 || report.FailedToExtract != 1 || report.Emitted != 0 {
		t.Fatalf("report = %+v, want one read, one failed to extract, none emitted", report)
	}
	if len(report.Unextracted) != 1 || report.Unextracted[0].Field != "window" {
		t.Errorf("the unextracted record does not name the field: %+v", report.Unextracted)
	}
	if report.Unextracted[0].Pointer != "mailbox:msg-0001" {
		t.Error("the unextracted record carries no pointer, so a human cannot open the original")
	}
}

// FR-059: an out-of-scope announcement is dropped, counted, and visible with its reason.
func TestAnOutOfScopeAnnouncementIsDroppedCountedAndVisible(t *testing.T) {
	f := newTestFeeder(t, false)
	em := &recorder{}
	other := maintenanceEntry()
	other["vendor"] = "someone-else"
	unused := maintenanceEntry()
	unused["product"] = "quantum-beta"
	src := &sliceSource{payloads: []feeder.Payload{
		announcementsPayload(t, vn.PayloadAnnouncements, readAt, other, unused),
	}}
	if err := f.Run(context.Background(), src, em); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if firstChange(em.events) != nil {
		t.Fatal("an out-of-scope announcement was emitted \"just in case\"")
	}
	report := f.Report()
	if report.DroppedByAllowlist != 2 || len(report.Drops) != 2 {
		t.Fatalf("report = %+v, want two drops", report)
	}
	note := report.Note()
	for _, want := range []string{"dropped_by_allowlist=2", "vendor_not_allowlisted", "product_not_allowlisted"} {
		if !strings.Contains(note, want) {
			t.Errorf("the cycle note omits %q: %s", want, note)
		}
	}
}

// FR-070: the same notice from two sources is counted as merged and **both observations are still
// emitted**. Suppression is a silent merge with no audit trail, and the published rules are what merge
// them, carrying their rule and rationale.
func TestTheSameNoticeFromTwoSourcesIsCountedAndBothAreEmitted(t *testing.T) {
	f := newTestFeeder(t, false)
	em := &recorder{}
	fromPage := maintenanceEntry()
	fromPage["source"] = "status_page"
	fromPage["pointer"] = "statuspage:entry-77"
	src := &sliceSource{payloads: []feeder.Payload{
		announcementsPayload(t, vn.PayloadAnnouncements, readAt, maintenanceEntry(), fromPage),
	}}
	if err := f.Run(context.Background(), src, em); err != nil {
		t.Fatalf("Run: %v", err)
	}
	report := f.Report()
	if report.Merged != 1 {
		t.Fatalf("merged = %d, want 1", report.Merged)
	}
	if report.Emitted != 2 {
		t.Fatalf("emitted = %d, want 2: the feeder suppresses neither observation, because suppression "+
			"is a silent merge with no audit trail (FR-070)", report.Emitted)
	}
	// Both observations address one change, which is what makes the merge the resolution layer's job
	// rather than a second row in the ranked list.
	refs := map[string]bool{}
	for _, ev := range em.events {
		if change := ev.GetObserveChange(); change != nil {
			refs[change.GetRef().GetValue()] = true
		}
	}
	if len(refs) != 1 {
		t.Fatalf("two readings produced %d change refs, want 1: %v", len(refs), refs)
	}
}

// FR-075: an unreachable source produces a gap in the checkpoint, not silence — the distinction between
// "the vendor announced nothing" and "we could not ask".
func TestAnUnreachableSourceProducesAGapAndNotSilence(t *testing.T) {
	f := newTestFeeder(t, false)
	em := &recorder{}
	src := &sliceSource{payloads: []feeder.Payload{
		announcementsPayload(t, vn.PayloadAnnouncements, readAt, maintenanceEntry()),
		{Kind: vn.PayloadCycleMarker, At: readAt.Add(time.Minute),
			Bytes: []byte(`{"gaps":[{"source":"status_page","why":"the feed returned 503 on three attempts"}]}`)},
	}}
	if err := f.Run(context.Background(), src, em); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !em.gapBefore[0] {
		t.Fatal("an unreachable source did not declare a gap (FR-075)")
	}
	report := f.Report()
	if len(report.Gaps) != 1 || report.Gaps[0].Source != vn.SourceStatusPage {
		t.Fatalf("gaps = %+v", report.Gaps)
	}
	if !strings.Contains(report.Note(), "gaps=[status_page:") {
		t.Errorf("the cycle note does not state the gap: %s", report.Note())
	}
}

// A gap with no reason is a silence with a label on it.
func TestAGapWithNoReasonIsRefused(t *testing.T) {
	f := newTestFeeder(t, false)
	src := &sliceSource{payloads: []feeder.Payload{
		{Kind: vn.PayloadCycleMarker, At: readAt, Bytes: []byte(`{"gaps":[{"source":"mailbox"}]}`)},
	}}
	if err := f.Run(context.Background(), src, &recorder{}); err == nil {
		t.Fatal("a gap with no reason was accepted")
	}
}

// FR-132b: an empty notice stream is indistinguishable from a working feeder in a quiet week, so a
// campaign whose sources yield nothing fails loudly rather than recording silence as evidence.
func TestAnEmptyNoticeStreamFailsLoudlyWhenTheCampaignAsksItTo(t *testing.T) {
	f := newTestFeeder(t, true)
	err := f.Run(context.Background(), &sliceSource{}, &recorder{})
	if err == nil {
		t.Fatal("a run that read nothing reported success (FR-132b)")
	}
	if !errors.Is(err, vn.ErrEmptyNoticeStream) {
		t.Fatalf("the refusal is %v, want ErrEmptyNoticeStream", err)
	}
	// And without the flag it is a quiet success, so the flag is what does the work.
	quiet := newTestFeeder(t, false)
	if err := quiet.Run(context.Background(), &sliceSource{}, &recorder{}); err != nil {
		t.Errorf("a run that read nothing failed without the flag set: %v", err)
	}
}

// A correction carries the same ref, so the graph holds one change with two observations.
//
// The correction is read two days after the notice and still a fortnight before the window, which is the
// ordinary shape: the vendor changed its mind while the maintenance was still ahead of everybody. Both
// reads are behind the feeder's clock, because an observation is never in the future (FR-063) — the test
// that a future one is refused is the next one down.
func TestACorrectionPayloadCorrectsTheSameChange(t *testing.T) {
	f := newTestFeeder(t, false)
	em := &recorder{}
	cancelled := maintenanceEntry()
	cancelled["state"] = "cancelled"
	cancelled["pointer"] = "mailbox:msg-0002"
	src := &sliceSource{payloads: []feeder.Payload{
		announcementsPayload(t, vn.PayloadAnnouncements, readAt, maintenanceEntry()),
		announcementsPayload(t, vn.PayloadCorrections, readAt.Add(48*time.Hour), cancelled),
	}}
	if err := f.Run(context.Background(), src, em); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var states []graphv1.AnnouncementState
	refs := map[string]bool{}
	for _, ev := range em.events {
		if change := ev.GetObserveChange(); change != nil {
			states = append(states, change.GetChange().GetAnnouncementState())
			refs[change.GetRef().GetValue()] = true
		}
	}
	if len(refs) != 1 {
		t.Fatalf("a correction created a second change node: %v (FR-067)", refs)
	}
	if len(states) != 2 || states[0] != graphv1.AnnouncementState_ANNOUNCED ||
		states[1] != graphv1.AnnouncementState_CANCELLED {
		t.Fatalf("states = %v, want ANNOUNCED then CANCELLED", states)
	}
}

// An unknown announcement state is refused rather than defaulted to announced: a payload saying
// `withdrawn` means somebody meant something the vocabulary does not have.
func TestAnUnknownAnnouncementStateIsRefused(t *testing.T) {
	f := newTestFeeder(t, false)
	entry := maintenanceEntry()
	entry["state"] = "withdrawn"
	src := &sliceSource{payloads: []feeder.Payload{
		announcementsPayload(t, vn.PayloadCorrections, readAt, entry),
	}}
	if err := f.Run(context.Background(), src, &recorder{}); err == nil {
		t.Fatal("an unknown announcement state was accepted")
	}
}

// An observation stamped ahead of the clock stops the run: that is FR-063 enforced where it would
// actually be violated.
func TestAnObservationInTheFutureStopsTheRun(t *testing.T) {
	f := newTestFeeder(t, false)
	src := &sliceSource{payloads: []feeder.Payload{
		announcementsPayload(t, vn.PayloadAnnouncements, clockNow.Add(time.Hour), maintenanceEntry()),
	}}
	err := f.Run(context.Background(), src, &recorder{})
	if err == nil {
		t.Fatal("an observation stamped in the future was accepted")
	}
	if !errors.Is(err, vn.ErrObservedInFuture) {
		t.Fatalf("the refusal is %v, want ErrObservedInFuture", err)
	}
}

// A payload kind nobody handles is an error rather than a silently ignored file.
func TestAnUnhandledPayloadKindIsAnError(t *testing.T) {
	f := newTestFeeder(t, false)
	src := &sliceSource{payloads: []feeder.Payload{
		{Kind: "something-else", At: readAt, Bytes: []byte(`{}`)},
	}}
	if err := f.Run(context.Background(), src, &recorder{}); err == nil {
		t.Fatal("an unknown payload kind was ignored")
	}
}

func firstNodeOfType(events []*graphv1.EventEnvelope, kind graphv1.NodeType) *graphv1.UpsertNode {
	for _, ev := range events {
		if node := ev.GetUpsertNode(); node != nil && node.GetType() == kind {
			return node
		}
	}
	return nil
}

func firstChange(events []*graphv1.EventEnvelope) *graphv1.ObserveChange {
	for _, ev := range events {
		if change := ev.GetObserveChange(); change != nil {
			return change
		}
	}
	return nil
}

func claimed(events []*graphv1.EventEnvelope, namespace, value string) bool {
	for _, ev := range events {
		if claim := ev.GetIdentityClaim(); claim != nil &&
			claim.GetClaim().GetNamespace() == namespace && claim.GetClaim().GetValue() == value {
			return true
		}
	}
	return false
}

// A reschedule is two observations of one change, so it needs two event ids — and the ref is
// deliberately the same, so the id cannot be built from the ref alone.
//
// This is the bug the reschedule fixture found: with the ref and the state in the id and nothing else,
// the re-announcement of a moved window was the *same* id as the original announcement, the graph
// answered DUPLICATE_NOOP, and the new window never reached it. Nothing errored. The fixture had two
// versions where it should have had three, and the only symptom was a golden that looked plausible.
func TestARescheduledWindowIsANewObservationAndNotADuplicate(t *testing.T) {
	f := newTestFeeder(t, false)
	em := &recorder{}
	moved := maintenanceEntry()
	moved["window_start"] = "2026-10-09T02:00:00Z"
	moved["window_end"] = "2026-10-09T04:00:00Z"
	moved["pointer"] = "mailbox:msg-0003"
	superseded := maintenanceEntry()
	superseded["state"] = "superseded"

	src := &sliceSource{payloads: []feeder.Payload{
		announcementsPayload(t, vn.PayloadAnnouncements, readAt, maintenanceEntry()),
		announcementsPayload(t, vn.PayloadCorrections, readAt.Add(48*time.Hour), superseded),
		announcementsPayload(t, vn.PayloadAnnouncements, readAt.Add(48*time.Hour+time.Second), moved),
	}}
	if err := f.Run(context.Background(), src, em); err != nil {
		t.Fatalf("Run: %v", err)
	}

	ids := map[string]bool{}
	refs := map[string]bool{}
	windows := map[string]bool{}
	for _, ev := range em.events {
		change := ev.GetObserveChange()
		if change == nil {
			continue
		}
		ids[ev.GetEventId()] = true
		refs[change.GetRef().GetValue()] = true
		windows[change.GetValidAt().AsTime().Format(time.RFC3339)] = true
	}
	if len(refs) != 1 {
		t.Fatalf("a reschedule created %d change refs, want one (FR-068): %v", len(refs), refs)
	}
	if len(ids) != 3 {
		t.Fatalf("three readings produced %d event ids, so one was silently deduplicated: %v", len(ids), ids)
	}
	if !windows["2026-10-09T02:00:00Z"] {
		t.Errorf("the moved window never reached the graph; windows = %v", windows)
	}
	// And no id carries a control character, which is what `string(anEnum)` produces.
	for id := range ids {
		for _, r := range id {
			if r < 0x20 {
				t.Errorf("event id %q carries a control character; a protobuf enum converted with "+
					"string() yields the rune at that code point", id)
				break
			}
		}
	}
}
