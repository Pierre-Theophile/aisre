// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"fmt"
	"sort"
	"strings"
	"time"

	compute "google.golang.org/api/compute/v1"
	dns "google.golang.org/api/dns/v1"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// Load balancers and DNS, where they are cheap (T167–T170; FR-055–FR-057; contract §7).
//
// # This is the first thing cut, and that is a design constraint rather than a disclaimer
//
// FR-057 makes these surfaces P3 and explicitly the first thing dropped when the budget is short.
// Everything here is therefore written so that **reading neither is a correct outcome**: the omission
// is a scope statement in the checkpoint, every other requirement holds unchanged, and nothing else in
// the connector depends on an `exposed-via` edge existing.
//
// The failure that rule prevents is the one that matters: a reader who cannot see that the load
// balancers were not read will read their absence as "this service is not exposed", which is the
// opposite of the truth and is exactly the kind of confident wrongness FR-012 exists to stop.
//
// # The join is a configured assertion, not a resemblance
//
// A Cloud Run service sits behind a load balancer through a **serverless network endpoint group**, and
// the NEG's `cloudRun.service` field names the service. That is somebody's configuration, read
// verbatim — so the chain
//
//	forwardingRule → target proxy → urlMap → backendService → NEG → Cloud Run service
//
// is a derivation from stated facts at every hop and never a name match. It costs one more list call
// than the three the budget document names (`networkEndpointGroups.list`), in the same cost class, and
// it is worth it: without the NEG there is no field anywhere that links the two, and the alternative
// would have been matching a backend service's name against a service's — which is how `checkout` in
// staging gets exposed via production's load balancer.
//
// # Host names are properties of the relation, not nodes
//
// FR-055 asks for the host names on the `exposed-via` relation, and they are **properties**. A host
// name is not an entity this connector owns: the vendor-notice connector claims host names in
// `server.address` for its own allowlist, and minting nodes for them here would be two connectors
// naming one thing. The relation carries them, and a rule that wants to resolve on one reads the
// property.
//
// # A superseded exposure is retracted at the instant of the swap, never deleted
//
// §7's last sentence, and it is the whole reason a backend swap is modelled at all. Deleting the old
// `exposed-via` would make "what was this service exposed through at 14:10?" unanswerable — the
// history would say it was always the new one. Retracting it at the swap's instant leaves both true,
// each over its own interval, which is what a bitemporal graph is for.

// The load-balancer and DNS payload kinds.
const (
	// PayloadForwardingRules is a `forwardingRules.list` response.
	PayloadForwardingRules = "forwarding_rules"
	// PayloadURLMaps is a `urlMaps.list` response.
	PayloadURLMaps = "url_maps"
	// PayloadBackendServices is a `backendServices.list` response.
	PayloadBackendServices = "backend_services"
	// PayloadNetworkEndpointGroups is a `networkEndpointGroups.list` response, which is the only
	// place a serverless NEG states the Cloud Run service it fronts.
	PayloadNetworkEndpointGroups = "network_endpoint_groups"
	// PayloadDNSRecordSets is a `resourceRecordSets.list` response.
	PayloadDNSRecordSets = "dns_record_sets"
)

