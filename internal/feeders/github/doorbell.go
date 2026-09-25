// SPDX-License-Identifier: Apache-2.0

package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// The doorbell (004 T039–T042; FR-053, FR-054, FR-055, SC-012).
//
// An inbound GitHub webhook may be configured. It means **"poll now" and nothing else**.
//
// ---------------------------------------------------------------------------------------------
// Why this file differs from internal/feeders/gcp/doorbell.go, and what that costs
//
// GCP's doorbell PULLS: `Drain` asks a subscription how many messages are waiting and returns an
// int, so no notification body ever enters the process and the guarantee is free. There is nothing
// in scope for a body to be.
//
// GitHub PUSHES. The body arrives at this process whether anybody wants it or not, and it cannot be
// refused before it is read, because the signature is computed **over the body** — verifying the
// sender requires having the bytes. So the guarantee here cannot be "the body never arrives". It has
// to be narrower and it has to be visible in the signatures:
//
//   - the body exists as a local `[]byte` inside `ServeHTTP` and nowhere else. `Doorbell` has no
//     field it could be stored in, and no method takes a payload argument;
//   - `verify` takes the bytes and returns a **bool**. It is the only function that sees them and it
//     cannot pass them on;
//   - `Ring` takes an **int**, exactly as GCP's does. Whatever the body said, the most it can become
//     is a number;
//   - the body is never decoded. There is no struct in this package for a webhook payload, so there
//     is nothing for a future edit to unmarshal into without adding a type a reviewer would see.
//
// FR-053 is the reason: the webhook must not be a source of data, and the feeder must re-read the
// object from the API. A body that was parsed "just for the deployment id" would be a second source
// with no read-only gate, no metered door, no quota accounting and no checkpoint — and it would be
// trusted, because it arrived signed.
//
// # Why a forged flood costs at most one poll
//
// The rate limit is a token bucket, one token per interval, burst one. A thousand forged
// notifications inside one interval earn one poll at most, and that poll reads the API — the truth —
// so the worst a flood achieves is one extra read of the truth. That is SC-012's bound, and it is a
// property of the bucket rather than of anybody remembering to check.
//
// Unsigned and badly-signed notifications never reach the bucket at all: they are counted and
// dropped. Counting them matters as much as dropping them — a doorbell being probed and a doorbell
// nobody is ringing are different operational facts, and only the counters can tell them apart.

// DefaultDoorbellMinInterval is the shortest gap between honoured rings.
//
// The poll interval, because a doorbell that could trigger a poll more often than the schedule's own
// floor would let a flood outspend the schedule it is supposed to anticipate. The doorbell exists to
// make a poll EARLIER, not to make polls more frequent.
const DefaultDoorbellMinInterval = DefaultPollInterval

// DefaultDoorbellBurst is how many honoured rings may happen back to back before the interval binds.
// One: a burst is the shape a forged flood takes, and a real burst carries no more information than
// its first message, because the poll reads the whole window either way.
const DefaultDoorbellBurst = 1

// MaxDoorbellBody is the most this endpoint will read from a request.
//
// GitHub's own limit is 25 MB; this is far below it deliberately. The body is only ever hashed, never
// parsed, so the only thing a larger body could buy an attacker is memory — and a signature over the
// first 64 KiB of a bigger payload fails, which is the correct outcome for a request this connector
// cannot verify.
const MaxDoorbellBody = 64 << 10

// SignatureHeader is the header GitHub signs with. `X-Hub-Signature-256` is HMAC-SHA256; the older
// `X-Hub-Signature` is HMAC-SHA1 and is deliberately NOT accepted — accepting it would let a sender
// choose the weaker algorithm, which is the sender choosing how hard the check is.
const SignatureHeader = "X-Hub-Signature-256"

// ErrNoSecret is returned for a doorbell with no secret. It is an error rather than a doorbell that
// accepts everything, because an unauthenticated webhook is not a weaker doorbell — it is a public
// endpoint that anybody can use to make this connector poll.
var ErrNoSecret = errors.New("github: a doorbell with no webhook secret")

// DoorbellOptions configures one doorbell.
type DoorbellOptions struct {
	// Secret is the webhook secret the operator configured on the GitHub side. Required.
	Secret string
	// MinInterval is the shortest gap between honoured rings. Zero uses the default.
	MinInterval time.Duration
	// Burst is how many honoured rings may happen back to back. Zero uses the default.
	Burst int
	// Now is the clock. Nil uses time.Now.
	Now func() time.Time
}

// Doorbell decides whether a notification earns an early poll.
//
// Every field is a counter, a limit or a clock. There is deliberately nothing here that could hold a
// notification's contents.
type Doorbell struct {
	secret      []byte
	minInterval time.Duration
	burst       int
	now         func() time.Time

	mu       sync.Mutex
	tokens   int
	lastFill time.Time
	// counters, for the report. They are the only thing a notification leaves behind.
	byReason map[string]int
	polls    int
}

