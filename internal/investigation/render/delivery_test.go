// SPDX-License-Identifier: Apache-2.0

package render_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/internal/investigation/render"
)

// The report-delivery contract (T087, FR-057f, ADR-0005 D8, constitution VII v1.1.0).
//
// This is the project's only write outside its own stores, so the tests are about the
// confinement rather than about the happy path. Each one names the clause of Principle VII it
// holds to.

func TestDeliveryCredentialCannotBeUsedForReading(t *testing.T) {
	t.Parallel()

	cred, err := render.NewCredential("slack:acme", render.DeliveryScope, "s3cr3t")
	if err != nil {
		t.Fatalf("new credential: %v", err)
	}

	// "It MUST use a credential scoped to writing that report and nothing else, separate from
	// every read credential" (constitution VII). Asking it for anything but delivering fails.
	for _, purpose := range []string{"graph:read", "telemetry:read", "reader", ""} {
		if _, err := cred.Secret(purpose); !errors.Is(err, render.ErrCredentialNotForReading) {
			t.Errorf("Secret(%q) = %v, want ErrCredentialNotForReading", purpose, err)
		}
	}
	if _, err := cred.Secret(render.DeliveryScope); err != nil {
		t.Errorf("Secret(%q) = %v, want the secret", render.DeliveryScope, err)
	}

	// A credential minted with a wider scope is refused outright, so the exception cannot be
	// widened by configuration.
	for _, scope := range []string{"read", "write", "admin", "report:write report:read", ""} {
		if _, err := render.NewCredential("slack:acme", scope, "s3cr3t"); !errors.Is(
			err, render.ErrCredentialScope) {
			t.Errorf("NewCredential(scope=%q) = %v, want ErrCredentialScope", scope, err)
		}
	}
}

func TestDeliveryCredentialNeverPrintsItsSecret(t *testing.T) {
	t.Parallel()

	cred, err := render.NewCredential("slack:acme", render.DeliveryScope, "s3cr3t-do-not-log-me")
	if err != nil {
		t.Fatalf("new credential: %v", err)
	}
	if strings.Contains(cred.String(), "s3cr3t-do-not-log-me") {
		t.Errorf("the credential printed its secret: %q", cred.String())
	}
	if !strings.Contains(cred.String(), "redacted") {
		t.Errorf("String() = %q, want it to say the secret is redacted", cred.String())
	}
}

// failingSink is a connector that is down.
type failingSink struct{ calls int }

func (s *failingSink) Deliver(context.Context, *render.Credential, render.DeliveryRequest) render.DeliveryResult {
	s.calls++
	return render.DeliveryResult{
		Outcome:       render.OutcomeFailed,
		FailureDetail: "the chat connector returned 503",
	}
}

// panickingSink is a connector with a bug.
type panickingSink struct{}

func (panickingSink) Deliver(context.Context, *render.Credential, render.DeliveryRequest) render.DeliveryResult {
	panic("connector bug")
}

// TestDeliveryFailureIsAResultNotAnError is the clause that matters most: delivery "MUST NOT be a
// precondition of reaching a terminal state — a delivery that fails is recorded and the
// investigation still concludes" (FR-057f).
//
// The guarantee is in the signature: Deliver returns no error, so a caller cannot write
// `if err := Deliver(...); err != nil { return err }` and accidentally make a chat outage fail an
// investigation.
func TestDeliveryFailureIsAResultNotAnError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cred, err := render.NewCredential("slack:acme", render.DeliveryScope, "s3cr3t")
	if err != nil {
		t.Fatalf("new credential: %v", err)
	}
	req := render.DeliveryRequest{
		InvestigationID: "inv-01", TargetSystem: "slack:acme", TargetRef: "C0123",
		Rendering: "## verdict\n\nshop/payments@rev7 caused it\n",
	}

	sink := &failingSink{}
	result := render.Deliver(ctx, sink, cred, req)
	if result.Outcome != render.OutcomeFailed {
		t.Fatalf("outcome = %q, want %q", result.Outcome, render.OutcomeFailed)
	}
	if result.FailureDetail == "" {
		t.Error("a failed delivery must record why (FR-057f)")
	}
	if result.RenderingDigest == "" {
		t.Error("the digest of what was attempted is not recorded")
	}

	// A connector that panics must not take the investigation with it.
	panicked := render.Deliver(ctx, panickingSink{}, cred, req)
	_ = panicked // the assertion is that the call returned at all

	// Nothing configured is `not_configured`, which is not a failure of anything.
	none := render.Deliver(ctx, nil, cred, req)
	if none.Outcome != render.OutcomeNotConfigured {
		t.Errorf("with no sink, outcome = %q, want %q", none.Outcome, render.OutcomeNotConfigured)
	}
	noCred := render.Deliver(ctx, render.NewLogSink(nil), nil, req)
	if noCred.Outcome != render.OutcomeNotConfigured {
		t.Errorf("with no credential, outcome = %q, want %q", noCred.Outcome, render.OutcomeNotConfigured)
	}
}

