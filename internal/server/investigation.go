// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1/investigationv1connect"
	"github.com/Pierre-Theophile/aisre/internal/investigation/intake"
	investigationstore "github.com/Pierre-Theophile/aisre/internal/investigation/store"
)

// InvestigationService: the dual-surface subset of FR-064 (T089, FR-008, FR-064, FR-066, FR-067,
// plan F10).
//
// # What is served, and what is deliberately not
//
// Eleven RPCs, exactly the commands FR-064 names as the dual surface: `Investigate`, `Declare`,
// `Get`, `List`, `Export`, `Replay`, `Reopen`, `SubmitHumanFact`, `Review`, `Label`. Operator
// tooling — `audit coverage`, `worker *`, `backend list`, `knowledge link`, `fixture *` — is
// **CLI-only by design**: it is run by a person at a terminal or by CI, and publishing it as an
// RPC would widen the served surface with no consumer on the other end.
//
// # The role mapping, stated once
//
//	Get, List, Export                                  reader
//	Investigate, Declare, Replay, Reopen, SubmitHumanFact   investigator
//	Review, Label                                      decider OR investigator
//
// `reader` is the floor for everything, because `investigator` implies it (auth.go). The reading
// of the middle row is that running an investigation spends a budget against a vendor's quota and
// calls a model, which is an authority an operator grants deliberately; the reading of the last
// row is that a review is a *decision about the graph's content* — it is the ground truth the
// corpus learns from — so either the person who decides such things or the person who ran the
// investigation may record it.
//
// # Authorization before anything else
//
// Every handler calls Require first. An operator reading a failure can therefore always tell a
// credential problem — Unauthenticated, PermissionDenied — from a problem with the question they
// asked, which is the same discipline QueryService and ResolutionService follow.
//
// # The engine is injected, not constructed
//
// The service holds a Runner and a store DAO. Without `--enable-investigation` the server does
// not register this handler at all and the binary behaves exactly as it did before (plan F10);
// with the flag and no Runner, the read RPCs work and the run RPCs return Unimplemented, which is
// the honest answer for a deployment with no model configuration.

// Runner is the investigation loop, as this service needs it.
//
// It is a narrow local interface rather than a dependency on the engine package: the server's job
// is authorization, transport and streaming, and it should compile and be testable with a stub.
// The engine implements it.
type Runner interface {
	// Investigate runs one investigation to a terminal state. It streams intermediate states
	// through `emit` — the anytime shape of FR-046a: the provisional prior-only ranking first,
	// then the first wave, then each turn — and returns the final one.
	//
	// It never blocks on a human (FR-057): the only thing that stops it is a terminal state or
	// the context going away.
	Investigate(ctx context.Context, req *investigationv1.InvestigateRequest, requester string,
		emit func(*investigationv1.Investigation) error) (*investigationv1.Investigation, error)

	// Declare opens an investigation from a human declaration (FR-001a). Re-delivery under a
	// seen idempotency key is a no-op returning the existing investigation (FR-008b).
	Declare(ctx context.Context, req *investigationv1.DeclareRequest, requester string,
		emit func(*investigationv1.Investigation) error) (*investigationv1.Investigation, error)

	// Replay replays an exported investigation with no network (FR-039–FR-041).
	Replay(ctx context.Context, req *investigationv1.ReplayRequest) (*investigationv1.ReplayResponse, error)

	// Export writes the self-contained artifact (FR-042).
	Export(ctx context.Context, req *investigationv1.ExportRequest) (*investigationv1.ExportResponse, error)

	// Reopen runs a new linked investigation after a fact bore on a concluded one (FR-057b).
	Reopen(ctx context.Context, parentID string, fact *investigationv1.HumanFact, requester string) (*investigationv1.Investigation, error)
}

// InvestigationStore is the persistence this service needs. It is an interface for the same
// reason Runner is: the handlers are about authorization and shape, and a test should not need a
// database to assert that `Get` refuses a token with no roles.
type InvestigationStore interface {
	Get(ctx context.Context, investigationID string) (*investigationv1.Investigation, error)
	List(ctx context.Context, f investigationstore.ListFilter) ([]*investigationv1.Investigation, error)
	SubmitFact(ctx context.Context, investigationID string, fact *investigationv1.HumanFact) (*investigationv1.HumanFact, error)
	RecordReview(ctx context.Context, investigationID string, review *investigationv1.HumanReview) (*investigationv1.HumanReview, error)
	RecordLabel(ctx context.Context, investigationID string, label *investigationv1.Label) (*investigationv1.Label, error)
	LifecycleOf(ctx context.Context, investigationID string) (lifecycle, conclusionKind, outcome string, err error)
}

