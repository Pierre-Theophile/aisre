// SPDX-License-Identifier: Apache-2.0

// Package migrate transforms logged events from one event-schema version to another.
//
// Constitution IX requires a breaking schema change to ship with "a migration path expressed as
// an event-log transformation, so that Principle III still holds": replaying a log recorded
// under an older schema must still reproduce the graph. FR-025 says the same thing from the
// feeder's side. This package is that path.
//
// # The shape of a migration
//
// A Transformer rewrites one envelope from version From to version To. Migrations compose: an
// envelope at 1.0.0 in a build that speaks 1.2.0 runs 1.0.0→1.1.0 then 1.1.0→1.2.0, and a
// Registry resolves that chain. There is no "downgrade" direction and there will not be one —
// history only moves forward, and a build that cannot read a version is told to say so rather
// than to guess.
//
// # Why it exists before it is needed
//
// Today the graph speaks exactly one version, 1.0.0, and the only registered transformer is the
// identity. That is deliberate. The hook has to be in the append path *before* the first
// breaking change, because the alternative — adding it during the migration — means the
// migration is written against a log format nothing has ever transformed, and every recorded
// fixture in the repository would have to be rewritten instead of replayed.
//
// So this is a working, tested mechanism with nothing yet to do:
//
//   - an envelope whose schema_version the build accepts is passed through untouched;
//   - an envelope at an older version the registry can reach the accepted one from is
//     transformed, then validated, then appended under the version it was transformed to;
//   - an envelope at a version the registry cannot reach is rejected with
//     `unknown_schema_version`, naming the versions that are accepted (edge case "schema
//     version drift").
//
// # What a transformer may and may not do
//
// It may add, rename, re-shape or drop fields of the payload. It MUST NOT change the event id,
// the idempotency key or the source id: those are what make a re-delivery a no-op (FR-020), and
// a migration that renumbered events would make every replay a different graph. It MUST be
// deterministic and free of side effects — it runs on every replay, so a transformer that read a
// clock or a database would make replay non-reproducible, which is the one thing constitution
// III does not permit.
package migrate

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"google.golang.org/protobuf/proto"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
)

// Transformer rewrites one event envelope from one schema version to the next.
//
// Apply must not mutate its argument: the caller may still hold the original — a replay reads
// it back out of the log — so a transformer returns a new envelope or the same pointer
// unchanged when it has nothing to do.
type Transformer interface {
	// From is the schema version this transformer reads.
	From() string
	// To is the schema version it produces.
	To() string
	// Apply rewrites env. It returns an error only when the envelope cannot be expressed in
	// the target version at all, which is a defect in the migration or a corrupt log.
	Apply(env *graphv1.EventEnvelope) (*graphv1.EventEnvelope, error)
}

// Func builds a Transformer from a function, which is what most migrations are.
func Func(from, to string, apply func(*graphv1.EventEnvelope) (*graphv1.EventEnvelope, error)) Transformer {
	return funcTransformer{from: from, to: to, apply: apply}
}

type funcTransformer struct {
	from, to string
	apply    func(*graphv1.EventEnvelope) (*graphv1.EventEnvelope, error)
}

func (f funcTransformer) From() string { return f.from }
func (f funcTransformer) To() string   { return f.to }
func (f funcTransformer) Apply(env *graphv1.EventEnvelope) (*graphv1.EventEnvelope, error) {
	return f.apply(env)
}

// Identity is the no-op transformer for one version onto itself. It is what the registry holds
// for the current version and what a chain of length zero resolves to.
func Identity(version string) Transformer {
	return Func(version, version, func(env *graphv1.EventEnvelope) (*graphv1.EventEnvelope, error) {
		return env, nil
	})
}

// Chain is an ordered sequence of transformers from one version to another. The zero value is
// the empty chain, which is a valid identity.
type Chain []Transformer

// From is the version the chain reads, empty for the identity chain.
func (c Chain) From() string {
	if len(c) == 0 {
		return ""
	}
	return c[0].From()
}

// To is the version the chain produces, empty for the identity chain.
func (c Chain) To() string {
	if len(c) == 0 {
		return ""
	}
	return c[len(c)-1].To()
}

