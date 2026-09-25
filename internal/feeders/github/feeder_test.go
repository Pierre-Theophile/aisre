// SPDX-License-Identifier: Apache-2.0

package github_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/feeders/github"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The poll cycle (004 US1; FR-003, FR-019, FR-056, FR-057, FR-073).

type payloadSource struct{ payloads []feeder.Payload }

func (s *payloadSource) Next(context.Context) (feeder.Payload, error) {
	if len(s.payloads) == 0 {
		return feeder.Payload{}, io.EOF
	}
	next := s.payloads[0]
	s.payloads = s.payloads[1:]
	return next, nil
}

type capturingEmitter struct {
	events      []*graphv1.EventEnvelope
	checkpoints int
	gapBefore   []bool
	flushed     int
	refuse      map[string]bool
	notes       []string
	extents     [][2]time.Time
}

func (e *capturingEmitter) Emit(_ context.Context, ev *graphv1.EventEnvelope) (*graphv1.IngestResult, error) {
	e.events = append(e.events, ev)
	if e.refuse[ev.GetEventId()] {
		return &graphv1.IngestResult{
			EventId: ev.GetEventId(), Status: graphv1.IngestResult_REJECTED,
			ReasonCode: "telemetry_value", ReasonDetail: "no",
		}, nil
	}
	return &graphv1.IngestResult{Status: graphv1.IngestResult_APPLIED}, nil
}

func (e *capturingEmitter) Checkpoint(_ context.Context, fact feeder.CheckpointFact) error {
	e.checkpoints++
	e.gapBefore = append(e.gapBefore, fact.GapBefore)
	// The note is captured, because a double that dropped it would let the feeder drop it too — which
	// is exactly what happened until T043.
	e.notes = append(e.notes, fact.Note)
	e.extents = append(e.extents, [2]time.Time{fact.ExtentFrom, fact.ExtentTo})
	return nil
}

func (e *capturingEmitter) Flush(context.Context) error { e.flushed++; return nil }

type refusingGate struct{ err error }

func (g refusingGate) Prove(context.Context) (github.GateResult, error) {
	if g.err != nil {
		return github.GateResult{}, g.err
	}
	return github.GateResult{
		Evidence: feeder.EvidencePlatformReported,
		Scope:    feeder.CredentialScope{PlatformEnforced: true, Selection: "selected"},
	}, nil
}

var pollAt = time.Date(2026, 3, 1, 14, 30, 0, 0, time.UTC)

