// SPDX-License-Identifier: Apache-2.0

package fixture

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	graphv1 "github.com/Pierre-Theophile/aisre/api/sreagent/graph/v1"
	"github.com/Pierre-Theophile/aisre/internal/graph"
	"github.com/Pierre-Theophile/aisre/internal/store/postgres"
)

// Measuring a fixture against its ground truth (SC-005, constitution V and VIII;
// contracts/fixture-format.md §Verification step 4).
//
// `fixture verify --report` answers a different question from `fixture verify`. Verification
// asks whether the graph still says what it said yesterday — a golden comparison, which a
// wrong-but-stable ranking passes with room to spare. The report asks whether what it says is
// *right*: on a fixture whose culprit is known, where does the published score put it?
//
// That is the number the project is judged on. SC-005 requires the true culprit in the top
// three on at least 90% of rollout-regression scenarios and first on at least 70%, and the only
// way for that to mean anything is for it to be computed from the same query an operator would
// run, against ground truth written down before the ranking existed.
//
// The metric is deliberately thin: a rank, a hit, and the size of the field it was chosen from.
// Precision and recall over a single-culprit fixture would be arithmetic dressed up as
// statistics; "the culprit was second out of four" is the whole truth about one scenario, and
// the aggregate over a family of fixtures is what SC-005 is about.

// Fixture families whose report has family-specific content.
const (
	// FamilyRolloutRegression is the family whose ground truth names a culprit change.
	FamilyRolloutRegression = "rollout-regression"
)

// Ground-truth keys read from the manifest (contracts/fixture-format.md §manifest.yaml).
const (
	// GroundTruthCulprit names the change that really caused the incident, as an entity id or
	// as one of the change node's identifiers, e.g. `k8s.change=shop/payments@rev7`.
	GroundTruthCulprit = "culprit_change"
	// GroundTruthExpectedTopK is the rank the culprit must reach for the scenario to count as
	// a hit.
	GroundTruthExpectedTopK = "expected_top_k"
	// GroundTruthCrossSourcePairs names the pairs SC-021's 95% figure is measured over: an entity
	// present in this connector's source AND in another, with an agreed environment, that must
	// merge automatically under a CERTAIN rule. Each entry is `{pair: [refA, refB], same: true}`,
	// optionally with `rule:` naming the rule expected to do it.
	GroundTruthCrossSourcePairs = "cross_source_pairs"
	// GroundTruthDistinctPairs names pairs that must stay apart. It is the other half of the same
	// claim: a rule generous enough to merge everything would satisfy the first list alone.
	GroundTruthDistinctPairs = "distinct_pairs"
)

// defaultExpectedTopK is the cutoff a fixture that names a culprit but no k is measured
// against — the top three of SC-005.
const defaultExpectedTopK = 3

// RankingMetrics measures one diff response against the fixture's ground truth (SC-005).
//
// It returns nil when the fixture is not of a family with a culprit, or names none: a metric
// nobody can interpret is worse than an absent one. The rank is 1-based; `found` false with
// rank 0 means the culprit was not in the ranked list at all, which is a failure of a different
// kind from ranking it last and is reported as such.
func RankingMetrics(m *Manifest, resp *graphv1.DiffResponse) map[string]any {
	culprit, _ := m.GroundTruth[GroundTruthCulprit].(string)
	if m.Family != FamilyRolloutRegression || culprit == "" {
		return nil
	}
	topK := defaultExpectedTopK
	if k, ok := asInt(m.GroundTruth[GroundTruthExpectedTopK]); ok && k > 0 {
		topK = k
	}

	rank := 0
	for i, item := range resp.GetChanges() {
		if changeMatches(item.GetChange(), culprit) {
			rank = i + 1
			break
		}
	}

	metrics := map[string]any{
		"culprit_change": culprit,
		"found":          rank > 0,
		"rank":           rank,
		"expected_top_k": topK,
		"top_k_hit":      rank > 0 && rank <= topK,
		"candidates":     len(resp.GetChanges()),
		"ranked":         rankedNames(resp.GetChanges(), topK),
	}
	if rank > 0 {
		item := resp.GetChanges()[rank-1]
		metrics["score"] = item.GetScore()
		metrics["hop_distance"] = item.GetHopDistance()
		metrics["time_distance_seconds"] = item.GetTimeDistanceSeconds()
	}
	return metrics
}

// changeMatches reports whether a ranked change is the one ground truth names.
//
// A fixture names its culprit the way a human would — by the identifier the source uses, e.g.
// `k8s.change=shop/payments@rev7` — while the response carries canonical ids, so the match is
// tried against the id, every identifier the change carries, and the bare value of each. It is
// never fuzzy: an identifier matches exactly or not at all (constitution VI).
func changeMatches(change *graphv1.NodeVersion, culprit string) bool {
	if change.GetEntityId() == culprit {
		return true
	}
	for _, alias := range change.GetAliases() {
		if alias.GetValue() == culprit ||
			alias.GetNamespace()+"="+alias.GetValue() == culprit {
			return true
		}
	}
	return false
}