// NewDoorbell returns a doorbell over opts.
func NewDoorbell(opts DoorbellOptions) (*Doorbell, error) {
	if strings.TrimSpace(opts.Secret) == "" {
		return nil, ErrNoSecret
	}
	interval := opts.MinInterval
	if interval <= 0 {
		interval = DefaultDoorbellMinInterval
	}
	burst := opts.Burst
	if burst <= 0 {
		burst = DefaultDoorbellBurst
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Doorbell{
		secret:      []byte(opts.Secret),
		minInterval: interval,
		burst:       burst,
		now:         now,
		tokens:      burst,
		lastFill:    now(),
		byReason:    map[string]int{},
	}, nil
}

// The published doorbell outcomes. A closed set, so a report can be read without parsing prose.
const (
	// DoorbellPoll is a ring that earned an early poll.
	DoorbellPoll = "poll_now"
	// DoorbellRateLimited is a ring inside the minimum interval. Dropping it costs nothing: the
	// scheduled poll is still coming, and it reads the same window.
	DoorbellRateLimited = "rate_limited"
	// DoorbellUnsigned is a request carrying no signature header at all.
	DoorbellUnsigned = "unsigned"
	// DoorbellBadSignature is a request whose signature did not verify.
	DoorbellBadSignature = "bad_signature"
	// DoorbellTooLarge is a body above MaxDoorbellBody.
	DoorbellTooLarge = "body_too_large"
	// DoorbellWrongMethod is anything but POST.
	DoorbellWrongMethod = "wrong_method"
)

// Ring records that a verified notification arrived and returns whether to poll now.
//
// It takes **no payload**, which is the guarantee rather than a convention: a function that cannot
// receive the body cannot parse it, cannot store it, and cannot be talked into trusting it — and a
// future edit that wanted to would have to change this signature, which is a diff a reviewer sees.
//
// `count` is how many arrived and is used only to count them. One notification and fifty earn exactly
// the same single poll, because the poll reads the whole window regardless.
func (d *Doorbell) Ring(count int) bool {
	if count <= 0 {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	now := d.now()
	// A plain token bucket: one token per interval, capped at the burst. This is the rate limit
	// FR-054 names, and it is what bounds a flood to one extra poll per interval however many
	// messages arrive (SC-012).
	if elapsed := now.Sub(d.lastFill); elapsed >= d.minInterval {
		d.tokens = min(d.burst, d.tokens+int(elapsed/d.minInterval))
		d.lastFill = now
	}
	if d.tokens <= 0 {
		d.byReason[DoorbellRateLimited] += count
		return false
	}
	d.tokens--
	d.byReason[DoorbellPoll] += count
	d.polls++
	return true
}

// ServeHTTP is the endpoint. It verifies, counts, and rings — and never does anything else.
//
// The body is read into a local, hashed, and dropped. Nothing is decoded, nothing is stored, and the
// only value that outlives this function is a counter.
//
// The response says nothing about what was found: 204 for a verified ring, 401 for anything that did
// not verify. In particular a rate-limited ring also gets 204, because whether this connector polled
// is not the sender's business and a distinguishable response would let a prober measure the bucket.
func (d *Doorbell) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		d.count(DoorbellWrongMethod)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	signature := strings.TrimSpace(r.Header.Get(SignatureHeader))
	if signature == "" {
		d.count(DoorbellUnsigned)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	// Bounded, and one byte over the bound is a refusal rather than a truncation: a signature over a
	// truncated body would fail anyway, and reporting "too large" rather than "bad signature" keeps
	// an operator's misconfiguration distinguishable from an attack.
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxDoorbellBody+1))
	if err != nil || len(body) > MaxDoorbellBody {
		d.count(DoorbellTooLarge)
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}

	if !d.verify(signature, body) {
		d.count(DoorbellBadSignature)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	// Verified. From here the body is not consulted again, and `Ring` cannot see it.
	d.Ring(1)
	w.WriteHeader(http.StatusNoContent)
}

// verify checks GitHub's HMAC-SHA256 signature in constant time (FR-054).
//
// It takes the bytes and returns a bool. That is the whole of its contract: it is the only function in
// this package that sees a notification's contents, and it cannot hand them anywhere.
//
// `hmac.Equal` rather than `==`: a byte-by-byte comparison that returns early leaks, through timing,
// how much of a guessed signature was right, which turns forging one from infeasible into a few
// thousand requests. The rate limit does not help here — it bounds polls, not verification attempts.
func (d *Doorbell) verify(signature string, body []byte) bool {
	hexDigest, ok := strings.CutPrefix(signature, "sha256=")
	if !ok {
		// An unprefixed or `sha1=` signature. Refused rather than guessed at: accepting the older
		// algorithm would let the sender choose how strong the check is.
		return false
	}
	want, err := hex.DecodeString(hexDigest)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, d.secret)
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), want)
}

// count records one outcome that never reached the bucket.
func (d *Doorbell) count(reason string) {
	d.mu.Lock()
	d.byReason[reason]++
	d.mu.Unlock()
}

// DoorbellReport is what the checkpoint says about the doorbell: counts, by reason, and nothing else.
// There is nothing else a notification left behind.
type DoorbellReport struct {
	// ByReason is every outcome seen, in the published vocabulary.
	ByReason map[string]int
	// Polls is how many early polls the doorbell earned. It is the number SC-012 bounds.
	Polls int
}

// Report returns the counts, as a copy so a caller cannot edit the accumulator through it.
func (d *Doorbell) Report() DoorbellReport {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[string]int, len(d.byReason))
	for reason, n := range d.byReason {
		out[reason] = n
	}
	return DoorbellReport{ByReason: out, Polls: d.polls}
}

// String renders the report for the checkpoint note, deterministically: sorted by reason, so two runs
// of the same shape read the same and a golden does not depend on Go's map iteration.
func (r DoorbellReport) String() string {
	reasons := make([]string, 0, len(r.ByReason))
	for reason := range r.ByReason {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)
	parts := make([]string, 0, len(reasons)+1)
	parts = append(parts, fmt.Sprintf("polls=%d", r.Polls))
	for _, reason := range reasons {
		parts = append(parts, fmt.Sprintf("%s=%d", reason, r.ByReason[reason]))
	}
	return strings.Join(parts, " ")
}
