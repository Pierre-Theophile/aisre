// SPDX-License-Identifier: Apache-2.0

package campaign_test

import (
	"strings"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/campaign"
)

// A Notion-only campaign (008 T042): a window names a Notion scope instead of projects, and no
// mailbox is required because the Notion connector reads none.
func TestANotionOnlyCampaignValidatesWithoutAMailbox(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	r := campaign.Record{
		ID: "campaign-2026-10-15", Organisation: "nova", StartedAt: start,
		Scopes: []campaign.Scope{{From: start, Notion: &campaign.NotionScope{
			Workspace: "nova", Databases: []string{"0a1b2c3d4e5f60718293a4b5c6d7e8f9"},
		}}},
		MailboxAddress: campaign.NoAddressRecorded,
		Signatories:    []string{"A. Owner"},
		PolicyVersion:  "1.0.0",
	}
	if err := r.Validate(); err != nil {
		t.Fatalf("a Notion-only campaign was refused: %v", err)
	}

	noDatabase := r
	noDatabase.Scopes = []campaign.Scope{{From: start, Notion: &campaign.NotionScope{Workspace: "nova"}}}
	if err := noDatabase.Validate(); err == nil || !strings.Contains(err.Error(), "database") {
		t.Errorf("a Notion scope with no database was accepted (%v)", err)
	}
	noWorkspace := r
	noWorkspace.Scopes = []campaign.Scope{{From: start, Notion: &campaign.NotionScope{Databases: []string{"x"}}}}
	if err := noWorkspace.Validate(); err == nil || !strings.Contains(err.Error(), "workspace") {
		t.Errorf("a Notion scope with no workspace was accepted (%v)", err)
	}
}
