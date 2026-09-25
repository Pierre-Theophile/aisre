// SPDX-License-Identifier: Apache-2.0

package fixture

import (
	"fmt"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
)

// Refusing to write a golden that asserts nothing (T151).
//
// ---------------------------------------------------------------------------------------------
// Why an empty golden is worse than a missing one
//
// A missing golden is a gap somebody can see. An empty one is a gap that reads as coverage: it is
// written without complaint, it compares equal to itself on every later run, `fixture verify` counts
// it among the goldens compared, and nothing about it looks wrong. It is the only artefact in this
// corpus that can be simultaneously green and vacuous.
//
// T150 is what this file is for. Eight of this corpus's deploy fixtures — all seven `github-*` and
// `vercel-promotion-01` — anchored their PRINCIPAL query on a change, at an instant no change was
// valid at, because a change's valid interval is one microsecond wide. The graph answered nothing,
// correctly. The recorder wrote that nothing, and the verifier compared it against itself, and the
// acceptance evidence for two user stories asserted nothing about the change for as long as the
// fixtures existed. `check-report.sh` counted "406 goldens compared", because it counts files.
//
// `fixture record --help` already promised half of this: "query kinds this build cannot answer yet
// are skipped and named in the report, never written as empty goldens". The reasoning does not
// depend on the query being unanswerable. A query the build CAN answer, whose answer is empty, is
// the same silent artefact — and is the more likely of the two, because it needs no missing feature
// to produce, only a question asked slightly wrong.
//
// # Why the escape hatch is a sentence rather than a boolean
//
// Some answers are meant to be empty, and the corpus has one: `baseline-topology-01`'s
// `before-history` query asks what the graph held before it held anything, and nothing is the right
// answer. A `bool` would let a fixture author silence this check with one character. A REASON has to
// be written down, read by a reviewer in a diff, and be wrong in a way somebody can point at — which
// is the same standard the sanitisation policy holds its own rows to.

// emptyAnswer reports whether a response carries no findings, and names the field that is empty.
//
// It asks the message itself rather than pattern-matching on the query kind, and the difference
// matters: a kind this file has not heard of — one added next year — is checked too, because the
// question "did the graph return any of the things this response is a list of?" does not depend on
// knowing what the kind is called.
//
// What is deliberately NOT counted as content: `extent` and `truncation`. Both are present on every
// response whether or not anything was found, so counting them would make every answer non-empty and
// this whole file a no-op. That is precisely the shape the eight vacuous goldens had — an extent, a
// truncation, and nothing else.
func emptyAnswer(got proto.Message) (string, bool) {
	if got == nil {
		return "the response itself", true
	}
	var populated []string
	var listed int
	message := got.ProtoReflect()
	message.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		name := string(fd.Name())
		if metadataOnly[name] {
			return true
		}
		if fd.IsList() {
			listed += v.List().Len()
			if v.List().Len() > 0 {
				populated = append(populated, name)
			}
			return true
		}
		if fd.IsMap() {
			listed += v.Map().Len()
			if v.Map().Len() > 0 {
				populated = append(populated, name)
			}
			return true
		}
		// A scalar or a message that is set at all counts: a query answering with one value rather
		// than a list has still answered.
		populated = append(populated, name)
		return true
	})
	if len(populated) > 0 || listed > 0 {
		return "", false
	}
	return describeEmpty(got), true
}

// metadataOnly are the fields every response carries regardless of what was found. They say when the
// graph was watching and where the walk was cut, not what it holds.
var metadataOnly = map[string]bool{
	"extent":          true,
	"truncation":      true,
	"ranking_formula": true,
	"as_of":           true,
}

// describeEmpty names what a reader will see in the file, so the refusal is recognisable without
// opening it.
func describeEmpty(got proto.Message) string {
	switch got.(type) {
	case *graphv1.SubgraphResponse:
		return "no nodes and no edges"
	case *graphv1.PointersResponse:
		return "no pointers"
	case *graphv1.DiffResponse:
		return "no changes"
	case *graphv1.ImpactResponse:
		return "nothing in the blast radius"
	case *graphv1.NodeHistoryResponse:
		return "no versions"
	default:
		return "nothing but the extent and the truncation"
	}
}

// EmptyGoldenError is the refusal. It is a distinct type because the fix is never "retry": it is a
// human deciding whether the query is wrong or the emptiness is the point.
type EmptyGoldenError struct {
	FixtureID string
	Query     string
	What      string
	// Pinned says which of the two passes produced the empty answer, because the answer to "why is
	// this empty?" is usually different for each and finding out which costs a reader several minutes
	// otherwise.
	//
	// The PINNED pass overrides observed time with the manifest's `clock.end`. A query that states its
	// own later `observed_at` therefore asks a different question under pinning — "as known at the end
	// of the window" rather than "as known at 20:00" — and if the facts were observed after
	// `clock.end`, nothing is the correct answer and a useless golden. Nine of this corpus's ten empty
	// pinned goldens are that shape.
	Pinned bool
}

func (e *EmptyGoldenError) Error() string {
	pass := "the normal pass"
	hint := "either the query is asking the wrong question — a change's valid interval is one " +
		"microsecond, so a subgraph anchored on one answers nothing at any other instant (T150) — or " +
		"the emptiness is the point"
	if e.Pinned {
		pass = "the observed-time-pinned pass"
		hint = "the pinned pass overrides observed time with the manifest's clock.end, so a query " +
			"stating its own later observed_at asks a different question here and answers nothing " +
			"when the facts were observed after clock.end; check that before assuming the query is " +
			"wrong, and check whether pinning this query was meant to happen at all"
	}
	return fmt.Sprintf("fixture: %s query %q answered with %s on %s, and a golden that asserts "+
		"nothing is worse than a missing one — it reads as coverage, compares equal to itself for "+
		"ever, and is counted among the goldens compared. So %s, in which case say so in the "+
		"manifest: `expect_empty: <why>` on that query",
		e.FixtureID, e.Query, e.What, pass, hint)
}

// checkGolden refuses an empty answer the manifest has not declared.
func checkGolden(m *Manifest, q Query, got proto.Message) error {
	what, empty := emptyAnswer(got)
	if !empty {
		return nil
	}
	if strings.TrimSpace(q.ExpectEmpty) != "" {
		return nil
	}
	return &EmptyGoldenError{
		FixtureID: m.ID, Query: q.Kind + "." + q.Name, What: what, Pinned: q.Pinned,
	}
}