// The load-balancer properties.
const (
	// PropLBScheme is `loadBalancingScheme`: EXTERNAL_MANAGED, INTERNAL_MANAGED and so on. It is what
	// says whether the address is reachable from outside at all.
	PropLBScheme = "sre.gcp.lb_scheme"
	// PropLBAddress is the forwarding rule's IP address. The sanitisation table pseudonymises it as
	// infrastructure rather than dropping it: an address that names a machine serving traffic is a
	// join key, and one that names where a person was sitting is not.
	PropLBAddress = "sre.gcp.lb_address"
	// PropLBPortRange and PropLBProtocol are the listener.
	PropLBPortRange = "sre.gcp.lb_port_range"
	PropLBProtocol  = "sre.gcp.lb_protocol"
	// PropLBURLMap is the URL map the rule routes through, by resource name.
	PropLBURLMap = "sre.gcp.lb_url_map"
	// PropExposedHosts is the host names an `exposed-via` relation serves, on the RELATION. See the
	// file comment: a host name is not an entity this connector owns.
	PropExposedHosts = "sre.gcp.exposed_hosts"
	// PropExposedVia names the backend service and the NEG the derivation went through, so a reader
	// can check the chain rather than take the edge on trust.
	PropExposedVia = "sre.gcp.exposed_via_chain"
	// PropDNSRecordName, PropDNSRecordType and PropDNSTargets are the record a DNS_SWITCH is about.
	PropDNSRecordName = "sre.gcp.dns_record_name"
	PropDNSRecordType = "sre.gcp.dns_record_type"
	PropDNSTargets    = "sre.gcp.dns_targets"
	// PropDNSPreviousTargets is the old targets, recorded **only where GCP states them**: an audit
	// entry for a record change carries the new set, and the old one comes from the previous poll. A
	// switch observed without a previous poll records the new targets and says nothing about the old.
	PropDNSPreviousTargets = "sre.gcp.dns_previous_targets"
	// PropSurfaceOmitted states, on a checkpoint, that a surface was not read. It is the scope
	// statement FR-057 requires, and it is what stops an absence reading as a fact.
	PropSurfaceOmitted = "sre.gcp.surface_omitted"
)

// LoadBalancer is a load balancer's coordinates: its fully qualified resource name, which is what GCP
// addresses it by and what an audit entry names.
type LoadBalancer struct {
	Project string
	// Region is empty for a global load balancer, which is what most Cloud Run exposures are.
	Region string
	Name   string
}

// Validate refuses coordinates with an empty part. The region is allowed to be empty — a global load
// balancer has none — and the emptiness is part of the identifier rather than a hole in it.
func (l LoadBalancer) Validate() error {
	for name, part := range map[string]string{"project": l.Project, "name": l.Name} {
		if strings.TrimSpace(part) == "" {
			return fmt.Errorf("%w: the %s of a load balancer", ErrEmptyIdentifierPart, name)
		}
	}
	return nil
}

// Value renders the identifier GCP uses: the fully qualified resource name.
//
// It is the resource name and not `<project>/<region>/<name>` because a load balancer is a composite —
// a forwarding rule, a proxy, a URL map and a backend service — and the forwarding rule's resource name
// is the one thing that names the whole of it unambiguously, in the same string an audit entry carries.
func (l LoadBalancer) Value() string {
	if l.Region == "" {
		return "projects/" + l.Project + "/global/forwardingRules/" + l.Name
	}
	return "projects/" + l.Project + "/regions/" + l.Region + "/forwardingRules/" + l.Name
}

// Ref returns the load balancer's addressing ref.
func (l LoadBalancer) Ref() *graphv1.Ref { return feeder.Ref(NSLoadBalancer, l.Value()) }

// ForwardingRuleObservation is one forwarding rule as a read saw it.
type ForwardingRuleObservation struct {
	LoadBalancer LoadBalancer
	Scheme       string
	Address      string
	PortRange    string
	Protocol     string
	// URLMap is the URL map the rule's target proxy routes through, by name. Empty where the target
	// is not an HTTP(S) proxy, which is every load balancer this connector cannot follow.
	URLMap string
}

// ObserveForwardingRule reads one forwarding rule.
func ObserveForwardingRule(rule *compute.ForwardingRule, project string) (ForwardingRuleObservation, error) {
	var obs ForwardingRuleObservation
	lb := LoadBalancer{Project: project, Region: lastSegment(rule.Region), Name: rule.Name}
	if from, ok := projectFromSelfLink(rule.SelfLink); ok {
		lb.Project = from
	}
	if err := lb.Validate(); err != nil {
		return obs, err
	}
	return ForwardingRuleObservation{
		LoadBalancer: lb,
		Scheme:       rule.LoadBalancingScheme,
		Address:      rule.IPAddress,
		PortRange:    rule.PortRange,
		Protocol:     rule.IPProtocol,
		URLMap:       lastSegment(rule.Target),
	}, nil
}

