// SPDX-License-Identifier: Apache-2.0

package ledger

import (
	"slices"

	investigationv1 "github.com/Pierre-Theophile/aisre/api/sreagent/investigation/v1"
)

// Conflict and non-separability (T056, FR-024, FR-025).
//
// Both of these are refusals to tidy up.
//
// A *conflict* is a hypothesis with evidence on both sides. The temptation is to report the side
// that won; FR-024 forbids it, and the ledger makes it impossible by construction rather than by
// convention: both sides are judgments, both are in the arithmetic (the posterior is the sum of
// every ln LR, not of the ones that agree), and Conflicts returns both lists with their evidence
// ids so every rendering shows both. There is no code path that can show one side, because there
// is no field that holds one side.
//
// *Non-separability* is two candidates the evidence cannot tell apart. The temptation there is to
// break the tie on rank order and report a winner; FR-025 forbids that too. Two hypotheses whose
// confidences differ by no more than the published separation epsilon are reported together, said
// to be non-separable, and accompanied by what would separate them — the next query on each, which
// is the same machinery FR-031 already requires for an untested hypothesis. Where no separating
// query is recorded, the group says so, which is a finding rather than an omission: it means the
// investigation has reached the limit of what this deployment's feeders can distinguish.

// SeparationEpsilon is the published width of "comparable confidence" (FR-025).
//
// 0.05 is a twentieth of the probability mass, which is smaller than every published bucket and
// therefore never groups hypotheses a reader would see in different buckets by chance, while still
// catching the case the fixture `two-simultaneous-01` exists for: two changes in the same window,
// judged identically, that the evidence genuinely cannot separate.
const SeparationEpsilon = 0.05

// Conflict is one hypothesis with evidence pointing both ways (FR-024).
type Conflict struct {
	// HypothesisID and Statement name what is in conflict.
	HypothesisID string
	Statement    string
	// Supporting and Refuting are both sides, never one. Each is in recording order.
	Supporting []Judgment
	Refuting   []Judgment
	// Confidence and Bucket are what the two sides together produced: the conflict is reflected
	// in the number, not merely reported beside it (FR-024).
	Confidence float64
	Bucket     Bucket
	// NetLnLR is the sum of every ln LR on this hypothesis — positive when the supporting side
	// weighs more, negative when the refuting side does. It is the one number that says which way
	// the evidence leans without hiding that it leans both ways.
	NetLnLR float64
}

// Conflicts returns every hypothesis carrying both supporting and refuting judgments, in
// published rank order.
func (l *Ledger) Conflicts() []Conflict {
	out := make([]Conflict, 0, 2)
	for _, h := range l.Hypotheses() {
		var supporting, refuting []Judgment
		net := 0.0
		for _, j := range l.judgments {
			if j.HypothesisID != h.ID {
				continue
			}
			net += j.LnLR
			switch j.Direction {
			case Supports:
				supporting = append(supporting, j)
			case Refutes:
				refuting = append(refuting, j)
			case Neutral:
			}
		}
		if len(supporting) == 0 || len(refuting) == 0 {
			continue
		}
		out = append(out, Conflict{
			HypothesisID: h.ID,
			Statement:    h.Statement,
			Supporting:   supporting,
			Refuting:     refuting,
			Confidence:   h.Confidence,
			Bucket:       h.Bucket,
			NetLnLR:      round6(net),
		})
	}
	return out
}

// Separator is what would separate two candidates the evidence cannot (FR-025).
type Separator struct {
	// HypothesisID is the hypothesis the query would test.
	HypothesisID string
	// Known is false when no separating query has been recorded for this hypothesis. A group of
	// non-separable candidates with no known separator is the honest end of an investigation:
	// nothing this deployment can ask would tell them apart.
	Known bool
	// NextQuery is the exact algebra term, and DeepLink the link a person can follow instead.
	NextQuery *investigationv1.AlgebraTerm
	DeepLink  string
}

// NonSeparableGroup is a set of candidates at comparable confidence (FR-025).
type NonSeparableGroup struct {
	// Hypotheses are the members, in published rank order, with their confidences.
	Hypotheses []Hypothesis
	// Spread is the difference between the highest and lowest confidence in the group; it is at
	// most SeparationEpsilon by construction.
	Spread float64
	// Separators names what would separate them, one entry per member.
	Separators []Separator
}

// IDs is the members' ids, in rank order.
func (g NonSeparableGroup) IDs() []string {
	out := make([]string, 0, len(g.Hypotheses))
	for _, h := range g.Hypotheses {
		out = append(out, h.ID)
	}
	return out
}

// Separable reports whether anything recorded would tell these candidates apart.
func (g NonSeparableGroup) Separable() bool {
	for _, s := range g.Separators {
		if s.Known {
			return true
		}
	}
	return false
}

// NonSeparable returns the groups of candidates the evidence cannot separate (FR-025).
//
// Membership is "within SeparationEpsilon of the group's leader" rather than of the previous
// member, so a long chain of hypotheses each a hair from the last is not silently collapsed into
// one group spanning half the probability mass. Refuted and exonerated hypotheses are not
// candidates any more and take no part; a hypothesis with no mass at all takes none either.
func (l *Ledger) NonSeparable() []NonSeparableGroup {
	ranked := make([]Hypothesis, 0, len(l.hypotheses))
	for _, h := range l.Hypotheses() {
		if h.Status == StatusRefuted || h.Status == StatusExonerated || h.Confidence <= 0 {
			continue
		}
		ranked = append(ranked, h)
	}

	out := make([]NonSeparableGroup, 0, 1)
	for i := 0; i < len(ranked); {
		leader := ranked[i]
		j := i + 1
		for j < len(ranked) && round6(leader.Confidence-ranked[j].Confidence) <= SeparationEpsilon {
			j++
		}
		if j-i >= 2 {
			members := slices.Clone(ranked[i:j])
			group := NonSeparableGroup{
				Hypotheses: members,
				Spread:     round6(members[0].Confidence - members[len(members)-1].Confidence),
			}
			for _, m := range members {
				group.Separators = append(group.Separators, Separator{
					HypothesisID: m.ID,
					Known:        m.NextQuery != nil || m.NextQueryDeepLink != "",
					NextQuery:    m.NextQuery,
					DeepLink:     m.NextQueryDeepLink,
				})
			}
			out = append(out, group)
		}
		i = j
	}
	return out
}
