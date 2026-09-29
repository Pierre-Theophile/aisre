// SPDX-License-Identifier: Apache-2.0

package datadog

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/versionstamp"
)

// Version-stamp discovery and the log pointer (T063–T065; contract §2, version-stamping.md §3,
// data-model.md §3 and §5).
//
// Every discovery tick asserts each watched log source — its SERVICE node and log-service correlation —
// whatever the graph holds (FR-040f). When the tick carries the source's measurements, the node also
// carries the `datadog-logs/v1` pointer and the discovery verdict:
//
//   - the measurements are counts the live poller read over the discovery window: all lines, error
//     lines, lines carrying a host, and, per convention, lines and error lines carrying it. They are
//     counts of where a field is present, never of what happened — no log line and no error rate
//     reaches the graph;
//   - the verdict is versionstamp.Decide over them, the rule every backend's pointer is judged by;
//   - the pointer's `version` join key is the accepted candidate, spelled as the selector grammar
//     spells it (`version` for the tag, `@version` for the attribute), and absent when none qualified.
//
// The Pointer message has no field for a verdict, so the verdict is carried by the node that carries
// the pointer, as properties versioned with it in valid time (FR-038). Only the verdict's CLASS is a
// property — which candidate was accepted, and why each other one was not — never its shares: the
// shares move every window, and a node re-asserted every hour because 99.97 % became 99.98 % would
// make the pointer's history unreadable. The shares are in the checkpoint.

// Properties the verdict is carried in.
const (
	PropVersionAttribute   = "sre.version_stamp.attribute"
	PropVersionSource      = "sre.version_stamp.source"
	PropVersionVerdict     = "sre.version_stamp.verdict"
	PropVersionConventions = "sre.version_stamp.conventions_version"
)

// SourceMeasurement is what the poller measured for one log source over the discovery window.
type SourceMeasurement struct {
	// Source is `<env>/<service>`.
	Source     string `json:"source"`
	Lines      int64  `json:"lines"`
	ErrorLines int64  `json:"error_lines"`
	HostLines  int64  `json:"host_lines"`
	// Candidates are the conventions measured, by label, in the published order.
	Candidates []CandidateCount `json:"candidates,omitempty"`
	// Values are the stamp's values first seen in this interval, with the instant of their first indexed
	// line (rollouts.go). Only values new to the poller are listed; a restarted poller lists them again,
	// and they re-derive the ids already sent.
	Values []ValueSighting `json:"values,omitempty"`
	// Tags are the allowlisted tag values on the source's lines (tags.go).
	Tags []TagCount `json:"tags,omitempty"`
	// ValuesFailed says the values could not be listed and why: this interval's rollouts are then
	// missing, and the checkpoint says so. The next interval looks again.
	ValuesFailed string `json:"values_failed,omitempty"`
	// EnvField is the field the environment was read from: `env` (the tag), an attribute such as
	// `@env`, or empty when none of the published fields carries it (envfield.go).
	EnvField string `json:"env_field,omitempty"`
	// EnvDiscovery is the environment-field discovery, when this tick made it.
	EnvDiscovery *EnvDiscovery `json:"env_discovery,omitempty"`
	// Failed says the measurement could not be completed and why; the source is then asserted without
	// a pointer change, and the checkpoint says so.
	Failed string `json:"failed,omitempty"`
}

// CandidateCount is one convention's presence.
type CandidateCount struct {
	Label      string `json:"label"`
	Lines      int64  `json:"lines"`
	ErrorLines int64  `json:"error_lines"`
}

// ValueSighting is one stamp value and its first indexed line.
type ValueSighting struct {
	Value     string    `json:"value"`
	FirstSeen time.Time `json:"first_seen"`
	// BeyondHorizon says the value has lines older than the first-seen horizon: it was deployed before
	// the connector could see it, and no change is inferred (contract §4).
	BeyondHorizon bool `json:"beyond_horizon,omitempty"`
}

