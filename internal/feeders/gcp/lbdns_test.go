// SPDX-License-Identifier: Apache-2.0

package gcp_test

import (
	"strings"
	"testing"
	"time"

	compute "google.golang.org/api/compute/v1"
	dns "google.golang.org/api/dns/v1"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	gcpfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/gcp"
)

// Load balancers and DNS (T167–T170; FR-055–FR-057).

const (
	lbProject = "nova-production"
	lbRegion  = "europe-west1"
	lbHost    = "shop.example.test"
)

func exposureInput(negService string) gcpfeeder.ExposureInput {
	rule, err := gcpfeeder.ObserveForwardingRule(&compute.ForwardingRule{
		Name:                "shop-https",
		SelfLink:            "https://www.googleapis.com/compute/v1/projects/" + lbProject + "/global/forwardingRules/shop-https",
		IPAddress:           "34.111.0.7",
		IPProtocol:          "TCP",
		PortRange:           "443-443",
		LoadBalancingScheme: "EXTERNAL_MANAGED",
		Target:              "https://www.googleapis.com/compute/v1/projects/" + lbProject + "/global/targetHttpsProxies/shop-urlmap",
	}, lbProject)
	if err != nil {
		panic(err)
	}
	group := "https://www.googleapis.com/compute/v1/projects/" + lbProject +
		"/regions/" + lbRegion + "/networkEndpointGroups/shop-neg"
	in := gcpfeeder.ExposureInput{
		Rules: []gcpfeeder.ForwardingRuleObservation{rule},
		URLMaps: []*compute.UrlMap{{
			Name:           "shop-urlmap",
			DefaultService: "projects/" + lbProject + "/global/backendServices/shop-backend",
			HostRules:      []*compute.HostRule{{Hosts: []string{lbHost}, PathMatcher: "all"}},
			PathMatchers: []*compute.PathMatcher{{
				Name:           "all",
				DefaultService: "projects/" + lbProject + "/global/backendServices/shop-backend",
			}},
		}},
		BackendServices: []*compute.BackendService{{
			Name:     "shop-backend",
			Backends: []*compute.Backend{{Group: group}},
		}},
		Project: lbProject,
	}
	if negService != "" {
		in.NEGs = []*compute.NetworkEndpointGroup{{
			Name:                "shop-neg",
			SelfLink:            group,
			Region:              "https://www.googleapis.com/compute/v1/projects/" + lbProject + "/regions/" + lbRegion,
			NetworkEndpointType: "SERVERLESS",
			CloudRun:            &compute.NetworkEndpointGroupCloudRun{Service: negService},
		}}
	}
	return in
}

// The chain is walked hop by hop from stated fields, and the edge carries the chain so a reviewer can
// check it rather than take it on trust.
func TestTheExposureChainIsDerivedHopByHopAndRecorded(t *testing.T) {
	exposures := gcpfeeder.DeriveExposures(exposureInput("storefront"))
	if len(exposures) != 1 {
		t.Fatalf("exposures = %+v, want one", exposures)
	}
	exposure := exposures[0]
	if exposure.Service.Name != "storefront" || exposure.Service.Region != lbRegion {
		t.Errorf("service = %+v, want storefront in %s; the region comes from the NEG's own self-link, "+
			"because a backend service states none", exposure.Service, lbRegion)
	}
	props := propsMap(t, exposure.Props())
	if !strings.Contains(props[gcpfeeder.PropExposedHosts], lbHost) {
		t.Errorf("hosts = %q, want the host the URL map routes (FR-055)", props[gcpfeeder.PropExposedHosts])
	}
	for _, hop := range []string{"forwardingRule=", "urlMap=", "backendService=", "cloudRun.service="} {
		if !strings.Contains(props[gcpfeeder.PropExposedVia], hop) {
			t.Errorf("the chain does not name %s: %q", hop, props[gcpfeeder.PropExposedVia])
		}
	}
	edge, err := exposure.EdgeFact(sqlObserved)
	if err != nil {
		t.Fatalf("EdgeFact: %v", err)
	}
	if edge.Type != graphv1.EdgeType_EXPOSED_VIA {
		t.Errorf("edge type = %s, want EXPOSED_VIA", edge.Type)
	}
	// The edge reads service → load balancer: "what is this service exposed through?" is the question
	// a reader asks.
	if edge.Src.GetNamespace() != gcpfeeder.NSService || edge.Dst.GetNamespace() != gcpfeeder.NSLoadBalancer {
		t.Errorf("edge is %s → %s, want service → load balancer",
			edge.Src.GetNamespace(), edge.Dst.GetNamespace())
	}
	// A forwarding rule states no creation instant, so the relation's start is genuinely unknown
	// rather than dated at the poll.
	if !edge.ValidFromUnknown {
		t.Error("the exposure asserts a known start; a forwarding rule carries no creation instant, " +
			"and a guessed start is a defect (FR-011)")
	}
}

