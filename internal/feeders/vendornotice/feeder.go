// SPDX-License-Identifier: Apache-2.0

package vendornotice

import (
	"context"
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

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The feeder (T069, T085, FR-002, FR-058, FR-078, contract §1, §9).
//
// # A separate credential, cadence, budget and checkpoint from the GCP feeder
//
// FR-002, and it is not tidiness. The two feeders read different systems with different failure modes:
// a mailbox that stops answering must not consume the GCP feeder's quota budget or advance its extent,
// and a GCP outage must not make the graph believe no vendor announced anything. Sharing any of the four
// would couple one source's silence to the other's, and silence is the thing this feeder exists to
// distinguish from absence.
//
// # It works with any subset of the three sources
//
// FR-058. A deployment with no status-page adapter configured still reads the mailbox; one with no
// mailbox still reads the feeds. What it must never do is treat an *unreachable* source as an empty one:
// that produces a gap in the checkpoint, not silence (FR-075).
//
// # The per-cycle report is the point of the whole thing
//
// FR-078: announcements read, extracted, dropped by allowlist, failed to extract, and merged. All five
// exist for one reason — *"the graph knows about no upcoming vendor change"* and *"the feeder read
// nothing"* are indistinguishable from the graph alone, and only one of them is good news. The coverage
// audit's finding was a notice nobody read; a feeder that silently read nothing would reproduce it.

// SourceIDPrefix prefixes every vendor-notice source id.
const SourceIDPrefix = "vendor-notice:"

// Kind is the connector family.
const Kind = "vendor-notice"

// SchemaVersion is the event schema version this feeder emits.
const SchemaVersion = "1.0.0"

// DefaultReorderingWindow is how far out of order this feeder may deliver its own events. Wider than the
// GCP feeder's, because a mailbox poll and a status-feed poll run on independent cadences and an email
// can arrive hours after the status page carried the same notice — the two readings of one announcement
// are exactly what must be allowed to arrive in either order.
const DefaultReorderingWindow = 6 * time.Hour

// Namespaces is what Describe declares.
var Namespaces = []string{NSServerAddress, NSVendor, NSVendorNotice}

// Options configures one run.
type Options struct {
	// OrgSlug suffixes the source id.
	OrgSlug string
	// Allowlist is the vendor × product filter. Required and non-empty: a run with an empty allowlist
	// reads a mailbox and drops everything, which looks exactly like a quiet week.
	Allowlist *Allowlist
	// ReorderingWindow overrides DefaultReorderingWindow.
	ReorderingWindow time.Duration
	// RefuseEmptyNoticeStream makes a cycle that read nothing an error rather than a quiet success
	// (FR-132b). An empty notice stream is indistinguishable from a working feeder in a quiet week,
	// so a campaign whose configured mailbox yields nothing fails loudly at campaign start rather
	// than recording silence as evidence.
	RefuseEmptyNoticeStream bool
	// Now supplies the wall clock, for FR-063's check. Nil uses time.Now; a fixture supplies a fixed
	// one so the rule is asserted at a known instant.
	Now func() time.Time
	// Log is where the per-cycle report goes. Nil discards.
	Log *slog.Logger
}

// Validate refuses options that could not produce an honest run.
func (o Options) Validate() error {
	if o.OrgSlug == "" {
		return errors.New("vendornotice: Options.OrgSlug is required; it is the suffix of the source id")
	}
	if o.Allowlist == nil || o.Allowlist.Len() == 0 {
		return errors.New("vendornotice: Options.Allowlist is empty. A run with no allowlisted vendor " +
			"reads its sources and drops everything, which is indistinguishable from a quiet week — so " +
			"it is a refusal at startup rather than a cycle that reports five zeroes (FR-059)")
	}
	if o.ReorderingWindow < 0 {
		return fmt.Errorf("vendornotice: Options.ReorderingWindow is negative (%s)", o.ReorderingWindow)
	}
	return nil
}

// Cycle is the per-cycle report of FR-078.
type Cycle struct {
	// Considered is how many candidate items the sources looked at, by source: messages in a folder,
	// entries in a feed, rows on a page. It is the denominator the other numbers mean nothing
	// without — "read nothing" and "looked at two hundred entries and none was an announcement" are
	// the same zero announcements and opposite facts about the estate.
	Considered map[SourceKind]int
	// Read is how many announcements the sources yielded.
	Read int
	// Extracted is how many produced a valid typed result.
	Extracted int
	// DroppedByAllowlist is how many were out of scope, with their reasons.
	DroppedByAllowlist int
	Drops              []Drop
	// FailedToExtract is how many could not be placed, with their reasons.
	FailedToExtract int
	Unextracted     []Unextracted
	// Merged is how many readings collapsed onto an already-seen notice ref. It is a *report* of what
	// the resolution layer will merge, not a merge: the feeder emits both observations and suppresses
	// neither, because suppression is a silent merge with no audit trail (FR-070).
	Merged int
	// Emitted is how many change observations were emitted. It is Extracted minus nothing: a merged
	// reading is still emitted.
	Emitted int
	// Gaps are the sources that could not be reached, so the checkpoint declares them (FR-075).
	Gaps []Gap
}

// Gap is a source that could not be read.
type Gap struct {
	Source SourceKind
	// Why is the reason, for the checkpoint. A gap with no reason is not a gap, it is a silence with
	// a label on it.
	Why string
}

// Note renders the cycle report deterministically: sorted lists, stable order, because it reaches a
// checkpoint note and a golden.
func (c Cycle) Note() string {
	parts := []string{}
	if len(c.Considered) > 0 {
		sources := make([]string, 0, len(c.Considered))
		for source := range c.Considered {
			sources = append(sources, string(source))
		}
		sort.Strings(sources)
		counted := make([]string, 0, len(sources))
		for _, source := range sources {
			counted = append(counted, fmt.Sprintf("%s=%d", source, c.Considered[SourceKind(source)]))
		}
		parts = append(parts, "considered=["+strings.Join(counted, ",")+"]")
	}
	parts = append(parts,
		fmt.Sprintf("read=%d", c.Read),
		fmt.Sprintf("extracted=%d", c.Extracted),
		fmt.Sprintf("dropped_by_allowlist=%d", c.DroppedByAllowlist),
		fmt.Sprintf("failed_to_extract=%d", c.FailedToExtract),
		fmt.Sprintf("merged=%d", c.Merged),
		fmt.Sprintf("emitted=%d", c.Emitted),
	)
	if summary := DropSummary(c.Drops); len(summary) > 0 {
		parts = append(parts, "drops=["+strings.Join(summary, ",")+"]")
	}
	if len(c.Unextracted) > 0 {
		reasons := map[string]int{}
		for _, u := range c.Unextracted {
			reasons[string(u.Reason)]++
		}
		keys := make([]string, 0, len(reasons))
		for key := range reasons {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		rendered := make([]string, 0, len(keys))
		for _, key := range keys {
			rendered = append(rendered, fmt.Sprintf("%s=%d", key, reasons[key]))
		}
		parts = append(parts, "unextracted=["+strings.Join(rendered, ",")+"]")
	}
	if len(c.Gaps) > 0 {
		gaps := make([]string, 0, len(c.Gaps))
		for _, gap := range c.Gaps {
			gaps = append(gaps, string(gap.Source)+":"+gap.Why)
		}
		sort.Strings(gaps)
		parts = append(parts, "gaps=["+strings.Join(gaps, "; ")+"]")
	}
	return strings.Join(parts, " ")
}

// ErrEmptyNoticeStream is the refusal of FR-132b.
var ErrEmptyNoticeStream = errors.New("vendornotice: the configured sources yielded no announcement")

// Feeder reads one organisation's vendor announcements.
type Feeder struct {
	opts Options
	log  *slog.Logger
	now  func() time.Time

	mu sync.Mutex
	// seenVendors is the vendors whose THIRD_PARTY node has been asserted this run, so it is asserted
	// once rather than per announcement.
	seenVendors map[string]bool
	// seenNotices is the notice refs seen this run, for the merged count.
	seenNotices map[string]bool
	cycle       Cycle
}

// New returns a feeder over opts.
func New(opts Options) (*Feeder, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Feeder{
		opts:        opts,
		log:         log,
		now:         now,
		seenVendors: map[string]bool{},
		seenNotices: map[string]bool{},
	}, nil
}

// Describe returns the feeder's contract.
func (f *Feeder) Describe() feeder.Description {
	window := f.opts.ReorderingWindow
	if window == 0 {
		window = DefaultReorderingWindow
	}
	return feeder.Description{
		SourceID:      SourceIDPrefix + f.opts.OrgSlug,
		Kind:          Kind,
		SchemaVersion: SchemaVersion,
		// Polls of independent third-party surfaces with no ordering guarantee between them.
		Ordering:         feeder.OrderingNone,
		ReorderingWindow: window,
		RequiredScopes: []string{
			"a mailbox read that cannot change message state: gmail.readonly, or IMAP EXAMINE with " +
				"UID FETCH BODY.PEEK (FR-007)",
			"unauthenticated HTTPS GET of the configured status-page and changelog feeds",
		},
		Namespaces: append([]string(nil), Namespaces...),
	}
}

// PayloadKinds are the fixture directory names this feeder's payloads are stored under.
const (
	// PayloadAnnouncements is a batch of typed announcements as a source yielded them.
	PayloadAnnouncements = "announcements"
	// PayloadCorrections is a batch of cancellations and reschedules.
	PayloadCorrections = "corrections"
	// PayloadCycleMarker closes a cycle: it carries the gaps and triggers the checkpoint. It is its
	// own payload because a gap is a property of the *cycle* — which source could not be reached —
	// and only a marker can state that a source yielded nothing because it was unreachable rather
	// than because it was quiet.
	PayloadCycleMarker = "cycle"
)

// announcementsPayload is the recorded shape of a batch.
type announcementsPayload struct {
	Announcements []recordedAnnouncement `json:"announcements"`
	// Unextracted are the readings the *source* could not turn into typed fields — a message with no
	// recognisable window, a feed entry naming no product. They travel in the payload so that a
	// recording keeps them and a replay reports the same cycle: dropping them here would make the
	// fixture say the source read less than it did, which is the one number this feeder exists to
	// get right (FR-074, FR-078).
	Unextracted []recordedUnextracted `json:"unextracted,omitempty"`
	// Considered is how many candidate items the source looked at in the reading this payload came
	// from — messages in a folder, entries in a feed — whether or not any was an announcement. See
	// Batch.Considered: without it, a source whose extraction rules stopped matching reports a clean
	// zero for ever.
	Considered int `json:"considered,omitempty"`
	// Source names which source the count belongs to, so a cycle's report can say which one went
	// quiet rather than only that the total fell.
	Source string `json:"source,omitempty"`
}

// recordedUnextracted is the wire shape of one unreadable announcement. It carries a pointer and a
// reason and never content, for the same reason Unextracted does.
type recordedUnextracted struct {
	Reason  string `json:"reason"`
	Field   string `json:"field,omitempty"`
	Pointer string `json:"pointer"`
	Source  string `json:"source"`
}

// recordedAnnouncement is the wire shape of one typed announcement. It mirrors Announcement rather than
// reusing it so that the fixture format is stable against a Go field rename.
type recordedAnnouncement struct {
	Vendor             string   `json:"vendor"`
	Product            string   `json:"product"`
	Kind               string   `json:"kind"`
	WindowStart        string   `json:"window_start,omitempty"`
	WindowStartUnknown bool     `json:"window_start_unknown,omitempty"`
	WindowEnd          string   `json:"window_end,omitempty"`
	AffectedResources  []string `json:"affected_resources,omitempty"`
	NoticeID           string   `json:"notice_id,omitempty"`
	Pointer            string   `json:"pointer"`
	Source             string   `json:"source"`
	// State is the announcement state this reading carries: empty means `announced`. A correction
	// payload sets `cancelled` or `superseded`.
	State string `json:"state,omitempty"`
}

// cycleMarker closes a cycle.
type cycleMarker struct {
	Gaps []cycleGap `json:"gaps,omitempty"`
}

// cycleGap is one source that could not be read this cycle.
type cycleGap struct {
	Source string `json:"source"`
	Why    string `json:"why"`
}

// Run reads from src and writes to em.
func (f *Feeder) Run(ctx context.Context, src feeder.Source, em feeder.Emitter) error {
	desc := f.Describe()
	if err := desc.Validate(); err != nil {
		return err
	}
	defer func() {
		if err := em.Flush(ctx); err != nil {
			f.log.ErrorContext(ctx, "flush failed", "error", err)
		}
	}()

	for {
		payload, err := src.Next(ctx)
		if errors.Is(err, io.EOF) {
			return f.finish(ctx)
		}
		if err != nil {
			return err
		}
		if err := f.apply(ctx, desc, em, payload); err != nil {
			return err
		}
	}
}

// finish enforces FR-132b at the end of a run.
func (f *Feeder) finish(ctx context.Context) error {
	f.mu.Lock()
	cycle := f.cycle
	f.mu.Unlock()
	f.log.InfoContext(ctx, "vendor-notice cycle", "report", cycle.Note())
	if f.opts.RefuseEmptyNoticeStream && cycle.Read == 0 {
		return fmt.Errorf("%w. An empty notice stream is indistinguishable from a working feeder in a "+
			"quiet week, so this is a refusal rather than a cycle reporting five zeroes (FR-132b)",
			ErrEmptyNoticeStream)
	}
	return nil
}

// Report returns the cycle report so far.
func (f *Feeder) Report() Cycle {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cycle
}

func (f *Feeder) apply(ctx context.Context, desc feeder.Description, em feeder.Emitter, payload feeder.Payload) error {
	switch payload.Kind {
	case PayloadAnnouncements, PayloadCorrections:
		var batch announcementsPayload
		if err := json.Unmarshal(payload.Bytes, &batch); err != nil {
			return fmt.Errorf("vendornotice: decoding a %s payload: %w", payload.Kind, err)
		}
		for _, recorded := range batch.Announcements {
			if err := f.applyAnnouncement(ctx, desc, em, recorded, payload.At); err != nil {
				return err
			}
		}
		for _, recorded := range batch.Unextracted {
			if err := f.applyUnextracted(ctx, recorded); err != nil {
				return err
			}
		}
		if batch.Considered > 0 {
			f.mu.Lock()
			if f.cycle.Considered == nil {
				f.cycle.Considered = map[SourceKind]int{}
			}
			f.cycle.Considered[SourceKind(batch.Source)] += batch.Considered
			f.mu.Unlock()
		}
		return nil
	case PayloadCycleMarker:
		var marker cycleMarker
		if err := json.Unmarshal(payload.Bytes, &marker); err != nil {
			return fmt.Errorf("vendornotice: decoding a %s payload: %w", payload.Kind, err)
		}
		return f.closeCycle(ctx, em, marker, payload.At)
	default:
		return fmt.Errorf("vendornotice: payload kind %q is not one this feeder reads (%s, %s, %s)",
			payload.Kind, PayloadAnnouncements, PayloadCorrections, PayloadCycleMarker)
	}
}

// applyUnextracted records a reading the source itself could not place.
//
// It counts against `read` as well as against `failed to extract`, because the source did read it: a
// cycle that reports ten read and ten extracted while a mailbox held eleven messages is a cycle that
// lies about its own coverage in the direction that hides a miss.
func (f *Feeder) applyUnextracted(ctx context.Context, recorded recordedUnextracted) error {
	if strings.TrimSpace(recorded.Reason) == "" {
		return fmt.Errorf("vendornotice: an unextracted record for %q with no reason; the record "+
			"exists so a human can see what the feeder could not read (FR-074)", recorded.Pointer)
	}
	if strings.TrimSpace(recorded.Pointer) == "" {
		return fmt.Errorf("vendornotice: an unextracted record with no pointer (reason %q); without "+
			"one nobody can open the original, which is the whole of what the record is for (FR-074)",
			recorded.Reason)
	}
	f.mu.Lock()
	f.cycle.Read++
	f.mu.Unlock()
	unextracted := Unextracted{
		Reason: UnextractedReason(recorded.Reason), Field: recorded.Field,
		Pointer: recorded.Pointer, Source: SourceKind(recorded.Source),
	}
	f.recordUnextracted(unextracted)
	f.log.WarnContext(ctx, "announcement unextracted", "reason", unextracted.Reason,
		"field", unextracted.Field, "pointer", unextracted.Pointer)
	return nil
}

// applyAnnouncement runs one reading through the allowlist, extraction validation and emission.
func (f *Feeder) applyAnnouncement(ctx context.Context, desc feeder.Description, em feeder.Emitter, recorded recordedAnnouncement, at time.Time) error {
	f.mu.Lock()
	f.cycle.Read++
	f.mu.Unlock()

	a, err := recorded.announcement()
	if err != nil {
		var unextracted *Unextracted
		if errors.As(err, &unextracted) {
			f.recordUnextracted(*unextracted)
			// Not emitted as a change (FR-074). The record is the deliverable: a human can see what
			// the feeder could not read.
			f.log.WarnContext(ctx, "announcement unextracted", "reason", unextracted.Reason,
				"field", unextracted.Field, "pointer", unextracted.Pointer)
			return nil
		}
		return err
	}

	decision := f.opts.Allowlist.Decide(a.Vendor, a.Product, a.Pointer)
	if !decision.InScope {
		f.mu.Lock()
		f.cycle.DroppedByAllowlist++
		f.cycle.Drops = append(f.cycle.Drops, decision.Drop)
		f.mu.Unlock()
		f.log.InfoContext(ctx, "announcement dropped", "reason", decision.Drop.Reason,
			"vendor", decision.Drop.Vendor, "product", decision.Drop.Product)
		return nil
	}

	if err := AssertObservedNotInFuture(at, f.now()); err != nil {
		return err
	}

	f.mu.Lock()
	f.cycle.Extracted++
	ref := NoticeRef(a).GetValue()
	if f.seenNotices[ref] {
		// A second source carried the same notice. It is *counted* as merged and still emitted: the
		// feeder suppresses nothing, because suppression is a silent merge with no audit trail
		// (FR-070). The published rules merge the two observations, carrying their rule and rationale.
		f.cycle.Merged++
	}
	f.seenNotices[ref] = true
	newVendor := !f.seenVendors[decision.Vendor.Slug]
	f.seenVendors[decision.Vendor.Slug] = true
	f.mu.Unlock()

	if newVendor {
		if err := f.emitVendor(ctx, desc, em, decision.Vendor, at); err != nil {
			return err
		}
	}

	state := graphv1.AnnouncementState_ANNOUNCED
	switch strings.ToLower(strings.TrimSpace(recorded.State)) {
	case "", "announced":
	case "cancelled":
		state = graphv1.AnnouncementState_CANCELLED
	case "superseded":
		state = graphv1.AnnouncementState_SUPERSEDED
	case "confirmed":
		// Reachable only from a payload that says so, and still checked: the passage of time is not
		// an observation (FR-066).
		state = graphv1.AnnouncementState_CONFIRMED
	default:
		return fmt.Errorf("vendornotice: announcement state %q is not one of announced, cancelled, "+
			"superseded or confirmed", recorded.State)
	}
	if err := AssertNotPromotedBySilence(state, a.Window, f.now()); err != nil {
		return err
	}

	change, err := announcement(a, decision.Vendor, at, state)
	if err != nil {
		return err
	}
	// The event id carries the ref, the state **and the window**, and each part is load-bearing.
	//
	// The ref alone would make a reschedule a duplicate of the announcement it replaces: FR-068 keeps
	// both readings on one change node, so the ref is deliberately the same and the id has to
	// distinguish them by something else. The window is what changed, so the window is what
	// distinguishes them — and re-reading the identical announcement still produces the identical id,
	// which is the no-op FR-076 requires.
	//
	// `state.String()` rather than a conversion: `string(state)` on a protobuf enum yields the rune at
	// that code point, so ANNOUNCED became "" and two states could collide in an id nobody could
	// read. It is the kind of mistake that produces a fixture with a missing version and no error.
	id := feeder.NewID(desc.SourceID, "change", change.Ref.GetValue(), state.String(),
		windowKey(a.Window))
	if err := emit(ctx, em, feeder.ObserveChange(desc, id, change)); err != nil {
		return err
	}

	for _, claim := range Claims(a, decision.Vendor) {
		if err := f.emitClaim(ctx, desc, em, change.Ref, claim, at); err != nil {
			return err
		}
	}

	f.mu.Lock()
	f.cycle.Emitted++
	f.mu.Unlock()
	return nil
}

// announcement decodes the recorded shape into the typed one, validating it.
func (r recordedAnnouncement) announcement() (Announcement, error) {
	a := Announcement{
		Vendor:            r.Vendor,
		Product:           r.Product,
		Kind:              AnnouncementKind(strings.ToLower(strings.TrimSpace(r.Kind))),
		AffectedResources: r.AffectedResources,
		NoticeID:          r.NoticeID,
		Pointer:           r.Pointer,
		Source:            SourceKind(strings.ToLower(strings.TrimSpace(r.Source))),
	}
	a.Window.StartUnknown = r.WindowStartUnknown
	if r.WindowStart != "" {
		start, err := time.Parse(time.RFC3339, r.WindowStart)
		if err != nil {
			return Announcement{}, &Unextracted{
				Reason: UnextractedInvalidWindow, Field: "window_start", Pointer: r.Pointer,
				Source: SourceKind(r.Source),
			}
		}
		a.Window.Start = start
	}
	if r.WindowEnd != "" {
		end, err := time.Parse(time.RFC3339, r.WindowEnd)
		if err != nil {
			return Announcement{}, &Unextracted{
				Reason: UnextractedInvalidWindow, Field: "window_end", Pointer: r.Pointer,
				Source: SourceKind(r.Source),
			}
		}
		a.Window.End = end
	}
	a = a.Normalise()
	if err := a.Validate(); err != nil {
		return Announcement{}, err
	}
	return a, nil
}

func (f *Feeder) recordUnextracted(u Unextracted) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cycle.FailedToExtract++
	f.cycle.Unextracted = append(f.cycle.Unextracted, u)
}

func (f *Feeder) emitVendor(ctx context.Context, desc feeder.Description, em feeder.Emitter, vendor Vendor, at time.Time) error {
	node, err := VendorNode(vendor, at)
	if err != nil {
		return err
	}
	node.SourceObservedAt = at
	if err := emit(ctx, em, feeder.UpsertNode(desc,
		feeder.NewID(desc.SourceID, "vendor", vendor.Slug), node)); err != nil {
		return err
	}
	for _, claim := range VendorClaims(vendor) {
		if err := f.emitClaim(ctx, desc, em, node.Ref, claim, at); err != nil {
			return err
		}
	}
	return nil
}

func (f *Feeder) emitClaim(ctx context.Context, desc feeder.Description, em feeder.Emitter, subject *graphv1.Ref, claim Claim, at time.Time) error {
	props := feeder.NewProps().Str("sre.vendor.claim_source", claim.Why)
	// Sorted, so that one observation emits the same bytes twice and no golden depends on Go's map
	// iteration order.
	for _, key := range slices.Sorted(maps.Keys(claim.Attrs)) {
		props = props.Str(key, claim.Attrs[key])
	}
	attrs, err := props.Build()
	if err != nil {
		return err
	}
	return emit(ctx, em, feeder.IdentityClaim(desc,
		feeder.NewID(desc.SourceID, "claim", subject.GetValue(), claim.Namespace, claim.Value),
		feeder.IdentityFact{
			Meta:       feeder.Meta{SourceObservedAt: at},
			Subject:    subject,
			Claim:      claim.Ref(),
			Attributes: attrs,
		}))
}

// closeCycle records the gaps and checkpoints.
//
// An unreachable source produces a **gap**, not silence (FR-075). The checkpoint's `gapBefore` is set
// when any source could not be reached, which is what tells the graph that the quiet in this extent was
// ignorance rather than absence.
func (f *Feeder) closeCycle(ctx context.Context, em feeder.Emitter, marker cycleMarker, at time.Time) error {
	f.mu.Lock()
	for _, gap := range marker.Gaps {
		if strings.TrimSpace(gap.Why) == "" {
			f.mu.Unlock()
			return fmt.Errorf("vendornotice: a gap on source %q with no reason; a gap with no reason "+
				"is a silence with a label on it (FR-075)", gap.Source)
		}
		f.cycle.Gaps = append(f.cycle.Gaps, Gap{Source: SourceKind(gap.Source), Why: gap.Why})
	}
	cycle := f.cycle
	f.mu.Unlock()

	f.log.InfoContext(ctx, "vendor-notice cycle", "report", cycle.Note())
	return em.Checkpoint(ctx, feeder.CheckpointFact{
		ExtentFrom: at.Add(-f.Describe().ReorderingWindow),
		ExtentTo:   at,
		GapBefore:  len(cycle.Gaps) > 0,
		// The cycle's own account — which sources were read, which were skipped and why — which
		// this feeder already renders for its log line and had nowhere to put on the event.
		Note: cycle.Note(),
	})
}

// emit sends one event and turns a rejection into an error naming it.
func emit(ctx context.Context, em feeder.Emitter, ev *graphv1.EventEnvelope) error {
	result, err := em.Emit(ctx, ev)
	if err != nil {
		return err
	}
	if result.GetStatus() == graphv1.IngestResult_REJECTED {
		return fmt.Errorf("vendornotice: the graph refused event %s: %s %s",
			ev.GetEventId(), result.GetReasonCode(), result.GetReasonDetail())
	}
	return nil
}
