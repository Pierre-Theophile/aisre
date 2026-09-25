// SPDX-License-Identifier: Apache-2.0

package github_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/feeders/github"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// The doorbell (004 T039–T042; FR-053, FR-054, FR-055, SC-012).
//
// GitHub PUSHES its webhook, so unlike GCP's pull the body arrives whether anybody wants it or not and
// must be read to verify the signature — the signature is computed over it. So the guarantee cannot be
// "the body never enters the process". These tests assert the narrower one that is true: whatever the
// body said, the most it becomes is a number, and a flood of forgeries costs at most one extra read of
// the truth.

const doorbellSecret = "s3cr3t-webhook-key"

// sign produces the header GitHub would send for a body.
func sign(t *testing.T, secret string, body []byte) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// doorbell builds one with a pinned clock a test can advance.
func doorbell(t *testing.T, at *time.Time) *github.Doorbell {
	t.Helper()
	d, err := github.NewDoorbell(github.DoorbellOptions{
		Secret: doorbellSecret,
		Now:    func() time.Time { return *at },
	})
	if err != nil {
		t.Fatalf("NewDoorbell: %v", err)
	}
	return d
}

// ring posts a body with the signature the caller chose, and returns the status.
func ring(d *github.Doorbell, signature string, body []byte) int {
	req := httptest.NewRequest(http.MethodPost, "/hooks/github", strings.NewReader(string(body)))
	if signature != "" {
		req.Header.Set(github.SignatureHeader, signature)
	}
	rec := httptest.NewRecorder()
	d.ServeHTTP(rec, req)
	return rec.Code
}

// A signed notification earns exactly one poll, and the body is never consulted for anything else.
func TestASignedNotificationEarnsOnePoll(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 3, 1, 14, 0, 0, 0, time.UTC)
	d := doorbell(t, &at)
	// A body that would be very interesting if anybody parsed it: it names a deployment that does not
	// exist and claims an environment nobody allowlisted.
	body := []byte(`{"action":"created","deployment":{"id":999999,"environment":"production"}}`)

	if code := ring(d, sign(t, doorbellSecret, body), body); code != http.StatusNoContent {
		t.Fatalf("a signed notification got %d, want 204", code)
	}
	report := d.Report()
	if report.Polls != 1 {
		t.Errorf("polls = %d, want 1", report.Polls)
	}
	if report.ByReason[github.DoorbellPoll] != 1 {
		t.Errorf("the report does not count the honoured ring: %s", report)
	}
	// Nothing about the body survived. The report is the only thing the notification left behind, and
	// it is counts — so there is nowhere for `999999` or `production` to be.
	if rendered := report.String(); strings.Contains(rendered, "999999") ||
		strings.Contains(rendered, "production") {
		t.Errorf("the report carries something from the body: %q. The webhook must not be a source of "+
			"data (FR-053) — a body that reached a report has reached a consumer", rendered)
	}
}

// An unsigned notification is dropped and counted, and earns no poll.
func TestAnUnsignedNotificationIsDroppedAndCounted(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 3, 1, 14, 0, 0, 0, time.UTC)
	d := doorbell(t, &at)
	if code := ring(d, "", []byte(`{}`)); code != http.StatusUnauthorized {
		t.Errorf("an unsigned notification got %d, want 401", code)
	}
	report := d.Report()
	if report.Polls != 0 {
		t.Errorf("an unsigned notification earned %d poll(s)", report.Polls)
	}
	if report.ByReason[github.DoorbellUnsigned] != 1 {
		t.Errorf("the drop was not counted: %s. A doorbell being probed and a doorbell nobody is "+
			"ringing are different operational facts, and only the counters separate them", report)
	}
}

