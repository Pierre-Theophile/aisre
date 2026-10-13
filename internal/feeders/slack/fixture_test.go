// SPDX-License-Identifier: Apache-2.0

package slack_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/feeders/slack"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/emit"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/record"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/source"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/testkit"
)

// The intake fixtures (006 T018). Each is a sequence of polls of a structural twin of a workspace's
// channel list, run through the real feeder with its payloads and events recorded. They are shaped
// like a recording: the creator is absent, because a recording drops people, and every name the rule's
// pattern does not match is blank, because the recording pre-pass blanks it.
//
// Regenerate with SRE_AGENT_GEN_FIXTURES=1; the goldens are then rewritten by `aisre fixture record`.
// On every run the conformance suite replays each fixture (FR-053, SC-004).

const genFixturesEnv = "SRE_AGENT_GEN_FIXTURES"

const twinTeam = "T0TWIN"

func at(h, m, s int) time.Time { return time.Date(2026, 10, 14, h, m, s, 0, time.UTC) }

type twinChannel struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Created  time.Time `json:"created"`
	Archived bool      `json:"archived,omitempty"`
	Shared   bool      `json:"shared,omitempty"`
}

type twinPoll struct {
	at       time.Time
	channels []twinChannel
}

type intakeFixture struct {
	dir, description string
	polls            []twinPoll
	queries          string
	end              time.Time
}

func (fx intakeFixture) payloads(t *testing.T) []feeder.Payload {
	t.Helper()
	out := make([]feeder.Payload, 0, len(fx.polls))
	for _, p := range fx.polls {
		raw, err := json.Marshal(struct {
			Team     string        `json:"team"`
			Channels []twinChannel `json:"channels"`
		}{Team: twinTeam, Channels: append([]twinChannel{}, p.channels...)})
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, feeder.Payload{Kind: slack.PayloadChannels, At: p.at, Bytes: raw})
	}
	return out
}

func fixtureOptions() slack.Options {
	return slack.Options{
		TeamID: twinTeam,
		Services: map[string]*graphv1.Ref{
			"checkout": feeder.Ref(feeder.NSOTelService, "checkout"),
			"payments": feeder.Ref(feeder.NSOTelService, "payments"),
		},
	}
}

var (
	sevCheckout = twinChannel{ID: "C0SEV2CHK", Name: "sev2-2026-10-14-checkout-latency", Created: at(9, 12, 3)}
	sevTest     = twinChannel{ID: "C0SEV2TST", Name: "sev2-2026-10-14-test-paging", Created: at(9, 20, 0)}
	unmatched   = twinChannel{ID: "C0OTHER01", Name: "", Created: at(9, 25, 10)}
	sevShared   = twinChannel{ID: "C0SEV1SHR", Name: "sev1-2026-10-14-vendor-bridge", Created: at(9, 26, 0), Shared: true}
)

func intake01() intakeFixture {
	return intakeFixture{
		dir: "fixtures/slack-intake-01",
		description: "Four channels appear in a workspace's public list over four polls. " +
			"`sev2-2026-10-14-checkout-latency`, created 09:12:03, is declared once — severity sev2, pinned to " +
			"its creation instant rather than the 09:12:30 poll, keyed on its channel id, watching the " +
			"checkout service through the operator's service map — and the three later polls that still list " +
			"it declare nothing. `sev2-…-test-paging` matches the pattern and is skipped as a test; a channel " +
			"whose name the pattern does not match (blanked, as a recording blanks it) is skipped; and a " +
			"severity channel shared with another organisation is skipped because it is not this " +
			"organisation's to declare. The creator is absent, as in a recording, so the declaring identity " +
			"says it was not recorded.",
		polls: []twinPoll{
			{at: at(9, 12, 30), channels: []twinChannel{sevCheckout}},
			{at: at(9, 20, 30), channels: []twinChannel{sevCheckout, sevTest}},
			{at: at(9, 25, 30), channels: []twinChannel{sevCheckout, sevTest, unmatched}},
			{at: at(9, 26, 30), channels: []twinChannel{sevCheckout, sevTest, unmatched, sevShared}},
		},
		end: at(9, 30, 0),
		queries: `
queries:
  # The declaration: an ALERT on the channel, declared at 09:12:03, watching checkout.
  - name: checkout-latency-declared
    kind: subgraph
    focus: incident=C0SEV2CHK
    valid_at: 2026-10-14T09:12:03Z
    observed_at: 2026-10-14T09:30:00Z
    hops: 1
    direction: both
  # A second before the channel existed, nothing was declared.
  - name: checkout-latency-before
    kind: subgraph
    focus: incident=C0SEV2CHK
    valid_at: 2026-10-14T09:12:02Z
    observed_at: 2026-10-14T09:30:00Z
    hops: 1
    direction: both
    expect_empty: the channel did not exist yet, so a declaration dated at the poll rather than at creation would show here
`,
	}
}

