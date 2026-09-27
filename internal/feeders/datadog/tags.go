// SPDX-License-Identifier: Apache-2.0

package datadog

import (
	"context"
	"fmt"
	"sort"
	"time"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// Tags become owners and identity claims (T076, T077; FR-065–FR-068).
//
// The tags read are the ones on a watched log source's own lines, measured at every discovery tick like
// the version stamp: for each allowlisted key, how many lines carry each value. A value is the source's
// only when it is on the published share of its lines — a team tag on 40 % of a service's lines is a
// shared host or a misconfigured sidecar, not an owner — and the rest is stated in the checkpoint.
//
//   - owner keys (`team`, `owner`) become OWNER nodes and `owned-by` edges from the log source. Their
//     valid start is unknown, like the log source's own: a tag carries no history, and the edge must
//     agree with both of its endpoints (003's placeholder lesson, internal/feeders/gcp/map.go);
//   - identifier keys become identity claims, never properties: `kube_namespace` with `kube_deployment`
//     is the Kubernetes deployment the logs came from, claimed as `k8s.deployment=<ns>/<name>`;
//   - a key off the allowlist becomes nothing, and a value that measures something (a bare number, a
//     number with a unit) becomes nothing either (FR-066, FR-067);
//   - two keys naming one owner differently are both kept, and the resolution layer decides in the open
//     (FR-068).

// TagCount is how many of a source's lines carry one value of one tag key. A key pair (the Kubernetes
// namespace and deployment) is counted together, keyed `kube_namespace/kube_deployment` and valued
// `<namespace>/<deployment>`.
type TagCount struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	Lines int64  `json:"lines"`
}

// The published tag allowlist (FR-066).
var (
	// OwnerTagKeys become OWNER nodes.
	OwnerTagKeys = []string{"team", "owner"}
	// KubeDeploymentPair is the identifier pair claimed as a Kubernetes deployment.
	KubeDeploymentPair = "kube_namespace/kube_deployment"
)

// TagKeysMeasured are the keys (and the pair) the poller measures.
func TagKeysMeasured() []string {
	return append(append([]string(nil), OwnerTagKeys...), KubeDeploymentPair)
}

func allowlisted(key string) (owner, identifier bool) {
	for _, k := range OwnerTagKeys {
		if k == key {
			return true, false
		}
	}
	return false, key == KubeDeploymentPair
}

// emitTags asserts a measured source's owners and identifier claims.
func (f *Feeder) emitTags(ctx context.Context, em feeder.Emitter, src LogSource, m SourceMeasurement, at time.Time) error {
	counts := append([]TagCount(nil), m.Tags...)
	sort.Slice(counts, func(i, j int) bool {
		if counts[i].Key != counts[j].Key {
			return counts[i].Key < counts[j].Key
		}
		return counts[i].Value < counts[j].Value
	})
	key := src.Env + "/" + src.Service
	for _, c := range counts {
		owner, identifier := allowlisted(c.Key)
		share := 0.0
		if m.Lines > 0 {
			share = float64(c.Lines) / float64(m.Lines)
		}
		switch {
		case !owner && !identifier:
			f.noteDiscovery(fmt.Sprintf("%s: tag key %q is not on the allowlist and became nothing (FR-066)", key, c.Key))
		case feeder.IsMeasurement(c.Value):
			f.noteDiscovery(fmt.Sprintf("%s: %s:%s measures something and became nothing (FR-067)", key, c.Key, c.Value))
		case share < f.opts.Thresholds.LineShare:
			f.noteDiscovery(fmt.Sprintf("%s: %s:%s is on %.1f%% of lines, below the %.0f%% share; not the source's",
				key, c.Key, c.Value, share*100, f.opts.Thresholds.LineShare*100))
		case owner:
			if err := f.emitOwner(ctx, em, src, c, at); err != nil {
				return err
			}
		default:
			if err := f.emitKubeClaim(ctx, em, src, c, at); err != nil {
				return err
			}
		}
	}
	return nil
}

func (f *Feeder) emitOwner(ctx context.Context, em feeder.Emitter, src LogSource, c TagCount, at time.Time) error {
	ref := feeder.Ref(feeder.NSOwnerTeam, c.Value)
	props, err := feeder.NewProps().
		Str(feeder.PropOwnerKind, "team").
		Str(feeder.PropOwnerTeam, c.Value).
		Str(feeder.PropOwnerSourceLabel, c.Key).
		Build()
	if err != nil {
		return err
	}
	if err := emit(ctx, em, feeder.UpsertNode(f.desc, feeder.NewID(f.desc.SourceID, "owner", c.Value), feeder.NodeFact{
		Meta: feeder.Meta{SourceObservedAt: at}, Ref: ref, Type: graphv1.NodeType_OWNER,
		DisplayName: c.Value, Props: props, ValidFromUnknown: true,
	})); err != nil {
		return err
	}
	return emit(ctx, em, feeder.UpsertEdge(f.desc,
		feeder.NewID(f.desc.SourceID, "edge", "owned-by", src.Ref().GetValue(), c.Key, c.Value),
		feeder.EdgeFact{Meta: feeder.Meta{SourceObservedAt: at}, Src: src.Ref(), Dst: ref,
			Type: graphv1.EdgeType_OWNED_BY, ValidFromUnknown: true}))
}

func (f *Feeder) emitKubeClaim(ctx context.Context, em feeder.Emitter, src LogSource, c TagCount, at time.Time) error {
	claim := feeder.Ref(feeder.NSK8sDeployment, c.Value)
	attrs, err := feeder.NewProps().
		Str(feeder.AttrDeploymentEnvironment, src.Env).
		Str(feeder.PropK8sClaimKey, KubeDeploymentPair).
		Build()
	if err != nil {
		return err
	}
	return emit(ctx, em, feeder.IdentityClaim(f.desc,
		feeder.NewID(f.desc.SourceID, "claim", src.Ref().GetValue(), feeder.NSK8sDeployment, c.Value),
		feeder.IdentityFact{Meta: feeder.Meta{SourceObservedAt: at}, Subject: src.Ref(), Claim: claim, Attributes: attrs}))
}
