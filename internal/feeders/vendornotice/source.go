// SPDX-License-Identifier: Apache-2.0

package vendornotice

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The source layer (T083, FR-058, FR-075, contract §1).
//
// # An unreachable source produces a gap, not silence
//
// This is the whole reason the layer exists rather than each adapter writing payloads directly. A
// mailbox that times out and a mailbox with nothing in it produce the same thing — no announcements —
// and the graph cannot tell them apart afterwards. So a source that fails is recorded as a **gap** in
// the cycle marker, which reaches the checkpoint's `gapBefore`, and the extent the feeder claims to
// have observed stops being a claim about a window nobody read.
//
// The failure mode this defeats is the one the coverage audit found: a notice nobody read, invisible
// because nothing said it had not been read.
//
// # Any subset of the three sources
//
// FR-058. A deployment with no status-page adapter still reads the mailbox. A Poller with no sources
// at all is refused at construction rather than polling nothing forever: that configuration produces
// an unbroken run of empty cycles, which is the most convincing possible impression of a working
// feeder.
//
// # Cadence, and the publisher's own limit
//
// The cadence is configuration. What is not configuration is a publisher's stated limit: a `429` with
// `Retry-After`, or a documented poll interval, is an instruction and the client waits it out
// (see `http.go`). A feeder that polled through a stated limit would get itself blocked, and a blocked
// source reads as a quiet week.

// Batch is what one source produced in one cycle.
//
// Unextracted travels beside the announcements rather than being dropped, because an announcement the
// source could not read is a fact about the cycle: FR-074 requires it recorded with its reason and its
// pointer so a human can see what the feeder could not read.
type Batch struct {
	Announcements []Announcement
	// Corrections are readings that *change* what an earlier announcement said: a cancellation, a
	// reschedule, or a confirmation that the window happened. They travel separately because they
	// are not new announcements — each one closes the observation on the **same** change ref and
	// opens another, and a source that returned a cancellation as an announcement would create a
	// second change node saying the opposite of the first (FR-067).
	Corrections []Correction
	Unextracted []Unextracted
	// Considered is how many candidate items the source looked at, whether or not any of them turned
	// out to be an announcement: messages in the mailbox folder, entries in the feed, rows on the
	// page.
	//
	// It exists because "read nothing" and "read two hundred entries and none of them was an
	// announcement" are the same number of announcements and completely different facts about the
	// estate. The second says the source works and the vendor is quiet; the first says nobody knows.
	// Without this count, a changelog whose stated rules stopped matching after a redesign reports a
	// clean zero for ever.
	Considered int
	// Gaps are parts of this source that could not be read while others could: one of several
	// mailboxes refused a connection, one page of a paginated feed timed out.
	//
	// They exist because a source is not always all-or-nothing, and FR-132a is why that matters here:
	// per-user delivery is a first-class path, so the mailbox source reads **several** mailboxes, and
	// one individual's mailbox being unreachable must not throw away the four that answered — nor be
	// reported as a clean read of five.
	Gaps []Gap
}

// Correction is one reading that revises an earlier one.
type Correction struct {
	// Announcement identifies the notice being corrected. It carries the vendor's notice identifier
	// where the source has one, which is what makes the correction land on the same change.
	Announcement Announcement
	// State is what the correction says: cancelled, superseded or confirmed. It is never
	// `announced` — that is an announcement, not a correction — and never empty.
	State string
}

// Announcer is one of the three sources.
//
// Read returns what the source found. An error means the source could not be *read* — unreachable,
// refused, timed out — and the caller turns it into a gap. An error must never mean "nothing to
// report": that is an empty Batch, and the distinction is the point of the interface.
type Announcer interface {
	// Kind names the source for the per-cycle report and for any gap it causes.
	Kind() SourceKind
	// Read polls once.
	Read(ctx context.Context) (Batch, error)
}

// PollerOptions configures a live poll.
type PollerOptions struct {
	// Sources are the adapters to poll. At least one is required.
	Sources []Announcer
	// Cadence is how long to wait between cycles. Zero means one cycle and then io.EOF, which is
	// what a one-shot run and every test use.
	Cadence time.Duration
	// Cycles bounds a cadenced run. Zero means unbounded: it runs until the context is done.
	Cycles int
	// Now supplies the clock. Nil uses time.Now.
	Now func() time.Time
	// Sleep waits between cycles. Nil uses a context-aware sleep; a test supplies its own so that
	// the cadence is asserted without spending it.
	Sleep func(ctx context.Context, d time.Duration) error
}