// TestLogSinkEditsInPlaceRatherThanReposting is "updated **in place** as the investigation
// progresses and concludes, rather than re-posted as a stream of messages" (FR-057f).
func TestLogSinkEditsInPlaceRatherThanReposting(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cred, err := render.NewCredential("slack:acme", render.DeliveryScope, "s3cr3t")
	if err != nil {
		t.Fatalf("new credential: %v", err)
	}
	sink := render.NewLogSink(nil)

	first := render.Deliver(ctx, sink, cred, render.DeliveryRequest{
		InvestigationID: "inv-01", TargetSystem: "slack:acme", TargetRef: "C0123",
		Rendering: "## verdict\n\nprovisional: prior-only ranking\n",
		At:        time.Date(2026, 9, 1, 14, 33, 0, 0, time.UTC),
	})
	if first.Outcome != render.OutcomeDelivered {
		t.Fatalf("first delivery = %+v", first)
	}
	if first.ExternalMessageRef == "" {
		t.Fatal("the first delivery minted no external reference to edit next time")
	}

	second := render.Deliver(ctx, sink, cred, render.DeliveryRequest{
		InvestigationID: "inv-01", TargetSystem: "slack:acme", TargetRef: "C0123",
		Rendering:          "## verdict\n\nshop/payments@rev7 caused it\n",
		PreviousMessageRef: first.ExternalMessageRef,
		At:                 time.Date(2026, 9, 1, 14, 35, 0, 0, time.UTC),
	})
	if second.ExternalMessageRef != first.ExternalMessageRef {
		t.Errorf("the second delivery minted a new reference %q; FR-057f edits %q in place",
			second.ExternalMessageRef, first.ExternalMessageRef)
	}
	if second.RenderingDigest == first.RenderingDigest {
		t.Error("the rendering digest did not change although the report did")
	}

	// A reader of that place sees one body, not two.
	body, ok := sink.Last("inv-01", "slack:acme", "C0123")
	if !ok {
		t.Fatal("the sink recorded nothing")
	}
	if strings.Contains(body, "provisional") {
		t.Errorf("the place still shows the provisional report; the later delivery did not "+
			"replace it:\n%s", body)
	}
	if !strings.Contains(body, "shop/payments@rev7") {
		t.Errorf("the place does not show the concluded report:\n%s", body)
	}
}

// TestLogSinkIsARecordedNoOp is the v1 transport: nothing leaves the process, and the whole
// edit-in-place path is exercised anyway (ADR-0005 D8).
func TestLogSinkIsARecordedNoOp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cred, err := render.NewCredential("slack:acme", render.DeliveryScope, "s3cr3t")
	if err != nil {
		t.Fatalf("new credential: %v", err)
	}
	sink := render.NewLogSink(nil)
	result := render.Deliver(ctx, sink, cred, render.DeliveryRequest{
		InvestigationID: "inv-01", TargetSystem: "", TargetRef: "",
		Rendering: "## verdict\n",
	})
	if result.Outcome != render.OutcomeNotConfigured {
		t.Errorf("a delivery with no target = %q, want %q", result.Outcome, render.OutcomeNotConfigured)
	}
}

// TestDeliveryBodyCarriesTheRenderingOrderAndTheDeepLinks is FR-057f: "The posting MUST carry the
// rendering order of FR-057c and the deep links of FR-057d".
func TestDeliveryBodyCarriesTheRenderingOrderAndTheDeepLinks(t *testing.T) {
	t.Parallel()

	report := syntheticReport(t)
	report.Investigation.Lifecycle = investigationv1.Lifecycle_CONCLUDED
	body, err := render.DeliveryBody(report)
	if err != nil {
		t.Fatalf("delivery body: %v", err)
	}
	if !strings.Contains(body, "# investigation inv-render-01") {
		t.Errorf("the body does not name the investigation:\n%s", firstLines(body, 3))
	}
	previous := -1
	for _, s := range render.SectionOrder {
		idx := strings.Index(body, "## "+s)
		if idx <= previous {
			t.Fatalf("the delivered body does not carry the published order: %q out of place", s)
		}
		previous = idx
	}
	if !strings.Contains(body, "https://metrics.invalid/") {
		t.Error("the delivered body carries no deep link (FR-057d)")
	}

	// And it carries only the rendering. There is nowhere in DeliveryRequest to put a command,
	// and DeliveryBody produces the human form and nothing else — the machine form, with its
	// internal ids and digests, stays in the tool.
	if strings.Contains(body, "\"investigationId\"") {
		t.Error("the delivered body contains the machine rendering; a chat thread is not a place " +
			"for canonical JSON")
	}
}

