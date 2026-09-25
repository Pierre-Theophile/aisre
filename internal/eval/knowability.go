// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Pierre-Theophile/aisre/internal/fixture"
)

// Knowability grading and the decoy rules (T111; FR-061, FR-061b, SC-007, SC-023,
// contracts/incident-format.md §Grading rules that are easy to get wrong).
//
// The rule that makes this file worth having, stated once: **`unknown` is sometimes the right
// answer, and a grader that cannot say so measures confidence rather than correctness.** Every
// incident fixture publishes a `knowability_time`, being the earliest observed instant at which
// the decisive evidence existed. Asked before it, `unknown` is a **pass** and naming the culprit
// is a **failure** — a lucky guess is not knowledge, and an evaluation that rewarded it would be
// selecting for an investigator that guesses. Asked after it, the culprit must be named *and* the
// decisive evidence must actually be satisfiable against what the run cited: a right answer with
// no evidence behind it is the same failure wearing the right hat.
//
// The decoys are graded on the **reason**, not only on the rank. `ground_truth.decoys[].causal_role`
// says why each ranked non-culprit is not the cause, and the contract publishes what the engine
// must do about each one. An investigator that exonerates a `coincident` change because it
// started after the onset — when it did not — has reached the right verdict by the wrong road,
// and the next incident is where that costs something.

// GroundTruth is the fixture's published ground truth. It is an alias rather than a copy so that
// a fixture and a grade never drift apart: adding a key to the manifest shape adds it here.
type GroundTruth = fixture.IncidentGroundTruth

// GradeResult is one run graded against one ground truth at one instant.
type GradeResult struct {
	// Fixture and RunIndex identify the run.
	Fixture  string `json:"fixture"`
	RunIndex int    `json:"run_index"`
	// Pass is the answer. It is a single boolean on purpose: FR-060 gates on pass@1 and pass^k,
	// and a partial credit that leaked into the gate would make the gate unreadable. Partial
	// credit lives in the report rows (T109), computed from the fields below.
	Pass bool `json:"pass"`
	// AskedAt is the instant the question was graded at, and BeforeKnowability whether that is
	// earlier than the fixture's `knowability_time`.
	AskedAt           time.Time `json:"asked_at"`
	KnowabilityTime   time.Time `json:"knowability_time"`
	BeforeKnowability bool      `json:"before_knowability"`
	// Expected is the verdict this grading expected, and Got what the run answered.
	Expected string `json:"expected"`
	Got      string `json:"got"`
	// Failures are the reasons the run did not pass, each a sentence naming what was wrong. An
	// empty list and Pass false cannot both happen.
	Failures []string `json:"failures,omitempty"`
	// Notes are the observations that are not failures — what a reviewer reads to understand a
	// pass.
	Notes []string `json:"notes,omitempty"`
	// Decisive are the decisive-evidence predicates and whether the run's own citations satisfy
	// them. Empty before the knowability instant, where no evidence is expected to exist yet.
	Decisive []PredicateResult `json:"decisive,omitempty"`
	// Decoys are the per-decoy results: whether it was named, and whether the engine's stated
	// reason matches the role the fixture declares.
	Decoys []DecoyResult `json:"decoys,omitempty"`
	// Localised says the symptom is still localised — the subject the question named is present
	// in the run's causal path with evidence behind it. It is what SC-023 grades a
	// culprit-deleted variant on: `unobserved` with the symptom placed is an answer, and
	// `unobserved` with nothing behind it is a shrug.
	Localised bool `json:"localised"`
	// DecoyReasonsMatch says every declared decoy was ruled out the way its role calls for, and
	// PassWithReasons is Pass and DecoyReasonsMatch together.
	//
	// They are separate from Pass on purpose. The contract says two different things about
	// decoys: **naming one is a failure** — that is in Pass, because it is a wrong answer — and
	// each one's *reason* must match its role, which is a check on how the answer was reached.
	// Folding the second into the headline would mean a run that named the culprit, cited the
	// decisive evidence and ruled out every decoy, but called one of them "inconclusive" where
	// the table says "refuted", would score the same as a run that named the wrong change.
	// Those are not the same failure and an evaluation that could not tell them apart would
	// send people to fix the wrong thing. Both numbers are published; which one gates is
	// `check-report.sh`'s decision (T112), not this file's.
	DecoyReasonsMatch bool `json:"decoy_reasons_match"`
	PassWithReasons   bool `json:"pass_with_reasons"`
}

