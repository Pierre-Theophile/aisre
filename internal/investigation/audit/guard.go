// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
)

// The ceiling guard (T017, FR-071, SC-022).
//
// FR-071 is unusually precise about scope, and the precision is the whole point: the ceiling
// bounds **recall- and coverage-dependent** targets only. Citation validity is 100 % whether or
// not the cause was observable; a replay is byte-identical or it is not; a 5-second provisional
// ranking does not get slower because a feeder is missing; and answering `unknown` correctly is
// the *complement* of the ceiling rather than a casualty of it. So this guard does three things
// and refuses to do a fourth:
//
//  1. It fails a `[ceiling-bounded]` target set above the ceiling of the audit it cites.
//  2. It fails a criterion carrying **no annotation at all** — loudly. An unannotated criterion
//     is the failure mode this guard exists to catch: it is the one that silently escapes both
//     branches, and "nobody said which side it was on" must never read as "it passed".
//  3. It fails a criterion whose annotation **disagrees with the published table** in spec
//     §"Which criteria the coverage ceiling bounds". Mis-annotating SC-001 as not
//     ceiling-bounded would let an unreachable target through; mis-annotating SC-005 as
//     ceiling-bounded would scale a latency target down to 62 %, which FR-071 forbids in the
//     same sentence. Both are caught here.
//  4. It does NOT check, scale, or comment on the value of a `[not ceiling-bounded: …]`
//     criterion. It reports it as not checked and moves on.

// Annotation is a success criterion's declared relationship to the coverage ceiling.
type Annotation string

const (
	// AnnotationCeilingBounded is `[ceiling-bounded]`: the target may not exceed the ceiling.
	AnnotationCeilingBounded Annotation = "ceiling-bounded"
	// AnnotationPrecision is `[not ceiling-bounded: precision]`.
	AnnotationPrecision Annotation = "not ceiling-bounded: precision"
	// AnnotationLatency is `[not ceiling-bounded: latency]`.
	AnnotationLatency Annotation = "not ceiling-bounded: latency"
	// AnnotationInvariance is `[not ceiling-bounded: invariance]`.
	AnnotationInvariance Annotation = "not ceiling-bounded: invariance"
	// AnnotationValidity is `[not ceiling-bounded: validity]`.
	AnnotationValidity Annotation = "not ceiling-bounded: validity"
)

var annotationOrder = []Annotation{
	AnnotationCeilingBounded,
	AnnotationPrecision,
	AnnotationLatency,
	AnnotationInvariance,
	AnnotationValidity,
}

// Annotations returns the published annotations.
func Annotations() []Annotation { return slices.Clone(annotationOrder) }

// Valid reports whether a is a published annotation.
func (a Annotation) Valid() bool { return slices.Contains(annotationOrder, a) }

// Bounded reports whether the ceiling bounds a criterion carrying this annotation.
func (a Annotation) Bounded() bool { return a == AnnotationCeilingBounded }

// parseAnnotation normalises the spellings the spec and a report may use: with or without the
// square brackets, any spacing around the colon, any case.
func parseAnnotation(raw string) (Annotation, error) {
	normalized := strings.ToLower(strings.TrimSpace(raw))
	normalized = strings.TrimPrefix(normalized, "[")
	normalized = strings.TrimSuffix(normalized, "]")
	normalized = strings.Join(strings.Fields(normalized), " ")
	normalized = strings.ReplaceAll(normalized, " :", ":")
	if normalized == "" {
		return "", fmt.Errorf("no annotation")
	}
	for _, annotation := range annotationOrder {
		if normalized == string(annotation) {
			return annotation, nil
		}
	}
	return "", fmt.Errorf("annotation %q: want one of %s", raw, oneOf(annotationOrder))
}

// publishedAnnotations is spec 002 §"Which criteria the coverage ceiling bounds", as data. The
// table is the contract; keeping a copy here is what makes it executable rather than decorative.
// A criterion added to the spec and not to this map is simply not cross-checked; a criterion in
// both must agree.
var publishedAnnotations = map[string]Annotation{
	"SC-001": AnnotationCeilingBounded,
	"SC-006": AnnotationCeilingBounded,
	"SC-021": AnnotationCeilingBounded,

	"SC-002": AnnotationValidity,
	"SC-004": AnnotationValidity,
	"SC-008": AnnotationValidity,
	"SC-009": AnnotationValidity,
	"SC-010": AnnotationValidity,
	"SC-012": AnnotationValidity,
	"SC-014": AnnotationValidity,
	"SC-015": AnnotationValidity,
	"SC-017": AnnotationValidity,
	"SC-019": AnnotationValidity,
	"SC-022": AnnotationValidity,
	"SC-024": AnnotationValidity,
	"SC-025": AnnotationValidity,

	"SC-003": AnnotationInvariance,
	"SC-011": AnnotationInvariance,
	"SC-018": AnnotationInvariance,
	"SC-020": AnnotationInvariance,

	"SC-005": AnnotationLatency,
	"SC-013": AnnotationLatency,

	"SC-007": AnnotationPrecision,
	"SC-016": AnnotationPrecision,
	"SC-023": AnnotationPrecision,
}