// LedgerReader reads one run's hypotheses, judgments and evidence.
//
// It is separate from InvestigationStore because the two are read at different times and for
// different reasons: `InvestigationDAO.Get` is deliberately cheap — a caller listing a hundred
// runs does not want a hundred ledgers — while `Get` on *one* investigation is the read a
// reviewer does, and a run without its ledger is a verdict with nothing under it (FR-053,
// FR-057c). `investigate get --ledger|--chain|--evidence` and quickstart §1, §4, §5 and §9 all
// rest on this.
//
// It is optional. A server built without one still answers Get with the row and its symptoms,
// facts, reviews, labels and deliveries, exactly as before.
type LedgerReader interface {
	Load(ctx context.Context, investigationID string) (*investigationstore.LedgerRows, error)
}

// InvestigationService implements sreagent.investigation.v1.InvestigationService.
type InvestigationService struct {
	runner Runner
	store  InvestigationStore
	ledger LedgerReader
	logger *slog.Logger
}

// InvestigationOption configures NewInvestigationService.
type InvestigationOption func(*InvestigationService)

// WithLedgerReader gives the service the ledger half of the store, so that `Get` returns the
// hypotheses, judgments and evidence of the run it read. Without it `Get` answers with the row
// alone.
func WithLedgerReader(r LedgerReader) InvestigationOption {
	return func(s *InvestigationService) { s.ledger = r }
}

var _ investigationv1connect.InvestigationServiceHandler = (*InvestigationService)(nil)

// NewInvestigationService returns the handler. `runner` may be nil, in which case the RPCs that
// would run the engine return Unimplemented and the read RPCs still work.
func NewInvestigationService(runner Runner, store InvestigationStore, logger *slog.Logger,
	opts ...InvestigationOption) *InvestigationService {
	if logger == nil {
		logger = slog.Default()
	}
	svc := &InvestigationService{runner: runner, store: store, logger: logger}
	for _, opt := range opts {
		opt(svc)
	}
	return svc
}

// ErrInvestigationDisabled is returned by the run RPCs when no engine is configured.
var ErrInvestigationDisabled = errors.New(
	"the investigation engine is not enabled on this server; start it with " +
		"`serve --enable-investigation --model-config <file>`")

// ErrNoInvestigationStore is returned when the service has no persistence.
var ErrNoInvestigationStore = errors.New("this server has no investigation store")

// Investigate runs one investigation, streaming the anytime shape (FR-046a).
//
// Role: `investigator`.
func (s *InvestigationService) Investigate(
	ctx context.Context,
	req *connect.Request[investigationv1.InvestigateRequest],
	stream *connect.ServerStream[investigationv1.Investigation],
) error {
	if err := Require(ctx, RoleInvestigator); err != nil {
		return err
	}
	if s.runner == nil {
		return connect.NewError(connect.CodeUnimplemented, ErrInvestigationDisabled)
	}
	requester, err := requesterFrom(ctx)
	if err != nil {
		return err
	}
	emit := emitter(req.Msg.GetStream(), stream)
	final, err := s.runner.Investigate(ctx, req.Msg, requester, emit)
	if err != nil {
		return investigationError("investigate", err)
	}
	return stream.Send(final)
}

