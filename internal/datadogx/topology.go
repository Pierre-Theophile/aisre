// SPDX-License-Identifier: Apache-2.0

package datadogx

import (
	"context"
	"errors"
	"sort"
	"time"

	ddfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/datadog"
)

// ReadTopology reads one environment over [from, to) and normalises Datadog's answers into the feeder's
// `topology` payload (T090): the service dependencies, then four span aggregates grouped by service and
// one facet each — the callee, the version, the operation and the host. Only those facets are ever asked
// for: not a container, a pod, a user or a resource, which is what keeps the payload free of ephemeral
// replicas and of people (FR-014).
//
// Each part's status is stated. A part that failed is `unread`, one Datadog reported incomplete is
// `partial`, and the first error is returned beside the payload so the poller can classify a quota stop
// or a rate limit. After such a stop the remaining parts are not attempted: they would be refused too, and
// an attempt is a call. Built from the published API shapes; not yet verified against a live organisation.
func (c *Client) ReadTopology(ctx context.Context, env string, from, to time.Time) (ddfeeder.TopologyPayload, error) {
	p := ddfeeder.TopologyPayload{Env: env, Window: ddfeeder.TopologyWindow{From: from, To: to}}
	stopped := func(err error) bool {
		var status *StatusError
		return IsQuotaStop(err) || (errors.As(err, &status) && status.Status == 429)
	}
	var first error
	fail := func(err error) {
		if first == nil {
			first = err
		}
	}

	deps, _, err := c.ServiceDependencies(ctx, env, from, to)
	if err != nil {
		fail(err)
		p.Parts.Dependencies = ddfeeder.PartUnread
		if stopped(err) {
			return p, first
		}
	} else {
		p.Parts.Dependencies = ddfeeder.PartComplete
		names := make([]string, 0, len(deps))
		for name := range deps {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			p.Services = append(p.Services, ddfeeder.TopologyService{Name: name, Calls: deps[name]})
		}
	}

	query := "env:" + env
	// aggregate counts spans grouped by service and one facet, returning the buckets and the part's status.
	aggregate := func(facet string, limit int) ([]SpanBucket, string, bool) {
		out, _, err := c.AggregateSpans(ctx, SpanAggregateRequest{
			Compute: []SpanCompute{{Aggregation: "count", Type: "total"}},
			Filter:  NewSpanFilter(query, from, to),
			GroupBy: []SpanGroupBy{{Facet: "service", Limit: ddfeeder.MaxTopologyServices}, {Facet: facet, Limit: limit}},
		})
		if err != nil {
			fail(err)
			return nil, ddfeeder.PartUnread, stopped(err)
		}
		if out.Meta.Partial() {
			fail(errors.New("datadog reported a timeout or warnings for a span aggregate"))
			return out.Data, ddfeeder.PartPartial, false
		}
		return out.Data, ddfeeder.PartComplete, false
	}
	hits := func(b SpanBucket) int64 { n, _ := b.Number(0); return int64(n) }

	buckets, status, stop := aggregate("@peer.service", 50)
	p.Parts.Traffic = status
	for _, b := range buckets {
		if callee := b.Key("@peer.service"); callee != "" {
			p.Traffic = append(p.Traffic, ddfeeder.TopologyTraffic{Caller: b.Key("service"), Callee: callee, Hits: hits(b)})
		}
	}
	if stop {
		return p, first
	}
	buckets, status, stop = aggregate("version", 20)
	p.Parts.Versions = status
	for _, b := range buckets {
		if v := b.Key("version"); v != "" {
			p.Versions = append(p.Versions, ddfeeder.TopologyVersion{Service: b.Key("service"), Version: v, Hits: hits(b)})
		}
	}
	if stop {
		return p, first
	}
	buckets, status, stop = aggregate("operation_name", 5)
	p.Parts.Operations = status
	for _, b := range buckets {
		if op := b.Key("operation_name"); op != "" {
			p.Operations = append(p.Operations, ddfeeder.TopologyOperation{Service: b.Key("service"), Operation: op, Hits: hits(b)})
		}
	}
	if stop {
		return p, first
	}
	// One over the cap, so the feeder can say it truncated.
	buckets, status, _ = aggregate("host", ddfeeder.MaxHostsPerService+1)
	p.Parts.Hosts = status
	for _, b := range buckets {
		if host := b.Key("host"); host != "" {
			p.Hosts = append(p.Hosts, ddfeeder.TopologyHost{Service: b.Key("service"), Host: host})
		}
	}
	return p, first
}
