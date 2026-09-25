// SPDX-License-Identifier: Apache-2.0

package github_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/feeders/github"
	"github.com/Pierre-Theophile/aisre/pkg/feeder"
)

// Honouring a rate limit, and resuming rather than restarting (004 T037; FR-074).
//
//	"Each feeder MUST honour the platform's rate-limit responses, waiting at least as long as the
//	 response asks, and MUST resume from where it stopped rather than restarting the cycle."
//
// ---------------------------------------------------------------------------------------------
// Two things were missing, and the second one is the one that costs
//
// The WAIT had nothing to wait for. `retryAfter` read `Retry-After` and returned zero otherwise,
// while its own comment named `X-RateLimit-Reset` as the other form. `Retry-After` is what a
// SECONDARY limit sends — abuse detection, too many concurrent requests. The PRIMARY hourly limit,
// which is the one an operator actually meets, sends only the reset instant. So the common case
// produced a wait of zero, and "at least as long as the response asks" had no number in it. The
// header was being parsed the whole time, one layer down in `feeder.ReadingFromHeaders`; nothing
// carried it up.
//
// The RESUME had nowhere to resume to. `PartialListError` reported how many pages were read and not
// where reading stopped, and `list` discarded the cursor at both of its exits — so the only thing a
// caller could do was start the list again from page one, re-spending every call it had already
// spent, on a limit it had just exhausted. That is the failure FR-074's second clause names, and it
// is the expensive one: restarting costs quota at exactly the moment there is none.
//
// The assertion that matters below is therefore not "all the pages arrived" — a restart would
// satisfy that too. It is that page one was fetched ONCE.
//
// # Which layer stops which refusal, found by writing these tests
//
// The first draft of the test below refused a page with `X-RateLimit-Remaining: 0`, the shape of a
// PRIMARY limit, and the wait never happened. The budget got there first: the client feeds every
// response's headers to `Issuer.Observe`, so a reading of zero remaining makes the NEXT call yield at
// the metered door before the transport's retry can issue it.
//
// That is the right layering rather than a defect, and it is worth stating because it bounds what
// this file tests. A primary exhaustion is the BUDGET's stop — it yields with the platform's own
// retry instant, and the cycle decides what to do with a quota that will not return for an hour.
// The transport's wait is for the other case: a refusal with allowance still on the clock, which is
// what a SECONDARY limit is — GitHub's abuse detection does not zero the primary counter. Both are
// tested here, each at the layer that actually handles it.

// rateLimitedOnce serves `total` pages, refusing page `refuseAt` the first time it is asked for with a
// SECONDARY limit: a 403 that states a reset instant and leaves the primary allowance intact, which is
// what GitHub's abuse detection sends. `remaining` is what the refusal reports left on the primary
// counter — nonzero, or the budget yields before the transport's wait is reached.
func rateLimitedOnce(t *testing.T, total, refuseAt int, reset time.Time) func(http.ResponseWriter, *http.Request) {
	t.Helper()
	refused := false
	return func(w http.ResponseWriter, r *http.Request) {
		page := 1
		if raw := r.URL.Query().Get("page"); raw != "" {
			if n, err := strconv.Atoi(raw); err == nil {
				page = n
			}
		}
		if page == refuseAt && !refused {
			refused = true
			// A secondary limit: no Retry-After, a reset instant, and the primary allowance intact.
			w.Header().Set("X-RateLimit-Limit", "5000")
			w.Header().Set("X-RateLimit-Remaining", "4000")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
			w.Header().Set("X-RateLimit-Resource", "core")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "4000")
		w.Header().Set("X-RateLimit-Resource", "core")
		if page < total {
			w.Header().Set("Link",
				fmt.Sprintf(`<http://%s%s?page=%d>; rel="next"`, r.Host, r.URL.Path, page+1))
		}
		_, _ = fmt.Fprintf(w, `[{"id": %d, "tag_name": "v%d"}]`, page, page)
	}
}

