// SPDX-License-Identifier: Apache-2.0

package datadog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// Datadog's event stream as a change source (T092; spec section D, FR-027–FR-033b).
//
// Changes come from the platforms themselves; this is for an organisation that also posts deployment,
// configuration or infrastructure events into Datadog. Where it does, those events are a SECOND
// observer of a rollout the platform feeders saw, and the whole point is that the pair collapses into
// one change — done by the projector, on the `deploy.*` keys below, never by this connector (FR-032).
//
//   - an event becomes one CHANGE, ref `datadog.change=<event id>`, valid at the instant Datadog
//     recorded (`timestamp`), never the poll's. A kind the taxonomy does not name is emitted as "other"
//     with the vendor's own kind kept (FR-028), and an event that names no kind is "other" as well;
//   - its targets are what the event's tags name: the service (in the environment the tag states) and,
//     where both tags are present, the Kubernetes deployment. An event that names none is kept and
//     counted unattached (FR-030), and one naming a target the graph has not seen is kept by the graph
//     and attaches when the target appears;
//   - a ROLLOUT carries the identifiers it states as correlation keys with the environment, in the same
//     published namespaces the deploy feeders use: a full commit under `deploy.commit_sha`, a
//     digest-pinned image under `deploy.image`, a release under `deploy.release`. Only a rollout: a
//     configuration change quoting a commit did not ship it, and C8 would merge it with the rollout that
//     did (the reasoning the Kubernetes feeder gives for minting a commit only when it changed);
//   - its actor kind is derived from what Datadog states — the event's trigger tag and its source — and
//     is UNKNOWN, with the evidence there was, when neither classifies it (FR-033b). The actor's name
//     never decides it.
//
// The scope (which sources and tags are changes) is applied by the poller that reads the events, so a
// payload holds only in-scope events and a recording of it replays without the configuration
// (FR-029). The feeder trusts the payload and states the configuration in force in its checkpoint.

// The payload kinds of the `changes` capability.
const (
	// PayloadEvents is one page of `GET /api/v2/events`, restricted by the poller to the events in
	// scope: a JSON object whose `data` array holds the events.
	PayloadEvents = "events"
	// PayloadEventsPoll declares an events poll's outcome. It is its own payload, not the monitor
	// poll's, because a monitor poll's completeness retracts the monitors it did not list, and an
	// events window retracts nothing: an event is a fact that happened.
	PayloadEventsPoll = "events-poll"
)

// The events cadence and window (FR-081: the budget decides what is affordable, this is the ask).
const (
	// DefaultEventsInterval is how often the events are read. Longer than the monitors' poll: a change
	// is a fact to explain an incident with, not a state to page on.
	DefaultEventsInterval = 5 * time.Minute
	// DefaultEventsOverlap is how far before the last complete window a window reaches. Datadog indexes
	// an event some time after it happened; an overlap re-reads what may have been late, and the ids are
	// Datadog's own so a re-read is a no-op.
	DefaultEventsOverlap = 5 * time.Minute
	// DefaultEventsPageSize is the events per page (the API allows up to 1000).
	DefaultEventsPageSize = 100
	// MaxEventsPages bounds one window: a window that needs more is partial, and says so.
	MaxEventsPages = 50
)

// Properties on a change this section emits.
const (
	// PropEventKind is the kind the vendor stated for the event, whatever the taxonomy made of it
	// (FR-028).
	PropEventKind = "sre.datadog.event_kind"
	// PropEventSource is the event's source (`source_type_name`).
	PropEventSource = "sre.datadog.event_source"
	// PropActorEvidence is what was available to classify the actor when it could not be: a change whose
	// actor kind is UNKNOWN says why (FR-033b).
	PropActorEvidence = "sre.change.actor_evidence"
)

// ---- the API's shape, minimal and tolerant -----------------------------------------------------------

