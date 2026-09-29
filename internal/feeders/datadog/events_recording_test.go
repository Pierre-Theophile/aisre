// SPDX-License-Identifier: Apache-2.0

package datadog_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	ddfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/datadog"
	"github.com/Pierre-Theophile/aisre/internal/feeders/deployrecord"
	"github.com/Pierre-Theophile/aisre/internal/sanitise"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/emit"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/source"
)

// A live events run recorded through the tee (T092; FR-072–FR-075, FR-137). Canaries seeded where a real
// event carries people, infrastructure names and free text never survive, raw or hashed; the actor's name
// is dropped and not pseudonymised (a people identifier survives in no form); and the recording still
// derives the change, its actor kind and its deploy claims from the sanitised payload alone.
func TestAnEventsRecordingDropsThePeopleAndStillDerivesTheChange(t *testing.T) {
	t.Parallel()
	material := make([]byte, sanitise.KeyBytes)
	for i := range material {
		material[i] = byte(i*17 + 5)
	}
	key, err := sanitise.NewKey(material)
	if err != nil {
		t.Fatal(err)
	}
	canaries := sanitise.NewCanarySet(key)
	seeded, err := canaries.Seed("datadog.events", sanitise.CanaryPerson, sanitise.CanaryInfrastructure, sanitise.CanaryFreeText)
	if err != nil {
		t.Fatal(err)
	}
	person, infra, free := seeded[0].Carrier, seeded[1].Carrier, seeded[2].Carrier
	san, err := sanitise.New(sanitise.ContractPolicy(), key)
	if err != nil {
		t.Fatal(err)
	}
	san = san.WithCanaries(canaries)

	// An event as Datadog returns it, with the fields the feeder never reads carrying the canaries too.
	data := []map[string]any{{
		"id": "evt-7001", "type": "event",
		"attributes": map[string]any{
			"timestamp": "2026-09-21T14:09:30Z",
			"tags": []string{"env:prod", "service:" + infra, "git.commit.sha:" + evCommitA, "image:" + evImage,
				"version:v2.4.0", "user:" + person, "triggered_by:ci", "note:" + free, "cost_center:cc-12"},
			"message": "Deployed by " + person + ". " + free,
			"attributes": map[string]any{
				"title": "Deployed " + infra + " for " + person, "source_type_name": "jenkins", "service": infra,
				"evt": map[string]any{"id": "9", "name": free, "type": "deployment"},
				"usr": map[string]any{"email": person, "name": free},
			},
		},
	}}
	raw, err := json.Marshal(map[string]any{"data": data, "meta": map[string]any{"page": map[string]any{"after": "c-1"}}})
	if err != nil {
		t.Fatal(err)
	}
	marker := eventsPoll(t, hm(14, 21), ddfeeder.EventsMarker{Pages: 1, Read: 4, OutOfScope: 3, Reason: "would quote " + free})
	opts := changesOptions()
	opts.Changes = ddfeeder.ChangeScope{Sources: []string{"jenkins", "org-private-tool"}, Tags: []string{"event_type:deployment", "team:" + infra}}

	dir := t.TempDir()
	tee, err := deployrecord.NewTee(source.NewSliceSource([]feeder.Payload{
		{Kind: ddfeeder.PayloadEvents, At: hm(14, 20), Bytes: raw}, marker,
	}), ddfeeder.Kind, san, dir)
	if err != nil {
		t.Fatal(err)
	}
	tee.WithPrepare(ddfeeder.PreparePayload(san))
	live, err := ddfeeder.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	liveEvents := emit.NewMemoryEmitter(live.Describe())
	if err := live.Run(context.Background(), tee, liveEvents); err != nil {
		t.Fatalf("the run ended: %v", err)
	}
	if tee.Written() != 2 {
		t.Fatalf("%d payloads written; refused: %v", tee.Written(), tee.Dropped())
	}
	assertCleanDir(t, dir, canaries)

	// The live run saw everything the event stated.
	liveChange := eventChanges(liveEvents)
	if len(liveChange) != 1 || liveChange[0].GetChange().GetActor() != person {
		t.Fatalf("the live change %v", liveChange)
	}

	// The recording's change is derived from the sanitised bytes alone.
	shadowOpts, err := ddfeeder.PseudonymousOptions(san, opts)
	if err != nil {
		t.Fatal(err)
	}
	shadow, err := ddfeeder.New(shadowOpts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tee.Finish(context.Background(), dir, "datadog-events", shadow); err != nil {
		t.Fatal(err)
	}
	replay, err := source.NewFileSource(dir)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := ddfeeder.New(shadowOpts)
	if err != nil {
		t.Fatal(err)
	}
	em := emit.NewMemoryEmitter(fresh.Describe())
	if err := fresh.Run(context.Background(), replay, em); err != nil {
		t.Fatal(err)
	}
	got := eventChanges(em)
	if len(got) != 1 {
		t.Fatalf("the recording derives %d changes, want 1", len(got))
	}
	c := got[0]
	if c.GetChange().GetActor() != "" || c.GetChange().GetActorKind() != graphv1.ActorKind_AUTOMATION {
		t.Errorf("the recorded actor is %q of kind %s; the name must be gone and the kind (from the trigger tag) kept",
			c.GetChange().GetActor(), c.GetChange().GetActorKind())
	}
	if c.GetChange().GetKind() != graphv1.ChangeKind_ROLLOUT || !c.GetValidAt().AsTime().Equal(hms(14, 9, 30)) {
		t.Errorf("the recorded change %v", c)
	}
	keys := correlationsOf(em)
	if keys[feeder.NSDeployCommitSHA] == "" || strings.Contains(keys[feeder.NSDeployCommitSHA], evCommitA) {
		t.Errorf("the recorded commit claim %q: it must exist and be the commit's pseudonym, not the commit", keys[feeder.NSDeployCommitSHA])
	}
	if _, isCommit := feeder.CommitSHA(strings.Fields(keys[feeder.NSDeployCommitSHA])[0]); !isCommit {
		t.Errorf("the recorded commit claim %q is not a commit id, so C8 could not join on it", keys[feeder.NSDeployCommitSHA])
	}
	if _, kept := keys[feeder.NSDeployImage]; kept {
		t.Error("an image reference survived the recording; its registry path names an organisation")
	}
	if keys[feeder.NSDeployRelease] != "v2.4.0 env="+mustIdentifier(t, san, sanitise.KindEnvironment, "prod") {
		t.Errorf("release claim %q", keys[feeder.NSDeployRelease])
	}
	// The scope, in the recording's vocabulary: the organisation's own source word is pseudonymised.
	if notes := checkpointNotes(em); strings.Contains(notes, "org-private-tool") || !strings.Contains(notes, "jenkins") {
		t.Errorf("the recorded scope:\n%s", notes)
	}
}

func mustIdentifier(t *testing.T, san *sanitise.Sanitiser, kind sanitise.Kind, value string) string {
	t.Helper()
	out, err := san.Identifier(kind, value)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
