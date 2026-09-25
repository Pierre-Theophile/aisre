// SPDX-License-Identifier: Apache-2.0

package feeder

import graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"

// An identifier a feeder knows an entity by (004 T056, T057; FR-041; docs/schema/resolution.md).
//
// This type was written twice before it was written here — once in the GCP feeder and once in the
// vendor-notice feeder, field for field the same — and the deploy feeders would have made it four.
// It is the same finding as the clock skew (T045): a shape that every connector needs belongs to the
// SDK, because otherwise the next fix goes into one copy of it.
//
// Both earlier copies are now aliases of this one rather than rewrites of their call sites: an alias
// keeps every existing use compiling and leaves exactly one definition to change.

// Claim is one identifier a feeder knows an entity by, with the namespace it lives in.
type Claim struct {
	Namespace string
	Value     string
	// Why names what the identifier was read from, recorded as claim evidence so a published rule can
	// tell an operator-declared name from a coincidence.
	Why string
	// Attrs are the supporting facts a published rule needs to decide, emitted as the claim's
	// attributes (IdentityFact.Attributes).
	//
	// They are not decoration. C4 needs a revision name with its project and region, C5 the marker
	// saying which side *declared* an OpenTelemetry service name, C8 the deployment environment, P4
	// the environment-variable names. A rule whose supporting attribute the feeder never emits is a
	// rule that never fires, and it fails **silently** — nothing errors, no merge is proposed, and the
	// published rule reads as though it works. Each connector asserts the two spellings agree for
	// exactly that reason.
	Attrs map[string]string
}

// Ref renders the claim as a ref.
func (c Claim) Ref() *graphv1.Ref { return Ref(c.Namespace, c.Value) }

// CorrelationKey is a value SEVERAL entities share (004 T148).
//
// A distinct type from Claim, and the duplication of fields is deliberate: the two are stored under
// different rules and mean different things, and a feeder author holding one should not be able to
// pass it where the other belongs. The compiler enforces the distinction that a comment would only
// describe.
//
// # Which one to emit
//
// Ask whether the value NAMES the entity or DESCRIBES it. `otel.service.name=checkout` names a
// service: two sources saying it are naming one thing, and only one thing can be called that. A commit
// describes a rollout: a monorepo run ships one commit to three services and a redeploy ships it again,
// so three changes legitimately carry it and no one of them owns it.
//
// Getting it wrong in the identity direction is the expensive mistake, and it is refused at the event
// log (internal/log, ReasonCorrelationAsIdentity) rather than discovered later. `graph.identity_claims`
// is constrained `UNIQUE (namespace, value, source_id)`, so a shared value stored as a name lands on
// whichever entity the projector processed first — and the graph's state then depends on the order
// events arrived in.
//
// # A correlation never merges anything by itself
//
// It is the first half of "correlated, then corroborated". C8 merges two rollouts sharing a deploy key
// only where they also agree on an environment AND already share a target the graph itself resolved.
// Put what a rule needs to corroborate with in Attrs — an unstated environment is not an agreed one.
type CorrelationKey struct {
	Namespace string
	Value     string
	// Why names what the value was read from, recorded as evidence.
	Why string
	// Attrs are what a rule corroborates with. A key whose supporting attribute the feeder never emits
	// is a key no rule can act on, and it fails silently.
	Attrs map[string]string
}

// Ref renders the key as a ref.
func (c CorrelationKey) Ref() *graphv1.Ref { return Ref(c.Namespace, c.Value) }

// CorrelationNamespaces is every namespace this SDK publishes as a correlation key, sorted.
//
// It mirrors internal/graph's registry, which is what the event log enforces against, and a test
// asserts the two agree in both directions — a namespace this SDK called a correlation that the log
// treats as a name would be a feeder whose events are refused at run time rather than at review.
var CorrelationNamespaces = []string{
	NSDeployCommitSHA,
	NSDeployImage,
	NSDeployRelease,
}
