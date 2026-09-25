// SPDX-License-Identifier: Apache-2.0

package vendornotice_test

import (
	"context"
	"testing"

	vn "github.com/Pierre-Theophile/aisre/internal/feeders/vendornotice"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/resolution"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// What this feeder emits and what C6 reads must be the same strings (T077, FR-119).
//
// The failure this guards against reports nothing: a rule reading `sre.vendor.allowlisted_host` and a
// feeder writing `sre.vendor.host` do not error and do not merge. C6 would stay published, registered
// and dead, and the vendor a notice is about would sit in the graph beside the third party observed
// from traffic as two entities — which is the state FR-119 exists to end.

func TestTheFeederAndC6SpellTheSameThings(t *testing.T) {
	t.Parallel()

	for _, pair := range []struct {
		what          string
		feeder, rules string
	}{
		{"the server-address namespace", vn.NSServerAddress, resolution.NamespaceServerAddress},
		{"the vendor namespace", vn.NSVendor, resolution.NamespaceVendor},
		{"the notice namespace", vn.NSVendorNotice, resolution.NamespaceVendorNotice},
		{"the allowlisted-host marker", vn.PropAllowlistedHost, resolution.AttrVendorAllowlistedHost},
	} {
		if pair.feeder != pair.rules {
			t.Errorf("%s: the feeder writes %q and the rules read %q; a rule whose attribute the "+
				"feeder never emits never fires, and it fails silently",
				pair.what, pair.feeder, pair.rules)
		}
	}
}

// And C6 fires on the claims the feeder actually emits, through the emitter rather than out of
// VendorClaims: the attributes travel a second hop, and a hop that dropped them would leave every
// unit test on VendorClaims green.
func TestC6FiresOnTheClaimsThisFeederEmits(t *testing.T) {
	t.Parallel()

	f := newTestFeeder(t, false)
	em := &recorder{}
	src := &sliceSource{payloads: []feeder.Payload{
		announcementsPayload(t, vn.PayloadAnnouncements, readAt, maintenanceEntry()),
	}}
	if err := f.Run(context.Background(), src, em); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var stored []resolution.Claim
	for _, ev := range em.events {
		claim := ev.GetIdentityClaim()
		if claim == nil {
			continue
		}
		attrs := map[string]string{}
		for key, value := range claim.GetAttributes().GetFields() {
			attrs[key] = value.GetStringValue()
		}
		stored = append(stored, resolution.Claim{
			ClaimID:    "vendor-notice:" + claim.GetClaim().GetNamespace() + "=" + claim.GetClaim().GetValue(),
			EntityID:   "entity-vendor",
			EntityType: graph.NodeTypeThirdParty, SourceID: "vendor-notice:twin",
			Namespace: claim.GetClaim().GetNamespace(), Value: claim.GetClaim().GetValue(),
			Attributes: attrs,
		})
	}

	// The other side belongs to the telemetry feeder: an outbound dependency observed on the host
	// the allowlist maps, with nothing configured about it.
	observed := resolution.Claim{
		ClaimID: "otel:dependency", EntityID: "entity-observed-third-party",
		EntityType: graph.NodeTypeThirdParty, SourceID: "otel:twin",
		Namespace: resolution.NamespaceServerAddress, Value: "api.acme-gpu.test",
	}
	store := claimSlice(append(stored, observed))

	matches, err := resolution.Evaluate(context.Background(), store, observed)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	var ids []string
	fired := false
	for _, match := range matches {
		ids = append(ids, match.RuleID)
		if match.RuleID == "C6" {
			fired = true
			if !match.Certain {
				t.Error("C6 fired as a suggestion; FR-119 publishes it as certain because it rests " +
					"on a configured assertion")
			}
		}
	}
	if !fired {
		t.Errorf("C6 did not fire on the claims this feeder emits: matched %v. The vendor a notice "+
			"is about and the third party observed from traffic stay two entities, and nothing "+
			"reports that they did", ids)
	}
}

// claimSlice is a ClaimStore over a fixed slice, enough for the rule exercised here.
type claimSlice []resolution.Claim

func (s claimSlice) ClaimsFor(_ context.Context, namespace, value string) ([]resolution.Claim, error) {
	var out []resolution.Claim
	for _, claim := range s {
		if claim.Namespace == namespace && claim.Value == value {
			out = append(out, claim)
		}
	}
	return out, nil
}

func (s claimSlice) ClaimsMatchingAttributes(_ context.Context, namespace string, attrs map[string]string) ([]resolution.Claim, error) {
	var out []resolution.Claim
	for _, claim := range s {
		if claim.Namespace != namespace {
			continue
		}
		match := true
		for key, want := range attrs {
			if claim.Attr(key) != want {
				match = false
				break
			}
		}
		if match {
			out = append(out, claim)
		}
	}
	return out, nil
}

func (s claimSlice) ClaimsInNamespaces(_ context.Context, namespaces []string) ([]resolution.Claim, error) {
	var out []resolution.Claim
	for _, claim := range s {
		for _, namespace := range namespaces {
			if claim.Namespace == namespace {
				out = append(out, claim)
				break
			}
		}
	}
	return out, nil
}

func (claimSlice) SharedOwner(context.Context, string, string) (bool, error) { return false, nil }

// ChangeTargets: no change in these fixtures has a target, so C8 finds no shared one. It is nil
// rather than a panic because Evaluate runs every registered rule on every claim.
func (claimSlice) ChangeTargets(context.Context, string) ([]string, error) { return nil, nil }

// CorrelatedWith completes resolution.ClaimStore. No correlation key reaches these rules: a vendor notice is a name for one announcement, and
// nothing about it is a value several entities share.
func (claimSlice) CorrelatedWith(context.Context, string, string) ([]resolution.Correlation, error) {
	return nil, nil
}
