// SPDX-License-Identifier: Apache-2.0

package vendornotice_test

import (
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
	"pgregory.net/rapid"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	vn "github.com/Pierre-Theophile/aisre/internal/feeders/vendornotice"
	"github.com/Pierre-Theophile/aisre/internal/query"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The announced-fact invariants, over generated inputs (T089, FR-063, FR-065, FR-067, SC-007).
//
// The worked examples in announce_test.go pin these at a handful of instants. These are the statements
// the feature rests on, and they have to hold at *every* instant, because an announced fact is the one
// kind of fact in this system whose valid time leads its observed time — and every mistake available
// there is a mistake that looks like a working feeder:
//
//	(i)   observed time is never in the future. Valid time may lead it; the asymmetry is
//	      one-directional, and the way it goes wrong is a feeder that sees a window ahead of it and
//	      moves its own observation to match (FR-063).
//	(ii)  a cancellation never closes a valid interval. It closes the *observed* interval and opens
//	      another on the same change ref, so "what did we believe on 20 September about 2 October?"
//	      stays answerable (FR-067).
//	(iii) a change whose valid start is after a ranked list's reference instant is excluded for that
//	      stated reason rather than by scoring low (FR-065, SC-007).
//
// (iii) is asserted against the real ranker rather than a restatement of the rule, because the rule
// exists to keep a forward-looking read from being reachable by accident from a backward-looking one —
// and "by accident" means through the code, not through a test's idea of the code.

// timestamp renders an instant the way the graph carries it.
func timestamp(at time.Time) *timestamppb.Timestamp { return timestamppb.New(at.UTC()) }

// announcementGen generates an announcement whose window may be anywhere relative to the read.
func announcementGen(t *rapid.T) (vn.Announcement, time.Time) {
	// Instants are drawn as offsets in seconds from a fixed base, so a failure reproduces exactly.
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	observedOffset := rapid.Int64Range(0, 400*24*3600).Draw(t, "observed_offset")
	// The window may begin before the read (a notice about something already under way), at it, or
	// long after it. All three are legal, and the fortnight-ahead case is the one this feature is for.
	startDelta := rapid.Int64Range(-90*24*3600, 365*24*3600).Draw(t, "start_delta")
	duration := rapid.Int64Range(0, 30*24*3600).Draw(t, "duration")
	vague := rapid.Bool().Draw(t, "vague_start")
	open := rapid.Bool().Draw(t, "open_end")

	observedAt := base.Add(time.Duration(observedOffset) * time.Second)
	window := vn.Window{}
	switch {
	case vague:
		window.StartUnknown = true
	default:
		window.Start = observedAt.Add(time.Duration(startDelta) * time.Second)
	}
	if !open && !vague {
		window.End = window.Start.Add(time.Duration(duration) * time.Second)
	}
	kind := vn.AnnouncementKind(rapid.SampledFrom([]string{
		string(vn.KindMaintenance), string(vn.KindDeprecation), string(vn.KindIncident),
	}).Draw(t, "kind"))
	return vn.Announcement{
		Vendor: "acme-gpu", Product: "inference-api", Kind: kind, Window: window,
		NoticeID: rapid.SampledFrom([]string{"", "ACME-1", "ACME-2"}).Draw(t, "notice_id"),
		Pointer:  "mailbox:" + rapid.StringMatching(`msg-[0-9]{4}`).Draw(t, "pointer"),
		Source:   vn.SourceMailbox,
	}, observedAt
}

// (i) Observed time is never in the future, whatever the window says. The one-directional asymmetry is
// the whole of FR-063, and the mistake it forbids is helpful rather than malicious: a feeder that saw a
// window a fortnight out and stamped its observation there, so the graph would say it knew on 2 October
// what it read on 17 September.
func TestObservedTimeIsNeverInTheFuture(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		a, observedAt := announcementGen(rt)
		// The clock is drawn independently of the observation, which is the point: the feeder is
		// handed an instant and must not trust it.
		clockDelta := rapid.Int64Range(-30*24*3600, 30*24*3600).Draw(rt, "clock_delta")
		now := observedAt.Add(time.Duration(clockDelta) * time.Second)

		fact, err := vn.Announced(a, testVendor(), observedAt)
		assertion := vn.AssertObservedNotInFuture(observedAt, now)

		if observedAt.After(now) {
			if assertion == nil {
				rt.Fatalf("an observation at %s with the clock at %s was accepted", observedAt, now)
			}
			return
		}
		if assertion != nil {
			rt.Fatalf("an observation at %s with the clock at %s was refused: %v", observedAt, now, assertion)
		}
		if err != nil {
			rt.Fatalf("Announced: %v", err)
		}
		// And the fact the feeder built stamps its observation where it was told, never at the window.
		if !fact.SourceObservedAt.Equal(observedAt) {
			rt.Fatalf("the observation moved from %s to %s", observedAt, fact.SourceObservedAt)
		}
		if !a.Window.StartUnknown && !fact.ValidAt.Equal(a.Window.Start) {
			rt.Fatalf("the valid start moved from %s to %s", a.Window.Start, fact.ValidAt)
		}
		// Valid time leading observed time is legal and must not be quietly clamped: that clamp is
		// the same bug wearing the other hat.
		if !a.Window.StartUnknown && a.Window.Start.After(observedAt) && !fact.ValidAt.After(fact.SourceObservedAt) {
			rt.Fatalf("an announced window at %s read at %s lost its lead", a.Window.Start, observedAt)
		}
	})
}

