// SPDX-License-Identifier: Apache-2.0

package campaign_test

import (
	"strings"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/campaign"
)

// A Datadog-only campaign (005 T025): a window names a Datadog scope instead of projects, and no
// mailbox is required because no connector in it reads one.
func TestADatadogOnlyCampaignValidatesWithoutAMailbox(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	r := campaign.Record{
		ID: "campaign-2026-10-01", Organisation: "nova", StartedAt: start,
		Scopes: []campaign.Scope{{From: start, Datadog: &campaign.DatadogScope{
			Site: "datadoghq.eu", Environments: []string{"production"},
			LogSources: []string{"production/voice-agent"},
		}}},
		MailboxAddress: campaign.NoAddressRecorded,
		Signatories:    []string{"A. Owner"},
		PolicyVersion:  "1.0.0",
	}
	if err := r.Validate(); err != nil {
		t.Fatalf("a Datadog-only campaign was refused: %v", err)
	}

	noEnv := r
	noEnv.Scopes = []campaign.Scope{{From: start, Datadog: &campaign.DatadogScope{Site: "datadoghq.eu"}}}
	if err := noEnv.Validate(); err == nil || !strings.Contains(err.Error(), "environment") {
		t.Errorf("a Datadog scope with no environment was accepted (%v)", err)
	}
	nothing := r
	nothing.Scopes = []campaign.Scope{{From: start}}
	if err := nothing.Validate(); err == nil {
		t.Error("a window naming neither projects nor a Datadog scope was accepted")
	}
	gcp := r
	gcp.Scopes = []campaign.Scope{{From: start, Projects: []string{"nova-production"}}}
	if err := gcp.Validate(); err == nil || !strings.Contains(err.Error(), "mailbox") {
		t.Errorf("a GCP campaign without a mailbox was accepted (%v); only a campaign reading no mailbox may omit it", err)
	}
}