func newFeeder(t *testing.T) *github.Feeder {
	t.Helper()
	f, err := github.New(github.Options{OrgSlug: "acme", Map: options()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return f
}

// The payloads for one production rollout of a mapped repository.
func rolloutPayloads(t *testing.T, outcome string) []feeder.Payload {
	t.Helper()
	scope := `{"total_count":1,"repository_selection":"selected","repositories":[
		{"id":556,"name":"monorepo","full_name":"acme/monorepo","private":true,"owner":{"login":"acme"}}]}`
	deployments := fmt.Sprintf(`[{"id":4321,"sha":%q,"ref":"main","task":"deploy",
		"environment":"production","production_environment":true,
		"created_at":"2026-03-01T14:00:00Z","updated_at":"2026-03-01T14:03:12Z",
		"creator":{"login":"ada","id":1,"type":"User"},
		"url":"https://api.github.com/repos/acme/monorepo/deployments/4321",
		"statuses_url":"https://api.github.com/repos/acme/monorepo/deployments/4321/statuses"}]`, shippedSHA)
	statuses := `{"repository":"acme/monorepo","deployment_id":4321,"statuses":[
		{"id":2,"state":"success","environment":"production","created_at":"2026-03-01T14:03:12Z",
		 "creator":{"login":"ada","id":1,"type":"User"},
		 "log_url":"https://deploy.example/logs?token=SECRET"},
		{"id":1,"state":"in_progress","environment":"production","created_at":"2026-03-01T14:01:30Z",
		 "creator":{"login":"ada","id":1,"type":"User"}}]}`
	runs := fmt.Sprintf(`{"total_count":1,"workflow_runs":[
		{"id":99,"name":"Deploy","run_number":12,"run_attempt":1,"head_sha":%q,"event":"push",
		 "status":"completed","conclusion":"success","run_started_at":"2026-03-01T14:00:00Z",
		 "actor":{"login":"ada","id":1,"type":"User"},
		 "triggering_actor":{"login":"ada","id":1,"type":"User"},
		 "html_url":"https://github.com/acme/monorepo/actions/runs/99",
		 "logs_url":"https://api.github.com/repos/acme/monorepo/actions/runs/99/logs"}]}`, shippedSHA)

	at := time.Date(2026, 3, 1, 14, 5, 0, 0, time.UTC)
	return []feeder.Payload{
		{Kind: github.PayloadInstallationRepositories, At: at, Bytes: []byte(scope)},
		{Kind: github.PayloadWorkflowRuns, At: at, Bytes: []byte(runs)},
		{Kind: github.PayloadDeployments, At: at, Bytes: []byte(deployments)},
		{Kind: github.PayloadDeploymentStatuses, At: at, Bytes: []byte(statuses)},
		{Kind: github.PayloadPollMarker, At: pollAt, Bytes: []byte(`{"outcome":"` + outcome + `"}`)},
	}
}

// FR-003: the gate runs before anything is emitted, and a refusal emits nothing at all.
func TestARefusedGateEmitsNothing(t *testing.T) {
	t.Parallel()
	f := newFeeder(t)
	f.Gate = refusingGate{err: errors.New("deployments=write")}
	em := &capturingEmitter{}

	err := f.Run(context.Background(), &payloadSource{payloads: rolloutPayloads(t, "complete")}, em)
	if err == nil {
		t.Fatal("a refused gate returned success")
	}
	if !strings.Contains(err.Error(), "nothing was emitted") {
		t.Errorf("the refusal does not say nothing was emitted: %v", err)
	}
	if len(em.events) != 0 {
		t.Errorf("%d events were emitted after the gate refused", len(em.events))
	}
	if em.checkpoints != 0 {
		t.Errorf("%d checkpoints were written after the gate refused", em.checkpoints)
	}
}

// The whole cycle: scope, runs, deployment, statuses, then the poll marker releases it.
func TestAPollReleasesTheDeploymentItsStatusesResolved(t *testing.T) {
	t.Parallel()
	f := newFeeder(t)
	f.Gate = refusingGate{}
	em := &capturingEmitter{}

	if err := f.Run(context.Background(), &payloadSource{payloads: rolloutPayloads(t, "complete")}, em); err != nil {
		t.Fatalf("Run: %v", err)
	}

	changes := changesOf(em.events)
	if len(changes) != 2 {
		t.Fatalf("the poll emitted %d changes, want one per mapped target", len(changes))
	}
	for _, change := range changes {
		if !change.GetValidAt().AsTime().Equal(time.Date(2026, 3, 1, 14, 3, 12, 0, time.UTC)) {
			t.Errorf("ValidAt = %v, want the success status's instant", change.GetValidAt().AsTime())
		}
		if change.GetChange().GetActorKind() != graphv1.ActorKind_PERSON {
			t.Errorf("actor kind = %v, want PERSON: a push by a user account",
				change.GetChange().GetActorKind())
		}
		// The run was matched by head sha, so the origin link is the run's page.
		if got := change.GetChange().GetOriginRef(); got != "https://github.com/acme/monorepo/actions/runs/99" {
			t.Errorf("OriginRef = %q, want the run this deployment's commit came from", got)
		}
	}
	if em.checkpoints != 1 {
		t.Errorf("%d checkpoints, want one per poll", em.checkpoints)
	}
	if em.flushed == 0 {
		t.Error("Run returned without flushing; a feeder must flush before it returns")
	}
	if f.Deferred() != 0 {
		t.Errorf("%d deployments deferred though every one resolved", f.Deferred())
	}
	if f.Scope().Selection != "selected" {
		t.Errorf("the scope reports %q, want the regime the gate stated (FR-008)", f.Scope().Selection)
	}
}

// A deployer-supplied log URL carrying a token never becomes a pointer. The status payload above has
// `?token=SECRET` on its log_url, which is exactly where one arrives in practice.
func TestADeployerSuppliedTokenNeverReachesTheGraph(t *testing.T) {
	t.Parallel()
	f := newFeeder(t)
	f.Gate = refusingGate{}
	em := &capturingEmitter{}
	if err := f.Run(context.Background(), &payloadSource{payloads: rolloutPayloads(t, "complete")}, em); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, event := range em.events {
		raw, err := json.Marshal(event)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if strings.Contains(string(raw), "SECRET") {
			t.Fatalf("a deployer-supplied token reached the graph in %s: %s", event.GetEventId(), raw)
		}
		if strings.Contains(string(raw), "deploy.example") {
			t.Errorf("a deployer-supplied URL became a pointer in %s; it points wherever that person's "+
				"tool pointed it, and this connector addresses objects in GitHub's API",
				event.GetEventId())
		}
	}
}

// FR-073: a deployment whose statuses have not arrived is DEFERRED, not excluded. One is work not done
// yet and the other is work decided against, and a checkpoint that conflated them would report a
// connector out of budget as one that found nothing.
func TestADeploymentWithNoStatusesYetIsDeferredNotExcluded(t *testing.T) {
	t.Parallel()
	f := newFeeder(t)
	f.Gate = refusingGate{}
	em := &capturingEmitter{}

	payloads := rolloutPayloads(t, "complete")
	// Drop the statuses payload: the deployment is observed and its transitions are not.
	var without []feeder.Payload
	for _, p := range payloads {
		if p.Kind == github.PayloadDeploymentStatuses {
			continue
		}
		without = append(without, p)
	}
	if err := f.Run(context.Background(), &payloadSource{payloads: without}, em); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := changesOf(em.events); len(got) != 0 {
		t.Errorf("%d changes were emitted for a deployment whose statuses never arrived; one without "+
			"the other would date every rollout at its request and mark it successful before it was",
			len(got))
	}
	if f.Deferred() != 1 {
		t.Errorf("Deferred() = %d, want 1", f.Deferred())
	}
	if total := f.ExcludedTotal(); total != 0 {
		t.Errorf("%d exclusions were counted for deferred work: %v; a deferral is work not done YET",
			total, f.Excluded())
	}
	// The checkpoint is still written: the window WAS read, and a poll that found nothing resolvable is
	// not a poll that did not happen.
	if em.checkpoints != 1 {
		t.Errorf("%d checkpoints, want one: the window was read", em.checkpoints)
	}
}

// FR-056: a partial poll makes the NEXT checkpoint declare a gap, so the silence before it reads as
// ignorance rather than absence.
func TestAPartialPollMakesTheNextCheckpointDeclareAGap(t *testing.T) {
	t.Parallel()
	f := newFeeder(t)
	f.Gate = refusingGate{}
	em := &capturingEmitter{}

	first := rolloutPayloads(t, "partial")
	second := []feeder.Payload{
		{Kind: github.PayloadPollMarker, At: pollAt.Add(10 * time.Minute), Bytes: []byte(`{"outcome":"complete"}`)},
	}
	if err := f.Run(context.Background(), &payloadSource{payloads: append(first, second...)}, em); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(em.gapBefore) != 2 {
		t.Fatalf("%d checkpoints, want 2", len(em.gapBefore))
	}
	if em.gapBefore[0] {
		t.Error("the first checkpoint declares a gap before it, and nothing said the window before it " +
			"was unread")
	}
	if !em.gapBefore[1] {
		t.Error("the checkpoint after a PARTIAL poll does not declare a gap; the window it follows was " +
			"read incompletely, and without the flag the silence reads as absence (FR-056)")
	}
}

// A payload kind this feeder does not read is an error, not a skip: a fixture directory naming one
// would verify nothing while appearing to pass.
func TestAnUnknownPayloadKindIsAnError(t *testing.T) {
	t.Parallel()
	f := newFeeder(t)
	f.Gate = refusingGate{}
	em := &capturingEmitter{}

	err := f.Run(context.Background(), &payloadSource{payloads: []feeder.Payload{
		{Kind: "issues", At: pollAt, Bytes: []byte(`[]`)},
	}}, em)
	if err == nil {
		t.Fatal("an unknown payload kind was skipped")
	}
	if !strings.Contains(err.Error(), "would verify nothing") {
		t.Errorf("the refusal does not say why it matters: %v", err)
	}
}

// A deployment in a repository the grant does not carry is counted, not dropped: it means the scope
// payload and the deployment payload disagree, which is worth seeing.
func TestADeploymentOutsideTheGrantIsCounted(t *testing.T) {
	t.Parallel()
	f := newFeeder(t)
	f.Gate = refusingGate{}
	em := &capturingEmitter{}

	payloads := rolloutPayloads(t, "complete")
	for i, p := range payloads {
		if p.Kind == github.PayloadInstallationRepositories {
			payloads[i].Bytes = []byte(`{"total_count":0,"repository_selection":"selected","repositories":[]}`)
		}
	}
	if err := f.Run(context.Background(), &payloadSource{payloads: payloads}, em); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := changesOf(em.events); len(got) != 0 {
		t.Errorf("%d changes were emitted for a repository outside the grant", len(got))
	}
	var found bool
	for reason := range f.Excluded() {
		if strings.Contains(reason, "grant does not carry") {
			found = true
		}
	}
	if !found {
		t.Errorf("the exclusion is %v, want it attributed to the grant disagreeing with the payload",
			f.Excluded())
	}
}

// An event the graph refuses stops the run. A rejection is an answer rather than a transport failure,
// and it is still a stop: it means this feeder built something the schema forbids.
func TestAnEventTheGraphRefusesStopsTheRun(t *testing.T) {
	t.Parallel()
	f := newFeeder(t)
	f.Gate = refusingGate{}

	// Learn the event ids from a clean run, then refuse the first change.
	probe := &capturingEmitter{}
	if err := f.Run(context.Background(), &payloadSource{payloads: rolloutPayloads(t, "complete")}, probe); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var firstChange string
	for _, event := range probe.events {
		if event.GetObserveChange() != nil {
			firstChange = event.GetEventId()
			break
		}
	}
	if firstChange == "" {
		t.Fatal("the clean run emitted no change to refuse")
	}

	again := newFeeder(t)
	again.Gate = refusingGate{}
	em := &capturingEmitter{refuse: map[string]bool{firstChange: true}}
	err := again.Run(context.Background(), &payloadSource{payloads: rolloutPayloads(t, "complete")}, em)
	if err == nil {
		t.Fatal("a refused event did not stop the run")
	}
	if !strings.Contains(err.Error(), "telemetry_value") {
		t.Errorf("the error does not carry the graph's reason code: %v", err)
	}
}

// Two runs of the same payloads emit the same events: nothing reads a clock, and no map iteration
// reaches a golden.
//
// THREE deployments released by one poll, because the held set is a map: with one deployment the order
// cannot vary and deleting the sort would still pass.
func TestTheCycleIsDeterministic(t *testing.T) {
	t.Parallel()
	var first []string
	for attempt := range 6 {
		f := newFeeder(t)
		f.Gate = refusingGate{}
		em := &capturingEmitter{}
		if err := f.Run(context.Background(), &payloadSource{payloads: manyRolloutPayloads(t)}, em); err != nil {
			t.Fatalf("Run: %v", err)
		}
		var ids []string
		for _, event := range em.events {
			ids = append(ids, event.GetEventId())
		}
		// Asserted against a STATED order rather than only run-to-run, because run-to-run equality is
		// probabilistic: Go randomises map iteration per range, so three keys coincide with sorted order
		// about one run in six and a whole comparison can pass by luck. The deployments arrive as
		// 7001, 5002, 6003 and must be released in id order.
		var releasedOrder []string
		for _, id := range ids {
			if !strings.Contains(id, ":change:") {
				continue
			}
			releasedOrder = append(releasedOrder, id)
		}
		want := []string{
			"github:acme:change:repositories/556/deployments/5002/targets/k8s.deployment/shop/catalogue",
			"github:acme:change:repositories/556/deployments/5002/targets/k8s.deployment/shop/checkout",
			"github:acme:change:repositories/556/deployments/6003/targets/k8s.deployment/shop/catalogue",
			"github:acme:change:repositories/556/deployments/6003/targets/k8s.deployment/shop/checkout",
			"github:acme:change:repositories/556/deployments/7001/targets/k8s.deployment/shop/catalogue",
			"github:acme:change:repositories/556/deployments/7001/targets/k8s.deployment/shop/checkout",
		}
		if strings.Join(releasedOrder, "\n") != strings.Join(want, "\n") {
			t.Fatalf("run %d released in this order:\n%s\nwant\n%s",
				attempt, strings.Join(releasedOrder, "\n"), strings.Join(want, "\n"))
		}
		if attempt == 0 {
			first = ids
			continue
		}
		if strings.Join(ids, "\n") != strings.Join(first, "\n") {
			t.Fatalf("run %d emitted a different order:\n%v\nwant\n%v", attempt, ids, first)
		}
	}
}

// A feeder with no organisation slug is refused: the source id would be `github:` and every
// organisation's events would be one source.
func TestAFeederWithNoOrgIsRefused(t *testing.T) {
	t.Parallel()
	if _, err := github.New(github.Options{}); err == nil {
		t.Error("a feeder with no organisation slug was built")
	}
}

// The description is what the harness compares a run against, so the namespaces it declares must cover
// what the run actually emits.
func TestTheDescriptionCoversEveryNamespaceTheRunEmits(t *testing.T) {
	t.Parallel()
	f := newFeeder(t)
	f.Gate = refusingGate{}
	em := &capturingEmitter{}
	if err := f.Run(context.Background(), &payloadSource{payloads: rolloutPayloads(t, "complete")}, em); err != nil {
		t.Fatalf("Run: %v", err)
	}
	declared := map[string]bool{}
	for _, ns := range f.Describe().Namespaces {
		declared[ns] = true
	}
	for _, event := range em.events {
		for _, ref := range refsOf(event) {
			if !declared[ref.GetNamespace()] {
				t.Errorf("the run emits the namespace %q, which Describe() does not declare; the testkit "+
					"fails such a run, and a namespace nothing declares is one nothing joins on",
					ref.GetNamespace())
			}
		}
	}
	if err := f.Describe().Validate(); err != nil {
		t.Errorf("Describe() does not validate: %v", err)
	}
}

func refsOf(event *graphv1.EventEnvelope) []*graphv1.Ref {
	var out []*graphv1.Ref
	if change := event.GetObserveChange(); change != nil {
		out = append(out, change.GetRef())
		out = append(out, change.GetTargets()...)
	}
	if claim := event.GetIdentityClaim(); claim != nil {
		out = append(out, claim.GetSubject(), claim.GetClaim())
	}
	if node := event.GetUpsertNode(); node != nil {
		out = append(out, node.GetRef())
	}
	return out
}

// manyRolloutPayloads is one poll releasing three deployments of the same repository, so that the order
// the held set is drained in is observable.
func manyRolloutPayloads(t *testing.T) []feeder.Payload {
	t.Helper()
	scope := `{"total_count":1,"repository_selection":"selected","repositories":[
		{"id":556,"name":"monorepo","full_name":"acme/monorepo","private":true,"owner":{"login":"acme"}}]}`

	var deployments []string
	var statusPayloads []feeder.Payload
	at := time.Date(2026, 3, 1, 14, 5, 0, 0, time.UTC)
	// Deliberately not in id order, so a drain that preserved arrival order and one that sorted would
	// differ.
	for _, id := range []int64{7001, 5002, 6003} {
		deployments = append(deployments, fmt.Sprintf(`{"id":%d,"sha":%q,"ref":"main","task":"deploy",
			"environment":"production","created_at":"2026-03-01T14:00:00Z",
			"creator":{"login":"ada","id":1,"type":"User"},
			"url":"https://api.github.com/repos/acme/monorepo/deployments/%d"}`, id, shippedSHA, id))
		statusPayloads = append(statusPayloads, feeder.Payload{
			Kind: github.PayloadDeploymentStatuses, At: at,
			Bytes: []byte(fmt.Sprintf(`{"repository":"acme/monorepo","deployment_id":%d,"statuses":[
				{"id":1,"state":"success","environment":"production","created_at":"2026-03-01T14:03:12Z",
				 "creator":{"login":"ada","id":1,"type":"User"}}]}`, id)),
		})
	}

	out := []feeder.Payload{
		{Kind: github.PayloadInstallationRepositories, At: at, Bytes: []byte(scope)},
		{Kind: github.PayloadDeployments, At: at,
			Bytes: []byte("[" + strings.Join(deployments, ",") + "]")},
	}
	out = append(out, statusPayloads...)
	return append(out, feeder.Payload{
		Kind: github.PayloadPollMarker, At: pollAt, Bytes: []byte(`{"outcome":"complete"}`),
	})
}

// And all three completing in the same second are each marked ambiguous rather than ordered by this
// connector (Edge case 11): the tie is computed over the whole released set, not per deployment.
func TestDeploymentsSharingAnInstantAreAllMarkedAmbiguous(t *testing.T) {
	t.Parallel()
	f := newFeeder(t)
	f.Gate = refusingGate{}
	em := &capturingEmitter{}
	if err := f.Run(context.Background(), &payloadSource{payloads: manyRolloutPayloads(t)}, em); err != nil {
		t.Fatalf("Run: %v", err)
	}
	changes := changesOf(em.events)
	if len(changes) != 6 {
		t.Fatalf("three deployments across two targets became %d changes, want 6", len(changes))
	}
	for _, change := range changes {
		if !change.GetProps().GetFields()[github.PropOrderingAmbiguous].GetBoolValue() {
			t.Errorf("%s is not marked ambiguous though three deployments completed in the same second; "+
				"which of two changes came first is the whole of a causal argument, and it is the one "+
				"thing an investigation must not have invented for it", change.GetRef().GetValue())
		}
	}
}

// The clock skew is observed and reported (004 T142, FR-058, FR-153).
//
// `pkg/feeder`'s Skew arrived with FR-058's threshold and its own tests, and NOTHING called Observe or
// Report. So 003's FR-153 — "the skew MUST be reported" — and 004's FR-058 were both satisfied by a
// measurement nobody took: the fourth time this project has found machinery that exists, is wired into an
// interface and is never reached, after C4, C5 and C7.
//
// The report is exported rather than only logged, which is what makes this assertable at all. A log line
// is not a measurement a test can read.
func TestTheCycleObservesGitHubsClockSkew(t *testing.T) {
	f := newFeeder(t)
	f.Gate = refusingGate{}
	em := &capturingEmitter{}
	// The rollout payloads state `created_at` of 2026-03-01T14:00:00Z. Delivering them a known interval
	// later makes the skew a known quantity rather than whatever the machine's clock says.
	payloads := rolloutPayloads(t, "complete")
	stated, err := time.Parse(time.RFC3339, "2026-03-01T14:00:00Z")
	if err != nil {
		t.Fatalf("parse the payload's own instant: %v", err)
	}
	arrival := stated.Add(3 * time.Minute)
	for i := range payloads {
		payloads[i].At = arrival
	}

	if err := f.Run(context.Background(), &payloadSource{payloads: payloads}, em); err != nil {
		t.Fatalf("run: %v", err)
	}

	got := f.Skew()
	if got.Samples == 0 {
		t.Fatal("no skew samples; FR-153 asks for the skew to be REPORTED and a mean of zero over no " +
			"samples is not a report")
	}
	if want := -3 * time.Minute; got.Last != want {
		t.Errorf("last skew = %s, want %s. The sign is kept rather than an absolute value: a vendor "+
			"running ahead makes facts appear to arrive from the future and one behind makes a poll look "+
			"like it missed something, and an absolute value hides which", got.Last, want)
	}
	if got.Threshold != feeder.DefaultSkewThreshold {
		t.Errorf("threshold = %s, want the published default %s", got.Threshold, feeder.DefaultSkewThreshold)
	}
	if !got.Beyond() {
		t.Errorf("a three-minute skew is not Beyond a %s threshold, so nothing would tell the operator",
			got.Threshold)
	}
}

// And nothing is corrected: the change's valid time is still the platform's instant, not the arrival.
//
// This is the assertion FR-058's one word turns on. A connector that "fixed" the skew would produce a
// graph that is subtly wrong rather than visibly skewed, and it would destroy the one property that makes
// a bitemporal graph worth having — that a reader can tell "it happened at 14:21" from "we found out at
// 14:38".
func TestTheSkewIsReportedAndNeverCorrected(t *testing.T) {
	f := newFeeder(t)
	f.Gate = refusingGate{}
	em := &capturingEmitter{}
	payloads := rolloutPayloads(t, "complete")
	arrival := time.Date(2026, 3, 1, 14, 30, 0, 0, time.UTC)
	for i := range payloads {
		payloads[i].At = arrival
	}
	if err := f.Run(context.Background(), &payloadSource{payloads: payloads}, em); err != nil {
		t.Fatalf("run: %v", err)
	}
	if f.Skew().Samples == 0 {
		t.Fatal("the skew was not observed, so this test is not checking what it claims")
	}

	var found bool
	for _, ev := range em.events {
		body := ev.GetObserveChange()
		if body == nil {
			continue
		}
		found = true
		if body.GetValidAt().AsTime().Equal(arrival) {
			t.Errorf("the change's valid time is the ARRIVAL instant %s; the skew was corrected into the "+
				"graph, which is what FR-058 forbids", arrival)
		}
	}
	if !found {
		t.Fatal("no change was emitted, so this test asserts nothing")
	}
}
