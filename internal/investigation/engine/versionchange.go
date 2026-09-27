// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"fmt"
	"sort"
	"strings"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
)

// A version group resolved to the change that shipped it (005 US5 scenario 9, T075).
//
// errors_by_version names each group's deploy reference — `deploy.commit_sha`, `deploy.image` — by the
// rule every backend applies (pkg/feeder/versionstamp), and a ranked change carries the correlation keys
// its sources stated, the same namespaces. So "the version whose errors rose" and "the rollout that
// shipped it" meet on one value, whichever platform deployed it and whichever backend holds the logs,
// and the engine can say so itself rather than leave the join to the model.
//
// The join is exact: a group names a change only when the change's own sources stated the same
// reference. A `deploy.release` is never joined, as C8 never merges on one: two sources stating
// `v2.3.0` are not evidence they mean the same deploy.

// deployNamespaces are the references a version group may be joined on.
var deployNamespaces = map[string]bool{"deploy.commit_sha": true, "deploy.image": true}

// NoteChangeKeys records the deploy references a ranked change's sources stated.
func (c *Catalogue) NoteChangeKeys(change *graphv1.RankedChange) {
	id := change.GetChange().GetEntityId()
	if id == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.changeByKey == nil {
		c.changeByKey = map[string]string{}
	}
	for _, key := range change.GetCorrelationKeys() {
		if deployNamespaces[key.GetNamespace()] {
			c.changeByKey[key.GetNamespace()+"="+key.GetValue()] = id
		}
	}
}

// ChangeForDeployRef is the change whose sources stated ref, if an answer described one.
func (c *Catalogue) ChangeForDeployRef(ref *graphv1.Ref) (string, bool) {
	if ref == nil || !deployNamespaces[ref.GetNamespace()] {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	id, ok := c.changeByKey[ref.GetNamespace()+"="+ref.GetValue()]
	return id, ok
}

// renderVersionChanges names, under an errors_by_version answer, the change each group's deploy
// reference resolves to. Empty when nothing resolves.
func renderVersionChanges(digest *investigationv1.ErrorsByVersionDigest, cat *Catalogue) string {
	var lines []string
	for _, v := range digest.GetVersions() {
		id, ok := cat.ChangeForDeployRef(v.GetDeployRef())
		if !ok {
			continue
		}
		name := cat.RefFor(id)
		if name == "" {
			name = id
		}
		lines = append(lines, fmt.Sprintf("  version %q is change %q (%s=%s)", v.GetVersion(), name,
			v.GetDeployRef().GetNamespace(), v.GetDeployRef().GetValue()))
	}
	if len(lines) == 0 {
		return ""
	}
	sort.Strings(lines)
	return "versions resolved to the changes that shipped them:\n" + strings.Join(lines, "\n") + "\n"
}