// PredicateResult is one decisive-evidence predicate evaluated against what a run cited.
type PredicateResult struct {
	// Term is the algebra family the predicate is about, and FieldPath/Op/Value the assertion.
	Term      string `json:"term"`
	FieldPath string `json:"field_path"`
	Op        string `json:"op"`
	Value     any    `json:"value,omitempty"`
	// Satisfied says a digest the run cited satisfies it, and EvidenceID names which one.
	Satisfied  bool   `json:"satisfied"`
	EvidenceID string `json:"evidence_id,omitempty"`
	// Detail says why not, when it was not: no citation of that family, the field absent from
	// every one of them, or the comparison failing.
	Detail string `json:"detail,omitempty"`
}

// DecoyResult is one declared decoy, graded.
type DecoyResult struct {
	// Entity is the decoy and CausalRole the role the fixture declares for it.
	Entity     string `json:"entity"`
	CausalRole string `json:"causal_role"`
	// Named says the run named it as the answer, which is always a failure.
	Named bool `json:"named"`
	// Why is the fixture's own sentence about this decoy, carried so a failure reads as the
	// fixture author wrote it.
	Why string `json:"why,omitempty"`
	// Status and Rationale are what the engine did with it and why, as the ledger states them.
	Status             string `json:"status,omitempty"`
	Rationale          string `json:"rationale,omitempty"`
	CausalRoleReported string `json:"causal_role_reported,omitempty"`
	Rank               int    `json:"rank,omitempty"`
	// ReasonMatches says the engine's stated reason is the one the role calls for, and Detail
	// says what was expected when it does not.
	ReasonMatches bool   `json:"reason_matches"`
	Detail        string `json:"detail,omitempty"`
}

// Grade grades one run against one ground truth, as asked at one instant.
//
// `askedAt` is the instant the question is put, not the instant the run happened: a fixture is
// graded on what was knowable when the on-call was asked, which is `incident.question.fired_at`
// for the ordinary case and any instant a caller chooses for the knowability sweep.
func Grade(outcome *RunOutcome, truth GroundTruth, askedAt time.Time) GradeResult {
	result := GradeResult{
		AskedAt:         askedAt.UTC(),
		KnowabilityTime: truth.KnowabilityTime.UTC(),
		Got:             VerdictUnknown,
	}
	if outcome == nil {
		result.Failures = append(result.Failures, "there is no outcome to grade")
		return result
	}
	result.Fixture = outcome.FixtureID
	result.RunIndex = outcome.RunIndex
	result.Got = outcome.Verdict
	result.Localised = localised(outcome, truth)
	result.BeforeKnowability = !truth.KnowabilityTime.IsZero() && askedAt.Before(truth.KnowabilityTime)

	if result.BeforeKnowability {
		gradeBeforeKnowability(outcome, truth, &result)
		result.DecoyReasonsMatch, result.PassWithReasons = true, result.Pass
		return result
	}
	gradeAfterKnowability(outcome, truth, &result)
	return result
}

// gradeBeforeKnowability applies the rule that makes `unknown` correct.
func gradeBeforeKnowability(outcome *RunOutcome, truth GroundTruth, result *GradeResult) {
	result.Expected = VerdictUnknown
	result.Notes = append(result.Notes, fmt.Sprintf(
		"asked at %s, before the decisive evidence existed at %s: `unknown` is the correct answer (FR-061b)",
		result.AskedAt.Format(time.RFC3339), result.KnowabilityTime.Format(time.RFC3339)))

	if outcome.IsUnknown() {
		result.Pass = true
		return
	}
	if namesCulprit(outcome, truth) {
		result.Failures = append(result.Failures, fmt.Sprintf(
			"named the culprit %s at %s, before it was knowable at %s; a correct answer reached before "+
				"the evidence for it existed is a guess, and is scored as one (FR-061b, SC-007)",
			truth.Culprit, result.AskedAt.Format(time.RFC3339), result.KnowabilityTime.Format(time.RFC3339)))
		return
	}
	result.Failures = append(result.Failures, fmt.Sprintf(
		"answered %q before the decisive evidence existed at %s; the correct answer was `unknown`",
		outcome.Verdict, result.KnowabilityTime.Format(time.RFC3339)))
}

