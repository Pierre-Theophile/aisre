// SPDX-License-Identifier: Apache-2.0

package campaign_test

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/campaign"
)

// The campaign record (T154; FR-130, FR-131, FR-132).
//
// Every test here is written against the question the record exists to answer rather than against
// its fields. The question is FR-130's: a year later, somebody reads the corpus, finds no events for
// a project over a week, and has to decide between **"it was quiet"** and **"we were not looking"**.
// Those are opposite conclusions, and the record is the only thing that can tell them apart.

var (
	campaignStart = time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	scopeAgreed   = time.Date(2026, 9, 8, 9, 0, 0, 0, time.UTC)
)

func validRecord() campaign.Record {
	return campaign.Record{
		ID:           "campaign-2026-09",
		Organisation: "twin",
		StartedAt:    campaignStart,
		Scopes: []campaign.Scope{{
			From:     campaignStart,
			To:       scopeAgreed,
			Projects: []string{"twin-production"},
			Why:      "recording began before the scope was agreed (FR-130)",
		}, {
			From:     scopeAgreed,
			Projects: []string{"twin-production", "twin-staging"},
			Why:      "staging added once the owner agreed the scope",
		}},
		Mailbox: campaign.Mailbox{
			Path:            campaign.AccessSharedMailbox,
			AuthorisedBy:    "the platform owner",
			AuthorisedOn:    campaignStart,
			SenderAllowlist: []string{"cloud-noreply"},
		},
		MailboxAddress: campaign.NoAddressRecorded,
		Signatories:    []string{"the platform owner"},
		PolicyVersion:  "1.0.0",
	}
}

// The question the record exists for, asked directly.
func TestTheRecordTellsNotPresentFromNotInScopeAtThatTime(t *testing.T) {
	t.Parallel()
	r := validRecord()

	// During the first window, staging was not in scope. A reader finding no staging events then
	// must not conclude staging was quiet.
	duringFirst := campaignStart.Add(24 * time.Hour)
	if r.InScopeAt("twin-staging", duringFirst) {
		t.Error("staging reads as in scope during the first window; it was added only when the " +
			"scope was agreed, and getting this wrong turns \"we were not looking\" into \"it was quiet\"")
	}
	if !r.InScopeAt("twin-production", duringFirst) {
		t.Error("production reads as out of scope during the first window, which it was not")
	}

	// After the change, both are.
	afterChange := scopeAgreed.Add(24 * time.Hour)
	for _, project := range []string{"twin-production", "twin-staging"} {
		if !r.InScopeAt(project, afterChange) {
			t.Errorf("%s reads as out of scope after the scope was agreed", project)
		}
	}

	// And before the campaign began, nothing was in scope — not "the earliest window", which would
	// be the record claiming to cover a period it was not recording.
	before := campaignStart.Add(-time.Hour)
	if _, ok := r.ScopeAt(before); ok {
		t.Error("an instant before the campaign started resolved to a scope window; the record " +
			"would then claim to describe a period during which nothing was being recorded")
	}
	if r.InScopeAt("twin-production", before) {
		t.Error("production reads as in scope before the campaign started")
	}
}

// The half-open window, at its boundaries. Off-by-one here is a whole poll interval attributed to
// the wrong scope.
func TestAScopeWindowIsHalfOpenAtBothEnds(t *testing.T) {
	t.Parallel()
	r := validRecord()
	first, second := r.Scopes[0], r.Scopes[1]

	if !first.Covers(first.From) {
		t.Error("a window does not cover its own start instant")
	}
	if first.Covers(first.To) {
		t.Error("a window covers its end instant; the windows are half-open [From, To), so the " +
			"instant the scope changed belongs to the NEW window and two windows would claim it")
	}
	if !second.Covers(first.To) {
		t.Error("the instant the scope changed is covered by neither window")
	}
	// The open window has no end, so it covers everything after its start — including instants in
	// the future, which is correct: the scope in force is in force until it is changed.
	if !second.Covers(scopeAgreed.Add(365 * 24 * time.Hour)) {
		t.Error("the open window does not cover a later instant; an open window is in force until " +
			"it is closed, not until now")
	}
}