// EventPage is the body of `GET /api/v2/events`, restricted to what the connector reads. A field
// Datadog adds is ignored; the types that Datadog has been seen to vary (a number where a string is
// documented, a timestamp as text or as epoch milliseconds) are read whichever it sends, so one odd
// event does not cost a page. NOTE: built from the published response shape; not yet verified against a
// live organisation (docs/connectors/datadog.md §5).
type EventPage struct {
	Data []EventJSON `json:"data"`
	// Meta is absent from a payload the poller pushed: the paging is the poller's, not the feeder's.
	Meta *EventsMeta `json:"meta,omitempty"`
}

// EventsMeta is the part of a page that says whether the answer is complete and where it continues.
type EventsMeta struct {
	Page struct {
		After string `json:"after,omitempty"`
	} `json:"page"`
	// Status is "done" or "timeout"; a timeout is a partial answer.
	Status string `json:"status,omitempty"`
	// Warnings are kept whole: any warning means the answer may be partial.
	Warnings []json.RawMessage `json:"warnings,omitempty"`
}

// Partial reports whether Datadog said the page is incomplete.
func (m EventsMeta) Partial() bool { return m.Status == "timeout" || len(m.Warnings) > 0 }

// EventJSON is one event.
type EventJSON struct {
	ID         text   `json:"id"`
	Type       string `json:"type,omitempty"`
	Attributes struct {
		Timestamp  flexTime `json:"timestamp"`
		Tags       []string `json:"tags,omitempty"`
		Attributes struct {
			Title          string   `json:"title,omitempty"`
			SourceTypeName text     `json:"source_type_name,omitempty"`
			Service        text     `json:"service,omitempty"`
			Tags           []string `json:"tags,omitempty"`
			Evt            struct {
				ID   text `json:"id,omitempty"`
				Name text `json:"name,omitempty"`
				Type text `json:"type,omitempty"`
			} `json:"evt"`
		} `json:"attributes"`
	} `json:"attributes"`
}

// text reads a JSON string, or a number as its digits, and anything else as empty.
type text string

// UnmarshalJSON is tolerant by design: it never fails, because an event whose field changed type is
// still an event.
func (t *text) UnmarshalJSON(b []byte) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return nil
	}
	switch x := v.(type) {
	case string:
		*t = text(x)
	case json.Number:
		*t = text(x.String())
	}
	return nil
}

// flexTime reads an RFC 3339 instant, or epoch seconds or milliseconds; anything else is the zero time,
// which the feeder counts as undated rather than dating the event at the poll.
type flexTime struct{ time.Time }

// UnmarshalJSON is tolerant by design, like text's.
func (t *flexTime) UnmarshalJSON(b []byte) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return nil
	}
	switch x := v.(type) {
	case string:
		if parsed, err := time.Parse(time.RFC3339Nano, x); err == nil {
			t.Time = parsed.UTC()
		}
	case json.Number:
		if n, err := x.Int64(); err == nil && n > 0 {
			if n > 1e11 { // milliseconds: seconds would be the year 5138
				t.Time = time.UnixMilli(n).UTC()
			} else {
				t.Time = time.Unix(n, 0).UTC()
			}
		}
	}
	return nil
}

// MarshalJSON writes the instant back as RFC 3339, so the sanitised payload reads with the same code.
func (t flexTime) MarshalJSON() ([]byte, error) {
	if t.IsZero() {
		return []byte("null"), nil
	}
	return json.Marshal(t.UTC().Format(time.RFC3339Nano))
}

// allTags is the event's tags wherever the response put them.
func (e EventJSON) allTags() []string {
	return append(append([]string(nil), e.Attributes.Tags...), e.Attributes.Attributes.Tags...)
}

func (e EventJSON) source() string { return string(e.Attributes.Attributes.SourceTypeName) }

// tagSet is an event's tags as key → values, keys and values as Datadog states them (it lowercases).
type tagSet map[string][]string

