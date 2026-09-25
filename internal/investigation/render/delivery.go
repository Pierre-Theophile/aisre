// SPDX-License-Identifier: Apache-2.0

package render

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// The report-delivery CONTRACT (T087, FR-057f, ADR-0005 D8, constitution VII v1.1.0).
//
// This is the project's **only** write outside its own stores, and the constitution grants it as
// a confined exception rather than as a capability. The confinement is the whole of this file:
//
//  1. **Report-only.** What travels is a rendering of an investigation — the FR-057c order, with
//     the FR-057d deep links — and nothing else. There is no code path here that carries a
//     command, a parameter or anything a receiving system could act on.
//  2. **Edited in place.** The first delivery mints an external reference; every later one edits
//     that same message. A stream of re-posts is explicitly not what FR-057f asks for, and the
//     Sink interface has no "post again" method — Deliver carries the previous reference and the
//     driver is obliged to update it.
//  3. **Non-blocking, never a precondition of a terminal state.** Deliver is called *after* an
//     investigation concludes, never inside the conclusion, and a failure is recorded and
//     returned as a DeliveryResult with Outcome "failed" rather than as an error that could
//     propagate into the lifecycle. The type signature is the guarantee: Deliver cannot fail the
//     conclusion because the conclusion does not call it.
//  4. **A credential that grants nothing else.** Credential below is a separate type from any
//     read credential in this codebase, it carries an explicit scope, and CheckScope refuses a
//     credential whose scope is anything other than writing that one report. A delivery
//     credential handed to a reader is refused by ErrCredentialNotForReading, which is a
//     compile-and-test-time property rather than a deployment convention.
//  5. **Transport is a connector concern.** The v1 sink is LogSink: it records what it would have
//     posted and posts nothing. Shipping a real transport means writing a Sink in a connector,
//     and this package neither imports nor knows about one.
//
// A note on what is deliberately absent. There is no retry loop, no queue and no background
// worker. A delivery that fails is recorded and the next `investigate report --deliver` tries
// again; building durability here would make delivery a system with a state of its own, and the
// one thing FR-057f is insistent about is that delivery has no state the investigation waits on.

// DeliveryScope is the only scope a delivery credential may carry. It is a constant rather than
// a configurable string so that "scoped to writing that report and nothing else" is checkable.
const DeliveryScope = "report:write"

// Delivery outcomes, mirroring `investigation.report_deliveries.outcome`.
const (
	OutcomeDelivered     = "delivered"
	OutcomeFailed        = "failed"
	OutcomeNotConfigured = "not_configured"
)

// Delivery errors.
var (
	// ErrCredentialNotForReading is a delivery credential used for anything but delivering a
	// report. It is what makes "separate from every read credential" testable: the credential
	// type exposes no reader, and asking it for one fails.
	ErrCredentialNotForReading = errors.New(
		"a report-delivery credential grants writing and editing one report and nothing else; " +
			"it may not be used to read (constitution VII, FR-057f)")
	// ErrCredentialScope is a credential whose scope is not exactly DeliveryScope.
	ErrCredentialScope = fmt.Errorf("a report-delivery credential must carry scope %q and no other", DeliveryScope)
	// ErrNoTarget is a delivery with nowhere to go.
	ErrNoTarget = errors.New("report delivery: no target system or reference")
	// ErrCredentialTarget is a credential used to write somewhere it was not minted for. The
	// doc comment on Credential has always claimed "a credential for one place is not a
	// credential for another"; this is the check that makes the claim true (T116).
	ErrCredentialTarget = errors.New(
		"a report-delivery credential may only write to the place it was minted for")
	// ErrUnknownSink is a sink id that is not one of the published transports. It is an error
	// rather than a fallback: defaulting an unrecognised transport to the one that happens to
	// be compiled in is how a write path ends up somewhere nobody chose (T116, constitution VII).
	ErrUnknownSink = errors.New("unknown report-delivery sink")
)

