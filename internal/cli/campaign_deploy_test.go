// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/sanitise"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres/pgtest"
)

// 003's campaign gates accept a deploy-feeder recording without a code change (004 T111).
//
// The gates were written for GCP, and nothing in them was supposed to know which connector made a
// recording: `record`, `sanitise`, `scan`, `sign` and `verify` walk committed bytes and a manifest,
// and `parity` compares two recordings through the same replay. This drives all six over what
// `feed github --record` and `feed vercel --record` actually write — not over a committed fixture,
// because a fixture is a twin and the campaign will be run over live recordings — so that the first
// time a deploy recording meets these gates is not the recording campaign itself (T114).
//
// It asserts acceptance, not safety. Whether the recordings are clean is what the connectors' own
// recording tests assert, with the leaks they look for named; the gates' job, and this test's, is
// that they run over these recordings, check a non-zero number of files, and hold.
func TestTheCampaignGatesAcceptADeployFeederRecordingUnchanged(t *testing.T) {
	key, err := sanitise.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(sanitise.KeyEnv, key.Hex())
	campaignDir := filepath.Join(t.TempDir(), "campaign-2026-09")

	recordGitHub := func(dir string) {
		t.Helper()
		gh, _ := githubStandIn(t, `{"deployments":"read","actions":"read","metadata":"read"}`)
		_, stderr, code := run(t, t.Context(), "--server", emptyGraph(t),
			"--token", devToken(t, "--user", "github-feeder", "--roles", "f", "--source-id", "github:twin"),
			"feed", "github", "--org", "twin", "--installation", "42", "--api", gh.URL,
			"--app-assertion", "a", "--map", githubMap(t), "--once", "--lookback", "9000h", "--record", dir)
		if code != ExitOK {
			t.Fatalf("feed github --record %s: exit %d\n%s", dir, code, stderr)
		}
	}
	recordVercel := func(dir string) {
		t.Helper()
		srv := vercelStandIn(t)
		_, stderr, code := run(t, t.Context(), "--server", emptyGraph(t),
			"--token", devToken(t, "--user", "vercel-feeder", "--roles", "f", "--source-id", "vercel:twin"),
			"feed", "vercel", "--org", "twin", "--api", srv.URL, "--vercel-token", "vercel-token",
			"--projects", "prj_storefront=k8s.deployment:shop/storefront", "--assert-read-only", "--once",
			"--lookback", "9000h", "--record", dir)
		if code != ExitOK {
			t.Fatalf("feed vercel --record %s: exit %d\n%s", dir, code, stderr)
		}
	}
	recordGitHub(filepath.Join(campaignDir, "github"))
	recordVercel(filepath.Join(campaignDir, "vercel"))

	// T113's measurement runs over the sanitised recordings as the campaign will hold them —
	// pseudonymised ids on both sides — and not only over the twins.
	stdout, stderr, code := run(t, t.Context(), "fixture", "coverage",
		filepath.Join(campaignDir, "github"), filepath.Join(campaignDir, "vercel"))
	if code != ExitOK || !strings.Contains(stdout, "github: 1 of 1") || !strings.Contains(stdout, "vercel: 1 of 1") {
		t.Errorf("coverage over the live recordings (exit %d):\n%s\n%s", code, stdout, stderr)
	}

	// record — the campaign's own account of itself, with the deploy platforms as its scope.
	_, stderr, code = run(t, t.Context(), "fixture", "campaign", "record", campaignDir,
		"--org", "twin",
		"--started-at", "2026-09-01T09:00:00Z",
		"--projects", "github-installation-42,vercel-team-twin",
		"--mailbox-path", "shared_mailbox",
		"--authorised-by", "the platform owner",
		"--authorised-on", "2026-09-01T09:00:00Z",
		"--signatory", "the platform owner",
		"--policy-version", "1.0.0")
	if code != ExitOK {
		t.Fatalf("campaign record exited %d: %s", code, stderr)
	}

	// sanitise and scan — both walk every committed byte of both recordings.
	for _, mode := range []string{"sanitise", "scan"} {
		stdout, stderr, code := run(t, t.Context(), "fixture", "campaign", mode, campaignDir)
		if code != ExitOK {
			t.Fatalf("%s refused a deploy-feeder recording (exit %d): %s\n%s", mode, code, stderr, stdout)
		}
		if !strings.Contains(stdout, "files checked") || strings.Contains(stdout, "files checked: 0") {
			t.Errorf("%s did not check any file, so the gate held over nothing:\n%s", mode, stdout)
		}
	}

	// sign, then verify — one manifest per recording, each matching the bytes it was written over.
	stdout, stderr, code = run(t, t.Context(), "fixture", "campaign", "sign", campaignDir,
		"--signer", "the platform owner", "--on", "2026-09-20T11:00:00Z")
	if code != ExitOK {
		t.Fatalf("sign exited %d: %s\n%s", code, stderr, stdout)
	}
	for _, rec := range []string{"github", "vercel"} {
		m, err := sanitise.ReadManifest(filepath.Join(campaignDir, rec))
		if err != nil {
			t.Fatalf("%s: no signed manifest was written: %v", rec, err)
		}
		if err := m.Verify(filepath.Join(campaignDir, rec)); err != nil {
			t.Errorf("%s: the manifest does not verify against its recording: %v", rec, err)
		}
	}
	stdout, stderr, code = run(t, t.Context(), "fixture", "campaign", "verify", campaignDir)
	if code != ExitOK {
		t.Fatalf("verify refused a signed deploy-feeder campaign (exit %d): %s\n%s", code, stderr, stdout)
	}

	// parity. What FR-143 asks for — the graph built live against the graph built from the recording
	// of that same run — needs a live credential, and is T116. What can be shown now is that the gate
	// runs over a deploy recording's shape and holds, and the two platforms differ in how strongly.
	pg := pgtest.Open(t).Pool().Config().ConnString()
	parity := func(name, live, recorded string) {
		t.Helper()
		stdout, stderr, code := run(t, t.Context(), "fixture", "campaign", "parity", live,
			"--recorded", recorded, "--db", pg)
		if code != ExitOK {
			t.Errorf("parity refused %s (exit %d): %s\n%s", name, code, stderr, stdout)
		}
	}

	// GitHub states every instant a change carries, so two INDEPENDENT runs over the same platform
	// answers must build the same graph. A feeder that read the clock, or took a path only when
	// recording, is what would make them differ.
	again := filepath.Join(t.TempDir(), "github-again")
	recordGitHub(again)
	parity("two independent recordings of one GitHub answer", again, filepath.Join(campaignDir, "github"))

	// Vercel states no promotion instant (research §5.2), so a rollout's valid start is UNKNOWN and
	// begins at the observation — the instant the run read it. Two independent runs therefore
	// legitimately disagree on it, by exactly the time between them, and comparing them is comparing
	// two runs rather than a run with its recording. So the Vercel half is the recording against a
	// copy of itself: the gate reads this recording's shape and holds, which is what "without code
	// change" asks; that a live Vercel run equals its own recording is T116's to show.
	copied := filepath.Join(t.TempDir(), "vercel-copy")
	copyTree(t, filepath.Join(campaignDir, "vercel"), copied, func(rel string) bool {
		return rel != sanitise.ManifestFile
	})
	parity("a Vercel recording and a copy of it", copied, filepath.Join(campaignDir, "vercel"))
}
