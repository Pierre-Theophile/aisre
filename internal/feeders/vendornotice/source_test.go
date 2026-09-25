// SPDX-License-Identifier: Apache-2.0

package vendornotice_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/deprecation"
	vn "github.com/Pierre-Theophile/aisre/internal/feeders/vendornotice"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The source layer (T082, T083, FR-058, FR-075).

// fakeSource is one adapter, scripted per cycle.
type fakeSource struct {
	kind    vn.SourceKind
	batches []vn.Batch
	errs    []error
	calls   int
}

func (f *fakeSource) Kind() vn.SourceKind { return f.kind }

func (f *fakeSource) Read(context.Context) (vn.Batch, error) {
	i := f.calls
	f.calls++
	if i < len(f.errs) && f.errs[i] != nil {
		return vn.Batch{}, f.errs[i]
	}
	if i < len(f.batches) {
		return f.batches[i], nil
	}
	return vn.Batch{}, nil
}

func announcement(pointer string, source vn.SourceKind) vn.Announcement {
	return vn.Announcement{
		Vendor: "acme-gpu", Product: "inference-api", Kind: vn.KindMaintenance,
		Window:  vn.Window{Start: windowFrom, End: windowTo},
		Pointer: pointer, Source: source,
	}
}

func drain(t *testing.T, src feeder.Source) []feeder.Payload {
	t.Helper()
	var out []feeder.Payload
	for {
		payload, err := src.Next(context.Background())
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		out = append(out, payload)
		if len(out) > 50 {
			t.Fatal("the poller did not end")
		}
	}
}

// An unreachable source is a gap in the cycle marker, not an absence of announcements. This is the
// whole reason the layer exists: a mailbox that times out and a mailbox with nothing in it produce the
// same announcements, and only one of them means the graph knows what happened (FR-075).
func TestAnUnreachableSourceIsAGapAndNotSilence(t *testing.T) {
	t.Parallel()

	working := &fakeSource{kind: vn.SourceStatusPage, batches: []vn.Batch{
		{Announcements: []vn.Announcement{announcement("statuspage:1", vn.SourceStatusPage)}},
	}}
	broken := &fakeSource{kind: vn.SourceMailbox, errs: []error{errors.New("dial tcp: i/o timeout")}}
	poller, err := vn.NewPoller(vn.PollerOptions{Sources: []vn.Announcer{working, broken}})
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}

	payloads := drain(t, poller)
	if len(payloads) != 2 {
		t.Fatalf("one cycle yielded %d payloads, want the working source's batch and the marker", len(payloads))
	}
	if payloads[1].Kind != vn.PayloadCycleMarker {
		t.Fatalf("the cycle does not end with a marker: %v", payloads[1].Kind)
	}
	var marker struct {
		Gaps []struct{ Source, Why string } `json:"gaps"`
	}
	if err := json.Unmarshal(payloads[1].Bytes, &marker); err != nil {
		t.Fatalf("decoding the marker: %v", err)
	}
	if len(marker.Gaps) != 1 || marker.Gaps[0].Source != string(vn.SourceMailbox) {
		t.Fatalf("the unreachable source is not a gap: %+v", marker.Gaps)
	}
	if marker.Gaps[0].Why == "" {
		t.Error("the gap carries no reason; a gap with no reason is a silence with a label on it")
	}
	// And the source that answered is still read: one source failing must not lose the other's work.
	if !strings.Contains(string(payloads[0].Bytes), "statuspage:1") {
		t.Errorf("the working source's announcement was lost: %s", payloads[0].Bytes)
	}
}

