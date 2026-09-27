// SPDX-License-Identifier: Apache-2.0

package datadog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// Monitors become ALERT nodes (T052; data-model.md §2).
//
// A monitor definition is asserted on change only. Its event id carries Datadog's `modified` instant
// and a digest of what is asserted, so an unchanged monitor re-polled every 20 seconds re-sends one id
// the graph answers DUPLICATE_NOOP, a restarted feeder does the same, and an edited monitor gets an id
// of its own dated from the edit. The notification message is never read into the graph (FR-074): it
// is free text written for people, carries @-mentions and links, and is not a fact about the system.

// monitorJSON is as much of a Datadog monitor as the feeder reads.
type monitorJSON struct {
	ID           int64    `json:"id"`
	Name         string   `json:"name"`
	Type         string   `json:"type"`
	Query        string   `json:"query"`
	Tags         []string `json:"tags"`
	Priority     *int     `json:"priority"`
	OverallState string   `json:"overall_state"`
	Created      string   `json:"created"`
	Modified     string   `json:"modified"`
	State        struct {
		Groups map[string]groupJSON `json:"groups"`
	} `json:"state"`
}

// groupJSON is one group's state as `group_states=all` reports it. The instants are epoch seconds.
type groupJSON struct {
	Status          string `json:"status"`
	LastTriggeredTS int64  `json:"last_triggered_ts"`
	LastResolvedTS  int64  `json:"last_resolved_ts"`
	LastNoDataTS    int64  `json:"last_nodata_ts"`
}

// monitorObservation is one monitor as a poll saw it.
type monitorObservation struct {
	ID       string
	Name     string
	Type     string
	Query    string
	Tags     []string
	Priority int // 0 when unset
	Created  time.Time
	Modified time.Time
	Groups   map[string]groupJSON
}

// Ref is the monitor's ref: `datadog.monitor=<id>`.
func (m monitorObservation) Ref() *graphv1.Ref { return feeder.Ref(NSMonitor, m.ID) }

// Grouped reports whether the monitor alerts per group. Datadog reports an ungrouped monitor's one
// state under the group `*`.
func (m monitorObservation) Grouped() bool {
	if len(m.Groups) == 0 {
		return false
	}
	if len(m.Groups) == 1 {
		_, star := m.Groups["*"]
		return !star
	}
	return true
}

// entity is the alerting entity of one group: the monitor itself when ungrouped, `<id>#<group>`
// otherwise (FR-022). The monitor is never reported alerting because one of its groups is.
func (m monitorObservation) entity(group string) (value, groupKey string) {
	if !m.Grouped() || group == "*" {
		return m.ID, ""
	}
	return m.ID + "#" + group, group
}

func decodeMonitor(raw monitorJSON) (monitorObservation, error) {
	if raw.ID <= 0 {
		return monitorObservation{}, fmt.Errorf("datadog: a monitor with no id (%q)", raw.Name)
	}
	obs := monitorObservation{
		ID: strconv.FormatInt(raw.ID, 10), Name: raw.Name, Type: raw.Type, Query: raw.Query,
		Tags: append([]string(nil), raw.Tags...), Groups: raw.State.Groups,
	}
	sort.Strings(obs.Tags)
	if raw.Priority != nil {
		obs.Priority = *raw.Priority
	}
	var err error
	if obs.Created, err = parseInstant(raw.Created); err != nil {
		return monitorObservation{}, fmt.Errorf("datadog: monitor %s created: %w", obs.ID, err)
	}
	if obs.Modified, err = parseInstant(raw.Modified); err != nil {
		return monitorObservation{}, fmt.Errorf("datadog: monitor %s modified: %w", obs.ID, err)
	}
	return obs, nil
}