// Without the NEG there is no field anywhere linking the load balancer to the service, and the walk ends
// rather than matching names. Matching `shop-backend` against `storefront` — or against a service
// called `shop` — is how a staging service gets exposed via production's load balancer.
func TestWithoutTheNEGNothingIsDerived(t *testing.T) {
	if exposures := gcpfeeder.DeriveExposures(exposureInput("")); len(exposures) != 0 {
		t.Fatalf("exposures = %+v, want none: the NEG is the only field that names the service",
			exposures)
	}
}

// A forwarding rule whose target is not an HTTP(S) proxy ends the walk: there is no URL map to read
// host names from and no backend service to follow.
func TestAForwardingRuleWithNoURLMapEndsTheWalk(t *testing.T) {
	in := exposureInput("storefront")
	in.Rules[0].URLMap = ""
	if exposures := gcpfeeder.DeriveExposures(in); len(exposures) != 0 {
		t.Fatalf("exposures = %+v, want none", exposures)
	}
}

// A global load balancer has no region, and that is part of its identifier rather than a hole in it.
func TestAGlobalLoadBalancersIdentifierSaysGlobal(t *testing.T) {
	global := gcpfeeder.LoadBalancer{Project: lbProject, Name: "shop-https"}
	if err := global.Validate(); err != nil {
		t.Fatalf("a global load balancer was refused: %v", err)
	}
	if !strings.Contains(global.Value(), "/global/") {
		t.Errorf("value = %q, want it to say global", global.Value())
	}
	regional := gcpfeeder.LoadBalancer{Project: lbProject, Region: lbRegion, Name: "shop-https"}
	if regional.Value() == global.Value() {
		t.Error("a regional and a global load balancer of the same name share an identifier")
	}
	// An empty project or name is still refused: a ref with a hole resolves against something.
	if (gcpfeeder.LoadBalancer{Name: "x"}).Validate() == nil {
		t.Error("a load balancer with no project was accepted")
	}
}

// A DNS switch is dated at the audit entry's instant and never at the poll, and a poll with no entry
// yields a refusal rather than a guess.
func TestADNSSwitchIsDatedAtTheAuditEntry(t *testing.T) {
	record := gcpfeeder.ObserveDNSRecord(&dns.ResourceRecordSet{
		Name: lbHost + ".", Type: "A", Ttl: 300, Rrdatas: []string{"34.111.0.9"},
	}, lbProject, "twin-zone")
	switched := time.Date(2026, 9, 21, 14, 25, 0, 0, time.UTC)

	change, err := gcpfeeder.DNSSwitch(record, []string{"34.111.0.7"}, switched,
		gcpfeeder.Actor{Kind: graphv1.ActorKind_PERSON, Rung: gcpfeeder.RungHumanDirectory}, nil)
	if err != nil {
		t.Fatalf("DNSSwitch: %v", err)
	}
	if change.Kind != graphv1.ChangeKind_DNS_SWITCH {
		t.Errorf("kind = %s, want DNS_SWITCH", change.Kind)
	}
	if !change.ValidAt.Equal(switched) {
		t.Errorf("valid_at = %s, want the audit entry's instant %s; a poll sees a record, not a switch",
			change.ValidAt, switched)
	}
	props := propsMap(t, gcpfeeder.DNSSwitchProps(record, []string{"34.111.0.7"}, gcpfeeder.Actor{}))
	if !strings.Contains(props[gcpfeeder.PropDNSTargets], "34.111.0.9") {
		t.Errorf("targets = %q, want the new ones", props[gcpfeeder.PropDNSTargets])
	}
	if !strings.Contains(props[gcpfeeder.PropDNSPreviousTargets], "34.111.0.7") {
		t.Errorf("previous targets = %q, want the ones the last poll saw",
			props[gcpfeeder.PropDNSPreviousTargets])
	}

	// With no instant it is refused rather than dated at the poll.
	if _, err := gcpfeeder.DNSSwitch(record, nil, time.Time{}, gcpfeeder.Actor{}, nil); err == nil {
		t.Error("a DNS switch with no instant was accepted")
	}
	// And on the first read of a record there is no previous value, so none is recorded: "we did not
	// see the old value" and "there was no old value" are different facts.
	first := propsMap(t, gcpfeeder.DNSSwitchProps(record, nil, gcpfeeder.Actor{}))
	if _, recorded := first[gcpfeeder.PropDNSPreviousTargets]; recorded {
		t.Error("a first read recorded previous targets it never saw")
	}
}

