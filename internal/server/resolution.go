// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel/attribute"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1/graphv1connect"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	eventlog "github.com/Pierre-Theophile/aisre/internal/log"
	"github.com/Pierre-Theophile/aisre/internal/projector"
	"github.com/Pierre-Theophile/aisre/internal/telemetry"
)

// Human resolution decisions (FR-040, FR-041, FR-041a, SC-011, constitution VI).
//
// This service is sugar over IngestService: each call becomes the corresponding event with
// `source_id = "human"` and the authenticated principal stamped on it, so a merge a person made
// is an ordinary, replayable log entry that carries the name of whoever made it. There is no
// second store and no side table: what a person decided is in the log, or it did not happen.
//
// Three things the handlers do that are worth stating plainly.
//
// # The decider role, on every method
//
// "The graph MUST NOT accept a resolution decision from an anonymous or shared credential"
// (FR-041) is enforced twice, on purpose. Here, because a caller without the role must be told
// so before anything is written; and again in the projector, which refuses any decision event
// whose principal is empty (`missing_principal`). The second check is the one that matters for a
// fixture or a replay, where no HTTP request exists at all.
//
// # Deterministic event ids
//
// A human decision needs an idempotency key like every other event (FR-020, constitution III),
// and the request message carries no nonce. The id is therefore derived from the decision
// itself:
//
//	human:<kind>:<subject>:<sha256(principal | rationale)[:12]>
//
// where `subject` is the pair of references in sorted order (or, for a split, the entity
// followed by the detached identifiers). Two consequences follow and both are deliberate:
//
//   - the same person making the same decision about the same pair with the same rationale twice
//     is a DUPLICATE_NOOP. A double click, a retried request or a re-run of a script changes
//     nothing, which is what an idempotency key is for;
//   - the same decision with a *different* rationale is a different event, because the rationale
//     is part of what constitution VI requires to be recorded. Somebody adding the reason they
//     forgot the first time gets a second decision row, and the audit shows both.
//
// A person who genuinely wants to re-record an identical decision can change the rationale, and
// anyone reading the log can see that is what they did.
//
// # The principal registry
//
// Every accepted decision upserts `graph.principals` (data-model.md), so that the audit can say
// who `sre-agent-dev|alice` is without asking the identity provider, which may no longer know.

// HumanSourceID is the source id every human decision is logged under. It is `internal/log`'s
// own constant rather than a second spelling of it: the investigation's human channel registers
// the same source for a fact, a reopen and a label (FR-057a/b/e), and two constants that must
// agree are one constant.
const HumanSourceID = eventlog.HumanSourceID

// humanSource is the source declaration registered for it: a person is not a feeder, delivers
// nothing in order, and needs no reordering window.
var humanSource = eventlog.HumanSource()

// ResolutionService implements sreagent.graph.v1.ResolutionService.
type ResolutionService struct {
	projector   *projector.Projector
	metrics     *telemetry.Metrics
	logger      *slog.Logger
	registered  sync.Once
	registerErr error
}

var _ graphv1connect.ResolutionServiceHandler = (*ResolutionService)(nil)

// NewResolutionService returns the resolution handler. metrics may be nil; logger defaults to
// slog.Default().
func NewResolutionService(p *projector.Projector, metrics *telemetry.Metrics, logger *slog.Logger) *ResolutionService {
	if logger == nil {
		logger = slog.Default()
	}
	return &ResolutionService{projector: p, metrics: metrics, logger: logger}
}

// Projector is the graph decisions are applied through.
func (s *ResolutionService) Projector() *projector.Projector { return s.projector }

// Metrics is the instrument set resolution handlers record decisions on.
func (s *ResolutionService) Metrics() *telemetry.Metrics { return s.metrics }

// Logger is the structured logger resolution handlers should use.
func (s *ResolutionService) Logger() *slog.Logger { return s.logger }