// Every source failing still closes the cycle. That cycle is precisely the one whose gap has to reach
// the checkpoint, so it is the one a poller must not skip.
func TestACycleWhereNothingCouldBeReadStillClosesWithItsGaps(t *testing.T) {
	t.Parallel()

	first := &fakeSource{kind: vn.SourceMailbox, errs: []error{errors.New("refused")}}
	second := &fakeSource{kind: vn.SourceChangelog, errs: []error{errors.New("no such host")}}
	poller, err := vn.NewPoller(vn.PollerOptions{Sources: []vn.Announcer{first, second}})
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}

	payloads := drain(t, poller)
	if len(payloads) != 1 || payloads[0].Kind != vn.PayloadCycleMarker {
		t.Fatalf("a cycle that read nothing yielded %d payloads: %+v", len(payloads), payloads)
	}
	var marker struct {
		Gaps []struct{ Source, Why string } `json:"gaps"`
	}
	if err := json.Unmarshal(payloads[0].Bytes, &marker); err != nil {
		t.Fatalf("decoding the marker: %v", err)
	}
	if len(marker.Gaps) != 2 {
		t.Fatalf("two unreachable sources produced %d gaps", len(marker.Gaps))
	}
	// Sorted, so a recording of one cycle is the same bytes twice.
	if marker.Gaps[0].Source > marker.Gaps[1].Source {
		t.Errorf("the gaps are not in source order: %+v", marker.Gaps)
	}
}

// A source that read nothing yields no payload, and the marker still closes the cycle. An empty batch
// is not a gap: the distinction is what the feeder reports as a quiet week versus a blind one.
func TestASourceThatReadNothingIsNotAGap(t *testing.T) {
	t.Parallel()

	quiet := &fakeSource{kind: vn.SourceMailbox}
	poller, err := vn.NewPoller(vn.PollerOptions{Sources: []vn.Announcer{quiet}})
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	payloads := drain(t, poller)
	if len(payloads) != 1 || payloads[0].Kind != vn.PayloadCycleMarker {
		t.Fatalf("a quiet cycle yielded %+v", payloads)
	}
	if got := string(payloads[0].Bytes); strings.Contains(got, "gaps") {
		t.Errorf("a quiet source was recorded as a gap: %s", got)
	}
}

// An unextracted reading travels in the payload. Dropping it here would make a recording say the source
// read less than it did — under-reporting in the direction that hides a miss (FR-074, FR-078).
func TestAnUnreadableAnnouncementTravelsInThePayload(t *testing.T) {
	t.Parallel()

	source := &fakeSource{kind: vn.SourceMailbox, batches: []vn.Batch{{
		Unextracted: []vn.Unextracted{{
			Reason: vn.UnextractedNoWindow, Field: "window", Pointer: "mailbox:msg-9",
			Source: vn.SourceMailbox,
		}},
	}}}
	poller, err := vn.NewPoller(vn.PollerOptions{Sources: []vn.Announcer{source}})
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	payloads := drain(t, poller)
	if len(payloads) != 2 {
		t.Fatalf("a batch of one unextracted reading yielded %d payloads", len(payloads))
	}
	body := string(payloads[0].Bytes)
	for _, want := range []string{"unextracted", string(vn.UnextractedNoWindow), "mailbox:msg-9"} {
		if !strings.Contains(body, want) {
			t.Errorf("the payload does not carry %q: %s", want, body)
		}
	}

	// And the feeder counts it: read and failed-to-extract both move, because the source did read it.
	f := newTestFeeder(t, false)
	em := &recorder{}
	if err := f.Run(context.Background(), &sliceSource{payloads: payloads}, em); err != nil {
		t.Fatalf("Run: %v", err)
	}
	cycle := f.Report()
	if cycle.Read != 1 || cycle.FailedToExtract != 1 {
		t.Errorf("cycle reports read=%d failed=%d, want 1 and 1: a cycle that reported ten read and "+
			"ten extracted while the mailbox held eleven lies about its own coverage",
			cycle.Read, cycle.FailedToExtract)
	}
}

