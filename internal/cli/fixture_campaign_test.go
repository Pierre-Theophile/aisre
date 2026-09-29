// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/campaign"
	"github.com/Pierre-Theophile/aisre/internal/sanitise"
)

// `fixture campaign` end to end (T153; FR-129–FR-132, FR-134, FR-138–FR-140).
//
// The sequence is what is tested rather than each command alone, because the gates are ordered and
// the ordering is the design: a recording is signed after it is scanned, and a scope change is two
// edits that have to happen together. A per-command test would pass on a command set that cannot be
// driven through in order.

// campaignFixture lays down a campaign directory holding one recording, by copying a real committed
// fixture. It is a real fixture rather than a hand-made file set because the commands walk whatever
// is there, and a two-file stand-in would not exercise the walk.
func campaignFixture(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "campaign-2026-09")
	src := filepath.Join("..", "..", "fixtures", "gcp-baseline-topology-01")
	// The goldens and the world are large and irrelevant here: the gates walk committed bytes, and
	// the payloads plus the events exercise that over a realistic shape without copying megabytes.
	copyTree(t, src, dir, func(rel string) bool {
		return !strings.HasPrefix(rel, "golden") && !strings.HasPrefix(rel, "world")
	})
	return dir
}

// The whole sequence, in order, on a one-person campaign.
func TestTheCampaignGatesRunInOrderAndProduceASignedRecording(t *testing.T) {
	dir := campaignFixture(t)

	// record — the campaign's own account of itself.
	_, stderr, code := run(t, t.Context(), "fixture", "campaign", "record", dir,
		"--org", "twin",
		"--started-at", "2026-09-01T09:00:00Z",
		"--projects", "twin-production",
		"--mailbox-path", "shared_mailbox",
		"--authorised-by", "the platform owner",
		"--authorised-on", "2026-09-01T09:00:00Z",
		"--signatory", "the platform owner",
		"--policy-version", "1.0.0")
	if code != ExitOK {
		t.Fatalf("record exited %d: %s", code, stderr)
	}
	r, err := campaign.Read(dir)
	if err != nil {
		t.Fatalf("the record does not read back: %v", err)
	}
	if len(r.Scopes) != 1 || r.Scopes[0].Projects[0] != "twin-production" {
		t.Errorf("the first scope window is %+v", r.Scopes)
	}
	if !r.Scopes[0].AllRegions() {
		t.Error("no --regions was passed, so the window must mean ALL regions (FR-131)")
	}
	if r.MailboxAddress != campaign.NoAddressRecorded {
		t.Errorf("mailbox_address = %q; it must carry the reason no address is stored", r.MailboxAddress)
	}

	// sanitise and scan — the same walk, reported differently.
	for _, mode := range []string{"sanitise", "scan"} {
		stdout, stderr, code := run(t, t.Context(), "fixture", "campaign", mode, dir)
		if code != ExitOK {
			t.Fatalf("%s exited %d: %s\n%s", mode, code, stderr, stdout)
		}
		if !strings.Contains(stdout, "files checked") {
			t.Errorf("%s did not report how many files it checked: %s", mode, stdout)
		}
		if strings.Contains(stdout, "files checked: 0") {
			t.Errorf("%s checked no file, so the gate held over nothing: %s", mode, stdout)
		}
		// Neither may claim to satisfy FR-138. Both contain the sanitiser.
		if !strings.Contains(stdout, "check-no-secrets.sh") {
			t.Errorf("%s does not name FR-138's independent scan, so a reader would take the "+
				"in-process check for it: %s", mode, stdout)
		}
	}

	// sign — the manifest, and the attestation that says what it is not.
	stdout, stderr, code := run(t, t.Context(), "fixture", "campaign", "sign", dir,
		"--signer", "the platform owner", "--on", "2026-09-20T11:00:00Z",
		"--note", "accepted the unfuzzed timestamps of contracts/sanitisation.md §4")
	if code != ExitOK {
		t.Fatalf("sign exited %d: %s\n%s", code, stderr, stdout)
	}
	m, err := sanitise.ReadManifest(dir)
	if err != nil {
		t.Fatalf("the manifest does not read back: %v", err)
	}
	if err := m.Check(len(r.Signatories)); err != nil {
		t.Errorf("the written manifest does not satisfy FR-140: %v", err)
	}
	if err := m.Verify(dir); err != nil {
		t.Errorf("the manifest does not verify against the recording it was written into: %v", err)
	}
	if m.PolicyVersion != "1.0.0" {
		t.Errorf("policy version = %q, want the campaign's (FR-134)", m.PolicyVersion)
	}
	if !strings.Contains(m.Attestation, "not a cryptographic signature") {
		t.Errorf("the manifest does not say a signature is not cryptographic: %q", m.Attestation)
	}

	// And the recording now fails its own hash if anything changes — the gate's whole point.
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("edit the recording: %v", err)
	}
	if err := m.Verify(dir); err == nil {
		t.Error("the manifest still verifies after the recording changed")
	}
}