// Credential is the scoped secret a delivery uses.
//
// It is a distinct type from every read credential in this codebase on purpose. The constitution
// requires the two to be separate; making them separate *types* means a read path cannot be
// handed one by accident, and a delivery path cannot be handed a read token that would let it do
// more. The secret is never rendered: String redacts it, so a credential in a log line or an
// error is a description rather than a key.
type Credential struct {
	scope  string
	secret string
	// TargetSystem names the place this credential may write to. A credential for one place is
	// not a credential for another, which is the narrowest confinement the interface allows.
	TargetSystem string
}

// NewCredential builds a delivery credential. It refuses any scope but DeliveryScope, so a
// caller cannot widen the exception by passing a broader one.
func NewCredential(targetSystem, scope, secret string) (*Credential, error) {
	if scope != DeliveryScope {
		return nil, fmt.Errorf("%w (got %q)", ErrCredentialScope, scope)
	}
	if targetSystem == "" {
		return nil, ErrNoTarget
	}
	return &Credential{scope: scope, secret: secret, TargetSystem: targetSystem}, nil
}

// Scope is the credential's scope, always DeliveryScope.
func (c *Credential) Scope() string {
	if c == nil {
		return ""
	}
	return c.scope
}

// Secret hands the secret to a Sink that is about to deliver, and to nothing else.
//
// `purpose` is checked rather than logged: a caller that asks for the secret for any purpose but
// delivering gets ErrCredentialNotForReading. This is the runtime half of the type-level
// separation, and it is what the constitution test exercises.
func (c *Credential) Secret(purpose string) (string, error) {
	if c == nil {
		return "", ErrNoTarget
	}
	if purpose != DeliveryScope {
		return "", fmt.Errorf("%w (asked for %q)", ErrCredentialNotForReading, purpose)
	}
	return c.secret, nil
}

// String redacts. A credential that printed itself would end up in a log line, an error message
// and eventually a ticket.
func (c *Credential) String() string {
	if c == nil {
		return "<no delivery credential>"
	}
	return fmt.Sprintf("report-delivery credential for %s (scope %s, secret redacted)",
		c.TargetSystem, c.scope)
}

// DeliveryRequest is one report on its way to the place the incident lives.
type DeliveryRequest struct {
	// InvestigationID is the investigation the rendering belongs to.
	InvestigationID string
	// TargetSystem and TargetRef are the stable identifier of the place.
	TargetSystem string
	TargetRef    string
	// Rendering is the report body: the FR-057c order with the FR-057d deep links, and nothing
	// else. It is the *only* payload field; there is nowhere to put a command.
	Rendering string
	// PreviousMessageRef is what to edit in place. Empty on the first delivery.
	PreviousMessageRef string
	// At is the delivery instant. Zero means now.
	At time.Time
}

// Identity is the delivery's canonical identity: one message per (investigation, target system,
// target ref). It is the same string `store.DeliveryIDFor` uses for the ledger row, and a test in
// the store package asserts the two agree — the row and the message it describes have to be the
// same thing, or "one target, one message" is true of the ledger and false of the chat thread.
//
// The investigation id is part of it on purpose. Two investigations of the same incident channel
// are two reports; keying on the target alone would make the second edit the first's message and
// silently delete an answer a human had already read (T116).
func (r DeliveryRequest) Identity() string {
	return DeliveryIdentity(r.InvestigationID, r.TargetSystem, r.TargetRef)
}

// DeliveryIdentity builds the canonical identity of a delivery.
func DeliveryIdentity(investigationID, targetSystem, targetRef string) string {
	return "delivery-" + investigationID + "-" + targetSystem + "-" + targetRef
}

// RenderingDigest is sha256 of the rendering that was delivered, stored so that "what did the
// on-call actually see?" is answerable after the report has been edited five times.
func (r DeliveryRequest) RenderingDigest() string {
	sum := sha256.Sum256([]byte(r.Rendering))
	return hex.EncodeToString(sum[:])
}

// DeliveryResult is what one delivery attempt produced. A failure is a result, not an error:
// nothing about it may propagate into the investigation's lifecycle (FR-057f).
type DeliveryResult struct {
	// Outcome is OutcomeDelivered, OutcomeFailed or OutcomeNotConfigured.
	Outcome string
	// ExternalMessageRef is what to edit next time. Carried forward on a success.
	ExternalMessageRef string
	// RenderingDigest is the digest of what was delivered.
	RenderingDigest string
	// FailureDetail says why, on a failure.
	FailureDetail string
	// At is when the attempt happened.
	At time.Time
}