// A backend swap is a CONFIG_CHANGE and the superseded exposure is RETRACTED at the instant of the
// swap, never deleted. Deleting it would make "what was this exposed through at 14:10?" unanswerable —
// the history would say it was always the new one.
func TestABackendSwapRetractsTheOldExposureRatherThanDeletingIt(t *testing.T) {
	lb := gcpfeeder.LoadBalancer{Project: lbProject, Name: "shop-https"}
	swapped := time.Date(2026, 9, 21, 14, 31, 0, 0, time.UTC)
	swap := gcpfeeder.BackendSwap{
		LoadBalancer: lb,
		From:         gcpfeeder.Service{Project: lbProject, Region: lbRegion, Name: "storefront"},
		To:           gcpfeeder.Service{Project: lbProject, Region: lbRegion, Name: "storefront-next"},
		At:           swapped,
	}
	change, err := swap.SwapChange(gcpfeeder.Actor{})
	if err != nil {
		t.Fatalf("SwapChange: %v", err)
	}
	if change.Kind != graphv1.ChangeKind_CONFIG_CHANGE {
		t.Errorf("kind = %s, want CONFIG_CHANGE: nothing about DNS changed, a load balancer's "+
			"configuration did", change.Kind)
	}
	if !change.ValidAt.Equal(swapped) {
		t.Errorf("valid_at = %s, want the audit entry's instant", change.ValidAt)
	}

	retraction, ok := swap.RetractSuperseded()
	if !ok {
		t.Fatal("the superseded exposure was not retracted")
	}
	if !retraction.ValidEnd.Equal(swapped) {
		t.Errorf("valid_end = %s, want the instant of the swap %s — not the moment the feeder noticed",
			retraction.ValidEnd, swapped)
	}
	if retraction.Type != graphv1.EdgeType_EXPOSED_VIA {
		t.Errorf("retraction type = %s, want EXPOSED_VIA", retraction.Type)
	}
	if retraction.Src.GetValue() != swap.From.Value() {
		t.Errorf("the retraction names %q, want the service the exposure left", retraction.Src.GetValue())
	}
	// A swap with nothing to retract — the load balancer had no known previous service — retracts
	// nothing rather than retracting an edge nobody asserted.
	noPrevious := gcpfeeder.BackendSwap{LoadBalancer: lb, To: swap.To, At: swapped}
	if _, ok := noPrevious.RetractSuperseded(); ok {
		t.Error("an exposure nobody asserted was retracted")
	}
}

