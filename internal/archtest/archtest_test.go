// SPDX-License-Identifier: Apache-2.0

package archtest_test

import (
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const module = "github.com/Pierre-Theophile/aisre/"

// infrastructure is what a domain or SDK package must never link, even transitively: a database
// driver, a vendor SDK, a cloud or cluster client, or the packages that wrap them.
var infrastructure = []string{
	"github.com/jackc/pgx",
	"github.com/anthropics/",
	"k8s.io/client-go",
	"cloud.google.com/go",
	"google.golang.org/api",
	"github.com/testcontainers",
	"github.com/fergusstrange",
	module + "internal/store/postgres",
	module + "internal/log/", // the Postgres-backed event log; its rules live in internal/event
}

// llmSDK is the vendor surface of the model boundary.
var llmSDK = []string{"github.com/anthropics/"}

type rule struct {
	// why is printed with a violation: the reason the rule exists.
	why       string
	packages  []string
	forbidden []string
}

var rules = []rule{
	{
		why: "ingestion (feeders) does not depend on the investigation context; shared vocabulary " +
			"lives in leaf packages (event, knowledgedoc, mask)",
		packages:  []string{"internal/feeders/..."},
		forbidden: []string{module + "internal/investigation/"},
	},
	{
		why: "a telemetry backend is an adapter of the pkg/backend port: it does not reach into the " +
			"workers or the engine that consume it",
		packages: []string{"internal/backends/...", "pkg/backend/..."},
		forbidden: []string{
			module + "internal/investigation/workers",
			module + "internal/investigation/engine",
		},
	},
	{
		why:      "a worker is orchestrated by the engine and never depends on it",
		packages: []string{"internal/investigation/workers/..."},
		forbidden: []string{
			module + "internal/investigation/engine",
			module + "internal/investigation/runner",
		},
	},
	{
		why: "domain packages and the public SDK (pkg/) are infrastructure-free: a connector built on " +
			"pkg/ never links a database driver or a vendor SDK, and domain rules run without a database",
		packages: []string{
			"internal/graph",
			"internal/event",
			"internal/mask",
			"internal/resolution",
			"internal/catalog",
			"internal/investigation/ledger",
			"internal/investigation/workers/logs",
			"internal/investigation/workers/metrics",
			"internal/investigation/workers/traces",
			"pkg/attribution",
			"pkg/backend",
			"pkg/feeder",
			"pkg/feeder/doorbell",
			"pkg/feeder/emit",
			"pkg/feeder/record",
			"pkg/feeder/source",
			"pkg/feeder/versionstamp",
			"pkg/knowledge",
			"pkg/knowledge/memory",
			"pkg/worker",
		},
		forbidden: infrastructure,
	},
	{
		why: "the investigation engine and verifier reason over ports; storage is injected by the runner",
		packages: []string{
			"internal/investigation/engine",
			"internal/investigation/verify",
		},
		forbidden: []string{
			"github.com/jackc/pgx",
			module + "internal/store/postgres",
			"k8s.io/client-go",
			"cloud.google.com/go",
		},
	},
	{
		why: "the model boundary is vendor-neutral: an LLM vendor's SDK is linked only by its adapter " +
			"(internal/investigation/model/anthropic), which the composition root imports",
		packages: []string{
			"internal/investigation/model",
			"internal/investigation/engine",
			"internal/investigation/verify",
			"internal/investigation/budget",
			"internal/investigation/runner",
			"internal/investigation/replay",
		},
		forbidden: llmSDK,
	},
	{
		why: "only the model boundary speaks to an LLM vendor; knowledge, the graph read side and the " +
			"sanitiser must not link one",
		packages: []string{
			"internal/embedding",
			"internal/query",
			"internal/server",
			"internal/sanitise",
		},
		forbidden: llmSDK,
	},
	{
		why: "the graph read side (query, server) does not depend on the investigation engine or on the " +
			"fixture and replay tooling",
		packages: []string{
			"internal/query",
			"internal/server",
		},
		forbidden: []string{
			module + "internal/investigation/engine",
			module + "internal/investigation/replay",
			module + "internal/fixture",
		},
	},
	{
		why: "shared utilities are leaves: they do not depend on an application layer",
		packages: []string{
			"internal/sanitise",
			"internal/mask",
			"internal/event",
			"internal/injection",
			"internal/logmine",
			"internal/knowledgedoc",
		},
		forbidden: []string{module + "internal/investigation/"},
	},
}

// TestDependencyRule fails when a package reaches, directly or transitively, a dependency its
// layer forbids. The fix is to move the dependency behind a port, never to relax the rule.
func TestDependencyRule(t *testing.T) {
	if testing.Short() {
		t.Skip("lists the module's dependency graph with the go tool")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rules {
		for _, pkg := range r.packages {
			deps := listDeps(t, root, pkg)
			for _, dep := range deps {
				for _, bad := range r.forbidden {
					if dep == strings.TrimSuffix(bad, "/") || strings.HasPrefix(dep, bad) {
						t.Errorf("%s depends on %s\n\trule: %s", pkg, dep, r.why)
					}
				}
			}
		}
	}
}

func listDeps(t *testing.T, root, pkg string) []string {
	t.Helper()
	cmd := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", "./"+pkg)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			t.Fatalf("go list %s: %v\n%s", pkg, err, ee.Stderr)
		}
		t.Fatalf("go list %s: %v", pkg, err)
	}
	var deps []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" && line != module+pkg && !strings.HasPrefix(line, module+strings.TrimSuffix(pkg, "...")) {
			deps = append(deps, line)
		}
	}
	return deps
}
