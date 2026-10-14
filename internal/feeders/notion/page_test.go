// SPDX-License-Identifier: Apache-2.0

package notion

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSummariseJoinsAndCaps(t *testing.T) {
	got, cut := summarise([]part{{"Summary", "Pool  exhausted\nafter rollout"}, {"Root cause", ""}, {"Resolution", "Rolled back"}})
	if got != "Pool exhausted after rollout — Rolled back" || cut {
		t.Errorf("summary %q, cut %v", got, cut)
	}
	long := strings.Repeat("word ", 300)
	got, cut = summarise([]part{{"Summary", long}})
	if !cut || utf8.RuneCountInString(got) > SummaryCap || !strings.HasSuffix(got, "word…") {
		t.Errorf("a long summary: %d characters, cut %v, ends %q", utf8.RuneCountInString(got), cut, got[len(got)-10:])
	}
}

func TestAPageWithNoURLGetsOneFromItsID(t *testing.T) {
	if got := pageURL(observedPage{ID: "a1000000-0000-4000-8000-000000000001"}); got != "https://www.notion.so/a1000000000040008000000000000001" {
		t.Errorf("url %q", got)
	}
}
