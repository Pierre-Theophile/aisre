// SPDX-License-Identifier: Apache-2.0

package backend_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
	engine "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
	"github.com/Pierre-Theophile/aisre/pkg/feeder/versionstamp"
)

// errors_by_version on an unstamped pointer is the engine's to answer (005 T011–T012, FR-040b,
// ADR-0010 item 2).

func unstampedPointer() *graphv1.Pointer {
	return &graphv1.Pointer{
		Kind:       graphv1.PointerKind_LOG,
		Selector:   "service:voice-agent env:production",
		Vocabulary: "datadog-logs/v1",
	}
}

func unstampedTerm() *engine.Term {
	return engine.ErrorsByVersion(unstampedPointer(), engine.NewWindow(horizonAt.Add(-time.Hour), horizonAt), "")
}

// The answer is NO_DATA whose absent source names every published convention, in order, and whose
// coverage is valid — "we looked for a stamp and there is none", not "unknown" and not a failure.
func TestAnUnstampedPointerIsAnsweredNoDataNamingEveryConvention(t *testing.T) {
	t.Parallel()
	resp, ok, err := engine.AnswerUnstamped(requestAt(unstampedTerm(), time.Time{}), "live")
	if err != nil || !ok {
		t.Fatalf("AnswerUnstamped: ok=%v err=%v", ok, err)
	}
	if resp.GetOutcome() != investigationv1.TermOutcome_NO_DATA {
		t.Fatalf("outcome %v, want NO_DATA", resp.GetOutcome())
	}
	if err := engine.ValidateCoverage(resp); err != nil {
		t.Fatalf("coverage: %v", err)
	}
	stated := resp.GetDigest().GetCoverage().GetTruncation()
	last := -1
	for _, label := range versionstamp.ConventionLabels() {
		at := strings.Index(stated, label)
		if at < 0 {
			t.Errorf("the answer does not name the convention %q: %s", label, stated)
			continue
		}
		if at < last {
			t.Errorf("the conventions are not named in the published order: %s", stated)
		}
		last = at
	}
}

// A stamped pointer is not the engine's: it falls through to the backend.
func TestAStampedPointerIsNotAnsweredByTheEngine(t *testing.T) {
	t.Parallel()
	term := engine.ErrorsByVersion(pointer(), engine.NewWindow(horizonAt.Add(-time.Hour), horizonAt), "service.version")
	if _, ok, err := engine.AnswerUnstamped(requestAt(term, time.Time{}), "live"); ok || err != nil {
		t.Fatalf("a stamped errors_by_version was answered by the engine (ok=%v err=%v)", ok, err)
	}
}

// Recorded mode gives the same answer from the pointer alone: it is neither served from the world nor
// a miss, so a world's miss rate does not move, and the digest equals the live one.
func TestTheUnstampedAnswerIsIdenticalRecordedAndNeedsNoWorldEntry(t *testing.T) {
	t.Parallel()
	unrelated := engine.MonitorState(pointer(), engine.NewWindow(horizonAt.Add(-time.Hour), horizonAt))
	recorded := recordedWorld(t, unrelated, horizonAt)

	req := requestAt(unstampedTerm(), time.Time{})
	got, err := recorded.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got.GetOutcome() != investigationv1.TermOutcome_NO_DATA {
		t.Fatalf("recorded outcome %v, want NO_DATA", got.GetOutcome())
	}
	misses := recorded.Misses()
	if len(misses.Missed) != 0 || misses.Served != 0 || misses.Refused != 0 {
		t.Errorf("the engine's answer moved the world's accounting: %+v", misses)
	}
	live, _, err := engine.AnswerUnstamped(req, "live")
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(live.GetDigest(), got.GetDigest()) {
		t.Errorf("live and recorded digests differ:\n live     %v\n recorded %v", live.GetDigest(), got.GetDigest())
	}
}