// gradeAfterKnowability applies the ordinary rules: the right answer, for the right reason, with
// the decoys ruled out the way the contract says they must be.
func gradeAfterKnowability(outcome *RunOutcome, truth GroundTruth, result *GradeResult) {
	result.Expected = expectedVerdict(truth)
	result.Pass = true

	switch truth.Class() {
	case fixture.CulpritUnobserved, fixture.CulpritNotChangeInduced:
		// `unobserved` ground truth: the answer must be `unobserved`, with the symptom still
		// localised. A run that names any change here is naming a decoy by construction, because
		// no change is the cause.
		if outcome.VerdictClass() != VerdictUnobserved {
			result.Pass = false
			result.Failures = append(result.Failures, fmt.Sprintf(
				"answered %q where no observed change explains the symptom; the expected answer is %q (FR-071b, SC-023)",
				outcome.Verdict, result.Expected))
		}
		if !result.Localised {
			result.Pass = false
			result.Failures = append(result.Failures, fmt.Sprintf(
				"answered the unobserved remainder without localising the symptom on %s; `unobserved` with "+
					"nothing behind it is a shrug, not a finding (SC-023)", symptomEntity(truth)))
		}
	default:
		if !namesCulprit(outcome, truth) {
			result.Pass = false
			result.Failures = append(result.Failures, fmt.Sprintf(
				"answered %q; the culprit is %s and the decisive evidence for it existed from %s",
				outcome.Verdict, truth.Culprit, result.KnowabilityTime.Format(time.RFC3339)))
		}
		result.Decisive = gradeDecisiveEvidence(outcome, truth)
		for _, predicate := range result.Decisive {
			if predicate.Satisfied {
				continue
			}
			result.Pass = false
			result.Failures = append(result.Failures, fmt.Sprintf(
				"the decisive predicate `%s %s %v` over a %s answer is not satisfiable against what the run "+
					"cited: %s", predicate.FieldPath, predicate.Op, predicate.Value, predicate.Term, predicate.Detail))
		}
	}

	result.Decoys = gradeDecoys(outcome, truth)
	result.DecoyReasonsMatch = true
	for _, decoy := range result.Decoys {
		if decoy.Named {
			result.Pass = false
			result.Failures = append(result.Failures, fmt.Sprintf(
				"named the decoy %s, whose causal role is %s: %s", decoy.Entity, decoy.CausalRole, decoy.Why))
			continue
		}
		if !decoy.ReasonMatches {
			result.DecoyReasonsMatch = false
			result.Notes = append(result.Notes, fmt.Sprintf(
				"decoy %s is declared %s but %s (contracts/incident-format.md §Decoy roles)",
				decoy.Entity, decoy.CausalRole, decoy.Detail))
		}
	}
	result.PassWithReasons = result.Pass && result.DecoyReasonsMatch
}

// expectedVerdict renders the answer a fixture's ground truth calls for.
func expectedVerdict(truth GroundTruth) string {
	switch truth.Class() {
	case fixture.CulpritUnobserved:
		return VerdictUnobserved
	case fixture.CulpritNotChangeInduced:
		return VerdictNotChangeInducedPrefix + ": " + truth.CauseCategory()
	default:
		return strings.TrimSpace(truth.Culprit)
	}
}

// namesCulprit reports whether the run's verdict is the culprit, in any spelling the graph
// publishes for it.
func namesCulprit(outcome *RunOutcome, truth GroundTruth) bool {
	want := strings.TrimSpace(truth.Culprit)
	if want == "" || outcome.VerdictClass() != "change_induced" {
		return false
	}
	if outcome.Verdict == want {
		return true
	}
	for _, ref := range outcome.VerdictRefs {
		if ref == want {
			return true
		}
	}
	return false
}

// symptomEntity is the last step of the declared causal path: the thing that broke.
func symptomEntity(truth GroundTruth) string {
	if len(truth.CausalPath) == 0 {
		return ""
	}
	return truth.CausalPath[len(truth.CausalPath)-1].Entity
}

