// SPDX-License-Identifier: Apache-2.0

// Package mcpfeeder is a feeder over an MCP server (ADR-0001 D4).
//
// It is a spike (T093): the point is not the ownership directory it happens to read, but the
// shape a connector takes when its transport is the Model Context Protocol rather than a
// vendor's own SDK — one client library for every source, a fixed allowlist of read-only
// tools, and a recording whose payloads are the protocol's own answers. docs/connectors/mcp.md
// is the write-up; ../mockserver is the server it is developed against.
//
// What it maps, and nothing else:
//
//	list_channels        -> an OWNER node per channel, an `owned_by` edge per owned service,
//	                        and an identity claim tying the channel to the team's slug
//	list_docs            -> SOURCE_LINK pointers on the services a runbook concerns
//	directory://changes  -> a FLAG_FLIP change per announcement, targeting its services
//
// It does not claim that a channel *is* a service. `#team-checkout` owns `checkout`; it is not
// another name for it, and an identity claim saying so would merge an owner into the thing it
// owns. Ownership is an edge; only the two names of the owner itself are a claim.
package mcpfeeder

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// NSMCPChange identifies a change announced through an MCP directory, valued
// `<flag>@<announcement id>`.
//
// It is a new namespace rather than a reuse of NSK8sChange or NSOTelChange: those name changes
// observed *in* Kubernetes and *in* telemetry, and an announcement is neither. Minting one for
// a system nobody has connected yet is what the SDK guide asks for; reusing an existing one
// for something it does not mean is what it forbids.
const NSMCPChange = "mcp.change"

// PropOwnerOncall is who is on call for an owner right now.
//
// The SDK publishes sre.owner.kind, sre.owner.team, sre.owner.slack_channel and
// sre.owner.source_label but not this one, so the connector spells it itself under the project
// namespace, exactly as the guide says to. If a second connector ever wants it, it belongs in
// pkg/feeder/props.go instead.
const PropOwnerOncall = "sre.owner.oncall"

// OwnerKindSlackChannel is the value of sre.owner.kind for an owner that is a chat channel.
const OwnerKindSlackChannel = "slack-channel"

// pointerVocabURL is the vocabulary of a SOURCE_LINK whose selector is a URL. There is no
// OpenTelemetry vocabulary for "a page on the web", which is the documented reason this
// pointer is not expressed in semconv terms (constitution IV).
const pointerVocabURL = "url"

// Feeder reads one MCP ownership directory.
type Feeder struct {
	// SourceID is this connector's identity, e.g. "mcp:acme-directory". A feeder token is
	// scoped to exactly one.
	SourceID string
	// ServerName is the MCP server's name, which becomes the `backend_kind` of every pointer
	// as `mcp:<ServerName>`.
	//
	// It is configuration and not read from the session's ServerInfo on purpose: a pointer
	// recorded live must be identical when the fixture is replayed, and a replay has no
	// session to ask.
	ServerName string
	// Environment is the deployment environment the directory describes, e.g. "prod".
	//
	// An ownership directory has no environment dimension — a chat channel owns "checkout",
	// not "checkout in prod" — so the operator declares it, and it goes on the identity claim
	// as the attribute that makes the claim decidable. Empty means the connector says nothing
	// about the environment, which is honest and makes the claim weaker.
	Environment string
}

var _ feeder.Feeder = (*Feeder)(nil)

// Describe returns the connector's contract.
func (f *Feeder) Describe() feeder.Description {
	return feeder.Description{
		SourceID: f.SourceID,
		Kind:     "mcp",
		// An MCP answer carries no cursor, offset or revision the graph could order events
		// by: `tools/call` returns a whole document and the next call returns another whole
		// document. So there is no per-source sequence to declare.
		Ordering: feeder.OrderingNone,
		// One poll's three answers arrive within a few milliseconds of each other and are
		// stamped with one instant by the server. A few seconds is one poll's flush, and
		// declaring it makes the shuffle check permute a poll's payloads among themselves.
		ReorderingWindow: 5 * time.Second,
		RequiredScopes: []string{
			"MCP read tools only; the connector never calls a tool with side effects — " +
				"enforce by an allowlist of tool names",
			"tools/call " + toolListChannels,
			"tools/call " + toolListDocs,
			"resources/read " + resourceChanges,
		},
		Namespaces: []string{feeder.NSOTelService, feeder.NSOwnerTeam, NSMCPChange},
	}
}

