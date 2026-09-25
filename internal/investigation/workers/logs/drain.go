// SPDX-License-Identifier: Apache-2.0

package logs

import (
	"sort"
	"strings"
	"time"

	backend "github.com/Pierre-Theophile/aisre/internal/investigation/backend"
)

// A Drain-style fixed-depth parse tree, in-repo (tasks.md T048, research §5, plan §Design
// Decisions 6).
//
// Drain is the published standard for algorithmic log-template mining: bucket by the number of
// tokens, then by the first few tokens, then match within the bucket by token similarity, and
// generalise the tokens that differ. It is linear in the number of lines and — the property that
// matters here — deterministic given an input order, which a recorded world fixes.
//
// It is implemented here rather than taken from a library for two reasons, both from research §5.
// The masking rules and the variable placeholders are **redaction policy**: a template is what
// makes a log line publishable at all (ADR-0003 D9), so the rules have to be ours and have to be
// versioned with the fixtures. And a library version bump that changed template identity would
// silently invalidate every recorded world, with no failing test to say so.
//
// The mining runs **first**, always. An optional model pass may label the templates afterwards
// and the digest states what it added (FR-009a); the model never sees a raw line.

// MinerVersion is recorded in every log digest. It moves when template identity could move,
// which invalidates every recorded log answer — so it is a published schema change.
const MinerVersion = "drain/1.0.0"

// The published miner parameters.
const (
	// TreeDepth is how many leading tokens the parse tree branches on before it compares whole
	// lines. Four is Drain's own default and the one every published evaluation uses.
	TreeDepth = 4
	// SimilarityThreshold is the fraction of positions two lines must agree on to join one
	// cluster. Below it they are different templates.
	SimilarityThreshold = 0.5
	// MaxClustersPerBucket bounds one bucket, so a pathological log stream costs bounded time
	// and produces a bounded digest rather than one template per line.
	MaxClustersPerBucket = 64
	// Wildcard is the token a cluster generalises a varying position to.
	Wildcard = "<*>"
)

// Line is one log line as the backend read it. Lines never leave the backend process: what
// leaves is the template.
type Line struct {
	// At is the line's instant.
	At time.Time
	// Text is the raw line. It is masked before it is ever compared, so a raw value never
	// reaches a template even transiently.
	Text string
	// Status is the level or status the source attached, e.g. "error", "warn", "info". It
	// groups the counts the digest reports.
	Status string
	// PodOrHost is the instance the line came from, carried into the template's join keys.
	PodOrHost string
	// Version is the deployed version, carried into the template's join keys.
	Version string
}

// Template is one mined cluster.
type Template struct {
	// Text is the masked template, with varying positions generalised to Wildcard.
	Text string
	// Count is how many lines matched it.
	Count int
	// FirstSeen is the earliest instant a line matched it.
	FirstSeen time.Time
	// Status is the status the lines carried, when they agreed on one.
	Status string
	// PodOrHost and Version are the join keys of the first matching line, so a template joins
	// back to a workload and a deploy.
	PodOrHost string
	// Version is the deployed version the first matching line carried.
	Version string
	// BaselineCount is how many lines matched this template in the baseline window, filled in
	// by Diff. Zero with NewInWindow set is the lead an investigator is looking for.
	BaselineCount int
	// NewInWindow says the template does not appear in the baseline window at all.
	NewInWindow bool
}

// Miner is a fixed-depth parse tree. The zero Miner is ready to use.
type Miner struct {
	buckets map[bucketKey]*bucket
	order   []bucketKey
}

type bucketKey struct {
	tokens int
	prefix string
}

type bucket struct {
	clusters []*cluster
}

type cluster struct {
	tokens    []string
	count     int
	firstSeen time.Time
	status    string
	statusMix bool
	podOrHost string
	version   string
}

// NewMiner returns an empty miner.
func NewMiner() *Miner {
	return &Miner{buckets: make(map[bucketKey]*bucket)}
}

