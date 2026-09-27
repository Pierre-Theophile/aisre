// SPDX-License-Identifier: Apache-2.0

// Package campaign is the record a recording campaign keeps about itself (T154; FR-130–FR-132).
//
// A campaign records an organisation's real payloads so the corpus stops being synthetic. The
// recording itself is the point, and this package is about the thing *beside* it: what scope was in
// force, how the mailbox was reached, and who said that was allowed.
//
// # Why the record exists at all, rather than the scope living in configuration
//
// FR-130 says recording begins **before** the campaign scope is finally agreed, because neither
// topology history nor an announcement stream can be reconstructed retrospectively — a day not
// recorded is a day that does not exist. That has a consequence people miss: the scope *changes
// during* the campaign. So "what was in scope" is not one fact about the campaign, it is a series of
// facts with instants, and a reader a year later has to be able to tell **"not present"** from
// **"not in scope at that time"**. Those are opposite conclusions about the same silence.
//
// Configuration cannot answer that, because configuration is what is true now. The checkpoint records
// the scope in force on every poll (FR-009) and this record is the campaign-level ledger of the same
// thing: the windows, in order, with what each one covered.
//
// # Why the mailbox access path is here and not in configuration either
//
// FR-132 requires the access path **and who authorised it**. The second half is why it is a record
// rather than a setting: an authorisation is an event, by a named person, on a date. A config file
// saying `mailbox: ops@…` states a destination; it does not state that anybody agreed the connector
// may read it. Whoever reviews this corpus in two years needs the second fact more than the first.
package campaign

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

// RecordFile is the campaign record's name inside a campaign directory.
const RecordFile = "campaign.yaml"

// AccessPath is how the notice mailbox is read. The three are not interchangeable and the choice is
// not ours: research §9 established that a Google Group's archive is readable by no API, so a
// notice stream that arrives at a Group has to be read from a subscribed member's mailbox instead
// (FR-132).
type AccessPath string

const (
	// AccessSharedMailbox is a Workspace mailbox the connector reads directly. The owner's intended
	// source, and the one that is only available if the address is a mailbox rather than a Group.
	AccessSharedMailbox AccessPath = "shared_mailbox"
	// AccessSubscribedMember is a named individual's mailbox, read because they are subscribed to
	// the Group the notices arrive at. It is a broader read than a shared mailbox — that person's
	// whole mailbox is reachable by the credential — and recording it as a distinct path is what
	// makes the difference reviewable rather than buried.
	AccessSubscribedMember AccessPath = "subscribed_member"
	// AccessEssentialContacts is per-user delivery, which FR-132a makes a first-class path rather
	// than a fallback: Google addresses platform notices to each project's Essential Contacts and to
	// billing and owner principals, so the stream generally arrives in individuals' mailboxes and
	// reaches a shared address only where somebody configured it to.
	AccessEssentialContacts AccessPath = "essential_contacts"
)

// Valid reports whether the path is one of the three published ones.
func (a AccessPath) Valid() bool {
	switch a {
	case AccessSharedMailbox, AccessSubscribedMember, AccessEssentialContacts:
		return true
	default:
		return false
	}
}

// AccessPaths returns the published paths, for a CLI's help and for a validation message that names
// what was expected rather than only what was wrong.
func AccessPaths() []AccessPath {
	return []AccessPath{AccessSharedMailbox, AccessSubscribedMember, AccessEssentialContacts}
}

// Scope is the project and region scope in force over one window (FR-009, FR-130, FR-131).
//
// `From` is required and `To` is open while the window is the current one. A closed window means the
// scope changed, and both windows stay in the record: the old one is not corrected, because it was
// true when it was true. That is the same rule the graph follows for a fact whose validity ended.
type Scope struct {
	From     time.Time `yaml:"from"`
	To       time.Time `yaml:"to,omitempty"`
	Projects []string  `yaml:"projects"`
	// Regions empty means ALL regions, which is the first campaign's scope (FR-131, owner's
	// decision 2026-09-21). It is empty rather than a list of every region name because a list
	// would go stale the day Google adds one, and "all" is the actual intent.
	Regions []string `yaml:"regions,omitempty"`
	// Datadog is the Datadog scope in force over the window, for a campaign recording that
	// connector (005 T025, FR-007, FR-070d). A window names projects, a Datadog scope, or both.
	Datadog *DatadogScope `yaml:"datadog,omitempty"`
	// Why says what changed and why, for the reader who finds two windows and wants to know which
	// one to trust for a given instant. Both are true; this says what happened between them.
	Why string `yaml:"why,omitempty"`
}