// IsIdentity reports whether the chain has nothing to do.
func (c Chain) IsIdentity() bool {
	for _, t := range c {
		if t.From() != t.To() {
			return false
		}
	}
	return true
}

// Steps renders the chain as "1.0.0 → 1.1.0 → 1.2.0", for a log line or a rejection detail.
func (c Chain) Steps() string {
	if len(c) == 0 {
		return ""
	}
	parts := make([]string, 0, len(c)+1)
	parts = append(parts, c[0].From())
	for _, t := range c {
		parts = append(parts, t.To())
	}
	return strings.Join(parts, " → ")
}

// Apply runs every step in order and returns the transformed envelope with its schema_version
// set to the chain's target.
//
// The envelope is cloned before the first transformer touches it, so a caller that still holds
// the original — the replay path does — sees it unchanged.
func (c Chain) Apply(env *graphv1.EventEnvelope) (*graphv1.EventEnvelope, error) {
	if env == nil {
		return nil, fmt.Errorf("migrate: nil envelope")
	}
	if len(c) == 0 || c.IsIdentity() {
		return env, nil
	}

	eventID, idempotencyKey, sourceID := env.GetEventId(), env.GetIdempotencyKey(), env.GetSourceId()
	out, ok := proto.Clone(env).(*graphv1.EventEnvelope)
	if !ok {
		return nil, fmt.Errorf("migrate: clone %s: unexpected message type", eventID)
	}
	for _, t := range c {
		next, err := t.Apply(out)
		if err != nil {
			return nil, fmt.Errorf("migrate: %s → %s on event %s: %w", t.From(), t.To(), eventID, err)
		}
		if next == nil {
			return nil, fmt.Errorf("migrate: %s → %s on event %s returned no envelope", t.From(), t.To(), eventID)
		}
		out = next
	}

	// Identity is not a transformer's to give away (FR-020, constitution III).
	if out.GetEventId() != eventID || out.GetIdempotencyKey() != idempotencyKey || out.GetSourceId() != sourceID {
		return nil, fmt.Errorf(
			"migrate: chain %s changed the identity of event %s; a transformation may rewrite a payload, never an event id, idempotency key or source",
			c.Steps(), eventID)
	}
	out.SchemaVersion = c.To()
	return out, nil
}

// Registry holds the known transformations and resolves chains between versions. The zero value
// is usable and empty; it is safe for concurrent use.
type Registry struct {
	mu sync.RWMutex
	// byFrom maps a source version onto the single transformer that reads it. One step per
	// version, deliberately: a registry with two ways out of 1.0.0 would make the chain — and
	// therefore the replayed graph — depend on map iteration order.
	byFrom map[string]Transformer
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{byFrom: map[string]Transformer{}} }

// Register adds one transformation. Registering a second transformer out of the same version,
// or one whose From equals its To for a version other than an identity, is a programming error
// and returns an error rather than silently winning.
func (r *Registry) Register(t Transformer) error {
	if t == nil {
		return fmt.Errorf("migrate: nil transformer")
	}
	from, to := t.From(), t.To()
	if from == "" || to == "" {
		return fmt.Errorf("migrate: transformer must declare both From and To (got %q → %q)", from, to)
	}
	if from == to {
		// An identity is registered as "this version is terminal", which is how the chain
		// resolver knows it has arrived. It needs no entry.
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byFrom == nil {
		r.byFrom = map[string]Transformer{}
	}
	if existing, ok := r.byFrom[from]; ok {
		return fmt.Errorf("migrate: %s already transforms to %s; a version may have only one successor",
			from, existing.To())
	}
	r.byFrom[from] = t
	return nil
}

// MustRegister is Register for a package init, where a duplicate is a build-time defect.
func (r *Registry) MustRegister(t Transformer) {
	if err := r.Register(t); err != nil {
		panic(err)
	}
}

// Versions lists every version the registry can transform out of, sorted.
func (r *Registry) Versions() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.byFrom))
	for from := range r.byFrom {
		out = append(out, from)
	}
	sort.Strings(out)
	return out
}