// A gap between windows is the failure this validation exists for, and it is the one an eye skips.
func TestValidateRefusesScopeWindowsThatAreNotContinuous(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		mutil func(*campaign.Record)
		want  string
	}{
		{
			name: "a gap",
			mutil: func(r *campaign.Record) {
				r.Scopes[1].From = scopeAgreed.Add(48 * time.Hour)
			},
			want: "not continuous",
		},
		{
			name: "an overlap",
			mutil: func(r *campaign.Record) {
				r.Scopes[0].To = scopeAgreed.Add(48 * time.Hour)
			},
			want: "not continuous",
		},
		{
			name: "two open windows",
			mutil: func(r *campaign.Record) {
				r.Scopes[0].To = time.Time{}
			},
			want: "only the last window may be open",
		},
		{
			name: "out of order",
			mutil: func(r *campaign.Record) {
				r.Scopes[0], r.Scopes[1] = r.Scopes[1], r.Scopes[0]
			},
			want: "not in order",
		},
		{
			name: "a window ending before it starts",
			mutil: func(r *campaign.Record) {
				r.Scopes[0].To = campaignStart.Add(-time.Hour)
			},
			want: "at or before it starts",
		},
		{
			name: "a window naming no project",
			mutil: func(r *campaign.Record) {
				r.Scopes[0].Projects = nil
			},
			want: "names no project",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := validRecord()
			tc.mutil(&r)
			err := r.Validate()
			if err == nil {
				t.Fatalf("Validate accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal reads %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// FR-132's second half is the one that gets dropped: an access path with no authoriser is a
// destination, not a permission.
func TestValidateRefusesAMailboxReadNobodyAuthorised(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		mutil func(*campaign.Record)
		want  string
	}{
		{"no authoriser", func(r *campaign.Record) { r.Mailbox.AuthorisedBy = "" }, "named authoriser"},
		{"no date", func(r *campaign.Record) { r.Mailbox.AuthorisedOn = time.Time{} }, "needs a date"},
		{"an unpublished path", func(r *campaign.Record) { r.Mailbox.Path = "imap" }, "is not one of"},
		{"no path at all", func(r *campaign.Record) { r.Mailbox.Path = "" }, "is not one of"},
		{
			name: "the subscribed-member path with no note",
			mutil: func(r *campaign.Record) {
				r.Mailbox.Path = campaign.AccessSubscribedMember
				r.Mailbox.Note = ""
			},
			want: "WHOLE mailbox",
		},
		{"no signatory", func(r *campaign.Record) { r.Signatories = nil }, "at least one signatory"},
		{"a blank signatory", func(r *campaign.Record) { r.Signatories = []string{" "} }, "blank signatory"},
		{"no policy version", func(r *campaign.Record) { r.PolicyVersion = "" }, "policy version"},
		{"no scope at all", func(r *campaign.Record) { r.Scopes = nil }, "not in scope at that time"},
		{"no start instant", func(r *campaign.Record) { r.StartedAt = time.Time{} }, "start instant"},
		{"no organisation", func(r *campaign.Record) { r.Organisation = "" }, "organisation slug"},
		{
			name:  "ending before it started",
			mutil: func(r *campaign.Record) { r.EndedAt = campaignStart.Add(-time.Hour) },
			want:  "before it started",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := validRecord()
			tc.mutil(&r)
			err := r.Validate()
			if err == nil {
				t.Fatalf("Validate accepted a record with %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal reads %q, want it to mention %q", err, tc.want)
			}
		})
	}

	// And the valid record passes, so the cases above are failing for their own reason rather than
	// because the fixture record was invalid all along.
	if err := validRecord().Validate(); err != nil {
		t.Fatalf("the valid record does not validate: %v; every case above proves nothing", err)
	}
}

// The subscribed-member path is broader than a shared mailbox, and the record has to say so.
func TestTheSubscribedMemberPathIsAcceptedWithItsNote(t *testing.T) {
	t.Parallel()
	r := validRecord()
	r.Mailbox.Path = campaign.AccessSubscribedMember
	r.Mailbox.Note = "reaches this person's whole mailbox, not only the Group's notices; the Group " +
		"archive is readable by no API (research §9)"
	if err := r.Validate(); err != nil {
		t.Errorf("the subscribed-member path with a note was refused: %v", err)
	}
}

// Round-tripping through disk, and the two refusals that keep the record honest there.
func TestTheRecordRoundTripsAndIsNotSilentlyReplaced(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "campaign-2026-09")

	if campaign.Exists(dir) {
		t.Fatal("an empty directory reports a campaign record")
	}
	if err := campaign.Write(dir, validRecord()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !campaign.Exists(dir) {
		t.Error("the record was written and Exists says otherwise")
	}

	back, err := campaign.Read(dir)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if back.ID != "campaign-2026-09" || len(back.Scopes) != 2 {
		t.Errorf("the record did not round-trip: %+v", back)
	}
	if !back.Scopes[0].From.Equal(campaignStart) {
		t.Errorf("the first window's start did not round-trip: %s", back.Scopes[0].From)
	}
	if back.MailboxAddress != campaign.NoAddressRecorded {
		t.Errorf("mailbox_address is %q; it must carry the reason no address is recorded, so a "+
			"reader who goes looking finds it in the record rather than concluding somebody forgot",
			back.MailboxAddress)
	}
	// The second window is open, and an open window must survive a round trip as open rather than
	// as the zero instant meaning something else.
	if !back.Scopes[1].To.IsZero() {
		t.Errorf("the open window came back closed at %s", back.Scopes[1].To)
	}

	// Writing again is refused: a campaign record is added to, not replaced.
	if err := campaign.Write(dir, validRecord()); err == nil {
		t.Error("Write replaced an existing record; the windows it already held were true when " +
			"they were written (FR-130)")
	}
}

// Update validates the RESULT, which is the only way to catch a half-finished scope change.
func TestUpdateRefusesAChangeThatBreaksContinuity(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "campaign-2026-09")
	if err := campaign.Write(dir, validRecord()); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Closing the open window without opening the next one is exactly half of a scope change, and
	// each half is individually reasonable.
	err := campaign.Update(dir, func(r *campaign.Record) error {
		r.Scopes[1].To = scopeAgreed.Add(72 * time.Hour)
		return nil
	})
	if err != nil {
		t.Errorf("closing the last window was refused: %v. A campaign that ended has a closed last "+
			"window, so this has to be allowed", err)
	}

	// But closing a window with another after it, and leaving a gap, must not be.
	err = campaign.Update(dir, func(r *campaign.Record) error {
		r.Scopes = append(r.Scopes, campaign.Scope{
			From:     scopeAgreed.Add(96 * time.Hour), // a gap of 24h
			Projects: []string{"twin-production"},
		})
		return nil
	})
	if err == nil {
		t.Error("Update accepted a change leaving a gap between windows; validating the change " +
			"rather than the result is how a half-finished scope change passes")
	}

	// A change that errors leaves the record alone.
	sentinel := errors.New("planted")
	before, _ := campaign.Read(dir)
	if err := campaign.Update(dir, func(*campaign.Record) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Errorf("Update returned %v, want the change's own error", err)
	}
	after, _ := campaign.Read(dir)
	if len(after.Scopes) != len(before.Scopes) {
		t.Error("a failed change altered the record")
	}
}

// Reading a directory with no record says what is missing and why it matters, rather than "no such
// file": the caller is a campaign command and the reader is a human deciding what went wrong.
func TestReadingADirectoryWithNoRecordSaysWhatIsMissing(t *testing.T) {
	t.Parallel()
	_, err := campaign.Read(t.TempDir())
	if err == nil {
		t.Fatal("Read accepted a directory with no record")
	}
	for _, want := range []string{campaign.RecordFile, "FR-130", "FR-132"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
}

// An all-regions scope is empty rather than a list, and means all.
func TestAnEmptyRegionListMeansEveryRegion(t *testing.T) {
	t.Parallel()
	// FR-131: the first campaign's scope is the production project, ALL regions. A list of every
	// region name would go stale the day Google adds one, and would read as a deliberate subset.
	scope := campaign.Scope{From: campaignStart, Projects: []string{"twin-production"}}
	if !scope.AllRegions() {
		t.Error("an empty region list does not report as all regions, so the first campaign's " +
			"scope cannot be expressed (FR-131)")
	}
	scope.Regions = []string{"europe-west1"}
	if scope.AllRegions() {
		t.Error("a scope naming one region reports as all regions")
	}
}