// A scope change is two edits, and --add-scope is what keeps them together.
func TestAddScopeClosesTheOpenWindowAndOpensTheNext(t *testing.T) {
	dir := campaignFixture(t)
	_, stderr, code := run(t, t.Context(), "fixture", "campaign", "record", dir,
		"--org", "twin", "--started-at", "2026-09-01T09:00:00Z",
		"--projects", "twin-production",
		"--mailbox-path", "shared_mailbox",
		"--authorised-by", "the platform owner", "--authorised-on", "2026-09-01T09:00:00Z",
		"--signatory", "the platform owner")
	if code != ExitOK {
		t.Fatalf("record exited %d: %s", code, stderr)
	}

	_, stderr, code = run(t, t.Context(), "fixture", "campaign", "record", dir,
		"--add-scope", "2026-09-08T09:00:00Z",
		"--projects", "twin-production,twin-staging",
		"--scope-why", "staging added once the owner agreed the scope")
	if code != ExitOK {
		t.Fatalf("record --add-scope exited %d: %s", code, stderr)
	}

	r, err := campaign.Read(dir)
	if err != nil {
		t.Fatalf("the record does not read back after the scope change: %v", err)
	}
	if len(r.Scopes) != 2 {
		t.Fatalf("the record holds %d windows after a scope change, want 2", len(r.Scopes))
	}
	// Continuous, which is the property a two-step change breaks. Read validates it, so getting
	// here at all is most of the assertion; this names it so a failure says what broke.
	if !r.Scopes[0].To.Equal(r.Scopes[1].From) {
		t.Errorf("the windows are not continuous: %s then %s", r.Scopes[0].To, r.Scopes[1].From)
	}
	if !r.Scopes[1].To.IsZero() {
		t.Error("the new window is not open; the scope in force has no end until it is changed")
	}
	// The question the record exists for.
	if r.InScopeAt("twin-staging", r.Scopes[0].From.Add(1)) {
		t.Error("staging reads as in scope during the first window")
	}
	if !r.InScopeAt("twin-staging", r.Scopes[1].From) {
		t.Error("staging reads as out of scope from the instant it was added")
	}

	// --add-scope with no projects is refused: a window naming none says nothing rather than "all".
	_, stderr, code = run(t, t.Context(), "fixture", "campaign", "record", dir,
		"--add-scope", "2026-09-15T09:00:00Z")
	if code == ExitOK {
		t.Error("--add-scope with no --projects was accepted")
	}
	if !strings.Contains(stderr, "needs --projects") {
		t.Errorf("the refusal reads %s", stderr)
	}

	// And re-running `record` with nothing to change says so rather than silently replacing.
	_, stderr, code = run(t, t.Context(), "fixture", "campaign", "record", dir, "--org", "twin")
	if code == ExitOK {
		t.Error("record on an existing campaign with nothing to change exited zero")
	}
	if !strings.Contains(stderr, "changes nothing") {
		t.Errorf("the refusal reads %s", stderr)
	}
}