// DatadogScope is what a Datadog campaign window covered (005 FR-007): the site, the environments,
// the watched log sources and the indexes. Monitors are scoped by tag in the connector's own
// configuration and recorded in its checkpoints, so they are not repeated here.
type DatadogScope struct {
	// Site is the Datadog site, e.g. `datadoghq.eu`. Configuration, never a pointer field.
	Site string `yaml:"site"`
	// Environments are the `env` values in scope.
	Environments []string `yaml:"environments"`
	// LogSources are the watched `<env>/<service>` pairs.
	LogSources []string `yaml:"log_sources,omitempty"`
	// Indexes are the log indexes searched; empty means the default.
	Indexes []string `yaml:"indexes,omitempty"`
}

func (d DatadogScope) validate(window int) error {
	switch {
	case strings.TrimSpace(d.Site) == "":
		return fmt.Errorf("campaign: scope window %d's Datadog scope names no site", window)
	case len(d.Environments) == 0:
		return fmt.Errorf("campaign: scope window %d's Datadog scope names no environment; a service "+
			"name is unique only within one, and an empty list would read as \"all\"", window)
	}
	return nil
}

// AllRegions reports whether this window covered every region.
func (s Scope) AllRegions() bool { return len(s.Regions) == 0 }

// Covers reports whether this window was in force at an instant. The window is half-open —
// `[From, To)` — which is the same convention valid time uses, so a reader does not have to hold two
// conventions in their head.
func (s Scope) Covers(at time.Time) bool {
	if at.Before(s.From) {
		return false
	}
	return s.To.IsZero() || at.Before(s.To)
}

// Mailbox is the notice mailbox and the authorisation to read it (FR-132).
type Mailbox struct {
	// Path is how it is read. Required.
	Path AccessPath `yaml:"path"`
	// AuthorisedBy names the individual who authorised the read, and AuthorisedOn the date. Both
	// required: an authorisation with no name is not one, and without a date nobody can tell whether
	// it predates the recording it is supposed to cover.
	AuthorisedBy string    `yaml:"authorised_by"`
	AuthorisedOn time.Time `yaml:"authorised_on"`
	// Note carries what a reviewer needs that the path alone does not say — typically the residual
	// risk of the path chosen. It is required for AccessSubscribedMember, because that path reaches
	// a person's whole mailbox and a record that did not say so would understate what was allowed.
	Note string `yaml:"note,omitempty"`
	// SenderAllowlist is the configured senders whose notices are read. It is recorded because
	// FR-132b forbids hard-coding any sender, which means the allowlist is a fact about this
	// campaign rather than about the code.
	SenderAllowlist []string `yaml:"sender_allowlist,omitempty"`
}

// NoAddressRecorded is what the record says where a mailbox address would otherwise go.
//
// The address is deliberately absent. FR-132b forbids hard-coding one and the sanitisation contract
// drops people identifiers, and a shared mailbox address is frequently a person's — `dana@` is a
// person however the mail is routed. The path, the authoriser and the allowlist are what a reviewer
// needs; the address is what a leak needs.
const NoAddressRecorded = "not recorded: an address is a people identifier under FR-135, and the " +
	"access path plus the authoriser is what a reviewer needs (FR-132b)"