// localised reports whether the run placed the symptom where the fixture says it is.
//
// It is deliberately weak — the symptom entity appears in the run's own causal path — because a
// stronger test would be grading the *explanation*, which is what the partial-credit rows in the
// report do. What this answers is the SC-023 question: did the run localise the symptom at all,
// or did it return `unobserved` with nothing attached to it.
func localised(outcome *RunOutcome, truth GroundTruth) bool {
	symptom := symptomEntity(truth)
	if symptom == "" {
		return len(outcome.CausalPath) > 0
	}
	for _, step := range outcome.CausalPath {
		if step.Entity == symptom {
			return true
		}
	}
	return false
}

// ---------- the decisive evidence ----------

// gradeDecisiveEvidence evaluates every declared predicate against the digests the run cited.
func gradeDecisiveEvidence(outcome *RunOutcome, truth GroundTruth) []PredicateResult {
	out := make([]PredicateResult, 0, len(truth.DecisiveEvidence))
	for _, predicate := range truth.DecisiveEvidence {
		out = append(out, SatisfiedBy(predicate, outcome.Citations))
	}
	return out
}

// SatisfiedBy evaluates one predicate against a run's citations, and reports which one satisfied
// it.
//
// The predicate names an algebra family; only citations of that family are considered, because a
// predicate about `errors_by_version` satisfied by a `compare` answer would be a predicate
// satisfied by coincidence of field names.
func SatisfiedBy(predicate fixture.EvidencePredicate, citations []Citation) PredicateResult {
	result := PredicateResult{
		Term:      predicateFamily(predicate),
		FieldPath: predicate.FieldPath,
		Op:        predicate.Op,
		Value:     predicate.Value,
	}
	var (
		considered int
		lastDetail string
	)
	for _, citation := range citations {
		if result.Term != "" && citation.Term != result.Term {
			continue
		}
		if len(citation.Digest) == 0 {
			continue
		}
		considered++
		ok, detail := evaluate(predicate, citation.Digest)
		if ok {
			result.Satisfied = true
			result.EvidenceID = citation.EvidenceID
			return result
		}
		lastDetail = detail
	}
	if considered == 0 {
		result.Detail = fmt.Sprintf("the run cited no %s answer at all", result.Term)
		return result
	}
	result.Detail = fmt.Sprintf("%d cited %s answer(s), none satisfying it (%s)",
		considered, result.Term, lastDetail)
	return result
}

// predicateFamily is the single algebra family a predicate names.
func predicateFamily(predicate fixture.EvidencePredicate) string {
	for family := range predicate.Term {
		return family
	}
	return ""
}

// evaluate applies one predicate to one digest.
func evaluate(predicate fixture.EvidencePredicate, digest json.RawMessage) (bool, string) {
	var root any
	if err := json.Unmarshal(digest, &root); err != nil {
		return false, "the digest is not JSON: " + err.Error()
	}
	values, err := Resolve(root, predicate.FieldPath)
	if err != nil {
		return false, err.Error()
	}
	switch predicate.Op {
	case "present":
		if len(values) == 0 {
			return false, "the field is absent"
		}
		return true, ""
	case "absent":
		if len(values) == 0 {
			return true, ""
		}
		return false, fmt.Sprintf("the field is present (%v)", values[0])
	}
	if len(values) == 0 {
		return false, fmt.Sprintf("the field path %q resolves to nothing in this answer", predicate.FieldPath)
	}
	var last string
	for _, value := range values {
		ok, detail := compare(value, predicate.Op, predicate.Value)
		if ok {
			return true, ""
		}
		last = detail
	}
	return false, last
}