// TestDeliveryBodyIsRefusedWhenTheGuardRefusesTheRendering keeps the two rules composed: a report
// that proposes a remediation is not delivered anywhere, because it is not rendered at all.
func TestDeliveryBodyIsRefusedWhenTheGuardRefusesTheRendering(t *testing.T) {
	t.Parallel()

	report := syntheticReport(t)
	report.Narrative = "Next steps: roll back shop/payments to rev6."
	if _, err := render.DeliveryBody(report); !errors.Is(err, render.ErrRemediationProposed) {
		t.Fatalf("error = %v, want the body refused (FR-028)", err)
	}
}

// --- T116, the security pass on the first write path -----------------------------------------

// TestOneInvestigationOneMessagePerTarget is the half of "one target, one message" that the
// original LogSink got wrong: it keyed the remembered message on the target alone, so a second
// investigation of the same incident channel edited the first one's message and deleted an answer
// a human had already read. The message is per (investigation, target), exactly as the ledger row
// is (FR-057f, T116).
func TestOneInvestigationOneMessagePerTarget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cred, err := render.NewCredential("slack:acme", render.DeliveryScope, "s3cr3t")
	if err != nil {
		t.Fatalf("new credential: %v", err)
	}
	sink := render.NewLogSink(nil)

	first := render.Deliver(ctx, sink, cred, render.DeliveryRequest{
		InvestigationID: "inv-01", TargetSystem: "slack:acme", TargetRef: "C0123",
		Rendering: "## verdict\n\nthe first investigation's answer\n",
	})
	second := render.Deliver(ctx, sink, cred, render.DeliveryRequest{
		InvestigationID: "inv-02", TargetSystem: "slack:acme", TargetRef: "C0123",
		Rendering: "## verdict\n\nthe second investigation's answer\n",
	})
	if first.ExternalMessageRef == second.ExternalMessageRef {
		t.Errorf("two investigations share one message reference %q; the second would edit the "+
			"first's report away", first.ExternalMessageRef)
	}
	if got := sink.Messages(); got != 2 {
		t.Errorf("the place holds %d messages, want 2 (one per investigation)", got)
	}
	body, ok := sink.Last("inv-01", "slack:acme", "C0123")
	if !ok || !strings.Contains(body, "first investigation") {
		t.Errorf("the first investigation's report was overwritten: %q", body)
	}
}

// TestRedeliveryEditsInPlaceWithoutRememberedState is the other half. A caller that has lost the
// previous reference — a fresh `investigate report --deliver` in a new process, which is the
// ordinary case until a transport connector persists the ledger row — must still edit the one
// message rather than post a second. The reference is therefore derived from the delivery's
// identity, not minted at random (T116).
func TestRedeliveryEditsInPlaceWithoutRememberedState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cred, err := render.NewCredential("slack:acme", render.DeliveryScope, "s3cr3t")
	if err != nil {
		t.Fatalf("new credential: %v", err)
	}
	req := render.DeliveryRequest{
		InvestigationID: "inv-01", TargetSystem: "slack:acme", TargetRef: "C0123",
		Rendering: "## verdict\n\nprovisional\n",
	}
	firstProcess := render.Deliver(ctx, render.NewLogSink(nil), cred, req)

	req.Rendering = "## verdict\n\nconcluded\n"
	req.PreviousMessageRef = "" // a second process, with nothing remembered
	secondProcess := render.Deliver(ctx, render.NewLogSink(nil), cred, req)

	if firstProcess.ExternalMessageRef != secondProcess.ExternalMessageRef {
		t.Errorf("a re-delivery with no remembered reference minted a new message %q instead of "+
			"editing %q", secondProcess.ExternalMessageRef, firstProcess.ExternalMessageRef)
	}
	if want := "log-sink:" + req.Identity(); firstProcess.ExternalMessageRef != want {
		t.Errorf("message reference = %q, want it derived from the delivery identity %q",
			firstProcess.ExternalMessageRef, want)
	}
}

