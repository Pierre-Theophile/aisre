// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"strings"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	githubfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/github"
	"github.com/Pierre-Theophile/aisre/internal/sanitise"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// Recording a live deploy-feeder run (004 T104). See internal/feeders/deployrecord for the shape.
//
// The recording's events are derived from its sanitised payloads by a second feeder, and that feeder
// needs the operator's mapping in the recording's vocabulary: the payloads name `px_org_…/px_rpo_…`
// where the live mapping names `acme/storefront`, and a mapping left in the clear would both attach
// nothing and write the estate's names into the recording's events. So every identifier the mapping
// names goes through the same sanitiser the payloads did.
//
// Automation accounts are dropped from the shadow's mapping rather than pseudonymised: the payloads no
// longer carry any login for them to match (people are dropped, never hashed — FR-063), so the recorded
// actor kind rests on the platform's own account typing alone.

// recordingSanitiser builds the sanitiser a live recording runs under: the contract table and the
// corpus key from the environment, which is never committed (FR-135). Missing either refuses the run
// before anything is read.
func recordingSanitiser() (*sanitise.Sanitiser, error) {
	key, err := sanitise.KeyFromEnv()
	if err != nil {
		return nil, err
	}
	return sanitise.New(sanitise.ContractPolicy(), key)
}

// pseudonymousRepository maps `owner/name` into the recording's vocabulary.
func pseudonymousRepository(san *sanitise.Sanitiser, slug string) (string, error) {
	owner, name, ok := strings.Cut(strings.TrimSpace(slug), "/")
	if !ok {
		return "", fmt.Errorf("mapping key %q is not owner/repository", slug)
	}
	o, err := san.Identifier(sanitise.KindOrganisation, owner)
	if err != nil {
		return "", err
	}
	r, err := san.Identifier(sanitise.KindRepository, name)
	if err != nil {
		return "", err
	}
	return o + "/" + r, nil
}

// pseudonymousTarget maps a target entity's value into the recording's vocabulary. Its namespace is
// the graph's own vocabulary and stays as it is.
func pseudonymousTarget(san *sanitise.Sanitiser, value string) (string, error) {
	return san.Identifier(sanitise.KindService, value)
}

// sanitisedGitHubMap is the operator's GitHub mapping in the recording's vocabulary.
func sanitisedGitHubMap(san *sanitise.Sanitiser, m githubfeeder.MapOptions) (githubfeeder.MapOptions, error) {
	out := githubfeeder.MapOptions{Allowlist: m.Allowlist, PollInterval: m.PollInterval}
	if len(m.Targets.Repositories) > 0 {
		out.Targets.Repositories = map[string][]githubfeeder.TargetRule{}
	}
	for slug, rules := range m.Targets.Repositories {
		repo, err := pseudonymousRepository(san, slug)
		if err != nil {
			return githubfeeder.MapOptions{}, err
		}
		for _, rule := range rules {
			value, err := pseudonymousTarget(san, rule.Value)
			if err != nil {
				return githubfeeder.MapOptions{}, err
			}
			rule.Value = value
			out.Targets.Repositories[repo] = append(out.Targets.Repositories[repo], rule)
		}
	}
	for _, slug := range m.Releases.ShipsByRelease {
		repo, err := pseudonymousRepository(san, slug)
		if err != nil {
			return githubfeeder.MapOptions{}, err
		}
		out.Releases.ShipsByRelease = append(out.Releases.ShipsByRelease, repo)
	}
	return out, nil
}

// sanitisedVercelTargets is the operator's Vercel mapping in the recording's vocabulary.
func sanitisedVercelTargets(san *sanitise.Sanitiser, targets map[string]*graphv1.Ref) (map[string]*graphv1.Ref, error) {
	out := make(map[string]*graphv1.Ref, len(targets))
	for project, ref := range targets {
		id, err := san.Identifier(sanitise.KindProject, project)
		if err != nil {
			return nil, err
		}
		value, err := pseudonymousTarget(san, ref.GetValue())
		if err != nil {
			return nil, err
		}
		out[id] = feeder.Ref(ref.GetNamespace(), value)
	}
	return out, nil
}