// Declare opens an investigation from a human declaration (FR-001a). Re-delivery under a seen
// idempotency key returns the existing investigation rather than opening a second one (FR-008b).
//
// Role: `investigator`.
func (s *InvestigationService) Declare(
	ctx context.Context,
	req *connect.Request[investigationv1.DeclareRequest],
	stream *connect.ServerStream[investigationv1.Investigation],
) error {
	if err := Require(ctx, RoleInvestigator); err != nil {
		return err
	}
	if s.runner == nil {
		return connect.NewError(connect.CodeUnimplemented, ErrInvestigationDisabled)
	}
	requester, err := requesterFrom(ctx)
	if err != nil {
		return err
	}
	declaration := req.Msg.GetDeclaration()
	if declaration == nil {
		return connect.NewError(connect.CodeInvalidArgument,
			errors.New("declare: no declaration; a declared incident carries the place it lives, "+
				"the instant of declaration and the declaring identity (FR-002a)"))
	}
	// FR-002a and FR-066: the declaring identity is the authenticated caller unless the caller
	// is a connector declaring on someone's behalf, in which case it is what the connector
	// observed. Either way it is never blank and never invented.
	if declaration.GetDeclaringIdentity() == "" {
		declaration.DeclaringIdentity = requester
	}
	emit := emitter(true, stream)
	final, err := s.runner.Declare(ctx, req.Msg, requester, emit)
	if err != nil {
		return investigationError("declare", err)
	}
	return stream.Send(final)
}

// Get reads one investigation.
//
// Role: `reader`.
func (s *InvestigationService) Get(
	ctx context.Context,
	req *connect.Request[investigationv1.GetRequest],
) (*connect.Response[investigationv1.Investigation], error) {
	if err := Require(ctx, RoleReader); err != nil {
		return nil, err
	}
	if s.store == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, ErrNoInvestigationStore)
	}
	inv, err := s.store.Get(ctx, req.Msg.GetInvestigationId())
	if err != nil {
		return nil, investigationError("get", err)
	}
	if err := s.attachLedger(ctx, inv); err != nil {
		return nil, investigationError("get", err)
	}
	return connect.NewResponse(inv), nil
}

// attachLedger fills in the run's ledger when the service has a reader and the row does not
// already carry one (the run RPCs answer from the engine, which already has it).
//
// A run whose ledger is empty — one that failed before it formed a hypothesis — is left with no
// ledger rather than with an empty one, so that "there is nothing here" and "I did not look" stay
// distinguishable to a reader.
func (s *InvestigationService) attachLedger(ctx context.Context, inv *investigationv1.Investigation) error {
	if s.ledger == nil || inv == nil || inv.GetLedger() != nil || inv.GetInvestigationId() == "" {
		return nil
	}
	rows, err := s.ledger.Load(ctx, inv.GetInvestigationId())
	if err != nil {
		return err
	}
	if rows == nil || (len(rows.Hypotheses) == 0 && len(rows.Evidence) == 0) {
		return nil
	}
	inv.Ledger = rows.Proto()
	return nil
}

// List lists investigations, newest first.
//
// Role: `reader`.
func (s *InvestigationService) List(
	ctx context.Context,
	req *connect.Request[investigationv1.ListInvestigationsRequest],
) (*connect.Response[investigationv1.ListInvestigationsResponse], error) {
	if err := Require(ctx, RoleReader); err != nil {
		return nil, err
	}
	if s.store == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, ErrNoInvestigationStore)
	}
	filter := investigationstore.ListFilter{
		IncidentID: req.Msg.GetIncidentId(),
		Lifecycle:  req.Msg.GetLifecycle(),
		Limit:      int(req.Msg.GetPageSize()),
	}
	if since := req.Msg.GetSince(); since != nil {
		filter.Since = since.AsTime()
	}
	rows, err := s.store.List(ctx, filter)
	if err != nil {
		return nil, investigationError("list", err)
	}
	return connect.NewResponse(&investigationv1.ListInvestigationsResponse{Investigations: rows}), nil
}

// Export writes the self-contained artifact (FR-042).
//
// Role: `reader`. Export copies what the caller may already read; it writes only into the
// directory the caller named, which is their own filesystem and not a production system.
func (s *InvestigationService) Export(
	ctx context.Context,
	req *connect.Request[investigationv1.ExportRequest],
) (*connect.Response[investigationv1.ExportResponse], error) {
	if err := Require(ctx, RoleReader); err != nil {
		return nil, err
	}
	if s.runner == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, ErrInvestigationDisabled)
	}
	resp, err := s.runner.Export(ctx, req.Msg)
	if err != nil {
		return nil, investigationError("export", err)
	}
	return connect.NewResponse(resp), nil
}