// compare applies one published comparison.
//
// Numbers compare as numbers, instants as instants and everything else as strings, and the
// coercion is one-directional: the *declared* value says what kind of comparison this is, because
// the fixture is where a human stated the intent.
func compare(got any, op string, want any) (bool, string) {
	switch op {
	case "contains", "not_contains":
		hit := strings.Contains(asString(got), asString(want))
		if op == "not_contains" {
			hit = !hit
		}
		if hit {
			return true, ""
		}
		return false, fmt.Sprintf("%q does not satisfy %s %q", asString(got), op, asString(want))
	}

	if wantTime, ok := asTime(want); ok {
		gotTime, ok := asTime(got)
		if !ok {
			return false, fmt.Sprintf("%v is not an instant, and the predicate compares against one", got)
		}
		return compareOrdered(float64(gotTime.UnixNano()), op, float64(wantTime.UnixNano()),
			gotTime.Format(time.RFC3339), wantTime.Format(time.RFC3339))
	}
	if wantNumber, ok := asNumber(want); ok {
		gotNumber, ok := asNumber(got)
		if !ok {
			return false, fmt.Sprintf("%v is not a number, and the predicate compares against one", got)
		}
		return compareOrdered(gotNumber, op, wantNumber,
			strconv.FormatFloat(gotNumber, 'g', -1, 64), strconv.FormatFloat(wantNumber, 'g', -1, 64))
	}

	gotText, wantText := asString(got), asString(want)
	switch op {
	case "eq":
		if gotText == wantText {
			return true, ""
		}
	case "ne":
		if gotText != wantText {
			return true, ""
		}
	default:
		return false, fmt.Sprintf("%s is an ordering comparison and %q is not ordered", op, wantText)
	}
	return false, fmt.Sprintf("%q %s %q is false", gotText, op, wantText)
}

// compareOrdered applies an ordering comparison to two numbers.
func compareOrdered(got float64, op string, want float64, gotText, wantText string) (bool, string) {
	var ok bool
	switch op {
	case "eq":
		ok = math.Abs(got-want) < comparisonEpsilon
	case "ne":
		ok = math.Abs(got-want) >= comparisonEpsilon
	case "lt":
		ok = got < want
	case "lte", "le":
		ok = got <= want
	case "gt":
		ok = got > want
	case "gte", "ge":
		ok = got >= want
	default:
		return false, fmt.Sprintf("%q is not a published comparison", op)
	}
	if ok {
		return true, ""
	}
	return false, fmt.Sprintf("%s %s %s is false", gotText, op, wantText)
}

// comparisonEpsilon is the tolerance an `eq` over floating-point values is judged within. It is
// published rather than silent because a digest's numbers arrive through JSON and a bit-exact
// equality over them would fail on a value nobody changed.
const comparisonEpsilon = 1e-9

func asString(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case bool:
		return strconv.FormatBool(v)
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64)
	case time.Time:
		return v.UTC().Format(time.RFC3339)
	default:
		return fmt.Sprintf("%v", v)
	}
}

func asNumber(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(v, 64)
		return f, err == nil
	default:
		return 0, false
	}
}

func asTime(value any) (time.Time, bool) {
	switch v := value.(type) {
	case time.Time:
		return v.UTC(), true
	case string:
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
			if parsed, err := time.Parse(layout, v); err == nil {
				return parsed.UTC(), true
			}
		}
	}
	return time.Time{}, false
}

// ---------- the field path ----------

// Resolve walks a `field_path` over a decoded digest and returns every value it addresses.
//
// The grammar, which is the one the corpus is written in:
//
//	name                     a field
//	name.other               a field of a field
//	list[*]                  every element of a list
//	list[key=value]          the elements whose `key` equals `value`
//	list[a.b=value]          the same, addressing a nested field as the selector
//
// A selector's key may itself be a dotted path, and its value may contain spaces and dots, so
// the scan is character by character rather than a `strings.Split` — `changes[change.display_name=rollout
// checkout-worker to revision 12].unattached` is a real predicate in the corpus and a naive split
// destroys it.
//
// Resolution is tried at the top level first and then inside the digest's oneof body, because a
// predicate is written against the answer (`versions[…]`) while the wire carries it wrapped in
// the envelope the algebra returns (`{"errors_by_version": {"versions": […]}}`). Trying both is
// what lets a fixture be written the way a person reads a digest.
func Resolve(root any, path string) ([]any, error) {
	segments, err := parsePath(path)
	if err != nil {
		return nil, err
	}
	if values := walk([]any{root}, segments); len(values) > 0 {
		return values, nil
	}
	// The envelope: try each single-key wrapper one level down.
	object, ok := root.(map[string]any)
	if !ok {
		return nil, nil
	}
	for _, key := range sortedObjectKeys(object) {
		nested, ok := object[key].(map[string]any)
		if !ok {
			continue
		}
		if values := walk([]any{nested}, segments); len(values) > 0 {
			return values, nil
		}
	}
	return nil, nil
}