func parseTags(tags []string) tagSet {
	out := tagSet{}
	for _, tag := range tags {
		key, value, ok := strings.Cut(tag, ":")
		if !ok || strings.TrimSpace(key) == "" || strings.TrimSpace(value) == "" {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		out[key] = append(out[key], strings.TrimSpace(value))
	}
	return out
}

// first is the first value of the first key that has one.
func (t tagSet) first(keys ...string) string {
	for _, key := range keys {
		if v := t[key]; len(v) > 0 {
			return v[0]
		}
	}
	return ""
}

// ---- the scope (FR-029) ------------------------------------------------------------------------------

// ChangeScope is which Datadog events are treated as changes: an event is in scope when its source is
// one of Sources, or it carries one of Tags. It is configuration and is recorded in every events
// checkpoint, so a later reader can tell "no change happened" from "we were not looking for that kind of
// change". Neither empty is refused: a capability that would read the whole stream and decide nothing
// is the half-enabled state FR-008b forbids.
type ChangeScope struct {
	// Sources are `source_type_name` values, compared without case.
	Sources []string
	// Tags are `key:value` tags, compared without case.
	Tags []string
}

// Validate refuses a scope that could not select anything honestly.
func (s ChangeScope) Validate() error {
	if len(s.Sources) == 0 && len(s.Tags) == 0 {
		return fmt.Errorf("datadog: the changes capability is enabled and names no event source or tag " +
			"(--change-sources, --change-tags); Datadog's stream holds monitor alerts, agent events and " +
			"whatever else is posted, and which of them are changes is the operator's to say (FR-029)")
	}
	for _, src := range s.Sources {
		if strings.TrimSpace(src) == "" || strings.ContainsAny(src, " ,()\"") {
			return fmt.Errorf("datadog: change source %q is not a single event source name", src)
		}
	}
	for _, tag := range s.Tags {
		key, value, ok := strings.Cut(tag, ":")
		if !ok || key == "" || value == "" || strings.ContainsAny(tag, " ,()\"") {
			return fmt.Errorf("datadog: change tag %q is not a single `key:value`", tag)
		}
	}
	return nil
}

// Matches reports whether the event is in scope, and by which clause.
func (s ChangeScope) Matches(ev EventJSON) (bool, string) {
	source := strings.ToLower(ev.source())
	for _, want := range s.Sources {
		if source != "" && source == strings.ToLower(want) {
			return true, "source " + want
		}
	}
	have := map[string]bool{}
	for _, tag := range ev.allTags() {
		have[strings.ToLower(strings.TrimSpace(tag))] = true
	}
	for _, want := range s.Tags {
		if have[strings.ToLower(want)] {
			return true, "tag " + want
		}
	}
	return false, ""
}

// Query is the server-side filter for the events search: the scope in Datadog's event search syntax,
// so the stream's other events are not paged through and paid for. Matches still decides; a query the
// API reads differently costs calls, never correctness. NOTE: the syntax is the published one and not
// yet verified against a live organisation.
func (s ChangeScope) Query() string {
	var terms []string
	for _, src := range s.Sources {
		terms = append(terms, "source:"+src)
	}
	for _, tag := range s.Tags {
		terms = append(terms, `tags:"`+tag+`"`)
	}
	return strings.Join(terms, " OR ")
}

// String is the spelling recorded in the checkpoint.
func (s ChangeScope) String() string {
	return fmt.Sprintf("sources [%s], tags [%s]", strings.Join(s.Sources, ", "), strings.Join(s.Tags, ", "))
}

// ---- the taxonomy (FR-028) ---------------------------------------------------------------------------

func normalise(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else if b.Len() > 0 && !strings.HasSuffix(b.String(), "_") {
			b.WriteByte('_')
		}
	}
	return strings.TrimSuffix(b.String(), "_")
}

func setOf(names ...string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out
}