// RegisterHumanSource declares the `human` source, which every decision event is logged under.
// It is called at server start and again, idempotently, before the first decision, so that an
// in-process caller that never ran Serve still works.
func RegisterHumanSource(ctx context.Context, p *projector.Projector) error {
	return p.RegisterSource(ctx, humanSource)
}

func (s *ResolutionService) ensureSource(ctx context.Context) error {
	s.registered.Do(func() { s.registerErr = RegisterHumanSource(ctx, s.projector) })
	return s.registerErr
}

// Confirm accepts a suggested merge (FR-040).
func (s *ResolutionService) Confirm(ctx context.Context, req *connect.Request[graphv1.ConfirmMerge]) (*connect.Response[graphv1.IngestResult], error) {
	msg := req.Msg
	return s.decide(ctx, "confirm", []string{msg.GetEntityA(), msg.GetEntityB()}, msg.GetRationale(),
		func(env *graphv1.EventEnvelope) { env.Body = &graphv1.EventEnvelope_ConfirmMerge{ConfirmMerge: msg} })
}

// Reject refuses a suggested merge and blocks every future automated merge of the pair
// (FR-040).
func (s *ResolutionService) Reject(ctx context.Context, req *connect.Request[graphv1.RejectMerge]) (*connect.Response[graphv1.IngestResult], error) {
	msg := req.Msg
	return s.decide(ctx, "reject", []string{msg.GetEntityA(), msg.GetEntityB()}, msg.GetRationale(),
		func(env *graphv1.EventEnvelope) { env.Body = &graphv1.EventEnvelope_RejectMerge{RejectMerge: msg} })
}

// ConfirmDependency accepts a proposed edge, which is what creates it (003 FR-029).
//
// The subject is the triple rather than a pair of entity ids, because that is what the proposal is
// keyed on and what a reviewer can name from the listing. It reaches the same `decide` helper as
// every other human decision, so the principal is stamped, the decision is an event, and the
// decider role is demanded in exactly one place.
func (s *ResolutionService) ConfirmDependency(ctx context.Context, req *connect.Request[graphv1.ConfirmDependency]) (*connect.Response[graphv1.IngestResult], error) {
	msg := req.Msg
	return s.decide(ctx, "confirm_dependency", dependencySubject(msg.GetSrc(), msg.GetDst(), msg.GetType()),
		msg.GetRationale(),
		func(env *graphv1.EventEnvelope) {
			env.Body = &graphv1.EventEnvelope_ConfirmDependency{ConfirmDependency: msg}
		})
}

// RejectDependency refuses a proposed edge and blocks every later automated proposal of the same
// triple, which is then recorded as a conflict rather than reopened (003 FR-122).
func (s *ResolutionService) RejectDependency(ctx context.Context, req *connect.Request[graphv1.RejectDependency]) (*connect.Response[graphv1.IngestResult], error) {
	msg := req.Msg
	return s.decide(ctx, "reject_dependency", dependencySubject(msg.GetSrc(), msg.GetDst(), msg.GetType()),
		msg.GetRationale(),
		func(env *graphv1.EventEnvelope) {
			env.Body = &graphv1.EventEnvelope_RejectDependency{RejectDependency: msg}
		})
}

// dependencySubject renders the triple for the deterministic event id, in the order asserted.
//
// The direction is part of the subject rather than sorted away: "checkout depends on orders-db"
// and its reverse are different decisions, and a subject that ordered its ends would give them one
// event id and make the second a duplicate of the first.
func dependencySubject(src, dst *graphv1.Ref, typ graphv1.EdgeType) []string {
	return []string{
		graph.RefFromProto(src).String(),
		graph.RefFromProto(dst).String(),
		typ.String(),
	}
}

// Split detaches claims from an entity that was wrongly merged (FR-039).
func (s *ResolutionService) Split(ctx context.Context, req *connect.Request[graphv1.SplitEntity]) (*connect.Response[graphv1.IngestResult], error) {
	msg := req.Msg
	subject := []string{msg.GetEntityId()}
	for _, ref := range msg.GetDetachClaims() {
		subject = append(subject, graph.RefFromProto(ref).String())
	}
	return s.decide(ctx, "split", subject, msg.GetRationale(),
		func(env *graphv1.EventEnvelope) { env.Body = &graphv1.EventEnvelope_SplitEntity{SplitEntity: msg} })
}

