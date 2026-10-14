// SPDX-License-Identifier: Apache-2.0

package notion

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The independent scan knows Notion's people (007 T029, SC-005, FR-138). It is a grep that shares no
// code with the connector, so this runs the script itself: over each shape Notion writes a person
// in, and over the committed twin, which must pass.
func TestTheIndependentScanRefusesANotionPerson(t *testing.T) {
	script, err := filepath.Abs("../../../scripts/check-no-secrets.sh")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	scan := func(dir string) (string, error) {
		out, err := exec.Command("bash", script, dir).CombinedOutput() //nolint:gosec // the repository's own script
		return string(out), err
	}
	for name, body := range map[string]string{
		"a page's creator":      `{"id":"p1","created_by":{"object":"user","id":"u1"}}`,
		"a page's last editor":  `{"id":"p1","last_edited_by":{"id":"u1"}}`,
		"a people property":     `{"Owner":{"id":"ow","type":"people","people":[]}}`,
		"a person's e-mail":     `{"person":{"email":"riley@shop.example"}}`,
		"an integration secret": `{"token":"ntn_` + strings.Repeat("a1", 23) + `"}`,
		"a canary":              `{"summary":"escalated by sreagentcanary5e1f0a7c"}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "payload.json"), []byte(body+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if out, err := scan(dir); err == nil {
				t.Errorf("the scan passed %s:\n%s", body, out)
			}
		})
	}
	if out, err := scan("../../../fixtures/notion-knowledge-twin-01"); err != nil {
		t.Errorf("the committed twin fails the scan: %v\n%s", err, out)
	}
}