// Poller polls any subset of the three sources and yields the payloads the feeder reads.
//
// One cycle yields one announcements payload per source that answered, then one cycle marker carrying
// the gaps. The marker is always emitted, including when every source failed: a cycle in which nothing
// could be read is exactly the cycle whose gap must reach the checkpoint.
type Poller struct {
	sources []Announcer
	cadence time.Duration
	cycles  int
	now     func() time.Time
	sleep   func(ctx context.Context, d time.Duration) error

	// pending holds the payloads of the current cycle, in order.
	pending []feeder.Payload
	// done counts completed cycles.
	done int
	// started marks whether the first cycle has been polled, so the first Next does not wait a
	// cadence before reading anything.
	started bool
}

// NewPoller builds a live source over the configured adapters.
func NewPoller(opts PollerOptions) (*Poller, error) {
	if len(opts.Sources) == 0 {
		return nil, errors.New("vendornotice: a poller with no sources. It would yield an unbroken " +
			"run of empty cycles, which is the most convincing impression of a working feeder there " +
			"is — so it is refused rather than started (FR-058)")
	}
	seen := map[SourceKind]bool{}
	for _, source := range opts.Sources {
		if source == nil {
			return nil, errors.New("vendornotice: a nil source in the poller")
		}
		if seen[source.Kind()] {
			return nil, fmt.Errorf("vendornotice: two sources both call themselves %q; a gap could "+
				"then name a source without saying which one", source.Kind())
		}
		seen[source.Kind()] = true
	}
	if opts.Cadence < 0 {
		return nil, fmt.Errorf("vendornotice: a negative cadence (%s)", opts.Cadence)
	}
	if opts.Cycles < 0 {
		return nil, fmt.Errorf("vendornotice: a negative cycle count (%d)", opts.Cycles)
	}
	p := &Poller{
		sources: append([]Announcer(nil), opts.Sources...),
		cadence: opts.Cadence,
		cycles:  opts.Cycles,
		now:     opts.Now,
		sleep:   opts.Sleep,
	}
	if p.now == nil {
		p.now = func() time.Time { return time.Now().UTC() }
	}
	if p.sleep == nil {
		p.sleep = sleepContext
	}
	return p, nil
}

// Next implements feeder.Source.
func (p *Poller) Next(ctx context.Context) (feeder.Payload, error) {
	for len(p.pending) == 0 {
		if err := p.nextCycle(ctx); err != nil {
			return feeder.Payload{}, err
		}
	}
	next := p.pending[0]
	p.pending = p.pending[1:]
	return next, nil
}

// nextCycle polls every source once and fills pending.
func (p *Poller) nextCycle(ctx context.Context) error {
	if p.cadence == 0 && p.started {
		return io.EOF
	}
	if p.cycles > 0 && p.done >= p.cycles {
		return io.EOF
	}
	if p.started {
		if err := p.sleep(ctx, p.cadence); err != nil {
			return err
		}
	}
	p.started = true

	at := p.now().UTC()
	var gaps []cycleGap
	for _, source := range p.sources {
		batch, err := source.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// The gap carries the source and the reason. The reason is the error's text, which comes
			// from this repository's own adapters rather than from an announcement, so it is not
			// untrusted content — and closeCycle refuses a gap with no reason at all, because a gap
			// with no reason is a silence with a label on it.
			gaps = append(gaps, cycleGap{Source: string(source.Kind()), Why: err.Error()})
			continue
		}
		// A source that answered may still report that part of it could not be read. That partial
		// gap reaches the checkpoint exactly like a whole-source one: the extent the feeder claims
		// must not cover a mailbox nobody opened.
		for _, gap := range batch.Gaps {
			why := strings.TrimSpace(gap.Why)
			if why == "" {
				return fmt.Errorf("vendornotice: the %s source reported a gap with no reason; a gap "+
					"with no reason is a silence with a label on it (FR-075)", source.Kind())
			}
			name := string(gap.Source)
			if name == "" {
				name = string(source.Kind())
			}
			gaps = append(gaps, cycleGap{Source: name, Why: why})
		}
		payloads, err := payloadsFor(source.Kind(), batch, at)
		if err != nil {
			return err
		}
		p.pending = append(p.pending, payloads...)
	}
	marker, err := cycleMarkerPayload(gaps, at)
	if err != nil {
		return err
	}
	p.pending = append(p.pending, marker)
	p.done++
	return nil
}

