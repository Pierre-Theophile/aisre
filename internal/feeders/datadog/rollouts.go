// SPDX-License-Identifier: Apache-2.0

package datadog

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/versionstamp"
)

// Log-observed rollouts (T069–T071; contract §4, data-model.md §6, FR-040g).
//
// A service whose logs carry an accepted version stamp tells us when a new version started running,
// whatever deployed it. Each value first seen in a discovery interval becomes a ROLLOUT:
//
//   - its ref is `datadog.change=<env>/<service>@<value>@<first-seen>`, built from Datadog-stated facts
//     only — the value and the instant of its first indexed line — so a restarted feeder asking the same
//     question gets the same first line and re-sends an id already sent (003 T184's lesson);
//   - its valid start is that first line, marked as a BOUND (`sre.change.valid_from_is_a_bound`): logs
//     say when a version was first seen running, never when it was deployed;
//   - it carries the value's `deploy.commit_sha` or `deploy.image` as a correlation key with the
//     environment, which is what C8 reads: once C9 has made the log source and a deploy feeder's service
//     one entity, the two rollouts of one commit merge;
//   - its actor is left unstated: the logs say nothing about who deployed, and UNKNOWN would claim an
//     actor was observed and could not be classified (pkg/feeder ChangeFact.ActorKind).
//
// No change is inferred from a `deploy.release` value (C8 excludes it, so such a rollout could never
// merge and would stand beside the platform's as a second one), from a value with no reference, or
// from a value whose lines reach back beyond the first-seen horizon. Each is counted in the checkpoint.

// PropChangeVersionValue is the stamp value a log-observed rollout was inferred from.
const PropChangeVersionValue = "sre.datadog.version_value"

// BoundFirstSeenInLogs is what a log-observed rollout's valid start is a bound from.
const BoundFirstSeenInLogs = "first_seen_in_logs"

func (f *Feeder) emitRollouts(ctx context.Context, em feeder.Emitter, src LogSource, v versionstamp.Verdict, m SourceMeasurement, at time.Time) error {
	if m.ValuesFailed != "" {
		f.noteDiscovery(fmt.Sprintf("%s/%s: the stamp's values could not be listed (%s); rollouts first seen in "+
			"this interval are missing until the next one", src.Env, src.Service, m.ValuesFailed))
	}
	if !v.Stamped() || len(m.Values) == 0 {
		return nil
	}
	sightings := append([]ValueSighting(nil), m.Values...)
	sort.Slice(sightings, func(i, j int) bool {
		if !sightings[i].FirstSeen.Equal(sightings[j].FirstSeen) {
			return sightings[i].FirstSeen.Before(sightings[j].FirstSeen)
		}
		return sightings[i].Value < sightings[j].Value
	})
	key := src.Env + "/" + src.Service
	var emitted []string
	for _, s := range sightings {
		ref, reason := versionstamp.Normalise(s.Value)
		switch {
		case s.BeyondHorizon:
			f.noteDiscovery(fmt.Sprintf("%s: version %q first seen before the first-seen horizon; deployed before "+
				"the connector could see it, so no rollout is inferred", key, s.Value))
			continue
		case ref == nil:
			f.noteDiscovery(fmt.Sprintf("%s: version %q names no deploy reference (%s); no rollout is inferred",
				key, s.Value, reasonText(reason)))
			continue
		case ref.GetNamespace() == feeder.NSDeployRelease:
			f.noteDiscovery(fmt.Sprintf("%s: version %q is a release form; C8 does not merge on it, so no rollout "+
				"is inferred (the stamping guide recommends the commit)", key, s.Value))
			continue
		}
		if err := f.emitRollout(ctx, em, src, s, ref, at); err != nil {
			return err
		}
		emitted = append(emitted, s.Value)
	}
	if len(emitted) > 1 {
		f.noteDiscovery(fmt.Sprintf("%s: %d versions first seen in one interval (%s): a canary or a rolling "+
			"deploy; each is one rollout, and their lines overlap", key, len(emitted), strings.Join(emitted, ", ")))
	}
	return nil
}

func (f *Feeder) emitRollout(ctx context.Context, em feeder.Emitter, src LogSource, s ValueSighting, deploy *graphv1.Ref, at time.Time) error {
	first := s.FirstSeen.UTC()
	value := src.Env + "/" + src.Service + "@" + s.Value + "@" + first.Format(time.RFC3339Nano)
	ref := feeder.Ref(NSChange, value)
	props, err := feeder.NewProps().
		Str(feeder.PropChangeValidFromIsABound, BoundFirstSeenInLogs).
		Str(feeder.AttrDeploymentEnvironment, src.Env).
		Str(PropChangeVersionValue, s.Value).
		Build()
	if err != nil {
		return err
	}
	change := feeder.ObserveChange(f.desc, feeder.NewID(f.desc.SourceID, "change", value), feeder.ChangeFact{
		Meta: feeder.Meta{SourceObservedAt: at}, Ref: ref, Kind: graphv1.ChangeKind_ROLLOUT,
		Summary: fmt.Sprintf("version %s first seen in %s's logs (%s)", s.Value, src.Service, src.Env),
		Targets: []*graphv1.Ref{src.Ref()}, ValidAt: first, Props: props,
	})
	if err := emit(ctx, em, change); err != nil {
		return err
	}
	attrs, err := feeder.NewProps().Str(feeder.AttrDeploymentEnvironment, src.Env).Build()
	if err != nil {
		return err
	}
	return emit(ctx, em, feeder.Correlate(f.desc,
		feeder.NewID(f.desc.SourceID, "correlation", value, deploy.GetNamespace(), deploy.GetValue()),
		feeder.CorrelationFact{Meta: feeder.Meta{SourceObservedAt: at}, Subject: ref, Key: deploy, Attributes: attrs}))
}

func (f *Feeder) noteDiscovery(note string) {
	f.mu.Lock()
	f.discoveryNotes = append(f.discoveryNotes, note)
	f.mu.Unlock()
}

func reasonText(r investigationv1.DeployRefAbsentReason) string {
	return strings.ToLower(strings.ReplaceAll(r.String(), "_", " "))
}