// Sink is the transport. It is an interface with exactly one method and no lifecycle, because
// the transport is a connector concern and this feature defines what is returned, not how it
// travels (FR-057f).
//
// An implementation must edit `PreviousMessageRef` in place when one is given, and must return
// the reference to edit next time. It must not throw: a transport failure is a DeliveryResult
// with Outcome "failed", so that a caller cannot accidentally let a chat outage fail an
// investigation.
type Sink interface {
	Deliver(ctx context.Context, cred *Credential, req DeliveryRequest) DeliveryResult
}

// LogSink is the v1 transport: a **recorded no-op driver**.
//
// It writes what it would have posted to the structured log and returns a stable synthetic
// message reference, so the whole edit-in-place path — first delivery mints a reference, later
// deliveries carry it and increment the update count — is exercised end to end with nothing
// leaving the process. Shipping a real transport is a connector's job (ADR-0005 D8).
//
// It also keeps the last rendering per target in memory, which is what makes "edited in place"
// observable in a test: Last(target) returns what a reader of that place would currently see.
type LogSink struct {
	logger *slog.Logger

	mu   sync.Mutex
	last map[string]string
}

// NewLogSink returns the v1 sink. A nil logger uses slog.Default().
func NewLogSink(logger *slog.Logger) *LogSink {
	if logger == nil {
		logger = slog.Default()
	}
	return &LogSink{logger: logger, last: map[string]string{}}
}

var _ Sink = (*LogSink)(nil)

// Deliver records the report and posts nothing.
func (s *LogSink) Deliver(_ context.Context, cred *Credential, req DeliveryRequest) DeliveryResult {
	at := req.At
	if at.IsZero() {
		at = time.Now().UTC()
	}
	result := DeliveryResult{RenderingDigest: req.RenderingDigest(), At: at.UTC()}

	if req.TargetSystem == "" || req.TargetRef == "" {
		result.Outcome = OutcomeNotConfigured
		result.FailureDetail = ErrNoTarget.Error()
		return result
	}
	// The credential is consulted even though nothing is posted, so that a misconfigured scope
	// fails here rather than on the day a real transport is wired in.
	if _, err := cred.Secret(DeliveryScope); err != nil {
		result.Outcome = OutcomeFailed
		result.FailureDetail = err.Error()
		return result
	}

	// The key is the delivery's canonical identity, not the target alone: two investigations of
	// the same channel are two messages (T116).
	key := req.Identity()
	ref := req.PreviousMessageRef
	if ref == "" {
		// A reference derived from the identity rather than minted at random, so that a
		// re-delivery that has forgotten the previous reference still edits the one message
		// instead of posting a second one.
		ref = "log-sink:" + key
	}

	s.mu.Lock()
	s.last[key] = req.Rendering
	s.mu.Unlock()

	s.logger.Info("report delivery (log sink: nothing was posted)",
		"investigation", req.InvestigationID,
		"target_system", req.TargetSystem,
		"target_ref", req.TargetRef,
		"message_ref", ref,
		"edited_in_place", req.PreviousMessageRef != "",
		"rendering_digest", result.RenderingDigest,
		"rendering_bytes", len(req.Rendering))

	result.Outcome = OutcomeDelivered
	result.ExternalMessageRef = ref
	return result
}

// Last returns what a reader of that place would currently see for one investigation. It exists
// so that "edited in place, not re-posted" is assertable: after two deliveries there is one body,
// not two. It takes the investigation id because the message is per (investigation, target).
func (s *LogSink) Last(investigationID, targetSystem, targetRef string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	body, ok := s.last[DeliveryIdentity(investigationID, targetSystem, targetRef)]
	return body, ok
}

// Messages is how many distinct messages this sink is holding. One investigation delivered five
// times is one message; that is the whole of FR-057f's "not re-posted as a stream".
func (s *LogSink) Messages() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.last)
}