// A Datadog-only campaign (005 T082): the window carries the Datadog scope, needs no project and no
// mailbox, and a site with no environment is refused rather than read as "all".
func TestADatadogCampaignRecordsItsScopeWithoutProjects(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "campaign-dd")
	_, stderr, code := run(t, t.Context(), "fixture", "campaign", "record", dir,
		"--org", "twin", "--started-at", "2026-09-01T09:00:00Z",
		"--datadog-site", "datadoghq.eu")
	if code == ExitOK {
		t.Error("a Datadog scope naming no environment was accepted")
	}
	if !strings.Contains(stderr, "names no environment") {
		t.Errorf("the refusal reads %s", stderr)
	}

	_, stderr, code = run(t, t.Context(), "fixture", "campaign", "record", dir,
		"--org", "twin", "--started-at", "2026-09-01T09:00:00Z",
		"--datadog-site", "datadoghq.eu", "--datadog-env", "production",
		"--datadog-watch", "production/checkout",
		"--signatory", "the platform owner")
	if code != ExitOK {
		t.Fatalf("record exited %d: %s", code, stderr)
	}
	r, err := campaign.Read(dir)
	if err != nil {
		t.Fatalf("the record does not read back: %v", err)
	}
	dd := r.Scopes[0].Datadog
	if dd == nil || dd.Site != "datadoghq.eu" || len(dd.Environments) != 1 || len(dd.LogSources) != 1 {
		t.Fatalf("the window's Datadog scope reads back as %+v", dd)
	}
	if len(r.Scopes[0].Projects) != 0 {
		t.Errorf("a Datadog-only window names projects: %v", r.Scopes[0].Projects)
	}

	_, stderr, code = run(t, t.Context(), "fixture", "campaign", "record", dir,
		"--add-scope", "2026-09-02T09:00:00Z",
		"--datadog-site", "datadoghq.eu", "--datadog-env", "production,staging",
		"--scope-why", "staging added")
	if code != ExitOK {
		t.Fatalf("record --add-scope with a Datadog scope exited %d: %s", code, stderr)
	}
	r, err = campaign.Read(dir)
	if err != nil {
		t.Fatalf("the record does not read back after the scope change: %v", err)
	}
	if len(r.Scopes) != 2 || len(r.Scopes[1].Datadog.Environments) != 2 {
		t.Errorf("the new window does not carry the new Datadog scope: %+v", r.Scopes)
	}
}

