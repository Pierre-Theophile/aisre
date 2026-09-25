// SPDX-License-Identifier: Apache-2.0

package vercel_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	vercelfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/vercel"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/emit"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/source"
)

// The run loop, end to end (004 US2).
//
// The mapper's own tests prove what a deployment becomes. These prove the FEEDER calls it — the
// distinction this project keeps paying for, since a correct mapper nothing invokes emits nothing and
// every unit test still passes.

func runFeeder(t *testing.T, payloads []feeder.Payload) ([]*graphv1.EventEnvelope, error) {
	t.Helper()
	f, err := vercelfeeder.New(vercelfeeder.Options{
		OrgSlug: "twin",
		Targets: map[string]*graphv1.Ref{
			mapProject: feeder.Ref(feeder.NSK8sDeployment, "shop/storefront"),
		},
	})
	if err != nil {
		t.Fatalf("new feeder: %v", err)
	}
	mem := emit.NewMemoryEmitter(f.Describe())
	if err := f.Run(t.Context(), source.NewSliceSource(payloads), mem); err != nil {
		return nil, err
	}
	if rejected := mem.Rejected(); len(rejected) > 0 {
		t.Fatalf("%d event(s) refused; the first is %s (%s)",
			len(rejected), rejected[0].GetEventId(), rejected[0].GetReasonDetail())
	}
	return mem.Events(), nil
}

// facts drops the cycle's checkpoints, leaving what the cycle asserted about the estate. The tests that
// say "a preview emits nothing" mean no CHANGE and no key: every cycle does write a checkpoint, and must
// (004 T153), because a cycle that read Vercel and found nothing is a different fact from one that
// never ran. These tests passed for a while by counting the checkpoint's absence as correct.
func facts(events []*graphv1.EventEnvelope) []*graphv1.EventEnvelope {
	var out []*graphv1.EventEnvelope
	for _, event := range events {
		if event.GetSourceCheckpoint() == nil {
			out = append(out, event)
		}
	}
	return out
}

func payload(kind string, at time.Time, body string) feeder.Payload {
	return feeder.Payload{Kind: kind, At: at, Bytes: []byte(body)}
}

func deploymentsPayload(at time.Time, deployments ...string) feeder.Payload {
	body := `{"deployments":[`
	for i, d := range deployments {
		if i > 0 {
			body += ","
		}
		body += d
	}
	return payload(vercelfeeder.PayloadDeployments, at, body+`]}`)
}

func deploymentJSON(uid, target, substate, sha string) string {
	return fmt.Sprintf(`{"uid":%q,"name":"storefront","projectId":%q,"target":%q,
		"readyState":"READY","readySubstate":%q,"source":"git",
		"inspectorUrl":"https://vercel.com/twin/storefront/%s",
		"creator":{"uid":"usr_1","type":"user"},
		"attribution":{"commitMeta":{"githubCommitSha":%q}},
		"ready":1758465000000}`, uid, mapProject, target, substate, uid, sha)
}