// A forged signature is dropped and counted, and the two drops stay distinguishable.
func TestAForgedSignatureIsDroppedAndCountedSeparately(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 3, 1, 14, 0, 0, 0, time.UTC)
	d := doorbell(t, &at)
	body := []byte(`{"deployment":{"id":1}}`)

	// Signed with the wrong key: the shape an attacker who knows the scheme but not the secret sends.
	if code := ring(d, sign(t, "not-the-secret", body), body); code != http.StatusUnauthorized {
		t.Errorf("a forged signature got %d, want 401", code)
	}
	// A valid signature over a DIFFERENT body: the shape a replayed-and-edited notification takes.
	if code := ring(d, sign(t, doorbellSecret, []byte(`{"deployment":{"id":2}}`)), body); code != http.StatusUnauthorized {
		t.Errorf("a signature over another body got %d, want 401; the signature is over THIS body or "+
			"it is not a signature", code)
	}
	report := d.Report()
	if report.Polls != 0 {
		t.Errorf("a forged notification earned %d poll(s)", report.Polls)
	}
	if got := report.ByReason[github.DoorbellBadSignature]; got != 2 {
		t.Errorf("bad_signature = %d, want 2: %s", got, report)
	}
	if report.ByReason[github.DoorbellUnsigned] != 0 {
		t.Errorf("a forged signature was counted as unsigned; the two are different facts about the "+
			"sender: %s", report)
	}
}

// The older SHA-1 scheme is refused, because accepting it would let the sender choose the weaker check.
func TestTheSha1SignatureSchemeIsRefused(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 3, 1, 14, 0, 0, 0, time.UTC)
	d := doorbell(t, &at)
	body := []byte(`{}`)
	// A correctly-computed SHA-256 digest offered under the sha1 label, and a bare hex digest with no
	// algorithm at all. Both are refused on the prefix, before any comparison.
	digest := strings.TrimPrefix(sign(t, doorbellSecret, body), "sha256=")
	for _, signature := range []string{"sha1=" + digest, digest} {
		if code := ring(d, signature, body); code != http.StatusUnauthorized {
			t.Errorf("signature %q got %d, want 401: an algorithm the sender picks is the sender "+
				"deciding how hard the check is", signature, code)
		}
	}
	if d.Report().Polls != 0 {
		t.Errorf("a differently-labelled signature earned a poll: %s", d.Report())
	}
}

// SC-012: a forged flood produces no node, no change and no event, and costs AT MOST ONE poll.
//
// This is the bound the whole design exists for, and it is a property of the token bucket rather than
// of anybody remembering to check. A thousand notifications inside one interval earn one poll — and
// that poll reads the API, so the worst a flood achieves is one extra read of the truth.
func TestAForgedFloodCostsAtMostOnePoll(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 3, 1, 14, 0, 0, 0, time.UTC)
	d := doorbell(t, &at)
	body := []byte(`{"deployment":{"id":7}}`)
	valid := sign(t, doorbellSecret, body)

	// A thousand: half forged, half genuinely signed, all inside one interval.
	for i := range 1000 {
		signature := valid
		if i%2 == 0 {
			signature = sign(t, "not-the-secret", body)
		}
		ring(d, signature, body)
	}

	report := d.Report()
	if report.Polls != 1 {
		t.Errorf("a flood of 1000 notifications earned %d poll(s), want 1. SC-012 bounds a forged "+
			"flood at one extra poll, and the bound is the bucket rather than a rule somebody "+
			"remembers: %s", report.Polls, report)
	}
	if report.ByReason[github.DoorbellBadSignature] != 500 {
		t.Errorf("the 500 forgeries were not all counted: %s", report)
	}
	// The 499 signed rings that arrived after the token was spent are rate-limited, not silently lost.
	if got := report.ByReason[github.DoorbellRateLimited]; got != 499 {
		t.Errorf("rate_limited = %d, want 499; a dropped ring that is not counted is indistinguishable "+
			"from one that never arrived: %s", got, report)
	}
}