// Record is a campaign's own account of itself, written to `campaign.yaml`.
type Record struct {
	// ID names the campaign, conventionally `campaign-<date>`.
	ID string `yaml:"id"`
	// Organisation is the org slug the source ids carry, e.g. `nova`. It is the pseudonymised slug
	// rather than a legal name.
	Organisation string `yaml:"organisation"`
	// StartedAt is when recording began — which FR-130 requires to be before the scope was agreed,
	// so it is normally *earlier* than the first scope window's `From` is comfortable to look at.
	StartedAt time.Time `yaml:"started_at"`
	// EndedAt is when it stopped, open while it runs.
	EndedAt time.Time `yaml:"ended_at,omitempty"`
	// Scopes are the scope windows in force, oldest first. At least one is required.
	Scopes []Scope `yaml:"scopes"`
	// Mailbox is the notice half's access path and authorisation.
	Mailbox Mailbox `yaml:"mailbox"`
	// MailboxAddress is always NoAddressRecorded. It is a field rather than a comment so that a
	// reader who goes looking for the address finds the reason it is absent, in the record, instead
	// of concluding somebody forgot.
	MailboxAddress string `yaml:"mailbox_address"`
	// Signatories are the people who may sign a recording's manifest under FR-140. It lives here
	// rather than per recording because "does a second person exist" is a fact about the
	// organisation, and a manifest cannot answer it about itself — see sanitise.Manifest.
	Signatories []string `yaml:"signatories"`
	// PolicyVersion is the sanitisation contract version the campaign ran under (FR-134).
	PolicyVersion string `yaml:"policy_version"`
	// Note is free text for the reviewer.
	Note string `yaml:"note,omitempty"`
}

// ErrNoScope is returned for a record with no scope window at all.
var ErrNoScope = errors.New("campaign: a record with no scope window cannot tell \"not present\" " +
	"from \"not in scope at that time\", which is the one question it exists to answer (FR-130)")

// Validate refuses a record that cannot answer the questions it exists for.
//
// It is strict on purpose, and the reason is the asymmetry: a campaign is recorded once, over weeks,
// and reviewed years later by somebody who cannot go back and ask. A missing field is not a gap that
// gets filled in later — it is a question that becomes unanswerable.
func (r Record) Validate() error {
	if strings.TrimSpace(r.ID) == "" {
		return errors.New("campaign: a record needs an id")
	}
	if strings.TrimSpace(r.Organisation) == "" {
		return errors.New("campaign: a record needs an organisation slug; it is what the source ids " +
			"carry and what joins the recording to the graph it produced")
	}
	if r.StartedAt.IsZero() {
		return errors.New("campaign: a record needs a start instant; FR-130's whole point is that " +
			"recording began before the scope was agreed, and without the instant nobody can tell " +
			"whether it did")
	}
	if !r.EndedAt.IsZero() && r.EndedAt.Before(r.StartedAt) {
		return fmt.Errorf("campaign: the campaign ended (%s) before it started (%s)",
			r.EndedAt.Format(time.RFC3339), r.StartedAt.Format(time.RFC3339))
	}
	if len(r.Scopes) == 0 {
		return ErrNoScope
	}
	if err := r.validateScopes(); err != nil {
		return err
	}
	// The mailbox is the GCP vendor-notice half's access path. A campaign recording only a connector
	// that reads no mailbox — a Datadog-only campaign (005 T025) — has none to authorise.
	if r.recordsProjects() {
		if err := r.Mailbox.validate(); err != nil {
			return err
		}
	}
	if len(r.Signatories) == 0 {
		return errors.New("campaign: a record needs at least one signatory; FR-140 requires a " +
			"recording to be signed off by a NAMED individual, and a campaign that names nobody " +
			"cannot produce one")
	}
	for _, who := range r.Signatories {
		if strings.TrimSpace(who) == "" {
			return errors.New("campaign: a blank signatory; FR-140 wants a name, and a blank one " +
				"is how an unsigned recording passes for a signed one")
		}
	}
	if strings.TrimSpace(r.PolicyVersion) == "" {
		return errors.New("campaign: a record needs the sanitisation policy version it ran under " +
			"(FR-134)")
	}
	return nil
}