// A refusal that states only a reset instant becomes a wait, and the read resumes on the page it was
// refused — never at page one.
func TestARateLimitIsWaitedOutAndTheReadResumes(t *testing.T) {
	t.Parallel()
	// cycleStart is the client's pinned clock, so the wait is an exact quantity rather than whatever
	// the machine's clock says.
	reset := cycleStart.Add(90 * time.Second)
	c, stand, _ := client(t, rateLimitedOnce(t, 3, 2, reset))

	releases, err := c.Releases(context.Background(), github.Repo{Owner: "acme", Name: "storefront"},
		github.ListWindow{MaxPages: 10})
	if err != nil {
		t.Fatalf("a rate limit that cleared should not end the read: %v", err)
	}
	if len(releases) != 3 {
		t.Fatalf("three pages yielded %d release(s); the read did not complete after the wait", len(releases))
	}

	waits := stand.waited()
	if len(waits) != 1 {
		t.Fatalf("the client waited %d time(s), want 1: %v", len(waits), waits)
	}
	if waits[0] != 90*time.Second {
		t.Errorf("the client waited %s, want the 90s the reset instant implies. `Retry-After` was "+
			"absent, which GitHub's refusals routinely are — and reading only that header is how "+
			"this connector came to wait zero and call it honouring the instruction", waits[0])
	}

	// The assertion this test exists for. A restart would have fetched page 1 twice and still
	// returned three releases, so counting releases proves nothing about FR-074's second clause.
	// The first request carries no `page` parameter (GitHub's cursor appears only from page 2), so
	// "page one" is any request that does not name a later page.
	var pageOne int
	for _, query := range stand.queries() {
		if !strings.Contains(query, "page=2") && !strings.Contains(query, "page=3") {
			pageOne++
		}
	}
	if pageOne != 1 {
		t.Errorf("page 1 was fetched %d times, want 1. The read restarted instead of resuming, which "+
			"re-spends every call already spent — at the one moment the quota is exhausted (FR-074)",
			pageOne)
	}
}

// A wait longer than the cap is refused, and the refusal says where to resume.
//
// GitHub's limits can reset an hour out, so a refused read can be told to wait fifty-nine minutes. Obeying would hold a cycle open for an hour, miss every poll in between, and report one
// enormous extent instead of a series of honest partial ones. So above the cap the read stops and
// declares the gap — with the cursor, so the next cycle continues rather than starting again.
func TestAWaitBeyondTheCapStopsAndSaysWhereToResume(t *testing.T) {
	t.Parallel()
	reset := cycleStart.Add(github.MaxRateLimitWait + time.Minute)
	c, stand, _ := client(t, rateLimitedOnce(t, 3, 2, reset))

	releases, err := c.Releases(context.Background(), github.Repo{Owner: "acme", Name: "storefront"},
		github.ListWindow{MaxPages: 10})
	var partial *github.PartialListError
	if !errors.As(err, &partial) {
		t.Fatalf("a read stopped by a long rate limit returned %v, want PartialListError", err)
	}
	if len(stand.waited()) != 0 {
		t.Errorf("the client waited %v although the instruction exceeds the %s cap; a cycle held open "+
			"for an hour misses every poll in between", stand.waited(), github.MaxRateLimitWait)
	}
	if partial.ResumeFrom == "" {
		t.Fatal("the refusal names no resume point, so the next cycle can only start the list again — " +
			"re-spending every call already spent on a limit that has not cleared (FR-074)")
	}
	if !strings.Contains(partial.ResumeFrom, "page=2") {
		t.Errorf("the resume point is %q, want the page that was refused", partial.ResumeFrom)
	}
	if len(releases) != 1 {
		t.Errorf("the page already read yielded %d release(s), want 1; the items are true and dropping "+
			"them turns a declared gap into a bigger undeclared one", len(releases))
	}
}