// The bucket refills, so a doorbell is not permanently spent by one burst.
func TestTheBucketRefillsAfterTheInterval(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 3, 1, 14, 0, 0, 0, time.UTC)
	d := doorbell(t, &at)
	body := []byte(`{}`)
	valid := sign(t, doorbellSecret, body)

	ring(d, valid, body)
	ring(d, valid, body) // inside the interval: rate-limited
	if d.Report().Polls != 1 {
		t.Fatalf("two rings inside one interval earned %d poll(s), want 1", d.Report().Polls)
	}
	at = at.Add(github.DefaultDoorbellMinInterval)
	ring(d, valid, body)
	if got := d.Report().Polls; got != 2 {
		t.Errorf("polls = %d after the interval elapsed, want 2. A doorbell permanently spent by one "+
			"burst is a doorbell a flood can switch off", got)
	}
}

// FR-055: the result does not depend on the order the two transports deliver in.
//
// The doorbell and the payload stream are independent, and a doorbell only makes a scheduled poll
// EARLIER. So a ring before the payloads and a ring after them must leave the same graph — which they
// do for a structural reason worth asserting rather than assuming: the doorbell contributes no events
// at all, so it cannot reorder any.
func TestTheResultDoesNotDependOnTheDoorbellsOrder(t *testing.T) {
	t.Parallel()
	body := []byte(`{"deployment":{"id":4321}}`)

	runWithRing := func(before bool) []string {
		at := time.Date(2026, 3, 1, 14, 0, 0, 0, time.UTC)
		d := doorbell(t, &at)
		f := newFeeder(t)
		f.Gate = refusingGate{}
		em := &capturingEmitter{}
		if before {
			ring(d, sign(t, doorbellSecret, body), body)
		}
		if err := f.Run(t.Context(), &payloadSource{payloads: rolloutPayloads(t, "complete")}, em); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if !before {
			ring(d, sign(t, doorbellSecret, body), body)
		}
		var ids []string
		for _, ev := range em.events {
			ids = append(ids, ev.GetEventId())
		}
		return ids
	}

	first, second := runWithRing(true), runWithRing(false)
	if len(first) == 0 {
		t.Fatal("the cycle emitted nothing, so this test compares two empty lists")
	}
	if strings.Join(first, "\n") != strings.Join(second, "\n") {
		t.Errorf("the events differ by when the doorbell rang:\nbefore: %v\nafter:  %v\nA doorbell "+
			"contributes no events, so it cannot change any (FR-055)", first, second)
	}
}

// A body above the cap is refused rather than truncated, and counted under its own reason.
func TestAnOversizedBodyIsRefusedNotTruncated(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 3, 1, 14, 0, 0, 0, time.UTC)
	d := doorbell(t, &at)
	body := []byte(strings.Repeat("x", github.MaxDoorbellBody+1))
	// Correctly signed, so the refusal is about the size and nothing else.
	if code := ring(d, sign(t, doorbellSecret, body), body); code != http.StatusRequestEntityTooLarge {
		t.Errorf("an oversized body got %d, want 413", code)
	}
	report := d.Report()
	if report.ByReason[github.DoorbellTooLarge] != 1 {
		t.Errorf("the refusal was not counted under its own reason: %s. An operator's "+
			"misconfiguration and an attack should not read the same", report)
	}
	if report.ByReason[github.DoorbellBadSignature] != 0 {
		t.Errorf("an oversized body was reported as a bad signature: %s", report)
	}
}

// Anything but POST is refused and counted.
func TestANonPostRequestIsRefused(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 3, 1, 14, 0, 0, 0, time.UTC)
	d := doorbell(t, &at)
	req := httptest.NewRequest(http.MethodGet, "/hooks/github", nil)
	rec := httptest.NewRecorder()
	d.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("a GET got %d, want 405", rec.Code)
	}
	if d.Report().ByReason[github.DoorbellWrongMethod] != 1 {
		t.Errorf("the refusal was not counted: %s", d.Report())
	}
}

