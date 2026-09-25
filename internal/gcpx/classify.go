// SPDX-License-Identifier: Apache-2.0

package gcpx

import (
	"sort"
	"strings"
)

// The verb classifier: an explicit read-verb allowlist, an explicit denylist, and FAILING CLOSED on
// anything unrecognised (FR-005; research §8.2).
//
// ---------------------------------------------------------------------------------------------
// Why this is not a substring match, which is the obvious implementation and is wrong.
//
// A rule over `create|update|delete|write|set|insert` misfires on real permissions, in both
// directions, and research §8.2 names the cases:
//
//	cloudasset.assets.exportIamPolicy            the export* family is IN the read role you want
//	compute.urlMaps.validate                     validates a config; writes nothing
//	serviceusage.values.test                     "test" contains no write verb and is a read
//	pubsub.schemas.validate                      as urlMaps.validate
//	cloudsql.instances.preCheckMajorVersionUpgrade   "Upgrade" reads as a write; it is a dry run
//	*.listEffectiveTags                          contains no write verb, is a read, and a naive
//	                                             allowlist on "list" would match the wrong segment
//
// The two failure modes are not symmetric. A false NEGATIVE — calling a write permission a read —
// makes the gate pass a credential that can write production, which is the entire failure this
// package exists to prevent. A false POSITIVE merely refuses to start and gets fixed in a minute. So
// the unrecognised verb is denied, and an operator who hits it adds the verb deliberately.

// readVerbs is the allowlist. A permission whose final segment's verb is here is a read.
//
// Matching is on the VERB, extracted from the permission's last segment, never on the whole string:
// `cloudsql.instances.preCheckMajorVersionUpgrade` has verb `preCheck`, and looking at the whole
// segment is how `Upgrade` would sink it.
var readVerbs = map[string]bool{
	"get":          true,
	"list":         true,
	"watch":        true,
	"read":         true,
	"view":         true,
	"query":        true,
	"search":       true,
	"check":        true,
	"preCheck":     true,
	"test":         true,
	"validate":     true,
	"aggregated":   true,
	"resolve":      true,
	"lookup":       true,
	"describe":     true,
	"download":     true,
	"troubleshoot": true,
	"explain":      true,
}

// writeVerbs is the denylist. It exists alongside fail-closed because an explicitly named write is
// a clearer refusal message than "unrecognised", and because a verb appearing here can never be
// added to the allowlist by accident: the two maps are checked against each other by a test.
var writeVerbs = map[string]bool{
	"create": true, "insert": true, "update": true, "patch": true, "delete": true,
	"remove": true, "set": true, "write": true, "add": true, "attach": true,
	"detach": true, "bind": true, "move": true, "restore": true, "import": true,
	"deploy": true, "run": true, "invoke": true, "publish": true, "send": true,
	"acknowledge": true, "consume": true, "seek": true, "modify": true, "enable": true,
	"disable": true, "reset": true, "restart": true, "stop": true, "start": true,
	"cancel": true, "abandon": true, "drain": true, "undelete": true, "purge": true,
	"truncate": true, "rotate": true, "sign": true, "impersonate": true, "act": true,
	// export is a WRITE for this integration's purposes even though it reads: it produces a
	// dump in a bucket, which is data egress with a side effect, and it is exactly what
	// disqualifies roles/cloudsql.viewer (research §8.2). Naming it here rather than treating it
	// as a read is a deliberate narrowing.
	"export": true,
	// reportMetrics writes metrics to Traffic Director, which is what disqualifies
	// roles/compute.networkViewer and makes the broader roles/compute.viewer the cleaner choice.
	"report": true,
}

// Classification is what the classifier concluded.
type Classification int

const (
	// ClassUnknown is the fail-closed answer: not recognisably a read, so treated as a write.
	ClassUnknown Classification = iota
	// ClassRead is on the allowlist.
	ClassRead
	// ClassWrite is on the denylist.
	ClassWrite
)

// Classify decides whether a permission is a read, a write, or unrecognised.
//
// Unrecognised is NOT a third outcome callers may treat as benign — IsWrite folds it in with write,
// which is the fail-closed rule. It is distinguished only so a refusal can say "this verb is not in
// the allowlist" rather than wrongly asserting the permission writes.
func Classify(permission string) Classification {
	verb := verbOf(permission)
	if verb == "" {
		return ClassUnknown
	}
	if writeVerbs[verb] {
		return ClassWrite
	}
	if readVerbs[verb] {
		return ClassRead
	}
	return ClassUnknown
}

// IsWrite is the gate's question: may this permission change anything? Unrecognised counts as yes.
func IsWrite(permission string) bool { return Classify(permission) != ClassRead }

// verbOf extracts the leading lowerCamel verb from a permission's final segment.
//
// `run.services.setIamPolicy` -> `set`; `cloudsql.instances.preCheckMajorVersionUpgrade` ->
// `preCheck`; `compute.instances.listEffectiveTags` -> `list`. The verb is the run of characters up
// to the first uppercase letter, which is how Google spells these uniformly.
//
// `preCheck` is the one that needs care: its verb by that rule is `pre`, which is in neither map, so
// it would fail closed on a permission that is a dry run. It is spelled out in readVerbs and matched
// as a two-word prefix below.
func verbOf(permission string) string {
	last := permission
	if i := strings.LastIndex(permission, "."); i >= 0 {
		last = permission[i+1:]
	}
	if last == "" {
		return ""
	}
	// Two-word verbs, checked first because the single-word rule would cut them short.
	for _, compound := range []string{"preCheck"} {
		if strings.HasPrefix(last, compound) {
			return compound
		}
	}
	for i, r := range last {
		if r >= 'A' && r <= 'Z' {
			if i == 0 {
				return ""
			}
			return last[:i]
		}
	}
	return last
}

// ReadVerbs and WriteVerbs expose the two lists, sorted, so the published read-only operation list
// in docs/connectors/gcp.md is generated from the same source the gate enforces rather than typed
// out beside it and left to drift (FR-005, SC-020).
func ReadVerbs() []string  { return sortedKeys(readVerbs) }
func WriteVerbs() []string { return sortedKeys(writeVerbs) }

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
