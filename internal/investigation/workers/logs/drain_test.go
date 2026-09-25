// SPDX-License-Identifier: Apache-2.0

package logs_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/investigation/workers/logs"
)

// The Drain-style miner (tasks.md T048, research §5).

var origin = time.Date(2026, 9, 1, 14, 0, 0, 0, time.UTC)

func line(offset int, text, status string) logs.Line {
	return logs.Line{
		At:        origin.Add(time.Duration(offset) * time.Second),
		Text:      text,
		Status:    status,
		PodOrHost: "checkout-7f9",
		Version:   "rev7",
	}
}

// TestMiningCollapsesVariablesAndKeepsStructure: the whole job of the miner is that a thousand
// lines differing only in an id become one template with a count of a thousand.
func TestMiningCollapsesVariablesAndKeepsStructure(t *testing.T) {
	t.Parallel()

	var lines []logs.Line
	for i := range 500 {
		lines = append(lines, line(i, fmt.Sprintf("handled request id=%d for checkout in %dms", 100000+i, 3+i%7), "info"))
	}
	for i := range 20 {
		lines = append(lines, line(600+i, fmt.Sprintf("upstream call failed: deadline exceeded after %dms", 250+i), "error"))
	}

	templates := logs.Mine(lines)
	if len(templates) != 2 {
		t.Fatalf("mined %d templates from two shapes of line: %v", len(templates), templates)
	}
	if templates[0].Count != 500 {
		t.Errorf("the most frequent template has count %d, want 500", templates[0].Count)
	}
	if templates[1].Count != 20 {
		t.Errorf("the second template has count %d, want 20", templates[1].Count)
	}
	for _, template := range templates {
		if strings.Contains(template.Text, "100000") || strings.Contains(template.Text, "250") {
			t.Errorf("template %q still carries a variable; a template is what makes a line publishable", template.Text)
		}
		if !strings.Contains(template.Text, "<num>") && !strings.Contains(template.Text, "<dur>") {
			t.Errorf("template %q generalised nothing", template.Text)
		}
	}
	if templates[1].Status != "error" {
		t.Errorf("status = %q, want error", templates[1].Status)
	}
	if !templates[0].FirstSeen.Equal(origin) {
		t.Errorf("first seen = %s, want %s", templates[0].FirstSeen, origin)
	}
}

// TestMiningIsDeterministic: the recorded world fixes the input order, and given that order the
// templates, their counts and their ordering must not move between runs.
func TestMiningIsDeterministic(t *testing.T) {
	t.Parallel()

	var lines []logs.Line
	for i := range 200 {
		switch i % 3 {
		case 0:
			lines = append(lines, line(i, fmt.Sprintf("GET /orders/%d 200", i), "info"))
		case 1:
			lines = append(lines, line(i, fmt.Sprintf("cache miss for key user:%d", i), "warn"))
		default:
			lines = append(lines, line(i, fmt.Sprintf("connection to 10.0.%d.%d refused", i%256, (i*7)%256), "error"))
		}
	}

	want := logs.Mine(lines)
	for range 20 {
		got := logs.Mine(lines)
		if len(got) != len(want) {
			t.Fatalf("mined %d templates, then %d", len(want), len(got))
		}
		for i := range got {
			if got[i].Text != want[i].Text || got[i].Count != want[i].Count {
				t.Fatalf("template %d moved: %q ×%d then %q ×%d",
					i, want[i].Text, want[i].Count, got[i].Text, got[i].Count)
			}
		}
	}
}

// TestDiffMarksWhatIsNew: a template that was always there is background; one that appeared when
// the symptom did is a lead, and the digest must say which is which.
func TestDiffMarksWhatIsNew(t *testing.T) {
	t.Parallel()

	var baseline, window []logs.Line
	for i := range 100 {
		baseline = append(baseline, line(i, fmt.Sprintf("handled request id=%d", i), "info"))
		window = append(window, line(1000+i, fmt.Sprintf("handled request id=%d", 5000+i), "info"))
	}
	for i := range 30 {
		window = append(window, line(2000+i, fmt.Sprintf("upstream call failed: deadline exceeded after %dms", 250+i), "error"))
	}

	diffed := logs.Diff(logs.Mine(window), logs.Mine(baseline))
	if len(diffed) != 2 {
		t.Fatalf("diffed %d templates, want 2", len(diffed))
	}
	if !diffed[0].NewInWindow {
		t.Errorf("the new template is not first: %v", diffed)
	}
	if !strings.Contains(diffed[0].Text, "deadline exceeded") {
		t.Errorf("first template = %q, want the one that is new in the window", diffed[0].Text)
	}
	if diffed[0].BaselineCount != 0 {
		t.Errorf("a new template has baseline count %d, want 0", diffed[0].BaselineCount)
	}
	if diffed[1].NewInWindow {
		t.Errorf("the background template %q was reported as new", diffed[1].Text)
	}
	if diffed[1].BaselineCount != 100 {
		t.Errorf("background baseline count = %d, want 100", diffed[1].BaselineCount)
	}
}

// TestMiningIsBounded: a pathological stream of unique shapes produces a bounded digest rather
// than one template per line.
func TestMiningIsBounded(t *testing.T) {
	t.Parallel()

	var lines []logs.Line
	for i := range 2000 {
		// Every line has a different token count, which is the worst case for the tree.
		lines = append(lines, line(i, strings.Repeat("word ", i%400+1)+"end", "info"))
	}
	templates := logs.Mine(lines)

	var total int
	for _, template := range templates {
		total += template.Count
	}
	if total != len(lines) {
		t.Errorf("the templates account for %d lines, want %d; every line is counted somewhere", total, len(lines))
	}
	if len(templates) > 400 {
		t.Errorf("mined %d templates from 400 distinct shapes; the tree is not bounding anything", len(templates))
	}
}

// TestRawValuesNeverReachATemplate: masking happens before clustering, so a value cannot reach a
// template even transiently.
func TestRawValuesNeverReachATemplate(t *testing.T) {
	t.Parallel()

	templates := logs.Mine([]logs.Line{
		line(0, "login failed for alice@example.com from 10.1.2.3", "error"),
		line(1, "login failed for bob@example.com from 10.1.2.4", "error"),
	})
	if len(templates) != 1 {
		t.Fatalf("mined %d templates, want 1", len(templates))
	}
	for _, forbidden := range []string{"alice", "bob", "example.com", "10.1.2"} {
		if strings.Contains(templates[0].Text, forbidden) {
			t.Errorf("template %q carries %q", templates[0].Text, forbidden)
		}
	}
	if templates[0].Count != 2 {
		t.Errorf("count = %d, want 2", templates[0].Count)
	}
}

func TestMinerVersionIsPublished(t *testing.T) {
	t.Parallel()
	if !strings.HasPrefix(logs.MinerVersion, "drain/") {
		t.Errorf("miner version = %q, want a drain/<semver> spelling", logs.MinerVersion)
	}
}
