// SPDX-License-Identifier: Apache-2.0

package sanitise_test

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/Pierre-Theophile/aisre/internal/sanitise"
)

// Whole deploy payloads through the table, each value keeping its shape (004 T104, T106, T107).

const (
	liveSHA      = "8f5bd3c0b0e4a1d2c3f4a5b6c7d8e9f0a1b2c3d4"
	ghDeployment = `[{"id":4321,"sha":"` + liveSHA + `","ref":"jane/hotfix-payments","task":"deploy",` +
		`"environment":"production","production_environment":true,"transient_environment":null,` +
		`"created_at":"2026-03-01T14:00:00Z","updated_at":"2026-03-01T14:03:12Z",` +
		`"creator":{"login":"ada","id":1,"type":"User"},` +
		`"statuses_url":"https://api.github.com/repos/Acme/storefront/deployments/4321/statuses",` +
		`"url":"https://api.github.com/repos/Acme/storefront/deployments/4321"}]`
	ghScope = `{"total_count":1,"repository_selection":"selected","repositories":[{"id":555,"name":"storefront",` +
		`"full_name":"acme/storefront","private":true,"owner":{"login":"acme"}}]}`
	ghStatuses = `{"repository":"acme/storefront","deployment_id":4321,"statuses":[{"id":2,"state":"success",` +
		`"environment":"production","created_at":"2026-03-01T14:03:12Z","creator":{"login":"ada","id":1,"type":"User"},` +
		`"log_url":"https://deploy.example/logs?token=SHOULD-NEVER-BE-STORED"}]}`
	vercelDeployment = `{"deployments":[{"uid":"dpl_42","name":"storefront","projectId":"prj_storefront",` +
		`"target":"production","readyState":"READY","readySubstate":"PROMOTED","source":"git",` +
		`"inspectorUrl":"https://vercel.com/acme/storefront/dpl_42","isRollbackCandidate":false,` +
		`"creator":{"uid":"usr_ada","type":"user"},"meta":null,` +
		`"attribution":{"commitMeta":{"githubCommitSha":"` + liveSHA + `"}},` +
		`"createdAt":1789998840000,"buildingAt":1789998900000,"ready":1789999140000}]}`
	vercelProject = `{"id":"prj_storefront","name":"storefront","createdAt":1740819600000,` +
		`"link":{"type":"github","org":"acme","repo":"storefront","repoId":555},"lastAliasRequest":null}`
)

func sanitisedJSON(t *testing.T, s *sanitise.Sanitiser, root, raw string) map[string]any {
	t.Helper()
	out, err := s.JSON(root, []byte(raw))
	if err != nil {
		t.Fatalf("JSON(%s): %v", root, err)
	}
	var wrapped map[string]any
	if strings.HasPrefix(strings.TrimSpace(raw), "[") {
		var list []any
		if err := json.Unmarshal(out, &list); err != nil {
			t.Fatalf("decode: %v", err)
		}
		wrapped = map[string]any{"": list}
	} else if err := json.Unmarshal(out, &wrapped); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, leak := range []string{"ada", "usr_ada", "jane", "SHOULD-NEVER-BE-STORED", "storefront", liveSHA, "\"acme"} {
		if strings.Contains(string(out), leak) {
			t.Errorf("%s: %q survived sanitisation: %s", root, leak, out)
		}
	}
	return wrapped
}

// A numeric id stays a number, a commit stays forty hex characters, a URL keeps its form with each
// identifying segment replaced, and a person and the deployer's free text are gone.
func TestADeployPayloadKeepsItsShape(t *testing.T) {
	s := newSanitiser(t)
	d := sanitisedJSON(t, s, "github.deployments", ghDeployment)[""].([]any)[0].(map[string]any)

	if _, ok := d["id"].(float64); !ok {
		t.Errorf("id = %#v; a numeric id must stay a number the connector can decode", d["id"])
	}
	if sha, _ := d["sha"].(string); !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(sha) {
		t.Errorf("sha = %q; a commit must stay forty hex characters, or C8 cannot join on it", sha)
	}
	url, _ := d["url"].(string)
	if !regexp.MustCompile(`^https://api\.github\.com/repos/px_org_[a-z2-7]+/px_rpo_[a-z2-7]+/deployments/[0-9]{15}$`).MatchString(url) {
		t.Errorf("url = %q; want the same URL with the organisation, repository and id replaced", url)
	}
	for _, gone := range []string{"ref"} {
		if _, ok := d[gone]; ok {
			t.Errorf("%s survived: a branch name is free text", gone)
		}
	}
	creator, _ := d["creator"].(map[string]any)
	if creator["type"] != "User" || creator["login"] != nil || creator["id"] != nil {
		t.Errorf("creator = %v; the platform's account type is actor-kind evidence and stays, the person goes", creator)
	}
	if d["environment"] != "production" || d["task"] != "deploy" {
		t.Errorf("environment/task = %v/%v; what the operator's allowlist matches stays verbatim", d["environment"], d["task"])
	}
}