// vendorKinds maps the kinds organisations post to the published change taxonomy. Anything not here is
// "other" with the vendor's kind kept.
var vendorKinds = map[graphv1.ChangeKind]map[string]bool{
	graphv1.ChangeKind_ROLLOUT:           setOf("deploy", "deployment", "deployed", "rollout", "release", "rollback"),
	graphv1.ChangeKind_CONFIG_CHANGE:     setOf("config", "config_change", "configuration", "configuration_change"),
	graphv1.ChangeKind_IAC_APPLY:         setOf("iac", "iac_apply", "terraform", "terraform_apply", "infrastructure", "infrastructure_change"),
	graphv1.ChangeKind_FLAG_FLIP:         setOf("flag", "feature_flag", "flag_flip", "flag_change"),
	graphv1.ChangeKind_SECRET_ROTATION:   setOf("secret_rotation", "rotate_secret"),
	graphv1.ChangeKind_SCALING:           setOf("scale", "scaling", "autoscale", "autoscaling"),
	graphv1.ChangeKind_MIGRATION:         setOf("migration", "db_migration", "schema_migration"),
	graphv1.ChangeKind_DNS_SWITCH:        setOf("dns", "dns_switch", "dns_change"),
	graphv1.ChangeKind_CLOUD_MAINTENANCE: setOf("maintenance", "cloud_maintenance"),
	graphv1.ChangeKind_IAM_CHANGE:        setOf("iam", "iam_change"),
	graphv1.ChangeKind_QUOTA_CHANGE:      setOf("quota", "quota_change"),
}

// KnownVendorKind reports whether the kind is one the taxonomy names, for the recording's pre-pass: a
// kind outside the published vocabulary is the organisation's own word and is pseudonymised there.
func KnownVendorKind(kind string) bool {
	n := normalise(kind)
	for _, names := range vendorKinds {
		if names[n] {
			return true
		}
	}
	return false
}

// kindTagKeys are the tags an event states its kind under, before the event's own `evt.type`.
var kindTagKeys = []string{"change_type", "event_type"}

// changeKindOf maps the vendor's kind. stated is what Datadog said, "" when it said nothing.
func changeKindOf(ev EventJSON, tags tagSet) (kind graphv1.ChangeKind, stated string, rollback bool) {
	stated = tags.first(kindTagKeys...)
	if stated == "" {
		stated = string(ev.Attributes.Attributes.Evt.Type)
	}
	n := normalise(stated)
	for k, names := range vendorKinds {
		if names[n] {
			return k, stated, n == "rollback"
		}
	}
	return graphv1.ChangeKind_CHANGE_KIND_OTHER, stated, false
}

// ---- the actor (FR-033a, FR-033b) --------------------------------------------------------------------

// The published evidence for an actor kind. Two things classify: the trigger tag an event states, and
// its source. Neither is a name: a bot called `alice` and a person called `deploy-bot` are exactly the
// case the name-based guess gets wrong, so the name is never read for the kind.
var (
	triggerTagKeys = []string{"triggered_by", "trigger"}
	// ActorNameKeys are the tags an actor's name is read from. The name is recorded and never
	// classified; a recording drops it (recording.go).
	ActorNameKeys = []string{"actor", "user", "author", "deployer", "deployed_by"}

	triggerKinds = map[string]graphv1.ActorKind{
		"user": graphv1.ActorKind_PERSON, "person": graphv1.ActorKind_PERSON,
		"human": graphv1.ActorKind_PERSON, "manual": graphv1.ActorKind_PERSON,
		"ci": graphv1.ActorKind_AUTOMATION, "cd": graphv1.ActorKind_AUTOMATION,
		"pipeline": graphv1.ActorKind_AUTOMATION, "bot": graphv1.ActorKind_AUTOMATION,
		"automation": graphv1.ActorKind_AUTOMATION, "gitops": graphv1.ActorKind_AUTOMATION,
		"autoscaler": graphv1.ActorKind_CONTROLLER, "hpa": graphv1.ActorKind_CONTROLLER,
		"scheduler": graphv1.ActorKind_CONTROLLER, "controller": graphv1.ActorKind_CONTROLLER,
		"operator": graphv1.ActorKind_CONTROLLER,
		"vendor":   graphv1.ActorKind_VENDOR, "provider": graphv1.ActorKind_VENDOR,
	}
	sourceKinds = func() map[string]graphv1.ActorKind {
		out := map[string]graphv1.ActorKind{}
		for _, s := range []string{"jenkins", "github", "gitlab", "circleci", "argocd", "argo_cd", "flux", "fluxcd",
			"spinnaker", "teamcity", "bitbucket", "buildkite", "travis_ci", "azure_devops", "harness", "ansible",
			"terraform", "terraform_cloud", "octopus_deploy", "codedeploy", "aws_codedeploy"} {
			out[s] = graphv1.ActorKind_AUTOMATION
		}
		for _, s := range []string{"kubernetes", "kubernetes_state", "k8s", "karpenter", "keda", "hpa", "vpa",
			"cluster_autoscaler", "aws_autoscaling"} {
			out[s] = graphv1.ActorKind_CONTROLLER
		}
		for _, s := range []string{"aws_health", "azure_service_health", "gcp_service_health"} {
			out[s] = graphv1.ActorKind_VENDOR
		}
		out["user"] = graphv1.ActorKind_PERSON
		return out
	}()
)