// (ii) A cancellation never closes a valid interval. It is a correction: the observed interval closes
// and another opens on the **same** change ref, and the announced window is emitted unchanged. The
// tempting implementation — end the valid interval at the cancellation — is what makes "what did we
// believe on 20 September about 2 October?" unanswerable, because the belief is gone rather than
// superseded (FR-067, FR-068).
func TestACancellationNeverClosesAValidInterval(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		a, observedAt := announcementGen(rt)
		correctionDelay := rapid.Int64Range(0, 60*24*3600).Draw(rt, "correction_delay")
		correctedAt := observedAt.Add(time.Duration(correctionDelay) * time.Second)

		announced, err := vn.Announced(a, testVendor(), observedAt)
		if err != nil {
			rt.Fatalf("Announced: %v", err)
		}
		for name, build := range map[string]func(vn.Announcement, vn.Vendor, time.Time) (feeder.ChangeFact, error){
			"cancelled":  vn.Cancelled,
			"superseded": vn.Superseded,
			"confirmed":  vn.Confirmed,
		} {
			corrected, err := build(a, testVendor(), correctedAt)
			if err != nil {
				rt.Fatalf("%s: %v", name, err)
			}
			// The same change, addressed the same way. A second node would be two adjacent rows for
			// one window in front of somebody being paged.
			if corrected.Ref.GetValue() != announced.Ref.GetValue() ||
				corrected.Ref.GetNamespace() != announced.Ref.GetNamespace() {
				rt.Fatalf("%s addressed %s rather than %s", name,
					corrected.Ref.GetValue(), announced.Ref.GetValue())
			}
			// The valid interval is byte-identical, including the unknown marker.
			if !corrected.ValidAt.Equal(announced.ValidAt) ||
				!corrected.ValidEnd.Equal(announced.ValidEnd) ||
				corrected.ValidFromUnknown != announced.ValidFromUnknown {
				rt.Fatalf("%s rewrote the valid interval: [%s, %s) unknown=%v, was [%s, %s) unknown=%v",
					name, corrected.ValidAt, corrected.ValidEnd, corrected.ValidFromUnknown,
					announced.ValidAt, announced.ValidEnd, announced.ValidFromUnknown)
			}
			// What moves is the observation, which is what makes it a correction.
			if !corrected.SourceObservedAt.Equal(correctedAt) {
				rt.Fatalf("%s observed at %s rather than %s", name, corrected.SourceObservedAt, correctedAt)
			}
			if corrected.AnnouncementState == announced.AnnouncementState {
				rt.Fatalf("%s carries the same state as the announcement (%s)",
					name, corrected.AnnouncementState)
			}
		}
	})
}