// NodeFact renders the load balancer as the INFRA_RESOURCE node it is.
//
// Its valid start is **unknown**: a forwarding rule carries no creation instant at all, so the
// connector cannot date it. That is the case FR-011 is about — a guessed start is a defect — and it is
// marked unknown rather than bounded because, unlike a Cloud SQL instance, nothing else in the response
// bounds it either.
func (o ForwardingRuleObservation) NodeFact(observedAt time.Time) feeder.NodeFact {
	return feeder.NodeFact{
		Ref:              o.LoadBalancer.Ref(),
		Type:             graphv1.NodeType_INFRA_RESOURCE,
		DisplayName:      o.LoadBalancer.Name,
		ValidAt:          observedAt,
		ValidFromUnknown: true,
	}
}

// Props renders the load balancer's properties.
func (o ForwardingRuleObservation) Props() *feeder.Props {
	props := feeder.NewProps().
		Str(PropProject, o.LoadBalancer.Project).
		Str(PropLBScheme, o.Scheme)
	if o.LoadBalancer.Region != "" {
		props = props.Str(PropRegion, o.LoadBalancer.Region).
			Str(feeder.AttrCloudRegion, o.LoadBalancer.Region)
	}
	if o.Address != "" {
		props = props.Str(PropLBAddress, o.Address)
	}
	if o.PortRange != "" {
		props = props.Str(PropLBPortRange, o.PortRange)
	}
	if o.Protocol != "" {
		props = props.Str(PropLBProtocol, o.Protocol)
	}
	if o.URLMap != "" {
		props = props.Str(PropLBURLMap, o.URLMap)
	}
	return props
}

// Claims returns the load balancer's identifiers, including the addressing ref (FR-115).
func (o ForwardingRuleObservation) Claims() []Claim {
	claims := []Claim{{
		Namespace: NSLoadBalancer, Value: o.LoadBalancer.Value(),
		Why: "the ref this feeder addresses the load balancer by, which is the forwarding rule's " +
			"fully qualified resource name and the string an audit entry carries",
	}}
	if o.Address != "" {
		// The address, in the namespace an observed outbound dependency's host is named in. It is how
		// telemetry that reached this load balancer resolves against it rather than duplicating it.
		claims = append(claims, Claim{
			Namespace: feeder.NSServerAddress, Value: o.Address,
			Why: "the address the load balancer answers on",
		})
	}
	return claims
}

// Exposure is one derived `exposed-via` relation: a service, the load balancer in front of it, and the
// chain that derived it.
type Exposure struct {
	Service      Service
	LoadBalancer LoadBalancer
	// Hosts are the host names the URL map routes to this backend, sorted. Properties of the relation.
	Hosts []string
	// Chain is the derivation, as `field=value` pairs a reader can check hop by hop.
	Chain []string
}

// Key identifies the relation, for de-duplication and for the edge's event id.
func (e Exposure) Key() string { return e.Service.Value() + "<-" + e.LoadBalancer.Value() }

// EdgeFact renders the exposure as the `exposed-via` edge it is.
//
// The edge reads service → load balancer: `exposed-via` points from the thing exposed to the thing
// exposing it, which is the direction a reader asks in ("what is this service exposed through?").
func (e Exposure) EdgeFact(observedAt time.Time) (feeder.EdgeFact, error) {
	if err := e.Service.Validate(); err != nil {
		return feeder.EdgeFact{}, err
	}
	if err := e.LoadBalancer.Validate(); err != nil {
		return feeder.EdgeFact{}, err
	}
	props, err := e.Props().Build()
	if err != nil {
		return feeder.EdgeFact{}, err
	}
	return feeder.EdgeFact{
		Src:   e.Service.Ref(),
		Dst:   e.LoadBalancer.Ref(),
		Type:  graphv1.EdgeType_EXPOSED_VIA,
		Props: props,
		// A forwarding rule states no creation instant and a URL map states no change instant, so the
		// relation's start is genuinely unknown. The graph bounds it at the observation and says so,
		// which is the honest answer to "since when was it exposed this way" — and the *change* that
		// moved it is the audit entry's business, at the audit entry's instant.
		ValidAt:          observedAt,
		ValidFromUnknown: true,
	}, nil
}