// Run maps polled MCP answers onto graph events.
//
// One poll is one round of every call the connector makes, grouped by Payload.Seq. A round is
// checkpointed when all three of its answers have been seen — never when the last payload
// happens to arrive, which would make the extent depend on delivery order.
func (f *Feeder) Run(ctx context.Context, src feeder.Source, em feeder.Emitter) error {
	d := f.Describe()
	if err := d.Validate(); err != nil {
		return err
	}
	if f.ServerName == "" {
		return errors.New("mcp-feeder: ServerName is required; it is the backend_kind of every pointer this connector attaches")
	}
	// FR-046. A live Source can ask the server what it offers; a recording has no credential
	// to check, and pretending otherwise would be the wrong kind of green test.
	if v, ok := src.(interface {
		VerifyReadOnly(context.Context) error
	}); ok {
		if err := v.VerifyReadOnly(ctx); err != nil {
			var unhinted *UnhintedToolsWarning
			if !errors.As(err, &unhinted) {
				return err
			}
		}
	}

	polls := map[int64]*poll{}
	checkpoints := 0
	var lastTo time.Time

	for {
		p, err := src.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if err := f.handle(ctx, em, d, p); err != nil {
			return err
		}

		round, ok := polls[p.Seq]
		if !ok {
			round = &poll{}
			polls[p.Seq] = round
		}
		round.observe(p)
		if !round.complete() {
			continue
		}
		delete(polls, p.Seq)

		// A poll's checkpoint covers everything since the previous poll's answer, because
		// that is the window the connector was actually watching. The first checkpoint of a
		// run declares a gap before it: the connector was not watching before it started,
		// which is as true of a restart as of a first run.
		from := lastTo
		if from.IsZero() {
			from = round.to
		}
		if err := em.Checkpoint(ctx, feeder.CheckpointFact{
			ExtentFrom: from, ExtentTo: round.to, GapBefore: checkpoints == 0,
		}); err != nil {
			return err
		}
		checkpoints++
		lastTo = round.to
	}

	return em.Flush(ctx)
}

// poll is one round of answers, and the extent they cover.
type poll struct {
	seen     map[string]bool
	from, to time.Time
}

// wantKinds is what one complete poll delivers.
var wantKinds = []string{KindChannels, KindDocs, KindChanges}

func (p *poll) observe(payload feeder.Payload) {
	if p.seen == nil {
		p.seen = map[string]bool{}
	}
	p.seen[payload.Kind] = true
	if payload.At.IsZero() {
		return
	}
	at := payload.At.UTC()
	if p.from.IsZero() || at.Before(p.from) {
		p.from = at
	}
	if at.After(p.to) {
		p.to = at
	}
}

func (p *poll) complete() bool {
	for _, kind := range wantKinds {
		if !p.seen[kind] {
			return false
		}
	}
	return true
}

// handle dispatches one payload to its mapping.
func (f *Feeder) handle(ctx context.Context, em feeder.Emitter, d feeder.Description, p feeder.Payload) error {
	var (
		events []*graphv1.EventEnvelope
		err    error
	)
	switch p.Kind {
	case KindChannels:
		events, err = f.mapChannels(d, p)
	case KindDocs:
		events, err = f.mapDocs(d, p)
	case KindChanges:
		events, err = f.mapChanges(d, p)
	default:
		return fmt.Errorf("mcp-feeder: unknown payload kind %q", p.Kind)
	}
	if err != nil {
		return err
	}
	return f.emit(ctx, em, events)
}

func (f *Feeder) emit(ctx context.Context, em feeder.Emitter, events []*graphv1.EventEnvelope) error {
	for _, ev := range events {
		result, err := em.Emit(ctx, ev)
		if err != nil {
			return err
		}
		// A REJECTED result is an answer, not a transport failure — but a connector whose
		// mapping is wrong should say so loudly rather than drop facts on the floor.
		if result.GetStatus() == graphv1.IngestResult_REJECTED {
			return fmt.Errorf("mcp-feeder: %s refused: %s (%s)",
				ev.GetEventId(), result.GetReasonCode(), result.GetReasonDetail())
		}
	}
	return nil
}

// backendKind names the MCP server a pointer leads back to: `mcp:acme-directory`.
func (f *Feeder) backendKind() string { return "mcp:" + f.ServerName }

