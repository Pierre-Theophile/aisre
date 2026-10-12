// SPDX-License-Identifier: Apache-2.0

package vendornotice_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/feeders/deployrecord"
	"github.com/Pierre-Theophile/aisre/internal/feeders/vendornotice"
	"github.com/Pierre-Theophile/aisre/internal/sanitise"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/source"
)

// A live vendor-notice recording keeps what a replay needs and nothing a mailbox could leak (T188,
// FR-137). The mailbox path cannot be exercised against a real mailbox in a test, so its two risks are
// planted in the payloads a mailbox read produces: an address quoted in a read error, and an
// organisation's own resource named by a notice. A region named by a notice must survive verbatim, or
// it would no longer join the GCP connector's region.
func TestALiveVendorNoticeRecordingKeepsNoMailboxDetail(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	payloads := []feeder.Payload{
		{Kind: vendornotice.PayloadAnnouncements, At: at, Bytes: []byte(`{"announcements":[{"vendor":"google-cloud",` +
			`"product":"cloud sql","kind":"maintenance","window_start":"2026-10-12T02:00:00Z","window_end":"2026-10-12T04:00:00Z",` +
			`"affected_resources":["europe-west1","orders-primary-db"],"notice_id":"GCP-MAINT-4471","pointer":"msg-18c2f9a7e01b",` +
			`"source":"mailbox"}],"considered":3,"source":"mailbox"}`)},
		{Kind: vendornotice.PayloadCycleMarker, At: at, Bytes: []byte(`{"gaps":[{"source":"mailbox",` +
			`"why":"imap: LOGIN failed for oncall@acme.example on imap.acme.example"}]}`)},
	}
	key, err := sanitise.NewKey([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	san, err := sanitise.New(sanitise.ContractPolicy(), key)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "recording")
	tee, err := deployrecord.NewTee(source.NewSliceSource(payloads), "vendornotice", san, dir)
	if err != nil {
		t.Fatal(err)
	}
	tee = tee.WithPrepare(vendornotice.PreparePayload)
	for {
		if _, err := tee.Next(t.Context()); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
	}
	if dropped := tee.Dropped(); len(dropped) > 0 {
		t.Fatalf("payloads were refused rather than recorded: %v", dropped)
	}
	var all strings.Builder
	if err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		raw, err := os.ReadFile(path)
		all.Write(raw)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	written := all.String()
	for _, leak := range []string{"oncall", "acme", "orders-primary-db", "GCP-MAINT-4471", "msg-18c2f9a7e01b"} {
		if strings.Contains(written, leak) {
			t.Errorf("%q reached disk in the recording:\n%s", leak, written)
		}
	}
	for _, kept := range []string{"europe-west1", "google-cloud", "cloud sql", "2026-10-12T02:00:00Z", vendornotice.RecordedGapReason} {
		if !strings.Contains(written, kept) {
			t.Errorf("%q is missing from the recording:\n%s", kept, written)
		}
	}
}