// Add feeds one line into the tree.
func (m *Miner) Add(line Line) {
	if m.buckets == nil {
		m.buckets = make(map[bucketKey]*bucket)
	}
	masked := backend.MaskLine(line.Text)
	if masked == "" {
		return
	}
	tokens := strings.Fields(masked)
	key := bucketKey{tokens: len(tokens), prefix: prefixOf(tokens)}
	b, ok := m.buckets[key]
	if !ok {
		b = &bucket{}
		m.buckets[key] = b
		m.order = append(m.order, key)
	}

	if c := b.match(tokens); c != nil {
		c.merge(tokens, line)
		return
	}
	if len(b.clusters) >= MaxClustersPerBucket {
		// The bucket is full. Rather than grow without bound, the line joins the bucket's
		// last cluster, generalised: a bounded, honest over-generalisation beats an unbounded
		// digest, and the count still says how many lines are behind it.
		last := b.clusters[len(b.clusters)-1]
		last.merge(tokens, line)
		return
	}
	b.clusters = append(b.clusters, newCluster(tokens, line))
}

// prefixOf is the first TreeDepth tokens, which is what the tree branches on. A token that is
// already a mask contributes the mask, so `GET /orders/<num>` and `GET /orders/<num>` share a
// branch whatever the number was.
func prefixOf(tokens []string) string {
	depth := min(TreeDepth, len(tokens))
	return strings.Join(tokens[:depth], " ")
}

func (b *bucket) match(tokens []string) *cluster {
	var best *cluster
	bestScore := SimilarityThreshold
	for _, c := range b.clusters {
		score := similarity(c.tokens, tokens)
		if score > bestScore {
			best, bestScore = c, score
		}
	}
	return best
}

// similarity is the fraction of positions two equal-length token sequences agree on, counting a
// wildcard as agreement. Drain's own measure.
func similarity(a, b []string) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var same int
	for i := range a {
		if a[i] == b[i] || a[i] == Wildcard {
			same++
		}
	}
	return float64(same) / float64(len(a))
}

func newCluster(tokens []string, line Line) *cluster {
	return &cluster{
		tokens:    append([]string(nil), tokens...),
		count:     1,
		firstSeen: line.At.UTC(),
		status:    line.Status,
		podOrHost: line.PodOrHost,
		version:   line.Version,
	}
}

func (c *cluster) merge(tokens []string, line Line) {
	for i := range c.tokens {
		if i < len(tokens) && c.tokens[i] != tokens[i] {
			c.tokens[i] = Wildcard
		}
	}
	c.count++
	if at := line.At.UTC(); !at.IsZero() && (c.firstSeen.IsZero() || at.Before(c.firstSeen)) {
		c.firstSeen = at
	}
	if line.Status != c.status {
		c.statusMix = true
	}
}

// Templates returns the mined templates, ordered by count descending and then by template text,
// so the digest's ordering is total and a golden is stable.
func (m *Miner) Templates() []Template {
	out := make([]Template, 0, len(m.buckets))
	for _, key := range m.order {
		for _, c := range m.buckets[key].clusters {
			t := Template{
				Text:      strings.Join(c.tokens, " "),
				Count:     c.count,
				FirstSeen: c.firstSeen,
				Status:    c.status,
				PodOrHost: c.podOrHost,
				Version:   c.version,
			}
			if c.statusMix {
				t.Status = "mixed"
			}
			out = append(out, t)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Text < out[j].Text
	})
	return out
}

// Mine is the one-shot form: feed every line, return the templates.
func Mine(lines []Line) []Template {
	m := NewMiner()
	for _, line := range lines {
		m.Add(line)
	}
	return m.Templates()
}

// Diff compares a window's templates against a baseline window's and marks which are new. "New"
// is the whole point of the term: a template that was always there is background, and a template
// that appeared when the symptom did is a lead.
func Diff(window, baseline []Template) []Template {
	baselineCounts := make(map[string]int, len(baseline))
	for _, t := range baseline {
		baselineCounts[t.Text] += t.Count
	}
	out := make([]Template, 0, len(window))
	for _, t := range window {
		t.BaselineCount = baselineCounts[t.Text]
		t.NewInWindow = t.BaselineCount == 0
		out = append(out, t)
	}
	// New templates first, then by count: the reader's eye should land on what changed.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].NewInWindow != out[j].NewInWindow {
			return out[i].NewInWindow
		}
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Text < out[j].Text
	})
	return out
}