// KnownEventSource reports whether the source is one the actor table names, for the recording's
// pre-pass: any other source is the organisation's own word and is pseudonymised there.
func KnownEventSource(source string) bool {
	_, ok := sourceKinds[normalise(source)]
	return ok
}

// KnownTrigger reports whether a trigger tag's value is in the published vocabulary.
func KnownTrigger(value string) bool {
	_, ok := triggerKinds[normalise(value)]
	return ok
}

// actorOf classifies an event's actor. The kind is never UNSPECIFIED: FR-033a requires every change to
// carry one, and UNKNOWN with its evidence is the honest reading of an actor nothing classifies.
func actorOf(ev EventJSON, tags tagSet) (name string, kind graphv1.ActorKind, evidence string) {
	name = tags.first(ActorNameKeys...)
	source := ev.source()
	trigger := tags.first(triggerTagKeys...)
	byTrigger, triggerKnown := triggerKinds[normalise(trigger)]
	bySource, sourceKnown := sourceKinds[normalise(source)]
	switch {
	case triggerKnown:
		// The trigger is what the event states about how it started; it outranks the tool that carried it.
		return name, byTrigger, ""
	case sourceKnown:
		return name, bySource, ""
	}
	var have []string
	if source != "" {
		have = append(have, fmt.Sprintf("event source %q is on no published list", source))
	} else {
		have = append(have, "the event states no source")
	}
	switch {
	case trigger != "":
		have = append(have, fmt.Sprintf("trigger tag value %q is on no published list", trigger))
	default:
		have = append(have, "no trigger tag")
	}
	if name != "" {
		have = append(have, "an actor is named, and a name does not classify itself")
	}
	return name, graphv1.ActorKind_ACTOR_KIND_UNKNOWN, strings.Join(have, "; ")
}

// ---- the identifiers (FR-031) ------------------------------------------------------------------------

var (
	commitTagKeys  = []string{"git.commit.sha", "git_commit_sha", "commit_sha"}
	imageTagKeys   = []string{"image", "container_image"}
	releaseTagKeys = []string{"version", "release"}
)