var (
	sevLate    = twinChannel{ID: "C0SEV3LTE", Name: "sev3-2026-10-14-payments", Created: at(8, 10, 0)}
	sevAncient = twinChannel{ID: "C0SEV2OLD", Name: "sev2-2026-10-10-search", Created: time.Date(2026, 10, 10, 15, 0, 0, 0, time.UTC)}
)

func intakeLate01() intakeFixture {
	return intakeFixture{
		dir: "fixtures/slack-intake-late-01",
		description: "The feeder starts at 09:00 and its first poll finds `sev3-2026-10-14-payments`, created at " +
			"08:10 while nothing was watching. It is declared with its true creation instant, 08:10, not the " +
			"09:00 discovery (FR-004a); the live run opens its investigation in review mode. " +
			"`sev2-2026-10-10-search`, created four days earlier, is outside the 24-hour backfill horizon " +
			"and is skipped as such rather than opening an investigation of a closed incident.",
		polls: []twinPoll{
			{at: at(9, 0, 0), channels: []twinChannel{sevAncient, sevLate}},
			{at: at(9, 1, 0), channels: []twinChannel{sevAncient, sevLate}},
		},
		end: at(9, 5, 0),
		queries: `
queries:
  - name: payments-declared-at-creation
    kind: subgraph
    focus: incident=C0SEV3LTE
    valid_at: 2026-10-14T08:10:00Z
    observed_at: 2026-10-14T09:05:00Z
    hops: 1
    direction: both
`,
	}
}

var (
	sevCheckoutRenamed = twinChannel{ID: "C0SEV2CHK", Name: "sev1-2026-10-14-checkout-outage", Created: at(9, 12, 3)}
	sevPaymentsTwin    = twinChannel{ID: "C0SEV2PAY", Name: "sev2-2026-10-14-payments-checkout", Created: at(9, 14, 0)}
)

func intakeRename01() intakeFixture {
	return intakeFixture{
		dir: "fixtures/slack-intake-rename-01",
		description: "`sev2-2026-10-14-checkout-latency` is declared at 09:12:03; at 09:31 it has been renamed " +
			"`sev1-2026-10-14-checkout-outage`. The rename opens nothing: the key is the channel id, never " +
			"the name (FR-008b), and the checkpoint states the rename. A second team opens " +
			"`sev2-2026-10-14-payments-checkout` for the same incident at 09:14: that is a second " +
			"declaration, watching both services the operator mapped, and grouping the two is the engine's " +
			"published association rule's business, never this connector's.",
		polls: []twinPoll{
			{at: at(9, 12, 30), channels: []twinChannel{sevCheckout}},
			{at: at(9, 14, 30), channels: []twinChannel{sevCheckout, sevPaymentsTwin}},
			{at: at(9, 31, 0), channels: []twinChannel{sevCheckoutRenamed, sevPaymentsTwin}},
		},
		end: at(9, 35, 0),
		queries: `
queries:
  - name: checkout-after-rename
    kind: subgraph
    focus: incident=C0SEV2CHK
    valid_at: 2026-10-14T09:35:00Z
    observed_at: 2026-10-14T09:35:00Z
    hops: 1
    direction: both
  - name: payments-second-channel
    kind: subgraph
    focus: incident=C0SEV2PAY
    valid_at: 2026-10-14T09:35:00Z
    observed_at: 2026-10-14T09:35:00Z
    hops: 1
    direction: both
`,
	}
}

func allFixtures() []intakeFixture {
	return []intakeFixture{intake01(), intakeLate01(), intakeRename01()}
}

func TestGenerateIntakeFixtures(t *testing.T) {
	if os.Getenv(genFixturesEnv) == "" {
		t.Skipf("set %s=1 to regenerate the intake fixtures", genFixturesEnv)
	}
	for _, fx := range allFixtures() {
		generate(t, fx)
	}
}

