// SPDX-License-Identifier: Apache-2.0

package gcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"cloud.google.com/go/run/apiv2/runpb"
	metricpb "google.golang.org/genproto/googleapis/api/metric"
	"google.golang.org/genproto/googleapis/cloud/audit"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	gcpfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/gcp"
	"github.com/Pierre-Theophile/aisre/internal/gcpx"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/emit"
)

// The live poller (T185). What these protect: a live cycle produces payloads the feeder's ONE decoder
// reads, in the order it needs them; a failed read makes the cycle partial and names it rather than
// vanishing; a budget refusal reads as a quota stop and never as "nothing found"; and the audit
// window only advances over a read that finished.

const (
	pollProject = "proj-a"
	pollRegion  = "europe-west1"
)

var pollStart = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

type fakeRun struct {
	services     []*runpb.Service
	revisions    []*runpb.Revision
	servicesErr  error
	revisionsErr error
}

func (f *fakeRun) ListServices(context.Context, string, string) ([]*runpb.Service, error) {
	return f.services, f.servicesErr
}

func (f *fakeRun) ListRevisions(_ context.Context, _, _, service string) ([]*runpb.Revision, error) {
	if service != "-" {
		return nil, errors.New("the poller must list every service's revisions in one call")
	}
	return f.revisions, f.revisionsErr
}

type fakeMonitoring struct {
	policies []*monitoringpb.AlertPolicy
	errs     []error
	calls    int
}

func (f *fakeMonitoring) ListAlertPolicies(context.Context, string) ([]*monitoringpb.AlertPolicy, error) {
	f.calls++
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		return nil, err
	}
	return f.policies, nil
}

func (f *fakeMonitoring) ListTimeSeries(context.Context, *monitoringpb.ListTimeSeriesRequest) ([]*monitoringpb.TimeSeries, error) {
	return nil, errors.New("the poller reads no time series")
}

func (f *fakeMonitoring) GetMetricDescriptor(context.Context, string, string) (*metricpb.MetricDescriptor, error) {
	return nil, errors.New("the poller reads no metric descriptor")
}

// fakeLogs serves pages in order and records the requests it was asked.
type fakeLogs struct {
	pages    []auditPage
	requests []*loggingpb.ListLogEntriesRequest
	err      error
}

type auditPage struct {
	entries []*loggingpb.LogEntry
	next    string
}

func (f *fakeLogs) ListEntries(_ context.Context, req *loggingpb.ListLogEntriesRequest) ([]*loggingpb.LogEntry, string, error) {
	f.requests = append(f.requests, req)
	if f.err != nil {
		return nil, "", f.err
	}
	if len(f.pages) == 0 {
		return nil, "", nil
	}
	page := f.pages[0]
	f.pages = f.pages[1:]
	return page.entries, page.next, nil
}

func service(name string) *runpb.Service {
	created := timestamppb.New(pollStart.Add(-48 * time.Hour))
	return &runpb.Service{
		Name:                "projects/" + pollProject + "/locations/" + pollRegion + "/services/" + name,
		Uid:                 "uid-" + name,
		CreateTime:          created,
		UpdateTime:          created,
		LatestReadyRevision: "projects/" + pollProject + "/locations/" + pollRegion + "/services/" + name + "/revisions/" + name + "-00001-abc",
		TrafficStatuses: []*runpb.TrafficTargetStatus{{
			Type:     runpb.TrafficTargetAllocationType_TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION,
			Revision: name + "-00001-abc", Percent: 100,
		}},
	}
}

func revision(svc, name string) *runpb.Revision {
	return &runpb.Revision{
		Name:       "projects/" + pollProject + "/locations/" + pollRegion + "/services/" + svc + "/revisions/" + name,
		Uid:        "uid-" + name,
		Service:    svc,
		CreateTime: timestamppb.New(pollStart.Add(-48 * time.Hour)),
	}
}

func auditEntry(t *testing.T, id string, at time.Time) *loggingpb.LogEntry {
	t.Helper()
	payload, err := anypb.New(&audit.AuditLog{
		ServiceName:  "run.googleapis.com",
		MethodName:   "google.cloud.run.v2.Services.UpdateService",
		ResourceName: "projects/" + pollProject + "/locations/" + pollRegion + "/services/storefront",
	})
	if err != nil {
		t.Fatal(err)
	}
	return &loggingpb.LogEntry{
		InsertId:         id,
		LogName:          "projects/" + pollProject + "/logs/cloudaudit.googleapis.com%2Factivity",
		Timestamp:        timestamppb.New(at),
		ReceiveTimestamp: timestamppb.New(at.Add(20 * time.Second)),
		Payload:          &loggingpb.LogEntry_ProtoPayload{ProtoPayload: payload},
	}
}

