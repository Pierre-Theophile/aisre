// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/store/postgres/pgtest"
)

// `catalog` and `attribute` against a running `serve` (009 T018, FR-012, FR-040): every decision
// prints its event and principal, an unchanged catalog file records nothing, and the attributor in
// `serve` attributes a monitor through an imported alias until a person corrects it.

func startServe(t *testing.T) string {
	t.Helper()
	dsn := pgtest.Open(t).Pool().Config().ConnString()
	addrCh := make(chan string, 1)
	previous := serveReady
	serveReady = func(addr string) { addrCh <- addr }
	t.Cleanup(func() { serveReady = previous })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		_, _, code := run(t, ctx, "--log-level", "error", "serve", "--db", dsn, "--listen", "127.0.0.1:0",
			"--auth", "dev", "--dev", "--migrate")
		done <- code
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("serve did not shut down")
		}
	})
	select {
	case addr := <-addrCh:
		return "http://" + addr
	case code := <-done:
		t.Fatalf("serve exited before listening, code %d", code)
	case <-time.After(60 * time.Second):
		t.Fatal("serve did not start listening")
	}
	return ""
}

func catalogToken(t *testing.T, args ...string) string {
	t.Helper()
	stdout, stderr, code := run(t, context.Background(), append([]string{"dev-token", "--dev"}, args...)...)
	if code != ExitOK {
		t.Fatalf("dev-token: exit %d (stderr %q)", code, stderr)
	}
	return strings.TrimSpace(stdout)
}

func ingestJSON(t *testing.T, baseURL, token, procedure, body string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, baseURL+procedure, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post %s: %v", procedure, err)
	}
	payload, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || strings.Contains(string(payload), "REJECTED") {
		t.Fatalf("%s: %d %s", procedure, resp.StatusCode, payload)
	}
}