// mapChannels turns the channel directory into owners, ownership edges and identity claims.
//
// Every fact here has an unknown valid start. The directory says who owns what *now*; it does
// not say since when, and inventing the poll's timestamp would be a guess (FR-011) that also
// made the events depend on when the connector happened to run.
func (f *Feeder) mapChannels(d feeder.Description, p feeder.Payload) ([]*graphv1.EventEnvelope, error) {
	var doc channelsDoc
	if err := decode(KindChannels, p.Bytes, &doc); err != nil {
		return nil, err
	}
	if doc.Version == "" {
		return nil, errors.New("mcp-feeder: channels: the server returned no content version, so event ids could not be made deterministic")
	}

	var events []*graphv1.EventEnvelope
	for _, ch := range doc.Channels {
		if ch.Channel == "" {
			continue
		}
		ownerRef := feeder.Ref(feeder.NSOwnerTeam, ch.Channel)

		props := feeder.NewProps().
			Str(feeder.PropOwnerKind, OwnerKindSlackChannel).
			Str(feeder.PropOwnerSlackChannel, ch.Channel)
		if ch.Team != "" {
			props = props.Str(feeder.PropOwnerTeam, ch.Team)
		}
		if ch.Oncall != "" {
			props = props.Str(PropOwnerOncall, ch.Oncall)
		}
		if f.Environment != "" {
			props = props.Str(feeder.AttrDeploymentEnvironment, f.Environment)
		}
		built, err := props.Build()
		if err != nil {
			return nil, err
		}

		var pointers []*graphv1.Pointer
		if ch.URL != "" {
			pointers = append(pointers, feeder.SourceLinkPointer(
				f.backendKind(), pointerVocabURL, ch.URL, f.ownerAttrs(ch)))
		}

		events = append(events, feeder.UpsertNode(d,
			feeder.NewID(d.SourceID, "owner", ch.Channel+"@"+doc.Version),
			feeder.NodeFact{
				Ref:              ownerRef,
				Type:             graphv1.NodeType_OWNER,
				DisplayName:      ch.Channel,
				Props:            built,
				Pointers:         pointers,
				ValidFromUnknown: true,
			}))

		// The channel and the team slug are two names for the *same owner*, which is what an
		// identity claim is for. The claim carries no content version: re-asserting the same
		// pair is the same fact, and should collapse onto the first delivery rather than
		// appear again every time an unrelated channel changes.
		if ch.Team != "" && ch.Team != ch.Channel {
			attrs, err := f.claimAttrs()
			if err != nil {
				return nil, err
			}
			events = append(events, feeder.IdentityClaim(d,
				feeder.NewID(d.SourceID, "claim", ch.Channel+"@"+ch.Team),
				feeder.IdentityFact{
					Subject:    ownerRef,
					Claim:      feeder.Ref(feeder.NSOwnerTeam, ch.Team),
					Attributes: attrs,
				}))
		}

		owns := append([]string(nil), ch.Owns...)
		sort.Strings(owns)
		for _, svc := range owns {
			if svc == "" {
				continue
			}
			events = append(events, feeder.UpsertEdge(d,
				feeder.NewID(d.SourceID, "edge", "owned-by", svc+"->"+ch.Channel+"@"+doc.Version),
				feeder.EdgeFact{
					Src: feeder.Ref(feeder.NSOTelService, svc),
					Dst: ownerRef,
					// The service side is very often a ref nothing has described yet. The
					// graph mints a placeholder for an unresolved endpoint rather than
					// refusing the edge, so ownership can be known before topology is.
					Type:             graphv1.EdgeType_OWNED_BY,
					ValidFromUnknown: true,
				}))
		}
	}
	return events, nil
}

// mapDocs attaches runbooks to the services they concern, as pointers.
//
// A pointer is the right concept here and a `depends_on` edge is not: a runbook is not part of
// the production system, it is *where to look* when the thing it concerns misbehaves — which
// is the definition of a pointer (constitution IV).
//
// The consequence is worth stating plainly: this produces an `upsert_node` on a SERVICE ref
// carrying pointers and one property, `service.name`. The connector is asserting that the
// directory knows of a service by that name and where its runbooks are, and nothing else. In
// the graph that version replaces the placeholder an `owned_by` edge would otherwise have
// minted, so the node is no longer marked `sre.placeholder` even though this source has very
// little to say about it. The alternative — hanging the runbook off the owner instead — was
// rejected because the question a responder asks is "what do I read about *checkout*", not
// "what does #team-checkout have on file".
func (f *Feeder) mapDocs(d feeder.Description, p feeder.Payload) ([]*graphv1.EventEnvelope, error) {
	var doc docsDoc
	if err := decode(KindDocs, p.Bytes, &doc); err != nil {
		return nil, err
	}
	if doc.Version == "" {
		return nil, errors.New("mcp-feeder: docs: the server returned no content version, so event ids could not be made deterministic")
	}

	// One node version per service carrying every runbook that concerns it, rather than one
	// per (service, document): a node version is the whole of what this source knows about
	// the entity, so emitting one per document would make each version contradict the last.
	byService := map[string][]*graphv1.Pointer{}
	for _, item := range doc.Docs {
		if item.URL == "" {
			continue
		}
		for _, svc := range item.Concerns {
			if svc == "" {
				continue
			}
			byService[svc] = append(byService[svc], feeder.SourceLinkPointer(
				f.backendKind(), pointerVocabURL, item.URL, f.serviceAttrs(svc)))
		}
	}

	var events []*graphv1.EventEnvelope
	for _, svc := range sortedKeys(byService) {
		pointers := byService[svc]
		sort.Slice(pointers, func(i, j int) bool { return pointers[i].GetSelector() < pointers[j].GetSelector() })

		props := feeder.NewProps().Str(feeder.AttrServiceName, svc)
		if f.Environment != "" {
			props = props.Str(feeder.AttrDeploymentEnvironment, f.Environment)
		}
		built, err := props.Build()
		if err != nil {
			return nil, err
		}

		events = append(events, feeder.UpsertNode(d,
			feeder.NewID(d.SourceID, "docs", svc+"@"+doc.Version),
			feeder.NodeFact{
				Ref:              feeder.Ref(feeder.NSOTelService, svc),
				Type:             graphv1.NodeType_SERVICE,
				DisplayName:      svc,
				Props:            built,
				Pointers:         pointers,
				ValidFromUnknown: true,
			}))
	}
	return events, nil
}

