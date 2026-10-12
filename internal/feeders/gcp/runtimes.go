// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// Cloud Functions runtime end-of-life (T192).
//
// Google publishes each runtime's lifecycle as data: `projects.locations.runtimes.list` states a
// runtime's stage and its deprecation and decommission dates. A function-backed Cloud Run service says
// which runtime it runs in its `goog-cloudfunctions-runtime` label. Joined, they are a future-dated,
// vendor-announced change on every service that runs a runtime Google is retiring — the kind of change
// this feature exists to put in front of an investigation before it happens rather than after.
//
// Two milestones, two changes, because they mean different things to an on-call:
//
//   - deprecation: Google stops supporting the runtime. Nothing breaks.
//   - decommission: new deployments and updates of a function on it are refused. A service that has
//     to be redeployed in an incident cannot be.
//
// A milestone is ANNOUNCED until Google's own `stage` says it happened (DEPRECATED, DECOMMISSIONED),
// and CONFIRMED from then on. The date passing is not that statement (FR-066): the stage is the vendor
// saying so, which is an observation, and the clock is not.
//
// A runtime no service in scope uses produces nothing. The catalogue is the whole of Google's
// runtimes, and a change about one nobody runs is noise in every investigation's candidate set.

// PayloadFunctionRuntimes is a `projects.locations.runtimes.list` response.
const PayloadFunctionRuntimes = "function_runtimes"

// FunctionRuntimeLabel is the label a function-backed Cloud Run service names its runtime in. It is
// Google's, not the operator's, and its value is a runtime id from Google's closed catalogue.
const FunctionRuntimeLabel = "goog-cloudfunctions-runtime"

// RuntimeActor is the vendor a runtime lifecycle change is attributed to.
const RuntimeActor = "Google Cloud Functions"

// RuntimeSupportSchedule is where a human reads the lifecycle the change was taken from.
const RuntimeSupportSchedule = "https://cloud.google.com/functions/docs/runtime-support"

// The milestones.
const (
	RuntimeDeprecation  = "deprecation"
	RuntimeDecommission = "decommission"
)

type runtimeDate struct {
	Year  int `json:"year"`
	Month int `json:"month"`
	Day   int `json:"day"`
}

func (d *runtimeDate) instant() (time.Time, bool) {
	if d == nil || d.Year == 0 || d.Month == 0 || d.Day == 0 {
		return time.Time{}, false
	}
	return time.Date(d.Year, time.Month(d.Month), d.Day, 0, 0, 0, 0, time.UTC), true
}

type functionRuntime struct {
	Name             string       `json:"name"`
	Stage            string       `json:"stage"`
	DeprecationDate  *runtimeDate `json:"deprecationDate"`
	DecommissionDate *runtimeDate `json:"decommissionDate"`
}

// RuntimeMilestoneChange builds one milestone of one runtime as the announced change on the services
// that run it.
func RuntimeMilestoneChange(runtime, milestone, stage string, at time.Time, services []Service, observedAt time.Time) feeder.ChangeFact {
	state := graphv1.AnnouncementState_ANNOUNCED
	if (milestone == RuntimeDeprecation && (stage == "DEPRECATED" || stage == "DECOMMISSIONED")) ||
		(milestone == RuntimeDecommission && stage == "DECOMMISSIONED") {
		state = graphv1.AnnouncementState_CONFIRMED
	}
	targets := make([]*graphv1.Ref, 0, len(services))
	for _, svc := range services {
		targets = append(targets, svc.Ref())
	}
	verb := map[string]string{RuntimeDeprecation: "deprecated", RuntimeDecommission: "decommissioned"}[milestone]
	return feeder.ChangeFact{
		Meta:              feeder.Meta{SourceObservedAt: observedAt},
		Ref:               feeder.Ref(NSChange, "function-runtime/"+runtime+"/"+milestone),
		Kind:              graphv1.ChangeKind_DEPRECATION,
		Summary:           fmt.Sprintf("Cloud Functions runtime %s %s on %s", runtime, verb, at.Format("2006-01-02")),
		Actor:             RuntimeActor,
		ActorKind:         graphv1.ActorKind_VENDOR,
		OriginRef:         RuntimeSupportSchedule,
		Targets:           targets,
		ValidAt:           at,
		AnnouncementState: state,
	}
}

// recordFunctionRuntime remembers which runtime a function-backed service runs, for the join.
func (f *Feeder) recordFunctionRuntime(svc Service, labels map[string]string) {
	runtime := strings.TrimSpace(labels[FunctionRuntimeLabel])
	if runtime == "" {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.functionRuntimes == nil {
		f.functionRuntimes = map[string]Service{}
	}
	f.functionRuntimes[svc.Value()+"\x00"+runtime] = svc
}

// servicesOnRuntime returns the services in scope that run a runtime, in a stable order.
func (f *Feeder) servicesOnRuntime(runtime string) []Service {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Service
	for key, svc := range f.functionRuntimes {
		if strings.HasSuffix(key, "\x00"+runtime) {
			out = append(out, svc)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Value() < out[j].Value() })
	return out
}

// applyFunctionRuntimes maps one runtimes list into the lifecycle changes of the runtimes in use.
func (f *Feeder) applyFunctionRuntimes(ctx context.Context, desc feeder.Description, em feeder.Emitter, raw []byte, at time.Time) error {
	var payload struct {
		Runtimes []functionRuntime `json:"runtimes"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return fmt.Errorf("gcp: decoding a %s payload: %w", PayloadFunctionRuntimes, err)
	}
	seen := map[string]bool{}
	for _, runtime := range payload.Runtimes {
		// The catalogue lists a runtime once per environment (gen1, gen2); the lifecycle is the
		// runtime's, so it is emitted once.
		if runtime.Name == "" || seen[runtime.Name] {
			continue
		}
		seen[runtime.Name] = true
		services := f.servicesOnRuntime(runtime.Name)
		if len(services) == 0 {
			continue
		}
		for _, milestone := range []struct {
			name string
			date *runtimeDate
		}{{RuntimeDeprecation, runtime.DeprecationDate}, {RuntimeDecommission, runtime.DecommissionDate}} {
			when, ok := milestone.date.instant()
			if !ok {
				continue
			}
			change := RuntimeMilestoneChange(runtime.Name, milestone.name, runtime.Stage, when, services, at)
			if err := emit(ctx, em, feeder.ObserveChange(desc, feeder.NewID(desc.SourceID, "change",
				change.Ref.GetValue(), change.AnnouncementState.String()), change)); err != nil {
				return err
			}
		}
	}
	return nil
}