// DiscoveryWindow is the window the measurements were taken over.
type DiscoveryWindow struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// OverrideCandidate reads an operator's per-service override: `@name` is an attribute, `name` a tag.
func OverrideCandidate(spec string) (versionstamp.Candidate, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" || strings.ContainsAny(spec, " :*\"") {
		return versionstamp.Candidate{}, fmt.Errorf("datadog: version override %q is not a tag or @attribute name", spec)
	}
	if strings.HasPrefix(spec, "@") {
		return versionstamp.Candidate{Name: strings.TrimPrefix(spec, "@"), Form: versionstamp.FormAttribute, SetBy: "operator override"}, nil
	}
	return versionstamp.Candidate{Name: spec, Form: versionstamp.FormTag, SetBy: "operator override"}, nil
}

// CandidatesFor are the candidates measured for a source: the override alone when there is one, the
// published list otherwise.
func CandidatesFor(override string) ([]versionstamp.Candidate, error) {
	if override == "" {
		return versionstamp.Conventions, nil
	}
	c, err := OverrideCandidate(override)
	if err != nil {
		return nil, err
	}
	return []versionstamp.Candidate{c}, nil
}

// FacetOf spells a candidate as the selector grammar and the backend's group-by spell it.
func FacetOf(c versionstamp.Candidate) string {
	switch c.Form {
	case versionstamp.FormTag:
		return c.Name
	case versionstamp.FormAttributePair:
		// The backend groups by one facet. The digest is the half that identifies an image; the name
		// alone is a mutable repository. A group then reads BARE_DIGEST until the backend groups by the
		// pair (version-stamping.md §4).
		return "@" + c.Pair
	default:
		return "@" + c.Name
	}
}

// DecideVerdict decides a source's verdict from its measurement: the rule the feeder asserts and the
// live poller uses to know which field's values to look for.
func DecideVerdict(m SourceMeasurement, override string, th versionstamp.Thresholds, window time.Duration) (versionstamp.Verdict, error) {
	source := versionstamp.SourceDiscovered
	if override != "" {
		source = versionstamp.SourceOperator
	}
	candidates, err := CandidatesFor(override)
	if err != nil {
		return versionstamp.Verdict{}, err
	}
	counted := map[string]CandidateCount{}
	for _, c := range m.Candidates {
		counted[c.Label] = c
	}
	measured := make([]versionstamp.Measurement, 0, len(candidates))
	for _, c := range candidates {
		n := counted[c.Label()]
		measured = append(measured, versionstamp.Measurement{Candidate: c, Lines: n.Lines, ErrorLines: n.ErrorLines})
	}
	if th == (versionstamp.Thresholds{}) {
		th = versionstamp.DefaultThresholds()
	}
	return versionstamp.Decide(measured, versionstamp.Totals{Lines: m.Lines, ErrorLines: m.ErrorLines}, th, window, source), nil
}

// verdictOf decides a source's verdict with this feeder's overrides and thresholds.
func (f *Feeder) verdictOf(src LogSource, m SourceMeasurement, window time.Duration) (versionstamp.Verdict, error) {
	return DecideVerdict(m, f.opts.VersionOverrides[src.Key()], f.opts.Thresholds, window)
}

// verdictClass is the verdict without its shares: what the node carries.
func verdictClass(v versionstamp.Verdict) string {
	parts := make([]string, 0, len(v.Candidates))
	for _, c := range v.Candidates {
		class := "accepted"
		switch {
		case c.Accepted:
		case strings.HasPrefix(c.Reason, "not needed"):
			class = "not needed"
		case strings.Contains(c.Reason, "wrote no lines"):
			class = "rejected: no lines in the window"
		case strings.Contains(c.Reason, "error-line share"):
			class = "rejected: below the error-line share"
		default:
			class = "rejected: below the line share"
		}
		parts = append(parts, c.Label+" "+class)
	}
	return strings.Join(parts, "; ")
}