// The omission is both surfaces or neither, and it names the reason (FR-057, T169).
func TestOmittingTheseSurfacesIsBothOrNeitherAndNamesWhy(t *testing.T) {
	for _, reason := range []string{
		gcpfeeder.SurfaceOmittedForBudget, gcpfeeder.SurfaceOmittedForPermission,
	} {
		omitted := gcpfeeder.OmitBoth(reason)
		if len(omitted) != 2 {
			t.Fatalf("OmitBoth returned %d surfaces, want both: half an exposure graph looks complete "+
				"and answers wrongly", len(omitted))
		}
		joined := strings.Join(omitted, " ")
		if !strings.Contains(joined, gcpfeeder.SurfaceLoadBalancers) ||
			!strings.Contains(joined, gcpfeeder.SurfaceCloudDNS) {
			t.Errorf("the omission does not name both surfaces: %v", omitted)
		}
		for _, statement := range omitted {
			if !strings.Contains(statement, reason) {
				t.Errorf("%q does not carry the reason", statement)
			}
		}
	}
	// And the permission reason says why compute.networkViewer is not the answer, because it is the
	// role a reader would reach for and it grants a write (FR-004).
	if !strings.Contains(gcpfeeder.SurfaceOmittedForPermission, "networkViewer") {
		t.Error("the permission reason does not mention compute.networkViewer, which is the role a " +
			"reader would reach for and which disqualifies itself under FR-004")
	}
	// The checkpoint carries it as a scope statement.
	checkpoint := gcpfeeder.Checkpoint{
		From: sqlObserved, To: sqlObserved.Add(time.Hour), Outcome: gcpfeeder.PollComplete,
		Scope:           gcpfeeder.Scope{Projects: []string{lbProject}, Regions: []string{lbRegion}},
		OmittedSurfaces: gcpfeeder.OmitBoth(gcpfeeder.SurfaceOmittedForBudget),
	}
	note := checkpoint.Note()
	if !strings.Contains(note, gcpfeeder.SurfaceLoadBalancers) || !strings.Contains(note, "FR-057") {
		t.Errorf("the checkpoint does not state the omission as a scope statement: %q", note)
	}
}

// A DNS switch targets the load balancers its record points at — the new one AND the old one (004 T153).
//
// It used to target nothing, so no query focused on any entity could reach it: `gcp-lb-dns-01`'s diff
// over storefront, written as "the window the switch falls in", answered with no changes. The old load
// balancer matters as much as the new: the service behind it lost its traffic at that instant, and an
// investigation into that service has to find the switch.
func TestADNSSwitchTargetsTheLoadBalancersItsRecordPointsAt(t *testing.T) {
	rule := func(name, address string) gcpfeeder.ForwardingRuleObservation {
		return gcpfeeder.ForwardingRuleObservation{
			LoadBalancer: gcpfeeder.LoadBalancer{Project: lbProject, Name: name}, Address: address,
		}
	}
	rules := []gcpfeeder.ForwardingRuleObservation{
		rule("shop-https", "34.111.0.9"),
		rule("shop-https-old", "34.111.0.7"),
		rule("unrelated", "34.111.0.99"),
	}
	fronting := gcpfeeder.LoadBalancersAnswering(rules, []string{"34.111.0.9"}, []string{"34.111.0.7"})
	if len(fronting) != 2 {
		t.Fatalf("fronting = %v, want the new and the old load balancer and nothing else", fronting)
	}

	record := gcpfeeder.ObserveDNSRecord(&dns.ResourceRecordSet{
		Name: lbHost + ".", Type: "A", Ttl: 300, Rrdatas: []string{"34.111.0.9"},
	}, lbProject, "twin-zone")
	change, err := gcpfeeder.DNSSwitch(record, []string{"34.111.0.7"},
		time.Date(2026, 9, 21, 14, 25, 0, 0, time.UTC), gcpfeeder.Actor{}, fronting)
	if err != nil {
		t.Fatalf("DNSSwitch: %v", err)
	}
	got := map[string]bool{}
	for _, target := range change.Targets {
		got[target.GetValue()] = true
	}
	for _, lb := range fronting {
		if !got[lb.Value()] {
			t.Errorf("the switch does not target %s, which its record points at", lb.Value())
		}
	}
	if len(change.Targets) != 2 {
		t.Errorf("targets = %v, want exactly the two load balancers", change.Targets)
	}

	// An address no rule has is not matched to anything, and nothing looser is tried.
	if none := gcpfeeder.LoadBalancersAnswering(rules, []string{"203.0.113.5"}); len(none) != 0 {
		t.Errorf("an address no forwarding rule has matched %v", none)
	}
}