// Replay replays an exported investigation with no network (FR-039–FR-041).
//
// Role: `investigator`. A replay runs the engine's loop against a recording; it is the same
// authority as running one, minus the network.
func (s *InvestigationService) Replay(
	ctx context.Context,
	req *connect.Request[investigationv1.ReplayRequest],
) (*connect.Response[investigationv1.ReplayResponse], error) {
	if err := Require(ctx, RoleInvestigator); err != nil {
		return nil, err
	}
	if s.runner == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, ErrInvestigationDisabled)
	}
	resp, err := s.runner.Replay(ctx, req.Msg)
	if err != nil {
		return nil, investigationError("replay", err)
	}
	return connect.NewResponse(resp), nil
}

// Reopen runs a new linked investigation after a fact bore on a concluded one (FR-057b).
//
// Role: `investigator`. The concluded record is untouched: what comes back is the child.
func (s *InvestigationService) Reopen(
	ctx context.Context,
	req *connect.Request[investigationv1.ReopenRequest],
) (*connect.Response[investigationv1.Investigation], error) {
	if err := Require(ctx, RoleInvestigator); err != nil {
		return nil, err
	}
	if s.runner == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, ErrInvestigationDisabled)
	}
	requester, err := requesterFrom(ctx)
	if err != nil {
		return nil, err
	}
	fact := req.Msg.GetFact()
	if fact == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("reopen: a reopen carries the fact that bears on the concluded "+
				"investigation (FR-057b)"))
	}
	if fact.GetAuthor() == "" {
		fact.Author = requester
	}
	child, err := s.runner.Reopen(ctx, req.Msg.GetInvestigationId(), fact, requester)
	if err != nil {
		return nil, investigationError("reopen", err)
	}
	return connect.NewResponse(child), nil
}

// SubmitHumanFact pushes a typed fact at a running or concluded investigation (FR-057a).
//
// Role: `investigator`.
//
// It **never blocks** (FR-057): the fact is written and the current state comes back
// immediately. Where the investigation has concluded, the caller is told so in the returned
// lifecycle and reopens it with Reopen — the RPC does not silently start a second run, because a
// run costs money and starting one is a decision.
func (s *InvestigationService) SubmitHumanFact(
	ctx context.Context,
	req *connect.Request[investigationv1.SubmitHumanFactRequest],
) (*connect.Response[investigationv1.Investigation], error) {
	if err := Require(ctx, RoleInvestigator); err != nil {
		return nil, err
	}
	if s.store == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, ErrNoInvestigationStore)
	}
	requester, err := requesterFrom(ctx)
	if err != nil {
		return nil, err
	}
	fact := req.Msg.GetFact()
	if fact == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("fact: no fact"))
	}
	if fact.GetAuthor() == "" {
		fact.Author = requester
	}
	if fact.GetSubmittedAt() == nil {
		fact.SubmittedAt = nowProto()
	}
	if _, err := s.store.SubmitFact(ctx, req.Msg.GetInvestigationId(), fact); err != nil {
		return nil, investigationError("fact", err)
	}
	inv, err := s.store.Get(ctx, req.Msg.GetInvestigationId())
	if err != nil {
		return nil, investigationError("fact", err)
	}
	return connect.NewResponse(inv), nil
}

// Review records a human review (FR-054).
//
// Role: `decider` **or** `investigator`. A review is the ground truth the corpus learns from, so
// the person who decides such things may record one; so may the person who ran the investigation,
// because in most teams they are the same person and requiring a second role would mean reviews
// do not get recorded.
func (s *InvestigationService) Review(
	ctx context.Context,
	req *connect.Request[investigationv1.ReviewRequest],
) (*connect.Response[investigationv1.Investigation], error) {
	if err := RequireAny(ctx, RoleDecider, RoleInvestigator); err != nil {
		return nil, err
	}
	if s.store == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, ErrNoInvestigationStore)
	}
	requester, err := requesterFrom(ctx)
	if err != nil {
		return nil, err
	}
	review := req.Msg.GetReview()
	if review == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("review: no review"))
	}
	if review.GetAuthor() == "" {
		review.Author = requester
	}
	if review.GetDecidedAt() == nil {
		review.DecidedAt = nowProto()
	}
	if _, err := s.store.RecordReview(ctx, req.Msg.GetInvestigationId(), review); err != nil {
		return nil, investigationError("review", err)
	}
	inv, err := s.store.Get(ctx, req.Msg.GetInvestigationId())
	if err != nil {
		return nil, investigationError("review", err)
	}
	return connect.NewResponse(inv), nil
}