// (iii) An announced change whose valid start is after the reference is excluded for that reason, and
// never merely ranked low (FR-065, SC-007).
//
// Asserted against the real ranker. The property has two halves and both matter: the announced future
// change is excluded *and* it is nowhere in the ranked list — a change that appeared in both would let a
// consumer reading only `changes` reach a forward-looking answer from a backward-looking question, which
// is the accident the requirement names.
func TestAnAnnouncedFutureChangeIsExcludedAndNotRanked(t *testing.T) {
	t.Parallel()

	rapid.Check(t, func(rt *rapid.T) {
		reference := time.Date(2026, 9, 21, 2, 10, 0, 0, time.UTC)
		// The announced window is drawn on both sides of the reference: after it, the change cannot
		// have caused anything; at or before it, it is an ordinary candidate and must stay one.
		delta := rapid.Int64Range(-30*24*3600, 30*24*3600).Draw(rt, "valid_start_delta")
		validFrom := reference.Add(time.Duration(delta) * time.Second)
		state := graphv1.AnnouncementState(rapid.SampledFrom([]int32{
			int32(graphv1.AnnouncementState_ANNOUNCEMENT_STATE_UNSPECIFIED),
			int32(graphv1.AnnouncementState_ANNOUNCED),
			int32(graphv1.AnnouncementState_CONFIRMED),
			int32(graphv1.AnnouncementState_CANCELLED),
			int32(graphv1.AnnouncementState_SUPERSEDED),
		}).Draw(rt, "state"))

		candidate := query.Candidate{
			Change: &graphv1.NodeVersion{
				EntityId: "entity-announced",
				Valid:    &graphv1.Interval{Start: timestamp(validFrom)},
				Change: &graphv1.Change{
					Kind: graphv1.ChangeKind_CLOUD_MAINTENANCE, AnnouncementState: state,
				},
			},
			Hop: 1,
		}
		ranked, excluded := query.Rank([]query.Candidate{candidate},
			query.RankParams{Reference: reference, HopCap: 3})

		announcedAhead := state == graphv1.AnnouncementState_ANNOUNCED && validFrom.After(reference)
		if announcedAhead {
			if len(excluded) != 1 {
				rt.Fatalf("an announced change valid from %s was not excluded against a reference of "+
					"%s: excluded=%d", validFrom, reference, len(excluded))
			}
			if excluded[0].GetReason() != graphv1.ExclusionReason_EXCLUSION_ANNOUNCED_AFTER_REFERENCE {
				rt.Fatalf("the exclusion reason is %s", excluded[0].GetReason())
			}
			// The reason is stated with the instants it was decided on, so a reader can check it.
			if excluded[0].GetDetail() == "" {
				rt.Fatal("the exclusion states no reason a reader could check")
			}
			// And it is not also in the ranked list. Both would be worse than either.
			if len(ranked) != 0 {
				rt.Fatalf("the excluded change was also ranked, at score %v — a consumer reading only "+
					"`changes` would reach a forward-looking answer from a backward-looking question",
					ranked[0].GetScore())
			}
			return
		}
		// Everything else is ranked. In particular a change that *happened* after the reference keeps
		// ADR-0005 D4's decayed score rather than vanishing: it may be an effect or a remediation,
		// and an operator wants to see it.
		if len(excluded) != 0 {
			rt.Fatalf("a change in state %s valid from %s against a reference of %s was excluded: %s",
				state, validFrom, reference, excluded[0].GetDetail())
		}
		if len(ranked) != 1 {
			rt.Fatalf("a rankable candidate produced %d ranked items", len(ranked))
		}
		if validFrom.After(reference) && !ranked[0].GetPostReference() {
			rt.Fatal("a change after the reference is not marked post-reference")
		}
	})
}

// The exclusion is not the sign of the time distance alone. Both halves of the condition are load
// bearing, and each has a failure that looks like a working list: without the state check every change
// later in the window disappears; without the instant check an announced maintenance already under way
// is excluded from the one list that should contain it.
func TestTheExclusionNeedsBothTheStateAndTheInstant(t *testing.T) {
	t.Parallel()

	reference := time.Date(2026, 9, 21, 2, 10, 0, 0, time.UTC)
	build := func(state graphv1.AnnouncementState, validFrom time.Time) query.Candidate {
		return query.Candidate{
			Change: &graphv1.NodeVersion{
				EntityId: "entity-change",
				Valid:    &graphv1.Interval{Start: timestamp(validFrom)},
				Change:   &graphv1.Change{AnnouncementState: state},
			},
			Hop: 1,
		}
	}
	cases := map[string]struct {
		candidate query.Candidate
		excluded  bool
	}{
		"an announced window a fortnight ahead": {
			candidate: build(graphv1.AnnouncementState_ANNOUNCED, reference.Add(14*24*time.Hour)),
			excluded:  true,
		},
		"an announced window already under way": {
			candidate: build(graphv1.AnnouncementState_ANNOUNCED, reference.Add(-time.Hour)),
			excluded:  false,
		},
		"a rollback five minutes after the alert": {
			candidate: build(graphv1.AnnouncementState_ANNOUNCEMENT_STATE_UNSPECIFIED, reference.Add(5*time.Minute)),
			excluded:  false,
		},
		"a confirmed window ahead of the reference": {
			// Confirmed means somebody observed that it happened, so the state no longer says "has
			// not happened yet" — and a future instant on a confirmed change is a disagreement to
			// surface rather than an exclusion to make silently.
			candidate: build(graphv1.AnnouncementState_CONFIRMED, reference.Add(time.Hour)),
			excluded:  false,
		},
		"a cancelled window ahead of the reference": {
			candidate: build(graphv1.AnnouncementState_CANCELLED, reference.Add(time.Hour)),
			excluded:  false,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ranked, excluded := query.Rank([]query.Candidate{tc.candidate},
				query.RankParams{Reference: reference, HopCap: 3})
			if tc.excluded {
				if len(excluded) != 1 || len(ranked) != 0 {
					t.Fatalf("ranked=%d excluded=%d, want it excluded", len(ranked), len(excluded))
				}
				return
			}
			if len(excluded) != 0 || len(ranked) != 1 {
				t.Fatalf("ranked=%d excluded=%d, want it ranked", len(ranked), len(excluded))
			}
		})
	}
}
