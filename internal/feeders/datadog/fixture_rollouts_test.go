// SPDX-License-Identifier: Apache-2.0

package datadog_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	ddfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/datadog"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/emit"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/source"
)

// datadog-log-rollout-01: rollouts seen only in a service's own logs (005 T069–T072).
//
// `worker` runs on virtual machines no feeder covers, and stamps the deployed commit with the `version`
// tag. Nothing but its logs knows when it deploys:
//
//   - 14:00: the commit it has run for weeks — first seen before the seven-day horizon, so it was
//     deployed before the connector could see it and no rollout is inferred;
//   - 15:00: a new commit first seen at 14:37:12 — one rollout, dated from that line and marked as a
//     bound; in the same interval a `v2.3.0` release label and an abbreviated `4c2e9ab` appear on a few
//     lines, and neither names a commit or an image, so both are counted and neither is a rollout;
//   - 16:00: a canary — two commits first seen two minutes apart — two rollouts, the overlap stated.

const rolloutFixture = "fixtures/datadog-log-rollout-01"

const (
	commitA = "1a2b3c4d5e6f708192a3b4c5d6e7f80918a2b3c4"
	commitB = "2b3c4d5e6f708192a3b4c5d6e7f80918a2b3c4d5"
	commitC = "3c4d5e6f708192a3b4c5d6e7f80918a2b3c4d5e6"
	commitD = "4d5e6f708192a3b4c5d6e7f80918a2b3c4d5e6f7"
)

func workerTick(t *testing.T, at time.Time, values ...ddfeeder.ValueSighting) feeder.Payload {
	t.Helper()
	m := ddfeeder.SourceMeasurement{Source: "production/worker", Lines: 20000, ErrorLines: 100, HostLines: 20000,
		Candidates: []ddfeeder.CandidateCount{{Label: "version (tag)", Lines: 20000, ErrorLines: 100}}, Values: values}
	raw, err := json.Marshal(ddfeeder.DiscoveryTick{LogSources: []string{"production/worker"},
		Window: &ddfeeder.DiscoveryWindow{From: at.Add(-time.Hour), To: at}, Measurements: []ddfeeder.SourceMeasurement{m}})
	if err != nil {
		t.Fatal(err)
	}
	return feeder.Payload{Kind: ddfeeder.PayloadDiscovery, At: at, Bytes: raw}
}

func rolloutPayloads(t *testing.T) []feeder.Payload {
	return []feeder.Payload{
		workerTick(t, hm(14, 0), ddfeeder.ValueSighting{Value: commitA, FirstSeen: hm(14, 0).Add(-7 * 24 * time.Hour), BeyondHorizon: true}),
		workerTick(t, hm(15, 0),
			ddfeeder.ValueSighting{Value: commitB, FirstSeen: hms(14, 37, 12)},
			ddfeeder.ValueSighting{Value: "v2.3.0", FirstSeen: hms(14, 41, 0)},
			ddfeeder.ValueSighting{Value: "4c2e9ab", FirstSeen: hms(14, 44, 30)}),
		workerTick(t, hm(16, 0),
			ddfeeder.ValueSighting{Value: commitC, FirstSeen: hms(15, 12, 40)},
			ddfeeder.ValueSighting{Value: commitD, FirstSeen: hms(15, 14, 2)}),
	}
}

func TestGenerateDatadogLogRolloutFixture(t *testing.T) {
	if os.Getenv(genFixturesEnv) == "" {
		t.Skipf("set %s=1 to regenerate %s", genFixturesEnv, rolloutFixture)
	}
	generateFixture(t, rolloutFixture, "datadog-rollouts",
		"Rollouts seen only in a service's own logs. `worker` runs on virtual machines no feeder covers and "+
			"stamps the deployed commit with the `version` tag. Its long-running commit was first seen before "+
			"the seven-day horizon and infers nothing; a new commit first seen at 14:37:12 is one ROLLOUT, "+
			"dated from that line and marked sre.change.valid_from_is_a_bound; a release label and an "+
			"abbreviated sha seen in the same interval name no commit or image and are counted, not inferred; "+
			"a canary's two commits, first seen two minutes apart, are two rollouts with the overlap stated.",
		ddfeeder.Options{OrgSlug: "twin"}, rolloutPayloads(t), hm(13, 59), hm(17, 0), `
queries:
  # What changed on worker between 14:00 and 17:00: three rollouts, each at its first log line.
  - name: what-changed-on-worker
    kind: diff
    focus: datadog.service=production/worker
    t1: 2026-09-21T14:00:00Z
    t2: 2026-09-21T17:00:00Z
    reference_at: 2026-09-21T16:00:00Z
    observed_at: 2026-09-21T17:00:00Z
    hops: 1
    direction: both
`)
}