// mapChanges turns announcements into change observations.
//
// Unlike everything else here, a change *does* know when it happened: the announcement says
// so, and that instant is the change's valid time. The targets become `changed-by` edges from
// each service to the change, which is what puts a flag flip at hop 0 of the service it
// changed.
func (f *Feeder) mapChanges(d feeder.Description, p feeder.Payload) ([]*graphv1.EventEnvelope, error) {
	var doc changesDoc
	if err := decode(KindChanges, p.Bytes, &doc); err != nil {
		return nil, err
	}

	var events []*graphv1.EventEnvelope
	for _, c := range doc.Changes {
		if c.ID == "" {
			continue
		}
		at, err := time.Parse(time.RFC3339, c.At)
		if err != nil {
			return nil, fmt.Errorf("mcp-feeder: change %s: at %q: %w", c.ID, c.At, err)
		}

		kind, other := changeKind(c.Kind)
		services := append([]string(nil), c.Services...)
		sort.Strings(services)
		targets := make([]*graphv1.Ref, 0, len(services))
		for _, svc := range services {
			if svc == "" {
				continue
			}
			targets = append(targets, feeder.Ref(feeder.NSOTelService, svc))
		}

		subject := c.Flag
		if subject == "" {
			subject = c.Kind
		}
		events = append(events, feeder.ObserveChange(d,
			// An announcement is immutable, so its id carries no content version: the same
			// announcement read on every poll for a week is one change, delivered once.
			feeder.NewID(d.SourceID, "change", c.ID),
			feeder.ChangeFact{
				Ref:       feeder.Ref(NSMCPChange, subject+"@"+c.ID),
				Kind:      kind,
				KindOther: other,
				Summary:   c.Summary,
				Actor:     c.Actor,
				OriginRef: c.URL,
				Targets:   targets,
				ValidAt:   at.UTC(),
			}))
	}
	return events, nil
}

// changeKind maps the directory's spelling onto the published taxonomy, falling back to
// CHANGE_KIND_OTHER with the original word rather than guessing a near-enough kind.
func changeKind(kind string) (graphv1.ChangeKind, string) {
	switch kind {
	case "flag_flip":
		return graphv1.ChangeKind_FLAG_FLIP, ""
	case "rollout":
		return graphv1.ChangeKind_ROLLOUT, ""
	case "config_change":
		return graphv1.ChangeKind_CONFIG_CHANGE, ""
	default:
		return graphv1.ChangeKind_CHANGE_KIND_OTHER, kind
	}
}

// ownerAttrs are the resource attributes on a pointer into the chat tool.
func (f *Feeder) ownerAttrs(ch channelDoc) map[string]string {
	attrs := map[string]string{feeder.PropOwnerSlackChannel: ch.Channel}
	if ch.Team != "" {
		attrs[feeder.PropOwnerTeam] = ch.Team
	}
	if f.Environment != "" {
		attrs[feeder.AttrDeploymentEnvironment] = f.Environment
	}
	return attrs
}

// serviceAttrs are the resource attributes on a pointer to a service's documentation.
func (f *Feeder) serviceAttrs(svc string) map[string]string {
	attrs := map[string]string{feeder.AttrServiceName: svc}
	if f.Environment != "" {
		attrs[feeder.AttrDeploymentEnvironment] = f.Environment
	}
	return attrs
}

// claimAttrs are the supporting facts a resolution rule needs to decide the channel/team
// claim: which environment it is about, and that it came from the directory's `team` field
// rather than from a coincidence of spelling.
func (f *Feeder) claimAttrs() (*structpb.Struct, error) {
	props := feeder.NewProps().
		Str(feeder.PropOwnerKind, OwnerKindSlackChannel).
		Str(feeder.PropOwnerSourceLabel, "team")
	if f.Environment != "" {
		props = props.Str(feeder.AttrDeploymentEnvironment, f.Environment)
	}
	return props.Build()
}
