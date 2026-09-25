// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	githubfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/github"
)

// The operator's GitHub mapping, as a file (004 T157).
//
// Everything a GitHub rollout needs from the operator and cannot get from GitHub: which environments are
// production (FR-023), which workflows ship where no deployment object records it (FR-024), which
// accounts are the organisation's own automation (FR-027), which repositories ship by release (FR-025),
// and which running entity each repository's rollouts change (FR-026). A monorepo's target rules name
// their environment or workflow, which is more than a flag can say legibly.
//
// Unknown keys are refused. A mapping file with `environment:` (singular) in it would otherwise load as an empty
// allowlist, and an empty allowlist emits nothing — a typo that looks exactly like a quiet estate.
//
//	environments: [production]
//	deploy_workflows: [Deploy]
//	automation_accounts: [release-runner]
//	ships_by_release: [acme/sdk]
//	targets:
//	  acme/storefront:
//	    - namespace: k8s.deployment
//	      value: shop/storefront
//	  acme/monorepo:
//	    - {workflow: Deploy search, namespace: k8s.deployment, value: shop/search}

type githubMapFile struct {
	Environments       []string                         `yaml:"environments"`
	DeployWorkflows    []string                         `yaml:"deploy_workflows"`
	AutomationAccounts []string                         `yaml:"automation_accounts"`
	ShipsByRelease     []string                         `yaml:"ships_by_release"`
	Targets            map[string][]githubTargetRuleRow `yaml:"targets"`
}

type githubTargetRuleRow struct {
	Environment string `yaml:"environment"`
	Workflow    string `yaml:"workflow"`
	Namespace   string `yaml:"namespace"`
	Value       string `yaml:"value"`
}

// loadGitHubMap reads the mapping file, if one was named, and adds the command line's allowlists to it.
func loadGitHubMap(path string, environments, workflows []string) (githubfeeder.MapOptions, error) {
	var file githubMapFile
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return githubfeeder.MapOptions{}, fmt.Errorf("read the mapping: %w", err)
		}
		decoder := yaml.NewDecoder(bytes.NewReader(raw))
		decoder.KnownFields(true)
		if err := decoder.Decode(&file); err != nil {
			return githubfeeder.MapOptions{}, fmt.Errorf("mapping %s: %w", path, err)
		}
	}
	opts := githubfeeder.MapOptions{
		Allowlist: githubfeeder.Allowlist{
			Environments: union(file.Environments, environments),
			Workflows:    union(file.DeployWorkflows, workflows),
		},
		Actors:   githubfeeder.ActorPolicy{DeploymentAutomation: union(file.AutomationAccounts, nil)},
		Releases: githubfeeder.ReleasePolicy{ShipsByRelease: union(file.ShipsByRelease, nil)},
	}
	if len(file.Targets) > 0 {
		opts.Targets.Repositories = map[string][]githubfeeder.TargetRule{}
	}
	for repo, rows := range file.Targets {
		if owner, name, ok := strings.Cut(strings.TrimSpace(repo), "/"); !ok || owner == "" || name == "" ||
			strings.Contains(name, "/") {
			return githubfeeder.MapOptions{}, fmt.Errorf("mapping %s: target key %q is not `owner/repository`", path, repo)
		}
		for i, row := range rows {
			if strings.TrimSpace(row.Namespace) == "" || strings.TrimSpace(row.Value) == "" {
				return githubfeeder.MapOptions{}, fmt.Errorf("mapping %s: target %d of %s names no entity; "+
					"a rule needs both `namespace` and `value`", path, i+1, repo)
			}
			opts.Targets.Repositories[strings.TrimSpace(repo)] = append(opts.Targets.Repositories[strings.TrimSpace(repo)],
				githubfeeder.TargetRule{
					Environment: row.Environment, Workflow: row.Workflow,
					Namespace: strings.TrimSpace(row.Namespace), Value: strings.TrimSpace(row.Value),
				})
		}
	}
	return opts, nil
}

// union joins two lists, dropping blanks and exact repeats, keeping first-seen order.
func union(a, b []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, list := range [][]string{a, b} {
		for _, v := range list {
			if v = strings.TrimSpace(v); v != "" && !seen[v] {
				seen[v] = true
				out = append(out, v)
			}
		}
	}
	return out
}
