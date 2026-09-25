// SPDX-License-Identifier: Apache-2.0

package sanitise_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/sanitise"
)

// The signed manifest (T155; FR-140, contracts/sanitisation.md §6 gate 3).
//
// The gate's job is accountability: somebody named looked at what was about to be committed. So the
// tests are about the ways that can be true on paper and false in fact — an unsigned manifest, one
// person signing twice to satisfy a two-signature rule, a second signature on content that changed
// since the first, and a hash that covers a description instead of the thing described.

var signedOn = time.Date(2026, 9, 20, 11, 0, 0, 0, time.UTC)

// writeRecording lays down a small recording and returns its directory.
func writeRecording(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", name, err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

func aRecording(t *testing.T) string {
	t.Helper()
	return writeRecording(t, map[string]string{
		"manifest.yaml":                 "id: campaign-recording\n",
		"events.jsonl":                  "{\"eventId\":\"gcp:twin:1\"}\n",
		"payloads/services/000001.json": "{\"services\":[]}\n",
	})
}

func onePerson() []sanitise.Signature {
	return []sanitise.Signature{{
		By: "the platform owner", On: signedOn,
		Note: "accepted §4's unfuzzed timestamps",
	}}
}

// The content hash is over the recording, not over the manifest. This is the property that makes the
// signature mean anything, and the way to test it is to change a payload and nothing else.
func TestTheContentHashCoversTheRecordingAndNotTheManifest(t *testing.T) {
	t.Parallel()
	dir := aRecording(t)

	m, err := sanitise.NewManifest(dir, "1.0.0", onePerson(), nil, nil, 3)
	if err != nil {
		t.Fatalf("NewManifest: %v", err)
	}
	if err := m.Verify(dir); err != nil {
		t.Fatalf("a freshly built manifest does not verify: %v", err)
	}

	// Edit a payload. Nothing about the manifest changes, which is exactly why a manifest-only hash
	// would still match.
	payload := filepath.Join(dir, "payloads", "services", "000001.json")
	if err := os.WriteFile(payload, []byte("{\"services\":[{\"name\":\"leaked\"}]}\n"), 0o644); err != nil {
		t.Fatalf("edit the payload: %v", err)
	}
	err = m.Verify(dir)
	if err == nil {
		t.Fatal("the manifest still verifies after a payload was edited; the hash covers a " +
			"description rather than the content, so a signature attests to nothing")
	}
	// And it says the file was EDITED rather than just that something is wrong, because "the hash
	// does not match" sends a reviewer through the whole directory.
	if !strings.Contains(err.Error(), "edited") {
		t.Errorf("the mismatch reads %q; it should say a file was edited, since the same files are "+
			"present", err)
	}
}

// A file added or removed after signing is a different failure from an edit, and a reviewer needs to
// be told which.
func TestVerifyNamesWhatChangedRatherThanOnlyThatSomethingDid(t *testing.T) {
	t.Parallel()

	t.Run("added", func(t *testing.T) {
		t.Parallel()
		dir := aRecording(t)
		m, err := sanitise.NewManifest(dir, "1.0.0", onePerson(), nil, nil, 0)
		if err != nil {
			t.Fatalf("NewManifest: %v", err)
		}
		extra := filepath.Join(dir, "payloads", "services", "000002.json")
		if err := os.WriteFile(extra, []byte("{}\n"), 0o644); err != nil {
			t.Fatalf("add a payload: %v", err)
		}
		err = m.Verify(dir)
		if err == nil {
			t.Fatal("a payload added after signing still verifies")
		}
		if !strings.Contains(err.Error(), "added since signing") ||
			!strings.Contains(err.Error(), "000002.json") {
			t.Errorf("the mismatch reads %q; it should name the added file", err)
		}
	})

	t.Run("removed", func(t *testing.T) {
		t.Parallel()
		dir := aRecording(t)
		m, err := sanitise.NewManifest(dir, "1.0.0", onePerson(), nil, nil, 0)
		if err != nil {
			t.Fatalf("NewManifest: %v", err)
		}
		if err := os.Remove(filepath.Join(dir, "events.jsonl")); err != nil {
			t.Fatalf("remove a file: %v", err)
		}
		err = m.Verify(dir)
		if err == nil {
			t.Fatal("a file removed after signing still verifies")
		}
		if !strings.Contains(err.Error(), "removed since signing") ||
			!strings.Contains(err.Error(), "events.jsonl") {
			t.Errorf("the mismatch reads %q; it should name the removed file", err)
		}
	})
}

// Two recordings that differ only in how their bytes are divided between files must not hash alike.
// Without the length prefixes they would, and the failure is silent: two different corpora carrying
// one signature.
func TestTwoRecordingsThatDifferOnlyInFileBoundariesDoNotHashAlike(t *testing.T) {
	t.Parallel()
	a := writeRecording(t, map[string]string{"ab": "c"})
	b := writeRecording(t, map[string]string{"a": "bc"})

	hashA, _, err := sanitise.HashRecording(a)
	if err != nil {
		t.Fatalf("HashRecording(a): %v", err)
	}
	hashB, _, err := sanitise.HashRecording(b)
	if err != nil {
		t.Fatalf("HashRecording(b): %v", err)
	}
	if hashA == hashB {
		t.Error("`ab` holding `c` hashes the same as `a` holding `bc`; without length prefixes the " +
			"digest is over a concatenation, so two different recordings share one signature")
	}
}

// The hash is stable across walks, or every verification is a false alarm.
func TestTheHashIsStableAndNamesItsAlgorithm(t *testing.T) {
	t.Parallel()
	dir := aRecording(t)
	first, files, err := sanitise.HashRecording(dir)
	if err != nil {
		t.Fatalf("HashRecording: %v", err)
	}
	second, again, err := sanitise.HashRecording(dir)
	if err != nil {
		t.Fatalf("HashRecording again: %v", err)
	}
	if first != second {
		t.Errorf("two walks of one recording produced %q then %q", first, second)
	}
	if len(files) != len(again) {
		t.Errorf("two walks covered %d then %d files", len(files), len(again))
	}
	if !strings.HasPrefix(first, "sha256:") {
		t.Errorf("the digest %q does not name its algorithm; an unprefixed digest silently "+
			"compares unlike against one from another algorithm", first)
	}
	// The manifest itself is excluded, because it carries the hash.
	for _, f := range files {
		if f.Path == sanitise.ManifestFile {
			t.Errorf("%s is inside its own hash, which cannot be computed", f.Path)
		}
		// And every file carries its own digest, which is what lets Verify name the one that
		// changed rather than report that one of them did.
		if !strings.HasPrefix(f.SHA256, "sha256:") {
			t.Errorf("%s carries the per-file digest %q, which does not name its algorithm",
				f.Path, f.SHA256)
		}
	}
}

// Verify names the file that was EDITED, not only that something was.
//
// The doc comment promised this before the code did: a manifest carrying paths alone can tell an
// added file from a removed one, and for an edit inside an unchanged file set it could say no more
// than "one of these". On a recording of hundreds of files that is not a finding a reviewer can act
// on, which is the whole difference between a gate and an alarm.
func TestVerifyNamesTheFileThatChanged(t *testing.T) {
	t.Parallel()
	dir := aRecording(t)
	m, err := sanitise.NewManifest(dir, "1.0.0", onePerson(), nil, nil, 0)
	if err != nil {
		t.Fatalf("NewManifest: %v", err)
	}
	if err := m.Verify(dir); err != nil {
		t.Fatalf("a freshly built manifest does not verify: %v", err)
	}

	// Edit one file, leaving the file SET identical — the case that used to be unnameable.
	target := filepath.Join(dir, "events.jsonl")
	body, err := os.ReadFile(target) //nolint:gosec // the test's own recording
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := os.WriteFile(target, append(body, []byte("{\"extra\":true}\n")...), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	err = m.Verify(dir)
	if err == nil {
		t.Fatal("Verify passed after a file was edited")
	}
	if !strings.Contains(err.Error(), "events.jsonl") {
		t.Errorf("Verify does not name the edited file: %v", err)
	}
	if !strings.Contains(err.Error(), "edited since signing") {
		t.Errorf("Verify does not say the file was edited rather than added or removed: %v", err)
	}
}

// A manifest whose content_hash no file explains is reported as what it is.
//
// If every file matches its recorded digest and the overall hash still differs, the recording did
// not move — the hash was written by something other than HashRecording, or edited by hand. Saying
// "a file was edited" there would send a reviewer hunting a change that is not in the files.
func TestVerifyDistinguishesAHashThatNeverMatchedThisRecording(t *testing.T) {
	t.Parallel()
	dir := aRecording(t)
	m, err := sanitise.NewManifest(dir, "1.0.0", onePerson(), nil, nil, 0)
	if err != nil {
		t.Fatalf("NewManifest: %v", err)
	}
	m.ContentHash = "sha256:" + strings.Repeat("0", 64)
	err = m.Verify(dir)
	if err == nil {
		t.Fatal("Verify passed a manifest whose content hash is not this recording's")
	}
	if !strings.Contains(err.Error(), "never on disk") {
		t.Errorf("Verify blames the files instead of the hash: %v", err)
	}
}

// FR-140's second-signature rule, and the three ways it gets defeated.
func TestTheSecondSignatureRuleTakesItsCountFromTheCampaign(t *testing.T) {
	t.Parallel()
	dir := aRecording(t)

	// One signatory declared, one signature: fine.
	one, err := sanitise.NewManifest(dir, "1.0.0", onePerson(), nil, nil, 0)
	if err != nil {
		t.Fatalf("NewManifest: %v", err)
	}
	if err := one.Check(1); err != nil {
		t.Errorf("one signature was refused on a one-person campaign: %v", err)
	}

	// Two declared, one signature: refused. This is the case a flag on the command would wave
	// through, which is why the count comes from the campaign record.
	if err := one.Check(2); !errors.Is(err, sanitise.ErrSecondSignatureMissing) {
		t.Errorf("Check(2) with one signature returned %v, want ErrSecondSignatureMissing", err)
	}

	// Two signatures from ONE person are one signature. Counting them as two is the obvious way to
	// satisfy the rule without a second reviewer.
	twice, err := sanitise.NewManifest(dir, "1.0.0", []sanitise.Signature{
		{By: "the platform owner", On: signedOn},
		{By: "The Platform Owner", On: signedOn.Add(time.Hour)},
	}, nil, nil, 0)
	if err != nil {
		t.Fatalf("NewManifest: %v", err)
	}
	err = twice.Check(2)
	if err == nil {
		t.Fatal("one person signing twice satisfied the two-signature rule")
	}
	if !strings.Contains(err.Error(), "signed twice") {
		t.Errorf("the refusal reads %q, want it to say the same person signed twice", err)
	}

	// No signature at all.
	none, err := sanitise.NewManifest(dir, "1.0.0", nil, nil, nil, 0)
	if err != nil {
		t.Fatalf("NewManifest: %v", err)
	}
	if err := none.Check(1); !errors.Is(err, sanitise.ErrUnsigned) {
		t.Errorf("an unsigned manifest returned %v, want ErrUnsigned", err)
	}

	// A campaign declaring nobody cannot produce a valid manifest, however many signatures appear.
	if err := one.Check(0); err == nil {
		t.Error("a campaign declaring no signatory produced a valid manifest; FR-140 needs a name")
	}
}

// A signature needs a name and a date, and "the platform team" is not a person.
func TestASignatureNeedsANamedPersonAndADate(t *testing.T) {
	t.Parallel()
	dir := aRecording(t)

	for _, tc := range []struct {
		name string
		sig  sanitise.Signature
		want string
	}{
		{"no name", sanitise.Signature{On: signedOn}, "names nobody"},
		{"a blank name", sanitise.Signature{By: "  ", On: signedOn}, "names nobody"},
		{"no date", sanitise.Signature{By: "the platform owner"}, "has no date"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m, err := sanitise.NewManifest(dir, "1.0.0", []sanitise.Signature{tc.sig}, nil, nil, 0)
			if err != nil {
				t.Fatalf("NewManifest: %v", err)
			}
			err = m.Check(1)
			if err == nil {
				t.Fatalf("Check accepted a signature with %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal reads %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// FR-140's "what was dropped", in both senses, and the reason they are two lists.
func TestTheManifestRecordsBothKindsOfDropSeparately(t *testing.T) {
	t.Parallel()
	dir := aRecording(t)

	m, err := sanitise.NewManifest(dir, "1.0.0", onePerson(),
		[]sanitise.Drop{
			{Field: "protopayload.authenticationinfo.principalemail", Why: "names a person"},
			{Field: "a.earlier.field", Why: "free text"},
		},
		[]sanitise.DroppedPayload{{Name: "audit/000007", Why: "an unclassified field"}},
		5)
	if err != nil {
		t.Fatalf("NewManifest: %v", err)
	}
	if len(m.DroppedFields) != 2 {
		t.Errorf("the manifest records %d dropped fields, want 2", len(m.DroppedFields))
	}
	// Sorted, so the same recording produces the same manifest twice and a diff is a real change.
	if m.DroppedFields[0].Field > m.DroppedFields[1].Field {
		t.Error("the dropped fields are unsorted, so the manifest is not reproducible")
	}
	if len(m.DroppedPayloads) != 1 || m.DroppedPayloads[0].Name != "audit/000007" {
		t.Errorf("the dropped payload was not recorded: %+v", m.DroppedPayloads)
	}
	if m.Canaries != 5 {
		t.Errorf("canaries_seeded = %d, want 5", m.Canaries)
	}
	if !strings.Contains(m.Attestation, "not a cryptographic signature") {
		t.Errorf("the attestation does not say what a signature is not: %q", m.Attestation)
	}
}

// Round-tripping, and the two refusals that keep a sign-off from being quietly discarded.
func TestTheManifestRoundTripsAndIsNotSilentlyReplaced(t *testing.T) {
	t.Parallel()
	dir := aRecording(t)

	if sanitise.HasManifest(dir) {
		t.Fatal("an unsigned recording reports a manifest")
	}
	m, err := sanitise.NewManifest(dir, "1.0.0", onePerson(),
		[]sanitise.Drop{{Field: "a.field", Why: "free text"}}, nil, 2)
	if err != nil {
		t.Fatalf("NewManifest: %v", err)
	}
	if err := sanitise.WriteManifest(dir, m, 1); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	if !sanitise.HasManifest(dir) {
		t.Error("the manifest was written and HasManifest says otherwise")
	}

	back, err := sanitise.ReadManifest(dir)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if back.ContentHash != m.ContentHash {
		t.Errorf("the content hash did not round-trip: %q then %q", m.ContentHash, back.ContentHash)
	}
	if len(back.Signatures) != 1 || back.Signatures[0].By != "the platform owner" {
		t.Errorf("the signature did not round-trip: %+v", back.Signatures)
	}
	if !back.Signatures[0].On.Equal(signedOn) {
		t.Errorf("the signature date did not round-trip: %s", back.Signatures[0].On)
	}
	// The written manifest still verifies against the recording — so writing it did not itself
	// change what it covers, which it would if the manifest were inside its own hash.
	if err := back.Verify(dir); err != nil {
		t.Errorf("the manifest does not verify after being written: %v. Writing it changed what it "+
			"covers, which means it is inside its own hash", err)
	}

	// Writing again is refused: it records a person's sign-off.
	if err := sanitise.WriteManifest(dir, m, 1); err == nil {
		t.Error("WriteManifest replaced an existing manifest, discarding a signature a human gave")
	}
}

// Countersigning re-verifies first, because two signatures on different content is worse than one.
func TestCountersigningRefusesARecordingThatChangedSinceTheFirstSignature(t *testing.T) {
	t.Parallel()
	dir := aRecording(t)
	m, err := sanitise.NewManifest(dir, "1.0.0", onePerson(), nil, nil, 0)
	if err != nil {
		t.Fatalf("NewManifest: %v", err)
	}
	if err := sanitise.WriteManifest(dir, m, 1); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}

	second := sanitise.Signature{By: "the on-call lead", On: signedOn.Add(24 * time.Hour)}

	// Unchanged: the countersignature lands and the two-signatory rule is now satisfied.
	if err := sanitise.Countersign(dir, second, 2); err != nil {
		t.Fatalf("Countersign on an unchanged recording: %v", err)
	}
	back, err := sanitise.ReadManifest(dir)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if len(back.Signatures) != 2 {
		t.Fatalf("the manifest holds %d signatures after countersigning, want 2", len(back.Signatures))
	}
	if err := back.Check(2); err != nil {
		t.Errorf("a countersigned manifest still fails the two-signatory rule: %v", err)
	}
	// The header survived the rewrite, so the file still explains itself to whoever opens it in the
	// private repository.
	body, err := os.ReadFile(filepath.Join(dir, sanitise.ManifestFile))
	if err != nil {
		t.Fatalf("read the manifest: %v", err)
	}
	if !strings.HasPrefix(string(body), "# SPDX-License-Identifier") {
		t.Error("countersigning dropped the file's header, so the file no longer explains itself")
	}

	// Now change the recording and try a third signature.
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte("{\"eventId\":\"changed\"}\n"), 0o644); err != nil {
		t.Fatalf("edit the recording: %v", err)
	}
	err = sanitise.Countersign(dir, sanitise.Signature{By: "a third reviewer", On: signedOn}, 3)
	if err == nil {
		t.Fatal("countersigned a recording that changed since the first signature; the signatures " +
			"would read as reviews of one recording when they reviewed two")
	}
	if !strings.Contains(err.Error(), "two signatures would read as two reviews of one recording") {
		t.Errorf("the refusal reads %q; it should say why a second signature on changed content is "+
			"worse than none", err)
	}
}

// An empty recording cannot be signed: a manifest over nothing attests to nothing.
func TestAManifestOverAnEmptyRecordingIsRefused(t *testing.T) {
	t.Parallel()
	_, err := sanitise.NewManifest(t.TempDir(), "1.0.0", onePerson(), nil, nil, 0)
	if err == nil {
		t.Fatal("a manifest was built over an empty directory")
	}
	if !strings.Contains(err.Error(), "nothing to sign") {
		t.Errorf("the refusal reads %q", err)
	}

	// And one with no policy version, which is FR-134's half of the same gate.
	dir := aRecording(t)
	if _, err := sanitise.NewManifest(dir, "  ", onePerson(), nil, nil, 0); err == nil {
		t.Error("a manifest was built with no policy version (FR-134)")
	}
}