// PublishedAnnotation returns the spec's annotation for a criterion id, if it publishes one.
func PublishedAnnotation(id string) (Annotation, bool) {
	annotation, ok := publishedAnnotations[strings.ToUpper(strings.TrimSpace(id))]
	return annotation, ok
}

// Criterion is one published target as an evaluation report states it.
type Criterion struct {
	// ID is the success criterion, e.g. `SC-001`.
	ID string `json:"id"`
	// Metric names what is measured, e.g. `pass@1`. It is reported, never interpreted.
	Metric string `json:"metric,omitempty"`
	// Target is the published target, as a fraction in [0, 1].
	Target float64 `json:"target"`
	// Annotation is the criterion's declared relationship to the ceiling. An empty annotation
	// is a hard failure, not a default.
	Annotation string `json:"annotation"`
	// Audit is the audit id the target cites. Required on a ceiling-bounded criterion.
	Audit string `json:"audit,omitempty"`
}

// GuardInput is the evaluation report the guard reads.
type GuardInput struct {
	// Criteria are the published targets.
	Criteria []Criterion `json:"criteria"`
}

// GuardOutcome is what the guard did with one criterion.
type GuardOutcome string

const (
	// GuardPass is a ceiling-bounded target at or below the ceiling of the audit it cites.
	GuardPass GuardOutcome = "pass"
	// GuardFail is a target the guard refuses.
	GuardFail GuardOutcome = "fail"
	// GuardNotChecked is a criterion the ceiling does not bound. It is reported so that
	// "not checked" is visible rather than silent, and its value is never scaled.
	GuardNotChecked GuardOutcome = "not_checked"
)

// GuardFinding is one criterion's outcome.
type GuardFinding struct {
	// Criterion is the id.
	Criterion string `json:"criterion"`
	// Metric is what it measures.
	Metric string `json:"metric,omitempty"`
	// Target is the published target.
	Target float64 `json:"target"`
	// Annotation is the annotation as read, empty when the criterion carried none.
	Annotation Annotation `json:"annotation,omitempty"`
	// Outcome is what the guard did.
	Outcome GuardOutcome `json:"outcome"`
	// Detail is why, in the words a build log should carry.
	Detail string `json:"detail"`
}

// GuardReport is the whole guard run.
type GuardReport struct {
	// AuditID and Ceiling are the audit the guard checked against.
	AuditID string  `json:"audit_id"`
	Ceiling float64 `json:"ceiling"`
	// FeederSet is the configuration that ceiling was measured against.
	FeederSet string `json:"feeder_set"`
	// ClassifiableCount is the count the ceiling rests on.
	ClassifiableCount int `json:"classifiable_count"`
	// Findings is one row per criterion, in the order the report listed them.
	Findings []GuardFinding `json:"findings"`
	// Checked, Passed, Failed and NotChecked are the counts.
	Checked    int `json:"checked"`
	Passed     int `json:"passed"`
	Failed     int `json:"failed"`
	NotChecked int `json:"not_checked"`
}

// OK reports whether every checked criterion held.
func (r *GuardReport) OK() bool { return r.Failed == 0 }

// LoadGuardInput reads an evaluation report's criteria from a file. Three shapes are accepted,
// because the report writer that will feed this guard does not exist yet (T109/T112) and a
// guard that only reads one shape would be a guard nobody wires up: an object with a
// `criteria` array, a bare array of criteria, or one criterion per line.
func LoadGuardInput(path string) (*GuardInput, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // the operator names the report they gate.
	if err != nil {
		return nil, fmt.Errorf("audit: read evaluation report: %w", err)
	}
	input, err := ParseGuardInput(raw)
	if err != nil {
		return nil, fmt.Errorf("audit: %s: %w", path, err)
	}
	return input, nil
}

// ParseGuardInput decodes the criteria of an evaluation report.
func ParseGuardInput(raw []byte) (*GuardInput, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("decode evaluation report: empty file")
	}

	switch trimmed[0] {
	case '[':
		var criteria []Criterion
		if err := json.Unmarshal(trimmed, &criteria); err != nil {
			return nil, fmt.Errorf("decode evaluation report: %w", err)
		}
		return &GuardInput{Criteria: criteria}, nil
	case '{':
		var input GuardInput
		decoder := json.NewDecoder(bytes.NewReader(trimmed))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err == nil && len(input.Criteria) > 0 {
			return &input, nil
		}
		return parseGuardJSONL(trimmed)
	default:
		return nil, fmt.Errorf("decode evaluation report: want JSON criteria, got %q", firstRune(trimmed))
	}
}