// Chain resolves the transformation from one version to another.
//
// from == to is the identity chain. Otherwise the registry is walked one successor at a time
// until it arrives at to; a version with no successor, or a cycle, is an error naming what was
// reachable, because "which versions can this build read?" is the question a feeder author and
// an operator both ask.
func (r *Registry) Chain(from, to string) (Chain, error) {
	if from == to {
		return nil, nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()

	var (
		chain Chain
		seen  = map[string]bool{from: true}
		at    = from
	)
	for at != to {
		next, ok := r.byFrom[at]
		if !ok {
			return nil, fmt.Errorf("migrate: no transformation from %s to %s (stuck at %s; this build can transform out of: %s)",
				from, to, at, joinOrNone(versionsLocked(r.byFrom)))
		}
		if seen[next.To()] {
			return nil, fmt.Errorf("migrate: transformation cycle from %s through %s", from, next.To())
		}
		seen[next.To()] = true
		chain = append(chain, next)
		at = next.To()
	}
	return chain, nil
}

// CanReach reports whether Chain(from, to) would succeed.
func (r *Registry) CanReach(from, to string) bool {
	_, err := r.Chain(from, to)
	return err == nil
}

// ChainToAny resolves a chain from `from` onto whichever of the accepted versions it can reach,
// preferring the newest.
//
// This is the form the append path wants: it holds a set of accepted versions, not one target,
// and an envelope that is already at an accepted version must not be transformed at all.
func (r *Registry) ChainToAny(from string, accepted []string) (Chain, error) {
	if slices.Contains(accepted, from) {
		return nil, nil
	}
	targets := slices.Clone(accepted)
	sort.Sort(sort.Reverse(byVersion(targets)))
	var lastErr error
	for _, target := range targets {
		chain, err := r.Chain(from, target)
		if err == nil {
			return chain, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("migrate: no accepted versions configured")
	}
	return nil, lastErr
}

func versionsLocked(byFrom map[string]Transformer) []string {
	out := make([]string, 0, len(byFrom))
	for from := range byFrom {
		out = append(out, from)
	}
	sort.Strings(out)
	return out
}

func joinOrNone(versions []string) string {
	if len(versions) == 0 {
		return "none"
	}
	return strings.Join(versions, ", ")
}

// byVersion sorts semantic versions numerically, so 1.10.0 is newer than 1.9.0. A version that
// does not parse sorts last, which keeps an unexpected string from being preferred.
type byVersion []string

func (v byVersion) Len() int      { return len(v) }
func (v byVersion) Swap(i, j int) { v[i], v[j] = v[j], v[i] }
func (v byVersion) Less(i, j int) bool {
	a, aok := parseVersion(v[i])
	b, bok := parseVersion(v[j])
	switch {
	case aok && !bok:
		return false
	case !aok && bok:
		return true
	case !aok && !bok:
		return v[i] < v[j]
	}
	for k := range 3 {
		if a[k] != b[k] {
			return a[k] < b[k]
		}
	}
	return v[i] < v[j]
}

func parseVersion(s string) ([3]int, bool) {
	var out [3]int
	parts := strings.SplitN(s, ".", 3)
	if len(parts) != 3 {
		return out, false
	}
	for i, part := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// ---------------------------------------------------------------------------
// The default registry
// ---------------------------------------------------------------------------

// CurrentVersion is the event schema version this build speaks. It is the same string as
// pkg/feeder.SchemaVersion and log.DefaultSchemaVersions; it is repeated here rather than
// imported so that this package stays importable from the log without a cycle.
const CurrentVersion = "1.0.0"

// defaultRegistry is the process-wide registry the log consults. A future migration registers
// itself here from an init(), beside the schema change that made it necessary.
var defaultRegistry = func() *Registry {
	r := NewRegistry()
	// The no-op 1.0.0 → 1.0.0 transformation: the graph speaks exactly one version today, so
	// the only chain that exists is the empty one. Registering it is documentation and a test
	// point, not behaviour — Register treats an identity as terminal and stores nothing.
	r.MustRegister(Identity(CurrentVersion))
	return r
}()

// Default is the registry the append path uses.
func Default() *Registry { return defaultRegistry }