// The second-signature rule, driven through the command, and the two ways it gets defeated.
func TestSigningRefusesAnUndeclaredSignerAndATeamOfOne(t *testing.T) {
	dir := campaignFixture(t)
	_, stderr, code := run(t, t.Context(), "fixture", "campaign", "record", dir,
		"--org", "twin", "--started-at", "2026-09-01T09:00:00Z",
		"--projects", "twin-production",
		"--mailbox-path", "shared_mailbox",
		"--authorised-by", "the platform owner", "--authorised-on", "2026-09-01T09:00:00Z",
		"--signatory", "the platform owner", "--signatory", "the on-call lead")
	if code != ExitOK {
		t.Fatalf("record exited %d: %s", code, stderr)
	}

	// A signer the campaign never declared would make the signatory count mean nothing.
	_, stderr, code = run(t, t.Context(), "fixture", "campaign", "sign", dir,
		"--signer", "somebody else", "--on", "2026-09-20T11:00:00Z")
	if code == ExitOK {
		t.Error("an undeclared signer was accepted; the second-signature rule counts the declared " +
			"signatories, so signing as anybody defeats it")
	}
	if !strings.Contains(stderr, "not one of the campaign's signatories") {
		t.Errorf("the refusal reads %s", stderr)
	}

	// The first declared signer signs; with two declared, the command says a second is required.
	stdout, stderr, code := run(t, t.Context(), "fixture", "campaign", "sign", dir,
		"--signer", "the platform owner", "--on", "2026-09-20T11:00:00Z")
	if code != ExitOK {
		t.Fatalf("sign exited %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "second signature is required") {
		t.Errorf("a two-signatory campaign with one signature did not say a second is required: %s", stdout)
	}
	// And the manifest on disk does NOT yet satisfy FR-140, which is the honest state: the file
	// exists, the gate is not passed.
	m, err := sanitise.ReadManifest(dir)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if err := m.Check(2); err == nil {
		t.Error("a single signature satisfied a two-signatory campaign")
	}

	// The second signs, and now it does.
	if _, stderr, code = run(t, t.Context(), "fixture", "campaign", "sign", dir,
		"--signer", "the on-call lead", "--on", "2026-09-21T11:00:00Z"); code != ExitOK {
		t.Fatalf("countersign exited %d: %s", code, stderr)
	}
	m, err = sanitise.ReadManifest(dir)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if err := m.Check(2); err != nil {
		t.Errorf("two signatures still fail the two-signatory rule: %v", err)
	}

	// Signing a third time as the first person is refused: two signatures from one person are one.
	_, stderr, code = run(t, t.Context(), "fixture", "campaign", "sign", dir,
		"--signer", "the platform owner", "--on", "2026-09-22T11:00:00Z")
	if code == ExitOK {
		t.Error("the same person signed twice")
	}
	if !strings.Contains(stderr, "signed twice") {
		t.Errorf("the refusal reads %s", stderr)
	}
}

// A campaign directory with a record and no recording describes a campaign that produced nothing.
func TestTheGatesRefuseACampaignWithNothingRecorded(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "campaign-empty")
	if err := campaign.Write(dir, campaign.Record{
		ID: "campaign-empty", Organisation: "twin",
		StartedAt: mustInstant(t, "2026-09-01T09:00:00Z"),
		Scopes: []campaign.Scope{{
			From: mustInstant(t, "2026-09-01T09:00:00Z"), Projects: []string{"twin-production"},
		}},
		Mailbox: campaign.Mailbox{
			Path: campaign.AccessSharedMailbox, AuthorisedBy: "the platform owner",
			AuthorisedOn: mustInstant(t, "2026-09-01T09:00:00Z"),
		},
		MailboxAddress: campaign.NoAddressRecorded,
		Signatories:    []string{"the platform owner"},
		PolicyVersion:  "1.0.0",
	}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	for _, mode := range []string{"sanitise", "scan", "sign"} {
		args := []string{"fixture", "campaign", mode, dir}
		if mode == "sign" {
			args = append(args, "--signer", "the platform owner")
		}
		_, stderr, code := run(t, t.Context(), args...)
		if code == ExitOK {
			t.Errorf("%s exited zero on a campaign with nothing recorded", mode)
		}
		if !strings.Contains(stderr, "holds no recording") {
			t.Errorf("%s's refusal reads %s", mode, stderr)
		}
	}
}

// And a recording with no campaign record cannot be gated at all: nothing says what scope it was
// taken under or who authorised the mailbox read.
func TestTheGatesRefuseARecordingWithNoCampaignRecord(t *testing.T) {
	dir := campaignFixture(t)
	_, stderr, code := run(t, t.Context(), "fixture", "campaign", "scan", dir)
	if code == ExitOK {
		t.Fatal("scan exited zero on a recording with no campaign record")
	}
	for _, want := range []string{campaign.RecordFile, "FR-130", "FR-132"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the refusal does not mention %q: %s", want, stderr)
		}
	}
}

func mustInstant(t *testing.T, value string) time.Time {
	t.Helper()
	p, err := parseInstant("--test", value)
	if err != nil {
		t.Fatalf("parse %s: %v", value, err)
	}
	return p
}

// ---- verify: the commit gate that had no caller ----------------------------------------------

// recordCampaign writes the record every verify test starts from.
func recordCampaign(t *testing.T, dir string, signatories ...string) {
	t.Helper()
	args := []string{"fixture", "campaign", "record", dir,
		"--org", "twin", "--started-at", "2026-09-01T09:00:00Z",
		"--projects", "twin-production",
		"--mailbox-path", "shared_mailbox",
		"--authorised-by", "the platform owner", "--authorised-on", "2026-09-01T09:00:00Z",
		"--policy-version", "1.0.0"}
	for _, s := range signatories {
		args = append(args, "--signatory", s)
	}
	if _, stderr, code := run(t, t.Context(), args...); code != ExitOK {
		t.Fatalf("record exited %d: %s", code, stderr)
	}
}

