// SPDX-License-Identifier: Apache-2.0

package gcp_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	gcpfeeder "github.com/Pierre-Theophile/aisre/internal/feeders/gcp"
)

// The doorbell (T122, FR-050, FR-006).
//
// The rules are about what a notification CANNOT do, so most of these tests are negative. Two of
// them assert a *signature* rather than a behaviour, and that is deliberate: a rule enforced by a
// type cannot be violated by a later edit without the edit being visible in the diff, and asserting
// the type is how that stays true after everyone who remembers the rule has moved on.

// The body is never parsed, trusted or stored — enforced by `Ring` having nowhere to receive it.
func TestRingCannotReceiveANotificationBody(t *testing.T) {
	method, ok := reflect.TypeOf(&gcpfeeder.Doorbell{}).MethodByName("Ring")
	if !ok {
		t.Fatal("Doorbell has no Ring method")
	}
	// receiver + one parameter.
	if got := method.Type.NumIn(); got != 2 {
		t.Fatalf("Ring takes %d arguments besides its receiver; it must take exactly one, a count", got-1)
	}
	if got := method.Type.In(1).Kind(); got != reflect.Int {
		t.Errorf("Ring's parameter is a %s; it must be an int. A doorbell that can receive a body "+
			"is a doorbell that can be talked into trusting one", got)
	}
}

// The one operation performed on the subscription yields a count and no content.
func TestDrainYieldsACountAndNoContent(t *testing.T) {
	method, ok := reflect.TypeOf((*gcpfeeder.DoorbellSource)(nil)).Elem().MethodByName("Drain")
	if !ok {
		t.Fatal("DoorbellSource has no Drain method")
	}
	if got := method.Type.NumOut(); got != 2 {
		t.Fatalf("Drain returns %d values, want (int, error)", got)
	}
	if got := method.Type.Out(0).Kind(); got != reflect.Int {
		t.Errorf("Drain's first return is a %s, want int; there must be no value in the process "+
			"for a notification body to be", got)
	}
}

type fakeDoorbellSource struct {
	counts []int
	err    error
	calls  int
}

func (f *fakeDoorbellSource) Drain(context.Context) (int, error) {
	f.calls++
	if f.err != nil {
		return 0, f.err
	}
	if len(f.counts) == 0 {
		return 0, nil
	}
	next := f.counts[0]
	f.counts = f.counts[1:]
	return next, nil
}

type fakeClock struct{ at time.Time }

func (c *fakeClock) now() time.Time { return c.at }

func newTestDoorbell(t *testing.T, clock *fakeClock) *gcpfeeder.Doorbell {
	t.Helper()
	d, err := gcpfeeder.NewDoorbell(gcpfeeder.DoorbellOptions{
		Subscription: "projects/nova-production/subscriptions/sre-agent-doorbell",
		Now:          clock.now,
	})
	if err != nil {
		t.Fatalf("NewDoorbell: %v", err)
	}
	return d
}

// A configured-but-unnamed doorbell is refused: the honest states are "off" and "bound to this
// subscription", and a third state is a deployment that thinks it has one.
func TestADoorbellWithNoSubscriptionIsRefused(t *testing.T) {
	_, err := gcpfeeder.NewDoorbell(gcpfeeder.DoorbellOptions{})
	if !errors.Is(err, gcpfeeder.ErrNoSubscription) {
		t.Fatalf("err = %v, want ErrNoSubscription", err)
	}
}

