// SPDX-License-Identifier: Apache-2.0

package gcp_test

import (
	"context"
	"testing"
	"time"

	gcpfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/gcp"
	"github.com/Pierre-Theophile/aisre/internal/gcpx"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The clock skew is observed and published (004 T142; 003 FR-153, 004 FR-058).
//
// `Instruments.Skew` was declared by feature 003 as the destination for this measurement and had no
// caller anywhere in the repository — nor did `Skew.Observe`, nor `Skew.Report`. So FR-153's "the
// skew MUST be reported" was met by an interface method nobody invoked, alongside an accumulator
// nobody fed: the third instance of this project's recurring shape after C4, C5 and C7, and the one
// the Go linter could never have caught, because an exported method on an interface is reachable by
// definition.
//
// The tests below are therefore written against the two things that are assertable from outside:
// what reaches `Instruments`, and what `Skew()` reports.

// recordingInstruments captures the skew report, and nothing else it is asked for.
type recordingInstruments struct {
	gcpx.NopInstruments
	reports []feeder.SkewReport
}

func (r *recordingInstruments) Skew(_ context.Context, report feeder.SkewReport) {
	r.reports = append(r.reports, report)
}

// A cycle carrying audit entries observes GCP's clock against this process's, and publishes the
// observation through the interface that declared it.
func TestTheCyclePublishesTheObservedSkewThroughItsInstruments(t *testing.T) {
	instruments := &recordingInstruments{}
	f, err := gcpfeeder.New(gcpfeeder.Options{
		OrgSlug: "nova", Scope: scope(), Actors: actorPolicy(), Instruments: instruments,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// GCP states it had the entry at 14:05:10. We read it at 14:30, so GCP's delivery clock reads
	// 24m50s BEHIND ours. Both instants are stated by the fixture rather than taken from the machine,
	// so the expected quantity is exact.
	arrived := time.Date(2026, 9, 21, 14, 30, 0, 0, time.UTC)
	page := `{"entries":[{"insertId":"s1",
      "logName":"projects/nova-production/logs/cloudaudit.googleapis.com%2Factivity",
      "timestamp":"2026-09-21T14:05:00Z","receiveTimestamp":"2026-09-21T14:05:10Z",
      "protoPayload":{"serviceName":"run.googleapis.com","methodName":"google.cloud.run.v2.Services.UpdateService",
        "resourceName":"projects/nova-production/locations/europe-west1/services/checkout",
        "authenticationInfo":{"principalEmail":"deployer@nova.example"}}}]}`
	src := &countingSource{payloads: []feeder.Payload{
		{Kind: gcpfeeder.PayloadAuditEntries, At: arrived, Bytes: []byte(page)},
		{Kind: gcpfeeder.PayloadPollMarker, At: arrived, Bytes: []byte(`{"outcome":"complete"}`)},
	}}
	if err := f.Run(context.Background(), src, &recordingEmitter{}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(instruments.reports) != 1 {
		t.Fatalf("Instruments.Skew was called %d time(s), want 1. FR-153 names operational telemetry "+
			"as where the skew is reported, and the interface method existed for a whole feature with "+
			"no caller", len(instruments.reports))
	}
	report := instruments.reports[0]
	if report.Samples == 0 {
		t.Fatal("the published report has no samples; a mean of zero over no observations is not a " +
			"report of zero skew, and the two are what an operator most needs told apart")
	}
	if want := -(24*time.Minute + 50*time.Second); report.Last != want {
		t.Errorf("last skew = %s, want %s. The sign is kept rather than an absolute value: a vendor "+
			"ahead makes facts appear to arrive from the future and one behind makes a poll look like "+
			"it missed something", report.Last, want)
	}
	if report.Threshold != feeder.DefaultSkewThreshold {
		t.Errorf("threshold = %s, want the published default %s", report.Threshold,
			feeder.DefaultSkewThreshold)
	}
	if !report.Beyond() {
		t.Errorf("a %s skew is not Beyond a %s threshold, so nothing would tell the operator",
			report.Last, report.Threshold)
	}
	// And the same measurement is readable from the feeder, which is what makes it assertable at all:
	// a log line is not a measurement a test can read.
	if got := f.Skew(); got.Samples != report.Samples || got.Last != report.Last {
		t.Errorf("Skew() reports %+v but %+v was published; two accounts of one measurement are two "+
			"things to diverge", got, report)
	}
}

// And nothing is corrected. The audit entry is still filed at the instant GCP stated for the event,
// not at the instant we read it — which is the clause FR-153's second half turns on.
//
// This is the assertion that makes the pair worth having: a connector that "fixed" the skew would
// produce a graph that is subtly wrong rather than visibly skewed, and a reader could no longer tell
// "it happened at 14:05" from "we found out at 14:30".
func TestTheSkewIsReportedAndNeitherClockIsCorrected(t *testing.T) {
	idx := gcpfeeder.NewScopedAuditIndex(gcpfeeder.AuditScope{}, 0)
	var skew feeder.Skew
	idx.Skew = &skew

	stated := time.Date(2026, 9, 21, 14, 5, 0, 0, time.UTC)
	arrived := time.Date(2026, 9, 21, 14, 30, 0, 0, time.UTC)
	page := `{"entries":[{"insertId":"s2",
      "logName":"projects/nova-production/logs/cloudaudit.googleapis.com%2Factivity",
      "timestamp":"2026-09-21T14:05:00Z","receiveTimestamp":"2026-09-21T14:05:10Z",
      "protoPayload":{"serviceName":"run.googleapis.com","methodName":"google.cloud.run.v2.Services.UpdateService",
        "resourceName":"projects/nova-production/locations/europe-west1/services/checkout",
        "authenticationInfo":{"principalEmail":"deployer@nova.example"}}}]}`
	if _, err := idx.Ingest([]byte(page), arrived); err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if skew.Report().Samples == 0 {
		t.Fatal("the skew was not observed, so this test is not checking what it claims")
	}

	changes := idx.Unconsumed(arrived.Add(time.Hour))
	if len(changes) != 1 {
		t.Fatalf("the entry produced %d general changes, want 1; with none, the dating assertion "+
			"below asserts nothing", len(changes))
	}
	if got := changes[0].At.UTC(); !got.Equal(stated) {
		t.Errorf("the change is dated %s; GCP stated %s and we read it at %s, so a change dated from "+
			"the arrival would be a skew corrected into the graph, which FR-153 forbids",
			got, stated, arrived)
	}
}

// A run with no audit entries publishes a report of NO samples rather than a mean of zero.
//
// The distinction is the whole reason `samples` is on the report. A GCP estate whose audit stream is
// not in scope, or a cycle that read nothing, has taken no measurement — and "the skew is 0s" would
// tell an operator their clocks agree on evidence that does not exist.
func TestACycleWithNoVendorInstantsPublishesNoSamplesRatherThanZeroSkew(t *testing.T) {
	instruments := &recordingInstruments{}
	f, err := gcpfeeder.New(gcpfeeder.Options{
		OrgSlug: "nova", Scope: scope(), Actors: actorPolicy(), Instruments: instruments,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	src := &countingSource{payloads: []feeder.Payload{
		{Kind: gcpfeeder.PayloadPollMarker, At: time.Now(), Bytes: []byte(`{"outcome":"complete"}`)},
	}}
	if err := f.Run(context.Background(), src, &recordingEmitter{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(instruments.reports) != 1 {
		t.Fatalf("Instruments.Skew was called %d time(s), want 1: a cycle that measured nothing still "+
			"reports, because an absent report and a report of no samples are read differently",
			len(instruments.reports))
	}
	if got := instruments.reports[0]; got.Samples != 0 || got.Mean != 0 {
		t.Errorf("a cycle with no vendor instant published %+v, want a zero-sample report", got)
	}
}

// Nil Instruments is a valid configuration: self-observability is never a precondition for feeding,
// so a replay and a test run without a meter provider.
func TestAFeederWithoutInstrumentsStillRuns(t *testing.T) {
	f := newFeeder(t)
	src := &countingSource{payloads: []feeder.Payload{
		{Kind: gcpfeeder.PayloadPollMarker, At: time.Now(), Bytes: []byte(`{"outcome":"complete"}`)},
	}}
	if err := f.Run(context.Background(), src, &recordingEmitter{}); err != nil {
		t.Fatalf("Run with no Instruments: %v", err)
	}
}