// deployKeys are the deploy identifiers a rollout event states, each in its published namespace's value
// form. A namespace an event states two different values of is left out and stated: a key that names
// two commits would merge the change with whichever the projector met first.
func deployKeys(tags tagSet, ambiguous *[]string) []feeder.CorrelationKey {
	var out []feeder.CorrelationKey
	add := func(namespace string, keys []string, normalise func(string) (string, bool), why string) {
		seen := map[string]bool{}
		var values []string
		for _, key := range keys {
			for _, raw := range tags[key] {
				if v, ok := normalise(raw); ok && !seen[v] {
					seen[v] = true
					values = append(values, v)
				}
			}
		}
		switch len(values) {
		case 0:
		case 1:
			out = append(out, feeder.CorrelationKey{Namespace: namespace, Value: values[0], Why: why})
		default:
			*ambiguous = append(*ambiguous, namespace+" ("+strings.Join(values, ", ")+")")
		}
	}
	add(feeder.NSDeployCommitSHA, commitTagKeys, feeder.CommitSHA, "the commit the event states under a git commit tag")
	add(feeder.NSDeployImage, imageTagKeys, feeder.Image, "the digest-pinned image the event states")
	// A version that is a full commit is a commit, unless a commit tag already said so; anything else is
	// the release form, which C8 does not merge on (a release tag is reused across repositories) but a
	// lookup can use.
	if tags.first(commitTagKeys...) == "" {
		add(feeder.NSDeployCommitSHA, releaseTagKeys, feeder.CommitSHA, "the version the event states, which is a full commit")
	}
	notCommit := func(raw string) (string, bool) {
		if _, isCommit := feeder.CommitSHA(raw); isCommit {
			return "", false
		}
		return feeder.Release(raw)
	}
	add(feeder.NSDeployRelease, releaseTagKeys, notCommit, "the release or version the event states")
	sort.Slice(out, func(i, j int) bool { return out[i].Namespace < out[j].Namespace })
	return out
}

// ---- the feeder --------------------------------------------------------------------------------------

// EventsMarker is the `events-poll` payload: what an events window covered and whether it finished.
type EventsMarker struct {
	// Outcome is "complete" or "partial".
	Outcome string `json:"outcome"`
	// Window is what was read. It is a sub-object, as the discovery tick's is: a bare `from` field is
	// on the sanitiser's people vocabulary (an email's sender).
	Window DiscoveryWindow `json:"window"`
	// Pages, Read and OutOfScope are counts: pages read, events read, and events read that the scope
	// left out (the poller filters, so the feeder cannot count them).
	Pages      int `json:"pages,omitempty"`
	Read       int `json:"read,omitempty"`
	OutOfScope int `json:"out_of_scope,omitempty"`
	// Reason says why a partial window stopped.
	Reason string `json:"reason,omitempty"`
	// StopReason, Deferred, ResumeAt and Usage are the poll marker's (FR-083, FR-084).
	StopReason string     `json:"stop_reason,omitempty"`
	Deferred   []string   `json:"deferred,omitempty"`
	ResumeAt   *time.Time `json:"resume_at,omitempty"`
	Usage      string     `json:"usage,omitempty"`
}

// changeStats is what a window stated rather than emitted, for the next events checkpoint.
type changeStats struct {
	emitted      int
	unattached   []string
	otherKind    []string
	unknownActor []string
	ambiguous    []string
	skipped      []string
}

// applyEvents turns one page of in-scope events into changes.
func (f *Feeder) applyEvents(ctx context.Context, em feeder.Emitter, raw []byte, at time.Time) error {
	if !f.opts.Capabilities.Enabled(CapChanges) {
		return fmt.Errorf("datadog: an %s payload arrived with the changes capability off; replaying it "+
			"would make the capability decorative (FR-008b)", PayloadEvents)
	}
	var page EventPage
	if err := json.Unmarshal(raw, &page); err != nil {
		return fmt.Errorf("datadog: decoding an %s payload: %w", PayloadEvents, err)
	}
	for _, ev := range page.Data {
		if err := f.emitEvent(ctx, em, ev, at); err != nil {
			return err
		}
	}
	return nil
}