// A doorbell with no secret is refused at construction.
//
// Not a doorbell that accepts everything: an unauthenticated webhook is not a weaker doorbell, it is a
// public endpoint anybody can use to make this connector poll.
func TestADoorbellWithoutASecretIsRefused(t *testing.T) {
	t.Parallel()
	for _, secret := range []string{"", "   "} {
		if _, err := github.NewDoorbell(github.DoorbellOptions{Secret: secret}); err == nil {
			t.Errorf("a doorbell with secret %q was accepted; an unauthenticated webhook is a public "+
				"endpoint for making this connector poll", secret)
		}
	}
}

// The report renders deterministically, so a checkpoint note reads the same twice.
func TestTheDoorbellReportIsDeterministic(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 3, 1, 14, 0, 0, 0, time.UTC)
	d := doorbell(t, &at)
	body := []byte(`{}`)
	ring(d, sign(t, doorbellSecret, body), body)
	ring(d, "", body)
	ring(d, sign(t, "wrong", body), body)
	ring(d, sign(t, doorbellSecret, body), body)

	first := d.Report().String()
	for range 20 {
		if again := d.Report().String(); again != first {
			t.Fatalf("the report is not deterministic:\n%s\n%s", first, again)
		}
	}
	for _, want := range []string{"polls=1", "bad_signature=1", "unsigned=1", "rate_limited=1"} {
		if !strings.Contains(first, want) {
			t.Errorf("the report does not contain %q: %s", want, first)
		}
	}
	_ = fmt.Sprint(first)
}

// The comparison is constant-time, asserted from the SOURCE because no behavioural test can see it.
//
// This is the one claim in this file a functional test cannot make. `hmac.Equal` and `==` return the
// same answer for every input; they differ only in how long they take to say it, and a unit test
// observes results rather than durations. Probing it proves the point: replacing `hmac.Equal` with a
// string comparison leaves every other test in this file green.
//
// A timing test would be worse than none. It would have to measure nanosecond differences on a shared
// CI runner, and the only two outcomes are a test that flakes and a test whose threshold is so loose it
// passes on the vulnerable code — which is a guard that reports success for the thing it exists to
// catch.
//
// So the guard is structural, and the repository already works this way: `docs/connectors/github.md`'s
// tables are compared to the enforced ones in both directions rather than trusted. What FR-054 asks
// for is a named function, so this asserts the named function is the one being called.
func TestTheSignatureComparisonIsConstantTime(t *testing.T) {
	t.Parallel()
	source, err := os.ReadFile("doorbell.go")
	if err != nil {
		t.Fatalf("read the doorbell's own source: %v", err)
	}
	body := string(source)
	if !strings.Contains(body, "hmac.Equal(") {
		t.Error("the doorbell does not call hmac.Equal. FR-054 asks for a CONSTANT-TIME comparison, and " +
			"a byte-by-byte one that returns early leaks through timing how much of a guessed " +
			"signature was right — which turns forging one from infeasible into a few thousand " +
			"requests. The rate limit does not help: it bounds polls, not verification attempts")
	}
	// And nothing compares the computed MAC with an operator that returns early. Named explicitly
	// rather than by a general scan, because the point is the comparison of the DIGEST.
	for _, wrong := range []string{
		"string(mac.Sum(nil)) ==",
		"bytes.Equal(mac.Sum(nil)",
		"mac.Sum(nil) ==",
	} {
		if strings.Contains(body, wrong) {
			t.Errorf("the doorbell compares the digest with %q, which is not constant-time", wrong)
		}
	}
}