// A flood costs at most one extra poll per interval, however many messages arrive. Fifty
// notifications and one earn the same single poll, because the poll reads the whole window.
func TestAFloodCostsAtMostOnePollPerInterval(t *testing.T) {
	clock := &fakeClock{at: time.Date(2026, 9, 21, 2, 0, 0, 0, time.UTC)}
	d := newTestDoorbell(t, clock)

	honoured := 0
	for range 50 {
		if d.Ring(1).PollNow {
			honoured++
		}
	}
	if honoured != 1 {
		t.Errorf("50 rings in the same instant earned %d polls, want 1", honoured)
	}

	// One interval later, one more is earned and no more.
	clock.at = clock.at.Add(gcpfeeder.DefaultDoorbellMinInterval)
	if !d.Ring(1).PollNow {
		t.Error("a ring one interval later was refused")
	}
	if d.Ring(1).PollNow {
		t.Error("two polls were earned inside one interval")
	}

	report := d.Report()
	if report.Rings != 52 {
		t.Errorf("rings = %d, want every notification counted", report.Rings)
	}
	if report.Honoured != 2 {
		t.Errorf("honoured = %d, want 2", report.Honoured)
	}
	if report.RateLimited == 0 {
		t.Error("the report does not say anything was rate limited")
	}
	if !strings.Contains(report.String(), "subscription=projects/nova-production/subscriptions/sre-agent-doorbell") {
		t.Errorf("the report does not name the resource the grant is bound to: %q", report.String())
	}
}

// A single drain of fifty waiting notifications is still one poll: the count is used to count, never
// to decide how much to do.
func TestManyWaitingNotificationsEarnOnePoll(t *testing.T) {
	clock := &fakeClock{at: time.Date(2026, 9, 21, 2, 0, 0, 0, time.UTC)}
	d := newTestDoorbell(t, clock)
	src := &fakeDoorbellSource{counts: []int{50}}

	if outcome := d.Drain(context.Background(), src); !outcome.PollNow {
		t.Fatalf("a drain of 50 earned no poll: %+v", outcome)
	}
	if got := d.Report().Rings; got != 50 {
		t.Errorf("rings = %d, want all 50 counted", got)
	}
	if got := d.Report().Honoured; got != 1 {
		t.Errorf("honoured = %d, want exactly one poll", got)
	}
}

// An empty drain earns nothing and says so, so "the doorbell was quiet" is a statement rather than
// an absence.
func TestAnEmptyDrainEarnsNothing(t *testing.T) {
	clock := &fakeClock{at: time.Date(2026, 9, 21, 2, 0, 0, 0, time.UTC)}
	d := newTestDoorbell(t, clock)
	outcome := d.Drain(context.Background(), &fakeDoorbellSource{counts: []int{0}})
	if outcome.PollNow {
		t.Error("an empty drain earned a poll")
	}
	if outcome.Reason != gcpfeeder.DoorbellEmpty {
		t.Errorf("reason = %q, want %q", outcome.Reason, gcpfeeder.DoorbellEmpty)
	}
}

// An unreachable doorbell is a doorbell that is absent, and the integration runs correctly with it
// absent. The failure is recorded so that "quiet" and "unreachable" stay different statements, and
// it does not become an error the caller has to handle by not polling.
func TestAnUnreachableDoorbellIsRecordedAndNotFatal(t *testing.T) {
	clock := &fakeClock{at: time.Date(2026, 9, 21, 2, 0, 0, 0, time.UTC)}
	d := newTestDoorbell(t, clock)
	outcome := d.Drain(context.Background(), &fakeDoorbellSource{err: errors.New("permission denied")})

	if outcome.PollNow {
		t.Error("a failed drain earned a poll")
	}
	report := d.Report()
	if len(report.DrainErrors) != 1 {
		t.Fatalf("the failure was not recorded: %+v", report)
	}
	if !strings.Contains(report.String(), "drain_errors") {
		t.Errorf("the report does not distinguish unreachable from quiet: %q", report.String())
	}
	if report.Rings != 0 {
		t.Errorf("rings = %d; a failed drain saw no notifications", report.Rings)
	}
}

// A nil source is the doorbell being off. It is not an error: polling is the source of truth and
// the feature runs correctly without a doorbell at all.
func TestNoDoorbellSourceIsNotAFailure(t *testing.T) {
	clock := &fakeClock{at: time.Date(2026, 9, 21, 2, 0, 0, 0, time.UTC)}
	d := newTestDoorbell(t, clock)
	if outcome := d.Drain(context.Background(), nil); outcome.PollNow {
		t.Error("a nil source earned a poll")
	}
}