func TestCatalogAndAttributeCommands(t *testing.T) {
	baseURL := startServe(t)
	decider := catalogToken(t, "--user", "pierre", "--roles", "r,d")
	feeder := catalogToken(t, "--user", "dd-feeder", "--roles", "f", "--source-id", "datadog:demo")
	cli := func(args ...string) string {
		t.Helper()
		stdout, stderr, code := run(t, context.Background(), append(args, "--server", baseURL, "--token", decider)...)
		if code != ExitOK {
			t.Fatalf("%v: exit %d (stderr %q, stdout %q)", args, code, stderr, stdout)
		}
		return stdout
	}

	// A monitor watching a service, as a connector would send them.
	ingestJSON(t, baseURL, feeder, "/sreagent.graph.v1.IngestService/RegisterSource",
		`{"sourceId":"datadog:demo","kind":"datadog","ordering":"none","schemaVersion":"1.0.0"}`)
	ingestJSON(t, baseURL, feeder, "/sreagent.graph.v1.IngestService/IngestBatch", `{"events":[
		{"eventId":"dd:svc","idempotencyKey":"dd:svc","sourceId":"datadog:demo","schemaVersion":"1.0.0",
		 "upsertNode":{"ref":{"namespace":"datadog.service","value":"production/candidate-api"},"type":"SERVICE",
		   "displayName":"candidate-api","validAt":"2026-10-19T09:00:00Z"}},
		{"eventId":"dd:mon","idempotencyKey":"dd:mon","sourceId":"datadog:demo","schemaVersion":"1.0.0",
		 "alertTransition":{"monitor":{"namespace":"datadog.monitor","value":"4242"},"transitionAt":"2026-10-19T09:05:00Z",
		   "fromState":"ok","toState":"alert","transport":"poll","title":"candidate-api error rate",
		   "watches":[{"namespace":"datadog.service","value":"production/candidate-api"}]}}]}`)

	file := filepath.Join(t.TempDir(), "catalog.yaml")
	if err := os.WriteFile(file, []byte(`version: 1
entries:
  - slug: candidate-view
    name: Candidate View
    aliases: [datadog.service=production/candidate-api]
  - slug: infra
    name: Infra
    kind: platform
`), 0o600); err != nil {
		t.Fatal(err)
	}
	out := cli("catalog", "import", file)
	if !strings.Contains(out, "declare entry candidate-view (Candidate View) recorded as ") ||
		!strings.Contains(out, " by ") || strings.Count(out, "recorded as") != 3 {
		t.Fatalf("first import:\n%s", out)
	}
	if out := cli("catalog", "import", file); !strings.Contains(out, "already matches") {
		t.Errorf("second import:\n%s", out)
	}
	if out := cli("catalog", "list"); !strings.Contains(out, "candidate-view") || !strings.Contains(out, "datadog.service=production/candidate-api") {
		t.Errorf("list:\n%s", out)
	}

	// The old maps (009 T030): 007's Notion `entities` and 006's Slack `--service`, one alias a line.
	out = cli("catalog", "import", "--from-notion-config", filepath.Join("testdata", "notion-twin-02.yaml"), "--dry-run")
	for _, want := range []string{
		"would record: declare entry checkout (Checkout)",
		"would record: alias notion.value=4e5f6071-8293-44a4-b5c6-d7e8f90a1b2c/Checkout → checkout",
		"would record: alias otel.service.name=checkout → checkout",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("--from-notion-config: no %q in\n%s", want, out)
		}
	}
	out = cli("catalog", "import", "--from-slack-flags", "candidate=datadog.service=production/candidate-api", "--dry-run")
	if !strings.Contains(out, "conflict, left as it is: datadog.service=production/candidate-api") ||
		!strings.Contains(out, "would record: alias slack.slug=candidate → candidate") {
		t.Errorf("--from-slack-flags:\n%s", out)
	}
	if _, stderr, code := run(t, context.Background(), "catalog", "import", file, "--from-slack-flags", "a=b=c",
		"--server", baseURL, "--token", decider); code != ExitUsage || !strings.Contains(stderr, "give one of") {
		t.Errorf("a file and a map at once: exit %d, %q", code, stderr)
	}

	// The attributor in serve picks the alias up.
	waitFor := func(want string) string {
		t.Helper()
		var out string
		for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
			if out = cli("attribute", "why", "datadog.monitor=4242"); strings.Contains(strings.SplitN(out, "\n", 2)[0], want) {
				return out
			}
		}
		t.Fatalf("attribution never became %q:\n%s", want, out)
		return ""
	}
	waitFor("candidate-view (alias A1")

	out = cli("attribute", "correct", "datadog.monitor=4242", "--entry", "infra", "--reason", "the monitor is on the shared ingress")
	if !strings.Contains(out, "correction of datadog.monitor=4242 recorded as ") || !strings.Contains(out, " by ") {
		t.Fatalf("correct:\n%s", out)
	}
	why := waitFor("infra (correction H")
	if !strings.Contains(why, "superseded by") {
		t.Errorf("the automated decision is not shown as superseded:\n%s", why)
	}

	if out := cli("catalog", "hide", "--name", "datadog.service=staging/candidate-api"); !strings.Contains(out, "hide of datadog.service=staging/candidate-api recorded as") {
		t.Errorf("hide name:\n%s", out)
	}
	if out := cli("catalog", "rename", "infra", "--name", "Platform"); !strings.Contains(out, "rename of infra recorded as") {
		t.Errorf("rename:\n%s", out)
	}
	if out := cli("catalog", "show", "infra"); !strings.Contains(out, "infra — Platform (platform, confirmed)") {
		t.Errorf("show:\n%s", out)
	}

	// Discovery proposes an entry for a service nobody catalogued, and its staging twin as an alias.
	ingestJSON(t, baseURL, feeder, "/sreagent.graph.v1.IngestService/IngestBatch", `{"events":[
		{"eventId":"dd:svc2","idempotencyKey":"dd:svc2","sourceId":"datadog:demo","schemaVersion":"1.0.0",
		 "upsertNode":{"ref":{"namespace":"datadog.service","value":"production/checkout-api"},"type":"SERVICE",
		   "displayName":"checkout-api","validAt":"2026-10-19T09:00:00Z"}},
		{"eventId":"dd:svc3","idempotencyKey":"dd:svc3","sourceId":"datadog:demo","schemaVersion":"1.0.0",
		 "upsertNode":{"ref":{"namespace":"datadog.service","value":"staging/checkout-api"},"type":"SERVICE",
		   "displayName":"checkout-api","validAt":"2026-10-19T09:00:00Z"}}]}`)
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		out = cli("catalog", "proposals")
		if strings.Contains(out, "alias  checkout-api") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("discovery proposed nothing:\n%s", out)
		}
	}
	// Discovery reads services in entity order: the staging one named the entry.
	if !strings.Contains(out, "P1") || !strings.Contains(out, "0.85") || !strings.Contains(out, "datadog.service=production/checkout-api") {
		t.Errorf("proposals:\n%s", out)
	}
	if out := cli("catalog", "alias", "accept", "datadog.service=production/checkout-api", "checkout-api"); !strings.Contains(out, "alias datadog.service=production/checkout-api → checkout-api recorded as") {
		t.Errorf("accept:\n%s", out)
	}
	if out := cli("catalog", "proposals", "--kind", "alias", "--status", "accepted"); !strings.Contains(out, "datadog.service=production/checkout-api") {
		t.Errorf("the accepted proposal:\n%s", out)
	}
	if out := cli("catalog", "alias", "reject", "datadog.service=staging/checkout-api", "infra"); !strings.Contains(out, "rejection of alias datadog.service=staging/checkout-api → infra recorded as") {
		t.Errorf("reject:\n%s", out)
	}
}