func changes(em *emit.MemoryEmitter) []*graphv1.ObserveChange {
	var out []*graphv1.ObserveChange
	for _, ev := range em.Events() {
		if c := ev.GetObserveChange(); c != nil {
			out = append(out, c)
		}
	}
	return out
}

// T070: only commits and images become rollouts, each once, at its first line, marked a bound; the rest
// is counted.
func TestOnlyReferencedValuesBecomeRollouts(t *testing.T) {
	t.Parallel()
	em := run(t, ddfeeder.Options{}, rolloutPayloads(t)...)
	got := changes(em)
	if len(got) != 3 {
		t.Fatalf("%d rollouts, want 3: %v", len(got), got)
	}
	want := map[string]time.Time{commitB: hms(14, 37, 12), commitC: hms(15, 12, 40), commitD: hms(15, 14, 2)}
	for _, c := range got {
		value := c.GetProps().GetFields()[ddfeeder.PropChangeVersionValue].GetStringValue()
		if !c.GetValidAt().AsTime().Equal(want[value]) || c.GetChange().GetKind() != graphv1.ChangeKind_ROLLOUT ||
			c.GetProps().GetFields()[feeder.PropChangeValidFromIsABound].GetStringValue() != ddfeeder.BoundFirstSeenInLogs ||
			c.GetChange().GetActorKind() != graphv1.ActorKind_ACTOR_KIND_UNSPECIFIED ||
			len(c.GetTargets()) != 1 || c.GetTargets()[0].GetValue() != "production/worker" {
			t.Errorf("rollout %v", c)
		}
	}
	notes := checkpointNotes(em)
	for _, want := range []string{"before the first-seen horizon", `"v2.3.0" is a release form`,
		`"4c2e9ab" names no deploy reference (abbreviated sha)`, "2 versions first seen in one interval"} {
		if !strings.Contains(notes, want) {
			t.Errorf("the checkpoint does not state %q:\n%s", want, notes)
		}
	}
	var keys []string
	for _, ev := range em.Events() {
		if c := ev.GetCorrelateEntity(); c != nil && c.GetSubject().GetNamespace() == ddfeeder.NSChange {
			keys = append(keys, c.GetKey().GetNamespace()+"="+c.GetKey().GetValue())
		}
	}
	if len(keys) != 3 || keys[0] != feeder.NSDeployCommitSHA+"="+commitB {
		t.Errorf("correlation keys %v", keys)
	}
}

// T071: two runs over the same recorded interval, with no state carried between them, emit identical
// ids — the ids are built from Datadog-stated facts only.
func TestRolloutIDsNeedNoMemory(t *testing.T) {
	t.Parallel()
	ids := func() string {
		src, err := source.NewFileSource(filepath.Join(repoRoot(t), rolloutFixture))
		if err != nil {
			t.Fatal(err)
		}
		f, err := ddfeeder.New(ddfeeder.Options{OrgSlug: "twin"})
		if err != nil {
			t.Fatal(err)
		}
		em := emit.NewMemoryEmitter(f.Describe())
		if err := f.Run(context.Background(), src, em); err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, ev := range em.Events() {
			out = append(out, ev.GetEventId())
		}
		return strings.Join(out, "\n")
	}
	first, second := ids(), ids()
	if first != second || first == "" {
		t.Fatal("two runs over one recorded interval emitted different ids")
	}
	if recorded := strings.Join(recordedEventIDs(t, filepath.Join(repoRoot(t), rolloutFixture, "events.jsonl")), "\n"); recorded != first {
		t.Errorf("the recording and the replay disagree")
	}
}