// What an extra poll costs, asserted where it can be (004 T041, T079; SC-012).
//
// `github-doorbell-forged-01` was built to carry this, and a probe showed it cannot. The fixture reads
// the same window twice and its `events.jsonl` holds one change; plant a second reading that reports
// something DIFFERENT at the same deployment, regenerate, and `fixture verify` still passes every step.
// The reason is structural rather than a defect in the fixture: a replay applies a recorded event log
// and the log deduplicates on event id, so a second event at the same ref is a DUPLICATE_NOOP before
// anything compares its contents — and a change invented at a DIFFERENT ref is not in any golden's
// focus. `fixture verify` never re-runs the mapper (004 T145), so no verification step can see either.
//
// So the fixture pins the rollout and its single version, which it can, and this test carries the claim
// the fixture cannot: two readings of one window produce ONE change, because the change's event id is
// derived from the deployment and its target and from nothing about the reading. That is what makes the
// log's DUPLICATE_NOOP inevitable rather than lucky. An id carrying the poll instant, the attempt, or a
// notification's delivery id would pass every fixture in the corpus and fail here.
//
// This is what a forged or replayed doorbell costs, stated precisely: one extra poll, inside the same
// budget, and not one new fact. The doorbell's BODY reaching nothing is a separate guarantee, enforced
// by `Ring` taking an int and this package having no type a payload could be decoded into.
func TestTwoReadingsOfOneWindowProduceOneChange(t *testing.T) {
	t.Parallel()

	// The set of change event ids one reading of the window produces, and the set two readings of the
	// same window produce. Compared as SETS rather than counted, because the count is a property of
	// the mapping — `rolloutPayloads` maps one deployment onto two targets, so one reading already
	// produces two changes, and an assertion on the number would be measuring that instead.
	readings := func(t *testing.T, count int) map[string]int {
		t.Helper()
		f := newFeeder(t)
		f.Gate = refusingGate{}
		em := &capturingEmitter{}
		var payloads []feeder.Payload
		for i := 0; i < count; i++ {
			// Each reading ARRIVES later than the one before, which is what a second poll is. The
			// bytes are identical — the same truth, read again — and only the arrival instants move.
			// That matters to the probe: two readings at the same instant would agree on an id
			// derived from the poll instant, and the comparison below would pass a feeder that had
			// started writing a new fact per poll.
			for _, payload := range rolloutPayloads(t, "complete") {
				payload.At = payload.At.Add(time.Duration(i) * 10 * time.Minute)
				payloads = append(payloads, payload)
			}
		}
		if err := f.Run(context.Background(), &payloadSource{payloads: payloads}, em); err != nil {
			t.Fatalf("Run with %d readings: %v", count, err)
		}
		ids := map[string]int{}
		for _, ev := range em.events {
			if ev.GetObserveChange() == nil {
				continue
			}
			ids[ev.GetEventId()]++
		}
		if len(ids) == 0 {
			t.Fatalf("%d readings produced no change at all; the precondition is gone", count)
		}
		return ids
	}

	scheduled := readings(t, 1)
	// The scheduled poll, then the early one a ring would trigger: the same window, read again.
	both := readings(t, 2)

	for id := range both {
		if _, known := scheduled[id]; !known {
			t.Errorf("the second reading of the same window produced a change event id the first did "+
				"not: %q. An extra poll must cost a poll and not a fact (SC-012)", id)
		}
	}
	for id := range scheduled {
		if _, still := both[id]; !still {
			t.Errorf("reading the window twice lost change event id %q that reading it once produced", id)
		}
	}

	// And the id says what it is derived from. Asserted on its text as well as on the set equality
	// above, because set equality alone would also hold if the id were derived from something
	// constant that has nothing to do with the deployment.
	for id := range scheduled {
		if !strings.Contains(id, "deployments/4321") {
			t.Errorf("change event id %q does not name the deployment it is about", id)
		}
		for _, reading := range []string{"14:30", "attempt", "poll", "delivery"} {
			if strings.Contains(id, reading) {
				t.Errorf("change event id %q carries %q, something about the READING rather than "+
					"the thing read: an extra poll would then write a new fact", id, reading)
			}
		}
	}
}