func parseInstant(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

// inScope applies the tag filter: every configured tag must be on the monitor.
func (f *Feeder) inScope(m monitorObservation) bool {
	for _, want := range f.opts.MonitorTags {
		if !containsString(m.Tags, want) {
			return false
		}
	}
	return true
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// applyMonitors maps one page of the monitor list.
func (f *Feeder) applyMonitors(ctx context.Context, em feeder.Emitter, raw []byte, at time.Time) error {
	var page []monitorJSON
	if err := json.Unmarshal(raw, &page); err != nil {
		return fmt.Errorf("datadog: decoding a %s payload: %w", PayloadMonitors, err)
	}
	for _, item := range page {
		obs, err := decodeMonitor(item)
		if err != nil {
			return err
		}
		if !f.inScope(obs) {
			f.mu.Lock()
			f.outOfScope++
			f.mu.Unlock()
			continue
		}
		f.mu.Lock()
		f.cycle[obs.ID] = obs
		f.mu.Unlock()
		if err := f.emitMonitor(ctx, em, obs, at); err != nil {
			return err
		}
		if err := f.emitTransitions(ctx, em, obs, at); err != nil {
			return err
		}
	}
	return nil
}

// emitMonitor asserts the monitor's ALERT node.
func (f *Feeder) emitMonitor(ctx context.Context, em feeder.Emitter, m monitorObservation, at time.Time) error {
	props := feeder.NewProps().
		Str("sre.datadog.monitor.type", m.Type).
		Bool("sre.datadog.monitor.grouped", m.Grouped())
	if m.Priority > 0 {
		props.Str("sre.alert.severity", fmt.Sprintf("P%d", m.Priority))
	}
	if env, service := serviceOf(m.Query, m.Tags, ""); service != "" {
		props.Str(feeder.AttrServiceName, service)
		if env != "" {
			props.Str(feeder.AttrDeploymentEnvironment, env)
		}
	}
	built, err := props.Build()
	if err != nil {
		return err
	}
	fact := feeder.NodeFact{
		Meta: feeder.Meta{SourceObservedAt: at}, Ref: m.Ref(), Type: graphv1.NodeType_ALERT,
		DisplayName: m.Name, Props: built, Pointers: f.monitorPointers(m),
	}
	switch {
	case !m.Modified.IsZero():
		fact.ValidAt = m.Modified
	case !m.Created.IsZero():
		fact.ValidAt = m.Created
	default:
		fact.ValidFromUnknown = true
	}
	digest, err := factDigest(fact)
	if err != nil {
		return err
	}
	stamp := "unstated"
	if !fact.ValidAt.IsZero() {
		stamp = fact.ValidAt.Format(time.RFC3339)
	}
	return emit(ctx, em, feeder.UpsertNode(f.desc, feeder.NewID(f.desc.SourceID, "monitor", m.ID+"@"+stamp+"@"+digest), fact))
}

// monitorPointers are the monitor's query, in `datadog-monitor/v1`, and a link to it. The query
// pointer's kind is the kind of what the monitor queries; a type with no published kind gets the link
// only (pointer-vocabularies.md §2).
func (f *Feeder) monitorPointers(m monitorObservation) []*graphv1.Pointer {
	attrs := map[string]string{feeder.AttrDatadogMonitorID: m.ID}
	var out []*graphv1.Pointer
	switch kind := pointerKindOf(m.Type); kind {
	case graphv1.PointerKind_LOG, graphv1.PointerKind_METRIC:
		out = append(out, feeder.NewPointer(kind, Kind, feeder.VocabDatadogMonitor, m.Query, attrs))
	}
	if f.opts.Site != "" {
		out = append(out, feeder.SourceLinkPointer(Kind, feeder.VocabURL,
			"https://app."+f.opts.Site+"/monitors/"+m.ID, attrs))
	}
	return out
}

// pointerKindOf maps a monitor type to the kind of what it queries.
func pointerKindOf(monitorType string) graphv1.PointerKind {
	switch monitorType {
	case "log alert":
		return graphv1.PointerKind_LOG
	case "metric alert", "query alert", "service check":
		return graphv1.PointerKind_METRIC
	default:
		return graphv1.PointerKind_POINTER_KIND_UNSPECIFIED
	}
}

// factDigest is a short digest of what a node assertion states, so a change in it — a rename, a new
// query, a new pointer — is a new event id even when Datadog's `modified` did not move.
func factDigest(fact feeder.NodeFact) (string, error) {
	node := feeder.UpsertNode(feeder.Description{}, "", fact).GetUpsertNode()
	node.ValidAt = nil
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(node)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:6]), nil
}

// ---- watched entities (T055) --------------------------------------------------------------------

// scopeTerm matches `service:<v>` and `env:<v>` wherever a monitor query or tag list states them: in a
// log query string, inside a metric query's `{…}` scope, or as a monitor tag.
var scopeTerm = regexp.MustCompile(`(?:^|[\s{,("])(service|env):([A-Za-z0-9_.\-/]+)`)

// serviceOf reads the service and environment a monitor watches. The group's own values win — a
// monitor grouped by service watches a different one per group — then the query, then the tags.
// Either may come back empty: the query does not always name them.
func serviceOf(query string, tags []string, group string) (env, service string) {
	for _, source := range append([]string{group, query}, tags...) {
		for _, m := range scopeTerm.FindAllStringSubmatch(source, -1) {
			switch {
			case m[1] == "service" && service == "":
				service = m[2]
			case m[1] == "env" && env == "":
				env = m[2]
			}
		}
	}
	return env, service
}

// watches resolves what one group watches to the log-source ref this connector asserts for a watched
// service, `datadog.service=<env>/<service>`. The ref is emitted whether or not that node exists yet:
// the graph keeps a WATCHES edge to an unknown entity unattached and attaches it when the entity
// arrives (FR-019, ADR-0009 item 5). Without both a service and an environment nothing is resolved —
// a service name alone is unique only within one environment — and the checkpoint says so.
func (f *Feeder) watches(m monitorObservation, group string) []*graphv1.Ref {
	env, service := serviceOf(m.Query, m.Tags, group)
	if env == "" || service == "" || strings.Contains(env, "/") || strings.Contains(service, "/") {
		f.mu.Lock()
		f.unresolved = append(f.unresolved, fmt.Sprintf("monitor %s group %q: service %q, env %q",
			m.ID, orUnset(group), service, env))
		f.mu.Unlock()
		return nil
	}
	return []*graphv1.Ref{LogSource{Env: env, Service: service}.Ref()}
}