// TestAPanickingConnectorIsARecordedFailure: a connector bug must not fail an investigation, and
// it must not cost the ledger its record either. Before T116 the recover left the zero result —
// outcome "", which `store.RecordDelivery` refuses to write — so the one thing a panicking
// connector produced was an unrecordable result and an empty failure line (FR-057f).
func TestAPanickingConnectorIsARecordedFailure(t *testing.T) {
	t.Parallel()

	cred, err := render.NewCredential("slack:acme", render.DeliveryScope, "s3cr3t")
	if err != nil {
		t.Fatalf("new credential: %v", err)
	}
	result := render.Deliver(context.Background(), panickingSink{}, cred, render.DeliveryRequest{
		InvestigationID: "inv-01", TargetSystem: "slack:acme", TargetRef: "C0123",
		Rendering: "## verdict\n",
	})
	if result.Outcome != render.OutcomeFailed {
		t.Errorf("a panicking connector produced outcome %q, want %q — an outcome the ledger "+
			"cannot store is not a record of anything", result.Outcome, render.OutcomeFailed)
	}
	if result.FailureDetail == "" {
		t.Error("a failed delivery must record why (FR-057f)")
	}
	if result.RenderingDigest == "" {
		t.Error("the digest of what was attempted is not recorded")
	}
	if result.At.IsZero() {
		t.Error("the attempt has no time")
	}
}

// TestACredentialForOnePlaceIsNotACredentialForAnother: the credential type has always claimed
// this in its doc comment; T116 made it a check. A credential minted for one chat workspace must
// not write into another, because "scoped to writing that report and nothing else" is a statement
// about a place as much as about a verb (constitution VII).
func TestACredentialForOnePlaceIsNotACredentialForAnother(t *testing.T) {
	t.Parallel()

	cred, err := render.NewCredential("slack:acme", render.DeliveryScope, "s3cr3t")
	if err != nil {
		t.Fatalf("new credential: %v", err)
	}
	sink := render.NewLogSink(nil)
	result := render.Deliver(context.Background(), sink, cred, render.DeliveryRequest{
		InvestigationID: "inv-01", TargetSystem: "jira:other-company", TargetRef: "OPS-1",
		Rendering: "## verdict\n",
	})
	if result.Outcome == render.OutcomeDelivered {
		t.Fatal("a credential for slack:acme wrote into jira:other-company")
	}
	if !strings.Contains(result.FailureDetail, "scoped to") {
		t.Errorf("failure detail = %q, want it to name both places", result.FailureDetail)
	}
	if sink.Messages() != 0 {
		t.Error("the transport was reached although the credential was for another place")
	}
}

// TestSinkRegistryAdmitsOnlyThePublishedSinks: an unknown transport id is refused, never
// defaulted to whichever sink happens to be compiled in (T116).
func TestSinkRegistryAdmitsOnlyThePublishedSinks(t *testing.T) {
	t.Parallel()

	if got := render.PublishedSinks(); len(got) != 1 || got[0] != render.SinkLog {
		t.Fatalf("published sinks = %v, want exactly [%s]; a new one needs a review and a line "+
			"in docs/security", got, render.SinkLog)
	}
	sink, err := render.NewSink(render.SinkLog, nil)
	if err != nil || sink == nil {
		t.Fatalf("NewSink(%q) = %v, %v", render.SinkLog, sink, err)
	}
	for _, id := range []string{"", "slack", "Log", "log ", "webhook", "../log"} {
		sink, err := render.NewSink(id, nil)
		if !errors.Is(err, render.ErrUnknownSink) {
			t.Errorf("NewSink(%q) = %v, want ErrUnknownSink", id, err)
		}
		if sink != nil {
			t.Errorf("NewSink(%q) returned a transport anyway", id)
		}
	}
}

// TestLogSinkNeverLogsTheSecret: the delivery credential is never written to the log, which is
// the one place a no-op transport does write (constitution VII, FR-038).
func TestLogSinkNeverLogsTheSecret(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	const secret = "xoxb-9999-do-not-log-me"
	cred, err := render.NewCredential("slack:acme", render.DeliveryScope, secret)
	if err != nil {
		t.Fatalf("new credential: %v", err)
	}
	render.Deliver(context.Background(), render.NewLogSink(logger), cred, render.DeliveryRequest{
		InvestigationID: "inv-01", TargetSystem: "slack:acme", TargetRef: "C0123",
		Rendering: "## verdict\n\nshop/payments@rev7\n",
	})
	if buf.Len() == 0 {
		t.Fatal("the recorded no-op driver recorded nothing")
	}
	if strings.Contains(buf.String(), secret) {
		t.Errorf("the delivery credential reached the log:\n%s", buf.String())
	}
	// Nor does the body of the report end up in the log line: a chat report is a rendering of
	// telemetry, and the log is not the place for it (FR-038).
	if strings.Contains(buf.String(), "shop/payments@rev7") {
		t.Errorf("the rendered report body reached the log:\n%s", buf.String())
	}
}