// §2.3 property 1 across payloads and across SOURCES: one identifier is one token wherever it
// appears. That is what keeps a sanitised deployment joined to its statuses and its repository, and a
// sanitised GitHub rollout joined by C8 to Vercel's record of the same commit (T107).
func TestOneIdentifierIsOneTokenAcrossPayloadsAndSources(t *testing.T) {
	s := newSanitiser(t)
	d := sanitisedJSON(t, s, "github.deployments", ghDeployment)[""].([]any)[0].(map[string]any)
	scope := sanitisedJSON(t, s, "github.installation-repositories", ghScope)
	statuses := sanitisedJSON(t, s, "github.deployment-statuses", ghStatuses)
	vd := sanitisedJSON(t, s, "vercel.deployments", vercelDeployment)["deployments"].([]any)[0].(map[string]any)
	vp := sanitisedJSON(t, s, "vercel.project", vercelProject)

	repo := scope["repositories"].([]any)[0].(map[string]any)
	owner := repo["owner"].(map[string]any)["login"].(string)
	name := repo["name"].(string)
	// Case-folded: GitHub's `Acme` in the URL and `acme` in the grant are one organisation.
	if want := owner + "/" + name; statuses["repository"] != want || !strings.Contains(d["url"].(string), "/"+want+"/") {
		t.Errorf("the repository is %q in the grant, %q in the statuses and %q in the URL; one repository, one token",
			want, statuses["repository"], d["url"])
	}
	if statuses["deployment_id"] != d["id"] {
		t.Errorf("the deployment is %v in its statuses and %v in itself", statuses["deployment_id"], d["id"])
	}
	link := vp["link"].(map[string]any)
	if link["org"] != owner || link["repo"] != name || link["repoId"] != repo["id"] {
		t.Errorf("Vercel's linked repository is %v/%v (%v); GitHub's is %s/%s (%v)", link["org"], link["repo"],
			link["repoId"], owner, name, repo["id"])
	}
	commit := vd["attribution"].(map[string]any)["commitMeta"].(map[string]any)["githubCommitSha"]
	if commit != d["sha"] {
		t.Errorf("the same commit is %v from Vercel and %v from GitHub; C8's join would not survive", commit, d["sha"])
	}
	if vd["projectId"] != vp["id"] || vd["name"] != vp["name"] {
		t.Errorf("the deployment's project (%v, %v) is not the project's own (%v, %v)", vd["projectId"], vd["name"], vp["id"], vp["name"])
	}
}

// A field no row names fails the whole payload: nothing of it is returned to be written (FR-064).
func TestAnUnassignedFieldRefusesTheWholePayload(t *testing.T) {
	s := newSanitiser(t)
	raw := strings.Replace(ghDeployment, `"task":"deploy",`, `"task":"deploy","description":"rotated sk_live_123",`, 1)
	out, err := s.JSON("github.deployments", []byte(raw))
	var unassigned *sanitise.UnassignedError
	if !errors.As(err, &unassigned) || out != nil {
		t.Fatalf("JSON = %s, %v; want no bytes and the unassigned field named", out, err)
	}
	if !strings.Contains(unassigned.Field, "description") {
		t.Errorf("the refusal names %q, not the field that caused it", unassigned.Field)
	}
}

// A value of the wrong shape is dropped rather than hashed into something that looks right: a branch
// name where a commit may be, a URL on a host nobody located the identifiers in.
func TestAValueOfTheWrongShapeIsDropped(t *testing.T) {
	s := newSanitiser(t)
	release := `{"repository":"acme/storefront","releases":[{"id":7,"tag_name":"v2.3.0","name":"Big release",` +
		`"target_commitish":"main","draft":false,"prerelease":false,"created_at":"2026-03-01T14:00:00Z",` +
		`"published_at":"2026-03-01T14:00:00Z","author":{"login":"ada","id":1,"type":"User"},"html_url":"https://github.com/acme/storefront/releases/tag/v2.3.0"}]}`
	r := sanitisedJSON(t, s, "github.releases", release)["releases"].([]any)[0].(map[string]any)
	if _, ok := r["target_commitish"]; ok {
		t.Errorf("target_commitish survived as %v; a branch name is not a commit to pseudonymise", r["target_commitish"])
	}
	if r["tag_name"] != "v2.3.0" {
		t.Errorf("tag_name = %v; the release key stays verbatim", r["tag_name"])
	}

	enterprise := strings.ReplaceAll(ghDeployment, "https://api.github.com", "https://ghe.internal.acme")
	d := sanitisedJSON(t, s, "github.deployments", enterprise)[""].([]any)[0].(map[string]any)
	if _, ok := d["url"]; ok {
		t.Errorf("url survived as %v; a URL the template does not describe is dropped", d["url"])
	}
}

// The table itself refuses a shape on a rule that cannot carry one, and a template that names no
// published kind.
func TestAShapedRuleIsValidated(t *testing.T) {
	for name, rule := range map[string]sanitise.Rule{
		"shape on a verbatim row": {Disposition: sanitise.Verbatim, Shape: sanitise.ShapeDigits, Why: "x"},
		"unknown kind in a template": {Disposition: sanitise.Pseudonym, Kind: sanitise.KindRepository,
			Shape: sanitise.ShapeTemplate, Template: "https://x/{nonsense}", Why: "x"},
		"template without its shape": {Disposition: sanitise.Pseudonym, Kind: sanitise.KindRepository,
			Template: "https://x/{repository}", Why: "x"},
	} {
		p := sanitise.NewPolicy("t", map[string]sanitise.Rule{"a.b": rule}, nil)
		if err := p.Validate(); err == nil {
			t.Errorf("%s: the policy validated", name)
		}
	}
}