// logPointer mints the source's pointer (data-model.md §5).
func logPointer(src LogSource, v versionstamp.Verdict, hosts bool) *graphv1.Pointer {
	selector := src.Query()
	if src.Index != "" {
		selector += " index:" + src.Index
	}
	attrs := map[string]string{feeder.AttrServiceName: src.Service}
	if src.Env != "" {
		attrs[feeder.AttrDeploymentEnvironment] = src.Env
	}
	p := feeder.LogPointer(Kind, selector, attrs)
	p.Vocabulary = feeder.VocabDatadogLogs
	keys := map[string]string{}
	if v.Stamped() {
		keys["version"] = FacetOf(v.Accepted)
	}
	if hosts {
		keys["host"] = "host"
	}
	if len(keys) > 0 {
		p = feeder.WithJoinKeys(p, keys)
	}
	return p
}

// emitMeasuredSource asserts a source's node with its pointer and verdict. The event id is the
// source's ref and a digest of what is asserted, so an unchanged verdict is an unchanged id and a
// no-op, and a changed one is a new version dated from the tick that measured it.
func (f *Feeder) emitMeasuredSource(ctx context.Context, em feeder.Emitter, src LogSource, m SourceMeasurement, window DiscoveryWindow, at time.Time) error {
	v, err := f.verdictOf(src, m, window.To.Sub(window.From))
	if err != nil {
		return err
	}
	hosts := m.Lines > 0 && float64(m.HostLines)/float64(m.Lines) >= f.opts.Thresholds.LineShare
	props := feeder.NewProps().Str(feeder.AttrServiceName, src.Service)
	if src.Env != "" {
		props.Str(feeder.AttrDeploymentEnvironment, src.Env)
	}
	props.
		Str(PropVersionSource, string(v.Source)).
		Str(PropVersionVerdict, verdictClass(v)).
		Str(PropVersionConventions, v.ConventionsVersion)
	if v.Stamped() {
		props.Str(PropVersionAttribute, v.Attribute)
	}
	built, err := props.Build()
	if err != nil {
		return err
	}
	fact := feeder.NodeFact{
		Meta: feeder.Meta{SourceObservedAt: at}, Ref: src.Ref(), Type: graphv1.NodeType_SERVICE,
		DisplayName: src.Service, Props: built, Pointers: []*graphv1.Pointer{logPointer(src, v, hosts)},
	}
	digest, err := factDigest(fact)
	if err != nil {
		return err
	}
	key := src.Ref().GetValue()
	f.mu.Lock()
	previous, seen := f.verdicts[key]
	f.verdicts[key] = digest
	f.discoveryNotes = append(f.discoveryNotes, fmt.Sprintf("%s over [%s, %s): %s", key,
		window.From.Format(time.RFC3339), window.To.Format(time.RFC3339), v))
	f.mu.Unlock()
	if !seen || previous == digest {
		// The first assertion this run makes: the verdict held before the window as far as anything
		// here can say, so its start is unknown. An unchanged verdict re-sends that id.
		fact.ValidFromUnknown = true
	} else {
		fact.ValidAt = at
	}
	id := feeder.NewID(f.desc.SourceID, "service", key+"@"+digest)
	if err := emit(ctx, em, feeder.UpsertNode(f.desc, id, fact)); err != nil {
		return err
	}
	if f.opts.Capabilities.Enabled(CapTags) {
		if err := f.emitTags(ctx, em, src, m, at); err != nil {
			return err
		}
	}
	return f.emitRollouts(ctx, em, src, v, m, at)
}

// sourceMeasurements indexes a tick's measurements by source.
func (f *Feeder) sourceMeasurements(tick DiscoveryTick) map[string]SourceMeasurement {
	out := map[string]SourceMeasurement{}
	for _, m := range tick.Measurements {
		out[m.Source] = m
	}
	return out
}

// discoveryLines renders what the checkpoint says about discovery, then clears it.
func (f *Feeder) discoveryLines() []string {
	notes := uniqueSorted(f.discoveryNotes)
	f.discoveryNotes = nil
	sort.Strings(notes)
	return notes
}