// pathSegment is one step: a field name, and the selector that follows it when there is one.
type pathSegment struct {
	// Name is the field. It is empty for a bare selector, which is not legal and is refused by
	// the parser.
	Name string
	// Selector is `*` for every element, `key=value` for a filter, or empty for no selector.
	SelectorKey   string
	SelectorValue string
	SelectorAll   bool
	HasSelector   bool
}

// parsePath reads the grammar above.
func parsePath(path string) ([]pathSegment, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("an empty field_path addresses nothing")
	}
	var (
		segments []pathSegment
		name     strings.Builder
	)
	flush := func() {
		if name.Len() > 0 {
			segments = append(segments, pathSegment{Name: name.String()})
			name.Reset()
		}
	}
	for i := 0; i < len(path); i++ {
		switch path[i] {
		case '.':
			flush()
		case '[':
			end := strings.IndexByte(path[i:], ']')
			if end < 0 {
				return nil, fmt.Errorf("field_path %q has a `[` with no `]`", path)
			}
			inner := path[i+1 : i+end]
			if name.Len() == 0 {
				return nil, fmt.Errorf("field_path %q has a selector with no field before it", path)
			}
			segment := pathSegment{Name: name.String(), HasSelector: true}
			name.Reset()
			if inner == "*" {
				segment.SelectorAll = true
			} else {
				key, value, ok := strings.Cut(inner, "=")
				if !ok {
					return nil, fmt.Errorf("field_path %q has a selector %q that is neither `*` nor `key=value`", path, inner)
				}
				segment.SelectorKey = strings.TrimSpace(key)
				segment.SelectorValue = strings.TrimSpace(value)
			}
			segments = append(segments, segment)
			i += end
		default:
			name.WriteByte(path[i])
		}
	}
	flush()
	if len(segments) == 0 {
		return nil, fmt.Errorf("field_path %q addresses nothing", path)
	}
	return segments, nil
}

// walk applies the segments to a set of values.
func walk(values []any, segments []pathSegment) []any {
	for _, segment := range segments {
		var next []any
		for _, value := range values {
			object, ok := value.(map[string]any)
			if !ok {
				continue
			}
			field, ok := object[segment.Name]
			if !ok {
				continue
			}
			if !segment.HasSelector {
				next = append(next, field)
				continue
			}
			list, ok := field.([]any)
			if !ok {
				continue
			}
			for _, element := range list {
				if segment.SelectorAll || selects(element, segment.SelectorKey, segment.SelectorValue) {
					next = append(next, element)
				}
			}
		}
		values = next
		if len(values) == 0 {
			return nil
		}
	}
	return values
}

// selects reports whether one list element matches a `key=value` selector.
func selects(element any, key, want string) bool {
	segments, err := parsePath(key)
	if err != nil {
		return false
	}
	for _, value := range walk([]any{element}, segments) {
		if asString(value) == want {
			return true
		}
	}
	return false
}