func TestIntakeFixturesConform(t *testing.T) {
	for _, fx := range allFixtures() {
		t.Run(filepath.Base(fx.dir), func(t *testing.T) {
			dir := filepath.Join(repoRoot(t), fx.dir)
			f, err := slack.New(fixtureOptions())
			if err != nil {
				t.Fatal(err)
			}
			testkit.Conformance(t, f, dir)
		})
	}
}

// TestIntake01 asserts what the fixture is for, on the events themselves.
func TestIntake01(t *testing.T) {
	var opened []slack.Declared
	opts := fixtureOptions()
	opts.OnDeclared = func(_ context.Context, d slack.Declared) { opened = append(opened, d) }
	f, err := slack.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	em := emit.NewMemoryEmitter(f.Describe())
	if err := f.Run(t.Context(), source.NewSliceSource(intake01().payloads(t)), em); err != nil {
		t.Fatal(err)
	}
	var transitions []*graphv1.AlertTransition
	for _, ev := range em.Events() {
		if tr := ev.GetAlertTransition(); tr != nil {
			transitions = append(transitions, tr)
		}
	}
	if len(transitions) != 1 || len(opened) != 1 {
		t.Fatalf("%d transition(s), %d investigation(s) opened; want one of each", len(transitions), len(opened))
	}
	tr, d := transitions[0], opened[0]
	if got := tr.GetTransitionAt().AsTime(); !got.Equal(at(9, 12, 3)) {
		t.Errorf("declared at %s, want the creation instant 09:12:03", got)
	}
	if tr.GetToState() != "declared" || tr.GetTransport() != feeder.TransportHumanDeclared ||
		tr.GetActorKind() != graphv1.ActorKind_PERSON || tr.GetGroupKey() != "" {
		t.Errorf("transition %+v", tr)
	}
	if tr.GetSeverity() != "sev2" || tr.GetTitle() != sevCheckout.Name || tr.GetOriginRef() != "slack:T0TWIN/C0SEV2CHK" {
		t.Errorf("severity %q title %q origin %q", tr.GetSeverity(), tr.GetTitle(), tr.GetOriginRef())
	}
	if tr.GetDeclaringIdentity() != "slack:T0TWIN/"+slack.UnrecordedCreator {
		t.Errorf("declarer %q", tr.GetDeclaringIdentity())
	}
	if len(tr.GetWatches()) != 1 || tr.GetWatches()[0].GetValue() != "checkout" {
		t.Errorf("watches %v; want checkout only — `latency` is not in the service map", tr.GetWatches())
	}
	if d.ReviewMode || d.RuleRef != "slack/sev-channel@1" || d.IdempotencyKey == "" {
		t.Errorf("declared %+v", d)
	}
	report := f.Report()
	want := map[string]int{"excluded marker test": 1, slack.SkipNoMatch: 1, slack.SkipShared: 1}
	for reason, n := range want {
		if report.Skipped[reason] != n {
			t.Errorf("skipped[%q] = %d, want %d (report: %s)", reason, report.Skipped[reason], n, report.Note())
		}
	}
}

func TestLateDiscoveryIsReviewModeAtTheTrueInstant(t *testing.T) {
	var opened []slack.Declared
	opts := fixtureOptions()
	opts.OnDeclared = func(_ context.Context, d slack.Declared) { opened = append(opened, d) }
	f, _ := slack.New(opts)
	em := emit.NewMemoryEmitter(f.Describe())
	if err := f.Run(t.Context(), source.NewSliceSource(intakeLate01().payloads(t)), em); err != nil {
		t.Fatal(err)
	}
	if len(opened) != 1 || opened[0].ChannelID != sevLate.ID {
		t.Fatalf("opened %+v", opened)
	}
	if !opened[0].ReviewMode || !opened[0].DeclaredAt.Equal(sevLate.Created) || !opened[0].FirstSeenAt.Equal(at(9, 0, 0)) {
		t.Errorf("late declaration %+v", opened[0])
	}
	if f.Report().Skipped[slack.SkipOutsideWindow] != 1 {
		t.Errorf("report %s", f.Report().Note())
	}
}

func TestRenameNeverReDeclares(t *testing.T) {
	var opened []string
	opts := fixtureOptions()
	opts.OnDeclared = func(_ context.Context, d slack.Declared) { opened = append(opened, d.ChannelID) }
	f, _ := slack.New(opts)
	em := emit.NewMemoryEmitter(f.Describe())
	if err := f.Run(t.Context(), source.NewSliceSource(intakeRename01().payloads(t)), em); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(opened, []string{"C0SEV2CHK", "C0SEV2PAY"}) {
		t.Errorf("opened %v", opened)
	}
	if r := f.Report(); r.Renamed != 1 || !strings.Contains(r.Note(), "1 rename") {
		t.Errorf("report %s", r.Note())
	}
}