// Props renders the relation's properties: the host names and the chain.
func (e Exposure) Props() *feeder.Props {
	props := feeder.NewProps()
	if len(e.Hosts) > 0 {
		props = props.Strs(PropExposedHosts, e.Hosts...)
	}
	if len(e.Chain) > 0 {
		props = props.Strs(PropExposedVia, e.Chain...)
	}
	return props
}

// ExposureInput is what DeriveExposures reads: the four responses, already decoded.
type ExposureInput struct {
	// Rules are the forwarding rules, by URL map name.
	Rules []ForwardingRuleObservation
	// URLMaps are the URL maps, which carry the host rules and the backend services.
	URLMaps []*compute.UrlMap
	// BackendServices are the backend services, which carry the NEG groups.
	BackendServices []*compute.BackendService
	// NEGs are the network endpoint groups, which are the only place a serverless NEG states the
	// Cloud Run service it fronts.
	NEGs []*compute.NetworkEndpointGroup
	// Project is the project the read covered, for the service coordinates the NEG does not state.
	Project string
}

// DeriveExposures walks the chain and returns the relations it can derive (T167).
//
// Every hop is a stated field, and a hop that cannot be followed **ends the walk** rather than being
// guessed past: a forwarding rule whose target is not an HTTP(S) proxy, a backend service with no
// serverless NEG, a NEG that names no Cloud Run service. Each of those is a load balancer this
// connector cannot attribute, and attributing it anyway is how a service ends up exposed through
// something that does not front it.
func DeriveExposures(in ExposureInput) []Exposure {
	// The NEGs by self-link and by name, because a backend's `group` is a self-link and a URL map's
	// `defaultService` is a name.
	negService := map[string]string{}
	for _, neg := range in.NEGs {
		if neg == nil || neg.CloudRun == nil || neg.CloudRun.Service == "" {
			// A NEG that fronts something other than Cloud Run, or a zonal NEG of VMs. Not this
			// connector's business, and not something to infer a service from.
			continue
		}
		negService[neg.SelfLink] = neg.CloudRun.Service
		negService[neg.Name] = neg.CloudRun.Service
	}

	// The Cloud Run services each backend service fronts, by backend-service name.
	backendServices := map[string][]string{}
	backendRegion := map[string]string{}
	for _, backend := range in.BackendServices {
		if backend == nil {
			continue
		}
		region := lastSegment(backend.Region)
		var services []string
		for _, member := range backend.Backends {
			if member == nil {
				continue
			}
			service, named := negService[member.Group]
			if !named {
				service, named = negService[lastSegment(member.Group)]
			}
			if named {
				services = append(services, service)
				if region == "" {
					region = negRegion(member.Group)
				}
			}
		}
		if len(services) == 0 {
			continue
		}
		backendServices[backend.Name] = sortedUniqueNames(services)
		backendRegion[backend.Name] = region
	}

	// The host names each backend service serves, from the URL maps.
	type mapEntry struct {
		hosts    map[string][]string
		backends map[string]bool
	}
	maps := map[string]mapEntry{}
	for _, urlMap := range in.URLMaps {
		if urlMap == nil {
			continue
		}
		entry := mapEntry{hosts: map[string][]string{}, backends: map[string]bool{}}
		record := func(backend string, hosts []string) {
			name := lastSegment(backend)
			if name == "" {
				return
			}
			entry.backends[name] = true
			entry.hosts[name] = append(entry.hosts[name], hosts...)
		}
		// The default service serves every host the map is for.
		var everyHost []string
		for _, rule := range urlMap.HostRules {
			if rule != nil {
				everyHost = append(everyHost, rule.Hosts...)
			}
		}
		record(urlMap.DefaultService, everyHost)
		for _, matcher := range urlMap.PathMatchers {
			if matcher == nil {
				continue
			}
			hosts := hostsFor(urlMap, matcher.Name)
			record(matcher.DefaultService, hosts)
			for _, rule := range matcher.PathRules {
				if rule != nil {
					record(rule.Service, hosts)
				}
			}
		}
		maps[urlMap.Name] = entry
	}

	var out []Exposure
	seen := map[string]bool{}
	for _, rule := range in.Rules {
		if rule.URLMap == "" {
			// A forwarding rule whose target is not an HTTP(S) proxy. The walk ends: there is no URL
			// map to read host names from and no backend service to follow.
			continue
		}
		entry, known := maps[rule.URLMap]
		if !known {
			continue
		}
		for backend := range entry.backends {
			for _, serviceName := range backendServices[backend] {
				region := backendRegion[backend]
				if region == "" {
					continue
				}
				svc := Service{Project: in.Project, Region: region, Name: serviceName}
				if svc.Validate() != nil {
					continue
				}
				exposure := Exposure{
					Service:      svc,
					LoadBalancer: rule.LoadBalancer,
					Hosts:        sortedUniqueNames(entry.hosts[backend]),
					Chain: []string{
						"forwardingRule=" + rule.LoadBalancer.Name,
						"urlMap=" + rule.URLMap,
						"backendService=" + backend,
						"networkEndpointGroup.cloudRun.service=" + serviceName,
					},
				}
				if seen[exposure.Key()] {
					continue
				}
				seen[exposure.Key()] = true
				out = append(out, exposure)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	return out
}

// hostsFor returns the hosts a path matcher serves.
func hostsFor(urlMap *compute.UrlMap, matcher string) []string {
	var hosts []string
	for _, rule := range urlMap.HostRules {
		if rule != nil && rule.PathMatcher == matcher {
			hosts = append(hosts, rule.Hosts...)
		}
	}
	return hosts
}

// negRegion reads the region out of a NEG self-link or partial URL.
func negRegion(group string) string {
	parts := strings.Split(group, "/")
	for i, part := range parts {
		if part == "regions" && i+1 < len(parts) {
			return parts[i+1]
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// The two changes (T168)
// ---------------------------------------------------------------------------

// ChangeRefDNSSwitch is the deterministic ref of a DNS change: the record and the instant.
func ChangeRefDNSSwitch(project, record, recordType string, at time.Time) *graphv1.Ref {
	return feeder.Ref(NSChange, "dns/"+project+"/"+record+"/"+recordType+"@"+instant(at))
}

// DNSRecord is one record set as a read saw it.
type DNSRecord struct {
	Project string
	Zone    string
	Name    string
	Type    string
	// Targets are `rrdatas`, sorted, so two reads of an unchanged record compare equal.
	Targets []string
	TTL     int64
}

// ObserveDNSRecord reads one record set.
func ObserveDNSRecord(record *dns.ResourceRecordSet, project, zone string) DNSRecord {
	return DNSRecord{
		Project: project,
		Zone:    zone,
		Name:    record.Name,
		Type:    record.Type,
		Targets: sortedUniqueNames(record.Rrdatas),
		TTL:     record.Ttl,
	}
}

// Key identifies the record across polls.
func (r DNSRecord) Key() string { return r.Project + "/" + r.Name + "/" + r.Type }

// DNSSwitch builds the change for a record whose targets moved (T168, FR-056).
//
// `at` is the audit entry's instant and nothing else: a poll sees a record, not a switch, so a change
// dated at the poll would be wrong by up to one poll interval — and the DNS poll's interval is thirty
// minutes, which is long enough for a reader to exonerate the wrong change.
//
// `previous` is the targets the last poll saw, and it is recorded **only when there was a last poll**.
// A switch observed on the first read of a record records the new targets and says nothing about the
// old, because "we did not see the old value" and "there was no old value" are different facts.
//
// `fronting` is the load balancers the record points at, now or before (see LoadBalancersAnswering), and
// they are the change's targets. Until 004 T153 a DNS switch had NO targets: it attached to nothing, so
// no query focused on any entity could ever reach it, and `gcp-lb-dns-01`'s own diff over storefront —
// whose comment says it is "the window the switch falls in" — answered with no changes at all. A DNS
// switch re-pointing a service's domain is one of the classic causes of an outage; a graph that holds it
// free-floating cannot put it in front of the investigation it belongs to.
func DNSSwitch(record DNSRecord, previous []string, at time.Time, actor Actor, fronting []LoadBalancer) (feeder.ChangeFact, error) {
	if at.IsZero() {
		return feeder.ChangeFact{}, fmt.Errorf("gcp: a DNS switch for %s with no instant; the instant is "+
			"the audit entry's timestamp, and a poll sees a record rather than a switch (FR-056)",
			record.Key())
	}
	if strings.TrimSpace(record.Name) == "" || strings.TrimSpace(record.Type) == "" {
		return feeder.ChangeFact{}, fmt.Errorf("%w: a DNS record with no name or no type", ErrEmptyIdentifierPart)
	}
	var targets []*graphv1.Ref
	for _, lb := range fronting {
		if lb.Validate() == nil {
			targets = append(targets, lb.Ref())
		}
	}
	return feeder.ChangeFact{
		Ref:       ChangeRefDNSSwitch(record.Project, record.Name, record.Type, at),
		Kind:      graphv1.ChangeKind_DNS_SWITCH,
		Summary:   record.Type + " " + record.Name + " now answers " + strings.Join(record.Targets, ", "),
		ActorKind: actor.Kind,
		Targets:   targets,
		ValidAt:   at,
	}, nil
}

// LoadBalancersAnswering is every load balancer whose forwarding-rule address a record answers, in
// either of two answer sets — what it answers now and what it answered before the switch.
//
// Both, because the load balancer traffic was switched AWAY from is as much a target as the one it was
// switched to: the service behind the old one lost its traffic at that instant, and an investigation
// into that service must find the switch. The match is on the address the platform states for the rule,
// exactly as given, and nothing looser — a CNAME is not followed and an address is not resolved, because
// either would put the answer at the mercy of DNS as it is today rather than as it was at the switch.
// Sorted and de-duplicated, so a change's target list reads the same on a replay.
func LoadBalancersAnswering(rules []ForwardingRuleObservation, answers ...[]string) []LoadBalancer {
	byAddress := map[string][]LoadBalancer{}
	for _, rule := range rules {
		if address := strings.TrimSpace(rule.Address); address != "" {
			byAddress[address] = append(byAddress[address], rule.LoadBalancer)
		}
	}
	seen := map[string]bool{}
	var out []LoadBalancer
	for _, set := range answers {
		for _, answer := range set {
			for _, lb := range byAddress[strings.TrimSpace(answer)] {
				if !seen[lb.Value()] {
					seen[lb.Value()] = true
					out = append(out, lb)
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Value() < out[j].Value() })
	return out
}

// DNSSwitchProps renders the change's properties.
func DNSSwitchProps(record DNSRecord, previous []string, actor Actor) *feeder.Props {
	props := feeder.NewProps().
		Str(PropProject, record.Project).
		Str(PropDNSRecordName, record.Name).
		Str(PropDNSRecordType, record.Type).
		Str(PropActorRung, actor.Rung)
	if len(record.Targets) > 0 {
		props = props.Strs(PropDNSTargets, record.Targets...)
	}
	if len(previous) > 0 {
		// Only where GCP stated them — which here means only where a previous poll saw them.
		props = props.Strs(PropDNSPreviousTargets, previous...)
	}
	if len(actor.Evidence) > 0 {
		props = props.Strs(PropActorEvidence, actor.Evidence...)
	}
	return props
}

// ChangeRefBackendSwap is the deterministic ref of a backend swap: the load balancer and the instant.
func ChangeRefBackendSwap(lb LoadBalancer, at time.Time) *graphv1.Ref {
	return feeder.Ref(NSChange, "lb-backend/"+lb.Value()+"@"+instant(at))
}

// BackendSwap is a load balancer's backend moving from one service to another.
type BackendSwap struct {
	LoadBalancer LoadBalancer
	// From and To are the services the load balancer fronted before and after.
	From, To Service
	// At is the instant the audit entry states.
	At time.Time
}

// SwapChange builds the swap as a CONFIG_CHANGE (T168).
//
// It is a CONFIG_CHANGE and not a DNS_SWITCH: nothing about DNS changed, a load balancer's
// configuration did. A reader filtering for DNS changes must not find a backend swap, and a reader
// asking what changed about the load balancer must.
func (s BackendSwap) SwapChange(actor Actor) (feeder.ChangeFact, error) {
	if s.At.IsZero() {
		return feeder.ChangeFact{}, fmt.Errorf("gcp: a backend swap on %s with no instant; the instant "+
			"is the audit entry's timestamp", s.LoadBalancer.Value())
	}
	if err := s.LoadBalancer.Validate(); err != nil {
		return feeder.ChangeFact{}, err
	}
	targets := []*graphv1.Ref{s.LoadBalancer.Ref()}
	summary := "load balancer " + s.LoadBalancer.Name + " backend swapped"
	if s.To.Validate() == nil {
		targets = append(targets, s.To.Ref())
		summary += " to " + s.To.Name
	}
	if s.From.Validate() == nil {
		targets = append(targets, s.From.Ref())
		summary += " from " + s.From.Name
	}
	return feeder.ChangeFact{
		Ref:       ChangeRefBackendSwap(s.LoadBalancer, s.At),
		Kind:      graphv1.ChangeKind_CONFIG_CHANGE,
		Summary:   summary,
		ActorKind: actor.Kind,
		Targets:   targets,
		ValidAt:   s.At,
	}, nil
}

// RetractSuperseded renders the retraction of the exposure the swap replaced (§7).
//
// **Retracted at the instant of the swap, not deleted.** Deleting the old relation would make "what was
// this service exposed through at 14:10?" unanswerable: the history would say it was always the new
// one. Retracting it at the swap's instant leaves both true, each over its own interval.
func (s BackendSwap) RetractSuperseded() (feeder.EdgeRetraction, bool) {
	if s.From.Validate() != nil || s.LoadBalancer.Validate() != nil || s.At.IsZero() {
		return feeder.EdgeRetraction{}, false
	}
	return feeder.EdgeRetraction{
		Src:      s.From.Ref(),
		Dst:      s.LoadBalancer.Ref(),
		Type:     graphv1.EdgeType_EXPOSED_VIA,
		ValidEnd: s.At,
	}, true
}

// ---------------------------------------------------------------------------
// The omission (T169)
// ---------------------------------------------------------------------------

// The published reasons a surface is not read. They are constants because they reach the checkpoint,
// and a reason a reader cannot look up is a reason they cannot act on.
const (
	// SurfaceOmittedForBudget is FR-057's first case: the budget was short and these are the first
	// thing cut.
	SurfaceOmittedForBudget = "outside the call budget: load balancers and DNS are P3 and the first " +
		"surface the published deferral order drops (FR-057)"
	// SurfaceOmittedForPermission is the second: the credential cannot read them. Note that
	// roles/compute.networkViewer is NOT the answer — it grants
	// trafficdirector.networks.reportMetrics, which writes, and disqualifies itself under FR-004
	// (research §8.2). roles/compute.viewer is the cleaner choice, and the one case where the wider
	// role is the safer one.
	SurfaceOmittedForPermission = "outside the credential's permissions: reading these needs " +
		"roles/compute.viewer and roles/dns.reader, and roles/compute.networkViewer is not an " +
		"alternative because it grants a write (FR-004, research §8.2)"
)

// The surfaces that can be omitted, named so a checkpoint says which.
const (
	SurfaceLoadBalancers = "load_balancers"
	SurfaceCloudDNS      = "cloud_dns"
)

// OmitBoth is FR-057's rule, and the rule is that it is **both or neither**.
//
// Reading the load balancers and not DNS would produce an exposure graph whose host names nothing
// resolves, and reading DNS without the load balancers would produce records pointing at addresses no
// node carries. Either half alone is a graph that looks complete and answers wrongly, so the omission
// is all-or-nothing and it is stated as one scope statement.
func OmitBoth(reason string) []string {
	return []string{
		SurfaceCloudDNS + ": " + reason,
		SurfaceLoadBalancers + ": " + reason,
	}
}