// payloadsFor renders one source's batch: the announcements and the unextracted records in one
// payload, the corrections in another.
//
// A source that read nothing yields no payload at all — there is nothing to record — and the cycle
// marker still closes the cycle, which is what keeps "quiet" and "unread" apart.
func payloadsFor(kind SourceKind, batch Batch, at time.Time) ([]feeder.Payload, error) {
	var out []feeder.Payload
	if batch.Considered > 0 || len(batch.Announcements) > 0 || len(batch.Unextracted) > 0 {
		body := announcementsPayload{Considered: batch.Considered, Source: string(kind)}
		for _, a := range batch.Announcements {
			recorded, err := recordAnnouncement(a, "")
			if err != nil {
				return nil, err
			}
			body.Announcements = append(body.Announcements, recorded)
		}
		for _, u := range batch.Unextracted {
			if strings.TrimSpace(string(u.Reason)) == "" || strings.TrimSpace(u.Pointer) == "" {
				return nil, fmt.Errorf("vendornotice: the %s source returned an unextracted record "+
					"with no reason or no pointer (%+v); the record exists so a human can open the "+
					"original and see what could not be read (FR-074)", kind, u)
			}
			body.Unextracted = append(body.Unextracted, recordedUnextracted{
				Reason: string(u.Reason), Field: u.Field, Pointer: u.Pointer, Source: string(u.Source),
			})
		}
		payload, err := encodePayload(kind, PayloadAnnouncements, body, at)
		if err != nil {
			return nil, err
		}
		out = append(out, payload)
	}
	if len(batch.Corrections) > 0 {
		body := announcementsPayload{}
		for _, correction := range batch.Corrections {
			state := strings.ToLower(strings.TrimSpace(correction.State))
			switch state {
			case "cancelled", "superseded", "confirmed":
			case "", "announced":
				return nil, fmt.Errorf("vendornotice: the %s source returned a correction whose state "+
					"is %q; a correction says what changed about an announcement, and %q is an "+
					"announcement", kind, correction.State, state)
			default:
				return nil, fmt.Errorf("vendornotice: the %s source returned a correction with state "+
					"%q, which is not one of cancelled, superseded or confirmed", kind, correction.State)
			}
			recorded, err := recordAnnouncement(correction.Announcement, state)
			if err != nil {
				return nil, err
			}
			body.Announcements = append(body.Announcements, recorded)
		}
		payload, err := encodePayload(kind, PayloadCorrections, body, at)
		if err != nil {
			return nil, err
		}
		out = append(out, payload)
	}
	return out, nil
}

// encodePayload sorts and marshals one batch.
//
// Sorted by pointer, so that two readings of one cycle produce the same bytes: a recording is a
// fixture, and a fixture that depended on the order a vendor's API happened to list things would fail
// the double-delivery and shuffle steps for a reason that has nothing to do with the feeder.
func encodePayload(kind SourceKind, payloadKind string, body announcementsPayload, at time.Time) (feeder.Payload, error) {
	sort.SliceStable(body.Announcements, func(i, j int) bool {
		return body.Announcements[i].Pointer < body.Announcements[j].Pointer
	})
	sort.SliceStable(body.Unextracted, func(i, j int) bool {
		return body.Unextracted[i].Pointer < body.Unextracted[j].Pointer
	})
	raw, err := json.Marshal(body)
	if err != nil {
		return feeder.Payload{}, fmt.Errorf("vendornotice: encoding the %s %s batch: %w",
			kind, payloadKind, err)
	}
	return feeder.Payload{Kind: payloadKind, At: at, Bytes: raw}, nil
}

func cycleMarkerPayload(gaps []cycleGap, at time.Time) (feeder.Payload, error) {
	sort.SliceStable(gaps, func(i, j int) bool { return gaps[i].Source < gaps[j].Source })
	raw, err := json.Marshal(cycleMarker{Gaps: gaps})
	if err != nil {
		return feeder.Payload{}, fmt.Errorf("vendornotice: encoding the cycle marker: %w", err)
	}
	return feeder.Payload{Kind: PayloadCycleMarker, At: at, Bytes: raw}, nil
}

// recordAnnouncement renders one announcement onto the wire shape.
func recordAnnouncement(a Announcement, state string) (recordedAnnouncement, error) {
	a = a.Normalise()
	if err := a.Validate(); err != nil {
		return recordedAnnouncement{}, err
	}
	out := recordedAnnouncement{
		Vendor: a.Vendor, Product: a.Product, Kind: string(a.Kind),
		WindowStartUnknown: a.Window.StartUnknown,
		AffectedResources:  a.AffectedResources,
		NoticeID:           a.NoticeID, Pointer: a.Pointer, Source: string(a.Source),
		State: state,
	}
	if !a.Window.StartUnknown {
		out.WindowStart = a.Window.Start.UTC().Format(time.RFC3339)
	}
	if !a.Window.End.IsZero() {
		out.WindowEnd = a.Window.End.UTC().Format(time.RFC3339)
	}
	return out, nil
}

// sleepContext waits for d, or returns early when the context is done.
func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