// A poller with no sources is refused. It would yield an unbroken run of empty cycles, which is the
// most convincing impression of a working feeder there is (FR-058).
func TestAPollerWithNoSourcesIsRefused(t *testing.T) {
	t.Parallel()

	if _, err := vn.NewPoller(vn.PollerOptions{}); err == nil {
		t.Fatal("a poller with no sources was built")
	}
	// And two adapters calling themselves the same thing, because a gap could then name a source
	// without saying which one.
	twice := []vn.Announcer{&fakeSource{kind: vn.SourceMailbox}, &fakeSource{kind: vn.SourceMailbox}}
	if _, err := vn.NewPoller(vn.PollerOptions{Sources: twice}); err == nil {
		t.Fatal("two sources of one kind were accepted")
	}
}

// A cadenced poller waits between cycles and stops at the configured count. The wait is asserted
// rather than spent: a test that slept would be a test nobody runs.
func TestACadencedPollerWaitsBetweenCyclesAndNotBeforeTheFirst(t *testing.T) {
	t.Parallel()

	source := &fakeSource{kind: vn.SourceMailbox, batches: []vn.Batch{
		{Announcements: []vn.Announcement{announcement("mailbox:1", vn.SourceMailbox)}},
		{Announcements: []vn.Announcement{announcement("mailbox:2", vn.SourceMailbox)}},
	}}
	var waits []time.Duration
	poller, err := vn.NewPoller(vn.PollerOptions{
		Sources: []vn.Announcer{source},
		Cadence: 15 * time.Minute,
		Cycles:  2,
		Sleep: func(_ context.Context, d time.Duration) error {
			waits = append(waits, d)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}

	payloads := drain(t, poller)
	if len(payloads) != 4 {
		t.Fatalf("two cycles yielded %d payloads, want a batch and a marker each: %+v", len(payloads), payloads)
	}
	if len(waits) != 1 || waits[0] != 15*time.Minute {
		t.Errorf("waits = %v, want exactly one 15m wait — the first cycle must not wait a cadence "+
			"before reading anything", waits)
	}
	if source.calls != 2 {
		t.Errorf("the source was read %d times, want once per cycle", source.calls)
	}
}

// A cancelled context ends the run rather than recording every source as a gap. A cancellation is this
// process being told to stop, and calling that a gap in the vendor's coverage would be a lie about the
// vendor.
func TestACancelledContextEndsTheRunRatherThanRecordingGaps(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	source := &fakeSource{kind: vn.SourceMailbox, errs: []error{context.Canceled}}
	poller, err := vn.NewPoller(vn.PollerOptions{Sources: []vn.Announcer{source}})
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	if _, err := poller.Next(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Next returned %v, want the context's error", err)
	}
}

// The shared client identifies itself and carries nothing about the organisation running it: a user
// agent goes to every third party polled, and an organisation slug in one tells a vendor who is
// watching them.
func TestTheClientIdentifiesItselfWithoutIdentifyingTheOrganisation(t *testing.T) {
	t.Parallel()

	var seen string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("User-Agent")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	client := vn.NewClient(vn.ClientOptions{})
	if _, err := client.Get(context.Background(), server.URL); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if seen == "" {
		t.Fatal("the client sent no User-Agent; a poller a publisher cannot identify is a poller " +
			"they can only block")
	}
	if !strings.Contains(seen, "sre-agent") {
		t.Errorf("the User-Agent %q does not name the project, so a publisher who wants to talk to "+
			"whoever is polling them has nobody to talk to", seen)
	}
	if !strings.Contains(seen, "https://") {
		t.Errorf("the User-Agent %q carries no contact URL", seen)
	}
	if strings.Contains(seen, "twin") || strings.Contains(seen, "@") {
		t.Errorf("the User-Agent %q carries something about the organisation running the feeder", seen)
	}
}

// A publisher's stated limit is an instruction. The client waits it out once; a limit longer than a
// cycle is a refusal, so the cycle records a gap rather than polling through it.
func TestAStatedLimitIsWaitedOutOnceAndThenBecomesAGap(t *testing.T) {
	t.Parallel()

	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	var waited []time.Duration
	client := vn.NewClient(vn.ClientOptions{Sleep: func(_ context.Context, d time.Duration) error {
		waited = append(waited, d)
		return nil
	}})
	resp, err := client.Get(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if resp.Status != http.StatusOK || !strings.Contains(string(resp.Body), "ok") {
		t.Errorf("the retry did not return the answer: %d %s", resp.Status, resp.Body)
	}
	if len(waited) != 1 || waited[0] != 2*time.Second {
		t.Errorf("waited %v, want the stated 2s exactly — a client that waits its own interval is "+
			"ignoring the publisher", waited)
	}

	// A limit longer than one cycle is not waited out at all.
	long := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "86400")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer long.Close()
	slept := false
	patient := vn.NewClient(vn.ClientOptions{Sleep: func(context.Context, time.Duration) error {
		slept = true
		return nil
	}})
	if _, err := patient.Get(context.Background(), long.URL); !errors.Is(err, vn.ErrStatedLimit) {
		t.Errorf("a day-long limit returned %v, want ErrStatedLimit so the cycle records a gap", err)
	}
	if slept {
		t.Error("the client waited out a limit longer than a cycle")
	}
}

// A 404 is an answer, not an error: for the status-page probe it means "not a Statuspage", which is a
// finding rather than a failure (FR-075).
func TestANotFoundIsAnAnswerAndNotAnError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	resp, err := vn.NewClient(vn.ClientOptions{}).Get(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("a 404 was returned as an error: %v", err)
	}
	if resp.Status != http.StatusNotFound {
		t.Errorf("status = %d", resp.Status)
	}
}

// The headers seen on the shared client become announcements through the same pipeline as everything
// else: no new credential, no new cadence, an observation on traffic that already flows (T082).
func TestADeprecationHeaderBecomesAnAnnouncement(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Deprecation", "@1767225600")
		w.Header().Set("Sunset", "Tue, 31 Mar 2026 23:59:59 GMT")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	log := deprecation.NewLog()
	client := vn.NewClient(vn.ClientOptions{Deprecations: log})
	if _, err := client.Get(context.Background(), server.URL+"/v1/inference"); err != nil {
		t.Fatalf("Get: %v", err)
	}

	// The allowlist attributes it by host, which is the same configured assertion C6 rests on. The
	// test server's host is what the response came from, so that is what the allowlist must map.
	host := strings.TrimPrefix(server.URL, "http://")
	list, err := vn.NewAllowlist([]vn.Vendor{{
		Slug: "acme-gpu", Name: "Acme GPU", Products: []string{"inference-api"},
		Hosts: []string{host},
	}})
	if err != nil {
		t.Fatalf("NewAllowlist: %v", err)
	}

	batch, err := vn.NewDeprecationHeaders(log, list).Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(batch.Announcements) != 1 {
		t.Fatalf("the headers produced %d announcements: %+v", len(batch.Announcements), batch)
	}
	got := batch.Announcements[0]
	if got.Kind != vn.KindDeprecation {
		t.Errorf("kind = %q, want a deprecation", got.Kind)
	}
	if got.Source != vn.SourceDeprecationHeader {
		t.Errorf("source = %q", got.Source)
	}
	if got.Window.StartUnknown || got.Window.Start.IsZero() || got.Window.End.IsZero() {
		t.Errorf("the pair did not become an interval: %+v", got.Window)
	}
	if !strings.Contains(got.Pointer, "/v1/inference") {
		t.Errorf("pointer = %q, want the resource that carried the header", got.Pointer)
	}
}

// A sunset with no deprecation is a window whose start is marked unknown. Dating it at the instant we
// noticed would put a valid-from on a change the vendor never dated (FR-069).
func TestASunsetWithNoDeprecationHasAnUnknownStart(t *testing.T) {
	t.Parallel()

	log := deprecation.NewLog()
	log.Observe("api.acme-gpu.test", "https://api.acme-gpu.test/v1/inference",
		http.Header{"Sunset": []string{"Tue, 31 Mar 2026 23:59:59 GMT"}})
	// The host mapping is stated, so the product is not this test's variable: what is under test is
	// what a missing `Deprecation` does to the window.
	vendor := testVendor()
	vendor.HostProducts = map[string]string{"api.acme-gpu.test": "inference-api"}
	list, err := vn.NewAllowlist([]vn.Vendor{vendor})
	if err != nil {
		t.Fatalf("NewAllowlist: %v", err)
	}
	batch, err := vn.NewDeprecationHeaders(log, list).Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(batch.Announcements) != 1 {
		t.Fatalf("got %d announcements: %+v", len(batch.Announcements), batch)
	}
	window := batch.Announcements[0].Window
	if !window.StartUnknown {
		t.Errorf("the start is %s rather than marked unknown; the vendor did not say when it was "+
			"deprecated (FR-069)", window.Start)
	}
	if !window.Start.IsZero() {
		t.Errorf("an unknown start carries the instant %s", window.Start)
	}
}

// A header cannot say which product it is about. Where the allowlist does not say either, the reading
// is unextracted naming the product — visible, and not a removal date put on the wrong thing.
func TestAHeaderThatCannotBeAttributedToAProductIsUnextracted(t *testing.T) {
	t.Parallel()

	list, err := vn.NewAllowlist([]vn.Vendor{{
		Slug: "acme-gpu", Products: []string{"inference-api", "training"},
		Hosts: []string{"acme-gpu.test"},
	}})
	if err != nil {
		t.Fatalf("NewAllowlist: %v", err)
	}
	log := deprecation.NewLog()
	log.Observe("api.acme-gpu.test", "https://api.acme-gpu.test/v1/x",
		http.Header{"Deprecation": []string{"@1767225600"}})

	batch, err := vn.NewDeprecationHeaders(log, list).Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(batch.Announcements) != 0 {
		t.Fatalf("a header on a two-product vendor was attributed to %+v", batch.Announcements)
	}
	if len(batch.Unextracted) != 1 || batch.Unextracted[0].Reason != vn.UnextractedNoProduct {
		t.Fatalf("unextracted = %+v, want one naming the product", batch.Unextracted)
	}
	if batch.Unextracted[0].Field != "product" {
		t.Errorf("the record does not name the field that failed (FR-073): %+v", batch.Unextracted[0])
	}

	// And an operator who states the mapping gets the announcement.
	mapped, err := vn.NewAllowlist([]vn.Vendor{{
		Slug: "acme-gpu", Products: []string{"inference-api", "training"},
		Hosts:        []string{"acme-gpu.test"},
		HostProducts: map[string]string{"api.acme-gpu.test": "inference-api"},
	}})
	if err != nil {
		t.Fatalf("NewAllowlist: %v", err)
	}
	log.Observe("api.acme-gpu.test", "https://api.acme-gpu.test/v1/x",
		http.Header{"Deprecation": []string{"@1767225600"}})
	batch, err = vn.NewDeprecationHeaders(log, mapped).Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(batch.Announcements) != 1 || batch.Announcements[0].Product != "inference-api" {
		t.Fatalf("the stated host mapping was not used: %+v", batch)
	}
}

// A header from a host nobody allowlisted is a drop with its reason, not an announcement about a vendor
// invented from a domain name (FR-059).
func TestAHeaderFromAnUnknownHostIsADrop(t *testing.T) {
	t.Parallel()

	list, err := vn.NewAllowlist([]vn.Vendor{testVendor()})
	if err != nil {
		t.Fatalf("NewAllowlist: %v", err)
	}
	log := deprecation.NewLog()
	log.Observe("api.someone-else.test", "https://api.someone-else.test/v1/x",
		http.Header{"Sunset": []string{"Tue, 31 Mar 2026 23:59:59 GMT"}})

	headers := vn.NewDeprecationHeaders(log, list)
	batch, err := headers.Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(batch.Announcements) != 0 || len(batch.Unextracted) != 0 {
		t.Fatalf("an unallowlisted host produced %+v", batch)
	}
	drops := headers.Drops()
	if len(drops) != 1 || drops[0].Reason != vn.DropVendorNotAllowlisted {
		t.Fatalf("drops = %+v, want one naming the vendor as not allowlisted", drops)
	}
}