func (f *Feeder) emitEvent(ctx context.Context, em feeder.Emitter, ev EventJSON, at time.Time) error {
	id, ok := feeder.StableIdentifier(string(ev.ID))
	skip := func(why string) error {
		f.mu.Lock()
		f.changes.skipped = append(f.changes.skipped, why)
		f.mu.Unlock()
		return nil
	}
	if !ok {
		return skip("an event with no usable id; a change is keyed on Datadog's own id and none is invented")
	}
	if ev.Attributes.Timestamp.IsZero() {
		return skip(fmt.Sprintf("event %s: Datadog states no instant for it; not emitted, because a change is "+
			"dated by the instant Datadog recorded and the poll's is never used", id))
	}
	tags := parseTags(ev.allTags())
	kind, vendorKind, rollback := changeKindOf(ev, tags)
	env := tags.first("env", "environment")
	service := tags.first("service")
	if service == "" {
		service = string(ev.Attributes.Attributes.Service)
	}

	var targets []*graphv1.Ref
	if service != "" {
		src := LogSource{Env: env, Service: service}
		if err := src.Validate(); err != nil {
			return skip(fmt.Sprintf("event %s: %v", id, err))
		}
		targets = append(targets, src.Ref())
	}
	if ns, dep := tags.first("kube_namespace"), tags.first("kube_deployment"); ns != "" && dep != "" {
		targets = append(targets, feeder.Ref(feeder.NSK8sDeployment, ns+"/"+dep))
	}

	name, actorKind, evidence := actorOf(ev, tags)
	source := ev.source()
	builder := feeder.NewProps()
	if env != "" {
		builder.Str(feeder.AttrDeploymentEnvironment, env)
	}
	if vendorKind != "" {
		builder.Str(PropEventKind, vendorKind)
	}
	if source != "" {
		builder.Str(PropEventSource, source)
	}
	if evidence != "" {
		builder.Str(PropActorEvidence, evidence)
	}
	props, err := builder.Build()
	if err != nil {
		return err
	}

	summary := strings.TrimSpace(ev.Attributes.Attributes.Title)
	if summary == "" {
		summary = "Datadog event"
		if vendorKind != "" {
			summary = vendorKind + " event"
		}
		if source != "" {
			summary += " from " + source
		}
	}
	if len(summary) > 200 {
		summary = summary[:200]
	}

	ref := feeder.Ref(NSChange, id)
	fact := feeder.ChangeFact{
		Meta: feeder.Meta{SourceObservedAt: at}, Ref: ref, Kind: kind, Summary: summary,
		Actor: name, ActorKind: actorKind, Targets: targets, ValidAt: ev.Attributes.Timestamp.Time, Props: props,
		Rollback: rollback,
	}
	if kind == graphv1.ChangeKind_CHANGE_KIND_OTHER {
		fact.KindOther = vendorKind
		if fact.KindOther == "" {
			fact.KindOther = "unstated"
		}
	}
	if f.opts.Site != "" {
		fact.OriginRef = "https://app." + f.opts.Site + "/event/event?id=" + id
		fact.Pointers = []*graphv1.Pointer{feeder.SourceLinkPointer(Kind, feeder.VocabURL, fact.OriginRef, nil)}
	}
	if err := emit(ctx, em, feeder.ObserveChange(f.desc, feeder.NewID(f.desc.SourceID, "change", id), fact)); err != nil {
		return err
	}

	var ambiguous []string
	if kind == graphv1.ChangeKind_ROLLOUT {
		attrs := feeder.NewProps()
		if env != "" {
			attrs.Str(feeder.AttrDeploymentEnvironment, env)
		}
		built, err := attrs.Build()
		if err != nil {
			return err
		}
		for _, key := range deployKeys(tags, &ambiguous) {
			if err := emit(ctx, em, feeder.Correlate(f.desc,
				feeder.NewID(f.desc.SourceID, "correlation", id, key.Namespace, key.Value),
				feeder.CorrelationFact{Meta: feeder.Meta{SourceObservedAt: at}, Subject: ref, Key: key.Ref(), Attributes: built})); err != nil {
				return err
			}
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.changes.emitted++
	if len(targets) == 0 {
		f.changes.unattached = append(f.changes.unattached, fmt.Sprintf("event %s (%s): names no service or "+
			"Kubernetes deployment, so it has no target; kept, and attaches to nothing until one is stated", id, summary))
	}
	if kind == graphv1.ChangeKind_CHANGE_KIND_OTHER {
		f.changes.otherKind = append(f.changes.otherKind, fmt.Sprintf("event %s: kind %q has no equivalent in the "+
			"published taxonomy; emitted as other, the vendor's kind kept as %s", id, fact.KindOther, PropEventKind))
	}
	if actorKind == graphv1.ActorKind_ACTOR_KIND_UNKNOWN {
		f.changes.unknownActor = append(f.changes.unknownActor, fmt.Sprintf("event %s: %s", id, evidence))
	}
	for _, a := range ambiguous {
		f.changes.ambiguous = append(f.changes.ambiguous, fmt.Sprintf("event %s states two %s; none is claimed", id, a))
	}
	return nil
}

// applyEventsPoll closes an events window with its checkpoint.
func (f *Feeder) applyEventsPoll(ctx context.Context, em feeder.Emitter, marker EventsMarker, at time.Time) error {
	if !f.opts.Capabilities.Enabled(CapChanges) {
		return fmt.Errorf("datadog: an %s payload arrived with the changes capability off", PayloadEventsPoll)
	}
	var complete bool
	switch marker.Outcome {
	case "complete":
		complete = true
	case "partial":
	default:
		return fmt.Errorf("datadog: events poll outcome %q is neither \"complete\" nor \"partial\"; a window whose "+
			"outcome is unstated cannot be evidence that no change happened (FR-012)", marker.Outcome)
	}
	// A complete window vouches for the interval it read. A partial one vouches for nothing and declares
	// the gap, as a partial monitor poll does.
	from, to := marker.Window.From, marker.Window.To
	if !complete || from.IsZero() || to.IsZero() {
		from, to = at, at
	}
	return em.Checkpoint(ctx, feeder.CheckpointFact{
		ExtentFrom: from, ExtentTo: to, GapBefore: !complete, Note: f.eventsNote(marker, complete),
	})
}

// eventsNote states the configuration in force and everything the window stated rather than emitted,
// then clears the latter (FR-029, FR-012).
func (f *Feeder) eventsNote(marker EventsMarker, complete bool) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	lines := []string{
		"datadog events poll: " + marker.Outcome,
		"capabilities: " + f.opts.Capabilities.String(),
		"site: " + orUnset(f.opts.Site),
		"read-only: " + readOnlyStatement(f.opts.OperatorAsserted),
		"change scope in force: " + f.opts.Changes.String() + "; an event that matches none is not a change to this " +
			"connector, which is different from no change having happened",
		fmt.Sprintf("window: %s to %s; events read: %d; out of scope: %d; changes emitted since the last checkpoint: %d",
			marker.Window.From.UTC().Format(time.RFC3339), marker.Window.To.UTC().Format(time.RFC3339), marker.Read, marker.OutOfScope,
			f.changes.emitted),
	}
	if !complete {
		lines = append(lines, "partial: "+orUnset(marker.Reason)+"; the events unread are unread, not absent")
	}
	if marker.StopReason != "" {
		stop := "stopped: " + marker.StopReason
		if marker.ResumeAt != nil {
			stop += "; reading again from " + marker.ResumeAt.UTC().Format(time.RFC3339) + ", as Datadog asked"
		}
		lines = append(lines, stop)
	}
	if len(marker.Deferred) > 0 {
		lines = append(lines, "deferred for quota, in the published order: "+strings.Join(marker.Deferred, ", ")+
			"; what they would have read is unread, not absent")
	}
	if marker.Usage != "" {
		lines = append(lines, "usage: "+marker.Usage)
	}
	add := func(title string, items []string) {
		items = uniqueSorted(items)
		if len(items) > 0 {
			lines = append(lines, title+":")
			for _, item := range items {
				lines = append(lines, "  - "+item)
			}
		}
	}
	add("unattached changes (FR-030)", f.changes.unattached)
	add("other-kind changes (FR-028)", f.changes.otherKind)
	add("actor kind unknown, with the evidence there was (FR-033b)", f.changes.unknownActor)
	add("identifiers left out because the event stated two", f.changes.ambiguous)
	add("skipped", f.changes.skipped)
	f.changes = changeStats{}
	return strings.Join(lines, "\n")
}