// rankedNames lists the first k ranked changes by the name a person would recognize, so a
// report says who beat the culprit rather than only that something did.
func rankedNames(changes []*graphv1.RankedChange, k int) []string {
	out := make([]string, 0, min(k, len(changes)))
	for i, item := range changes {
		if i >= k {
			break
		}
		out = append(out, changeName(item.GetChange()))
	}
	return out
}

// changeName prefers the identifier a fixture would name the change by, then its summary, then
// its id.
func changeName(change *graphv1.NodeVersion) string {
	if aliases := change.GetAliases(); len(aliases) > 0 {
		return aliases[0].GetNamespace() + "=" + aliases[0].GetValue()
	}
	if summary := change.GetChange().GetSummary(); summary != "" {
		return summary
	}
	return change.GetEntityId()
}

// rankingReport runs the manifest's diff queries against the loaded graph and measures each one
// against ground truth.
//
// The queries are re-run rather than read back from `golden/`: the report is about what this
// build answers, and a build whose ranking regressed would otherwise be measured against the
// last build's recorded answer and pass.
func rankingReport(ctx context.Context, runner QueryRunner, m *Manifest) (map[string]any, error) {
	if m.Family != FamilyRolloutRegression || m.GroundTruth == nil {
		return nil, nil
	}
	out := map[string]any{}
	for _, q := range m.Queries {
		if q.Kind != "diff" {
			continue
		}
		got, err := runner.Run(ctx, q)
		if errors.Is(err, ErrUnsupportedQueryKind) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("fixture: %s: report query %s.%s: %w", m.ID, q.Kind, q.Name, err)
		}
		resp, ok := got.(*graphv1.DiffResponse)
		if !ok {
			return nil, fmt.Errorf("fixture: %s: query %s.%s answered %T, want a DiffResponse",
				m.ID, q.Kind, q.Name, got)
		}
		if metrics := RankingMetrics(m, resp); metrics != nil {
			out[q.Name] = metrics
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// asInt reads a YAML-decoded number as an int. A manifest may spell a count as an int or, if it
// was written with a decimal point, as a float.
func asInt(raw any) (int, bool) {
	switch value := raw.(type) {
	case int:
		return value, true
	case int64:
		return int(value), true
	case float64:
		return int(value), true
	default:
		return 0, false
	}
}

// rankingMarkdown renders the ranking metrics as their own section of a verification report:
// one row per diff query, saying where the culprit landed and out of how many.
func rankingMarkdown(ranking map[string]any) string {
	var out strings.Builder
	out.WriteString("\n### Ranking (SC-005)\n\n")
	out.WriteString("| query | culprit | rank | top-k hit | candidates |\n|---|---|---|---|---|\n")

	names := make([]string, 0, len(ranking))
	for name := range ranking {
		names = append(names, name)
	}
	slices.Sort(names)

	for _, name := range names {
		metrics, ok := ranking[name].(map[string]any)
		if !ok {
			continue
		}
		rank := "not ranked"
		if value, ok := asInt(metrics["rank"]); ok && value > 0 {
			rank = fmt.Sprintf("%d of %v", value, metrics["candidates"])
		}
		hit := "no"
		if value, ok := metrics["top_k_hit"].(bool); ok && value {
			hit = fmt.Sprintf("yes (k=%v)", metrics["expected_top_k"])
		}
		fmt.Fprintf(&out, "| %s | %v | %s | %s | %v |\n",
			name, metrics[GroundTruthCulprit], rank, hit, metrics["candidates"])
	}
	return out.String()
}

// ---------- calibration (SC-007, SC-011, constitution V) ----------

// Measuring entity resolution against ground truth.
//
// Constitution V says a stated confidence of 0.8 must be right roughly 80% of the time on the
// evaluation set, and that the measurement must be reported in CI. A resolution fixture is where
// that becomes checkable: the manifest states, independently of what the rules decided, which
// pairs really are one entity, and this is the comparison.
//
// Three numbers, answering three different questions:
//
//   - *certain-rule precision* — of the merges the graph made with no human in the loop, how many
//     were right? This is the number that must stay at 1.0. A certain rule is allowed to miss a
//     merge; it is not allowed to make a wrong one, because a wrong merge silently poisons every
//     diff and blast radius downstream (constitution VI).
//   - *probable-score calibration* — a Brier score and a reliability table over the suggestions.
//     This one is not expected to be perfect and is not a gate: it is the evidence for whether
//     0.70 means 0.70, and the reason P1's score is a published constant that moves in a pull
//     request rather than a tuned parameter.
//   - *human decisions surviving replay* — SC-007's second half, checked after the replay the
//     verifier has just done: every decision a person made is still recorded, still attributable,
//     and still in effect.

// FamilyAmbiguousIdentity is the family whose ground truth labels resolution pairs.
const FamilyAmbiguousIdentity = "ambiguous-identity"

// Ground-truth keys read from an ambiguous-identity manifest.
const (
	// GroundTruthCertainPairs lists the pairs a certain rule is expected to judge, each with the
	// truth about whether they are one entity.
	GroundTruthCertainPairs = "certain_pairs"
	// GroundTruthProbablePairs lists the pairs a probable rule scored, with the same truth.
	GroundTruthProbablePairs = "probable_pairs"
)

// truthPair is one labelled pair from the manifest: two references and whether they really are
// one entity.
type truthPair struct {
	refs [2]string
	same bool
	// aID and bID are the entities the references resolve to *now*, after every merge and
	// split; they are what a precision measurement compares.
	aID   string
	bID   string
	found bool
	// mintedA and mintedB are the ids the two identifiers hash to, which never move. A
	// suggestion stores the ids the pair had when it was filed, so this is the key that finds
	// it again after the pair has been merged, split, or both (research §4).
	mintedA string
	mintedB string
}

// calibrationReport measures the loaded graph against the manifest's ground truth.
//
// It returns nil for a fixture of another family or one that labels no pairs: a calibration
// number computed from nothing would read like a measurement and be an assumption.
func calibrationReport(ctx context.Context, store *postgres.Store, m *Manifest) (map[string]any, error) {
	if m.Family != FamilyAmbiguousIdentity || m.GroundTruth == nil {
		return nil, nil
	}
	certain, err := loadTruthPairs(ctx, store, m.GroundTruth[GroundTruthCertainPairs])
	if err != nil {
		return nil, err
	}
	probable, err := loadTruthPairs(ctx, store, m.GroundTruth[GroundTruthProbablePairs])
	if err != nil {
		return nil, err
	}
	if len(certain) == 0 && len(probable) == 0 {
		return nil, nil
	}

	certainMetrics, err := certainPrecision(ctx, store, certain)
	if err != nil {
		return nil, err
	}
	probableMetrics, err := probableCalibration(ctx, store, probable)
	if err != nil {
		return nil, err
	}
	autoMerged, err := countRows(ctx, store, `
		SELECT count(*) FROM graph.resolution_decisions
		WHERE kind = 'auto_merge' AND rule_id LIKE 'P%'`)
	if err != nil {
		return nil, err
	}
	human, survived, err := humanDecisionMetrics(ctx, store)
	if err != nil {
		return nil, err
	}

	return map[string]any{
		"certain":                        certainMetrics,
		"probable":                       probableMetrics,
		"probable_auto_merges":           autoMerged,
		"human_decisions":                human,
		"human_decisions_survive_replay": survived,
	}, nil
}

// loadTruthPairs reads one ground-truth list and resolves each reference to the entity it names
// in the loaded graph.
func loadTruthPairs(ctx context.Context, store *postgres.Store, raw any) ([]truthPair, error) {
	items, ok := raw.([]any)
	if !ok {
		return nil, nil
	}
	out := make([]truthPair, 0, len(items))
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		refs, ok := entry["pair"].([]any)
		if !ok || len(refs) != 2 {
			return nil, fmt.Errorf("fixture: ground_truth pair %v: want exactly two references", entry["pair"])
		}
		pair := truthPair{}
		for i, ref := range refs {
			pair.refs[i], _ = ref.(string)
		}
		pair.same, _ = entry["same"].(bool)

		aID, aFound, err := canonicalEntityOf(ctx, store, pair.refs[0])
		if err != nil {
			return nil, err
		}
		bID, bFound, err := canonicalEntityOf(ctx, store, pair.refs[1])
		if err != nil {
			return nil, err
		}
		pair.aID, pair.bID, pair.found = aID, bID, aFound && bFound
		if refA, err := graph.ParseRef(pair.refs[0]); err == nil {
			pair.mintedA = refA.EntityID()
		}
		if refB, err := graph.ParseRef(pair.refs[1]); err == nil {
			pair.mintedB = refB.EntityID()
		}
		out = append(out, pair)
	}
	return out, nil
}

// canonicalEntityOf resolves `<namespace>=<value>` to the entity it currently denotes, following
// merge redirects, and reports whether the graph knows it at all.
func canonicalEntityOf(ctx context.Context, store *postgres.Store, raw string) (string, bool, error) {
	ref, err := graph.ParseRef(raw)
	if err != nil {
		return "", false, fmt.Errorf("fixture: ground_truth reference %q: %w", raw, err)
	}
	var entityID string
	err = store.Pool().QueryRow(ctx, `
		WITH seed AS (
			SELECT entity_id FROM graph.entities WHERE entity_id = $1
			UNION ALL
			SELECT entity_id FROM graph.identity_claims WHERE namespace = $2 AND value = $3
		)
		SELECT entity_id FROM seed ORDER BY entity_id LIMIT 1`,
		ref.EntityID(), ref.Namespace, ref.Value).Scan(&entityID)
	if err != nil {
		// A reference the graph never saw is a ground-truth statement about something absent,
		// which is a fixture problem, not a measurement; it is reported as "not found" and
		// counted as unresolved rather than silently scored.
		return "", false, nil
	}
	resolved, err := followMergedInto(ctx, store, entityID)
	if err != nil {
		return "", false, err
	}
	return resolved, true, nil
}

// followMergedInto walks the merge redirects to the surviving entity.
func followMergedInto(ctx context.Context, store *postgres.Store, entityID string) (string, error) {
	current := entityID
	for range 64 {
		var next *string
		if err := store.Pool().QueryRow(ctx,
			`SELECT merged_into FROM graph.entities WHERE entity_id = $1`, current).Scan(&next); err != nil {
			return current, nil
		}
		if next == nil {
			return current, nil
		}
		current = *next
	}
	return "", fmt.Errorf("fixture: merge chain from %s does not terminate", entityID)
}

// certainPrecision measures the merges the graph made without a human (SC-007's first half).
func certainPrecision(ctx context.Context, store *postgres.Store, pairs []truthPair) (map[string]any, error) {
	truePositives, falsePositives, missed, unresolved := 0, 0, 0, 0
	wrong := []string{}
	for _, pair := range pairs {
		if !pair.found {
			unresolved++
			continue
		}
		merged, err := autoMergedPair(ctx, store, pair.aID, pair.bID)
		if err != nil {
			return nil, err
		}
		switch {
		case merged && pair.same:
			truePositives++
		case merged && !pair.same:
			falsePositives++
			wrong = append(wrong, pair.refs[0]+" ↔ "+pair.refs[1])
		case !merged && pair.same:
			missed++
		}
	}
	metrics := map[string]any{
		"labelled_pairs":  len(pairs),
		"unresolved":      unresolved,
		"true_positives":  truePositives,
		"false_positives": falsePositives,
		"missed":          missed,
		"wrong_merges":    wrong,
	}
	if decided := truePositives + falsePositives; decided > 0 {
		metrics["precision"] = float64(truePositives) / float64(decided)
	} else {
		metrics["precision"] = nil
	}
	return metrics, nil
}

// autoMergedPair reports whether a certain rule merged this pair with no human involved: the two
// references resolve to one entity and the decision that made them one names a rule.
func autoMergedPair(ctx context.Context, store *postgres.Store, aID, bID string) (bool, error) {
	if aID != bID {
		return false, nil
	}
	var merged bool
	if err := store.Pool().QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM graph.resolution_decisions
			WHERE kind = 'auto_merge' AND superseded_by IS NULL
			  AND (surviving_id = $1 OR merged_id = $1))`, aID).Scan(&merged); err != nil {
		return false, fmt.Errorf("fixture: read auto_merge decisions for %s: %w", aID, err)
	}
	return merged, nil
}

// probableCalibration scores the suggestions against ground truth.
//
// The Brier score is the mean squared error between the score the rule stated and what turned out
// to be true, so 0 is perfect and 0.25 is what a constant 0.5 would get. The reliability table is
// the same data grouped into fifths, which is the form that shows *where* a score is wrong:
// "0.70 was right half the time on two samples" says more than one aggregate number, and on a
// hand-authored fixture the sample size is the first thing a reader needs to see.
func probableCalibration(ctx context.Context, store *postgres.Store, pairs []truthPair) (map[string]any, error) {
	type scored struct {
		score float64
		truth float64
	}
	var points []scored
	unscored, unresolved := 0, 0
	details := make([]map[string]any, 0, len(pairs))

	for _, pair := range pairs {
		if !pair.found {
			unresolved++
			continue
		}
		score, rule, status, found, err := suggestionFor(ctx, store, pair)
		if err != nil {
			return nil, err
		}
		if !found {
			unscored++
			continue
		}
		truth := 0.0
		if pair.same {
			truth = 1.0
		}
		points = append(points, scored{score: score, truth: truth})
		details = append(details, map[string]any{
			"pair":   pair.refs[0] + " ↔ " + pair.refs[1],
			"rule":   rule,
			"score":  score,
			"status": status,
			"same":   pair.same,
		})
	}

	metrics := map[string]any{
		"labelled_pairs": len(pairs),
		"scored":         len(points),
		"unscored":       unscored,
		"unresolved":     unresolved,
		"pairs":          details,
	}
	if len(points) == 0 {
		metrics["brier_score"] = nil
		metrics["reliability"] = []any{}
		return metrics, nil
	}

	sum := 0.0
	buckets := map[int]*struct {
		predicted float64
		observed  float64
		n         int
	}{}
	for _, point := range points {
		delta := point.score - point.truth
		sum += delta * delta
		key := min(int(point.score*5), 4)
		bucket, ok := buckets[key]
		if !ok {
			bucket = &struct {
				predicted float64
				observed  float64
				n         int
			}{}
			buckets[key] = bucket
		}
		bucket.predicted += point.score
		bucket.observed += point.truth
		bucket.n++
	}
	metrics["brier_score"] = sum / float64(len(points))

	reliability := make([]map[string]any, 0, len(buckets))
	for _, key := range slices.Sorted(maps.Keys(buckets)) {
		bucket := buckets[key]
		reliability = append(reliability, map[string]any{
			"bucket":    fmt.Sprintf("%.1f-%.1f", float64(key)/5, float64(key+1)/5),
			"predicted": bucket.predicted / float64(bucket.n),
			"observed":  bucket.observed / float64(bucket.n),
			"n":         bucket.n,
		})
	}
	metrics["reliability"] = reliability
	return metrics, nil
}

// suggestionFor reads the suggestion filed for a pair, whatever its status.
//
// The lookup is by pair key, and it tries the ids the identifiers hash to before the ids they
// resolve to today. A suggestion records the pair as it stood when the rule fired; a merge or a
// split moves one side afterwards, so matching on today's ids alone would miss the row — and
// matching loosely on "any suggestion naming either side" would find the wrong one, because a
// conflict filed about a third entity names one of them too.
func suggestionFor(ctx context.Context, store *postgres.Store, pair truthPair) (score float64, rule, status string, found bool, err error) {
	keys := []string{}
	for _, key := range []string{
		graph.PairKey(pair.mintedA, pair.mintedB),
		graph.PairKey(pair.aID, pair.bID),
	} {
		if key != "|" && !slices.Contains(keys, key) {
			keys = append(keys, key)
		}
	}
	for _, key := range keys {
		err = store.Pool().QueryRow(ctx, `
			SELECT score, rule_id, status FROM graph.suggestions WHERE pair_key = $1`, key).
			Scan(&score, &rule, &status)
		if err == nil {
			return score, rule, status, true, nil
		}
	}
	return 0, "", "", false, nil
}

// humanDecisionMetrics counts what the people did and whether it is still true after the replay
// the verifier has just performed (SC-007, SC-011).
//
// "Still in effect" is checked structurally, not by trusting the row: for every confirm and
// manual_merge still in force, the two entities it names must resolve to one; for every
// rejection, they must not.
func humanDecisionMetrics(ctx context.Context, store *postgres.Store) (map[string]any, bool, error) {
	rows, err := store.Pool().Query(ctx, `
		SELECT kind, coalesce(surviving_id, ''), coalesce(merged_id, ''), coalesce(principal, ''),
		       superseded_by IS NOT NULL
		FROM graph.resolution_decisions
		WHERE kind IN ('confirm', 'manual_merge', 'reject', 'split')
		ORDER BY decided_at, decision_id`)
	if err != nil {
		return nil, false, fmt.Errorf("fixture: read human decisions: %w", err)
	}
	defer rows.Close()

	type decision struct {
		kind, a, b, principal string
		superseded            bool
	}
	var decisions []decision
	for rows.Next() {
		var d decision
		if err := rows.Scan(&d.kind, &d.a, &d.b, &d.principal, &d.superseded); err != nil {
			return nil, false, fmt.Errorf("fixture: scan human decision: %w", err)
		}
		decisions = append(decisions, d)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("fixture: read human decisions: %w", err)
	}

	total, withPrincipal, inForce, inEffect, superseded := 0, 0, 0, 0, 0
	problems := []string{}
	for _, d := range decisions {
		total++
		if d.principal != "" {
			withPrincipal++
		} else {
			problems = append(problems, d.kind+" decision carries no principal (FR-041)")
		}
		if d.superseded {
			// A decision a later decision overtook is history, not a claim about the graph
			// today; the one that superseded it is checked instead.
			superseded++
			continue
		}
		inForce++
		a, err := followMergedInto(ctx, store, d.a)
		if err != nil {
			return nil, false, err
		}
		b, err := followMergedInto(ctx, store, d.b)
		if err != nil {
			return nil, false, err
		}
		switch d.kind {
		case "confirm", "manual_merge":
			if a == b {
				inEffect++
			} else {
				problems = append(problems, d.kind+" decision no longer holds: the pair is two entities")
			}
		case "reject", "split":
			if a != b {
				inEffect++
			} else {
				problems = append(problems, d.kind+" decision no longer holds: the pair is one entity")
			}
		}
	}

	survived := total > 0 && total == withPrincipal && inForce == inEffect
	return map[string]any{
		"total":          total,
		"with_principal": withPrincipal,
		"in_force":       inForce,
		"in_effect":      inEffect,
		"superseded":     superseded,
		"problems":       problems,
	}, survived, nil
}

// calibrationMarkdown renders the calibration metrics as their own section of a verification
// report, which is what a reviewer reads in a pull request comment.
func calibrationMarkdown(calibration map[string]any) string {
	var out strings.Builder
	out.WriteString("\n### Resolution calibration (SC-007, constitution V)\n\n")

	if certain, ok := calibration["certain"].(map[string]any); ok {
		fmt.Fprintf(&out, "- certain-rule precision: %s (%v right, %v wrong, %v missed of %v labelled pairs)\n",
			formatPrecision(certain["precision"]), certain["true_positives"], certain["false_positives"],
			certain["missed"], certain["labelled_pairs"])
		if wrong, ok := certain["wrong_merges"].([]string); ok && len(wrong) > 0 {
			fmt.Fprintf(&out, "  - WRONG MERGES: %s\n", strings.Join(wrong, "; "))
		}
	}
	fmt.Fprintf(&out, "- automated merges from probable rules: %v (SC-007 requires 0)\n",
		calibration["probable_auto_merges"])
	fmt.Fprintf(&out, "- human decisions survive replay: %v\n", calibration["human_decisions_survive_replay"])
	if human, ok := calibration["human_decisions"].(map[string]any); ok {
		fmt.Fprintf(&out, "  - %v decisions, %v with an authenticated principal, %v still in force, "+
			"%v of those still in effect, %v superseded\n",
			human["total"], human["with_principal"], human["in_force"], human["in_effect"], human["superseded"])
		if problems, ok := human["problems"].([]string); ok && len(problems) > 0 {
			fmt.Fprintf(&out, "  - PROBLEMS: %s\n", strings.Join(problems, "; "))
		}
	}

	probable, ok := calibration["probable"].(map[string]any)
	if !ok {
		return out.String()
	}
	fmt.Fprintf(&out, "- probable-score Brier: %s over %v scored pair(s)\n",
		formatPrecision(probable["brier_score"]), probable["scored"])
	reliability, ok := probable["reliability"].([]map[string]any)
	if !ok || len(reliability) == 0 {
		return out.String()
	}
	out.WriteString("\n| score bucket | mean predicted | observed | n |\n|---|---|---|---|\n")
	for _, row := range reliability {
		fmt.Fprintf(&out, "| %v | %.2f | %.2f | %v |\n",
			row["bucket"], row["predicted"], row["observed"], row["n"])
	}
	return out.String()
}

// formatPrecision renders a rate that may legitimately be absent: a fixture whose rules decided
// nothing has no precision, and printing 0.00 would read as "everything was wrong".
func formatPrecision(value any) string {
	rate, ok := value.(float64)
	if !ok {
		return "n/a"
	}
	return fmt.Sprintf("%.2f", rate)
}

// SC-021's two cross-source figures (T175).
//
// The criterion: *"Zero automated merges result from probable rules; 100% of automated merges
// involving a GCP or vendor claim are explainable by the audit query; and for entities present in
// both GCP and another source with an agreed environment, at least 95% merge automatically under a
// certain rule."*
//
// The first clause is held over the whole corpus from `decisions_by_rule`, in
// `scripts/check-report.sh`. The other two are about pairs, and a pair has to be labelled: whether
// two identifiers *ought* to be one entity is exactly the thing no code can decide, which is why the
// manifest names them.
//
// It returns nil for a fixture that labels none — and that nil is the point of the gate that reads
// this metric. Every fixture in the corpus was single-source when this was written, so both figures
// were satisfied by having nothing to measure: 100% of no merges are explainable, and 95% of no
// pairs merge. A metric absent everywhere reads exactly like a metric that passed.
func crossSourceReport(ctx context.Context, store *postgres.Store, m *Manifest) (map[string]any, error) {
	if m.GroundTruth == nil {
		return nil, nil
	}
	pairs, err := loadTruthPairs(ctx, store, m.GroundTruth[GroundTruthCrossSourcePairs])
	if err != nil {
		return nil, err
	}
	distinct, err := loadTruthPairs(ctx, store, m.GroundTruth[GroundTruthDistinctPairs])
	if err != nil {
		return nil, err
	}
	if len(pairs) == 0 && len(distinct) == 0 {
		return nil, nil
	}

	var merged, explainable, twoSources, agreedEnvironment, unresolved int
	var unmerged, unexplained []string
	rules := map[string]int{}
	for _, pair := range pairs {
		if !pair.found {
			unresolved++
			continue
		}
		label := pair.refs[0] + " ↔ " + pair.refs[1]

		sources, env, err := pairSourcesAndEnvironment(ctx, store, pair)
		if err != nil {
			return nil, err
		}
		if sources >= 2 {
			twoSources++
		}
		if env {
			agreedEnvironment++
		}

		rule, ok, err := certainAutoMergeRule(ctx, store, pair.aID, pair.bID)
		if err != nil {
			return nil, err
		}
		if !ok {
			unmerged = append(unmerged, label)
			continue
		}
		merged++
		rules[rule]++

		// "Explainable by the audit query" is a property of what the decision recorded, because
		// that is what the query renders: the rule that fired, the reasoning, and the claims it
		// stood on. A decision missing any of the three is a merge an operator cannot check, and an
		// unexplainable merge gets undone by the next skeptical operator.
		explained, err := decisionIsExplainable(ctx, store, pair.aID)
		if err != nil {
			return nil, err
		}
		if explained {
			explainable++
			continue
		}
		unexplained = append(unexplained, label)
	}

	var held int
	var wronglyMerged []string
	for _, pair := range distinct {
		if !pair.found {
			continue
		}
		if _, ok, err := certainAutoMergeRule(ctx, store, pair.aID, pair.bID); err != nil {
			return nil, err
		} else if ok {
			wronglyMerged = append(wronglyMerged, pair.refs[0]+" ↔ "+pair.refs[1])
			continue
		}
		held++
	}

	metrics := map[string]any{
		"labelled_pairs":      len(pairs),
		"unresolved":          unresolved,
		"two_sources":         twoSources,
		"agreed_environment":  agreedEnvironment,
		"auto_merged_certain": merged,
		"explainable":         explainable,
		"by_rule":             rules,
		"not_merged":          unmerged,
		"not_explainable":     unexplained,
		"distinct_pairs":      len(distinct),
		"distinct_held":       held,
		"wrongly_merged":      wronglyMerged,
	}
	if decidable := len(pairs) - unresolved; decidable > 0 {
		metrics["auto_merge_rate"] = float64(merged) / float64(decidable)
	} else {
		// No labelled pair resolved to anything, so there is no rate. Reporting 1.0 here would be
		// the vacuous pass this whole metric exists to make visible.
		metrics["auto_merge_rate"] = nil
	}
	return metrics, nil
}

// pairSourcesAndEnvironment reports how many distinct sources claim this pair's identifiers and
// whether they state one agreed environment.
//
// Both are guards on the LABEL rather than on the graph. SC-021's figure is about entities "present
// in both GCP and another source with an agreed environment": a labelled pair whose identifiers come
// from one source, or which states no environment, is outside the criterion — and counting it would
// let a fixture satisfy a cross-source figure with a single-source pair.
func pairSourcesAndEnvironment(ctx context.Context, store *postgres.Store, pair truthPair) (int, bool, error) {
	sources := map[string]bool{}
	environments := map[string]bool{}
	var stated int
	for _, ref := range pair.refs {
		parsed, err := graph.ParseRef(ref)
		if err != nil {
			return 0, false, fmt.Errorf("fixture: cross-source reference %q: %w", ref, err)
		}
		// Both kinds again, and for the same reason (004 T148). An identifier NAMES the entity, so the
		// sources come from the claims; the environment can be stated on either — C4's pairs carry it
		// on the claim and C8's on the correlation key, because what states the environment there is
		// the deploy observation rather than the name. Reading only claims reported every C8 pair as
		// stating no environment, which put a correctly-labelled pair outside the criterion.
		rows, err := store.Pool().Query(ctx, `
			SELECT source_id, coalesce(attributes->>'deployment.environment.name', '')
			FROM graph.identity_claims WHERE namespace = $1 AND value = $2
			UNION ALL
			SELECT source_id, coalesce(attributes->>'deployment.environment.name', '')
			FROM graph.correlation_keys
			WHERE entity_id = (
				SELECT entity_id FROM graph.identity_claims
				WHERE namespace = $1 AND value = $2 LIMIT 1)`,
			parsed.Namespace, parsed.Value)
		if err != nil {
			return 0, false, fmt.Errorf("fixture: read evidence for %s: %w", ref, err)
		}
		var sideStated bool
		var sideEnvironments []string
		for rows.Next() {
			var source, environment string
			if err := rows.Scan(&source, &environment); err != nil {
				rows.Close()
				return 0, false, fmt.Errorf("fixture: read evidence for %s: %w", ref, err)
			}
			sources[source] = true
			if environment != "" {
				sideStated = true
				sideEnvironments = append(sideEnvironments, environment)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return 0, false, fmt.Errorf("fixture: read evidence for %s: %w", ref, err)
		}
		if sideStated {
			stated++
			for _, environment := range sideEnvironments {
				environments[environment] = true
			}
		}
	}
	// Agreed means BOTH sides said it and they said the same thing. One side silent is not
	// agreement — it is the missing-environment case C5 refuses on for the same reason.
	return len(sources), stated == len(pair.refs) && len(environments) == 1, nil
}

// certainAutoMergeRule returns the certain rule that merged this pair, if one did.
//
// A probable rule is not an answer here even if it somehow produced an auto_merge: SC-021's first
// clause forbids that outright, and a figure that counted it would report a violation as a success.
func certainAutoMergeRule(ctx context.Context, store *postgres.Store, aID, bID string) (string, bool, error) {
	if aID != bID || aID == "" {
		return "", false, nil
	}
	var rule string
	err := store.Pool().QueryRow(ctx, `
		SELECT coalesce(rule_id, '') FROM graph.resolution_decisions
		WHERE kind = 'auto_merge' AND superseded_by IS NULL
		  AND (surviving_id = $1 OR merged_id = $1)
		ORDER BY decided_at, decision_id LIMIT 1`, aID).Scan(&rule)
	if err != nil {
		return "", false, nil
	}
	if rule == "" || !strings.HasPrefix(rule, "C") {
		return rule, false, nil
	}
	return rule, true, nil
}

// deployMergeReport is SC-017's second clause held over every automated merge involving a deploy
// claim — every C8 auto_merge the fixture's graph holds — rather than over the pairs a manifest
// labelled (004 T129).
//
// The labelled figure (crossSourceReport) answers "did the merges we expected happen, and can they be
// explained?". It cannot answer SC-017's own wording, "every automated merge involving a deploy claim
// is explainable", because a merge nobody labelled is exactly the one nobody checked:
// `deploy-cross-source-merge-01` holds five C8 merges and labels three. So this counts them all.
//
// Nil on a fixture with no C8 merge, for the reason crossSourceReport gives: a figure held over
// nothing must read as not measured, not as passed.
func deployMergeReport(ctx context.Context, store *postgres.Store) (map[string]any, error) {
	rows, err := store.Pool().Query(ctx, `
		SELECT decision_id, surviving_id
		FROM graph.resolution_decisions
		WHERE kind = 'auto_merge' AND rule_id = 'C8' AND superseded_by IS NULL
		ORDER BY decided_at, decision_id`)
	if err != nil {
		return nil, fmt.Errorf("fixture: read deploy-claim merges: %w", err)
	}
	type merge struct{ decision, survivor string }
	var merges []merge
	for rows.Next() {
		var m merge
		if err := rows.Scan(&m.decision, &m.survivor); err != nil {
			rows.Close()
			return nil, fmt.Errorf("fixture: scan deploy-claim merge: %w", err)
		}
		merges = append(merges, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("fixture: read deploy-claim merges: %w", err)
	}
	if len(merges) == 0 {
		return nil, nil
	}
	explainable := 0
	var unexplained []string
	for _, m := range merges {
		ok, err := decisionExplained(ctx, store, m.decision)
		if err != nil {
			return nil, err
		}
		if ok {
			explainable++
			continue
		}
		unexplained = append(unexplained, m.decision)
	}
	return map[string]any{
		"total":           len(merges),
		"explainable":     explainable,
		"not_explainable": unexplained,
	}, nil
}

// decisionExplained is decisionIsExplainable for one decision by id.
func decisionExplained(ctx context.Context, store *postgres.Store, decisionID string) (bool, error) {
	var rule, rationale string
	var claimIDs []string
	if err := store.Pool().QueryRow(ctx, `
		SELECT coalesce(rule_id, ''), rationale, supporting_claim_ids
		FROM graph.resolution_decisions WHERE decision_id = $1`, decisionID).Scan(&rule, &rationale, &claimIDs); err != nil {
		return false, fmt.Errorf("fixture: read decision %s: %w", decisionID, err)
	}
	return evidenceResolves(ctx, store, decisionID, rule, rationale, claimIDs)
}

// evidenceResolves is the explainability test itself: a rule, a reasoning, and cited evidence every item
// of which still resolves, as an identity claim or a correlation key.
func evidenceResolves(ctx context.Context, store *postgres.Store, what, rule, rationale string, claimIDs []string) (bool, error) {
	if rule == "" || strings.TrimSpace(rationale) == "" || len(claimIDs) == 0 {
		return false, nil
	}
	var found int
	if err := store.Pool().QueryRow(ctx, `
		SELECT (SELECT count(*) FROM graph.identity_claims WHERE claim_id = ANY($1))
		     + (SELECT count(*) FROM graph.correlation_keys WHERE correlation_id = ANY($1))`,
		claimIDs).Scan(&found); err != nil {
		return false, fmt.Errorf("fixture: read supporting evidence of %s: %w", what, err)
	}
	return found == len(claimIDs), nil
}

// decisionIsExplainable reports whether the auto_merge on this entity recorded everything the audit
// query renders: the rule, the reasoning, and the claims it stood on, each of which still resolves.
func decisionIsExplainable(ctx context.Context, store *postgres.Store, entityID string) (bool, error) {
	var rule, rationale string
	var claimIDs []string
	err := store.Pool().QueryRow(ctx, `
		SELECT coalesce(rule_id, ''), rationale, supporting_claim_ids
		FROM graph.resolution_decisions
		WHERE kind = 'auto_merge' AND superseded_by IS NULL
		  AND (surviving_id = $1 OR merged_id = $1)
		ORDER BY decided_at, decision_id LIMIT 1`, entityID).Scan(&rule, &rationale, &claimIDs)
	if err != nil {
		return false, nil
	}
	if rule == "" || strings.TrimSpace(rationale) == "" || len(claimIDs) == 0 {
		return false, nil
	}
	// Every cited record must still be readable, or the explanation points at nothing.
	//
	// BOTH kinds, because a decision's evidence can be either (004 T148): C1, C4, C5 and C7 cite
	// identity claims, and C8 cites CORRELATION keys — a commit describes a rollout rather than naming
	// one. Reading only `identity_claims` here reported every C8 merge as unexplainable, which is the
	// same miss as the event log's reader: a second kind was introduced and a consumer written for the
	// first went on asking the first question. What makes this one worth a comment is that it failed
	// in the direction that looks like diligence — a gate reporting a violation nobody had committed.
	var found int
	if err := store.Pool().QueryRow(ctx, `
		SELECT (SELECT count(*) FROM graph.identity_claims WHERE claim_id = ANY($1))
		     + (SELECT count(*) FROM graph.correlation_keys WHERE correlation_id = ANY($1))`,
		claimIDs).Scan(&found); err != nil {
		return false, fmt.Errorf("fixture: read supporting evidence of %s: %w", entityID, err)
	}
	return found == len(claimIDs), nil
}