// `Retry-After` still wins where the platform sends it, because a secondary limit states a duration
// directly and it is the more specific instruction.
func TestASecondaryLimitsRetryAfterIsHonoured(t *testing.T) {
	t.Parallel()
	refused := false
	c, stand, _ := client(t, func(w http.ResponseWriter, r *http.Request) {
		if !refused {
			refused = true
			w.Header().Set("Retry-After", "30")
			// A reset far in the future, to prove the seconds form is preferred rather than the
			// larger of the two being taken.
			w.Header().Set("X-RateLimit-Limit", "5000")
			w.Header().Set("X-RateLimit-Remaining", "4000")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(cycleStart.Add(5*time.Minute).Unix(), 10))
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(`[{"id": 1, "tag_name": "v1"}]`))
	})

	if _, err := c.Releases(context.Background(), github.Repo{Owner: "acme", Name: "storefront"},
		github.ListWindow{MaxPages: 10}); err != nil {
		t.Fatalf("Releases after a secondary limit cleared: %v", err)
	}
	waits := stand.waited()
	if len(waits) != 1 || waits[0] != 30*time.Second {
		t.Errorf("the client waited %v, want one 30s wait: `Retry-After` states a duration directly "+
			"and is the more specific instruction of the two", waits)
	}
}

// A limit the platform refused to date is not guessed at.
//
// With no `Retry-After` and no reset, there is no duration to honour — so the read reports a partial
// window and lets the caller decide. Inventing a backoff would be this connector choosing how hard to
// press somebody else's quota, on no evidence.
func TestARateLimitWithNoStatedInstantIsNotGuessedAt(t *testing.T) {
	t.Parallel()
	c, stand, _ := client(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.WriteHeader(http.StatusForbidden)
	})
	_, err := c.Releases(context.Background(), github.Repo{Owner: "acme", Name: "storefront"},
		github.ListWindow{MaxPages: 10})
	var partial *github.PartialListError
	if !errors.As(err, &partial) {
		t.Fatalf("an undated rate limit returned %v, want PartialListError", err)
	}
	if len(stand.waited()) != 0 {
		t.Errorf("the client waited %v with no instruction to honour; a backoff this connector "+
			"invented is it deciding how hard to press somebody else's quota", stand.waited())
	}
}

// A PRIMARY exhaustion is the budget's stop, not the transport's wait — and it still says where to
// resume.
//
// This is the layering the first draft of this file got wrong, pinned so nobody "fixes" the transport
// to wait here too. The client feeds every response's headers to `Issuer.Observe`, so a reading of
// zero remaining makes the NEXT call yield at the metered door before any retry can issue it. That is
// correct: an hour-long primary limit is a fact about the cycle, not about one paginated read, and the
// yield carries the platform's own retry instant for the cycle to act on.
//
// What the transport must still do is hand back the cursor, so that whatever the cycle decides, it
// does not start the list again.
func TestAPrimaryExhaustionYieldsAtTheDoorAndStillSaysWhereToResume(t *testing.T) {
	t.Parallel()
	exhausted := false
	c, stand, _ := client(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Resource", "core")
		if exhausted {
			// Should never be reached: the door yields before this call is issued.
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		exhausted = true
		// The first page succeeds and reports the quota spent to the last call.
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(cycleStart.Add(time.Hour).Unix(), 10))
		w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?page=2>; rel="next"`, r.Host, r.URL.Path))
		_, _ = w.Write([]byte(`[{"id": 1, "tag_name": "v1"}]`))
	})

	releases, err := c.Releases(context.Background(), github.Repo{Owner: "acme", Name: "storefront"},
		github.ListWindow{MaxPages: 10})
	var partial *github.PartialListError
	if !errors.As(err, &partial) {
		t.Fatalf("an exhausted primary quota returned %v, want PartialListError", err)
	}
	if _, yielded := feeder.YieldedForQuota(err); !yielded {
		t.Errorf("the cause is %v, want a quota yield: an exhausted primary limit is the BUDGET's "+
			"stop, and the transport's rate-limit wait must not pre-empt it — an hour-long limit is a "+
			"fact about the cycle, not about one paginated read", err)
	}
	if len(stand.waited()) != 0 {
		t.Errorf("the transport waited %v on an exhausted primary quota; the door yielded, so there "+
			"was no refusal for it to honour", stand.waited())
	}
	if partial.ResumeFrom == "" {
		t.Error("the refusal names no resume point, so whatever the cycle decides it must start the " +
			"list again (FR-074)")
	}
	if len(releases) != 1 {
		t.Errorf("the page read before the quota ran out yielded %d release(s), want 1", len(releases))
	}
}