// Label records the one-click "was this right?" (FR-057e).
//
// Role: `decider` or `investigator`, for the same reason as Review.
func (s *InvestigationService) Label(
	ctx context.Context,
	req *connect.Request[investigationv1.LabelRequest],
) (*connect.Response[investigationv1.Investigation], error) {
	if err := RequireAny(ctx, RoleDecider, RoleInvestigator); err != nil {
		return nil, err
	}
	if s.store == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, ErrNoInvestigationStore)
	}
	requester, err := requesterFrom(ctx)
	if err != nil {
		return nil, err
	}
	label := &investigationv1.Label{
		WasThisRight: req.Msg.GetWasThisRight(),
		Author:       requester,
		LabelledAt:   nowProto(),
	}
	if _, err := s.store.RecordLabel(ctx, req.Msg.GetInvestigationId(), label); err != nil {
		return nil, investigationError("label", err)
	}
	inv, err := s.store.Get(ctx, req.Msg.GetInvestigationId())
	if err != nil {
		return nil, investigationError("label", err)
	}
	return connect.NewResponse(inv), nil
}

// RequireAny returns nil when ctx carries a principal holding at least one of roles. It is the
// "decider OR investigator" case, which the single-role Require cannot express.
func RequireAny(ctx context.Context, roles ...Role) error {
	p, ok := PrincipalFrom(ctx)
	if !ok {
		return connect.NewError(connect.CodeUnauthenticated, ErrMissingToken)
	}
	for _, role := range roles {
		if p.Has(role) {
			return nil
		}
	}
	return connect.NewError(connect.CodePermissionDenied,
		fmt.Errorf("principal %s holds none of the roles %v this call needs", p.Key(), roles))
}

// requesterFrom is FR-066 at the handler: every investigation and every human decision records
// the authenticated identity responsible for it, and an anonymous or shared credential is
// refused. The interceptor has already refused a token that names nobody; this is the second
// check, the one that holds when a caller reaches a handler in-process.
func requesterFrom(ctx context.Context) (string, error) {
	p, ok := PrincipalFrom(ctx)
	if !ok || p.Issuer == "" || p.Subject == "" {
		return "", connect.NewError(connect.CodeUnauthenticated, ErrAnonymous)
	}
	return p.Key(), nil
}

// emitter adapts the server stream to the Runner's callback. With streaming off, intermediate
// states are dropped and only the final one is sent — the same claims, fewer messages.
func emitter(streaming bool, stream *connect.ServerStream[investigationv1.Investigation]) func(*investigationv1.Investigation) error {
	if !streaming {
		return func(*investigationv1.Investigation) error { return nil }
	}
	return func(inv *investigationv1.Investigation) error {
		if inv == nil {
			return nil
		}
		return stream.Send(inv)
	}
}

// investigationError maps a store or engine failure onto a Connect code. A missing investigation
// is NotFound; a lifecycle refusal — concluding a concluded run, reopening a running one — is
// FailedPrecondition, because the caller asked for something the state does not permit rather
// than something malformed; an anonymous principal is Unauthenticated.
func investigationError(what string, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, investigationstore.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, fmt.Errorf("%s: %w", what, err))
	case errors.Is(err, investigationstore.ErrNotRunning),
		errors.Is(err, investigationstore.ErrNotConcluded):
		return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("%s: %w", what, err))
	case errors.Is(err, investigationstore.ErrAnonymousPrincipal):
		return connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("%s: %w", what, err))
	case errors.Is(err, investigationstore.ErrUnknownFactKind),
		errors.Is(err, investigationstore.ErrDecisiveHumanFact),
		errors.Is(err, intake.ErrUnknownProvenance),
		errors.Is(err, intake.ErrParsedWithoutRule),
		errors.Is(err, intake.ErrNoSubject),
		errors.Is(err, intake.ErrNoInstant),
		errors.Is(err, intake.ErrAnonymous):
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("%s: %w", what, err))
	default:
		var connectErr *connect.Error
		if errors.As(err, &connectErr) {
			return connectErr
		}
		return connect.NewError(connect.CodeInternal, fmt.Errorf("%s: %w", what, err))
	}
}

func nowProto() *timestamppb.Timestamp { return timestamppb.New(time.Now().UTC()) }
