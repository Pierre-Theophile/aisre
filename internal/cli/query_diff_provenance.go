// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"sort"
	"strconv"
	"strings"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// Who, what and where, for each ranked change (004 T128, SC-016).
//
// The ranked table answers "which change?". The next four questions an on-call asks are who did it,
// whether a person or a machine, which commit it shipped, and whether it was already a rollback, and then
// they want to open the run. Every one of those is in the graph; before T128 none of them was on screen.
//
// The commit comes from the change's correlation keys, which the diff attaches (RankedChange.
// correlation_keys): a C8-merged rollout carries the key every source stated, once.

// renderProvenance prints one row per ranked change, in rank order.
func renderProvenance(p *printer, changes []*graphv1.RankedChange) error {
	if len(changes) == 0 {
		return nil
	}
	rows := make([][]string, 0, len(changes))
	for i, item := range changes {
		change := item.GetChange().GetChange()
		rows = append(rows, []string{
			strconv.Itoa(i + 1),
			orDash(change.GetActor()),
			actorKindCell(item.GetActorKind()),
			orDash(commitCell(item.GetCorrelationKeys())),
			rollbackCell(change),
			orDash(changeLink(item.GetChange())),
		})
	}
	if err := p.writeLine(""); err != nil {
		return err
	}
	return p.writeTable([]string{"RANK", "ACTOR", "ACTOR-KIND", "COMMIT", "ROLLBACK", "LINK"}, rows)
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func actorKindCell(kind graphv1.ActorKind) string {
	if kind == graphv1.ActorKind_ACTOR_KIND_UNSPECIFIED {
		return "-"
	}
	return kind.String()
}

// commitCell is every commit the change carries, abbreviated to twelve characters for the table. More
// than one is not an error to hide: it means sources disagree about what shipped, and the reader should
// see both.
func commitCell(keys []*graphv1.Ref) string {
	var commits []string
	for _, key := range keys {
		if key.GetNamespace() != feeder.NSDeployCommitSHA {
			continue
		}
		value := key.GetValue()
		if len(value) > 12 {
			value = value[:12]
		}
		commits = append(commits, value)
	}
	sort.Strings(commits)
	return strings.Join(commits, ", ")
}

// rollbackCell says whether the platform stated a rollback, and what it restored where stated.
func rollbackCell(change *graphv1.Change) string {
	if !change.GetRollback() {
		return "-"
	}
	if to := change.GetRolledBackTo(); to != "" {
		return "yes → " + to
	}
	return "yes"
}

// changeLink is the link that opens the run, or the closest thing the change states.
//
// In order: a GitHub workflow run's SOURCE_LINK, rewritten from the API path the pointer vocabulary stores
// (`repos/{owner}/{repo}/actions/runs/{id}`) to the page that opens the run — first, because SC-016 asks
// for the run, and a C8-merged rollout can carry both the run and the platform's own page; an origin
// reference that is already a web link (Vercel's inspector URL); any SOURCE_LINK that is itself a web link
// (a cloud console URL); the origin reference as stated; and the first source link as stated. The GitHub rewrite assumes github.com: the
// vocabulary leaves the host out on purpose (pkg/feeder/pointer.go), and an Enterprise Server estate
// reads the path and its own host.
func changeLink(version *graphv1.NodeVersion) string {
	origin := strings.TrimSpace(version.GetChange().GetOriginRef())
	var run, web string
	for _, pointer := range version.GetPointers() {
		if pointer.GetKind() != graphv1.PointerKind_SOURCE_LINK {
			continue
		}
		selector := strings.TrimSpace(pointer.GetSelector())
		switch {
		case pointer.GetVocabulary() == feeder.VocabGitHubResource && strings.Contains(selector, "/actions/runs/"):
			if run == "" {
				run = "https://github.com/" + strings.TrimPrefix(selector, "repos/")
			}
		case isWebLink(selector):
			if web == "" {
				web = selector
			}
		}
	}
	switch {
	case run != "":
		return run
	case isWebLink(origin):
		return origin
	case web != "":
		return web
	case origin != "":
		return origin
	}
	// Nothing opens in a browser: the first source link as stated is still where to look.
	for _, pointer := range version.GetPointers() {
		if pointer.GetKind() == graphv1.PointerKind_SOURCE_LINK && strings.TrimSpace(pointer.GetSelector()) != "" {
			return pointer.GetSelector()
		}
	}
	return ""
}

func isWebLink(s string) bool {
	return strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "http://")
}