type pollerRig struct {
	run      *fakeRun
	mon      *fakeMonitoring
	logs     *fakeLogs
	now      time.Time
	waits    []time.Duration
	connects int
}

func newRig() *pollerRig {
	return &pollerRig{
		run: &fakeRun{
			services:  []*runpb.Service{service("storefront")},
			revisions: []*runpb.Revision{revision("storefront", "storefront-00001-abc")},
		},
		mon:  &fakeMonitoring{},
		logs: &fakeLogs{},
		now:  pollStart,
	}
}

func (r *pollerRig) poller(t *testing.T, cycles int) *gcpfeeder.Poller {
	t.Helper()
	p, err := gcpfeeder.NewPoller(gcpfeeder.PollerOptions{
		Connect: func(context.Context) (*gcpfeeder.Transport, error) {
			r.connects++
			return &gcpfeeder.Transport{Run: r.run, Monitoring: r.mon, AuditLogs: r.logs}, nil
		},
		Scope:    gcpfeeder.Scope{Projects: []string{pollProject}, Regions: []string{pollRegion}},
		Interval: 5 * time.Minute,
		Cycles:   cycles,
		Now:      func() time.Time { return r.now },
		Wait: func(_ context.Context, d time.Duration) error {
			r.waits = append(r.waits, d)
			r.now = r.now.Add(d)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func drain(t *testing.T, src feeder.Source) []feeder.Payload {
	t.Helper()
	var out []feeder.Payload
	for {
		p, err := src.Next(t.Context())
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		out = append(out, p)
	}
}

func kinds(payloads []feeder.Payload) string {
	out := make([]string, len(payloads))
	for i, p := range payloads {
		out[i] = p.Kind
	}
	return strings.Join(out, ",")
}

func marker(t *testing.T, payloads []feeder.Payload) gcpfeeder.PollMarker {
	t.Helper()
	last := payloads[len(payloads)-1]
	if last.Kind != gcpfeeder.PayloadPollMarker {
		t.Fatalf("the cycle does not end with its marker: %s", kinds(payloads))
	}
	var m gcpfeeder.PollMarker
	if err := json.Unmarshal(last.Bytes, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// The central claim: what the poller writes is what the feeder reads. A live cycle is run through the
// real feeder, so a payload shape the decoder refuses fails here rather than on the first live run.
func TestALiveCycleIsReadByTheFeedersOwnDecoder(t *testing.T) {
	t.Parallel()
	rig := newRig()
	rig.logs.pages = []auditPage{{entries: []*loggingpb.LogEntry{auditEntry(t, "e1", pollStart.Add(-10*time.Minute))}}}
	p := rig.poller(t, 1)
	if rig.connects != 0 {
		t.Fatal("the transport was connected before anything was read; it must wait for the gate")
	}

	f, err := gcpfeeder.New(gcpfeeder.Options{
		OrgSlug: "acme",
		Scope:   gcpfeeder.Scope{Projects: []string{pollProject}, Regions: []string{pollRegion}},
		Horizon: gcpfeeder.Horizon{Earliest: pollStart.Add(-30 * 24 * time.Hour), Reason: gcpfeeder.HorizonConfigured},
	})
	if err != nil {
		t.Fatal(err)
	}
	memory := emit.NewMemoryEmitter(f.Describe())
	src := &tap{inner: p}
	if err := f.Run(t.Context(), src, memory); err != nil {
		t.Fatalf("the feeder refused a live cycle: %v", err)
	}
	if got, want := kinds(src.seen), "services,revisions,alert_policies,audit,poll"; got != want {
		t.Errorf("payload order = %s, want %s", got, want)
	}
	if len(memory.Events()) == 0 {
		t.Error("a live cycle with a service in it emitted no event")
	}
	m := marker(t, src.seen)
	if m.Outcome != "complete" || len(m.Covered) != 1 || m.Covered[0] != pollProject+"/"+pollRegion {
		t.Errorf("marker = %+v, want complete and covering %s/%s", m, pollProject, pollRegion)
	}
	audit := string(src.seen[3].Bytes)
	for _, field := range []string{`"insertId":"e1"`, `"serviceName":"run.googleapis.com"`, `"timestamp":`} {
		if !strings.Contains(audit, field) {
			t.Errorf("the audit payload lacks %s; the AuditLog inside protoPayload was not written as the API's JSON:\n%s", field, audit)
		}
	}
}

type tap struct {
	inner feeder.Source
	seen  []feeder.Payload
}

func (s *tap) Next(ctx context.Context) (feeder.Payload, error) {
	p, err := s.inner.Next(ctx)
	if err == nil {
		s.seen = append(s.seen, p)
	}
	return p, err
}

// A failed read is a partial cycle that names the read. It is not the end of the run and not a quiet
// window: the services that were read are still written, and the pair is not covered.
func TestAFailedReadMakesThePartialCycleSayWhich(t *testing.T) {
	t.Parallel()
	rig := newRig()
	rig.run.revisionsErr = errors.New("googleapi: Error 503")
	payloads := drain(t, rig.poller(t, 1))
	m := marker(t, payloads)
	if m.Outcome != "partial" || !strings.Contains(m.Reason, "run.revisions.list "+pollProject+"/"+pollRegion) {
		t.Errorf("marker = %+v, want partial naming the revisions read", m)
	}
	if len(m.Covered) != 0 {
		t.Errorf("covered = %v; a pair whose revisions were not read is not covered", m.Covered)
	}
	if m.StopReason == string(gcpx.StopQuota) {
		t.Error("an API error was reported as a quota stop")
	}
	if !strings.Contains(kinds(payloads), "services") {
		t.Error("the services that were read were not written")
	}
}

// A reserve yield is a quota stop, which is never evidence of absence (FR-149).
func TestABudgetRefusalIsAQuotaStopNotAQuietWindow(t *testing.T) {
	t.Parallel()
	rig := newRig()
	rig.mon.errs = []error{&gcpx.YieldError{Class: gcpx.ClassMonitoringPolicies, Kind: gcpx.YieldReserve, RetryAt: pollStart.Add(time.Minute)}}
	m := marker(t, drain(t, rig.poller(t, 1)))
	if m.Outcome != "partial" || m.StopReason != string(gcpx.StopQuota) {
		t.Errorf("marker = %+v, want partial with stop reason %s", m, gcpx.StopQuota)
	}
}

// Pacing is a delay, not a refusal: it is waited out and the read happens.
func TestAPacedReadIsWaitedOutNotReportedAsQuota(t *testing.T) {
	t.Parallel()
	rig := newRig()
	rig.mon.errs = []error{&gcpx.YieldError{Class: gcpx.ClassMonitoringPolicies, Kind: gcpx.YieldPaced, RetryAt: pollStart.Add(3 * time.Second)}}
	m := marker(t, drain(t, rig.poller(t, 1)))
	if m.Outcome != "complete" || m.StopReason != "" {
		t.Errorf("marker = %+v, want a complete cycle once the pacing was waited out", m)
	}
	if rig.mon.calls != 2 || len(rig.waits) != 1 || rig.waits[0] != 3*time.Second {
		t.Errorf("calls = %d, waits = %v; want the read retried once after the 3s the budget asked for", rig.mon.calls, rig.waits)
	}
}

// The audit window: an empty page WITH a token is not the end of the search, the window advances
// only over a read that finished, and the next cycle re-reads the overlap behind its start.
func TestTheAuditWindowAdvancesOnlyOverAFinishedRead(t *testing.T) {
	t.Parallel()
	rig := newRig()
	rig.logs.pages = []auditPage{
		{next: "still-searching"},
		{entries: []*loggingpb.LogEntry{auditEntry(t, "e1", pollStart.Add(-5*time.Minute))}},
	}
	p := rig.poller(t, 2)
	payloads := drain(t, p)
	if n := strings.Count(kinds(payloads), "audit"); n != 3 {
		t.Errorf("audit payloads = %d, want 3: two pages in the first cycle, one in the second", n)
	}
	if len(rig.logs.requests) != 3 || rig.logs.requests[1].GetPageToken() != "still-searching" {
		t.Fatalf("requests = %v; the empty page's token must be followed", rig.logs.requests)
	}
	first := pollStart.Add(-gcpfeeder.DefaultAuditLookback).Format(time.RFC3339)
	second := pollStart.Add(-gcpfeeder.DefaultReorderingWindow).Format(time.RFC3339)
	if !strings.Contains(rig.logs.requests[0].GetFilter(), first) {
		t.Errorf("first window filter = %q, want it from %s", rig.logs.requests[0].GetFilter(), first)
	}
	if !strings.Contains(rig.logs.requests[2].GetFilter(), second) {
		t.Errorf("second window filter = %q, want it from %s (the first cycle's start less the overlap)",
			rig.logs.requests[2].GetFilter(), second)
	}
	if !strings.Contains(rig.logs.requests[0].GetFilter(), "cloudaudit.googleapis.com%2Factivity") {
		t.Errorf("filter %q does not restrict to the admin-activity stream (FR-042)", rig.logs.requests[0].GetFilter())
	}
}

func TestAnUnfinishedAuditReadIsReReadFromTheSameStart(t *testing.T) {
	t.Parallel()
	rig := newRig()
	rig.logs.err = errors.New("googleapi: Error 500")
	p := rig.poller(t, 2)
	payloads := drain(t, p)
	if m := marker(t, payloads); m.Outcome != "partial" || !strings.Contains(m.Reason, "logging.entries.list") {
		t.Errorf("marker = %+v, want partial naming the audit read", m)
	}
	if got := rig.logs.requests[1].GetFilter(); got != rig.logs.requests[0].GetFilter() {
		t.Errorf("second cycle filter = %q, want the same start as the failed read %q", got, rig.logs.requests[0].GetFilter())
	}
}

func TestAPollerNeedsAScope(t *testing.T) {
	t.Parallel()
	connect := func(context.Context) (*gcpfeeder.Transport, error) { return &gcpfeeder.Transport{}, nil }
	for name, scope := range map[string]gcpfeeder.Scope{
		"no projects": {Regions: []string{pollRegion}},
		"no regions":  {Projects: []string{pollProject}},
	} {
		if _, err := gcpfeeder.NewPoller(gcpfeeder.PollerOptions{Connect: connect, Scope: scope}); err == nil {
			t.Errorf("%s: accepted; a scope of none reads nothing and reports a quiet window", name)
		}
	}
}

// The first live run's IAM entry: an AuditLog whose deprecated `serviceData` holds a type this binary
// does not link. It must be written without the field, not end the run; and an entry whose whole
// payload is unresolvable is skipped and named, so the cycle is partial rather than quiet.
func TestAnAuditEntryWithAnUnresolvableAnyIsStillRead(t *testing.T) {
	t.Parallel()
	rig := newRig()
	withServiceData := auditEntry(t, "iam-1", pollStart.Add(-5*time.Minute))
	var log audit.AuditLog
	if err := withServiceData.GetProtoPayload().UnmarshalTo(&log); err != nil {
		t.Fatal(err)
	}
	log.ServiceData = &anypb.Any{TypeUrl: "type.googleapis.com/google.iam.admin.v1.AuditData", Value: []byte{0x0a, 0x01, 0x78}} //nolint:staticcheck // the deprecated field the live run met
	repacked, err := anypb.New(&log)
	if err != nil {
		t.Fatal(err)
	}
	withServiceData.Payload = &loggingpb.LogEntry_ProtoPayload{ProtoPayload: repacked}
	opaque := auditEntry(t, "opaque-1", pollStart.Add(-4*time.Minute))
	opaque.Payload = &loggingpb.LogEntry_ProtoPayload{ProtoPayload: &anypb.Any{TypeUrl: "type.googleapis.com/vendor.Unknown", Value: []byte{0x0a, 0x01, 0x78}}}
	rig.logs.pages = []auditPage{{entries: []*loggingpb.LogEntry{withServiceData, opaque}}}

	payloads := drain(t, rig.poller(t, 1))
	var auditPayload string
	for _, p := range payloads {
		if p.Kind == gcpfeeder.PayloadAuditEntries {
			auditPayload = string(p.Bytes)
		}
	}
	if !strings.Contains(auditPayload, `"insertId":"iam-1"`) || strings.Contains(auditPayload, "serviceData") {
		t.Errorf("the IAM entry was not written without its serviceData:\n%s", auditPayload)
	}
	if strings.Contains(auditPayload, "opaque-1") {
		t.Errorf("an entry nothing can encode was written:\n%s", auditPayload)
	}
	if m := marker(t, payloads); m.Outcome != "partial" || !strings.Contains(m.Reason, "opaque-1") {
		t.Errorf("marker = %+v, want partial naming the skipped entry", m)
	}
}