func TestDeclarerMapping(t *testing.T) {
	opts := fixtureOptions()
	opts.People = map[string]string{"U0MAPPED": "oidc|morgan"}
	var opened []slack.Declared
	opts.OnDeclared = func(_ context.Context, d slack.Declared) { opened = append(opened, d) }
	f, _ := slack.New(opts)
	raw := func(creator string, id string) []byte {
		b, _ := json.Marshal(map[string]any{"team": twinTeam, "channels": []map[string]any{{
			"id": id, "name": "sev2-2026-10-14-checkout", "created": at(9, 0, 0), "creator": creator,
		}}})
		return b
	}
	payloads := []feeder.Payload{
		{Kind: slack.PayloadChannels, At: at(9, 0, 30), Bytes: raw("U0MAPPED", "C1")},
		{Kind: slack.PayloadChannels, At: at(9, 1, 30), Bytes: raw("U0OTHER", "C2")},
	}
	if err := f.Run(t.Context(), source.NewSliceSource(payloads), emit.NewMemoryEmitter(f.Describe())); err != nil {
		t.Fatal(err)
	}
	if len(opened) != 2 || opened[0].Declarer != "oidc|morgan" || opened[1].Declarer != "slack:T0TWIN/U0OTHER" {
		t.Errorf("declarers %+v", opened)
	}
}

func TestOriginRefRoundTrips(t *testing.T) {
	team, channel, ok := slack.ChannelOf(slack.OriginRef("T1", "C9"))
	if !ok || team != "T1" || channel != "C9" {
		t.Errorf("ChannelOf = %s %s %v", team, channel, ok)
	}
	for _, bad := range []string{"", "slack:", "slack:T1", "slack:T1/", "datadog:T1/C9", "slack:T1/C9/x"} {
		if _, _, ok := slack.ChannelOf(bad); ok {
			t.Errorf("ChannelOf(%q) accepted", bad)
		}
	}
}

func generate(t *testing.T, fx intakeFixture) {
	t.Helper()
	dir := filepath.Join(repoRoot(t), fx.dir)
	for _, generated := range []string{"payloads", "events.jsonl", "rejected.jsonl", "manifest.yaml", "golden"} {
		if err := os.RemoveAll(filepath.Join(dir, generated)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := slack.New(fixtureOptions())
	if err != nil {
		t.Fatal(err)
	}
	clock := &arrivalClock{}
	src := record.Wrap(clock.wrap(source.NewSliceSource(fx.payloads(t))), dir)
	memory := emit.NewMemoryEmitter(f.Describe(), emit.WithMemoryClock(clock.Now))
	events := record.Emitter(memory, dir)
	if err := f.Run(t.Context(), src, events); err != nil {
		t.Fatal(err)
	}
	if err := src.Err(); err != nil {
		t.Fatal(err)
	}
	if err := events.Err(); err != nil {
		t.Fatal(err)
	}
	if rejected := memory.Rejected(); len(rejected) > 0 {
		t.Fatalf("%d events refused; the first is %s (%s)", len(rejected), rejected[0].GetEventId(), rejected[0].GetReasonDetail())
	}
	if err := record.WriteManifest(dir, record.Manifest{
		Family:      "slack-intake",
		Description: fx.description + " Synthetic structural twin: no identifier is derived from the organisation.",
		Sources:     []record.ManifestSource{record.SourceOf(f.Describe())},
		Start:       fx.polls[0].at.Add(-time.Minute),
		End:         fx.end,
	}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "manifest.yaml")
	existing, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(existing, []byte(fx.queries)...), 0o644); err != nil {
		t.Fatal(err)
	}
}

// arrivalClock stamps each event a microsecond after the last, from the instant of the payload that
// carried it, so a recording's observed times are its polls' and nothing else.
type arrivalClock struct {
	base time.Time
	step int64
	last time.Time
}

func (c *arrivalClock) Now() time.Time {
	at := c.base.Add(time.Duration(c.step) * time.Microsecond)
	c.step++
	if !c.last.IsZero() && !at.After(c.last) {
		at = c.last.Add(time.Microsecond)
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
	p, err := s.src.Next(ctx)
	if err == nil {
		s.clock.base, s.clock.step = p.At, 0
	}
	return p, err
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test")
		}
		dir = parent
	}
}