func sortedObjectKeys(object map[string]any) []string {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// ---------- the decoys ----------

// gradeDecoys grades every declared decoy against what the run did with it.
func gradeDecoys(outcome *RunOutcome, truth GroundTruth) []DecoyResult {
	out := make([]DecoyResult, 0, len(truth.Decoys))
	for _, decoy := range truth.Decoys {
		out = append(out, gradeDecoy(outcome, truth, decoy))
	}
	return out
}

// gradeDecoy applies the contract's table: each role says what the engine must have done, and
// what it must have said about it.
func gradeDecoy(outcome *RunOutcome, truth GroundTruth, decoy fixture.Decoy) DecoyResult {
	result := DecoyResult{Entity: decoy.Entity, CausalRole: decoy.CausalRole, Why: decoy.Why}
	hypothesis, found := outcome.Hypothesis(decoy.Entity)
	if found {
		result.Status = hypothesis.Status
		result.Rationale = hypothesis.Rationale
		result.CausalRoleReported = hypothesis.CausalRole
		result.Rank = hypothesis.Rank
	}
	// Naming a decoy is a failure whatever its role, and it is checked before the role table so
	// the message says the thing that matters.
	for _, ref := range append([]string{outcome.Verdict}, outcome.VerdictRefs...) {
		if ref != "" && ref == decoy.Entity {
			result.Named = true
			result.Detail = decoy.Why
			return result
		}
	}

	switch decoy.CausalRole {
	case "candidate_effect":
		// Exonerated by the causal ordering, shown as an effect.
		result.ReasonMatches = found &&
			hypothesis.Status == statusExonerated &&
			hypothesis.CausalRole == "candidate_effect"
		result.Detail = "the contract expects it exonerated by the causal ordering and reported as an effect; " +
			describeDecoy(found, hypothesis)
	case "coincident":
		// Refuted by evidence on its own target. Read as substance rather than as a status
		// name: evidence was brought against it and it did not survive. A ledger that calls
		// that `inconclusive` because the judgments were neutral has still done the thing the
		// contract asks for, and one that leaves the candidate untouched has not.
		result.ReasonMatches = found &&
			hypothesis.Status != statusSupported &&
			(hypothesis.Status == statusRefuted || hypothesis.Status == statusExonerated || hypothesis.Tested())
		result.Detail = "the contract expects it refuted by evidence on its own target; " +
			describeDecoy(found, hypothesis)
	case "stale":
		// Refuted or left untested with a stated reason. A candidate sitting at `proposed` with
		// no judgment and no reason is neither, and saying so is the point: the fixture places
		// it 70 minutes before the onset precisely so that something has to rule it out.
		result.ReasonMatches = found &&
			(hypothesis.Status == statusRefuted || hypothesis.Status == statusExonerated ||
				(hypothesis.Status == statusUntested && strings.TrimSpace(hypothesis.UntestedReason) != "") ||
				(hypothesis.Status == statusInconclusive && hypothesis.Tested()))
		result.Detail = "the contract expects it refuted, or left untested with a stated reason; " +
			describeDecoy(found, hypothesis)
	case "out_of_scope":
		// Not a candidate; it must not be invented.
		result.ReasonMatches = !found
		result.Detail = "the contract expects it never to become a candidate — it is excluded, not out-argued; " +
			describeDecoy(found, hypothesis)
	case "unattached":
		// Reported unattached at reduced rank: present, below the top, and never supported.
		result.ReasonMatches = !found ||
			(hypothesis.Status != statusSupported && hypothesis.Rank > 1)
		result.Detail = "the contract expects it reported unattached at reduced rank, never supported; " +
			describeDecoy(found, hypothesis)
	case "not_separable":
		// Both reported, tie-break stated, neither exonerated.
		result.ReasonMatches = found &&
			hypothesis.Status != statusExonerated &&
			comparableConfidence(outcome, truth, hypothesis)
		result.Detail = "the contract expects it reported beside the culprit at comparable confidence and " +
			"exonerated by nothing (FR-025); " + describeDecoy(found, hypothesis)
	default:
		result.ReasonMatches = true
		result.Detail = "no rule is published for this role"
	}
	if result.ReasonMatches {
		result.Detail = ""
	}
	return result
}

// describeDecoy renders what the engine actually did with a decoy.
func describeDecoy(found bool, hypothesis Hypothesis) string {
	if !found {
		return "the run's ledger holds no hypothesis naming it"
	}
	detail := fmt.Sprintf("the run ranked it %d at %s", hypothesis.Rank, hypothesis.Status)
	if hypothesis.CausalRole != "" {
		detail += " as " + hypothesis.CausalRole
	}
	if hypothesis.Rationale != "" {
		detail += fmt.Sprintf(" (%q)", hypothesis.Rationale)
	}
	return detail
}

// comparableConfidence reports whether a `not_separable` decoy is reported at a confidence
// comparable to the culprit's — the same published bucket, which is what "comparable" means when
// confidence is reported in buckets (FR-023).
func comparableConfidence(outcome *RunOutcome, truth GroundTruth, decoy Hypothesis) bool {
	culprit, ok := outcome.Hypothesis(truth.Culprit)
	if !ok {
		return false
	}
	return culprit.Bucket == decoy.Bucket
}