// The three ways a recording can fail the commit gate, and they are distinct because the remedy is.
//
// `sanitise.Manifest.Check` is documented as the commit gate and, until this command existed, had no
// caller outside its own unit tests — so every one of these cases committed clean.
func TestVerifyRefusesARecordingNoSignedManifestCovers(t *testing.T) {
	t.Run("no manifest at all", func(t *testing.T) {
		dir := campaignFixture(t)
		recordCampaign(t, dir, "the platform owner")
		stdout, _, code := run(t, t.Context(), "fixture", "campaign", "verify", dir)
		if code != ExitRejected {
			t.Fatalf("verify exited %d on an unsigned recording, want ExitRejected: %s", code, stdout)
		}
		if !strings.Contains(stdout, "FR-140") {
			t.Errorf("the refusal does not name the requirement it enforces: %s", stdout)
		}
	})

	t.Run("the recording changed after it was signed", func(t *testing.T) {
		dir := campaignFixture(t)
		recordCampaign(t, dir, "the platform owner")
		if _, stderr, code := run(t, t.Context(), "fixture", "campaign", "sign", dir,
			"--signer", "the platform owner", "--on", "2026-09-20T11:00:00Z"); code != ExitOK {
			t.Fatalf("sign exited %d: %s", code, stderr)
		}
		// Signed, and passing.
		if _, stderr, code := run(t, t.Context(), "fixture", "campaign", "verify", dir); code != ExitOK {
			t.Fatalf("verify exited %d on a freshly signed recording: %s", code, stderr)
		}
		// Now move the bytes the signature attests to. This is worse than unsigned: it reads as
		// reviewed.
		if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte("{}\n"), 0o600); err != nil {
			t.Fatalf("edit the recording: %v", err)
		}
		stdout, _, code := run(t, t.Context(), "fixture", "campaign", "verify", dir)
		if code != ExitRejected {
			t.Fatalf("verify exited %d after the recording changed, want ExitRejected: %s", code, stdout)
		}
		// "the hash differs" is not something a person can act on; the changed file has to be named.
		if !strings.Contains(stdout, "events.jsonl") {
			t.Errorf("the refusal does not name what changed since the signature: %s", stdout)
		}
	})

	t.Run("one signature where the campaign declares two", func(t *testing.T) {
		dir := campaignFixture(t)
		recordCampaign(t, dir, "the platform owner", "the security reviewer")
		if _, stderr, code := run(t, t.Context(), "fixture", "campaign", "sign", dir,
			"--signer", "the platform owner", "--on", "2026-09-20T11:00:00Z"); code != ExitOK {
			t.Fatalf("sign exited %d: %s", code, stderr)
		}
		stdout, _, code := run(t, t.Context(), "fixture", "campaign", "verify", dir)
		if code != ExitRejected {
			t.Fatalf("verify exited %d with one of two signatures, want ExitRejected: %s", code, stdout)
		}

		// And it passes once the second reader has read it. The count comes from the campaign
		// record, so it cannot be argued down at the command line.
		if _, stderr, code := run(t, t.Context(), "fixture", "campaign", "sign", dir,
			"--signer", "the security reviewer", "--on", "2026-09-20T12:00:00Z"); code != ExitOK {
			t.Fatalf("countersign exited %d: %s", code, stderr)
		}
		stdout, stderr, code := run(t, t.Context(), "fixture", "campaign", "verify", dir)
		if code != ExitOK {
			t.Fatalf("verify exited %d on a countersigned recording: %s\n%s", code, stderr, stdout)
		}
		for _, who := range []string{"the platform owner", "the security reviewer"} {
			if !strings.Contains(stdout, who) {
				t.Errorf("the report does not name signatory %q: %s", who, stdout)
			}
		}
	})
}

// A campaign with nothing recorded fails rather than reporting a pass over zero recordings.
//
// This is the same hole `scan` and `sanitise` guard: a gate that held over nothing is the most
// dangerous kind of green, because it looks identical to a gate that held.
func TestVerifyRefusesACampaignWithNothingRecorded(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "campaign-2026-09")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	recordCampaign(t, dir, "the platform owner")
	stdout, _, code := run(t, t.Context(), "fixture", "campaign", "verify", dir)
	if code == ExitOK {
		t.Fatalf("verify passed a campaign with no recording: %s", stdout)
	}
}