// validateScopes checks the windows are ordered, non-overlapping and continuous.
//
// Continuity is the property that matters and it is the one an eye skips. A gap between two windows
// is a stretch of campaign during which the record says nothing was in scope — so a reader asking
// about an instant in the gap gets "not in scope", which is a claim, and a false one. Overlap is the
// mirror image: two answers to a question that has one.
func (r Record) validateScopes() error {
	ordered := slices.Clone(r.Scopes)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].From.Before(ordered[j].From) })
	for i, scope := range r.Scopes {
		if !scope.From.Equal(ordered[i].From) {
			return errors.New("campaign: the scope windows are not in order, oldest first; the " +
				"order is what a reader follows to find the window covering an instant")
		}
	}
	for i, scope := range r.Scopes {
		if scope.From.IsZero() {
			return fmt.Errorf("campaign: scope window %d has no start instant", i+1)
		}
		if len(scope.Projects) == 0 && scope.Datadog == nil {
			return fmt.Errorf("campaign: scope window %d names no project and no Datadog scope; no "+
				"code or checked-in default may assume a project name (FR-131), so an empty list is a "+
				"record that says nothing rather than one that means \"all\"", i+1)
		}
		if scope.Datadog != nil {
			if err := scope.Datadog.validate(i + 1); err != nil {
				return err
			}
		}
		if !scope.To.IsZero() && !scope.To.After(scope.From) {
			return fmt.Errorf("campaign: scope window %d ends (%s) at or before it starts (%s)",
				i+1, scope.To.Format(time.RFC3339), scope.From.Format(time.RFC3339))
		}
		if i == 0 {
			continue
		}
		previous := r.Scopes[i-1]
		if previous.To.IsZero() {
			return fmt.Errorf("campaign: scope window %d is open and window %d follows it; only the "+
				"last window may be open, or two windows claim the same instant", i, i+1)
		}
		if !previous.To.Equal(scope.From) {
			return fmt.Errorf("campaign: scope windows %d and %d are not continuous (%s then %s); a "+
				"gap makes the record answer \"not in scope\" for an instant it was simply not "+
				"asked about, and an overlap gives two answers to one question",
				i, i+1, previous.To.Format(time.RFC3339), scope.From.Format(time.RFC3339))
		}
	}
	return nil
}

func (m Mailbox) validate() error {
	if !m.Path.Valid() {
		paths := make([]string, 0, len(AccessPaths()))
		for _, p := range AccessPaths() {
			paths = append(paths, string(p))
		}
		return fmt.Errorf("campaign: mailbox access path %q is not one of %s (FR-132)",
			m.Path, strings.Join(paths, ", "))
	}
	if strings.TrimSpace(m.AuthorisedBy) == "" {
		return errors.New("campaign: the mailbox read needs a named authoriser; FR-132 makes it an " +
			"owner action, and \"somebody said it was fine\" is not a record of one")
	}
	if m.AuthorisedOn.IsZero() {
		return errors.New("campaign: the mailbox authorisation needs a date; without one nobody " +
			"can tell whether it predates the recording it is supposed to cover")
	}
	if m.Path == AccessSubscribedMember && strings.TrimSpace(m.Note) == "" {
		return errors.New("campaign: the subscribed-member path needs a note; it reaches that " +
			"person's WHOLE mailbox rather than a shared address, and a record that did not say so " +
			"would understate what was authorised (FR-132)")
	}
	return nil
}

// recordsProjects reports whether any window names a GCP project, which is what brings the
// vendor-notice mailbox into the campaign.
func (r Record) recordsProjects() bool {
	for _, scope := range r.Scopes {
		if len(scope.Projects) > 0 {
			return true
		}
	}
	return false
}

// ScopeAt returns the window in force at an instant, and whether one was.
//
// The false is the useful half. A caller asking about an instant the campaign did not cover gets
// "no window" rather than the nearest one, because the nearest one is a different claim: it would
// turn "we were not recording then" into "this is what we were recording".
func (r Record) ScopeAt(at time.Time) (Scope, bool) {
	for _, scope := range r.Scopes {
		if scope.Covers(at) {
			return scope, true
		}
	}
	return Scope{}, false
}

// InScopeAt reports whether a project was in scope at an instant. It answers FR-130's question
// directly, so a caller does not have to reimplement the window walk and get the open end wrong.
func (r Record) InScopeAt(project string, at time.Time) bool {
	scope, ok := r.ScopeAt(at)
	if !ok {
		return false
	}
	return slices.Contains(scope.Projects, project)
}
