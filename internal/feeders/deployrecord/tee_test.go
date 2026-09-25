// SPDX-License-Identifier: Apache-2.0

package deployrecord_test

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/feeders/deployrecord"
	"github.com/Pierre-Theophile/aisre/internal/feeders/github"
	"github.com/Pierre-Theophile/aisre/internal/sanitise"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/source"
)

// Recording a live run without writing an unsanitised byte (004 T104, T105; FR-062, FR-137).

const sha = "8f5bd3c0b0e4a1d2c3f4a5b6c7d8e9f0a1b2c3d4"

var at = time.Date(2026, 3, 1, 14, 5, 0, 0, time.UTC)

func payload(kind, body string) feeder.Payload {
	return feeder.Payload{Kind: kind, At: at, Bytes: []byte(body)}
}

// The estate as a live GitHub cycle reads it: the grant, one deployment by a person, its statuses with
// a token in the deployer's log link, and the marker.
func livePayloads() []feeder.Payload {
	return []feeder.Payload{
		payload(github.PayloadInstallationRepositories, `{"total_count":1,"repository_selection":"selected",`+
			`"repositories":[{"id":555,"name":"storefront","full_name":"acme/storefront","private":true,"owner":{"login":"acme"}}]}`),
		payload(github.PayloadDeployments, `[{"id":4321,"sha":"`+sha+`","ref":"main","task":"deploy",`+
			`"environment":"production","production_environment":true,"created_at":"2026-03-01T14:00:00Z",`+
			`"updated_at":"2026-03-01T14:03:12Z","creator":{"login":"ada","id":1,"type":"User"},`+
			`"statuses_url":"https://api.github.com/repos/acme/storefront/deployments/4321/statuses",`+
			`"url":"https://api.github.com/repos/acme/storefront/deployments/4321"}]`),
		payload(github.PayloadDeploymentStatuses, `{"repository":"acme/storefront","deployment_id":4321,"statuses":[`+
			`{"id":2,"state":"success","environment":"production","created_at":"2026-03-01T14:03:12Z",`+
			`"creator":{"login":"ada","id":1,"type":"User"},"log_url":"https://deploy.example/logs?token=SHOULD-NEVER-BE-STORED"},`+
			`{"id":1,"state":"in_progress","environment":"production","created_at":"2026-03-01T14:01:30Z",`+
			`"creator":{"login":"ada","id":1,"type":"User"}}]}`),
		payload(github.PayloadPollMarker, `{"outcome":"complete"}`),
	}
}

// leaks are the estate's own strings. None may be anywhere under the recording directory.
var leaks = []string{"acme", "storefront", "ada", sha, "SHOULD-NEVER-BE-STORED", "shop/"}

func assertNothingLeaked(t *testing.T, dir string) (files int) {
	t.Helper()
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		files++
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("read %s: %v", path, readErr)
		}
		for _, leak := range leaks {
			if strings.Contains(string(raw), leak) {
				t.Errorf("%s holds %q: an unsanitised byte reached disk", strings.TrimPrefix(path, dir), leak)
			}
		}
		return nil
	})
	return files
}

