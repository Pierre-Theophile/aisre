// SPDX-License-Identifier: Apache-2.0

package datadog

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The live topology poll (T090; the `apm_topology` capability).
//
// For each configured environment it reads the window since the last complete one, through a reader that
// normalises Datadog's answers into a `topology` payload and states, per part, whether the part finished.
// A window with an unfinished part is partial: the feeder retracts nothing on it and infers no rollout
// from it, the checkpoint declares the gap, and the next window reaches back from the last COMPLETE end,
// so what was unread is read again and not lost (FR-012, FR-016).

// TopologyReader reads one environment over [from, to). It returns the payload with every part's status,
// and the first error that made a part partial or unread, which the poller classifies (a quota stop, a
// rate limit) for the payload's marker fields.
type TopologyReader interface {
	ReadTopology(ctx context.Context, env string, from, to time.Time) (TopologyPayload, error)
}

func (p *Poller) topologyEnabled() bool {
	return p.Capabilities.Enabled(CapAPMTopology) && p.Topology != nil
}

func (p *Poller) topologyInterval() time.Duration {
	if p.TopologyInterval > 0 {
		return p.TopologyInterval
	}
	return DefaultTopologyInterval
}

// PollTopology reads one window of every configured environment.
func (p *Poller) PollTopology(ctx context.Context) error {
	if !p.topologyEnabled() {
		return nil
	}
	if p.topologyTo == nil {
		p.topologyTo = map[string]time.Time{}
	}
	area := WithArea(ctx, AreaTopology)
	for _, env := range p.TopologyScope.Envs {
		to := p.now()
		from := to.Add(-p.topologyInterval())
		if last := p.topologyTo[env]; !last.IsZero() {
			from = last
		}
		var payload TopologyPayload
		var err error
		if p.waiting() {
			payload = TopologyPayload{Env: env, Window: TopologyWindow{From: from, To: to},
				Reason: "not read, Datadog asked to wait", StopReason: StopRateLimited}
		} else {
			payload, err = p.Topology.ReadTopology(area, env, from, to)
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
		payload.Env, payload.Window = env, TopologyWindow{From: from, To: to}
		if err != nil {
			payload.Reason = fmt.Sprintf("a part of the read failed: %v", err)
			if reason := p.failure(AreaTopology, err); reason == StopQuota || reason == StopRateLimited {
				payload.StopReason = reason
			}
		}
		if p.waiting() {
			resume := p.resumeAt
			payload.ResumeAt = &resume
		}
		payload.Deferred = p.takeDeferred()
		if p.Usage != nil {
			payload.Usage = p.Usage()
		}
		if payload.Parts.allComplete() {
			p.topologyTo[env] = to
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		if err := p.Push(ctx, feeder.Payload{Kind: PayloadTopology, At: p.now(), Bytes: raw}); err != nil {
			return err
		}
	}
	return nil
}
