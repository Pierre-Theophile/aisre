// SPDX-License-Identifier: Apache-2.0

// Package catalog is the service catalog's logic (feature 009): what a signal's names are, the
// published decision model that attributes it to catalog entries, the attributor that runs it in
// `serve`, and the catalog file import. It is neither a connector nor resolution: it reads what both
// produce and decides something different (plan §Structure Decision).
package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/Pierre-Theophile/aisre/internal/graph"
)

// Signal kinds (research R3).
const (
	KindAlert    = "alert"
	KindDeclared = "declared"
	KindDocument = "document"
	KindChange   = "change"
)

// AttrEnvironment is the claim attribute a name's environment is read from (005, rule C9).
const AttrEnvironment = "deployment.environment.name"

// Name is one name a signal goes by: a ref, and the environment it was stated in, if any.
type Name struct {
	Ref         graph.Ref
	Environment string
}

func (n Name) String() string {
	if n.Environment == "" {
		return n.Ref.String()
	}
	return n.Ref.String() + "@" + n.Environment
}

// Signal is what the decision model reads about one alert, declaration, document or change.
type Signal struct {
	// EntityID is the signal's canonical entity id.
	EntityID string
	// Ref is a ref that resolves to it, for the event the attributor appends.
	Ref graph.Ref
	// Kind is one of the four kinds.
	Kind string
	// Names are what the signal goes by, sorted and deduplicated.
	Names []Name
	// Text is the signal's title, for the optional classifier (PR 3) and nothing else.
	Text string
	// Related are the names of the entities a watched or target entity runs on, depends on or
	// calls (rule P3), sorted and deduplicated.
	Related []Name
	// Owners are the entity ids a watched or target entity is owned by (rule P4), sorted.
	Owners []string
}

// signalKinds are the node types a signal is.
var signalKinds = map[graph.NodeType]string{
	graph.NodeTypeAlert:        KindAlert,
	graph.NodeTypeKnowledgeDoc: KindDocument,
	graph.NodeTypeChange:       KindChange,
}

// IsSignalType reports whether a node type is one the attributor attributes.
func IsSignalType(t graph.NodeType) bool {
	_, ok := signalKinds[t]
	return ok
}

// Names reads signals out of the graph as it is now (research R3). In `serve` "now" is the
// signal's observed instant, which is what the decision is about; the log records the decision, so
// nothing here runs during a replay.
type Names struct {
	store Store
}

// NewNames reads through store.
func NewNames(store Store) *Names { return &Names{store: store} }

// ErrNotSignal is returned for an entity that is not an alert, a document or a change.
var ErrNotSignal = errors.New("catalog: not a signal")

// Signal reads one signal by entity id, following merges.
func (n *Names) Signal(ctx context.Context, entityID string) (Signal, error) {
	canonical, typ, ref, found, err := n.store.ResolveEntity(ctx, entityID)
	switch {
	case err != nil:
		return Signal{}, err
	case !found:
		return Signal{}, fmt.Errorf("catalog: entity %s: %w", entityID, ErrNotSignal)
	}
	kind, ok := signalKinds[typ]
	if !ok {
		return Signal{}, fmt.Errorf("catalog: entity %s is a %s: %w", entityID, typ, ErrNotSignal)
	}
	signal := Signal{EntityID: canonical, Ref: ref, Kind: kind}

	display, props, err := n.store.LatestVersion(ctx, canonical)
	if err != nil {
		return Signal{}, err
	}
	signal.Text = display
	values := propValues(props)

	var names []Name
	switch kind {
	case KindAlert:
		watched, err := n.store.Targets(ctx, canonical, graph.EdgeTypeWatches, false)
		if err != nil {
			return Signal{}, err
		}
		names = append(names, watched...)
		if signal.Related, signal.Owners, err = n.neighbourhood(ctx, canonical, graph.EdgeTypeWatches, false); err != nil {
			return Signal{}, err
		}
		for _, spelled := range stringList(values[graph.AlertUnattachedWatchesProp]) {
			if ref, err := graph.ParseRef(spelled); err == nil {
				names = append(names, Name{Ref: ref})
			}
		}
		if s, _ := values[graph.AlertTransportProp].(string); s == "human_declared" {
			signal.Kind = KindDeclared
			for _, token := range SlugTokens(display) {
				names = append(names, Name{Ref: graph.Ref{Namespace: graph.SlackSlugNamespace, Value: token}})
			}
		}
	case KindDocument:
		database, _ := values[graph.NotionDatabaseProp].(string)
		for _, value := range stringList(values[graph.NotionValuesProp]) {
			if database != "" && value != "" {
				names = append(names, Name{Ref: NotionValueRef(database, value)})
			}
		}
	case KindChange:
		targets, err := n.store.Targets(ctx, canonical, graph.EdgeTypeChangedBy, true)
		if err != nil {
			return Signal{}, err
		}
		names = append(names, targets...)
		if signal.Related, signal.Owners, err = n.neighbourhood(ctx, canonical, graph.EdgeTypeChangedBy, true); err != nil {
			return Signal{}, err
		}
	}
	signal.Names = sortNames(names)
	return signal, nil
}

// neighbourhood reads what the probable rules P3 and P4 look at around a signal's watched or target
// entities, sorted and deduplicated.
func (n *Names) neighbourhood(ctx context.Context, signalID string, typ graph.EdgeType, inbound bool) ([]Name, []string, error) {
	related, owners, err := n.store.Neighbourhood(ctx, signalID, typ, inbound)
	if err != nil {
		return nil, nil, err
	}
	return sortNames(related), sortedUnique(owners), nil
}

// NotionValueRef names a Notion page by one value of its entity property (ADR-0015 item 7).
func NotionValueRef(database, value string) graph.Ref {
	return graph.Ref{Namespace: graph.NotionValueNamespace, Value: database + "/" + value}
}

// declaredPrefix is the severity-and-date head of a declared incident's channel name, which names
// no service (006 `slack/sev-channel@1`).
var declaredPrefix = regexp.MustCompile(`^sev[0-9]-[0-9]{4}-[0-9]{2}-[0-9]{2}-?`)

// SlugTokens are the tokens of a declared incident's channel slug, as 006 reads them.
func SlugTokens(channel string) []string {
	slug := declaredPrefix.ReplaceAllString(strings.ToLower(strings.TrimSpace(channel)), "")
	tokens := strings.FieldsFunc(slug, func(r rune) bool { return r == '-' || r == '_' })
	slices.Sort(tokens)
	return slices.Compact(tokens)
}

func sortNames(names []Name) []Name {
	slices.SortFunc(names, func(a, b Name) int { return strings.Compare(a.String(), b.String()) })
	return slices.CompactFunc(names, func(a, b Name) bool { return a == b })
}

// propValues decodes a props column into its first value per key, provenance stripped.
func propValues(raw []byte) map[string]any {
	out := map[string]any{}
	if len(raw) == 0 {
		return out
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return out
	}
	type record struct {
		Value any `json:"value"`
	}
	for key, value := range fields {
		var many []record
		if err := json.Unmarshal(value, &many); err == nil && len(many) > 0 {
			out[key] = many[0].Value
			continue
		}
		var one record
		if err := json.Unmarshal(value, &one); err == nil {
			out[key] = one.Value
		}
	}
	return out
}

func stringList(v any) []string {
	items, _ := v.([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