// The projects read describes each project, and a promoted deployment becomes a change AND its
// correlation key, in that order.
func TestACycleEmitsTheChangeThenItsCorrelation(t *testing.T) {
	t.Parallel()
	events, err := runFeeder(t, []feeder.Payload{
		payload(vercelfeeder.PayloadProjects, mapAt,
			fmt.Sprintf(`{"projects":[{"id":%q,"name":"storefront","link":{"type":"github","org":"acme","repo":"storefront"}}]}`, mapProject)),
		deploymentsPayload(mapAt, deploymentJSON("dpl_42", "production", "PROMOTED", mapCommit)),
		payload(vercelfeeder.PayloadPollMarker, mapAt, `{"outcome":"complete"}`),
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	events = facts(events)
	if len(events) != 3 {
		t.Fatalf("got %d events, want the project, the change and its correlation: %v", len(events), events)
	}
	// The project the deployment belongs to, as a SERVICE node: without it the change's target is a ref
	// nothing describes, and C8, which reads targets from the graph, can never merge it (004 T093).
	project := events[0].GetUpsertNode()
	if project == nil || project.GetRef().GetNamespace() != "vercel.project" ||
		project.GetRef().GetValue() != mapProject || project.GetType() != graphv1.NodeType_SERVICE ||
		project.GetDisplayName() != "storefront" {
		t.Fatalf("the first event is not the project as a SERVICE node: %v", events[0])
	}
	if !project.GetValidFromUnknown() {
		t.Errorf("the payload states no createdAt, so the project's start must be unknown (FR-011), not the read")
	}
	events = events[1:]
	if events[0].GetObserveChange() == nil {
		t.Errorf("the first event is not the change: %T", events[0].GetBody())
	}
	if events[1].GetCorrelateEntity() == nil {
		t.Errorf("the second event is not the correlation: %T", events[1].GetBody())
	}
	// The repository came from the PROJECTS payload, not from the deployment — which is the reason the
	// two reads are separate.
	if repo := events[0].GetObserveChange().GetProps().GetFields()["sre.vercel.repository"].GetStringValue(); repo != "acme/storefront" {
		t.Errorf("the repository is %q; it is stated by the projects read and nothing else", repo)
	}
}

// A staged deployment emits NOTHING. This is the feeder-level half of T083.
func TestACycleEmitsNothingForAStagedDeployment(t *testing.T) {
	t.Parallel()
	events, err := runFeeder(t, []feeder.Payload{
		deploymentsPayload(mapAt, deploymentJSON("dpl_43", "production", "STAGED", mapCommit)),
		payload(vercelfeeder.PayloadPollMarker, mapAt, `{"outcome":"complete"}`),
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if events = facts(events); len(events) != 0 {
		t.Errorf("a staged deployment emitted %d event(s); READY is built and available, not serving: %v",
			len(events), events)
	}
}

// And a preview emits nothing either.
func TestACycleEmitsNothingForAPreview(t *testing.T) {
	t.Parallel()
	events, err := runFeeder(t, []feeder.Payload{
		deploymentsPayload(mapAt, deploymentJSON("dpl_44", "", "PROMOTED", mapCommit)),
		payload(vercelfeeder.PayloadPollMarker, mapAt, `{"outcome":"complete"}`),
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if events = facts(events); len(events) != 0 {
		t.Errorf("a preview emitted %d event(s): %v", len(events), events)
	}
}

// A payload kind this feeder does not read is an ERROR, not a skip.
//
// A fixture naming an unread kind would replay, report a pass, and assert less than its author believed.
func TestAnUnknownPayloadKindIsAnError(t *testing.T) {
	t.Parallel()
	_, err := runFeeder(t, []feeder.Payload{payload("aliases", mapAt, `{}`)})
	if err == nil {
		t.Error("an unread payload kind was accepted; a fixture naming it would verify nothing")
	}
}

// The description names every namespace this feeder writes into, including the operator's targets.
//
// A feeder that attached changes to `k8s.deployment` without declaring it would be writing into a
// namespace its own description says it does not touch.
func TestTheDescriptionDeclaresTheOperatorsTargetNamespace(t *testing.T) {
	t.Parallel()
	f, err := vercelfeeder.New(vercelfeeder.Options{
		OrgSlug: "twin",
		Targets: map[string]*graphv1.Ref{
			mapProject: feeder.Ref(feeder.NSK8sDeployment, "shop/storefront"),
		},
	})
	if err != nil {
		t.Fatalf("new feeder: %v", err)
	}
	desc := f.Describe()
	if err := desc.Validate(); err != nil {
		t.Fatalf("the description is invalid: %v", err)
	}
	if desc.SourceID != "vercel:twin" {
		t.Errorf("source id = %q, want vercel:twin", desc.SourceID)
	}
	var found bool
	for _, ns := range desc.Namespaces {
		if ns == feeder.NSK8sDeployment {
			found = true
		}
	}
	if !found {
		t.Errorf("namespaces %v omit the operator's target namespace", desc.Namespaces)
	}
	// And the window is non-zero, or the fixture shuffle permutes nothing and order-independence goes
	// untested.
	if desc.ReorderingWindow <= 0 {
		t.Error("the reordering window is zero, so a fixture would permute nothing")
	}
}

// A feeder with no org has no source id, and is refused rather than emitting under an empty one.
func TestAFeederNeedsAnOrg(t *testing.T) {
	t.Parallel()
	if _, err := vercelfeeder.New(vercelfeeder.Options{}); err == nil {
		t.Error("a feeder was built with no org slug")
	}
}

// A REJECTED event stops the cycle rather than being dropped.
//
// This needed an emitter that refuses, and it is worth saying why: with only the memory emitter, which
// accepts everything the feeder builds, deleting the REJECTED check changed no test at all. The guard
// was there and nothing exercised it — the shape this project keeps finding. A recording that continued
// past a refusal would replay to a different graph than the run that made it.
type rejectingEmitter struct{ desc feeder.Description }

func (r rejectingEmitter) Describe() feeder.Description { return r.desc }

func (r rejectingEmitter) Emit(context.Context, *graphv1.EventEnvelope) (*graphv1.IngestResult, error) {
	return &graphv1.IngestResult{
		Status:       graphv1.IngestResult_REJECTED,
		ReasonCode:   "invalid_argument",
		ReasonDetail: "the graph refused this",
	}, nil
}

func (r rejectingEmitter) Checkpoint(context.Context, feeder.CheckpointFact) error { return nil }
func (r rejectingEmitter) Flush(context.Context) error                             { return nil }

func TestARefusedEventStopsTheCycle(t *testing.T) {
	t.Parallel()
	f, err := vercelfeeder.New(vercelfeeder.Options{OrgSlug: "twin"})
	if err != nil {
		t.Fatalf("new feeder: %v", err)
	}
	err = f.Run(t.Context(), source.NewSliceSource([]feeder.Payload{
		deploymentsPayload(mapAt, deploymentJSON("dpl_45", "production", "PROMOTED", mapCommit)),
	}), rejectingEmitter{desc: f.Describe()})
	if err == nil {
		t.Fatal("the cycle continued past a refused event; the recording would replay to a different graph")
	}
	if !strings.Contains(err.Error(), "refused") {
		t.Errorf("the error does not say the graph refused the event: %v", err)
	}
}

// The clock skew is observed and reported (004 T142, FR-058).
//
// `pkg/feeder`'s Skew arrived with FR-058's threshold and its own tests, and NOTHING called Observe or
// Report — the fourth time this project has found machinery that exists, is wired into an interface,
// and is never reached, after C4, C5 and C7. So 003's FR-153 and 004's FR-058 were both satisfied by a
// measurement nobody took.
//
// These tests are what make the difference observable: a feeder that stopped observing would fail them
// rather than go on reporting a mean of zero over no samples.
func TestTheCycleObservesTheClockSkew(t *testing.T) {
	t.Parallel()
	f, err := vercelfeeder.New(vercelfeeder.Options{OrgSlug: "twin"})
	if err != nil {
		t.Fatalf("new feeder: %v", err)
	}
	// The payload arrives two minutes after Vercel says the build was ready, so the skew is a known
	// quantity rather than whatever the machine's clock happens to say.
	arrival := mapAt
	ready := arrival.Add(-2 * time.Minute)
	deployment := fmt.Sprintf(`{"uid":"dpl_50","name":"storefront","projectId":%q,"target":"production",
		"readyState":"READY","readySubstate":"PROMOTED","source":"git",
		"creator":{"uid":"usr_1","type":"user"},"ready":%d}`, mapProject, ready.UnixMilli())

	mem := emit.NewMemoryEmitter(f.Describe())
	if err := f.Run(t.Context(), source.NewSliceSource([]feeder.Payload{
		payload(vercelfeeder.PayloadDeployments, arrival, `{"deployments":[`+deployment+`]}`),
		payload(vercelfeeder.PayloadPollMarker, arrival, `{"outcome":"complete"}`),
	}), mem); err != nil {
		t.Fatalf("run: %v", err)
	}

	got := f.Skew()
	if got.Samples != 1 {
		t.Fatalf("samples = %d, want 1; a mean of zero over no samples is not a measurement", got.Samples)
	}
	// The vendor's clock reads BEHIND this process's, so the skew is negative. Both directions are kept
	// rather than an absolute value: a vendor ahead makes facts appear to arrive from the future, and one
	// behind makes a poll look like it missed something.
	if want := -2 * time.Minute; got.Last != want {
		t.Errorf("last skew = %s, want %s", got.Last, want)
	}
	if got.Threshold != feeder.DefaultSkewThreshold {
		t.Errorf("threshold = %s, want the published default %s", got.Threshold, feeder.DefaultSkewThreshold)
	}
	// Two minutes is past the one-minute threshold, so this must be reportable rather than merely recorded.
	if !got.Beyond() {
		t.Errorf("a two-minute skew is not Beyond a %s threshold; the operator would never be told",
			got.Threshold)
	}
}

// Every deployment is observed, not only the promoted ones.
//
// A preview's timestamps come off the same platform clock, so measuring only the rollouts would measure
// the skew of a subset chosen by a rule that has nothing to do with clocks — and on an estate that
// deploys mostly previews, that subset is small enough to say very little.
func TestSkewIsObservedOnExcludedDeploymentsToo(t *testing.T) {
	t.Parallel()
	f, err := vercelfeeder.New(vercelfeeder.Options{OrgSlug: "twin"})
	if err != nil {
		t.Fatalf("new feeder: %v", err)
	}
	arrival := mapAt
	ready := arrival.Add(-90 * time.Second)
	// A preview and a staged production build: neither becomes a change.
	preview := fmt.Sprintf(`{"uid":"dpl_p","projectId":%q,"target":"","readyState":"READY",
		"readySubstate":"PROMOTED","creator":{"type":"user"},"ready":%d}`, mapProject, ready.UnixMilli())
	staged := fmt.Sprintf(`{"uid":"dpl_s","projectId":%q,"target":"production","readyState":"READY",
		"readySubstate":"STAGED","creator":{"type":"user"},"ready":%d}`, mapProject, ready.UnixMilli())

	mem := emit.NewMemoryEmitter(f.Describe())
	if err := f.Run(t.Context(), source.NewSliceSource([]feeder.Payload{
		payload(vercelfeeder.PayloadDeployments, arrival, `{"deployments":[`+preview+`,`+staged+`]}`),
		payload(vercelfeeder.PayloadPollMarker, arrival, `{"outcome":"complete"}`),
	}), mem); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := facts(mem.Events()); len(got) != 0 {
		t.Fatalf("this cycle should emit no graph fact; it emitted %d", len(got))
	}
	if got := f.Skew(); got.Samples != 2 {
		t.Errorf("samples = %d, want 2: both deployments state a platform instant, and excluding them "+
			"would measure the skew of whichever subset the rollout rule happened to admit", got.Samples)
	}
}

// A deployment stating no platform instant is not observed as a zero-skew sample.
//
// Counting it would drag the mean towards zero with a reading nobody took, which is worse than a smaller
// sample: it would make a skewed clock look less skewed the more incomplete the payloads were.
//
// Where the guarantee actually lives is worth stating, because this test does not prove what it looks
// like it proves. `Skew.Observe` ignores a zero instant itself, so the feeder's own `if ok` is defence in
// depth rather than the thing being relied on — a probe that removes it changes no behaviour and this
// test still passes. What the test does lock is the property at the level that matters, the CYCLE: this
// payload goes in and no sample comes out, whichever layer refuses it.
func TestADeploymentWithNoPlatformInstantIsNotASample(t *testing.T) {
	{
		t.Parallel()
		f, err := vercelfeeder.New(vercelfeeder.Options{OrgSlug: "twin"})
		if err != nil {
			t.Fatalf("new feeder: %v", err)
		}
		mem := emit.NewMemoryEmitter(f.Describe())
		if err := f.Run(t.Context(), source.NewSliceSource([]feeder.Payload{
			payload(vercelfeeder.PayloadDeployments, mapAt,
				`{"deployments":[{"uid":"dpl_x","target":"production","readySubstate":"PROMOTED"}]}`),
			payload(vercelfeeder.PayloadPollMarker, mapAt, `{"outcome":"complete"}`),
		}), mem); err != nil {
			t.Fatalf("run: %v", err)
		}
		if got := f.Skew(); got.Samples != 0 {
			t.Errorf("samples = %d, want 0; a payload stating no instant counted as a zero-skew reading "+
				"would make a skewed clock look better the more incomplete the payloads were", got.Samples)
		}
	}
}

// envPayload is a recorded `GET /projects/{id}/env` response, carrying the project id because the real
// response does not (see applyProjectEnv).
func envPayload(at time.Time, envs ...string) feeder.Payload {
	body := `{"projectId":"` + mapProject + `","envs":[` + strings.Join(envs, ",") + `]}`
	return payload(vercelfeeder.PayloadProjectEnv, at, body)
}

// envJSON is one variable's metadata, written as the platform sends it — WITH the value fields, because
// that is what arrives and the point is that nothing here keeps them.
func envJSON(id, key string, createdAt, updatedAt time.Time) string {
	updated := ""
	if !updatedAt.IsZero() {
		updated = fmt.Sprintf(`,"updatedAt":%d,"updatedBy":"usr_grace"`, updatedAt.UnixMilli())
	}
	return fmt.Sprintf(`{"id":%q,"key":%q,"target":["production"],"type":"encrypted",
		"createdBy":"usr_ada","createdAt":%d%s,
		"value":%q,"legacyValue":"AQICAHh7ZmFrZQ==","decrypted":false,"securityIssues":[]}`,
		id, key, createdAt.UnixMilli(), updated, theSecret)
}

// FR-039 and T099/T100: a configuration change and a rollout are two changes, and neither is folded into
// the other.
//
// The cycle reads the configuration FIRST and the deployment second, which is the order that would let a
// "hold the config until a rollout applies it" implementation look correct — the rollout is right there,
// one payload later. Both changes come out, each dated by its own platform instant.
func TestAConfigChangeAndARolloutAreTwoChangesNeitherFoldedIntoTheOther(t *testing.T) {
	t.Parallel()
	editedAt := time.Date(2026, 9, 21, 3, 10, 0, 0, time.UTC)
	events, err := runFeeder(t, []feeder.Payload{
		envPayload(mapAt, envJSON("icfg_payments_url", "PAYMENTS_API_URL", editedAt, time.Time{})),
		deploymentsPayload(mapAt, deploymentJSON("dpl_42", "production", "PROMOTED", mapCommit)),
		payload(vercelfeeder.PayloadPollMarker, mapAt, `{"outcome":"complete"}`),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var config, rollout *graphv1.ObserveChange
	for _, event := range events {
		change := event.GetObserveChange()
		if change == nil {
			continue
		}
		switch change.GetChange().GetKind() {
		case graphv1.ChangeKind_CONFIG_CHANGE:
			config = change
		case graphv1.ChangeKind_ROLLOUT:
			rollout = change
		}
	}
	if config == nil {
		t.Fatal("no CONFIG_CHANGE: the configuration read produced nothing, so US3 is not wired")
	}
	if rollout == nil {
		t.Fatal("no ROLLOUT: the precondition for 'neither folded into the other' is gone")
	}
	// Two changes at two refs. One change carrying both facts would be the folding FR-039 forbids.
	if config.GetRef().GetValue() == rollout.GetRef().GetValue() {
		t.Errorf("the configuration change and the rollout share the ref %q, so one was folded into "+
			"the other (FR-039)", config.GetRef().GetValue())
	}
	// The config change is dated 03:10 whatever the rollout's instant is.
	if !config.GetValidAt().AsTime().Equal(editedAt) {
		t.Errorf("the configuration change is valid at %v, want the platform's 03:10. Delaying it until "+
			"the rollout applied it is exactly what FR-039 forbids", config.GetValidAt().AsTime())
	}
	// And the rollout carries no configuration property, which is the other direction of the same fold.
	for key := range rollout.GetProps().GetFields() {
		if strings.HasPrefix(key, "sre.vercel.config_") {
			t.Errorf("the rollout carries %q; a configuration change is its own change, not an "+
				"attribute of the next deploy", key)
		}
	}
	// Nothing anywhere in the cycle carries the value, in any form. Asserted over the whole cycle here
	// rather than one event, because the feeder is where a payload's fields could reach a second event.
	for _, event := range events {
		if body := event.String(); strings.Contains(body, theSecret) ||
			strings.Contains(body, "hunter2") || strings.Contains(body, "AQICAHh7ZmFrZQ==") {
			t.Errorf("event %s carries the variable's value or its ciphertext (FR-038, SC-006)",
				event.GetEventId())
		}
	}
}

// A configuration change with NO deployment in the cycle at all is still emitted. The mirror of the test
// above: FR-039's "independently of when a deployment next applied it" includes "never".
func TestAConfigChangeIsEmittedWithNoDeploymentInTheCycle(t *testing.T) {
	t.Parallel()
	editedAt := time.Date(2026, 9, 21, 3, 10, 0, 0, time.UTC)
	events, err := runFeeder(t, []feeder.Payload{
		envPayload(mapAt, envJSON("icfg_payments_url", "PAYMENTS_API_URL", editedAt, time.Time{})),
		payload(vercelfeeder.PayloadPollMarker, mapAt, `{"outcome":"complete"}`),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var changes int
	for _, event := range events {
		if event.GetObserveChange().GetChange().GetKind() == graphv1.ChangeKind_CONFIG_CHANGE {
			changes++
		}
	}
	if changes != 1 {
		t.Fatalf("the cycle emitted %d configuration changes with no deployment present, want 1; a "+
			"config change must not wait for a rollout that may never come (FR-039)", changes)
	}
}

// T101 at the feeder level: the preview-only variable is refused, and the refusal is counted rather than
// dropped on the floor.
func TestAPreviewOnlyVariableProducesNoChangeInACycle(t *testing.T) {
	t.Parallel()
	editedAt := time.Date(2026, 9, 21, 3, 10, 0, 0, time.UTC)
	preview := fmt.Sprintf(`{"id":"icfg_preview_flag","key":"PREVIEW_FLAG","target":["preview"],
		"type":"plain","createdBy":"usr_ada","createdAt":%d,"value":"on","securityIssues":[]}`,
		editedAt.UnixMilli())
	events, err := runFeeder(t, []feeder.Payload{
		envPayload(mapAt, preview),
		payload(vercelfeeder.PayloadPollMarker, mapAt, `{"outcome":"complete"}`),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, event := range events {
		if event.GetObserveChange().GetChange().GetKind() == graphv1.ChangeKind_CONFIG_CHANGE {
			t.Errorf("a preview-only variable produced a configuration change (%s)", event.GetEventId())
		}
	}
}

// Every cycle writes a checkpoint, including one that emitted nothing (FR-056, FR-057; 004 T153).
//
// The cycle here reads only previews, so it emits no change at all — which is exactly the case the
// checkpoint exists for. Without one, the graph cannot tell "Vercel was read and nothing reached
// production" from "Vercel was never read", and an investigation would conclude the first when the
// truth was the second. This feeder shipped without a checkpoint; the test is what stops that
// returning.
func TestEveryCycleWritesACheckpointEvenWhenNothingIsEmitted(t *testing.T) {
	t.Parallel()
	events, err := runFeeder(t, []feeder.Payload{
		deploymentsPayload(mapAt, deploymentJSON("dpl_preview", "", "PROMOTED", mapCommit)),
		payload(vercelfeeder.PayloadPollMarker, mapAt.Add(time.Minute), `{"outcome":"complete"}`),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var checkpoints []*graphv1.SourceCheckpoint
	for _, event := range events {
		if cp := event.GetSourceCheckpoint(); cp != nil {
			checkpoints = append(checkpoints, cp)
		}
		if event.GetObserveChange() != nil {
			t.Errorf("a preview produced a change: %s", event.GetEventId())
		}
	}
	if len(checkpoints) != 1 {
		t.Fatalf("the cycle wrote %d checkpoints, want 1", len(checkpoints))
	}
	cp := checkpoints[0]
	if !cp.GetExtentFrom().AsTime().Equal(mapAt) || !cp.GetExtentTo().AsTime().Equal(mapAt.Add(time.Minute)) {
		t.Errorf("extent = [%s, %s), want the cycle's first payload to its poll marker",
			cp.GetExtentFrom().AsTime(), cp.GetExtentTo().AsTime())
	}
	if cp.GetGapBefore() {
		t.Error("the first complete cycle declared a gap before it")
	}
	// The note states the filter in force and what it refused, so "nothing" reads as a measurement.
	for _, want := range []string{"poll=complete", "environment=production", "excluded_preview=1"} {
		if !strings.Contains(cp.GetNote(), want) {
			t.Errorf("checkpoint note %q does not state %q", cp.GetNote(), want)
		}
	}
}

// A partial poll makes the NEXT checkpoint declare the gap: the tail it did not reach was not
// watched, so the silence before the next extent is ignorance (FR-056).
func TestAPartialPollMakesTheNextCheckpointDeclareAGap(t *testing.T) {
	t.Parallel()
	events, err := runFeeder(t, []feeder.Payload{
		deploymentsPayload(mapAt, deploymentJSON("dpl_1", "production", "PROMOTED", mapCommit)),
		payload(vercelfeeder.PayloadPollMarker, mapAt, `{"outcome":"partial","reason":"rate limited"}`),
		payload(vercelfeeder.PayloadPollMarker, mapAt.Add(10*time.Minute), `{"outcome":"complete"}`),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var gaps []bool
	var notes []string
	for _, event := range events {
		if cp := event.GetSourceCheckpoint(); cp != nil {
			gaps = append(gaps, cp.GetGapBefore())
			notes = append(notes, cp.GetNote())
		}
	}
	if len(gaps) != 2 {
		t.Fatalf("got %d checkpoints, want one per poll marker", len(gaps))
	}
	if gaps[0] || !gaps[1] {
		t.Errorf("gap_before = %v, want [false true]: the gap follows the partial poll", gaps)
	}
	if !strings.Contains(notes[0], `reason="rate limited"`) {
		t.Errorf("the partial poll's note %q does not carry its reason", notes[0])
	}
}