// Deliver runs one delivery through a sink, never failing the caller (FR-057f).
//
// The signature is the contract: it returns a DeliveryResult and no error. A caller therefore
// *cannot* write `if err := Deliver(...); err != nil { return err }` and accidentally make
// delivery a precondition of anything.
func Deliver(ctx context.Context, sink Sink, cred *Credential, req DeliveryRequest) (out DeliveryResult) {
	at := req.At
	if at.IsZero() {
		at = time.Now().UTC()
	}
	if sink == nil {
		return DeliveryResult{
			Outcome:         OutcomeNotConfigured,
			RenderingDigest: req.RenderingDigest(),
			FailureDetail:   "no report-delivery connector is configured; the report was not returned to the place the incident lives",
			At:              at.UTC(),
		}
	}
	if cred == nil {
		return DeliveryResult{
			Outcome:         OutcomeNotConfigured,
			RenderingDigest: req.RenderingDigest(),
			FailureDetail:   "no report-delivery credential is configured",
			At:              at.UTC(),
		}
	}
	// A credential is minted for one place. Using it to write to another is the widening the
	// constitution forbids, and it is checked here — before the transport sees the secret —
	// rather than left to each connector to remember (T116).
	if cred.TargetSystem != req.TargetSystem {
		return DeliveryResult{
			Outcome:         OutcomeNotConfigured,
			RenderingDigest: req.RenderingDigest(),
			FailureDetail: fmt.Sprintf("%s: it is scoped to %q and this report is for %q",
				ErrCredentialTarget.Error(), cred.TargetSystem, req.TargetSystem),
			At: at.UTC(),
		}
	}
	// A panicking connector is a connector bug and must still not fail an investigation. The
	// recovered value becomes a *recorded failure* rather than a zero-valued result: an empty
	// outcome is one `store.RecordDelivery` refuses to write, so a connector bug would have cost
	// the ledger its record of what happened (T116).
	defer func() {
		if r := recover(); r != nil {
			out = DeliveryResult{
				Outcome:         OutcomeFailed,
				RenderingDigest: req.RenderingDigest(),
				FailureDetail:   fmt.Sprintf("the report-delivery connector panicked: %v", r),
				At:              at.UTC(),
			}
		}
	}()
	result := sink.Deliver(ctx, cred, req)
	if result.RenderingDigest == "" {
		result.RenderingDigest = req.RenderingDigest()
	}
	if result.At.IsZero() {
		result.At = at.UTC()
	}
	return result
}

// SinkLog is the id of the v1 transport, the recorded no-op driver.
const SinkLog = "log"

// PublishedSinks lists every transport this build admits, in the order an operator sees them.
//
// It is a list rather than a plugin mechanism. A transport is a connector, a connector is
// reviewed, and a build that has not had one reviewed into it has exactly one: the log sink that
// posts nothing (ADR-0005 D8).
func PublishedSinks() []string { return []string{SinkLog} }

// NewSink resolves a sink id to a transport, or refuses.
//
// There is deliberately no default. An id this build does not publish — a typo, a stale config, a
// connector that was removed — is ErrUnknownSink, never "the one that happened to be compiled
// in": silently falling back would mean a report going somewhere nobody chose, which is precisely
// the write the constitution confines (T116, constitution VII).
func NewSink(id string, logger *slog.Logger) (Sink, error) {
	switch id {
	case SinkLog:
		return NewLogSink(logger), nil
	default:
		return nil, fmt.Errorf("%w %q; this build publishes %s", ErrUnknownSink, id,
			strings.Join(PublishedSinks(), ", "))
	}
}

// DeliveryBody is the rendering that travels: the published order, with a header naming the
// investigation so a reader of the place knows what they are looking at.
//
// It is the human rendering and nothing more. The machine rendering stays in the tool: a chat
// thread is not a place for canonical JSON, and sending it there would widen what the exception
// carries.
func DeliveryBody(report *Report) (string, error) {
	body, err := report.Human()
	if err != nil {
		return "", err
	}
	id := report.Investigation.GetInvestigationId()
	header := fmt.Sprintf("# investigation %s\n\n", id)
	if report.Investigation.GetLifecycle().String() != "" {
		header = fmt.Sprintf("# investigation %s (%s)\n\n", id,
			strings.ToLower(report.Investigation.GetLifecycle().String()))
	}
	return header + body, nil
}