// Merge records a merge a person asked for directly, without a suggestion (FR-040).
func (s *ResolutionService) Merge(ctx context.Context, req *connect.Request[graphv1.ManualMerge]) (*connect.Response[graphv1.IngestResult], error) {
	msg := req.Msg
	return s.decide(ctx, "merge", []string{msg.GetEntityA(), msg.GetEntityB()}, msg.GetRationale(),
		func(env *graphv1.EventEnvelope) { env.Body = &graphv1.EventEnvelope_ManualMerge{ManualMerge: msg} })
}

// decide is the body every method shares: authorize, build the envelope, apply it with the
// principal attached, and record who decided.
func (s *ResolutionService) decide(ctx context.Context, kind string, subject []string, rationale string, setBody func(*graphv1.EventEnvelope)) (*connect.Response[graphv1.IngestResult], error) {
	if err := Require(ctx, RoleDecider); err != nil {
		return nil, err
	}
	// One span per decision RPC (plan.md §Observability). The `projector.apply` span for the
	// event it becomes is its child, so a trace answers "who decided what, and what did the
	// graph do about it" in one place.
	ctx, span := telemetry.StartSpan(ctx, telemetry.SpanResolutionPrefix+kind,
		attribute.String(telemetry.SpanAttrRPC, kind))
	defer span.End()

	p, _ := PrincipalFrom(ctx)
	if err := s.ensureSource(ctx); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	env := &graphv1.EventEnvelope{
		EventId:       graph.HumanEventID(kind, subject, p.Key(), rationale),
		SourceId:      HumanSourceID,
		SchemaVersion: firstSchemaVersion(s.projector),
	}
	env.IdempotencyKey = env.EventId
	setBody(env)

	result, err := s.projector.ApplyWithOptions(ctx, env, projector.ApplyOptions{Principal: p.Key()})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if result.GetStatus() == graphv1.IngestResult_APPLIED {
		if err := s.recordPrincipal(ctx, p); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
	}
	span.SetAttributes(
		attribute.String(telemetry.SpanAttrEventID, env.GetEventId()),
		attribute.String(telemetry.SpanAttrResult, result.GetStatus().String()),
	)
	s.logger.Info("resolution decision",
		"kind", kind, "principal", p.Key(), "event_id", env.GetEventId(),
		"status", result.GetStatus().String(), "reason_code", result.GetReasonCode())
	return connect.NewResponse(result), nil
}

// recordPrincipal upserts the individual behind a decision (data-model.md §graph.principals).
// `first_seen` is kept, `last_seen` moves, and the roles recorded are the ones the provider
// granted for this call.
func (s *ResolutionService) recordPrincipal(ctx context.Context, p *Principal) error {
	roles := make([]string, 0, len(p.Roles))
	for _, role := range p.Roles {
		roles = append(roles, string(role))
	}
	_, err := s.projector.Store().Pool().Exec(ctx, `
		INSERT INTO graph.principals (principal, email, display_name, roles)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (principal) DO UPDATE SET
			email = coalesce(nullif(EXCLUDED.email, ''), graph.principals.email),
			display_name = coalesce(nullif(EXCLUDED.display_name, ''), graph.principals.display_name),
			roles = EXCLUDED.roles,
			last_seen = now()`,
		p.Key(), p.Email, p.DisplayName, roles)
	if err != nil {
		return fmt.Errorf("server: record principal %s: %w", p.Key(), err)
	}
	return nil
}

// firstSchemaVersion is the event schema version decisions are stamped with: the first the log
// accepts, so a deployment that has moved on does not have its own decisions refused.
func firstSchemaVersion(p *projector.Projector) string {
	if versions := p.Log().SchemaVersions(); len(versions) > 0 {
		return versions[0]
	}
	return "1.0.0"
}