func sanitiser(t *testing.T) *sanitise.Sanitiser {
	t.Helper()
	key, err := sanitise.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	s, err := sanitise.New(sanitise.ContractPolicy(), key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func drain(t *testing.T, src feeder.Source) ([]feeder.Payload, error) {
	t.Helper()
	var out []feeder.Payload
	for {
		p, err := src.Next(t.Context())
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		out = append(out, p)
	}
}

// The live feeder gets the platform's bytes; the recording gets the sanitiser's, and they replay into a
// recording whose events are derived from nothing else.
func TestTheLiveFeederGetsThePlatformAndTheRecordingGetsTheSanitiser(t *testing.T) {
	dir := t.TempDir()
	san := sanitiser(t)
	tee, err := deployrecord.NewTee(source.NewSliceSource(livePayloads()), github.Kind, san, dir)
	if err != nil {
		t.Fatal(err)
	}
	passed, err := drain(t, tee)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(passed) != 4 || !strings.Contains(string(passed[1].Bytes), `"login":"ada"`) {
		t.Fatalf("the live feeder was handed something other than the platform's payloads: %d, %s", len(passed), passed[1].Bytes)
	}
	if tee.Written() != 4 {
		t.Fatalf("%d payloads recorded, want 4; refused: %v", tee.Written(), tee.Dropped())
	}

	shadow, err := github.New(github.Options{OrgSlug: "twin", Map: github.MapOptions{
		Allowlist: github.Allowlist{Environments: []string{"production"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	events, err := tee.Finish(t.Context(), dir, "github-deployment", shadow)
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if events < 2 {
		t.Errorf("%d events derived; the recorded rollout should replay to a change and its commit key", events)
	}
	if files := assertNothingLeaked(t, dir); files < 6 {
		t.Errorf("%d files recorded; want the payloads, their index, the events and the manifest", files)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if !strings.Contains(string(raw), `"ROLLOUT"`) || !strings.Contains(string(raw), "px_org_") {
		t.Errorf("the derived events carry no rollout in the recording's vocabulary:\n%s", raw)
	}
}

// T105's three shapes. A people identifier is dropped from a payload that is still written; a field no
// row names stops that payload from being written at all; a canary reaching a value ends the run with
// nothing of that payload on disk.
func TestNothingUnsanitisedReachesDisk(t *testing.T) {
	t.Run("an unassigned field", func(t *testing.T) {
		dir := t.TempDir()
		payloads := livePayloads()
		payloads[1].Bytes = []byte(strings.Replace(string(payloads[1].Bytes), `"task":"deploy",`,
			`"task":"deploy","description":"rotated sk_live_123 for ada",`, 1))
		tee, err := deployrecord.NewTee(source.NewSliceSource(payloads), github.Kind, sanitiser(t), dir)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := drain(t, tee); err != nil {
			t.Fatalf("drain: %v", err)
		}
		if tee.Written() != 3 || len(tee.Dropped()) != 1 || !strings.Contains(tee.Dropped()[0], "description") {
			t.Errorf("written %d, refused %v; the payload with an unnamed field is refused, named, and the rest recorded",
				tee.Written(), tee.Dropped())
		}
		assertNothingLeaked(t, dir)
		if hits, _ := filepath.Glob(filepath.Join(dir, "payloads", "deployments", "*")); len(hits) != 0 {
			t.Errorf("the refused payload's kind has files on disk: %v", hits)
		}
	})

	t.Run("a canary", func(t *testing.T) {
		dir := t.TempDir()
		san := sanitiser(t)
		canary, err := sanitise.NewCanary(sanitise.CanaryInfrastructure, "github")
		if err != nil {
			t.Fatal(err)
		}
		set := sanitise.NewCanarySet(san.Key())
		set.Add(canary)
		san = san.WithCanaries(set)
		payloads := livePayloads()
		payloads[0].Bytes = []byte(strings.Replace(string(payloads[0].Bytes), `"name":"storefront"`,
			`"name":"`+canary.Token+`"`, 1))
		tee, err := deployrecord.NewTee(source.NewSliceSource(payloads), github.Kind, san, dir)
		if err != nil {
			t.Fatal(err)
		}
		_, err = drain(t, tee)
		var survived *sanitise.CanarySurvivedError
		if !errors.As(err, &survived) {
			t.Fatalf("drain = %v; a canary reaching a recorded value is a broken gate and ends the run", err)
		}
		if tee.Written() != 0 {
			t.Errorf("%d payloads written before the canary stopped the run; the canary's payload was the first", tee.Written())
		}
		if _, err := os.Stat(filepath.Join(dir, "payloads")); !os.IsNotExist(err) {
			t.Error("the recording directory was created for a run that wrote nothing")
		}
	})

	t.Run("an aborted run", func(t *testing.T) {
		dir := t.TempDir()
		ctx, cancel := context.WithCancel(t.Context())
		tee, err := deployrecord.NewTee(&cancelling{payloads: livePayloads(), after: 2, cancel: cancel},
			github.Kind, sanitiser(t), dir)
		if err != nil {
			t.Fatal(err)
		}
		for {
			if _, err := tee.Next(ctx); err != nil {
				break
			}
		}
		if tee.Written() != 2 {
			t.Errorf("%d written before the abort, want 2", tee.Written())
		}
		assertNothingLeaked(t, dir)
	})
}

// cancelling ends the run part-way, the way an operator's Ctrl-C does.
type cancelling struct {
	payloads []feeder.Payload
	after    int
	cancel   context.CancelFunc
	n        int
}

func (c *cancelling) Next(ctx context.Context) (feeder.Payload, error) {
	if c.n == c.after {
		c.cancel()
	}
	if err := ctx.Err(); err != nil {
		return feeder.Payload{}, err
	}
	p := c.payloads[c.n]
	c.n++
	return p, nil
}

func TestATeeWithoutASanitiserIsRefused(t *testing.T) {
	if _, err := deployrecord.NewTee(source.NewSliceSource(nil), github.Kind, nil, t.TempDir()); err == nil {
		t.Error("a recording with no sanitiser was built; it would write whatever it was handed")
	}
}