func parseGuardJSONL(raw []byte) (*GuardInput, error) {
	input := &GuardInput{}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for line := 1; scanner.Scan(); line++ {
		text := bytes.TrimSpace(scanner.Bytes())
		if len(text) == 0 {
			continue
		}
		// Strict per line: a JSON object that is not a criterion — an object with a
		// `criteria` key whose value is the wrong shape, say — must be reported as
		// malformed rather than read as a criterion with every field defaulted.
		var criterion Criterion
		decoder := json.NewDecoder(bytes.NewReader(text))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&criterion); err != nil {
			return nil, fmt.Errorf("decode evaluation report line %d: %w", line, err)
		}
		input.Criteria = append(input.Criteria, criterion)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("decode evaluation report: %w", err)
	}
	if len(input.Criteria) == 0 {
		return nil, fmt.Errorf("decode evaluation report: no criteria")
	}
	return input, nil
}

func firstRune(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	return string(raw[0])
}

// Guard checks an evaluation report's published targets against an audit's ceiling.
func Guard(input *GuardInput, result *Result) (*GuardReport, error) {
	if input == nil {
		return nil, fmt.Errorf("audit: guard: no evaluation report")
	}
	if result == nil {
		return nil, fmt.Errorf("audit: guard: no audit to check against — " +
			"a ceiling-bounded target with no audit is exactly what FR-071 forbids")
	}
	if len(input.Criteria) == 0 {
		return nil, fmt.Errorf("audit: guard: the evaluation report publishes no criteria")
	}

	report := &GuardReport{
		AuditID:           result.AuditID,
		Ceiling:           result.Ceiling,
		FeederSet:         result.FeederSetInForce,
		ClassifiableCount: result.ClassifiableCount,
	}
	for _, criterion := range input.Criteria {
		report.Findings = append(report.Findings, checkCriterion(criterion, result))
	}
	for _, finding := range report.Findings {
		switch finding.Outcome {
		case GuardPass:
			report.Checked++
			report.Passed++
		case GuardFail:
			report.Checked++
			report.Failed++
		case GuardNotChecked:
			report.NotChecked++
		}
	}
	return report, nil
}

func checkCriterion(criterion Criterion, result *Result) GuardFinding {
	finding := GuardFinding{
		Criterion: criterion.ID,
		Metric:    criterion.Metric,
		Target:    criterion.Target,
		Outcome:   GuardFail,
	}
	if strings.TrimSpace(criterion.ID) == "" {
		finding.Detail = "a criterion with no id: the guard cannot check what it cannot name"
		return finding
	}

	annotation, err := parseAnnotation(criterion.Annotation)
	if err != nil {
		// FR-071: the guard fails loudly if a criterion carries no annotation at all.
		finding.Detail = fmt.Sprintf(
			"%s carries no usable ceiling annotation (%v). Every published criterion must declare "+
				"[ceiling-bounded] or [not ceiling-bounded: precision|latency|invariance|validity] "+
				"(spec §\"Which criteria the coverage ceiling bounds\")", criterion.ID, err)
		return finding
	}
	finding.Annotation = annotation

	if published, ok := PublishedAnnotation(criterion.ID); ok && published != annotation {
		finding.Detail = fmt.Sprintf(
			"%s is annotated %q but the specification publishes it as %q. "+
				"Re-annotating a criterion is how a target escapes the guard, or how a "+
				"precision, latency, invariance or validity target gets wrongly scaled down to "+
				"the ceiling — FR-071 forbids both",
			criterion.ID, annotation, published)
		return finding
	}

	if !annotation.Bounded() {
		finding.Outcome = GuardNotChecked
		finding.Detail = fmt.Sprintf(
			"%s is %s: the ceiling does not bound it and its target is NOT scaled down to it (FR-071)",
			criterion.ID, annotation)
		return finding
	}

	if criterion.Target < 0 || criterion.Target > 1 {
		finding.Detail = fmt.Sprintf(
			"%s: target %g is outside [0, 1] — publish a fraction, not a percentage",
			criterion.ID, criterion.Target)
		return finding
	}
	if strings.TrimSpace(criterion.Audit) == "" {
		finding.Detail = fmt.Sprintf(
			"%s is ceiling-bounded but cites no audit. FR-071: each such target MUST cite the "+
				"audit it rests on", criterion.ID)
		return finding
	}
	if criterion.Audit != result.AuditID {
		finding.Detail = fmt.Sprintf(
			"%s cites audit %q but the guard was given %q: a target must be checked against the "+
				"audit it names", criterion.ID, criterion.Audit, result.AuditID)
		return finding
	}
	if criterion.Target > result.Ceiling {
		finding.Detail = fmt.Sprintf(
			"%s target %.6f exceeds the ceiling %.6f of audit %s (%d of %d classifiable incidents, "+
				"feeder set %s). No ceiling-bounded target may be set above the measured ceiling (FR-071)",
			criterion.ID, criterion.Target, result.Ceiling, result.AuditID,
			result.ceilingNumerator(), result.ClassifiableCount, result.FeederSetInForce)
		return finding
	}

	finding.Outcome = GuardPass
	finding.Detail = fmt.Sprintf("%s target %.6f is at or below the ceiling %.6f of audit %s",
		criterion.ID, criterion.Target, result.Ceiling, result.AuditID)
	return finding
}
